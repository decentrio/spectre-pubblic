package adapter

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"attestor/core"
	"attestor/host"
	"attestor/optimism/opstack"
	"attestor/types/attestation"
	attestorpb "attestor/types/attestor"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// providerKey stands for the API key an RPC provider puts in the endpoint URL.
const providerKey = "DUMMY-KEY-0123456789"

// endpointQueries are the shapes the query of a provider URL can take. The key
// sits after characters that are legal in a URL and that a text scan for its end
// could mistake for the end of it: apostrophes, brackets, semicolons, commas.
var endpointQueries = []struct{ name, query string }{
	{"plain", "apikey=" + providerKey},
	{"apostrophes before the key", "label='node'&apikey=" + providerKey},
	{"sub-delimiters before the key", "a=(b);c,d!*&e=[f]&apikey=" + providerKey},
	{"key between apostrophes", "x='1'&apikey=" + providerKey + "&y='2'"},
}

// connectionDroppingServer accepts every request and closes the connection
// without answering, as a provider outage or a reset does.
func connectionDroppingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type errorLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *errorLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *errorLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// serveHost runs a Host on an in-memory listener and returns a client for it.
func serveHost(t *testing.T, config host.Config) attestorpb.AttestorServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	config.Listener = listener
	attestorHost, err := host.New(config)
	if err != nil {
		t.Fatalf("host.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- attestorHost.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	connection, err := googlegrpc.NewClient(
		"passthrough:///attestor-e2e",
		googlegrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return attestorpb.NewAttestorServiceClient(connection)
}

// The audit's reproduction, end to end over the real dialer: a connection
// reset on the op-node endpoint produced a gRPC status that quoted
// `Post "http://127.0.0.1:.../rpc?apikey=<key>": EOF` to any caller.
func TestOPUpstreamTransportFailureDoesNotReachTheGRPCCaller(t *testing.T) {
	for _, endpoint := range endpointQueries {
		t.Run(endpoint.name, func(t *testing.T) {
			opUpstreamTransportFailure(t, "/rpc?"+endpoint.query)
		})
	}
}

func opUpstreamTransportFailure(t *testing.T, requestURI string) {
	t.Helper()
	server := connectionDroppingServer(t)
	replica, err := opstack.DialReplica(context.Background(), server.URL+requestURI, time.Minute)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer replica.Close()
	store, err := opstack.LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	attestor := opstack.New(
		opstack.Config{SrcChain: "op-e2e", AttestationHead: opstack.HeadFinalized},
		nil, replica, store, nil, nil, nil,
	)
	signer, err := attestation.NewSigner(10, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	bridge, err := New(attestor, signer, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Premise: the adapter's own error does carry the key. What keeps it from
	// the caller is the gRPC adapter, not the plugin.
	_, direct := bridge.VerifyStateRoot(context.Background(), core.BlockIdentityRequest{BlockNumber: 7, RunMode: core.RunModeFinalized})
	if direct == nil || !strings.Contains(direct.Error(), providerKey) {
		t.Fatalf("premise: adapter error %v should carry the endpoint URL", direct)
	}

	var logs errorLog
	client := serveHost(t, host.Config{
		Routes:   map[string]core.Ports{"op-e2e": {Feed: bridge, Verifier: bridge, Status: bridge, Watcher: bridge}},
		ErrorLog: logs.logf,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = client.VerifyStateRoot(ctx, &attestorpb.VerifyStateRootRequest{
		SrcChain:          "op-e2e",
		BlockNumber:       7,
		ExpectedStateRoot: make([]byte, 32),
		L2Router:          make([]byte, 20),
		AttestorSetHash:   make([]byte, 32),
		RunMode:           attestorpb.RunMode_RUN_MODE_FINALIZED,
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("VerifyStateRoot code = %v, want Unavailable (err %v)", status.Code(err), err)
	}
	if message := status.Convert(err).Message(); message != "read replica finalized head" {
		t.Fatalf("status message = %q, want only the classified message", message)
	}
	for _, leaked := range []string{providerKey, "apikey", "127.0.0.1", "EOF"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("caller-visible error %q carries %q", err, leaked)
		}
	}

	lines := logs.snapshot()
	if len(lines) != 1 || !strings.Contains(lines[0], "optimism_syncStatus") || !strings.Contains(lines[0], "EOF") {
		t.Fatalf("server log = %q, want one line with the upstream cause", lines)
	}
	// The log keeps the host, so the operator can see which endpoint failed, and
	// nothing of the path or the query.
	if !strings.Contains(lines[0], `Post "http://127.0.0.1:`) {
		t.Fatalf("server log line %q should name the endpoint's host", lines[0])
	}
	for _, leaked := range []string{providerKey, "apikey", "label", "node", "/rpc", "?"} {
		if strings.Contains(lines[0], leaked) {
			t.Fatalf("server log line %q carries %q", lines[0], leaked)
		}
	}
}

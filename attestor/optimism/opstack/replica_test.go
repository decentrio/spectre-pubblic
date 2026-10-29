package opstack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testRPCTimeout is the replica transport timeout for tests that do not
// exercise it.
const testRPCTimeout = time.Minute

// newFakeOpNode serves a minimal JSON-RPC endpoint speaking the two op-node
// methods the attestor uses.
func newFakeOpNode(t *testing.T, safe, unsafe uint64, outputRoot string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad rpc request: %v", err)
			return
		}
		var result any
		switch req.Method {
		case "optimism_syncStatus":
			result = map[string]any{
				"safe_l2":   map[string]any{"number": safe},
				"unsafe_l2": map[string]any{"number": unsafe},
			}
		case "optimism_outputAtBlock":
			result = map[string]any{"outputRoot": outputRoot}
		default:
			t.Errorf("unexpected rpc method %q", req.Method)
			return
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("failed to encode rpc response: %v", err)
		}
	}))
}

func TestReplicaClientSyncStatusAndOutput(t *testing.T) {
	wantRoot := "0x" + "ab" + "00000000000000000000000000000000000000000000000000000000000000"[0:62]
	srv := newFakeOpNode(t, 123, 456, wantRoot)
	defer srv.Close()

	ctx := context.Background()
	rc, err := DialReplica(ctx, srv.URL, testRPCTimeout)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer rc.Close()

	status, err := rc.SyncStatus(ctx)
	if err != nil {
		t.Fatalf("SyncStatus: %v", err)
	}
	if status.SafeL2 != 123 || status.UnsafeL2 != 456 {
		t.Fatalf("SyncStatus = %+v, want safe 123 unsafe 456", status)
	}

	got, err := rc.OutputAtBlock(ctx, 100)
	if err != nil {
		t.Fatalf("OutputAtBlock: %v", err)
	}
	if got[0] != 0xab {
		t.Fatalf("OutputAtBlock root[0] = %#x, want 0xab", got[0])
	}
}

// An op-node that accepts a request and never answers must not park the
// caller: DialReplica gives an HTTP endpoint the same transport timeout the L1
// client has.
func TestDialReplicaBoundsAHangingHTTPCall(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// httptest.Server.Close waits for handlers, so release them first.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	rc, err := DialReplica(context.Background(), srv.URL, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer rc.Close()

	done := make(chan error, 1)
	go func() {
		_, err := rc.SyncStatus(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call on a silent op-node succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call on a silent op-node was not bounded by the transport timeout")
	}
}

// A dial error reaches the process log at startup, and an RPC provider's API key
// travels in the URL: the error may name the endpoint, never the key.
func TestDialReplicaErrorsDoNotQuoteTheKey(t *testing.T) {
	for _, endpoint := range []string{
		"http://[::1/rpc?apikey=SECRET-KEY",
		"ws://127.0.0.1:1/ws/SECRET-KEY",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := DialReplica(ctx, endpoint, time.Second)
		cancel()
		if err == nil {
			t.Fatalf("DialReplica(%q) succeeded", endpoint)
		}
		if strings.Contains(err.Error(), "SECRET-KEY") || strings.Contains(err.Error(), "apikey") {
			t.Fatalf("DialReplica(%q) error %q carries the key", endpoint, err)
		}
	}
}

func TestReplicaClientRejectsShortOutputRoot(t *testing.T) {
	srv := newFakeOpNode(t, 1, 2, "0xabcd")
	defer srv.Close()

	ctx := context.Background()
	rc, err := DialReplica(ctx, srv.URL, testRPCTimeout)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer rc.Close()

	if _, err := rc.OutputAtBlock(ctx, 100); err == nil {
		t.Fatal("a non-32-byte output root must be rejected")
	}
}

func TestReplicaClientPropagatesRPCFailureForEveryRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error":   map[string]any{"code": -32000, "message": "op-node unavailable"},
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client, err := DialReplica(context.Background(), srv.URL, testRPCTimeout)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer client.Close()

	if _, err := client.SyncStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "op-node unavailable") {
		t.Fatalf("SyncStatus error = %v", err)
	}
	if _, err := client.OutputAtBlock(context.Background(), 100); err == nil || !strings.Contains(err.Error(), "op-node unavailable") {
		t.Fatalf("OutputAtBlock error = %v", err)
	}
	if _, err := client.CommitmentAt(context.Background(), 100); err == nil || !strings.Contains(err.Error(), "op-node unavailable") {
		t.Fatalf("CommitmentAt error = %v", err)
	}
}

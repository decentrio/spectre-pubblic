package types

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

func blackHoleServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	// httptest.Server.Close waits for handlers, so release them first.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server
}

func TestDialEthRPCBoundsHangingHTTPCall(t *testing.T) {
	server := blackHoleServer(t)
	client, err := DialEthRPC(context.Background(), server.URL, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("DialEthRPC: %v", err)
	}
	t.Cleanup(client.Close)

	done := make(chan error, 1)
	go func() {
		_, err := client.BlockNumber(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("hanging HTTP call succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hanging HTTP call was not bounded")
	}
}

type delayedBlockNumberService struct {
	delay time.Duration
}

func (s *delayedBlockNumberService) BlockNumber(ctx context.Context) (hexutil.Uint64, error) {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return 42, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestDialEthRPCLeavesWebSocketUnbounded(t *testing.T) {
	const httpTimeout = 5 * time.Millisecond
	rpcServer := rpc.NewServer()
	t.Cleanup(rpcServer.Stop)
	if err := rpcServer.RegisterName("eth", &delayedBlockNumberService{delay: 10 * httpTimeout}); err != nil {
		t.Fatalf("register RPC service: %v", err)
	}
	server := httptest.NewServer(rpcServer.WebsocketHandler(nil))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := DialEthRPC(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), httpTimeout)
	if err != nil {
		t.Fatalf("DialEthRPC: %v", err)
	}
	t.Cleanup(client.Close)

	// The server holds this call past the HTTP timeout, while the caller's
	// context still bounds the test if the WebSocket request gets stuck.
	started := time.Now()
	blockNumber, err := client.BlockNumber(ctx)
	if err != nil {
		t.Fatalf("WebSocket call beyond HTTP timeout: %v", err)
	}
	if blockNumber != 42 {
		t.Fatalf("block number = %d, want 42", blockNumber)
	}
	if elapsed := time.Since(started); elapsed <= httpTimeout {
		t.Fatalf("WebSocket call took %s, want more than HTTP timeout %s", elapsed, httpTimeout)
	}
}

// A dial error reaches the process log at startup, and an RPC provider's API
// key travels in the URL: the error may name the endpoint, never the key.
func TestDialEthRPCErrorsDoNotQuoteTheKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"unparsable url", "http://[::1/v2/SECRET-KEY"},
		{"refused websocket", "ws://127.0.0.1:1/ws/SECRET-KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := DialEthRPC(ctx, tc.url, time.Second)
			if err == nil {
				t.Fatalf("DialEthRPC(%q) succeeded", tc.url)
			}
			if strings.Contains(err.Error(), "SECRET-KEY") {
				t.Fatalf("DialEthRPC error %q carries the key", err)
			}
		})
	}
}

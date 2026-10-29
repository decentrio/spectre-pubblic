package types

import (
	"context"
	"testing"
	"time"
)

func TestDialRPCBoundsHangingHTTPCall(t *testing.T) {
	server := blackHoleServer(t)
	client, err := DialRPC(context.Background(), server.URL, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("DialRPC: %v", err)
	}
	t.Cleanup(client.Close)

	done := make(chan error, 1)
	go func() {
		var out string
		done <- client.CallContext(context.Background(), &out, "eth_blockNumber")
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

func TestDialRPCRejectsAnUnparsableURL(t *testing.T) {
	if _, err := DialRPC(context.Background(), "http://[::1", time.Second); err == nil {
		t.Fatal("DialRPC accepted a malformed URL")
	}
}

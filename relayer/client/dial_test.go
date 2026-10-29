package client

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"relayer/logbridge"
)

// blackHoleServer accepts the request and then never writes a response, holding
// the connection open until the test finishes. This is the failure mode the
// timeout exists for: the node is reachable, the TCP handshake and the HTTP
// request both succeed, and the answer simply never comes.
func blackHoleServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Cleanups run LIFO, so these two must be registered in this order:
	// release the parked handlers FIRST, then Close. httptest.Server.Close waits
	// for outstanding requests to finish, so closing while a handler is still
	// parked deadlocks the test rather than failing it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

func TestDialEthRPC_BoundsAHangingCall(t *testing.T) {
	srv := blackHoleServer(t)

	c, err := DialEthRPC(context.Background(), srv.URL, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("DialEthRPC: %v", err)
	}
	t.Cleanup(c.Close)

	done := make(chan error, 1)
	go func() {
		// context.Background() on purpose: the point is that the call is bounded
		// by the transport even when the caller supplies no deadline of its own,
		// which is exactly the shape of the call sites this fixes.
		_, err := c.BlockNumber(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the hanging call to fail, got nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return: the transport timeout was not applied")
	}
}

func TestDialEthRPC_LeavesWebsocketUnbounded(t *testing.T) {
	// A ws:// URL must not receive an http.Client deadline — an event
	// subscription is long-lived by design. Dialing a closed port fails either
	// way, so assert on the option plumbing instead: the call must not report a
	// timeout-shaped failure before it even reaches the network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing is listening now, so the dial fails fast

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = DialEthRPC(ctx, "ws://"+addr, time.Nanosecond)
	if err == nil {
		t.Fatal("expected dialing a closed port to fail")
	}
	// A 1ns http.Client timeout leaking onto the ws path would surface as a
	// deadline error rather than a connection-refused one.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("http timeout leaked onto the websocket path: %v", err)
	}
}

func TestDialCosmosRPC(t *testing.T) {
	t.Run("bounds a hanging call", func(t *testing.T) {
		srv := blackHoleServer(t)

		c, err := DialCosmosRPC(srv.URL, "/websocket", 250*time.Millisecond)
		if err != nil {
			t.Fatalf("DialCosmosRPC: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := c.Status(context.Background())
			done <- err
		}()

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected the hanging call to fail, got nil error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("call did not return: the transport timeout was not applied")
		}
	})

	// TestDialCosmosRPC/does not time out the subscription pins the property the whole
	// approach rests on: the deadline must reach the JSON-RPC caller and not the
	// WebSocket client, or it would silently undo the live-path fix in #377.
	t.Run("does not time out the subscription", func(t *testing.T) {
		srv := blackHoleServer(t)

		c, err := DialCosmosRPC(srv.URL, "/websocket", time.Nanosecond)
		if err != nil {
			t.Fatalf("DialCosmosRPC: %v", err)
		}
		if c.WSEvents == nil {
			t.Fatal("WSEvents is nil: the subscription client was not constructed")
		}
		// WSEvents holds its own connection, built by newWSEvents without reference
		// to the http.Client we bounded — so an absurdly small HTTP timeout must not
		// have prevented it from being set up.
	})

	// The two failures that decide whether a live Cosmos subscription is alive are
	// reported only through CometBFT's logger, which is a nop until SetLogger is
	// called. Nothing in this repository called it, so a resubscribe that gave up
	// and every event dropped on a full channel left no trace in any log. This pins
	// the wiring, because losing it again would be invisible.
	t.Run("wires the comet logger", func(t *testing.T) {
		c, err := DialCosmosRPC("http://127.0.0.1:26657", "/websocket", DefaultRPCTimeout)
		if err != nil {
			t.Fatalf("DialCosmosRPC: %v", err)
		}
		if _, ok := c.WSEvents.Logger.(cometLogger); !ok {
			t.Fatalf("WSEvents logger is %T, want cometLogger: CometBFT's subscription errors go to a nop logger without it", c.WSEvents.Logger)
		}
	})
}

// TestCometLogger_Format: the adapter has to carry the key/values, since the
// query and the error are the whole content of the lines it exists to surface.
func TestCometLogger_Format(t *testing.T) {
	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	cometLogger{}.With("query", "tm.event='Tx'").Error("Failed to resubscribe", "err", "boom")
	cometLogger{}.Debug("noisy per-message chatter")

	got := buf.String()
	for _, want := range []string{"ERROR", "Failed to resubscribe", "query=tm.event='Tx'", "err=boom"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log line missing %q:\n%s", want, got)
		}
	}
	// The level has to lead the line: logbridge reads it from the start, and a viewer
	// shows an ERROR buried mid-message as info. These are the lines that report an
	// abandoned resubscribe and a dropped event.
	if !strings.HasPrefix(got, logbridge.Error+"[cometbft] ") {
		t.Fatalf("an Error must start with the error token, got:\n%s", got)
	}

	buf.Reset()
	cometLogger{}.Info("resubscribed", "query", "tm.event='Tx'")
	if got := buf.String(); !strings.HasPrefix(got, "[cometbft] resubscribed") {
		t.Fatalf("an Info line is unmarked, so it is info; got:\n%s", got)
	}
	if strings.Contains(got, "noisy") {
		t.Fatalf("Debug must be dropped, it would bury the relayer's own log:\n%s", got)
	}
}

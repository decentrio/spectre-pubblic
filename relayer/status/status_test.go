package status

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"relayer/relay"
	"relayer/services"
	"strings"
	"testing"
	"time"
)

func fixedSnapshot(_ context.Context) Snapshot {
	return Snapshot{
		Now:    time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
		Paths:  []relay.Status{{Path: "cosmos->eth", ClientID: "07-tendermint-0", TrustedHeight: 41, RelayableHeight: 42}},
		Queues: []services.QueueStatus{{Direction: "cosmos", Queued: 3}},
	}
}

// The endpoint exposes packet identities and client ids. It must never grow a
// way to CHANGE them by accident, so anything that is not a read is refused
// rather than routed.
func TestHandler_IsReadOnly(t *testing.T) {
	srv := httptest.NewServer(Handler(fixedSnapshot, nil))
	t.Cleanup(srv.Close)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, srv.URL+Path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s returned %s, want 405: a read-only endpoint must refuse writes, not ignore them",
					method, resp.Status)
			}
			if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
				t.Errorf("Allow = %q, want GET so a caller learns what is permitted", allow)
			}
		})
	}
}

// Fetch and Handler are two halves of one wire format. Testing them apart would
// let the pair drift while both stay green.
func TestFetch_RoundTripsTheSnapshot(t *testing.T) {
	srv := httptest.NewServer(Handler(fixedSnapshot, nil))
	t.Cleanup(srv.Close)

	got, err := Fetch(context.Background(), strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := fixedSnapshot(context.Background())
	if len(got.Paths) != 1 || got.Paths[0].Path != want.Paths[0].Path {
		t.Fatalf("paths = %+v, want the served one", got.Paths)
	}
	if got.Paths[0].RelayableHeight != 42 || got.Paths[0].TrustedHeight != 41 {
		t.Fatalf("heights = %d/%d, want 41/42: these are the two numbers the command exists to show",
			got.Paths[0].TrustedHeight, got.Paths[0].RelayableHeight)
	}
	if len(got.Queues) != 1 || got.Queues[0].Queued != 3 {
		t.Fatalf("queues = %+v, want the served one", got.Queues)
	}
}

// An operator who cannot reach the endpoint is the common case -- it is off by
// default -- so the error has to say that rather than surface a bare dial error.
func TestFetch_SaysWhyItCouldNotConnect(t *testing.T) {
	srv := httptest.NewServer(Handler(fixedSnapshot, nil))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // nothing is listening now

	_, err := Fetch(context.Background(), addr)
	if err == nil {
		t.Fatal("Fetch succeeded against a closed endpoint")
	}
	if !strings.Contains(err.Error(), "status_address") {
		t.Fatalf("error = %v; want it to name the setting that turns the endpoint on", err)
	}
}

func TestServe(t *testing.T) {
	// Off is the default, and an engine must not have to know whether anyone is
	// listening: Serve with no address returns rather than binding something.
	t.Run("with no address binds nothing", func(t *testing.T) {
		done := make(chan error, 1)
		go func() { done <- Serve(context.Background(), "", fixedSnapshot, nil) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve(\"\") = %v, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve(\"\") blocked; an unconfigured endpoint must not hold a goroutine open")
		}
	})

	// A bind failure has to be reported, not logged and forgotten: an operator told
	// to query the endpoint must not find out during an incident that it never
	// started.
	t.Run("reports a bind failure", func(t *testing.T) {
		taken := httptest.NewServer(Handler(fixedSnapshot, nil))
		t.Cleanup(taken.Close)

		err := Serve(context.Background(), strings.TrimPrefix(taken.URL, "http://"), fixedSnapshot, nil)
		if err == nil {
			t.Fatal("Serve returned nil for an address already in use")
		}
		if !strings.Contains(err.Error(), "status endpoint") {
			t.Fatalf("error = %v, want it to name what failed to start", err)
		}
	})

	// The write route must not be served where other people can reach it.
	//
	// Reported by @DongLieu: warnIfReachable covers the snapshot, whose risk is
	// disclosure, and a warning is the right weight there. Reactivation CHANGES
	// relayer state and nothing in this package authenticates it, so binding it to
	// 0.0.0.0 publishes an unauthenticated mutation -- a decision an operator
	// inherits from an example config rather than makes.
	t.Run("refuses controls off loopback", func(t *testing.T) {
		ctl := &Controls{Reactivate: func(DeadLetterSelector) error { return nil }}

		t.Run("a non-loopback bind with controls on is refused", func(t *testing.T) {
			// Bounded on purpose: without the guard Serve does not return, it SERVES,
			// so an unbounded call here reports the defect as a wall-clock timeout
			// rather than as the defect. The mutation found that the first time.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- Serve(ctx, "0.0.0.0:0", fixedSnapshot, ctl) }()

			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Serve did not return: it is serving the write route on 0.0.0.0 instead of refusing it")
			}
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("served the write route on 0.0.0.0 (err = %v); anyone who can reach it could reactivate packets", err)
			}
			for _, want := range []string{"status_allow_control", ReactivatePath, "loopback"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q; an operator has to be able to act on it", err, want)
				}
			}
		})

		// The read-only endpoint keeps binding anywhere: its risk is disclosure, it
		// already warns, and taking it away would remove the diagnosis this whole
		// package exists for.
		t.Run("the same bind is allowed read-only", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, "0.0.0.0:0", fixedSnapshot, nil) }()

			select {
			case err := <-done:
				t.Fatalf("read-only serve returned early: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("read-only serve: %v", err)
			}
		})

		t.Run("loopback with controls is allowed", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, "127.0.0.1:0", fixedSnapshot, ctl) }()

			select {
			case err := <-done:
				t.Fatalf("loopback serve with controls returned early: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("loopback serve: %v", err)
			}
		})
	})

	// Reported by @neitdung. Serve waits on the shutdown goroutine unconditionally
	// after srv.Serve returns, and that goroutine closes its channel only after the
	// parent context is cancelled. So a listener that dies AFTER startup -- the one
	// case where the error is news -- never reaches the caller: Serve blocks until
	// something unrelated cancels the context, and the relayer runs on with a status
	// endpoint that answers nothing.
	//
	// Bounded, because the defect is "does not return": an unbounded version reports
	// it as a timeout of the whole package rather than as this test failing.
	t.Run("returns a listener failure without waiting for shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel() // NOT cancelled during the assertion: that is the point

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}

		served := make(chan error, 1)
		go func() { served <- serveOn(ctx, listener, fixedSnapshot, nil) }()

		// Kill the listener out from under the running server.
		time.Sleep(20 * time.Millisecond)
		_ = listener.Close()

		select {
		case err := <-served:
			if err == nil {
				t.Fatal("a listener that died mid-run reported a clean stop")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not return after its listener failed; it is waiting for a context " +
				"cancellation that may never come, and the endpoint is dead in the meantime")
		}
	})
}

// Enabling the write route must not make the READ route writable, nor the write
// route readable: two routes, two methods, no overlap.
func TestHandler_ControlRouteKeepsItsOwnMethod(t *testing.T) {
	ctl := &Controls{Reactivate: func(DeadLetterSelector) error { return nil }}
	srv := httptest.NewServer(Handler(fixedSnapshot, ctl))
	t.Cleanup(srv.Close)

	t.Run("the snapshot stays read-only", func(t *testing.T) {
		resp, err := http.Post(srv.URL+Path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s returned %s, want 405 even with controls on", Path, resp.Status)
		}
	})

	t.Run("the write route refuses a read", func(t *testing.T) {
		resp, err := http.Get(srv.URL + ReactivatePath)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s returned %s, want 405", ReactivatePath, resp.Status)
		}
	})
}

package status

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"relayer/relay"
	"relayer/services"
	"strings"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	// Status is off by default, so an engine registers with a registry that may not
	// exist. Making that the engine's problem would put a nil check at every call
	// site and eventually miss one.
	t.Run("nil registry answers instead of panicking", func(t *testing.T) {
		var reg *Registry
		reg.AddPath(func(context.Context) relay.Status { return relay.Status{} })
		reg.AddQueues("cosmos<->eth", func(time.Time) []services.QueueStatus { return nil })

		snap := reg.Snapshot(context.Background())
		if len(snap.Paths) != 0 || len(snap.Queues) != 0 {
			t.Fatalf("a nil registry produced %+v, want an empty snapshot", snap)
		}
	})

	t.Run("merges every source in order", func(t *testing.T) {
		reg := &Registry{}
		reg.AddPath(func(context.Context) relay.Status { return relay.Status{Path: "cosmos->eth"} })
		reg.AddPath(func(context.Context) relay.Status { return relay.Status{Path: "eth->cosmos"} })
		reg.AddQueues("cosmos<->eth", func(time.Time) []services.QueueStatus {
			return []services.QueueStatus{{Direction: "cosmos"}, {Direction: "eth"}}
		})

		snap := reg.Snapshot(context.Background())
		if len(snap.Paths) != 2 || snap.Paths[0].Path != "cosmos->eth" || snap.Paths[1].Path != "eth->cosmos" {
			t.Fatalf("paths = %+v, want both in registration order", snap.Paths)
		}
		if len(snap.Queues) != 2 {
			t.Fatalf("queues = %+v, want both directions flattened into one list", snap.Queues)
		}
		if snap.Now.IsZero() {
			t.Fatal("the snapshot carries no timestamp, so every age in it is unanchored")
		}
	})

	// The registry must not hold its lock across the functions it calls. One of them
	// makes a bounded RPC, and holding the lock across that would let a slow endpoint
	// block a second concurrent request -- during exactly the incident the endpoint
	// exists for.
	t.Run("does not hold its lock while asking", func(t *testing.T) {
		reg := &Registry{}
		reentered := false
		reg.AddPath(func(ctx context.Context) relay.Status {
			// Reenter exactly ONCE, guarded by a plain flag. Unconditional reentry is
			// infinite recursion rather than a lock test, and sync.Once cannot express
			// this either: calling Do from inside f deadlocks by design, which would
			// fail the test for a reason that has nothing to do with the registry.
			// One goroutine drives this, so the flag needs no synchronisation.
			if !reentered {
				reentered = true
				reg.Snapshot(ctx)
			}
			return relay.Status{Path: "reentrant"}
		})

		done := make(chan struct{})
		go func() { defer close(done); reg.Snapshot(context.Background()) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Snapshot deadlocked: the registry is holding its lock while calling out")
		}
	})
}

func TestReactivate(t *testing.T) {
	// The write route is ABSENT rather than forbidden when controls are off. 405
	// would say "this exists but not for you", which is the wrong thing to tell an
	// operator whose next step is to turn it on.
	t.Run("route exists only when controls are on", func(t *testing.T) {
		t.Run("off by default", func(t *testing.T) {
			srv := httptest.NewServer(Handler(fixedSnapshot, nil))
			t.Cleanup(srv.Close)

			err := Reactivate(context.Background(), strings.TrimPrefix(srv.URL, "http://"), DeadLetterSelector{Path: "cosmos<->eth", Direction: "cosmos", SourceClient: "src-0", Sequence: 7})
			if err == nil {
				t.Fatal("reactivate succeeded against an endpoint with no write route")
			}
			if !strings.Contains(err.Error(), "status_allow_control") {
				t.Fatalf("error = %v; want it to name the setting that enables the route", err)
			}
		})

		t.Run("enabled explicitly", func(t *testing.T) {
			var got DeadLetterSelector
			ctl := &Controls{Reactivate: func(sel DeadLetterSelector) error {
				got = sel
				return nil
			}}
			srv := httptest.NewServer(Handler(fixedSnapshot, ctl))
			t.Cleanup(srv.Close)

			if err := Reactivate(context.Background(), strings.TrimPrefix(srv.URL, "http://"), DeadLetterSelector{Path: "cosmos<->eth", Direction: "cosmos", SourceClient: "src-0", Sequence: 7}); err != nil {
				t.Fatalf("reactivate: %v", err)
			}
			if want := (DeadLetterSelector{Path: "cosmos<->eth", Direction: "cosmos", SourceClient: "src-0", Sequence: 7}); got != want {
				t.Fatalf("handler received %+v, want %+v; every field has to survive the round trip, "+
					"because path and direction are what make the write unambiguous", got, want)
			}
		})
	})

	// A request that names no packet must be refused, not turned into a lookup for
	// the empty client at sequence zero -- which would reach the tracker and refuse
	// there, with a worse message.
	t.Run("refuses an unnamed packet", func(t *testing.T) {
		called := false
		ctl := &Controls{Reactivate: func(DeadLetterSelector) error { called = true; return nil }}
		srv := httptest.NewServer(Handler(fixedSnapshot, ctl))
		t.Cleanup(srv.Close)

		// Each of these omits exactly one required field, plus the undecodable case.
		// Path and direction are in the list because they are what makes the write
		// name one tracker; accepting a request without them is the reported bug.
		for _, body := range []string{
			`{}`,
			`{"source_client":"src-0"}`,
			`{"sequence":7}`,
			`{"path":"cosmos<->eth","direction":"cosmos","source_client":"src-0"}`,
			`{"path":"cosmos<->eth","direction":"cosmos","sequence":7}`,
			`{"path":"cosmos<->eth","source_client":"src-0","sequence":7}`,
			`{"direction":"cosmos","source_client":"src-0","sequence":7}`,
			`not json`,
		} {
			resp, err := http.Post(srv.URL+ReactivatePath, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("post %q: %v", body, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("POST %q returned %s, want 400", body, resp.Status)
			}
		}
		if called {
			t.Fatal("an unnamed packet reached the tracker")
		}
	})

	// The tracker's refusal is the caller's answer -- "that packet is not where you
	// think it is" -- so it must arrive intact rather than as a bare 500.
	t.Run("carries the trackers refusal", func(t *testing.T) {
		ctl := &Controls{Reactivate: func(DeadLetterSelector) error {
			return errors.New("no dead-lettered packet for client src-0 sequence 7")
		}}
		srv := httptest.NewServer(Handler(fixedSnapshot, ctl))
		t.Cleanup(srv.Close)

		err := Reactivate(context.Background(), strings.TrimPrefix(srv.URL, "http://"), DeadLetterSelector{Path: "cosmos<->eth", Direction: "cosmos", SourceClient: "src-0", Sequence: 7})
		if err == nil {
			t.Fatal("a refused reactivation reported success")
		}
		if !strings.Contains(err.Error(), "no dead-lettered packet") {
			t.Fatalf("error = %v; want the tracker's own reason", err)
		}
	})

	// Reported by @neitdung. Refusing a non-loopback bind stops other HOSTS from
	// reaching the control route; it does nothing about the browser already running
	// on this one. A page on any site can POST to http://127.0.0.1:<port>/reactivate
	// as a CORS "simple request" -- no preflight, so nothing on this server is ever
	// asked for permission. The same-origin policy hides the RESPONSE from the page;
	// it does not stop the state change, and a state change is the whole risk here.
	t.Run("refuses a browser cross origin post", func(t *testing.T) {
		body := `{"path":"cosmos->eth","direction":"cosmos","source_client":"08-wasm-0","sequence":7}`

		for _, tc := range []struct {
			name        string
			contentType string
			origin      string
		}{
			// text/plain is what makes the request "simple": the browser sends it
			// without asking this server first.
			{name: "simple content type from a page", contentType: "text/plain;charset=UTF-8", origin: "https://attacker.example"},
			{name: "form encoding from a page", contentType: "application/x-www-form-urlencoded", origin: "https://attacker.example"},
			{name: "multipart from a page", contentType: "multipart/form-data; boundary=x", origin: "https://attacker.example"},
			// An Origin header at all means a browser sent this. Operators use curl.
			{name: "json claimed by a page", contentType: "application/json", origin: "https://attacker.example"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var called int
				handler := reactivateHandler(func(DeadLetterSelector) error { called++; return nil })

				req := httptest.NewRequest(http.MethodPost, ReactivatePath, strings.NewReader(body))
				req.Header.Set("Content-Type", tc.contentType)
				req.Header.Set("Origin", tc.origin)
				rec := httptest.NewRecorder()
				handler(rec, req)

				if called != 0 {
					t.Fatalf("the control ran for a cross-origin browser POST (%s / %s); "+
						"the same-origin policy hides the reply, not the mutation", tc.contentType, tc.origin)
				}
				if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnsupportedMediaType {
					t.Fatalf("status = %d, want the request refused before it reached the control", rec.Code)
				}
			})
		}

		// The content-type half on its own. Every case above also carries an Origin,
		// so the first check catches them and the second is never the thing that
		// refuses -- removing it left this whole test green, which is how a second
		// defence quietly becomes decoration. These carry NO Origin, so only the
		// content type can refuse them.
		t.Run("a simple content type is refused on its own merits", func(t *testing.T) {
			for _, contentType := range []string{
				"text/plain;charset=UTF-8",
				"application/x-www-form-urlencoded",
				"multipart/form-data; boundary=x",
				"", // a POST with no declared type at all
			} {
				var called int
				handler := reactivateHandler(func(DeadLetterSelector) error { called++; return nil })

				req := httptest.NewRequest(http.MethodPost, ReactivatePath, strings.NewReader(body))
				if contentType != "" {
					req.Header.Set("Content-Type", contentType)
				}
				rec := httptest.NewRecorder()
				handler(rec, req)

				if called != 0 {
					t.Fatalf("the control ran for a %q body with no Origin; requiring application/json "+
						"is what forces a preflight this server never answers", contentType)
				}
				if rec.Code != http.StatusUnsupportedMediaType {
					t.Fatalf("status = %d for %q, want 415", rec.Code, contentType)
				}
			}
		})

		// The control must still work for the caller it exists for.
		t.Run("an operator's own request still works", func(t *testing.T) {
			var called int
			handler := reactivateHandler(func(DeadLetterSelector) error { called++; return nil })

			req := httptest.NewRequest(http.MethodPost, ReactivatePath, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler(rec, req)

			if called != 1 || rec.Code != http.StatusOK {
				t.Fatalf("a plain application/json POST with no Origin was refused: status=%d calls=%d body=%q",
					rec.Code, called, rec.Body.String())
			}
		})
	})

	// The regression @neitdung asked for: two engines, one holding the same
	// (source_client, sequence) as the other, and the requested one alone changes.
	//
	// Client ids are chain-local, so a pair that identifies a packet inside one
	// tracker identifies nothing across a process. The old Reactivate offered the
	// pair to each registered engine in order and returned on the first success, so
	// an operator acting on the SECOND row `relayer status` printed gave a fresh
	// retry budget to the first and left the packet they meant dead-lettered.
	t.Run("acts on the named engine alone", func(t *testing.T) {
		type call struct {
			direction    string
			sourceClient string
			sequence     uint64
		}
		var first, second []call

		reg := &Registry{}
		reg.AddReactivator("cosmos<->eth", func(direction, sourceClient string, sequence uint64) error {
			first = append(first, call{direction, sourceClient, sequence})
			return nil
		})
		reg.AddReactivator("cosmos->l2", func(direction, sourceClient string, sequence uint64) error {
			second = append(second, call{direction, sourceClient, sequence})
			return nil
		})

		// The SECOND engine, named explicitly. Under the old first-success scan this
		// reached the first one instead, and reported success for it.
		sel := DeadLetterSelector{Path: "cosmos->l2", Direction: "l2", SourceClient: "client-0", Sequence: 9}
		if err := reg.Reactivate(sel); err != nil {
			t.Fatalf("Reactivate(%s): %v", sel, err)
		}
		if len(first) != 0 {
			t.Fatalf("the engine that was NOT named was asked: %+v; registration order must not decide a write", first)
		}
		if len(second) != 1 || second[0] != (call{"l2", "client-0", 9}) {
			t.Fatalf("named engine received %+v, want one call {l2 client-0 9}", second)
		}
	})

	// A name nobody registered must be refused, and the refusal must list the names
	// that exist -- the likely mistake is a typo in a string the operator just read.
	t.Run("refuses a path that is not registered", func(t *testing.T) {
		reg := &Registry{}
		reg.AddReactivator("cosmos<->eth", func(string, string, uint64) error {
			t.Fatal("an engine was asked for a path that does not name it")
			return nil
		})

		err := reg.Reactivate(DeadLetterSelector{Path: "typo", Direction: "cosmos", SourceClient: "c", Sequence: 1})
		if err == nil {
			t.Fatal("reactivating an unregistered path reported success")
		}
		if !strings.Contains(err.Error(), "cosmos<->eth") {
			t.Errorf("error %q does not name the paths this process serves", err)
		}
	})

	// Validation happens before anything is resolved, so a request that names no
	// packet never reaches an engine.
	t.Run("validates before it resolves", func(t *testing.T) {
		reg := &Registry{}
		reg.AddReactivator("cosmos<->eth", func(string, string, uint64) error {
			t.Fatal("an incomplete selector reached an engine")
			return nil
		})

		for _, sel := range []DeadLetterSelector{
			{},
			{Direction: "cosmos", SourceClient: "c", Sequence: 1},
			{Path: "cosmos<->eth", SourceClient: "c", Sequence: 1},
			{Path: "cosmos<->eth", Direction: "cosmos", Sequence: 1},
			{Path: "cosmos<->eth", Direction: "cosmos", SourceClient: "c"},
		} {
			if err := reg.Reactivate(sel); err == nil {
				t.Errorf("Reactivate(%+v) reported success; every field is required", sel)
			}
		}
	})
}

// The engine name a queue is reported under is the one a reactivation must
// quote back. If the snapshot printed one string and the write route accepted
// another, the operator would have nothing to copy.
func TestSnapshot_StampsTheEngineThatReportedTheQueue(t *testing.T) {
	reg := &Registry{}
	reg.AddQueues("cosmos<->eth", func(time.Time) []services.QueueStatus {
		return []services.QueueStatus{{Direction: "cosmos"}, {Direction: "eth"}}
	})
	reg.AddQueues("cosmos->l2", func(time.Time) []services.QueueStatus {
		return []services.QueueStatus{{Direction: "l2"}}
	})

	snap := reg.Snapshot(context.Background())
	got := map[string]string{}
	for _, q := range snap.Queues {
		got[q.Direction] = q.Path
	}
	want := map[string]string{"cosmos": "cosmos<->eth", "eth": "cosmos<->eth", "l2": "cosmos->l2"}
	for direction, path := range want {
		if got[direction] != path {
			t.Errorf("queue %q reported path %q, want %q", direction, got[direction], path)
		}
	}
}

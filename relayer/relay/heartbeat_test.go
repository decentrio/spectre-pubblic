package relay

import (
	"context"
	"errors"
	"relayer/chain"
	"strings"
	"testing"
	"time"
)

// The signal is "a tick finished", not "the chain moved". Those look identical
// on a busy chain and opposite on a quiet one, and the quiet one is where the
// wrong rule does damage: a relayer restarted for a chain that was healthy
// loses whatever it was half way through.
func TestHeartbeats(t *testing.T) {
	t.Run("records a completed pass", func(t *testing.T) {
		h := newHeartbeats()
		base := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
		h.now = func() time.Time { return base }
		h.record(workerRefresh, "client expires in 4h0m0s")

		h.now = func() time.Time { return base.Add(90 * time.Second) }
		got := h.snapshot()
		if len(got) != 1 {
			t.Fatalf("snapshot = %+v, want the one loop that beat", got)
		}
		if got[0].Worker != workerRefresh {
			t.Errorf("worker = %q, want %q", got[0].Worker, workerRefresh)
		}
		if got[0].SinceText != "1m30s" {
			t.Errorf("since = %q, want 1m30s: the age is the whole signal", got[0].SinceText)
		}
		if got[0].Note == "" {
			t.Error("the note was dropped; 'turning' and 'merely alive' read the same without it")
		}
	})

	// The loop an operator is looking for is the one that stopped, so it must not
	// be somewhere in the middle of a list.
	//
	// Five entries and twenty snapshots, deliberately. The source is a map, so
	// asserting an order over two entries is a coin flip that passes half the time
	// with the sort deleted -- which is exactly what happened to the first version
	// of this test. Five entries make a random permutation ordered one time in
	// 120, and twenty draws make "ordered every time" mean the sort ran.
	t.Run("puts the stalest loop first", func(t *testing.T) {
		base := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
		h := newHeartbeats()
		for i, worker := range []string{"a", "b", "c", "d", "e"} {
			h.now = func() time.Time { return base.Add(-time.Duration(i+1) * time.Minute) }
			h.record(worker, "")
		}
		h.now = func() time.Time { return base }

		for draw := 0; draw < 20; draw++ {
			got := h.snapshot()
			if len(got) != 5 {
				t.Fatalf("snapshot = %+v, want all five loops", got)
			}
			for i := 1; i < len(got); i++ {
				if got[i-1].Since < got[i].Since {
					t.Fatalf("draw %d out of order: %s (%s) before %s (%s)",
						draw, got[i-1].Worker, got[i-1].SinceText, got[i].Worker, got[i].SinceText)
				}
			}
		}
	})

	// A loop that ran and FAILED still beat: the beat measures whether the loop is
	// turning, and confusing that with success would make a healthy loop reporting
	// a broken endpoint look like a dead one.
	t.Run("a later pass replaces the earlier one", func(t *testing.T) {
		h := newHeartbeats()
		base := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
		h.now = func() time.Time { return base }
		h.record(workerRefresh, "first")
		h.now = func() time.Time { return base.Add(time.Minute) }
		h.record(workerRefresh, "second")

		got := h.snapshot()
		if len(got) != 1 {
			t.Fatalf("snapshot = %+v, want one entry per worker", got)
		}
		if got[0].Note != "second" {
			t.Fatalf("note = %q, want the latest pass", got[0].Note)
		}
	})

	t.Run("a loop that never ran is absent, not zero", func(t *testing.T) {
		// Reporting "last pass 55 years ago" for a loop that was never started is
		// worse than saying nothing: it looks like the wedge it is not.
		if got := newHeartbeats().snapshot(); len(got) != 0 {
			t.Fatalf("snapshot = %+v, want empty", got)
		}
	})

	t.Run("a nil heartbeats neither records nor panics", func(t *testing.T) {
		var h *heartbeats
		h.record(workerRefresh, "")
		if got := h.snapshot(); got != nil {
			t.Fatalf("snapshot = %+v, want nil", got)
		}
	})
}

// Each loop must actually beat. The type above can be correct while no loop
// calls it, which is the shape that leaves an operator reading an empty list and
// concluding the process is dead.
func TestHeartbeat_LoopsBeatWhenTheyRun(t *testing.T) {
	beat := func(t *testing.T, worker string, run func(*Module, context.Context)) {
		t.Helper()
		withFastRefreshTick(t)

		m := NewModule("test", "client-0",
			&mockSource{latest: 42},
			&mockDest{expiresAt: time.Now().Add(refreshMarginFor(0) + time.Hour)}, // healthy: no refresh work
			// trustingPeriod stays zero, so the margin is the fixed default; #451
			// replaced the constant this line used with a per-client size.
			&mockBuilder{},
			WithTimeoutScanner(time.Millisecond, func(context.Context) {}),
			WithPeriodicUpdate(time.Millisecond, 0, func(context.Context) error { return nil }),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); run(m, ctx) }()

		waitFor(t, ctx, func() bool {
			for _, loop := range m.beats.snapshot() {
				if loop.Worker == worker {
					return true
				}
			}
			return false
		}, worker+" completed passes without recording one; the status output would call a running loop dead")
		cancel()
		<-done
	}

	// A HEALTHY client is deliberate here: the refresh loop must beat on the pass
	// where it decides there is nothing to do. Beating only when it submits an
	// update would make a client with hours of trust left look like a stopped loop.
	t.Run(workerRefresh, func(t *testing.T) {
		beat(t, workerRefresh, func(m *Module, ctx context.Context) { m.refreshLoop(ctx) })
	})
	t.Run(workerTimeoutScan, func(t *testing.T) {
		beat(t, workerTimeoutScan, func(m *Module, ctx context.Context) { m.scanLoop(ctx) })
	})
	t.Run(workerPeriodicUpdate, func(t *testing.T) {
		beat(t, workerPeriodicUpdate, func(m *Module, ctx context.Context) { m.periodicUpdateLoop(ctx) })
	})
}

func refreshBeatNote(m *Module) (string, bool) { return beatNote(m, workerRefresh) }

// The enumeration backstop has to beat too, and for the sharpest reason of the
// four loops: it is the one that finds packets a lost cursor or an outage hid
// from the block scan. Reported by @DongLieu: it recorded nothing, so `relayer
// status` omitted it entirely and an operator asking "is anything still looking
// for my old packet" got no answer, which reads as a loop that never started.
func TestHeartbeat_FlushBeatsOnEveryEnding(t *testing.T) {
	send := chain.Event{Type: chain.SendPacket, Sequence: 7, Height: 40, Raw: []byte("old-packet")}

	for _, tc := range []struct {
		name string
		src  *listerSource
		want string
	}{
		{
			name: "the enumeration fails",
			src:  &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, listErr: errors.New("connection refused")},
			want: "enumeration failed",
		},
		{
			name: "nothing outstanding",
			src:  &listerSource{mockSource: mockSource{latest: 100, relayable: 100}},
			want: "nothing outstanding",
		},
		{
			name: "packets the scan did not find",
			src:  &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{send}},
			want: "relaying 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewModule("test", "client-0", tc.src, &mockDest{}, &mockBuilder{})
			m.flushOnce(context.Background(), tc.src)

			note, ok := beatNote(m, workerPacketFlush)
			if !ok {
				t.Fatalf("the flush ran and recorded no heartbeat; status would omit the backstop entirely")
			}
			if !strings.Contains(note, tc.want) {
				t.Fatalf("flush note = %q, want it to contain %q", note, tc.want)
			}
		})
	}
}

func beatNote(m *Module, worker string) (string, bool) {
	for _, h := range m.beats.snapshot() {
		if h.Worker == worker {
			return h.Note, true
		}
	}
	return "", false
}

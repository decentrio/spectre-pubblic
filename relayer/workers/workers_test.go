package workers

import (
	"slices"
	"testing"
	"time"
)

func TestDrain_WorkerGroupDrainsAndNamesStuckWorkers(t *testing.T) {
	g := New()
	done := make(chan struct{})
	g.Go("subscriber", func() { <-done })

	if running := g.stillRunning(); !slices.Equal(running, []string{"subscriber"}) {
		t.Fatalf("running = %v, want [subscriber]", running)
	}
	if running := g.Drain(20 * time.Millisecond); !slices.Equal(running, []string{"subscriber"}) {
		t.Fatalf("timed-out workers = %v, want [subscriber]", running)
	}
	close(done)
	if running := g.Drain(time.Second); len(running) != 0 {
		t.Fatalf("workers after release = %v, want none", running)
	}
}

// The nested drains must not share a budget. A source drains its own goroutines
// inside the drain the relay module runs over the source; with equal timeouts
// both expire on the same cancellation at the same moment, and whether the module
// observes the stuck worker or sees its subscriber return first is a race.
//
// This is an invariant rather than a behaviour test on purpose: reproducing the
// race takes the real timeouts, and the property that prevents it is one
// comparison. Reported by @DongLieu on #434.
func TestTheInnerDrainBudgetIsStrictlyShorterThanTheOuterOne(t *testing.T) {
	if SourceDrainTimeout >= ShutdownDrainTimeout {
		t.Fatalf("SourceDrainTimeout = %s, ShutdownDrainTimeout = %s; the inner drain must finish "+
			"while the outer one is still listening, or a stuck source worker is reported as a clean shutdown",
			SourceDrainTimeout, ShutdownDrainTimeout)
	}
	if SourceDrainTimeout <= 0 {
		t.Fatalf("SourceDrainTimeout = %s; a non-positive budget drains nothing", SourceDrainTimeout)
	}
}

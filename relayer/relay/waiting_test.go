package relay

import (
	"log"
	"relayer/chain"
	"strings"
	"testing"
	"time"
)

// fakeClock drives the tracker's notion of time so age assertions are exact
// rather than timing-dependent.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestTracker() (*waitTracker, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	w := newWaitTracker()
	w.now = clk.now
	return w, clk
}

func ackEvent(seq, height uint64) chain.Event {
	return chain.Event{Type: chain.AckPacket, Sequence: seq, Height: height}
}

// TestObserve_AccumulatesAge is the property the whole change exists for: a
// packet re-observed across flushes must report a GROWING age, because that is
// the only signal separating "waiting for finality" from "stuck forever".
func TestObserve_AccumulatesAge(t *testing.T) {
	w, clk := newTestTracker()
	e := ackEvent(1, 21)

	if age := w.observe(e); age != 0 {
		t.Fatalf("first observation age = %s, want 0 (no age can be known yet)", age)
	}

	clk.t = clk.t.Add(90 * time.Second)
	if age := w.observe(e); age != 90*time.Second {
		t.Fatalf("age after 90s = %s, want 1m30s", age)
	}

	clk.t = clk.t.Add(30 * time.Second)
	if age := w.observe(e); age != 2*time.Minute {
		t.Fatalf("age after a further 30s = %s, want 2m0s", age)
	}
}

// TestClear_ResetsAge covers the settle path: a delivered packet must not leave
// an entry behind that would inflate the age of a later packet reusing the same
// sequence (which happens when a client is re-created and sequences restart).
func TestClear_ResetsAge(t *testing.T) {
	w, clk := newTestTracker()
	e := ackEvent(1, 21)

	w.observe(e)
	clk.t = clk.t.Add(5 * time.Minute)
	w.clearPacket(chain.RelayPacket{Type: e.Type, Sequence: e.Sequence})

	if age := w.observe(e); age != 0 {
		t.Fatalf("age after clear = %s, want 0 (the entry must be gone)", age)
	}
}

// TestWaiting_RecvAndAckAreTrackedSeparately: a packet's forward delivery and its
// returning ack share a sequence, so keying on sequence alone would make one
// leg's clear wipe the other's age.
func TestWaiting_RecvAndAckAreTrackedSeparately(t *testing.T) {
	w, clk := newTestTracker()
	recv := chain.Event{Type: chain.SendPacket, Sequence: 7, Height: 10}
	ack := ackEvent(7, 21)

	w.observe(recv)
	clk.t = clk.t.Add(time.Minute)
	w.observe(ack)
	clk.t = clk.t.Add(time.Minute)

	w.clearPacket(chain.RelayPacket{Type: chain.SendPacket, Sequence: 7})

	if age := w.observe(ack); age != time.Minute {
		t.Fatalf("ack age after clearing the recv = %s, want 1m0s (clears must not cross legs)", age)
	}
}

// TestPurgeStale_BoundsTheTracker: entries for packets that left the queue via a
// path with no clear must not accumulate for the process's lifetime.
func TestPurgeStale_BoundsTheTracker(t *testing.T) {
	w, clk := newTestTracker()
	stale := ackEvent(1, 21)
	fresh := ackEvent(2, 22)

	w.observe(stale)
	clk.t = clk.t.Add(waitEntryTTL + time.Minute)
	w.observe(fresh) // refreshes only fresh's lastSeen

	w.purgeStale()

	if len(w.entries) != 1 {
		t.Fatalf("entries after purge = %d, want 1 (only the fresh one survives)", len(w.entries))
	}
	if age := w.observe(stale); age != 0 {
		t.Fatalf("purged entry still carried an age of %s, want 0", age)
	}
}

// TestPurgeStale_KeepsActivelyWaitingEntries guards the inverse mistake: purging on
// first-seen instead of last-seen would silently reset the age of exactly the
// long-waiting packet the operator is trying to diagnose.
func TestPurgeStale_KeepsActivelyWaitingEntries(t *testing.T) {
	w, clk := newTestTracker()
	e := ackEvent(1, 21)

	w.observe(e)
	// Re-observed every 15s (the waiting backoff cap) for well past the TTL.
	for elapsed := time.Duration(0); elapsed < waitEntryTTL+10*time.Minute; elapsed += 15 * time.Second {
		clk.t = clk.t.Add(15 * time.Second)
		w.observe(e)
		w.purgeStale()
	}

	if age := w.observe(e); age < waitEntryTTL {
		t.Fatalf("actively-waiting packet lost its age: %s, want >= %s", age, waitEntryTTL)
	}
}

func TestDescribeWaiting(t *testing.T) {
	t.Run("names each packet", func(t *testing.T) {
		got := describeWaiting([]waiting{
			{event: ackEvent(1, 21), age: 4*time.Minute + 12*time.Second},
		})
		want := "ack seq=1 h=21 for 4m12s"
		if got != want {
			t.Fatalf("describeWaiting = %q, want %q", got, want)
		}
	})

	// TestDescribeWaiting/orders by age: with the list capped, the entries shown must
	// be the oldest ones — those are the ones worth looking at.
	t.Run("orders by age", func(t *testing.T) {
		got := describeWaiting([]waiting{
			{event: ackEvent(1, 21), age: time.Second},
			{event: ackEvent(2, 22), age: time.Hour},
			{event: ackEvent(3, 23), age: time.Minute},
		})
		if !strings.HasPrefix(got, "ack seq=2 ") {
			t.Fatalf("describeWaiting = %q, want the 1h entry first", got)
		}
		if strings.Index(got, "seq=3") > strings.Index(got, "seq=1") {
			t.Fatalf("describeWaiting = %q, want seq=3 (1m) before seq=1 (1s)", got)
		}
	})

	t.Run("caps the list", func(t *testing.T) {
		items := make([]waiting, 0, waitListLimit+3)
		for i := range waitListLimit + 3 {
			items = append(items, waiting{
				event: ackEvent(uint64(i+1), 21),
				age:   time.Duration(i) * time.Second,
			})
		}
		got := describeWaiting(items)
		if strings.Count(got, "seq=") != waitListLimit {
			t.Fatalf("describeWaiting listed %d entries, want %d: %q",
				strings.Count(got, "seq="), waitListLimit, got)
		}
		if !strings.HasSuffix(got, "+3 more") {
			t.Fatalf("describeWaiting = %q, want a trailing \"+3 more\"", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := describeWaiting(nil); got != "" {
			t.Fatalf("describeWaiting(nil) = %q, want empty", got)
		}
	})
}

func TestEventType_Names(t *testing.T) {
	cases := map[chain.EventType]string{
		chain.SendPacket:    "recv",
		chain.AckPacket:     "ack",
		chain.TimeoutPacket: "timeout",
		chain.EventType(99): "unknown(99)",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Fatalf("EventType(%d).String() = %q, want %q", int(typ), got, want)
		}
	}
}

// --- wiring: the age must survive across flushes through handleBatch itself ---

// captureLog redirects the standard logger for the duration of fn and returns
// what was written. The waiting output is the deliverable here, so it is asserted
// directly rather than through the tracker's internals.
func captureLog(fn func()) string {
	var buf strings.Builder
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// TestWaiting_ReusedSequenceRestartsTheAge: sequences restart when create-clients-eth
// repoints a router client with migrateClient — the client id is unchanged, so
// only the source height tells the two generations apart. The later packet must
// start its own clock instead of reporting the retired one's age.
func TestWaiting_ReusedSequenceRestartsTheAge(t *testing.T) {
	w, clk := newTestTracker()

	retired := ackEvent(1, 21)
	w.observe(retired)
	clk.t = clk.t.Add(45 * time.Minute)
	if age := w.observe(retired); age != 45*time.Minute {
		t.Fatalf("age of the still-waiting packet = %s, want 45m0s", age)
	}

	// Same sequence after the re-creation, emitted at a height of its own.
	fresh := retired
	fresh.Height = retired.Height + 500
	if age := w.observe(fresh); age != 0 {
		t.Fatalf("reused sequence reported age %s; it inherited the retired packet's clock", age)
	}

	// And it now ages on its own.
	clk.t = clk.t.Add(2 * time.Minute)
	if age := w.observe(fresh); age != 2*time.Minute {
		t.Fatalf("age after 2m = %s, want 2m0s", age)
	}
}

// Every proof failure re-queues, so the loop revisits the same packet every
// flush. Without a ladder that is one identical line per flush -- 296 of them in
// the run that prompted this, with nothing to separate a ten-second RPC blip from
// a packet nobody will ever deliver.
func TestProofFailureThreshold(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want int
		why  string
	}{
		{"the first failure always reports", 0, 1, "an operator has to see the error at least once"},
		{"still inside the first minute", 30 * time.Second, 1, "no new information yet"},
		{"one minute", time.Minute, 2, ""},
		{"four minutes is still the one-minute step", 4 * time.Minute, 2, "the ladder widens on purpose"},
		{"five minutes", 5 * time.Minute, 3, ""},
		{"fifteen minutes", 15 * time.Minute, 4, "where the wording changes to STUCK"},
		{"one hour", time.Hour, 5, ""},
		{"two hours", 2 * time.Hour, 6, "past the ladder it keeps counting hourly"},
		{"nine hours", 9 * time.Hour, 13, "a packet stuck overnight leaves a trail, not one line then silence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := proofFailureThreshold(tc.age); got != tc.want {
				t.Fatalf("proofFailureThreshold(%s) = %d, want %d: %s", tc.age, got, tc.want, tc.why)
			}
		})
	}
}

// The ladder is only worth anything if a repeated failure is actually suppressed
// between thresholds, and released again when one is crossed.
func TestObserveProofFailure(t *testing.T) {
	t.Run("reports only on new thresholds", func(t *testing.T) {
		now := time.Unix(1_700_000_000, 0)
		w := newWaitTracker()
		w.now = func() time.Time { return now }
		event := chain.Event{Type: chain.AckPacket, Sequence: 21, Height: 11576899}

		if _, report := w.observeProofFailure(event); !report {
			t.Fatal("the first failure must be reported; otherwise the error never reaches the log at all")
		}

		// Same threshold: every flush in the next minute is silent.
		for i := 0; i < 20; i++ {
			now = now.Add(2 * time.Second)
			if _, report := w.observeProofFailure(event); report {
				t.Fatalf("a repeat inside the same threshold was reported at %s; this is the 296-identical-lines case",
					now.Sub(time.Unix(1_700_000_000, 0)))
			}
		}

		// Crossing one releases exactly one more line.
		now = now.Add(time.Minute)
		age, report := w.observeProofFailure(event)
		if !report {
			t.Fatal("crossing a threshold must report; suppressing it would hide a failure that is getting worse")
		}
		if age < time.Minute {
			t.Fatalf("reported age %s does not carry the real wait; the age is the whole point of the line", age)
		}
		now = now.Add(time.Second)
		if _, report := w.observeProofFailure(event); report {
			t.Fatal("the threshold was already reported; it must not report twice")
		}
	})

	// The ladder runs on the PROOF clock, not the total wait. A packet parks at
	// the finality or destination-coverage gate for as long as those gates take,
	// and none of that time is a proof failure. Charging it to the ladder makes
	// the first failure claim an age it never had -- and, worse, silently spends
	// the early steps, so the next line is an hour away.
	t.Run("the clock starts at the first proof failure, not the first wait", func(t *testing.T) {
		now := time.Unix(1_700_000_000, 0)
		w := newWaitTracker()
		w.now = func() time.Time { return now }
		event := chain.Event{Type: chain.AckPacket, Sequence: 7, Height: 48041794}

		// Sixteen minutes parked at a gate. Only the waiting log sees this.
		w.observe(event)
		now = now.Add(16 * time.Minute)
		if total := w.observe(event); total != 16*time.Minute {
			t.Fatalf("waiting age = %s, want 16m: the waiting log still needs the TOTAL wait", total)
		}

		// Now a proof is attempted for the first time, and fails.
		age, report := w.observeProofFailure(event)
		if !report {
			t.Fatal("the first proof failure must always report")
		}
		if age != 0 {
			t.Fatalf("first proof failure reported age %s, want 0: nothing had failed to prove yet, and %s >= proofFailureStuckAfter would log it as STUCK",
				age, age)
		}

		// The early steps must still be there. If the pre-proof wait had been
		// charged to the ladder, proofFailureThreshold(16m) = 4 would already be
		// recorded and everything below the one-hour step would be silent.
		now = now.Add(time.Minute)
		if _, report := w.observeProofFailure(event); !report {
			t.Fatal("the one-minute step was consumed before any proof ran; the whole early window is then silent until the one-hour step")
		}
	})

	// A packet that relays, or is dropped, must start from scratch if it ever
	// comes back -- a reused sequence is a different packet and inherits nothing.
	t.Run("restarts after a clear", func(t *testing.T) {
		now := time.Unix(1_700_000_000, 0)
		w := newWaitTracker()
		w.now = func() time.Time { return now }
		event := chain.Event{Type: chain.SendPacket, Sequence: 7, Height: 100}

		w.observeProofFailure(event)
		now = now.Add(2 * time.Hour)
		w.observeProofFailure(event)

		w.clear(event)

		now = now.Add(time.Second)
		age, report := w.observeProofFailure(event)
		if !report {
			t.Fatal("a packet observed again after being cleared must report its first failure")
		}
		if age != 0 {
			t.Fatalf("age after a clear = %s, want 0; a cleared packet must not inherit the old one's clock", age)
		}
	})
}

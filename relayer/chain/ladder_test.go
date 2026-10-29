package chain

import "testing"

// HalvingLadder is shared by three call sites in three packages
// (transaction/retry.go, chain/l2rollup/subscribe.go, subscriber/ethereum.go)
// and had no test of its own. Sharing it was deliberate -- "a fix to how the
// search terminates lands on both" -- which is exactly why the termination
// needs pinning here rather than in whichever caller someone remembered.
func TestHalvingLadder(t *testing.T) {
	// The whole ladder, not a sample: a caller walks it from 0 until !ok, so
	// the sequence AND where it stops are both the contract. 1 is the last
	// rung -- one message, or one block, is the smallest request that still
	// asks for something, and a rejection there is no longer a sizing problem.
	t.Run("halves down to 1 and then stops", func(t *testing.T) {
		for _, tc := range []struct {
			from uint64
			want []uint64
		}{
			{from: 2, want: []uint64{1}},
			{from: 3, want: []uint64{1}},
			{from: 100, want: []uint64{50, 25, 12, 6, 3, 1}},
			{from: 10_000, want: []uint64{5000, 2500, 1250, 625, 312, 156, 78, 39, 19, 9, 4, 2, 1}},
		} {
			ladder := HalvingLadder("blocks per log range", tc.from)
			var got []uint64
			for attempt := 0; attempt < len(tc.want)+5; attempt++ {
				next, ok := ladder(attempt)
				if !ok {
					break
				}
				got = append(got, next.To)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("from=%d walked %v (%d rungs), want %v (%d)", tc.from, got, len(got), tc.want, len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("from=%d rung %d = %d, want %d (full walk %v)", tc.from, i, got[i], tc.want[i], got)
				}
			}
		}
	})

	// The reason halving was chosen over a constant step, quoted from the
	// comment on the function: provider caps differ by three orders of
	// magnitude, so a linear walk from 10_000 to 10 would spend a thousand
	// rejected calls and halving spends ten. That is a claim about a number,
	// so it gets an assertion rather than a comment.
	t.Run("reaches a provider cap of 10 within ten attempts", func(t *testing.T) {
		ladder := HalvingLadder("blocks per log range", 10_000)
		for attempt := 0; attempt < 10; attempt++ {
			next, ok := ladder(attempt)
			if !ok {
				t.Fatalf("ladder ran out at attempt %d before reaching 10", attempt)
			}
			if next.To <= 10 {
				return
			}
		}
		t.Fatal("ten attempts did not get the request under a 10-block cap; " +
			"the comment on HalvingLadder promises exactly that")
	})

	// from is a RUNTIME value at every call site -- uint64(msgCount) in
	// transaction/retry.go:307, and a block-range width in the two log-range
	// callers -- so 0 and 1 are reachable inputs, not theoretical ones. Both
	// must yield no rung at all: there is nothing below 1 to ask for, and
	// handing back 0 would be a request for nothing.
	//
	// Note for whoever edits ladder.go next: the explicit `from <= 1` clause is
	// an early-out, not a distinct branch -- removing it leaves this subtest
	// green, because the loop below it reaches size < 1 on the first division
	// either way. Measured, not assumed. The behaviour is still worth a test;
	// the clause is worth keeping as documentation of intent.
	t.Run("a size that cannot shrink offers no rung", func(t *testing.T) {
		for _, from := range []uint64{0, 1} {
			if next, ok := HalvingLadder("messages per batch", from)(0); ok {
				t.Fatalf("from=%d offered %+v; there is no smaller request to make", from, next)
			}
		}
	})

	// A negative attempt is the branch that actually needs its own guard. The
	// loop below it runs `for i := 0; i <= attempt`, so with attempt = -1 it
	// does not run at all and the ladder would return `from` UNCHANGED while
	// reporting ok -- a "reduction" to the size that just failed, which the
	// caller would then retry forever.
	t.Run("a negative attempt is not a reduction to the original size", func(t *testing.T) {
		for _, attempt := range []int{-1, -2, -100} {
			if next, ok := HalvingLadder("messages per batch", 100)(attempt); ok {
				t.Fatalf("attempt=%d offered %+v, want no rung: that is the size that just failed", attempt, next)
			}
		}
	})

	// ChangeFunc is documented as "a pure function of the attempt number,
	// holding no state of its own... what stops two callers sharing a ladder
	// from advancing each other's position". Two callers do share it, so the
	// property is load-bearing rather than stylistic.
	t.Run("is a pure function of the attempt number", func(t *testing.T) {
		ladder := HalvingLadder("messages per batch", 64)
		first, ok := ladder(3)
		if !ok {
			t.Fatal("attempt 3 of a 64-message batch offered no rung")
		}
		// Walk the whole ladder past that point, out of order, and interleave
		// a second ladder built from the same size -- neither may move it.
		other := HalvingLadder("messages per batch", 64)
		for _, attempt := range []int{0, 5, 2, 9, 1, 4} {
			ladder(attempt)
			other(attempt)
		}
		again, ok := ladder(3)
		if !ok || again != first {
			t.Fatalf("attempt 3 = (%+v, %t) after other calls, want (%+v, true)", again, ok, first)
		}
	})

	// What is what an operator reads in the log line ("blocks per log range=39").
	// It is passed per call site, so carrying the wrong one -- or dropping it --
	// would name the wrong quantity in an incident.
	//
	// The walk is BOUNDED, and exhausting the bound is a failure rather than a
	// hang: halving from 100 cannot yield more than a handful of rungs, so a
	// ladder still offering them at 64 is one that never terminates. That is
	// also what catches a ladder whose floor slips from 1 to 0 -- it keeps
	// handing out zero forever, and an unbounded walk here would hang instead
	// of failing. A hung test says less than a red one.
	t.Run("carries the caller's name on every rung, and terminates", func(t *testing.T) {
		const what = "messages per batch"
		const bound = 64
		ladder := HalvingLadder(what, 100)
		for attempt := 0; attempt < bound; attempt++ {
			next, ok := ladder(attempt)
			if !ok {
				if attempt == 0 {
					t.Fatal("ladder offered no rung at all")
				}
				return
			}
			if next.What != what {
				t.Fatalf("rung %d named %q, want %q", attempt, next.What, what)
			}
		}
		t.Fatalf("ladder still offering rungs after %d attempts from 100; it does not terminate", bound)
	})
}

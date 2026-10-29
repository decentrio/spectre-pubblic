package chain

import (
	"errors"
	"fmt"
	"testing"
)

// There are three error classes and the third carries its own remedy.
// The type exists so that carrying it is not optional: the compiler, not a
// comment, is what stops a NeedsChange without a change.
func TestRetry_Classification(t *testing.T) {
	cause := errors.New("boom")

	t.Run("transient retries the same attempt", func(t *testing.T) {
		r := Transient(cause)
		if r.Outcome() != OutcomeTransient {
			t.Fatalf("outcome = %s, want transient", r.Outcome())
		}
		if _, ok := r.Next(0); ok {
			t.Fatal("a transient failure has nothing to change; offering a ladder invites a caller to walk one")
		}
	})

	t.Run("permanent offers no ladder", func(t *testing.T) {
		if _, ok := Permanent(cause).Next(0); ok {
			t.Fatal("a permanent failure must not look like it can be retried differently")
		}
	})

	// The class is only useful if the remedy travels with it. A caller must not
	// have to remember which errors have a ladder.
	t.Run("needs-change carries its ladder", func(t *testing.T) {
		r := NeedsChange(cause, func(attempt int) (Attempt, bool) {
			if attempt > 1 {
				return Attempt{}, false
			}
			return Attempt{What: "gas", To: uint64(100 * (attempt + 1))}, true
		})
		if r.Outcome() != OutcomeNeedsChange {
			t.Fatalf("outcome = %s, want needs-change", r.Outcome())
		}
		first, ok := r.Next(0)
		if !ok || first.To != 100 {
			t.Fatalf("first rung = %+v ok=%v, want gas=100", first, ok)
		}
		if _, ok := r.Next(2); ok {
			t.Fatal("past the last rung the ladder must report the bottom, not repeat itself")
		}
	})

	// A ladder that is nil despite the signature is treated as a ladder with no
	// rungs: the engine turns that permanent, which is the safe direction.
	t.Run("a nil ladder bottoms out rather than panicking", func(t *testing.T) {
		if _, ok := NeedsChange(cause, nil).Next(0); ok {
			t.Fatal("a nil ladder must not claim it has a next attempt")
		}
	})

	// errors.Is through the classification is what lets existing checks --
	// services/timeout_retry.go's errors.Is(err, ErrPacketAlreadyReceived) --
	// survive the migration untouched.
	t.Run("the cause stays reachable", func(t *testing.T) {
		sentinel := errors.New("already received")
		r := Permanent(fmt.Errorf("relay: %w", sentinel))
		if !errors.Is(r, sentinel) {
			t.Fatal("errors.Is no longer reaches the cause; every existing check would need rewriting")
		}
		if !errors.Is(r, ErrPermanent) {
			t.Fatal("the pre-existing sentinel must stay reachable while call sites migrate")
		}
		if !errors.Is(Transient(cause), ErrRetryable) {
			t.Fatal("same for the transient sentinel")
		}
	})
}

// Classify is the reading end. An error nobody classified must default to
// transient: a packet re-queued in error costs a retry, a packet dropped in
// error costs the funds in its escrow until the timeout scanner finds it.
func TestClassify_DefaultsToKeepingThePacket(t *testing.T) {
	t.Run("an unclassified error is transient and says so", func(t *testing.T) {
		r, known := Classify(errors.New("who knows"))
		if known {
			t.Fatal("Classify claimed to recognise an error it did not")
		}
		if r.Outcome() != OutcomeTransient {
			t.Fatalf("outcome = %s, want transient -- dropping an unclassified packet loses funds", r.Outcome())
		}
	})

	t.Run("nil is not a failure", func(t *testing.T) {
		if _, known := Classify(nil); known {
			t.Fatal("nil classified as a failure")
		}
	})

	t.Run("a wrapped Retry is found through the wrapping", func(t *testing.T) {
		r, known := Classify(fmt.Errorf("submit: %w", Permanent(errors.New("revert"))))
		if !known || r.Outcome() != OutcomePermanent {
			t.Fatalf("Classify = %s known=%v, want permanent", r.Outcome(), known)
		}
	})

	// The two-class helpers still tag most of the tree. Reading them is what lets
	// the migration proceed one call site at a time.
	t.Run("the old sentinels still classify", func(t *testing.T) {
		if r, known := Classify(fmt.Errorf("x: %w", ErrPermanent)); !known || r.Outcome() != OutcomePermanent {
			t.Fatalf("ErrPermanent = %s known=%v", r.Outcome(), known)
		}
		if r, known := Classify(fmt.Errorf("x: %w", ErrRetryable)); !known || r.Outcome() != OutcomeTransient {
			t.Fatalf("ErrRetryable = %s known=%v", r.Outcome(), known)
		}
	})
}

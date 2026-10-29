package chain

import (
	"errors"
	"fmt"
)

// This file is the third error class. There are three: Retryable and Permanent
// (see errors.go), and this one.
//
// The third is "not enough resources": out of gas, batch too large,
// provider refusing a log range. It is the easiest of the three to get wrong,
// and both wrong answers cost real money:
//
//   - filed as TRANSIENT and retried unchanged, it is not a retry at all, it is
//     an infinite loop that burns fees and sequence numbers and blocks every
//     packet behind it;
//   - filed as PERMANENT, a perfectly valid packet is dead-lettered and its
//     escrow stays locked.
//
// So the class carries its own remedy. A retry that cannot say what it will do
// differently is not in this class.

// Outcome is which of the three classes a failure belongs to.
type Outcome int

const (
	// OutcomeTransient: retry the identical attempt. Does not count against a
	// retry budget -- the attempt was never wrong, the world was briefly
	// unavailable.
	OutcomeTransient Outcome = iota
	// OutcomePermanent: the attempt cannot succeed as posed. Counts against the
	// budget; at the end of it the packet is dead-lettered and said out loud.
	OutcomePermanent
	// OutcomeNeedsChange: retry, but only after changing something. The change is
	// carried with the outcome, not left for the call site to remember.
	OutcomeNeedsChange
)

func (o Outcome) String() string {
	switch o {
	case OutcomeTransient:
		return "transient"
	case OutcomePermanent:
		return "permanent"
	case OutcomeNeedsChange:
		return "needs-change"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// Attempt is one rung of a change ladder: the quantity the next try will change,
// and its new value.
//
// One name and one number, not a bag. Every resource failure this relayer has is
// a number that must move -- messages per batch, gas headroom, blocks per log
// range -- so a struct that can hold anything would be wider than the problem
// and would let a ladder return something a test cannot compare. Two comparable
// fields are what lets C2 walk a ladder with change(0), change(1), change(2) and
// assert the three answers differ.
type Attempt struct {
	// What names the quantity, in the units an operator reads: "messages per
	// batch", "gas headroom (basis points)", "blocks per log range".
	What string
	// To is its value on this attempt.
	To uint64
}

func (a Attempt) String() string { return fmt.Sprintf("%s=%d", a.What, a.To) }

// ChangeFunc computes the attempt after n failures. ok is false at the bottom of
// the ladder: there is nothing left to change, and the engine turns the failure
// permanent there.
//
// It is a pure function of the attempt number, holding no state of its own.
// That is not tidiness -- it is what makes the ladder testable without running a
// relay loop, and it is what stops two callers sharing a ladder from advancing
// each other's position.
type ChangeFunc func(attempt int) (next Attempt, ok bool)

// Retry is a classified failure. It is an error, so errors.Is and errors.As keep
// working on whatever it wraps.
//
// Every field is unexported, and the only ways to build one are the three
// constructors below. That is the point of the type rather than an enum: a
// NeedsChange without a change does not compile, instead of compiling and
// looping forever. A struct with an exported nullable Change field would make
// the invariant a comment, and comments do not fail the build.
type Retry struct {
	outcome Outcome
	change  ChangeFunc
	reason  error
}

// Transient: retry the same attempt unchanged.
//
// It replaces the old Retryable helper and keeps its name in the sentinel, so
// errors.Is(err, ErrRetryable) still answers for callers that have not migrated.
func Transient(reason error) Retry {
	return Retry{outcome: OutcomeTransient, reason: wrapSentinel(ErrRetryable, reason)}
}

// Permanent: this attempt cannot succeed. Counts against the retry budget.
//
// Same shape as Transient: the sentinel stays underneath so IsPermanent and every
// existing errors.Is check keep working while call sites move over.
func Permanent(reason error) Retry {
	return Retry{outcome: OutcomePermanent, reason: wrapSentinel(ErrPermanent, reason)}
}

// wrapSentinel keeps the pre-existing sentinel reachable through Unwrap. Returns
// the bare sentinel when there is no cause, so a nil reason cannot produce an
// error that says nothing.
//
// BOTH are %w. The helpers this replaces wrote "%w: %v", which wrapped the
// sentinel and stringified the cause -- so errors.Is(chain.Permanent(x), x) was
// false, and a caller that classified a failure lost the ability to ask what it
// was. The rendered text is identical either way, which is why it went unnoticed.
func wrapSentinel(sentinel, reason error) error {
	if reason == nil {
		return sentinel
	}
	return fmt.Errorf("%w: %w", sentinel, reason)
}

// NeedsChange: retry, but change what the ladder says first.
//
// change is required by the signature, so the class cannot exist without its
// remedy. A nil ladder is still expressible by a caller determined to pass one,
// and Next treats it as a ladder with no rungs -- which the engine then turns
// permanent, the safe direction.
func NeedsChange(reason error, change ChangeFunc) Retry {
	return Retry{outcome: OutcomeNeedsChange, change: change, reason: reason}
}

// Outcome reports the class.
func (r Retry) Outcome() Outcome { return r.outcome }

// Next asks the ladder what to change after n failures.
//
// It answers false for every class but NeedsChange, so a caller that forgets to
// switch on the outcome cannot accidentally walk a ladder that does not exist.
func (r Retry) Next(attempt int) (Attempt, bool) {
	if r.outcome != OutcomeNeedsChange || r.change == nil {
		return Attempt{}, false
	}
	return r.change(attempt)
}

// Error renders the class and the cause. The class is in the text because these
// lines are read in a log, where the type is not visible.
func (r Retry) Error() string {
	if r.reason == nil {
		return r.outcome.String()
	}
	return fmt.Sprintf("%s: %v", r.outcome, r.reason)
}

// Unwrap keeps errors.Is/As working through the classification, so an existing
// check like errors.Is(err, ErrPacketAlreadyReceived) needs no rewrite.
func (r Retry) Unwrap() error { return r.reason }

// Classify reads the class out of any error, including one wrapped around a
// Retry.
//
// An unclassified error is TRANSIENT, and deliberately so: the default on a
// failure nobody has classified must be to keep the packet, not to drop it. A
// packet re-queued in error costs a retry; a packet dropped in error costs the
// funds in its escrow until the timeout scanner catches it.
func Classify(err error) (Retry, bool) {
	if err == nil {
		return Retry{}, false
	}
	var r Retry
	if errors.As(err, &r) {
		return r, true
	}
	// The two-class helpers in errors.go predate this type and still tag most of
	// the tree. Reading them here is what lets the migration proceed a call site
	// at a time instead of in one commit.
	switch {
	case IsPermanent(err):
		return Retry{outcome: OutcomePermanent, reason: err}, true
	case IsRetryable(err):
		return Retry{outcome: OutcomeTransient, reason: err}, true
	}
	return Retry{outcome: OutcomeTransient, reason: err}, false
}

// Climb asks a classified failure what the next attempt changes.
//
// This is the one place the doc asks for. Before it, every out-of-gas site wrote
// its own "the ladder is exhausted, report permanent" branch -- two in
// transaction/cosmos.go and one in transaction/ethereum.go, three copies of a
// decision that is not the call site's to make. A call site describes a ladder;
// who has climbed all of one is the engine's business.
//
// ok is true when there was a rung: use next and retry.
//
// ok is false at the bottom, and bottom is the SAME failure re-classified as
// Permanent -- returning it is then the only correct move. The original cause
// stays wrapped, so a caller's existing errors.Is (services.ErrPermanentRelayFailure,
// a *services.CosmosTxFailure fished out with errors.As) still finds what it
// looks for; the classification is added to the error, it does not replace it.
//
// A Transient or Permanent failure has no ladder by construction, so it reports
// the bottom immediately: a caller that Climbs the wrong class gets the safe
// answer rather than a rung that does not exist.
func Climb(r Retry, attempt int) (next Attempt, bottom Retry, ok bool) {
	if a, has := r.Next(attempt); has {
		return a, Retry{}, true
	}
	if r.outcome == OutcomePermanent {
		return Attempt{}, r, false
	}
	return Attempt{}, Permanent(r.reason), false
}

// Keep adds an outer layer's context to a failure WITHOUT overriding the class
// the inner layer already chose.
//
// It exists because the opposite is easy to write by accident and impossible to
// see afterwards: a wrapping layer that reaches for Transient re-labels every
// failure underneath it, including the ones an inner layer had deliberately
// called permanent. l2rollup's Builder.Build did exactly that to every header
// build, so an attestor answering "that route does not exist" -- a config
// mistake no retry can fix -- arrived at the engine looking like an RPC blip.
//
// An unclassified error still becomes transient, which is the same safe default
// Classify uses: an error nobody has classified must not cost a packet.
func Keep(err error) Retry {
	r, _ := Classify(err)
	switch r.outcome {
	case OutcomePermanent:
		if IsPermanent(err) {
			return Retry{outcome: OutcomePermanent, reason: err}
		}
		return Permanent(err)
	case OutcomeNeedsChange:
		return Retry{outcome: OutcomeNeedsChange, change: r.change, reason: err}
	default:
		// The sentinel is added when the error does not already carry one, and only
		// then. Without it a wrapped failure stops answering chain.IsRetryable, and
		// callers that predate the Retry type -- most of the tree -- ask exactly
		// that question. Adding it twice would read as "retryable: retryable".
		if IsRetryable(err) {
			return Retry{outcome: OutcomeTransient, reason: err}
		}
		return Transient(err)
	}
}

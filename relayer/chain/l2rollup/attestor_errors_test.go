package l2rollup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"relayer/chain"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for one test so an alarm can be
// asserted. The alarm is half of what two rows of the table require, so a test
// that only checked the class would pass on code that alarms about nothing.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	// Restore the writer the logger actually had. Passing nil here instead sets a
	// nil writer, and the next test in the package that logs anything panics --
	// which is how this helper failed the first time it was written.
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	})
	return &buf
}

// One case per row of the attestor table in §C1. The two directions that matter
// are opposite mistakes: a permanent failure classified transient is retried
// forever at one log line a flush, and a transient failure classified permanent
// throws away a good packet.
func TestClassifyAttestorFailure_ErrorTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		want    chain.Outcome
		alarm   bool
		because string
	}{
		{
			name:    "InvalidArgument is our own bug",
			err:     ErrAttestorBadRequest,
			want:    chain.OutcomePermanent,
			because: "the request is malformed on this side; retrying sends the same malformed request",
		},
		{
			name:    "NotFound is a misconfigured route",
			err:     ErrAttestorUnknownRoute,
			want:    chain.OutcomePermanent,
			because: "no retry makes a source chain the attestor does not serve appear",
		},
		{
			name:    "FailedPrecondition is the replica catching up",
			err:     ErrAttestorReplicaBehind,
			want:    chain.OutcomeTransient,
			because: "the replica reaches that height on its own; dropping the packet locks its escrow",
		},
		{
			name:    "Unavailable is silence, not disagreement",
			err:     ErrAttestorUnavailable,
			want:    chain.OutcomeTransient,
			because: "an attestor that cannot answer has said nothing about the block",
		},
		{
			name:    "Unimplemented is an invariant violation",
			err:     ErrAttestorUnimplemented,
			want:    chain.OutcomePermanent,
			alarm:   true,
			because: "startup should have rejected this daemon, so a human has to know a check did not run",
		},
		{
			name:    "VerifyStateRoot-specific unimplemented classifies as the general one",
			err:     ErrVerifyStateRootUnsupported,
			want:    chain.OutcomePermanent,
			alarm:   true,
			because: "it wraps ErrAttestorUnimplemented, so the table needs no second row for it",
		},
		{
			name:    "valid=false retries but must be seen",
			err:     ErrAttestorDivergence,
			want:    chain.OutcomeTransient,
			alarm:   true,
			because: "a replica lagging and a real divergence produce the same answer; retrying in silence hides the second",
		},
		{
			name:    "a signature that does not verify is not retryable",
			err:     ErrAttestorSignature,
			want:    chain.OutcomePermanent,
			alarm:   true,
			because: "either the pinned public key is not this daemon's, or the daemon signs something other than the block it was asked about; neither becomes true on the next flush",
		},
		{
			name:    "an unrecognised failure keeps the packet",
			err:     errors.New("something new"),
			want:    chain.OutcomeTransient,
			because: "a failure nobody classified must cost a retry, never a packet",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			got := classifyAttestorFailure("RelayableHeight", fmt.Errorf("call failed: %w", tc.err))

			if got.Outcome() != tc.want {
				t.Fatalf("classified %s, want %s — %s", got.Outcome(), tc.want, tc.because)
			}
			// The cause must survive classification, or every errors.Is below this
			// point starts answering no.
			if !errors.Is(got, tc.err) {
				t.Fatalf("the cause is no longer reachable through the classification: %v", got)
			}

			alarmed := strings.Contains(buf.String(), "[ATTENTION]")
			if alarmed != tc.alarm {
				if tc.alarm {
					t.Fatalf("no alarm raised — %s; log was %q", tc.because, buf.String())
				}
				t.Fatalf("alarm raised for an ordinary failure; operators stop reading a channel that cries wolf: %q", buf.String())
			}
			if tc.alarm && !strings.Contains(buf.String(), "RelayableHeight") {
				t.Fatalf("the alarm does not say which relay path raised it: %q", buf.String())
			}
		})
	}
}

// A permanent attestor failure must still be permanent by the time the engine
// reads it. The header builder classifies, but Builder.Build wraps whatever it
// returns — and a wrapper that reaches for Transient silently undoes the row
// above it.
func TestBuilder_DoesNotFlattenAClassifiedFailure(t *testing.T) {
	inner := classifyAttestorFailure("l2-opstack", fmt.Errorf("verify: %w", ErrAttestorUnknownRoute))
	m := &mockHeaderBuilder{err: inner}

	_, err := b(m).Build(context.Background(), headerRequest(1))
	if err == nil {
		t.Fatal("Build accepted a failing header build")
	}

	// Read the class the way C1 says the engine should: through Classify, which
	// answers with the OUTERMOST classification. The predicate pair cannot be used
	// here — chain.Transient(permanentErr) leaves the inner permanent sentinel
	// reachable, so IsPermanent stays true and a flattening wrapper looks correct
	// while Classify reports transient. That disagreement is the ambiguity the
	// Retry type exists to remove, so it is what this test pins.
	r, known := chain.Classify(err)
	if !known || r.Outcome() != chain.OutcomePermanent {
		t.Fatalf("Build classified a misconfigured route as %s; it would be retried on every flush forever", r.Outcome())
	}
	// And the two readings must agree, or a caller's answer depends on which one it
	// happened to reach for.
	if chain.IsRetryable(err) {
		t.Fatalf("the failure reads as BOTH retryable and permanent: %v", err)
	}
	if !errors.Is(err, ErrAttestorUnknownRoute) {
		t.Fatalf("the attestor cause did not survive the wrap: %v", err)
	}
}

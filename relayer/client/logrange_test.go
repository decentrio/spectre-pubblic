package client

import (
	"errors"
	"fmt"
	"testing"
)

// The two failures this must tell apart both arrive as an opaque JSON-RPC error,
// and the right action for each is the opposite of the other: a range rejection
// is fixed only by asking for less, a rate limit only by waiting. Getting it
// backwards is not a wasted retry — a rate limit read as a range rejection walks
// the span down to one block and is then reported permanent, abandoning the
// recovery scan that closes the event-loss window.
func TestIsLogRangeRejection(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want bool
	}{
		// The real wordings, one per provider family.
		{"geth and infura", "query returned more than 10000 results", true},
		{"erigon", "logs matched by query exceeds limit of 10000", true},
		{"alchemy size", "Log response size exceeded. this block range should work: [0x1, 0x64]", true},
		{"alchemy span", "You can make eth_getLogs requests with up to a 2K block range", true},
		{"gateway", "block range too large", true},
		{"quicknode", "eth_getLogs range limit exceeded", true},
		{"drpc", "requested range too wide", true},

		// Rate limits. Several providers answer these with the SAME JSON-RPC code
		// (-32005) they use for range limits, which is why the code is never
		// consulted and these are checked first.
		{"plain rate limit", "rate limit exceeded", false},
		{"http 429", "429 Too Many Requests", false},
		{"alchemy compute units", "Your app has exceeded its compute units per second capacity", false},
		{"rate limit naming a range", "rate limit exceeded for block range queries", false},

		// Everything else stays unclassified, which leaves it transient.
		{"transport", "dial tcp 127.0.0.1:8545: connect: connection refused", false},
		{"revert", "execution reverted", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLogRangeRejection(errors.New(tc.msg)); got != tc.want {
				t.Fatalf("IsLogRangeRejection(%q) = %v, want %v", tc.msg, got, tc.want)
			}
			// It must survive wrapping: every call site wraps the provider error
			// with the span it was refused for before anyone classifies it.
			wrapped := fmt.Errorf("span [100,200]: %w", errors.New(tc.msg))
			if got := IsLogRangeRejection(wrapped); got != tc.want {
				t.Fatalf("wrapped %q = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
	if IsLogRangeRejection(nil) {
		t.Fatal("nil classified as a range rejection")
	}
}

// A pruned-history refusal is fixed by a higher lower bound, not a narrower span,
// so it must be told apart from a range rejection in BOTH directions: read as a
// range rejection it would shrink the span to one block -- still refused, since
// geth rejects on fromBlock alone -- and then be reported permanent (#537).
func TestIsPrunedHistory(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want bool
	}{
		{"geth history expiry", "pruned history unavailable", true},
		{"wrapped by the flush", "[eth->cosmos Flush] seq=1511 logs [0,11847537]: pruned history unavailable", true},
		{"other wording", "history has been pruned for block 0x0", true},
		{"range rejection", "block range too large", false},
		{"transport", "dial tcp 127.0.0.1:8545: connect: connection refused", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPrunedHistory(errors.New(tc.msg)); got != tc.want {
				t.Fatalf("IsPrunedHistory(%q) = %v, want %v", tc.msg, got, tc.want)
			}
			if tc.want && IsLogRangeRejection(errors.New(tc.msg)) {
				t.Fatalf("%q is also classified a range rejection; narrowing cannot fix it", tc.msg)
			}
		})
	}
	if IsPrunedHistory(nil) {
		t.Fatal("nil classified as pruned history")
	}
}

// Width is what makes narrowing work for a source nobody configured: with no
// chunk set, the ladder must anchor on the span the provider actually refused,
// not on a zero that would end the ladder immediately.
func TestLogSpan_WidthAnchorsOnWhatWasAsked(t *testing.T) {
	full := &LogSpan{}
	if got := full.Width(100, 199); got != 100 {
		t.Fatalf("unconfigured span over [100,199] = %d, want the full 100 blocks", got)
	}
	capped := &LogSpan{Chunk: 10}
	if got := capped.Width(100, 199); got != 10 {
		t.Fatalf("configured span = %d, want the configured 10", got)
	}
	// A chunk wider than the range is not the width that gets refused.
	wide := &LogSpan{Chunk: 1000}
	if got := wide.Width(100, 199); got != 100 {
		t.Fatalf("chunk wider than the range = %d, want the range width 100", got)
	}
	if got := full.Width(200, 100); got != 0 {
		t.Fatalf("inverted range = %d, want 0", got)
	}
}

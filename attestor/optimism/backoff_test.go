package attestor

import (
	"testing"
	"time"
)

func TestBackoffDoublesAndCaps(t *testing.T) {
	var b Backoff
	now := time.Unix(1000, 0)

	if !b.Ready(now) {
		t.Fatal("fresh backoff must be ready")
	}
	if d := b.RecordFailure(now); d != failureBackoffMin {
		t.Fatalf("first failure delay = %s, want %s", d, failureBackoffMin)
	}
	if b.Ready(now.Add(30 * time.Second)) {
		t.Fatal("must not be ready before the delay elapses")
	}
	if !b.Ready(now.Add(failureBackoffMin)) {
		t.Fatal("must be ready once the delay elapses")
	}
	if d := b.RecordFailure(now); d != 2*failureBackoffMin {
		t.Fatalf("second failure delay = %s, want %s", d, 2*failureBackoffMin)
	}
	for range 10 {
		b.RecordFailure(now)
	}
	if d := b.RecordFailure(now); d != failureBackoffMax {
		t.Fatalf("delay must cap at %s, got %s", failureBackoffMax, d)
	}

	b.RecordSuccess()
	if !b.Ready(now) {
		t.Fatal("backoff must reset on success")
	}
	if d := b.RecordFailure(now); d != failureBackoffMin {
		t.Fatalf("post-reset failure delay = %s, want %s", d, failureBackoffMin)
	}
}

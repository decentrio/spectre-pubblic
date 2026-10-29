package services

import (
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// newServicesWithDeadLetterInBoth builds a Services whose cosmos AND eth
// trackers hold the SAME (source client, sequence), dead-lettered.
//
// That collision is the whole point: an implementation that scans in a fixed
// order passes every test where only one tracker holds the packet, because
// there is no wrong answer available to give.
func newServicesWithDeadLetterInBoth(t *testing.T, sourceClient string, sequence uint64) *Services {
	t.Helper()
	deadLetter := func() *PendingPacketTracker {
		tracker := NewPendingPacketTracker()
		pkt := channeltypesv2.Packet{SourceClient: sourceClient, Sequence: sequence, TimeoutTimestamp: 1}
		if !tracker.Add(pkt, 100) {
			t.Fatal("Add rejected the packet")
		}
		for i := 0; i <= maxTimeoutAttempts; i++ {
			tracker.recordTimeoutFailureForTest(pkt.SourceClient, pkt.Sequence, time.Now())
		}
		if tracker.DeadLetteredTimeouts() != 1 {
			t.Fatalf("dead-lettered %d, want the packet to have given up first", tracker.DeadLetteredTimeouts())
		}
		return tracker
	}
	return &Services{BatchBuilder: &BatchBuilder{
		PendingTracker:    deadLetter(),
		EthPendingTracker: deadLetter(),
	}}
}

// Reactivation acts on the tracker it is TOLD, never on the first one that
// happens to hold a matching pair.
//
// Reported by @neitdung: this used to try cosmos, then eth, then l2 and return
// on the first success. A packet identity is (source client, sequence) and
// client ids are chain-local, so two trackers can hold the same pair -- and the
// operator's choice was then decided by the order of a switch statement.
func TestReactivateDeadLetter_ActsOnTheNamedTrackerAlone(t *testing.T) {
	const client = "client-0"
	const sequence = uint64(9)

	// The same identity dead-lettered in BOTH trackers. Without that, any
	// implementation passes: the bug only shows when there is a wrong answer
	// available to give.
	svc := newServicesWithDeadLetterInBoth(t, client, sequence)

	if err := svc.ReactivateDeadLetter(DirectionEth, client, sequence); err != nil {
		t.Fatalf("reactivate eth: %v", err)
	}
	if got := len(svc.BatchBuilder.EthPendingTracker.DeadLetteredTimeoutPackets()); got != 0 {
		t.Fatalf("the eth tracker still holds %d dead letters; the named one was not reactivated", got)
	}
	if got := len(svc.BatchBuilder.PendingTracker.DeadLetteredTimeoutPackets()); got != 1 {
		t.Fatalf("the cosmos tracker holds %d dead letters, want 1: naming eth must not touch it", got)
	}
}

// An unknown direction is refused rather than treated as "search them all",
// which would restore the scan this replaced.
func TestReactivateDeadLetter_RefusesAnUnknownDirection(t *testing.T) {
	svc := newServicesWithDeadLetterInBoth(t, "client-0", 9)

	err := svc.ReactivateDeadLetter("", "client-0", 9)
	if err == nil {
		t.Fatal("an empty direction reported success")
	}
	if got := len(svc.BatchBuilder.PendingTracker.DeadLetteredTimeoutPackets()); got != 1 {
		t.Fatalf("the cosmos tracker changed on an unnamed direction: %d dead letters left", got)
	}
}

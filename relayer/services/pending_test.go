package services

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	client "relayer/client"
	relayerclient "relayer/client"
	"sync"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func TestPendingPacketTracker(t *testing.T) {
	t.Run("add remove", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		p := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 1}

		pt.Add(p, 100)
		if pt.Len() != 1 {
			t.Fatalf("expected len 1, got %d", pt.Len())
		}

		pt.removeCurrentForTest("src-0", 1)
		if pt.Len() != 0 {
			t.Fatalf("expected len 0 after remove, got %d", pt.Len())
		}
	})

	t.Run("add preserves timeout retry state", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		p := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 1, TimeoutTimestamp: 1}
		pt.Add(p, 100)
		now := time.Now()
		pt.recordTimeoutFailureForTest(p.SourceClient, p.Sequence, now)
		pt.deferTimeoutRetryForTest(p.SourceClient, p.Sequence, now)

		// Mirrors the relay module re-offering a requeued event before every attempt.
		pt.Add(p, 200)

		got := pt.GetAll()
		if len(got) != 1 {
			t.Fatalf("tracked packets = %d, want 1", len(got))
		}
		if got[0].TimeoutAttempts != 1 || got[0].Deferrals != 1 {
			t.Fatalf("retry state after duplicate Add = attempts:%d deferrals:%d, want 1:1",
				got[0].TimeoutAttempts, got[0].Deferrals)
		}
		if !got[0].NotBefore.After(now) {
			t.Fatalf("duplicate Add cleared retry deadline: %s", got[0].NotBefore)
		}
		if got[0].BlockNumber != 100 {
			t.Fatalf("duplicate Add replaced block number = %d, want original 100", got[0].BlockNumber)
		}
	})

	t.Run("add treats equivalent protobuf representations as same packet", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		nilPayloads := channeltypesv2.Packet{
			SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 1,
			TimeoutTimestamp: 100, Payloads: nil,
		}
		pt.Add(nilPayloads, 100)
		now := time.Now()
		pt.recordTimeoutFailureForTest(nilPayloads.SourceClient, nilPayloads.Sequence, now)

		emptyPayloads := nilPayloads
		emptyPayloads.Payloads = []channeltypesv2.Payload{}
		pt.Add(emptyPayloads, 200)

		got := pt.GetAll()
		if len(got) != 1 || got[0].TimeoutAttempts != 1 {
			t.Fatalf("equivalent protobuf replay reset retry state: %#v", got)
		}
		if got[0].BlockNumber != 100 {
			t.Fatalf("equivalent protobuf replay replaced block number = %d, want 100", got[0].BlockNumber)
		}
	})

	t.Run("dead letter tombstone blocks replay until retention expires", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		p := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 1, TimeoutTimestamp: 1}
		pt.Add(p, 100)
		now := time.Now()
		for i := 0; i <= maxTimeoutAttempts; i++ {
			pt.recordTimeoutFailureForTest(p.SourceClient, p.Sequence, now)
		}
		if pt.Len() != 0 || pt.DeadLetteredTimeouts() != 1 {
			t.Fatalf("dead-letter state = active:%d dead:%d, want 0:1", pt.Len(), pt.DeadLetteredTimeouts())
		}

		// A replayed source event must not silently receive a fresh retry budget.
		pt.Add(p, 200)
		if pt.Len() != 0 {
			t.Fatal("replayed event revived a dead-lettered timeout")
		}

		pt.mtx.Lock()
		pt.deadLettered[0].DeadLetteredAt = now.Add(-deadLetterRetention)
		pt.mtx.Unlock()
		pt.PurgeStaleWithoutTimeout(time.Hour)
		got := pt.GetAll()
		if len(got) != 1 || got[0].TimeoutAttempts != 0 || got[0].Deferrals != 0 || !got[0].NotBefore.IsZero() {
			t.Fatalf("renewed retry state = %#v, want fresh active packet", got)
		}
		if pt.DeadLetteredTimeouts() != 0 {
			t.Fatal("expired dead letter remained in the gauge")
		}
	})

	t.Run("terminal tombstone blocks an in-flight replay but not a reused sequence", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		settled := channeltypesv2.Packet{
			SourceClient: "src-0", DestinationClient: "dst-old", Sequence: 7,
			TimeoutTimestamp: 1000, Payloads: []channeltypesv2.Payload{{Value: []byte("old")}},
		}
		if err := pt.MarkSettled(settled); err != nil {
			t.Fatal(err)
		}
		if !pt.IsSettled(settled) {
			t.Fatal("terminal packet has no tombstone")
		}
		if !pt.Add(settled, 100) {
			t.Fatal("a settled replay must be a successful no-op, not a persistence failure")
		}
		if got := pt.Len(); got != 0 {
			t.Fatalf("settled replay recreated %d pending packet(s)", got)
		}

		// A client migration may reuse seq=7. The old terminal event must not
		// hide the new packet merely because it occupies the same slot.
		replacement := settled
		replacement.DestinationClient = "dst-new"
		replacement.Payloads = []channeltypesv2.Payload{{Value: []byte("new")}}
		if pt.IsSettled(replacement) {
			t.Fatal("old terminal tombstone matched a different packet identity")
		}
		if !pt.Add(replacement, 200) {
			t.Fatal("replacement packet was not tracked")
		}
		if got := pt.Len(); got != 1 {
			t.Fatalf("replacement left %d pending packet(s), want 1", got)
		}
	})

	t.Run("terminal event right after add leaves no orphaned entry", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		packet := channeltypesv2.Packet{
			SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 11,
			TimeoutTimestamp: 1000, Payloads: []channeltypesv2.Payload{{Value: []byte("race")}},
		}
		// This is the other ordering of the handoff race: handleBatch's Add wins
		// against the terminal event's MarkSettled, so the active record briefly
		// exists before the terminal event lands.
		if !pt.Add(packet, 100) {
			t.Fatal("initial track failed")
		}
		if got := pt.Len(); got != 1 {
			t.Fatalf("tracked packet count = %d, want 1", got)
		}

		// SubscribeCosmos observes acknowledge_packet immediately after the
		// handler recorded the packet.
		if err := pt.MarkSettled(packet); err != nil {
			t.Fatal(err)
		}
		if got := pt.Len(); got != 0 {
			t.Fatalf("MarkSettled right after Add left %d pending packet(s), want 0 -- an orphaned tracker entry", got)
		}
		if !pt.IsSettled(packet) {
			t.Fatal("MarkSettled right after Add did not leave a tombstone")
		}
	})

	t.Run("terminal tombstone survives restart", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.json")
		first, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		packet := channeltypesv2.Packet{
			SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 9,
			TimeoutTimestamp: 1000, Payloads: []channeltypesv2.Payload{{Value: []byte("packet")}},
		}
		if err := first.MarkSettled(packet); err != nil {
			t.Fatal(err)
		}

		restarted, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		if !restarted.IsSettled(packet) {
			t.Fatal("restart lost the terminal tombstone")
		}
		if !restarted.Add(packet, 100) {
			t.Fatal("settled replay reported a false durability failure")
		}
		if got := restarted.Len(); got != 0 {
			t.Fatalf("restart replay recreated %d pending packet(s)", got)
		}
	})

	t.Run("add replaces renewed packet when sequence is reused", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		old := channeltypesv2.Packet{SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 7, TimeoutTimestamp: 1000}
		pt.Add(old, 100)
		now := time.Now()
		for i := 0; i <= maxTimeoutAttempts; i++ {
			pt.recordTimeoutFailureForTest(old.SourceClient, old.Sequence, now)
		}

		pt.mtx.Lock()
		pt.deadLettered[0].DeadLetteredAt = now.Add(-deadLetterRetention)
		pt.mtx.Unlock()
		pt.PurgeStaleWithoutTimeout(time.Hour)

		// A counterparty client can be re-created while reusing its router client ID,
		// restarting packet sequences. The new packet must replace the expired
		// tombstone's renewed packet rather than being silently ignored as a replay.
		new := channeltypesv2.Packet{SourceClient: "src-0", DestinationClient: "dst-1", Sequence: 7, TimeoutTimestamp: 999999}
		pt.Add(new, 500)
		got := pt.GetAll()
		if len(got) != 1 {
			t.Fatalf("tracked packets = %d, want 1", len(got))
		}
		if !reflect.DeepEqual(got[0].Packet, new) || got[0].BlockNumber != 500 {
			t.Fatalf("tracked packet = %#v at block %d, want new packet at block 500", got[0].Packet, got[0].BlockNumber)
		}
	})

	t.Run("stale scan cannot mutate replacement", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		old := channeltypesv2.Packet{SourceClient: "src-0", DestinationClient: "dst-old", Sequence: 7, TimeoutTimestamp: 1000}
		pt.Add(old, 100)
		stale := pt.GetDue(time.Now())[0]

		newPacket := channeltypesv2.Packet{SourceClient: "src-0", DestinationClient: "dst-new", Sequence: 7, TimeoutTimestamp: 999999}
		pt.Add(newPacket, 500)

		// Every mutation a timeout scan can apply must be conditional on the packet
		// identity it scanned, not merely the reused lookup key. The same applies to
		// a delayed terminal source event carrying the old packet.
		pt.RemovePacketIfCurrent(old)
		pt.DeferTimeoutRetryIfCurrent(stale, time.Now())
		pt.RecordTimeoutFailureIfCurrent(stale, time.Now())
		pt.ClearDeferralsIfCurrent(stale)
		pt.RemoveIfCurrent(stale)

		got := pt.GetAll()
		if len(got) != 1 {
			t.Fatalf("stale scan removed replacement: tracked=%d", len(got))
		}
		if got[0].identity != identifyPacket(newPacket) || got[0].BlockNumber != 500 {
			t.Fatalf("tracked packet = %#v at block %d, want replacement at block 500", got[0].Packet, got[0].BlockNumber)
		}
		if got[0].TimeoutAttempts != 0 || got[0].Deferrals != 0 || !got[0].NotBefore.IsZero() {
			t.Fatalf("stale scan mutated replacement retry state: %#v", got[0])
		}
	})

	t.Run("get all snapshot", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		p1 := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 1}
		p2 := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 2}

		pt.Add(p1, 100)
		pt.Add(p2, 200)

		all := pt.GetAll()
		if len(all) != 2 {
			t.Fatalf("expected 2 items, got %d", len(all))
		}

		// Mutating the returned slice must not affect internal state
		all = append(all, pendingPacketInfo{})
		if pt.Len() != 2 {
			t.Fatalf("expected internal len 2 after external append, got %d", pt.Len())
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(seq uint64) {
				defer wg.Done()
				p := channeltypesv2.Packet{SourceClient: "src-0", Sequence: seq}
				pt.Add(p, seq)
				if seq%2 == 0 {
					pt.removeCurrentForTest("src-0", seq)
				}
			}(uint64(i))
		}
		wg.Wait()

		// All even sequences should have been removed
		for i := 0; i < 50; i++ {
			exists := false
			for _, info := range pt.GetAll() {
				if info.Packet.Sequence == uint64(i) {
					exists = true
					break
				}
			}
			if i%2 == 0 && exists {
				t.Fatalf("seq %d should have been removed", i)
			}
			if i%2 == 1 && !exists {
				t.Fatalf("seq %d should still exist", i)
			}
		}
	})

	t.Run("purge stale without timeout", func(t *testing.T) {
		pt := NewPendingPacketTracker()
		noTimeout := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 1}
		withTimeout := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 2, TimeoutTimestamp: uint64(time.Now().Add(2 * time.Hour).Unix())}

		pt.Add(noTimeout, 100)
		pt.Add(withTimeout, 200)

		pt.mtx.Lock()
		for key, info := range pt.packets {
			info.ObservedAt = time.Now().Add(-2 * time.Hour)
			pt.packets[key] = info
		}
		pt.mtx.Unlock()

		pt.PurgeStaleWithoutTimeout(1 * time.Hour)

		all := pt.GetAll()
		if len(all) != 1 {
			t.Fatalf("expected len 1 after targeted purge, got %d", len(all))
		}
		if all[0].Packet.Sequence != 2 {
			t.Fatalf("expected packet with timeout to remain, got seq=%d", all[0].Packet.Sequence)
		}
	})

	// An RPC outage lasting the whole retry window must delay a timeout, never
	// abandon it — the packet's funds are escrowed and only a timeout releases them.
	t.Run("transient send failures never dead letter", func(t *testing.T) {
		t.Parallel()

		tracker, pkt := trackerWith(t, 7)
		now := time.Now()
		transient := errors.New("dial tcp: connection refused")

		for i := 0; i < maxTimeoutAttempts*3; i++ {
			if timeoutOutcomeForError(transient) == timeoutFailed {
				tracker.recordTimeoutFailureForTest(pkt.SourceClient, pkt.Sequence, now)
			} else {
				tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now)
			}
		}

		if got := tracker.DeadLetteredTimeouts(); got != 0 {
			t.Fatalf("an RPC outage dead-lettered %d packet(s); escrowed funds must not be abandoned for someone else's downtime", got)
		}
		if tracker.Len() != 1 {
			t.Fatal("the packet must stay tracked so it is retried once the outage clears")
		}
	})

	// A received packet must never consume the retry budget, on either direction.
	//
	// This replaces an earlier test that pinned the opposite: the classifier used to
	// map ErrPacketAlreadyReceived to timeoutFailed so that any path which did not
	// special-case it would at least stop retrying. That fallback charged the packet
	// and dead-lettered it, which reports escrowed funds needing rescue for a packet
	// whose funds already moved. Both scanners now recognise the sentinel directly,
	// so the fallback is gone and charging is simply wrong.
	t.Run("received packet never charges the retry budget", func(t *testing.T) {
		t.Parallel()

		tracker, pkt := trackerWith(t, 11)
		now := time.Now()
		received := fmt.Errorf("build timeout proof: %w", relayerclient.ErrPacketAlreadyReceived)

		outcome := timeoutOutcomeForError(received)
		if outcome == timeoutFailed {
			t.Fatal("a received packet must not be charged; dead-lettering it reports stuck funds that are not stuck")
		}
		if !outcome.packetIsDone() {
			t.Fatal("a received packet must leave the tracker rather than be retried")
		}

		// Drive the scanners' action for that outcome many times over: it must never
		// dead-letter, no matter how often it is reached.
		for i := 0; i < maxTimeoutAttempts*2; i++ {
			if outcome.packetIsDone() {
				tracker.removeCurrentForTest(pkt.SourceClient, pkt.Sequence)
				continue
			}
			tracker.recordTimeoutFailureForTest(pkt.SourceClient, pkt.Sequence, now)
		}
		if got := tracker.DeadLetteredTimeouts(); got != 0 {
			t.Fatalf("DeadLetteredTimeouts() = %d; a delivered packet is not an operator problem", got)
		}
		if tracker.Len() != 0 {
			t.Fatal("a received packet must leave the tracker")
		}
	})

	// A packet whose receipt already exists on the destination is not a failure: it
	// was received, its funds moved, and no timeout for it can ever be valid. It
	// must leave the tracker quietly.
	//
	// Dead-lettering it instead — which is what the classifier alone would do —
	// would count it under "escrowed funds cannot be refunded without operator
	// action", sending someone to investigate a packet that completed normally.
	t.Run("received packet is dropped not dead lettered", func(t *testing.T) {
		t.Parallel()

		tracker, pkt := trackerWith(t, 21)
		received := fmt.Errorf("build timeout proof: %w", relayerclient.ErrPacketAlreadyReceived)

		// The action scanForCosmosTimeouts takes on this error.
		if errors.Is(received, relayerclient.ErrPacketAlreadyReceived) {
			tracker.removeCurrentForTest(pkt.SourceClient, pkt.Sequence)
		}

		if got := tracker.Len(); got != 0 {
			t.Fatalf("tracker still holds %d packet(s); a received packet must not be retried", got)
		}
		if got := tracker.DeadLetteredTimeouts(); got != 0 {
			t.Fatalf("DeadLetteredTimeouts() = %d; a received packet is not an operator problem", got)
		}
	})

	// Deferrals shape the backoff and feed the stuck gauge, so a packet that
	// recovers must not keep paying for an outage that has passed.
	t.Run("clear deferrals resets after recovery", func(t *testing.T) {
		t.Parallel()

		tracker, pkt := trackerWith(t, 23)
		now := time.Now()
		for i := 0; i < deferralAlertThreshold; i++ {
			tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now)
		}
		if count, worst := tracker.StuckTimeouts(); count != 1 || worst != deferralAlertThreshold {
			t.Fatalf("StuckTimeouts() = (%d, %d), want (1, %d)", count, worst, deferralAlertThreshold)
		}

		tracker.clearDeferralsForTest(pkt.SourceClient, pkt.Sequence)

		if count, worst := tracker.StuckTimeouts(); count != 0 || worst != 0 {
			t.Fatalf("StuckTimeouts() = (%d, %d) after recovery, want (0, 0)", count, worst)
		}
		if got := tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now); got != 1 {
			t.Fatalf("first deferral after a reset reported %d, want 1", got)
		}
		if tracker.Len() != 1 {
			t.Fatal("clearing deferrals must not drop the packet")
		}
	})

	t.Run("timeout retry backoff and dead letter", func(t *testing.T) {
		tracker := NewPendingPacketTracker()
		packet := timeoutRetryPacket(1)
		tracker.Add(packet, 10)
		now := time.Now()

		if tracker.recordTimeoutFailureForTest(packet.SourceClient, packet.Sequence, now) {
			t.Fatal("first deterministic failure must not dead-letter")
		}
		if due := tracker.GetDue(now); len(due) != 0 {
			t.Fatalf("packet under retry backoff is immediately due: %d", len(due))
		}
		if due := tracker.GetDue(now.Add(timeoutRetryBackoffMax)); len(due) != 1 {
			t.Fatalf("packet did not become due after its bounded backoff: %d", len(due))
		}

		for attempt := 1; attempt <= maxTimeoutAttempts; attempt++ {
			dead := tracker.recordTimeoutFailureForTest(packet.SourceClient, packet.Sequence, now)
			if attempt < maxTimeoutAttempts && dead {
				t.Fatalf("attempt %d dead-lettered early", attempt+1)
			}
			if attempt == maxTimeoutAttempts && !dead {
				t.Fatalf("attempt %d did not dead-letter", attempt+1)
			}
		}
		if tracker.Len() != 0 || tracker.DeadLetteredTimeouts() != 1 {
			t.Fatalf("dead-letter state = tracked:%d dead:%d, want 0:1", tracker.Len(), tracker.DeadLetteredTimeouts())
		}
	})
}

// A dead-lettered packet has funds escrowed and a log line asking an operator to
// intervene. Until this existed, the only mechanism for intervening was
// unreachable Go: the set could be listed and never left.
func TestReactivateDeadLetteredTimeout(t *testing.T) {
	deadLetter := func(t *testing.T) (*PendingPacketTracker, channeltypesv2.Packet) {
		t.Helper()
		tracker := NewPendingPacketTracker()
		pkt := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 7, TimeoutTimestamp: 1}
		if !tracker.Add(pkt, 100) {
			t.Fatal("Add rejected the packet")
		}
		for i := 0; i <= maxTimeoutAttempts; i++ {
			tracker.recordTimeoutFailureForTest(pkt.SourceClient, pkt.Sequence, time.Now())
		}
		if tracker.DeadLetteredTimeouts() != 1 {
			t.Fatalf("dead-lettered %d, want the packet to have given up first", tracker.DeadLetteredTimeouts())
		}
		return tracker, pkt
	}

	t.Run("returns the packet to the scan set", func(t *testing.T) {
		tracker, pkt := deadLetter(t)

		if err := tracker.ReactivateDeadLetteredTimeout(pkt.SourceClient, pkt.Sequence); err != nil {
			t.Fatalf("reactivate: %v", err)
		}
		if got := tracker.DeadLetteredTimeouts(); got != 0 {
			t.Fatalf("still %d dead-lettered; the tombstone would keep denying it a retry budget", got)
		}
		info, ok := tracker.currentPacketForTest(pkt.SourceClient, pkt.Sequence)
		if !ok {
			t.Fatal("the packet left the dead-letter set without joining the active one; it is now in neither")
		}

		// Resumed rather than reset, the ladder is still exhausted and the very
		// next scan spends an attempt re-discovering that.
		if info.TimeoutAttempts != 0 {
			t.Errorf("attempts = %d, want a fresh budget: reactivating means a person decided the cause is gone",
				info.TimeoutAttempts)
		}
		if !info.NotBefore.IsZero() {
			t.Errorf("NotBefore = %s, want it due on the next scan", info.NotBefore)
		}
		if !info.DeadLetteredAt.IsZero() {
			t.Errorf("DeadLetteredAt = %s, want it cleared: the packet has not given up any more", info.DeadLetteredAt)
		}
	})

	t.Run("is due on the very next scan", func(t *testing.T) {
		tracker, pkt := deadLetter(t)
		if err := tracker.ReactivateDeadLetteredTimeout(pkt.SourceClient, pkt.Sequence); err != nil {
			t.Fatalf("reactivate: %v", err)
		}
		if due := tracker.GetDue(time.Now()); len(due) != 1 || due[0].Packet.Sequence != pkt.Sequence {
			t.Fatalf("due = %+v, want the reactivated packet: anything else and the operator's action did nothing", due)
		}
	})

	// An operator who mistyped a sequence must learn it here, not by watching
	// nothing happen for the next half hour.
	t.Run("refuses a packet it does not hold", func(t *testing.T) {
		tracker, pkt := deadLetter(t)

		if err := tracker.ReactivateDeadLetteredTimeout("src-0", 999); err == nil {
			t.Error("reactivating an unknown sequence reported success")
		}
		if err := tracker.ReactivateDeadLetteredTimeout("src-other", pkt.Sequence); err == nil {
			t.Error("reactivating another client's sequence reported success")
		}
		if tracker.DeadLetteredTimeouts() != 1 {
			t.Fatal("a refused reactivation changed the dead-letter set")
		}
	})

	t.Run("refuses a packet that is merely pending", func(t *testing.T) {
		tracker := NewPendingPacketTracker()
		pkt := channeltypesv2.Packet{SourceClient: "src-0", Sequence: 3, TimeoutTimestamp: 1}
		tracker.Add(pkt, 100)

		if err := tracker.ReactivateDeadLetteredTimeout(pkt.SourceClient, pkt.Sequence); err == nil {
			t.Error("reactivated a packet that had not given up; the command would read as a no-op that worked")
		}
	})
}

func TestNewPersistentPendingPacketTracker(t *testing.T) {
	t.Run("rehydrates active and tombstones", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.json")
		tracker, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Nanosecond)
		active := channeltypesv2.Packet{SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 1, TimeoutTimestamp: 100}
		dead := channeltypesv2.Packet{SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 2, TimeoutTimestamp: 200}
		tracker.Add(active, 101)
		tracker.recordTimeoutFailureForTest(active.SourceClient, active.Sequence, now)
		tracker.deferTimeoutRetryForTest(active.SourceClient, active.Sequence, now)
		tracker.Add(dead, 202)
		for i := 0; i <= maxTimeoutAttempts; i++ {
			tracker.recordTimeoutFailureForTest(dead.SourceClient, dead.Sequence, now)
		}

		restored, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		got := restored.GetAll()
		if len(got) != 1 {
			t.Fatalf("restored active packets = %d, want 1", len(got))
		}
		if got[0].TimeoutAttempts != 1 || got[0].Deferrals != 1 || got[0].BlockNumber != 101 || got[0].NotBefore.IsZero() {
			t.Fatalf("restored active retry state = %#v", got[0])
		}
		if restored.DeadLetteredTimeouts() != 1 {
			t.Fatalf("restored tombstones = %d, want 1", restored.DeadLetteredTimeouts())
		}

		// Startup/gap recovery may replay this event immediately. The rehydrated
		// tombstone must block it instead of granting a fresh retry budget.
		restored.Add(dead, 303)
		if restored.Len() != 1 || restored.DeadLetteredTimeouts() != 1 {
			t.Fatalf("replayed dead letter after restore: active=%d dead=%d", restored.Len(), restored.DeadLetteredTimeouts())
		}
	})

	t.Run("rolls back failed mutation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.json")
		tracker, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		packet := channeltypesv2.Packet{SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 1, TimeoutTimestamp: 100}
		tracker.Add(packet, 10)
		tracker.writeState = func(string, []byte) error { return errors.New("disk full") }

		info := tracker.GetAll()[0]
		if dead, err := tracker.RecordTimeoutFailureIfCurrent(info, time.Now()); err == nil || dead {
			t.Fatal("failed persistence reported a committed dead letter")
		}
		if tracker.PersistenceError() == nil {
			t.Fatal("failed retry-state mutation did not surface the persistence error")
		}
		if due := tracker.GetDue(time.Now()); len(due) != 0 {
			t.Fatalf("failed retry-state mutation left %d packet(s) immediately due", len(due))
		}
		if _, err := tracker.DeferTimeoutRetriesIfCurrent([]pendingPacketInfo{info}, time.Now()); err == nil {
			t.Fatal("failed deferral mutation did not report its persistence error")
		}
		got := tracker.GetAll()
		if len(got) != 1 || got[0].TimeoutAttempts != 0 || !got[0].NotBefore.IsZero() {
			t.Fatalf("failed persistence changed in-memory retry state: %#v", got)
		}

		restored, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		persisted := restored.GetAll()
		if len(persisted) != 1 || persisted[0].TimeoutAttempts != 0 || !persisted[0].NotBefore.IsZero() {
			t.Fatalf("failed mutation leaked to durable retry state: %#v", persisted)
		}
	})

	t.Run("add reports failed mutation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.json")
		tracker, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		tracker.writeState = func(string, []byte) error { return errors.New("disk full") }
		packet := channeltypesv2.Packet{
			SourceClient: "src-0", DestinationClient: "dst-0",
			Sequence: 1, TimeoutTimestamp: 100,
		}

		if tracker.Add(packet, 10) {
			t.Fatal("Add reported success after its durable mutation was rolled back")
		}
		if tracker.Len() != 0 {
			t.Fatalf("failed Add left %d packet(s) in memory, want 0", tracker.Len())
		}

		restored, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		if restored.Len() != 0 {
			t.Fatalf("failed Add left %d packet(s) on disk, want 0", restored.Len())
		}
	})

	t.Run("timeout retry persistence failure circuit breaks until recovery", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pending.json")
		tracker, err := NewPersistentPendingPacketTracker(path)
		if err != nil {
			t.Fatal(err)
		}
		tracker.Add(channeltypesv2.Packet{
			SourceClient: "src-0", DestinationClient: "dst-0", Sequence: 1,
			TimeoutTimestamp: uint64(time.Now().Add(-time.Minute).Unix()),
		}, 10)
		tracker.writeState = func(string, []byte) error { return errors.New("disk full") }

		prepareCalls := 0
		timeoutCalls := 0
		svc := New(nil, nil, DefaultConfig())
		scan := func() {
			svc.scanForEVMTimeouts(context.Background(), evmTimeoutDeps{}, evmTimeoutScanOptions{
				tag:     "Persistence",
				tracker: tracker,
				prepareTimeouts: func(context.Context, evmTimeoutDeps) (*client.LightBlock, bool) {
					prepareCalls++
					return &client.LightBlock{}, true
				},
				hasPendingCommitment: func(context.Context, evmTimeoutDeps, channeltypesv2.Packet) (bool, error) {
					return true, nil
				},
				timeoutSend: func(context.Context, evmTimeoutDeps, EthPacket, *client.LightBlock) timeoutSendOutcome {
					timeoutCalls++
					return timeoutDeferred
				},
			})
		}

		scan()
		if prepareCalls != 1 || timeoutCalls != 1 {
			t.Fatalf("initial timeout work = prepare:%d send:%d, want 1:1", prepareCalls, timeoutCalls)
		}
		if tracker.PersistenceError() == nil {
			t.Fatal("failed timeout backoff did not trip the circuit breaker")
		}
		scan()
		if prepareCalls != 1 || timeoutCalls != 1 {
			t.Fatalf("circuit-broken timeout work repeated = prepare:%d send:%d", prepareCalls, timeoutCalls)
		}

		tracker.writeState = replaceStateFile
		scan() // successful probe clears the breaker but deliberately holds this scan.
		if prepareCalls != 1 || timeoutCalls != 1 {
			t.Fatalf("recovery probe repeated timeout work = prepare:%d send:%d", prepareCalls, timeoutCalls)
		}
		if tracker.PersistenceError() != nil {
			t.Fatalf("successful persistence probe left circuit breaker open: %v", tracker.PersistenceError())
		}
		scan()
		if prepareCalls != 2 || timeoutCalls != 2 {
			t.Fatalf("timeout work did not resume after persistence recovery = prepare:%d send:%d", prepareCalls, timeoutCalls)
		}
		info := tracker.GetAll()
		if len(info) != 1 || info[0].Deferrals != 1 || info[0].NotBefore.IsZero() {
			t.Fatalf("recovered timeout backoff was not durable: %#v", info)
		}
	})
}

func TestDeferTimeoutRetriesIfCurrent_PersistsBatchOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	tracker, err := NewPersistentPendingPacketTracker(path)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 5; sequence++ {
		tracker.Add(channeltypesv2.Packet{
			SourceClient: "src-0", DestinationClient: "dst-0",
			Sequence: sequence, TimeoutTimestamp: 100,
		}, sequence)
	}

	writes := 0
	tracker.writeState = func(string, []byte) error {
		writes++
		return nil
	}
	counts, err := tracker.DeferTimeoutRetriesIfCurrent(tracker.GetAll(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("persist writes = %d, want 1 for the full deferral batch", writes)
	}
	for i, count := range counts {
		if count != 1 {
			t.Fatalf("deferral count[%d] = %d, want 1", i, count)
		}
	}
}

// currentPacketForTest lets unit tests exercise the identity-checked mutation
// APIs without keeping the unsafe key-only production methods around.
func (t *PendingPacketTracker) currentPacketForTest(sourceClient string, sequence uint64) (pendingPacketInfo, bool) {
	for _, info := range t.GetAll() {
		if info.Packet.SourceClient == sourceClient && info.Packet.Sequence == sequence {
			return info, true
		}
	}
	return pendingPacketInfo{}, false
}

func (t *PendingPacketTracker) removeCurrentForTest(sourceClient string, sequence uint64) {
	if info, ok := t.currentPacketForTest(sourceClient, sequence); ok {
		t.RemoveIfCurrent(info)
	}
}

func (t *PendingPacketTracker) recordTimeoutFailureForTest(sourceClient string, sequence uint64, now time.Time) bool {
	info, ok := t.currentPacketForTest(sourceClient, sequence)
	if !ok {
		return false
	}
	deadLettered, err := t.RecordTimeoutFailureIfCurrent(info, now)
	if err != nil {
		return false
	}
	return deadLettered
}

func (t *PendingPacketTracker) deferTimeoutRetryForTest(sourceClient string, sequence uint64, now time.Time) int {
	if info, ok := t.currentPacketForTest(sourceClient, sequence); ok {
		count, err := t.DeferTimeoutRetryIfCurrent(info, now)
		if err != nil {
			return 0
		}
		return count
	}
	return 0
}

func (t *PendingPacketTracker) clearDeferralsForTest(sourceClient string, sequence uint64) {
	if info, ok := t.currentPacketForTest(sourceClient, sequence); ok {
		t.ClearDeferralsIfCurrent(info)
	}
}

// trackerWith returns a tracker already holding one packet. trackedPacket (in
// timeout_retry_test.go) builds the packet itself.
func trackerWith(t *testing.T, seq uint64) (*PendingPacketTracker, channeltypesv2.Packet) {
	t.Helper()
	tracker := NewPendingPacketTracker()
	pkt := trackedPacket(seq)
	tracker.Add(pkt, 1)
	return tracker, pkt
}

func TestDeferTimeoutRetryIfCurrent(t *testing.T) {
	// TestDeferTimeoutRetryIfCurrent/never dead letters is the core of the first P1: a packet
	// must not be abandoned for an outage it had no part in.
	//
	// RecordTimeoutFailure was charged on every false return of timeoutEthSend,
	// including the two paths that never build or send anything — a failed
	// light-client update, and a counterparty whose clock has not reached the
	// timeout. A counterparty lagging across the ~90 minutes the backoff sums to
	// therefore dead-lettered a healthy packet, and a dead-lettered timeout means
	// escrowed funds that nothing will release.
	t.Run("never dead letters", func(t *testing.T) {
		t.Parallel()

		tracker, pkt := trackerWith(t, 7)
		now := time.Now()

		// Far more deferrals than maxTimeoutAttempts.
		for i := 0; i < maxTimeoutAttempts*3; i++ {
			tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now)
		}

		if got := tracker.DeadLetteredTimeouts(); got != 0 {
			t.Fatalf("deferrals dead-lettered %d packet(s); an outage must never abandon a packet", got)
		}
		if tracker.Len() != 1 {
			t.Fatal("the packet must stay tracked so it is retried once the outage clears")
		}
	})

	// A deferral still backs off — the RLY-06 cost problem is real whoever is at
	// fault. Without this the scanner re-runs a client update and a Groth16 proof
	// for every tracked packet every 30s for as long as the outage lasts.
	t.Run("still backs off", func(t *testing.T) {
		t.Parallel()

		tracker, pkt := trackerWith(t, 7)
		now := time.Now()

		tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now)
		if due := tracker.GetDue(now.Add(30 * time.Second)); len(due) != 0 {
			t.Fatal("a deferred packet must not come due on the next 30s scan")
		}
		if due := tracker.GetDue(now.Add(timeoutRetryBackoffBase + time.Second)); len(due) != 1 {
			t.Fatal("a deferred packet must come due once its backoff elapses")
		}
	})

	// Deferrals and attempts are counted separately: an outage must not consume the
	// budget that exists to catch a genuinely poisoned packet.
	t.Run("deferrals do not consume the attempt budget", func(t *testing.T) {
		t.Parallel()

		tracker, pkt := trackerWith(t, 7)
		now := time.Now()

		for i := 0; i < maxTimeoutAttempts; i++ {
			tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now)
		}
		// The full attempt budget must still be available afterwards.
		for i := 0; i < maxTimeoutAttempts; i++ {
			if tracker.recordTimeoutFailureForTest(pkt.SourceClient, pkt.Sequence, now) {
				t.Fatalf("dead-lettered on attributable attempt %d of %d; deferrals ate the budget", i+1, maxTimeoutAttempts)
			}
		}
		if !tracker.recordTimeoutFailureForTest(pkt.SourceClient, pkt.Sequence, now) {
			t.Fatal("the attempt budget must still terminate at maxTimeoutAttempts")
		}
	})
}

// TestDeadLetteredTimeouts_AreInspectable covers the RLY-02 half: the set used to
// be countable but not inspectable, so the packet identities behind the count
// existed only in log lines — unlike the batch dead letters, which have
// DrainDeadLetters/OldestDeadLetter.
func TestDeadLetteredTimeouts_AreInspectable(t *testing.T) {
	t.Parallel()

	tracker, pkt := trackerWith(t, 42)
	deadAt := time.Now()
	for i := 0; i <= maxTimeoutAttempts; i++ {
		tracker.recordTimeoutFailureForTest(pkt.SourceClient, pkt.Sequence, deadAt)
	}
	if tracker.DeadLetteredTimeouts() != 1 {
		t.Fatalf("expected the packet to be dead-lettered, got %d", tracker.DeadLetteredTimeouts())
	}

	if age := tracker.OldestDeadLetteredTimeout(deadAt.Add(2 * time.Hour)); age < 2*time.Hour {
		t.Fatalf("age = %s, want at least 2h: a count says a timeout gave up, the age says nobody has looked", age)
	}

	packets := tracker.DeadLetteredTimeoutPackets()
	if len(packets) != 1 || packets[0].Sequence != 42 || packets[0].SourceClient != pkt.SourceClient {
		t.Fatalf("the listing must identify which packet gave up (client and sequence), got %+v", packets)
	}

	if again := tracker.DeadLetteredTimeoutPackets(); len(again) != 1 || again[0].Sequence != pkt.Sequence {
		t.Fatalf("reading must not silence the timeout alarm, got %+v", again)
	}
}

// The stuck gauge exists because deferrals never dead-letter: without it a
// packet at the backoff cap reads as zero on every other counter.
func TestStuckTimeouts_CountsOnlyPastTheThreshold(t *testing.T) {
	t.Parallel()

	tracker, pkt := trackerWith(t, 29)
	now := time.Now()

	for i := 1; i < deferralAlertThreshold; i++ {
		tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now)
		if count, _ := tracker.StuckTimeouts(); count != 0 {
			t.Fatalf("deferral %d already counted as stuck", i)
		}
	}
	tracker.deferTimeoutRetryForTest(pkt.SourceClient, pkt.Sequence, now)
	if count, worst := tracker.StuckTimeouts(); count != 1 || worst != deferralAlertThreshold {
		t.Fatalf("StuckTimeouts() = (%d, %d), want (1, %d)", count, worst, deferralAlertThreshold)
	}
}

func TestRecordTimeoutFailureIfCurrent(t *testing.T) {
	// A failed timeout submission must hold the packet out of the next scans, so the
	// scanner stops re-proving it every 30 seconds.
	t.Run("holds packet out of scan", func(t *testing.T) {
		tr := NewPendingPacketTracker()
		tr.Add(trackedPacket(1), 100)

		now := time.Now()
		if got := len(tr.GetDue(now)); got != 1 {
			t.Fatalf("a fresh packet must be due, got %d", got)
		}

		if dead := tr.recordTimeoutFailureForTest("client-0", 1, now); dead {
			t.Fatal("one failure must not dead-letter")
		}
		if got := len(tr.GetDue(now)); got != 0 {
			t.Fatalf("packet under backoff must not be re-scanned, got %d due", got)
		}
		// It comes back once the backoff elapses.
		if got := len(tr.GetDue(now.Add(2 * time.Minute))); got != 1 {
			t.Fatalf("packet must be due again after its backoff, got %d", got)
		}
	})

	// After maxTimeoutAttempts the packet stops being retried and is kept for an
	// operator. It must leave the scan set — otherwise the cap achieves nothing.
	t.Run("dead letters at cap", func(t *testing.T) {
		tr := NewPendingPacketTracker()
		tr.Add(trackedPacket(1), 100)

		now := time.Now()
		for i := 1; i <= maxTimeoutAttempts; i++ {
			if dead := tr.recordTimeoutFailureForTest("client-0", 1, now); dead {
				t.Fatalf("attempt %d dead-lettered early (cap is %d)", i, maxTimeoutAttempts)
			}
		}
		if dead := tr.recordTimeoutFailureForTest("client-0", 1, now); !dead {
			t.Fatalf("attempt %d must dead-letter", maxTimeoutAttempts+1)
		}
		if tr.Len() != 0 {
			t.Fatalf("a dead-lettered packet must leave the scan set, %d still tracked", tr.Len())
		}
		if got := tr.DeadLetteredTimeouts(); got != 1 {
			t.Fatalf("DeadLetteredTimeouts = %d, want 1 — escrowed funds must stay visible", got)
		}
	})

	// Recording against a packet that is no longer tracked (it succeeded, or was
	// purged) must be a no-op rather than resurrecting it.
	t.Run("ignores unknown packet", func(t *testing.T) {
		tr := NewPendingPacketTracker()
		if dead := tr.recordTimeoutFailureForTest("client-0", 99, time.Now()); dead {
			t.Fatal("unknown packet must not be reported as dead-lettered")
		}
		if tr.Len() != 0 || tr.DeadLetteredTimeouts() != 0 {
			t.Fatal("unknown packet must not be added to any set")
		}
	})
}

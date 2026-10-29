package services

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

func ackDuePacket(seq uint64) channeltypesv2.Packet {
	return channeltypesv2.Packet{SourceClient: "source", DestinationClient: "destination", Sequence: seq}
}

func ackDueServices(t *testing.T) *Services {
	t.Helper()
	svc, err := NewWithPendingState(nil, nil, DefaultConfig(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// failWrites makes every durable write fail until the returned func restores it.
func failWrites(t *testing.T, svc *Services) func() {
	t.Helper()
	tracker := svc.BatchBuilder.AckDueTracker
	original := tracker.writeState
	tracker.writeState = func(string, []byte) error { return errors.New("disk full") }
	return func() { tracker.writeState = original }
}

func TestAckDueLedger(t *testing.T) {
	// The ledger is shared between the two relay modules because the two halves of
	// one debt land on different ones: cosmos->eth delivers the receive that owes
	// the ack, eth->cosmos relays the ack that settles it. Each subtest below is a
	// case that a per-module retry queue got wrong.

	t.Run("a record that could not be written is retried", func(t *testing.T) {
		svc := ackDueServices(t)
		restore := failWrites(t, svc)

		if svc.RecordAckDue(ackDuePacket(7)) {
			t.Fatal("a failed durable write must be reported as failed")
		}
		if got := svc.PendingAckDueWrites(); got != 1 {
			t.Fatalf("ledger holds %d, want the failed record retained", got)
		}

		restore()
		// Any later ack-due call, from either module, drains the ledger.
		svc.RecordAckDue(ackDuePacket(8))

		if got := svc.PendingAckDueWrites(); got != 0 {
			t.Fatalf("ledger still holds %d after storage recovered", got)
		}
		if got := len(svc.OverdueAcks(0)); got != 2 {
			t.Fatalf("tracker holds %d debts, want both seq=7 and seq=8", got)
		}
	})

	// The bug this closes: the retry queue used to live on the module that
	// DELIVERED the receive, while the acknowledgement settles on the other one.
	// A settlement therefore could not cancel a record still queued, and the next
	// batch on the delivering side recreated a debt whose ack was already home --
	// an overdue alert that fires forever and can never be satisfied.
	t.Run("a settlement cancels a record still queued for writing", func(t *testing.T) {
		svc := ackDueServices(t)
		restore := failWrites(t, svc)

		svc.RecordAckDue(ackDuePacket(7)) // fails, queued

		// The acknowledgement comes back and is relayed by the OTHER module while
		// storage is STILL down -- so the queued record is not merely flushed on
		// the way past, it has to be actively cancelled. Restoring writes before
		// this line hides the bug: the flush at the top of ClearAckDue would write
		// the debt and the removal right after would take it straight back out.
		svc.ClearAckDue(ackDuePacket(7))

		// Storage recovers, and a later delivery on the recording side drains the
		// ledger. Nothing must resurrect the settled debt.
		restore()
		svc.RecordAckDue(ackDuePacket(8))

		for _, name := range svc.OverdueAcks(0) {
			if name == "seq=7 src=source" {
				t.Fatal("the settled acknowledgement was recreated by the retry; it can never be cleared again")
			}
		}
		if got := svc.PendingAckDueWrites(); got != 0 {
			t.Fatalf("ledger holds %d, want the cancelled record gone", got)
		}
	})

	// The other half: RemovePacketIfCurrent rolls its deletion back when the write
	// fails, and the ack relay itself succeeded, so no event brings us back here.
	// Left alone, awaiting-acks.json keeps a debt that alerts forever.
	t.Run("a removal the tracker rolled back is retried", func(t *testing.T) {
		svc := ackDueServices(t)
		if !svc.RecordAckDue(ackDuePacket(7)) {
			t.Fatal("the initial record should persist")
		}

		restore := failWrites(t, svc)
		svc.ClearAckDue(ackDuePacket(7))

		if len(svc.OverdueAcks(0)) != 1 {
			t.Fatal("a failed removal must roll back; the test is not exercising the case")
		}
		if got := svc.PendingAckDueWrites(); got != 1 {
			t.Fatalf("ledger holds %d, want the failed removal queued", got)
		}

		restore()
		svc.RecordAckDue(ackDuePacket(8)) // any ack-due call drains the ledger

		for _, name := range svc.OverdueAcks(0) {
			if name == "seq=7 src=source" {
				t.Fatal("the rolled-back removal was never retried; seq=7 alerts forever")
			}
		}
		if got := svc.PendingAckDueWrites(); got != 0 {
			t.Fatalf("ledger holds %d after storage recovered", got)
		}
	})

	t.Run("a settled debt is not reported as overdue", func(t *testing.T) {
		svc := ackDueServices(t)
		svc.RecordAckDue(ackDuePacket(7))
		svc.ClearAckDue(ackDuePacket(7))

		if got := svc.OverdueAcks(0); len(got) != 0 {
			t.Fatalf("overdue = %v, want none after the acknowledgement was relayed", got)
		}
	})

	t.Run("an owed debt is reported once it is old enough", func(t *testing.T) {
		svc := ackDueServices(t)
		svc.RecordAckDue(ackDuePacket(7))

		if got := svc.OverdueAcks(time.Hour); len(got) != 0 {
			t.Fatalf("overdue = %v, want none below the threshold", got)
		}
		if got := svc.OverdueAcks(0); len(got) != 1 || got[0] != "seq=7 src=source" {
			t.Fatalf("overdue = %v, want the owed packet named", got)
		}
	})
}

func TestRecordAckDue(t *testing.T) {
	// The ack-watch tick is the only clock this bookkeeping has. Without it a debt
	// whose write failed is retried only when the next packet moves -- so a relayer
	// that goes idle while storage is down, and whose acknowledgement never comes
	// back, keeps the record in RAM and loses it at the next restart. That restart
	// is exactly what the record exists to survive.
	t.Run("retries on the watch tick while idle", func(t *testing.T) {
		svc := ackDueServices(t)
		restore := failWrites(t, svc)

		if svc.RecordAckDue(ackDuePacket(7)) {
			t.Fatal("the write was supposed to fail")
		}
		restore() // storage comes back, and nothing else happens on this relayer

		// The ack-watch loop calls OverdueAcks on its timer. That is the tick.
		svc.OverdueAcks(time.Hour)

		if got := svc.PendingAckDueWrites(); got != 0 {
			t.Fatalf("ledger still holds %d after the watch tick; an idle relayer never writes it and a restart loses it", got)
		}
		if got := svc.OverdueAcks(0); len(got) != 1 || got[0] != "seq=7 src=source" {
			t.Fatalf("overdue = %v, want the debt now recorded durably", got)
		}
	})

	// A client migration keeps the client id and restarts sequences from 1, so one
	// (client, sequence) slot names two different packets over its life. The ledger
	// used to key on that slot alone: a queued write for the OLD packet was replayed
	// ahead of the ACK for the new one, RemovePacketIfCurrent then no-opped on the
	// identity mismatch -- returning nil, which reads as success -- and the old debt
	// was left durable with nothing queued to settle it, alerting forever.
	t.Run("does not replay a stale intent into a reused slot", func(t *testing.T) {
		svc := ackDueServices(t)

		oldPacket := ackDuePacket(7)
		oldPacket.TimeoutTimestamp = 1000
		newPacket := ackDuePacket(7) // same client, same sequence, after a migration
		newPacket.TimeoutTimestamp = 2000

		restore := failWrites(t, svc)
		if svc.RecordAckDue(oldPacket) {
			t.Fatal("the write was supposed to fail")
		}
		restore()

		// The acknowledgement that comes back belongs to the NEW packet.
		svc.ClearAckDue(newPacket)

		if got := svc.OverdueAcks(0); len(got) != 0 {
			t.Fatalf("overdue = %v; the pre-migration debt was written by the retry and can never be settled", got)
		}
		if got := svc.PendingAckDueWrites(); got != 0 {
			t.Fatalf("ledger holds %d, want the superseded intent dropped", got)
		}
	})

	// The half of the migration case that ordering does not fix: the stale debt is
	// already ON DISK, written successfully before the client was recreated.
	//
	// RemovePacketIfCurrent no-ops when the slot holds a superseded identity, and it
	// returns nil -- which reads as success -- so the settlement of the packet that
	// replaced it left the old debt in awaiting-acks.json with nothing able to
	// remove it. No further acknowledgement can ever name it: the packet it belonged
	// to no longer exists on the source.
	t.Run("settlement clears a superseded debt in the same slot", func(t *testing.T) {
		svc := ackDueServices(t)

		oldPacket := ackDuePacket(7)
		oldPacket.TimeoutTimestamp = 1000
		newPacket := ackDuePacket(7) // same client, same sequence, after a migration
		newPacket.TimeoutTimestamp = 2000

		if !svc.RecordAckDue(oldPacket) {
			t.Fatal("the pre-migration debt should persist")
		}
		if got := svc.OverdueAcks(0); len(got) != 1 {
			t.Fatalf("overdue = %v, want the pre-migration debt on disk; the fixture is wrong otherwise", got)
		}

		// Another relayer delivered the post-migration packet, so its acknowledgement
		// arrives without this process ever having recorded a debt for it.
		svc.ClearAckDue(newPacket)

		if got := svc.OverdueAcks(0); len(got) != 0 {
			t.Fatalf("overdue = %v; the superseded debt outlived the settlement and is reported for the life of the state file", got)
		}
	})
}

// The multi-relayer case, which is the ordinary one: this process delivers the
// receive and records the debt, ANOTHER process submits the acknowledgement, and
// this one only ever observes the terminal event.
//
// Reported by @DongLieu: the ledger was cleared only when this process relayed
// the AckPacket itself. The three terminal paths removed the pending record and
// stopped, so the debt stayed -- durable, surviving restart, and reported overdue
// for a packet already settled on-chain. It can never time out either, because
// the receive left a receipt. A watcher that names settled packets is worse than
// no watcher: the real overdue entry becomes indistinguishable from the noise.
func TestSettleOwedAck_ClosesADebtAnotherRelayerAcknowledged(t *testing.T) {
	svc := New(nil, nil, Config{})
	pkt := channeltypesv2.Packet{SourceClient: "client-0", Sequence: 7, TimeoutTimestamp: 1}

	if !svc.RecordAckDue(pkt) {
		t.Fatal("the debt was not recorded; the rest of this test would prove nothing")
	}
	if got := len(svc.BatchBuilder.AckDueTracker.GetAll()); got != 1 {
		t.Fatalf("ledger holds %d entries, want 1", got)
	}

	// Exactly what a terminal-event path does: it never relayed the ack itself.
	svc.BatchBuilder.SettleOwedAck(pkt)

	if got := len(svc.BatchBuilder.AckDueTracker.GetAll()); got != 0 {
		t.Fatalf("ledger still holds %d entries after the acknowledgement arrived; "+
			"the packet is settled on-chain and would be reported overdue forever", got)
	}
	if overdue := svc.OverdueAcks(0); len(overdue) != 0 {
		t.Fatalf("watcher still names %v as overdue", overdue)
	}
}

// The hook has to be installed by the constructors, or every terminal path calls
// a nil and settles nothing. This is the wiring half, and wiring is what the
// suite does not otherwise hold.
func TestServices_ConstructorsInstallTheOwedAckSettler(t *testing.T) {
	t.Run("New", func(t *testing.T) {
		svc := New(nil, nil, Config{})
		if svc.BatchBuilder.settleOwedAck == nil {
			t.Fatal("New left the settler nil; every terminal acknowledgement would leave its debt behind")
		}
	})

	t.Run("NewWithPendingState", func(t *testing.T) {
		svc, err := NewWithPendingState(nil, nil, Config{}, t.TempDir())
		if err != nil {
			t.Fatalf("NewWithPendingState: %v", err)
		}
		if svc.BatchBuilder.settleOwedAck == nil {
			t.Fatal("NewWithPendingState left the settler nil, which is the durable path")
		}
	})
}

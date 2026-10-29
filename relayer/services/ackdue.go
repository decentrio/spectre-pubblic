// This file owns the owed-acknowledgement bookkeeping: the record a delivered
// receive creates, the settlement a relayed acknowledgement closes, the overdue
// report, and the retry ledger that survives a failed durable write.
//
// It lives beside the shared AckDueTracker rather than in relay.Module because
// the two halves of one debt land on DIFFERENT modules: cosmos->eth delivers the
// receive that owes the acknowledgement, and eth->cosmos relays the
// acknowledgement that settles it. Retry state owned by one module cannot be
// cancelled by the other -- which is exactly how a failed write, retried after
// the acknowledgement had already come back, recreated a debt nothing would ever
// clear.
package services

import (
	"fmt"
	"log"
	"sync"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// ackDueIntent is the state the tracker must reach for one packet.
//
// One entry per packet, last write wins. That is the point: a settlement
// arriving while a record is still queued CANCELS the record instead of racing
// it, so the debt cannot be resurrected after the acknowledgement is home.
type ackDueIntent struct {
	packet channeltypesv2.Packet
	// settle false = the debt must be written; true = it must be removed.
	settle bool
}

// ackDueLedger holds the ack-due writes that did not reach disk.
//
// Its mutex is the single serialization point for ALL ack-due bookkeeping: every
// exported method below takes it and holds it across the tracker calls too. That
// costs nothing (these run once per delivered packet) and removes the whole
// class of races between the two module goroutines that share this ledger.
//
// Lock order is ledger -> tracker, and nothing takes the tracker first, so there
// is no cycle. The zero value is usable; the map is created on first write.
type ackDueLedger struct {
	mu sync.Mutex
	// pending is keyed by the (client, sequence) SLOT, which is what the tracker
	// can hold one of. Keying by full packet identity was tried and removed: two
	// intents for one slot can never both be written, forgetSlotLocked drops the
	// slot rather than one identity, and no test could tell the two keyings
	// apart. What actually prevents a stale write is the ORDER below, not a
	// finer key.
	pending map[packetKey]ackDueIntent
}

func ackDueSlot(packet channeltypesv2.Packet) packetKey {
	return packetKey{sourceClient: packet.SourceClient, sequence: packet.Sequence}
}

// wantLocked records the state this packet must reach, replacing any earlier one
// for the SAME packet.
func (l *ackDueLedger) wantLocked(packet channeltypesv2.Packet, settle bool) {
	if l.pending == nil {
		l.pending = make(map[packetKey]ackDueIntent)
	}
	l.pending[ackDueSlot(packet)] = ackDueIntent{packet: packet, settle: settle}
}

// forgetSlotLocked drops every intent for this packet's (client, sequence) slot,
// not just the one matching its identity.
//
// The tracker can hold exactly one entry per slot, so two live debts there are
// not representable and a settlement for the slot supersedes any write still
// queued for it. Cancelling only the exact identity leaves the OTHER one to be
// replayed afterwards, and because RemovePacketIfCurrent no-ops on an identity
// mismatch -- returning nil, which reads as success -- that replayed debt is
// written durably with nothing left to settle it, and the overdue watcher names
// it forever. A client migration restarting sequences at 1 is what produces two
// identities in one slot.
func (l *ackDueLedger) forgetSlotLocked(packet channeltypesv2.Packet) {
	delete(l.pending, ackDueSlot(packet))
}

// RecordAckDue notes that a receive relay succeeded and its acknowledgement is
// now owed. False means the durable write failed; the debt is remembered here
// and retried on the next ack-due call from either module.
//
// It exists because waitTracker only follows packets it OBSERVED: an
// acknowledgement written while the relayer was down, or outside the scan
// window, produces no event to wait on, so a missing ack is indistinguishable
// from one that simply has not arrived. This record is what makes it nameable.
func (s *Services) RecordAckDue(packet channeltypesv2.Packet) bool {
	s.ackDue.mu.Lock()
	defer s.ackDue.mu.Unlock()
	// Clear this slot BEFORE replaying anything. A stale intent for the same
	// (client, sequence) must not be written ahead of the operation that is
	// happening now; see forgetSlotLocked.
	s.ackDue.forgetSlotLocked(packet)
	s.retryAckDueLocked()

	if s.BatchBuilder.AckDueTracker.Add(packet, 0) {
		return true
	}
	s.ackDue.wantLocked(packet, false)
	return false
}

// ClearAckDue closes the record once the acknowledgement is relayed.
//
// Two failures are handled here, and both used to end as an alert that never
// stops firing. A record still queued for writing is DROPPED rather than
// written: the acknowledgement is already home, so recording the debt now would
// create one nothing can clear. And a removal the tracker rolled back is queued
// for retry, because the acknowledgement relay itself succeeded -- no event will
// bring us back here on its own.
func (s *Services) ClearAckDue(packet channeltypesv2.Packet) {
	s.ackDue.mu.Lock()
	defer s.ackDue.mu.Unlock()
	// Order matters: cancel the slot first, then replay the rest. Replaying
	// first would write a stale same-slot debt that this settlement can no
	// longer remove.
	s.ackDue.forgetSlotLocked(packet)
	s.retryAckDueLocked()

	if !s.removeAckDueLocked(packet) {
		s.ackDue.wantLocked(packet, true)
	}
}

// removeAckDueLocked removes the record and reports whether the removal reached
// disk. A false return means the tracker rolled the deletion back, so the debt
// is still recorded and has to be retried -- the acknowledgement relay itself
// succeeded, and nothing will bring us back here on its own.
func (s *Services) removeAckDueLocked(packet channeltypesv2.Packet) bool {
	// By SLOT, not by identity. RemovePacketIfCurrent no-ops when the slot holds a
	// superseded identity -- and returns nil, which reads as success -- so a debt
	// recorded before a client migration survived the acknowledgement of the
	// packet that replaced it and was reported overdue for the life of the state
	// file. Nothing else could ever settle it: the packet it belonged to no longer
	// exists on the source. See RemoveSlotIfPresent.
	if _, err := s.BatchBuilder.AckDueTracker.RemoveSlotIfPresent(packet.SourceClient, packet.Sequence); err != nil {
		log.Printf("ERROR [AckWatch][ATTENTION] failed to persist settlement of owed acknowledgement seq=%d: %v; queued for retry",
			packet.Sequence, err)
		return false
	}
	return true
}

// retryAckDueLocked re-applies the intents whose durable write failed before.
//
// It is driven by the ack-due calls AND by the ack-watch tick through
// OverdueAcks. The calls alone are not enough: they only happen while packets
// are moving, and the case that matters most is storage recovering while the
// relayer is idle and the acknowledgement never comes back. Left to the calls,
// that debt stays in RAM until the process restarts and then vanishes -- which
// is precisely the restart this record exists to survive.
func (s *Services) retryAckDueLocked() {
	for key, intent := range s.ackDue.pending {
		var done bool
		if intent.settle {
			done = s.removeAckDueLocked(intent.packet)
		} else {
			done = s.BatchBuilder.AckDueTracker.Add(intent.packet, 0)
		}
		if done {
			delete(s.ackDue.pending, key)
		}
	}
}

// PendingAckDueWrites is the number of ack-due records that have not reached
// disk. It is reported rather than acted on: a durable write that keeps failing
// is a storage problem, and the count is what says whether it is one packet or
// every packet.
func (s *Services) PendingAckDueWrites() int {
	s.ackDue.mu.Lock()
	defer s.ackDue.mu.Unlock()
	return len(s.ackDue.pending)
}

// OverdueAcks names the packets whose acknowledgement has been owed longer than
// threshold, oldest first. It returns rendered identities rather than packets
// because the caller reports them: the sequence and the client are what an
// operator needs, and handing back packets would invite the report to grow into
// a second decision point.
func (s *Services) OverdueAcks(threshold time.Duration) []string {
	// Flush before reporting. This runs on the ack-watch tick, which is the only
	// clock this bookkeeping has: without it a debt whose write failed is retried
	// only when the next packet moves, so an idle relayer whose storage came back
	// keeps the record in RAM and loses it at the next restart. Reporting what is
	// overdue is also exactly the moment to make sure what we know is on disk.
	s.ackDue.mu.Lock()
	s.retryAckDueLocked()
	s.ackDue.mu.Unlock()

	due := s.BatchBuilder.AckDueTracker.OlderThan(threshold)
	names := make([]string, 0, len(due))
	for _, packet := range due {
		names = append(names, fmt.Sprintf("seq=%d src=%s", packet.Sequence, packet.SourceClient))
	}
	return names
}

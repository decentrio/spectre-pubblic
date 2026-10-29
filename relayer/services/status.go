package services

import (
	"fmt"
	"log"
	"sort"
	"time"
)

// QueueStatus is the queue half of a status answer: packets the relayer has
// ALREADY relayed and is now waiting to see settled, plus the ones it gave up on.
//
// It is the counterpart to relay.Status's waiting set, and the pair is the split
// an operator cannot make today: "not relayable yet, and here is the gate" versus
// "relayed, awaiting delivery or a timeout refund". A commitment surviving on the
// source says the round trip is incomplete; it does not say which half.
type QueueStatus struct {
	// Path names the engine these trackers belong to. It is stamped by whoever
	// registers the engine, because a services instance does not know what the
	// process decided to call it.
	//
	// It exists because Direction alone does not identify a queue. Reported by
	// @neitdung: reactivation is a WRITE, and a write must name exactly one
	// tracker -- client ids are chain-local, so nothing stops two trackers from
	// holding the same (source_client, sequence).
	Path string `json:"path,omitempty"`
	// Direction names the tracker, not the chain pair: "cosmos" holds
	// Cosmos-origin sends, "eth" ETH-origin, "l2" L2-origin.
	Direction string `json:"direction"`
	// Queued is how many packets are batched but not yet submitted.
	Queued int `json:"queued"`
	// Pending is relayed-and-awaiting-settlement, oldest first.
	Pending []PendingEntry `json:"pending"`
	// DeadLettered stopped being retried. Non-zero means escrowed funds are
	// waiting on a person.
	DeadLettered []DeadLetterEntry `json:"dead_lettered"`
	// RetryPaused reports the tracker's fail-closed circuit breaker: its durable
	// state could not be written, so timeout work is held rather than run against
	// state that may not survive a restart.
	RetryPaused       bool   `json:"retry_paused"`
	RetryPausedReason string `json:"retry_paused_reason,omitempty"`
}

// PendingEntry is one packet awaiting settlement, with the age that separates a
// normal wait from a stuck one.
type PendingEntry struct {
	SourceClient string        `json:"source_client"`
	Sequence     uint64        `json:"sequence"`
	BlockNumber  uint64        `json:"block_number"`
	Age          time.Duration `json:"age_ns"`
	AgeText      string        `json:"age"`
	// TimeoutAttempts is how many timeout submissions have already failed for
	// this packet. A climbing count on a packet that is not moving is the shape
	// of a deterministic failure walking its way to the dead-letter set.
	TimeoutAttempts int `json:"timeout_attempts"`
}

// QueueStatus snapshots all three trackers. Read-only and safe from any
// goroutine; each tracker takes its own lock briefly.
func (s *Services) QueueStatus(now time.Time) []QueueStatus {
	cosmosQueued, ethQueued := s.BatchBuilder.QueueDepths()
	return []QueueStatus{
		trackerStatus(DirectionCosmos, cosmosQueued, s.BatchBuilder.PendingTracker, now),
		trackerStatus(DirectionEth, ethQueued, s.BatchBuilder.EthPendingTracker, now),
		// The L2 tracker shares the Cosmos-bound queue, so it has no depth of its
		// own to report. Saying 0 would read as "nothing queued" rather than "not
		// a separate queue"; the pending and dead-lettered sets are the real answer
		// for this direction.
		trackerStatus(DirectionL2, 0, s.BatchBuilder.L2PendingTracker, now),
	}
}

func trackerStatus(direction string, queued int, t *PendingPacketTracker, now time.Time) QueueStatus {
	out := QueueStatus{Direction: direction, Queued: queued}
	if t == nil {
		return out
	}
	for _, info := range t.GetAll() {
		age := now.Sub(info.ObservedAt)
		out.Pending = append(out.Pending, PendingEntry{
			SourceClient:    info.Packet.SourceClient,
			Sequence:        info.Packet.Sequence,
			BlockNumber:     info.BlockNumber,
			Age:             age,
			AgeText:         age.Round(time.Second).String(),
			TimeoutAttempts: info.TimeoutAttempts,
		})
	}
	sortByAgeDescending(out.Pending)
	out.DeadLettered = t.DeadLetteredTimeoutPackets()
	if err := t.PersistenceError(); err != nil {
		out.RetryPaused, out.RetryPausedReason = true, err.Error()
	}
	return out
}

// sortByAgeDescending puts the oldest first: the entry most likely to be stuck
// is the one an operator should read first, and a truncated list must not hide it.
func sortByAgeDescending(entries []PendingEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Age != entries[j].Age {
			return entries[i].Age > entries[j].Age
		}
		return entries[i].Sequence < entries[j].Sequence
	})
}

// Direction names for the three trackers, and the values an operator types.
// They are the same strings QueueStatus reports, so what `relayer status` prints
// is what `status reactivate` accepts.
const (
	DirectionCosmos = "cosmos"
	DirectionEth    = "eth"
	DirectionL2     = "l2"
)

// ReactivateDeadLetter returns one dead-lettered packet to the timeout scan set,
// in the tracker the caller NAMES.
//
// The direction is required, and that is the fix. Reported by @neitdung: this
// used to try cosmos, then eth, then l2 and return on the first success. A
// packet identity is (source client, sequence) and client ids are chain-local,
// so two trackers can legitimately hold the same pair -- and an operator acting
// on the second entry `relayer status` printed would silently give a fresh
// retry budget to the first, leaving the packet they meant dead-lettered. For a
// state-changing control path, fixed scan order is not a tie-breaker.
//
// An unknown direction is refused rather than treated as "search everything",
// which would restore exactly the behaviour above.
func (s *Services) ReactivateDeadLetter(direction, sourceClient string, sequence uint64) error {
	var tracker *PendingPacketTracker
	switch direction {
	case DirectionCosmos:
		tracker = s.BatchBuilder.PendingTracker
	case DirectionEth:
		tracker = s.BatchBuilder.EthPendingTracker
	case DirectionL2:
		tracker = s.BatchBuilder.L2PendingTracker
	default:
		return fmt.Errorf("unknown direction %q: name one of %s, %s, %s as printed by `relayer status`",
			direction, DirectionCosmos, DirectionEth, DirectionL2)
	}
	if tracker == nil {
		return fmt.Errorf("this relay path has no %s tracker", direction)
	}
	if err := tracker.ReactivateDeadLetteredTimeout(sourceClient, sequence); err != nil {
		return fmt.Errorf("%s: %w", direction, err)
	}
	log.Printf("[PendingTracker] %s/%d reactivated by an operator: back in the %s timeout scan set with a fresh retry budget",
		sourceClient, sequence, direction)
	return nil
}

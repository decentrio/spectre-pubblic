package services

import (
	"errors"
	"log"
	"time"

	client "relayer/client"
)

type timeoutSendOutcome int

const (
	timeoutSent timeoutSendOutcome = iota
	timeoutNotDue
	timeoutDeferred
	timeoutFailed
	timeoutAlreadyReceived
)

func (o timeoutSendOutcome) packetIsDone() bool {
	return o == timeoutSent || o == timeoutAlreadyReceived
}

// timeoutOutcomeForError charges only an error that proves this packet's
// transaction was included and deterministically reverted. RPC, broadcast,
// proof-endpoint, and receipt-wait failures are infrastructure deferrals.
func timeoutOutcomeForError(err error) timeoutSendOutcome {
	if errors.Is(err, client.ErrPacketAlreadyReceived) {
		return timeoutAlreadyReceived
	}
	if errors.Is(err, ErrPermanentRelayFailure) {
		return timeoutFailed
	}
	return timeoutDeferred
}

// deferTimeoutRetries returns false when the state transition was not durable.
// Callers must stop this scan: the tracker has entered its fail-closed circuit
// breaker and a later scan will first verify that persistence recovered.
func deferTimeoutRetries(tracker *PendingPacketTracker, pending []pendingPacketInfo, now time.Time, tag string) bool {
	deferralCounts, err := tracker.DeferTimeoutRetriesIfCurrent(pending, now)
	if err != nil {
		log.Printf("ERROR [%sTimeoutScan][ATTENTION] timeout retry state was not durable: %v; pausing timeout work", tag, err)
		return false
	}
	for i, info := range pending {
		deferrals := deferralCounts[i]
		if deferralIsStuck(deferrals) {
			log.Printf("ERROR [%sTimeoutScan] STUCK: src=%s seq=%d has deferred timeout %d times; funds remain escrowed", tag, info.Packet.SourceClient, info.Packet.Sequence, deferrals)
		}
	}
	return true
}

func chargeTimeoutFailure(tracker *PendingPacketTracker, info pendingPacketInfo, now time.Time, tag string) bool {
	deadLettered, err := tracker.RecordTimeoutFailureIfCurrent(info, now)
	if err != nil {
		log.Printf("ERROR [%sTimeoutScan][ATTENTION] timeout attempt state was not durable: %v; pausing timeout work", tag, err)
		return false
	}
	if deadLettered {
		log.Printf("ERROR [%sTimeoutScan][ATTENTION] src=%s seq=%d exhausted timeout attempts; escrowed funds require operator action", tag, info.Packet.SourceClient, info.Packet.Sequence)
	}
	return true
}

func applyTimeoutOutcome(tracker *PendingPacketTracker, info pendingPacketInfo, outcome timeoutSendOutcome, now time.Time, tag string) bool {
	switch {
	case outcome.packetIsDone():
		if err := tracker.RemoveIfCurrent(info); err != nil {
			log.Printf("ERROR [%sTimeoutScan][ATTENTION] completed timeout state was not durable: %v; pausing timeout work", tag, err)
			return false
		}
		return true
	case outcome == timeoutNotDue:
		// The counterparty clock has not reached the packet deadline. This is not
		// chargeable, but leaving it immediately due makes every scan repeat the
		// shared client update and proof preparation while the clock is lagging.
		return deferTimeoutRetries(tracker, []pendingPacketInfo{info}, now, tag)
	case outcome == timeoutDeferred:
		return deferTimeoutRetries(tracker, []pendingPacketInfo{info}, now, tag)
	case outcome == timeoutFailed:
		return chargeTimeoutFailure(tracker, info, now, tag)
	}
	return true
}

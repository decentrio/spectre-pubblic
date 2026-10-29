package l2rollup

import (
	"context"
	"fmt"
	"log"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"

	"relayer/chain"
	"relayer/services"
	"relayer/subscriber"
)

// CosmosPacketState answers what the Cosmos side already records about one packet.
// It is the L2 source's half of the reconciliation the other three relay paths
// have had all along: an event found by scanning blocks says only that something
// happened on the L2, never whether the work it implies is still outstanding.
//
// Consumer-defined here (like AttestorClient) rather than imported from
// subscriber, so the scan loop can be driven in tests without a Cosmos node.
type CosmosPacketState interface {
	// HasPacketReceipt reports whether the packet has already been received on
	// Cosmos. Same question Destination.HasPacketReceipt answers, asked of a
	// decoded packet.
	HasPacketReceipt(ctx context.Context, packet channeltypesv2.Packet) (bool, error)
	// HasPacketCommitment reports whether Cosmos still stores the packet's
	// commitment. False means it was already acknowledged or timed out there.
	HasPacketCommitment(ctx context.Context, packet channeltypesv2.Packet) (bool, error)
}

// reconcileTimeout bounds one packet's pair of ABCI queries. The scan loop must
// not stall behind an unresponsive Cosmos RPC: reconciliation is an optimisation,
// and a slow one is worse than none.
const reconcileTimeout = 10 * time.Second

// l2ReconcileInterval is how often events the handler re-queued are re-checked
// against Cosmos. Re-queued events are the ones a second relayer can settle
// underneath us, so they need re-checking; doing it every poll tick would issue
// two ABCI queries per pending packet every few seconds for no gain.
//
// NOTE for B2: unlike the subscribers' gap-recovery tick (recoveryTick in
// relayer/subscriber/blockrate.go, which is derived from the measured block
// time), this interval is fixed. Derive it the same way.
var l2ReconcileInterval = 30 * time.Second

// NewCosmosPacketState reads packet state straight from a Cosmos RPC endpoint.
func NewCosmosPacketState(endpoint services.CosmosEndpoint) CosmosPacketState {
	return cosmosEndpointState{endpoint: endpoint}
}

type cosmosEndpointState struct {
	endpoint services.CosmosEndpoint
}

func (s cosmosEndpointState) HasPacketReceipt(ctx context.Context, packet channeltypesv2.Packet) (bool, error) {
	return subscriber.HasCosmosPacketReceipt(ctx, s.endpoint, packet)
}

func (s cosmosEndpointState) HasPacketCommitment(ctx context.Context, packet channeltypesv2.Packet) (bool, error) {
	return subscriber.HasPendingCosmosPacketCommitment(ctx, s.endpoint, packet)
}

// dropAlreadyHandled returns the events that still have work left on Cosmos.
// reason names the caller for the log line, since the two callers reconcile for
// different reasons and a stripped event should say which one dropped it.
//
// A source without a CosmosPacketState returns its input untouched: one-shot and
// test callers have no Cosmos endpoint, and losing reconciliation there costs a
// duplicate relay, which the destination rejects.
func (s *Source) dropAlreadyHandled(ctx context.Context, events []chain.Event, reason string) []chain.Event {
	if s.cosmosState == nil || len(events) == 0 {
		return events
	}
	kept := make([]chain.Event, 0, len(events))
	for _, event := range events {
		if s.stillNeedsRelay(ctx, event, reason) {
			kept = append(kept, event)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// stillNeedsRelay asks Cosmos whether this event's work is outstanding.
//
// It FAILS OPEN: an undecidable event is kept. The two outcomes are not
// symmetric — keeping an already-handled packet costs one duplicate relay that
// the destination rejects and the module drops permanently, while dropping an
// outstanding one strands the packet and its escrow with nothing in the log to
// say why. A reconciliation that can lose packets is worse than no
// reconciliation at all.
func (s *Source) stillNeedsRelay(ctx context.Context, event chain.Event, reason string) bool {
	var packet channeltypesv2.Packet
	if err := packet.Unmarshal(event.Raw); err != nil {
		log.Printf("WARN [l2->cosmos Subscribe] %s: seq=%d decode packet: %v; keeping it", reason, event.Sequence, err)
		return true
	}

	queryCtx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	handled, err := s.alreadyHandled(queryCtx, event.Type, packet)
	if err != nil {
		log.Printf("WARN [l2->cosmos Subscribe] %s: seq=%d %v; keeping it", reason, event.Sequence, err)
		return true
	}
	if handled {
		log.Printf("[l2->cosmos Subscribe] %s: seq=%d already settled on Cosmos (%s at L2 block %d), skipping",
			reason, event.Sequence, event.Type, event.Height)
		return false
	}
	return true
}

// alreadyHandled maps an event type to the Cosmos state that settles it.
//
// The two branches query DIFFERENT paths and read the answer in OPPOSITE
// directions, which is the whole content of this function: a send is settled by a
// receipt that EXISTS, an acknowledgement by a commitment that is GONE.
func (s *Source) alreadyHandled(ctx context.Context, eventType chain.EventType, packet channeltypesv2.Packet) (bool, error) {
	switch eventType {
	case chain.SendPacket:
		received, err := s.cosmosState.HasPacketReceipt(ctx, packet)
		if err != nil {
			return false, fmt.Errorf("check Cosmos packet receipt: %w", err)
		}
		return received, nil
	case chain.AckPacket:
		pending, err := s.cosmosState.HasPacketCommitment(ctx, packet)
		if err != nil {
			return false, fmt.Errorf("check Cosmos packet commitment: %w", err)
		}
		return !pending, nil
	default:
		// Unknown event types are never dropped: this function decides what to
		// skip, so an unhandled case must fall through to relaying.
		//
		// NOTE for B5, which adds TimeoutPacket to the L2 scan: it lands here and
		// is relayed unreconciled until it gets a branch. The ETH mirror's is
		// shouldEnqueueRecoveredCosmosPacket's CosmosTimeout case
		// (relayer/subscriber/cosmos.go) -- a timeout is settled by the SOURCE commitment
		// being gone, the same direction as the ack branch above.
		return false, nil
	}
}

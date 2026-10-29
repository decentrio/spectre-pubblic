// Package cosmos adapts a Cosmos (Tendermint) chain to the chain.Source and
// chain.Builder contracts. It wraps the existing relayer/client and
// relayer/services code — it does not reimplement signature extraction, proof
// generation, or gap-recovery.
package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/services"
	"relayer/subscriber"
	workergroup "relayer/workers"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
)

// ICS-24 path type discriminators (mirrors the constants used inline by the
// legacy packet planner): 1 = packet commitment, 2 = receipt, 3 = ack.
var (
	pathCommitment = []byte{1}
	pathReceipt    = []byte{2}
	pathAck        = []byte{3}
)

// Source is the Cosmos implementation of chain.Source. It holds the Cosmos and
// EVM endpoints needed by packet proofs and timeout checks; it is constructed by
// the wiring, not the cfg-only registry.
//
// It also holds the shared *services.BatchBuilder so Subscribe drains the same
// queue the (single) Services instance owns and can re-queue handler failures
// back onto it (with a waiting backoff).
type Source struct {
	cosmos       services.CosmosEndpoint
	evm          services.EVMEndpoint
	ids          services.ClientIDs
	fetchTimeout time.Duration
	batchConfig  services.BatchConfig
	logger       *log.Logger
	bb           *services.BatchBuilder
	recovery     *services.RecoveryStateStore

	// flushCursor is the last sequence the enumeration pass took, so the next
	// pass starts after it. Without it every pass re-takes the same oldest
	// flushSequenceCap sequences, and a prefix that never resolves -- pruned
	// history, or delivered packets still waiting on their acknowledgement --
	// starves every later packet permanently. Touched only from UnrelayedPackets,
	// which the relay module calls on one goroutine.
	flushCursor uint64
	// ackProbeCursor is the same rotation for the acknowledgement leg, over the
	// EVM router's commitments rather than this chain's: the lowest sequence the
	// last probe reached, 0 meaning "start from the newest".
	ackProbeCursor uint64
}

// NewSource wires the Cosmos source to its endpoints and the shared batch builder.
func NewSource(cosmos services.CosmosEndpoint, evm services.EVMEndpoint, ids services.ClientIDs, fetchTimeout time.Duration, batchConfig services.BatchConfig, logger *log.Logger, bb *services.BatchBuilder, recovery *services.RecoveryStateStore) *Source {
	return &Source{cosmos: cosmos, evm: evm, ids: ids, fetchTimeout: fetchTimeout, batchConfig: batchConfig, logger: logger, bb: bb, recovery: recovery}
}

func (s *Source) Chain() chain.ChainType { return chain.Cosmos }

// LatestHeight returns the latest committed Tendermint height. Tendermint has
// BFT instant finality, so the latest committed block is already final.
func (s *Source) LatestHeight(ctx context.Context) (uint64, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()
	lb, err := relayerclient.GetLatestLightBlock(fetchCtx, s.cosmos.CosmosClient())
	if err != nil {
		return 0, fmt.Errorf("cosmos source: latest light block: %w", err)
	}
	if lb.BlockHeight < 0 {
		return 0, fmt.Errorf("cosmos source: negative block height %d", lb.BlockHeight)
	}
	return uint64(lb.BlockHeight), nil
}

// cosmosAppHashLag is how far the queryable app state trails the committed
// height: a packet commitment written at block H is only reflected in the AppHash
// (and thus provable) at H+2. RelayableHeight subtracts this.
const cosmosAppHashLag = 2

// RelayableHeight returns latest committed height minus the AppHash lag, so the
// module only proves a Cosmos packet once its commitment is queryable.
func (s *Source) RelayableHeight(ctx context.Context) (uint64, error) {
	latest, err := s.LatestHeight(ctx)
	if err != nil {
		return 0, err
	}
	return relayableFromLatest(latest), nil
}

// relayableFromLatest applies the AppHash lag to a committed height.
//
// The subtraction is the whole precondition: a commitment written at H is not in
// the AppHash until H+2, so proving at anything above latest-2 asks the node for a
// proof of state it has not committed to yet, and the proof fails. Subtracting too
// much is merely slow; subtracting too little breaks every relay at the tip.
//
// Heights below the lag clamp to 0 rather than wrapping — this is unsigned
// arithmetic, and on a chain that has just started, latest-2 would otherwise
// become an enormous height.
func relayableFromLatest(latest uint64) uint64 {
	if latest < cosmosAppHashLag {
		return 0
	}
	return latest - cosmosAppHashLag
}

// QueryHeader returns the JSON-encoded light block at height.
func (s *Source) QueryHeader(ctx context.Context, height uint64) ([]byte, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()
	lb, err := relayerclient.GetLightBlock(fetchCtx, s.cosmos.CosmosClient(), int64(height))
	if err != nil {
		return nil, fmt.Errorf("cosmos source: light block at %d: %w", height, err)
	}
	raw, err := json.Marshal(lb)
	if err != nil {
		return nil, fmt.Errorf("cosmos source: encode light block: %w", err)
	}
	return raw, nil
}

// MembershipProof proves the packet's commitment (recv) or ack (ack) exists at
// height, wrapping services.CosmosMembership. eventType selects the ICS-24 path.
func (s *Source) MembershipProof(ctx context.Context, packet []byte, height uint64, eventType chain.EventType) ([]byte, error) {
	// Decode and classify BEFORE fetching the light block, the way the ETH and L2
	// sources already do. Both inputs are permanently wrong when they are wrong,
	// so discovering it after a round trip spends one for nothing -- and on a
	// slow endpoint it arrives as a timeout, hiding the real cause.
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(packet); err != nil {
		return nil, fmt.Errorf("cosmos source: decode packet: %w", err)
	}
	var clientID string
	var pathType []byte
	switch eventType {
	case chain.SendPacket:
		// A Cosmos->ETH send whose timeout has passed (measured against ETH block
		// time, the destination clock) can never be received on ETH — recvPacket
		// would revert. Report it permanent so the module DROPS it and the async
		// timeout scanner refunds it on Cosmos instead. Without this the
		// module retries the dead packet forever, each retry re-running a full
		// client update and draining gas.
		if timedOut, err := s.sendTimedOutOnEth(ctx, pkt); err != nil {
			return nil, err // transient (RPC) — let the module retry
		} else if timedOut {
			return nil, chain.Permanent(fmt.Errorf("cosmos source: send seq=%d timed out; deferred to timeout scanner", pkt.Sequence))
		}
		clientID, pathType = pkt.SourceClient, pathCommitment
	case chain.AckPacket:
		clientID, pathType = pkt.DestinationClient, pathAck
	default:
		return nil, fmt.Errorf("cosmos source: MembershipProof: unsupported event type %d", eventType)
	}
	lb, err := s.lightBlock(ctx, height)
	if err != nil {
		return nil, err
	}
	proofCtx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()
	return services.CosmosMembership(proofCtx, s.cosmos, pkt, clientID, pathType, lb)
}

// sendTimedOutOnEth reports whether the packet's timeout timestamp has passed on
// the ETH side, comparing against the latest ETH block time (the destination
// clock the on-chain timeout check uses). A zero timeout never expires.
func (s *Source) sendTimedOutOnEth(parent context.Context, pkt channeltypesv2.Packet) (bool, error) {
	if pkt.TimeoutTimestamp == 0 {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(parent, s.fetchTimeout)
	defer cancel()
	header, err := s.evm.EthClient().HeaderByNumber(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("cosmos source: eth block time: %w", err)
	}
	return header.Time >= pkt.TimeoutTimestamp, nil
}

// NonMembershipProof proves the packet receipt is absent at height (timeout),
// wrapping services.CosmosNonMembership.
func (s *Source) NonMembershipProof(ctx context.Context, packet []byte, height uint64) ([]byte, error) {
	pkt, lb, err := s.decodePacketAndBlock(ctx, packet, height)
	if err != nil {
		return nil, err
	}
	proofCtx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()
	return services.CosmosNonMembership(proofCtx, s.cosmos, pkt, pkt.DestinationClient, pathReceipt, lb)
}

// decodePacketAndBlock decodes the proto packet and fetches the light block the
// proof is built against.
func (s *Source) decodePacketAndBlock(ctx context.Context, packet []byte, height uint64) (channeltypesv2.Packet, *relayerclient.LightBlock, error) {
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(packet); err != nil {
		return channeltypesv2.Packet{}, nil, fmt.Errorf("cosmos source: decode packet: %w", err)
	}
	lb, err := s.lightBlock(ctx, height)
	if err != nil {
		return channeltypesv2.Packet{}, nil, err
	}
	return pkt, lb, nil
}

// lightBlock is the round trip the proof paths share, split out so a caller can
// finish validating its cheap inputs before spending one.
func (s *Source) lightBlock(ctx context.Context, height uint64) (*relayerclient.LightBlock, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()
	lb, err := relayerclient.GetLightBlock(fetchCtx, s.cosmos.CosmosClient(), int64(height))
	if err != nil {
		return nil, fmt.Errorf("cosmos source: light block at %d: %w", height, err)
	}
	return lb, nil
}

// drainInterval is how often the Subscribe bridge flushes the batch builder.
const drainInterval = 500 * time.Millisecond

// Subscribe bridges the existing gap-recovery subscriber to the handler-based
// contract: it runs services' SubscribeCosmos (which keeps all the startup
// lookback / reconnect-gap recovery) pushing into a BatchBuilder, then drains
// the builder on the configured batch window and emits each queued CosmosPacket
// as a chain.Event. It uses the batch config (not BatchSize=1) so CheckCosmos
// returns multi-packet batches the module folds into one RelayPackets multicall.
func (s *Source) Subscribe(ctx context.Context, handler func(context.Context, []chain.Event) []int) (err error) {
	sub := subscriber.NewSubscriber(s.recovery)
	// Named rather than anonymous: when a shutdown times out, the module reports
	// "workers still running: [subscribe]" and the two goroutines below are what
	// is inside it. Without names the log stops one level above the thing that is
	// stuck, which is exactly where diagnosis needs it.
	workers := workergroup.New()
	workers.Go("cosmos-subscribe", func() {
		sub.SubscribeCosmos(ctx, s.cosmos, s.evm, s.ids, s.logger, s.bb)
	})
	defer func() {
		running := workers.Drain(workergroup.SourceDrainTimeout)
		if len(running) == 0 {
			return
		}
		s.logger.Printf("ERROR [SubscribeCosmos] shutdown drain timed out after %s; workers still running: %v",
			workergroup.SourceDrainTimeout, running)
		// RETURN it, do not only log it. The caller cancels on SIGTERM and then
		// normalises context.Canceled to a clean exit, so a stuck worker reported
		// only through ctx.Err() is reported as a successful shutdown. This error
		// is not a cancellation, so it survives that normalisation.
		stuck := fmt.Errorf("cosmos source: %w after %s; workers still running: %v",
			workergroup.ErrWorkersStillRunning, workergroup.SourceDrainTimeout, running)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = stuck
			return
		}
		// A real failure already happened; keep it, and carry the drain alongside
		// rather than choosing between two things the operator needs.
		err = errors.Join(err, stuck)
	}()

	// Use the configured batch window so CheckCosmos returns multi-packet batches
	// the handler can fold into one multicall (BatchSize=1 would defeat that).
	cfg := s.batchConfig
	ch := make(chan services.CosmosBatch, services.BatchHandoffCapacity)

	workers.Go("cosmos-drain-batches", func() {
		ticker := time.NewTicker(drainInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.bb.CheckCosmos(ctx, cfg, ch)
			}
		}
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case batch := <-ch:
			events, orig := eventsWithOrigins(batch.Packets, s.ids.CosmosOnEVM)
			// Re-queue un-relayed packets with a waiting backoff so a packet not yet
			// relayable (AppHash H+2 lag) or hit by a brief RPC hiccup is retried with
			// a growing delay instead of every batch period — quiet, and no per-flush
			// proof/RPC churn. CheckCosmos already sliced them off the queue.
			for _, idx := range handler(ctx, events) {
				s.bb.RequeueCosmosWaiting([]services.CosmosPacket{orig[idx]})
			}
			s.bb.ReleaseCosmosInFlight(batch)
		}
	}
}

// eventsWithOrigins maps a drained batch to relay events, keeping a parallel slice
// of the packets they came from.
//
// The two slices MUST stay index-aligned: the handler reports failures as indices
// into the events slice, and Subscribe re-queues orig[idx]. A packet that cannot be
// converted is dropped from BOTH, never from one — appending to orig outside the
// conversion guard would shift every later index and re-queue the wrong packet,
// silently relaying one packet twice and losing another.
func eventsWithOrigins(packets []services.CosmosPacket, clientID string) ([]chain.Event, []services.CosmosPacket) {
	events := make([]chain.Event, 0, len(packets))
	orig := make([]services.CosmosPacket, 0, len(packets))
	for _, p := range packets {
		e, ok := cosmosPacketToEvent(p, clientID)
		if !ok {
			continue
		}
		events = append(events, e)
		orig = append(orig, p)
	}
	return events, orig
}

// cosmosPacketToEvent maps a queued CosmosPacket to a chain.Event. It returns
// false when the packet cannot be encoded (skip rather than relay a bad event) or
// when the event must not be relayed to ETH.
//
// routerClientID is this Cosmos chain's ETH-side router client id, used to drop
// Cosmos-originated timeouts: an IBC v2 timeout is submitted to the SOURCE chain,
// so a Cosmos→ETH packet's timeout fires on Cosmos and ETH never held a
// commitment for it — relaying an ETH timeoutPacket would waste a Groth16 proof +
// tx (and historically blocked the relay loop). The live subscribe path enqueues
// these (destination_client == routerClientID matches the configured-client
// filter), so the check is applied here. ETH-originated timeouts (destination_client != routerClientID)
// still relay so ETH deletes its commitment + refunds.
func cosmosPacketToEvent(p services.CosmosPacket, routerClientID string) (chain.Event, bool) {
	if p.Packet == nil {
		return chain.Event{}, false
	}
	raw, err := p.Packet.Marshal()
	if err != nil {
		return chain.Event{}, false
	}
	e := chain.Event{
		Height:   p.BlockNumber,
		Sequence: p.Packet.Sequence,
		ClientID: p.Packet.DestinationClient,
		Raw:      raw,
	}
	switch p.Type {
	case services.CosmosSend:
		e.Type = chain.SendPacket
	case services.CosmosAck:
		// An ack with no acknowledgement bytes cannot be built into a MsgAckPacket
		// (the ETH side needs the ack) — skip it, rather than emitting an event that
		// would error and re-queue forever.
		if len(p.AckBytes) == 0 {
			return chain.Event{}, false
		}
		e.Type = chain.AckPacket
		e.AckBytes = p.AckBytes
	case services.CosmosTimeout:
		if !services.ShouldRelayCosmosTimeoutToEth(p.Packet, routerClientID) {
			return chain.Event{}, false // Cosmos-originated timeout handled locally on Cosmos
		}
		e.Type = chain.TimeoutPacket
	default:
		return chain.Event{}, false
	}
	return e, true
}

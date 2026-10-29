package evm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"time"

	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/services"
	"relayer/subscriber"
	workergroup "relayer/workers"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

// Source is the Ethereum L1 implementation of chain.Source for the ETH->Cosmos
// direction. It holds the endpoints and IDs needed by beacon proofs and state reads,
// and the shared *services.BatchBuilder so Subscribe drains the same queue the
// Services instance owns (where the ETH subscriber records EthPendingTracker
// entries) and can re-queue handler failures (with a waiting backoff).
// Constructed by the wiring, not the cfg-only registry.
type Source struct {
	cosmos      services.CosmosEndpoint
	evm         services.EVMEndpoint
	ids         services.ClientIDs
	batchConfig services.BatchConfig
	logger      *log.Logger
	bb          *services.BatchBuilder
	recovery    *services.RecoveryStateStore

	// routerDeployBlock is the block the ICS26Router was deployed at; 0 when not
	// configured. The flush's log lookups start there (see evmflush.Router).
	routerDeployBlock uint64

	// flush is the enumeration backstop's cursors and log-range policy; see
	// flush.go. Touched only from UnrelayedPackets, which the relay module calls
	// on one goroutine.
	flush flushState
}

// NewSource wires the Ethereum source to its endpoints and the shared batch builder.
func NewSource(cosmos services.CosmosEndpoint, evm services.EVMEndpoint, ids services.ClientIDs, batchConfig services.BatchConfig, logger *log.Logger, bb *services.BatchBuilder, recovery *services.RecoveryStateStore) *Source {
	return &Source{cosmos: cosmos, evm: evm, ids: ids, batchConfig: batchConfig, logger: logger, bb: bb, recovery: recovery}
}

// WithRouterDeployBlock sets the block the ICS26Router was deployed at, so the
// enumeration backstop asks for the router's whole history in one log query
// instead of walking bounded windows back from the head. 0 keeps the walk.
func (s *Source) WithRouterDeployBlock(n uint64) *Source {
	s.routerDeployBlock = n
	return s
}

func (s *Source) Chain() chain.ChainType { return chain.Ethereum }

// LatestHeight returns the execution block number of the latest FINALIZED beacon
// header. It MUST be the same number space as RelayableHeight, event heights, and
// ClientUpdate.Height (all execution block numbers, see BeaconBuilder.Build) —
// the module's refreshLoop feeds this into updateClientTo, which compares it
// against m.lastHeight (an execution block number). Returning a beacon slot here
// would mix number spaces and make the anti-expiry refresh skip incorrectly. The
// beacon light client only advances to finalized headers, so there is no
// unsafe-head exposure — latest and relayable coincide on the ETH source side.
func (s *Source) LatestHeight(ctx context.Context) (uint64, error) {
	return s.finalizedExecBlock(ctx)
}

// RelayableHeight returns the execution block number of the latest FINALIZED
// beacon header — the highest ETH block the beacon light client can prove against
// right now. The module compares packet event heights (execution block numbers)
// against this, so a packet whose block is not yet finalized is re-queued without
// building a proof.
func (s *Source) RelayableHeight(ctx context.Context) (uint64, error) {
	return s.finalizedExecBlock(ctx)
}

// finalizedExecBlock reads the beacon finality update and returns the execution
// block number of the finalized header — the single finality-gated height both
// LatestHeight and RelayableHeight report.
func (s *Source) finalizedExecBlock(parent context.Context) (uint64, error) {
	beaconURL := s.evm.BeaconAPIURL
	if beaconURL == "" {
		return 0, fmt.Errorf("eth source: beacon API URL is not configured")
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	fu, err := relayerclient.GetFinalityUpdate(ctx, beaconURL)
	if err != nil {
		return 0, fmt.Errorf("eth source: finality update: %w", err)
	}
	block, err := relayerclient.FinalizedExecutionBlockNumber(ctx, s.evm.EthClient(), fu.FinalizedHeader)
	if err != nil {
		return 0, fmt.Errorf("eth source: finalized exec block: %w", err)
	}
	return block, nil
}

// QueryHeader is a no-op for the beacon path: the BeaconBuilder self-fetches the
// finality/sync-committee data via the beacon API (like the Cosmos groth16
// builder), so it ignores the header argument.
func (s *Source) QueryHeader(_ context.Context, _ uint64) ([]byte, error) {
	return nil, nil
}

// ethDrainInterval is how often the Subscribe bridge flushes the batch builder.
const ethDrainInterval = 500 * time.Millisecond

// Subscribe bridges the existing SubscribeEth gap-recovery subscriber to the
// handler-based contract (mirror of the Cosmos source bridge): it runs
// SubscribeEth pushing into a BatchBuilder, drains it, and emits each EthSend as
// a recv event and each EthWriteAck as an ack event. EthTimeout is skipped —
// ETH-origin timeouts are handled by the async scanner, not this path.
func (s *Source) Subscribe(ctx context.Context, handler func(context.Context, []chain.Event) []int) (err error) {
	sub := subscriber.NewSubscriber(s.recovery)
	// Named for the same reason as the Cosmos mirror: a stuck shutdown must name
	// which of the two goroutines did not return, not just that "subscribe" did not.
	workers := workergroup.New()
	workers.Go("eth-subscribe", func() {
		sub.SubscribeEth(ctx, s.cosmos, s.evm, s.ids, s.logger, s.bb)
	})
	defer func() {
		running := workers.Drain(workergroup.SourceDrainTimeout)
		if len(running) == 0 {
			return
		}
		s.logger.Printf("ERROR [eth->cosmos Subscribe] shutdown drain timed out after %s; workers still running: %v",
			workergroup.SourceDrainTimeout, running)
		// RETURN it, do not only log it. The caller cancels on SIGTERM and then
		// normalises context.Canceled to a clean exit, so a stuck worker reported
		// only through ctx.Err() is reported as a successful shutdown. This error
		// is not a cancellation, so it survives that normalisation.
		stuck := fmt.Errorf("eth source: %w after %s; workers still running: %v",
			workergroup.ErrWorkersStillRunning, workergroup.SourceDrainTimeout, running)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = stuck
			return
		}
		// A real failure already happened; keep it, and carry the drain alongside
		// rather than choosing between two things the operator needs.
		err = errors.Join(err, stuck)
	}()

	// Use the configured batch window so CheckEth returns multi-packet batches the
	// handler can fold into one multicall (BatchSize=1 would defeat that).
	cfg := s.batchConfig
	ch := make(chan services.EthBatch, services.BatchHandoffCapacity)

	workers.Go("eth-drain-batches", func() {
		ticker := time.NewTicker(ethDrainInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.bb.CheckEth(ctx, cfg, ch)
			}
		}
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case batch := <-ch:
			events, orig := eventsWithOrigins(batch.Packets, s.bb.EthPendingTracker.RemovePacketIfCurrent, s.bb.SettleOwedAck)
			// Re-queue un-relayed packets with a waiting backoff so a packet not yet
			// relayable (beacon finality lag) or hit by a brief RPC hiccup is retried
			// with a growing delay instead of every batch period — quiet, and no
			// per-flush finality RPC. CheckEth already sliced them off the queue.
			for _, idx := range handler(ctx, events) {
				s.bb.RequeueEthWaiting([]services.EthPacket{orig[idx]})
			}
			s.bb.ReleaseEthInFlight(batch)
		}
	}
}

// eventsWithOrigins maps a drained batch to relay events, keeping a parallel slice
// of the packets they came from, and settles terminal events along the way.
//
// The two slices MUST stay index-aligned: the handler reports failures as indices
// into the events slice, and Subscribe re-queues orig[idx]. A packet dropped here
// — terminal or unconvertible — leaves BOTH slices, never one, since appending to
// orig outside the conversion guard would shift every later index and re-queue the
// wrong packet: one packet relayed twice, another lost. This is the mirror of the
// same invariant in chain/cosmos.
//
// settle removes an acked or timed-out ETH-origin send from the pending tracker so
// the timeout scanner stops considering it; such a packet is then skipped, as
// there is nothing to relay.
func eventsWithOrigins(
	packets []services.EthPacket,
	settle func(channeltypesv2.Packet) error,
	settleOwedAck func(channeltypesv2.Packet),
) ([]chain.Event, []services.EthPacket) {
	events := make([]chain.Event, 0, len(packets))
	orig := make([]services.EthPacket, 0, len(packets))
	for _, p := range packets {
		if p.Packet != nil && (p.Type == services.EthAck || p.Type == services.EthTimeout) {
			if err := settle(*p.Packet); err != nil {
				log.Printf("ERROR [PendingTracker][ATTENTION] failed to persist removal of settled ETH packet seq=%d: %v", p.Packet.Sequence, err)
			}
			// Only EthAck settles an owed acknowledgement. A timeout is the other
			// ending: the packet is refunded, no acknowledgement is owed or coming,
			// and clearing a debt on it would hide a real one if both were somehow
			// recorded for one slot.
			if p.Type == services.EthAck {
				settleOwedAck(*p.Packet)
			}
			continue
		}
		e, ok := ethPacketToEvent(p)
		if !ok {
			continue
		}
		events = append(events, e)
		orig = append(orig, p)
	}
	return events, orig
}

// ethPacketToEvent maps a queued EthPacket to a chain.Event, or false to skip
// (unencodable, or an EthTimeout the scanner owns).
func ethPacketToEvent(p services.EthPacket) (chain.Event, bool) {
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
	case services.EthSend:
		e.Type = chain.SendPacket
	case services.EthWriteAck:
		// Skip an ack with no acknowledgement bytes — it cannot be built into a
		// MsgAcknowledgement.
		if len(p.AckBytes) == 0 {
			return chain.Event{}, false
		}
		e.Type = chain.AckPacket
		e.AckBytes = p.AckBytes
	default:
		return chain.Event{}, false // EthTimeout is scanner-handled
	}
	return e, true
}

// sendPacketExpired reports whether an ETH→Cosmos send is already past its
// timeout, in which case it can never be received on Cosmos.
//
// Timeouts are absolute SECONDS, and Cosmos has ~wall-clock BFT time, so `now` is
// the right clock to compare against. The boundary is inclusive: a packet whose
// timeout equals the current second is expired, matching the chain's own check.
// A zero timeout means "no timeout" and never expires.
func sendPacketExpired(pkt channeltypesv2.Packet, now time.Time) bool {
	if pkt.TimeoutTimestamp == 0 {
		return false
	}
	return uint64(now.Unix()) >= pkt.TimeoutTimestamp
}

// MembershipProof builds an Ethereum storage proof that the packet's commitment
// (recv) or ack exists, wrapping client.GetEthMembershipProof.
//
// height is the EXECUTION BLOCK the proof is taken at, as chain.Source specifies
// and as chain/README.md step 4 spells out: "targeting the prepared update height
// when it is not on-chain yet". The Cosmos and L2 sources already honour it; this
// one used to ignore it and read the block from the on-chain client instead.
//
// That was wrong for every folded batch. Folding submits the client update and the
// packet in ONE tx, so at proof-build time the update is not on chain yet: the
// client still reports the PREVIOUS execution block, while the MsgRecvPacket the
// destination builds names the consensus height the folded update installs
// (chain/cosmos.RelayWithUpdate). The proof was therefore built against one state
// root and verified against another, and the client rejected it with
// "get trie node failed: Invalid state root" -- a message that points at the trie,
// not at the height mismatch that caused it.
func (s *Source) MembershipProof(ctx context.Context, packet []byte, height uint64, eventType chain.EventType) ([]byte, error) {
	// Zero is not "latest" here -- it would prove at genesis and fail every time,
	// far from where the mistake was made.
	if height == 0 {
		return nil, fmt.Errorf("eth source: MembershipProof needs the execution block to prove at, got 0")
	}
	var pkt channeltypesv2.Packet
	if err := pkt.Unmarshal(packet); err != nil {
		return nil, fmt.Errorf("eth source: decode packet: %w", err)
	}
	var clientID string
	var pathType byte
	switch eventType {
	case chain.SendPacket:
		// An ETH->Cosmos send past its timeout can never be received on Cosmos —
		// MsgRecvPacket would fail. Report it permanent so the module DROPS it and
		// the async scanner (scanForEthTimeouts) refunds it on ETH instead (mirrors
		// the legacy ethPacketExpired pre-filter). Cosmos has ~wall-clock BFT time,
		// so compare against time.Now like the legacy path does. Without this a dead
		// send is retried forever, draining gas on repeated client updates.
		if sendPacketExpired(pkt, time.Now()) {
			return nil, chain.Permanent(fmt.Errorf("eth source: send seq=%d timed out; deferred to timeout scanner", pkt.Sequence))
		}
		clientID, pathType = pkt.SourceClient, 1 // packet commitment
	case chain.AckPacket:
		clientID, pathType = pkt.DestinationClient, 3 // ack
	default:
		return nil, fmt.Errorf("eth source: MembershipProof: unsupported event type %d", eventType)
	}
	path := services.EthPath(clientID, pkt.Sequence, pathType)
	proofCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return relayerclient.GetEthMembershipProof(
		proofCtx, s.evm.EthClient(), s.evm.Contracts.Router, path,
		ethcommon.HexToHash(services.ICS26_IBC_STORAGE_SLOT),
		new(big.Int).SetUint64(height),
	)
}

// NonMembershipProof is unused on the ETH source relay path: ETH-origin packet
// timeouts are handled by the async timeout scanner (scanForEthTimeouts), not the
// Subscribe->relay flow, so no TimeoutPacket event reaches here.
func (s *Source) NonMembershipProof(_ context.Context, _ []byte, _ uint64) ([]byte, error) {
	return nil, fmt.Errorf("eth source: NonMembershipProof unused (timeouts are scanner-handled)")
}

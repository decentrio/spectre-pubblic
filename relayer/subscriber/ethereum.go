// This file is the Ethereum direction of the subscriber: the go-ethereum log
// watchers, the eth_getLogs gap recovery, and the decode/enqueue path for
// ETH-origin packet events.
//
// Its mirror is cosmos.go; see that file for why the pairing is worth a file
// boundary.
package subscriber

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"time"

	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/services"
	"relayer/utils"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

func EthPacketToCosmosPacket(ethPacket contractICS26Router.IICS26RouterMsgsPacket, sequence *big.Int) channeltypesv2.Packet {
	var payloads []channeltypesv2.Payload
	for _, p := range ethPacket.Payloads {
		payloads = append(payloads, channeltypesv2.Payload{
			SourcePort:      p.SourcePort,
			DestinationPort: p.DestPort,
			Version:         p.Version,
			Encoding:        p.Encoding,
			Value:           p.Value,
		})
	}

	return channeltypesv2.Packet{
		Sequence:          sequence.Uint64(),
		SourceClient:      ethPacket.SourceClient,
		DestinationClient: ethPacket.DestClient,
		TimeoutTimestamp:  ethPacket.TimeoutTimestamp,
		Payloads:          payloads,
	}
}

func ethStartupRecoveryLookbackBlocksFromEnv(raw string) uint64 {
	if raw == "" {
		return defaultEthStartupRecoveryLookbackBlocks
	}

	lookback, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return defaultEthStartupRecoveryLookbackBlocks
	}

	return lookback
}

func ethStartupRecoveryLookbackBlocks() uint64 {
	return ethStartupRecoveryLookbackBlocksFromEnv(os.Getenv(ethStartupRecoveryLookbackEnv))
}

func ethStartupRecoveryStartBlock(latestBlock, lookback uint64) uint64 {
	if lookback >= latestBlock {
		return 0
	}

	return latestBlock - lookback
}

func ethEventClientIDFilter(ctx ethDeps) []string {
	clientID := ctx.IDs.CosmosOnEVM
	if clientID == "" {
		return nil
	}
	return []string{clientID}
}

func ethEventKeyForLog(eventType string, raw gethtypes.Log) ethEventKey {
	return ethEventKey{
		EventType:   eventType,
		TxHash:      raw.TxHash.Hex(),
		LogIndex:    raw.Index,
		BlockNumber: raw.BlockNumber,
	}
}

func markEthEventSeen(seenEvents map[ethEventKey]struct{}, key ethEventKey) bool {
	if seenEvents == nil {
		return true
	}
	if _, ok := seenEvents[key]; ok {
		return false
	}
	seenEvents[key] = struct{}{}
	return true
}

func pruneEthSeenEvents(seenEvents map[ethEventKey]struct{}, currentBlock uint64) {
	if currentBlock <= ethSeenEventRetentionBlocks {
		return
	}
	minBlock := currentBlock - ethSeenEventRetentionBlocks
	for key := range seenEvents {
		if key.BlockNumber > 0 && key.BlockNumber < minBlock {
			delete(seenEvents, key)
		}
	}
}

func enqueueEthWriteAcknowledgement(
	batchBuilder *services.BatchBuilder,
	ev *contractICS26Router.ContractICS26RouterWriteAcknowledgement,
	seenEvents map[ethEventKey]struct{},
) bool {
	if !markEthEventSeen(seenEvents, ethEventKeyForLog("WriteAcknowledgement", ev.Raw)) {
		return false
	}
	cosmosPacket := EthPacketToCosmosPacket(ev.Packet, ev.Sequence)
	batchBuilder.AddEth(services.EthPacket{
		Type:        services.EthWriteAck,
		Packet:      &cosmosPacket,
		AckBytes:    ev.Acknowledgements,
		BlockNumber: ev.Raw.BlockNumber,
	})
	return true
}

// enqueueEthTerminal records an AckPacket or TimeoutPacket — the two events that
// SETTLE an ETH-origin send rather than starting new work. It removes the packet
// from the pending tracker (it can no longer time out) and queues it so the
// source bridge can observe the settlement.
//
// Extracted from the subscribe select so BlockNumber is set in one place. The
// two enqueues used to be written inline and both omitted it, which is harmless
// only for as long as nothing downstream reads Height for these types — a trap
// rather than a bug, and one worth closing where it can be tested.
func enqueueEthTerminal(
	batchBuilder *services.BatchBuilder,
	logger *log.Logger,
	packetType services.EthPacketType,
	packet contractICS26Router.IICS26RouterMsgsPacket,
	sequence *big.Int,
	ackBytes [][]byte,
	blockNumber uint64,
) {
	cosmosPacket := EthPacketToCosmosPacket(packet, sequence)
	if err := batchBuilder.EthPendingTracker.RemovePacketIfCurrent(cosmosPacket); err != nil {
		logger.Printf("ERROR [eth->cosmos Subscribe][ATTENTION] failed to persist removal of settled ETH packet seq=%d: %v",
			cosmosPacket.Sequence, err)
	}
	batchBuilder.AddEth(services.EthPacket{
		Type:        packetType,
		Packet:      &cosmosPacket,
		AckBytes:    ackBytes,
		BlockNumber: blockNumber,
	})
}

func enqueueEthSendPacket(
	batchBuilder *services.BatchBuilder,
	ev *contractICS26Router.ContractICS26RouterSendPacket,
	seenEvents map[ethEventKey]struct{},
) (bool, error) {
	cosmosPacket := EthPacketToCosmosPacket(ev.Packet, ev.Sequence)
	if !batchBuilder.EthPendingTracker.Add(cosmosPacket, ev.Raw.BlockNumber) {
		if err := batchBuilder.EthPendingTracker.PersistenceError(); err != nil {
			return false, fmt.Errorf("durably track ETH SendPacket seq=%d from block %d: %w", cosmosPacket.Sequence, ev.Raw.BlockNumber, err)
		}
		return false, fmt.Errorf("durably track ETH SendPacket seq=%d from block %d", cosmosPacket.Sequence, ev.Raw.BlockNumber)
	}
	if !markEthEventSeen(seenEvents, ethEventKeyForLog("SendPacket", ev.Raw)) {
		return false, nil
	}
	batchBuilder.AddEth(services.EthPacket{
		Type:        services.EthSend,
		Packet:      &cosmosPacket,
		BlockNumber: ev.Raw.BlockNumber,
	})
	return true, nil
}

func hasCosmosIBCPathValue(stdCtx context.Context, endpoint services.CosmosEndpoint, path [][]byte) (bool, error) {
	// The guard services.HasEthIBCPathValue already has on its side. Without it an
	// unconfigured endpoint panics here instead of erroring, and a panic inside a
	// receipt check is the worst place for one: the answer this returns decides
	// whether a packet may be timed out.
	if endpoint.CosmosClient() == nil {
		return false, fmt.Errorf("cosmos client is nil")
	}
	queryPath := fmt.Sprintf("store/%s/key", string(path[0]))
	request := path[1]

	rpcCtx, cancel := context.WithTimeout(stdCtx, subscriberRPCTimeout)
	defer cancel()
	result, err := endpoint.CosmosClient().ABCIQuery(rpcCtx, queryPath, request)
	if err != nil {
		return false, fmt.Errorf("ABCI query failed: %w", err)
	}
	if result.Response.Code != 0 {
		return false, fmt.Errorf("query failed with code %d: %s", result.Response.Code, result.Response.Log)
	}

	return len(result.Response.Value) > 0, nil
}

func hasPendingCosmosPacketCommitment(stdCtx context.Context, ctx ethDeps, packet channeltypesv2.Packet) (bool, error) {
	return hasCosmosIBCPathValue(stdCtx, ctx.Cosmos, utils.IBCCommitmentPath(packet, []byte{1}))
}

// HasCosmosPacketReceipt is retained for one-shot callers. Long-running relay
// paths must call HasCosmosPacketReceiptWithContext.
// HasPendingCosmosPacketCommitment reports whether Cosmos still stores the
// packet's commitment. A cleared commitment means the packet has already been
// acknowledged or timed out there, so relaying its acknowledgement again is work
// with no effect. Exported alongside HasCosmosPacketReceipt for the L2 source,
// which reconciles against the same two pieces of Cosmos state.
func HasPendingCosmosPacketCommitment(
	stdCtx context.Context,
	endpoint services.CosmosEndpoint,
	packet channeltypesv2.Packet,
) (bool, error) {
	return hasCosmosIBCPathValue(stdCtx, endpoint, utils.IBCCommitmentPath(packet, []byte{1}))
}

func HasCosmosPacketReceipt(stdCtx context.Context, endpoint services.CosmosEndpoint, packet channeltypesv2.Packet) (bool, error) {
	return hasCosmosIBCPathValue(stdCtx, endpoint, utils.IBCPath(packet.DestinationClient, packet.Sequence, []byte{2}))
}

func recoverEthSendPackets(
	stdCtx context.Context,
	ctx ethDeps,
	batchBuilder *services.BatchBuilder,
	filterer *contractICS26Router.ContractICS26RouterFilterer,
	startBlock uint64,
	endBlock uint64,
	seenEvents map[ethEventKey]struct{},
) (ethRecoveryStats, error) {
	var stats ethRecoveryStats
	if endBlock < startBlock {
		return stats, nil
	}
	filterCtx, cancel := context.WithTimeout(stdCtx, subscriberRPCTimeout)
	defer cancel()

	filterOpts := &bind.FilterOpts{
		Start:   startBlock,
		End:     &endBlock,
		Context: filterCtx,
	}
	iter, err := filterer.FilterSendPacket(filterOpts, ethEventClientIDFilter(ctx), nil)
	if err != nil {
		return stats, fmt.Errorf("failed to filter SendPacket logs in [%d,%d]: %w", startBlock, endBlock, err)
	}
	defer iter.Close()

	var firstErr error
	for iter.Next() {
		ev := iter.Event
		key := ethEventKeyForLog("SendPacket", ev.Raw)
		if seenEvents != nil {
			if _, ok := seenEvents[key]; ok {
				stats.skipped++
				continue
			}
		}

		cosmosPacket := EthPacketToCosmosPacket(ev.Packet, ev.Sequence)

		received, err := HasCosmosPacketReceipt(stdCtx, ctx.Cosmos, cosmosPacket)
		if err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] recovery: seq=%d failed to check Cosmos packet receipt: %v",
				cosmosPacket.Sequence, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if received {
			markEthEventSeen(seenEvents, key)
			stats.skipped++
			ctx.Logger.Printf("[eth->cosmos Subscribe] recovery: seq=%d already received on Cosmos, skipping historical SendPacket from ETH block %d",
				cosmosPacket.Sequence, ev.Raw.BlockNumber)
			continue
		}

		pending, err := services.HasPendingEthPacketCommitment(stdCtx, ctx.EVM, cosmosPacket)
		if err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] recovery: seq=%d failed to check ETH packet commitment: %v",
				cosmosPacket.Sequence, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !pending {
			markEthEventSeen(seenEvents, key)
			if err := batchBuilder.EthPendingTracker.RemovePacketIfCurrent(cosmosPacket); err != nil {
				ctx.Logger.Printf("ERROR [eth->cosmos Subscribe][ATTENTION] recovery: failed to persist removal of cleared ETH packet seq=%d: %v",
					cosmosPacket.Sequence, err)
			}
			stats.skipped++
			ctx.Logger.Printf("[eth->cosmos Subscribe] recovery: seq=%d already cleared on ETH, skipping historical SendPacket from ETH block %d",
				cosmosPacket.Sequence, ev.Raw.BlockNumber)
			continue
		}

		enqueued, err := enqueueEthSendPacket(batchBuilder, ev, seenEvents)
		if err != nil {
			ctx.Logger.Printf("ERROR [eth->cosmos Subscribe][ATTENTION] recovery: %v; leaving SendPacket range [%d,%d] retryable", err, startBlock, endBlock)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if enqueued {
			ctx.Logger.Printf("[eth->cosmos Subscribe] recovery: recovered SendPacket seq=%d from ETH block %d",
				cosmosPacket.Sequence, ev.Raw.BlockNumber)
			stats.recovered++
		} else {
			stats.skipped++
		}
	}

	if err := iter.Error(); err != nil {
		ctx.Logger.Printf("WARN [eth->cosmos Subscribe] recovery: SendPacket iterator error in [%d,%d]: %v", startBlock, endBlock, err)
		if firstErr == nil {
			firstErr = err
		}
	}

	if stats.recovered > 0 || stats.skipped > 0 {
		ctx.Logger.Printf("[eth->cosmos Subscribe] recovery scanned [%d,%d] for SendPacket: recovered=%d skipped=%d",
			startBlock, endBlock, stats.recovered, stats.skipped)
	}
	return stats, firstErr
}

func recoverEthWriteAcknowledgements(
	stdCtx context.Context,
	ctx ethDeps,
	batchBuilder *services.BatchBuilder,
	filterer *contractICS26Router.ContractICS26RouterFilterer,
	startBlock uint64,
	endBlock uint64,
	seenEvents map[ethEventKey]struct{},
) (ethRecoveryStats, error) {
	var stats ethRecoveryStats
	if endBlock < startBlock {
		return stats, nil
	}
	filterCtx, cancel := context.WithTimeout(stdCtx, subscriberRPCTimeout)
	defer cancel()

	filterOpts := &bind.FilterOpts{
		Start:   startBlock,
		End:     &endBlock,
		Context: filterCtx,
	}
	iter, err := filterer.FilterWriteAcknowledgement(filterOpts, ethEventClientIDFilter(ctx), nil)
	if err != nil {
		return stats, fmt.Errorf("failed to filter WriteAcknowledgement logs in [%d,%d]: %w", startBlock, endBlock, err)
	}
	defer iter.Close()

	var firstErr error
	for iter.Next() {
		ev := iter.Event
		key := ethEventKeyForLog("WriteAcknowledgement", ev.Raw)
		if seenEvents != nil {
			if _, ok := seenEvents[key]; ok {
				stats.skipped++
				continue
			}
		}

		cosmosPacket := EthPacketToCosmosPacket(ev.Packet, ev.Sequence)

		pending, err := hasPendingCosmosPacketCommitment(stdCtx, ctx, cosmosPacket)
		if err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] recovery: seq=%d failed to check Cosmos packet commitment: %v",
				cosmosPacket.Sequence, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !pending {
			markEthEventSeen(seenEvents, key)
			if err := batchBuilder.PendingTracker.RemovePacketIfCurrent(cosmosPacket); err != nil {
				ctx.Logger.Printf("ERROR [eth->cosmos Subscribe][ATTENTION] recovery: failed to persist removal of cleared Cosmos packet seq=%d: %v",
					cosmosPacket.Sequence, err)
			}
			stats.skipped++
			ctx.Logger.Printf("[eth->cosmos Subscribe] recovery: seq=%d already cleared on Cosmos, skipping historical WriteAcknowledgement from ETH block %d",
				cosmosPacket.Sequence, ev.Raw.BlockNumber)
			continue
		}

		if enqueueEthWriteAcknowledgement(batchBuilder, ev, seenEvents) {
			if err := batchBuilder.PendingTracker.RemovePacketIfCurrent(cosmosPacket); err != nil {
				ctx.Logger.Printf("ERROR [eth->cosmos Subscribe][ATTENTION] recovery: failed to persist removal after recovered WriteAcknowledgement seq=%d: %v",
					cosmosPacket.Sequence, err)
			}
			ctx.Logger.Printf("[eth->cosmos Subscribe] recovery: recovered WriteAcknowledgement seq=%d from ETH block %d",
				cosmosPacket.Sequence, ev.Raw.BlockNumber)
			stats.recovered++
		} else {
			stats.skipped++
		}
	}

	if err := iter.Error(); err != nil {
		ctx.Logger.Printf("WARN [eth->cosmos Subscribe] recovery: WriteAcknowledgement iterator error in [%d,%d]: %v", startBlock, endBlock, err)
		if firstErr == nil {
			firstErr = err
		}
	}

	if stats.recovered > 0 || stats.skipped > 0 {
		ctx.Logger.Printf("[eth->cosmos Subscribe] recovery scanned [%d,%d] for WriteAcknowledgement: recovered=%d skipped=%d",
			startBlock, endBlock, stats.recovered, stats.skipped)
	}
	return stats, firstErr
}

func advanceRecoveryStart(nextRecoveryStartBlock *uint64, candidate uint64) {
	// Only historical scans move this cursor. Live subscriptions are not a
	// substitute for gap recovery: advancing past a reconnect gap would make the
	// missed logs permanently invisible.
	if candidate > *nextRecoveryStartBlock {
		*nextRecoveryStartBlock = candidate
	}
}

// scanEthRangeInChunks walks [*cursor, endBlock] in span-sized pieces, advancing
// and persisting the cursor after each piece that succeeded.
//
// A provider refusing the span is the one failure it does not simply return.
// Waiting cannot fix it — the identical request is refused again, forever, and
// the ETH recovery scan is the only thing that closes an event-loss window — so
// the span is halved and the SAME piece is retried. Unlike the L2 mirror this
// does not restart the range: the cursor has already been persisted up to the
// last piece that worked, and re-scanning what is durably done would re-offer
// events the batch builder has seen.
//
// chain.Climb is what ends the halving. At a one-block span a provider still
// refusing is no longer a sizing problem, and the failure is reported permanent
// rather than halved into an infinite loop.
func (s *Subscriber) scanEthRangeInChunks(
	stdCtx context.Context,
	ctx ethDeps,
	label string,
	cursor *uint64,
	endBlock uint64,
	span *relayerclient.LogSpan,
	scan func(from, to uint64) (ethRecoveryStats, error),
	persist func(),
) (ethRecoveryStats, error) {
	var combined ethRecoveryStats
	for *cursor <= endBlock {
		if err := stdCtx.Err(); err != nil {
			return combined, err
		}
		width := span.Width(*cursor, endBlock)
		to := *cursor + width - 1
		if to < *cursor || to > endBlock {
			to = endBlock
		}
		stats, err := scan(*cursor, to)
		combined.recovered += stats.recovered
		combined.skipped += stats.skipped
		if err != nil {
			if relayerclient.IsLogRangeRejection(err) {
				next, bottom, ok := chain.Climb(
					chain.NeedsChange(err, chain.HalvingLadder(relayerclient.KnobBlocksPerLogRange, width)),
					0, // each rejection re-anchors on the span that was just refused
				)
				if ok {
					span.Chunk = next.To
					ctx.Logger.Printf("WARN [eth->cosmos Subscribe] %s: provider refused a %d-block log range at [%d,%d]; narrowing to %d: %v",
						label, width, *cursor, to, next.To, err)
					continue // same cursor, smaller span — the cursor never advances on a failure
				}
				ctx.Logger.Printf("WARN [eth->cosmos Subscribe] %s recovery failed at [%d,%d] and the range cannot be narrowed further: %v",
					label, *cursor, to, err)
				return combined, bottom
			}
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] %s recovery failed at [%d,%d]: %v", label, *cursor, to, err)
			return combined, err
		}
		*cursor = to + 1
		persist()
	}
	return combined, nil
}

func (s *Subscriber) recoverEthGapToBlock(
	stdCtx context.Context,
	ctx ethDeps,
	batchBuilder *services.BatchBuilder,
	filterer *contractICS26Router.ContractICS26RouterFilterer,
	nextSendRecoveryStartBlock *uint64,
	nextWriteAckRecoveryStartBlock *uint64,
	endBlock uint64,
	seenEvents map[ethEventKey]struct{},
	quietScans *uint64,
	span *relayerclient.LogSpan,
) error {
	var firstErr error
	found := false
	persist := func() {
		s.persistEthCursors(ctx, batchBuilder, *nextSendRecoveryStartBlock, *nextWriteAckRecoveryStartBlock)
	}

	if endBlock >= *nextSendRecoveryStartBlock {
		stats, err := s.scanEthRangeInChunks(stdCtx, ctx, "SendPacket", nextSendRecoveryStartBlock, endBlock, span,
			func(from, to uint64) (ethRecoveryStats, error) {
				return recoverEthSendPackets(stdCtx, ctx, batchBuilder, filterer, from, to, seenEvents)
			}, persist)
		if err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] SendPacket recovery failed: %v", err)
			firstErr = err
		} else {
			found = found || stats.foundSomething()
		}
	}

	if endBlock >= *nextWriteAckRecoveryStartBlock {
		stats, err := s.scanEthRangeInChunks(stdCtx, ctx, "WriteAcknowledgement", nextWriteAckRecoveryStartBlock, endBlock, span,
			func(from, to uint64) (ethRecoveryStats, error) {
				return recoverEthWriteAcknowledgements(stdCtx, ctx, batchBuilder, filterer, from, to, seenEvents)
			}, persist)
		if err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] WriteAcknowledgement recovery failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			found = found || stats.foundSomething()
		}
	}

	// Mirror of the Cosmos side: a scan that found nothing says nothing, except a
	// periodic heartbeat so "gap recovery is alive" stays observable.
	if firstErr == nil {
		if found {
			*quietScans = 0
		} else if beat := advanceQuietScans(quietScans); beat {
			ctx.Logger.Printf("[eth->cosmos Subscribe] gap recovery healthy: %d consecutive scans found nothing, now current at block %d",
				*quietScans, endBlock)
		}
		pruneEthSeenEvents(seenEvents, endBlock)
	}
	return firstErr
}

func (s *Subscriber) recoverEthGapToLatest(
	stdCtx context.Context,
	ctx ethDeps,
	batchBuilder *services.BatchBuilder,
	filterer *contractICS26Router.ContractICS26RouterFilterer,
	nextSendRecoveryStartBlock *uint64,
	nextWriteAckRecoveryStartBlock *uint64,
	seenEvents map[ethEventKey]struct{},
	quietScans *uint64,
	span *relayerclient.LogSpan,
	rate *blockRate,
) error {
	rpcCtx, cancel := context.WithTimeout(stdCtx, subscriberRPCTimeout)
	defer cancel()
	latestBlock, err := ctx.EVM.EthClient().BlockNumber(rpcCtx)
	if err != nil {
		return err
	}
	// The head this pass already read is the block-rate sample; see blockRate.
	rate.observe(latestBlock, time.Now())
	return s.recoverEthGapToBlock(
		stdCtx,
		ctx,
		batchBuilder,
		filterer,
		nextSendRecoveryStartBlock,
		nextWriteAckRecoveryStartBlock,
		latestBlock,
		seenEvents,
		quietScans,
		span,
	)
}

func (s *Subscriber) subscribeEthOnce(
	stdCtx context.Context,
	ctx ethDeps,
	batchBuilder *services.BatchBuilder,
	watchClient *ethclient.Client,
	recoveryFilterer *contractICS26Router.ContractICS26RouterFilterer,
	watchStartBlock uint64,
	nextSendRecoveryStartBlock *uint64,
	nextWriteAckRecoveryStartBlock *uint64,
	seenEvents map[ethEventKey]struct{},
	quietScans *uint64,
	span *relayerclient.LogSpan,
	rate *blockRate,
) error {
	watchFilterer, err := contractICS26Router.NewContractICS26RouterFilterer(*ctx.EVM.RouterContract(), watchClient)
	if err != nil {
		return fmt.Errorf("failed to create ICS26Router watch filterer instance: %w", err)
	}

	sendPacketCh := make(chan *contractICS26Router.ContractICS26RouterSendPacket)
	writeAckCh := make(chan *contractICS26Router.ContractICS26RouterWriteAcknowledgement)
	ackPacketCh := make(chan *contractICS26Router.ContractICS26RouterAckPacket)
	timeoutPacketCh := make(chan *contractICS26Router.ContractICS26RouterTimeoutPacket)

	watchOpts := &bind.WatchOpts{Start: &watchStartBlock, Context: stdCtx}
	clientIDFilter := ethEventClientIDFilter(ctx)

	sendPacketSub, err := watchFilterer.WatchSendPacket(watchOpts, sendPacketCh, clientIDFilter, nil)
	if err != nil {
		return fmt.Errorf("failed to subscribe to SendPacket events: %w", err)
	}
	defer sendPacketSub.Unsubscribe()

	writeAckSub, err := watchFilterer.WatchWriteAcknowledgement(watchOpts, writeAckCh, clientIDFilter, nil)
	if err != nil {
		return fmt.Errorf("failed to subscribe to WriteAcknowledgement events: %w", err)
	}
	defer writeAckSub.Unsubscribe()

	ackPacketSub, err := watchFilterer.WatchAckPacket(watchOpts, ackPacketCh, clientIDFilter, nil)
	if err != nil {
		return fmt.Errorf("failed to subscribe to AckPacket events: %w", err)
	}
	defer ackPacketSub.Unsubscribe()

	timeoutPacketSub, err := watchFilterer.WatchTimeoutPacket(watchOpts, timeoutPacketCh, clientIDFilter, nil)
	if err != nil {
		return fmt.Errorf("failed to subscribe to TimeoutPacket events: %w", err)
	}
	defer timeoutPacketSub.Unsubscribe()

	if len(clientIDFilter) > 0 {
		ctx.Logger.Printf("[eth->cosmos Subscribe] Successfully subscribed to ICS26Router events from block %d for client_id=%s",
			watchStartBlock, clientIDFilter[0])
	} else {
		ctx.Logger.Printf("[eth->cosmos Subscribe] Successfully subscribed to ICS26Router events from block %d", watchStartBlock)
	}

	// Starts at the ceiling and narrows once the chain's rate is observable.
	gapRecoveryTicker := time.NewTicker(recoveryTick(0))
	defer gapRecoveryTicker.Stop()

	for {
		select {
		case <-stdCtx.Done():
			return stdCtx.Err()
		case ev, ok := <-sendPacketCh:
			if !ok || ev == nil {
				return fmt.Errorf("SendPacket event channel closed")
			}
			ctx.Logger.Printf("SendPacket event received: clientId=%x, sequence=%s", ev.ClientId, ev.Sequence.String())
			if _, err := enqueueEthSendPacket(batchBuilder, ev, seenEvents); err != nil {
				if ev.Raw.BlockNumber < *nextSendRecoveryStartBlock {
					*nextSendRecoveryStartBlock = ev.Raw.BlockNumber
				}
				return fmt.Errorf("ETH SendPacket seq=%s was not durably tracked; recovery rewound to block %d: %w",
					ev.Sequence.String(), *nextSendRecoveryStartBlock, err)
			}

		case ev, ok := <-writeAckCh:
			if !ok || ev == nil {
				return fmt.Errorf("WriteAcknowledgement event channel closed")
			}
			ctx.Logger.Printf("WriteAcknowledgement event received: clientId=%x, sequence=%s", ev.ClientId, ev.Sequence.String())
			enqueueEthWriteAcknowledgement(batchBuilder, ev, seenEvents)
			cosmosPacket := EthPacketToCosmosPacket(ev.Packet, ev.Sequence)
			if err := batchBuilder.PendingTracker.RemovePacketIfCurrent(cosmosPacket); err != nil {
				ctx.Logger.Printf("ERROR [eth->cosmos Subscribe][ATTENTION] failed to persist removal after WriteAcknowledgement seq=%d: %v",
					cosmosPacket.Sequence, err)
			}

		case ev, ok := <-ackPacketCh:
			if !ok || ev == nil {
				return fmt.Errorf("AckPacket event channel closed")
			}
			ctx.Logger.Printf("AckPacket event received: clientId=%x, sequence=%s", ev.ClientId, ev.Sequence.String())
			enqueueEthTerminal(batchBuilder, ctx.Logger, services.EthAck, ev.Packet, ev.Sequence,
				[][]byte{ev.Acknowledgement}, ev.Raw.BlockNumber)

		case ev, ok := <-timeoutPacketCh:
			if !ok || ev == nil {
				return fmt.Errorf("TimeoutPacket event channel closed")
			}
			ctx.Logger.Printf("TimeoutPacket event received: clientId=%x, sequence=%s", ev.ClientId, ev.Sequence.String())
			enqueueEthTerminal(batchBuilder, ctx.Logger, services.EthTimeout, ev.Packet, ev.Sequence,
				nil, ev.Raw.BlockNumber)

		case err := <-sendPacketSub.Err():
			return fmt.Errorf("SendPacket subscription error: %w", err)

		case err := <-writeAckSub.Err():
			return fmt.Errorf("WriteAcknowledgement subscription error: %w", err)

		case err := <-ackPacketSub.Err():
			return fmt.Errorf("AckPacket subscription error: %w", err)

		case err := <-timeoutPacketSub.Err():
			return fmt.Errorf("TimeoutPacket subscription error: %w", err)

		case <-gapRecoveryTicker.C:
			if err := s.recoverEthGapToLatest(
				stdCtx,
				ctx,
				batchBuilder,
				recoveryFilterer,
				nextSendRecoveryStartBlock,
				nextWriteAckRecoveryStartBlock,
				seenEvents,
				quietScans,
				span,
				rate,
			); err != nil {
				ctx.Logger.Printf("WARN [eth->cosmos Subscribe] periodic recovery failed: %v", err)
			}
			if blockTime, ok := rate.blockTime(); ok {
				gapRecoveryTicker.Reset(recoveryTick(blockTime))
			}
		}
	}
}

// SubscribeEth subscribes to Ethereum events from the ICS26Router contract
func (s *Subscriber) SubscribeEth(stdCtx context.Context, cosmos services.CosmosEndpoint, evm services.EVMEndpoint, ids services.ClientIDs, logger *log.Logger, batchBuilder *services.BatchBuilder) {
	ctx := ethDeps{Cosmos: cosmos, EVM: evm, IDs: ids, Logger: logger}
	if stdCtx.Err() != nil {
		return
	}
	if ctx.EVM.EthWsURL() == "" {
		ctx.Logger.Printf("ERROR Failed to subscribe to Ethereum events: eth websocket URL is not configured")
		return
	}

	recoveryFilterer, err := contractICS26Router.NewContractICS26RouterFilterer(*ctx.EVM.RouterContract(), ctx.EVM.EthClient())
	if err != nil {
		ctx.Logger.Printf("ERROR Failed to create ICS26Router recovery filterer instance: %v", err)
		return
	}

	lookback := ethStartupRecoveryLookbackBlocks()
	var nextSendRecoveryStartBlock uint64
	var nextWriteAckRecoveryStartBlock uint64
	// quietScans counts consecutive recovery passes that found nothing. It lives
	// out here, beside the cursors, so a resubscribe does not reset the heartbeat.
	var quietScans uint64
	// Owned by SubscribeEth's goroutine for the life of the process, so a span the
	// provider refuses is narrowed once instead of being rediscovered — at the cost
	// of one rejected request — on every recovery pass.
	span := relayerclient.LogSpan{Chunk: recoveryChunkSize(ethRecoveryChunkEnv, defaultEthRecoveryChunkBlocks)}
	seenEvents := make(map[ethEventKey]struct{})

	for {
		if stdCtx.Err() != nil {
			return
		}
		rpcCtx, cancel := context.WithTimeout(stdCtx, subscriberRPCTimeout)
		latestBlock, err := ctx.EVM.EthClient().BlockNumber(rpcCtx)
		cancel()
		if err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] Failed to get latest Ethereum block before subscription: %v", err)
			if !sleepOrDone(stdCtx, ethSubscriptionReconnectDelay) {
				return
			}
			continue
		}

		if nextSendRecoveryStartBlock == 0 || nextWriteAckRecoveryStartBlock == 0 {
			send, writeAck := s.resumeEthCursors(ctx, latestBlock, lookback)
			if nextSendRecoveryStartBlock == 0 {
				nextSendRecoveryStartBlock = send
			}
			if nextWriteAckRecoveryStartBlock == 0 {
				nextWriteAckRecoveryStartBlock = writeAck
			}
		}

		if err := s.recoverEthGapToBlock(
			stdCtx,
			ctx,
			batchBuilder,
			recoveryFilterer,
			&nextSendRecoveryStartBlock,
			&nextWriteAckRecoveryStartBlock,
			latestBlock,
			seenEvents,
			&quietScans,
			&span,
		); err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] startup recovery failed: %v", err)
		}

		watchStartBlock := latestBlock + 1

		if stdCtx.Err() != nil {
			return
		}
		dialCtx, cancelDial := context.WithTimeout(stdCtx, subscriberRPCTimeout)
		watchClient, err := ethclient.DialContext(dialCtx, ctx.EVM.EthWsURL())
		cancelDial()
		if err != nil {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] Failed to connect to Ethereum WS at %s: %v", ctx.EVM.EthWsURL(), err)
			if !sleepOrDone(stdCtx, ethSubscriptionReconnectDelay) {
				return
			}
			continue
		}

		// Declared per iteration, not once for the loop: each pass is one
		// subscription, and a rate window must not span two of them. Whatever the
		// chain did while this loop was reconnecting was not observed, so averaging
		// across it reports the size of the gap rather than the rate of the chain.
		// A fresh value per subscription makes that unforgettable -- there is no
		// reset call anyone can drop.
		var rate blockRate

		err = s.subscribeEthOnce(
			stdCtx,
			ctx,
			batchBuilder,
			watchClient,
			recoveryFilterer,
			watchStartBlock,
			&nextSendRecoveryStartBlock,
			&nextWriteAckRecoveryStartBlock,
			seenEvents,
			&quietScans,
			&span,
			&rate,
		)
		watchClient.Close()
		// Mirror of SubscribeCosmos: a requested stop is not a dropped subscription.
		if stdCtx.Err() != nil {
			ctx.Logger.Printf("[eth->cosmos Subscribe] Subscription loop ended: %v", err)
		} else {
			ctx.Logger.Printf("WARN [eth->cosmos Subscribe] Subscription loop ended: %v", err)
		}
		s.persistEthCursors(ctx, batchBuilder, nextSendRecoveryStartBlock, nextWriteAckRecoveryStartBlock)
		if !sleepOrDone(stdCtx, ethSubscriptionReconnectDelay) {
			return
		}
	}
}

func (s *Subscriber) resumeEthCursors(ctx ethDeps, latestBlock, lookback uint64) (send, writeAck uint64) {
	lookbackStart := ethStartupRecoveryStartBlock(latestBlock, lookback)
	if s.recovery == nil {
		return lookbackStart, lookbackStart
	}
	cursors, ok := s.recovery.Get()
	if !ok {
		return lookbackStart, lookbackStart
	}
	send, sendResumed, sendBehind := services.ResumeCursor(cursors[cursorEthSend], lookbackStart, latestBlock)
	writeAck, ackResumed, ackBehind := services.ResumeCursor(cursors[cursorEthWriteAck], lookbackStart, latestBlock)
	if sendResumed || ackResumed {
		ctx.Logger.Printf("[eth->cosmos Subscribe] scanning from send=%d write_ack=%d (persisted send=%d write_ack=%d, head=%d); behind send=%d write_ack=%d",
			send, writeAck, cursors[cursorEthSend], cursors[cursorEthWriteAck], latestBlock, sendBehind, ackBehind)
	}
	return send, writeAck
}

func (s *Subscriber) persistEthCursors(ctx ethDeps, batchBuilder *services.BatchBuilder, send, writeAck uint64) {
	if s.recovery == nil || (send == 0 && writeAck == 0) {
		return
	}
	if batchBuilder != nil {
		floor := batchBuilder.LowestUnsubmittedEthHeight()
		send = clampRecoveryCursor(send, floor)
		writeAck = clampRecoveryCursor(writeAck, floor)
	}
	if err := s.recovery.SaveCheckpoint(services.RecoveryCursors{cursorEthSend: send, cursorEthWriteAck: writeAck}); err != nil {
		ctx.Logger.Printf("WARN [eth->cosmos Subscribe] failed to persist recovery cursors (send=%d write_ack=%d): %v", send, writeAck, err)
	}
}

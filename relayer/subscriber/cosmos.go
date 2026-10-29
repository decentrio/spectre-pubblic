// This file is the Cosmos direction of the subscriber: the live CometBFT
// websocket subscriptions, the TxSearch gap recovery, and the decode/enqueue
// path for Cosmos-origin packet events.
//
// Its mirror is ethereum.go. Function names already pair up across the two --
// resumeCosmosCursor / resumeEthCursors, subscribeCosmosOnce / subscribeEthOnce,
// recoverCosmosGapToLatest / recoverEthGapToLatest -- and splitting the file is
// what makes that pairing something you can diff instead of scroll.
package subscriber

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"relayer/services"

	abcitypes "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	commettypes "github.com/cometbft/cometbft/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/gogo/protobuf/proto"
)

func unsubscribeCosmos(parent context.Context, cosmos services.CosmosEndpoint) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), subscriptionCleanupTimeout)
	defer cancel()
	_ = cosmos.CosmosClient().UnsubscribeAll(cleanupCtx, "")
}

func (s *Subscriber) SubscribeCosmos(stdCtx context.Context, cosmos services.CosmosEndpoint, evm services.EVMEndpoint, ids services.ClientIDs, logger *log.Logger, batchBuilder *services.BatchBuilder) {
	ctx := cosmosDeps{Cosmos: cosmos, EVM: evm, IDs: ids, Logger: logger}
	lookback := cosmosStartupRecoveryLookbackBlocks()
	var nextRecoveryStartHeight uint64
	// quietScans counts consecutive recovery passes that found nothing. It lives
	// beside the cursor, outside the resubscribe loop, so a WS reconnect does not
	// restart the heartbeat — mirroring the Ethereum side.
	var quietScans uint64
	// Outside the resubscribe loop: a reconnect must not reset the picture of
	// whether the live path has been delivering.
	var liveHealth cosmosLiveHealth
	seenEvents := make(map[cosmosEventKey]struct{})

	for {
		if stdCtx.Err() != nil {
			return
		}
		if nextRecoveryStartHeight == 0 {
			latestHeight, err := latestCosmosHeight(stdCtx, ctx)
			if err != nil {
				ctx.Logger.Printf("WARN [SubscribeCosmos] Failed to get latest Cosmos height before subscription: %v", err)
				if !sleepOrDone(stdCtx, cosmosSubscriptionReconnectDelay) {
					return
				}
				continue
			}
			nextRecoveryStartHeight = s.resumeCosmosCursor(ctx, latestHeight, lookback)
		}

		// Declared per iteration, not once for the loop: each pass is one
		// subscription, and a rate window must not span two of them. Whatever the
		// chain did while this loop was reconnecting was not observed, so averaging
		// across it reports the size of the gap rather than the rate of the chain.
		// A fresh value per subscription makes that unforgettable -- there is no
		// reset call anyone can drop.
		var rate blockRate

		err := s.subscribeCosmosOnce(stdCtx, ctx, batchBuilder, &nextRecoveryStartHeight, seenEvents, &quietScans, &liveHealth, &rate)
		// A stop the process asked for ends the loop too, and nothing went wrong
		// there: only a subscription that dropped is worth a warning.
		if stdCtx.Err() != nil {
			ctx.Logger.Printf("[SubscribeCosmos] Subscription loop ended: %v", err)
		} else {
			ctx.Logger.Printf("WARN [SubscribeCosmos] Subscription loop ended: %v", err)
		}
		s.persistCosmosCursor(ctx, batchBuilder, nextRecoveryStartHeight)
		if !sleepOrDone(stdCtx, cosmosSubscriptionReconnectDelay) {
			return
		}
	}
}

func (s *Subscriber) resumeCosmosCursor(ctx cosmosDeps, latestHeight, lookback uint64) uint64 {
	lookbackStart := cosmosStartupRecoveryStartHeight(latestHeight, lookback)
	if s.recovery == nil {
		return lookbackStart
	}
	cursors, ok := s.recovery.Get()
	if !ok {
		return lookbackStart
	}
	start, resumed, behind := services.ResumeCursor(cursors[cursorCosmos], lookbackStart, latestHeight)
	if resumed && behind > 0 {
		ctx.Logger.Printf("[SubscribeCosmos] resuming from persisted height %d, %d block(s) behind head %d", start, behind, latestHeight)
	} else if resumed {
		ctx.Logger.Printf("[SubscribeCosmos] persisted cursor %d is inside the lookback window; rescanning from %d", cursors[cursorCosmos], start)
	}
	return start
}

func (s *Subscriber) persistCosmosCursor(ctx cosmosDeps, batchBuilder *services.BatchBuilder, height uint64) {
	if s.recovery == nil || height == 0 {
		return
	}
	if batchBuilder != nil {
		height = clampRecoveryCursor(height, batchBuilder.LowestUnsubmittedCosmosHeight())
	}
	if height == 0 {
		return
	}
	if err := s.recovery.SaveCheckpoint(services.RecoveryCursors{cursorCosmos: height}); err != nil {
		ctx.Logger.Printf("WARN [SubscribeCosmos] failed to persist recovery cursor (height %d): %v", height, err)
	}
}

// cosmosLiveHealth tracks whether the live subscription is actually delivering.
//
// It exists because the failure it detects is invisible by construction: a
// subscription that stops delivering looks exactly like a chain with no traffic.
// Gap recovery quietly picks up the slack, the relayer keeps working, and
// nothing says the primary path is dead -- until recovery misses one too and a
// packet is lost. Recovery deliberately stops short of the head
// (cosmosIndexerLagBlocks), so that miss is not hypothetical.
//
// Owned by one goroutine (the subscribe loop), so it needs no lock.
type cosmosLiveHealth struct {
	events   uint64
	lastSeen time.Time // last live event, or the last successful subscribe
	// sawTraffic records whether the PREVIOUS recovery pass found any IBC event
	// AT ALL -- delivered live (skipped) or missed (recovered).
	//
	// SCOPE: this watchdog measures the SUBSCRIPTION, not one relay path, and
	// sawTraffic and lastSeen are deliberately scoped the same way. The live
	// subscriptions carry no client filter ("send_packet.encoded_packet_hex
	// EXISTS"), recordEvent runs before enqueueCosmosPackets does any filtering,
	// and the three recovery TxSearch queries use the same three predicates. So
	// an event for an unrelated client counts on BOTH sides: it inflates skipped
	// here and it refreshes lastSeen there. That symmetry is what makes the guard
	// correct -- narrowing one side to this relayer's client without narrowing
	// the other would arm the check with traffic that can never clear it.
	//
	// Silence is only evidence of a problem when there was something to deliver.
	// This used to ask whether the CHAIN advanced instead, which is the wrong
	// question on Cosmos: CometBFT produces blocks whether or not any IBC packet
	// moves, so the guard was true on every pass and a healthy subscription over
	// a quiet chain was torn down every cosmosLivePathStaleAfter. Measured on
	// Sepolia<->cosmoshub 2026-09-02: 4 live events in 6h13m, 181 resubscribes,
	// one every 2m04s.
	sawTraffic bool
	// armFromHeight is the chain head observed when this subscription came up.
	// Only heights ABOVE it are the subscription's responsibility, so only
	// evidence from above it may arm sawTraffic. Zero means "not established
	// yet", and nothing arms until it is.
	//
	// This is a HEIGHT rather than a flag on the caller because the two are not
	// the same question. The previous version passed armsWatchdog=false from the
	// startup call and true from every tick, justified by "the startup pass has
	// consumed the lookback and moved the cursor to the head". That is untrue
	// whenever the startup pass errors: subscribeCosmosOnce logs the failure and
	// starts the loop anyway, so the cursor stays inside the pre-subscription
	// lookback and the first tick rescans it WITH arming on. One old event there
	// then arms the watchdog against a subscription that has carried nothing, and
	// two minutes of legitimate quiet tears it down -- the exact false positive
	// this file exists to remove. A boundary cannot be wrong about that, because
	// it describes the heights themselves rather than who is asking.
	armFromHeight uint64
	// staleSince is when the CURRENT outage was first judged stale; zero when the
	// live path is healthy. It survives a reconnect on purpose: the question the
	// ladder answers is "how long has this been broken", not "how long since the
	// last attempt to fix it".
	staleSince time.Time
	// reported is the highest staleness threshold already logged for this outage.
	reported int
}

// recordSubscribed starts the delivery clock at subscribe time.
//
// Without it the clock starts at the first live event, so a subscription that
// NEVER delivers -- the exact case worth catching -- has nothing to measure and
// is never reported. It does not clear staleSince: subscribing is an attempt to
// fix the outage, not evidence that it is over. Only a delivered event is that.
//
// It DOES clear sawTraffic. Evidence gathered before this subscription existed
// cannot be evidence about it: the startup lookback and the gap after a
// reconnect both cover events this subscription was never running to deliver.
// Counting them would arm the stale check against a subscription that has had
// nothing to do yet, and the teardown that follows re-seeds the same gap.
func (h *cosmosLiveHealth) recordSubscribed(now time.Time) {
	h.lastSeen = now
	h.sawTraffic = false
	// The previous subscription's boundary says nothing about this one. The next
	// successful height read establishes a new one; until then nothing arms.
	h.armFromHeight = 0
}

// establishArmFrom records the chain head observed just after subscribing, once
// per subscription. Later passes must not move it: the boundary is where THIS
// subscription began, not where the most recent scan happened to reach.
func (h *cosmosLiveHealth) establishArmFrom(latestHeight uint64) {
	if h.armFromHeight == 0 {
		h.armFromHeight = latestHeight
	}
}

// armsFromHeight reports whether a scan reaching up to chunkEnd covers heights
// this subscription was running to deliver.
func (h *cosmosLiveHealth) armsFromHeight(chunkEnd uint64) bool {
	return h.armFromHeight != 0 && chunkEnd > h.armFromHeight
}

func (h *cosmosLiveHealth) recordEvent() {
	h.events++
	h.lastSeen = time.Now()
	h.staleSince = time.Time{}
	h.reported = 0
}

// recordRecoveryPass records what the recovery scan found, so the NEXT pass can
// tell "nothing to deliver" from "not delivering".
//
// recovered counts events the live path never delivered; skipped counts events
// it had already delivered, plus ones dropped as unrelated, malformed or already
// settled. Every one of them came from a real IBC event in the window, and the
// unfiltered live subscription would have received all of them -- which is the
// only property this signal needs. Both zero proves there was nothing to deliver.
func (h *cosmosLiveHealth) recordRecoveryPass(recovered, skipped uint64) {
	h.sawTraffic = recovered > 0 || skipped > 0
}

// hadTrafficToDeliver reports whether the previous pass saw any IBC event.
//
// It is deliberately the PREVIOUS pass rather than this one: the judgement runs
// before the scan, so that a dead subscription is caught without first waiting
// for recovery to relay a packet the live path already lost.
func (h *cosmosLiveHealth) hadTrafficToDeliver() bool { return h.sawTraffic }

// livePathStale reports whether the live subscription should be treated as dead,
// how long it has been silent, and whether this pass crosses a new reporting
// threshold.
//
// stale drives a reconnect and silence is only for the message; report is
// separate because a permanent outage must keep saying so on a widening ladder
// rather than once. The previous version latched a single bool that was only
// cleared by a live event -- so a subscription that died for good produced
// exactly one warning line for the process's lifetime, and the log then read as
// though it had recovered.
func (h *cosmosLiveHealth) livePathStale(hadTraffic bool, now time.Time) (stale bool, silence time.Duration, report bool) {
	if !hadTraffic {
		return false, 0, false
	}
	silence = now.Sub(h.lastSeen)
	if silence < cosmosLivePathStaleAfter {
		return false, silence, false
	}
	if h.staleSince.IsZero() {
		h.staleSince = now
	}
	if threshold := ageReportThreshold(now.Sub(h.staleSince)); threshold > h.reported {
		h.reported = threshold
		report = true
	}
	return true, silence, report
}

// liveStaleThresholds is the escalation ladder for an outage that a reconnect
// does not fix. Reporting every pass would be 120 lines an hour; reporting once
// is what the old latched flag did, and it read as a recovery.
//
// D6 in the refactor plan unifies this ladder with the relay package's
// proof-failure one under this name; until that runs they are deliberately the
// same steps in two places rather than a new shared package invented here.
var liveStaleThresholds = []time.Duration{
	0,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

// ageReportThreshold returns how many reporting thresholds age has crossed, and
// keeps counting hourly past the last one so an overnight outage leaves a trail.
func ageReportThreshold(age time.Duration) int {
	crossed := 0
	for _, t := range liveStaleThresholds {
		if age >= t {
			crossed++
		}
	}
	if last := liveStaleThresholds[len(liveStaleThresholds)-1]; age >= last {
		crossed += int((age - last) / time.Hour)
	}
	return crossed
}

func (s *Subscriber) subscribeCosmosOnce(
	stdCtx context.Context,
	ctx cosmosDeps,
	batchBuilder *services.BatchBuilder,
	nextRecoveryStartHeight *uint64,
	seenEvents map[cosmosEventKey]struct{},
	quietScans *uint64,
	liveHealth *cosmosLiveHealth,
	rate *blockRate,
) error {
	if err := stdCtx.Err(); err != nil {
		return err
	}
	sendPacketSub, err := ctx.Cosmos.CosmosClient().WSEvents.Subscribe(stdCtx, "", COMETBFT_SEND_PACKET_EVENT, cosmosLiveEventBuffer)
	if err != nil {
		return fmt.Errorf("failed to subscribe to send_packet events: %w", err)
	}
	ackPacketSub, err := ctx.Cosmos.CosmosClient().WSEvents.Subscribe(stdCtx, "", COMETBFT_WRITE_ACK_PACKET_EVENT, cosmosLiveEventBuffer)
	if err != nil {
		unsubscribeCosmos(stdCtx, ctx.Cosmos)
		return fmt.Errorf("failed to subscribe to write_acknowledgement events: %w", err)
	}
	timeoutPacketSub, err := ctx.Cosmos.CosmosClient().WSEvents.Subscribe(stdCtx, "", COMETBFT_TIMEOUT_PACKET_EVENT, cosmosLiveEventBuffer)
	if err != nil {
		unsubscribeCosmos(stdCtx, ctx.Cosmos)
		return fmt.Errorf("failed to subscribe to timeout_packet events: %w", err)
	}
	settledPacketSub, err := ctx.Cosmos.CosmosClient().WSEvents.Subscribe(stdCtx, "", COMETBFT_ACK_PACKET_EVENT, cosmosLiveEventBuffer)
	if err != nil {
		unsubscribeCosmos(stdCtx, ctx.Cosmos)
		return fmt.Errorf("failed to subscribe to acknowledge_packet events: %w", err)
	}
	ctx.Logger.Println("[SubscribeCosmos] Successfully subscribed to CometBFT events")
	defer unsubscribeCosmos(stdCtx, ctx.Cosmos)
	// The delivery clock starts here, not at the first event: a subscription that
	// never delivers anything is the case this watchdog exists for.
	liveHealth.recordSubscribed(time.Now())

	// This pass reads the historical lookback. It arms nothing, and does not need
	// to be told so: it establishes the boundary from the head it reads, and its
	// whole range sits at or below that. A later tick retrying the same range --
	// which happens whenever this pass fails, since the failure is non-fatal
	// below -- is held off by the same boundary rather than by its caller.
	if _, err := s.recoverCosmosGapToLatest(stdCtx, ctx, batchBuilder, nextRecoveryStartHeight, seenEvents, quietScans, liveHealth, rate); err != nil {
		ctx.Logger.Printf("WARN [SubscribeCosmos] startup recovery failed: %v", err)
	}

	// Starts at the ceiling and narrows once the chain's rate is observable.
	gapRecoveryTicker := time.NewTicker(recoveryTick(0))
	defer gapRecoveryTicker.Stop()

	for {
		select {
		case <-stdCtx.Done():
			return stdCtx.Err()
		case e, ok := <-sendPacketSub:
			if !ok {
				return fmt.Errorf("send_packet subscription channel closed")
			}
			s.processLiveCosmosEvent(stdCtx, ctx, batchBuilder, nextRecoveryStartHeight, seenEvents, liveHealth, e)
		case e, ok := <-ackPacketSub:
			if !ok {
				return fmt.Errorf("write_acknowledgement subscription channel closed")
			}
			s.processLiveCosmosEvent(stdCtx, ctx, batchBuilder, nextRecoveryStartHeight, seenEvents, liveHealth, e)
		case e, ok := <-timeoutPacketSub:
			if !ok {
				return fmt.Errorf("timeout_packet subscription channel closed")
			}
			s.processLiveCosmosEvent(stdCtx, ctx, batchBuilder, nextRecoveryStartHeight, seenEvents, liveHealth, e)
		case e, ok := <-settledPacketSub:
			if !ok {
				return fmt.Errorf("acknowledge_packet subscription channel closed")
			}
			s.processLiveCosmosEvent(stdCtx, ctx, batchBuilder, nextRecoveryStartHeight, seenEvents, liveHealth, e)
		case <-gapRecoveryTicker.C:
			stale, err := s.recoverCosmosGapToLatest(stdCtx, ctx, batchBuilder, nextRecoveryStartHeight, seenEvents, quietScans, liveHealth, rate)
			if err != nil {
				ctx.Logger.Printf("WARN [SubscribeCosmos] periodic recovery failed: %v", err)
			}
			if blockTime, ok := rate.blockTime(); ok {
				gapRecoveryTicker.Reset(recoveryTick(blockTime))
			}
			// CometBFT never closes the Go channel when a subscription dies -- not
			// when the node cancels it for exceeding its buffer, and not when a
			// resubscribe fails -- so the three cases above cannot detect this.
			// Returning is what lets the outer loop build a new subscription; the
			// alternative is waiting for a close that never comes.
			if stale {
				return fmt.Errorf("live subscription delivered nothing for %s while the chain advanced; reconnecting",
					cosmosLivePathStaleAfter)
			}
		}
	}
}

func (s *Subscriber) processLiveCosmosEvent(
	stdCtx context.Context,
	ctx cosmosDeps,
	batchBuilder *services.BatchBuilder,
	nextRecoveryStartHeight *uint64,
	seenEvents map[cosmosEventKey]struct{},
	liveHealth *cosmosLiveHealth,
	e coretypes.ResultEvent,
) {
	height := txHeightFromEvent(e.Data, e.Events)
	if height > 0 && *nextRecoveryStartHeight > 0 && height > *nextRecoveryStartHeight {
		stats, err := recoverCosmosEvents(stdCtx, ctx, batchBuilder, *nextRecoveryStartHeight, height-1, seenEvents)
		if err != nil {
			ctx.Logger.Printf("WARN [SubscribeCosmos] gap recovery before live height %d failed: %v", height, err)
		} else {
			ctx.Logger.Printf("[SubscribeCosmos] gap recovery complete before live height %d: recovered=%d skipped=%d",
				height, stats.recovered, stats.skipped)
			*nextRecoveryStartHeight = height
			s.persistCosmosCursor(ctx, batchBuilder, *nextRecoveryStartHeight)
		}
	}

	liveHealth.recordEvent()

	packets := decodeCosmosPacketsFromEvents(ctx.Logger, e.Data, e.Events, "SubscribeCosmos", "")
	stats, err := enqueueCosmosPackets(stdCtx, ctx, batchBuilder, packets, seenEvents, false)
	if err != nil {
		ctx.Logger.Printf("WARN [SubscribeCosmos] live enqueue failed: %v", err)
	}
	if stats.skipped > 0 {
		ctx.Logger.Printf("[SubscribeCosmos] skipped %d live Cosmos event(s)", stats.skipped)
	}
	if height > 0 {
		pruneCosmosSeenEvents(seenEvents, height)
	}
}

type cosmosEventKey struct {
	PacketType        services.CosmosPacketType
	SourceClient      string
	DestinationClient string
	Sequence          uint64
	Height            uint64
}

type cosmosRecoveryStats struct {
	recovered uint64
	skipped   uint64
}

type ethEventKey struct {
	EventType   string
	TxHash      string
	LogIndex    uint
	BlockNumber uint64
}

type ethRecoveryStats struct {
	recovered uint64
	skipped   uint64
}

// foundSomething reports whether a recovery pass has anything to say. Every
// branch that runs a scan must fold its result in through this: a pass that
// recovered events while another found none is NOT a quiet scan, and reporting
// it as one prints "found nothing" beside the line that found something.
func (s ethRecoveryStats) foundSomething() bool {
	return s.recovered > 0 || s.skipped > 0
}

func cosmosStartupRecoveryLookbackBlocksFromEnv(raw string) uint64 {
	if raw == "" {
		return defaultCosmosStartupRecoveryLookbackBlocks
	}

	lookback, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return defaultCosmosStartupRecoveryLookbackBlocks
	}

	return lookback
}

func cosmosStartupRecoveryLookbackBlocks() uint64 {
	return cosmosStartupRecoveryLookbackBlocksFromEnv(os.Getenv(cosmosStartupRecoveryLookbackEnv))
}

func cosmosStartupRecoveryStartHeight(latestHeight, lookback uint64) uint64 {
	if latestHeight == 0 {
		return 0
	}
	if lookback == 0 || lookback >= latestHeight {
		return 1
	}
	return latestHeight - lookback + 1
}

func latestCosmosHeight(stdCtx context.Context, ctx cosmosDeps) (uint64, error) {
	rpcCtx, cancel := context.WithTimeout(stdCtx, subscriberRPCTimeout)
	defer cancel()
	status, err := ctx.Cosmos.CosmosClient().Status(rpcCtx)
	if err != nil {
		return 0, err
	}
	if status.SyncInfo.LatestBlockHeight <= 0 {
		return 0, nil
	}
	return uint64(status.SyncInfo.LatestBlockHeight), nil
}

func (s *Subscriber) recoverCosmosGapToLatest(
	stdCtx context.Context,
	ctx cosmosDeps,
	batchBuilder *services.BatchBuilder,
	nextRecoveryStartHeight *uint64,
	seenEvents map[cosmosEventKey]struct{},
	quietScans *uint64,
	liveHealth *cosmosLiveHealth,
	rate *blockRate,
) (stale bool, err error) {
	if *nextRecoveryStartHeight == 0 {
		return false, nil
	}

	latestHeight, err := latestCosmosHeight(stdCtx, ctx)
	if err != nil {
		return false, err
	}
	// The head this pass already read is the block-rate sample; see blockRate.
	rate.observe(latestHeight, time.Now())

	// The first head this subscription manages to read is its boundary: heights
	// above it are the ones the live path was running to deliver. Establishing it
	// here rather than in recordSubscribed keeps it to one RPC -- this pass runs
	// immediately after subscribing -- and a read that fails simply leaves the
	// boundary unset, which arms nothing. That is the safe direction: a delayed
	// watchdog beats one armed by history.
	liveHealth.establishArmFrom(latestHeight)

	// A pass with no newly indexable range is a pass with nothing to deliver, and
	// it must SAY so rather than leave the previous pass's answer standing.
	//
	// The range is empty only when the chain produced no new block the indexer has
	// caught up on, and no new block means no new IBC event in one. Recording (0,0)
	// is therefore a measurement, not a guess. Leaving the old answer latched is
	// what would be the guess: a chain that stops producing would keep the last
	// "there was traffic" verdict alive, and two minutes later tear down a live
	// subscription that is healthy and simply has nothing to carry -- the exact
	// false positive this change exists to remove.
	//
	// Traffic can still exist in the newest cosmosIndexerLagBlocks, which recovery
	// never scans. That is not lost: as the chain advances those heights enter the
	// scannable range and show up as `recovered` if the live path missed them. The
	// watchdog is delayed by the lag window, which it always was.
	scanFrom, scanTo, ok := cosmosScanRange(*nextRecoveryStartHeight, latestHeight)
	if !ok {
		liveHealth.recordRecoveryPass(0, 0)
		return false, nil
	}
	passStart := scanFrom

	// Judge the live path before the scan, not after it. The previous version
	// only asked when recovery had already relayed something -- i.e. after the
	// live path had missed a packet. Recovery stops cosmosIndexerLagBlocks short
	// of the head, so waiting for that evidence means waiting for a window in
	// which a packet can be lost outright. The evidence is still the PREVIOUS
	// pass's, so nothing here waits on the scan below.
	stale, silence, report := liveHealth.livePathStale(liveHealth.hadTrafficToDeliver(), time.Now())
	if report {
		ctx.Logger.Printf("WARN [SubscribeCosmos] ATTENTION: live subscription has delivered nothing for %s "+
			"while the chain advanced to height %d (%d live event(s) this run). Gap recovery is the "+
			"BACKSTOP, not the delivery path -- it stops short of the head, so a packet in the newest "+
			"blocks can still be missed. Reconnecting.",
			silence.Round(time.Second), latestHeight, liveHealth.events)
	}

	var stats, arming cosmosRecoveryStats
	chunkSize := recoveryChunkSize(cosmosRecoveryChunkEnv, defaultCosmosRecoveryChunkHeights)
	for scanFrom <= scanTo {
		chunkEnd := scanFrom + chunkSize - 1
		if chunkEnd < scanFrom || chunkEnd > scanTo {
			chunkEnd = scanTo
		}
		// Never let one chunk straddle the boundary. recoverCosmosEvents reports
		// per-chunk totals with no heights attached, so a straddling chunk would
		// force a choice between arming on its historical half and discarding its
		// live half. Cutting at the boundary keeps every chunk unambiguously one
		// or the other; it happens at most once per subscription.
		if boundary := liveHealth.armFromHeight; boundary >= scanFrom && boundary < chunkEnd {
			chunkEnd = boundary
		}
		chunkStats, err := recoverCosmosEvents(stdCtx, ctx, batchBuilder, scanFrom, chunkEnd, seenEvents)
		if liveHealth.armsFromHeight(chunkEnd) {
			// recoverCosmosEvents accumulates across its three queries BEFORE
			// returning the first error, so a failing chunk still reports what it
			// managed to see. That is real evidence about real heights.
			arming.recovered += chunkStats.recovered
			arming.skipped += chunkStats.skipped
		}
		if err != nil {
			// Evidence from the chunks that already completed must not die with
			// this error. The cursor has advanced past them and been persisted, so
			// no later pass will scan those heights again -- this is the only
			// moment that evidence exists. Dropping it leaves a live subscription
			// recovery just proved dead sitting UNARMED, so the watchdog never
			// fires and the subscription is never reconnected.
			//
			// Only ever arm here, never clear: a pass that stopped early has not
			// finished looking, so (0,0) would assert "nothing to deliver" on the
			// strength of a range it never read.
			if arming.recovered > 0 || arming.skipped > 0 {
				liveHealth.recordRecoveryPass(arming.recovered, arming.skipped)
			}
			return stale, err
		}
		stats.recovered += chunkStats.recovered
		stats.skipped += chunkStats.skipped
		*nextRecoveryStartHeight = chunkEnd + 1
		s.persistCosmosCursor(ctx, batchBuilder, *nextRecoveryStartHeight)
		pruneCosmosSeenEvents(seenEvents, chunkEnd)
		scanFrom = *nextRecoveryStartHeight
	}
	// Report a scan that FOUND something, and otherwise only a periodic heartbeat.
	// This ticks every 30s for the life of the process, and the overwhelmingly
	// common outcome is recovered=0 skipped=0 — two lines a tick, ~5.8k lines a day
	// per direction, burying the events worth reading. The heartbeat keeps "gap
	// recovery is alive and current" observable without the repetition.
	// What this pass saw is what the next pass judges silence against.
	liveHealth.recordRecoveryPass(arming.recovered, arming.skipped)
	if stats.recovered > 0 || stats.skipped > 0 {
		ctx.Logger.Printf("[SubscribeCosmos] recovery scanned [%d,%d]: recovered=%d skipped=%d",
			passStart, scanTo, stats.recovered, stats.skipped)
		*quietScans = 0
	} else if beat := advanceQuietScans(quietScans); beat {
		ctx.Logger.Printf("[SubscribeCosmos] gap recovery healthy: %d consecutive scans found nothing, now current at height %d",
			*quietScans, scanTo)
	}

	return stale, nil
}

// cosmosScanRange decides the height range one recovery pass covers, given the
// cursor and the chain head. ok is false when nothing can be scanned safely yet.
//
// The whole decision lives here, in one pure function, because the property that
// matters is not "the arithmetic is right" but "the scan never reads up to the
// head" -- and a helper the caller could stop calling would let that property be
// removed without a test noticing.
func cosmosScanRange(cursor, latestHeight uint64) (from, to uint64, ok bool) {
	if cursor == 0 {
		return 0, 0, false
	}
	to = cosmosIndexedHeight(latestHeight)
	if to < cursor {
		return 0, 0, false
	}
	return cursor, to, true
}

// cosmosIndexedHeight is the newest height gap recovery may scan: far enough
// behind the head that the tx indexer has certainly caught up. Returns 0 on a
// chain too short to have one, which makes the caller wait.
func cosmosIndexedHeight(latestHeight uint64) uint64 {
	if latestHeight <= cosmosIndexerLagBlocks {
		return 0
	}
	return latestHeight - cosmosIndexerLagBlocks
}

func recoverCosmosEvents(
	stdCtx context.Context,
	ctx cosmosDeps,
	batchBuilder *services.BatchBuilder,
	startHeight uint64,
	endHeight uint64,
	seenEvents map[cosmosEventKey]struct{},
) (cosmosRecoveryStats, error) {
	var combined cosmosRecoveryStats
	if endHeight < startHeight || endHeight == 0 {
		return combined, nil
	}
	if startHeight == 0 {
		startHeight = 1
	}

	queries := []string{
		cometBFTSendPacketTxSearch,
		cometBFTWriteAckPacketTxSearch,
		cometBFTTimeoutPacketTxSearch,
		cometBFTAckPacketTxSearch,
	}

	var firstErr error
	for _, query := range queries {
		stats, err := recoverCosmosEventsForQuery(stdCtx, ctx, batchBuilder, query, startHeight, endHeight, seenEvents)
		combined.recovered += stats.recovered
		combined.skipped += stats.skipped
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return combined, firstErr
}

func recoverCosmosEventsForQuery(
	stdCtx context.Context,
	ctx cosmosDeps,
	batchBuilder *services.BatchBuilder,
	baseQuery string,
	startHeight uint64,
	endHeight uint64,
	seenEvents map[cosmosEventKey]struct{},
) (cosmosRecoveryStats, error) {
	var stats cosmosRecoveryStats
	page := 1
	perPage := cosmosTxSearchPerPage
	query := cosmosTxSearchQuery(baseQuery, startHeight, endHeight)

	for {
		rpcCtx, cancel := context.WithTimeout(stdCtx, subscriberRPCTimeout)
		result, err := ctx.Cosmos.CosmosClient().TxSearch(rpcCtx, query, false, &page, &perPage, "asc")
		cancel()
		if err != nil {
			return stats, fmt.Errorf("TxSearch query %q page %d failed: %w", query, page, err)
		}
		if result == nil || len(result.Txs) == 0 {
			return stats, nil
		}

		for _, tx := range result.Txs {
			if tx.TxResult.Code != 0 {
				continue
			}
			data, events := cosmosEventsFromTxResult(tx)
			packets := decodeCosmosPacketsFromEvents(ctx.Logger, data, events, "SubscribeCosmos", "recovery:")
			enqueued, err := enqueueCosmosPackets(stdCtx, ctx, batchBuilder, packets, seenEvents, true)
			stats.recovered += enqueued.recovered
			stats.skipped += enqueued.skipped
			if err != nil {
				return stats, err
			}
		}

		if page*perPage >= result.TotalCount {
			return stats, nil
		}
		page++
	}
}

func cosmosTxSearchQuery(baseQuery string, startHeight uint64, endHeight uint64) string {
	if startHeight == 0 {
		startHeight = 1
	}
	return fmt.Sprintf("%s AND tx.height >= %d AND tx.height <= %d", baseQuery, startHeight, endHeight)
}

func cosmosEventsFromTxResult(tx *coretypes.ResultTx) (commettypes.TMEventData, map[string][]string) {
	events := map[string][]string{}
	if tx == nil {
		return commettypes.EventDataTx{}, events
	}

	events[EVENT_TX_HEIGHT_FIELD] = []string{strconv.FormatInt(tx.Height, 10)}
	for _, ev := range tx.TxResult.Events {
		for _, attr := range ev.Attributes {
			key := ev.Type + "." + attr.Key
			events[key] = append(events[key], attr.Value)
		}
	}

	return commettypes.EventDataTx{
		TxResult: abcitypes.TxResult{
			Height: tx.Height,
			Index:  tx.Index,
			Tx:     []byte(tx.Tx),
			Result: tx.TxResult,
		},
	}, events
}

func decodeCosmosPacketsFromEvents(
	logger *log.Logger,
	data commettypes.TMEventData,
	events map[string][]string,
	logPrefix string,
	phase string,
) []services.CosmosPacket {
	// One bracketed label, with the phase composed INSIDE it rather than as a
	// second pair of brackets. `[a][b]` reads as a second tier that the label
	// vocabulary does not have; `[a] b` keeps one tier and still says which pass
	// produced the line.
	label := "[" + logPrefix + "]"
	if phase != "" {
		label += " " + phase
	}
	if logger == nil {
		logger = log.Default()
	}

	blockNumber := txHeightFromEvent(data, events)
	packets := make([]services.CosmosPacket, 0)

	sendPacketEvent := events[EVENT_SEND_PACKET_FIELD]
	for _, packetEncodedStr := range sendPacketEvent {
		packetBytes, err := hex.DecodeString(packetEncodedStr)
		if err != nil {
			logger.Printf("ERROR %s send_packet: failed to decode hex: %v", label, err)
			continue
		}

		var packet channeltypesv2.Packet
		err = proto.Unmarshal(packetBytes, &packet)
		if err != nil {
			logger.Printf("ERROR %s send_packet: failed to unmarshal: %v", label, err)
			continue
		}

		logger.Printf("%s send_packet received: seq=%d src=%s",
			logPrefix, packet.Sequence, packet.SourceClient)
		packet.TimeoutTimestamp = normalizeTimeoutSeconds(packet.TimeoutTimestamp)
		packets = append(packets, services.CosmosPacket{
			Type:        services.CosmosSend,
			Packet:      &packet,
			BlockNumber: blockNumber,
		})
	}

	ackPacketEvent := events[EVENT_WRITE_ACK_PACKET_FIELD]
	ackEvent := events[EVENT_ACKNOWLEDGEMENT_FIELD]
	if len(ackPacketEvent) != 0 || len(ackEvent) != 0 {
		if len(ackPacketEvent) != len(ackEvent) {
			logger.Printf("ERROR %s write_ack: packet/ack count mismatch (%d vs %d), skipping",
				logPrefix, len(ackPacketEvent), len(ackEvent))
		} else {
			for i := range ackPacketEvent {
				packetBytes, err := hex.DecodeString(ackPacketEvent[i])
				if err != nil {
					logger.Printf("ERROR %s write_ack: failed to decode packet hex: %v", label, err)
					continue
				}

				var packet channeltypesv2.Packet
				err = proto.Unmarshal(packetBytes, &packet)
				if err != nil {
					logger.Printf("ERROR %s write_ack: failed to unmarshal packet: %v", label, err)
					continue
				}

				ackBytes, err := hex.DecodeString(ackEvent[i])
				if err != nil {
					logger.Printf("ERROR %s write_ack seq=%d: failed to decode ack hex: %v",
						logPrefix, packet.Sequence, err)
					continue
				}

				var acknowledgement channeltypesv2.Acknowledgement
				err = proto.Unmarshal(ackBytes, &acknowledgement)
				if err != nil {
					logger.Printf("ERROR %s write_ack seq=%d: failed to unmarshal ack: %v",
						logPrefix, packet.Sequence, err)
					continue
				}
				if len(acknowledgement.AppAcknowledgements) == 0 {
					logger.Printf("ERROR %s write_ack seq=%d: missing app acknowledgements", label, packet.Sequence)
					continue
				}

				logger.Printf("%s write_ack received: seq=%d src=%s",
					logPrefix, packet.Sequence, packet.SourceClient)
				packet.TimeoutTimestamp = normalizeTimeoutSeconds(packet.TimeoutTimestamp)
				packets = append(packets, services.CosmosPacket{
					Type:        services.CosmosAck,
					Packet:      &packet,
					AckBytes:    acknowledgement.AppAcknowledgements,
					BlockNumber: blockNumber,
				})
			}
		}
	}

	timeoutPacketEvent := events[EVENT_TIMEOUT_PACKET_FIELD]
	for _, packetEncodedStr := range timeoutPacketEvent {
		packetBytes, err := hex.DecodeString(packetEncodedStr)
		if err != nil {
			logger.Printf("ERROR %s timeout: failed to decode hex: %v", label, err)
			continue
		}

		var packet channeltypesv2.Packet
		err = proto.Unmarshal(packetBytes, &packet)
		if err != nil {
			logger.Printf("ERROR %s timeout: failed to unmarshal: %v", label, err)
			continue
		}

		logger.Printf("%s timeout received: seq=%d src=%s",
			logPrefix, packet.Sequence, packet.SourceClient)
		packet.TimeoutTimestamp = normalizeTimeoutSeconds(packet.TimeoutTimestamp)
		packets = append(packets, services.CosmosPacket{
			Type:        services.CosmosTimeout,
			Packet:      &packet,
			BlockNumber: blockNumber,
		})
	}

	settledEvent := events[EVENT_ACK_PACKET_FIELD]
	for _, packetEncodedStr := range settledEvent {
		packetBytes, err := hex.DecodeString(packetEncodedStr)
		if err != nil {
			logger.Printf("ERROR [%s] acknowledge_packet: failed to decode hex: %v", logPrefix, err)
			continue
		}

		var packet channeltypesv2.Packet
		if err := proto.Unmarshal(packetBytes, &packet); err != nil {
			logger.Printf("ERROR [%s] acknowledge_packet: failed to unmarshal: %v", logPrefix, err)
			continue
		}

		logger.Printf("[%s] acknowledge_packet received: seq=%d src=%s (settled)",
			logPrefix, packet.Sequence, packet.SourceClient)
		packet.TimeoutTimestamp = normalizeTimeoutSeconds(packet.TimeoutTimestamp)
		packets = append(packets, services.CosmosPacket{
			Type:        services.CosmosAcknowledged,
			Packet:      &packet,
			BlockNumber: blockNumber,
		})
	}

	return packets
}

func enqueueCosmosPackets(
	stdCtx context.Context,
	ctx cosmosDeps,
	batchBuilder *services.BatchBuilder,
	packets []services.CosmosPacket,
	seenEvents map[cosmosEventKey]struct{},
	recovery bool,
) (cosmosRecoveryStats, error) {
	var stats cosmosRecoveryStats
	for _, packet := range packets {
		if packet.Packet == nil {
			stats.skipped++
			continue
		}
		if !cosmosPacketMatchesConfiguredClient(ctx, packet.Packet) {
			ctx.Logger.Printf("[SubscribeCosmos] %s seq=%d src=%s dst=%s ignored: unrelated client",
				packet.Type, packet.Packet.Sequence, packet.Packet.SourceClient, packet.Packet.DestinationClient)
			stats.skipped++
			continue
		}

		key := cosmosEventKeyForPacket(packet)
		if _, ok := seenEvents[key]; ok {
			stats.skipped++
			continue
		}

		// A terminal event settles the tracker and stops here. It must not reach
		// AddCosmos: the batch is relay work, and this packet's lifecycle is over.
		// The recovery pre-check below is skipped for the same reason -- it asks
		// whether a packet still NEEDS relaying, which is not the question.
		if packet.Type == services.CosmosAcknowledged {
			seenEvents[key] = struct{}{}
			// Persist the terminal fact even when there is no active entry. The
			// packet may already be in a handed-off batch, where DropCosmosQueued
			// cannot reach it; its later tracking attempt must see this tombstone
			// instead of recreating a pending record after the cursor moved on.
			if err := batchBuilder.PendingTracker.MarkSettled(*packet.Packet); err != nil {
				ctx.Logger.Printf("ERROR [SubscribeCosmos][ATTENTION] failed to persist terminal settlement for seq=%d: %v",
					packet.Packet.Sequence, err)
			}
			// The owed-acknowledgement ledger too, not only the pending tracker.
			// This branch runs when the acknowledgement arrived -- whoever relayed
			// it -- and a debt this process recorded is settled by that fact.
			batchBuilder.SettleOwedAck(*packet.Packet)
			// The queue too, not only the tracker: one pass can read a send and,
			// further down the same list, the acknowledge_packet that closes it.
			dequeued := batchBuilder.DropCosmosQueued(*packet.Packet)
			ctx.Logger.Printf("[SubscribeCosmos] send seq=%d settled by acknowledge_packet; dropped from the pending tracker%s",
				packet.Packet.Sequence, queuedSuffix(dequeued))
			stats.recovered++
			continue
		}

		if recovery {
			shouldEnqueue, err := shouldEnqueueRecoveredCosmosPacket(stdCtx, ctx, packet)
			if err != nil {
				return stats, err
			}
			if !shouldEnqueue {
				seenEvents[key] = struct{}{}
				stats.skipped++
				continue
			}
		}

		seenEvents[key] = struct{}{}
		batchBuilder.AddCosmos(packet)
		stats.recovered++
	}
	return stats, nil
}

func cosmosPacketMatchesConfiguredClient(ctx cosmosDeps, packet *channeltypesv2.Packet) bool {
	if packet == nil {
		return false
	}

	ethClientID := ctx.IDs.EVMOnCosmos
	routerClientID := ctx.IDs.CosmosOnEVM
	if ethClientID == "" && routerClientID == "" {
		return true
	}

	return packet.SourceClient == ethClientID ||
		packet.DestinationClient == ethClientID ||
		packet.SourceClient == routerClientID ||
		packet.DestinationClient == routerClientID
}

func shouldEnqueueRecoveredCosmosPacket(stdCtx context.Context, ctx cosmosDeps, packet services.CosmosPacket) (bool, error) {
	if packet.Packet == nil {
		return false, nil
	}

	switch packet.Type {
	case services.CosmosSend:
		received, err := services.HasEthPacketReceipt(stdCtx, ctx.EVM, *packet.Packet)
		if err != nil {
			return false, fmt.Errorf("seq=%d failed to check ETH packet receipt: %w", packet.Packet.Sequence, err)
		}
		if received {
			ctx.Logger.Printf("[SubscribeCosmos] recovery: seq=%d already received on ETH, skipping historical SendPacket from Cosmos height %d",
				packet.Packet.Sequence, packet.BlockNumber)
			return false, nil
		}
		return true, nil
	case services.CosmosAck:
		pending, err := services.HasPendingEthPacketCommitment(stdCtx, ctx.EVM, *packet.Packet)
		if err != nil {
			return false, fmt.Errorf("seq=%d failed to check ETH packet commitment: %w", packet.Packet.Sequence, err)
		}
		if !pending {
			ctx.Logger.Printf("[SubscribeCosmos] recovery: seq=%d already cleared on ETH, skipping historical WriteAcknowledgement from Cosmos height %d",
				packet.Packet.Sequence, packet.BlockNumber)
			return false, nil
		}
		return true, nil
	case services.CosmosTimeout:
		if !services.ShouldRelayCosmosTimeoutToEth(packet.Packet, ctx.IDs.CosmosOnEVM) {
			ctx.Logger.Printf("[SubscribeCosmos] recovery: seq=%d Cosmos-originated timeout already handled locally, skipping historical TimeoutPacket from Cosmos height %d",
				packet.Packet.Sequence, packet.BlockNumber)
			return false, nil
		}
		pending, err := services.HasPendingEthPacketCommitment(stdCtx, ctx.EVM, *packet.Packet)
		if err != nil {
			return false, fmt.Errorf("seq=%d failed to check ETH packet commitment before timeout recovery: %w", packet.Packet.Sequence, err)
		}
		if !pending {
			ctx.Logger.Printf("[SubscribeCosmos] recovery: seq=%d commitment already cleared on ETH, skipping historical TimeoutPacket from Cosmos height %d",
				packet.Packet.Sequence, packet.BlockNumber)
			return false, nil
		}
		return true, nil
	default:
		return false, nil
	}
}

func cosmosEventKeyForPacket(packet services.CosmosPacket) cosmosEventKey {
	key := cosmosEventKey{
		PacketType: packet.Type,
		Height:     packet.BlockNumber,
	}
	if packet.Packet != nil {
		key.SourceClient = packet.Packet.SourceClient
		key.DestinationClient = packet.Packet.DestinationClient
		key.Sequence = packet.Packet.Sequence
	}
	return key
}

func pruneCosmosSeenEvents(seenEvents map[cosmosEventKey]struct{}, currentHeight uint64) {
	if currentHeight <= cosmosSeenEventRetentionHeights {
		return
	}
	minHeight := currentHeight - cosmosSeenEventRetentionHeights
	for key := range seenEvents {
		if key.Height > 0 && key.Height < minHeight {
			delete(seenEvents, key)
		}
	}
}

func txHeightFromEvent(data commettypes.TMEventData, events map[string][]string) uint64 {
	switch txData := data.(type) {
	case commettypes.EventDataTx:
		if txData.Height > 0 {
			return uint64(txData.Height)
		}
	case *commettypes.EventDataTx:
		if txData != nil && txData.Height > 0 {
			return uint64(txData.Height)
		}
	}

	values := events[EVENT_TX_HEIGHT_FIELD]
	if len(values) == 0 {
		return 0
	}
	height, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil {
		return 0
	}
	return height
}

// EthPacketToCosmosPacket converts an Ethereum ICS26Router packet to a Cosmos IBC v2 packet

// queuedSuffix keeps the settlement line honest: it says the queue was touched
// only when it was. A line that always claims both would be unfalsifiable, and
// the queue case is the rarer one worth noticing.
func queuedSuffix(dequeued bool) string {
	if dequeued {
		return " and from the relay queue"
	}
	return ""
}

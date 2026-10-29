package l2rollup

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/services"
	"relayer/subscriber"

	contractICS26Router "relayer/bindings/ICS26Router"

	"github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
)

// A standalone poll-based L2 event listener (deliberately NOT the ETH
// subscriber.SubscribeEth, which is bound to the L1 endpoint + BatchBuilder via
// shared context). It polls eth_getLogs on the L2 RPC for the ICS26Router
// SendPacket / WriteAcknowledgement events of this source's client id, decodes them
// with the shared ICS26Router bindings, and drives the adapter handler with its own
// pending buffer for re-queued (not-yet-relayable) events. Within one run the cursor is
// gap-recovery-safe: it only advances past a range that scanned successfully, so an RPC
// hiccup re-scans rather than skips (mistake #11).
//
// The cursor is persisted across runs in the process's own cursors.json, beside the
// pending trackers (B6), so a restart resumes where the last successful scan ended
// rather than one startup window below the head. That window remains the FIRST-RUN
// start and the floor for a resumed cursor, so a restart still rescans it.
//
// Without the durable cursor a restart later than the window never rescanned the
// blocks before it, and a packet whose acknowledgement fell outside stayed pending
// forever with nothing in the log to say why (#308). Sources built without a store
// still behave that way, which is right for one-shot and test callers.
//
// The window itself is a DURATION converted to blocks at the chain's measured block
// time, so a fast chain gets more blocks for the same amount of recovery time rather
// than less time for the same number of blocks.
//
// LIMITATION (unsafe head-kind): the cursor only moves FORWARD — it never rewinds on an
// L2 reorg. With head_kind=unsafe a scanned block can be reorged out and replaced; an
// event only present on the losing branch is then missed (the cursor already passed
// it). This is a non-issue for safe/finalized head-kinds (the relayable frontier is
// post-reorg), which is the reason unsafe is the lowest-trust setting.
// cursorL2 keys this source's scan cursor inside the process's shared
// cursors.json. It shares that namespace with the keys subscriber/ declares for
// the Cosmos and ETH scans, so it must stay distinct from them.
//
// LIMITATION, restated here because this is where the cursor is defined and an
// operator picking head_kind will not go looking in the loop below: the cursor
// only moves FORWARD across restarts. It is never rewound on an L2 reorg, so with
// head_kind=unsafe a scanned block can be reorged out and an event that existed
// only on the losing branch is missed. Safe and finalized are unaffected: their
// frontier is post-reorg by construction.
const cursorL2 = "l2"

// l2SubscribeInterval is a var, not a const, purely so tests can shorten the poll
// period; nothing outside the package reassigns it.
var l2SubscribeInterval = 4 * time.Second

const (
	// l2StartupLookbackEnv optionally widens the recovery window below the head at
	// startup. It takes a DURATION ("15m", "2h"), not a block count: the window's
	// job is measured in time on every chain, and the number of blocks that spans
	// is a property of the chain, not of the operator's intent.
	l2StartupLookbackEnv = "L2_STARTUP_LOOKBACK"

	// l2StartupLookbackBlocksEnv is the key this one replaced. It is still read --
	// only to refuse it. See RejectRetiredLookbackEnv.
	l2StartupLookbackBlocksEnv = "L2_STARTUP_LOOKBACK_BLOCKS"

	// blockTimeSampleTimeout bounds ONE historical-header read while sampling the
	// chain's block time, matching the deadline Source.head applies to its own.
	blockTimeSampleTimeout = 15 * time.Second

	// defaultStartupWindow is the baseline rescan window, and it is deliberately
	// the window this code already had -- 256 blocks at OP's ~2s block time. What
	// changes is that every chain now gets the same amount of TIME instead of the
	// same number of blocks.
	//
	// What the durable cursor changes about this: it is now the FIRST-RUN start
	// rather than the resume point (#308). Neither reaches a packet whose only
	// event predates the window, because reconciliation is a filter applied AFTER
	// the scan and its reach is the scan's reach.
	// That distinction is the whole point of the constant. 256 blocks is ~8.5
	// minutes on OP and ~64 SECONDS on Arbitrum Nitro, so the chain that produces
	// blocks fastest -- and therefore emits packets fastest -- had the shortest
	// crash-recovery window. Nothing about a restart is faster on Arbitrum.
	defaultStartupWindow = 512 * time.Second

	// blockTimeSamples is how many recent blocks the block-time measurement spans.
	// The doc's pseudocode says 20; it is wide enough to average out one slow block
	// and narrow enough to stay on any archive-free RPC.
	blockTimeSamples = uint64(20)

	// fallbackBlockTime is used only when the measurement itself fails. It is
	// deliberately the FASTEST block time in the fleet (Arbitrum Nitro), because
	// under-estimating the block time over-estimates the block count, and a window
	// that is too wide only re-offers packets the destination already rejects as
	// duplicates. Too narrow loses them.
	fallbackBlockTime = 250 * time.Millisecond

	// maxMeasuredBlockTime caps what the two-sample average is allowed to claim.
	//
	// Two timestamps cannot tell a slow chain from a fast chain that was idle:
	// if Nitro sits idle for an hour and then bursts, the newest 20 blocks span
	// that hour and the average reads ~180s per block. The 512s window then
	// sizes at TWO blocks, and a packet emitted ten blocks -- and seconds --
	// before the restart falls outside the startup scan. Nothing persists this
	// cursor (see line 29), so that packet is never rediscovered. Found in
	// review.
	//
	// The cap is not a guess at any chain's block time; it is the point past
	// which a measurement stops being usable. Erring low is the safe direction
	// and the same one fallbackBlockTime already takes: under-estimating the
	// block time over-estimates the block count, and a window that is too large
	// is merely slower to scan. 15s is slower than every L2 in the fleet and
	// slower than Ethereum L1, so a real chain is never capped.
	maxMeasuredBlockTime = 15 * time.Second
)

// startupWindow is the time span a restart rescans below the head. It is a
// DOWNTIME allowance and nothing else -- head_kind deliberately does not enter it.
//
// That is worth stating because the opposite is the intuitive answer, and it is
// wrong. The scan is anchored at head(head_kind): Source.head reads
// SafeBlockNumber or FinalizedBlockNumber, not the chain tip. Both ends of
// "head minus window" therefore live in the same delayed view, so the finality
// lag is already absorbed. Across an outage of D the finalized head advances by
// D/blockTime blocks, which is exactly what a window of D covers. A packet still
// above the finalized head at restart has not entered that view at all, and the
// running loop scans it when the head reaches it.
//
// Adding a per-head-kind lag on top would make every finalized run pay for one
// case that is not a crash: an operator CHANGING head_kind between runs. Going
// from finalized to unsafe moves the anchor forward by the finality lag, leaving
// [old finalized head, new unsafe head - window] unscanned. That is a real gap
// and it is not fixed here -- widening every window forever is the wrong price
// for it, and closing it properly needs the head_kind of the previous run, which
// nothing persists yet (see the durable cursor work in #308).
func startupWindow(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return defaultStartupWindow
}

// lookbackBlocks converts a time window into a block count at the given block
// time, rounding UP so the window is never short. A non-positive block time
// cannot be inverted, so it yields no lookback rather than a division by zero.
func lookbackBlocks(window, blockTime time.Duration) uint64 {
	if window <= 0 || blockTime <= 0 {
		return 0
	}
	blocks := (window + blockTime - 1) / blockTime
	return uint64(blocks)
}

// startupWindowFromEnv reads the operator override. An unparseable value is
// ignored with a line rather than failing startup: the window is a recovery
// nicety, and dying here would turn a typo into an outage.
func startupWindowFromEnv(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	window, err := time.ParseDuration(raw)
	if err != nil || window < 0 {
		log.Printf("WARN [start] ignoring invalid %s=%q; using the default window", l2StartupLookbackEnv, raw)
		return 0
	}
	return window
}

// measureBlockTime derives the chain's block time from the timestamps of two
// headers blockTimeSamples apart.
//
// It is measured rather than configured because the alternative is asking an
// operator to know it, and the number they would have to supply is exactly the
// one the chain already reports. Sampling a span rather than adjacent blocks
// averages out a single slow or empty block.
func (s *Source) measureBlockTime(ctx context.Context, head uint64) (time.Duration, error) {
	if head < blockTimeSamples {
		return 0, fmt.Errorf("l2 source: head %d is below the %d-block sample span", head, blockTimeSamples)
	}
	// The same deadline Source.head puts on its own read, and for the same
	// reason. These two calls run on the subscribe goroutine before the cursor is
	// seeded, so an endpoint that accepts the connection and then stops answering
	// blocks the L2 direction before it has scanned anything -- silently, because
	// nothing has been logged yet. DialEthRPC's transport timeout bounds an http
	// endpoint at DefaultRPCTimeout, but a ws:// one has no such bound, and two
	// minutes of nothing at startup is already the wrong answer.
	//
	// A timeout is not fatal: it returns an error, and startupLookback's existing
	// fallback sizes the window at fallbackBlockTime instead.
	cctx, cancel := context.WithTimeout(ctx, blockTimeSampleTimeout)
	defer cancel()
	newer, err := s.eth.HeaderByNumber(cctx, new(big.Int).SetUint64(head))
	if err != nil {
		return 0, fmt.Errorf("l2 source: header at %d: %w", head, err)
	}
	older, err := s.eth.HeaderByNumber(cctx, new(big.Int).SetUint64(head-blockTimeSamples))
	if err != nil {
		return 0, fmt.Errorf("l2 source: header at %d: %w", head-blockTimeSamples, err)
	}
	if newer.Time <= older.Time {
		return 0, fmt.Errorf("l2 source: block timestamps did not advance across %d blocks", blockTimeSamples)
	}
	span := time.Duration(newer.Time-older.Time) * time.Second
	return span / time.Duration(blockTimeSamples), nil
}

// startupLookback resolves the block count to rescan below head for this source.
// A measurement failure falls back to the fastest block time in the fleet rather
// than to a block count, so the window stays a time window either way.
func (s *Source) startupLookback(ctx context.Context, head uint64) uint64 {
	window := startupWindow(startupWindowFromEnv(os.Getenv(l2StartupLookbackEnv)))
	blockTime, err := s.measureBlockTime(ctx, head)
	if err != nil {
		log.Printf("WARN [l2->cosmos Subscribe] block time unmeasurable (%v); sizing the %s startup window at %s per block",
			err, window, fallbackBlockTime)
		blockTime = fallbackBlockTime
	}
	// An idle-inflated average is ACCEPTED by measureBlockTime -- it only
	// rejects timestamps that did not advance -- so the cap, not the error path,
	// is what keeps the window from collapsing.
	if blockTime > maxMeasuredBlockTime {
		log.Printf("[l2->cosmos Subscribe] measured block time %s exceeds the %s cap (an idle span inflates a "+
			"two-sample average); sizing the %s startup window at the cap instead",
			blockTime, maxMeasuredBlockTime, window)
		blockTime = maxMeasuredBlockTime
	}
	return lookbackBlocks(window, blockTime)
}

// Subscribe polls the L2 for ICS26Router packet events and drives handler in batches
// until ctx is cancelled.
func (s *Source) Subscribe(ctx context.Context, handler func(context.Context, []chain.Event) []int) error {
	filterer, err := contractICS26Router.NewContractICS26RouterFilterer(s.router, s.eth)
	if err != nil {
		return fmt.Errorf("l2 source: new ICS26Router filterer: %w", err)
	}

	ticker := time.NewTicker(l2SubscribeInterval)
	defer ticker.Stop()

	// The cursor is seeded on the FIRST SUCCESSFUL head read inside the loop, not
	// before it. Reading it up front made a transient RPC error at startup fatal
	// while the identical error one tick later was logged and retried — the process
	// died on a rate-limit blip during boot, which is exactly when an operator can
	// least tell a transient failure from a misconfiguration. The mirrors already
	// get this right: SubscribeCosmos and SubscribeEth both read their starting
	// height inside the retry loop and only return early for PERMANENT conditions
	// (missing websocket URL, filterer construction) — the same distinction the
	// filterer check above makes. Errors are classified by kind, never by position.
	var (
		from    uint64
		seeded  bool
		pending []chain.Event // events the handler re-queued (not yet relayable)
		// rescan marks the FIRST successful scan of this run, the one that covers
		// the resume window rather than fresh blocks. Its events are historical,
		// so each is reconciled against Cosmos before being relayed. Later scans
		// cover blocks produced while this process was watching, where a query
		// would only confirm what the scan just saw.
		rescan = true
		// lastReconcile paces the re-check of the pending buffer; the zero time
		// makes the first tick with anything pending reconcile immediately.
		lastReconcile time.Time
	)
	// Owned by this goroutine for the life of the loop, so a span the provider
	// refuses is narrowed once rather than on every tick.
	span := relayerclient.LogSpan{Chunk: s.logScanChunk}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		head, err := s.head(ctx)
		if err != nil {
			log.Printf("WARN [l2->cosmos Subscribe] head: %v", err)
			continue // do NOT advance the cursor on failure
		}
		if !seeded {
			// Sized here, not before the loop: it needs a head and two headers, and
			// a transient RPC error at startup must not be fatal (see the comment on
			// the cursor above). Every path below leaves a usable window.
			lookback := s.startupLookback(ctx, head)
			if head > lookback {
				from = head - lookback
			}
			from = s.resumeCursor(from, head)
			seeded = true
			log.Printf("[l2->cosmos Subscribe] polling ICS26Router %s from block %d (%d block lookback, client_id=%s)",
				s.router.Hex(), from, lookback, s.l2ClientID)
		}

		// Only the SCAN is gated on there being a new block range — the pending
		// buffer must be re-offered on every tick regardless. A demand-driven
		// rollup (Arbitrum Nitro seals a block per transaction) stops at its last
		// block when traffic stops, so `head == from-1` becomes the steady state.
		// Skipping the whole iteration there strands every re-queued packet until
		// the next transaction happens to arrive, and the acknowledgement is the
		// worst case: it is the last event its packet writes on the L2, so nothing
		// further will be written and no new block will ever come. The ack then
		// waits forever, its escrow stays locked, and the log says nothing after
		// the first "waiting" line.
		var fresh []chain.Event
		var settled map[settledKey]struct{}
		if head >= from {
			fresh, settled, err = s.scanPacketLogs(ctx, filterer, from, head, &span)
			if err != nil {
				log.Printf("WARN [l2->cosmos Subscribe] scan [%d,%d]: %v", from, head, err)
				continue // do NOT advance the cursor on failure (re-scan next tick)
			}
			from = head + 1 // range consumed; fresh events are now carried in the batch
			if rescan {
				fresh = s.dropAlreadyHandled(ctx, fresh, "startup rescan")
				rescan = false
			}
		}

		// Re-check what the handler could not relay yet. These are the events a
		// second relayer can settle underneath us: they sit here for as long as
		// the destination is behind, and nothing else in the loop would ever ask
		// again whether the work is still outstanding.
		if len(pending) > 0 && time.Since(lastReconcile) >= l2ReconcileInterval {
			pending = s.dropAlreadyHandled(ctx, pending, "pending recheck")
			lastReconcile = time.Now()
		}

		// Persist BEFORE handing the batch to the handler, and never past the
		// lowest event still waiting to be relayed. A packet the handler re-queues
		// exists only in `pending`, in memory: if the cursor advanced past it and
		// the process died, the restart would scan from beyond its block and the
		// packet would never be seen again. The cursor is therefore allowed to
		// move backwards, which is why the store overlays rather than maximises.
		s.persistCursor(from, pending, fresh)

		// Filter BEFORE the handler, and filter the whole batch. A packet settled
		// by this very scan must not reach handleBatch: it would be relayed for
		// nothing and, worse, tracked again -- after settle removed it and after
		// the cursor moved past the terminal log that would clear it. See
		// dropSettled.
		batch := dropSettled(append(pending[:len(pending):len(pending)], fresh...), settled)
		if len(batch) == 0 {
			pending = nil
			continue
		}
		requeue := handler(ctx, batch)
		pending = keepIndexed(batch, requeue)
	}
}

// resumeCursor prefers a durable cursor over the lookback start, on the same
// terms as the Cosmos and ETH mirrors: a cursor older than the lookback extends
// recovery backwards, a newer one still rescans the lookback because events it
// scanned may only have reached memory.
func (s *Source) resumeCursor(lookbackStart, head uint64) uint64 {
	if s.recovery == nil {
		return lookbackStart
	}
	cursors, ok := s.recovery.Get()
	if !ok {
		return lookbackStart
	}
	start, resumed, behind := services.ResumeCursor(cursors[cursorL2], lookbackStart, head)
	if resumed {
		log.Printf("[l2->cosmos Subscribe] resuming from persisted block %d (%d behind head %d)", start, behind, head)
	}
	return start
}

// persistCursor records how far the scan has got, clamped to the lowest block of
// any event not yet relayed. A failure is logged and NOT fatal: the scan is still
// correct in memory, and killing a working relayer because one write failed
// trades a recoverable degradation for an outage.
func (s *Source) persistCursor(next uint64, batches ...[]chain.Event) {
	if s.recovery == nil || next == 0 {
		return
	}
	for _, batch := range batches {
		for _, event := range batch {
			if event.Height > 0 && event.Height < next {
				next = event.Height
			}
		}
	}
	if err := s.recovery.SaveCheckpoint(services.RecoveryCursors{cursorL2: next}); err != nil {
		log.Printf("WARN [l2->cosmos Subscribe] failed to persist scan cursor (block %d): %v", next, err)
	}
}

// scanPacketLogs fetches the SendPacket + WriteAcknowledgement logs of this source's
// client id in [from,to] and maps them to chain.Events, splitting the request into
// log_scan_chunk-sized spans when one is configured.
// chunkSpans splits [from,to] into chunk-sized pieces. A chunk of 0, or one at
// least as wide as the range, means one piece.
//
// It is a function rather than an inline loop because the boundary arithmetic has
// to be exactly right in two ways at once — cover [from,to] with no gap (a gap
// loses an event silently) and never exceed the chunk (the provider rejects it) —
// and because a test of an inline loop can only re-implement it, which proves
// nothing about the loop that runs.
func chunkSpans(from, to, chunk uint64) [][2]uint64 {
	if to < from {
		return nil
	}
	if chunk == 0 || to-from < chunk {
		return [][2]uint64{{from, to}}
	}
	var out [][2]uint64
	for start := from; start <= to; start += chunk {
		end := start + chunk - 1
		if end > to {
			end = to
		}
		out = append(out, [2]uint64{start, end})
	}
	return out
}

// scanNarrowing runs scan over [from,to] in span-sized pieces, halving the span
// and starting over whenever the provider refuses one.
//
// A provider refusing the span is neither transient nor permanent. Waiting cannot
// fix it — the identical request is refused again, forever — but a smaller span
// works, so it is the third class: the change is carried with the failure and
// chain.Climb decides when there is nothing left to halve.
//
// It restarts the whole range rather than resuming after the pieces that
// succeeded, because this scanner has no cursor and no dedupe: its caller
// discards everything on error and re-scans from the same block next tick. That
// is the opposite choice from the ETH mirror in subscriber/ethereum.go, which
// persists a cursor per piece and therefore must NOT re-scan — the difference is
// in what each side can remember, not in the policy.
//
// The settled set is unioned across the pieces and rebuilt from scratch on each
// attempt. Both halves matter: a send in piece 1 can be settled by a terminal log
// in piece 3, and splitting the range must not hide that from dropSettled; while
// a narrowing restart re-reads every piece, so carrying the previous attempt's
// keys over would let a piece that is about to be read again be counted twice.
func scanNarrowing(
	from, to uint64,
	span *relayerclient.LogSpan,
	scan func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error),
) ([]chain.Event, map[settledKey]struct{}, error) {
	if to < from {
		return nil, nil, nil
	}
	for {
		var all []chain.Event
		settled := map[settledKey]struct{}{}
		var failed error
		for _, piece := range chunkSpans(from, to, span.Chunk) {
			events, pieceSettled, err := scan(piece[0], piece[1])
			if err != nil {
				failed = fmt.Errorf("span [%d,%d]: %w", piece[0], piece[1], err)
				break
			}
			all = append(all, events...)
			for key := range pieceSettled {
				settled[key] = struct{}{}
			}
		}
		if failed == nil {
			return all, settled, nil
		}
		if !relayerclient.IsLogRangeRejection(failed) {
			return nil, nil, failed
		}
		width := span.Width(from, to)
		next, bottom, ok := chain.Climb(
			chain.NeedsChange(failed, chain.HalvingLadder(relayerclient.KnobBlocksPerLogRange, width)),
			0, // each rejection re-anchors on the span that was just refused
		)
		if !ok {
			return nil, nil, bottom
		}
		span.Chunk = next.To
		log.Printf("WARN [l2->cosmos Subscribe] provider refused a %d-block log range; narrowing to %d and rescanning [%d,%d]: %v",
			width, next.To, from, to, failed)
	}
}

// scanPacketLogs fetches the SendPacket + WriteAcknowledgement logs of this
// source's client id in [from,to] and maps them to chain.Events.
//
// A failure in any piece fails the whole scan, so the caller leaves its cursor
// untouched: a partially scanned range must never be mistaken for a complete one.
func (s *Source) scanPacketLogs(ctx context.Context, filterer *contractICS26Router.ContractICS26RouterFilterer, from, to uint64, span *relayerclient.LogSpan) ([]chain.Event, map[settledKey]struct{}, error) {
	return scanNarrowing(from, to, span, func(start, end uint64) ([]chain.Event, map[settledKey]struct{}, error) {
		return s.scanPacketLogRange(ctx, filterer, start, end)
	})
}

// scanPacketLogRange is one eth_getLogs pair over a span the provider will serve.
func (s *Source) scanPacketLogRange(ctx context.Context, filterer *contractICS26Router.ContractICS26RouterFilterer, from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
	opts := &bind.FilterOpts{Start: from, End: &to, Context: ctx}
	clientFilter := []string{s.l2ClientID}

	var events []chain.Event

	sends, err := filterer.FilterSendPacket(opts, clientFilter, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("filter SendPacket: %w", err)
	}
	defer sends.Close()
	for sends.Next() {
		if e, ok := l2SendToEvent(sends.Event, s.cosmosWasmClientID); ok {
			events = append(events, e)
		}
	}
	if err := sends.Error(); err != nil {
		return nil, nil, fmt.Errorf("iterate SendPacket: %w", err)
	}

	acks, err := filterer.FilterWriteAcknowledgement(opts, clientFilter, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("filter WriteAcknowledgement: %w", err)
	}
	defer acks.Close()
	for acks.Next() {
		if e, ok := l2AckToEvent(acks.Event, s.cosmosWasmClientID); ok {
			events = append(events, e)
		}
	}
	if err := acks.Error(); err != nil {
		return nil, nil, fmt.Errorf("iterate WriteAcknowledgement: %w", err)
	}

	settled, err := s.scanTerminalLogs(opts, filterer, clientFilter)
	if err != nil {
		return nil, nil, err
	}
	return events, settled, nil
}

// scanTerminalLogs reads the two events that CLOSE a packet's lifecycle on the
// L2 -- AckPacket and TimeoutPacket -- and settles them.
//
// They return no chain.Event on purpose. A terminal event creates no relay work,
// and feeding one to the relay loop would produce an empty "relay" of a packet
// with nothing left to do. Their entire value is that they are free: without
// them a packet another relayer acknowledged stays in our pending tracker until
// a timeout scan happens to query for it.
//
// A nil settle hook makes this a no-op rather than an error: reading the logs
// still costs two eth_getLogs calls, but a source that does not own a tracker
// has nothing to do with them.
func (s *Source) scanTerminalLogs(opts *bind.FilterOpts, filterer *contractICS26Router.ContractICS26RouterFilterer, clientFilter []string) (map[settledKey]struct{}, error) {
	if s.settle == nil {
		return nil, nil
	}
	settled := map[settledKey]struct{}{}

	acked, err := filterer.FilterAckPacket(opts, clientFilter, nil)
	if err != nil {
		return nil, fmt.Errorf("filter AckPacket: %w", err)
	}
	defer acked.Close()
	for acked.Next() {
		s.settleTerminal("AckPacket", true, acked.Event.Packet, acked.Event.Sequence, settled)
	}
	if err := acked.Error(); err != nil {
		return nil, fmt.Errorf("iterate AckPacket: %w", err)
	}

	timedOut, err := filterer.FilterTimeoutPacket(opts, clientFilter, nil)
	if err != nil {
		return nil, fmt.Errorf("filter TimeoutPacket: %w", err)
	}
	defer timedOut.Close()
	for timedOut.Next() {
		s.settleTerminal("TimeoutPacket", false, timedOut.Event.Packet, timedOut.Event.Sequence, settled)
	}
	if err := timedOut.Error(); err != nil {
		return nil, fmt.Errorf("iterate TimeoutPacket: %w", err)
	}
	return settled, nil
}

// settledKey identifies a packet across the two shapes this file handles: the
// chain.Event a send produced, and the router log a terminal event carries.
//
// It is the MARSHALED PACKET, not the client and sequence. Both shapes reach it
// through the same EthPacketToCosmosPacket + proto.Marshal, so the bytes agree
// whenever the packets do -- and disagree when they do not. That matters because
// a client migration keeps the client id and restarts sequences from 1, so
// seq=N names two different packets over the life of one client; a key of
// client+sequence would let a stale terminal event drop the NEW seq=N from the
// batch, and nothing downstream would notice it was never relayed.
type settledKey string

// dropSettled removes packets whose lifecycle closed inside the very range this
// scan just read.
//
// Settling only the tracker is not enough, and the gap is not a small one. A
// scan window that contains BOTH a send and its later AckPacket -- ordinary
// after a restart, since the window is sized to cover the downtime -- settles
// the packet and then hands the send to the relay loop anyway. handleBatch
// relays it and TRACKS IT AGAIN, so the tracker entry comes back after settle
// removed it, and the cursor has already moved past the terminal log that would
// have cleared it. Nothing settles it a second time: the entry stays pending for
// the life of the process and the timeout scanner keeps querying it.
//
// The same applies to a send already sitting in the retry queue that another
// relayer acknowledged, which is why both halves of the batch are filtered and
// not just the fresh ones.
func dropSettled(batch []chain.Event, settled map[settledKey]struct{}) []chain.Event {
	if len(settled) == 0 || len(batch) == 0 {
		return batch
	}
	kept := batch[:0:0]
	for _, e := range batch {
		if _, done := settled[settledKey(e.Raw)]; done {
			log.Printf("[l2->cosmos Subscribe] dropping %s seq=%d from this batch: it settled inside the same scan range",
				e.Type, e.Sequence)
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// settleTerminal converts one terminal log to the packet identity the tracker
// keys on and hands it to the hook.
// acknowledged separates the two terminal endings. Both drop the pending record;
// only an acknowledgement settles an owed one. A timeout is the other ending --
// the packet is refunded and no acknowledgement is owed or coming -- so clearing
// a debt on it would hide a real one.
func (s *Source) settleTerminal(kind string, acknowledged bool, packet contractICS26Router.IICS26RouterMsgsPacket, sequence *big.Int, settled map[settledKey]struct{}) {
	cosmosPacket := subscriber.EthPacketToCosmosPacket(packet, sequence)
	raw, err := proto.Marshal(&cosmosPacket)
	if err != nil {
		// The packet came from the chain's own log, so this cannot happen without
		// the binding and the proto type having diverged. Say so rather than
		// settling a packet identity nobody can reproduce.
		log.Printf("ERROR [l2->cosmos Subscribe] %s seq=%d: marshal for settlement: %v", kind, cosmosPacket.Sequence, err)
		return
	}
	log.Printf("[l2->cosmos Subscribe] %s seq=%d settled; dropped from the pending tracker", kind, cosmosPacket.Sequence)
	settled[settledKey(raw)] = struct{}{}
	s.settle(raw)
	if acknowledged && s.settleAck != nil {
		s.settleAck(raw)
	}
}

// l2SendToEvent maps a SendPacket log to a recv (SendPacket) chain.Event, reusing the
// ETH source's packet decoder. Returns false if the packet cannot be marshaled.
func l2SendToEvent(ev *contractICS26Router.ContractICS26RouterSendPacket, cosmosWasmClientID string) (chain.Event, bool) {
	if cosmosWasmClientID != "" && ev.Packet.DestClient != cosmosWasmClientID {
		return chain.Event{}, false
	}
	pkt := subscriber.EthPacketToCosmosPacket(ev.Packet, ev.Sequence)
	raw, err := pkt.Marshal()
	if err != nil {
		return chain.Event{}, false
	}
	return chain.Event{
		Type:     chain.SendPacket,
		Height:   ev.Raw.BlockNumber,
		Sequence: pkt.Sequence,
		ClientID: pkt.DestinationClient,
		Raw:      raw,
	}, true
}

// l2AckToEvent maps a WriteAcknowledgement log to an AckPacket chain.Event. An empty
// acknowledgement is skipped — it cannot be built into a MsgAcknowledgement (mirrors
// the ETH source's no-ack skip).
func l2AckToEvent(ev *contractICS26Router.ContractICS26RouterWriteAcknowledgement, cosmosWasmClientID string) (chain.Event, bool) {
	if len(ev.Acknowledgements) == 0 {
		return chain.Event{}, false
	}
	if cosmosWasmClientID != "" && ev.Packet.SourceClient != cosmosWasmClientID {
		return chain.Event{}, false
	}
	pkt := subscriber.EthPacketToCosmosPacket(ev.Packet, ev.Sequence)
	raw, err := pkt.Marshal()
	if err != nil {
		return chain.Event{}, false
	}
	return chain.Event{
		Type:     chain.AckPacket,
		Height:   ev.Raw.BlockNumber,
		Sequence: pkt.Sequence,
		ClientID: pkt.DestinationClient,
		Raw:      raw,
		AckBytes: ev.Acknowledgements,
	}, true
}

// keepIndexed returns the events at the given indices (the handler's re-queue set),
// preserving order and ignoring out-of-range indices.
func keepIndexed(events []chain.Event, indices []int) []chain.Event {
	if len(indices) == 0 {
		return nil
	}
	kept := make([]chain.Event, 0, len(indices))
	for _, i := range indices {
		if i >= 0 && i < len(events) {
			kept = append(kept, events[i])
		}
	}
	return kept
}

// RejectRetiredLookbackEnv fails startup when the retired L2_STARTUP_LOOKBACK_BLOCKS
// is still set.
//
// The key was not just renamed, it changed UNITS: it used to name a block count
// and now names a duration. So an old value cannot be honoured even in
// principle -- "256" read as a duration is not 256 blocks, it is a parse error,
// and "256s" would be a window the operator never asked for. Carrying it forward
// would be guessing at intent.
//
// Refusing is the other half of the same argument. An environment variable that
// used to size the crash-recovery window and now does nothing is exactly the
// failure NFR 10 forbids: the relayer would start, run on the default 8m32s, and
// the operator would learn about it only when a packet older than that window
// was never re-scanned. There is nothing to deploy against yet, so this is a
// removal that says so, not a deprecation window.
func RejectRetiredLookbackEnv() error {
	raw, set := os.LookupEnv(l2StartupLookbackBlocksEnv)
	if !set {
		return nil
	}
	return fmt.Errorf(
		"%s=%q is no longer read: the startup recovery window is now a DURATION under %s "+
			"(for example %s=%s), because the same block count is a different amount of time on "+
			"every chain. Unset %s and set %s to the window you want",
		l2StartupLookbackBlocksEnv, raw, l2StartupLookbackEnv,
		l2StartupLookbackEnv, defaultStartupWindow,
		l2StartupLookbackBlocksEnv, l2StartupLookbackEnv)
}

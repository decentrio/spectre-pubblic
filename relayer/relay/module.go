// Package relay drives one relay path (source -> destination) using only the
// chain adapter contract. Chain-specific work (RPC, event decode, proof/attest,
// gap recovery) lives inside the adapters, so this loop is generic and depends
// only on the chain interfaces — no god-object, testable with mock adapters.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"relayer/chain"
	"relayer/services"
	workergroup "relayer/workers"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultRefreshMargin is used only when the destination cannot report a
// trusting period. A margin fixed in absolute time is wrong in both directions:
// against a period measured in days it acts far earlier than needed, and against
// one shorter than the margin itself the condition below is ALWAYS true, so the
// routine refreshes on every tick forever instead of when the client needs it.
//
// The real margin is a fraction of the period the destination reports; see
// services.RefreshSafetyMargin, which is the same rule the refresh-interval
// calculation already uses. Keeping a second rule here is what produced a
// hard-coded 30 minutes with no relation to any client's real trusting period.
const defaultRefreshMargin = 30 * time.Minute

// refreshTick is how often the refresh routine re-checks client expiry.
// It is a var, not a const, only so tests can drive the loop without waiting a
// minute; nothing outside the package reassigns it (same arrangement as
// l2rollup's l2SubscribeInterval).
var refreshTick = time.Minute

// refreshBackoffCeiling is the widest spacing the retry ever uses, whatever the
// client's own window allows. Five minutes fits eight attempts into the default
// one-hour margin and keeps no gap longer than a twelfth of it.
const refreshBackoffCeiling = 5 * time.Minute

// refreshBackoffFloor keeps the retry from becoming a hot loop on a client whose
// margin is seconds wide.
const refreshBackoffFloor = time.Second

// defaultScanInterval is the default timeout scan cadence.
const defaultScanInterval = 30 * time.Second

// defaultAckWatchInterval is how often the owed-acknowledgement records are
// checked, and defaultAckOverdueAfter how long an acknowledgement may be owed
// before it is named.
//
// Fifteen minutes is past every normal round trip on the paths this drives -- a
// finalized L2 frontier is the slowest and sits tens of minutes behind the head,
// so the threshold has to clear that or a perfectly healthy packet would be
// reported as overdue.
const (
	defaultAckWatchInterval = 5 * time.Minute
	defaultAckOverdueAfter  = 15 * time.Minute
)

// defaultFlushInterval is the cadence of the enumeration backstop. Five minutes
// rather than the scan's thirty seconds: each pass is real queries against both
// chains, and since a query finds a packet at ANY age, running it more often
// buys nothing -- unlike the block scan, which only ever sees its own window.
const defaultFlushInterval = 5 * time.Minute

// Periodic-update failure backoff bounds (exponential 1m→15m), mirroring the legacy
// routineBackoff: a persistently failing client-update — e.g. a rotation tx that
// keeps reverting — must not resubmit every interval and drain the signer's gas.
const (
	periodicUpdateBackoffMin = time.Minute
	periodicUpdateBackoffMax = 15 * time.Minute
)

// defaultPeriodicUpdateInterval is the fallback force-rotation cadence when the
// caller does not derive one from the on-chain trusting period (matches the
// legacy DEFAULT_REFRESH_INTERVAL).
const defaultPeriodicUpdateInterval = 24 * time.Hour

// Module drives a single source -> destination relay path.
type Module struct {
	name     string
	clientID string
	src      chain.Source
	dst      chain.Destination
	builder  chain.Builder

	// Optional timeout-recovery wiring (nil when the path has no timeout scanner,
	// e.g. a mock or a permissioned client that cannot time out).
	scan                ScanFunc
	scanInterval        time.Duration
	ackDue              AckDueFunc
	ackSettled          AckSettledFunc
	overdueAcks         OverdueAcksFunc
	ackWatchInterval    time.Duration
	ackOverdueAfter     time.Duration
	flushInterval       time.Duration
	track               TrackFunc
	untrack             UntrackFunc
	pendingSettled      PendingSettledFunc
	markPendingSettled  MarkPendingSettledFunc
	observeClientUpdate ClientUpdateObserver

	// Optional fixed-cadence forced update (nil when the destination client's
	// freshness is fully captured by ClientExpiresAt, e.g. the beacon client).
	periodicUpdate             PeriodicUpdateFunc
	periodicUpdateInterval     time.Duration
	periodicUpdateInitialDelay time.Duration

	// waits records how long each parked packet has been waiting, so the waiting
	// logs report an age and not just a count. Purely observational — it never
	// gates a relay decision.
	waits *waitTracker
	// beats records when each background loop last completed a pass, so a quiet
	// direction can be told from a wedged one without a goroutine dump.
	beats *heartbeats

	// lastHeight is the highest source height whose ClientUpdate we have
	// submitted. It enforces the append-only / monotonic invariant across the
	// event and refresh goroutines, so a lower or stale update (e.g. a builder
	// returning "no update needed") is skipped rather than replayed.
	mu         sync.Mutex
	lastHeight uint64

	// sourceProbeAt is when the source may be asked again after it returned a
	// PERMANENT failure, and sourceBackoff is the interval that produced it. Both
	// zero while the source is healthy. Guarded by batchMu, like lastWait.
	//
	// A permanent source failure -- a misconfigured attestor route, a malformed
	// request, an RPC the daemon does not implement -- cannot be fixed by asking
	// again, but it CAN be fixed by an operator editing config, and nothing tells
	// us when. So it is still probed, just not at the relay loop's own cadence:
	// l2SubscribeInterval is 4s, and re-offering a queued packet at that rate turns
	// one bad config line into ~21,600 attestor calls and log lines a day, each one
	// looking like an ordinary transient RPC failure.
	//
	// This changes only HOW OFTEN the source is asked. It does not drop a packet
	// and does not stop the module, both of which would be decisions about what
	// chain.Permanent means for every adapter rather than about this cost.
	sourceProbeAt time.Time
	sourceBackoff time.Duration

	// clientInactive is set while the destination's light client refuses every
	// message (frozen, or on Cosmos otherwise not Active); clientProbeAt and
	// clientBackoff space the re-checks the same way the source hold does. All
	// three are guarded by batchMu. See client_hold.go.
	//
	// clientHold mirrors the hold for Status, which runs on another goroutine and
	// must not wait on batchMu behind a batch that is proving.
	clientInactive bool
	clientProbeAt  time.Time
	clientBackoff  time.Duration
	clientHold     atomic.Pointer[string]

	// batchMu serializes handleBatch.
	//
	// It has two callers now, not one: the Subscribe callback and the flush loop,
	// on separate goroutines. Without it they can build and submit a client update
	// at the same time, and relay the same packet twice -- the flush enumerates
	// outstanding commitments, which includes packets the scan is delivering at
	// that moment. The lock is held across the whole batch, proving included,
	// because that is the work that must not overlap; a flush waiting on a live
	// batch is the intended outcome.
	//
	// It is NOT m.mu: that one guards lastHeight and is taken inside the batch.
	batchMu sync.Mutex

	// lastWait is the last "not yet relayable" state logged, so a wait that is not
	// progressing prints once instead of once per flush. Guarded by batchMu --
	// handleBatch is its only reader and writer.
	lastWait waitLogState
}

// waitLogState is the identity of a wait: an identical value means nothing changed
// since the last line, so there is nothing new to tell the operator.
//
// description carries the per-packet detail (type, sequence, height, age), so a
// packet whose reported age advances re-logs even while the frontier is static —
// which is the case the age was added to expose.
type waitLogState struct {
	packets     int
	relayable   uint64
	description string
	logged      bool
}

// NewModule wires a relay path. name is a human label for logs; clientID is the
// destination client this path advances. Optional timeout-recovery behavior is
// added via WithTimeoutScanner / WithPacketTracker.
func NewModule(name, clientID string, src chain.Source, dst chain.Destination, builder chain.Builder, opts ...Option) *Module {
	m := &Module{
		name: name, clientID: clientID, src: src, dst: dst, builder: builder,
		waits: newWaitTracker(),
		beats: newHeartbeats(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Run drives the path until ctx is cancelled: it subscribes to source packet
// events (the adapter owns gap recovery) and runs a background refresh routine
// that keeps the destination client from expiring.
func (m *Module) Run(ctx context.Context) error {
	m.checkClientAtStartup(ctx)

	runCtx, cancel := context.WithCancel(ctx)
	workers := workergroup.New()
	subscribeErr := make(chan error, 1)

	workers.Go(workerSubscriber, func() {
		subscribeErr <- m.src.Subscribe(runCtx, m.handleBatch)
	})
	workers.Go(workerRefresh, func() { m.refreshLoop(runCtx) })
	if m.scan != nil {
		workers.Go(workerTimeoutScan, func() { m.scanLoop(runCtx) })
	}
	if m.periodicUpdate != nil {
		workers.Go(workerPeriodicUpdate, func() { m.periodicUpdateLoop(runCtx) })
	}
	if m.overdueAcks != nil {
		workers.Go("ack-watch", func() { m.ackWatchLoop(runCtx) })
	}
	if lister, ok := m.src.(chain.PacketLister); ok && m.flushInterval > 0 {
		workers.Go("packet-flush", func() { m.flushLoop(runCtx, lister) })
	}

	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case err := <-subscribeErr:
		runErr = err
	}
	cancel()

	if running := workers.Drain(workergroup.ShutdownDrainTimeout); len(running) > 0 {
		return fmt.Errorf("relay %s: shutdown drain timed out after %s; workers still running: %v", m.name, workergroup.ShutdownDrainTimeout, running)
	}

	// On SIGTERM the select above takes ctx.Done() and never reads subscribeErr,
	// so whatever Subscribe returned was dropped -- including a report that one of
	// its own goroutines never stopped. The drain has just waited for that worker,
	// so its value is in the buffered channel now; prefer it over a bare
	// cancellation, which the normalisation below would turn into a clean exit.
	if isShutdownCancellation(runErr) {
		select {
		case err := <-subscribeErr:
			if err != nil && !isShutdownCancellation(err) {
				runErr = err
			}
		default:
		}
	}

	if runErr != nil {
		if ctx.Err() != nil && isShutdownCancellation(runErr) {
			return nil
		}
		return fmt.Errorf("relay %s: subscribe: %w", m.name, runErr)
	}
	return nil
}

// isShutdownCancellation reports whether err is the ordinary "we asked it to
// stop" signal, as opposed to something that went wrong while stopping.
func isShutdownCancellation(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// scanLoop periodically runs the timeout-recovery sweep so packets that were
// recv-relayed but never delivered are refunded on the source chain before the
// pending set grows unbounded. It is a no-op path when no scanner is configured
// (Run does not start it in that case).
func (m *Module) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil { // select may pick the tick even when ctx is done
				return
			}
			m.scan(ctx)
			// After the sweep, not before: the beat means "a pass finished", and a
			// sweep that hangs must not look like one that ran.
			m.beats.record(workerTimeoutScan, "")
		}
	}
}

// flushLoop is the enumeration backstop: it asks the source which packets are
// still outstanding and relays the ones the destination has not settled.
//
// Unlike scanLoop it does not run on the first tick alone -- it runs one pass
// IMMEDIATELY. The situation it exists for (a fresh process with no cursor, on a
// path with an old unrelayed packet) is at its worst at startup, and waiting a
// full interval before the first query is waiting exactly where the block scan
// is already blind.
func (m *Module) flushLoop(ctx context.Context, lister chain.PacketLister) {
	m.flushOnce(ctx, lister)
	ticker := time.NewTicker(m.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil { // select may pick the tick even when ctx is done
				return
			}
			m.flushOnce(ctx, lister)
		}
	}
}

// flushOnce runs one enumeration pass.
//
// The requeue list handleBatch returns is deliberately DISCARDED. The query is
// the source of truth here: anything still outstanding is found again by the
// next pass, so keeping a second pending buffer beside the subscriber's would
// duplicate state and give a packet two independent retry clocks.
func (m *Module) flushOnce(ctx context.Context, lister chain.PacketLister) {
	candidates, err := lister.UnrelayedPackets(ctx)
	if err != nil {
		log.Printf("WARN [%s Relay] packet flush: %v", m.name, err)
		// A failed pass beats too, which is heartbeats.record's stated contract.
		// Reported by @DongLieu: this loop recorded nothing at all, so `relayer
		// status` omitted it entirely -- and it is the ENUMERATION backstop, the
		// one that finds packets a lost cursor or an outage hid from the block
		// scan. An operator asking "is anything still looking for my old packet"
		// got no answer at all, which reads as a loop that never started.
		m.beats.record(workerPacketFlush, "enumeration failed: "+err.Error())
		return
	}
	if len(candidates) == 0 {
		m.beats.record(workerPacketFlush, "nothing outstanding")
		return
	}
	// Filter once outside the lock. This is the bulk of the work after downtime
	// -- one receipt query per outstanding commitment -- and holding batchMu
	// across all of it would stall the live relay path for the whole pass.
	unsettled := m.dropSettled(ctx, candidates)
	if len(unsettled) == 0 {
		m.beats.record(workerPacketFlush, strconv.Itoa(len(candidates))+" outstanding, all already settled")
		return
	}

	m.batchMu.Lock()
	defer m.batchMu.Unlock()
	// Re-check under the lock, over the few that survived. The filter above is an
	// optimisation and carries no guarantee: a live subscription batch can relay
	// any of these between that check and this point, and the flush would then
	// submit a duplicate receive -- an on-chain revert and wasted gas, and one
	// that can poison a batch carrying other packets with it.
	unsettled = m.dropSettled(ctx, unsettled)
	if len(unsettled) == 0 {
		m.beats.record(workerPacketFlush, strconv.Itoa(len(candidates))+" outstanding, settled while this pass ran")
		return
	}
	// Worth a line every time. A flush that finds work means the block scan
	// missed it, which is the condition an operator wants to know about even
	// though the packet is now being relayed.
	//
	// Info, not a warning: the enumeration lists every outstanding commitment, so
	// it also returns packets the live path already holds while it waits for the
	// source to become provable. On an Ethereum source that wait outlasts the flush
	// interval, so this fires on an ordinary transfer -- seen in the local devnet
	// run, three packets the subscriber had logged a minute earlier.
	log.Printf("[%s Relay] packet flush found %d packet(s) the scan did not: %s",
		m.name, len(unsettled), describeEvents(unsettled))
	m.beats.record(workerPacketFlush, "relaying "+strconv.Itoa(len(unsettled))+" the scan did not find")
	m.handleBatchLocked(ctx, unsettled)
}

// dropSettled removes candidates the destination has already delivered. An
// outstanding source commitment only says the packet was never acknowledged
// BACK -- the destination may hold a receipt already, with the ack still in
// flight. Relaying those would re-prove and re-submit the same packets every
// pass for as long as the ack takes.
//
// A receipt query that fails KEEPS the packet: dropping on an RPC error would
// silently skip exactly the packet this loop exists to find, and a duplicate
// receive costs gas where a missed packet costs the user their funds.
//
// This is NOT a guarantee on its own -- handleBatch does not re-check receipts.
// flushOnce calls it a second time under batchMu, and that call is what makes
// the decision hold until submission.
func (m *Module) dropSettled(ctx context.Context, candidates []chain.Event) []chain.Event {
	unsettled := make([]chain.Event, 0, len(candidates))
	for _, e := range candidates {
		settled, err := m.packetSettled(ctx, e)
		if err != nil {
			log.Printf("WARN [%s Relay] packet flush: settlement check for %s seq=%d: %v; relaying anyway",
				m.name, e.Type, e.Sequence, err)
			unsettled = append(unsettled, e)
			continue
		}
		if !settled {
			unsettled = append(unsettled, e)
		}
	}
	return unsettled
}

// packetSettled asks the destination the question that fits the event's type.
//
// A destination that implements chain.SettlementChecker answers for every type;
// one that does not can only answer the receive question, and an
// acknowledgement or timeout it is handed is left unsettled -- relayed, and
// answered on chain with a no-op -- rather than judged by a receipt that lives
// on the other chain and would say "not delivered" forever.
func (m *Module) packetSettled(ctx context.Context, e chain.Event) (bool, error) {
	if checker, ok := m.dst.(chain.SettlementChecker); ok {
		return checker.PacketSettled(ctx, e)
	}
	if e.Type != chain.SendPacket {
		return false, nil
	}
	return m.dst.HasPacketReceipt(ctx, e.Raw)
}

// describeEvents names the packets in a flush line, capped like the waiting log
// so a large backlog does not produce a log line of hundreds of entries.
func describeEvents(events []chain.Event) string {
	shown := events
	if len(shown) > waitListLimit {
		shown = shown[:waitListLimit]
	}
	parts := make([]string, 0, len(shown))
	for _, e := range shown {
		parts = append(parts, fmt.Sprintf("%s seq=%d h=%d", e.Type, e.Sequence, e.Height))
	}
	out := strings.Join(parts, ", ")
	if len(events) > len(shown) {
		out += fmt.Sprintf(", +%d more", len(events)-len(shown))
	}
	return out
}

// nextPeriodicUpdateBackoff advances the exponential failure backoff: the first
// failure waits periodicUpdateBackoffMin, each subsequent failure doubles up to
// periodicUpdateBackoffMax. current == 0 means "no prior failure".
func nextPeriodicUpdateBackoff(current time.Duration) time.Duration {
	if current == 0 {
		return periodicUpdateBackoffMin
	}
	return min(current*2, periodicUpdateBackoffMax)
}

// forcedRotationPollInterval waits for a new source block without spending the
// rotation's safety margin on transaction-failure backoff. This path has not
// generated a proof or submitted a transaction. For short trusting periods the
// rotation interval is at most 3/4 of the period, so interval/12 uses at most
// 1/4 of its safety margin. The one-second ceiling keeps repeated caught-up
// results cheap to retry even when the snapshot is already near expiry.
func forcedRotationPollInterval(interval time.Duration) time.Duration {
	return min(time.Second, max(time.Nanosecond, interval/12))
}

// periodicUpdateLoop runs the fixed-cadence forced client update (e.g. the
// pinned-set rotation). It fires on periodicUpdateInterval on success and on an
// exponential backoff (periodicUpdateBackoffMin→Max) after a failure, so a
// transient failure of the infrequent update is retried promptly rather than
// waiting a full interval. A caught-up source is polled separately: no proof or
// transaction failed, and the pinned snapshot still needs its rotation.
func (m *Module) periodicUpdateLoop(ctx context.Context) {
	// The first fire uses the on-chain-freshness-derived initial delay (0 = due
	// now); every fire after that uses the full interval (a success resets the
	// on-chain timestamp to now) or the failure backoff.
	timer := time.NewTimer(m.periodicUpdateInitialDelay)
	defer timer.Stop()
	var backoff time.Duration // 0 = healthy; grows on consecutive failures
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			// select picks a ready case at random, so the timer branch can win even
			// when ctx is already done — re-check so shutdown never starts a rotation
			// tx (the wired RotatePinnedSet does not yet honor ctx itself).
			if ctx.Err() != nil {
				return
			}
			// A frozen client reverts the rotation too; wait it out at the
			// failure ceiling rather than paying for a revert each backoff step.
			if m.clientStillHeld(ctx) {
				next := min(periodicUpdateBackoffMax, m.periodicUpdateInterval)
				m.beats.record(workerPeriodicUpdate, "held: destination client not active; next in "+next.String())
				timer.Reset(next)
				continue
			}
			next := m.periodicUpdateInterval
			err := m.periodicUpdate(ctx)
			switch {
			case err == nil:
				backoff = 0 // success resets the backoff
			case errors.Is(err, services.ErrForcedRotationNotReady):
				backoff = 0
				next = forcedRotationPollInterval(m.periodicUpdateInterval)
				log.Printf("[%s UpdateClient] waiting for a new source block to rotate the pinned set; retrying after %s", m.name, next)
			case m.armClientHoldFrom(ctx, err):
				// Refused by an inactive client: the hold owns the cadence from here,
				// exactly as on the held branch above, instead of a backoff that
				// keeps paying for reverts.
				backoff = 0
				next = min(periodicUpdateBackoffMax, m.periodicUpdateInterval)
				m.beats.record(workerPeriodicUpdate, "held: destination client not active; next in "+next.String())
				timer.Reset(next)
				continue
			default:
				// Exponential backoff on repeated failure (1m→15m) so a persistently
				// failing client-update tx does not resubmit every interval and drain
				// the signer's gas (the legacy routineBackoff). Never slower than the
				// normal cadence (guards a configuration with a shorter interval).
				backoff = nextPeriodicUpdateBackoff(backoff)
				log.Printf("WARN [%s UpdateClient] periodic update: %v; retrying after %s", m.name, err, backoff)
				next = min(backoff, m.periodicUpdateInterval)
			}
			m.beats.record(workerPeriodicUpdate, "next in "+next.String())
			timer.Reset(next)
		}
	}
}

// settleDelivered closes out packets the destination accepted.
//
// It clears each one's wait entry — the packet landed, so any age recorded for
// it must not be reported again (nor inflate a later packet that reuses the same
// sequence after a client re-creation).
//
// It also removes each delivered SendPacket from the pending tracker: once
// received on the destination it can no longer time out, so leaving it in the
// tracker only bloats it and makes the timeout scanner keep querying a receipt
// it will always find. Only paths that supply an untracker
// (cosmos->eth) do this; eth->cosmos removes via its source's terminal
// EthAck/EthTimeout instead.
func (m *Module) settleDelivered(packets []chain.RelayPacket) {
	for _, p := range packets {
		m.waits.clearPacket(p)
		// A delivered receive is the moment the acknowledgement becomes owed, and
		// a relayed acknowledgement is the moment the debt closes. Recording it
		// here rather than where the event was observed is deliberate: an ack is
		// owed because WE delivered the packet, not because we saw it sent.
		switch {
		case p.Type == chain.SendPacket:
			m.handOverDeliveredSend(p)
		case p.Type == chain.AckPacket && m.ackSettled != nil:
			m.ackSettled(p.Packet)
		}
	}
}

// handOverDeliveredSend moves one delivered receive from the timeout obligation
// to the acknowledgement obligation, in the order that leaves a durable record
// of it at every instant.
//
// The owed-ack record is written FIRST, and the pending record is removed only
// once that write is confirmed on disk. The other order -- which this took until
// a review caught it -- has a window where NEITHER durable record exists: the
// timeout obligation is already deleted, the owed-ack record was never written,
// and the receive is committed on the destination, so the packet can no longer
// time out and nothing durable remains from which the missing acknowledgement
// could be noticed. A crash between the two calls produces that state even when
// both writes would have succeeded, so handling the write failure alone is not
// enough -- the ORDER is the fix.
//
// When the write does fail the pending record is KEPT. It is then slightly wrong
// -- the packet cannot time out any more -- but wrong in the recoverable
// direction: the timeout scanner checks for a receipt before acting on it, finds
// one, and removes the record itself. A packet in neither ledger has no such
// path back.
//
// The ledger behind ackDue is SHARED between the two modules on purpose: the
// acknowledgement that settles this debt is relayed by the other one, and only a
// shared ledger lets that settlement cancel a record still queued here. See
// services/ackdue.go.
func (m *Module) handOverDeliveredSend(p chain.RelayPacket) {
	if m.ackDue == nil {
		// No acknowledgement ledger on this path -- a destination with no return
		// leg -- so there is no second obligation to hand this to, and nothing to
		// wait for before releasing the first.
		if m.untrack != nil {
			m.untrack(p.Packet)
		}
		return
	}
	if !m.ackDue(p.Packet) {
		log.Printf("ERROR [%s Relay] ATTENTION: could not durably record the owed acknowledgement for seq=%d; "+
			"keeping the pending record so the packet stays recoverable, and queued for retry",
			m.name, p.Sequence)
		return
	}
	if m.untrack != nil {
		m.untrack(p.Packet)
	}
}

// ackWatchLoop names acknowledgements that never came back. It reports rather
// than acts: an overdue ack is not automatically recoverable -- the packet has a
// receipt on the destination, so it can never be timed out either -- and the
// operator needs the sequence before anything else can be decided.
func (m *Module) ackWatchLoop(ctx context.Context) {
	ticker := time.NewTicker(m.ackWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil { // select may pick the tick even when ctx is done
				return
			}
			overdue := m.overdueAcks(m.ackOverdueAfter)
			if len(overdue) == 0 {
				continue
			}
			shown := overdue
			if len(shown) > waitListLimit {
				shown = shown[:waitListLimit]
			}
			line := strings.Join(shown, ", ")
			if len(overdue) > len(shown) {
				line += fmt.Sprintf(", +%d more", len(overdue)-len(shown))
			}
			log.Printf("WARN [%s Relay] %d acknowledgement(s) owed for more than %s and never seen: %s",
				m.name, len(overdue), m.ackOverdueAfter, line)
		}
	}
}

// allIndices returns [0, n) — every position in a batch (whole-batch re-queue).
func allIndices(n int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// updateClientTo advances the destination client so it covers source height,
// honoring the append-only invariant. It is safe to call from multiple
// goroutines.
// MaxClientUpdateHops re-exports the ceiling so `update-client --max-hops` can
// default to it without cmd reaching into chain for a number. The definition is
// chain.MaxClientUpdateHops -- there is one, and services/client.go's periodic
// refresh climb uses the same one.
const MaxClientUpdateHops = chain.MaxClientUpdateHops

// updateClientTo advances the destination client to height, climbing through
// intermediate updates when one is not enough.
//
// Validator churn can outrun the trust-overlap requirement: when too much of the
// set has rotated since the trusted height, an update straight to the live head
// cannot be verified at all, and the only way across is a chain of updates that
// each overlap the last.
//
// Two climbers already existed: the manual `update-client` command, and the
// periodic pinned-set refresh (services.Worker.RefreshCosmosClient, reached from
// periodicUpdateLoop through Services.RotatePinnedSet). What had none were the
// engine's own paths -- refreshLoop and the packet-driven batch update -- so a
// client that drifted past the threshold stayed stuck there until the rotation
// cadence or a person happened to cross the gap for it, while refreshLoop
// retried and failed every cycle underneath.
//
// The climb is bounded and each step is logged with its number, because linear
// cost with no ceiling is how a fix becomes an outage.
func (m *Module) updateClientTo(ctx context.Context, height uint64) error {
	return m.updateClient(ctx, height, false)
}

// refreshClientTo is updateClientTo for the expiry-driven refresh: every step
// is built with BuildRotation when the builder has one, so the update moves the
// snapshot the destination measures expiry from rather than only its latest
// height (see chain.RotatingBuilder). Packet-driven updates stay on Build --
// forcing the heavy path there would pay for a rotation on every batch.
func (m *Module) refreshClientTo(ctx context.Context, height uint64) error {
	return m.updateClient(ctx, height, true)
}

func (m *Module) updateClient(ctx context.Context, height uint64, forceRotation bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for hop := 0; ; hop++ {
		if hop >= MaxClientUpdateHops {
			return fmt.Errorf(
				"relay %s: client still short of height %d after %d hop(s); the trust overlap has not been re-established",
				m.name, height, hop)
		}
		again, err := m.updateClientStepLocked(ctx, height, forceRotation)
		if err != nil || !again {
			return err
		}
		log.Printf("[%s Relay] client update hop %d landed at %d, short of %d; climbing again",
			m.name, hop, m.lastHeight, height)
	}
}

// updateClientStepLocked performs one update and reports whether another is
// needed. The caller holds m.mu.
//
// again is true ONLY when the builder said it took a hop AND the client actually
// moved. Both halves matter: without the first, an adapter that simply cannot
// prove as far as asked would be climbed at forever; without the second, a hop
// that installs nothing would loop without progress.
//
// A forced step owes a rotation, not a height. "Already covered" is therefore
// never success for it: a packet update can have brought the client to the
// source head while leaving the pinned snapshot -- and so the expiry -- exactly
// where it was. It goes to BuildRotation anyway, and a result that installs
// nothing comes back as services.ErrForcedRotationNotReady, so the refresh
// retries instead of reporting a refresh that did not happen.
func (m *Module) updateClientStepLocked(ctx context.Context, height uint64, forceRotation bool) (again bool, err error) {
	_, rotating := m.builder.(chain.RotatingBuilder)
	owesRotation := forceRotation && rotating
	if height <= m.lastHeight && !owesRotation {
		return false, nil // already covered — nothing to prove
	}

	header, err := m.src.QueryHeader(ctx, height)
	if err != nil {
		return false, fmt.Errorf("query header at %d: %w", height, err)
	}
	update, err := m.buildUpdate(ctx, header, forceRotation)
	if err != nil {
		return false, fmt.Errorf("build client update: %w", err)
	}
	// An empty payload list means "client already current"; the builder still reports
	// its current height, so learn it (seeding lastHeight from on-chain reality)
	// without submitting a tx. This keeps the provability guard correct across a
	// restart, where the client may already cover incoming packets and every
	// Build would otherwise report a no-op the module could not learn from.
	if len(update.Payloads) == 0 {
		if update.Height > m.lastHeight {
			m.lastHeight = update.Height
		}
		m.recordClientUpdate(update)
		if owesRotation {
			return false, rotationNotLanded(update.Height)
		}
		return false, nil
	}
	// A stale update (at or below what we already trust) is skipped without a tx.
	// This is also the anti-spin guard for the climb: a hop that installs nothing
	// ends the loop instead of repeating.
	if update.Height <= m.lastHeight {
		if owesRotation {
			return false, rotationNotLanded(update.Height)
		}
		return false, nil
	}

	if err := m.dst.UpdateClient(ctx, m.clientID, update); err != nil {
		return false, fmt.Errorf("submit client update at %d: %w", update.Height, err)
	}
	m.lastHeight = update.Height
	m.recordClientUpdate(update)
	return update.IsHop, nil
}

// rotationNotLanded reports a forced step that submitted nothing: the rotation
// is still owed, and only a new source block can carry it.
func rotationNotLanded(height uint64) error {
	return fmt.Errorf("rotation build at %d installs nothing: %w", height, services.ErrForcedRotationNotReady)
}

// buildUpdate picks the build the caller needs. forceRotation falls back to
// Build for a builder with no snapshot to rotate, whose plain update already
// moves its expiry.
func (m *Module) buildUpdate(ctx context.Context, header []byte, forceRotation bool) (chain.ClientUpdate, error) {
	if forceRotation {
		if rb, ok := m.builder.(chain.RotatingBuilder); ok {
			return rb.BuildRotation(ctx, header)
		}
	}
	return m.builder.Build(ctx, header)
}

func (m *Module) recordClientUpdate(update chain.ClientUpdate) {
	if m.observeClientUpdate != nil && !update.TrustedAt.IsZero() {
		m.observeClientUpdate(update.TrustedAt)
	}
}

package services

import (
	"context"
	"log"
	"sync"
	"time"
)

// queueReportInterval is a variable so the reporter can be exercised without
// waiting a minute. Production keeps the intentionally low-noise cadence.
var queueReportInterval = time.Minute

// queueReportQuietHeartbeat is how many consecutive UNCHANGED ticks pass before
// one is reported, so a healthy relayer stays visible (~10 min at the 1m
// interval) without printing every tick.
//
// It matches subscriber.quietScanHeartbeat in wall-clock terms rather than in
// count: that loop ticks every 30s and heartbeats every 20 passes, this one
// ticks every minute. The shared property is the cadence an operator reads, not
// the number.
const queueReportQuietHeartbeat = 10

type clientUpdateTimes struct {
	mu          sync.RWMutex
	cosmosOnEVM time.Time
	evmOnCosmos time.Time
}

func (s *Services) ObserveCosmosOnEVMUpdate(trustedAt time.Time) {
	s.clientUpdates.mu.Lock()
	defer s.clientUpdates.mu.Unlock()
	if trustedAt.After(s.clientUpdates.cosmosOnEVM) {
		s.clientUpdates.cosmosOnEVM = trustedAt
	}
}

func (s *Services) ObserveEVMOnCosmosUpdate(trustedAt time.Time) {
	s.clientUpdates.mu.Lock()
	defer s.clientUpdates.mu.Unlock()
	if trustedAt.After(s.clientUpdates.evmOnCosmos) {
		s.clientUpdates.evmOnCosmos = trustedAt
	}
}

// clientUpdateAges reports staleness using the source-chain timestamps trusted
// by each light client, not process uptime.
func (s *Services) clientUpdateAges(now time.Time) (cosmosOnEVM, evmOnCosmos time.Duration, ok bool) {
	s.clientUpdates.mu.RLock()
	defer s.clientUpdates.mu.RUnlock()
	if s.clientUpdates.cosmosOnEVM.IsZero() && s.clientUpdates.evmOnCosmos.IsZero() {
		return 0, 0, false
	}
	if !s.clientUpdates.cosmosOnEVM.IsZero() {
		cosmosOnEVM = now.Sub(s.clientUpdates.cosmosOnEVM)
	}
	if !s.clientUpdates.evmOnCosmos.IsZero() {
		evmOnCosmos = now.Sub(s.clientUpdates.evmOnCosmos)
	}
	return cosmosOnEVM, evmOnCosmos, true
}

type queueState struct {
	cosmosQueued, ethQueued                                   int
	cosmosPending, ethPending, l2Pending                      int
	cosmosTimeoutDead, ethTimeoutDead, l2TimeoutDead          int
	cosmosStuck, ethStuck, l2Stuck                            int
	cosmosWorstDeferrals, ethWorstDeferrals, l2WorstDeferrals int
	cosmosTimeoutDeadAge, ethTimeoutDeadAge, l2TimeoutDeadAge time.Duration
	cosmosRetryPaused, ethRetryPaused, l2RetryPaused          bool
	// ackDueUnwritten is the number of owed-acknowledgement records that have not
	// reached disk. Reported here rather than only at the moment of failure: a
	// storage problem persists, and a single line at the failure scrolls away
	// while the records stay unwritten.
	ackDueUnwritten int
}

func (s *Services) queueStateAt(now time.Time) queueState {
	cosmosQueued, ethQueued := s.BatchBuilder.QueueDepths()
	cosmosStuck, cosmosWorst := s.BatchBuilder.PendingTracker.StuckTimeouts()
	ethStuck, ethWorst := s.BatchBuilder.EthPendingTracker.StuckTimeouts()
	l2Stuck, l2Worst := s.BatchBuilder.L2PendingTracker.StuckTimeouts()
	return queueState{
		ackDueUnwritten:      s.PendingAckDueWrites(),
		cosmosQueued:         cosmosQueued,
		ethQueued:            ethQueued,
		cosmosPending:        s.BatchBuilder.PendingTracker.Len(),
		ethPending:           s.BatchBuilder.EthPendingTracker.Len(),
		l2Pending:            s.BatchBuilder.L2PendingTracker.Len(),
		cosmosTimeoutDead:    s.BatchBuilder.PendingTracker.DeadLetteredTimeouts(),
		ethTimeoutDead:       s.BatchBuilder.EthPendingTracker.DeadLetteredTimeouts(),
		l2TimeoutDead:        s.BatchBuilder.L2PendingTracker.DeadLetteredTimeouts(),
		cosmosStuck:          cosmosStuck,
		ethStuck:             ethStuck,
		l2Stuck:              l2Stuck,
		cosmosWorstDeferrals: cosmosWorst,
		ethWorstDeferrals:    ethWorst,
		l2WorstDeferrals:     l2Worst,
		cosmosTimeoutDeadAge: s.BatchBuilder.PendingTracker.OldestDeadLetteredTimeout(now),
		ethTimeoutDeadAge:    s.BatchBuilder.EthPendingTracker.OldestDeadLetteredTimeout(now),
		l2TimeoutDeadAge:     s.BatchBuilder.L2PendingTracker.OldestDeadLetteredTimeout(now),
		cosmosRetryPaused:    s.BatchBuilder.PendingTracker.PersistenceError() != nil,
		ethRetryPaused:       s.BatchBuilder.EthPendingTracker.PersistenceError() != nil,
		l2RetryPaused:        s.BatchBuilder.L2PendingTracker.PersistenceError() != nil,
	}
}

// counts is the part of a queue state that is a SIGNAL, separated from the part
// that changes on its own.
//
// The *Age fields are deliberately excluded. They advance every tick by
// construction, so comparing whole queueStates would find a difference every
// time and a de-duplicating reporter would de-duplicate nothing -- the same
// mistake D6 documents in the relay package's waiting-line dedupe. The ages
// still print; they just do not decide whether to print.
func (q queueState) counts() queueCounts {
	return queueCounts{
		cosmosQueued: q.cosmosQueued, ethQueued: q.ethQueued,
		cosmosPending: q.cosmosPending, ethPending: q.ethPending, l2Pending: q.l2Pending,
		cosmosTimeoutDead: q.cosmosTimeoutDead, ethTimeoutDead: q.ethTimeoutDead, l2TimeoutDead: q.l2TimeoutDead,
		cosmosStuck: q.cosmosStuck, ethStuck: q.ethStuck, l2Stuck: q.l2Stuck,
		cosmosWorstDeferrals: q.cosmosWorstDeferrals, ethWorstDeferrals: q.ethWorstDeferrals, l2WorstDeferrals: q.l2WorstDeferrals,
		cosmosRetryPaused: q.cosmosRetryPaused, ethRetryPaused: q.ethRetryPaused, l2RetryPaused: q.l2RetryPaused,
		ackDueUnwritten: q.ackDueUnwritten,
	}
}

type queueCounts struct {
	cosmosQueued, ethQueued                                   int
	cosmosPending, ethPending, l2Pending                      int
	cosmosTimeoutDead, ethTimeoutDead, l2TimeoutDead          int
	cosmosStuck, ethStuck, l2Stuck                            int
	cosmosWorstDeferrals, ethWorstDeferrals, l2WorstDeferrals int
	// The paused flags are counts() material by the rule above: they are signal,
	// not something that advances on its own. Including them is what makes a
	// persistence outage ENDING visible -- the ATTENTION line simply stops, and
	// without this the routine lines would stay suppressed and the log would go
	// quiet with nothing saying the state directory came back.
	cosmosRetryPaused, ethRetryPaused, l2RetryPaused bool
	// Same rule, same reason: a count of records that failed to reach disk is
	// signal, not something that advances on its own. Without it the storage
	// problem CLEARING is invisible -- the ATTENTION line just stops, and the
	// routine lines stay suppressed because no other count moved.
	ackDueUnwritten int
}

// reportQueueState prints the routine lines only when something changed, or
// every queueReportQuietHeartbeat ticks so an idle relayer still proves it is
// running. It returns the counts it saw, for the caller to compare next tick.
//
// Measured before this: a six-hour run with an empty queue throughout produced
// 748 [QueueState] lines, two a minute, none of which carried news. That volume
// is what buries the lines worth reading.
//
// The two ATTENTION branches are NOT rate-limited. They fire only when funds are
// stranded or timeouts are stuck, they are already rare, and an operator who
// greps for them must find every occurrence.
func (s *Services) reportQueueState(now time.Time, last *queueCounts, quietTicks *uint64) queueCounts {
	state := s.queueStateAt(now)
	current := state.counts()
	changed := last == nil || current != *last
	if changed {
		*quietTicks = 0
	}
	routine := changed || advanceQuietTicks(quietTicks)

	if routine {
		s.logRoutineQueueLines(now, state)
	}
	s.logQueueAttention(state)
	return current
}

// advanceQuietTicks counts one unchanged tick and reports whether this is the
// one to log.
func advanceQuietTicks(quietTicks *uint64) bool {
	*quietTicks++
	return *quietTicks%queueReportQuietHeartbeat == 0
}

func (s *Services) logRoutineQueueLines(now time.Time, state queueState) {
	log.Printf("[QueueState] queued(cosmos=%d eth=%d) pending(cosmos=%d eth=%d l2=%d) timeout-dead-letter(cosmos=%d eth=%d l2=%d)",
		state.cosmosQueued, state.ethQueued,
		state.cosmosPending, state.ethPending, state.l2Pending,
		state.cosmosTimeoutDead, state.ethTimeoutDead, state.l2TimeoutDead)
	if cosmosOnEVM, evmOnCosmos, ok := s.clientUpdateAges(now); ok {
		log.Printf("[QueueState] last client update: cosmos-on-evm=%s ago, evm-on-cosmos=%s ago",
			cosmosOnEVM.Truncate(time.Second), evmOnCosmos.Truncate(time.Second))
	}
}

func (s *Services) logQueueAttention(state queueState) {
	if state.cosmosTimeoutDead > 0 || state.ethTimeoutDead > 0 || state.l2TimeoutDead > 0 {
		log.Printf("ERROR [QueueState] ATTENTION: timeout submission gave up for cosmos=%d eth=%d l2=%d packet(s) (oldest: cosmos=%s eth=%s l2=%s); their escrowed funds require operator action.",
			state.cosmosTimeoutDead, state.ethTimeoutDead, state.l2TimeoutDead,
			state.cosmosTimeoutDeadAge.Truncate(time.Second), state.ethTimeoutDeadAge.Truncate(time.Second), state.l2TimeoutDeadAge.Truncate(time.Second))
	}
	if state.cosmosStuck > 0 || state.ethStuck > 0 || state.l2Stuck > 0 {
		log.Printf("ERROR [QueueState] ATTENTION: timeout deferrals are stuck for cosmos=%d eth=%d l2=%d packet(s) (most deferrals: cosmos=%d eth=%d l2=%d); check counterparty RPC and light-client health.",
			state.cosmosStuck, state.ethStuck, state.l2Stuck,
			state.cosmosWorstDeferrals, state.ethWorstDeferrals, state.l2WorstDeferrals)
	}
	if state.cosmosRetryPaused || state.ethRetryPaused || state.l2RetryPaused {
		log.Printf("ERROR [QueueState][ATTENTION] timeout retries are paused because pending-state persistence is unavailable (cosmos=%t eth=%t l2=%t); restore the state directory before retries resume.",
			state.cosmosRetryPaused, state.ethRetryPaused, state.l2RetryPaused)
	}
	if state.ackDueUnwritten > 0 {
		log.Printf("ERROR [QueueState][ATTENTION] %d owed-acknowledgement record(s) could not be written durably and are queued for retry; "+
			"until they land, a missing acknowledgement for those packets cannot be reported after a restart.",
			state.ackDueUnwritten)
	}
}

// RunQueueReporter emits an immediate structured queue-state line and then
// reports periodically until the engine context is cancelled. The adapter owns
// the goroutine, so each Services instance has exactly one reporter.
func (s *Services) RunQueueReporter(ctx context.Context) {
	// Both live on this goroutine and nowhere else, so neither needs a lock.
	var quietTicks uint64
	last := s.reportQueueState(time.Now(), nil, &quietTicks)
	ticker := time.NewTicker(queueReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			last = s.reportQueueState(now, &last, &quietTicks)
		}
	}
}

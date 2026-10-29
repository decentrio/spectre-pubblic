package subscriber

import "time"

// The gap-recovery tick used to be one hard-coded 30 seconds for every chain.
// The scan range per tick is [cursor, head], so a fixed interval means the number
// of blocks a missed event can hide in scales with how fast the chain produces
// them: ~2-3 blocks of exposure on Ethereum at 12s, ~120 on a chain at 250ms.
// The interval has to come from the block time, the same way the L2 startup
// window's block count does.
//
// The tick is NOT widened to save work. The range is [cursor, head], so a longer
// tick makes each scan bigger, not cheaper -- and in the steady state the scan is
// nearly free, because seenEvents is checked before any query.
const (
	// recoveryTickBlocks is the exposure the tick targets, in blocks. Three blocks
	// at Ethereum's 12s is 36s, which the ceiling below pins back to the 30s this
	// code already used -- so the chain the constant was originally tuned for keeps
	// exactly its old cadence, and only faster chains change.
	recoveryTickBlocks = 3

	// minRecoveryTick floors the interval so a sub-second chain cannot turn the
	// ticker into a spin loop against the RPC endpoint.
	minRecoveryTick = 5 * time.Second

	// maxRecoveryTick is the interval this code had before the block time entered
	// the calculation, and it stays the ceiling: a slow chain gains nothing from
	// waiting longer, and recovery latency is the thing being bounded.
	maxRecoveryTick = 30 * time.Second
)

// recoveryTick converts an observed block time into a gap-recovery interval.
// An unknown block time (zero or negative) yields the ceiling, which is the
// behaviour every chain had before this existed.
func recoveryTick(blockTime time.Duration) time.Duration {
	if blockTime <= 0 {
		return maxRecoveryTick
	}
	tick := time.Duration(recoveryTickBlocks) * blockTime
	if tick < minRecoveryTick {
		return minRecoveryTick
	}
	if tick > maxRecoveryTick {
		return maxRecoveryTick
	}
	return tick
}

// blockRate estimates a chain's block time from heights the subscriber already
// reads every tick.
//
// It samples rather than querying because the subscriber reads the head on every
// pass anyway: asking the endpoint for two headers to learn something two
// consecutive head reads already say would add RPC calls to a loop whose whole
// purpose is to stay cheap. It also measures the rate the chain is ACTUALLY
// producing at, which is what decides scan volume, rather than a nominal figure.
//
// Owned by the subscribe goroutine that observes it; no lock.
type blockRate struct {
	firstHeight uint64
	firstAt     time.Time
	lastHeight  uint64
	lastAt      time.Time
}

// observe records a head reading. Only a head that actually ADVANCED extends the
// measured window; anything else restarts it.
//
// The window has to restart on an unchanged head, not merely carry the clock
// forward. A demand-driven chain that sits at one height for an hour and then
// produces ten blocks in ten seconds would otherwise be measured as ten blocks
// across an hour -- six minutes a block, pinning the recovery tick to its 30s
// ceiling exactly when the chain became busy and the tick should have narrowed
// to the floor. Arbitrum Nitro idles like that normally.
//
// The cost is smoothing: a window that restarts at the last idle sample measures
// the burst since that sample rather than averaging several. That is the right
// trade here, because the only alternative is averaging across idle time, and
// idle time is not block time.
func (r *blockRate) observe(height uint64, at time.Time) {
	if r.firstAt.IsZero() || height <= r.lastHeight {
		// First reading; a head that went BACKWARDS since the previous sample (an
		// unsafe-head reorg); or a head that did not move at all. Restart the window
		// rather than average across a discontinuity or an idle stretch -- comparing
		// against firstHeight instead would miss a reorg that retreats to a height
		// still above where the window began.
		r.firstHeight, r.firstAt = height, at
		r.lastHeight, r.lastAt = height, at
		return
	}
	r.lastHeight, r.lastAt = height, at
}

// blockTime reports the observed seconds per block, and whether enough has been
// observed to say. It needs at least one block of progress and a non-zero span.
func (r *blockRate) blockTime() (time.Duration, bool) {
	blocks := r.lastHeight - r.firstHeight
	span := r.lastAt.Sub(r.firstAt)
	if blocks == 0 || span <= 0 {
		return 0, false
	}
	return span / time.Duration(blocks), true
}

package subscriber

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"relayer/rpcmock"
	"relayer/services"
	"sync/atomic"
	"testing"
	"time"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
)

// TestRecoveryTick is the point of the change: one exposure target must produce
// a DIFFERENT interval per chain. The old constant did the opposite -- one
// interval produced a wildly different exposure per chain, and the chain that
// produces blocks fastest hid the most blocks behind each tick.
func TestRecoveryTick(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		chain     string
		blockTime time.Duration
		want      time.Duration
		why       string
	}{
		{chain: "unknown block time", blockTime: 0, want: 30 * time.Second, why: "the behaviour every chain had before this existed"},
		{chain: "Ethereum L1", blockTime: 12 * time.Second, want: 30 * time.Second, why: "the chain the 30s was tuned for keeps its cadence"},
		{chain: "Cosmos Hub", blockTime: 6 * time.Second, want: 18 * time.Second, why: ""},
		{chain: "fast Cosmos devnet", blockTime: time.Second, want: 5 * time.Second, why: "floored so the ticker cannot spin"},
		{chain: "Arbitrum Nitro", blockTime: 250 * time.Millisecond, want: 5 * time.Second, why: "floored"},
	} {
		t.Run(tc.chain, func(t *testing.T) {
			t.Parallel()
			if got := recoveryTick(tc.blockTime); got != tc.want {
				t.Fatalf("recoveryTick(%s) = %s, want %s: %s", tc.blockTime, got, tc.want, tc.why)
			}
		})
	}

	t.Run("a faster chain never waits longer", func(t *testing.T) {
		t.Parallel()
		slow := recoveryTick(12 * time.Second)
		fast := recoveryTick(time.Second)
		if fast > slow {
			t.Fatalf("fast chain tick %s > slow chain tick %s; a longer tick makes each scan bigger, not cheaper", fast, slow)
		}
	})

	t.Run("never exceeds the interval this replaced", func(t *testing.T) {
		t.Parallel()
		for _, blockTime := range []time.Duration{time.Minute, time.Hour} {
			if got := recoveryTick(blockTime); got > 30*time.Second {
				t.Fatalf("recoveryTick(%s) = %s; widening the tick only widens the scan range", blockTime, got)
			}
		}
	})
}

// TestBlockRate covers the sampling: it must refuse to report a rate it has not
// observed rather than invent one, because an invented rate resizes the tick.
func TestBlockRate(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)

	t.Run("says nothing before a block has passed", func(t *testing.T) {
		t.Parallel()
		var r blockRate
		if _, ok := r.blockTime(); ok {
			t.Fatal("reported a block time before observing anything")
		}
		r.observe(100, start)
		if _, ok := r.blockTime(); ok {
			t.Fatal("one sample is not a rate")
		}
	})

	t.Run("a chain that stops producing has no rate", func(t *testing.T) {
		t.Parallel()
		var r blockRate
		r.observe(100, start)
		r.observe(100, start.Add(time.Minute))
		if _, ok := r.blockTime(); ok {
			t.Fatal("a head that did not move is not evidence of a block time; Arbitrum Nitro idles like this normally")
		}
	})

	t.Run("averages across the observed window", func(t *testing.T) {
		t.Parallel()
		var r blockRate
		r.observe(100, start)
		r.observe(110, start.Add(20*time.Second))
		got, ok := r.blockTime()
		if !ok {
			t.Fatal("10 blocks over 20s is measurable")
		}
		if got != 2*time.Second {
			t.Fatalf("blockTime = %s, want 2s", got)
		}
	})

	// The exact shape Nitro produces: demand-driven, so the head sits still for a
	// long time and then moves quickly. Carrying the idle clock into the burst
	// charged an hour of nothing to ten real blocks, and the tick stayed at its
	// 30s ceiling at the moment the chain became busy and it should have gone to
	// the floor.
	t.Run("an idle stretch is not charged to the blocks that follow", func(t *testing.T) {
		t.Parallel()
		var r blockRate
		r.observe(100, start)
		for i := 1; i <= 6; i++ { // an hour of a head that does not move
			r.observe(100, start.Add(time.Duration(i)*10*time.Minute))
		}
		idleEnd := start.Add(time.Hour)

		r.observe(110, idleEnd.Add(10*time.Second)) // then ten blocks in ten seconds

		got, ok := r.blockTime()
		if !ok {
			t.Fatal("ten blocks over ten seconds is measurable")
		}
		if got != time.Second {
			t.Fatalf("blockTime = %s, want 1s: the idle hour was averaged into the burst", got)
		}
		if tick := recoveryTick(got); tick >= 30*time.Second {
			t.Fatalf("recoveryTick = %s; the chain just became busy and the tick stayed at its ceiling", tick)
		}
	})

	t.Run("a head that goes backwards restarts the window", func(t *testing.T) {
		t.Parallel()
		// An unsafe-head reorg. Averaging across it would report a negative or
		// nonsense rate and resize the tick from a discontinuity.
		var r blockRate
		r.observe(100, start)
		r.observe(200, start.Add(time.Minute))
		r.observe(150, start.Add(2*time.Minute))
		if _, ok := r.blockTime(); ok {
			t.Fatal("the window must restart at the reorg, leaving one sample")
		}
		r.observe(160, start.Add(3*time.Minute))
		got, ok := r.blockTime()
		if !ok || got != 6*time.Second {
			t.Fatalf("blockTime after the restart = %s (ok=%v), want 6s measured from the reorg", got, ok)
		}
	})

	// TestBlockRate/cosmos recovery feeds the block rate covers the WIRING, not the type. Every
	// test above drives blockRate directly, so all of them stayed green while the
	// Cosmos recovery loop passed its *blockRate around without ever calling
	// observe on a successful head read -- the tick stayed at its fixed fallback
	// forever. The ETH mirror (recoverEthGapToLatest) observes right after its head
	// fetch; this asserts the Cosmos path does the same.
	t.Run("cosmos recovery feeds the block rate", func(t *testing.T) {
		t.Parallel()

		var height atomic.Int64
		height.Store(100)
		node := rpcmock.NewCosmosNode(t)
		node.Handle("status", func(json.RawMessage) (any, error) {
			return &coretypes.ResultStatus{
				SyncInfo: coretypes.SyncInfo{LatestBlockHeight: height.Load()},
			}, nil
		})

		deps := cosmosDeps{
			Cosmos: services.CosmosEndpoint{Client: node.Client(t)},
			Logger: log.New(io.Discard, "", 0),
		}
		var (
			s          Subscriber
			rate       blockRate
			quietScans uint64
			liveHealth cosmosLiveHealth
			// Far above any head the stub reports, so cosmosScanRange finds nothing to
			// scan and the pass ends right after the head read -- the only part under test.
			cursor = uint64(1_000_000)
		)
		recover := func() {
			t.Helper()
			if _, err := s.recoverCosmosGapToLatest(context.Background(), deps, services.NewBatchBuilder(),
				&cursor, map[cosmosEventKey]struct{}{}, &quietScans, &liveHealth, &rate); err != nil {
				t.Fatalf("recovery pass: %v", err)
			}
		}

		recover()
		if _, ok := rate.blockTime(); ok {
			t.Fatal("one head read is not a rate; blockTime must not claim to know one yet")
		}

		height.Store(110)
		recover()

		if _, ok := rate.blockTime(); !ok {
			t.Fatal("two recovery passes over an advancing chain taught the rate nothing: " +
				"the head read is not reaching rate.observe, so recoveryTick never leaves its fallback")
		}
	})

	// Reported by @DongLieu on #451, and it is the sibling of the idle-then-burst
	// case above rather than a repeat of it. That one is a head that did not move;
	// this one is a head that DID move, just not while anyone was watching.
	//
	// observe cannot tell them apart: height > lastHeight only says the chain
	// advanced, never whether the subscriber was connected while it did. So a window
	// carried across a reconnect reports the size of the outage rather than the rate
	// of the chain -- and it does so at the worst moment, when the cursor is furthest
	// behind and the recovery tick most needs to be tight.
	//
	// The fix is structural rather than a call: SubscribeCosmos and SubscribeEth now
	// declare their blockRate INSIDE the reconnect loop, so each subscription gets a
	// fresh window and there is no reset anyone can forget. These two subtests are
	// what that structure buys and what it avoids.
	t.Run("across a reconnect", func(t *testing.T) {
		// One subscription's window, then the next subscription's own.
		t.Run("a fresh window per subscription measures the chain", func(t *testing.T) {
			start := time.Now()

			var first blockRate
			first.observe(100, start)
			first.observe(110, start.Add(10*time.Second))
			if got, ok := first.blockTime(); !ok || got != time.Second {
				t.Fatalf("blockTime before the outage = %v (ok=%v), want 1s", got, ok)
			}

			// The subscription drops for ten minutes. The chain is demand-driven and
			// quiet, so it produces two blocks the whole time. The loop then starts a
			// new iteration, which is a new blockRate.
			var second blockRate
			reconnected := start.Add(10 * time.Minute)
			second.observe(112, reconnected)
			if _, ok := second.blockTime(); ok {
				t.Fatal("a single reading reported a block time; one sample is a baseline, not a rate")
			}

			second.observe(122, reconnected.Add(10*time.Second))
			got, ok := second.blockTime()
			if !ok {
				t.Fatal("no block time after two post-reconnect readings")
			}
			if got != time.Second {
				t.Fatalf("blockTime = %v, want 1s", got)
			}
			if tick := recoveryTick(got); tick != minRecoveryTick {
				t.Fatalf("recoveryTick = %v, want the %v floor for a one-second chain", tick, minRecoveryTick)
			}
		})

		// The harm the structure avoids, kept as a measurement rather than a claim:
		// one window spanning the outage sees 22 blocks across 10m10s, ~27s a block,
		// which pins the tick to its ceiling.
		t.Run("one window spanning the outage measures the outage", func(t *testing.T) {
			start := time.Now()
			var rate blockRate
			rate.observe(100, start)
			rate.observe(110, start.Add(10*time.Second))
			reconnected := start.Add(10 * time.Minute)
			rate.observe(112, reconnected)
			rate.observe(122, reconnected.Add(10*time.Second))

			got, ok := rate.blockTime()
			if !ok {
				t.Fatal("no block time")
			}
			if got < 20*time.Second {
				t.Fatalf("blockTime = %v; this subtest exists to show the un-reset behaviour is badly "+
					"inflated, so the per-subscription window above is measuring something real", got)
			}
			if recoveryTick(got) != maxRecoveryTick {
				t.Fatalf("recoveryTick = %v, want the ceiling: that is what a shared window costs",
					recoveryTick(got))
			}
		})
	})
}

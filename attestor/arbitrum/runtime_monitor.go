package arbitrum

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"attestor/types/redact"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	defaultSubscriptionRetryInterval = time.Second
	nitroHeadBufferSize              = 64
	refreshBackoffBase               = time.Second
	refreshBackoffRateLimitBase      = 10 * time.Second
	refreshBackoffMax                = time.Minute
	defaultRuntimeRefreshTimeout     = 2 * time.Minute
)

// RuntimeRefresher refreshes the Nitro view consumed by the attestor.
type RuntimeRefresher interface {
	Refresh(context.Context) ([]FinalizedConsistency, error)
}

// NitroHeadSubscriber receives new Nitro execution heads.
type NitroHeadSubscriber interface {
	SubscribeNewHead(context.Context, chan<- *types.Header) (ethereum.Subscription, error)
}

// RuntimeMonitorConfig controls the reconciliation and subscription retry
// cadence. The command supplies the chain-specific reconciliation interval;
// tests can shorten subscription retries without changing production defaults.
type RuntimeMonitorConfig struct {
	ReconcileInterval         time.Duration
	SubscriptionRetryInterval time.Duration
	// RefreshTimeout bounds the full refresh, including its concurrent Nitro
	// calls and derived-root sync. Zero selects the production default.
	RefreshTimeout time.Duration
}

// MonitorRuntimeState keeps the runtime view current from both periodic polls
// and Nitro head notifications. Transient refresh and subscription errors are
// retried until ctx is cancelled.
func MonitorRuntimeState(
	ctx context.Context,
	runtimeState RuntimeRefresher,
	subscriber NitroHeadSubscriber,
	config RuntimeMonitorConfig,
) error {
	if runtimeState == nil {
		return errors.New("runtime refresher must not be nil")
	}
	if subscriber == nil {
		return errors.New("Nitro head subscriber must not be nil")
	}
	if config.ReconcileInterval <= 0 {
		return fmt.Errorf("runtime reconcile interval must be greater than zero")
	}
	retryInterval := config.SubscriptionRetryInterval
	if retryInterval == 0 {
		retryInterval = defaultSubscriptionRetryInterval
	}
	if retryInterval < 0 {
		return fmt.Errorf("Nitro subscription retry interval must not be negative")
	}
	refreshTimeout := config.RefreshTimeout
	if refreshTimeout == 0 {
		refreshTimeout = defaultRuntimeRefreshTimeout
	}
	if refreshTimeout < 0 {
		return fmt.Errorf("Nitro runtime refresh timeout must not be negative")
	}
	monitorRuntimeStateWithRetry(ctx, runtimeState, subscriber, config.ReconcileInterval, retryInterval, refreshTimeout)
	return nil
}

func monitorRuntimeStateWithRetry(
	ctx context.Context,
	runtimeState RuntimeRefresher,
	subscriber NitroHeadSubscriber,
	reconcileInterval time.Duration,
	subscriptionRetryInterval time.Duration,
	refreshTimeout time.Duration,
) {
	headUpdates := make(chan struct{}, 1)
	go streamNitroHeadUpdates(ctx, subscriber, headUpdates, subscriptionRetryInterval)

	var refreshStartNanos atomic.Int64
	go watchRuntimeRefresh(ctx, &refreshStartNanos, reconcileInterval)

	refresh := func() error {
		start := time.Now()
		refreshStartNanos.Store(start.UnixNano())
		defer refreshStartNanos.Store(0)
		refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
		defer cancel()
		checks, err := runtimeState.Refresh(refreshCtx)
		if elapsed := time.Since(start); elapsed > reconcileInterval {
			log.Printf("attestor runtime refresh slow: elapsed=%s interval=%s", elapsed, reconcileInterval)
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("attestor runtime refresh failed: %v", redact.Error(err))
			}
			return err
		}
		for _, check := range checks {
			switch {
			case check.FinalizedReorg:
				log.Printf(
					"attestor finalized head changed or regressed: height=%d state_root=%s previous_height=%d previous_state_root=%s",
					check.Finalized.BlockNumber,
					check.Finalized.StateRoot.Hex(),
					check.PreviousFinalized.BlockNumber,
					check.PreviousFinalized.StateRoot.Hex(),
				)
			case check.Consistent():
				log.Printf(
					"attestor finalized consistency verified: height=%d state_root=%s",
					check.Finalized.BlockNumber,
					check.Finalized.StateRoot.Hex(),
				)
			case !check.UnsafeObserved || !check.SafeObserved:
				log.Printf(
					"attestor finalized consistency incomplete: height=%d unsafe_observed=%t safe_observed=%t",
					check.Finalized.BlockNumber,
					check.UnsafeObserved,
					check.SafeObserved,
				)
			default:
				log.Printf(
					"attestor finalized consistency mismatch: height=%d finalized_root=%s unsafe_root=%s safe_root=%s finalized_reorg=%t",
					check.Finalized.BlockNumber,
					check.Finalized.StateRoot.Hex(),
					check.Unsafe.StateRoot.Hex(),
					check.Safe.StateRoot.Hex(),
					check.FinalizedReorg,
				)
			}
		}
		return nil
	}

	// A failed refresh backs off before the next attempt instead of letting
	// every new-head event retrigger it immediately: without this, a tripped
	// RPC rate limit is hammered ~once per L2 block and never recovers.
	failures := 0
	runRefresh := func() {
		if err := refresh(); err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			rateLimited := IsRateLimited(err)
			delay := RuntimeRefreshBackoff(failures, rateLimited)
			log.Printf(
				"attestor runtime refresh backing off: failures=%d delay=%s rate_limited=%t",
				failures,
				delay,
				rateLimited,
			)
			select {
			case <-ctx.Done():
			case <-time.After(delay):
			}
			return
		}
		failures = 0
	}

	runRefresh()
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-headUpdates:
			runRefresh()
		case <-ticker.C:
			runRefresh()
		}
	}
}

// RuntimeRefreshBackoff returns the wait before the next refresh attempt after
// consecutive failures: exponential from the base, capped, with up to +20%
// jitter so restarted attestors sharing an endpoint do not retry in lockstep.
// A rate-limited failure starts high — retrying a 429 quickly only extends it.
func RuntimeRefreshBackoff(failures int, rateLimited bool) time.Duration {
	base := refreshBackoffBase
	if rateLimited {
		base = refreshBackoffRateLimitBase
	}
	backoff := base
	for i := 1; i < failures; i++ {
		backoff *= 2
		if backoff >= refreshBackoffMax {
			backoff = refreshBackoffMax
			break
		}
	}
	if backoff > refreshBackoffMax {
		backoff = refreshBackoffMax
	}
	return backoff + rand.N(backoff/5)
}

// watchRuntimeRefresh logs when a runtime refresh has been in flight longer
// than twice the reconcile interval, so a hung Nitro RPC call stays visible
// even though the monitor goroutine is blocked inside Refresh.
func watchRuntimeRefresh(ctx context.Context, startNanos *atomic.Int64, interval time.Duration) {
	threshold := 2 * interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			started := startNanos.Load()
			if started == 0 {
				continue
			}
			if elapsed := time.Since(time.Unix(0, started)); elapsed > threshold {
				log.Printf("attestor runtime refresh still running: elapsed=%s threshold=%s", elapsed, threshold)
			}
		}
	}
}

func streamNitroHeadUpdates(
	ctx context.Context,
	subscriber NitroHeadSubscriber,
	updates chan<- struct{},
	retryInterval time.Duration,
) {
	for {
		headers := make(chan *types.Header, nitroHeadBufferSize)
		subscription, err := subscriber.SubscribeNewHead(ctx, headers)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("subscribe to Nitro new heads failed: %v", redact.Error(err))
			if !waitForRetry(ctx, retryInterval) {
				return
			}
			continue
		}

		// Reconcile immediately after subscribing to close the gap between the
		// previous snapshot and the subscription becoming active.
		signalRuntimeUpdate(updates)
		err = consumeNitroHeadSubscription(ctx, subscription, headers, updates)
		subscription.Unsubscribe()
		if ctx.Err() != nil {
			return
		}
		log.Printf("Nitro new-head subscription ended: %v; reconnecting", redact.Error(err))
		if !waitForRetry(ctx, retryInterval) {
			return
		}
	}
}

func consumeNitroHeadSubscription(
	ctx context.Context,
	subscription ethereum.Subscription,
	headers <-chan *types.Header,
	updates chan<- struct{},
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case header, ok := <-headers:
			if !ok {
				return errors.New("Nitro new-head channel closed")
			}
			if header != nil {
				signalRuntimeUpdate(updates)
			}
		case err, ok := <-subscription.Err():
			if !ok || err == nil {
				return errors.New("Nitro new-head subscription closed")
			}
			return err
		}
	}
}

func signalRuntimeUpdate(updates chan<- struct{}) {
	select {
	case updates <- struct{}{}:
	default:
	}
}

func waitForRetry(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

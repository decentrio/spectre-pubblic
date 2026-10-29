package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"attestor/arbitrum"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestStartCommandPassesConfigAndContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "command-context")

	var receivedConfig string
	var receivedContextValue any
	command := newRootCommand(func(ctx context.Context, configPath string) error {
		receivedConfig = configPath
		receivedContextValue = ctx.Value(contextKey{})
		return nil
	})
	command.SetArgs([]string{"start", "--config", "custom-arbitrum.json"})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatalf("execute start command: %v", err)
	}
	if receivedConfig != "custom-arbitrum.json" {
		t.Fatalf("config path: got %q want %q", receivedConfig, "custom-arbitrum.json")
	}
	if receivedContextValue != "command-context" {
		t.Fatalf("command context value: got %v", receivedContextValue)
	}
}

func TestStartCommandUsesDefaultConfig(t *testing.T) {
	var receivedConfig string
	command := newRootCommand(func(_ context.Context, configPath string) error {
		receivedConfig = configPath
		return nil
	})
	command.SetArgs([]string{"start"})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute start command: %v", err)
	}
	if receivedConfig != defaultConfigPath {
		t.Fatalf("default config path: got %q want %q", receivedConfig, defaultConfigPath)
	}
}

func TestStartCommandRejectsArguments(t *testing.T) {
	command := newRootCommand(func(context.Context, string) error {
		t.Fatal("start runner called with positional arguments")
		return nil
	})
	command.SetArgs([]string{"start", "unexpected"})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})

	if err := command.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected positional argument error")
	}
}

func TestAssertionSourceIdentityPinsBoLDStorageLayout(t *testing.T) {
	config := arbitrum.DaemonConfig{
		L1ChainID:             11_155_111,
		L2ChainID:             421_614,
		RollupCoreAddress:     "0x042B2E6C5E99d4c521bd49beeD5E99651D9B0Cf4",
		AssertionsMappingSlot: "0x75",
		AssertionStatusOffset: 25,
	}
	identity := assertionSourceIdentity(config)
	for _, expected := range []string{
		"l1:11155111",
		"l2:421614",
		"rollup:0x042B2E6C5E99d4c521bd49beeD5E99651D9B0Cf4",
		"assertions:0x0000000000000000000000000000000000000000000000000000000000000075",
		"status-offset:25",
	} {
		if !strings.Contains(identity, expected) {
			t.Fatalf("source identity %q does not contain %q", identity, expected)
		}
	}
}

func TestMonitorRuntimeStateRefreshesOnHeadsAndReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refresher := &testRuntimeRefresher{calls: make(chan struct{}, 16)}
	attempts := make(chan testHeadSubscriptionAttempt, 2)
	subscriber := headSubscriberFunc(func(
		_ context.Context,
		headers chan<- *types.Header,
	) (ethereum.Subscription, error) {
		subscription := newTestSubscription()
		attempts <- testHeadSubscriptionAttempt{
			headers:      headers,
			subscription: subscription,
		}
		return subscription, nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorRuntimeStateWithRetry(
			ctx,
			refresher,
			subscriber,
			time.Hour,
			time.Millisecond,
		)
	}()

	first := waitForSubscriptionAttempt(t, attempts)
	waitForRefreshes(t, refresher.calls, 2) // Startup and post-subscribe reconciliation.

	first.headers <- &types.Header{}
	waitForRefreshes(t, refresher.calls, 1)

	first.subscription.err <- errors.New("test disconnect")
	second := waitForSubscriptionAttempt(t, attempts)
	waitForRefreshes(t, refresher.calls, 1) // Post-reconnect reconciliation.

	cancel()
	select {
	case <-second.subscription.unsubscribed:
	case <-time.After(time.Second):
		t.Fatal("reconnected subscription was not unsubscribed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime monitor did not stop")
	}
}

func TestMonitorRuntimeStatePeriodicallyReconcilesWithoutHeads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refresher := &testRuntimeRefresher{calls: make(chan struct{}, 16)}
	attempts := make(chan testHeadSubscriptionAttempt, 1)
	subscriber := headSubscriberFunc(func(
		_ context.Context,
		headers chan<- *types.Header,
	) (ethereum.Subscription, error) {
		subscription := newTestSubscription()
		attempts <- testHeadSubscriptionAttempt{
			headers:      headers,
			subscription: subscription,
		}
		return subscription, nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorRuntimeStateWithRetry(
			ctx,
			refresher,
			subscriber,
			10*time.Millisecond,
			time.Hour,
		)
	}()

	attempt := waitForSubscriptionAttempt(t, attempts)
	waitForRefreshes(t, refresher.calls, 3) // Startup, subscribe, then fallback tick.
	cancel()

	select {
	case <-attempt.subscription.unsubscribed:
	case <-time.After(time.Second):
		t.Fatal("subscription was not unsubscribed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime monitor did not stop")
	}
}

func TestMonitorRuntimeStateLogsSlowAndStuckRefresh(t *testing.T) {
	logs := &syncLogBuffer{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refresher := &blockingRuntimeRefresher{
		started: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	subscriber := headSubscriberFunc(func(
		_ context.Context,
		_ chan<- *types.Header,
	) (ethereum.Subscription, error) {
		return newTestSubscription(), nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorRuntimeStateWithRetry(
			ctx,
			refresher,
			subscriber,
			20*time.Millisecond,
			time.Hour,
		)
	}()

	select {
	case <-refresher.started:
	case <-time.After(time.Second):
		t.Fatal("startup refresh never began")
	}
	// The watchdog must announce the blocked refresh while it is in flight.
	waitForLogContains(t, logs, "attestor runtime refresh still running")

	close(refresher.release)
	// Once the refresh returns, its duration exceeds the reconcile interval.
	waitForLogContains(t, logs, "attestor runtime refresh slow")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime monitor did not stop")
	}
}

func TestMonitorRuntimeStateBoundsRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refresher := &deadlineAwareBlockingRuntimeRefresher{cancelled: make(chan error, 1)}
	subscriber := headSubscriberFunc(func(
		_ context.Context,
		_ chan<- *types.Header,
	) (ethereum.Subscription, error) {
		return newTestSubscription(), nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := arbitrum.MonitorRuntimeState(ctx, refresher, subscriber, arbitrum.RuntimeMonitorConfig{
			ReconcileInterval:         time.Hour,
			SubscriptionRetryInterval: time.Hour,
			RefreshTimeout:            20 * time.Millisecond,
		}); err != nil {
			t.Errorf("MonitorRuntimeState: %v", err)
		}
	}()

	select {
	case err := <-refresher.cancelled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("refresh cancellation = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh was not cancelled at its deadline")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime monitor did not stop after cancellation")
	}
}

func TestRefreshBackoffGrowsAndCaps(t *testing.T) {
	cases := []struct {
		failures    int
		rateLimited bool
		min, max    time.Duration
	}{
		{failures: 1, rateLimited: false, min: time.Second, max: 1200 * time.Millisecond},
		{failures: 3, rateLimited: false, min: 4 * time.Second, max: 4800 * time.Millisecond},
		{failures: 1, rateLimited: true, min: 10 * time.Second, max: 12 * time.Second},
		{failures: 30, rateLimited: false, min: time.Minute, max: 72 * time.Second},
		{failures: 30, rateLimited: true, min: time.Minute, max: 72 * time.Second},
	}
	for _, tc := range cases {
		for range 32 { // jitter is random; check the bounds hold
			delay := refreshBackoff(tc.failures, tc.rateLimited)
			if delay < tc.min || delay > tc.max {
				t.Fatalf(
					"backoff(failures=%d rate_limited=%t): got %s want [%s, %s]",
					tc.failures, tc.rateLimited, delay, tc.min, tc.max,
				)
			}
		}
	}
}

func TestMonitorRuntimeStateBacksOffAfterFailedRefresh(t *testing.T) {
	logs := &syncLogBuffer{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refresher := &flakyRuntimeRefresher{failuresLeft: 1, calls: make(chan time.Time, 16)}
	subscriber := headSubscriberFunc(func(
		_ context.Context,
		_ chan<- *types.Header,
	) (ethereum.Subscription, error) {
		return newTestSubscription(), nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorRuntimeStateWithRetry(ctx, refresher, subscriber, 5*time.Millisecond, time.Hour)
	}()

	first := waitForRefreshAt(t, refresher.calls)
	second := waitForRefreshAt(t, refresher.calls)
	if gap := second.Sub(first); gap < 900*time.Millisecond {
		t.Fatalf("second refresh fired %s after a failure; want the >=1s backoff", gap)
	}
	if !strings.Contains(logs.String(), "attestor runtime refresh backing off: failures=1") {
		t.Fatalf("backoff log missing in:\n%s", logs.String())
	}

	// The second attempt succeeded, so the loop is back on the fast cadence.
	third := waitForRefreshAt(t, refresher.calls)
	if gap := third.Sub(second); gap > 500*time.Millisecond {
		t.Fatalf("refresh cadence did not recover after success: %s between calls", gap)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime monitor did not stop")
	}
}

func TestValidateNitroChainID(t *testing.T) {
	if err := validateNitroChainID(
		context.Background(),
		chainIDReaderFunc(func(context.Context) (*big.Int, error) {
			return big.NewInt(421614), nil
		}),
		421614,
		"RPC",
	); err != nil {
		t.Fatalf("validate matching chain ID: %v", err)
	}

	err := validateNitroChainID(
		context.Background(),
		chainIDReaderFunc(func(context.Context) (*big.Int, error) {
			return big.NewInt(42161), nil
		}),
		421614,
		"WebSocket",
	)
	if err == nil || !strings.Contains(err.Error(), "expected 421614") {
		t.Fatalf("validate mismatched chain ID: got %v", err)
	}
}

type chainIDReaderFunc func(context.Context) (*big.Int, error)

func (f chainIDReaderFunc) ChainID(ctx context.Context) (*big.Int, error) {
	return f(ctx)
}

func monitorRuntimeStateWithRetry(
	ctx context.Context,
	runtimeState runtimeStateRefresher,
	subscriber arbitrum.NitroHeadSubscriber,
	reconcileInterval time.Duration,
	subscriptionRetryInterval time.Duration,
) {
	if err := arbitrum.MonitorRuntimeState(ctx, runtimeState, subscriber, arbitrum.RuntimeMonitorConfig{
		ReconcileInterval:         reconcileInterval,
		SubscriptionRetryInterval: subscriptionRetryInterval,
	}); err != nil {
		panic(err)
	}
}

func refreshBackoff(failures int, rateLimited bool) time.Duration {
	return arbitrum.RuntimeRefreshBackoff(failures, rateLimited)
}

type testRuntimeRefresher struct {
	calls chan struct{}
}

func (r *testRuntimeRefresher) Refresh(
	context.Context,
) ([]arbitrum.FinalizedConsistency, error) {
	r.calls <- struct{}{}
	return nil, nil
}

// flakyRuntimeRefresher fails its first failuresLeft calls, then succeeds,
// reporting each call's start time.
type flakyRuntimeRefresher struct {
	mu           sync.Mutex
	failuresLeft int
	calls        chan time.Time
}

func (r *flakyRuntimeRefresher) Refresh(
	context.Context,
) ([]arbitrum.FinalizedConsistency, error) {
	r.calls <- time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failuresLeft > 0 {
		r.failuresLeft--
		return nil, errors.New("synthetic refresh failure")
	}
	return nil, nil
}

func waitForRefreshAt(t *testing.T, calls <-chan time.Time) time.Time {
	t.Helper()
	select {
	case at := <-calls:
		return at
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a runtime refresh")
		return time.Time{}
	}
}

// blockingRuntimeRefresher signals each Refresh start and blocks every call
// until release is closed.
type blockingRuntimeRefresher struct {
	started chan struct{}
	release chan struct{}
}

type deadlineAwareBlockingRuntimeRefresher struct {
	cancelled chan error
}

func (r *deadlineAwareBlockingRuntimeRefresher) Refresh(
	ctx context.Context,
) ([]arbitrum.FinalizedConsistency, error) {
	<-ctx.Done()
	r.cancelled <- ctx.Err()
	return nil, ctx.Err()
}

func (r *blockingRuntimeRefresher) Refresh(
	context.Context,
) ([]arbitrum.FinalizedConsistency, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.release
	return nil, nil
}

// syncLogBuffer is a log.SetOutput sink safe to read while the watchdog
// goroutine is still writing.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForLogContains(t *testing.T, logs *syncLogBuffer, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(logs.String(), want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for log %q in:\n%s", want, logs.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type headSubscriberFunc func(
	context.Context,
	chan<- *types.Header,
) (ethereum.Subscription, error)

func (f headSubscriberFunc) SubscribeNewHead(
	ctx context.Context,
	headers chan<- *types.Header,
) (ethereum.Subscription, error) {
	return f(ctx, headers)
}

type testHeadSubscriptionAttempt struct {
	headers      chan<- *types.Header
	subscription *testSubscription
}

type testSubscription struct {
	err          chan error
	unsubscribed chan struct{}
	once         sync.Once
}

func newTestSubscription() *testSubscription {
	return &testSubscription{
		err:          make(chan error, 1),
		unsubscribed: make(chan struct{}),
	}
}

func (s *testSubscription) Err() <-chan error {
	return s.err
}

func (s *testSubscription) Unsubscribe() {
	s.once.Do(func() {
		close(s.unsubscribed)
	})
}

func waitForSubscriptionAttempt(
	t *testing.T,
	attempts <-chan testHeadSubscriptionAttempt,
) testHeadSubscriptionAttempt {
	t.Helper()
	select {
	case attempt := <-attempts:
		return attempt
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Nitro subscription attempt")
		return testHeadSubscriptionAttempt{}
	}
}

func waitForRefreshes(t *testing.T, calls <-chan struct{}, count int) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for index := range count {
		select {
		case <-calls:
		case <-timer.C:
			t.Fatalf("timed out after %d of %d runtime refreshes", index, count)
		}
	}
}

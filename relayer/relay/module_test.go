package relay

import (
	"context"
	"errors"
	"fmt"
	"relayer/chain"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- mock adapters (the whole point: Module is testable without any chain) ---
type mockSource struct {
	latest                uint64
	relayable             uint64 // highest provable height; if 0, defaults to latest
	membershipCalls       int
	nonMembershipCalls    int
	proofHeights          []uint64
	failMembershipOn      map[string]bool // Raw payload -> return a retryable proof error
	permanentMembershipOn map[string]bool // Raw payload -> return a permanent proof error
	// relayableErr, when set, is returned by RelayableHeight instead of a height.
	// relayableCalls counts how often the source was actually asked, which is the
	// thing the permanent-failure hold is about.
	relayableErr   error
	relayableCalls int
	// onMembershipProof, when set, runs synchronously inside MembershipProof for
	// the given packet. It is how a test simulates SubscribeCosmos's goroutine
	// delivering a terminal event WHILE handleBatch is spending a Groth16 proof
	// on that exact packet, a window no earlier settlement check can observe.
	onMembershipProof func(packet []byte)
}

func (m *mockSource) Chain() chain.ChainType { return chain.Cosmos }

func (m *mockSource) Subscribe(context.Context, func(context.Context, []chain.Event) []int) error {
	return nil
}

func (m *mockSource) LatestHeight(context.Context) (uint64, error) { return m.latest, nil }

func (m *mockSource) RelayableHeight(context.Context) (uint64, error) {
	m.relayableCalls++
	if m.relayableErr != nil {
		return 0, m.relayableErr
	}
	if m.relayable != 0 {
		return m.relayable, nil
	}
	return m.latest, nil
}

func (m *mockSource) QueryHeader(_ context.Context, h uint64) ([]byte, error) {
	return []byte{byte(h)}, nil
}

func (m *mockSource) MembershipProof(_ context.Context, packet []byte, height uint64, _ chain.EventType) ([]byte, error) {
	m.membershipCalls++
	m.proofHeights = append(m.proofHeights, height)
	if m.onMembershipProof != nil {
		m.onMembershipProof(packet)
	}
	if m.permanentMembershipOn[string(packet)] {
		return nil, chain.Permanent(errors.New("timed out"))
	}
	if m.failMembershipOn[string(packet)] {
		return nil, chain.Transient(errors.New("proof unavailable"))
	}
	return []byte("membership"), nil
}

func (m *mockSource) NonMembershipProof(_ context.Context, _ []byte, height uint64) ([]byte, error) {
	m.nonMembershipCalls++
	m.proofHeights = append(m.proofHeights, height)
	return []byte("non-membership"), nil
}

type mockDest struct {
	updates    []chain.ClientUpdate // recorded UpdateClient calls
	updateErr  error                // if set, UpdateClient returns it
	relayed    []chain.RelayPacket  // recorded RelayPackets calls
	relayCalls int                  // number of RelayPackets invocations (multicall folding check)
	relayErr   error                // if set, RelayPackets returns it (transient)
	// relayCommit is how many packets a FAILING RelayPackets reports as already on
	// chain, so a partial commit can be driven. Read only when relayErr is set.
	relayCommit int
	// hasReceipt / receiptErr drive HasPacketReceipt for the flush tests: an
	// outstanding source commitment does not say whether the destination already
	// delivered the packet, so the flush asks.
	hasReceipt bool
	receiptErr error
	// receiptAnswers, when non-empty, is consumed one per HasPacketReceipt call
	// and falls back to hasReceipt once exhausted. It is how a test makes a packet
	// become delivered BETWEEN the flush's two receipt checks.
	receiptAnswers []bool
	// poison: Raw payload -> this packet deterministically reverts any batch it is
	// in (chain.Permanent), like a timed-out/duplicate packet in a real multicall.
	poison map[string]bool
	// expiresAt / trustingPeriod / expiresErr drive ClientExpiresAt for the
	// anti-expiry refresh tests. A zero trustingPeriod means the destination could
	// not report one, which is what makes the module fall back to its default margin.
	trustingPeriod time.Duration
	expiresAt      time.Time
	expiresErr     error
}

func (m *mockDest) Chain() chain.ChainType { return chain.Ethereum }

func (m *mockDest) UpdateClient(_ context.Context, _ string, u chain.ClientUpdate) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.updates = append(m.updates, u)
	return nil
}

func (m *mockDest) RelayPackets(_ context.Context, packets []chain.RelayPacket) (int, error) {
	m.relayCalls++
	if m.relayErr != nil {
		// A reported prefix has to be RECORDED as relayed too. A stub that claims
		// a commit without delivering anything would let the engine mis-settle and
		// still pass.
		//
		// The COUNT is returned unclamped on purpose. Clamping it here would make
		// the stub incapable of the contract violation one subtest is about, and
		// that subtest would then pass with the engine's own clamp deleted. Only
		// the recording is bounded: a stub cannot deliver what it was not given.
		m.relayed = append(m.relayed, packets[:min(m.relayCommit, len(packets))]...)
		return m.relayCommit, m.relayErr
	}
	for _, p := range packets {
		if m.poison[string(p.Packet)] {
			// This mock is an EVM destination: one multicall, so a revert commits
			// nothing.
			return 0, chain.Permanent(errors.New("poison packet reverted the batch"))
		}
	}
	m.relayed = append(m.relayed, packets...)
	return len(packets), nil
}

func (m *mockDest) HasPacketReceipt(context.Context, []byte) (bool, error) {
	if len(m.receiptAnswers) > 0 {
		answer := m.receiptAnswers[0]
		m.receiptAnswers = m.receiptAnswers[1:]
		return answer, m.receiptErr
	}
	return m.hasReceipt, m.receiptErr
}

func (m *mockDest) ClientExpiresAt(context.Context, string) (time.Time, time.Duration, error) {
	return m.expiresAt, m.trustingPeriod, m.expiresErr
}

type foldingMockDest struct {
	mockDest
	enabled       bool
	foldCalls     int
	foldedUpdates []chain.ClientUpdate
	foldedPackets [][]chain.RelayPacket
	foldErr       error
	foldFn        func(chain.ClientUpdate, []chain.RelayPacket) error
}

func (m *foldingMockDest) SupportsUpdatePacketFolding() bool { return m.enabled }

func (m *foldingMockDest) RelayWithUpdate(_ context.Context, _ string, update chain.ClientUpdate, packets []chain.RelayPacket) error {
	m.foldCalls++
	m.foldedUpdates = append(m.foldedUpdates, update)
	m.foldedPackets = append(m.foldedPackets, packets)
	if m.foldFn != nil {
		return m.foldFn(update, packets)
	}
	return m.foldErr
}

// mockBuilder returns an update whose Height is the height it was asked to prove,
// unless overridden (to simulate a "no update needed" zero-height result).
type mockBuilder struct {
	forceHeight uint64 // if non-zero, always return this height
	noPayload   bool   // if true, return no payloads ("client already current")
	built       int    // count of Build calls
	// hops is the ladder a churn-blocked builder walks: each Build returns the
	// next entry and marks it IsHop, until the list runs out and the last update
	// is the real one. Empty means "never hops", which is every adapter but the
	// Groth16 one.
	hops []uint64
	// trustedAt is the timestamp the built update claims the client now trusts.
	// It is what the client-update observer reports, so a test that checks the
	// observer must be able to set it to something distinguishable.
	trustedAt time.Time
}

func (m *mockBuilder) Name() string { return "mock" }

func (m *mockBuilder) Build(_ context.Context, header []byte) (chain.ClientUpdate, error) {
	m.built++
	if len(m.hops) > 0 {
		h := m.hops[0]
		m.hops = m.hops[1:]
		return chain.ClientUpdate{
			Height: h, Payloads: [][]byte{header}, TrustedAt: m.trustedAt,
			IsHop: len(m.hops) > 0, // the last rung is the target, not a hop
		}, nil
	}
	h := uint64(header[0])
	if m.forceHeight != 0 {
		h = m.forceHeight
	}
	if m.noPayload {
		return chain.ClientUpdate{Height: h, TrustedAt: m.trustedAt}, nil
	}
	return chain.ClientUpdate{Height: h, Payloads: [][]byte{header}, TrustedAt: m.trustedAt}, nil
}

// transientOnIsolate is a destination where the whole batch reverts permanently
// (poison present) but the named packet fails TRANSIENTLY when retried alone.
type transientOnIsolate struct {
	mockDest
	poison       string
	transientFor string
}

func (d *transientOnIsolate) RelayPackets(_ context.Context, packets []chain.RelayPacket) (int, error) {
	for _, p := range packets {
		if string(p.Packet) == d.poison {
			return 0, chain.Permanent(errors.New("poison")) // one multicall: nothing lands
		}
	}
	if len(packets) == 1 && string(packets[0].Packet) == d.transientFor {
		return 0, errors.New("rpc blip") // transient
	}
	return len(packets), nil
}

// TestPeriodicUpdateLoop_Runs verifies the fixed-cadence forced update fires
// (the pinned-set rotation guarantee) and keeps firing until the context ends.
func TestPeriodicUpdateLoop_Runs(t *testing.T) {
	var calls atomic.Int32
	m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
		WithPeriodicUpdate(10*time.Millisecond, 0, func(context.Context) error {
			calls.Add(1)
			return nil
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go m.periodicUpdateLoop(ctx)

	deadline := time.After(2 * time.Second)
	for calls.Load() < 2 {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("periodicUpdate fired %d times, want >= 2", calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
}

// TestPeriodicUpdateLoop_RetriesOnFailure verifies a failing run does not stop the
// loop — it keeps retrying (on the shorter retry cadence).
func TestPeriodicUpdateLoop_RetriesOnFailure(t *testing.T) {
	var calls atomic.Int32
	m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
		WithPeriodicUpdate(10*time.Millisecond, 0, func(context.Context) error {
			calls.Add(1)
			return errors.New("rotate failed")
		}),
	)
	// Shrink the retry cadence for the test so failures re-fire quickly.
	m.periodicUpdateInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	m.periodicUpdateLoop(ctx) // returns when ctx times out

	if calls.Load() < 2 {
		t.Fatalf("failing periodicUpdate must keep retrying, fired %d times", calls.Load())
	}
}

// TestNextPeriodicUpdateBackoff verifies the exponential failure backoff sequence
// (1m→2m→4m→8m→15m cap) that prevents a persistently-failing client-update tx
// from draining the signer's gas.
func TestNextPeriodicUpdateBackoff(t *testing.T) {
	want := []time.Duration{
		1 * time.Minute, // first failure
		2 * time.Minute, // doubling...
		4 * time.Minute,
		8 * time.Minute,
		15 * time.Minute, // capped (would be 16m)
		15 * time.Minute, // stays capped
	}
	cur := time.Duration(0) // no prior failure
	for i, w := range want {
		cur = nextPeriodicUpdateBackoff(cur)
		if cur != w {
			t.Fatalf("backoff step %d: got %s, want %s", i, cur, w)
		}
	}
	// A success (reset to 0) restarts the sequence at the minimum.
	if got := nextPeriodicUpdateBackoff(0); got != periodicUpdateBackoffMin {
		t.Fatalf("reset backoff: got %s, want %s", got, periodicUpdateBackoffMin)
	}
}

// withFastRefreshTick shortens the refresh cadence so a loop test finishes in
// milliseconds instead of a minute, and restores it afterwards.
func withFastRefreshTick(t *testing.T) {
	t.Helper()
	prev := refreshTick
	refreshTick = time.Millisecond
	t.Cleanup(func() { refreshTick = prev })
}

// waitFor polls cond until it holds or ctx expires.
func waitFor(t *testing.T, ctx context.Context, cond func() bool, msg string) {
	t.Helper()
	for {
		if cond() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(msg)
		case <-time.After(time.Millisecond):
		}
	}
}

// updateClientTo is the single place the module advances its view of the
// destination client. Every scenario it must get right lives here.
func TestUpdateClientTo(t *testing.T) {
	// an empty-payload-list ("already current")
	// build advances lastHeight without submitting a tx, so a restart where the
	// client already covers incoming packets does not stall the provability guard.
	t.Run("seeds lastHeight from a no-op build", func(t *testing.T) {
		src := &mockSource{latest: 200}
		dst := &mockDest{}
		b := &mockBuilder{noPayload: true, forceHeight: 200} // "client current at 200"
		m := NewModule("test", "client-0", src, dst, b)

		if err := m.updateClientTo(context.Background(), 150); err != nil {
			t.Fatalf("seed update: %v", err)
		}
		if len(dst.updates) != 0 {
			t.Fatalf("no-op build must not submit a tx, got %d", len(dst.updates))
		}
		if m.lastHeight != 200 {
			t.Fatalf("lastHeight must be seeded to reported current height 200, got %d", m.lastHeight)
		}
		// A packet at 180 is now provable (<= seeded 200) even though no tx was sent.
		if rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 180, Raw: []byte("p")}}); len(rq) != 0 {
			t.Fatalf("packet under seeded height must relay, got re-queue %v", rq)
		}
	})

	t.Run("is append-only", func(t *testing.T) {
		src := &mockSource{}
		dst := &mockDest{}
		b := &mockBuilder{}
		m := NewModule("test", "client-0", src, dst, b)
		ctx := context.Background()

		// 1. first update at height 10 -> submitted, lastHeight=10
		if err := m.updateClientTo(ctx, 10); err != nil {
			t.Fatalf("update to 10: %v", err)
		}
		if len(dst.updates) != 1 || dst.updates[0].Height != 10 {
			t.Fatalf("want one update at height 10, got %+v", dst.updates)
		}

		// 2. re-request height 10 (<= lastHeight) -> skipped before Build
		builtBefore := b.built
		if err := m.updateClientTo(ctx, 10); err != nil {
			t.Fatalf("re-update to 10: %v", err)
		}
		if len(dst.updates) != 1 {
			t.Fatalf("append-only violated: expected no new submit, got %d", len(dst.updates))
		}
		if b.built != builtBefore {
			t.Fatalf("expected Build skipped for covered height, but it ran")
		}

		// 3. height 20 but builder reports the client already current (Height 0)
		b.forceHeight = 0 // header[0]=20 -> but simulate no-op: override to 0? no: use a builder that returns stale
		b.forceHeight = 5 // stale (<= lastHeight 10): must be skipped without submit
		if err := m.updateClientTo(ctx, 20); err != nil {
			t.Fatalf("update to 20 (stale build): %v", err)
		}
		if len(dst.updates) != 1 {
			t.Fatalf("stale/no-op update must not submit, got %d submits", len(dst.updates))
		}

		// 4. height 20, builder returns a real advance -> submitted, lastHeight=20
		b.forceHeight = 0 // back to echoing the requested height (20)
		if err := m.updateClientTo(ctx, 20); err != nil {
			t.Fatalf("update to 20: %v", err)
		}
		if len(dst.updates) != 2 || dst.updates[1].Height != 20 {
			t.Fatalf("want second update at height 20, got %+v", dst.updates)
		}
	})

	// A failed submission must leave lastHeight where it was. lastHeight is what the
	// module believes the destination client covers; advancing it on a failure makes
	// the module skip the very update that did not land, and the client then sits at
	// an older height than every later decision assumes.
	//
	// This is a named failure mode for this repo — "never advance a
	// timestamp/cursor/tracker on a failed operation" — and the check that catches it
	// is the retry: if lastHeight moved, the second attempt returns early instead of
	// submitting.
	t.Run("does not advance lastHeight on a failed submit", func(t *testing.T) {
		src := &mockSource{}
		dst := &mockDest{updateErr: errors.New("submit reverted")}
		m := NewModule("test", "client-0", src, dst, &mockBuilder{})
		ctx := context.Background()

		if err := m.updateClientTo(ctx, 10); err == nil {
			t.Fatal("a failing UpdateClient must surface as an error")
		}
		if m.lastHeight != 0 {
			t.Fatalf("lastHeight advanced to %d on a failed submit; the client is still at 0", m.lastHeight)
		}

		// The destination recovers: the same height must now actually be submitted.
		dst.updateErr = nil
		if err := m.updateClientTo(ctx, 10); err != nil {
			t.Fatalf("retry after recovery: %v", err)
		}
		if len(dst.updates) != 1 || dst.updates[0].Height != 10 {
			t.Fatalf("retry did not submit the update that previously failed, got %+v", dst.updates)
		}
	})

	// The stale-update guard is inclusive: an update AT the height already trusted is
	// as pointless as one below it, and submitting it spends a proof and a tx to move
	// the client nowhere. The equal case is the one worth pinning — a strictly-less
	// comparison still passes every test that only exercises heights below lastHeight.
	t.Run("skips an update at a height already trusted", func(t *testing.T) {
		src := &mockSource{}
		dst := &mockDest{}
		b := &mockBuilder{}
		m := NewModule("test", "client-0", src, dst, b)
		ctx := context.Background()

		if err := m.updateClientTo(ctx, 10); err != nil {
			t.Fatalf("first update: %v", err)
		}
		if len(dst.updates) != 1 {
			t.Fatalf("want the first update submitted, got %+v", dst.updates)
		}

		// Ask for a higher height (so the pre-Build guard lets it through) but have the
		// builder report exactly the height already trusted.
		b.forceHeight = 10
		if err := m.updateClientTo(ctx, 20); err != nil {
			t.Fatalf("update at trusted height: %v", err)
		}
		if len(dst.updates) != 1 {
			t.Fatalf("submitted an update at the height already trusted: %+v", dst.updates)
		}
		if m.lastHeight != 10 {
			t.Fatalf("lastHeight = %d, want it unchanged at 10", m.lastHeight)
		}
	})

	// A module with no observer must still update. The option is optional, and the
	// nil check is what stops every path that does not set one from panicking on its
	// first client update.
	//
	// The builder MUST report a non-zero TrustedAt here. recordClientUpdate is
	// guarded by `observer != nil && !TrustedAt.IsZero()`, so a zero timestamp short-
	// circuits before the nil check is reached -- an earlier version of this test
	// used the default builder and passed with the nil guard deleted.
	t.Run("works without an observer", func(t *testing.T) {
		b := &mockBuilder{trustedAt: time.Unix(1_700_000_000, 0)}
		m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, b)
		if err := m.updateClientTo(context.Background(), 10); err != nil {
			t.Fatalf("update without an observer: %v", err)
		}
	})

	// Validator churn can outrun the trust-overlap requirement: an update from the
	// trusted height straight to the head then cannot be verified at all, and the
	// only way across is a chain of updates that each overlap the last.
	//
	// That mechanism lived only in the manual `update-client` command, so the
	// AUTOMATIC path could not cross a churn gap: the client stayed stuck until a
	// person typed a command, while refreshLoop retried and failed underneath.
	t.Run("climbs through intermediate hops", func(t *testing.T) {
		t.Run("crosses a churn gap without a manual command", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &mockDest{}
			b := &mockBuilder{hops: []uint64{30, 60, 90}} // two hops, then the target
			m := NewModule("test", "client-0", src, dst, b)

			if err := m.updateClientTo(context.Background(), 90); err != nil {
				t.Fatalf("climb: %v", err)
			}
			if len(dst.updates) != 3 {
				t.Fatalf("submitted %d updates, want the two hops and the target", len(dst.updates))
			}
			for i, want := range []uint64{30, 60, 90} {
				if dst.updates[i].Height != want {
					t.Fatalf("update %d at height %d, want %d: each hop must overlap the last",
						i, dst.updates[i].Height, want)
				}
			}
			if m.lastHeight != 90 {
				t.Fatalf("lastHeight = %d, want 90", m.lastHeight)
			}
		})

		// An adapter that never hops must behave exactly as before: one Build, one
		// update, no loop.
		t.Run("an adapter that does not hop is unchanged", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &mockDest{}
			b := &mockBuilder{}
			m := NewModule("test", "client-0", src, dst, b)

			if err := m.updateClientTo(context.Background(), 42); err != nil {
				t.Fatalf("update: %v", err)
			}
			if b.built != 1 || len(dst.updates) != 1 {
				t.Fatalf("built %d, submitted %d; want exactly one of each", b.built, len(dst.updates))
			}
		})

		// Each hop costs a proof and a transaction, so an unbounded climb is an
		// expensive silent loop -- the third failure class dressed as progress.
		t.Run("stops at the ceiling and says how far it got", func(t *testing.T) {
			endless := make([]uint64, MaxClientUpdateHops+5)
			for i := range endless {
				endless[i] = uint64(i + 1)
			}
			src := &mockSource{latest: 1000}
			dst := &mockDest{}
			b := &mockBuilder{hops: endless}
			m := NewModule("test", "client-0", src, dst, b)

			err := m.updateClientTo(context.Background(), 1000)
			if err == nil {
				t.Fatal("an endless climb reported success")
			}
			if !strings.Contains(err.Error(), "hop") {
				t.Errorf("error = %v; it must say the climb ran out, not look like a build failure", err)
			}
			if len(dst.updates) > MaxClientUpdateHops {
				t.Fatalf("submitted %d updates past a ceiling of %d", len(dst.updates), MaxClientUpdateHops)
			}
		})

		// A hop that installs nothing must end the climb rather than repeat it. This
		// is the anti-spin guard, and without it a builder stuck on one height would
		// burn the whole ceiling in proofs.
		t.Run("a hop that does not advance ends the climb", func(t *testing.T) {
			src := &mockSource{latest: 100}
			dst := &mockDest{}
			b := &mockBuilder{hops: []uint64{50, 50, 50, 50}}
			m := NewModule("test", "client-0", src, dst, b)

			if err := m.updateClientTo(context.Background(), 90); err != nil {
				t.Fatalf("climb: %v", err)
			}
			if len(dst.updates) != 1 {
				t.Fatalf("submitted %d updates for a builder stuck at one height, want 1", len(dst.updates))
			}
		})
	})
}

func TestWithTimeoutScanner(t *testing.T) {
	t.Run("installs the scan function and its interval", func(t *testing.T) {
		called := 0
		m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
			WithTimeoutScanner(5*time.Second, func(context.Context) { called++ }))

		if m.scan == nil {
			t.Fatal("no scan function installed; Run would skip the timeout loop entirely")
		}
		if m.scanInterval != 5*time.Second {
			t.Fatalf("scanInterval = %s, want 5s", m.scanInterval)
		}
		m.scan(context.Background())
		if called != 1 {
			t.Fatalf("the installed function ran %d times, want 1", called)
		}
	})

	t.Run("a non-positive interval falls back to the default", func(t *testing.T) {
		// Zero is what a caller passes to mean "use the standard cadence" --
		// run_adapters.go does exactly that. Left as zero it becomes
		// time.NewTicker(0), which panics and takes the module down at startup.
		for _, interval := range []time.Duration{0, -time.Second} {
			m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
				WithTimeoutScanner(interval, func(context.Context) {}))
			if m.scanInterval != defaultScanInterval {
				t.Errorf("interval %s produced scanInterval %s, want the default %s",
					interval, m.scanInterval, defaultScanInterval)
			}
		}
	})
}

func TestWithClientUpdateObserver(t *testing.T) {
	var seen []time.Time
	trustedAt := time.Unix(1_700_000_000, 0)
	src := &mockSource{}
	dst := &mockDest{}
	b := &mockBuilder{trustedAt: trustedAt}
	m := NewModule("test", "client-0", src, dst, b,
		WithClientUpdateObserver(func(at time.Time) { seen = append(seen, at) }))

	if err := m.updateClientTo(context.Background(), 10); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("observer called %d times for one client update, want 1", len(seen))
	}
	// The observed value is the height the client now trusts, not the moment the
	// relayer happened to submit it: the freshness metric is about the CLIENT.
	if !seen[0].Equal(trustedAt) {
		t.Fatalf("observer got %s, want the update's TrustedAt %s", seen[0], trustedAt)
	}
}

// withFastScanTick shortens the scan cadence so a loop test finishes in
// milliseconds rather than waiting out the 30s production interval.
func withFastScanTick(m *Module) { m.scanInterval = time.Millisecond }

func TestScanLoop(t *testing.T) {
	t.Run("keeps scanning until the context is cancelled", func(t *testing.T) {
		// The timeout sweep is what refunds a packet that was relayed but never
		// delivered. A loop that runs once and stops leaves the pending set
		// growing and the escrow locked, with nothing in the log to say so.
		var mu sync.Mutex
		scans := 0
		m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
			WithTimeoutScanner(time.Second, func(context.Context) {
				mu.Lock()
				scans++
				mu.Unlock()
			}))
		withFastScanTick(m)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); m.scanLoop(ctx) }()

		waitFor(t, ctx, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return scans >= 3
		}, "the scan loop did not keep running")
		cancel()
		wg.Wait()
	})

	t.Run("returns immediately on cancellation, not at the next tick", func(t *testing.T) {
		// The interval here is deliberately LONG and the deadline short. Waiting
		// for the next tick would be correct-looking and still wrong: in
		// production the cadence is 30s, and a shutdown that waits it out spends
		// most of the 45s process budget on one worker.
		//
		// A short interval hides this -- an earlier version used a 1ms tick and a
		// 2s deadline, which passed with the ctx.Done() arm deleted entirely.
		const scanInterval = 30 * time.Second
		m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
			WithTimeoutScanner(scanInterval, func(context.Context) {}))

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { m.scanLoop(ctx); close(done) }()

		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("scan loop did not return within 2s of cancellation on a %s cadence; "+
				"shutdown waits for the next tick", scanInterval)
		}
	})

	t.Run("never scans on an already-cancelled context", func(t *testing.T) {
		var mu sync.Mutex
		scans := 0
		m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
			WithTimeoutScanner(time.Second, func(context.Context) {
				mu.Lock()
				scans++
				mu.Unlock()
			}))
		withFastScanTick(m)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		m.scanLoop(ctx)

		mu.Lock()
		defer mu.Unlock()
		if scans != 0 {
			t.Fatalf("ran %d scans on an already-cancelled context", scans)
		}
	})

	t.Run("does not start a sweep once cancellation and a tick are both ready", func(t *testing.T) {
		// The case the re-check inside the tick branch exists for. `select` picks
		// at random among ready cases, so when the ticker has already fired and
		// the context is done, half the time it takes the tick -- and without the
		// re-check a sweep begins during shutdown, issuing RPCs nobody is left to
		// read.
		//
		// Forcing that state deterministically is not possible; forcing it REPEATEDLY
		// is. Each round blocks inside the scan long enough for the ticker to fire,
		// cancels while blocked, and then releases. With the re-check the loop
		// returns every time; without it, each round is an independent coin flip,
		// so 20 rounds leave a 1-in-a-million chance of not catching it. The test
		// never fails spuriously -- correct code scans exactly once per round.
		for round := 0; round < 20; round++ {
			var mu sync.Mutex
			scans := 0
			blocked := make(chan struct{})
			release := make(chan struct{})

			ctx, cancel := context.WithCancel(context.Background())
			m := NewModule("test", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
				WithTimeoutScanner(time.Second, func(context.Context) {
					mu.Lock()
					first := scans == 0
					scans++
					mu.Unlock()
					if first {
						close(blocked)
						<-release
					}
				}))
			withFastScanTick(m)

			done := make(chan struct{})
			go func() { m.scanLoop(ctx); close(done) }()

			<-blocked                        // inside the first scan
			time.Sleep(5 * time.Millisecond) // let the 1ms ticker fire while blocked
			cancel()                         // now both cases are ready
			close(release)

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatalf("round %d: scan loop did not return", round)
			}

			mu.Lock()
			got := scans
			mu.Unlock()
			if got != 1 {
				t.Fatalf("round %d: ran %d sweeps, want 1; a sweep started after cancellation", round, got)
			}
		}
	})
}

type drainingSource struct {
	started chan struct{}
	stopped chan struct{}
}

func (s *drainingSource) Chain() chain.ChainType { return chain.Cosmos }

func (s *drainingSource) Subscribe(ctx context.Context, _ func(context.Context, []chain.Event) []int) error {
	close(s.started)
	<-ctx.Done()
	close(s.stopped)
	return ctx.Err()
}

func (s *drainingSource) LatestHeight(context.Context) (uint64, error) { return 0, nil }

func (s *drainingSource) RelayableHeight(context.Context) (uint64, error) { return 0, nil }

func (s *drainingSource) QueryHeader(context.Context, uint64) ([]byte, error) { return nil, nil }

func (s *drainingSource) MembershipProof(context.Context, []byte, uint64, chain.EventType) ([]byte, error) {
	return nil, nil
}

func (s *drainingSource) NonMembershipProof(context.Context, []byte, uint64) ([]byte, error) {
	return nil, nil
}

func TestModule(t *testing.T) {
	// Status is the read side of a running path, so the two things that make it
	// usable during an incident are that it reports the oldest wait first and that
	// it answers at all while the path is busy.
	t.Run("status", func(t *testing.T) {
		t.Run("reports the waiting set oldest first", func(t *testing.T) {
			m := NewModule("test", "client-0", &mockSource{latest: 100, relayable: 90}, &mockDest{}, &mockBuilder{})

			// Distinct ages, inserted youngest first so a passing test cannot be map
			// iteration order.
			base := time.Now()
			m.waits.now = func() time.Time { return base }
			m.waits.observe(chain.Event{Type: chain.AckPacket, Sequence: 2, Height: 8})
			m.waits.now = func() time.Time { return base.Add(-2 * time.Minute) }
			m.waits.observe(chain.Event{Type: chain.SendPacket, Sequence: 1, Height: 7})
			m.waits.now = func() time.Time { return base.Add(time.Minute) }

			got := m.Status(context.Background())
			if len(got.Waiting) != 2 {
				t.Fatalf("waiting = %+v, want both packets", got.Waiting)
			}
			if got.Waiting[0].Sequence != 1 {
				t.Fatalf("first entry is seq=%d; the oldest wait must lead, because it is the one most likely to be stuck",
					got.Waiting[0].Sequence)
			}
			if got.Waiting[0].Age <= got.Waiting[1].Age {
				t.Fatalf("ages = %s, %s: not sorted oldest first", got.Waiting[0].Age, got.Waiting[1].Age)
			}
			if got.Waiting[0].AgeText == "" {
				t.Error("no human-readable age; the raw nanoseconds are for machines, not the operator reading this")
			}
		})

		t.Run("reports the heights the operator is asking for", func(t *testing.T) {
			m := NewModule("test", "client-0", &mockSource{latest: 100, relayable: 90}, &mockDest{}, &mockBuilder{})
			m.lastHeight = 88

			got := m.Status(context.Background())
			if got.TrustedHeight != 88 {
				t.Errorf("trusted height = %d, want 88", got.TrustedHeight)
			}
			if got.RelayableHeight != 90 {
				t.Errorf("relayable height = %d, want 90: this is the number that answers 'why has nothing moved'", got.RelayableHeight)
			}
			if got.Path != "test" || got.ClientID != "client-0" {
				t.Errorf("identity = %s/%s, want the module's", got.Path, got.ClientID)
			}
		})

		// The one RPC in the snapshot must not be able to fail the snapshot: an
		// unreachable source is exactly when someone runs this.
		t.Run("a failed probe is reported, not fatal", func(t *testing.T) {
			src := &mockSource{latest: 100, relayableErr: errors.New("source rpc down")}
			m := NewModule("test", "client-0", src, &mockDest{}, &mockBuilder{})
			m.lastHeight = 88

			got := m.Status(context.Background())
			if got.RelayableHeightErr == "" {
				t.Fatal("a failed probe produced no error text; the field would read as height 0")
			}
			if got.TrustedHeight != 88 {
				t.Errorf("trusted height = %d, want the in-memory value to survive a failed probe", got.TrustedHeight)
			}
		})

		// updateClientTo holds m.mu across the builder, which on the Cosmos->EVM path
		// is across proof generation. A status command that blocks for the length of a
		// Groth16 proof is useless at the moment someone runs it, so the busy lock is
		// reported as an answer instead of waited on.
		t.Run("answers while a client update holds the lock", func(t *testing.T) {
			m := NewModule("test", "client-0", &mockSource{latest: 100, relayable: 90}, &mockDest{}, &mockBuilder{})
			m.mu.Lock()
			defer m.mu.Unlock()

			done := make(chan Status, 1)
			go func() { done <- m.Status(context.Background()) }()
			select {
			case got := <-done:
				if !got.UpdateInFlight {
					t.Fatal("the snapshot did not say an update was in flight, so a stale trusted height reads as current")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Status blocked on the module lock: it would hang for the length of a proof")
			}
		})
	})

	t.Run("clean cancellation drains subscriber", func(t *testing.T) {
		src := &drainingSource{started: make(chan struct{}), stopped: make(chan struct{})}
		m := NewModule("test", "client", src, &mockDest{}, &mockBuilder{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.Run(ctx) }()
		<-src.started
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run cancellation error = %v, want nil", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not drain after cancellation")
		}
		select {
		case <-src.stopped:
		default:
			t.Fatal("Run returned before subscriber stopped")
		}
	})

	t.Run("reports internal subscriber cancellation", func(t *testing.T) {
		m := NewModule("test", "client", internallyCancelledSource{}, &mockDest{}, &mockBuilder{})
		err := m.Run(context.Background())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want wrapped context.Canceled", err)
		}
	})

	// The loops must reach the status snapshot, or the heartbeat is a field nobody
	// reads. This is the wiring half: the type above can be perfect while nothing
	// calls it.
	t.Run("status carries the loop health", func(t *testing.T) {
		m := NewModule("test", "client-0", &mockSource{latest: 100, relayable: 90}, &mockDest{}, &mockBuilder{})
		m.beats.record(workerTimeoutScan, "")

		got := m.Status(context.Background())
		if len(got.Loops) != 1 || got.Loops[0].Worker != workerTimeoutScan {
			t.Fatalf("loops = %+v, want the timeout scan's beat", got.Loops)
		}
	})
}

type internallyCancelledSource struct{}

func (internallyCancelledSource) Chain() chain.ChainType { return chain.Cosmos }

func (internallyCancelledSource) Subscribe(context.Context, func(context.Context, []chain.Event) []int) error {
	return context.Canceled
}

func (internallyCancelledSource) LatestHeight(context.Context) (uint64, error) { return 0, nil }

func (internallyCancelledSource) RelayableHeight(context.Context) (uint64, error) {
	return 0, nil
}

func (internallyCancelledSource) QueryHeader(context.Context, uint64) ([]byte, error) {
	return nil, nil
}

func (internallyCancelledSource) MembershipProof(context.Context, []byte, uint64, chain.EventType) ([]byte, error) {
	return nil, nil
}

func (internallyCancelledSource) NonMembershipProof(context.Context, []byte, uint64) ([]byte, error) {
	return nil, nil
}

// stuckWorkerSource is what the real Cosmos and EVM adapters became: on
// cancellation they drain their own goroutines, and if one of them never
// returns they report THAT rather than the cancellation that triggered it.
type stuckWorkerSource struct {
	started chan struct{}
	err     error
}

func (s *stuckWorkerSource) Chain() chain.ChainType { return chain.Cosmos }

func (s *stuckWorkerSource) Subscribe(ctx context.Context, _ func(context.Context, []chain.Event) []int) error {
	close(s.started)
	<-ctx.Done()
	return s.err
}

func (s *stuckWorkerSource) LatestHeight(context.Context) (uint64, error) { return 0, nil }

func (s *stuckWorkerSource) RelayableHeight(context.Context) (uint64, error) { return 0, nil }

func (s *stuckWorkerSource) QueryHeader(context.Context, uint64) ([]byte, error) { return nil, nil }

func (s *stuckWorkerSource) MembershipProof(context.Context, []byte, uint64, chain.EventType) ([]byte, error) {
	return nil, nil
}

func (s *stuckWorkerSource) NonMembershipProof(context.Context, []byte, uint64) ([]byte, error) {
	return nil, nil
}

// Reported by @DongLieu on #434. Two things had to line up for a stuck worker to
// be reported as a clean shutdown, and both were true:
//
//  1. Run's select takes ctx.Done() on SIGTERM, sets runErr = context.Canceled,
//     and NEVER reads subscribeErr -- so whatever Subscribe returned was dropped.
//  2. A bare context.Canceled during shutdown is normalised to nil.
//
// So a source that knew one of its own goroutines had not stopped had no way to
// say so: the value it returned was discarded, and the value that replaced it
// meant "stopped cleanly". The process exited 0 with a goroutine still running.
func TestRun_ReportsASourceThatCouldNotStopItsOwnWorkers(t *testing.T) {
	stuck := errors.New("cosmos source: shutdown drain timed out after 20s; workers still running: [cosmos-subscribe]")
	src := &stuckWorkerSource{started: make(chan struct{}), err: stuck}
	m := NewModule("test", "client", src, &mockDest{}, &mockBuilder{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	<-src.started
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run reported a clean shutdown while a named source worker was still running; " +
				"SIGTERM would exit 0 with the goroutine alive")
		}
		if !errors.Is(err, stuck) {
			t.Fatalf("Run error = %v, want it to carry the source's drain failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// The mirror of TestModule/clean cancellation drains subscriber: preferring the
// source's error must not turn an ordinary stop into a failure. A source that
// returns the cancellation it was given is still a clean shutdown.
func TestRun_StillExitsCleanWhenTheSourceOnlyReportsCancellation(t *testing.T) {
	src := &stuckWorkerSource{started: make(chan struct{}), err: context.Canceled}
	m := NewModule("test", "client", src, &mockDest{}, &mockBuilder{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	<-src.started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error = %v, want nil: an ordinary stop is not a failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestAckWatchLoop(t *testing.T) {
	send := chain.RelayPacket{Type: chain.SendPacket, Sequence: 7, Packet: []byte("pkt-7")}
	ack := chain.RelayPacket{Type: chain.AckPacket, Sequence: 7, Packet: []byte("pkt-7")}

	newModule := func(due AckDueFunc, settled AckSettledFunc, overdue OverdueAcksFunc) *Module {
		return NewModule("cosmos->eth", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
			WithAckWatch(0, 0, due, settled, overdue))
	}

	t.Run("a delivered receive makes the acknowledgement owed", func(t *testing.T) {
		var recorded [][]byte
		m := newModule(func(raw []byte) bool { recorded = append(recorded, raw); return true }, nil, func(time.Duration) []string { return nil })

		m.settleDelivered([]chain.RelayPacket{send})

		if len(recorded) != 1 || string(recorded[0]) != "pkt-7" {
			t.Fatalf("recorded %v, want the delivered packet: the debt starts when WE deliver, not when we see the send", recorded)
		}
	})

	t.Run("a relayed acknowledgement closes the debt", func(t *testing.T) {
		var cleared [][]byte
		m := newModule(nil, func(raw []byte) { cleared = append(cleared, raw) }, func(time.Duration) []string { return nil })

		m.settleDelivered([]chain.RelayPacket{ack})

		if len(cleared) != 1 || string(cleared[0]) != "pkt-7" {
			t.Fatalf("cleared %v, want the acknowledged packet", cleared)
		}
	})

	// A durable write that fails is the case where the record would exist only in
	// memory -- and a restart is the most likely reason the ack was missed in the
	// first place, so failing quietly here loses exactly the packet this watches.
	t.Run("a failed record is reported", func(t *testing.T) {
		m := newModule(func([]byte) bool { return false }, nil, func(time.Duration) []string { return nil })

		out := captureLog(func() { m.settleDelivered([]chain.RelayPacket{send}) })

		if !strings.Contains(out, "could not durably record the owed acknowledgement for seq=7") {
			t.Fatalf("a failed durable write must reach the log:\n%s", out)
		}
	})

	// The retry itself is NOT tested here any more: it moved into
	// services.ackDueLedger, because the module that records a debt is not the one
	// that settles it, so a per-module queue could never be cancelled by the
	// settlement. See services/ackdue_test.go -- "a settlement cancels a record
	// still queued for writing".

	// The second module gets the hooks with a nil overdue reporter: the tracker
	// is shared, so a second watch loop would report every overdue ack twice.
	// nil must leave the loop unstarted while recording and settlement still work
	// -- that combination is what lets the debt be recorded on one relay
	// direction and cleared on the other.
	t.Run("a nil overdue reporter still wires recording and settlement", func(t *testing.T) {
		var recorded, cleared [][]byte
		m := newModule(
			func(raw []byte) bool { recorded = append(recorded, raw); return true },
			func(raw []byte) { cleared = append(cleared, raw) },
			nil,
		)
		if m.overdueAcks != nil {
			t.Fatal("a nil reporter must stay nil; Module.Run starts the watch loop on it")
		}

		m.settleDelivered([]chain.RelayPacket{send})
		m.settleDelivered([]chain.RelayPacket{ack})

		if len(recorded) != 1 || len(cleared) != 1 {
			t.Fatalf("recorded %d and cleared %d, want 1 each without a watch loop", len(recorded), len(cleared))
		}
	})

	t.Run("the watch names overdue acknowledgements and stays quiet otherwise", func(t *testing.T) {
		var threshold time.Duration
		overdue := []string{"seq=7 src=08-wasm-0"}
		m := newModule(nil, nil, func(d time.Duration) []string { threshold = d; return overdue })
		m.ackWatchInterval = time.Millisecond

		ctx, cancel := context.WithCancel(context.Background())
		out := captureLog(func() {
			done := make(chan struct{})
			go func() { defer close(done); m.ackWatchLoop(ctx) }()
			time.Sleep(20 * time.Millisecond)
			cancel()
			<-done
		})

		if !strings.Contains(out, "seq=7 src=08-wasm-0") {
			t.Fatalf("an overdue acknowledgement must be named:\n%s", out)
		}
		if threshold != defaultAckOverdueAfter {
			t.Fatalf("watch asked for packets older than %s, want the default %s", threshold, defaultAckOverdueAfter)
		}

		overdue = nil
		quiet := captureLog(func() {
			ctx2, cancel2 := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); m.ackWatchLoop(ctx2) }()
			time.Sleep(20 * time.Millisecond)
			cancel2()
			<-done
		})
		if strings.Contains(quiet, "acknowledgement(s) owed") {
			t.Fatalf("nothing overdue must produce no line:\n%s", quiet)
		}
	})
}

func TestFlushOnce(t *testing.T) {
	send := chain.Event{Type: chain.SendPacket, Sequence: 7, Height: 40, Raw: []byte("old-packet")}

	t.Run("relays a packet the scan never saw", func(t *testing.T) {
		src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{send}}
		dst := &mockDest{}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

		out := captureLog(func() { m.flushOnce(context.Background(), src) })

		if len(dst.relayed) != 1 {
			t.Fatalf("relayed %d packets, want 1: the flush exists to relay what the scan cannot reach", len(dst.relayed))
		}
		if !strings.Contains(out, "packet flush found 1 packet(s)") {
			t.Fatalf("a flush that finds work must say so:\n%s", out)
		}
	})

	// An outstanding source commitment only says the packet was never acked BACK.
	// A delivered packet whose ack is in flight still has one, and re-proving it
	// every pass for as long as the ack takes is pure waste.
	t.Run("skips packets the destination already holds a receipt for", func(t *testing.T) {
		src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{send}}
		dst := &mockDest{hasReceipt: true}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

		out := captureLog(func() { m.flushOnce(context.Background(), src) })

		if len(dst.relayed) != 0 {
			t.Fatalf("relayed %d packets, want 0: the destination already has a receipt", len(dst.relayed))
		}
		if strings.Contains(out, "packet flush found") {
			t.Fatalf("a pass that finds nothing unsettled must stay quiet:\n%s", out)
		}
	})

	// The receipt check has to be serialized with submission. It used to run
	// entirely outside batchMu, so a live subscription batch could relay the
	// packet between the check and the lock -- and the flush then submitted a
	// duplicate receive: an on-chain revert, wasted gas, and a permanent failure
	// that takes any other packet folded into the same batch down with it.
	t.Run("re-checks receipts under the batch lock", func(t *testing.T) {
		src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{send}}
		// Outstanding at the first check; delivered by the time the lock is held.
		dst := &mockDest{receiptAnswers: []bool{false, true}}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

		out := captureLog(func() { m.flushOnce(context.Background(), src) })

		if len(dst.relayed) != 0 {
			t.Fatalf("relayed %d packet(s), want 0: it was delivered between the two checks, so this is a duplicate receive", len(dst.relayed))
		}
		if strings.Contains(out, "packet flush found") {
			t.Fatalf("a pass whose packets were relayed underneath it must stay quiet:\n%s", out)
		}
	})

	// Dropping on a failed receipt query would silently skip exactly the packet
	// this loop exists to find, and the next pass would ask the same broken
	// endpoint again.
	t.Run("a failed receipt check relays rather than skips", func(t *testing.T) {
		src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{send}}
		dst := &mockDest{receiptErr: errors.New("rpc down")}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

		captureLog(func() { m.flushOnce(context.Background(), src) })

		if len(dst.relayed) != 1 {
			t.Fatalf("relayed %d packets, want 1: an unknown receipt must not be read as delivered", len(dst.relayed))
		}
	})

	t.Run("an enumeration failure is logged and relays nothing", func(t *testing.T) {
		src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, listErr: errors.New("query failed")}
		dst := &mockDest{}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

		out := captureLog(func() { m.flushOnce(context.Background(), src) })

		if len(dst.relayed) != 0 {
			t.Fatalf("relayed %d packets on a failed query, want 0", len(dst.relayed))
		}
		if !strings.Contains(out, "packet flush:") {
			t.Fatalf("a failed enumeration must reach the log:\n%s", out)
		}
	})

	// The enumeration backstop lists acknowledgements as well as sends, and a
	// receipt is the wrong question for an acknowledgement: it lives on the OTHER
	// chain, so asking this destination answers "not delivered" forever and the ack
	// is re-submitted every pass. A destination that can answer per type is asked
	// per type, and one that cannot is not asked a question it would get wrong.
	t.Run("settles acknowledgements by commitment not receipt", func(t *testing.T) {
		ack := chain.Event{Type: chain.AckPacket, Sequence: 7, Height: 40, Raw: []byte("old-packet"), AckBytes: [][]byte{[]byte("ok")}}

		t.Run("an acknowledgement the destination already settled is dropped", func(t *testing.T) {
			src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{ack}}
			dst := &checkingDest{settled: map[string]bool{settledKey(ack): true}}
			m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

			m.flushOnce(context.Background(), src)

			if len(dst.relayed) != 0 {
				t.Fatalf("relayed %d packets, want 0: the destination no longer holds the send's commitment, so the ack landed", len(dst.relayed))
			}
			if len(dst.asked) == 0 || dst.asked[0] != chain.AckPacket {
				t.Fatalf("destination asked about %v, want the event's own type (ack)", dst.asked)
			}
		})

		t.Run("an acknowledgement still owed is relayed", func(t *testing.T) {
			src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{ack}}
			dst := &checkingDest{settled: map[string]bool{}}
			m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

			m.flushOnce(context.Background(), src)

			if len(dst.relayed) != 1 || dst.relayed[0].Type != chain.AckPacket {
				t.Fatalf("relayed %+v, want the one owed acknowledgement", dst.relayed)
			}
		})

		// The fallback: a destination with only a receipt check must not have that
		// check applied to an acknowledgement. hasReceipt is true here, and it
		// would drop the ack if the receipt question were asked of it.
		t.Run("a receipt-only destination is not asked about an acknowledgement", func(t *testing.T) {
			src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{ack}}
			dst := &mockDest{hasReceipt: true}
			m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

			m.flushOnce(context.Background(), src)

			if len(dst.relayed) != 1 {
				t.Fatalf("relayed %d packets, want 1: a receipt says nothing about an acknowledgement and must not drop it", len(dst.relayed))
			}
		})

		t.Run("a receive is still judged by its receipt through the checker", func(t *testing.T) {
			send := chain.Event{Type: chain.SendPacket, Sequence: 8, Height: 40, Raw: []byte("old-packet")}
			src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}, candidate: []chain.Event{send}}
			dst := &checkingDest{settled: map[string]bool{settledKey(send): true}}
			m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

			m.flushOnce(context.Background(), src)

			if len(dst.relayed) != 0 {
				t.Fatalf("relayed %d packets, want 0: the destination holds the receipt", len(dst.relayed))
			}
		})
	})
}

// The restart boundary the ledger exists to close, and the one it did not until
// a review caught the order.
//
// A delivered receive moves from one durable obligation to another: the pending
// record that would time it out, and the owed-ack record that says an
// acknowledgement is expected. Between those two there must never be an instant
// where the packet is in neither -- the receive is committed on the destination,
// so it can no longer time out, and with no owed-ack record nothing durable is
// left from which the missing acknowledgement could be noticed.
func TestSettleDelivered_NeverLeavesAPacketInNeitherLedger(t *testing.T) {
	send := chain.RelayPacket{Type: chain.SendPacket, Sequence: 7, Packet: []byte("pkt-7")}

	// order records which durable obligation was touched, and when. The sequence
	// is the property: "recorded" must precede "untracked", because a crash
	// between them must leave the packet in the FIRST ledger, not in none.
	newModule := func(t *testing.T, durable bool, order *[]string) *Module {
		t.Helper()
		return NewModule("cosmos->eth", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
			WithPacketTracker(
				func([]byte, uint64) bool { return true },
				func([]byte) { *order = append(*order, "untracked") },
			),
			WithAckWatch(0, 0,
				func([]byte) bool { *order = append(*order, "recorded"); return durable },
				nil, nil),
		)
	}

	t.Run("records the owed acknowledgement before releasing the pending record", func(t *testing.T) {
		var order []string
		m := newModule(t, true, &order)

		m.settleDelivered([]chain.RelayPacket{send})

		want := []string{"recorded", "untracked"}
		if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
			t.Fatalf("order = %v, want %v: a crash between the two must land in the owed-ack ledger, not in neither",
				order, want)
		}
	})

	// The write failing is the case the reviewer named, and it is the easier half:
	// the ORDER covers the crash, this covers the error return.
	t.Run("keeps the pending record when the owed-ack write does not reach disk", func(t *testing.T) {
		var order []string
		m := newModule(t, false, &order)

		m.settleDelivered([]chain.RelayPacket{send})

		if len(order) != 1 || order[0] != "recorded" {
			t.Fatalf("order = %v; the pending record was released even though the owed-ack write failed, "+
				"leaving the packet in neither durable ledger", order)
		}
	})

	// A path with no return leg has no second obligation to hand the packet to,
	// so holding the pending record forever would strand it instead of protecting
	// it. Per-destination gating leaves ackDue nil there.
	t.Run("releases the pending record when there is no acknowledgement ledger", func(t *testing.T) {
		var order []string
		m := NewModule("cosmos->l2", "client-0", &mockSource{}, &mockDest{}, &mockBuilder{},
			WithPacketTracker(
				func([]byte, uint64) bool { return true },
				func([]byte) { order = append(order, "untracked") },
			),
		)

		m.settleDelivered([]chain.RelayPacket{send})

		if len(order) != 1 || order[0] != "untracked" {
			t.Fatalf("order = %v, want the pending record released: with no ledger to hand it to, keeping it strands the packet",
				order)
		}
	})
}

// waitForSoft is waitFor without the t.Fatal: the caller decides what a timeout
// means, because here it is the assertion rather than a broken test.
func waitForSoft(ctx context.Context, cond func() bool, ok *bool) {
	for {
		if cond() {
			return
		}
		select {
		case <-ctx.Done():
			*ok = false
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// countingExpiryDest counts expiry queries -- one per refresh ATTEMPT, since it
// is the first call the loop makes -- and can be healed mid-test.
type countingExpiryDest struct {
	mockDest
	mu             sync.Mutex
	n              int
	expiresErr     error
	expiresAt      time.Time
	trustingPeriod time.Duration
}

func (d *countingExpiryDest) ClientExpiresAt(context.Context, string) (time.Time, time.Duration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	return d.expiresAt, d.trustingPeriod, d.expiresErr
}

func (d *countingExpiryDest) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func (d *countingExpiryDest) heal(expiresAt time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expiresErr, d.expiresAt = nil, expiresAt
}

func (d *countingExpiryDest) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expiresErr = err
}

func TestFlushLoop_RunsImmediately(t *testing.T) {
	src := &listerSource{mockSource: mockSource{latest: 100, relayable: 100}}
	m := NewModule("cosmos->eth", "client-0", src, &mockDest{}, &mockBuilder{})
	m.flushInterval = time.Hour // so only the immediate pass can run

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.flushLoop(ctx, src) }()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && src.callCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got := src.callCount(); got != 1 {
		t.Fatalf("enumeration ran %d times before the first tick, want 1", got)
	}
}

type listerSource struct {
	mockSource
	mu        sync.Mutex
	calls     int
	candidate []chain.Event
	listErr   error
}

func (l *listerSource) UnrelayedPackets(context.Context) ([]chain.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return append([]chain.Event(nil), l.candidate...), l.listErr
}

func (l *listerSource) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// handleBatch has two callers now -- the Subscribe callback and the flush loop,
// on separate goroutines -- and it writes m.lastWait, which no lock covered.
// Under -race that write is a data race without Module.batchMu.
//
// The race is the smaller half. Two batches in flight at once can each build and
// submit a client update, and can relay the same packet twice: the flush
// enumerates outstanding commitments, which includes the packets the scan is
// delivering at that moment.
//
// Overlap is measured from INSIDE the batch, through a source call handleBatch
// makes while holding the lock. Counting around the call instead would count
// goroutines queued on the lock, not batches running in it -- which is what a
// first version of this test did, and it failed against correct code.
type overlapProbeSource struct {
	mockSource
	inFlight atomic.Int32
	overlap  atomic.Bool
}

func (s *overlapProbeSource) RelayableHeight(ctx context.Context) (uint64, error) {
	if s.inFlight.Add(1) > 1 {
		s.overlap.Store(true)
	}
	time.Sleep(time.Millisecond) // widen the window a real batch would occupy
	s.inFlight.Add(-1)
	return s.mockSource.RelayableHeight(ctx)
}

// checkingDest is a destination that answers settlement PER EVENT TYPE, the
// capability chain.SettlementChecker adds over the bare receipt check.
type checkingDest struct {
	mockDest
	mu      sync.Mutex
	settled map[string]bool
	asked   []chain.EventType
}

func settledKey(e chain.Event) string { return fmt.Sprintf("%s/%d", e.Type, e.Sequence) }

func (d *checkingDest) PacketSettled(_ context.Context, e chain.Event) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, e.Type)
	return d.settled[settledKey(e)], nil
}

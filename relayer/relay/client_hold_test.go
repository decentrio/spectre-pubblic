package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relayer/chain"
	"relayer/services"
)

// statusDest is a mockDest that can report its client's status, like the EVM,
// Cosmos and L2 destinations do.
type statusDest struct {
	mockDest
	active      bool
	status      string
	statusErr   error
	statusCalls int
}

func (d *statusDest) ClientActive(context.Context) (bool, string, error) {
	d.statusCalls++
	return d.active, d.status, d.statusErr
}

// frozenRevert is what the EVM transaction handler returns for a receipt whose
// replay reverted with FrozenClientState().
var frozenRevert = fmt.Errorf("tx 0xabc reverted with FrozenClientState (status=0, gasUsed=1): %w", services.ErrClientNotActive)

func threePackets() []chain.Event {
	return []chain.Event{
		{Type: chain.SendPacket, Sequence: 1, Height: 3, Raw: []byte("a")},
		{Type: chain.AckPacket, Sequence: 2, Height: 4, Raw: []byte("b")},
		{Type: chain.SendPacket, Sequence: 3, Height: 5, Raw: []byte("c")},
	}
}

func TestClientHold(t *testing.T) {
	// #402: a frozen client's revert used to be permanent, so the batch was
	// isolated and every packet dropped one transaction at a time. It must hold
	// the whole batch instead, without a single extra transaction.
	t.Run("frozen revert holds every packet instead of dropping", func(t *testing.T) {
		dst := &mockDest{relayErr: frozenRevert}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		var rq []int
		out := captureLog(func() { rq = m.handleBatch(context.Background(), threePackets()) })

		if len(rq) != 3 {
			t.Fatalf("every packet must be re-queued, got %v", rq)
		}
		if dst.relayCalls != 1 {
			t.Fatalf("a frozen client must not be isolated packet by packet: %d RelayPackets calls", dst.relayCalls)
		}
		if strings.Contains(out, "DROP") {
			t.Fatalf("nothing may be dropped on a frozen client:\n%s", out)
		}
		if !strings.Contains(out, "[ATTENTION] destination client client-0 is not active") {
			t.Fatalf("the freeze must be named once, loudly:\n%s", out)
		}
		if got := m.Status(context.Background()).ClientHeld; !strings.Contains(got, "client-0") {
			t.Fatalf("status must report the hold, got %q", got)
		}

		// Held: the next batch spends nothing -- no update, no proof, no tx.
		built := m.builder.(*mockBuilder).built
		rq = m.handleBatch(context.Background(), threePackets())
		if len(rq) != 3 || dst.relayCalls != 1 || m.builder.(*mockBuilder).built != built {
			t.Fatalf("held batch must be re-queued untouched: rq=%v relayCalls=%d built=%d->%d",
				rq, dst.relayCalls, built, m.builder.(*mockBuilder).built)
		}
	})

	// A revert that reached the relayer without its selector is still not charged
	// against the packet when the destination says its client is frozen.
	t.Run("permanent revert on a frozen client is not dropped", func(t *testing.T) {
		dst := &statusDest{mockDest: mockDest{relayErr: chain.Permanent(errors.New("reverted"))}, status: "Frozen"}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		var rq []int
		out := captureLog(func() {
			rq = m.handleBatch(context.Background(), []chain.Event{{Type: chain.AckPacket, Sequence: 7, Height: 5, Raw: []byte("p")}})
		})
		if len(rq) != 1 {
			t.Fatalf("the packet must be kept, got %v", rq)
		}
		if strings.Contains(out, "DROP") || !strings.Contains(out, "is Frozen") {
			t.Fatalf("want a hold naming Frozen and no DROP:\n%s", out)
		}
	})

	// The guard only changes the frozen case: a poison packet on an active client
	// is still dropped, exactly as before.
	t.Run("permanent revert on an active client still drops", func(t *testing.T) {
		dst := &statusDest{mockDest: mockDest{relayErr: chain.Permanent(errors.New("reverted"))}, active: true, status: "Active"}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		rq := m.handleBatch(context.Background(), []chain.Event{{Type: chain.SendPacket, Height: 5, Raw: []byte("p")}})
		if len(rq) != 0 {
			t.Fatalf("a poison packet on an active client must still drop, got %v", rq)
		}
		if m.clientHold.Load() != nil {
			t.Fatal("an active client must not be held")
		}
	})

	// The client freezes while a permanently-failed batch is being isolated: the
	// packet that hit the freeze and every one after it are held, not dropped.
	t.Run("freeze during isolation holds the rest", func(t *testing.T) {
		dst := &freezingIsolationDest{}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		rq := m.handleBatch(context.Background(), threePackets())
		if len(dst.relayed) != 1 || string(dst.relayed[0].Packet) != "a" {
			t.Fatalf("the first singleton landed before the freeze, got %+v", dst.relayed)
		}
		if len(rq) != 2 || rq[0] != 1 || rq[1] != 2 {
			t.Fatalf("the packets after the freeze must be re-queued, got %v", rq)
		}
		// batch + "a" + "b": once the client refuses, "c" is not sent into it.
		if dst.relayCalls != 3 {
			t.Fatalf("isolation must stop at the freeze, got %d RelayPackets calls", dst.relayCalls)
		}
	})

	// The client update is what a frozen client refuses first on a path that is
	// not folding. That must arm the hold too, not read as a source failure.
	t.Run("refused client update holds", func(t *testing.T) {
		dst := &mockDest{updateErr: fmt.Errorf("evm: submit client update: %w", frozenRevert)}
		src := &mockSource{latest: 100}
		m := NewModule("cosmos->eth", "client-0", src, dst, &mockBuilder{})

		rq := m.handleBatch(context.Background(), threePackets())
		if len(rq) != 3 || dst.relayCalls != 0 {
			t.Fatalf("want all 3 held and no packet tx, got rq=%v relayCalls=%d", rq, dst.relayCalls)
		}
		if m.clientHold.Load() == nil {
			t.Fatal("a refused client update must arm the hold")
		}
		if !m.sourceProbeAt.IsZero() {
			t.Fatal("a frozen destination must not be blamed on the source")
		}
	})

	// The packet path's client update, refused without a selector: the status
	// fallback holds it, and the next batch spends no further update.
	t.Run("refused client update without selector holds", func(t *testing.T) {
		dst := &statusDest{mockDest: mockDest{updateErr: bareRevert}, status: "Frozen"}
		b := &mockBuilder{}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, b)

		if rq := m.handleBatch(context.Background(), threePackets()); len(rq) != 3 {
			t.Fatalf("want all 3 held, got %v", rq)
		}
		if m.clientHold.Load() == nil || !m.sourceProbeAt.IsZero() {
			t.Fatal("a frozen client must be held, and not blamed on the source")
		}
		built := b.built
		m.handleBatch(context.Background(), threePackets())
		if b.built != built {
			t.Fatalf("held: no further update may be built, got %d -> %d", built, b.built)
		}
	})

	// The fallback only changes the frozen case: a permanent update failure on an
	// active client keeps its previous handling.
	t.Run("refused client update on an active client is not held", func(t *testing.T) {
		dst := &statusDest{mockDest: mockDest{updateErr: bareRevert}, active: true, status: "Active"}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		m.handleBatch(context.Background(), threePackets())
		if m.clientHold.Load() != nil {
			t.Fatal("an active client must not be held")
		}
	})

	// #402 task: a relayer restarted against an already-frozen client says so at
	// startup, and resumes by itself once the client is active again.
	t.Run("startup check holds, probe lifts", func(t *testing.T) {
		dst := &statusDest{status: "Frozen"}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		out := captureLog(func() { m.checkClientAtStartup(context.Background()) })
		if !strings.Contains(out, "is Frozen") {
			t.Fatalf("startup must name the frozen client:\n%s", out)
		}
		if rq := m.handleBatch(context.Background(), threePackets()); len(rq) != 3 || dst.relayCalls != 0 {
			t.Fatalf("held at startup: want 3 re-queued and no tx, got rq=%v relayCalls=%d", rq, dst.relayCalls)
		}

		// Unfrozen. Before the probe is due nothing changes; once it is, the next
		// batch re-reads the status, lifts the hold and relays.
		dst.active, dst.status = true, "Active"
		m.handleBatch(context.Background(), threePackets())
		if dst.relayCalls != 0 {
			t.Fatal("the hold must not be re-checked before its probe is due")
		}
		m.batchMu.Lock()
		m.clientProbeAt = time.Now().Add(-time.Second)
		m.batchMu.Unlock()
		if rq := m.handleBatch(context.Background(), threePackets()); len(rq) != 0 || len(dst.relayed) != 3 {
			t.Fatalf("after unfreeze every packet relays: rq=%v relayed=%d", rq, len(dst.relayed))
		}
		if m.clientHold.Load() != nil {
			t.Fatal("the hold must be lifted")
		}
	})

	// A quiet path delivers no batch, so the refresh loop's check must be able to
	// lift the hold by itself -- or an unfrozen client is never refreshed again.
	t.Run("quiet path lifts the hold from the refresh check", func(t *testing.T) {
		dst := &statusDest{status: "Frozen"}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})
		m.checkClientAtStartup(context.Background())
		if !m.clientStillHeld(context.Background()) {
			t.Fatal("frozen at startup must be held")
		}

		dst.active, dst.status = true, "Active"
		m.batchMu.Lock()
		m.clientProbeAt = time.Now().Add(-time.Second)
		m.batchMu.Unlock()
		if m.clientStillHeld(context.Background()) {
			t.Fatal("an active client must lift the hold from the refresh check")
		}
	})

	// "Do not know" is neither answer: an unreadable status at startup must not
	// hold a healthy path, and one while held must not lift the hold.
	t.Run("status read errors change nothing", func(t *testing.T) {
		dst := &statusDest{statusErr: errors.New("rpc down")}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})
		m.checkClientAtStartup(context.Background())
		if m.clientHold.Load() != nil {
			t.Fatal("an unreadable status must not hold the path")
		}

		m.batchMu.Lock()
		m.noteClientInactive("Frozen", nil)
		m.clientProbeAt = time.Now().Add(-time.Second)
		m.batchMu.Unlock()
		if !m.clientStillHeld(context.Background()) {
			t.Fatal("an unreadable status must not lift the hold")
		}
	})
}

// freezingIsolationDest reverts the full batch permanently while its client is
// still active, then freezes after the first singleton lands.
type freezingIsolationDest struct {
	mockDest
	frozen bool
}

func (d *freezingIsolationDest) RelayPackets(_ context.Context, packets []chain.RelayPacket) (int, error) {
	d.relayCalls++
	if len(packets) > 1 {
		return 0, chain.Permanent(errors.New("poison reverted the batch"))
	}
	if d.frozen {
		return 0, frozenRevert
	}
	d.relayed = append(d.relayed, packets...)
	d.frozen = true
	return 1, nil
}

// frozenUpdateDest refuses every client update the way a frozen SpectreClient
// does, and reports a client that is already due for refresh.
type frozenUpdateDest struct {
	mockDest
	updateCalls atomic.Int32
	// updateErr overrides the refusal; nil means the recognised frozenRevert.
	updateErr error
}

func (d *frozenUpdateDest) UpdateClient(context.Context, string, chain.ClientUpdate) error {
	d.updateCalls.Add(1)
	if d.updateErr != nil {
		return d.updateErr
	}
	return fmt.Errorf("evm: submit client update: %w", frozenRevert)
}

// selectorlessFrozenDest refuses updates the way a frozen client does through an
// RPC that omits revert data -- a bare permanent failure -- while its status
// query still says Frozen.
type selectorlessFrozenDest struct {
	frozenUpdateDest
}

func (d *selectorlessFrozenDest) ClientActive(context.Context) (bool, string, error) {
	return false, "Frozen", nil
}

// bareRevert is what the EVM handler returns when the replay carried no revert
// data to match a selector against.
var bareRevert = fmt.Errorf("evm: submit client update: tx 0xabc reverted (status=0, gasUsed=1): %w",
	services.ErrPermanentRelayFailure)

func (d *frozenUpdateDest) ClientExpiresAt(context.Context, string) (time.Time, time.Duration, error) {
	return time.Now(), time.Hour, nil // inside the refresh margin: due now
}

// A client that freezes after startup on a QUIET path is noticed by the loops
// that keep running there. Each one must arm the hold on its first refused
// update and stop submitting, and Status must report HELD without waiting for a
// packet to reach handleBatch.
func TestClientHoldArmedByBackgroundUpdates(t *testing.T) {
	// How long the loop is left running after the hold is armed. Far more than
	// its fast cadence, far less than the hold's one-minute probe.
	const settle = 100 * time.Millisecond

	t.Run("refresh", func(t *testing.T) {
		withFastRefreshTick(t)
		dst := &frozenUpdateDest{}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); m.refreshLoop(ctx) }()

		waitFor(t, ctx, func() bool { return m.clientHold.Load() != nil }, "refresh never armed the hold")
		time.Sleep(settle)
		cancel()
		<-done

		if got := dst.updateCalls.Load(); got != 1 {
			t.Fatalf("refresh must stop after the first refused update, got %d submissions", got)
		}
		if got := m.Status(context.Background()).ClientHeld; got == "" {
			t.Fatal("status must report HELD on a quiet path")
		}
	})

	t.Run("periodic update", func(t *testing.T) {
		var calls atomic.Int32
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, &mockDest{}, &mockBuilder{},
			WithPeriodicUpdate(time.Millisecond, 0, func(context.Context) error {
				calls.Add(1)
				return fmt.Errorf("[RefreshCosmosClient] send tx (hop 0): %w", frozenRevert)
			}),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); m.periodicUpdateLoop(ctx) }()

		waitFor(t, ctx, func() bool { return m.clientHold.Load() != nil }, "periodic update never armed the hold")
		time.Sleep(settle)
		cancel()
		<-done

		if got := calls.Load(); got != 1 {
			t.Fatalf("periodic update must stop after the first refused rotation, got %d", got)
		}
	})

	// The revert reached the relayer without its selector. The status query is the
	// fallback: one refused submission, then held.
	t.Run("refresh, revert without selector", func(t *testing.T) {
		withFastRefreshTick(t)
		dst := &selectorlessFrozenDest{frozenUpdateDest{updateErr: bareRevert}}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); m.refreshLoop(ctx) }()

		waitFor(t, ctx, func() bool { return m.clientHold.Load() != nil }, "refresh never armed the hold")
		time.Sleep(settle)
		cancel()
		<-done

		if got := dst.updateCalls.Load(); got != 1 {
			t.Fatalf("want 1 refused submission, got %d", got)
		}
	})

	t.Run("periodic update, revert without selector", func(t *testing.T) {
		var calls atomic.Int32
		dst := &selectorlessFrozenDest{}
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, dst, &mockBuilder{},
			WithPeriodicUpdate(time.Millisecond, 0, func(context.Context) error {
				calls.Add(1)
				return fmt.Errorf("[RefreshCosmosClient] send tx (hop 0): %w", bareRevert)
			}),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); m.periodicUpdateLoop(ctx) }()

		waitFor(t, ctx, func() bool { return m.clientHold.Load() != nil }, "periodic update never armed the hold")
		time.Sleep(settle)
		cancel()
		<-done

		if got := calls.Load(); got != 1 {
			t.Fatalf("want 1 refused rotation, got %d", got)
		}
	})

	// Any other failure keeps its own backoff and must not hold the path.
	t.Run("ordinary failure does not hold", func(t *testing.T) {
		m := NewModule("cosmos->eth", "client-0", &mockSource{latest: 100}, &mockDest{}, &mockBuilder{})
		if m.armClientHoldFrom(context.Background(), errors.New("rpc unavailable")) || m.clientHold.Load() != nil {
			t.Fatal("an ordinary failure must not arm the hold")
		}
	})
}

// sdkSimulationRejection is what a folded Cosmos/L2 batch returns on a frozen
// client: the SDK re-wraps the Simulate handler's client/29 as sdk/6, so the
// error is neither the sentinel nor permanent (PR #536 review).
var sdkSimulationRejection = fmt.Errorf("[SendCosmosTx] simulation failed; refusing to split atomic batch: %w",
	&services.CosmosTxFailure{
		Stage: services.CosmosStageSimulation, Code: 6, Codespace: "sdk",
		Log: "rpc error: code = Unknown desc = 08-wasm-0 is Frozen: client state is not active: unknown request",
	})

// foldingStatusDest is a folding Cosmos destination that can report its
// client's status, like chain/cosmos and chain/l2rollup.
type foldingStatusDest struct {
	foldingMockDest
	active      bool
	status      string
	statusCalls int
}

func (d *foldingStatusDest) ClientActive(context.Context) (bool, string, error) {
	d.statusCalls++
	return d.active, d.status, nil
}

func TestClientHoldOnSimulationRejection(t *testing.T) {
	t.Run("folded batch on a frozen client is held", func(t *testing.T) {
		dst := &foldingStatusDest{foldingMockDest: foldingMockDest{enabled: true, foldErr: sdkSimulationRejection}, status: "Frozen"}
		b := &mockBuilder{}
		m := NewModule("eth->cosmos", "08-wasm-0", &mockSource{latest: 100}, dst, b)

		if rq := m.handleBatch(context.Background(), threePackets()); len(rq) != 3 {
			t.Fatalf("want all 3 kept, got %v", rq)
		}
		if m.clientHold.Load() == nil {
			t.Fatal("a Simulate rejection on a frozen client must arm the hold")
		}
		// Held: the next flush builds no update and submits nothing.
		built := b.built
		m.handleBatch(context.Background(), threePackets())
		if dst.foldCalls != 1 || b.built != built {
			t.Fatalf("held batch re-proved or re-submitted: foldCalls=%d built=%d->%d", dst.foldCalls, built, b.built)
		}
	})

	t.Run("unfolded batch on a frozen client is held", func(t *testing.T) {
		dst := &statusDest{mockDest: mockDest{relayErr: sdkSimulationRejection}, status: "Frozen"}
		m := NewModule("eth->cosmos", "08-wasm-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		if rq := m.handleBatch(context.Background(), threePackets()); len(rq) != 3 || m.clientHold.Load() == nil {
			t.Fatalf("want all 3 kept and the path held, got rq=%v held=%v", rq, m.clientHold.Load() != nil)
		}
		m.handleBatch(context.Background(), threePackets())
		if dst.relayCalls != 1 {
			t.Fatalf("held batch re-submitted: %d RelayPackets calls", dst.relayCalls)
		}
	})

	// A Simulate rejection on an ACTIVE client is a real message failure: it keeps
	// the transient re-queue it had, after one status query.
	t.Run("active client keeps the transient re-queue", func(t *testing.T) {
		dst := &foldingStatusDest{foldingMockDest: foldingMockDest{enabled: true, foldErr: sdkSimulationRejection}, active: true, status: "Active"}
		m := NewModule("eth->cosmos", "08-wasm-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		if rq := m.handleBatch(context.Background(), threePackets()); len(rq) != 3 {
			t.Fatalf("want all 3 re-queued, got %v", rq)
		}
		if m.clientHold.Load() != nil || dst.statusCalls != 1 {
			t.Fatalf("want no hold after exactly one status query, got held=%v statusCalls=%d",
				m.clientHold.Load() != nil, dst.statusCalls)
		}
	})

	// A transport failure carries no ABCI metadata and costs no status query.
	t.Run("transport failure does not query the status", func(t *testing.T) {
		dst := &foldingStatusDest{foldingMockDest: foldingMockDest{enabled: true, foldErr: errors.New("simulate ABCI query failed: connection refused")}, status: "Frozen"}
		m := NewModule("eth->cosmos", "08-wasm-0", &mockSource{latest: 100}, dst, &mockBuilder{})

		m.handleBatch(context.Background(), threePackets())
		if dst.statusCalls != 0 || m.clientHold.Load() != nil {
			t.Fatalf("a transport failure must not query or hold: statusCalls=%d", dst.statusCalls)
		}
	})
}

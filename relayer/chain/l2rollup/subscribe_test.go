package l2rollup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/services"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

func TestKeepIndexed(t *testing.T) {
	events := []chain.Event{
		{Height: 1}, {Height: 2}, {Height: 3},
	}
	cases := []struct {
		name    string
		indices []int
		want    []uint64
	}{
		{"none re-queued", nil, nil},
		{"middle re-queued", []int{1}, []uint64{2}},
		{"all re-queued preserves order", []int{0, 1, 2}, []uint64{1, 2, 3}},
		{"out-of-range ignored", []int{2, 9, -1}, []uint64{3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := keepIndexed(events, tc.indices)
			if len(got) != len(tc.want) {
				t.Fatalf("kept %d events, want %d", len(got), len(tc.want))
			}
			for i, h := range tc.want {
				if got[i].Height != h {
					t.Fatalf("kept[%d].Height = %d, want %d", i, got[i].Height, h)
				}
			}
		})
	}
}

func TestL2AckToEvent(t *testing.T) {
	// TestL2AckToEvent/empty ack skipped guards the no-ack skip: a WriteAcknowledgement
	// with no acknowledgement bytes cannot become a MsgAcknowledgement, so it must be
	// dropped rather than relayed.
	t.Run("empty ack skipped", func(t *testing.T) {
		ev := &contractICS26Router.ContractICS26RouterWriteAcknowledgement{
			Acknowledgements: nil,
		}
		if _, ok := l2AckToEvent(ev, "08-wasm-3"); ok {
			t.Fatal("empty acknowledgement must be skipped")
		}
	})

	t.Run("filters stale cosmos client", func(t *testing.T) {
		ev := &contractICS26Router.ContractICS26RouterWriteAcknowledgement{
			Sequence:         big.NewInt(1),
			Packet:           contractICS26Router.IICS26RouterMsgsPacket{SourceClient: "08-wasm-1", DestClient: "arb-client-0"},
			Acknowledgements: [][]byte{[]byte(`{"result":"AQ=="}`)},
		}
		if _, ok := l2AckToEvent(ev, "08-wasm-3"); ok {
			t.Fatal("ack for stale Cosmos wasm client must be skipped")
		}
	})

	t.Run("allows configured cosmos client", func(t *testing.T) {
		ev := &contractICS26Router.ContractICS26RouterWriteAcknowledgement{
			Sequence: big.NewInt(2),
			Packet: contractICS26Router.IICS26RouterMsgsPacket{
				SourceClient: "08-wasm-3",
				DestClient:   "arb-client-0",
				Payloads: []contractICS26Router.IICS26RouterMsgsPayload{{
					SourcePort: "transfer",
					DestPort:   "transfer",
					Version:    "ics20-1",
					Encoding:   "application/x-solidity-abi",
					Value:      []byte("packet"),
				}},
			},
			Acknowledgements: [][]byte{[]byte(`{"result":"AQ=="}`)},
		}
		got, ok := l2AckToEvent(ev, "08-wasm-3")
		if !ok {
			t.Fatal("ack for configured Cosmos wasm client must be emitted")
		}
		if got.Type != chain.AckPacket || got.Height != 0 || len(got.AckBytes) != 1 {
			t.Fatalf("unexpected event: %+v", got)
		}
	})
}

func TestSubscribe(t *testing.T) {
	// #308: the L2 scan cursor used to live only in Subscribe's stack frame, so a
	// restart resumed at head - L2_STARTUP_LOOKBACK_BLOCKS and never looked below
	// it. An acknowledgement written before that window stranded its packet, and
	// its escrow, with nothing in the log to say why.
	//
	// The distinguishing case is a restart AFTER the chain has moved past the
	// lookback window -- exactly the outage the cursor is for. A restart inside the
	// window is indistinguishable with or without persistence, so a test using one
	// would pass either way.
	t.Run("persists its scan cursor across restarts", func(t *testing.T) {
		restore := l2SubscribeInterval
		l2SubscribeInterval = 20 * time.Millisecond
		t.Cleanup(func() { l2SubscribeInterval = restore })

		router := ethcommon.HexToAddress("0x2222222222222222222222222222222222222222")
		store, err := services.LoadRecoveryState(filepath.Join(t.TempDir(), "cursors.json"))
		if err != nil {
			t.Fatalf("load store: %v", err)
		}

		// First run: a quiet chain at block 100. The scan consumes [90,100] and the
		// cursor lands at 101.
		first := &idleL2Node{router: router, head: 100, logBlock: 100}
		persisted := runUntilCursor(t, &Source{
			eth: first.start(t), headKind: Unsafe, router: router,
			l2ClientID: idleTestClientID, cosmosWasmClientID: idleTestDestID,
			recovery: store,
		}, store)

		// Second run, after a long outage: the head is now 1000, so the lookback
		// alone would start at 990 and blocks 101..989 would never be scanned.
		second := &idleL2Node{router: router, head: 1000, logBlock: 1000}
		runUntilScanned(t, &Source{
			eth: second.start(t), headKind: Unsafe, router: router,
			l2ClientID: idleTestClientID, cosmosWasmClientID: idleTestDestID,
			recovery: store,
		}, second)

		ranges := second.scannedRanges()
		if len(ranges) == 0 {
			t.Fatal("the restarted source never scanned")
		}
		if got := ranges[0][0]; got != persisted {
			t.Fatalf("the restart scanned from block %d, want the persisted cursor %d "+
				"(the lookback alone would have started at 990 and skipped everything below it)", got, persisted)
		}
	})

	// The control, and the shape of the bug: without a store the same restart starts
	// at the lookback and the blocks below it are never read. Without this, the test
	// above would also pass against a source that always rescans from zero.
	t.Run("without a store resumes only from the lookback", func(t *testing.T) {
		restore := l2SubscribeInterval
		l2SubscribeInterval = 20 * time.Millisecond
		t.Cleanup(func() { l2SubscribeInterval = restore })
		t.Setenv(l2StartupLookbackEnv, "10")

		router := ethcommon.HexToAddress("0x3333333333333333333333333333333333333333")
		node := &idleL2Node{router: router, head: 1000, logBlock: 1000}
		runUntilScanned(t, &Source{
			eth: node.start(t), headKind: Unsafe, router: router,
			l2ClientID: idleTestClientID, cosmosWasmClientID: idleTestDestID,
		}, node)

		ranges := node.scannedRanges()
		if len(ranges) == 0 {
			t.Fatal("never scanned")
		}
		// Against the window the source itself computes, not a literal. The number
		// used to be head-minus-10, set through L2_STARTUP_LOOKBACK; that knob was
		// retired for a measured window, so a literal here would pin the old knob
		// rather than the property -- which is that WITHOUT a store the scan starts at
		// the window and nowhere else.
		src := &Source{eth: node.start(t), headKind: Unsafe, router: router}
		lookback := src.startupLookback(context.Background(), 1000)
		want := uint64(0)
		if 1000 > lookback {
			want = 1000 - lookback
		}
		if got := ranges[0][0]; got != want {
			t.Fatalf("scanned from %d, want the window start %d (head 1000 - lookback %d)", got, want, lookback)
		}
	})

	// The cursor must never advance past an event the handler re-queued. Such an
	// event exists only in memory; if the cursor moved beyond its block and the
	// process died, the restart would scan from above it and the packet would never
	// be seen again. This is why the store overlays rather than takes a maximum --
	// the cursor is allowed to move backwards.
	t.Run("cursor never passes a requeued event", func(t *testing.T) {
		restore := l2SubscribeInterval
		l2SubscribeInterval = 20 * time.Millisecond
		t.Cleanup(func() { l2SubscribeInterval = restore })
		t.Setenv(l2StartupLookbackEnv, "10")

		router := ethcommon.HexToAddress("0x4444444444444444444444444444444444444444")
		store, err := services.LoadRecoveryState(filepath.Join(t.TempDir(), "cursors.json"))
		if err != nil {
			t.Fatalf("load store: %v", err)
		}
		// The log sits at 95, inside the scanned range, and the handler never accepts it.
		node := &idleL2Node{router: router, head: 100, logBlock: 95}
		src := &Source{
			eth: node.start(t), headKind: Unsafe, router: router,
			l2ClientID: idleTestClientID, cosmosWasmClientID: idleTestDestID,
			recovery: store,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- src.Subscribe(ctx, func(_ context.Context, batch []chain.Event) []int {
				requeue := make([]int, len(batch))
				for i := range batch {
					requeue[i] = i
				}
				return requeue
			})
		}()

		// Same reason as runUntilCursor: wait for the cursor, not for the scan that
		// precedes it in the same iteration.
		var cursors services.RecoveryCursors
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if got, ok := store.Get(); ok && got[cursorL2] != 0 {
				cursors = got
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done
		if cursors == nil {
			t.Fatal("nothing was persisted")
		}
		if got := cursors[cursorL2]; got > 95 {
			t.Fatalf("cursor = %d, want no higher than the re-queued event's block 95; "+
				"a restart would scan past a packet that only exists in memory", got)
		}
	})

	// TestSubscribe/retries pending while L2 is idle is the regression test for a stall that
	// silently stranded packets — in practice, acknowledgements.
	//
	// After a successful scan the cursor moves to head+1, so on a chain that has stopped
	// producing blocks every later tick sees head < from. The loop used to `continue` on
	// that condition, skipping the handler call entirely — and the handler call is the
	// only thing that re-offers `pending`, the events the handler asked to retry because
	// they were not yet relayable.
	//
	// The acknowledgement is the worst case: it is normally the last event a packet
	// produces on the L2, so nothing further is written, no new block ever arrives, and
	// the re-queued ack is never retried. The packet stays committed on Cosmos and its
	// escrow stays locked, with a single "waiting" line in the log and then silence.
	// Observed on a Cosmos↔Arbitrum devnet: the ack sat for 15 minutes while the attestor
	// frontier passed its height, and only a relayer restart (which resets the cursor)
	// delivered it.
	//
	// The handler here always re-queues, mimicking a packet whose attested height has not
	// been reached. The test asserts the handler keeps being called even though the head
	// never moves.
	t.Run("retries pending while L2 is idle", func(t *testing.T) {
		restore := l2SubscribeInterval
		l2SubscribeInterval = 20 * time.Millisecond
		t.Cleanup(func() { l2SubscribeInterval = restore })

		router := ethcommon.HexToAddress("0x1111111111111111111111111111111111111111")
		node := &idleL2Node{router: router}
		src := &Source{
			eth:                node.start(t),
			headKind:           Unsafe,
			l2ClientID:         idleTestClientID,
			cosmosWasmClientID: idleTestDestID,
			router:             router,
		}

		var (
			mu       sync.Mutex
			calls    int
			lastSize int
		)
		handler := func(_ context.Context, batch []chain.Event) []int {
			mu.Lock()
			calls++
			lastSize = len(batch)
			mu.Unlock()
			requeue := make([]int, len(batch)) // never relayable in this test
			for i := range batch {
				requeue[i] = i
			}
			return requeue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- src.Subscribe(ctx, handler) }()

		// Give the loop many poll periods. The head never advances after the first scan,
		// so a loop that gates the handler on new blocks calls it exactly once.
		time.Sleep(600 * time.Millisecond)
		cancel()
		<-done

		mu.Lock()
		gotCalls, gotSize := calls, lastSize
		mu.Unlock()

		if gotCalls < 2 {
			t.Fatalf("handler called %d time(s) on an idle L2: the re-queued packet is never retried, "+
				"so an acknowledgement waiting on finality is stranded until the relayer restarts", gotCalls)
		}
		if gotSize != 1 {
			t.Fatalf("last batch carried %d event(s), want the 1 re-queued packet", gotSize)
		}
		logs, heads := node.counts()
		if heads < 2 {
			t.Fatalf("head polled %d time(s); the loop should keep polling", heads)
		}
		if logs == 0 {
			t.Fatal("expected the first tick to scan for logs")
		}
	})

	// TestSubscribe/survives unsafe head reorg covers the other way `head < from` happens:
	// with head_kind=unsafe the head can move BACKWARDS when the sequencer reorgs, not
	// merely stop advancing. The two share a branch, but the recovery behaviour is
	// distinct and worth pinning:
	//
	//   - while the head is behind the cursor, no scan is issued at all (an eth_getLogs
	//     with fromBlock > toBlock is a malformed request, not an empty result), yet the
	//     pending buffer must still be re-offered;
	//   - when the head climbs back, scanning must resume at exactly the cursor the loop
	//     already reached — not rewound (that is the documented forward-only limitation)
	//     and not skipped past (which would silently drop the blocks in between).
	t.Run("survives unsafe head reorg", func(t *testing.T) {
		restore := l2SubscribeInterval
		l2SubscribeInterval = 20 * time.Millisecond
		t.Cleanup(func() { l2SubscribeInterval = restore })

		// Above the default startup lookback so the initial cursor is a real number (head-256),
		// which is what makes "did the cursor move?" observable at all.
		const (
			startHead = uint64(1000)
			reorgHead = uint64(900) // sequencer drops 100 blocks
			afterHead = uint64(1010)
		)
		wantCursor := startHead + 1 // where the loop must resume after the reorg

		router := ethcommon.HexToAddress("0x2222222222222222222222222222222222222222")
		node := &idleL2Node{router: router, logBlock: startHead}
		node.setHead(startHead)
		src := &Source{
			eth:                node.start(t),
			headKind:           Unsafe,
			l2ClientID:         idleTestClientID,
			cosmosWasmClientID: idleTestDestID,
			router:             router,
		}

		var (
			mu            sync.Mutex
			calls         int
			callsAtReorg  int
			lastBatchSize int
		)
		handler := func(_ context.Context, batch []chain.Event) []int {
			mu.Lock()
			calls++
			lastBatchSize = len(batch)
			mu.Unlock()
			requeue := make([]int, len(batch)) // never relayable in this test
			for i := range batch {
				requeue[i] = i
			}
			return requeue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- src.Subscribe(ctx, handler) }()

		// Phase 1: the head is ahead, the first scan picks the packet up.
		time.Sleep(200 * time.Millisecond)

		// Phase 2: reorg — the head falls behind the cursor.
		mu.Lock()
		callsAtReorg = calls
		mu.Unlock()
		node.setHead(reorgHead)
		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		callsDuringReorg := calls
		mu.Unlock()
		if callsDuringReorg <= callsAtReorg {
			t.Fatalf("handler stopped being called during the reorg (%d -> %d): the pending packet "+
				"is stranded exactly as it was on an idle chain", callsAtReorg, callsDuringReorg)
		}

		// Phase 3: the head climbs back past the cursor.
		node.setHead(afterHead)
		time.Sleep(300 * time.Millisecond)
		cancel()
		<-done

		mu.Lock()
		gotSize := lastBatchSize
		mu.Unlock()
		if gotSize != 1 {
			t.Fatalf("last batch carried %d event(s), want the 1 re-queued packet", gotSize)
		}

		ranges := node.scannedRanges()
		if len(ranges) < 2 {
			t.Fatalf("scanned ranges %v: expected a scan before the reorg and another after recovery", ranges)
		}
		// No scan may be issued while the head is behind the cursor.
		for _, r := range ranges {
			if r[0] > r[1] {
				t.Errorf("scanned an inverted range [%d,%d] — the head was behind the cursor and no "+
					"scan should have been issued", r[0], r[1])
			}
		}
		// The recovery scan resumes at the cursor the loop had already reached.
		last := ranges[len(ranges)-1]
		if last[0] != wantCursor {
			t.Errorf("recovery scan started at block %d, want %d: the cursor %s",
				last[0], wantCursor,
				map[bool]string{true: "rewound over already-scanned blocks", false: "skipped blocks"}[last[0] < wantCursor])
		}
		if last[1] != afterHead {
			t.Errorf("recovery scan ended at block %d, want the new head %d", last[1], afterHead)
		}
	})

	// A transient RPC error on the FIRST head read used to be fatal while the identical
	// error one tick later was logged and retried: the read sat before the loop and its
	// error propagated out of Subscribe, up through the relay module, into logger.Fatal.
	// A rate-limit blip during a restart therefore stopped the process from coming up at
	// all, instead of it starting and recovering on the next tick.
	//
	// The stub rejects the first two head reads the way a public endpoint rate-limits,
	// then serves normally. Subscribe must survive that and still deliver the packet.
	t.Run("survives transient head error at startup", func(t *testing.T) {
		restore := l2SubscribeInterval
		l2SubscribeInterval = 20 * time.Millisecond
		t.Cleanup(func() { l2SubscribeInterval = restore })

		// A head well above the lookback, so the seeded cursor is a distinctive number
		// rather than the 0 that "head below the window" and "never seeded" share.
		const startHead = uint64(1000)

		router := ethcommon.HexToAddress("0x1111111111111111111111111111111111111111")
		node := &idleL2Node{router: router, failHeads: 2, logBlock: startHead}
		node.setHead(startHead)
		src := &Source{
			eth:                node.start(t),
			headKind:           Unsafe,
			l2ClientID:         idleTestClientID,
			cosmosWasmClientID: idleTestDestID,
			router:             router,
		}

		delivered := make(chan int, 1)
		handler := func(_ context.Context, batch []chain.Event) []int {
			select {
			case delivered <- len(batch):
			default:
			}
			return nil // relayed
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		errCh := make(chan error, 1)
		go func() { errCh <- src.Subscribe(ctx, handler) }()

		select {
		case n := <-delivered:
			if n == 0 {
				t.Fatal("handler ran with an empty batch")
			}
		case err := <-errCh:
			t.Fatalf("Subscribe returned before delivering anything: %v", err)
		case <-ctx.Done():
			t.Fatal("packet never delivered after the startup head errors cleared")
		}

		// The cursor must be seeded from the head that finally answered, not from zero:
		// a scan starting at block 0 would hammer the endpoint that just rate-limited us.
		cancel()
		<-errCh
		ranges := node.scannedRanges()
		if len(ranges) == 0 {
			t.Fatal("no eth_getLogs range recorded")
		}
		wantLookback := lookbackBlocks(startupWindow(0), idleTestBlockTime)
		if wantFrom := startHead - wantLookback; ranges[0][0] != wantFrom {
			t.Fatalf("first scan started at %d, want %d (head - %d block lookback)", ranges[0][0], wantFrom, wantLookback)
		}
	})

	// The filter above is only worth anything if the scan loop actually calls it.
	// This drives the real Subscribe against a node serving one historical
	// SendPacket log and asserts the handler never sees it.
	t.Run("skips a packet another relayer already delivered", func(t *testing.T) {
		handled, state := runSubscribeWithCosmosState(t,
			ethcommon.HexToAddress("0x5555555555555555555555555555555555555555"),
			&fakeCosmosState{receipt: true})

		if handled != 0 {
			t.Fatalf("the handler was offered %d events; a packet Cosmos already has a receipt for "+
				"must never reach the relay path", handled)
		}
		if state.askedReceipt == 0 {
			t.Fatal("the scan never reconciled against Cosmos at all")
		}
	})

	// The control. Without it, a filter that dropped every event would pass the test
	// above -- so this pins that the same setup DOES relay when the work is real.
	t.Run("still relays a packet nobody has delivered", func(t *testing.T) {
		handled, _ := runSubscribeWithCosmosState(t,
			ethcommon.HexToAddress("0x6666666666666666666666666666666666666666"),
			&fakeCosmosState{receipt: false})

		if handled == 0 {
			t.Fatal("the handler was never offered the packet; reconciliation must only drop settled work")
		}
	})

	// The other half of B3, and the one the startup rescan cannot cover: an event the
	// handler keeps re-queueing because the destination is behind. It sits in the
	// pending buffer for as long as that lasts, and it is exactly the window in which
	// a second relayer can deliver the packet. Nothing else in the loop ever asks
	// again whether the work is still outstanding.
	t.Run("drops a pending packet another relayer settles", func(t *testing.T) {
		restoreInterval := l2SubscribeInterval
		l2SubscribeInterval = 20 * time.Millisecond
		restoreReconcile := l2ReconcileInterval
		l2ReconcileInterval = time.Millisecond // recheck on every tick
		t.Cleanup(func() {
			l2SubscribeInterval = restoreInterval
			l2ReconcileInterval = restoreReconcile
		})
		t.Setenv(l2StartupLookbackEnv, "10")

		router := ethcommon.HexToAddress("0x7777777777777777777777777777777777777777")
		node := &idleL2Node{router: router, head: 100, logBlock: 95}
		state := &settlingState{}
		src := &Source{
			eth: node.start(t), headKind: Unsafe, router: router,
			l2ClientID: idleTestClientID, cosmosWasmClientID: idleTestDestID,
			cosmosState: state,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		offered := make(chan struct{}, 128)
		done := make(chan error, 1)
		go func() {
			done <- src.Subscribe(ctx, func(_ context.Context, batch []chain.Event) []int {
				select {
				case offered <- struct{}{}:
				default:
				}
				requeue := make([]int, len(batch)) // never relayable: always re-queued
				for i := range batch {
					requeue[i] = i
				}
				return requeue
			})
		}()

		// The packet is offered repeatedly while nobody has delivered it.
		for i := 0; i < 2; i++ {
			select {
			case <-offered:
			case <-time.After(3 * time.Second):
				cancel()
				<-done
				t.Fatal("the pending packet was never re-offered to the handler")
			}
		}

		// Another relayer delivers it. From here the re-check must drop it, so the
		// batch goes empty and the handler is never called again.
		state.settle()
		drainDeadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(drainDeadline) { // let in-flight offers through
			select {
			case <-offered:
			case <-time.After(20 * time.Millisecond):
			}
		}

		select {
		case <-offered:
			cancel()
			<-done
			t.Fatal("the handler was offered a packet Cosmos already has a receipt for; " +
				"the pending buffer is never re-checked against the destination")
		case <-time.After(300 * time.Millisecond):
		}
		cancel()
		<-done
	})
}

// runUntilScanned drives one Subscribe run until the node has served at least
// one eth_getLogs, then stops it. Bounded: a hung run fails the test rather than
// hanging it.
func runUntilScanned(t *testing.T, src *Source, node *idleL2Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- src.Subscribe(ctx, func(context.Context, []chain.Event) []int { return nil })
	}()
	waitForScan(t, node)
	cancel()
	<-done
}

// runUntilCursor waits for the postcondition that actually matters -- a cursor
// in the store -- rather than for the scan that precedes it. Waiting on the scan
// races the persist that follows it in the same iteration, and the race is
// invisible: the test simply reports "nothing was persisted".
func runUntilCursor(t *testing.T, src *Source, store *services.RecoveryStateStore) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- src.Subscribe(ctx, func(context.Context, []chain.Event) []int { return nil })
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cursors, ok := store.Get(); ok && cursors[cursorL2] != 0 {
			cancel()
			<-done
			return cursors[cursorL2]
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("no cursor was persisted after a successful scan")
	return 0
}

func waitForScan(t *testing.T, node *idleL2Node) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(node.scannedRanges()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the source never issued an eth_getLogs")
}

// TestScanTerminalLogs_SettlesWithoutRelaying pins the rule that makes terminal
// events safe to read: they close a packet's lifecycle, so they must settle the
// tracker and produce NO relay work. Emitting one as a chain.Event would relay a
// packet with nothing left to do.
func TestScanTerminalLogs_SettlesWithoutRelaying(t *testing.T) {
	t.Parallel()

	t.Run("a source with no hook ignores them", func(t *testing.T) {
		t.Parallel()
		// The behaviour every L2 source had before terminal events were read: no
		// tracker to settle, so nothing to do and nothing to fail on.
		s := &Source{}
		settled, err := s.scanTerminalLogs(nil, nil, nil)
		if err != nil {
			t.Fatalf("a nil settle hook must be a no-op, got %v", err)
		}
		if settled != nil {
			t.Fatalf("a source with no tracker settles nothing, got %v", settled)
		}
	})

	t.Run("the hook receives the marshaled packet", func(t *testing.T) {
		t.Parallel()
		var got [][]byte
		s := (&Source{}).WithSettleHook(func(raw []byte) { got = append(got, raw) })
		if s.settle == nil {
			t.Fatal("WithSettleHook did not install the hook")
		}
		s.settle([]byte("packet"))
		if len(got) != 1 || string(got[0]) != "packet" {
			t.Fatalf("hook got %v, want one marshaled packet", got)
		}
	})
}

// A scan window that contains BOTH a send and its later terminal event is the
// ordinary case after a restart -- the window is sized to cover the downtime.
// Settling only the tracker leaves the send in the batch, so handleBatch relays
// it and TRACKS IT AGAIN, after settle removed it and after the cursor moved
// past the terminal log that would have cleared it. Nothing settles it a second
// time.
func TestDropSettled(t *testing.T) {
	t.Parallel()

	// Raw is the identity: both the send event and the terminal log reach it
	// through the same EthPacketToCosmosPacket + proto.Marshal.
	event := func(seq uint64, client, raw string) chain.Event {
		return chain.Event{Type: chain.SendPacket, Sequence: seq, ClientID: client, Height: 100, Raw: []byte(raw)}
	}
	settledSet := func(raws ...string) map[settledKey]struct{} {
		set := map[settledKey]struct{}{}
		for _, raw := range raws {
			set[settledKey(raw)] = struct{}{}
		}
		return set
	}

	send := event(5, "08-wasm-7", "packet-5")
	other := event(6, "08-wasm-7", "packet-6")
	elsewhere := event(5, "08-wasm-9", "packet-5-other-client")

	t.Run("nothing settled leaves the batch alone", func(t *testing.T) {
		t.Parallel()
		batch := []chain.Event{send, other}
		if got := dropSettled(batch, nil); len(got) != 2 {
			t.Fatalf("kept %d of 2 with an empty settled set", len(got))
		}
	})

	t.Run("a settled packet is removed", func(t *testing.T) {
		t.Parallel()
		got := dropSettled([]chain.Event{send, other}, settledSet("packet-5"))
		if len(got) != 1 || got[0].Sequence != 6 {
			t.Fatalf("kept %+v, want only seq=6", got)
		}
	})

	// A bare sequence would drop an unrelated packet that happens to share a
	// number on another client.
	t.Run("the same sequence on another client survives", func(t *testing.T) {
		t.Parallel()
		got := dropSettled([]chain.Event{send, elsewhere}, settledSet("packet-5"))
		if len(got) != 1 || got[0].ClientID != "08-wasm-9" {
			t.Fatalf("kept %+v, want only the other client's packet", got)
		}
	})

	// And the case client+sequence cannot express at all: a migration keeps the
	// client id and restarts sequences from 1, so seq=5 names two different
	// packets on the SAME client. A stale terminal event for the old one must
	// not drop the new one -- it would never be relayed and nothing would notice.
	t.Run("a replayed sequence on the same client survives", func(t *testing.T) {
		t.Parallel()
		replayed := event(5, "08-wasm-7", "packet-5-after-migration")
		got := dropSettled([]chain.Event{replayed}, settledSet("packet-5"))
		if len(got) != 1 {
			t.Fatalf("kept %+v; the pre-migration seq=5 settled, not this one", got)
		}
	})

	// Both halves of the batch are filtered, not just the fresh ones: a send
	// already in the retry queue can be settled by another relayer.
	t.Run("a requeued packet is removed too", func(t *testing.T) {
		t.Parallel()
		pending := []chain.Event{send}
		fresh := []chain.Event{other}
		got := dropSettled(append(pending[:len(pending):len(pending)], fresh...), settledSet("packet-5"))
		if len(got) != 1 || got[0].Sequence != 6 {
			t.Fatalf("kept %+v; a settled packet in the RETRY queue must go too", got)
		}
	})
}

// TestSettledKey_IdentitySurvivesASequenceReplay drives BOTH ends through production
// code: settleTerminal builds the settled key, l2SendToEvent builds the batch
// event. That is the point -- a test that constructs the key itself can only
// document the choice, not hold it, because changing the key type changes the
// test with it.
//
// A client migration keeps the client id and restarts sequences from 1, so seq=N
// names two different packets on one client. The terminal event for the old one
// must not drop the new one from the batch: it would never be relayed, and the
// cursor has already moved past the log that would have settled it.
func TestSettledKey_IdentitySurvivesASequenceReplay(t *testing.T) {
	t.Parallel()

	const client = "arb-client-0"
	source := &Source{settle: func([]byte) {}}

	old := IICS26RouterMsgsPacketFor("08-wasm-1", client)
	old.TimeoutTimestamp = 1000
	old.Payloads[0].Value = []byte("pre-migration")

	replayed := IICS26RouterMsgsPacketFor("08-wasm-1", client)
	replayed.TimeoutTimestamp = 2000
	replayed.Payloads[0].Value = []byte("post-migration")

	// Both carry sequence 1 -- the migration restarted the numbering.
	settled := map[settledKey]struct{}{}
	source.settleTerminal("AckPacket", true, old, big.NewInt(1), settled)

	event, ok := l2SendToEvent(&contractICS26Router.ContractICS26RouterSendPacket{
		Sequence: big.NewInt(1), Packet: replayed,
	}, client)
	if !ok {
		t.Fatal("the replayed send must decode; the fixture is wrong otherwise")
	}

	if got := dropSettled([]chain.Event{event}, settled); len(got) != 1 {
		t.Fatal("the pre-migration seq=1 settled, not this one; dropping it loses a packet that was never relayed")
	}

	// And the packet the terminal event really names is still dropped.
	sameEvent, ok := l2SendToEvent(&contractICS26Router.ContractICS26RouterSendPacket{
		Sequence: big.NewInt(1), Packet: old,
	}, client)
	if !ok {
		t.Fatal("the original send must decode")
	}
	if got := dropSettled([]chain.Event{sameEvent}, settled); len(got) != 0 {
		t.Fatalf("kept %+v; the settled packet's own identity must still match", got)
	}
}

func TestLookbackBlocks(t *testing.T) {
	t.Parallel()

	window := 512 * time.Second
	for _, tc := range []struct {
		chain     string
		blockTime time.Duration
		want      uint64
	}{
		{chain: "OP Stack", blockTime: 2 * time.Second, want: 256},
		{chain: "Base", blockTime: 2 * time.Second, want: 256},
		{chain: "Arbitrum Nitro", blockTime: 250 * time.Millisecond, want: 2048},
		{chain: "Ethereum L1", blockTime: 12 * time.Second, want: 43},
	} {
		t.Run(tc.chain, func(t *testing.T) {
			t.Parallel()
			if got := lookbackBlocks(window, tc.blockTime); got != tc.want {
				t.Fatalf("lookbackBlocks(%s, %s) = %d, want %d", window, tc.blockTime, got, tc.want)
			}
		})
	}

	t.Run("Arbitrum and OP differ for the same window", func(t *testing.T) {
		t.Parallel()
		op := lookbackBlocks(window, 2*time.Second)
		arb := lookbackBlocks(window, 250*time.Millisecond)
		if op == arb {
			t.Fatalf("both chains got %d blocks; a block count that ignores block time is the bug B2 removes", op)
		}
		if arb <= op {
			t.Fatalf("Arbitrum lookback %d <= OP lookback %d; the faster chain needs MORE blocks for the same time", arb, op)
		}
	})

	t.Run("rounds up so the window is never short", func(t *testing.T) {
		t.Parallel()
		// 10s at 3s per block is 3.33 blocks; 3 would cover only 9 seconds.
		if got := lookbackBlocks(10*time.Second, 3*time.Second); got != 4 {
			t.Fatalf("lookbackBlocks(10s, 3s) = %d, want 4 (rounded up)", got)
		}
	})

	t.Run("an unusable block time yields no lookback rather than dividing by zero", func(t *testing.T) {
		t.Parallel()
		for _, blockTime := range []time.Duration{0, -time.Second} {
			if got := lookbackBlocks(window, blockTime); got != 0 {
				t.Fatalf("lookbackBlocks(%s, %s) = %d, want 0", window, blockTime, got)
			}
		}
	})
}

// TestStartupWindow pins the window as a SAFETY property. It is the entire
// crash-recovery window for the L2 path -- the cursor is not persisted, so a
// packet emitted before the window is never rescanned and its escrow stays
// locked. Shrinking it must not reach main silently.

func TestStartupWindow(t *testing.T) {
	t.Parallel()

	t.Run("the default reproduces the window this code already had", func(t *testing.T) {
		t.Parallel()
		// 256 blocks at OP's 2s block time, now stated in time so every chain gets
		// the same amount of it. If this changes, re-derive it and say why.
		if got := startupWindow(0); got != 512*time.Second {
			t.Fatalf("startupWindow(0) = %s, want 8m32s", got)
		}
	})

	// The window is a downtime allowance, and head_kind must not widen it. The
	// scan is anchored at head(head_kind) -- Source.head reads SafeBlockNumber or
	// FinalizedBlockNumber -- so both ends of "head minus window" sit in the same
	// delayed view and the finality lag is already absorbed. Adding it again would
	// make every finalized run pay for a case that is not a crash.
	t.Run("head kind does not widen the window", func(t *testing.T) {
		t.Parallel()
		if got := startupWindow(0); got != defaultStartupWindow {
			t.Fatalf("startupWindow(0) = %s, want %s", got, defaultStartupWindow)
		}
	})

	t.Run("an operator override replaces the baseline", func(t *testing.T) {
		t.Parallel()
		if got := startupWindow(2 * time.Hour); got != 2*time.Hour {
			t.Fatalf("startupWindow(2h) = %s, want 2h", got)
		}
	})
}

// TestStartupWindowFromEnv covers the fallback, not the size of the window:
// an absent or unparseable override must leave the default in place rather than
// turning a typo into a chain that starts at the head with no recovery.

func TestStartupWindowFromEnv(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "empty uses the default", raw: "", want: 0},
		{name: "valid duration overrides", raw: "90m", want: 90 * time.Minute},
		{name: "seconds are a duration too", raw: "45s", want: 45 * time.Second},
		{name: "a bare block count is no longer valid", raw: "1200", want: 0},
		{name: "unparseable uses the default", raw: "not-a-duration", want: 0},
		{name: "negative uses the default", raw: "-5m", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := startupWindowFromEnv(tc.raw); got != tc.want {
				t.Fatalf("startupWindowFromEnv(%q) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

// The retired key changed units as well as name -- block count to duration --
// so an old value cannot be carried forward even in principle. Silently ignoring
// it leaves the operator on the default window and nothing says so, which is the
// event-loss shape the window exists to prevent.

func TestRejectRetiredLookbackEnv(t *testing.T) {
	t.Run("unset is fine", func(t *testing.T) {
		t.Setenv(l2StartupLookbackBlocksEnv, "")
		os.Unsetenv(l2StartupLookbackBlocksEnv)
		if err := RejectRetiredLookbackEnv(); err != nil {
			t.Fatalf("an unset key must not fail startup: %v", err)
		}
	})

	// Even an empty value is a deliberate export, and it still means the operator
	// believes that key does something.
	for _, raw := range []string{"256", "1024", ""} {
		t.Run("set to "+raw+" is refused", func(t *testing.T) {
			t.Setenv(l2StartupLookbackBlocksEnv, raw)
			err := RejectRetiredLookbackEnv()
			if err == nil {
				t.Fatal("a retired key that sizes nothing must fail startup, not be ignored")
			}
			// The message has to carry the operator from what they set to what
			// they should set; naming only the dead key leaves them guessing.
			for _, want := range []string{l2StartupLookbackBlocksEnv, l2StartupLookbackEnv, "DURATION"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// measureBlockTime runs on the subscribe goroutine before the cursor is seeded,
// so an endpoint that accepts the connection and never answers must not hold the
// L2 direction open indefinitely. A ws:// endpoint has no transport timeout at
// all, so this deadline is the only bound.

func TestMeasureBlockTime_IsBounded(t *testing.T) {
	// Order matters: defers run LIFO, and httptest.Server.Close waits for its
	// handlers. Registering Close first means the handler is released before
	// Close runs -- the other order deadlocks the test rather than failing it.
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // accept, then never answer
	}))
	defer server.Close()
	defer close(blocked)

	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatalf("dial stub: %v", err)
	}
	defer client.Close()

	source := &Source{eth: client}
	start := time.Now()
	// The subscribe lifetime context: nothing here cancels it, so only the
	// per-call deadline can end this.
	if _, err := source.measureBlockTime(context.Background(), 1_000); err == nil {
		t.Fatal("a hung endpoint must surface as an error, not a completed measurement")
	}
	if elapsed := time.Since(start); elapsed > blockTimeSampleTimeout+5*time.Second {
		t.Fatalf("measureBlockTime took %s; it must be bounded by %s", elapsed, blockTimeSampleTimeout)
	}
}

// Both L2 terminal endings drop the pending record; only the acknowledged one
// closes an owed-acknowledgement record.
//
// Reported by @DongLieu: WithSettleHook was wired to the pending tracker alone,
// so an acknowledgement another relayer submitted left this process's debt in
// place -- durable, surviving restart, reported overdue for a packet settled
// on-chain. A timeout must NOT clear a debt: the packet is refunded and nothing
// is owed, so clearing there would hide a real entry.
func TestSettleTerminal_ClosesTheOwedAckOnlyOnAnAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		kind         string
		acknowledged bool
		wantAck      int
	}{
		{"AckPacket", true, 1},
		{"TimeoutPacket", false, 0},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var settled, ackSettled int
			src := &Source{
				settle:    func([]byte) { settled++ },
				settleAck: func([]byte) { ackSettled++ },
			}
			src.settleTerminal(tc.kind, tc.acknowledged,
				contractICS26Router.IICS26RouterMsgsPacket{}, big.NewInt(1), map[settledKey]struct{}{})

			if settled != 1 {
				t.Fatalf("pending settlements = %d, want 1: every terminal ending drops the record", settled)
			}
			if ackSettled != tc.wantAck {
				t.Fatalf("owed-ack settlements = %d, want %d", ackSettled, tc.wantAck)
			}
		})
	}
}

func TestChunkSpans_LogScanSpansCoverTheRangeExactly(t *testing.T) {
	cases := []struct {
		name            string
		from, to, chunk uint64
		wantSpans       int
	}{
		{"unchunked when chunk is zero", 100, 5000, 0, 1},
		{"unchunked when the range fits", 100, 105, 10, 1},
		{"exact multiple", 0, 29, 10, 3},
		{"ragged tail", 0, 25, 10, 3},
		{"single block", 42, 42, 10, 1},
		{"alchemy free tier over a startup lookback", 0, 255, 10, 26},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkSpans(tc.from, tc.to, tc.chunk)
			if len(got) != tc.wantSpans {
				t.Fatalf("span count = %d, want %d: %v", len(got), tc.wantSpans, got)
			}
			if got[0][0] != tc.from {
				t.Fatalf("first span starts at %d, want %d", got[0][0], tc.from)
			}
			if last := got[len(got)-1]; last[1] != tc.to {
				t.Fatalf("last span ends at %d, want %d", last[1], tc.to)
			}
			for i, s := range got {
				if s[1] < s[0] {
					t.Fatalf("span %d is inverted: %v", i, s)
				}
				if tc.chunk > 0 && s[1]-s[0]+1 > tc.chunk {
					t.Fatalf("span %d spans %d blocks, over the %d cap: %v",
						i, s[1]-s[0]+1, tc.chunk, s)
				}
				if i > 0 && s[0] != got[i-1][1]+1 {
					t.Fatalf("gap or overlap between %v and %v", got[i-1], s)
				}
			}
		})
	}
}

// A chunk of 1 is degenerate but must still be correct rather than looping forever
// or skipping the last block.
func TestChunkSpans_LogScanSpansWithChunkOfOne(t *testing.T) {
	got := chunkSpans(10, 14, 1)
	if len(got) != 5 {
		t.Fatalf("span count = %d, want 5: %v", len(got), got)
	}
	for i, s := range got {
		if s[0] != s[1] || s[0] != uint64(10+i) {
			t.Fatalf("span %d = %v, want [%d,%d]", i, s, 10+i, 10+i)
		}
	}
}

func TestScanNarrowing(t *testing.T) {
	// The third NeedsChange branch: a provider that refuses the span.
	//
	// The scanner has no way to ask what the cap is — the comment on logScanChunk
	// records the shape of the answer, "a plain 400 that names neither the setting
	// nor a workable value" — so the only way to find a working span is to try a
	// smaller one. Retrying the same one is not a retry at all: it is the identical
	// refused request, forever, and on the L2 path that means every packet on the
	// chain stops being seen.
	t.Run("narrows when the provider refuses the span", func(t *testing.T) {
		const cap = uint64(10)
		span := &relayerclient.LogSpan{} // unconfigured: one call for the whole range
		var asked []uint64

		// A scan that never narrows does not fail, it spins: same width, same refusal,
		// forever. Bounding the calls turns that into a red test instead of a hung one
		// — an unbounded test here reports a broken narrowing as a wall-clock timeout,
		// which is indistinguishable from a slow machine.
		events, _, err := scanNarrowing(0, 99, span, func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			width := to - from + 1
			asked = append(asked, width)
			if len(asked) > 64 {
				t.Fatalf("the scan asked 64 times without settling; widths so far: %v", asked)
			}
			if width > cap {
				return nil, nil, errors.New("query returned more than 10000 results")
			}
			return []chain.Event{{Height: from}}, nil, nil
		})
		if err != nil {
			t.Fatalf("scan never found a span the provider would serve: %v", err)
		}
		if span.Chunk == 0 || span.Chunk > cap {
			t.Fatalf("span settled at %d, want a value at or below the provider cap %d", span.Chunk, cap)
		}
		// It must have asked for something SMALLER each time it was refused; asking
		// the same width twice is the loop this branch exists to prevent.
		for i := 1; i < len(asked); i++ {
			if asked[i] >= asked[i-1] && asked[i-1] > cap {
				t.Fatalf("width %d did not shrink after %d was refused: %v", asked[i], asked[i-1], asked)
			}
		}
		// And the whole range is still covered once a working span is found: one event
		// per piece, and the pieces tile [0,99] exactly (the last one is short unless
		// the span divides 100).
		wantPieces := uint64(len(chunkSpans(0, 99, span.Chunk)))
		if uint64(len(events)) != wantPieces {
			t.Fatalf("got %d events over [0,99] at span %d, want %d; the range was not covered exactly",
				len(events), span.Chunk, wantPieces)
		}
	})

	// The narrowing must SURVIVE the call, or every tick pays one rejected request to
	// rediscover the same cap and the log says nothing after the first line.
	t.Run("narrowed span is remembered", func(t *testing.T) {
		span := &relayerclient.LogSpan{Chunk: 64}
		// Every scan in this test is bounded. A scan that does not narrow repeats the
		// same refused width forever, and an unbounded one turns that into a hang —
		// which reads as a slow machine, not as the defect it is.
		attempts := 0
		refuse := func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			attempts++
			if attempts > 64 {
				return nil, nil, errors.New("BOUND: the scan repeated a width it had already been refused")
			}
			if to-from+1 > 8 {
				return nil, nil, errors.New("block range too large")
			}
			return nil, nil, nil
		}
		if _, _, err := scanNarrowing(0, 63, span, refuse); err != nil {
			t.Fatalf("first scan: %v", err)
		}
		attempts = 0
		settled := span.Chunk
		if settled > 8 {
			t.Fatalf("span settled at %d, want <= 8", settled)
		}

		calls := 0
		if _, _, err := scanNarrowing(64, 127, span, func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			calls++
			if calls > 64 {
				t.Fatal("the scan is repeating a width it already had refused")
			}
			return refuse(from, to)
		}); err != nil {
			t.Fatalf("second scan: %v", err)
		}
		if span.Chunk != settled {
			t.Fatalf("span moved to %d on a scan with no refusals, want it to stay at %d", span.Chunk, settled)
		}
		if wantCalls := 64 / int(settled); calls != wantCalls {
			t.Fatalf("second scan made %d calls, want %d — it re-discovered the cap instead of remembering it", calls, wantCalls)
		}
	})

	// The bottom of the ladder. A provider still refusing a ONE-block span is not a
	// sizing problem, and halving it further is how a "retry" becomes an infinite
	// loop. chain.Climb is what ends it, and it must end as permanent.
	t.Run("stops when the span cannot shrink further", func(t *testing.T) {
		span := &relayerclient.LogSpan{Chunk: 4}
		// The bound is the half of this test that fails LOUDLY. Its own subject is
		// "the scan halves forever", and a scan that never narrows does not report a
		// wrong answer -- it reports none, and the assertions below are never
		// reached. 4 -> 2 -> 1 -> bottom is three rejections; anything past a
		// generous multiple of that is the loop this test exists to rule out.
		calls := 0
		_, _, err := scanNarrowing(0, 7, span, func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			calls++
			if calls > 8 {
				t.Fatalf("the scan asked for [%d,%d] a %dth time without reaching the bottom of the ladder; it is not narrowing",
					from, to, calls)
			}
			return nil, nil, errors.New("block range too large")
		})
		if err == nil {
			t.Fatal("a provider refusing every span was reported as a successful scan")
		}
		if !chain.IsPermanent(err) {
			t.Fatalf("error = %v; a span that cannot shrink further must be permanent, or the scan halves forever", err)
		}
		if r, known := chain.Classify(err); !known || r.Outcome() != chain.OutcomePermanent {
			t.Fatalf("classified as %s, want permanent", r.Outcome())
		}
	})

	// A failure that is NOT a range rejection must pass straight through. Narrowing
	// on a transport error would shrink the span for a reason that has nothing to do
	// with size, and would end at the bottom of the ladder reporting permanent — a
	// dropped scan for what was a reconnect.
	t.Run("does not narrow on an ordinary failure", func(t *testing.T) {
		span := &relayerclient.LogSpan{Chunk: 32}
		wantErr := errors.New("dial tcp: connection refused")
		calls := 0
		_, _, err := scanNarrowing(0, 63, span, func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			calls++
			return nil, nil, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want the transport cause", err)
		}
		if chain.IsPermanent(err) {
			t.Fatalf("a transport failure was reported permanent: %v", err)
		}
		if span.Chunk != 32 {
			t.Fatalf("span narrowed to %d on a transport failure", span.Chunk)
		}
		if calls != 1 {
			t.Fatalf("made %d calls, want 1 — it retried a failure that retrying cannot fix here", calls)
		}
	})

	// Wrapping must not hide the rejection: the scanner wraps each piece with the
	// span it was refused for before anything classifies it.
	t.Run("rejection is found through the span wrapper", func(t *testing.T) {
		span := &relayerclient.LogSpan{Chunk: 8}
		calls := 0
		_, _, _ = scanNarrowing(0, 15, span, func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			calls++
			if calls > 64 {
				t.Fatal("the scan never bottomed out")
			}
			return nil, nil, fmt.Errorf("filter SendPacket: %w", errors.New("logs matched by query exceeds limit of 10000"))
		})
		if span.Chunk == 8 {
			t.Fatal("the rejection was not recognised through the wrapping, so nothing narrowed")
		}
	})

	// #453 stated this invariant on the chunk loop it wrote and never asserted it:
	// "One set across every span: a send in span 1 can be settled by a terminal log
	// in span 3, and chunking must not hide that from the filter." That loop is now
	// scanNarrowing, so the property has to be carried here -- and a stated
	// invariant with nothing enforcing it is a missing assertion, not documentation.
	//
	// What it costs if it breaks: dropSettled sees only the last piece's keys, so a
	// send whose AckPacket landed in a later piece is relayed for nothing and
	// tracked again -- after the settle removed it and after the cursor moved past
	// the terminal log that would have cleared it. Nothing settles it a second time.
	t.Run("unions settled across pieces", func(t *testing.T) {
		span := &relayerclient.LogSpan{Chunk: 10}
		// One key per piece, named by the piece that produced it, so a set built from
		// only the last piece is distinguishable from a set built from all of them.
		_, settled, err := scanNarrowing(0, 29, span, func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			return nil, map[settledKey]struct{}{settledKey(fmt.Sprintf("piece@%d", from)): {}}, nil
		})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, want := range []settledKey{"piece@0", "piece@10", "piece@20"} {
			if _, ok := settled[want]; !ok {
				t.Fatalf("%q is missing from the settled set %v; a packet settled in one piece "+
					"would be relayed again and re-tracked with nothing left to clear it", want, settled)
			}
		}
	})

	// The union is per ATTEMPT, not per call. scanNarrowing restarts the WHOLE range
	// after a narrowing, so keys from the attempt that failed describe pieces that
	// are about to be read again -- and if that re-read no longer reports one (a
	// reorg unwound the terminal event), the stale key would drop a packet that is
	// live again from the batch.
	//
	// The scenario has to have a piece SUCCEED before the refusal, or it proves
	// nothing: a piece that errors never reaches the union at all -- the loop breaks
	// first -- so its keys are discarded whether the set is reset or hoisted. My
	// first version of this test refused on the first piece and a hoisted set walked
	// straight through it.
	//
	// [0,63] at span 32: piece [0,31] succeeds and reports "stale", then [32,63] is
	// refused. Narrowed to 16, all four pieces succeed and report "fresh". Reset
	// gives {fresh}; hoisted gives {stale, fresh}.
	t.Run("does not carry settled across a restart", func(t *testing.T) {
		span := &relayerclient.LogSpan{Chunk: 32}
		refused := false
		// Same bound, same reason as TestScanNarrowing/stops when the span cannot shrink further: the
		// stub refuses every piece wider than 16, so a scan that does not narrow asks
		// forever and the assertions below are never reached. Two calls at span 32
		// plus four at 16 is six; the cap is a generous multiple.
		calls := 0
		_, settled, err := scanNarrowing(0, 63, span, func(from, to uint64) ([]chain.Event, map[settledKey]struct{}, error) {
			calls++
			if calls > 24 {
				t.Fatalf("the scan asked for [%d,%d] a %dth time; the span is not narrowing", from, to, calls)
			}
			if to-from+1 > 16 {
				if from == 0 {
					return nil, map[settledKey]struct{}{"unwound-by-the-reorg": {}}, nil
				}
				refused = true
				return nil, nil, errors.New("query returned more than 10000 results")
			}
			return nil, map[settledKey]struct{}{"still-settled": {}}, nil
		})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !refused {
			t.Fatal("the provider was never refused, so no restart happened and this asserts nothing")
		}
		if _, stale := settled["unwound-by-the-reorg"]; stale {
			t.Fatal("a key from a piece that SUCCEEDED in the failed attempt survived the restart; " +
				"the set was hoisted out of the retry loop")
		}
		if _, ok := settled["still-settled"]; !ok {
			t.Fatal("the completed attempt's keys were lost")
		}
	})
}

// burstyHeaderServer answers eth_getBlockByNumber with timestamps describing a
// demand-driven chain: a long idle gap, then a recent burst. Anything at or
// above burstFrom is one second apart; the block below it sits idleSeconds
// earlier, which is the shape Nitro takes after an hour of no demand.
func burstyHeaderServer(t *testing.T, burstFrom, idleSeconds uint64) *ethclient.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method != "eth_getBlockByNumber" {
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"%s"}}`, req.ID, req.Method)
			return
		}
		var number uint64
		if hex, ok := req.Params[0].(string); ok {
			_, _ = fmt.Sscanf(hex, "0x%x", &number)
		}
		// The burst is one second per block; everything below it is pushed back
		// by the idle span, so a span crossing the boundary averages badly.
		ts := number
		if number < burstFrom {
			ts = number - idleSeconds
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{`+
			`"number":"0x%x","hash":"0x%064x","parentHash":"0x%064x","timestamp":"0x%x",`+
			`"sha3Uncles":"0x%064x","miner":"0x%040x","stateRoot":"0x%064x",`+
			`"transactionsRoot":"0x%064x","receiptsRoot":"0x%064x","logsBloom":"0x%0512x",`+
			`"difficulty":"0x0","gasLimit":"0x0","gasUsed":"0x0","extraData":"0x",`+
			`"mixHash":"0x%064x","nonce":"0x0000000000000000"}}`,
			req.ID, number, number, number-1, ts, 0, 0, 0, 0, 0, 0, 0)
	}))
	t.Cleanup(server.Close)

	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatalf("dial stub: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// Found in review: two timestamps cannot tell a slow chain from a fast chain
// that was idle. After an hour of no demand the newest blockTimeSamples span
// that hour, the average reads minutes per block, and the startup window
// collapses to a couple of blocks -- while the packets that matter were emitted
// seconds ago during the burst. Nothing persists this cursor, so anything
// outside the window is lost for good.
func TestStartupLookback_SurvivesAnIdleSpanBeforeABurst(t *testing.T) {
	const (
		head        = uint64(10_000)
		idleSeconds = uint64(3600) // an hour of no demand, inside the sample span
	)
	// The burst starts INSIDE the sample span, so the span straddles the gap.
	source := &Source{eth: burstyHeaderServer(t, head-blockTimeSamples/2, idleSeconds)}

	blockTime, err := source.measureBlockTime(context.Background(), head)
	if err != nil {
		t.Fatalf("measureBlockTime: %v", err)
	}
	// The raw measurement is expected to be wrong -- that is the premise. If it
	// is not, this test is not exercising the case it was written for.
	if blockTime <= maxMeasuredBlockTime {
		t.Fatalf("measured %s, which is already within the %s cap; the stub no longer "+
			"produces an idle-inflated average and this test proves nothing",
			blockTime, maxMeasuredBlockTime)
	}
	t.Logf("raw two-sample average = %s (inflated by the idle span)", blockTime)

	got := source.startupLookback(context.Background(), head)
	want := lookbackBlocks(defaultStartupWindow, maxMeasuredBlockTime)
	if got != want {
		t.Fatalf("startupLookback = %d blocks, want %d: an idle span before a burst sized the "+
			"window from a %s average, so a packet emitted seconds ago but %d blocks back falls "+
			"outside the startup scan -- and this cursor is not persisted, so it is never rediscovered",
			got, want, blockTime, got+1)
	}
	t.Logf("capped window = %d blocks", got)
}

const (
	idleTestClientID = "arb-client-0"
	idleTestDestID   = "08-wasm-1"
	idleTestHead     = uint64(100)
)

// idleL2Node is a JSON-RPC stub for a demand-driven rollup that has gone quiet:
// it serves one SendPacket log on the first eth_getLogs call and nothing after,
// while eth_getBlockByNumber reports the same head forever. Arbitrum Nitro seals a
// block per transaction, so "no traffic" really does mean "the head stops" — this
// is the steady state, not an edge case.
type idleL2Node struct {
	router ethcommon.Address

	// logBlock is where the single SendPacket log sits; zero means idleTestHead.
	logBlock uint64

	mu sync.Mutex
	// head is the block eth_getBlockByNumber reports; zero means idleTestHead. It is
	// settable so a test can drive an unsafe-head reorg — the head moving BACKWARDS —
	// rather than only a head that stops advancing.
	head      uint64
	logCalls  int
	headCalls int
	// failHeads makes the first N head reads answer with a JSON-RPC error, standing
	// in for the rate limit a public endpoint returns during a restart.
	failHeads int
	// logRanges records the [fromBlock,toBlock] of every eth_getLogs, so a test can
	// assert the cursor neither drifted nor rewound across a reorg.
	logRanges [][2]uint64
}

func (n *idleL2Node) setHead(h uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.head = h
}

func (n *idleL2Node) scannedRanges() [][2]uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([][2]uint64(nil), n.logRanges...)
}

func (n *idleL2Node) start(t *testing.T) *ethclient.Client {
	t.Helper()
	logBlock := n.logBlock
	if logBlock == 0 {
		logBlock = idleTestHead
	}
	sendLog := sendPacketLogJSON(t, n.router, logBlock)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var result string
		switch req.Method {
		case "eth_getBlockByNumber":
			n.mu.Lock()
			n.headCalls++
			calls := n.headCalls
			failing := n.failHeads
			head := n.head
			n.mu.Unlock()
			if calls <= failing {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32005,`+
					`"message":"You reached Public endpoint rate limit"}}`, req.ID)
				return
			}
			if head == 0 {
				head = idleTestHead
			}
			// measureBlockTime asks for two specific heights; answering every
			// request with the head would report a chain whose clock never moves.
			result = idleHeaderJSON(idleRequestedBlock(t, req.Params, head))
		case "eth_getLogs":
			from, to := idleFilterRange(t, req.Params)
			n.mu.Lock()
			n.logCalls++
			first := n.logCalls == 1
			n.logRanges = append(n.logRanges, [2]uint64{from, to})
			n.mu.Unlock()
			if first {
				result = "[" + sendLog + "]" // the only packet this L2 ever emits
			} else {
				result = "[]"
			}
		case "eth_chainId":
			result = `"0x1"`
		default:
			result = "null"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	t.Cleanup(srv.Close)

	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatalf("dial stub L2: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func (n *idleL2Node) counts() (logs, heads int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.logCalls, n.headCalls
}

// idleFilterRange pulls the block range out of an eth_getLogs filter so a test can
// assert which range was scanned, not merely that a scan happened.
func idleFilterRange(t *testing.T, params []json.RawMessage) (uint64, uint64) {
	t.Helper()
	if len(params) == 0 {
		t.Fatalf("eth_getLogs called with no params")
	}
	var filter struct {
		FromBlock string `json:"fromBlock"`
		ToBlock   string `json:"toBlock"`
	}
	if err := json.Unmarshal(params[0], &filter); err != nil {
		t.Fatalf("decode eth_getLogs filter: %v", err)
	}
	parse := func(field, raw string) uint64 {
		v, err := strconv.ParseUint(strings.TrimPrefix(raw, "0x"), 16, 64)
		if err != nil {
			t.Fatalf("parse %s %q: %v", field, raw, err)
		}
		return v
	}
	return parse("fromBlock", filter.FromBlock), parse("toBlock", filter.ToBlock)
}

// sendPacketLogJSON ABI-encodes a real SendPacket log so the production filterer
// decodes it — a hand-rolled log would only prove the stub matches itself.
func sendPacketLogJSON(t *testing.T, router ethcommon.Address, block uint64) string {
	t.Helper()
	parsed, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("parse ICS26Router ABI: %v", err)
	}
	event, ok := parsed.Events["SendPacket"]
	if !ok {
		t.Fatal("ICS26Router ABI has no SendPacket event")
	}
	packet := IICS26RouterMsgsPacketFor(idleTestClientID, idleTestDestID)
	data, err := event.Inputs.NonIndexed().Pack(packet)
	if err != nil {
		t.Fatalf("pack SendPacket data: %v", err)
	}
	// Indexed string params are stored as the keccak of their contents.
	clientTopic := crypto.Keccak256Hash([]byte(idleTestClientID))
	seqTopic := ethcommon.BigToHash(big.NewInt(1))
	return fmt.Sprintf(`{
		"address":%q,"topics":[%q,%q,%q],"data":"0x%x",
		"blockNumber":"0x%x","transactionHash":%q,"transactionIndex":"0x0",
		"blockHash":%q,"logIndex":"0x0","removed":false
	}`, router.Hex(), event.ID.Hex(), clientTopic.Hex(), seqTopic.Hex(), data,
		block, ethcommon.Hash{}.Hex(), ethcommon.Hash{}.Hex())
}

// IICS26RouterMsgsPacketFor builds the minimal packet the SendPacket decoder accepts.
func IICS26RouterMsgsPacketFor(sourceClient, destClient string) contractICS26Router.IICS26RouterMsgsPacket {
	return contractICS26Router.IICS26RouterMsgsPacket{
		Sequence:         1,
		SourceClient:     sourceClient,
		DestClient:       destClient,
		TimeoutTimestamp: uint64(time.Now().Add(time.Hour).Unix()),
		Payloads: []contractICS26Router.IICS26RouterMsgsPayload{{
			SourcePort: "transfer", DestPort: "transfer",
			Version: "ics20-2", Encoding: "application/x-solidity-abi",
			Value: []byte{0x01},
		}},
	}
}

// idleHeaderJSON is the minimum header go-ethereum decodes into types.Header.
// idleTestBlockTime is the block time the fake node's timestamps imply, so
// measureBlockTime reads a real number rather than falling back.
const idleTestBlockTime = 2 * time.Second

func idleHeaderJSON(number uint64) string {
	zero := ethcommon.Hash{}.Hex()
	// Timestamps advance with the block number at idleTestBlockTime. A header
	// endpoint reporting one timestamp for every block is indistinguishable from
	// a stopped chain, and measureBlockTime rightly refuses to size a window from it.
	timestamp := number * uint64(idleTestBlockTime/time.Second)
	return fmt.Sprintf(`{
		"parentHash":%q,"sha3Uncles":%q,"miner":"0x0000000000000000000000000000000000000000",
		"stateRoot":%q,"transactionsRoot":%q,"receiptsRoot":%q,
		"logsBloom":"0x%0512x","difficulty":"0x1","number":"0x%x",
		"gasLimit":"0x1","gasUsed":"0x0","timestamp":"0x%x","extraData":"0x",
		"mixHash":%q,"nonce":"0x0000000000000000","baseFeePerGas":"0x1","hash":%q
	}`, zero, zero, zero, zero, zero, 0, number, timestamp, zero, zero)
}

// idleRequestedBlock reads the block number an eth_getBlockByNumber call asked
// for. Tags ("latest", "safe", "finalized") mean the head; a hex quantity means
// that exact block, which is what measureBlockTime needs.
func idleRequestedBlock(t *testing.T, params []json.RawMessage, head uint64) uint64 {
	t.Helper()
	if len(params) == 0 {
		return head
	}
	var raw string
	if err := json.Unmarshal(params[0], &raw); err != nil {
		return head
	}
	if !strings.HasPrefix(raw, "0x") {
		return head
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(raw, "0x"), 16, 64)
	if err != nil {
		return head
	}
	return number
}

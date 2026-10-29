package evmflush_test

import (
	"context"
	"io"
	"log"
	"math/big"
	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/chain/evmflush"
	relayerclient "relayer/client"
	"relayer/rpcmock"
	"relayer/services"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var routerAddr = ethcommon.HexToAddress("0x000000000000000000000000000000000000c26e")

func routerABI(t *testing.T) *abi.ABI {
	t.Helper()
	parsed, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("router ABI: %v", err)
	}
	return parsed
}

func packet(seq uint64, src, dst string) contractICS26Router.IICS26RouterMsgsPacket {
	return contractICS26Router.IICS26RouterMsgsPacket{
		Sequence: seq, SourceClient: src, DestClient: dst, TimeoutTimestamp: 1_900_000_000,
		Payloads: []contractICS26Router.IICS26RouterMsgsPayload{{
			SourcePort: "transfer", DestPort: "transfer", Version: "ics20-1", Encoding: "application/json", Value: []byte("{}"),
		}},
	}
}

// sendLog is a SendPacket log as the router emits it: the event id, the
// keccak of the indexed client id, the sequence, and the ABI-encoded packet.
func sendLog(t *testing.T, a *abi.ABI, clientID string, pkt contractICS26Router.IICS26RouterMsgsPacket, block uint64) rpcmock.RawLog {
	t.Helper()
	ev := a.Events["SendPacket"]
	data, err := ev.Inputs.NonIndexed().Pack(pkt)
	if err != nil {
		t.Fatalf("pack SendPacket: %v", err)
	}
	return rpcmock.RawLog{
		Address:     routerAddr,
		Topics:      []ethcommon.Hash{ev.ID, crypto.Keccak256Hash([]byte(clientID)), ethcommon.BigToHash(new(big.Int).SetUint64(pkt.Sequence))},
		Data:        data,
		BlockNumber: block,
	}
}

func writeAckLog(t *testing.T, a *abi.ABI, clientID string, pkt contractICS26Router.IICS26RouterMsgsPacket, acks [][]byte, block uint64) rpcmock.RawLog {
	t.Helper()
	ev := a.Events["WriteAcknowledgement"]
	data, err := ev.Inputs.NonIndexed().Pack(pkt, acks)
	if err != nil {
		t.Fatalf("pack WriteAcknowledgement: %v", err)
	}
	return rpcmock.RawLog{
		Address:     routerAddr,
		Topics:      []ethcommon.Hash{ev.ID, crypto.Keccak256Hash([]byte(clientID)), ethcommon.BigToHash(new(big.Int).SetUint64(pkt.Sequence))},
		Data:        data,
		BlockNumber: block,
	}
}

func newRouter(t *testing.T, node *rpcmock.EVMNode) evmflush.Router {
	t.Helper()
	return evmflush.Router{
		Client: node.Client(t), Address: routerAddr, Span: &relayerclient.LogSpan{},
		Label: "[FlushTest]", Logger: log.New(io.Discard, "", 0),
	}
}

func commitmentOf(pkt contractICS26Router.IICS26RouterMsgsPacket) ethcommon.Hash {
	return evmflush.PacketCommitment(pkt, new(big.Int).SetUint64(pkt.Sequence))
}

func commitmentPath(seq uint64) []byte {
	return services.EthPath("client-0", seq, services.EthPathCommitment)
}

// The probe is bounded above by prevSequenceSend and below by the cap, walks
// downward across passes, and wraps to the newest -- never 1..N.
func TestWindow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		prev, cursor uint64
		cap          int
		lo, hi       uint64
		ok           bool
	}{
		{name: "a router that never sent has nothing to probe", prev: 0, cursor: 0, cap: 200},
		{name: "fewer sends than the cap: the whole space, once", prev: 5, cursor: 0, cap: 200, lo: 1, hi: 5, ok: true},
		{name: "first pass takes the newest cap", prev: 1000, cursor: 0, cap: 200, lo: 801, hi: 1000, ok: true},
		{name: "next pass continues below the last", prev: 1000, cursor: 801, cap: 200, lo: 601, hi: 800, ok: true},
		{name: "the last slice is short", prev: 1000, cursor: 150, cap: 200, lo: 1, hi: 149, ok: true},
		{name: "reaching sequence 1 wraps to the newest", prev: 1000, cursor: 1, cap: 200, lo: 801, hi: 1000, ok: true},
		{name: "a cursor above prev (client re-created) restarts from the newest", prev: 300, cursor: 801, cap: 200, lo: 101, hi: 300, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi, ok := evmflush.Window(tc.prev, tc.cursor, tc.cap)
			if ok != tc.ok || lo != tc.lo || hi != tc.hi {
				t.Fatalf("Window(prev=%d, cursor=%d, cap=%d) = [%d,%d] ok=%v, want [%d,%d] ok=%v",
					tc.prev, tc.cursor, tc.cap, lo, hi, ok, tc.lo, tc.hi, tc.ok)
			}
			if ok && hi-lo+1 > uint64(tc.cap) {
				t.Fatalf("window [%d,%d] is wider than the cap %d", lo, hi, tc.cap)
			}
		})
	}
}

// A pass that runs out of time resumes at the first slot it did not resolve,
// and a pass that resolved nothing still moves past one slot.
func TestSendCursor(t *testing.T) {
	slots := []evmflush.Slot{{Sequence: 90}, {Sequence: 70}, {Sequence: 50}} // window [41,100], newest first
	for _, tc := range []struct {
		name string
		stop int
		want uint64
	}{
		{name: "a finished pass moves to the bottom of the window", stop: 3, want: 41},
		{name: "a pass stopped at 70 resumes there", stop: 1, want: 71},
		{name: "a pass stopped at the last slot resumes there", stop: 2, want: 51},
		{name: "a pass that resolved nothing still moves past the first slot", stop: 0, want: 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := evmflush.SendCursor(41, slots, tc.stop)
			if got != tc.want {
				t.Fatalf("SendCursor(stop=%d) = %d, want %d", tc.stop, got, tc.want)
			}
			// The cursor's meaning, checked through Window: the next pass's top.
			if lo, hi, _ := evmflush.Window(100, got, 200); tc.stop > 0 && tc.stop < len(slots) && hi != slots[tc.stop].Sequence {
				t.Fatalf("next window [%d,%d] does not start at the unresolved seq %d", lo, hi, slots[tc.stop].Sequence)
			}
		})
	}
	if got := evmflush.SendCursor(41, nil, 0); got != 41 {
		t.Fatalf("SendCursor with nothing outstanding = %d, want the window's bottom 41", got)
	}
}

// A send the pass refused to handle is retried next time, even when it is the
// first slot: that is the one case SendCursor's step past slots[0] is wrong for.
func TestSendCursorAt(t *testing.T) {
	slots := []evmflush.Slot{{Sequence: 90}, {Sequence: 70}, {Sequence: 50}} // window [41,100], newest first
	for at, slot := range slots {
		got := evmflush.SendCursorAt(slots, at)
		if _, hi, _ := evmflush.Window(100, got, 200); hi != slot.Sequence {
			t.Fatalf("SendCursorAt(%d) = %d: next window tops at %d, want the refused seq %d", at, got, hi, slot.Sequence)
		}
	}
	// The newest outstanding send is the router's last: the cursor lands above
	// prevSequenceSend, which Window treats as "start from the newest".
	newest := []evmflush.Slot{{Sequence: 100}}
	if _, hi, _ := evmflush.Window(100, evmflush.SendCursorAt(newest, 0), 200); hi != 100 {
		t.Fatalf("next window tops at %d, want 100", hi)
	}
}

func TestAckCursor(t *testing.T) {
	// The ack window after cursor 10: 11..13, then wrapped to 2..3.
	window := []uint64{11, 12, 13, 2, 3}
	written := []evmflush.Slot{{Sequence: 12}, {Sequence: 2}, {Sequence: 3}}
	for _, tc := range []struct {
		name string
		stop int
		want uint64
	}{
		{name: "a finished pass moves to the window's last", stop: 3, want: 3},
		{name: "a pass stopped past the wrap resumes at 2", stop: 1, want: 1},
		{name: "a pass stopped at 3 resumes there", stop: 2, want: 2},
		{name: "a pass that resolved nothing still moves past the first slot", stop: 0, want: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evmflush.AckCursor(window, written, tc.stop); got != tc.want {
				t.Fatalf("AckCursor(stop=%d) = %d, want %d", tc.stop, got, tc.want)
			}
		})
	}
}

func TestSequences_AreNewestFirst(t *testing.T) {
	got := evmflush.Sequences(3, 5)
	if len(got) != 3 || got[0] != 5 || got[2] != 3 {
		t.Fatalf("Sequences(3,5) = %v, want [5 4 3]", got)
	}
	if got := evmflush.Sequences(5, 3); got != nil {
		t.Fatalf("an inverted range must be empty, got %v", got)
	}
}

func TestPrevSequenceSend_ReadsTheRouterSlot(t *testing.T) {
	node := rpcmock.NewEVMNode(t)
	node.SetStorage(routerAddr, services.EthPrevSequenceSendKey("client-0"), ethcommon.BigToHash(big.NewInt(1234)))

	prev, err := newRouter(t, node).PrevSequenceSend(context.Background(), "client-0")
	if err != nil || prev != 1234 {
		t.Fatalf("PrevSequenceSend = (%d, %v), want 1234", prev, err)
	}
	if prev, err := newRouter(t, node).PrevSequenceSend(context.Background(), "client-9"); err != nil || prev != 0 {
		t.Fatalf("a client that never sent must read 0, got (%d, %v)", prev, err)
	}
}

// One pass over 150 sequences is TWO round trips (batches of 100), not 150, and
// only the slots that hold a value come back -- with the value, which is what
// says which packet the sequence names.
func TestOutstandingSlots_IsBatchedAndReturnsOnlyHeldSlots(t *testing.T) {
	node := rpcmock.NewEVMNode(t)
	held7 := ethcommon.HexToHash("0x07")
	held120 := ethcommon.HexToHash("0x0120")
	node.SetStorage(routerAddr, services.EthIBCStorageKey(commitmentPath(7)), held7)
	node.SetStorage(routerAddr, services.EthIBCStorageKey(commitmentPath(120)), held120)

	slots, err := newRouter(t, node).OutstandingSlots(context.Background(), evmflush.Sequences(1, 150), commitmentPath)
	if err != nil {
		t.Fatalf("OutstandingSlots: %v", err)
	}
	if len(slots) != 2 || slots[0].Sequence != 120 || slots[0].Value != held120 || slots[1].Sequence != 7 || slots[1].Value != held7 {
		t.Fatalf("slots = %+v, want [120, 7] with their values, newest first", slots)
	}
	if got := node.MethodCalls("eth_getStorageAt"); got != 150 {
		t.Fatalf("eth_getStorageAt calls = %d, want one per probed sequence (150)", got)
	}
	if got := node.Batches(); got != 2 {
		t.Fatalf("JSON-RPC batches = %d, want 2: the probe must be one round trip per 100 slots, not one per slot", got)
	}
}

func TestSendLog(t *testing.T) {
	t.Run("finds the event by topic on a short chain in one call", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		node.SetBlockNumber(5000)
		node.AddLog(sendLog(t, a, "client-0", packet(41, "client-0", "08-wasm-0"), 10))
		node.AddLog(sendLog(t, a, "client-0", packet(42, "client-0", "08-wasm-0"), 11))
		node.AddLog(sendLog(t, a, "client-1", packet(42, "client-1", "08-wasm-0"), 12)) // same sequence, other client

		ev, found, err := newRouter(t, node).SendLog(context.Background(), "client-0", 42, commitmentOf(packet(42, "client-0", "08-wasm-0")))
		if err != nil || !found {
			t.Fatalf("SendLog = (found=%v, %v), want the log at block 11", found, err)
		}
		if ev.Packet.SourceClient != "client-0" || ev.Sequence.Uint64() != 42 || ev.Raw.BlockNumber != 11 {
			t.Fatalf("wrong log: client=%s seq=%d block=%d", ev.Packet.SourceClient, ev.Sequence.Uint64(), ev.Raw.BlockNumber)
		}
		if got := node.MethodCalls("eth_getLogs"); got != 1 {
			t.Fatalf("eth_getLogs calls = %d, want 1: a chain shorter than DefaultLogChunk is one window", got)
		}
	})

	// #537: never [0, head]. Even an uncapped provider is asked for bounded
	// windows walking back from the head -- a full-range eth_getLogs per probed
	// sequence is the most expensive query a node can be asked.
	t.Run("walks back from the head in bounded windows, never from block zero", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		const head = 1_000_000
		node.SetBlockNumber(head)
		node.AddLog(sendLog(t, a, "client-0", packet(7, "client-0", "08-wasm-0"), head-500))
		node.AddLog(sendLog(t, a, "client-0", packet(8, "client-0", "08-wasm-0"), head-3*evmflush.DefaultLogChunk))

		router := newRouter(t, node)
		ev, found, err := router.SendLog(context.Background(), "client-0", 7, commitmentOf(packet(7, "client-0", "08-wasm-0")))
		if err != nil || !found || ev.Raw.BlockNumber != head-500 {
			t.Fatalf("SendLog(recent) = (found=%v, %v), want the log near the head", found, err)
		}
		if got := node.MethodCalls("eth_getLogs"); got != 1 {
			t.Fatalf("eth_getLogs calls = %d, want 1: a log in the newest window is one query", got)
		}
		ev, found, err = router.SendLog(context.Background(), "client-0", 8, commitmentOf(packet(8, "client-0", "08-wasm-0")))
		if err != nil || !found || ev.Raw.BlockNumber != head-3*evmflush.DefaultLogChunk {
			t.Fatalf("SendLog(older) = (found=%v, %v), want the log four windows down", found, err)
		}
		for _, r := range node.LogRanges() {
			if r[0] == 0 || r[1]-r[0]+1 > evmflush.DefaultLogChunk {
				t.Fatalf("eth_getLogs asked [%d,%d]: a lookup must stay within DefaultLogChunk below the head", r[0], r[1])
			}
		}
		if router.Span.Chunk != 0 {
			t.Fatalf("span chunk = %d, want untouched: no provider refused anything", router.Span.Chunk)
		}
	})

	// #537, observed live: geth with history expiry refuses ANY range whose
	// fromBlock is below its cutoff, so every lookup failed and the backstop
	// recovered nothing. The walk must find the floor inside the chunk that
	// straddles it, still find a log above it, report a log below it as not
	// found (not an error), and remember the floor so later lookups never ask
	// below it again.
	t.Run("learns a pruned-history floor and stops there", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		const head, floor = 100_000, 95_500
		node.SetBlockNumber(head)
		node.PruneLogsBelow(floor, "pruned history unavailable")
		node.AddLog(sendLog(t, a, "client-0", packet(1, "client-0", "08-wasm-0"), floor+100)) // inside the straddling chunk
		node.AddLog(sendLog(t, a, "client-0", packet(2, "client-0", "08-wasm-0"), 50_000))    // below the floor

		router := newRouter(t, node)
		ev, found, err := router.SendLog(context.Background(), "client-0", 1, commitmentOf(packet(1, "client-0", "08-wasm-0")))
		if err != nil || !found || ev.Raw.BlockNumber != floor+100 {
			t.Fatalf("SendLog(above floor) = (found=%v, %v), want the log at %d", found, err, floor+100)
		}
		if router.Span.Floor != floor {
			t.Fatalf("span floor = %d, want %d", router.Span.Floor, floor)
		}
		if router.Span.Chunk != 0 {
			t.Fatalf("span chunk = %d: a pruned-history refusal is not a range refusal and must not narrow", router.Span.Chunk)
		}
		if got := node.MethodCalls("eth_getLogs"); got > 20 {
			t.Fatalf("eth_getLogs calls = %d, want about log2(DefaultLogChunk) probes plus two lookups", got)
		}

		before := node.MethodCalls("eth_getLogs")
		_, found, err = router.SendLog(context.Background(), "client-0", 2, commitmentOf(packet(2, "client-0", "08-wasm-0")))
		if err != nil {
			t.Fatalf("a log below the pruned floor is not an error: %v", err)
		}
		if found {
			t.Fatal("a log below the pruned floor was found")
		}
		if got := node.MethodCalls("eth_getLogs") - before; got != 1 {
			t.Fatalf("second lookup cost %d eth_getLogs calls, want 1: the floor is known, [floor, head] is one window", got)
		}
		ranges := node.LogRanges()
		if last := ranges[len(ranges)-1]; last[0] != floor || last[1] != head {
			t.Fatalf("second lookup asked [%d,%d], want [%d,%d]", last[0], last[1], floor, head)
		}
	})

	// A node keeps pruning. A floor learned earlier is just the next refused
	// lower bound, found the same way.
	t.Run("re-learns a floor that moved up", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		node.SetBlockNumber(100_000)
		node.PruneLogsBelow(95_500, "pruned history unavailable")
		node.AddLog(sendLog(t, a, "client-0", packet(3, "client-0", "08-wasm-0"), 98_000))

		router := newRouter(t, node)
		if _, _, err := router.SendLog(context.Background(), "client-0", 9, ethcommon.HexToHash("0x01")); err != nil {
			t.Fatalf("first lookup: %v", err)
		}
		node.PruneLogsBelow(97_000, "pruned history unavailable")
		ev, found, err := router.SendLog(context.Background(), "client-0", 3, commitmentOf(packet(3, "client-0", "08-wasm-0")))
		if err != nil || !found || ev.Raw.BlockNumber != 98_000 {
			t.Fatalf("SendLog after the floor moved = (found=%v, %v), want the log at 98000", found, err)
		}
		if router.Span.Floor != 97_000 {
			t.Fatalf("span floor = %d, want the new floor 97000", router.Span.Floor)
		}
	})

	// With router_deploy_block configured there is nothing to guess: the router's
	// whole history is [deploy, head], asked in one call -- including an event
	// older than the default walk would ever reach.
	t.Run("asks [deploy block, head] in one call when the deploy block is known", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		const head, deploy = 2_000_000, 1_000_000
		node.SetBlockNumber(head)
		pkt := packet(5, "client-0", "08-wasm-0")
		node.AddLog(sendLog(t, a, "client-0", pkt, deploy+50)) // ~1M blocks back, past the default walk's reach

		router := newRouter(t, node)
		router.DeployBlock = deploy
		ev, found, err := router.SendLog(context.Background(), "client-0", 5, commitmentOf(pkt))
		if err != nil || !found || ev.Raw.BlockNumber != deploy+50 {
			t.Fatalf("SendLog = (found=%v, %v), want the log at %d", found, err, deploy+50)
		}
		ranges := node.LogRanges()
		if len(ranges) != 1 || ranges[0] != [2]uint64{deploy, head} {
			t.Fatalf("eth_getLogs ranges = %v, want exactly [[%d %d]]", ranges, deploy, head)
		}

		// The same lookup without the deploy block walks bounded windows and
		// gives up before reaching it -- the reason the setting exists.
		_, found, err = newRouter(t, node).SendLog(context.Background(), "client-0", 5, commitmentOf(pkt))
		if err != nil || found {
			t.Fatalf("without a deploy block = (found=%v, %v), want not found after MaxLogChunks windows", found, err)
		}
	})

	// A capped provider still narrows, and the walk never goes below the deploy
	// block.
	t.Run("narrows from [deploy block, head] and never asks below it", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		const head, deploy = 1_200_000, 1_000_000
		node.SetBlockNumber(head)
		node.CapLogRange(50_000, "block range too large")
		pkt := packet(6, "client-0", "08-wasm-0")
		node.AddLog(sendLog(t, a, "client-0", pkt, deploy+10))

		router := newRouter(t, node)
		router.DeployBlock = deploy
		ev, found, err := router.SendLog(context.Background(), "client-0", 6, commitmentOf(pkt))
		if err != nil || !found || ev.Raw.BlockNumber != deploy+10 {
			t.Fatalf("SendLog = (found=%v, %v), want the log at %d", found, err, deploy+10)
		}
		for _, r := range node.LogRanges() {
			if r[0] < deploy {
				t.Fatalf("eth_getLogs asked [%d,%d], below the deploy block %d", r[0], r[1], deploy)
			}
		}
	})

	// A pruned floor above the deploy block wins; the deploy block is not a
	// reason to keep asking for history the node no longer has.
	t.Run("a pruned floor above the deploy block takes over", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		const head, deploy, floor = 1_200_000, 1_000_000, 1_150_000
		node.SetBlockNumber(head)
		node.PruneLogsBelow(floor, "pruned history unavailable")
		above, below := packet(7, "client-0", "08-wasm-0"), packet(8, "client-0", "08-wasm-0")
		node.AddLog(sendLog(t, a, "client-0", above, floor+10_000))
		node.AddLog(sendLog(t, a, "client-0", below, deploy+10))

		router := newRouter(t, node)
		router.DeployBlock = deploy
		if _, found, err := router.SendLog(context.Background(), "client-0", 7, commitmentOf(above)); err != nil || !found {
			t.Fatalf("SendLog(above floor) = (found=%v, %v), want found", found, err)
		}
		if router.Span.Floor != floor {
			t.Fatalf("span floor = %d, want %d", router.Span.Floor, floor)
		}
		before := node.MethodCalls("eth_getLogs")
		if _, found, err := router.SendLog(context.Background(), "client-0", 8, commitmentOf(below)); err != nil || found {
			t.Fatalf("SendLog(below floor) = (found=%v, %v), want not found and no error", found, err)
		}
		if got := node.MethodCalls("eth_getLogs") - before; got != 1 {
			t.Fatalf("lookup after the floor is known cost %d calls, want 1 ([floor, head])", got)
		}
	})

	t.Run("a deploy block above the head asks nothing", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		node.SetBlockNumber(1_000)
		router := newRouter(t, node)
		router.DeployBlock = 5_000
		if _, found, err := router.SendLog(context.Background(), "client-0", 1, ethcommon.HexToHash("0x01")); err != nil || found {
			t.Fatalf("SendLog = (found=%v, %v), want not found and no error", found, err)
		}
		if got := node.MethodCalls("eth_getLogs"); got != 0 {
			t.Fatalf("eth_getLogs calls = %d, want 0: nothing of this router exists below the head", got)
		}
	})

	// A provider that caps the range is asked for less and the walk starts at the
	// head, where an outstanding packet is most likely to be.
	t.Run("narrows when the provider refuses and walks back from the head", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		node.SetBlockNumber(1000)
		node.CapLogRange(100, "query returned more than 10000 results")
		node.AddLog(sendLog(t, a, "client-0", packet(3, "client-0", "08-wasm-0"), 850))

		router := newRouter(t, node)
		ev, found, err := router.SendLog(context.Background(), "client-0", 3, commitmentOf(packet(3, "client-0", "08-wasm-0")))
		if err != nil || !found || ev.Raw.BlockNumber != 850 {
			t.Fatalf("SendLog = (found=%v, %v), want the log at block 850", found, err)
		}
		if router.Span.Chunk == 0 || router.Span.Chunk > 100 {
			t.Fatalf("span chunk = %d after a refusal, want narrowed to at most the provider's 100", router.Span.Chunk)
		}
		if got := node.MethodCalls("eth_getLogs"); got > 8 {
			t.Fatalf("eth_getLogs calls = %d; a log 150 blocks below the head must not cost a walk of the whole chain", got)
		}
	})

	// The walk is bounded. A packet whose event is older than MaxLogChunks provider
	// chunks is reported and left, not chased to genesis one chunk at a time.
	t.Run("gives up after max log chunks", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		node.SetBlockNumber(100_000)
		node.CapLogRange(10, "block range too large")
		node.AddLog(sendLog(t, a, "client-0", packet(1, "client-0", "08-wasm-0"), 5))

		_, found, err := newRouter(t, node).SendLog(context.Background(), "client-0", 1, commitmentOf(packet(1, "client-0", "08-wasm-0")))
		if err != nil {
			t.Fatalf("giving up is not an error: %v", err)
		}
		if found {
			t.Fatal("a log 100k blocks back was found through a 10-block cap; the walk is not bounded")
		}
		// MaxLogChunks walks plus the handful of refused requests it took to find the cap.
		if got := node.MethodCalls("eth_getLogs"); got > evmflush.MaxLogChunks+16 {
			t.Fatalf("eth_getLogs calls = %d, want at most MaxLogChunks (%d) plus the ladder", got, evmflush.MaxLogChunks)
		}
	})

	// Reported by @neitdung on #496. A client re-created on the same id restarts
	// its sequences, so the topic filter matches one SendPacket log per generation
	// and a full-range query returns the OLDEST first. Stopping at the first log
	// rebuilt the closed packet, its commitment mismatched, and the live send behind
	// the same sequence was never examined -- on every pass. The held commitment
	// picks the generation; a commitment no log matches is "not found", not an
	// error, and costs one query.
	t.Run("picks the generation whose commitment the router holds", func(t *testing.T) {
		node := rpcmock.NewEVMNode(t)
		a := routerABI(t)
		node.SetBlockNumber(5000)
		old := packet(1, "client-0", "08-wasm-0")
		old.TimeoutTimestamp = 1_700_000_000
		current := packet(1, "client-0", "08-wasm-0")
		node.AddLog(sendLog(t, a, "client-0", old, 10))
		node.AddLog(sendLog(t, a, "client-0", current, 4000))

		router := newRouter(t, node)
		ev, found, err := router.SendLog(context.Background(), "client-0", 1, commitmentOf(current))
		if err != nil || !found || ev.Raw.BlockNumber != 4000 {
			t.Fatalf("SendLog(current) = (found=%v, block=%v, %v), want the live generation at block 4000", found, ev, err)
		}
		ev, found, err = router.SendLog(context.Background(), "client-0", 1, commitmentOf(old))
		if err != nil || !found || ev.Raw.BlockNumber != 10 {
			t.Fatalf("SendLog(old) = (found=%v, %v), want the old generation at block 10", found, err)
		}
		calls := node.MethodCalls("eth_getLogs")
		_, found, err = router.SendLog(context.Background(), "client-0", 1, ethcommon.HexToHash("0xdead"))
		if err != nil || found {
			t.Fatalf("SendLog(unknown commitment) = (found=%v, %v), want not found and no error", found, err)
		}
		if got := node.MethodCalls("eth_getLogs") - calls; got != 1 {
			t.Fatalf("an unmatched commitment cost %d eth_getLogs calls, want 1: the whole (short) chain was already asked", got)
		}
	})
}

func TestWriteAckLog_CarriesTheAcknowledgements(t *testing.T) {
	node := rpcmock.NewEVMNode(t)
	a := routerABI(t)
	node.SetBlockNumber(50)
	acks := [][]byte{[]byte(`{"result":"AQ=="}`)}
	node.AddLog(writeAckLog(t, a, "client-0", packet(9, "08-wasm-0", "client-0"), acks, 40))

	ev, found, err := newRouter(t, node).WriteAckLog(context.Background(), "client-0", 9, commitmentOf(packet(9, "08-wasm-0", "client-0")).Bytes())
	if err != nil || !found {
		t.Fatalf("WriteAckLog = (found=%v, %v)", found, err)
	}
	if len(ev.Acknowledgements) != 1 || string(ev.Acknowledgements[0]) != string(acks[0]) {
		t.Fatalf("acknowledgements = %q, want the log's", ev.Acknowledgements)
	}
	if ev.Packet.DestClient != "client-0" || ev.Sequence.Uint64() != 9 {
		t.Fatalf("wrong log: dest=%s seq=%d", ev.Packet.DestClient, ev.Sequence.Uint64())
	}
}

// The same rule on the acknowledgement side, where the commitment is the one the
// SENDING chain holds -- and the walk-back path, where the live generation sits
// in a newer chunk than the stale one.
func TestWriteAckLog_PicksTheGenerationTheSenderStillHolds(t *testing.T) {
	node := rpcmock.NewEVMNode(t)
	a := routerABI(t)
	node.SetBlockNumber(1000)
	node.CapLogRange(100, "block range too large")
	acks := [][]byte{[]byte(`{"result":"AQ=="}`)}
	old := packet(2, "08-wasm-0", "client-0")
	old.TimeoutTimestamp = 1_700_000_000
	current := packet(2, "08-wasm-0", "client-0")
	node.AddLog(writeAckLog(t, a, "client-0", old, acks, 300))
	node.AddLog(writeAckLog(t, a, "client-0", current, acks, 950))

	ev, found, err := newRouter(t, node).WriteAckLog(context.Background(), "client-0", 2, commitmentOf(current).Bytes())
	if err != nil || !found || ev.Raw.BlockNumber != 950 {
		t.Fatalf("WriteAckLog(current) = (found=%v, %v), want the live generation at block 950", found, err)
	}
	ev, found, err = newRouter(t, node).WriteAckLog(context.Background(), "client-0", 2, commitmentOf(old).Bytes())
	if err != nil || !found || ev.Raw.BlockNumber != 300 {
		t.Fatalf("WriteAckLog(old) = (found=%v, %v), want the old generation at block 300", found, err)
	}
}

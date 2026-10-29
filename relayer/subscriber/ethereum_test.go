package subscriber

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/chain"
	relayerclient "relayer/client"
	"relayer/services"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gorilla/websocket"
)

func TestEthPacketToCosmosPacket(t *testing.T) {
	t.Run("single payload", func(t *testing.T) {
		ethPacket := contractICS26Router.IICS26RouterMsgsPacket{
			SourceClient:     "eth-client-0",
			DestClient:       "cosmos-client-0",
			TimeoutTimestamp: 1700000000,
			Payloads: []contractICS26Router.IICS26RouterMsgsPayload{
				{
					SourcePort: "transfer",
					DestPort:   "transfer",
					Version:    "ics20-1",
					Encoding:   "proto3",
					Value:      []byte("test-value"),
				},
			},
		}

		seq := big.NewInt(42)
		result := EthPacketToCosmosPacket(ethPacket, seq)

		if result.Sequence != 42 {
			t.Errorf("Sequence: got %d, want 42", result.Sequence)
		}
		if result.SourceClient != "eth-client-0" {
			t.Errorf("SourceClient: got %q, want %q", result.SourceClient, "eth-client-0")
		}
		if result.DestinationClient != "cosmos-client-0" {
			t.Errorf("DestinationClient: got %q, want %q", result.DestinationClient, "cosmos-client-0")
		}
		if result.TimeoutTimestamp != 1700000000 {
			t.Errorf("TimeoutTimestamp: got %d, want 1700000000", result.TimeoutTimestamp)
		}
		if len(result.Payloads) != 1 {
			t.Fatalf("Payloads length: got %d, want 1", len(result.Payloads))
		}

		p := result.Payloads[0]
		if p.SourcePort != "transfer" {
			t.Errorf("SourcePort: got %q", p.SourcePort)
		}
		if p.DestinationPort != "transfer" {
			t.Errorf("DestinationPort: got %q", p.DestinationPort)
		}
		if p.Version != "ics20-1" {
			t.Errorf("Version: got %q", p.Version)
		}
		if p.Encoding != "proto3" {
			t.Errorf("Encoding: got %q", p.Encoding)
		}
		if string(p.Value) != "test-value" {
			t.Errorf("Value: got %q", p.Value)
		}
	})

	t.Run("multiple payloads", func(t *testing.T) {
		ethPacket := contractICS26Router.IICS26RouterMsgsPacket{
			SourceClient:     "src",
			DestClient:       "dst",
			TimeoutTimestamp: 999,
			Payloads: []contractICS26Router.IICS26RouterMsgsPayload{
				{SourcePort: "port1", DestPort: "port1d", Version: "v1", Encoding: "e1", Value: []byte("val1")},
				{SourcePort: "port2", DestPort: "port2d", Version: "v2", Encoding: "e2", Value: []byte("val2")},
				{SourcePort: "port3", DestPort: "port3d", Version: "v3", Encoding: "e3", Value: []byte("val3")},
			},
		}

		result := EthPacketToCosmosPacket(ethPacket, big.NewInt(1))

		if len(result.Payloads) != 3 {
			t.Fatalf("Payloads length: got %d, want 3", len(result.Payloads))
		}
		for i, p := range result.Payloads {
			if p.SourcePort == "" {
				t.Errorf("payload[%d] SourcePort is empty", i)
			}
			if p.DestinationPort == "" {
				t.Errorf("payload[%d] DestinationPort is empty", i)
			}
		}
	})

	t.Run("empty payloads", func(t *testing.T) {
		ethPacket := contractICS26Router.IICS26RouterMsgsPacket{
			SourceClient:     "src",
			DestClient:       "dst",
			TimeoutTimestamp: 0,
			Payloads:         []contractICS26Router.IICS26RouterMsgsPayload{},
		}

		result := EthPacketToCosmosPacket(ethPacket, big.NewInt(0))

		if len(result.Payloads) != 0 {
			t.Errorf("expected 0 payloads, got %d", len(result.Payloads))
		}
	})
}

func TestEthStartupRecoveryLookbackBlocksFromEnv(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		raw  string
		want uint64
	}{
		{name: "empty uses default", raw: "", want: defaultEthStartupRecoveryLookbackBlocks},
		{name: "invalid uses default", raw: "not-a-number", want: defaultEthStartupRecoveryLookbackBlocks},
		{name: "zero allowed", raw: "0", want: 0},
		{name: "custom value", raw: "1024", want: 1024},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ethStartupRecoveryLookbackBlocksFromEnv(tc.raw)
			if got != tc.want {
				t.Fatalf("ethStartupRecoveryLookbackBlocksFromEnv(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEthStartupRecoveryStartBlock(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		latest    uint64
		lookback  uint64
		wantStart uint64
	}{
		{name: "lookback smaller than latest", latest: 1000, lookback: 256, wantStart: 744},
		{name: "lookback equals latest", latest: 256, lookback: 256, wantStart: 0},
		{name: "lookback greater than latest", latest: 42, lookback: 100, wantStart: 0},
		{name: "zero lookback", latest: 42, lookback: 0, wantStart: 42},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := ethStartupRecoveryStartBlock(tc.latest, tc.lookback)
			if got != tc.wantStart {
				t.Fatalf("ethStartupRecoveryStartBlock(%d, %d) = %d, want %d",
					tc.latest, tc.lookback, got, tc.wantStart)
			}
		})
	}
}

func TestEthEventClientIDFilter(t *testing.T) {
	t.Parallel()

	ctx := ethDeps{}
	if got := ethEventClientIDFilter(ctx); got != nil {
		t.Fatalf("ethEventClientIDFilter without router client ID = %v, want nil", got)
	}

	ctx.IDs.CosmosOnEVM = "simd-2-client-0"
	got := ethEventClientIDFilter(ctx)
	if len(got) != 1 || got[0] != "simd-2-client-0" {
		t.Fatalf("ethEventClientIDFilter = %v, want [simd-2-client-0]", got)
	}
}

func TestAdvanceRecoveryStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		initial   uint64
		candidate uint64
		want      uint64
	}{
		{name: "advance forward", initial: 10, candidate: 15, want: 15},
		{name: "equal keeps current", initial: 10, candidate: 10, want: 10},
		{name: "smaller keeps current", initial: 10, candidate: 5, want: 10},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.initial
			advanceRecoveryStart(&got, tc.candidate)
			if got != tc.want {
				t.Fatalf("advanceRecoveryStart(%d, %d) = %d, want %d", tc.initial, tc.candidate, got, tc.want)
			}
		})
	}
}

func TestEnqueueEthSendPacket(t *testing.T) {
	bb := services.NewBatchBuilder()
	ev := &contractICS26Router.ContractICS26RouterSendPacket{
		Sequence: big.NewInt(7),
		Packet: contractICS26Router.IICS26RouterMsgsPacket{
			SourceClient:     "eth-client-0",
			DestClient:       "cosmos-client-0",
			TimeoutTimestamp: 1234,
			Payloads: []contractICS26Router.IICS26RouterMsgsPayload{
				{
					SourcePort: "transfer",
					DestPort:   "transfer",
					Version:    "ics20-1",
					Encoding:   "proto3",
					Value:      []byte("payload"),
				},
			},
		},
		Raw: gethtypes.Log{BlockNumber: 88},
	}

	if _, err := enqueueEthSendPacket(bb, ev, nil); err != nil {
		t.Fatal(err)
	}

	got := flushSingleEthPacket(t, bb)
	if got.Type != services.EthSend {
		t.Fatalf("packet type = %v, want %v", got.Type, services.EthSend)
	}
	if got.Packet.Sequence != 7 {
		t.Fatalf("packet sequence = %d, want 7", got.Packet.Sequence)
	}
	if got.BlockNumber != 88 {
		t.Fatalf("block number = %d, want 88", got.BlockNumber)
	}
	if bb.EthPendingTracker.Len() != 1 {
		t.Fatalf("eth pending tracker len = %d, want 1", bb.EthPendingTracker.Len())
	}
	pending := bb.EthPendingTracker.GetAll()
	if pending[0].Packet.Sequence != 7 || pending[0].BlockNumber != 88 {
		t.Fatalf("eth pending entry = seq %d block %d, want seq 7 block 88",
			pending[0].Packet.Sequence, pending[0].BlockNumber)
	}

	t.Run("dedupes seen event", func(t *testing.T) {
		t.Parallel()

		bb := services.NewBatchBuilder()
		ev := &contractICS26Router.ContractICS26RouterSendPacket{
			Sequence: big.NewInt(7),
			Packet: contractICS26Router.IICS26RouterMsgsPacket{
				SourceClient: "eth-client-0",
				DestClient:   "cosmos-client-0",
			},
			Raw: gethtypes.Log{BlockNumber: 88, Index: 3},
		}
		seen := make(map[ethEventKey]struct{})

		if enqueued, err := enqueueEthSendPacket(bb, ev, seen); err != nil || !enqueued {
			t.Fatal("first enqueue should be accepted")
		}
		if enqueued, err := enqueueEthSendPacket(bb, ev, seen); err != nil || enqueued {
			t.Fatal("duplicate enqueue should be skipped")
		}

		got := flushSingleEthPacket(t, bb)
		if got.Packet.Sequence != 7 {
			t.Fatalf("packet sequence = %d, want 7", got.Packet.Sequence)
		}
		if bb.EthPendingTracker.Len() != 1 {
			t.Fatalf("eth pending tracker len = %d, want 1", bb.EthPendingTracker.Len())
		}
	})

	t.Run("retries when pending state cannot persist", func(t *testing.T) {
		dir := t.TempDir()
		bb, err := services.NewPersistentBatchBuilder(dir)
		if err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(dir, "eth.json")
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(statePath, 0o700); err != nil {
			t.Fatal(err)
		}

		ev := &contractICS26Router.ContractICS26RouterSendPacket{
			Sequence: big.NewInt(7),
			Packet: contractICS26Router.IICS26RouterMsgsPacket{
				SourceClient: "eth-client-0",
				DestClient:   "cosmos-client-0",
			},
			Raw: gethtypes.Log{BlockNumber: 88, Index: 3},
		}
		seen := make(map[ethEventKey]struct{})

		if enqueued, err := enqueueEthSendPacket(bb, ev, seen); err == nil || enqueued {
			t.Fatal("enqueue succeeded after pending-state persistence failed")
		}
		if len(seen) != 0 {
			t.Fatal("failed pending-state write marked the source event seen")
		}
		if bb.EthPendingTracker.Len() != 0 {
			t.Fatal("failed pending-state write left the packet tracked in memory")
		}

		ch := make(chan services.EthBatch, 1)
		bb.CheckEth(context.Background(), services.BatchConfig{BatchSize: 1, BatchPeriods: time.Hour}, ch)
		select {
		case batch := <-ch:
			t.Fatalf("failed pending-state write still enqueued relay batch: %+v", batch)
		default:
		}
	})
}

func TestRecoverEthSendPackets(t *testing.T) {
	t.Run("failed eth pending add keeps recovery cursor retryable", func(t *testing.T) {
		dir := t.TempDir()
		bb, err := services.NewPersistentBatchBuilder(dir)
		if err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(dir, "eth.json")
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(statePath, 0o700); err != nil {
			t.Fatal(err)
		}

		router := common.HexToAddress("0x00000000000000000000000000000000000000AA")
		server := ethRecoveryRPCServer(t, ethRecoverySendPacketLog(t, router))
		ethClient, err := ethclient.Dial(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ethClient.Close)
		cosmosClient, err := rpchttp.New(server.URL, "/websocket")
		if err != nil {
			t.Fatal(err)
		}
		filterer, err := contractICS26Router.NewContractICS26RouterFilterer(router, ethClient)
		if err != nil {
			t.Fatal(err)
		}

		cursor := uint64(88)
		sub := NewSubscriber(nil)
		deps := ethDeps{
			Cosmos: services.CosmosEndpoint{Client: cosmosClient},
			EVM: services.EVMEndpoint{
				Client:    ethClient,
				Contracts: services.EVMContracts{Router: router},
			},
			Logger: log.New(io.Discard, "", 0),
		}
		span := &relayerclient.LogSpan{Chunk: 3}
		_, err = sub.scanEthRangeInChunks(context.Background(), deps, "SendPacket", &cursor, 90, span,
			func(from, to uint64) (ethRecoveryStats, error) {
				return recoverEthSendPackets(context.Background(), deps, bb, filterer, from, to, nil)
			}, func() {})
		if err == nil {
			t.Fatal("failed durable pending add did not fail the recovery chunk")
		}
		if cursor != 88 {
			t.Fatalf("recovery cursor advanced to %d after failed durable add, want 88", cursor)
		}
		if bb.EthPendingTracker.Len() != 0 {
			t.Fatal("failed durable pending add left packet tracked")
		}
	})

	t.Run("refuses backwards range before dialing", func(t *testing.T) {
		assertRefusesBackwardsRange(t, ethBackwardsRangeScans["send packets"])
	})

	t.Run("filter logs honors cancellation", func(t *testing.T) {
		server, started := silentRPCServer(t)
		client, err := ethclient.Dial(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		filterer, err := contractICS26Router.NewContractICS26RouterFilterer(common.Address{}, client)
		if err != nil {
			t.Fatal(err)
		}
		deps := ethDeps{EVM: services.EVMEndpoint{Client: client}, Logger: log.New(io.Discard, "", 0)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := recoverEthSendPackets(ctx, deps, services.NewBatchBuilder(), filterer, 1, 2, nil)
			done <- err
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("FilterLogs ignored cancellation")
		}
	})
}

func TestEnqueueEthWriteAcknowledgement(t *testing.T) {
	t.Parallel()

	bb := services.NewBatchBuilder()
	ev := &contractICS26Router.ContractICS26RouterWriteAcknowledgement{
		Sequence:         big.NewInt(9),
		Acknowledgements: [][]byte{[]byte("ack")},
		Packet: contractICS26Router.IICS26RouterMsgsPacket{
			SourceClient:     "eth-client-0",
			DestClient:       "cosmos-client-0",
			TimeoutTimestamp: 5678,
			Payloads: []contractICS26Router.IICS26RouterMsgsPayload{
				{
					SourcePort: "transfer",
					DestPort:   "transfer",
					Version:    "ics20-1",
					Encoding:   "proto3",
					Value:      []byte("payload"),
				},
			},
		},
		Raw: gethtypes.Log{BlockNumber: 99},
	}

	enqueueEthWriteAcknowledgement(bb, ev, nil)

	got := flushSingleEthPacket(t, bb)
	if got.Type != services.EthWriteAck {
		t.Fatalf("packet type = %v, want %v", got.Type, services.EthWriteAck)
	}
	if got.Packet.Sequence != 9 {
		t.Fatalf("packet sequence = %d, want 9", got.Packet.Sequence)
	}
	if got.BlockNumber != 99 {
		t.Fatalf("block number = %d, want 99", got.BlockNumber)
	}
	if len(got.AckBytes) != 1 || string(got.AckBytes[0]) != "ack" {
		t.Fatalf("ack bytes = %q, want [ack]", got.AckBytes)
	}
}

// The bug this pins, found in review: the ETH driver folded only the SendPacket
// branch's stats into `found` and discarded the WriteAcknowledgement branch's, so
// a pass that recovered acks still counted as quiet — and after 20 of them printed
// "found nothing" directly beside the line reporting what it found. Both branches
// now go through foundSomething(), and any future branch must too.
func TestEthRecoveryStats_FoundSomething(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats ethRecoveryStats
		want  bool
	}{
		{"nothing", ethRecoveryStats{}, false},
		{"recovered", ethRecoveryStats{recovered: 1}, true},
		{"skipped only", ethRecoveryStats{skipped: 1}, true},
		{"both", ethRecoveryStats{recovered: 2, skipped: 3}, true},
	} {
		if got := tc.stats.foundSomething(); got != tc.want {
			t.Errorf("%s: foundSomething() = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The aggregation itself: a quiet SendPacket pass must not mask a productive
	// WriteAcknowledgement one.
	found := false
	found = found || ethRecoveryStats{}.foundSomething()
	found = found || ethRecoveryStats{recovered: 1}.foundSomething()
	if !found {
		t.Fatal("a pass that recovered WriteAcknowledgements must not count as quiet")
	}
}

// TestEnqueueEthTerminal_RecordsBlockNumber pins that a settling event carries
// the block it was observed at. Both enqueues used to omit it, which is
// invisible today only because the source bridge drops these types before
// reading Height — a trap for the next change, not a live bug.
func TestEnqueueEthTerminal_RecordsBlockNumber(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		typ      services.EthPacketType
		ackBytes [][]byte
	}{
		{"ack", services.EthAck, [][]byte{[]byte("ack")}},
		{"timeout", services.EthTimeout, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bb := services.NewBatchBuilder()
			pkt := contractICS26Router.IICS26RouterMsgsPacket{
				SourceClient: "eth-client-0",
				DestClient:   "cosmos-client-0",
			}
			enqueueEthTerminal(bb, log.New(io.Discard, "", 0), tc.typ, pkt, big.NewInt(4), tc.ackBytes, 4242)

			ch := make(chan services.EthBatch, 1)
			bb.CheckEth(context.Background(), services.BatchConfig{BatchSize: 1}, ch)

			select {
			case batch := <-ch:
				if len(batch.Packets) != 1 {
					t.Fatalf("got %d packets, want 1", len(batch.Packets))
				}
				if got := batch.Packets[0].BlockNumber; got != 4242 {
					t.Fatalf("BlockNumber = %d, want 4242", got)
				}
				if got := batch.Packets[0].Type; got != tc.typ {
					t.Fatalf("Type = %v, want %v", got, tc.typ)
				}
			default:
				t.Fatal("no batch flushed")
			}
		})
	}
}

// TestEnqueueEthTerminal_SettlesThePendingTracker: a settling event must clear
// the tracker entry, or the timeout scanner keeps querying a packet that can
// never time out.
func TestEnqueueEthTerminal_SettlesThePendingTracker(t *testing.T) {
	t.Parallel()

	bb := services.NewBatchBuilder()
	pkt := contractICS26Router.IICS26RouterMsgsPacket{
		SourceClient: "eth-client-0",
		DestClient:   "cosmos-client-0",
	}
	cosmosPacket := EthPacketToCosmosPacket(pkt, big.NewInt(4))
	bb.EthPendingTracker.Add(cosmosPacket, 100)
	if bb.EthPendingTracker.Len() != 1 {
		t.Fatalf("tracker not seeded")
	}

	enqueueEthTerminal(bb, log.New(io.Discard, "", 0), services.EthAck, pkt, big.NewInt(4), [][]byte{[]byte("ack")}, 4242)

	if got := bb.EthPendingTracker.Len(); got != 0 {
		t.Fatalf("tracker length = %d after settlement, want 0", got)
	}
}

// --- dedupe keys and their retention window ---
//
// These three functions are the whole of the subscriber's "have I already handled
// this event" memory. They had no test. What makes them worth pinning is not the
// bookkeeping but the two opposite failures they sit between: a key that is too
// coarse suppresses an event that genuinely needs relaying, and a window that
// prunes too eagerly lets an already-relayed event come back around.

// Mirror of the Cosmos side. The two windows are different sizes (blocks vs
// heights, different chains) but the boundary rule must be the same, or one
// direction re-relays where the other does not.
func TestPruneEthSeenEvents_PrunesAtTheWindowBoundary(t *testing.T) {
	const current = ethSeenEventRetentionBlocks + 1_000
	minKept := current - ethSeenEventRetentionBlocks

	seen := map[ethEventKey]struct{}{
		{TxHash: "0x1", BlockNumber: minKept - 1}: {},
		{TxHash: "0x2", BlockNumber: minKept}:     {},
		{TxHash: "0x3", BlockNumber: current}:     {},
		{TxHash: "0x4", BlockNumber: 0}:           {},
	}
	pruneEthSeenEvents(seen, current)

	for _, want := range []struct {
		hash string
		b    uint64
		kept bool
	}{
		{"0x1", minKept - 1, false},
		{"0x2", minKept, true},
		{"0x3", current, true},
		{"0x4", 0, true},
	} {
		_, ok := seen[ethEventKey{TxHash: want.hash, BlockNumber: want.b}]
		if ok != want.kept {
			t.Errorf("block %d kept=%v, want %v", want.b, ok, want.kept)
		}
	}
}

// Mirror of the Cosmos young-chain case, and for the same reason: below the window
// the subtraction wraps and takes every entry with it.
func TestPruneEthSeenEvents_YoungChainKeepsEverything(t *testing.T) {
	const young = 10
	if young >= ethSeenEventRetentionBlocks {
		t.Fatalf("this test needs a block below the %d-block window", ethSeenEventRetentionBlocks)
	}
	seen := map[ethEventKey]struct{}{
		{TxHash: "0x1", BlockNumber: 1}:     {},
		{TxHash: "0x2", BlockNumber: young}: {},
	}
	pruneEthSeenEvents(seen, young)
	if len(seen) != 2 {
		t.Fatalf("pruned %d of 2 entries on a chain younger than the retention window", 2-len(seen))
	}
}

// --- gap-recovery range guards ---
//
// Both recovery scans refuse an empty or backwards range before dialing. The
// guard is what keeps a cursor that has run ahead of the head from turning into a
// backwards scan; without it the range is passed to the RPC as-is, and what comes
// back is either nothing (silent event loss, the failure this whole path exists to
// prevent) or an error on every recovery tick.

func TestRecoverEthWriteAcknowledgements_RefusesBackwardsRangeBeforeDialing(t *testing.T) {
	assertRefusesBackwardsRange(t, ethBackwardsRangeScans["write acknowledgements"])
}

func newRecoveryTestSubscriber(t *testing.T) (*Subscriber, *services.RecoveryStateStore, cosmosDeps) {
	t.Helper()
	store, err := services.LoadRecoveryState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	deps := cosmosDeps{IDs: services.ClientIDs{CosmosOnEVM: "source-a"}, Logger: log.New(io.Discard, "", 0)}
	return NewSubscriber(store), store, deps
}

func TestScanEthRangeInChunks(t *testing.T) {
	t.Run("persists only successful chunks", func(t *testing.T) {
		sub, store, deps := newRecoveryTestSubscriber(t)
		cursor := uint64(1)
		calls := 0
		persist := func() {
			if err := store.SaveCheckpoint(services.RecoveryCursors{"eth_send": cursor}); err != nil {
				t.Fatal(err)
			}
		}
		// "provider limit" is deliberately NOT a range rejection: it names no range, no
		// block window and no result-set size, so it stays an ordinary failure and the
		// scan stops at the chunk that failed rather than narrowing.
		wantErr := errors.New("provider limit")
		span := &relayerclient.LogSpan{Chunk: 4}
		_, err := sub.scanEthRangeInChunks(context.Background(), deps, "send", &cursor, 10, span,
			func(from, to uint64) (ethRecoveryStats, error) {
				calls++
				if calls == 2 {
					return ethRecoveryStats{}, wantErr
				}
				return ethRecoveryStats{recovered: 1}, nil
			}, persist)
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
		if cursor != 5 {
			t.Fatalf("cursor = %d, want first failed chunk start 5", cursor)
		}
		got, _ := store.Get()
		if got["eth_send"] != 5 {
			t.Fatalf("persisted cursor = %d, want 5", got["eth_send"])
		}
	})

	// The ETH mirror of the L2 narrowing branch, and the one place the two policies
	// differ on purpose: this side persists a cursor after every piece that
	// succeeded, so a narrowing must resume at that cursor rather than re-scan the
	// range. Re-scanning would re-offer events already handed to the batch builder.
	t.Run("narrows and resumes at the cursor", func(t *testing.T) {
		sub, store, deps := newRecoveryTestSubscriber(t)
		cursor := uint64(1)
		span := &relayerclient.LogSpan{Chunk: 8}
		persist := func() {
			if err := store.SaveCheckpoint(services.RecoveryCursors{"eth_send": cursor}); err != nil {
				t.Fatal(err)
			}
		}

		const cap = uint64(2)
		var asked [][2]uint64
		_, err := sub.scanEthRangeInChunks(context.Background(), deps, "send", &cursor, 16, span,
			func(from, to uint64) (ethRecoveryStats, error) {
				asked = append(asked, [2]uint64{from, to})
				// Bounded on purpose: a scan that does not narrow asks for the same
				// refused width forever, and an unbounded test reports that as a
				// wall-clock timeout rather than as the defect it is.
				if len(asked) > 64 {
					t.Fatalf("the scan asked 64 times without settling: %v", asked)
				}
				if to-from+1 > cap {
					return ethRecoveryStats{}, errors.New("query returned more than 10000 results")
				}
				return ethRecoveryStats{recovered: 1}, nil
			}, persist)
		if err != nil {
			t.Fatalf("scan never found a servable span: %v", err)
		}
		if span.Chunk > cap {
			t.Fatalf("span settled at %d, want <= %d", span.Chunk, cap)
		}
		if cursor != 17 {
			t.Fatalf("cursor = %d, want 17 (the range was not finished)", cursor)
		}
		// Every refused attempt must have started at the SAME block: narrowing changes
		// the width, never the position, and the cursor never moves on a failure.
		if asked[0][0] != 1 {
			t.Fatalf("first attempt started at %d, want 1", asked[0][0])
		}
		for i := 1; i < len(asked); i++ {
			prev, cur := asked[i-1], asked[i]
			if prev[1]-prev[0]+1 > cap && cur[0] != prev[0] {
				t.Fatalf("after refusing [%d,%d] the scan moved to [%d,%d]; a failed piece must be retried in place, not skipped",
					prev[0], prev[1], cur[0], cur[1])
			}
		}
		// The pieces that succeeded must tile the range with no gap — a gap here is a
		// silently missed packet, which is what this recovery scan exists to prevent.
		var next uint64 = 1
		for _, a := range asked {
			if a[1]-a[0]+1 > cap {
				continue // refused, contributed nothing
			}
			if a[0] != next {
				t.Fatalf("gap or overlap: expected a piece starting at %d, got [%d,%d]", next, a[0], a[1])
			}
			next = a[1] + 1
		}
		if next != 17 {
			t.Fatalf("scanned pieces ended at %d, want 17", next)
		}
	})

	// The bottom of the ladder on the ETH side. A one-block span still refused is not
	// a sizing problem; halving further is how the recovery scan would spin forever.
	t.Run("stops when the span cannot shrink further", func(t *testing.T) {
		sub, _, deps := newRecoveryTestSubscriber(t)
		cursor := uint64(1)
		span := &relayerclient.LogSpan{Chunk: 4}
		calls := 0
		_, err := sub.scanEthRangeInChunks(context.Background(), deps, "send", &cursor, 8, span,
			func(from, to uint64) (ethRecoveryStats, error) {
				calls++
				if calls > 32 {
					t.Fatal("the scan is halving forever instead of bottoming out")
				}
				return ethRecoveryStats{}, errors.New("block range too large")
			}, func() {})
		if err == nil {
			t.Fatal("a provider refusing every span was reported as a successful scan")
		}
		if !chain.IsPermanent(err) {
			t.Fatalf("error = %v, want permanent at the bottom of the ladder", err)
		}
		if cursor != 1 {
			t.Fatalf("cursor = %d, want 1 — it must never advance past a piece that failed", cursor)
		}
	})
}

func TestSubscribeEth_AlreadyCancelledDoesNotDialOrDereferenceClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewSubscriber().SubscribeEth(ctx, services.CosmosEndpoint{}, services.EVMEndpoint{WSURL: "ws://127.0.0.1:1"}, services.ClientIDs{}, nil, nil)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Ethereum subscriber dialled after cancellation")
	}
}

func TestSubscribeEth_WatchHonorsCancellation(t *testing.T) {
	var subscriptions atomic.Int32
	ready := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  string          `json:"method"`
			}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			var result any = true
			if req.Method == "eth_subscribe" {
				n := subscriptions.Add(1)
				result = fmt.Sprintf("0x%x", n)
				if n == 4 {
					close(ready)
				}
			}
			if err := conn.WriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  result,
			}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	watchClient, err := ethclient.Dial("ws" + strings.TrimPrefix(server.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(watchClient.Close)
	router := common.Address{}
	deps := ethDeps{
		EVM:    services.EVMEndpoint{Client: watchClient, Contracts: services.EVMContracts{Router: router}},
		Logger: log.New(io.Discard, "", 0),
	}
	recoveryFilterer, err := contractICS26Router.NewContractICS26RouterFilterer(router, watchClient)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewSubscriber().subscribeEthOnce(
			ctx, deps, services.NewBatchBuilder(), watchClient, recoveryFilterer,
			1, new(uint64), new(uint64), nil, new(uint64), &relayerclient.LogSpan{}, &blockRate{},
		)
	}()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Ethereum watch subscriptions did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("watch error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ethereum watch ignored cancellation")
	}
}

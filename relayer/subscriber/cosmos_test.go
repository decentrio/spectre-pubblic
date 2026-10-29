package subscriber

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/rpcmock"
	"relayer/services"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	abcitypes "github.com/cometbft/cometbft/abci/types"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	commettypes "github.com/cometbft/cometbft/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gogo/protobuf/proto"
)

func TestTxHeightFromEvent(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		data   commettypes.TMEventData
		events map[string][]string
		want   uint64
	}{
		{
			name:   "uses tx result height when available",
			data:   commettypes.EventDataTx{TxResult: abcitypes.TxResult{Height: 77}},
			events: map[string][]string{EVENT_TX_HEIGHT_FIELD: {"12"}},
			want:   77,
		},
		{
			name:   "falls back to tx height event field",
			events: map[string][]string{EVENT_TX_HEIGHT_FIELD: {"77"}},
			want:   77,
		},
		{
			name:   "missing tx height returns zero",
			events: map[string][]string{},
			want:   0,
		},
		{
			name:   "invalid tx height returns zero",
			events: map[string][]string{EVENT_TX_HEIGHT_FIELD: {"not-a-height"}},
			want:   0,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := txHeightFromEvent(tc.data, tc.events); got != tc.want {
				t.Fatalf("txHeightFromEvent(%v, %v) = %d, want %d", tc.data, tc.events, got, tc.want)
			}
		})
	}
}

func TestCosmosStartupRecoveryLookbackBlocksFromEnv(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		raw  string
		want uint64
	}{
		{name: "empty uses bounded default", raw: "", want: 256},
		{name: "invalid uses bounded default", raw: "not-a-number", want: 256},
		{name: "zero means full history", raw: "0", want: 0},
		{name: "custom bounded value", raw: "5000", want: 5000},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := cosmosStartupRecoveryLookbackBlocksFromEnv(tc.raw)
			if got != tc.want {
				t.Fatalf("cosmosStartupRecoveryLookbackBlocksFromEnv(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestCosmosStartupRecoveryStartHeight(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		latestHeight uint64
		lookback     uint64
		wantStart    uint64
	}{
		{name: "no latest height", latestHeight: 0, lookback: 0, wantStart: 0},
		{name: "zero lookback scans full history", latestHeight: 100, lookback: 0, wantStart: 1},
		{name: "lookback greater than latest scans from first block", latestHeight: 42, lookback: 100, wantStart: 1},
		{name: "bounded lookback is inclusive", latestHeight: 100, lookback: 10, wantStart: 91},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := cosmosStartupRecoveryStartHeight(tc.latestHeight, tc.lookback)
			if got != tc.wantStart {
				t.Fatalf("cosmosStartupRecoveryStartHeight(%d, %d) = %d, want %d",
					tc.latestHeight, tc.lookback, got, tc.wantStart)
			}
		})
	}
}

func TestCosmosTxSearchQuery(t *testing.T) {
	t.Parallel()

	got := cosmosTxSearchQuery(cometBFTSendPacketTxSearch, 0, 25)
	want := "send_packet.encoded_packet_hex EXISTS AND tx.height >= 1 AND tx.height <= 25"
	if got != want {
		t.Fatalf("cosmosTxSearchQuery = %q, want %q", got, want)
	}
}

func TestCosmosEventsFromTxResult(t *testing.T) {
	t.Parallel()

	tx := &coretypes.ResultTx{
		Height: 66,
		Index:  3,
		Tx:     []byte("tx-bytes"),
		TxResult: abcitypes.ExecTxResult{
			Events: []abcitypes.Event{
				{
					Type: "send_packet",
					Attributes: []abcitypes.EventAttribute{
						{Key: "encoded_packet_hex", Value: "abc"},
					},
				},
			},
		},
	}

	data, events := cosmosEventsFromTxResult(tx)
	if got := events[EVENT_TX_HEIGHT_FIELD]; len(got) != 1 || got[0] != "66" {
		t.Fatalf("tx.height event = %v, want [66]", got)
	}
	if got := events[EVENT_SEND_PACKET_FIELD]; len(got) != 1 || got[0] != "abc" {
		t.Fatalf("send packet event = %v, want [abc]", got)
	}
	if got := txHeightFromEvent(data, events); got != 66 {
		t.Fatalf("txHeightFromEvent = %d, want 66", got)
	}
}

func TestCosmosPacketMatchesConfiguredClient(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		ethClientID    string
		routerClientID string
		packet         *channeltypesv2.Packet
		want           bool
	}{
		{
			name: "nil packet does not match",
			want: false,
		},
		{
			name: "no configured IDs preserves legacy behavior",
			packet: &channeltypesv2.Packet{
				SourceClient:      "unrelated-src",
				DestinationClient: "unrelated-dst",
			},
			want: true,
		},
		{
			name:        "source matches eth client",
			ethClientID: "08-wasm-0",
			packet: &channeltypesv2.Packet{
				SourceClient:      "08-wasm-0",
				DestinationClient: "cosmoshub-1",
			},
			want: true,
		},
		{
			name:        "destination matches eth client",
			ethClientID: "08-wasm-0",
			packet: &channeltypesv2.Packet{
				SourceClient:      "cosmoshub-1",
				DestinationClient: "08-wasm-0",
			},
			want: true,
		},
		{
			name:           "source matches router client",
			routerClientID: "cosmoshub-1",
			packet: &channeltypesv2.Packet{
				SourceClient:      "cosmoshub-1",
				DestinationClient: "08-wasm-0",
			},
			want: true,
		},
		{
			name:           "destination matches router client",
			routerClientID: "cosmoshub-1",
			packet: &channeltypesv2.Packet{
				SourceClient:      "08-wasm-0",
				DestinationClient: "cosmoshub-1",
			},
			want: true,
		},
		{
			name:           "configured IDs skip unrelated packet",
			ethClientID:    "08-wasm-0",
			routerClientID: "cosmoshub-1",
			packet: &channeltypesv2.Packet{
				SourceClient:      "other-src",
				DestinationClient: "other-dst",
			},
			want: false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := cosmosDeps{IDs: services.ClientIDs{EVMOnCosmos: tc.ethClientID, CosmosOnEVM: tc.routerClientID}}

			if got := cosmosPacketMatchesConfiguredClient(ctx, tc.packet); got != tc.want {
				t.Fatalf("cosmosPacketMatchesConfiguredClient = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecodeCosmosPacketsFromEvents(t *testing.T) {
	t.Parallel()

	logger := log.New(io.Discard, "", 0)
	sendPacketHex := mustPacketHex(t, channeltypesv2.Packet{
		Sequence:          11,
		SourceClient:      "cosmos-client",
		DestinationClient: "eth-client",
		TimeoutTimestamp:  1_700_000_000_000_000_000,
	})
	ackPacketHex := mustPacketHex(t, channeltypesv2.Packet{
		Sequence:          12,
		SourceClient:      "eth-client",
		DestinationClient: "cosmos-client",
		TimeoutTimestamp:  1234,
	})
	timeoutPacketHex := mustPacketHex(t, channeltypesv2.Packet{
		Sequence:          13,
		SourceClient:      "eth-client",
		DestinationClient: "other-client",
	})
	settledPacketHex := mustPacketHex(t, channeltypesv2.Packet{
		Sequence:          14,
		SourceClient:      "cosmos-client",
		DestinationClient: "eth-client",
	})
	ackHex := mustAckHex(t, channeltypesv2.Acknowledgement{
		AppAcknowledgements: [][]byte{[]byte("ack")},
	})

	packets := decodeCosmosPacketsFromEvents(
		logger,
		commettypes.EventDataTx{TxResult: abcitypes.TxResult{Height: 55}},
		map[string][]string{
			EVENT_SEND_PACKET_FIELD:      {sendPacketHex},
			EVENT_WRITE_ACK_PACKET_FIELD: {ackPacketHex},
			EVENT_ACKNOWLEDGEMENT_FIELD:  {ackHex},
			EVENT_TIMEOUT_PACKET_FIELD:   {timeoutPacketHex},
			EVENT_ACK_PACKET_FIELD:       {settledPacketHex},
			EVENT_TX_HEIGHT_FIELD:        {"44"},
		},
		"test",
		"",
	)

	if len(packets) != 4 {
		t.Fatalf("decoded packet count = %d, want 4", len(packets))
	}
	if packets[0].Type != services.CosmosSend || packets[0].Packet.Sequence != 11 {
		t.Fatalf("packet[0] = type %v seq %d, want CosmosSend seq 11", packets[0].Type, packets[0].Packet.Sequence)
	}
	if packets[0].Packet.TimeoutTimestamp != 1_700_000_000 {
		t.Fatalf("normalized timeout = %d, want 1700000000", packets[0].Packet.TimeoutTimestamp)
	}
	if packets[1].Type != services.CosmosAck || packets[1].Packet.Sequence != 12 {
		t.Fatalf("packet[1] = type %v seq %d, want CosmosAck seq 12", packets[1].Type, packets[1].Packet.Sequence)
	}
	if len(packets[1].AckBytes) != 1 || string(packets[1].AckBytes[0]) != "ack" {
		t.Fatalf("ack bytes = %q, want [ack]", packets[1].AckBytes)
	}
	if packets[2].Type != services.CosmosTimeout || packets[2].Packet.Sequence != 13 {
		t.Fatalf("packet[2] = type %v seq %d, want CosmosTimeout seq 13", packets[2].Type, packets[2].Packet.Sequence)
	}
	// acknowledge_packet is the fourth event and the only terminal one on this
	// side: Cosmos consumed the ack for a packet it sent, so that packet is done.
	// Without it a packet another relayer acknowledged stays in the pending
	// tracker until a timeout scan happens to query for it.
	if packets[3].Type != services.CosmosAcknowledged || packets[3].Packet.Sequence != 14 {
		t.Fatalf("packet[3] = type %v seq %d, want CosmosAcknowledged seq 14", packets[3].Type, packets[3].Packet.Sequence)
	}
	for i, p := range packets {
		if p.BlockNumber != 55 {
			t.Fatalf("packet[%d] block = %d, want 55", i, p.BlockNumber)
		}
	}
}

func ethRecoveryRPCServer(t *testing.T, sendPacketLog gethtypes.Log) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var result any
		switch request.Method {
		case "eth_getLogs":
			result = []gethtypes.Log{sendPacketLog}
		case "eth_getStorageAt":
			// The pending commitment exists, so recovery reaches the durable Add.
			result = "0x01"
		case "abci_query":
			// No Cosmos receipt exists for the SendPacket yet.
			result = map[string]any{"response": map[string]any{"code": 0, "value": ""}}
		default:
			http.Error(w, "unexpected RPC method: "+request.Method, http.StatusNotImplemented)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  result,
		}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func ethRecoverySendPacketLog(t *testing.T, router common.Address) gethtypes.Log {
	t.Helper()
	parsed, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	event := parsed.Events["SendPacket"]
	packet := contractICS26Router.IICS26RouterMsgsPacket{
		SourceClient: "eth-client-0",
		DestClient:   "cosmos-client-0",
	}
	data, err := event.Inputs.NonIndexed().Pack(packet)
	if err != nil {
		t.Fatal(err)
	}
	return gethtypes.Log{
		Address: router,
		Topics: []common.Hash{
			event.ID,
			crypto.Keccak256Hash([]byte(packet.SourceClient)),
			common.BigToHash(big.NewInt(8)),
		},
		Data:        data,
		BlockNumber: 88,
		Index:       4,
	}
}

func flushSingleEthPacket(t *testing.T, bb *services.BatchBuilder) services.EthPacket {
	t.Helper()

	ch := make(chan services.EthBatch, 1)
	bb.CheckEth(context.Background(), services.BatchConfig{
		BatchSize:    1,
		BatchPeriods: time.Hour,
	}, ch)

	select {
	case batch := <-ch:
		if len(batch.Packets) != 1 {
			t.Fatalf("batch packet count = %d, want 1", len(batch.Packets))
		}
		return batch.Packets[0]
	default:
		t.Fatal("expected one flushed eth batch")
		return services.EthPacket{}
	}
}

func mustPacketHex(t *testing.T, packet channeltypesv2.Packet) string {
	t.Helper()
	bz, err := proto.Marshal(&packet)
	if err != nil {
		t.Fatalf("failed to marshal packet: %v", err)
	}
	return hex.EncodeToString(bz)
}

func mustAckHex(t *testing.T, ack channeltypesv2.Acknowledgement) string {
	t.Helper()
	bz, err := proto.Marshal(&ack)
	if err != nil {
		t.Fatalf("failed to marshal acknowledgement: %v", err)
	}
	return hex.EncodeToString(bz)
}

// The scan must never read up to the chain head. Status reports a height as soon
// as the block commits, but TxSearch reads the tx indexer, which CometBFT writes
// from a separate goroutine after commit — so the newest block is routinely
// committed and not yet indexed. Since the scan advances its cursor past
// whatever it read, a tx there is skipped permanently and silently.
func TestCosmosIndexedHeight_StopsShortOfTheHead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		latest uint64
		want   uint64
	}{
		{0, 0},
		{1, 0},
		{cosmosIndexerLagBlocks, 0},
		{cosmosIndexerLagBlocks + 1, 1},
		{202_197, 202_197 - cosmosIndexerLagBlocks},
	}
	for _, tc := range cases {
		got := cosmosIndexedHeight(tc.latest)
		if got != tc.want {
			t.Fatalf("cosmosIndexedHeight(%d) = %d, want %d", tc.latest, got, tc.want)
		}
	}

	// The property that actually protects packets: the range one pass scans must
	// never reach the head, at any cursor position. Asserted through
	// cosmosScanRange -- the function the scanner calls -- so removing the lag
	// from the scan fails here, which a test of cosmosIndexedHeight alone would
	// not.
	const latest = 202_197
	for cursor := uint64(1); cursor <= latest; cursor += 977 {
		from, to, ok := cosmosScanRange(cursor, latest)
		if !ok {
			continue
		}
		if to >= latest {
			t.Fatalf("cosmosScanRange(%d, %d) scans to %d, which reaches the head; "+
				"a tx in the newest block may be committed but not yet indexed, and the cursor "+
				"advances past it permanently", cursor, latest, to)
		}
		if from != cursor {
			t.Fatalf("cosmosScanRange(%d, %d) starts at %d; the cursor must not be skipped forward",
				cursor, latest, from)
		}
	}

	// Nothing scannable yet must be reported, not silently scanned as [x, 0].
	if _, _, ok := cosmosScanRange(0, latest); ok {
		t.Fatal("an unseeded cursor must not produce a scan range")
	}
	if _, _, ok := cosmosScanRange(latest, latest); ok {
		t.Fatal("a cursor at the head has nothing safely indexed to scan yet")
	}
}

// A live subscription that stops delivering is invisible by construction: it
// looks exactly like a quiet chain. These pin the two signals that tell them
// apart -- the chain still producing blocks, and how long the silence has run.
func TestLivePathStale(t *testing.T) {
	t.Parallel()

	// fresh returns a health tracker whose delivery clock starts at now, as
	// subscribeCosmosOnce sets it immediately after subscribing.
	fresh := func(now time.Time) *cosmosLiveHealth {
		h := &cosmosLiveHealth{}
		h.recordSubscribed(now)
		return h
	}
	start := time.Unix(1_700_000_000, 0)

	// An idle chain delivers nothing because there is nothing to deliver.
	// Reconnecting here would tear down a healthy subscription every few minutes.
	t.Run("a chain that is not advancing is never stale", func(t *testing.T) {
		h := fresh(start)
		stale, _, report := h.livePathStale(false, start.Add(24*time.Hour))
		if stale || report {
			t.Fatalf("stale=%v report=%v on an idle chain; silence proves nothing without block production", stale, report)
		}
	})

	t.Run("silence inside the window is not yet evidence", func(t *testing.T) {
		h := fresh(start)
		if stale, _, _ := h.livePathStale(true, start.Add(cosmosLivePathStaleAfter-time.Second)); stale {
			t.Fatal("reported a dead subscription while it was only briefly quiet")
		}
	})

	// The case the watchdog exists for. Note the clock runs from the SUBSCRIBE,
	// so a subscription that never delivered a single event is caught -- the old
	// version measured from the first live event and so had nothing to measure.
	t.Run("a subscription that never delivers is caught", func(t *testing.T) {
		h := fresh(start)
		stale, silence, report := h.livePathStale(true, start.Add(cosmosLivePathStaleAfter+time.Second))
		if !stale || !report {
			t.Fatalf("stale=%v report=%v; a subscription silent past the window while the chain advances must reconnect and say so", stale, report)
		}
		if silence < cosmosLivePathStaleAfter {
			t.Fatalf("silence = %s, want at least %s: the line has to carry the real number", silence, cosmosLivePathStaleAfter)
		}
	})

	// The half that was missing. A latched bool cleared only by a live event
	// means an outage that never ends produces exactly one line for the whole
	// life of the process -- and the log then reads as though it recovered.
	t.Run("a permanent outage keeps reporting on the ladder", func(t *testing.T) {
		h := fresh(start)
		now := start.Add(cosmosLivePathStaleAfter)
		if _, _, report := h.livePathStale(true, now); !report {
			t.Fatal("the first stale pass must report")
		}

		// Every 30s tick up to (not including) the five-minute step: same
		// threshold, so every one of them must be silent.
		staleSince := now
		for tick := 30 * time.Second; tick < 5*time.Minute; tick += 30 * time.Second {
			if _, _, report := h.livePathStale(true, staleSince.Add(tick)); report {
				t.Fatalf("repeated the warning %s into one threshold; that is 120 lines an hour", tick)
			}
		}

		// Crossing 5m, then 15m, then an hour each release exactly one more line.
		for _, step := range []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 2 * time.Hour} {
			if _, _, report := h.livePathStale(true, start.Add(cosmosLivePathStaleAfter+step)); !report {
				t.Fatalf("an outage still dead at %s said nothing; silence reads as recovery", step)
			}
		}
	})

	t.Run("a live event ends the outage and resets the ladder", func(t *testing.T) {
		h := fresh(start)
		h.livePathStale(true, start.Add(time.Hour)) // outage under way, thresholds burnt
		h.recordEvent()
		if !h.staleSince.IsZero() || h.reported != 0 {
			t.Fatalf("a delivered event must end the outage: staleSince=%v reported=%d", h.staleSince, h.reported)
		}
		if stale, _, _ := h.livePathStale(true, h.lastSeen.Add(time.Second)); stale {
			t.Fatal("a subscription that just delivered must not be torn down")
		}
		// And a NEW outage after it reports from the first step again.
		if _, _, report := h.livePathStale(true, h.lastSeen.Add(cosmosLivePathStaleAfter+time.Second)); !report {
			t.Fatal("a fresh outage must report again rather than inherit the old one's ladder")
		}
	})

	// Reconnecting is an attempt to fix the outage, not evidence it is over.
	// Resetting the ladder here would make a reconnect loop silent after one line.
	t.Run("resubscribing does not end the outage", func(t *testing.T) {
		h := fresh(start)
		h.livePathStale(true, start.Add(cosmosLivePathStaleAfter))
		h.recordSubscribed(start.Add(cosmosLivePathStaleAfter))
		if h.staleSince.IsZero() {
			t.Fatal("resubscribing cleared the outage; only a delivered event may do that")
		}
	})

	// The regression this fixes, stated as the arithmetic that produced it: a quiet
	// chain must never be judged stale, however long the silence runs.
	t.Run("ignores silence on a quiet chain", func(t *testing.T) {
		t.Parallel()

		start := time.Unix(1_700_000_000, 0)
		var h cosmosLiveHealth
		h.recordSubscribed(start)
		h.recordRecoveryPass(0, 0) // a scan that found nothing

		// Six hours of silence, the length of the run that exposed this.
		for _, after := range []time.Duration{
			cosmosLivePathStaleAfter, 10 * time.Minute, time.Hour, 6 * time.Hour,
		} {
			stale, _, report := h.livePathStale(h.hadTrafficToDeliver(), start.Add(after))
			if stale {
				t.Fatalf("silence for %s on a chain with no IBC traffic was judged stale", after)
			}
			if report {
				t.Fatalf("silence for %s on a quiet chain was reported", after)
			}
		}

		// One event in the window, still undelivered live, and the same silence is
		// now evidence: the subscription had something to deliver and did not.
		h.recordRecoveryPass(1, 0)
		stale, _, _ := h.livePathStale(h.hadTrafficToDeliver(), start.Add(cosmosLivePathStaleAfter))
		if !stale {
			t.Fatal("silence while the chain carried IBC traffic is exactly what must trigger a reconnect")
		}
	})
}

// IBC traffic, not block production, is what makes silence meaningful. Asking
// whether the chain advanced tore down a healthy subscription every
// cosmosLivePathStaleAfter on any Cosmos chain, because CometBFT produces blocks
// whether or not a packet moves.
func TestHadTrafficToDeliver(t *testing.T) {
	t.Parallel()

	var h cosmosLiveHealth
	if h.hadTrafficToDeliver() {
		t.Fatal("before any scan there is no evidence either way, so silence must prove nothing")
	}

	h.recordRecoveryPass(0, 0)
	if h.hadTrafficToDeliver() {
		t.Fatal("a scan that found no IBC event means there was nothing to deliver, not a dead subscription")
	}

	// skipped counts events recovery found that the live path had ALREADY
	// delivered. It is still proof the chain carried traffic.
	h.recordRecoveryPass(0, 1)
	if !h.hadTrafficToDeliver() {
		t.Fatal("an event the live path delivered is still traffic")
	}

	h.recordRecoveryPass(2, 0)
	if !h.hadTrafficToDeliver() {
		t.Fatal("an event the live path missed is the strongest evidence of traffic")
	}

	// The window does not latch: a burst followed by a quiet stretch must go
	// back to proving nothing, or one packet would arm the reconnect loop for
	// the life of the process.
	h.recordRecoveryPass(0, 0)
	if h.hadTrafficToDeliver() {
		t.Fatal("traffic seen once must not keep justifying reconnects after it stops")
	}
}

func TestAgeReportThreshold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		age  time.Duration
		want int
		why  string
	}{
		{0, 1, "the first stale pass always reports"},
		{4 * time.Minute, 1, "no new information yet"},
		{5 * time.Minute, 2, ""},
		{15 * time.Minute, 3, ""},
		{time.Hour, 4, ""},
		{3 * time.Hour, 6, "past the ladder it keeps counting hourly, so an overnight outage leaves a trail"},
	}
	for _, tc := range cases {
		if got := ageReportThreshold(tc.age); got != tc.want {
			t.Fatalf("ageReportThreshold(%s) = %d, want %d: %s", tc.age, got, tc.want, tc.why)
		}
	}
}

// Pruning bounds the memory of a process that runs for weeks. The boundary is what
// matters: prune one height too far and an event still inside the reorg window is
// forgotten, so a re-delivery is relayed a second time.
func TestPruneCosmosSeenEvents_PrunesAtTheWindowBoundary(t *testing.T) {
	const current = cosmosSeenEventRetentionHeights + 1_000
	minKept := current - cosmosSeenEventRetentionHeights

	seen := map[cosmosEventKey]struct{}{
		{Sequence: 1, Height: minKept - 1}: {}, // just outside the window
		{Sequence: 2, Height: minKept}:     {}, // exactly on the boundary
		{Sequence: 3, Height: minKept + 1}: {}, // inside
		{Sequence: 4, Height: current}:     {}, // current height
		{Sequence: 5, Height: 0}:           {}, // unknown height
	}
	pruneCosmosSeenEvents(seen, current)

	for _, want := range []struct {
		seq  uint64
		h    uint64
		kept bool
		why  string
	}{
		{1, minKept - 1, false, "outside the retention window"},
		{2, minKept, true, "exactly on the boundary — the window is inclusive"},
		{3, minKept + 1, true, "inside the window"},
		{4, current, true, "the current height"},
		{5, 0, true, "unknown height: no way to tell if it aged out, so it is kept"},
	} {
		_, ok := seen[cosmosEventKey{Sequence: want.seq, Height: want.h}]
		if ok != want.kept {
			t.Errorf("height %d kept=%v, want %v (%s)", want.h, ok, want.kept, want.why)
		}
	}
}

// Below the retention window there is nothing old enough to prune, and the
// subtraction must not be attempted: this is unsigned arithmetic, so an unguarded
// currentHeight - retention on a young chain wraps to an enormous minimum and
// deletes every entry — on a chain that has produced nothing old enough to forget.
//
// The height chosen here is well below the window, not on its boundary. At exactly
// currentHeight == retention the subtraction yields 0 and prunes nothing anyway,
// so a test there passes with or without the guard and proves nothing.
func TestPruneCosmosSeenEvents_YoungChainKeepsEverything(t *testing.T) {
	const young = 10 // vs a retention window of thousands of heights
	if young >= cosmosSeenEventRetentionHeights {
		t.Fatalf("this test needs a height below the %d-height window", cosmosSeenEventRetentionHeights)
	}
	seen := map[cosmosEventKey]struct{}{
		{Sequence: 1, Height: 1}:     {},
		{Sequence: 2, Height: young}: {},
	}
	pruneCosmosSeenEvents(seen, young)
	if len(seen) != 2 {
		t.Fatalf("pruned %d of 2 entries on a chain younger than the retention window", 2-len(seen))
	}
}

func TestRecoverCosmosEvents_RefusesEmptyRangeBeforeDialing(t *testing.T) {
	// Zero-value deps have no Cosmos client: reaching the RPC would panic rather
	// than return the empty stats asserted here.
	for _, tt := range []struct {
		name       string
		start, end uint64
	}{
		{"end below start", 200, 100},
		{"end is zero", 0, 0},
		{"end is zero with a real start", 100, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stats, err := recoverCosmosEvents(context.Background(), cosmosDeps{}, nil, tt.start, tt.end, nil)
			if err != nil {
				t.Fatalf("an empty range is not an error, got %v", err)
			}
			if stats.recovered != 0 || stats.skipped != 0 {
				t.Fatalf("reported work on an empty range: %+v", stats)
			}
		})
	}
}

// The ETH side scans SendPacket and WriteAcknowledgement in two independent
// functions, and each carries its own copy of the range guard. They get one test
// each, because "recoverEth" is not a unit -- there is no such function, and E9
// wants the prefix to name something real.
//
// The pair is what matters, though: a guard present on one scan and missing on
// the other is the one-sided asymmetry this repo keeps finding. Keeping them
// adjacent, over the same table, is what makes that visible. Change one, look at
// the other.
var ethBackwardsRangeScans = map[string]func(context.Context) (ethRecoveryStats, error){
	"send packets": func(ctx context.Context) (ethRecoveryStats, error) {
		return recoverEthSendPackets(ctx, ethDeps{}, nil, nil, 200, 100, nil)
	},
	"write acknowledgements": func(ctx context.Context) (ethRecoveryStats, error) {
		return recoverEthWriteAcknowledgements(ctx, ethDeps{}, nil, nil, 200, 100, nil)
	},
}

// assertRefusesBackwardsRange checks that the scan returns without dialing. A nil
// filterer means any attempt to scan would panic rather than return.
func assertRefusesBackwardsRange(t *testing.T, scan func(context.Context) (ethRecoveryStats, error)) {
	t.Helper()
	stats, err := scan(context.Background())
	if err != nil {
		t.Fatalf("a backwards range is not an error, got %v", err)
	}
	if stats.recovered != 0 {
		t.Fatalf("reported work on a backwards range: %+v", stats)
	}
}

// The dedupe key decides whether an event is treated as one already handled.
// Too coarse and a real event is suppressed; the reorg case below is where
// that costs a packet.
func TestCosmosEventKeyForPacket(t *testing.T) {
	// A reorg re-emits the same logical packet at a different height. The key must
	// treat that as a NEW event, or the second emission is silently dropped as a
	// duplicate and the packet is never relayed from the height that actually stuck.
	//
	// Everything else about the packet being equal, height alone must change the key.
	t.Run("height separates reorged events", func(t *testing.T) {
		packet := func(height uint64) services.CosmosPacket {
			return services.CosmosPacket{
				Type:        services.CosmosSend,
				BlockNumber: height,
				Packet: &channeltypesv2.Packet{
					Sequence:          9,
					SourceClient:      "cosmos-client-0",
					DestinationClient: "eth-router-0",
				},
			}
		}

		if cosmosEventKeyForPacket(packet(100)) != cosmosEventKeyForPacket(packet(100)) {
			t.Fatal("the same event at the same height must produce the same key")
		}
		if cosmosEventKeyForPacket(packet(100)) == cosmosEventKeyForPacket(packet(101)) {
			t.Fatal("a reorged event re-emitted at a new height was treated as a duplicate")
		}
	})

	// Each identifying field must participate in the key on its own. A key that
	// ignores, say, the packet type would let a WriteAcknowledgement be swallowed by
	// the SendPacket already seen for the same sequence.
	t.Run("every identifying field counts", func(t *testing.T) {
		base := services.CosmosPacket{
			Type:        services.CosmosSend,
			BlockNumber: 100,
			Packet: &channeltypesv2.Packet{
				Sequence:          9,
				SourceClient:      "cosmos-client-0",
				DestinationClient: "eth-router-0",
			},
		}

		tests := []struct {
			field  string
			mutate func(p *services.CosmosPacket)
		}{
			{"packet type", func(p *services.CosmosPacket) { p.Type = services.CosmosAck }},
			{"sequence", func(p *services.CosmosPacket) { p.Packet.Sequence = 10 }},
			{"source client", func(p *services.CosmosPacket) { p.Packet.SourceClient = "cosmos-client-1" }},
			{"destination client", func(p *services.CosmosPacket) { p.Packet.DestinationClient = "eth-router-1" }},
			{"height", func(p *services.CosmosPacket) { p.BlockNumber = 101 }},
		}
		for _, tt := range tests {
			t.Run(tt.field, func(t *testing.T) {
				other := base
				inner := *base.Packet
				other.Packet = &inner
				tt.mutate(&other)
				if cosmosEventKeyForPacket(base) == cosmosEventKeyForPacket(other) {
					t.Fatalf("%s does not affect the dedupe key; two distinct events collide", tt.field)
				}
			})
		}
	})

	// A packet that failed to decode carries no identity. The key must still be
	// well-formed rather than panicking — the caller records it like any other.
	t.Run("handles a packet that did not decode", func(t *testing.T) {
		key := cosmosEventKeyForPacket(services.CosmosPacket{Type: services.CosmosSend, BlockNumber: 5})
		if key.Height != 5 || key.Sequence != 0 || key.SourceClient != "" {
			t.Fatalf("nil packet produced %+v, want only the height filled in", key)
		}
	})
}

// --- the live enqueue decision ---
//
// enqueueCosmosPackets decides which of the packets decoded out of a Cosmos event
// actually reach the relay queue. Everything it drops is dropped SILENTLY as far
// as the packet is concerned -- there is no retry behind it, so a packet wrongly
// filtered here is simply never relayed.

// cosmosTestDeps returns deps with the two client ids set and a quiet logger, so
// a test asserts on the queue rather than on log output.
func cosmosTestDeps(ethOnCosmos, cosmosOnEVM string) cosmosDeps {
	return cosmosDeps{
		IDs:    services.ClientIDs{EVMOnCosmos: ethOnCosmos, CosmosOnEVM: cosmosOnEVM},
		Logger: log.New(io.Discard, "", 0),
	}
}

func cosmosTestPacket(seq uint64, src, dst string) services.CosmosPacket {
	return services.CosmosPacket{
		Type:        services.CosmosSend,
		BlockNumber: 100,
		Packet:      &channeltypesv2.Packet{Sequence: seq, SourceClient: src, DestinationClient: dst},
	}
}

func TestEnqueueCosmosPackets(t *testing.T) {
	const (
		ethOnCosmos = "08-wasm-0"
		cosmosOnEVM = "eth-router-0"
	)

	t.Run("enqueues a packet on a configured client", func(t *testing.T) {
		bb := services.NewBatchBuilder()
		seen := map[cosmosEventKey]struct{}{}

		stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{cosmosTestPacket(1, "cosmos-client", cosmosOnEVM)}, seen, false)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if stats.recovered != 1 || stats.skipped != 0 {
			t.Fatalf("stats = %+v, want recovered=1 skipped=0", stats)
		}
		if cosmos, _ := bb.QueueDepths(); cosmos != 1 {
			t.Fatalf("queued %d packets, want 1", cosmos)
		}
	})

	// One pass can carry BOTH a send and the acknowledge_packet that closes it --
	// ordinary during recovery, where the window is sized to cover the downtime.
	// Settling only the tracker leaves the send QUEUED, so it is relayed for
	// nothing and, worse, tracked again by that relay: after settling removed it,
	// and after the cursor moved past the terminal event that would clear it a
	// second time. The entry then stays pending for the life of the process.
	t.Run("a send settled later in the same pass is removed from the queue too", func(t *testing.T) {
		bb := services.NewBatchBuilder()
		send := cosmosTestPacket(7, "cosmos-client", cosmosOnEVM)
		settled := cosmosTestPacket(7, "cosmos-client", cosmosOnEVM)
		settled.Type = services.CosmosAcknowledged

		if _, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{send, settled}, map[cosmosEventKey]struct{}{}, false); err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		if cosmos, _ := bb.QueueDepths(); cosmos != 0 {
			t.Fatalf("queued %d packet(s); the send settled in this very pass and must not be relayed", cosmos)
		}
		if got := bb.PendingTracker.Len(); got != 0 {
			t.Fatalf("pending tracker holds %d packet(s) after the settlement", got)
		}
	})

	// The multi-relayer case. This process delivered the receive and recorded an
	// owed acknowledgement; ANOTHER process submitted the acknowledgement, so this
	// one only ever sees the terminal event. Reported by @DongLieu: the branch
	// settled the pending tracker and stopped, leaving the debt durable, surviving
	// restart, and reported overdue for a packet already settled on-chain -- which
	// can never time out either, because the receive left a receipt.
	t.Run("a terminal acknowledge_packet closes the owed-acknowledgement record too", func(t *testing.T) {
		bb := services.NewBatchBuilder()
		var settledOwed []uint64
		bb.WithOwedAckSettler(func(p channeltypesv2.Packet) {
			settledOwed = append(settledOwed, p.Sequence)
		})

		packet := cosmosTestPacket(9, "cosmos-client", cosmosOnEVM)
		packet.Type = services.CosmosAcknowledged
		bb.PendingTracker.Add(*packet.Packet, 40)

		if _, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{packet}, map[cosmosEventKey]struct{}{}, false); err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		if len(settledOwed) != 1 || settledOwed[0] != 9 {
			t.Fatalf("owed acknowledgements settled = %v, want [9]: the acknowledgement arrived, "+
				"so the debt this process recorded is closed whoever relayed it", settledOwed)
		}
	})

	// The rule that makes terminal events safe to read at all: they close a
	// packet's lifecycle, so they settle the tracker and must NEVER reach the
	// batch. A terminal event in the relay queue is an empty "relay" of a packet
	// with nothing left to do -- it would be proven, submitted, and rejected.
	t.Run("a terminal acknowledge_packet settles the tracker and never reaches the batch", func(t *testing.T) {
		bb := services.NewBatchBuilder()
		packet := cosmosTestPacket(1, "cosmos-client", cosmosOnEVM)
		packet.Type = services.CosmosAcknowledged
		bb.PendingTracker.Add(*packet.Packet, 40)
		if bb.PendingTracker.Len() != 1 {
			t.Fatal("the packet must be tracked before the terminal event arrives")
		}

		stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{packet}, map[cosmosEventKey]struct{}{}, false)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if cosmos, _ := bb.QueueDepths(); cosmos != 0 {
			t.Fatalf("queued %d terminal event(s); a settled packet has no relay work left", cosmos)
		}
		if got := bb.PendingTracker.Len(); got != 0 {
			t.Fatalf("pending tracker holds %d packet(s) after the settlement; the whole point is dropping it without a query", got)
		}
		if stats.recovered != 1 {
			t.Fatalf("stats = %+v: a settlement is progress, not a skip", stats)
		}
	})

	t.Run("drops a packet belonging to another client pair", func(t *testing.T) {
		// A Cosmos chain relays for more than one counterparty. Enqueuing another
		// pair's packet does not merely waste a proof: it is submitted to a router
		// that has no commitment for it, and fails permanently.
		bb := services.NewBatchBuilder()
		stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{cosmosTestPacket(1, "other-src", "other-dst")},
			map[cosmosEventKey]struct{}{}, false)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if stats.skipped != 1 || stats.recovered != 0 {
			t.Fatalf("stats = %+v, want skipped=1 recovered=0", stats)
		}
		if cosmos, _ := bb.QueueDepths(); cosmos != 0 {
			t.Fatalf("queued %d packets from an unrelated client pair", cosmos)
		}
	})

	t.Run("matches on either side of either configured client", func(t *testing.T) {
		// The four ways a packet can belong to this relayer's path. Missing one
		// silently drops a whole direction -- an ack travels with the clients
		// swapped relative to the send it answers.
		for name, packet := range map[string]services.CosmosPacket{
			"source is the eth client":         cosmosTestPacket(1, ethOnCosmos, "x"),
			"destination is the eth client":    cosmosTestPacket(2, "x", ethOnCosmos),
			"source is the router client":      cosmosTestPacket(3, cosmosOnEVM, "x"),
			"destination is the router client": cosmosTestPacket(4, "x", cosmosOnEVM),
		} {
			t.Run(name, func(t *testing.T) {
				bb := services.NewBatchBuilder()
				stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
					[]services.CosmosPacket{packet}, map[cosmosEventKey]struct{}{}, false)
				if err != nil || stats.recovered != 1 {
					t.Fatalf("stats = %+v, err = %v; want recovered=1", stats, err)
				}
			})
		}
	})

	t.Run("drops a packet that did not decode", func(t *testing.T) {
		bb := services.NewBatchBuilder()
		stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{{Type: services.CosmosSend}}, map[cosmosEventKey]struct{}{}, false)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if stats.skipped != 1 {
			t.Fatalf("stats = %+v, want skipped=1", stats)
		}
	})

	t.Run("enqueues a packet once across repeated deliveries", func(t *testing.T) {
		// The live subscription and the gap recovery overlap by design, so the
		// same packet arrives twice. Relaying it twice costs a proof and a tx for
		// a submission the destination rejects.
		bb := services.NewBatchBuilder()
		seen := map[cosmosEventKey]struct{}{}
		packet := cosmosTestPacket(1, "cosmos-client", cosmosOnEVM)

		for i := 0; i < 3; i++ {
			if _, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
				[]services.CosmosPacket{packet}, seen, false); err != nil {
				t.Fatalf("delivery %d: %v", i, err)
			}
		}
		if cosmos, _ := bb.QueueDepths(); cosmos != 1 {
			t.Fatalf("queued %d copies of one packet", cosmos)
		}
	})

	t.Run("an enqueued packet is recorded, which is what the dedupe above rests on", func(t *testing.T) {
		seen := map[cosmosEventKey]struct{}{}
		packet := cosmosTestPacket(1, "cosmos-client", cosmosOnEVM)
		bb := services.NewBatchBuilder()

		if _, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{packet}, seen, false); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if _, ok := seen[cosmosEventKeyForPacket(packet)]; !ok {
			t.Fatal("an enqueued packet was not recorded in the seen set")
		}
	})

	// Which drops are remembered is not uniform, and the difference is the cost of
	// the decision that produced them. Both branches below were untested; the
	// subtest above used to carry the name of the first one while exercising
	// neither, which review caught.

	t.Run("a packet declined during recovery is marked seen", func(t *testing.T) {
		// This is the drop worth remembering: deciding it cost a round trip to the
		// counterparty (HasEthPacketReceipt / HasPendingEthPacketCommitment), and a
		// recovery pass re-reads the same range every time. A Cosmos-originated
		// timeout reaches the same branch without an RPC -- it is declined by
		// ShouldRelayCosmosTimeoutToEth, which is pure -- so the branch is driven
		// here with no server standing in for the chain.
		seen := map[cosmosEventKey]struct{}{}
		bb := services.NewBatchBuilder()
		packet := cosmosTestPacket(1, "cosmos-client", cosmosOnEVM)
		packet.Type = services.CosmosTimeout

		stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{packet}, seen, true)
		if err != nil {
			t.Fatalf("recovery enqueue: %v", err)
		}
		if stats.recovered != 0 || stats.skipped != 1 {
			t.Fatalf("stats = %+v; want the packet declined, not enqueued", stats)
		}
		if cosmos, _ := bb.QueueDepths(); cosmos != 0 {
			t.Fatalf("a declined packet was queued anyway (depth %d)", cosmos)
		}
		if _, ok := seen[cosmosEventKeyForPacket(packet)]; !ok {
			t.Fatal("a packet declined during recovery was not marked seen, so every later " +
				"recovery pass pays for the same decision again")
		}
	})

	t.Run("an unrelated packet is NOT marked seen", func(t *testing.T) {
		// The opposite of the branch above, and deliberately so. Rejecting an
		// unrelated client is a field comparison: stateless, free, and identical
		// on every pass. Caching it would buy nothing and would grow the map with
		// packets this relayer will never relay -- unbounded in the number of
		// other clients on the chain, not in its own traffic.
		seen := map[cosmosEventKey]struct{}{}
		bb := services.NewBatchBuilder()
		packet := cosmosTestPacket(1, "somebody-else", "not-our-router")

		stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps(ethOnCosmos, cosmosOnEVM), bb,
			[]services.CosmosPacket{packet}, seen, false)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if stats.recovered != 0 || stats.skipped != 1 {
			t.Fatalf("stats = %+v; want the packet skipped as unrelated", stats)
		}
		if _, ok := seen[cosmosEventKeyForPacket(packet)]; ok {
			t.Fatal("an unrelated packet was cached in the seen set; that map then grows with " +
				"other clients' traffic rather than this relayer's")
		}
	})

	t.Run("with no client ids configured every packet matches", func(t *testing.T) {
		// The permissive default: an unconfigured relayer relays whatever it sees
		// rather than silently nothing, which is the failure that looks like a
		// dead relayer with a clean log.
		bb := services.NewBatchBuilder()
		stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps("", ""), bb,
			[]services.CosmosPacket{cosmosTestPacket(1, "anything", "at-all")},
			map[cosmosEventKey]struct{}{}, false)
		if err != nil || stats.recovered != 1 {
			t.Fatalf("stats = %+v, err = %v; want recovered=1", stats, err)
		}
	})
}

// --- the live event handler ---

// failingCosmosDeps returns deps whose Cosmos RPC answers every method with an
// error, so a gap recovery triggered from the live path fails rather than
// panicking on a nil client.
func failingCosmosDeps(t *testing.T, ethOnCosmos, cosmosOnEVM string) cosmosDeps {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) +
			`,"error":{"code":-32603,"message":"node is catching up"}}`))
	}))
	t.Cleanup(server.Close)

	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatalf("create CometBFT client: %v", err)
	}
	deps := cosmosTestDeps(ethOnCosmos, cosmosOnEVM)
	deps.Cosmos = services.CosmosEndpoint{Client: client}
	return deps
}

// liveEventAtHeight builds the ResultEvent shape the CometBFT subscription
// delivers, carrying only the tx height -- which is all the cursor logic reads.
func liveEventAtHeight(height int64) coretypes.ResultEvent {
	return coretypes.ResultEvent{
		Data:   commettypes.EventDataTx{TxResult: abcitypes.TxResult{Height: height}},
		Events: map[string][]string{},
	}
}

func TestProcessLiveCosmosEvent(t *testing.T) {
	// The recovery cursor is what bounds the window a restart re-scans. Advancing it
	// past a gap the relayer did NOT successfully re-scan means those heights are
	// never looked at again: the events in them are lost silently, which is the exact
	// failure the gap recovery exists to prevent.
	//
	// This is a named failure mode for this repo -- never advance a cursor on a failed
	// operation -- and the live path is where it is easiest to get wrong, because the
	// success and failure branches sit either side of one `else`.
	t.Run("cursor survives a failed gap recovery", func(t *testing.T) {
		const (
			cursorBefore = uint64(100)
			liveHeight   = int64(150)
		)
		deps := failingCosmosDeps(t, "08-wasm-0", "eth-router-0")
		s := NewSubscriber()
		cursor := cursorBefore
		bb := services.NewBatchBuilder()

		s.processLiveCosmosEvent(context.Background(), deps, bb, &cursor,
			map[cosmosEventKey]struct{}{}, &cosmosLiveHealth{}, liveEventAtHeight(liveHeight))

		if cursor != cursorBefore {
			t.Fatalf("cursor advanced to %d after a failed gap recovery; heights %d-%d would never be re-scanned",
				cursor, cursorBefore, liveHeight-1)
		}
	})

	// The mirror of the above: when the gap scan succeeds the cursor MUST move, or
	// every later event re-scans the same range and the relayer spends its time
	// re-reading history instead of relaying.
	//
	// The gap here is real -- heights 100 to 149 are scanned against a node that
	// answers -- so this exercises the success branch rather than the guard that
	// skips an empty range.
	t.Run("cursor advances on a successful gap recovery", func(t *testing.T) {
		const (
			cursorBefore = uint64(100)
			liveHeight   = int64(150)
		)
		deps, queries := emptyTxSearchCosmosDeps(t, "08-wasm-0", "eth-router-0")
		s := NewSubscriber()
		cursor := cursorBefore

		s.processLiveCosmosEvent(context.Background(), deps, services.NewBatchBuilder(), &cursor,
			map[cosmosEventKey]struct{}{}, &cosmosLiveHealth{}, liveEventAtHeight(liveHeight))

		if cursor != uint64(liveHeight) {
			t.Fatalf("cursor = %d after a successful scan of %d-%d, want %d; a cursor that "+
				"does not move makes every later event re-scan the same range",
				cursor, cursorBefore, liveHeight-1, liveHeight)
		}

		// The gap ENDS one below the live height. Including the live height would
		// re-scan the very event being processed here, enqueuing it twice -- once from
		// the live path and once from the recovery beside it.
		asked := queries.all()
		if len(asked) == 0 {
			t.Fatal("no tx_search was issued, so no gap was scanned")
		}
		wantRange := fmt.Sprintf("tx.height >= %d AND tx.height <= %d", cursorBefore, liveHeight-1)
		for _, q := range asked {
			if !strings.Contains(q, wantRange) {
				t.Fatalf("scanned %q, want the range %q", q, wantRange)
			}
		}
	})

	// A live event must count towards liveness even when the gap recovery beside it
	// fails. The health signal answers "is the subscription still delivering", and
	// conflating it with "is recovery healthy" makes a working subscription look dead
	// and triggers a reconnect that loses in-flight events.
	t.Run("records liveness despite recovery failure", func(t *testing.T) {
		deps := failingCosmosDeps(t, "08-wasm-0", "eth-router-0")
		s := NewSubscriber()
		cursor := uint64(100)
		health := &cosmosLiveHealth{}

		s.processLiveCosmosEvent(context.Background(), deps, services.NewBatchBuilder(), &cursor,
			map[cosmosEventKey]struct{}{}, health, liveEventAtHeight(150))

		if health.events != 1 {
			t.Fatalf("live health recorded %d events, want 1", health.events)
		}
		if health.lastSeen.IsZero() {
			t.Fatal("live health did not record when the event arrived")
		}
	})

	// An event with no height still has to be processed: the packets in it are real.
	// What must not happen is a prune keyed on height zero, which would compute a
	// retention floor from nothing.
	t.Run("heightless event does not prune", func(t *testing.T) {
		deps := cosmosTestDeps("08-wasm-0", "eth-router-0")
		s := NewSubscriber()
		cursor := uint64(100)
		seen := map[cosmosEventKey]struct{}{
			{Sequence: 1, Height: 1}: {}, // far below any plausible retention floor
		}

		s.processLiveCosmosEvent(context.Background(), deps, services.NewBatchBuilder(), &cursor,
			seen, &cosmosLiveHealth{}, coretypes.ResultEvent{Events: map[string][]string{}})

		if len(seen) != 1 {
			t.Fatalf("an event with no height pruned the seen set (%d entries left)", len(seen))
		}
		if cursor != 100 {
			t.Fatalf("cursor moved to %d on an event with no height", cursor)
		}
	})
}

// recordedQueries collects the tx_search queries a stub node was asked, so a test
// can assert on the height range the relayer computed rather than only on what
// the stub chose to answer.
type recordedQueries struct {
	mu      sync.Mutex
	queries []string
}

func (r *recordedQueries) add(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, q)
}

func (r *recordedQueries) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

// emptyTxSearchCosmosDeps answers tx_search with a valid, empty result -- what a
// node returns for a range containing no IBC transactions, which is the ordinary
// case for most heights. It records every query it was asked.
func emptyTxSearchCosmosDeps(t *testing.T, ethOnCosmos, cosmosOnEVM string) (cosmosDeps, *recordedQueries) {
	t.Helper()
	seen := &recordedQueries{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Query string `json:"query"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method != "tx_search" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) +
				`,"error":{"code":-32601,"message":"unexpected method ` + req.Method + `"}}`))
			return
		}
		seen.add(req.Params.Query)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) +
			`,"result":{"txs":[],"total_count":"0"}}`))
	}))
	t.Cleanup(server.Close)

	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatalf("create CometBFT client: %v", err)
	}
	deps := cosmosTestDeps(ethOnCosmos, cosmosOnEVM)
	deps.Cosmos = services.CosmosEndpoint{Client: client}
	return deps, seen
}

func TestCosmosLiveHealth(t *testing.T) {
	// TestCosmosLiveHealth/scope is the subscription not the client pins the invariant the review
	// round turned on, because reading the code two people reached opposite answers.
	//
	// The finding was that stats.skipped counts packets dropped as unrelated-client,
	// so traffic for someone else's client arms the stale check. That half is true.
	// What makes it harmless is the other half: the live subscriptions carry NO
	// client filter, and processLiveCosmosEvent calls recordEvent BEFORE
	// enqueueCosmosPackets filters anything. So the same unrelated event that
	// inflates skipped also refreshes lastSeen, and the check cannot fire on it.
	//
	// The two sides must therefore stay scoped together. Narrowing skipped to this
	// relayer's client without narrowing recordEvent the same way would arm the
	// guard with traffic that can never clear it -- a healthy subscription torn down
	// every cosmosLivePathStaleAfter, which is the bug this PR exists to remove.
	t.Run("scope is the subscription not the client", func(t *testing.T) {
		const (
			ours   = "08-wasm-7"
			theirs = "08-wasm-OTHER"
		)
		foreign := channeltypesv2.Packet{Sequence: 3, SourceClient: theirs, DestinationClient: "someone-else"}

		t.Run("an unrelated client's live event still counts as delivery", func(t *testing.T) {
			var s Subscriber
			health := &cosmosLiveHealth{}
			cursor := uint64(0)

			s.processLiveCosmosEvent(context.Background(), cosmosTestDeps("08-wasm-0", ours),
				services.NewBatchBuilder(), &cursor, map[cosmosEventKey]struct{}{}, health,
				coretypes.ResultEvent{
					Data: commettypes.EventDataTx{TxResult: abcitypes.TxResult{Height: 10}},
					Events: map[string][]string{
						"send_packet.encoded_packet_hex": {mustPacketHex(t, foreign)},
					},
				})

			if health.events != 1 {
				t.Fatalf("events = %d, want 1: recordEvent runs before any client filter, so this refreshes lastSeen", health.events)
			}
			if health.lastSeen.IsZero() {
				t.Fatal("lastSeen was not refreshed; an unrelated event would then arm the stale check without ever clearing it")
			}
		})

		t.Run("and the same event is what skipped counts", func(t *testing.T) {
			stats, err := enqueueCosmosPackets(context.Background(), cosmosTestDeps("08-wasm-0", ours),
				services.NewBatchBuilder(),
				[]services.CosmosPacket{{Type: services.CosmosSend, BlockNumber: 10, Packet: &foreign}},
				map[cosmosEventKey]struct{}{}, false)
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if stats.skipped != 1 || stats.recovered != 0 {
				t.Fatalf("stats = %+v, want skipped=1: an unrelated client is dropped here, and that is what arms sawTraffic", stats)
			}
		})
	})

	// Evidence from before this subscription existed is not evidence about it. The
	// startup lookback and the gap after a reconnect both cover events no live
	// subscription was running to deliver; counting them arms the stale check
	// against a subscription that has had nothing to do yet, and the teardown that
	// follows re-seeds exactly the same gap.
	t.Run("traffic before this subscription is not evidence", func(t *testing.T) {
		h := &cosmosLiveHealth{}

		// PRODUCTION ORDER, which the first version of this test had backwards: the
		// subscription is established first (cosmos.go:279) and the startup recovery
		// runs immediately after (cosmos.go:281). Asserting the clear in the other
		// order proved something that never happens, and missed the ordering that
		// does -- found in review by @DongLieu.
		h.recordSubscribed(time.Now())
		if h.hadTrafficToDeliver() {
			t.Fatal("a fresh subscription starts with no traffic verdict")
		}

		// The startup pass scans the historical lookback and finds old events. It must
		// not write them to the signal; the gate that stops it is the armsWatchdog
		// parameter, exercised directly below.
		if h.hadTrafficToDeliver() {
			t.Fatal("startup lookback traffic armed the watchdog for a subscription that " +
				"has not been asked to carry anything yet")
		}

		stale, _, _ := h.livePathStale(h.hadTrafficToDeliver(), time.Now().Add(2*cosmosLivePathStaleAfter))
		if stale {
			t.Fatal("a brand-new subscription over a quiet window must not be judged dead")
		}
	})

	// P1, found in review: a startup scan that FAILS leaves the cursor inside the
	// pre-subscription lookback, because subscribeCosmosOnce logs the error and
	// starts the loop anyway. The next tick then re-reads that historical range. If
	// arming depends on which caller is asking, that tick arms the watchdog with
	// events produced before the subscription existed -- and two minutes of
	// legitimate quiet tears down a healthy subscription, the false positive this
	// file exists to remove.
	t.Run("a failed startup scan does not let history arm the watchdog", func(t *testing.T) {
		const (
			head   = int64(200)
			cursor = uint64(100) // inside the lookback, below the boundary
			ours   = "08-wasm-7"
		)
		var attempt int
		deps := rangedTxSearchCosmosDeps(t, head, ours, func(from, to uint64) (int, bool) {
			attempt++
			if attempt <= 3 { // the startup pass issues three queries; fail them all
				return 0, true
			}
			return 1, false // the tick retries the same historical range, and finds an old event
		})

		var s Subscriber
		health := &cosmosLiveHealth{}
		health.recordSubscribed(time.Now())
		c := cursor
		var quiet uint64

		// startup pass: fails, so the cursor cannot move off the lookback
		if _, err := s.recoverCosmosGapToLatest(context.Background(), deps, services.NewBatchBuilder(),
			&c, map[cosmosEventKey]struct{}{}, &quiet, health, &blockRate{}); err == nil {
			t.Fatal("startup scan was expected to fail; the test would not reach the case it covers")
		}
		if c != cursor {
			t.Fatalf("cursor moved to %d after a failed startup scan; this test needs it to stay at %d", c, cursor)
		}
		if health.armFromHeight != uint64(head) {
			t.Fatalf("armFromHeight = %d, want %d: the boundary is the head read when the subscription came up",
				health.armFromHeight, head)
		}

		// the tick re-reads the very same historical range, this time successfully
		if _, err := s.recoverCosmosGapToLatest(context.Background(), deps, services.NewBatchBuilder(),
			&c, map[cosmosEventKey]struct{}{}, &quiet, health, &blockRate{}); err != nil {
			t.Fatalf("periodic pass: %v", err)
		}
		if health.hadTrafficToDeliver() {
			t.Fatal("a tick that re-read the pre-subscription lookback armed the watchdog: " +
				"two minutes of quiet now tears down a subscription that has carried nothing")
		}
	})

	// A reconnect gets its own boundary. Found by mutation: recordSubscribed cleared
	// sawTraffic but leaving armFromHeight alone still passed everything. The stale
	// boundary then belongs to the PREVIOUS subscription, so the heights produced
	// while the connection was down -- the gap this reconnect exists to close, and
	// which no live subscription was running to deliver -- sit above it and arm the
	// watchdog for a subscription that has carried nothing. The same false positive
	// as the startup case, one reconnect later.
	t.Run("a reconnect does not inherit the previous boundary", func(t *testing.T) {
		const ours = "08-wasm-7"
		deps := rangedTxSearchCosmosDeps(t, 400, ours, func(from, to uint64) (int, bool) {
			return 1, false // there is traffic in the gap, as there would be
		})

		var s Subscriber
		health := &cosmosLiveHealth{}
		health.armFromHeight = 100 // the subscription that just died came up at 100

		// reconnecting: this subscription is new, and so is its boundary
		health.recordSubscribed(time.Now())
		if health.armFromHeight != 0 {
			t.Fatalf("armFromHeight = %d after resubscribing, want 0: the previous "+
				"subscription's boundary says nothing about this one", health.armFromHeight)
		}

		c := uint64(300) // the gap left behind by the outage
		var quiet uint64
		if _, err := s.recoverCosmosGapToLatest(context.Background(), deps, services.NewBatchBuilder(),
			&c, map[cosmosEventKey]struct{}{}, &quiet, health, &blockRate{}); err != nil {
			t.Fatalf("recovery pass: %v", err)
		}
		if health.hadTrafficToDeliver() {
			t.Fatal("traffic from the outage gap armed the reconnected subscription: " +
				"it is being held responsible for events it was not running to receive")
		}
	})

	// The mirror, so the test above cannot pass by never arming at all: an event at
	// a height produced while the subscription was up IS evidence the live path
	// should have delivered something.
	t.Run("evidence above the subscription boundary arms the watchdog", func(t *testing.T) {
		const ours = "08-wasm-7"
		deps := rangedTxSearchCosmosDeps(t, 400, ours, func(from, to uint64) (int, bool) {
			return 1, false
		})

		var s Subscriber
		health := &cosmosLiveHealth{}
		health.recordSubscribed(time.Now())
		health.armFromHeight = 100 // subscription came up at height 100
		c := uint64(300)           // scanning well above it
		var quiet uint64

		if _, err := s.recoverCosmosGapToLatest(context.Background(), deps, services.NewBatchBuilder(),
			&c, map[cosmosEventKey]struct{}{}, &quiet, health, &blockRate{}); err != nil {
			t.Fatalf("recovery pass: %v", err)
		}
		if !health.hadTrafficToDeliver() {
			t.Fatal("traffic above the boundary did not arm the watchdog: a dead subscription " +
				"would now never be reported, and its packets sit until they time out")
		}
	})

	// P2, found in review: the chunk loop persists the cursor after every chunk that
	// succeeds, but the health signal was only recorded once the WHOLE range did. So
	// traffic found in chunk 1 was thrown away by an error in chunk 2 -- and since
	// the cursor had already moved past chunk 1, no later pass would ever see that
	// evidence again. A live subscription recovery had just proved dead stayed
	// unarmed, so the watchdog never fired and it was never reconnected.
	t.Run("evidence from completed chunks survives a later chunk error", func(t *testing.T) {
		const ours = "08-wasm-7"
		t.Setenv(cosmosRecoveryChunkEnv, "10")

		deps := rangedTxSearchCosmosDeps(t, 400, ours, func(from, to uint64) (int, bool) {
			if from >= 310 { // the second chunk onwards
				return 0, true
			}
			return 1, false // chunk one holds real traffic
		})

		var s Subscriber
		health := &cosmosLiveHealth{}
		health.recordSubscribed(time.Now())
		health.armFromHeight = 100 // everything scanned here is the subscription's own window
		c := uint64(300)
		var quiet uint64

		if _, err := s.recoverCosmosGapToLatest(context.Background(), deps, services.NewBatchBuilder(),
			&c, map[cosmosEventKey]struct{}{}, &quiet, health, &blockRate{}); err == nil {
			t.Fatal("the later chunk was expected to fail; the test would not reach the case it covers")
		}
		if c <= 300 {
			t.Fatalf("cursor = %d, want it past the chunk that succeeded: this test is about "+
				"evidence being unreachable BECAUSE the cursor moved", c)
		}
		if !health.hadTrafficToDeliver() {
			t.Fatal("evidence from the chunk that completed was discarded by the later chunk's error, " +
				"and the cursor has already moved past those heights: it can never be seen again")
		}
	})

	// A pass that finds no newly indexable range must SAY it saw nothing, not leave
	// the previous pass's answer standing.
	//
	// Found in review: livePathStale was judged before the range was known, and the
	// no-range path returned without recording anything. A chain that stops
	// producing blocks therefore kept the last "there was traffic" verdict alive,
	// and two minutes later tore down a live subscription that was healthy and
	// simply had nothing to carry -- the very false positive this change removes.
	t.Run("quiet recovery pass clears the traffic signal", func(t *testing.T) {
		const latest = int64(100)
		deps := statusOnlyCosmosDeps(t, latest)

		var s Subscriber
		health := &cosmosLiveHealth{}
		// The previous pass found traffic, and the live path has been silent well past
		// the threshold: exactly the state that used to force a reconnect.
		health.recordRecoveryPass(0, 5)
		health.lastSeen = time.Now().Add(-10 * cosmosLivePathStaleAfter)

		// Above the highest indexable height, so cosmosScanRange finds nothing.
		cursor := uint64(latest)
		var quietScans uint64

		stale, err := s.recoverCosmosGapToLatest(context.Background(), deps, services.NewBatchBuilder(),
			&cursor, map[cosmosEventKey]struct{}{}, &quietScans, health, &blockRate{})
		if err != nil {
			t.Fatalf("recovery pass: %v", err)
		}

		if stale {
			t.Fatal("a pass with nothing new to scan judged a healthy subscription dead on the previous pass's evidence")
		}
		if health.hadTrafficToDeliver() {
			t.Fatal("the traffic signal stayed latched; the next pass would tear the subscription down for the same reason")
		}
	})
}

// The gate itself, at the level that owns it. recordRecoveryPass is the ONLY way
// sawTraffic is armed, so a pass that cannot call it cannot arm the watchdog --
// which is what makes gating that one call the whole fix.
// The bool gate this used to test (armsWatchdog, passed false by the startup
// call and true by every tick) is gone. It rested on "the startup pass has
// consumed the lookback and moved the cursor to the head", and subscribeCosmosOnce
// treats a startup failure as non-fatal -- so the cursor could stay inside the
// pre-subscription lookback and the first tick rescanned it with arming ON.
// Arming is now decided by the height boundary captured when the subscription
// came up, which is a property of the heights rather than of the caller. The
// three tests below cover it: history does not arm even when re-read by a tick,
// heights above the boundary do, and evidence already gathered is not thrown
// away by a later chunk's error.

// rangedTxSearchCosmosDeps answers `status` with a fixed head and `tx_search`
// per height range, so a test can place traffic at chosen heights and fail
// chosen chunks. answer reports how many unrelated-client packets the range
// holds (each counts as one `skipped`, which is what arms sawTraffic) and
// whether the query should fail.
func rangedTxSearchCosmosDeps(t *testing.T, head int64, ours string,
	answer func(from, to uint64) (foreign int, fail bool),
) cosmosDeps {
	t.Helper()
	rangeRe := regexp.MustCompile(`tx\.height >= (\d+) AND tx\.height <= (\d+)`)
	foreign := channeltypesv2.Packet{Sequence: 1, SourceClient: "08-wasm-SOMEONE-ELSE", DestinationClient: "not-ours"}
	packetHex := mustPacketHex(t, foreign)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Query string `json:"query"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		id := string(req.ID)

		switch req.Method {
		case "status":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"sync_info":{"latest_block_height":"%d"}}}`, id, head)
		case "tx_search":
			m := rangeRe.FindStringSubmatch(req.Params.Query)
			if m == nil {
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"no height range in %q"}}`, id, req.Params.Query)
				return
			}
			from, _ := strconv.ParseUint(m[1], 10, 64)
			to, _ := strconv.ParseUint(m[2], 10, 64)
			n, fail := answer(from, to)
			if fail {
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"tx_search refused [%d,%d]"}}`, id, from, to)
				return
			}
			txs := make([]string, 0, n)
			for i := 0; i < n; i++ {
				txs = append(txs, fmt.Sprintf(`{"hash":"%064X","height":"%d","index":%d,"tx_result":{"code":0,"events":[{"type":"send_packet","attributes":[{"key":"encoded_packet_hex","value":"%s"}]}]},"tx":""}`,
					i+1, to, i, packetHex))
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"txs":[%s],"total_count":"%d"}}`, id, strings.Join(txs, ","), n)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unexpected method %s"}}`, id, req.Method)
		}
	}))
	t.Cleanup(server.Close)

	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatalf("create CometBFT client: %v", err)
	}
	deps := cosmosTestDeps("08-wasm-0", ours)
	deps.Cosmos = services.CosmosEndpoint{Client: client}
	return deps
}

// statusOnlyCosmosDeps answers `status` with a fixed height and nothing else, so
// a recovery pass can be driven to the point where it decides whether there is a
// range to scan.
func statusOnlyCosmosDeps(t *testing.T, height int64) cosmosDeps {
	t.Helper()
	node := rpcmock.NewCosmosNode(t)
	node.Handle("status", func(json.RawMessage) (any, error) {
		return &coretypes.ResultStatus{SyncInfo: coretypes.SyncInfo{LatestBlockHeight: height}}, nil
	})
	deps := cosmosTestDeps("08-wasm-0", "eth-router-0")
	deps.Cosmos = services.CosmosEndpoint{Client: node.Client(t)}
	return deps
}

func TestSubscribeCosmos_AlreadyCancelledDoesNotDereferenceClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewSubscriber().SubscribeCosmos(ctx, services.CosmosEndpoint{}, services.EVMEndpoint{}, services.ClientIDs{}, nil, nil)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Cosmos subscriber ignored already-cancelled context")
	}
}

func silentRPCServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return server, started
}

func TestRecoverCosmosEventsForQuery_TxSearchHonorsCancellation(t *testing.T) {
	server, started := silentRPCServer(t)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	deps := cosmosDeps{Cosmos: services.CosmosEndpoint{Client: client}, Logger: log.New(io.Discard, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := recoverCosmosEventsForQuery(ctx, deps, services.NewBatchBuilder(), cometBFTSendPacketTxSearch, 1, 2, nil)
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
		t.Fatal("TxSearch ignored cancellation")
	}
}

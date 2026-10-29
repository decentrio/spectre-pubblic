package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"relayer/chain"
	"relayer/chain/chaintest"
	"relayer/services"
	"strings"
	"sync"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

func ethPacket(t services.EthPacketType) services.EthPacket {
	p := services.EthPacket{
		Type:        t,
		Packet:      &channeltypesv2.Packet{Sequence: 1, SourceClient: "eth-router-0", DestinationClient: "08-wasm-0"},
		BlockNumber: 42,
	}
	// Only a WriteAck carries acknowledgement bytes; without them the mapping skips.
	if t == services.EthWriteAck {
		p.AckBytes = [][]byte{{0x01}}
	}
	return p
}

func TestEthPacketToEvent(t *testing.T) {
	// TestEthPacketToEvent/type mapping mirrors cosmos TestCosmosPacketToEvent: it
	// pins which queued EthPacket types become relay events and which are skipped
	// (EthTimeout is scanner-owned; EthAck is a terminal settle, not relayed here).
	t.Run("type mapping", func(t *testing.T) {
		tests := []struct {
			name     string
			packet   services.EthPacket
			wantOK   bool
			wantType chain.EventType
		}{
			{
				name:     "send relayed as recv",
				packet:   ethPacket(services.EthSend),
				wantOK:   true,
				wantType: chain.SendPacket,
			},
			{
				name:     "write-ack relayed as ack",
				packet:   ethPacket(services.EthWriteAck),
				wantOK:   true,
				wantType: chain.AckPacket,
			},
			{
				// EthTimeout is refunded on ETH by the async scanner, never relayed here.
				name:   "eth timeout skipped (scanner-owned)",
				packet: ethPacket(services.EthTimeout),
				wantOK: false,
			},
			{
				// EthAck settles a pending ETH-origin send; it is not a relayable event.
				name:   "eth ack skipped (terminal settle)",
				packet: ethPacket(services.EthAck),
				wantOK: false,
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				e, ok := ethPacketToEvent(tc.packet)
				if ok != tc.wantOK {
					t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
				}
				if ok && e.Type != tc.wantType {
					t.Fatalf("type = %d, want %d", e.Type, tc.wantType)
				}
			})
		}
	})

	// TestEthPacketToEvent/ack bytes carried guards that an ack event carries its
	// acknowledgement bytes through to the event (needed to build MsgAcknowledgement).
	t.Run("ack bytes carried", func(t *testing.T) {
		e, ok := ethPacketToEvent(ethPacket(services.EthWriteAck))
		if !ok {
			t.Fatal("write-ack with bytes must be relayed")
		}
		if len(e.AckBytes) != 1 || len(e.AckBytes[0]) != 1 || e.AckBytes[0][0] != 0x01 {
			t.Fatalf("ack bytes not carried: %v", e.AckBytes)
		}
	})

	// TestEthPacketToEvent/empty ack skipped guards the no-ack-bytes skip: a write-ack
	// without acknowledgement bytes cannot be built into a MsgAcknowledgement.
	t.Run("empty ack skipped", func(t *testing.T) {
		p := services.EthPacket{
			Type:   services.EthWriteAck,
			Packet: &channeltypesv2.Packet{Sequence: 1, SourceClient: "eth-router-0", DestinationClient: "08-wasm-0"},
			// AckBytes deliberately empty
		}
		if _, ok := ethPacketToEvent(p); ok {
			t.Fatal("write-ack with no acknowledgement bytes must be skipped")
		}
	})

	// TestEthPacketToEvent/nil packet guards the encode-failure skip.
	t.Run("nil packet", func(t *testing.T) {
		if _, ok := ethPacketToEvent(services.EthPacket{Type: services.EthSend}); ok {
			t.Fatal("nil packet must be skipped")
		}
	})
}

// Mirror of chain/cosmos TestEventsWithOrigins_StaysIndexAligned. The handler
// reports failures as indices into the events slice and Subscribe re-queues
// orig[idx], so a packet dropped from one slice and not the other shifts every
// later index: the relayer re-queues a packet that already succeeded and forgets
// the one that failed.
//
// The ETH side drops packets for one more reason than Cosmos does — a terminal
// EthAck/EthTimeout settles the pending tracker and is not relayed — so those are
// interleaved here too.
func TestEventsWithOrigins_StaysIndexAligned(t *testing.T) {
	seq := func(p services.EthPacket, n uint64) services.EthPacket {
		p.Packet.Sequence = n
		return p
	}
	ackNoBytes := func(n uint64) services.EthPacket {
		p := ethPacket(services.EthWriteAck)
		p.AckBytes = nil // no ack bytes: cannot build a MsgAcknowledgement, skipped
		p.Packet.Sequence = n
		return p
	}

	packets := []services.EthPacket{
		seq(ethPacket(services.EthAck), 1),     // terminal: settled, not relayed
		seq(ethPacket(services.EthSend), 2),    //
		ackNoBytes(3),                          // skipped, between
		seq(ethPacket(services.EthTimeout), 4), // terminal: settled, not relayed
		seq(ethPacket(services.EthSend), 5),    //
		seq(ethPacket(services.EthWriteAck), 6),
		ackNoBytes(7), // skipped, last
	}

	var settled []uint64
	var owedSettled []uint64
	events, orig := eventsWithOrigins(packets, func(p channeltypesv2.Packet) error {
		settled = append(settled, p.Sequence)
		return nil
	}, func(p channeltypesv2.Packet) {
		owedSettled = append(owedSettled, p.Sequence)
	})

	if len(events) != len(orig) {
		t.Fatalf("slices out of step: %d events vs %d origins", len(events), len(orig))
	}

	// Only an EthAck settles an owed acknowledgement. An EthTimeout is the other
	// ending -- refunded, nothing owed -- so clearing a debt on it would hide a
	// real one. Reported by @DongLieu: this path settled the pending tracker and
	// left the ledger untouched, so an acknowledgement another relayer submitted
	// left the debt reported overdue forever.
	var wantOwed []uint64
	for _, p := range packets {
		if p.Packet != nil && p.Type == services.EthAck {
			wantOwed = append(wantOwed, p.Packet.Sequence)
		}
	}
	if len(wantOwed) == 0 {
		t.Fatal("this fixture has no EthAck; the owed-ack assertion below would prove nothing")
	}
	if len(owedSettled) != len(wantOwed) {
		t.Fatalf("owed acknowledgements settled = %v, want %v", owedSettled, wantOwed)
	}
	for i := range wantOwed {
		if owedSettled[i] != wantOwed[i] {
			t.Fatalf("owed acknowledgements settled = %v, want %v", owedSettled, wantOwed)
		}
	}
	for i := range events {
		if orig[i].Packet == nil {
			t.Fatalf("origin %d has no packet", i)
		}
		if events[i].Sequence != orig[i].Packet.Sequence {
			t.Fatalf("index %d maps event seq=%d to origin seq=%d",
				i, events[i].Sequence, orig[i].Packet.Sequence)
		}
	}
	want := []uint64{2, 5, 6}
	if len(events) != len(want) {
		t.Fatalf("want %d relayable packets, got %d", len(want), len(events))
	}
	for i, w := range want {
		if events[i].Sequence != w {
			t.Fatalf("events[%d].Sequence = %d, want %d (dropped packets must leave BOTH slices)",
				i, events[i].Sequence, w)
		}
	}
	// A terminal event must still reach the tracker even though it is not relayed:
	// dropping it silently leaves the timeout scanner chasing a settled packet.
	wantSettled := []uint64{1, 4}
	if len(settled) != len(wantSettled) {
		t.Fatalf("settled %v, want %v", settled, wantSettled)
	}
	for i, w := range wantSettled {
		if settled[i] != w {
			t.Fatalf("settled %v, want %v", settled, wantSettled)
		}
	}
}

func TestEventsWithOrigins_LogsTerminalSettlementFailure(t *testing.T) {
	p := ethPacket(services.EthAck)
	p.Packet.Sequence = 44

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	events, orig := eventsWithOrigins([]services.EthPacket{p}, func(channeltypesv2.Packet) error {
		return errors.New("disk unavailable")
	}, func(channeltypesv2.Packet) {})
	if len(events) != 0 || len(orig) != 0 {
		t.Fatalf("terminal packet must not be relayed, got %d events / %d origins", len(events), len(orig))
	}
	if got := logs.String(); !strings.Contains(got, "[PendingTracker][ATTENTION]") ||
		!strings.Contains(got, "seq=44") || !strings.Contains(got, "disk unavailable") {
		t.Fatalf("settlement persistence failure was not logged with context: %q", got)
	}
}

// An ETH→Cosmos send past its timeout can never be received on Cosmos, so the
// relay path must hand it to the timeout scanner instead of proving it. Getting
// the direction wrong is expensive both ways: a live packet dropped as dead, or a
// dead packet re-proven forever. The boundary second is included because the chain
// itself treats an equal timestamp as expired.
func TestSendPacketExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name    string
		timeout uint64
		want    bool
	}{
		{"no timeout set never expires", 0, false},
		{"timeout well in the future", uint64(now.Unix()) + 600, false},
		{"one second left", uint64(now.Unix()) + 1, false},
		{"timeout is exactly now", uint64(now.Unix()), true},
		{"one second past", uint64(now.Unix()) - 1, true},
		{"long past", uint64(now.Unix()) - 86400, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkt := channeltypesv2.Packet{Sequence: 1, TimeoutTimestamp: tt.timeout}
			if got := sendPacketExpired(pkt, now); got != tt.want {
				t.Fatalf("sendPacketExpired(timeout=%d, now=%d) = %v, want %v",
					tt.timeout, now.Unix(), got, tt.want)
			}
		})
	}
}

// --- chain.Source contract ---
//
// Mirror of the set in chain/cosmos/source_test.go: the guards that must hold
// before this source touches the network.

// The chain.Source contract on the ETH side -- the guards that must hold before
// this source touches the network. Mirror of the set in chain/cosmos.
//
// TestSource, not TestEthSource: E9 wants a production identifier, and the type
// is Source. chain/cosmos has one too, and the plan calls that collision correct.
func TestSource(t *testing.T) {
	// Both height methods read the finalized beacon header, so a missing beacon
	// endpoint must be reported as the configuration error it is. Returning zero
	// instead would read as "the chain is at genesis" and silently hold every packet
	// as not-yet-relayable, with nothing in the log pointing at the config.
	t.Run("reports a missing beacon endpoint on both height methods", func(t *testing.T) {
		s := &Source{} // no beacon URL configured

		for _, tt := range []struct {
			name string
			call func() (uint64, error)
		}{
			{"LatestHeight", func() (uint64, error) { return s.LatestHeight(context.Background()) }},
			{"RelayableHeight", func() (uint64, error) { return s.RelayableHeight(context.Background()) }},
		} {
			t.Run(tt.name, func(t *testing.T) {
				h, err := tt.call()
				if err == nil {
					t.Fatal("missing beacon endpoint reported as a valid height")
				}
				if h != 0 {
					t.Fatalf("returned height %d alongside an error", h)
				}
				if !strings.Contains(err.Error(), "beacon") {
					t.Fatalf("error must name the missing endpoint, got: %v", err)
				}
			})
		}
	})

	// QueryHeader is a deliberate no-op here: the beacon builder self-fetches its
	// finality and sync-committee data, so the module's header argument is unused.
	// Pinned because "returns nothing, successfully" is easy to mistake for a stub
	// somebody forgot to finish — and turning it into an error would break the module,
	// which calls it on every update.
	t.Run("treats QueryHeader as a no-op", func(t *testing.T) {
		s := &Source{}
		header, err := s.QueryHeader(context.Background(), 12345)
		if err != nil {
			t.Fatalf("QueryHeader must succeed as a no-op, got %v", err)
		}
		if header != nil {
			t.Fatalf("QueryHeader must return no header, got %q", header)
		}
	})

	// ETH-origin timeouts belong to the async scanner, never to the relay path. If
	// this ever starts succeeding, two components are refunding the same packet.
	t.Run("refuses NonMembershipProof, which the scanner owns", func(t *testing.T) {
		s := &Source{}
		if _, err := s.NonMembershipProof(context.Background(), nil, 10); err == nil {
			t.Fatal("the relay path built a timeout proof the scanner owns")
		}
	})

	// Packet bytes that do not decode must fail before the source dials anything: a
	// zero-value Source has no clients, so reaching the network would panic rather
	// than return the error asserted here.
	t.Run("rejects undecodable packet bytes before dialing", func(t *testing.T) {
		s := &Source{}
		if _, err := s.MembershipProof(context.Background(), []byte{0xff, 0xff, 0xff, 0xff}, 10, chain.SendPacket); err == nil {
			t.Fatal("MembershipProof accepted undecodable packet bytes")
		}
	})
}

// proofRecorder is an EVM JSON-RPC stub that records the block eth_getProof was
// asked for. The proof body is irrelevant to these tests -- the question is only
// WHICH block the source proves at, so the stub answers with an error and the
// assertion reads the recorded parameter.
type proofRecorder struct {
	server *httptest.Server

	mu     sync.Mutex
	blocks []string
}

func newProofRecorder(t *testing.T) (*proofRecorder, *ethclient.Client) {
	t.Helper()
	r := &proofRecorder{}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
			t.Errorf("stub: decode request: %v", err)
			return
		}
		if call.Method == "eth_getProof" && len(call.Params) == 3 {
			if block, ok := call.Params[2].(string); ok {
				r.mu.Lock()
				r.blocks = append(r.blocks, block)
				r.mu.Unlock()
			}
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(call.ID) +
			`,"error":{"code":-32000,"message":"stub: proof body not modelled"}}`))
	}))
	t.Cleanup(r.server.Close)

	client, err := ethclient.Dial(r.server.URL)
	if err != nil {
		t.Fatalf("dial stub: %v", err)
	}
	t.Cleanup(client.Close)
	return r, client
}

func (r *proofRecorder) requestedBlocks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.blocks...)
}

func livePacket(t *testing.T) []byte {
	t.Helper()
	packet := channeltypesv2.Packet{
		Sequence:          7,
		SourceClient:      "client-eth",
		DestinationClient: "08-wasm-8",
		TimeoutTimestamp:  uint64(time.Now().Add(time.Hour).Unix()),
	}
	raw, err := packet.Marshal()
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	return raw
}

// The proof MUST be taken at the height the module hands down, not at whatever
// the on-chain client currently reports.
//
// Folding is why: the client update and the packet go out in ONE tx, so at
// proof-build time the update is not on chain and the client still reports the
// PREVIOUS execution block -- while the MsgRecvPacket names the consensus height
// the folded update installs. Proving at the client's current height there builds
// a proof against one state root and declares another, and the light client
// rejects it with "get trie node failed: Invalid state root".
func TestMembershipProof(t *testing.T) {
	cases := []struct {
		name      string
		eventType chain.EventType
	}{
		{"a recv proves the commitment at the height it is given", chain.SendPacket},
		{"an ack proves the acknowledgement at the height it is given", chain.AckPacket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder, client := newProofRecorder(t)
			s := &Source{evm: services.EVMEndpoint{Client: client}}

			// The stub cannot produce a real proof, so an error here is expected;
			// the recorded request is the subject.
			_, _ = s.MembershipProof(context.Background(), livePacket(t), 0xabcdef, tc.eventType)

			blocks := recorder.requestedBlocks()
			if len(blocks) != 1 {
				t.Fatalf("eth_getProof was called %d times, want exactly 1", len(blocks))
			}
			if !strings.EqualFold(blocks[0], "0xabcdef") {
				t.Fatalf("proof was requested at block %s, want 0xabcdef -- the height the module "+
					"asked for. Proving at any other block builds a proof against a state root the "+
					"destination will not be verifying against", blocks[0])
			}
		})
	}

	// Zero is not "latest": go-ethereum sends it as block 0, so a caller that
	// forgot to supply a height would silently prove against genesis and fail on
	// chain, far from where the mistake was made.
	t.Run("rejects a zero height", func(t *testing.T) {
		recorder, client := newProofRecorder(t)
		s := &Source{evm: services.EVMEndpoint{Client: client}}

		if _, err := s.MembershipProof(context.Background(), livePacket(t), 0, chain.SendPacket); err == nil {
			t.Fatal("MembershipProof accepted height 0; it would have proven against genesis")
		}
		if got := len(recorder.requestedBlocks()); got != 0 {
			t.Fatalf("eth_getProof was called %d times for a zero height; it must fail before dialing", got)
		}
	})
}

// The adapter contract suite. Everything asserted here is asserted identically
// on the other two adapters: adding a fourth chain is a package plus this file.
func TestAdapterContract(t *testing.T) {
	chaintest.RunSourceContract(t, chaintest.SourceFixture{Name: "evm", Unwired: &Source{}})
	chaintest.RunDestinationContract(t, chaintest.DestinationFixture{Name: "evm", Unwired: &Destination{}})
}

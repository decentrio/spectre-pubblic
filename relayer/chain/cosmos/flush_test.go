package cosmos

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"relayer/chain"
	"relayer/rpcmock"
	"relayer/services"
	"relayer/utils"
	"strconv"
	"strings"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	abcitypes "github.com/cometbft/cometbft/abci/types"
	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	cmttypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	"github.com/cosmos/gogoproto/proto"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	ethcommon "github.com/ethereum/go-ethereum/common"
)

// commitmentFor is the hash the chain stores for a packet, and therefore what a
// flush pass compares a candidate transaction against. Built through the same
// ibc-go function the chain uses, not restated here, so the test cannot agree
// with the code about a value the chain would disagree with.
func commitmentFor(packet channeltypesv2.Packet) flushCommitment {
	return flushCommitment{sequence: packet.Sequence, data: channeltypesv2.CommitPacket(packet)}
}

func encodedSendAttributes(packet channeltypesv2.Packet) []abcitypes.EventAttribute {
	raw, err := proto.Marshal(&packet)
	if err != nil {
		panic(err)
	}
	return []abcitypes.EventAttribute{
		{Key: "packet_source_client", Value: packet.SourceClient},
		{Key: "packet_sequence", Value: "7"},
		{Key: "encoded_packet_hex", Value: hex.EncodeToString(raw)},
	}
}

// TestPacketFromSendEvent: the enumeration path rebuilds a packet from the same
// encoded attribute the subscriber consumes, so one wire format serves both.
// Decoding a malformed one must report failure rather than hand a zero packet
// to the relay loop, which would relay sequence 0 on an empty client id.
func TestPacketFromSendEvent(t *testing.T) {
	t.Parallel()

	want := channeltypesv2.Packet{
		Sequence:          7,
		SourceClient:      "08-wasm-0",
		DestinationClient: "client-0",
		TimeoutTimestamp:  1_700_000_000,
	}

	t.Run("decodes the encoded packet", func(t *testing.T) {
		t.Parallel()
		got, ok := packetFromSendEvent(encodedSendAttributes(want))
		if !ok {
			t.Fatal("a well-formed send_packet event must decode")
		}
		if got.Sequence != want.Sequence || got.SourceClient != want.SourceClient ||
			got.DestinationClient != want.DestinationClient {
			t.Fatalf("decoded %+v, want %+v", got, want)
		}
	})

	t.Run("reports failure rather than returning a zero packet", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name       string
			attributes []abcitypes.EventAttribute
		}{
			{"no encoded attribute", []abcitypes.EventAttribute{{Key: "packet_sequence", Value: "7"}}},
			{"not hex", []abcitypes.EventAttribute{{Key: "encoded_packet_hex", Value: "zzzz"}}},
			{"hex but not a packet", []abcitypes.EventAttribute{{Key: "encoded_packet_hex", Value: "ffffffff"}}},
			{"no attributes at all", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if _, ok := packetFromSendEvent(tc.attributes); ok {
					t.Fatal("reported success on an undecodable event; the relay loop would then relay sequence 0")
				}
			})
		}
	})
}

// TestMustMarshalPacket: the packet was just decoded from the chain's own event,
// so a failure here means it did not round-trip. Returning nil keeps a malformed
// event from taking the process down.
func TestMustMarshalPacket(t *testing.T) {
	t.Parallel()

	packet := channeltypesv2.Packet{Sequence: 7, SourceClient: "08-wasm-0", DestinationClient: "client-0"}
	raw := mustMarshalPacket(packet)
	if len(raw) == 0 {
		t.Fatal("a valid packet must re-encode")
	}
	var round channeltypesv2.Packet
	if err := proto.Unmarshal(raw, &round); err != nil {
		t.Fatalf("re-encoded packet does not decode: %v", err)
	}
	if round.Sequence != packet.Sequence || round.SourceClient != packet.SourceClient {
		t.Fatalf("round trip changed the packet: %+v -> %+v", packet, round)
	}
}

// Cosmos stores a send commitment under the packet's LOCAL source client, which
// for a Cosmos->EVM packet is the EVM light client on Cosmos (EVMOnCosmos), not
// the id by which the EVM router knows Cosmos (CosmosOnEVM). Querying the latter
// enumerates an empty set, so the backstop silently finds nothing on exactly the
// path it exists for.
func TestCommitmentQueryClientID_FlushQueriesTheLocalSourceClient(t *testing.T) {
	source := &Source{
		ids:    services.ClientIDs{CosmosOnEVM: "eth-side-id", EVMOnCosmos: "08-wasm-7"},
		logger: log.New(io.Discard, "", 0),
	}
	if got := source.commitmentQueryClientID(); got != "08-wasm-7" {
		t.Fatalf("commitment query client id = %q, want the local source client %q", got, "08-wasm-7")
	}
}

// TxSearch matches TRANSACTIONS; the event list it hands back is the whole
// transaction's, unfiltered. A transaction carrying several sends must not
// answer a request for sequence N with the first send it happens to contain.
func TestMatchSendEvent_ForSequencePicksTheMatchingEvent(t *testing.T) {
	events := []abcitypes.Event{
		{Type: "send_packet", Attributes: encodedSendAttributes(channeltypesv2.Packet{
			Sequence: 41, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
		})},
		{Type: "send_packet", Attributes: encodedSendAttributes(channeltypesv2.Packet{
			Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
		})},
	}

	wanted := channeltypesv2.Packet{Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id"}
	got, found := matchSendEvent(events, "08-wasm-7", commitmentFor(wanted))
	if !found {
		t.Fatal("the requested sequence is present in the transaction and must be found")
	}
	if got.Sequence != 42 {
		t.Fatalf("returned sequence %d for a request for 42; the earlier send in the same tx was taken", got.Sequence)
	}

	absent := channeltypesv2.Packet{Sequence: 43, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id"}
	if _, found := matchSendEvent(events, "08-wasm-7", commitmentFor(absent)); found {
		t.Fatal("a sequence absent from the transaction must report not-found, not the nearest send")
	}
}

// Sequences are per-client, so seq=N exists independently on every client. The
// query names the client, but that only decides which TRANSACTIONS come back --
// one carrying sends for two clients matches it and offers both events. Taking
// the wrong client's send relays the wrong packet to the wrong destination and
// leaves the sequence that was actually asked about outstanding.
func TestMatchSendEvent_ForSequenceRejectsAnotherClientsSameSequence(t *testing.T) {
	events := []abcitypes.Event{
		{Type: "send_packet", Attributes: encodedSendAttributes(channeltypesv2.Packet{
			Sequence: 42, SourceClient: "08-wasm-OTHER", DestinationClient: "other-destination",
		})},
		{Type: "send_packet", Attributes: encodedSendAttributes(channeltypesv2.Packet{
			Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
		})},
	}

	wanted := channeltypesv2.Packet{Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id"}
	got, found := matchSendEvent(events, "08-wasm-7", commitmentFor(wanted))
	if !found {
		t.Fatal("the requested client's send is present and must be found")
	}
	if got.SourceClient != "08-wasm-7" || got.DestinationClient != "eth-side-id" {
		t.Fatalf("returned src=%s dst=%s; another client's seq=42 was taken, so the requested packet stays outstanding",
			got.SourceClient, got.DestinationClient)
	}

	// And a client with no send in this transaction must not borrow one.
	if _, found := matchSendEvent(events, "08-wasm-ABSENT", commitmentFor(wanted)); found {
		t.Fatal("a client absent from the transaction must report not-found")
	}
}

var ackTestRouter = ethcommon.HexToAddress("0x000000000000000000000000000000000000c26e")

// encodedWriteAckAttributes are the attributes an ibc-go v2
// write_acknowledgement event carries, for a packet this chain received.
func encodedWriteAckAttributes(t *testing.T, packet channeltypesv2.Packet, acks [][]byte) []abci.EventAttribute {
	t.Helper()
	rawPacket, err := proto.Marshal(&packet)
	if err != nil {
		t.Fatal(err)
	}
	rawAck, err := proto.Marshal(&channeltypesv2.Acknowledgement{AppAcknowledgements: acks})
	if err != nil {
		t.Fatal(err)
	}
	return []abci.EventAttribute{
		{Key: "packet_source_client", Value: packet.SourceClient},
		{Key: "packet_dest_client", Value: packet.DestinationClient},
		{Key: "packet_sequence", Value: big.NewInt(int64(packet.Sequence)).String()},
		{Key: "encoded_packet_hex", Value: hex.EncodeToString(rawPacket)},
		{Key: "encoded_acknowledgement_hex", Value: hex.EncodeToString(rawAck)},
	}
}

func txWithWriteAck(t *testing.T, height int64, packet channeltypesv2.Packet, acks [][]byte) *coretypes.ResultTx {
	t.Helper()
	return &coretypes.ResultTx{
		Height: height,
		Tx:     cmttypes.Tx{},
		TxResult: abci.ExecTxResult{
			Events: []abci.Event{{Type: "write_acknowledgement", Attributes: encodedWriteAckAttributes(t, packet, acks)}},
		},
	}
}

func TestUnrelayedPackets(t *testing.T) {
	// The acknowledgement leg answers "the router still holds the send AND this
	// chain wrote an ack for it". Both halves are asked; either alone is wrong.
	t.Run("lists acks for sends the router still holds", func(t *testing.T) {
		// The router: two EVM->Cosmos sends still committed, seq 1 and 2.
		evmNode := rpcmock.NewEVMNode(t)
		evmNode.SetStorage(ackTestRouter, services.EthPrevSequenceSendKey("eth-side-id"), ethcommon.BigToHash(big.NewInt(2)))
		acked := channeltypesv2.Packet{Sequence: 1, SourceClient: "eth-side-id", DestinationClient: "08-wasm-7", TimeoutTimestamp: 4_000_000_000}
		unreceived := channeltypesv2.Packet{Sequence: 2, SourceClient: "eth-side-id", DestinationClient: "08-wasm-7", TimeoutTimestamp: 4_000_000_000}
		for _, pkt := range []channeltypesv2.Packet{acked, unreceived} {
			evmNode.SetStorage(ackTestRouter,
				services.EthIBCStorageKey(services.EthPath("eth-side-id", pkt.Sequence, services.EthPathCommitment)),
				ethcommon.BytesToHash(channeltypesv2.CommitPacket(pkt)))
		}

		// This chain: an ack written for seq 1 only, and its indexed transaction.
		acks := [][]byte{[]byte(`{"result":"AQ=="}`)}
		rpc := &flushRPC{
			t:      t,
			store:  map[string]bool{string(utils.IBCPath("08-wasm-7", 1, []byte{3})[1]): true},
			ackTxs: []*coretypes.ResultTx{txWithWriteAck(t, 77, acked, acks)},
		}
		server := httptest.NewServer(rpc)
		t.Cleanup(server.Close)
		client, err := rpchttp.New(server.URL, "/websocket")
		if err != nil {
			t.Fatal(err)
		}
		source := &Source{
			cosmos: services.CosmosEndpoint{Client: client},
			evm:    services.EVMEndpoint{Client: evmNode.Client(t), Contracts: services.EVMContracts{Router: ackTestRouter}},
			ids:    services.ClientIDs{CosmosOnEVM: "eth-side-id", EVMOnCosmos: "08-wasm-7"},
			logger: log.New(io.Discard, "", 0),
		}

		events, err := source.UnrelayedPackets(context.Background())
		if err != nil {
			t.Fatalf("UnrelayedPackets: %v", err)
		}
		var ackEvents []chain.Event
		for _, e := range events {
			if e.Type == chain.AckPacket {
				ackEvents = append(ackEvents, e)
			}
		}
		if len(ackEvents) != 1 || ackEvents[0].Sequence != 1 || ackEvents[0].Height != 77 {
			t.Fatalf("ack events = %+v, want exactly seq 1 at height 77: seq 2 has no ack here yet", ackEvents)
		}
		if len(ackEvents[0].AckBytes) != 1 || string(ackEvents[0].AckBytes[0]) != string(acks[0]) {
			t.Fatalf("ack bytes = %q, want the event's", ackEvents[0].AckBytes)
		}
		if ackEvents[0].ClientID != "08-wasm-7" {
			t.Fatalf("event client = %q, want the packet's destination client on this chain", ackEvents[0].ClientID)
		}
		if source.ackProbeCursor != 1 {
			t.Fatalf("ack probe cursor = %d, want 1 (the whole space, two sequences, in one pass)", source.ackProbeCursor)
		}
		// The cost, not only the answer: a send this chain has not acknowledged is
		// not worth a transaction search. Without the ack-path check the pass costs
		// one TxSearch per OPEN send on the router rather than per acknowledged one
		// -- the answer would be the same, the backlog would not.
		if rpc.txCalls != 1 {
			t.Fatalf("tx_search calls = %d, want 1: only the send this chain acknowledged is searched for", rpc.txCalls)
		}
	})

	// The transaction found for a sequence must be the packet the ROUTER's
	// commitment names: a re-created client reuses sequences and the index keeps
	// the old acknowledgement.
	t.Run("ack must match the routers commitment", func(t *testing.T) {
		evmNode := rpcmock.NewEVMNode(t)
		evmNode.SetStorage(ackTestRouter, services.EthPrevSequenceSendKey("eth-side-id"), ethcommon.BigToHash(big.NewInt(1)))
		current := channeltypesv2.Packet{Sequence: 1, SourceClient: "eth-side-id", DestinationClient: "08-wasm-7", TimeoutTimestamp: 4_000_000_000}
		stale := channeltypesv2.Packet{Sequence: 1, SourceClient: "eth-side-id", DestinationClient: "08-wasm-7", TimeoutTimestamp: 1_000}
		evmNode.SetStorage(ackTestRouter,
			services.EthIBCStorageKey(services.EthPath("eth-side-id", 1, services.EthPathCommitment)),
			ethcommon.BytesToHash(channeltypesv2.CommitPacket(current)))

		rpc := &flushRPC{
			t:      t,
			store:  map[string]bool{string(utils.IBCPath("08-wasm-7", 1, []byte{3})[1]): true},
			ackTxs: []*coretypes.ResultTx{txWithWriteAck(t, 5, stale, [][]byte{[]byte("old")})},
		}
		server := httptest.NewServer(rpc)
		t.Cleanup(server.Close)
		client, err := rpchttp.New(server.URL, "/websocket")
		if err != nil {
			t.Fatal(err)
		}
		source := &Source{
			cosmos: services.CosmosEndpoint{Client: client},
			evm:    services.EVMEndpoint{Client: evmNode.Client(t), Contracts: services.EVMContracts{Router: ackTestRouter}},
			ids:    services.ClientIDs{CosmosOnEVM: "eth-side-id", EVMOnCosmos: "08-wasm-7"},
			logger: log.New(io.Discard, "", 0),
		}
		events, err := source.UnrelayedPackets(context.Background())
		if err != nil {
			t.Fatalf("UnrelayedPackets: %v", err)
		}
		for _, e := range events {
			if e.Type == chain.AckPacket {
				t.Fatalf("relayed an acknowledgement for a packet the router does not commit to: %+v", e)
			}
		}
	})

	// A probe that fails must not move the ack cursor (the EVM sources' rule).
	t.Run("a failed router probe leaves the ack cursor alone", func(t *testing.T) {
		evmNode := rpcmock.NewEVMNode(t)
		evmNode.SetStorage(ackTestRouter, services.EthPrevSequenceSendKey("eth-side-id"), ethcommon.BigToHash(big.NewInt(3)))
		rpc := &flushRPC{t: t, store: map[string]bool{}}
		server := httptest.NewServer(rpc)
		t.Cleanup(server.Close)
		client, err := rpchttp.New(server.URL, "/websocket")
		if err != nil {
			t.Fatal(err)
		}
		source := &Source{
			cosmos: services.CosmosEndpoint{Client: client},
			evm:    services.EVMEndpoint{Client: evmNode.Client(t), Contracts: services.EVMContracts{Router: ackTestRouter}},
			ids:    services.ClientIDs{CosmosOnEVM: "eth-side-id", EVMOnCosmos: "08-wasm-7"},
			logger: log.New(io.Discard, "", 0),
		}

		evmNode.FailBatches(errors.New("probe down"))
		if _, err := source.UnrelayedPackets(context.Background()); err != nil {
			t.Fatalf("a failing ack leg is reported, not returned: %v", err)
		}
		if source.ackProbeCursor != 0 {
			t.Fatalf("ack probe cursor advanced to %d on a failed probe", source.ackProbeCursor)
		}

		evmNode.FailBatches(nil)
		if _, err := source.UnrelayedPackets(context.Background()); err != nil {
			t.Fatal(err)
		}
		if source.ackProbeCursor != 1 {
			t.Fatalf("ack probe cursor = %d after a successful probe, want 1 (window 1..3 taken)", source.ackProbeCursor)
		}
	})
}

// flushRPC serves the two calls a flush pass makes -- the paginated commitment
// query and the transaction search -- against a synthetic chain, and counts the
// ABCI round-trips so a test can assert what a pass COSTS and not only what it
// returns.
type flushRPC struct {
	t *testing.T

	// commitments is the outstanding set, ascending, exactly as the store holds
	// it: keyed by sequence, valued by the commitment hash.
	commitments []channeltypesv2.PacketState
	// txs are the indexed send_packet transactions, oldest first, which is the
	// order "asc" hands back. ackTxs are the write_acknowledgement ones.
	txs    []*coretypes.ResultTx
	ackTxs []*coretypes.ResultTx
	// store answers the point lookups the acknowledgement leg makes, keyed by
	// the raw ICS-24 path bytes: present means the path holds a value.
	store map[string]bool

	abciCalls int
	txCalls   int
}

type flushRPCRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type flushRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
}

func (m *flushRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req flushRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		m.t.Errorf("decode RPC request: %v", err)
		return
	}
	switch req.Method {
	case "abci_query":
		m.abciCalls++
		var q struct {
			Path string            `json:"path"`
			Data cmtbytes.HexBytes `json:"data"`
		}
		if err := cmtjson.Unmarshal(req.Params, &q); err != nil {
			m.t.Fatalf("decode abci_query params: %v", err)
		}
		var value []byte
		if q.Path == "store/ibc/key" {
			// The acknowledgement leg's point lookup: "is this ICS-24 path set".
			if m.store[string(q.Data)] {
				value = []byte{1}
			}
		} else {
			value = m.serveCommitments(req.Params)
		}
		m.writeResult(w, req.ID, &coretypes.ResultABCIQuery{Response: abci.ResponseQuery{Value: value}})
	case "tx_search":
		m.txCalls++
		// per_page is honoured, because it is load-bearing: a search that asks for
		// one result gets only the OLDEST transaction at that sequence, which on a
		// replayed sequence is the stale one. A stub that ignored it would let the
		// commitment check look sufficient on its own.
		//
		// `page` is honoured for the same reason, and it is what the second
		// review round turned up: a stub that always answered page 1 would let a
		// single-page lookup look complete while the live send sat at result 11.
		txs := m.txs
		if strings.Contains(m.query(req.Params), "write_acknowledgement") {
			txs = m.ackTxs
		}
		m.writeResult(w, req.ID, &coretypes.ResultTxSearch{
			Txs: m.pageOf(req.Params, txs), TotalCount: len(txs),
		})
	default:
		m.t.Errorf("unexpected RPC method %q", req.Method)
	}
}

// serveCommitments answers one page the way the SDK paginator does: start the
// iterator at Pagination.Key, return Limit entries, and hand back the key of the
// next one. That is the behaviour the window query depends on, so faking it
// loosely would make the test agree with a store that does not exist.
func (m *flushRPC) serveCommitments(params json.RawMessage) []byte {
	var decoded struct {
		Data cmtbytes.HexBytes `json:"data"`
	}
	if err := cmtjson.Unmarshal(params, &decoded); err != nil {
		m.t.Fatalf("decode abci_query params: %v", err)
	}
	var request channeltypesv2.QueryPacketCommitmentsRequest
	if err := proto.Unmarshal(decoded.Data, &request); err != nil {
		m.t.Fatalf("decode commitments request: %v", err)
	}

	start := 0
	if key := request.Pagination.GetKey(); len(key) == 8 {
		from := binary.BigEndian.Uint64(key)
		for start < len(m.commitments) && m.commitments[start].Sequence < from {
			start++
		}
	}
	limit := int(request.Pagination.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	response := channeltypesv2.QueryPacketCommitmentsResponse{Pagination: &query.PageResponse{}}
	for i := start; i < len(m.commitments) && len(response.Commitments) < limit; i++ {
		commitment := m.commitments[i]
		response.Commitments = append(response.Commitments, &commitment)
	}
	if next := start + len(response.Commitments); next < len(m.commitments) {
		response.Pagination.NextKey = sequenceKey(m.commitments[next].Sequence)
	}
	raw, err := proto.Marshal(&response)
	if err != nil {
		m.t.Fatalf("marshal commitments response: %v", err)
	}
	return raw
}

// pageOf slices the indexed transactions the way CometBFT paginates them:
// one-based page, per_page entries, and an empty slice past the end.
func (m *flushRPC) pageOf(params json.RawMessage, txs []*coretypes.ResultTx) []*coretypes.ResultTx {
	perPage := m.perPage(params)
	page := m.page(params)
	start := (page - 1) * perPage
	if start >= len(txs) {
		return nil
	}
	return txs[start:min(start+perPage, len(txs))]
}

// query reads the tx_search query string, which is what tells the send leg's
// lookups from the acknowledgement leg's.
func (m *flushRPC) query(params json.RawMessage) string {
	var decoded struct {
		Query string `json:"query"`
	}
	if err := cmtjson.Unmarshal(params, &decoded); err != nil {
		m.t.Fatalf("decode tx_search params: %v", err)
	}
	return decoded.Query
}

// page reads the one-based page number, defaulting the way CometBFT does.
func (m *flushRPC) page(params json.RawMessage) int {
	var decoded struct {
		Page *string `json:"page"`
	}
	if err := cmtjson.Unmarshal(params, &decoded); err != nil {
		m.t.Fatalf("decode tx_search params: %v", err)
	}
	if decoded.Page == nil {
		return 1
	}
	n, err := strconv.Atoi(*decoded.Page)
	if err != nil {
		m.t.Fatalf("page %q: %v", *decoded.Page, err)
	}
	return n
}

// perPage reads the page size out of the tx_search parameters, defaulting the
// way CometBFT does when the caller does not ask.
func (m *flushRPC) perPage(params json.RawMessage) int {
	var decoded struct {
		PerPage *string `json:"per_page"`
	}
	if err := cmtjson.Unmarshal(params, &decoded); err != nil {
		m.t.Fatalf("decode tx_search params: %v", err)
	}
	if decoded.PerPage == nil {
		return 30
	}
	n, err := strconv.Atoi(*decoded.PerPage)
	if err != nil {
		m.t.Fatalf("per_page %q: %v", *decoded.PerPage, err)
	}
	return n
}

func (m *flushRPC) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, err := cmtjson.Marshal(result)
	if err != nil {
		m.t.Fatalf("marshal RPC result: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(flushRPCResponse{JSONRPC: "2.0", ID: id, Result: raw}); err != nil {
		m.t.Errorf("encode RPC response: %v", err)
	}
}

func newFlushSource(t *testing.T, rpc *flushRPC) *Source {
	t.Helper()
	server := httptest.NewServer(rpc)
	t.Cleanup(server.Close)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatalf("rpc client: %v", err)
	}
	return &Source{
		cosmos: services.CosmosEndpoint{Client: client},
		ids:    services.ClientIDs{CosmosOnEVM: "eth-side-id", EVMOnCosmos: "08-wasm-7"},
		logger: log.New(io.Discard, "", 0),
	}
}

func syntheticCommitments(n int) []channeltypesv2.PacketState {
	out := make([]channeltypesv2.PacketState, 0, n)
	for i := 1; i <= n; i++ {
		packet := channeltypesv2.Packet{
			Sequence: uint64(i), SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
		}
		out = append(out, channeltypesv2.PacketState{
			ClientId: "08-wasm-7", Sequence: packet.Sequence, Data: channeltypesv2.CommitPacket(packet),
		})
	}
	return out
}

func TestOutstandingCommitments(t *testing.T) {
	// Reported by @DongLieu on #452 (P2). The pass used to page the WHOLE commitment
	// set before capping it, so its query cost was proportional to the backlog: a
	// chain with a large outstanding set spent hundreds of serial ABCI round-trips
	// and could burn the 60-second pass timeout without relaying anything. The cap
	// that was advertised as bounding the work only ever bounded the TxSearch half.
	//
	// The bound asserted here is the one that matters operationally: the number of
	// round-trips must depend on the WINDOW, not on how much is outstanding.
	t.Run("cost does not grow with the backlog", func(t *testing.T) {
		const backlog = 50_000
		rpc := &flushRPC{t: t, commitments: syntheticCommitments(backlog)}
		source := newFlushSource(t, rpc)

		window, err := source.outstandingCommitments(context.Background())
		if err != nil {
			t.Fatalf("outstanding commitments: %v", err)
		}
		if len(window) != flushSequenceCap {
			t.Fatalf("window = %d commitments, want the cap %d", len(window), flushSequenceCap)
		}

		wantCalls := flushSequenceCap / services.CosmosCommitmentPageSize
		if rpc.abciCalls > wantCalls {
			t.Fatalf("a %d-commitment backlog cost %d ABCI queries, want at most %d; "+
				"the query is paging the whole set again and the pass timeout bounds the backstop, not the cap",
				backlog, rpc.abciCalls, wantCalls)
		}
	})

	// The rotation has to survive moving into the query: passes must advance, and
	// the pass that runs out of tail must wrap rather than stop.
	t.Run("rotates and wraps", func(t *testing.T) {
		rpc := &flushRPC{t: t, commitments: syntheticCommitments(flushSequenceCap * 2)}
		source := newFlushSource(t, rpc)

		take := func() []flushCommitment {
			window, err := source.outstandingCommitments(context.Background())
			if err != nil {
				t.Fatalf("outstanding commitments: %v", err)
			}
			source.advanceFlushCursor(window)
			return window
		}

		first, second, third := take(), take(), take()

		if first[0].sequence != 1 {
			t.Fatalf("the first pass must start at the oldest, got %d", first[0].sequence)
		}
		if second[0].sequence != first[len(first)-1].sequence+1 {
			t.Fatalf("second pass starts at %d, want %d; later packets would never be considered",
				second[0].sequence, first[len(first)-1].sequence+1)
		}
		if third[0].sequence != 1 {
			t.Fatalf("the pass after the end must wrap to the oldest, got %d", third[0].sequence)
		}
	})

	// A set that fits inside one window must come back whole on every pass, and the
	// cursor must not skip its own head next time.
	t.Run("keeps a short set whole", func(t *testing.T) {
		rpc := &flushRPC{t: t, commitments: syntheticCommitments(3)}
		source := newFlushSource(t, rpc)

		for pass := 0; pass < 3; pass++ {
			window, err := source.outstandingCommitments(context.Background())
			if err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
			if len(window) != 3 || window[0].sequence != 1 {
				t.Fatalf("pass %d returned %d commitments starting at %d, want all 3 starting at 1",
					pass, len(window), window[0].sequence)
			}
			source.advanceFlushCursor(window)
		}
	})
}

func TestSendEventForCommitment(t *testing.T) {
	// Reported by @DongLieu on #452 (P1). A sequence names a POSITION, not a packet:
	// a client migration keeps the client id and restarts sequences, so the index
	// holds an old send at the same (client, sequence). Ordering by "asc" hands back
	// that old transaction first, and matching on the sequence alone would rebuild a
	// packet whose lifecycle closed long ago -- while the packet actually
	// outstanding stayed undiscovered, identically, on every pass.
	//
	// The commitment is what separates them, and it is the same hash the chain would
	// check a proof against.
	t.Run("picks the packet the commitment names", func(t *testing.T) {
		stale := channeltypesv2.Packet{
			Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
			Payloads: []channeltypesv2.Payload{{SourcePort: "transfer", DestinationPort: "transfer", Value: []byte("old")}},
		}
		live := channeltypesv2.Packet{
			Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
			Payloads: []channeltypesv2.Payload{{SourcePort: "transfer", DestinationPort: "transfer", Value: []byte("new")}},
		}

		rpc := &flushRPC{t: t, txs: []*coretypes.ResultTx{
			// Oldest first, which is what "asc" returns and what a single-result
			// search would have taken.
			txWithSend(11, stale),
			txWithSend(97, live),
		}}
		source := newFlushSource(t, rpc)

		event, found, err := source.sendEventForCommitment(context.Background(), commitmentFor(live))
		if err != nil {
			t.Fatalf("send event: %v", err)
		}
		if !found {
			t.Fatal("the outstanding packet is indexed and must be found")
		}
		if event.Height != 97 {
			t.Fatalf("resolved the send at height %d, want 97: the replayed sequence's OLD transaction was taken, "+
				"so a settled packet is relayed and the outstanding one is never recovered", event.Height)
		}

		// And a commitment naming a packet nothing indexed must report not-found
		// rather than borrowing the nearest send at that sequence.
		absent := live
		absent.Payloads[0].Value = []byte("never sent")
		if _, found, err := source.sendEventForCommitment(context.Background(), commitmentFor(absent)); err != nil || found {
			t.Fatalf("found=%v err=%v; a commitment with no matching send must report not-found", found, err)
		}
	})

	// Found in review: raising per_page from 1 to 10 defeated a stale entry that
	// came back FIRST, but the lookup still read only page one. On the very nodes
	// that cross-match -- psql, or kv entries in the legacy format -- the stale
	// entries can FILL that page and leave the live send at result 11. The lookup
	// then reported not-found, the cursor moved on, and the next pass asked the
	// identical question and got the identical answer: outstanding forever, while
	// every pass says there is nothing to do.
	t.Run("pages past a full page of stale matches", func(t *testing.T) {
		live := channeltypesv2.Packet{
			Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
			Payloads: []channeltypesv2.Payload{{SourcePort: "transfer", DestinationPort: "transfer", Value: []byte("new")}},
		}

		// Exactly one full page of cross-matches, oldest first, then the packet the
		// commitment actually names.
		txs := make([]*coretypes.ResultTx, 0, flushTxSearchResults+1)
		for i := range flushTxSearchResults {
			stale := live
			stale.Payloads = []channeltypesv2.Payload{{
				SourcePort: "transfer", DestinationPort: "transfer",
				Value: []byte(fmt.Sprintf("stale-%d", i)),
			}}
			txs = append(txs, txWithSend(int64(11+i), stale))
		}
		txs = append(txs, txWithSend(97, live))

		rpc := &flushRPC{t: t, txs: txs}
		source := newFlushSource(t, rpc)

		event, found, err := source.sendEventForCommitment(context.Background(), commitmentFor(live))
		if err != nil {
			t.Fatalf("send event: %v", err)
		}
		if !found {
			t.Fatalf("the outstanding packet is indexed at result %d and must be found; a lookup that "+
				"reads one page leaves it outstanding forever while every pass reports no work",
				len(txs))
		}
		if event.Height != 97 {
			t.Fatalf("resolved height %d, want 97", event.Height)
		}
		if rpc.txCalls < 2 {
			t.Fatalf("tx_search was called %d time(s); the match is on page two, so a single call "+
				"cannot have found it honestly", rpc.txCalls)
		}
	})

	// The other end of the same loop: a cross-matching index must not be able to
	// spin it. The pass is bounded per sequence, and reaching the bound reports
	// rather than looking like a clean not-found.
	t.Run("stops at the scan bound", func(t *testing.T) {
		live := channeltypesv2.Packet{
			Sequence: 42, SourceClient: "08-wasm-7", DestinationClient: "eth-side-id",
			Payloads: []channeltypesv2.Payload{{SourcePort: "transfer", DestinationPort: "transfer", Value: []byte("new")}},
		}

		// More stale entries than the bound allows, and the live one past all of
		// them, so the lookup must give up rather than page on forever.
		total := flushTxSearchMaxResults + flushTxSearchResults*3
		txs := make([]*coretypes.ResultTx, 0, total+1)
		for i := range total {
			stale := live
			stale.Payloads = []channeltypesv2.Payload{{
				SourcePort: "transfer", DestinationPort: "transfer",
				Value: []byte(fmt.Sprintf("stale-%d", i)),
			}}
			txs = append(txs, txWithSend(int64(11+i), stale))
		}
		txs = append(txs, txWithSend(9_000, live))

		rpc := &flushRPC{t: t, txs: txs}
		source := newFlushSource(t, rpc)

		if _, found, err := source.sendEventForCommitment(context.Background(), commitmentFor(live)); err != nil || found {
			t.Fatalf("found=%v err=%v; past the scan bound the lookup must give up", found, err)
		}
		maxCalls := flushTxSearchMaxResults/flushTxSearchResults + 1
		if rpc.txCalls > maxCalls {
			t.Fatalf("tx_search was called %d times, want at most %d: one sequence must not be able "+
				"to page without bound, because flushSequenceCap of them run per pass",
				rpc.txCalls, maxCalls)
		}
	})
}

func txWithSend(height int64, packet channeltypesv2.Packet) *coretypes.ResultTx {
	return &coretypes.ResultTx{
		Height: height,
		Tx:     cmttypes.Tx{},
		TxResult: abci.ExecTxResult{
			Events: []abci.Event{{Type: "send_packet", Attributes: encodedSendAttributes(packet)}},
		},
	}
}

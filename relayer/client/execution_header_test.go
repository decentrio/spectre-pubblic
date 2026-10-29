package client

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// testExecutionHeader builds a Prague-era header. go-ethereum encodes it independently of this
// package's decoder, so the tests do not just check the decoder against itself.
func testExecutionHeader(stateRoot common.Hash, number, timestamp uint64) *types.Header {
	baseFee := big.NewInt(7)
	withdrawals := types.EmptyWithdrawalsHash
	blobGas := uint64(0)
	parentBeaconRoot := common.HexToHash("0x01")
	requests := types.EmptyRequestsHash
	return &types.Header{
		ParentHash:       common.HexToHash("0x02"),
		UncleHash:        types.EmptyUncleHash,
		Coinbase:         common.HexToAddress("0x03"),
		Root:             stateRoot,
		TxHash:           types.EmptyTxsHash,
		ReceiptHash:      types.EmptyReceiptsHash,
		Difficulty:       big.NewInt(0),
		Number:           new(big.Int).SetUint64(number),
		GasLimit:         30_000_000,
		GasUsed:          21_000,
		Time:             timestamp,
		Extra:            []byte("spectre"),
		BaseFee:          baseFee,
		WithdrawalsHash:  &withdrawals,
		BlobGasUsed:      &blobGas,
		ExcessBlobGas:    &blobGas,
		ParentBeaconRoot: &parentBeaconRoot,
		RequestsHash:     &requests,
	}
}

func encodeHeader(t *testing.T, h *types.Header) []byte {
	t.Helper()
	raw, err := rlp.EncodeToBytes(h)
	if err != nil {
		t.Fatalf("encode header: %v", err)
	}
	return raw
}

func TestDecodeExecutionHeaderRLP(t *testing.T) {
	stateRoot := common.HexToHash("0xaabbcc")
	header := testExecutionHeader(stateRoot, 1_234_567, 1_790_000_000)
	raw := encodeHeader(t, header)

	t.Run("reads the state root, number and timestamp the hash commits to", func(t *testing.T) {
		got, err := DecodeExecutionHeaderRLP(raw, header.Hash())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := ExecutionSummary{StateRoot: stateRoot.Hex(), Timestamp: 1_790_000_000, BlockNumber: 1_234_567}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("rejects bytes that do not hash to the proven block hash", func(t *testing.T) {
		forged := encodeHeader(t, testExecutionHeader(common.HexToHash("0xaabbcd"), 1_234_567, 1_790_000_000))
		_, err := DecodeExecutionHeaderRLP(forged, header.Hash())
		if err == nil || !strings.Contains(err.Error(), "proven execution block hash") {
			t.Fatalf("want a hash mismatch error, got %v", err)
		}
	})

	t.Run("rejects oversized input before hashing", func(t *testing.T) {
		big := make([]byte, maxExecutionHeaderRLPBytes+1)
		if _, err := DecodeExecutionHeaderRLP(big, crypto.Keccak256Hash(big)); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("want a size limit error, got %v", err)
		}
	})

	t.Run("rejects malformed encodings even when the hash matches", func(t *testing.T) {
		for name, bad := range map[string][]byte{
			"not a list":    {0x83, 1, 2, 3},
			"trailing byte": append(append([]byte{}, raw...), 0x00),
			"too few items": mustEncode(t, []common.Hash{{1}}),
		} {
			if _, err := DecodeExecutionHeaderRLP(bad, crypto.Keccak256Hash(bad)); err == nil {
				t.Errorf("%s: expected an error", name)
			}
		}
	})
}

func mustEncode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := rlp.EncodeToBytes(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

type fakeHeaderSource struct {
	raw   []byte
	err   error
	calls int
}

func (f *fakeHeaderSource) RawHeaderByHash(_ context.Context, _ common.Hash) ([]byte, error) {
	f.calls++
	return f.raw, f.err
}

func TestResolveExecution(t *testing.T) {
	ctx := context.Background()
	stateRoot := common.HexToHash("0x1234")
	header := testExecutionHeader(stateRoot, 42, 9_000)
	raw := encodeHeader(t, header)

	t.Run("pre-Gloas format uses the payload header and needs no node", func(t *testing.T) {
		source := &fakeHeaderSource{}
		got, rlpBytes, err := ResolveExecution(ctx, source, LightClientHeader{
			Execution: &ExecutionPayloadHeader{StateRoot: "0xroot", BlockNumber: "7", Timestamp: "1000"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != (ExecutionSummary{StateRoot: "0xroot", BlockNumber: 7, Timestamp: 1000}) {
			t.Errorf("got %+v", got)
		}
		if rlpBytes != nil || source.calls != 0 {
			t.Errorf("a pre-Gloas header must not fetch anything (rlp=%x calls=%d)", rlpBytes, source.calls)
		}
	})

	t.Run("Gloas format fetches the header and returns its RLP for the wasm client", func(t *testing.T) {
		source := &fakeHeaderSource{raw: raw}
		got, rlpBytes, err := ResolveExecution(ctx, source, LightClientHeader{ExecutionBlockHash: header.Hash().Hex()})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.StateRoot != stateRoot.Hex() || got.BlockNumber != 42 || got.Timestamp != 9_000 {
			t.Errorf("got %+v", got)
		}
		if string(rlpBytes) != string(raw) {
			t.Errorf("the RLP handed to the wasm client must be the exact bytes that were hashed")
		}
	})

	t.Run("Gloas format rejects a node that serves a different header", func(t *testing.T) {
		other := encodeHeader(t, testExecutionHeader(common.HexToHash("0xdead"), 42, 9_000))
		source := &fakeHeaderSource{raw: other}
		if _, _, err := ResolveExecution(ctx, source, LightClientHeader{ExecutionBlockHash: header.Hash().Hex()}); err == nil {
			t.Fatal("a header that does not hash to the proven block hash must be rejected")
		}
	})

	t.Run("Gloas format without a node is an error, not a zero value", func(t *testing.T) {
		if _, _, err := ResolveExecution(ctx, nil, LightClientHeader{ExecutionBlockHash: header.Hash().Hex()}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("node errors surface", func(t *testing.T) {
		source := &fakeHeaderSource{err: errors.New("boom")}
		if _, _, err := ResolveExecution(ctx, source, LightClientHeader{ExecutionBlockHash: header.Hash().Hex()}); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("want the node error, got %v", err)
		}
	})

	t.Run("exactly one execution field must be set", func(t *testing.T) {
		both := LightClientHeader{Execution: &ExecutionPayloadHeader{}, ExecutionBlockHash: header.Hash().Hex()}
		if _, _, err := ResolveExecution(ctx, &fakeHeaderSource{raw: raw}, both); err == nil {
			t.Error("both set: expected an error")
		}
		if _, _, err := ResolveExecution(ctx, &fakeHeaderSource{raw: raw}, LightClientHeader{}); err == nil {
			t.Error("neither set: expected an error")
		}
	})
}

func TestLightClientHeaderJSONFormats(t *testing.T) {
	t.Run("pre-Gloas JSON decodes with the payload header and re-encodes unchanged", func(t *testing.T) {
		in := `{"beacon":{"slot":"1","proposer_index":"2","parent_root":"0x01","state_root":"0x02","body_root":"0x03"},` +
			`"execution":{"parent_hash":"0xa","fee_recipient":"0xb","state_root":"0xc","receipts_root":"0xd","logs_bloom":"0xe",` +
			`"prev_randao":"0xf","block_number":"7","gas_limit":"1","gas_used":"2","timestamp":"1000","extra_data":"0x","base_fee_per_gas":"3",` +
			`"block_hash":"0x10","transactions_root":"0x11","withdrawals_root":"0x12","blob_gas_used":"0","excess_blob_gas":"0"},` +
			`"execution_branch":["0x01","0x02"]}`
		var h LightClientHeader
		if err := json.Unmarshal([]byte(in), &h); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if h.Execution == nil || h.Execution.BlockNumber != "7" || h.IsGloasFormat() {
			t.Fatalf("pre-Gloas header misread: %+v", h)
		}
		out, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if strings.Contains(string(out), "execution_block_hash") {
			t.Errorf("a pre-Gloas header must not emit execution_block_hash: %s", out)
		}
	})

	t.Run("Gloas JSON decodes to the block hash form and omits the payload header", func(t *testing.T) {
		in := `{"beacon":{"slot":"1","proposer_index":"2","parent_root":"0x01","state_root":"0x02","body_root":"0x03"},` +
			`"execution_block_hash":"0x00000000000000000000000000000000000000000000000000000000000000aa","execution_branch":["0x01"]}`
		var h LightClientHeader
		if err := json.Unmarshal([]byte(in), &h); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !h.IsGloasFormat() || h.Execution != nil {
			t.Fatalf("Gloas header misread: %+v", h)
		}
		out, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if strings.Contains(string(out), `"execution":`) {
			t.Errorf("a Gloas header must not emit an execution payload header: %s", out)
		}
	})

	t.Run("update header omits execution_header_rlp unless set", func(t *testing.T) {
		out, err := json.Marshal(EthereumHeader{})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if strings.Contains(string(out), "execution_header_rlp") {
			t.Errorf("unset RLP must not appear, so pre-Gloas messages stay byte-identical: %s", out)
		}
		out, err = json.Marshal(EthereumHeader{ExecutionHeaderRLP: "0xabcd"})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !strings.Contains(string(out), `"execution_header_rlp":"0xabcd"`) {
			t.Errorf("RLP missing: %s", out)
		}
	})
}

func TestToForkParametersGloas(t *testing.T) {
	base := BeaconSpec{
		GenesisForkVersion: "0x00000000",
		AltairForkEpoch:    "0", BellatrixForkEpoch: "0", CapellaForkEpoch: "0", DenebForkEpoch: "0",
		ElectraForkVersion: "0x05000000", ElectraForkEpoch: "100",
	}

	t.Run("scheduled Gloas keeps its own version and epoch", func(t *testing.T) {
		spec := base
		spec.GloasForkVersion, spec.GloasForkEpoch = "0x07000000", "353024"
		fp, err := spec.ToForkParameters()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fp.Gloas.Version != "0x07000000" || fp.Gloas.Epoch != 353024 {
			t.Errorf("Gloas: got %q@%d", fp.Gloas.Version, fp.Gloas.Epoch)
		}
	})

	t.Run("unscheduled Gloas maps to never, with a valid zero version", func(t *testing.T) {
		spec := base
		fp, err := spec.ToForkParameters()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fp.Gloas.Epoch != ^uint64(0) || fp.Gloas.Version != "0x00000000" {
			t.Errorf("Gloas: got %q@%d, want 0x00000000@^uint64(0)", fp.Gloas.Version, fp.Gloas.Epoch)
		}
	})

	t.Run("client state JSON carries the gloas fork for the wasm client", func(t *testing.T) {
		spec := base
		spec.GloasForkVersion, spec.GloasForkEpoch = "0x07000000", "353024"
		fp, err := spec.ToForkParameters()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out, err := json.Marshal(fp)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !strings.Contains(string(out), `"gloas":{"epoch":353024,"version":"0x07000000"}`) {
			t.Errorf("gloas fork missing from the client state: %s", out)
		}
	})
}

// TestGloasBootstrapDoesNotNeedAnExecutionObject is the regression test for
//
//	failed to create Ethereum client on Cosmos: strconv.ParseUint: parsing "": invalid syntax
//
// A Gloas bootstrap has no execution object, so reading execution.block_number yields "" and
// ParseUint fails. The values must come from the execution header the block hash names.
func TestGloasBootstrapDoesNotNeedAnExecutionObject(t *testing.T) {
	stateRoot := common.HexToHash("0xabcdef")
	header := testExecutionHeader(stateRoot, 777, 1_790_000_123)
	raw := encodeHeader(t, header)

	body := `{"data":{"header":{"beacon":{"slot":"1","proposer_index":"2","parent_root":"0x01","state_root":"0x02","body_root":"0x03"},` +
		`"execution_block_hash":"` + header.Hash().Hex() + `","execution_branch":["0x01"]},` +
		`"current_sync_committee":{"pubkeys":[],"aggregate_pubkey":"0x"},` +
		`"current_sync_committee_branch":["0x01","0x02","0x03","0x04","0x05","0x06","0x07","0x08","0x09","0x0a","0x0b"]}}`

	var bootstrap BootstrapResponse
	if err := json.Unmarshal([]byte(body), &bootstrap); err != nil {
		t.Fatalf("decode Gloas bootstrap: %v", err)
	}
	if len(bootstrap.Data.CurrentSyncCommitteeBranch) != 11 {
		t.Fatalf("the Gloas current_sync_committee_branch has 11 nodes, decoded %d", len(bootstrap.Data.CurrentSyncCommitteeBranch))
	}

	got, _, err := ResolveExecution(context.Background(), &fakeHeaderSource{raw: raw}, bootstrap.Data.Header)
	if err != nil {
		t.Fatalf("resolve execution for a Gloas bootstrap: %v", err)
	}
	want := ExecutionSummary{StateRoot: stateRoot.Hex(), Timestamp: 1_790_000_123, BlockNumber: 777}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

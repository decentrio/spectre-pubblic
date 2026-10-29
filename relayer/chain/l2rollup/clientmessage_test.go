package l2rollup

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// semanticJSONEqual compares two JSON documents ignoring field order and
// insignificant whitespace — the equality Dũng specified for the cross-language
// contract (do NOT assert raw text equality).
func semanticJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON not semantically equal:\n got: %s\nwant: %s", got, want)
	}
}

// attestedExampleJSON is shared verbatim with the Rust decoder test.
func attestedExampleJSON(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "test", "fixtures", "wasm-contracts", "l2-client-message.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestAttestedL2Header(t *testing.T) {
	// TestAttestedL2Header/round trip decodes the canonical example into the Go types and
	// re-encodes it, asserting the wire form is semantically identical. This is the
	// freeze test for the encoding contract now shared by every chain — the per-chain
	// fixtures it replaces died with the settlement headers.
	t.Run("round trip", func(t *testing.T) {
		want := attestedExampleJSON(t)

		var env clientMessageEnvelope
		if err := json.Unmarshal([]byte(want), &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if env.Type != "header" {
			t.Fatalf("type = %q, want header", env.Type)
		}
		var h SignedAttestedL2Header
		if err := json.Unmarshal(env.Value, &h); err != nil {
			t.Fatalf("decode header: %v", err)
		}

		// Spot-check the tricky representations: hex quantities, optional fields, and the
		// number-array proof matrix that must never serialize as base64.
		if h.L2Header.Number != 42 || h.L2Header.Difficulty.Uint64() != 0 || h.L2Header.BaseFeePerGas.Uint64() != 9 {
			t.Fatalf("scalar decode mismatch")
		}
		if h.L2Header.BlobGasUsed == nil || *h.L2Header.BlobGasUsed != 11 {
			t.Fatalf("blob_gas_used decode mismatch")
		}
		if len(h.RouterProof.Proof) != 2 || h.RouterProof.Proof[0][1] != 2 {
			t.Fatalf("router_proof decode mismatch: %v", h.RouterProof.Proof)
		}
		if len(h.AttestorSignature) != 1 || h.AttestorSignature[0].AttestorIndex != 0 || len(h.AttestorSignature[0].Signature) != 64 {
			t.Fatalf("signed fixture certificate mismatch: %+v", h.AttestorSignature)
		}

		got, err := h.EncodeClientMessage()
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		semanticJSONEqual(t, got, []byte(want))
	})

	// The []byte→base64 gotcha at the struct level, not just the scalar unit test: an
	// account proof must go out as arrays of numbers.
	t.Run("proof is number arrays", func(t *testing.T) {
		h := &SignedAttestedL2Header{
			RouterProof:       EvmAccountProof{Proof: byteMatrix{{222, 173}}},
			AttestorSignature: []IndexedAttestorSignature{{AttestorIndex: 0, Signature: make([]byte, 64)}},
		}
		raw, err := h.EncodeClientMessage()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !strings.Contains(string(raw), `"proof":[[222,173]]`) {
			t.Fatalf("router_proof not number arrays: %s", raw)
		}
	})

	t.Run("rejects unsigned encoding", func(t *testing.T) {
		if _, err := (&SignedAttestedL2Header{}).EncodeClientMessage(); err == nil {
			t.Fatal("unsigned L2 header encoded successfully")
		}
	})
}

// --- toCanonicalHeader: the go-ethereum header -> wire header mapping ---
//
// This mapping had no test at all. It is the tightest contract in the system:
// packages/l2-client/src/canonical_header.rs carries #[serde(deny_unknown_fields)]
// and re-derives the block hash by RLP-encoding these exact fields, so a dropped,
// swapped or renamed field is not a compile error and not a test failure — it is a
// rejected update, discovered on chain after a transaction has been paid for.

// pragueHeader is a full post-Prague execution header: every optional field
// present, and every value distinct from every other of its type so a mapping that
// pairs the wrong two cannot come out equal.
func pragueHeader() *types.Header {
	b32 := func(b byte) common.Hash {
		var h common.Hash
		for i := range h {
			h[i] = b
		}
		return h
	}
	beacon := b32(0xbb)
	requests := b32(0xcc)
	withdrawals := b32(0xdd)
	blobGas := uint64(131072)
	excessBlob := uint64(262144)
	var bloom types.Bloom
	bloom[0], bloom[255] = 0xa1, 0xa2
	return &types.Header{
		ParentHash:       b32(0x11),
		UncleHash:        b32(0x22),
		Coinbase:         common.HexToAddress("0x3333333333333333333333333333333333333333"),
		Root:             b32(0x44),
		TxHash:           b32(0x55),
		ReceiptHash:      b32(0x66),
		Bloom:            bloom,
		Difficulty:       big.NewInt(0),
		Number:           big.NewInt(9_912_004),
		GasLimit:         30_000_000,
		GasUsed:          21_000,
		Time:             1_700_000_000,
		Extra:            []byte{0xde, 0xad, 0xbe, 0xef},
		MixDigest:        b32(0x77),
		Nonce:            types.BlockNonce{1, 2, 3, 4, 5, 6, 7, 8},
		BaseFee:          big.NewInt(0x9),
		WithdrawalsHash:  &withdrawals,
		BlobGasUsed:      &blobGas,
		ExcessBlobGas:    &excessBlob,
		ParentBeaconRoot: &beacon,
		RequestsHash:     &requests,
	}
}

// rebuildHeader maps a CanonicalEvmHeader back to a go-ethereum header, using only
// what the wire carries. It exists so the test can ask the question the L2 client
// asks: does RLP-hashing these fields reproduce the block hash?
func rebuildHeader(ch CanonicalEvmHeader) *types.Header {
	hash := func(b hexBytes) common.Hash { return common.BytesToHash(b) }
	optHash := func(b *hexBytes) *common.Hash {
		if b == nil {
			return nil
		}
		h := common.BytesToHash(*b)
		return &h
	}
	h := &types.Header{
		ParentHash:       hash(ch.ParentHash),
		UncleHash:        hash(ch.OmmersHash),
		Coinbase:         common.BytesToAddress(ch.Beneficiary),
		Root:             hash(ch.StateRoot),
		TxHash:           hash(ch.TransactionsRoot),
		ReceiptHash:      hash(ch.ReceiptsRoot),
		Bloom:            types.BytesToBloom(ch.LogsBloom),
		Difficulty:       new(big.Int).Set(&ch.Difficulty.Int),
		Number:           new(big.Int).SetUint64(ch.Number),
		GasLimit:         ch.GasLimit,
		GasUsed:          ch.GasUsed,
		Time:             ch.Timestamp,
		Extra:            ch.ExtraData,
		MixDigest:        hash(ch.MixHash),
		WithdrawalsHash:  optHash(ch.WithdrawalsRoot),
		BlobGasUsed:      ch.BlobGasUsed,
		ExcessBlobGas:    ch.ExcessBlobGas,
		ParentBeaconRoot: optHash(ch.ParentBeaconBlockRoot),
		RequestsHash:     optHash(ch.RequestsHash),
	}
	copy(h.Nonce[:], ch.Nonce)
	if ch.BaseFeePerGas != nil {
		h.BaseFee = new(big.Int).Set(&ch.BaseFeePerGas.Int)
	}
	return h
}

func TestToCanonicalHeader(t *testing.T) {
	// The invariant the L2 client actually checks: RLP-encode the canonical header,
	// keccak it, and get the block hash back (canonical_header.rs rlp_bytes + the hash
	// derivation above it). Comparing against a golden JSON blob would prove the shape;
	// this proves the mapping carries the block's identity, which is what a wrong field
	// destroys.
	t.Run("preserves the block hash", func(t *testing.T) {
		src := pragueHeader()
		if got := rebuildHeader(toCanonicalHeader(src)).Hash(); got != src.Hash() {
			t.Fatalf("block hash after the round trip = %s, want %s", got, src.Hash())
		}
	})

	// Every field, one at a time: change the source header and the hash must move. A
	// field the mapping silently drops shows up here as a hash that did not change,
	// which is the failure the block-hash test alone cannot localise.
	t.Run("carries every field into the hash", func(t *testing.T) {
		for name, mutate := range map[string]func(*types.Header){
			"parent_hash":              func(h *types.Header) { h.ParentHash[0] ^= 0xff },
			"ommers_hash":              func(h *types.Header) { h.UncleHash[0] ^= 0xff },
			"beneficiary":              func(h *types.Header) { h.Coinbase[0] ^= 0xff },
			"state_root":               func(h *types.Header) { h.Root[0] ^= 0xff },
			"transactions_root":        func(h *types.Header) { h.TxHash[0] ^= 0xff },
			"receipts_root":            func(h *types.Header) { h.ReceiptHash[0] ^= 0xff },
			"logs_bloom":               func(h *types.Header) { h.Bloom[7] ^= 0xff },
			"difficulty":               func(h *types.Header) { h.Difficulty = big.NewInt(3) },
			"number":                   func(h *types.Header) { h.Number = big.NewInt(9_912_005) },
			"gas_limit":                func(h *types.Header) { h.GasLimit++ },
			"gas_used":                 func(h *types.Header) { h.GasUsed++ },
			"timestamp":                func(h *types.Header) { h.Time++ },
			"extra_data":               func(h *types.Header) { h.Extra = []byte{0xfe} },
			"mix_hash":                 func(h *types.Header) { h.MixDigest[0] ^= 0xff },
			"nonce":                    func(h *types.Header) { h.Nonce[0] ^= 0xff },
			"base_fee_per_gas":         func(h *types.Header) { h.BaseFee = big.NewInt(0xabc) },
			"withdrawals_root":         func(h *types.Header) { v := common.Hash{9}; h.WithdrawalsHash = &v },
			"blob_gas_used":            func(h *types.Header) { v := uint64(1); h.BlobGasUsed = &v },
			"excess_blob_gas":          func(h *types.Header) { v := uint64(1); h.ExcessBlobGas = &v },
			"parent_beacon_block_root": func(h *types.Header) { v := common.Hash{9}; h.ParentBeaconRoot = &v },
			"requests_hash":            func(h *types.Header) { v := common.Hash{9}; h.RequestsHash = &v },
		} {
			t.Run(name, func(t *testing.T) {
				base := pragueHeader()
				changed := pragueHeader()
				mutate(changed)
				if changed.Hash() == base.Hash() {
					t.Fatalf("the fixture makes %s indistinguishable; the mutation changed nothing", name)
				}
				if rebuildHeader(toCanonicalHeader(changed)).Hash() == rebuildHeader(toCanonicalHeader(base)).Hash() {
					t.Fatalf("%s does not reach the wire header: two different blocks produced one hash", name)
				}
			})
		}
	})

	// serde reads a missing Option as None, and deny_unknown_fields rejects an extra
	// key. So an optional field the chain does not have must be ABSENT, never null:
	// null is an unknown shape for a field the fork does not define.
	t.Run("omits fields the fork does not have", func(t *testing.T) {
		optional := []string{
			"base_fee_per_gas", "withdrawals_root", "blob_gas_used",
			"excess_blob_gas", "parent_beacon_block_root", "requests_hash",
		}
		for _, tc := range []struct {
			fork    string
			header  func() *types.Header
			present []string
		}{
			{
				fork:    "prague",
				header:  pragueHeader,
				present: optional,
			},
			{
				fork: "cancun",
				header: func() *types.Header {
					h := pragueHeader()
					h.RequestsHash = nil
					return h
				},
				present: optional[:5],
			},
			{
				fork: "shanghai",
				header: func() *types.Header {
					h := pragueHeader()
					h.RequestsHash, h.BlobGasUsed, h.ExcessBlobGas, h.ParentBeaconRoot = nil, nil, nil, nil
					return h
				},
				present: optional[:2],
			},
			{
				fork: "pre-london carries no optional tail at all",
				header: func() *types.Header {
					h := pragueHeader()
					h.BaseFee, h.WithdrawalsHash, h.BlobGasUsed = nil, nil, nil
					h.ExcessBlobGas, h.ParentBeaconRoot, h.RequestsHash = nil, nil, nil
					return h
				},
				present: nil,
			},
		} {
			t.Run(tc.fork, func(t *testing.T) {
				raw, err := json.Marshal(toCanonicalHeader(tc.header()))
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				want := map[string]bool{}
				for _, f := range tc.present {
					want[f] = true
				}
				for _, f := range optional {
					value, ok := got[f]
					if want[f] && !ok {
						t.Errorf("%s is missing though this fork defines it", f)
					}
					if !want[f] && ok {
						t.Errorf("%s is present as %s though this fork does not define it; serde reads a "+
							"missing Option as None, and any other shape is rejected", f, value)
					}
					if ok && string(value) == "null" {
						t.Errorf("%s serialized as null; absent and null are different to serde", f)
					}
				}
			})
		}
	})

	// deny_unknown_fields makes the key set itself the contract: a field added to the
	// Go struct is not a compile error here and not a test failure anywhere else — the
	// wasm client rejects the update, on chain, after the proof was paid for. This list
	// is copied from packages/l2-client/src/canonical_header.rs; change both together.
	t.Run("emits exactly the rust field set", func(t *testing.T) {
		rust := []string{
			"parent_hash", "ommers_hash", "beneficiary", "state_root", "transactions_root",
			"receipts_root", "logs_bloom", "difficulty", "number", "gas_limit", "gas_used",
			"timestamp", "extra_data", "mix_hash", "nonce",
			"base_fee_per_gas", "withdrawals_root", "blob_gas_used", "excess_blob_gas",
			"parent_beacon_block_root", "requests_hash",
		}

		raw, err := json.Marshal(toCanonicalHeader(pragueHeader()))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		want := map[string]bool{}
		for _, f := range rust {
			want[f] = true
			if _, ok := got[f]; !ok {
				t.Errorf("%q is in the Rust struct but not on the wire", f)
			}
		}
		for f := range got {
			if !want[f] {
				t.Errorf("%q is on the wire but not in the Rust struct; deny_unknown_fields rejects it, "+
					"and only on chain", f)
			}
		}
	})
}

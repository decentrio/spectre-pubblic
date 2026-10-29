package l2rollup

import (
	"fmt"
	"time"
)

// This file mirrors, byte-for-byte on the JSON wire, the header types the cw-ics08
// L2 wasm light clients deserialize. Ground truth, and there is no longer a
// per-chain half — the three verifier crates are identical adapters:
//   - packages/l2-client/src/msg.rs              → ClientMessage, SignedAttestedL2Header,
//                                                  EvmAccountProof, EvmStorageProof
//   - packages/l2-client/src/canonical_header.rs → CanonicalEvmHeader
//
// The client verifies the header against itself (the canonical fork layout pinned in
// the profile, and the block hash derived from it), validates every attestor
// signature, then checks the L2 router account proof against the header state root.
//
// Field NAMES and value REPRESENTATIONS are the frozen encoding contract (Dũng, PR
// #245); the Rust structs carry `#[serde(deny_unknown_fields)]`, so emit exactly
// these fields and no extras.
//
// scalar wire types (hexBytes / byteList / byteMatrix / u256) live in wire.go.

// ClientMessage is a signed header that encodes to the wasm ClientMessage envelope
// {"type":"header","value":<header>}.
type ClientMessage interface {
	// EncodeClientMessage marshals the header into the tagged ClientMessage JSON.
	EncodeClientMessage() ([]byte, error)
}

// EvmAccountProof mirrors l2-client `EvmAccountProof { proof: Vec<Vec<u8>> }`.
type EvmAccountProof struct {
	Proof byteMatrix `json:"proof"`
}

// EvmStorageProof mirrors l2-client `EvmStorageProof { key: B256, value: Vec<u8>,
// proof: Vec<Vec<u8>> }`. value is minimal big-endian bytes (1→[1], zero→[]).
type EvmStorageProof struct {
	Key   hexBytes   `json:"key"`
	Value byteList   `json:"value"`
	Proof byteMatrix `json:"proof"`
}

// CanonicalEvmHeader mirrors l2-client `CanonicalEvmHeader` — the full EVM execution
// header (l2_header) whose state_root the rollup output/assertion commits to. The
// optional tail is fork-gated on the Rust side (validate_for_fork): a Cancun L2
// carries base_fee..parent_beacon_block_root; Prague adds requests_hash. serde
// treats a missing Option field as None, so nil optionals are omitted here.
type CanonicalEvmHeader struct {
	ParentHash       hexBytes `json:"parent_hash"`
	OmmersHash       hexBytes `json:"ommers_hash"`
	Beneficiary      hexBytes `json:"beneficiary"`
	StateRoot        hexBytes `json:"state_root"`
	TransactionsRoot hexBytes `json:"transactions_root"`
	ReceiptsRoot     hexBytes `json:"receipts_root"`
	LogsBloom        hexBytes `json:"logs_bloom"`
	Difficulty       u256     `json:"difficulty"`
	Number           uint64   `json:"number"`
	GasLimit         uint64   `json:"gas_limit"`
	GasUsed          uint64   `json:"gas_used"`
	Timestamp        uint64   `json:"timestamp"`
	ExtraData        hexBytes `json:"extra_data"`
	MixHash          hexBytes `json:"mix_hash"`
	Nonce            hexBytes `json:"nonce"`

	BaseFeePerGas         *u256     `json:"base_fee_per_gas,omitempty"`
	WithdrawalsRoot       *hexBytes `json:"withdrawals_root,omitempty"`
	BlobGasUsed           *uint64   `json:"blob_gas_used,omitempty"`
	ExcessBlobGas         *uint64   `json:"excess_blob_gas,omitempty"`
	ParentBeaconBlockRoot *hexBytes `json:"parent_beacon_block_root,omitempty"`
	RequestsHash          *hexBytes `json:"requests_hash,omitempty"`
}

// IndexedAttestorSignature mirrors l2-client `IndexedAttestorSignature`. Its
// signature is base64 JSON, as CosmWasm Binary expects.
type IndexedAttestorSignature struct {
	AttestorIndex uint16 `json:"attestor_index"`
	Signature     []byte `json:"signature"`
}

// SignedAttestedL2Header mirrors l2-client `SignedAttestedL2Header` — the only update
// shape the attestor-trusted clients accept, and identical for every chain.
//
// The settlement fields the per-chain headers used to carry (beacon_slot,
// l1_state_root, factory/game/assertion proofs, output-root preimages) are gone
// because nothing verifies them any more. Indexed signatures replace settlement
// evidence as the contract-verifiable source of trust.
type SignedAttestedL2Header struct {
	L2Header          CanonicalEvmHeader         `json:"l2_header"`
	RouterProof       EvmAccountProof            `json:"router_proof"`
	AttestorSignature []IndexedAttestorSignature `json:"attestor_signature"`
}

// EncodeClientMessage marshals the header into the ClientMessage envelope. There is
// no inner envelope any more: the Arbitrum verifier used to wrap its header in a
// second tagged enum (`{"type":"bold_v2",...}`) because it had more than one header
// variant, and it no longer does.
// TrustedAt is the L2 block time the header carries: the source-chain timestamp
// a client update installs, which is what the client-update observer measures
// staleness from (the same figure the ETH and Cosmos builders report).
func (h *SignedAttestedL2Header) TrustedAt() time.Time {
	return time.Unix(int64(h.L2Header.Timestamp), 0).UTC()
}

func (h *SignedAttestedL2Header) EncodeClientMessage() ([]byte, error) {
	if len(h.AttestorSignature) == 0 {
		return nil, fmt.Errorf("l2 client message: signed header requires at least one attestor signature")
	}
	return encodeHeaderMessage(h)
}

// Header types satisfy ClientMessage.
var _ ClientMessage = (*SignedAttestedL2Header)(nil)

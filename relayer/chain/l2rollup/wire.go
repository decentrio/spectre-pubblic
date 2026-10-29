package l2rollup

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// This file holds the JSON scalar types that make the Go client message serialize
// byte-identically to the cw-ics08 L2 client's serde (Rust). The encoding contract
// is Dũng's spec on PR #245; the ground-truth Rust types live in packages/l2-client
// (msg.rs, canonical_header.rs) — the per-chain verifier crates carry no header
// types of their own. Two representations must be kept apart:
//
//   - byte VECTORS (serde `Vec<u8>` / `Vec<Vec<u8>>`) serialize as JSON NUMBER
//     ARRAYS ([248,81]). Go's built-in `[]byte` marshals to a base64 STRING, which
//     the verifier rejects — hence byteList / byteMatrix below. (`[]uint8` does not
//     help: it is the same type as `[]byte` in Go and also base64-encodes.)
//   - fixed-width words and opaque bytes (alloy `B256`, `Address`, `FixedBytes`,
//     `Bloom`, `Bytes`) serialize as lowercase 0x-hex STRINGS — hexBytes below.
//     `Address` is lowercase, NOT EIP-55 checksummed.
//   - alloy `U256` serializes as a minimal hex QUANTITY ("0x0", "0x9") — u256 below.

// hexBytes is the wire form of any alloy fixed-width or opaque-bytes field
// (B256/Address/FixedBytes<N>/Bloom/Bytes): a lowercase, 0x-prefixed hex string
// whose length follows the byte content (32 for B256, 20 for Address, 8 for a
// nonce, 256 for a bloom, variable for Bytes). Empty encodes as "0x".
type hexBytes []byte

func (h hexBytes) MarshalJSON() ([]byte, error) {
	return json.Marshal("0x" + hex.EncodeToString(h))
}

func (h *hexBytes) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 { // tolerate odd-length hex (e.g. a trimmed quantity), left-pad a nibble
		s = "0" + s
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("l2rollup: decode hex field %q: %w", s, err)
	}
	*h = raw
	return nil
}

// byteList is the wire form of a serde `Vec<u8>`: a JSON array of numbers
// ([222,173]), never a base64 string. Used for the minimal big-endian
// EvmStorageProof value (`1`→[1], zero→[]). A nil list encodes as [].
type byteList []byte

func (b byteList) MarshalJSON() ([]byte, error) {
	nums := make([]uint16, len(b))
	for i, x := range b {
		nums[i] = uint16(x)
	}
	return json.Marshal(nums) // len 0 → "[]", never "null"
}

func (b *byteList) UnmarshalJSON(data []byte) error {
	var nums []uint16
	if err := json.Unmarshal(data, &nums); err != nil {
		return err
	}
	out := make([]byte, len(nums))
	for i, n := range nums {
		if n > 0xff {
			return fmt.Errorf("l2rollup: byte value %d out of range 0-255", n)
		}
		out[i] = byte(n)
	}
	*b = out
	return nil
}

// byteMatrix is the wire form of a serde `Vec<Vec<u8>>`: a JSON array of number
// arrays ([[248,81],[249,0,1]]). Used for EVM MPT proof node lists
// (EvmAccountProof.proof, EvmStorageProof.proof). A nil matrix encodes as [].
type byteMatrix [][]byte

func (m byteMatrix) MarshalJSON() ([]byte, error) {
	lists := make([]byteList, len(m))
	for i, node := range m {
		lists[i] = byteList(node)
	}
	return json.Marshal(lists) // len 0 → "[]", never "null"
}

func (m *byteMatrix) UnmarshalJSON(data []byte) error {
	var lists []byteList
	if err := json.Unmarshal(data, &lists); err != nil {
		return err
	}
	out := make([][]byte, len(lists))
	for i, l := range lists {
		out[i] = []byte(l)
	}
	*m = out
	return nil
}

// u256 is the wire form of an alloy `U256`: a minimal, lowercase, 0x-prefixed hex
// quantity ("0x0" for zero, "0x9" for nine) — NOT a fixed-width word. Used for the
// canonical header's difficulty and base_fee_per_gas.
type u256 struct{ big.Int }

func newU256(v uint64) u256 {
	var u u256
	u.SetUint64(v)
	return u
}

func (u u256) MarshalJSON() ([]byte, error) {
	return json.Marshal("0x" + u.Text(16))
}

func (u *u256) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		s = "0"
	}
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return fmt.Errorf("l2rollup: invalid u256 hex quantity %q", s)
	}
	u.Int = *v
	return nil
}

// clientMessageEnvelope is the serde-tagged ClientMessage the wasm client reads:
// `{"type":"header","value":<header>}` (serde tag="type", content="value",
// rename_all="snake_case"). Misbehaviour updates use this envelope too.
type clientMessageEnvelope struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// encodeHeaderMessage wraps a header in the ClientMessage envelope
// {"type":"header","value":<header>}. Arbitrum used to need a second, inner tagged
// envelope because its verifier `Header` was an enum with more than one variant; it
// is a bare struct now, like the others, so there is only the outer one.
func encodeHeaderMessage(header any) ([]byte, error) {
	value, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("l2rollup: marshal header value: %w", err)
	}
	out, err := json.Marshal(clientMessageEnvelope{Type: "header", Value: value})
	if err != nil {
		return nil, fmt.Errorf("l2rollup: marshal client message envelope: %w", err)
	}
	return out, nil
}

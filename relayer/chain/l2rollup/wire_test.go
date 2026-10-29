package l2rollup

import (
	"encoding/json"
	"testing"
)

func TestHexBytes_RoundTrip(t *testing.T) {
	cases := map[string]struct {
		bytes []byte
		json  string
	}{
		"empty":    {[]byte{}, `"0x"`},
		"b256":     {make([]byte, 32), `"0x0000000000000000000000000000000000000000000000000000000000000000"`},
		"address":  {[]byte{0xab, 0xCD, 0xef}, `"0xabcdef"`}, // lowercase, not checksummed
		"extradat": {[]byte{0xab, 0xcd}, `"0xabcd"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(hexBytes(tc.bytes))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.json {
				t.Fatalf("marshal = %s, want %s", got, tc.json)
			}
			var back hexBytes
			if err := json.Unmarshal([]byte(tc.json), &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if string(back) != string(tc.bytes) {
				t.Fatalf("round-trip bytes = %x, want %x", back, tc.bytes)
			}
		})
	}
}

// TestByteList_IsNumberArrayNotBase64 is the core encoding-contract regression:
// serde `Vec<u8>` is a JSON number array. Go's []byte would base64-encode; byteList
// must not.
func TestByteList_IsNumberArrayNotBase64(t *testing.T) {
	got, err := json.Marshal(byteList{222, 173})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != "[222,173]" {
		t.Fatalf("byteList marshal = %s, want [222,173] (base64 string means the []byte gotcha slipped in)", got)
	}
	// Empty value must be [] (serde zero storage value), never null or "".
	empty, _ := json.Marshal(byteList(nil))
	if string(empty) != "[]" {
		t.Fatalf("empty byteList = %s, want []", empty)
	}

	var back byteList
	if err := json.Unmarshal([]byte("[1]"), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != 1 || back[0] != 1 {
		t.Fatalf("round-trip = %v, want [1]", back)
	}
	if err := json.Unmarshal([]byte("[256]"), &back); err == nil {
		t.Fatal("expected out-of-range byte to be rejected")
	}
}

func TestByteMatrix_ArrayOfNumberArrays(t *testing.T) {
	got, err := json.Marshal(byteMatrix{{248, 81}, {249, 0, 1}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != "[[248,81],[249,0,1]]" {
		t.Fatalf("byteMatrix marshal = %s, want [[248,81],[249,0,1]]", got)
	}
	if empty, _ := json.Marshal(byteMatrix(nil)); string(empty) != "[]" {
		t.Fatalf("empty byteMatrix = %s, want []", empty)
	}
	var back byteMatrix
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != 2 || len(back[1]) != 3 || back[1][2] != 1 {
		t.Fatalf("round-trip = %v", back)
	}
}

func TestU256_HexQuantity(t *testing.T) {
	cases := map[uint64]string{0: `"0x0"`, 9: `"0x9"`, 255: `"0xff"`, 30000000: `"0x1c9c380"`}
	for v, want := range cases {
		got, err := json.Marshal(newU256(v))
		if err != nil {
			t.Fatalf("marshal %d: %v", v, err)
		}
		if string(got) != want {
			t.Fatalf("u256(%d) = %s, want %s", v, got, want)
		}
		var back u256
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", got, err)
		}
		if back.Uint64() != v {
			t.Fatalf("round-trip = %d, want %d", back.Uint64(), v)
		}
	}
}

func TestEncodeHeaderMessage_TaggedEnvelope(t *testing.T) {
	raw, err := encodeHeaderMessage(&SignedAttestedL2Header{L2Header: CanonicalEvmHeader{Number: 7}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var env clientMessageEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != "header" {
		t.Fatalf("envelope type = %q, want header", env.Type)
	}
	var inner SignedAttestedL2Header
	if err := json.Unmarshal(env.Value, &inner); err != nil {
		t.Fatalf("unmarshal value: %v", err)
	}
	if inner.L2Header.Number != 7 {
		t.Fatalf("value.l2_header.number = %d, want 7", inner.L2Header.Number)
	}
}

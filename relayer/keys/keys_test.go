package keys

import (
	"crypto/ecdsa"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

const knownPrivKeyHex = "fad9c8855b740a0b7ed4c221dbad0f33a83a49cad6b3fe8d5817ac83d38b6a19"

func TestRestoreKey(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid hex without 0x prefix", input: knownPrivKeyHex, wantErr: false},
		{name: "valid hex with 0x prefix", input: "0x" + knownPrivKeyHex, wantErr: false},
		{name: "invalid hex string", input: "notvalidhex", wantErr: true},
		{name: "empty string", input: "", wantErr: true},
		{name: "too short hex", input: "deadbeef", wantErr: true},
		{name: "0x prefix only", input: "0x", wantErr: true},
	}

	var keyWith, keyWithout *ecdsa.PrivateKey
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, err := RestoreKey(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if key == nil {
				t.Fatal("expected non-nil key")
			}
			if tc.name == "valid hex with 0x prefix" {
				keyWith = key
			}
			if tc.name == "valid hex without 0x prefix" {
				keyWithout = key
			}
		})
	}

	if keyWith != nil && keyWithout != nil && keyWith.D.Cmp(keyWithout.D) != 0 {
		t.Fatal("keys with and without 0x prefix differ")
	}
}

func TestPublicKey(t *testing.T) {
	privKey, err := RestoreKey(knownPrivKeyHex)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	pub, err := PublicKey(privKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub == nil {
		t.Fatal("expected non-nil public key")
	}

	wantPub := privKey.PublicKey
	if pub.X.Cmp(wantPub.X) != 0 || pub.Y.Cmp(wantPub.Y) != 0 {
		t.Fatal("public key does not match expected value")
	}

	gotAddr := crypto.PubkeyToAddress(*pub)
	wantAddr := crypto.PubkeyToAddress(wantPub)
	if gotAddr != wantAddr {
		t.Fatalf("address mismatch: got %s, want %s", gotAddr.Hex(), wantAddr.Hex())
	}
}

package attestation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignerBindsEveryWasmHeaderIdentityField(t *testing.T) {
	signer, err := NewSigner(8453, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	router := make([]byte, RouterLength)
	setHash := make([]byte, HashLength)
	blockHash := make([]byte, HashLength)
	stateRoot := make([]byte, HashLength)
	router[19] = 1
	setHash[31] = 2
	blockHash[31] = 3
	stateRoot[31] = 1
	signature, err := signer.Sign(router, setHash, 123, blockHash, stateRoot)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	const expectedPublicKey = "79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664"
	const expectedSignature = "bbf5d84390b7316391593425428d3e89c44f930795482bb5afaf4aa4b187c13436c0accbbe1ad323b99c39ed67b57894700a44553f62b46afcd3f4c8c882c105"
	if got := hex.EncodeToString(signer.PublicKey()); got != expectedPublicKey {
		t.Fatalf("public key = %s, want %s", got, expectedPublicKey)
	}
	if got := hex.EncodeToString(signature); got != expectedSignature {
		t.Fatalf("signature = %s, want %s", got, expectedSignature)
	}
	message, err := SigningBytes(8453, router, setHash, 123, blockHash, stateRoot)
	if err != nil {
		t.Fatalf("SigningBytes: %v", err)
	}
	if !ed25519.Verify(signer.PublicKey(), message, signature) {
		t.Fatal("valid block identity signature did not verify")
	}
	for _, bad := range [][]byte{
		mustSigningBytes(t, 8454, router, setHash, 123, blockHash, stateRoot),
		mustSigningBytes(t, 8453, append([]byte(nil), router[:19]...), setHash, 123, blockHash, stateRoot),
		mustSigningBytes(t, 8453, router, append([]byte(nil), stateRoot...), 123, blockHash, stateRoot),
		mustSigningBytes(t, 8453, router, setHash, 124, blockHash, stateRoot),
		mustSigningBytes(t, 8453, router, setHash, 123, stateRoot, stateRoot),
	} {
		if ed25519.Verify(signer.PublicKey(), bad, signature) {
			t.Fatal("signature verified for a different L2 block identity")
		}
	}
}

func mustSigningBytes(t *testing.T, chainID uint64, router, setHash []byte, blockNumber uint64, blockHash, stateRoot []byte) []byte {
	t.Helper()
	message, err := SigningBytes(chainID, router, setHash, blockNumber, blockHash, stateRoot)
	if err != nil {
		return []byte("different-invalid-statement")
	}
	return message
}

func TestWasmAttestationVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "wasm-contracts", "l2-attestation-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		ProtocolDomain string `json:"protocol_domain"`
		Cases          []struct {
			Name            string   `json:"name"`
			Threshold       uint16   `json:"threshold"`
			PublicKeys      []string `json:"public_keys"`
			AttestorSetHash string   `json:"attestor_set_hash"`
			Statement       string   `json:"statement"`
			Context         struct {
				L2ChainID     uint64 `json:"l2_chain_id"`
				RouterAddress string `json:"router_address"`
				BlockNumber   uint64 `json:"block_number"`
				BlockHash     string `json:"block_hash"`
				StateRoot     string `json:"state_root"`
			} `json:"context"`
			Signatures []struct {
				AttestorIndex uint16 `json:"attestor_index"`
				PublicKey     string `json:"public_key"`
				Signature     string `json:"signature"`
			} `json:"signatures"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ProtocolDomain[:]); got != vectors.ProtocolDomain {
		t.Fatalf("protocol domain = %s, want %s", got, vectors.ProtocolDomain)
	}
	for _, vector := range vectors.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			keys := make([][]byte, len(vector.PublicKeys))
			for i, encoded := range vector.PublicKeys {
				keys[i], err = base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
			}
			config := AttestorConfig{PublicKeys: keys, Threshold: vector.Threshold}
			setHash, err := config.SetHash()
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(setHash[:]); got != vector.AttestorSetHash {
				t.Fatalf("set hash = %s, want %s", got, vector.AttestorSetHash)
			}
			router, _ := hex.DecodeString(vector.Context.RouterAddress)
			blockHash, _ := hex.DecodeString(vector.Context.BlockHash)
			stateRoot, _ := hex.DecodeString(vector.Context.StateRoot)
			statement, err := SigningBytes(vector.Context.L2ChainID, router, setHash[:], vector.Context.BlockNumber, blockHash, stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := hex.DecodeString(vector.Statement)
			if !bytes.Equal(statement, want) {
				t.Fatalf("statement = %x, want %x", statement, want)
			}
			for _, signed := range vector.Signatures {
				key, _ := base64.StdEncoding.DecodeString(signed.PublicKey)
				signature, _ := base64.StdEncoding.DecodeString(signed.Signature)
				if !ed25519.Verify(key, statement, signature) {
					t.Fatalf("signature for index %d did not verify", signed.AttestorIndex)
				}
			}
		})
	}
}

func TestNewSignerValidatesKeyMaterialAndEnvironment(t *testing.T) {
	const seed = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	t.Setenv("ATTESTATION_TEST_SEED", seed)
	t.Setenv("ATTESTATION_TEST_MISSING", "")
	if _, err := NewSigner(10, "env:ATTESTATION_TEST_SEED"); err != nil {
		t.Fatalf("NewSigner from environment: %v", err)
	}

	for _, tc := range []struct {
		name    string
		chainID uint64
		key     string
		want    string
	}{
		{name: "zero chain ID", chainID: 0, key: seed, want: "chain ID"},
		{name: "invalid hex", chainID: 10, key: "not-hex", want: "decode"},
		{name: "wrong seed length", chainID: 10, key: "01", want: "32-byte"},
		{name: "missing environment key", chainID: 10, key: "env:ATTESTATION_TEST_MISSING", want: "unset"},
		{name: "empty environment variable name", chainID: 10, key: "env:", want: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSigner(tc.chainID, tc.key)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewSigner(%d, %q) error = %v, want %q", tc.chainID, tc.key, err, tc.want)
			}
		})
	}
}

func TestSigningBytesRejectsInvalidIdentity(t *testing.T) {
	validRouter := make([]byte, RouterLength)
	validHash := make([]byte, HashLength)
	for _, tc := range []struct {
		name      string
		chainID   uint64
		router    []byte
		setHash   []byte
		blockHash []byte
		stateRoot []byte
	}{
		{name: "zero chain ID", router: validRouter, setHash: validHash, blockHash: validHash, stateRoot: validHash},
		{name: "short router", chainID: 10, router: validRouter[:RouterLength-1], setHash: validHash, blockHash: validHash, stateRoot: validHash},
		{name: "short set hash", chainID: 10, router: validRouter, setHash: validHash[:HashLength-1], blockHash: validHash, stateRoot: validHash},
		{name: "short block hash", chainID: 10, router: validRouter, setHash: validHash, blockHash: validHash[:HashLength-1], stateRoot: validHash},
		{name: "short state root", chainID: 10, router: validRouter, setHash: validHash, blockHash: validHash, stateRoot: validHash[:HashLength-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := SigningBytes(tc.chainID, tc.router, tc.setHash, 1, tc.blockHash, tc.stateRoot); err == nil {
				t.Fatal("SigningBytes succeeded for invalid identity")
			}
		})
	}
}

func singleKeyConfig(key []byte) AttestorConfig {
	return AttestorConfig{PublicKeys: [][]byte{key}, Threshold: 1}
}

func TestValidateForInstallRejectsSmallOrderKeys(t *testing.T) {
	// Five y coordinates, each with either sign bit. A shorter list means an encoding was lost.
	if len(smallOrderPublicKeys) != 10 {
		t.Fatalf("small-order table has %d entries, want 10", len(smallOrderPublicKeys))
	}
	seen := map[string]bool{}
	for _, key := range smallOrderPublicKeys {
		if len(key) != PublicKeyLength || seen[string(key)] {
			t.Fatalf("small-order table entry %x is malformed or repeated", key)
		}
		seen[string(key)] = true

		config := singleKeyConfig(key)
		err := config.ValidateForInstall()
		if err == nil || !strings.Contains(err.Error(), "small-order") {
			t.Fatalf("ValidateForInstall(%x) = %v, want a small-order error", key, err)
		}
		// A client that already holds such a key must keep loading, and SetHash goes through Validate.
		if err := config.Validate(); err != nil {
			t.Fatalf("Validate(%x) = %v, want the on-every-call rules to keep accepting it", key, err)
		}
	}
}

func TestHasCanonicalYAgainstIntegerComparison(t *testing.T) {
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	top := new(big.Int).Lsh(big.NewInt(1), 255)
	for delta := int64(-20); delta <= 20; delta++ {
		y := new(big.Int).Add(p, big.NewInt(delta))
		if y.Cmp(top) >= 0 {
			continue // not a 255-bit value
		}
		bigEndian := y.FillBytes(make([]byte, PublicKeyLength))
		for _, sign := range []byte{0x00, 0x80} {
			key := make([]byte, PublicKeyLength)
			for i, b := range bigEndian {
				key[PublicKeyLength-1-i] = b
			}
			key[PublicKeyLength-1] |= sign
			if got, want := hasCanonicalY(key), y.Cmp(p) < 0; got != want {
				t.Fatalf("hasCanonicalY(p%+d, sign %#x) = %v, want %v", delta, sign, got, want)
			}
		}
	}
}

func TestValidateForInstallRejectsNonCanonicalY(t *testing.T) {
	// y = p and y = p + 1 are second encodings of y = 0 and y = 1, and y = 2^255 - 1 is p + 18.
	for _, hexKey := range []string{
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	} {
		err := singleKeyConfig(mustHex(hexKey)).ValidateForInstall()
		if err == nil || !strings.Contains(err.Error(), "non-canonical") {
			t.Fatalf("ValidateForInstall(%s) = %v, want a non-canonical error", hexKey, err)
		}
	}
}

func TestValidateForInstallAcceptsOrdinaryKeys(t *testing.T) {
	keys := [][]byte{}
	// The keys shipped in the example configs.
	for _, b64 := range []string{
		"XZ+76qOWCo/r113X56pAXt0hi3AZpY9LU44nF4vCMmg=",
		"gojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w=",
	} {
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	for i := 0; i < 64; i++ {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = byte(i)
		seed[1] = 0xa5
		keys = append(keys, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	}
	for _, key := range keys {
		if err := singleKeyConfig(key).ValidateForInstall(); err != nil {
			t.Fatalf("ValidateForInstall(%x) = %v, want an ordinary key accepted", key, err)
		}
	}
}

func TestValidateForInstallKeepsTheOnEveryCallRules(t *testing.T) {
	// The key rules index into the key, so Validate must reject a short one first.
	for _, config := range []AttestorConfig{
		{},
		{PublicKeys: [][]byte{make([]byte, PublicKeyLength-1)}, Threshold: 1},
		{PublicKeys: [][]byte{mustHex(strings.Repeat("11", 32)), mustHex(strings.Repeat("11", 32))}, Threshold: 1},
	} {
		if err := config.ValidateForInstall(); err == nil {
			t.Fatalf("ValidateForInstall(%+v) succeeded, want the Validate error", config)
		}
	}
}

// Package attestation defines the stable signed L2-block attestation format.
package attestation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

const (
	HashLength         = 32
	RouterLength       = 20
	SignatureLength    = ed25519.SignatureSize
	PublicKeyLength    = ed25519.PublicKeySize
	MaxAttestors       = 32
	StatementLength    = 164
	protocolDomainText = "SPECTRE_L2_ATTESTATION_V1"
)

// ProtocolDomain is SHA-256(UTF8("SPECTRE_L2_ATTESTATION_V1")). It is shared
// verbatim with packages/l2-client/src/verification.rs; do not change it without
// a coordinated wire-protocol migration.
var ProtocolDomain = sha256.Sum256([]byte(protocolDomainText))

// AttestorConfig is the immutable, canonically ordered Ed25519 attestor set
// stored in an L2 wasm ClientState. []byte uses base64 in JSON, the same form
// CosmWasm Binary uses.
type AttestorConfig struct {
	PublicKeys [][]byte `json:"public_keys"`
	Threshold  uint16   `json:"threshold"`
}

// smallOrderPublicKeys lists the compressed Ed25519 encodings of the points of order 1, 2, 4 and 8:
// five y coordinates, each valid with either sign bit. A key from this list satisfies the
// verification equation for signatures nobody made, so the wasm client refuses it at install time.
// The list mirrors SMALL_ORDER_ED25519_KEYS in packages/l2-client/src/state.rs.
var smallOrderPublicKeys = [][]byte{
	mustHex("0000000000000000000000000000000000000000000000000000000000000000"),
	mustHex("0000000000000000000000000000000000000000000000000000000000000080"),
	mustHex("0100000000000000000000000000000000000000000000000000000000000000"),
	mustHex("0100000000000000000000000000000000000000000000000000000000000080"),
	mustHex("26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05"),
	mustHex("26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85"),
	mustHex("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a"),
	mustHex("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa"),
	mustHex("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"),
	mustHex("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// Validate applies the bounds and ordering rules the wasm client applies on every call. It does
// not apply the stricter key rules the client applies when a set is installed, so that a set the
// client already holds keeps loading; ValidateForInstall adds them.
func (c AttestorConfig) Validate() error {
	if len(c.PublicKeys) == 0 {
		return fmt.Errorf("attestor set must contain at least one public key")
	}
	if len(c.PublicKeys) > MaxAttestors {
		return fmt.Errorf("attestor set has %d public keys, maximum is %d", len(c.PublicKeys), MaxAttestors)
	}
	if c.Threshold == 0 || int(c.Threshold) > len(c.PublicKeys) {
		return fmt.Errorf("attestor threshold %d must be between 1 and %d", c.Threshold, len(c.PublicKeys))
	}
	for i, key := range c.PublicKeys {
		if len(key) != PublicKeyLength {
			return fmt.Errorf("attestor public key %d must be %d bytes, got %d", i, PublicKeyLength, len(key))
		}
		if i != 0 && bytes.Compare(c.PublicKeys[i-1], key) >= 0 {
			return fmt.Errorf("attestor public keys must be strictly increasing lexicographically")
		}
	}
	return nil
}

// ValidateForInstall validates a set that is about to be installed, at client creation or on
// rotation. On top of Validate, every key must be a canonical Ed25519 encoding of a point that
// does not have small order, which is what the wasm client enforces (AttestorConfig::validate_for_install
// in packages/l2-client/src/state.rs). It is not part of Validate, because SetHash depends on
// Validate and a client that already holds such a key must keep working until it is rotated.
func (c AttestorConfig) ValidateForInstall() error {
	if err := c.Validate(); err != nil {
		return err
	}
	for i, key := range c.PublicKeys {
		if !hasCanonicalY(key) {
			return fmt.Errorf("attestor public key %d has a non-canonical y coordinate", i)
		}
		for _, weak := range smallOrderPublicKeys {
			if bytes.Equal(key, weak) {
				return fmt.Errorf("attestor public key %d is a small-order point", i)
			}
		}
	}
	return nil
}

// hasCanonicalY reports whether the low 255 bits of the key, its y coordinate, are below the field
// prime 2^255 - 19. RFC 8032 requires it, but ZIP-215 verification, which the wasm client's
// ed25519_verify follows, also accepts y in [p, 2^255), a second encoding of the point with y - p.
// The key must be PublicKeyLength bytes.
func hasCanonicalY(key []byte) bool {
	last := len(key) - 1
	// p is 0x7fff...ffed in little-endian bytes, so y >= p exactly when the top byte is 0x7f (sign
	// bit aside), every byte below it is 0xff, and the lowest byte is at least 0xed.
	if key[last]&0x7f != 0x7f {
		return true
	}
	for _, b := range key[1:last] {
		if b != 0xff {
			return true
		}
	}
	return key[0] < 0xed
}

// SetHash returns SHA-256(u16be(threshold) || u16be(count) || sorted keys),
// the value that every signature and the wasm client state bind to.
func (c AttestorConfig) SetHash() ([HashLength]byte, error) {
	if err := c.Validate(); err != nil {
		return [HashLength]byte{}, err
	}
	encoded := make([]byte, 4+len(c.PublicKeys)*PublicKeyLength)
	binary.BigEndian.PutUint16(encoded[0:2], c.Threshold)
	binary.BigEndian.PutUint16(encoded[2:4], uint16(len(c.PublicKeys)))
	offset := 4
	for _, key := range c.PublicKeys {
		offset += copy(encoded[offset:], key)
	}
	return sha256.Sum256(encoded), nil
}

// Signer owns the key used to attest canonical L2 block identities for one L2.
type Signer struct {
	chainID uint64
	key     ed25519.PrivateKey
}

// Configured reports whether the signer has usable key material. It lets
// transport adapters fail closed instead of accidentally returning a positive
// but unsigned verdict when composition omitted a per-chain signer.
func (s Signer) Configured() bool {
	return s.chainID != 0 && len(s.key) == ed25519.PrivateKeySize
}

// NewSigner parses a 32-byte Ed25519 seed encoded as 0x-prefixed or bare hex.
func NewSigner(chainID uint64, privateKeyHex string) (Signer, error) {
	if chainID == 0 {
		return Signer{}, fmt.Errorf("L2 chain ID must be non-zero")
	}
	privateKeyHex, err := resolvePrivateKey(privateKeyHex)
	if err != nil {
		return Signer{}, err
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(privateKeyHex, "0x"))
	if err != nil {
		return Signer{}, fmt.Errorf("decode attestation signing key: %w", err)
	}
	if len(raw) != ed25519.SeedSize {
		return Signer{}, fmt.Errorf("attestation signing key must be a %d-byte Ed25519 seed, got %d", ed25519.SeedSize, len(raw))
	}
	return Signer{chainID: chainID, key: ed25519.NewKeyFromSeed(raw)}, nil
}

func resolvePrivateKey(value string) (string, error) {
	if !strings.HasPrefix(value, "env:") {
		return value, nil
	}
	name := strings.TrimPrefix(value, "env:")
	if name == "" {
		return "", fmt.Errorf("attestation signing-key environment variable is empty")
	}
	key, ok := os.LookupEnv(name)
	if !ok || key == "" {
		return "", fmt.Errorf("attestation signing-key environment variable %q is unset", name)
	}
	return key, nil
}

// PublicKey returns the configured signer's 32-byte Ed25519 public key.
func (s Signer) PublicKey() []byte {
	return append([]byte(nil), s.key.Public().(ed25519.PublicKey)...)
}

// Sign signs one exact L2 wasm-header attestation statement. The finality mode is
// intentionally absent: it is an attestor-local policy, while every byte here is
// rechecked by the client contract.
func (s Signer) Sign(l2Router []byte, attestorSetHash []byte, blockNumber uint64, blockHash, stateRoot []byte) ([]byte, error) {
	message, err := SigningBytes(s.chainID, l2Router, attestorSetHash, blockNumber, blockHash, stateRoot)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(s.key, message), nil
}

// SigningBytes is the exact, fixed-width cross-language byte encoding verified by
// the L2 wasm client:
// SHA256("SPECTRE_L2_ATTESTATION_V1") || l2_chain_id (u64be) || l2_router
// (20 bytes) || attestor_set_hash (32 bytes) || l2_block_number (u64be) ||
// l2_block_hash (32 bytes) || state_root (32 bytes).
func SigningBytes(chainID uint64, l2Router, attestorSetHash []byte, blockNumber uint64, blockHash, stateRoot []byte) ([]byte, error) {
	if chainID == 0 {
		return nil, fmt.Errorf("L2 chain ID must be non-zero")
	}
	if len(l2Router) != RouterLength {
		return nil, fmt.Errorf("L2 router must be %d bytes, got %d", RouterLength, len(l2Router))
	}
	if len(attestorSetHash) != HashLength || len(blockHash) != HashLength || len(stateRoot) != HashLength {
		return nil, fmt.Errorf("attestor set hash, block hash, and state root must all be %d bytes", HashLength)
	}
	message := make([]byte, StatementLength)
	offset := copy(message, ProtocolDomain[:])
	binary.BigEndian.PutUint64(message[offset:], chainID)
	offset += 8
	offset += copy(message[offset:], l2Router)
	offset += copy(message[offset:], attestorSetHash)
	binary.BigEndian.PutUint64(message[offset:], blockNumber)
	offset += 8
	offset += copy(message[offset:], blockHash)
	copy(message[offset:], stateRoot)
	return message, nil
}

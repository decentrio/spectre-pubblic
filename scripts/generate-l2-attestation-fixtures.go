// Command generate-l2-attestation-fixtures writes deterministic cross-language
// Ed25519 fixtures consumed by the Rust and wasmvm conformance suites.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"golang.org/x/crypto/sha3"
)

const (
	protocolDomain = "SPECTRE_L2_ATTESTATION_V1"
	chainID        = uint64(11_155_420)
	blockNumber    = uint64(42)
	routerHex      = "645280885749dc97ea461de280eb3273c91d36df"
	blockHashHex   = "1112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f30"
	stateRootHex   = "3132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f50"
)

type vectorFile struct {
	ProtocolDomain string       `json:"protocol_domain"`
	Cases          []vectorCase `json:"cases"`
}

type vectorCase struct {
	Name            string             `json:"name"`
	Threshold       uint16             `json:"threshold"`
	PublicKeys      [][]byte           `json:"public_keys"`
	AttestorSetHash string             `json:"attestor_set_hash"`
	Context         attestationContext `json:"context"`
	Statement       string             `json:"statement"`
	Signatures      []vectorSignature  `json:"signatures"`
}

type attestationContext struct {
	L2ChainID   uint64 `json:"l2_chain_id"`
	Router      string `json:"router_address"`
	BlockNumber uint64 `json:"block_number"`
	BlockHash   string `json:"block_hash"`
	StateRoot   string `json:"state_root"`
}

type vectorSignature struct {
	AttestorIndex uint16 `json:"attestor_index"`
	PublicKey     []byte `json:"public_key"`
	Signature     []byte `json:"signature"`
}

type clientMessage struct {
	Type  string       `json:"type"`
	Value signedHeader `json:"value"`
}

type signedHeader struct {
	L2Header          canonicalHeader            `json:"l2_header"`
	RouterProof       accountProof               `json:"router_proof"`
	AttestorSignature []indexedAttestorSignature `json:"attestor_signature,omitempty"`
}

type indexedAttestorSignature struct {
	AttestorIndex uint16 `json:"attestor_index"`
	Signature     []byte `json:"signature"`
}

type accountProof struct {
	Proof [][]uint16 `json:"proof"`
}

type canonicalHeader struct {
	ParentHash            string `json:"parent_hash"`
	OmmersHash            string `json:"ommers_hash"`
	Beneficiary           string `json:"beneficiary"`
	StateRoot             string `json:"state_root"`
	TransactionsRoot      string `json:"transactions_root"`
	ReceiptsRoot          string `json:"receipts_root"`
	LogsBloom             string `json:"logs_bloom"`
	Difficulty            string `json:"difficulty"`
	Number                uint64 `json:"number"`
	GasLimit              uint64 `json:"gas_limit"`
	GasUsed               uint64 `json:"gas_used"`
	Timestamp             uint64 `json:"timestamp"`
	ExtraData             string `json:"extra_data"`
	MixHash               string `json:"mix_hash"`
	Nonce                 string `json:"nonce"`
	BaseFeePerGas         string `json:"base_fee_per_gas"`
	WithdrawalsRoot       string `json:"withdrawals_root"`
	BlobGasUsed           uint64 `json:"blob_gas_used"`
	ExcessBlobGas         uint64 `json:"excess_blob_gas"`
	ParentBeaconBlockRoot string `json:"parent_beacon_block_root"`
	RequestsHash          string `json:"requests_hash"`
}

type signer struct {
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
}

func main() {
	output := flag.String(
		"out",
		"../test/fixtures/wasm-contracts/l2-attestation-vectors.json",
		"output path, or - for stdout",
	)
	clientMessageOutput := flag.String("client-message-out", "", "optional signed client-message output path")
	unsignedMessageOutput := flag.String("unsigned-client-message-out", "", "optional unsigned negative client-message output path")
	flag.Parse()

	domain := sha256.Sum256([]byte(protocolDomain))
	vectors := vectorFile{
		ProtocolDomain: hex.EncodeToString(domain[:]),
		Cases: []vectorCase{
			buildCase("1-of-1", 1, 1),
			buildCase("2-of-3", 3, 2),
			buildCase("32-of-32", 32, 32),
		},
	}
	writeJSON(*output, vectors)

	if *clientMessageOutput != "" || *unsignedMessageOutput != "" {
		signed, unsigned := buildClientMessages()
		if *clientMessageOutput != "" {
			writeJSON(*clientMessageOutput, signed)
		}
		if *unsignedMessageOutput != "" {
			writeJSON(*unsignedMessageOutput, unsigned)
		}
	}
}

func writeJSON(path string, value any) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	encoded = append(encoded, '\n')
	if path == "-" {
		if _, err := os.Stdout.Write(encoded); err != nil {
			panic(err)
		}
		return
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		panic(err)
	}
}

func buildClientMessages() (clientMessage, clientMessage) {
	parentHash := mustDecode("1111111111111111111111111111111111111111111111111111111111111111", 32)
	ommersHash := mustDecode("2222222222222222222222222222222222222222222222222222222222222222", 32)
	beneficiary := mustDecode("3333333333333333333333333333333333333333", 20)
	stateRoot := mustDecode("4444444444444444444444444444444444444444444444444444444444444444", 32)
	transactionsRoot := mustDecode("5555555555555555555555555555555555555555555555555555555555555555", 32)
	receiptsRoot := mustDecode("6666666666666666666666666666666666666666666666666666666666666666", 32)
	mixHash := mustDecode("7777777777777777777777777777777777777777777777777777777777777777", 32)
	withdrawalsRoot := mustDecode("8888888888888888888888888888888888888888888888888888888888888888", 32)
	parentBeaconRoot := mustDecode("9999999999999999999999999999999999999999999999999999999999999999", 32)
	requestsHash := mustDecode("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 32)
	blobGasUsed, excessBlobGas := uint64(11), uint64(12)
	bloom := make([]byte, 256)
	nonce := make([]byte, 8)
	headerHash := keccak256(rlpList(
		rlpBytes(parentHash), rlpBytes(ommersHash), rlpBytes(beneficiary),
		rlpBytes(stateRoot), rlpBytes(transactionsRoot), rlpBytes(receiptsRoot),
		rlpBytes(bloom), rlpUint(0), rlpUint(blockNumber), rlpUint(30_000_000),
		rlpUint(21_000), rlpUint(1_700_000_000), rlpBytes(nil), rlpBytes(mixHash),
		rlpBytes(nonce), rlpUint(9), rlpBytes(withdrawalsRoot), rlpUint(blobGasUsed),
		rlpUint(excessBlobGas), rlpBytes(parentBeaconRoot), rlpBytes(requestsHash),
	))

	signers := signerSet(1)
	publicKeys := [][]byte{signers[0].publicKey}
	setHash := attestorSetHash(publicKeys, 1)
	statement := clientMessageStatement(setHash, headerHash, stateRoot)
	value := signedHeader{
		L2Header: canonicalHeader{
			ParentHash: hexValue(parentHash), OmmersHash: hexValue(ommersHash), Beneficiary: hexValue(beneficiary),
			StateRoot: hexValue(stateRoot), TransactionsRoot: hexValue(transactionsRoot), ReceiptsRoot: hexValue(receiptsRoot),
			LogsBloom: "0x" + fmt.Sprintf("%0512x", 0), Difficulty: "0x0", Number: blockNumber,
			GasLimit: 30_000_000, GasUsed: 21_000, Timestamp: 1_700_000_000,
			ExtraData: "0x", MixHash: hexValue(mixHash), Nonce: "0x0000000000000000", BaseFeePerGas: "0x9",
			WithdrawalsRoot: hexValue(withdrawalsRoot), BlobGasUsed: blobGasUsed, ExcessBlobGas: excessBlobGas,
			ParentBeaconBlockRoot: hexValue(parentBeaconRoot), RequestsHash: hexValue(requestsHash),
		},
		RouterProof: accountProof{Proof: [][]uint16{{1, 2}, {3}}},
		AttestorSignature: []indexedAttestorSignature{{
			AttestorIndex: 0,
			Signature:     ed25519.Sign(signers[0].privateKey, statement),
		}},
	}
	unsigned := value
	unsigned.AttestorSignature = nil
	return clientMessage{Type: "header", Value: value}, clientMessage{Type: "header", Value: unsigned}
}

func hexValue(value []byte) string { return "0x" + hex.EncodeToString(value) }

func keccak256(value []byte) []byte {
	hasher := sha3.NewLegacyKeccak256()
	_, _ = hasher.Write(value)
	return hasher.Sum(nil)
}

func rlpUint(value uint64) []byte {
	if value == 0 {
		return rlpBytes(nil)
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	first := 0
	for encoded[first] == 0 {
		first++
	}
	return rlpBytes(encoded[first:])
}

func rlpBytes(value []byte) []byte {
	if len(value) == 1 && value[0] < 0x80 {
		return append([]byte(nil), value...)
	}
	return append(rlpPrefix(0x80, len(value)), value...)
}

func rlpList(items ...[]byte) []byte {
	length := 0
	for _, item := range items {
		length += len(item)
	}
	payload := make([]byte, 0, length)
	for _, item := range items {
		payload = append(payload, item...)
	}
	return append(rlpPrefix(0xc0, len(payload)), payload...)
}

func rlpPrefix(shortBase byte, length int) []byte {
	if length <= 55 {
		return []byte{shortBase + byte(length)}
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(length))
	first := 0
	for encoded[first] == 0 {
		first++
	}
	lengthBytes := encoded[first:]
	return append([]byte{shortBase + 55 + byte(len(lengthBytes))}, lengthBytes...)
}

func clientMessageStatement(setHash [32]byte, blockHash, stateRoot []byte) []byte {
	domain := sha256.Sum256([]byte(protocolDomain))
	router := mustDecode(routerHex, 20)
	statement := make([]byte, 164)
	copy(statement[0:32], domain[:])
	binary.BigEndian.PutUint64(statement[32:40], chainID)
	copy(statement[40:60], router)
	copy(statement[60:92], setHash[:])
	binary.BigEndian.PutUint64(statement[92:100], blockNumber)
	copy(statement[100:132], blockHash)
	copy(statement[132:164], stateRoot)
	return statement
}

func buildCase(name string, members int, threshold uint16) vectorCase {
	signers := signerSet(members)
	publicKeys := make([][]byte, len(signers))
	for index, item := range signers {
		publicKeys[index] = append([]byte(nil), item.publicKey...)
	}
	setHash := attestorSetHash(publicKeys, threshold)
	statement := attestationStatement(setHash)
	signatures := make([]vectorSignature, threshold)
	for index := range signatures {
		signatures[index] = vectorSignature{
			AttestorIndex: uint16(index),
			PublicKey:     append([]byte(nil), signers[index].publicKey...),
			Signature:     ed25519.Sign(signers[index].privateKey, statement),
		}
	}
	return vectorCase{
		Name:            name,
		Threshold:       threshold,
		PublicKeys:      publicKeys,
		AttestorSetHash: hex.EncodeToString(setHash[:]),
		Context: attestationContext{
			L2ChainID:   chainID,
			Router:      routerHex,
			BlockNumber: blockNumber,
			BlockHash:   blockHashHex,
			StateRoot:   stateRootHex,
		},
		Statement:  hex.EncodeToString(statement),
		Signatures: signatures,
	}
}

func signerSet(members int) []signer {
	signers := make([]signer, members)
	for member := range signers {
		seed := make([]byte, ed25519.SeedSize)
		for index := range seed {
			seed[index] = byte(member + 1)
		}
		privateKey := ed25519.NewKeyFromSeed(seed)
		signers[member] = signer{
			publicKey:  privateKey.Public().(ed25519.PublicKey),
			privateKey: privateKey,
		}
	}
	sort.Slice(signers, func(left, right int) bool {
		return string(signers[left].publicKey) < string(signers[right].publicKey)
	})
	return signers
}

func attestorSetHash(publicKeys [][]byte, threshold uint16) [32]byte {
	encoded := make([]byte, 4, 4+len(publicKeys)*ed25519.PublicKeySize)
	binary.BigEndian.PutUint16(encoded[0:2], threshold)
	binary.BigEndian.PutUint16(encoded[2:4], uint16(len(publicKeys)))
	for _, publicKey := range publicKeys {
		encoded = append(encoded, publicKey...)
	}
	return sha256.Sum256(encoded)
}

func attestationStatement(setHash [32]byte) []byte {
	domain := sha256.Sum256([]byte(protocolDomain))
	router := mustDecode(routerHex, 20)
	blockHash := mustDecode(blockHashHex, 32)
	stateRoot := mustDecode(stateRootHex, 32)
	statement := make([]byte, 164)
	copy(statement[0:32], domain[:])
	binary.BigEndian.PutUint64(statement[32:40], chainID)
	copy(statement[40:60], router)
	copy(statement[60:92], setHash[:])
	binary.BigEndian.PutUint64(statement[92:100], blockNumber)
	copy(statement[100:132], blockHash)
	copy(statement[132:164], stateRoot)
	return statement
}

func mustDecode(encoded string, expectedLength int) []byte {
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != expectedLength {
		panic(fmt.Sprintf("invalid fixed fixture value %q", encoded))
	}
	return decoded
}

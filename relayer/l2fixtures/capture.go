// Package l2fixtures captures fixed-block rollup evidence for offline Rust verifier tests.
package l2fixtures

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

const schemaVersion = 2

// Config identifies one immutable configuration and the exact blocks from which to capture proofs.
// RPC URLs are supplied at run time and never written into fixture output.
type Config struct {
	ConfigID               string         `json:"config_id"`
	EthereumSourceRevision string         `json:"ethereum_source_revision"`
	RollupSourceRevision   string         `json:"rollup_source_revision"`
	ProfileSHA256          string         `json:"profile_sha256"`
	L1RPCURL               string         `json:"l1_rpc_url"`
	L2RPCURL               string         `json:"l2_rpc_url"`
	L1BlockNumber          uint64         `json:"l1_block_number"`
	L1BeaconSlot           uint64         `json:"l1_beacon_slot"`
	L2BlockNumber          uint64         `json:"l2_block_number"`
	ProofRequests          []ProofRequest `json:"proof_requests"`
}

// ProofRequest is an explicitly named account/storage proof. Its role is part of the reviewable
// fixture contract; callers cannot submit an anonymous proof bag.
type ProofRequest struct {
	Role        string   `json:"role"`
	Chain       string   `json:"chain"`
	Address     string   `json:"address"`
	StorageKeys []string `json:"storage_keys"`
}

// Fixture is the deterministic evidence payload. Proof responses remain raw JSON so the Rust
// verifier owns all semantic decoding and validation.
type Fixture struct {
	SchemaVersion uint32          `json:"schema_version"`
	ConfigID      string          `json:"config_id"`
	L1Block       Block           `json:"l1_block"`
	L2Block       Block           `json:"l2_block"`
	Proofs        []CapturedProof `json:"proofs"`
}

// Block pins a JSON-RPC proof response to a numeric block and its returned hash.
type Block struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

// CapturedProof preserves the request and provider response together for offline replay.
type CapturedProof struct {
	Role     string          `json:"role"`
	Chain    string          `json:"chain"`
	Address  string          `json:"address"`
	Response json.RawMessage `json:"response"`
}

// Provenance is written adjacent to a fixture and is consumed by l2-client::load_fixture.
type Provenance struct {
	SchemaVersion          uint32 `json:"schema_version"`
	ConfigID               string `json:"config_id"`
	L1BlockNumber          uint64 `json:"l1_block_number"`
	L1BlockHash            string `json:"l1_block_hash"`
	L2BlockNumber          uint64 `json:"l2_block_number"`
	L2BlockHash            string `json:"l2_block_hash"`
	L1BeaconSlot           uint64 `json:"l1_beacon_slot"`
	EthereumSourceRevision string `json:"ethereum_source_revision"`
	RollupSourceRevision   string `json:"rollup_source_revision"`
	ProfileSHA256          string `json:"profile_sha256"`
	FixtureSHA256          string `json:"fixture_sha256"`
}

// Caller is the small JSON-RPC surface needed by capture; tests use it to replay fixed responses.
type Caller interface {
	CallContext(context.Context, interface{}, string, ...interface{}) error
}

// Capture validates config, fetches fixed blocks and proofs, and returns deterministic payloads.
func Capture(ctx context.Context, config Config, l1, l2 Caller) (Fixture, Provenance, error) {
	if err := config.Validate(); err != nil {
		return Fixture{}, Provenance{}, err
	}
	l1Block, err := blockAt(ctx, l1, config.L1BlockNumber)
	if err != nil {
		return Fixture{}, Provenance{}, fmt.Errorf("fetch L1 block: %w", err)
	}
	l2Block, err := blockAt(ctx, l2, config.L2BlockNumber)
	if err != nil {
		return Fixture{}, Provenance{}, fmt.Errorf("fetch L2 block: %w", err)
	}

	proofs := make([]CapturedProof, 0, len(config.ProofRequests))
	for _, request := range config.ProofRequests {
		client, block := l1, l1Block
		if request.Chain == "l2" {
			client, block = l2, l2Block
		}
		response, err := proofAt(ctx, client, request, block.Number)
		if err != nil {
			return Fixture{}, Provenance{}, fmt.Errorf("capture %s proof: %w", request.Role, err)
		}
		proofs = append(proofs, CapturedProof{
			Role: request.Role, Chain: request.Chain, Address: request.Address, Response: response,
		})
	}
	sort.Slice(proofs, func(i, j int) bool { return proofs[i].Role < proofs[j].Role })

	fixture := Fixture{
		SchemaVersion: schemaVersion,
		ConfigID:      config.ConfigID,
		L1Block:       l1Block,
		L2Block:       l2Block,
		Proofs:        proofs,
	}
	fixtureBytes, err := json.Marshal(fixture)
	if err != nil {
		return Fixture{}, Provenance{}, fmt.Errorf("encode fixture: %w", err)
	}
	digest := sha256.Sum256(fixtureBytes)
	provenance := Provenance{
		SchemaVersion:          schemaVersion,
		ConfigID:               config.ConfigID,
		L1BlockNumber:          l1Block.Number,
		L1BlockHash:            l1Block.Hash,
		L2BlockNumber:          l2Block.Number,
		L2BlockHash:            l2Block.Hash,
		L1BeaconSlot:           config.L1BeaconSlot,
		EthereumSourceRevision: config.EthereumSourceRevision,
		RollupSourceRevision:   config.RollupSourceRevision,
		ProfileSHA256:          config.ProfileSHA256,
		FixtureSHA256:          hex.EncodeToString(digest[:]),
	}
	return fixture, provenance, nil
}

// WriteCapture writes a fixture and its manifest to outDir without recording RPC URLs.
func WriteCapture(outDir string, fixture Fixture, provenance Provenance) error {
	fixtureBytes, err := json.Marshal(fixture)
	if err != nil {
		return fmt.Errorf("encode fixture: %w", err)
	}
	digest := sha256.Sum256(fixtureBytes)
	if provenance.FixtureSHA256 != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("provenance fixture SHA-256 does not match fixture payload")
	}
	manifestBytes, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provenance: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "fixture.json"), fixtureBytes, 0o644); err != nil {
		return fmt.Errorf("write fixture: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "provenance.json"), manifestBytes, 0o644); err != nil {
		return fmt.Errorf("write provenance: %w", err)
	}
	return nil
}

// DialAndCapture is the network entry point used by the capture command.
func DialAndCapture(ctx context.Context, config Config) (Fixture, Provenance, error) {
	l1, err := rpc.DialContext(ctx, config.L1RPCURL)
	if err != nil {
		return Fixture{}, Provenance{}, fmt.Errorf("dial L1 RPC: %w", err)
	}
	defer l1.Close()
	l2, err := rpc.DialContext(ctx, config.L2RPCURL)
	if err != nil {
		return Fixture{}, Provenance{}, fmt.Errorf("dial L2 RPC: %w", err)
	}
	defer l2.Close()
	return Capture(ctx, config, l1, l2)
}

// Validate refuses tags and ambiguous requests before any network operation occurs.
func (c Config) Validate() error {
	if c.ConfigID == "" || c.EthereumSourceRevision == "" || c.RollupSourceRevision == "" || c.L1RPCURL == "" || c.L2RPCURL == "" {
		return fmt.Errorf("config_id, ethereum_source_revision, rollup_source_revision, l1_rpc_url, and l2_rpc_url are required")
	}
	if !validDigest(c.ProfileSHA256) {
		return fmt.Errorf("profile_sha256 must be a lowercase SHA-256 digest")
	}
	if c.L1BlockNumber == 0 || c.L1BeaconSlot == 0 || c.L2BlockNumber == 0 {
		return fmt.Errorf("L1 block number, beacon slot, and L2 block number must be non-zero")
	}
	if len(c.ProofRequests) == 0 {
		return fmt.Errorf("at least one explicitly named proof request is required")
	}
	roles := make(map[string]struct{}, len(c.ProofRequests))
	for _, request := range c.ProofRequests {
		if request.Role == "" || (request.Chain != "l1" && request.Chain != "l2") {
			return fmt.Errorf("every proof request needs a role and l1 or l2 chain")
		}
		if !common.IsHexAddress(request.Address) {
			return fmt.Errorf("proof request %q has an invalid address", request.Role)
		}
		if _, exists := roles[request.Role]; exists {
			return fmt.Errorf("duplicate proof role %q", request.Role)
		}
		roles[request.Role] = struct{}{}
		for _, key := range request.StorageKeys {
			if len(key) != 66 || !common.IsHexHash(key) {
				return fmt.Errorf("proof request %q has an invalid storage key", request.Role)
			}
		}
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func blockAt(ctx context.Context, client Caller, number uint64) (Block, error) {
	var response struct {
		Number string `json:"number"`
		Hash   string `json:"hash"`
	}
	if err := client.CallContext(ctx, &response, "eth_getBlockByNumber", blockTag(number), false); err != nil {
		return Block{}, err
	}
	if response.Hash == "" || !common.IsHexHash(response.Hash) {
		return Block{}, fmt.Errorf("response has no valid block hash")
	}
	var parsedNumber uint64
	if _, err := fmt.Sscanf(response.Number, "0x%x", &parsedNumber); err != nil || parsedNumber != number {
		return Block{}, fmt.Errorf("response block number %s does not match requested %s", response.Number, blockTag(number))
	}
	return Block{Number: number, Hash: response.Hash}, nil
}

func proofAt(ctx context.Context, client Caller, request ProofRequest, number uint64) (json.RawMessage, error) {
	var response json.RawMessage
	if err := client.CallContext(ctx, &response, "eth_getProof", request.Address, request.StorageKeys, blockTag(number)); err != nil {
		return nil, err
	}
	var validated struct {
		Address      string            `json:"address"`
		AccountProof []string          `json:"accountProof"`
		StorageHash  string            `json:"storageHash"`
		StorageProof []json.RawMessage `json:"storageProof"`
	}
	if err := json.Unmarshal(response, &validated); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if !common.IsHexAddress(validated.Address) || !common.IsHexHash(validated.StorageHash) || len(validated.AccountProof) == 0 {
		return nil, fmt.Errorf("response is not a complete account proof")
	}
	if common.HexToAddress(validated.Address) != common.HexToAddress(request.Address) {
		return nil, fmt.Errorf("response address %s does not match requested address %s", validated.Address, request.Address)
	}
	if len(validated.StorageProof) != len(request.StorageKeys) {
		return nil, fmt.Errorf("response returned %d storage proofs for %d requested keys", len(validated.StorageProof), len(request.StorageKeys))
	}
	for i, raw := range validated.StorageProof {
		var sp struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(raw, &sp); err != nil {
			return nil, fmt.Errorf("decode storage proof %d: %w", i, err)
		}
		if common.HexToHash(sp.Key) != common.HexToHash(request.StorageKeys[i]) {
			return nil, fmt.Errorf("storage proof %d key %s does not match requested key %s", i, sp.Key, request.StorageKeys[i])
		}
	}
	return response, nil
}

func blockTag(number uint64) string {
	return fmt.Sprintf("0x%x", number)
}

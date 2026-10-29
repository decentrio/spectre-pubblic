package l2fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureCaller struct {
	blockHash string
	proof     json.RawMessage
	blockNum  uint64
}

func (c fixtureCaller) CallContext(_ context.Context, result interface{}, method string, args ...interface{}) error {
	switch method {
	case "eth_getBlockByNumber":
		tag := fmt.Sprintf("0x%x", c.blockNum)
		return json.Unmarshal([]byte(fmt.Sprintf(`{"number":%q,"hash":%q}`, tag, c.blockHash)), result)
	case "eth_getProof":
		return json.Unmarshal(c.proof, result)
	default:
		return fmt.Errorf("unexpected method %s", method)
	}
}

func validConfig() Config {
	return Config{
		ConfigID: "op-mainnet-v1", EthereumSourceRevision: "ethereum-client@v1.2.0",
		RollupSourceRevision: "op-contracts@v1.8.0",
		ProfileSHA256:        "1111111111111111111111111111111111111111111111111111111111111111",
		L1RPCURL:             "https://l1.invalid", L2RPCURL: "https://l2.invalid",
		L1BlockNumber: 1, L1BeaconSlot: 1, L2BlockNumber: 2,
		ProofRequests: []ProofRequest{{
			Role: "l1_factory", Chain: "l1", Address: "0x0000000000000000000000000000000000000001",
		}},
	}
}

func TestCapture_PinsBlocksAndProducesAFixtureChecksum(t *testing.T) {
	proof := json.RawMessage(`{
		"address":"0x0000000000000000000000000000000000000001",
		"accountProof":["0x01"],
		"storageHash":"0x0000000000000000000000000000000000000000000000000000000000000001",
		"storageProof":[]
	}`)
	fixture, manifest, err := Capture(
		context.Background(), validConfig(),
		fixtureCaller{blockNum: 1, blockHash: "0x0000000000000000000000000000000000000000000000000000000000000002", proof: proof},
		fixtureCaller{blockNum: 2, blockHash: "0x0000000000000000000000000000000000000000000000000000000000000003", proof: proof},
	)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if fixture.L1Block.Number != 1 || fixture.L2Block.Number != 2 || len(fixture.Proofs) != 1 {
		t.Fatalf("unexpected fixture: %#v", fixture)
	}
	if manifest.SchemaVersion != 2 || len(manifest.FixtureSHA256) != 64 || manifest.L2BlockHash != fixture.L2Block.Hash {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(manifestJSON, &fields); err != nil {
		t.Fatalf("decode manifest fields: %v", err)
	}
	for _, required := range []string{
		"schema_version", "config_id", "l1_block_number", "l1_block_hash",
		"l2_block_number", "l2_block_hash", "l1_beacon_slot",
		"ethereum_source_revision", "rollup_source_revision",
		"profile_sha256", "fixture_sha256",
	} {
		if _, ok := fields[required]; !ok {
			t.Fatalf("manifest is missing loader field %q: %s", required, manifestJSON)
		}
	}
	if len(fields) != 11 {
		t.Fatalf("manifest has fields rejected by the strict loader: %s", manifestJSON)
	}
}

func TestConfig_RejectsDuplicateRolesBeforeNetworkAccess(t *testing.T) {
	config := validConfig()
	config.ProofRequests = append(config.ProofRequests, config.ProofRequests[0])
	if err := config.Validate(); err == nil {
		t.Fatal("expected duplicate proof role rejection")
	}
}

func TestConfig_RejectsInvalidProfileDigestBeforeNetworkAccess(t *testing.T) {
	config := validConfig()
	config.ProfileSHA256 = "ABC"
	if err := config.Validate(); err == nil {
		t.Fatal("expected invalid profile digest rejection")
	}
}

func TestCapture_RejectsMismatchedBlockNumber(t *testing.T) {
	proof := json.RawMessage(`{
		"address":"0x0000000000000000000000000000000000000001",
		"accountProof":["0x01"],
		"storageHash":"0x0000000000000000000000000000000000000000000000000000000000000001",
		"storageProof":[]
	}`)
	_, _, err := Capture(
		context.Background(), validConfig(),
		fixtureCaller{blockNum: 1, blockHash: "0x0000000000000000000000000000000000000000000000000000000000000002", proof: proof},
		fixtureCaller{blockNum: 1, blockHash: "0x0000000000000000000000000000000000000000000000000000000000000003", proof: proof},
	)
	if err == nil || !strings.Contains(err.Error(), "fetch L2 block") {
		t.Fatalf("expected mismatched L2 block rejection, got %v", err)
	}
}

func TestProofAt_RejectsMismatchedResponseIdentity(t *testing.T) {
	request := ProofRequest{
		Address: "0x0000000000000000000000000000000000000001",
		StorageKeys: []string{
			"0x0000000000000000000000000000000000000000000000000000000000000001",
		},
	}
	valid := `"accountProof":["0x01"],"storageHash":"0x0000000000000000000000000000000000000000000000000000000000000001"`
	for name, proof := range map[string]json.RawMessage{
		"address":     json.RawMessage(`{"address":"0x0000000000000000000000000000000000000002",` + valid + `,"storageProof":[{"key":"0x0000000000000000000000000000000000000000000000000000000000000001"}]}`),
		"storage key": json.RawMessage(`{"address":"0x0000000000000000000000000000000000000001",` + valid + `,"storageProof":[{"key":"0x0000000000000000000000000000000000000000000000000000000000000002"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := proofAt(context.Background(), fixtureCaller{proof: proof}, request, 1)
			if err == nil || !strings.Contains(err.Error(), "does not match requested") {
				t.Fatalf("expected mismatched %s rejection, got %v", name, err)
			}
		})
	}
}

func TestWriteCapture_RejectsAManifestForDifferentFixtureBytes(t *testing.T) {
	fixture := Fixture{SchemaVersion: schemaVersion, ConfigID: "configuration"}
	manifest := Provenance{FixtureSHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
	if err := WriteCapture(filepath.Join(t.TempDir(), "fixture"), fixture, manifest); err == nil {
		t.Fatal("expected mismatched provenance rejection")
	}
}

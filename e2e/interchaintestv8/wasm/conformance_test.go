//go:build cgo && !nolink_libwasmvm

package wasm

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"

	wasmvm "github.com/CosmWasm/wasmvm/v2"
	vmtypes "github.com/CosmWasm/wasmvm/v2/types"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/herumi/bls-eth-go-binary/bls"
)

const hostGasLimit = uint64(2_000_000_000_000)

type conformanceGas struct {
	StoreCode           uint64 `json:"store_code"`
	Instantiate         uint64 `json:"instantiate"`
	Status              uint64 `json:"status"`
	DelayError          uint64 `json:"delay_error"`
	VerifyClientMessage uint64 `json:"verify_client_message,omitempty"`
	Verify2Of3          uint64 `json:"verify_2_of_3,omitempty"`
	Verify32Of32        uint64 `json:"verify_32_of_32,omitempty"`
	UpdateState         uint64 `json:"update_state,omitempty"`
	Membership          uint64 `json:"membership,omitempty"`
	MigrateKeep         uint64 `json:"migrate_keep,omitempty"`
	MigrateReplace      uint64 `json:"migrate_replace,omitempty"`
}

type proofAccount struct {
	Nonce       uint64
	Balance     *big.Int
	StorageRoot common.Hash
	CodeHash    common.Hash
}

type memoryStore map[string][]byte

func (s memoryStore) Get(key []byte) []byte { return append([]byte(nil), s[string(key)]...) }
func (s memoryStore) Set(key, value []byte) { s[string(key)] = append([]byte(nil), value...) }
func (s memoryStore) Delete(key []byte)     { delete(s, string(key)) }
func (s memoryStore) Iterator(_, _ []byte) vmtypes.Iterator {
	return emptyIterator{}
}
func (s memoryStore) ReverseIterator(_, _ []byte) vmtypes.Iterator {
	return emptyIterator{}
}

type emptyIterator struct{}

func (emptyIterator) Domain() ([]byte, []byte) { return nil, nil }
func (emptyIterator) Valid() bool              { return false }
func (emptyIterator) Next()                    { panic("Next called on empty iterator") }
func (emptyIterator) Key() []byte              { panic("Key called on empty iterator") }
func (emptyIterator) Value() []byte            { panic("Value called on empty iterator") }
func (emptyIterator) Error() error             { return nil }
func (emptyIterator) Close() error             { return nil }

type zeroGasMeter struct{}

func (zeroGasMeter) GasConsumed() uint64 { return 0 }

type rejectingQuerier struct{}

func (rejectingQuerier) Query(vmtypes.QueryRequest, uint64) ([]byte, error) {
	return nil, errors.New("custom queries are unavailable in conformance fixtures")
}
func (rejectingQuerier) GasConsumed() uint64 { return 0 }

type ethereumBLSQuerier struct{}

var (
	blsInitOnce sync.Once
	blsInitErr  error
)

func (ethereumBLSQuerier) Query(request vmtypes.QueryRequest, _ uint64) ([]byte, error) {
	blsInitOnce.Do(func() {
		blsInitErr = bls.Init(bls.BLS12_381)
	})
	if blsInitErr != nil {
		return nil, blsInitErr
	}
	var query struct {
		AggregateVerify *struct {
			PublicKeys [][]byte `json:"public_keys"`
			Message    []byte   `json:"message"`
			Signature  []byte   `json:"signature"`
		} `json:"aggregate_verify"`
		Aggregate *struct {
			PublicKeys [][]byte `json:"public_keys"`
		} `json:"aggregate"`
	}
	if err := json.Unmarshal(request.Custom, &query); err != nil {
		return nil, err
	}
	if query.AggregateVerify != nil {
		publicKeys, err := deserializeBLSPublicKeys(query.AggregateVerify.PublicKeys)
		if err != nil {
			return nil, err
		}
		var signature bls.Sign
		if err := signature.Deserialize(query.AggregateVerify.Signature); err != nil {
			return nil, err
		}
		return json.Marshal(signature.FastAggregateVerify(publicKeys, query.AggregateVerify.Message))
	}
	if query.Aggregate != nil {
		publicKeys, err := deserializeBLSPublicKeys(query.Aggregate.PublicKeys)
		if err != nil {
			return nil, err
		}
		if len(publicKeys) == 0 {
			return nil, errors.New("cannot aggregate an empty BLS public-key set")
		}
		aggregate := publicKeys[0]
		for index := 1; index < len(publicKeys); index++ {
			aggregate.Add(&publicKeys[index])
		}
		return json.Marshal(aggregate.Serialize())
	}
	return nil, errors.New("unsupported Ethereum custom query")
}

func (ethereumBLSQuerier) GasConsumed() uint64 { return 0 }

func deserializeBLSPublicKeys(encoded [][]byte) ([]bls.PublicKey, error) {
	publicKeys := make([]bls.PublicKey, len(encoded))
	for index := range encoded {
		if err := publicKeys[index].Deserialize(encoded[index]); err != nil {
			return nil, err
		}
	}
	return publicKeys, nil
}

func hostAPI() wasmvm.GoAPI {
	return wasmvm.GoAPI{
		HumanizeAddress: func(address []byte) (string, uint64, error) {
			return string(address), 0, nil
		},
		CanonicalizeAddress: func(address string) ([]byte, uint64, error) {
			return []byte(address), 0, nil
		},
		ValidateAddress: func(string) (uint64, error) { return 0, nil },
	}
}

func hostEnv() vmtypes.Env {
	return vmtypes.Env{
		Block: vmtypes.BlockInfo{
			Height:  1,
			Time:    vmtypes.Uint64(1_700_000_000_000_000_000),
			ChainID: "wasm-conformance-1",
		},
		Contract: vmtypes.ContractInfo{Address: "wasm-client"},
	}
}

func hostInfo() vmtypes.MessageInfo {
	return vmtypes.MessageInfo{Sender: "08-wasm", Funds: vmtypes.Array[vmtypes.Coin]{}}
}

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func snapshot(store memoryStore) string {
	keys := make([]string, 0, len(store))
	for key := range store {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		hash.Write([]byte(key))
		hash.Write(store[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func requireContractError(t *testing.T, result *vmtypes.ContractResult, expected string) {
	t.Helper()
	if result == nil || result.Err != expected {
		t.Fatalf("contract error = %#v, want %q", result, expected)
	}
}

func requireDataOnly(t *testing.T, result *vmtypes.ContractResult, expected []byte) {
	t.Helper()
	if result == nil || result.Ok == nil {
		t.Fatalf("missing successful contract response: %#v", result)
	}
	if len(result.Ok.Messages) != 0 || len(result.Ok.Attributes) != 0 || len(result.Ok.Events) != 0 {
		t.Fatalf("response contains forbidden host output: %#v", result.Ok)
	}
	if !bytes.Equal(result.Ok.Data, expected) {
		t.Fatalf("response data = %q, want %q", result.Ok.Data, expected)
	}
}

func TestOptimizedArtifactsConformToPinnedWasmVM(t *testing.T) {
	repoRoot := os.Getenv("WASM_CONFORMANCE_REPO_ROOT")
	if repoRoot == "" {
		_, sourceFile, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("resolve conformance test source path")
		}
		repoRoot = filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	}

	artifactDir := os.Getenv("WASM_CONFORMANCE_ARTIFACT_DIR")
	if artifactDir == "" {
		artifactDir = filepath.Join(repoRoot, "artifacts")
		for _, artifact := range []string{
			"cw_ics08_wasm_op.wasm",
			"cw_ics08_wasm_base.wasm",
			"cw_ics08_wasm_arbitrum.wasm",
			"cw_ics08_wasm_eth.wasm",
		} {
			if _, err := os.Stat(filepath.Join(artifactDir, artifact)); os.IsNotExist(err) {
				t.Skipf("optimized artifact %s is absent; run VALIDATE_OPTIMIZED=1 scripts/validate-l2-clients.sh", artifact) // wasm-conformance: allow-env-skip
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}

	gasReport := make(map[string]conformanceGas)
	profiles := map[string]string{
		"cw_ics08_wasm_op.wasm":       "packages/l2-op-stack/config/op-sepolia.json",
		"cw_ics08_wasm_base.wasm":     "packages/l2-op-stack/config/base-sepolia.json",
		"cw_ics08_wasm_arbitrum.wasm": "packages/l2-arbitrum/config/arbitrum-sepolia.json",
	}
	for artifact, profilePath := range profiles {
		artifact, profilePath := artifact, profilePath
		t.Run(artifact, func(t *testing.T) {
			gasReport[artifact] = validateL2Artifact(t, repoRoot, artifactDir, artifact, profilePath)
		})
	}
	t.Run("cw_ics08_wasm_eth.wasm", func(t *testing.T) {
		gasReport["cw_ics08_wasm_eth.wasm"] = validateEthereumArtifact(t, repoRoot, artifactDir)
	})
	if baselinePath := os.Getenv("WASM_CONFORMANCE_GAS_BASELINE"); baselinePath != "" {
		assertGasRegressionBound(t, baselinePath, gasReport)
	}

	if reportPath := os.Getenv("WASM_CONFORMANCE_REPORT"); reportPath != "" {
		encoded, err := json.MarshalIndent(gasReport, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, '\n')
		if err := os.WriteFile(reportPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGasRegressionBound(t *testing.T, path string, actual map[string]conformanceGas) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline := make(map[string]conformanceGas)
	if err := json.Unmarshal(encoded, &baseline); err != nil {
		t.Fatal(err)
	}
	for artifact, current := range actual {
		approved, ok := baseline[artifact]
		if !ok {
			t.Fatalf("gas baseline is missing %s", artifact)
		}
		metrics := map[string][2]uint64{
			"store_code":            {approved.StoreCode, current.StoreCode},
			"instantiate":           {approved.Instantiate, current.Instantiate},
			"status":                {approved.Status, current.Status},
			"delay_error":           {approved.DelayError, current.DelayError},
			"verify_client_message": {approved.VerifyClientMessage, current.VerifyClientMessage},
			"verify_2_of_3":         {approved.Verify2Of3, current.Verify2Of3},
			"verify_32_of_32":       {approved.Verify32Of32, current.Verify32Of32},
			"update_state":          {approved.UpdateState, current.UpdateState},
			"membership":            {approved.Membership, current.Membership},
			"migrate_keep":          {approved.MigrateKeep, current.MigrateKeep},
			"migrate_replace":       {approved.MigrateReplace, current.MigrateReplace},
		}
		for name, values := range metrics {
			if values[0] == 0 && values[1] == 0 {
				continue
			}
			if values[0] == 0 {
				t.Fatalf("%s %s gas baseline must be non-zero", artifact, name)
			}
			if values[1]*100 > values[0]*110 {
				t.Fatalf("%s %s gas regressed from %d to %d (>10%%)", artifact, name, values[0], values[1])
			}
		}
	}
}

func validateL2Artifact(
	t *testing.T,
	repoRoot, artifactDir, artifact, profilePath string,
) conformanceGas {
	t.Helper()
	vm, checksum, compileGas := loadArtifact(t, filepath.Join(artifactDir, artifact))
	defer vm.Cleanup()
	store := memoryStore{}

	profileBytes, err := os.ReadFile(filepath.Join(repoRoot, profilePath))
	if err != nil {
		t.Fatal(err)
	}
	var profile any
	if err := json.Unmarshal(profileBytes, &profile); err != nil {
		t.Fatal(err)
	}
	client := marshal(t, map[string]any{
		"latest_height": 1,
		"frozen_height": nil,
		"profile":       profile,
		"attestors":     l2TestAttestorConfig(1),
	})
	consensus := marshal(t, map[string]any{
		"state_root":        "0x0000000000000000000000000000000000000000000000000000000000000001",
		"ibc_storage_root":  "0x0000000000000000000000000000000000000000000000000000000000000002",
		"timestamp_nanos":   1,
		"l2_height":         1,
		"l2_block_hash":     "0x0000000000000000000000000000000000000000000000000000000000000003",
		"parent_hash":       "0x0000000000000000000000000000000000000000000000000000000000000004",
		"first_accepted_at": 0,
	})
	initMsg := marshal(t, map[string]any{
		"client_state":    client,
		"consensus_state": consensus,
		"checksum":        []byte(checksum),
	})
	result, instantiateGas, err := vm.Instantiate(
		checksum, hostEnv(), hostInfo(), initMsg, store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, result, nil)
	if store["clientState"] == nil || store["consensusStates/0-1"] == nil {
		t.Fatalf("missing ICS-08 host keys: %#v", store)
	}

	beforeKeep := snapshot(store)
	keep, keepGas, err := vm.Migrate(
		checksum, hostEnv(), []byte(`{"keep_attestors":{}}`), store, hostAPI(),
		rejectingQuerier{}, zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, keep, nil)
	if snapshot(store) != beforeKeep {
		t.Fatal("keep_attestors rewrote contract storage")
	}

	beforeInvalidMigration := snapshot(store)
	invalidMigration, _, err := vm.Migrate(
		checksum, hostEnv(), marshal(t, map[string]any{
			"replace_attestors": map[string]any{
				"public_keys": [][]byte{l2TestPublicKey(1)}, "threshold": uint16(0),
			},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireContractError(t, invalidMigration, "invalid attestor threshold")
	if snapshot(store) != beforeInvalidMigration {
		t.Fatal("invalid attestor migration changed contract storage")
	}

	consensusBeforeMigration := append([]byte(nil), store["consensusStates/0-1"]...)
	replace, replaceGas, err := vm.Migrate(
		checksum, hostEnv(), marshal(t, map[string]any{
			"replace_attestors": map[string]any{
				"public_keys": [][]byte{l2TestPublicKey(2)}, "threshold": uint16(1),
			},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, replace, nil)
	if snapshot(store) == beforeInvalidMigration {
		t.Fatal("replace_attestors did not change client storage")
	}
	if !bytes.Equal(store["consensusStates/0-1"], consensusBeforeMigration) {
		t.Fatal("replace_attestors changed consensus storage")
	}

	queryResult, statusGas, err := vm.Query(
		checksum, hostEnv(), []byte(`{"status":{}}`), store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	if queryResult.Err != "" || !bytes.Equal(queryResult.Ok, []byte(`{"status":"Active"}`)) {
		t.Fatalf("status response = %#v", queryResult)
	}

	before := snapshot(store)
	var delayGas uint64
	for index, delay := range [][]byte{delayMembershipMessage(), delayNonMembershipMessage()} {
		delayResult, gas, err := vm.Sudo(
			checksum, hostEnv(), delay, store, hostAPI(), rejectingQuerier{}, zeroGasMeter{},
			hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			delayGas = gas
		}
		requireContractError(t, delayResult,
			"non-zero delay is unsupported: delay_time_period=7, delay_block_period=3")
		if snapshot(store) != before {
			t.Fatal("delay failure changed contract storage")
		}
	}

	for _, lifecycle := range []struct {
		message   []byte
		operation string
	}{
		{[]byte(`{"migrate_client_store":{}}`), "migrate_client_store"},
		{[]byte(`{"verify_upgrade_and_update_state":{"upgrade_client_state":"","upgrade_consensus_state":"","proof_upgrade_client":"","proof_upgrade_consensus_state":""}}`), "verify_upgrade_and_update_state"},
	} {
		lifecycleResult, _, err := vm.Sudo(
			checksum, hostEnv(), lifecycle.message, store, hostAPI(), rejectingQuerier{},
			zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		requireContractError(t, lifecycleResult,
			"lifecycle operation is unsupported: "+lifecycle.operation)
		if snapshot(store) != before {
			t.Fatal("unsupported lifecycle operation changed contract storage")
		}
	}

	_, updateGas, membershipGas := validateSuccessfulL2Flow(
		t, vm, checksum, profile, profileBytes,
	)
	verifyGas := measureL2CertificateGas(t, vm, checksum, profile, profileBytes, 1, 1)
	verify2Of3Gas := measureL2CertificateGas(t, vm, checksum, profile, profileBytes, 3, 2)
	verify32Of32Gas := measureL2CertificateGas(t, vm, checksum, profile, profileBytes, 32, 32)
	validateFrozenL2Artifact(t, vm, checksum, repoRoot, profile, consensus)

	return conformanceGas{
		StoreCode: compileGas, Instantiate: instantiateGas, Status: statusGas,
		DelayError: delayGas, VerifyClientMessage: verifyGas, UpdateState: updateGas,
		Verify2Of3: verify2Of3Gas, Verify32Of32: verify32Of32Gas,
		Membership: membershipGas, MigrateKeep: keepGas, MigrateReplace: replaceGas,
	}
}

// measureL2CertificateGas proves the complete configured certificate passes the
// optimized contract before recording its query cost. The normal successful flow
// above is the production 1-of-1 and duplicate sudo measurement; these additional
// cases lock the bounded threshold and maximum-set costs.
func measureL2CertificateGas(
	t *testing.T,
	vm *wasmvm.VM,
	checksum wasmvm.Checksum,
	profile any,
	profileBytes []byte,
	members, threshold int,
) uint64 {
	t.Helper()
	var configured struct {
		Common struct {
			L2ChainID  uint64         `json:"l2_chain_id"`
			Router     common.Address `json:"l2_router"`
			HeaderFork string         `json:"l2_header_fork"`
		} `json:"common"`
	}
	if err := json.Unmarshal(profileBytes, &configured); err != nil {
		t.Fatal(err)
	}

	account := proofAccount{
		Balance: big.NewInt(0), StorageRoot: common.HexToHash("0x02"), CodeHash: common.Hash{},
	}
	accountValue, err := rlp.EncodeToBytes(account)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot, accountLeaf := singleLeaf(t, configured.Common.Router.Bytes(), accountValue)
	parentHash := common.HexToHash("0x03")
	evmHeader := &types.Header{
		ParentHash: parentHash, UncleHash: common.HexToHash("0x11"),
		Coinbase: common.HexToAddress("0x22"), Root: stateRoot,
		TxHash: common.HexToHash("0x33"), ReceiptHash: common.HexToHash("0x44"),
		Difficulty: big.NewInt(0), Number: big.NewInt(2), GasLimit: 30_000_000,
		GasUsed: 21_000, Time: 1_700_000_001, Extra: []byte{},
		MixDigest: common.HexToHash("0x55"), BaseFee: big.NewInt(9),
	}
	header := map[string]any{
		"parent_hash": evmHeader.ParentHash.Hex(), "ommers_hash": evmHeader.UncleHash.Hex(),
		"beneficiary": evmHeader.Coinbase.Hex(), "state_root": evmHeader.Root.Hex(),
		"transactions_root": evmHeader.TxHash.Hex(),
		"receipts_root":     evmHeader.ReceiptHash.Hex(),
		"logs_bloom":        "0x" + string(bytes.Repeat([]byte{'0'}, 512)),
		"difficulty":        "0x0", "number": uint64(2), "gas_limit": uint64(30_000_000),
		"gas_used": uint64(21_000), "timestamp": uint64(1_700_000_001),
		"extra_data": "0x", "mix_hash": evmHeader.MixDigest.Hex(),
		"nonce": "0x0000000000000000", "base_fee_per_gas": "0x9",
	}
	if configured.Common.HeaderFork == "prague" {
		withdrawalsRoot := common.HexToHash("0x66")
		blobGasUsed := uint64(11)
		excessBlobGas := uint64(12)
		parentBeaconRoot := common.HexToHash("0x77")
		requestsHash := common.HexToHash("0x88")
		evmHeader.WithdrawalsHash = &withdrawalsRoot
		evmHeader.BlobGasUsed = &blobGasUsed
		evmHeader.ExcessBlobGas = &excessBlobGas
		evmHeader.ParentBeaconRoot = &parentBeaconRoot
		evmHeader.RequestsHash = &requestsHash
		header["withdrawals_root"] = withdrawalsRoot.Hex()
		header["blob_gas_used"] = blobGasUsed
		header["excess_blob_gas"] = excessBlobGas
		header["parent_beacon_block_root"] = parentBeaconRoot.Hex()
		header["requests_hash"] = requestsHash.Hex()
	}

	signers := l2TestSignerSet(members, 1)
	publicKeys := make([][]byte, len(signers))
	for index := range signers {
		publicKeys[index] = append([]byte(nil), signers[index].publicKey...)
	}
	statement := l2AttestationStatement(
		configured.Common.L2ChainID, configured.Common.Router, publicKeys, uint16(threshold),
		evmHeader.Number.Uint64(), evmHeader.Hash(), evmHeader.Root,
	)
	signatures := make([]any, threshold)
	for index := range threshold {
		signatures[index] = map[string]any{
			"attestor_index": uint16(index),
			"signature":      ed25519.Sign(signers[index].privateKey, statement),
		}
	}
	messageValue := map[string]any{
		"l2_header": header, "router_proof": map[string]any{
			"proof": []any{byteNumbers(accountLeaf)},
		},
		"attestor_signature": signatures,
	}
	clientMessage := marshal(t, map[string]any{"type": "header", "value": messageValue})

	store := memoryStore{}
	client := marshal(t, map[string]any{
		"latest_height": 1, "frozen_height": nil, "profile": profile,
		"attestors": map[string]any{
			"public_keys": publicKeys, "threshold": uint16(threshold),
		},
	})
	consensus := marshal(t, map[string]any{
		"state_root": common.HexToHash("0x01").Hex(), "ibc_storage_root": common.HexToHash("0x02").Hex(),
		"timestamp_nanos": uint64(1), "l2_height": uint64(1), "l2_block_hash": parentHash.Hex(),
		"parent_hash": common.HexToHash("0x04").Hex(), "first_accepted_at": uint64(0),
	})
	result, _, err := vm.Instantiate(
		checksum, hostEnv(), hostInfo(), marshal(t, map[string]any{
			"client_state": client, "consensus_state": consensus, "checksum": []byte(checksum),
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, result, nil)
	query, gas, err := vm.Query(
		checksum, hostEnv(), marshal(t, map[string]any{
			"verify_client_message": map[string]any{"client_message": clientMessage},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil || query.Err != "" || len(query.Ok) != 0 {
		t.Fatalf("%d-of-%d optimized verification = %#v, error = %v", threshold, members, query, err)
	}

	if members == 1 && threshold == 1 {
		newSigners := l2TestSignerSet(1, 100)
		migration, _, err := vm.Migrate(
			checksum, hostEnv(), marshal(t, map[string]any{
				"replace_attestors": map[string]any{
					"public_keys": [][]byte{newSigners[0].publicKey}, "threshold": uint16(1),
				},
			}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
			ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		requireDataOnly(t, migration, nil)
		beforeOld := snapshot(store)
		oldUpdate, _, err := vm.Sudo(
			checksum, hostEnv(), marshal(t, map[string]any{
				"update_state": map[string]any{"client_message": clientMessage},
			}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
			ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		requireContractError(t, oldUpdate, "attestor signature verification failed")
		if snapshot(store) != beforeOld {
			t.Fatal("old-set certificate changed storage after replacement")
		}

		newStatement := l2AttestationStatement(
			configured.Common.L2ChainID, configured.Common.Router,
			[][]byte{newSigners[0].publicKey}, 1, evmHeader.Number.Uint64(),
			evmHeader.Hash(), evmHeader.Root,
		)
		newValue := map[string]any{
			"l2_header": header, "router_proof": map[string]any{
				"proof": []any{byteNumbers(accountLeaf)},
			},
			"attestor_signature": []any{map[string]any{
				"attestor_index": uint16(0),
				"signature":      ed25519.Sign(newSigners[0].privateKey, newStatement),
			}},
		}
		newClientMessage := marshal(t, map[string]any{"type": "header", "value": newValue})
		newQuery, _, err := vm.Query(
			checksum, hostEnv(), marshal(t, map[string]any{
				"verify_client_message": map[string]any{"client_message": newClientMessage},
			}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
			ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil || newQuery.Err != "" || len(newQuery.Ok) != 0 {
			t.Fatalf("new-set certificate query = %#v, error = %v", newQuery, err)
		}
		newUpdate, _, err := vm.Sudo(
			checksum, hostEnv(), marshal(t, map[string]any{
				"update_state": map[string]any{"client_message": newClientMessage},
			}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
			ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		requireDataOnly(t, newUpdate,
			[]byte(`{"heights":[{"revision_number":0,"revision_height":2}]}`))
	}

	return gas
}

func validateSuccessfulL2Flow(
	t *testing.T,
	vm *wasmvm.VM,
	checksum wasmvm.Checksum,
	profile any,
	profileBytes []byte,
) (uint64, uint64, uint64) {
	t.Helper()
	var configured struct {
		Common struct {
			L2ChainID      uint64         `json:"l2_chain_id"`
			Router         common.Address `json:"l2_router"`
			CommitmentSlot common.Hash    `json:"commitment_slot"`
			HeaderFork     string         `json:"l2_header_fork"`
		} `json:"common"`
	}
	if err := json.Unmarshal(profileBytes, &configured); err != nil {
		t.Fatal(err)
	}

	path := append([]byte("08-wasm-0"), byte(1))
	path = binary.BigEndian.AppendUint64(path, 1)
	value := []byte{0xab}
	proofKey := commitmentStorageKey(path, configured.Common.CommitmentSlot)
	encodedValue, err := rlp.EncodeToBytes(value)
	if err != nil {
		t.Fatal(err)
	}
	storageRoot, storageLeaf := singleLeaf(t, proofKey[:], encodedValue)
	account := proofAccount{
		Balance: big.NewInt(0), StorageRoot: storageRoot, CodeHash: common.Hash{},
	}
	accountValue, err := rlp.EncodeToBytes(account)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot, accountLeaf := singleLeaf(t, configured.Common.Router.Bytes(), accountValue)

	parentHash := common.HexToHash("0x03")
	evmHeader := &types.Header{
		ParentHash: parentHash, UncleHash: common.HexToHash("0x11"),
		Coinbase: common.HexToAddress("0x22"), Root: stateRoot,
		TxHash: common.HexToHash("0x33"), ReceiptHash: common.HexToHash("0x44"),
		Difficulty: big.NewInt(0), Number: big.NewInt(2), GasLimit: 30_000_000,
		GasUsed: 21_000, Time: 1_700_000_001, Extra: []byte{},
		MixDigest: common.HexToHash("0x55"), BaseFee: big.NewInt(9),
	}
	header := map[string]any{
		"parent_hash": evmHeader.ParentHash.Hex(), "ommers_hash": evmHeader.UncleHash.Hex(),
		"beneficiary": evmHeader.Coinbase.Hex(), "state_root": evmHeader.Root.Hex(),
		"transactions_root": evmHeader.TxHash.Hex(),
		"receipts_root":     evmHeader.ReceiptHash.Hex(),
		"logs_bloom":        "0x" + string(bytes.Repeat([]byte{'0'}, 512)),
		"difficulty":        "0x0", "number": uint64(2), "gas_limit": uint64(30_000_000),
		"gas_used": uint64(21_000), "timestamp": uint64(1_700_000_001),
		"extra_data": "0x", "mix_hash": common.HexToHash("0x55").Hex(),
		"nonce": "0x0000000000000000", "base_fee_per_gas": "0x9",
	}
	if configured.Common.HeaderFork == "prague" {
		withdrawalsRoot := common.HexToHash("0x66")
		blobGasUsed := uint64(11)
		excessBlobGas := uint64(12)
		parentBeaconRoot := common.HexToHash("0x77")
		requestsHash := common.HexToHash("0x88")
		evmHeader.WithdrawalsHash = &withdrawalsRoot
		evmHeader.BlobGasUsed = &blobGasUsed
		evmHeader.ExcessBlobGas = &excessBlobGas
		evmHeader.ParentBeaconRoot = &parentBeaconRoot
		evmHeader.RequestsHash = &requestsHash
		header["withdrawals_root"] = withdrawalsRoot.Hex()
		header["blob_gas_used"] = blobGasUsed
		header["excess_blob_gas"] = excessBlobGas
		header["parent_beacon_block_root"] = parentBeaconRoot.Hex()
		header["requests_hash"] = requestsHash.Hex()
	}
	publicKey, privateKey := l2TestAttestor(1)
	statement := l2AttestationStatement(
		configured.Common.L2ChainID, configured.Common.Router, [][]byte{publicKey}, 1,
		evmHeader.Number.Uint64(), evmHeader.Hash(), evmHeader.Root,
	)
	clientMessage := marshal(t, map[string]any{
		"type": "header",
		"value": map[string]any{
			"l2_header":    header,
			"router_proof": map[string]any{"proof": []any{byteNumbers(accountLeaf)}},
			"attestor_signature": []any{map[string]any{
				"attestor_index": uint16(0), "signature": ed25519.Sign(privateKey, statement),
			}},
		},
	})

	store := memoryStore{}
	client := marshal(t, map[string]any{
		"latest_height": 1, "frozen_height": nil, "profile": profile,
		"attestors": l2TestAttestorConfig(1),
	})
	consensus := marshal(t, map[string]any{
		"state_root":       common.HexToHash("0x01").Hex(),
		"ibc_storage_root": common.HexToHash("0x02").Hex(),
		"timestamp_nanos":  uint64(1), "l2_height": uint64(1),
		"l2_block_hash": parentHash.Hex(), "parent_hash": common.HexToHash("0x04").Hex(),
		"first_accepted_at": uint64(0),
	})
	result, _, err := vm.Instantiate(
		checksum, hostEnv(), hostInfo(), marshal(t, map[string]any{
			"client_state": client, "consensus_state": consensus, "checksum": []byte(checksum),
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, result, nil)

	var invalidEnvelope map[string]any
	if err := json.Unmarshal(clientMessage, &invalidEnvelope); err != nil {
		t.Fatal(err)
	}
	invalidHeader := invalidEnvelope["value"].(map[string]any)
	invalidHeader["router_proof"] = map[string]any{"proof": []any{[]int{0xff}}}
	invalidHeader["attestor_signature"] = []any{map[string]any{
		"attestor_index": float64(0),
		"signature":      base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}}
	invalidClientMessage := marshal(t, invalidEnvelope)
	beforeInvalid := snapshot(store)
	invalidQuery, _, err := vm.Query(
		checksum, hostEnv(), marshal(t, map[string]any{
			"verify_client_message": map[string]any{"client_message": invalidClientMessage},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	if invalidQuery.Err != "attestor signature verification failed" {
		t.Fatalf("invalid certificate query error = %q", invalidQuery.Err)
	}
	invalidUpdate, _, err := vm.Sudo(
		checksum, hostEnv(), marshal(t, map[string]any{
			"update_state": map[string]any{"client_message": invalidClientMessage},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireContractError(t, invalidUpdate, "attestor signature verification failed")
	if snapshot(store) != beforeInvalid {
		t.Fatal("invalid signed header changed contract storage")
	}

	query, verifyGas, err := vm.Query(
		checksum, hostEnv(), marshal(t, map[string]any{
			"verify_client_message": map[string]any{"client_message": clientMessage},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil || query.Err != "" || len(query.Ok) != 0 {
		t.Fatalf("verify_client_message response = %#v, error = %v", query, err)
	}

	update, updateGas, err := vm.Sudo(
		checksum, hostEnv(), marshal(t, map[string]any{
			"update_state": map[string]any{"client_message": clientMessage},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, update,
		[]byte(`{"heights":[{"revision_number":0,"revision_height":2}]}`))
	if store["consensusStates/0-2"] == nil {
		t.Fatal("successful update did not write height 2")
	}

	proof := marshal(t, map[string]any{
		"key": proofKey.Hex(), "value": byteNumbers(value),
		"proof": []any{byteNumbers(storageLeaf)},
	})
	membership, membershipGas, err := vm.Sudo(
		checksum, hostEnv(), marshal(t, map[string]any{
			"verify_membership": map[string]any{
				"height":            map[string]uint64{"revision_number": 0, "revision_height": 2},
				"delay_time_period": uint64(0), "delay_block_period": uint64(0),
				"proof": proof, "merkle_path": map[string]any{
					"key_path": []string{base64.StdEncoding.EncodeToString(path)},
				},
				"value": value,
			},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, membership, nil)

	absentPath := append([]byte("08-wasm-0"), byte(2))
	absentPath = binary.BigEndian.AppendUint64(absentPath, 1)
	absentKey := commitmentStorageKey(absentPath, configured.Common.CommitmentSlot)
	nonMembershipProof := marshal(t, map[string]any{
		"key": absentKey.Hex(), "value": []int{},
		"proof": []any{byteNumbers(storageLeaf)},
	})
	nonMembership, _, err := vm.Sudo(
		checksum, hostEnv(), marshal(t, map[string]any{
			"verify_non_membership": map[string]any{
				"height":            map[string]uint64{"revision_number": 0, "revision_height": 2},
				"delay_time_period": uint64(0), "delay_block_period": uint64(0),
				"proof": nonMembershipProof, "merkle_path": map[string]any{
					"key_path": []string{base64.StdEncoding.EncodeToString(absentPath)},
				},
			},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, nonMembership, nil)

	var firstEnvelope map[string]any
	if err := json.Unmarshal(clientMessage, &firstEnvelope); err != nil {
		t.Fatal(err)
	}
	firstValue := firstEnvelope["value"].(map[string]any)
	secondValueBytes := marshal(t, firstValue)
	var secondValue map[string]any
	if err := json.Unmarshal(secondValueBytes, &secondValue); err != nil {
		t.Fatal(err)
	}
	secondEVMHeader := *evmHeader
	secondEVMHeader.MixDigest = common.HexToHash("0x99")
	secondValue["l2_header"].(map[string]any)["mix_hash"] = secondEVMHeader.MixDigest.Hex()
	secondStatement := l2AttestationStatement(
		configured.Common.L2ChainID, configured.Common.Router, [][]byte{publicKey}, 1,
		secondEVMHeader.Number.Uint64(), secondEVMHeader.Hash(), secondEVMHeader.Root,
	)
	secondValue["attestor_signature"] = []any{map[string]any{
		"attestor_index": uint16(0), "signature": ed25519.Sign(privateKey, secondStatement),
	}}
	misbehaviour := marshal(t, map[string]any{
		"type":  "misbehaviour",
		"value": map[string]any{"header_1": firstValue, "header_2": secondValue},
	})
	consensusBeforeFreeze := append([]byte(nil), store["consensusStates/0-2"]...)
	freeze, _, err := vm.Sudo(
		checksum, hostEnv(), marshal(t, map[string]any{
			"update_state_on_misbehaviour": map[string]any{"client_message": misbehaviour},
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, freeze, nil)
	frozenStatus, _, err := vm.Query(
		checksum, hostEnv(), []byte(`{"status":{}}`), store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil || frozenStatus.Err != "" || !bytes.Equal(frozenStatus.Ok, []byte(`{"status":"Frozen"}`)) {
		t.Fatalf("authenticated misbehaviour did not freeze: %#v, error=%v", frozenStatus, err)
	}
	if !bytes.Equal(store["consensusStates/0-2"], consensusBeforeFreeze) {
		t.Fatal("authenticated same-height misbehaviour overwrote the trusted consensus state")
	}

	return verifyGas, updateGas, membershipGas
}

func singleLeaf(t *testing.T, key, value []byte) (common.Hash, []byte) {
	t.Helper()
	path := append([]byte{0x20}, crypto.Keccak256(key)...)
	leaf, err := rlp.EncodeToBytes([][]byte{path, value})
	if err != nil {
		t.Fatal(err)
	}
	return crypto.Keccak256Hash(leaf), leaf
}

func commitmentStorageKey(path []byte, commitmentSlot common.Hash) common.Hash {
	preimage := make([]byte, 64)
	copy(preimage[:32], crypto.Keccak256(path))
	copy(preimage[32:], commitmentSlot[:])
	return crypto.Keccak256Hash(preimage)
}

func byteNumbers(data []byte) []int {
	numbers := make([]int, len(data))
	for index, value := range data {
		numbers[index] = int(value)
	}
	return numbers
}

func l2TestAttestor(member byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = member + byte(index)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	return privateKey.Public().(ed25519.PublicKey), privateKey
}

type l2TestSigner struct {
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
}

func l2TestSignerSet(members int, firstMember byte) []l2TestSigner {
	signers := make([]l2TestSigner, members)
	for index := range signers {
		publicKey, privateKey := l2TestAttestor(firstMember + byte(index))
		signers[index] = l2TestSigner{publicKey: publicKey, privateKey: privateKey}
	}
	sort.Slice(signers, func(left, right int) bool {
		return bytes.Compare(signers[left].publicKey, signers[right].publicKey) < 0
	})
	return signers
}

func l2TestPublicKey(member byte) []byte {
	publicKey, _ := l2TestAttestor(member)
	return append([]byte(nil), publicKey...)
}

func l2TestAttestorConfig(member byte) map[string]any {
	return map[string]any{
		"public_keys": [][]byte{l2TestPublicKey(member)},
		"threshold":   uint16(1),
	}
}

func l2AttestationStatement(
	chainID uint64,
	router common.Address,
	publicKeys [][]byte,
	threshold uint16,
	blockNumber uint64,
	blockHash, stateRoot common.Hash,
) []byte {
	setEncoding := make([]byte, 4, 4+len(publicKeys)*ed25519.PublicKeySize)
	binary.BigEndian.PutUint16(setEncoding[0:2], threshold)
	binary.BigEndian.PutUint16(setEncoding[2:4], uint16(len(publicKeys)))
	for _, publicKey := range publicKeys {
		setEncoding = append(setEncoding, publicKey...)
	}
	setHash := sha256.Sum256(setEncoding)
	domain := sha256.Sum256([]byte("SPECTRE_L2_ATTESTATION_V1"))
	statement := make([]byte, 164)
	copy(statement[0:32], domain[:])
	binary.BigEndian.PutUint64(statement[32:40], chainID)
	copy(statement[40:60], router[:])
	copy(statement[60:92], setHash[:])
	binary.BigEndian.PutUint64(statement[92:100], blockNumber)
	copy(statement[100:132], blockHash[:])
	copy(statement[132:164], stateRoot[:])
	return statement
}

func validateFrozenL2Artifact(
	t *testing.T,
	vm *wasmvm.VM,
	checksum wasmvm.Checksum,
	repoRoot string,
	profile any,
	consensus []byte,
) {
	t.Helper()
	store := memoryStore{}
	client := marshal(t, map[string]any{
		"latest_height": 1,
		"frozen_height": 1,
		"profile":       profile,
		"attestors":     l2TestAttestorConfig(1),
	})
	result, _, err := vm.Instantiate(
		checksum, hostEnv(), hostInfo(), marshal(t, map[string]any{
			"client_state": client, "consensus_state": consensus, "checksum": []byte(checksum),
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, result, nil)
	status, _, err := vm.Query(
		checksum, hostEnv(), []byte(`{"status":{}}`), store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil || status.Err != "" || !bytes.Equal(status.Ok, []byte(`{"status":"Frozen"}`)) {
		t.Fatalf("frozen status response = %#v, error = %v", status, err)
	}
	header, err := os.ReadFile(filepath.Join(
		repoRoot, "test/fixtures/wasm-contracts/l2-client-message.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var signedEnvelope map[string]any
	if err := json.Unmarshal(header, &signedEnvelope); err != nil {
		t.Fatal(err)
	}
	signedHeader, ok := signedEnvelope["value"].(map[string]any)
	if !ok {
		t.Fatal("historical L2 header fixture has no object value")
	}
	// The frozen-state guard follows boundary decoding but precedes authentication. Add a
	// structurally valid certificate to the header so this fixture cannot be rejected
	// as unsigned before it proves the frozen error and zero-write behavior.
	signedHeader["attestor_signature"] = []any{map[string]any{
		"attestor_index": uint16(0),
		"signature":      make([]byte, ed25519.SignatureSize),
	}}
	header = marshal(t, signedEnvelope)
	before := snapshot(store)
	for _, message := range [][]byte{
		marshal(t, map[string]any{"update_state": map[string]any{"client_message": header}}),
		marshal(t, map[string]any{"update_state_on_misbehaviour": map[string]any{"client_message": header}}),
		marshal(t, map[string]any{"verify_membership": map[string]any{
			"height":            map[string]uint64{"revision_number": 0, "revision_height": 1},
			"delay_time_period": uint64(0), "delay_block_period": uint64(0),
			"proof": []byte("not-json"), "merkle_path": map[string]any{"key_path": []string{}},
			"value": []byte{},
		}}),
		marshal(t, map[string]any{"verify_non_membership": map[string]any{
			"height":            map[string]uint64{"revision_number": 0, "revision_height": 1},
			"delay_time_period": uint64(0), "delay_block_period": uint64(0),
			"proof": []byte("not-json"), "merkle_path": map[string]any{"key_path": []string{}},
		}}),
	} {
		result, _, err := vm.Sudo(
			checksum, hostEnv(), message, store, hostAPI(), rejectingQuerier{}, zeroGasMeter{},
			hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		requireContractError(t, result, "client is frozen at height 1")
		if snapshot(store) != before {
			t.Fatal("frozen L2 failure changed contract storage")
		}
	}
}

func validateEthereumArtifact(t *testing.T, repoRoot, artifactDir string) conformanceGas {
	t.Helper()
	const artifact = "cw_ics08_wasm_eth.wasm"
	vm, checksum, compileGas := loadArtifact(t, filepath.Join(artifactDir, artifact))
	defer vm.Cleanup()
	store := memoryStore{}

	fixtureBytes, err := os.ReadFile(filepath.Join(
		repoRoot,
		"packages/ethereum/light-client/src/test_utils/fixtures/Test_ICS20TransferERC20TokenfromEthereumToCosmosAndBack.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Steps []struct {
			Data struct {
				ClientState    json.RawMessage `json:"client_state"`
				ConsensusState json.RawMessage `json:"consensus_state"`
				RelayerTxBody  string          `json:"relayer_tx_body"`
			} `json:"data"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Steps) < 2 {
		t.Fatal("Ethereum fixture must contain bootstrap and relayer transaction steps")
	}
	initMsg := marshal(t, map[string]any{
		"client_state":    []byte(fixture.Steps[0].Data.ClientState),
		"consensus_state": []byte(fixture.Steps[0].Data.ConsensusState),
		"checksum":        []byte(checksum),
	})
	result, instantiateGas, err := vm.Instantiate(
		checksum, hostEnv(), hostInfo(), initMsg, store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, result, nil)

	queryResult, statusGas, err := vm.Query(
		checksum, hostEnv(), []byte(`{"status":{}}`), store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	if queryResult.Err != "" || !bytes.Equal(queryResult.Ok, []byte(`{"status":"Active"}`)) {
		t.Fatalf("status response = %#v", queryResult)
	}

	before := snapshot(store)
	var delayGas uint64
	for index, delay := range [][]byte{delayMembershipMessage(), delayNonMembershipMessage()} {
		delayResult, gas, err := vm.Sudo(
			checksum, hostEnv(), delay, store, hostAPI(), rejectingQuerier{}, zeroGasMeter{},
			hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			delayGas = gas
		}
		requireContractError(t, delayResult,
			"non-zero delay is unsupported: delay_time_period=7, delay_block_period=3")
		if snapshot(store) != before {
			t.Fatal("delay failure changed contract storage")
		}
	}
	for _, lifecycle := range []struct {
		message   []byte
		operation string
	}{
		{[]byte(`{"migrate_client_store":{}}`), "migrate_client_store"},
		{[]byte(`{"verify_upgrade_and_update_state":{"upgrade_client_state":"","upgrade_consensus_state":"","proof_upgrade_client":"","proof_upgrade_consensus_state":""}}`), "verify_upgrade_and_update_state"},
	} {
		lifecycleResult, _, err := vm.Sudo(
			checksum, hostEnv(), lifecycle.message, store, hostAPI(), rejectingQuerier{},
			zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		requireContractError(t, lifecycleResult,
			"lifecycle operation is unsupported: "+lifecycle.operation)
		if snapshot(store) != before {
			t.Fatal("unsupported lifecycle operation changed contract storage")
		}
	}

	executeResult, _, err := vm.Execute(
		checksum, hostEnv(), hostInfo(), []byte(`{}`), store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireContractError(t, executeResult, "lifecycle operation is unsupported: execute")
	if snapshot(store) != before {
		t.Fatal("unsupported execute changed contract storage")
	}

	verifyGas, updateGas, membershipGas := validateSuccessfulEthereumFlow(
		t, vm, checksum, fixture.Steps[0].Data.ClientState,
		fixture.Steps[0].Data.ConsensusState, fixture.Steps[1].Data.RelayerTxBody,
	)
	validateFrozenEthereumArtifact(
		t, vm, checksum, fixture.Steps[0].Data.ClientState, fixture.Steps[0].Data.ConsensusState,
	)

	return conformanceGas{
		StoreCode: compileGas, Instantiate: instantiateGas, Status: statusGas,
		DelayError: delayGas, VerifyClientMessage: verifyGas, UpdateState: updateGas,
		Membership: membershipGas,
	}
}

func validateSuccessfulEthereumFlow(
	t *testing.T,
	vm *wasmvm.VM,
	checksum wasmvm.Checksum,
	clientJSON, consensusJSON json.RawMessage,
	relayerTxBody string,
) (uint64, uint64, uint64) {
	t.Helper()
	header, recv := decodeEthereumFixtureMessages(t, relayerTxBody)
	store := memoryStore{}
	querier := ethereumBLSQuerier{}
	result, _, err := vm.Instantiate(
		checksum, hostEnv(), hostInfo(), marshal(t, map[string]any{
			"client_state": []byte(clientJSON), "consensus_state": []byte(consensusJSON),
			"checksum": []byte(checksum),
		}), store, hostAPI(), querier, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, result, nil)

	var timing struct {
		ConsensusUpdate struct {
			AttestedHeader struct {
				Execution struct {
					Timestamp string `json:"timestamp"`
				} `json:"execution"`
			} `json:"attested_header"`
		} `json:"consensus_update"`
	}
	if err := json.Unmarshal(header, &timing); err != nil {
		t.Fatal(err)
	}
	timestamp, err := strconv.ParseUint(timing.ConsensusUpdate.AttestedHeader.Execution.Timestamp, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	env := hostEnv()
	env.Block.Time = vmtypes.Uint64((timestamp + 1_000) * 1_000_000_000)

	query, verifyGas, err := vm.Query(
		checksum, env, marshal(t, map[string]any{
			"verify_client_message": map[string]any{"client_message": header},
		}), store, hostAPI(), querier, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil || query.Err != "" || len(query.Ok) != 0 {
		t.Fatalf("Ethereum verify_client_message response = %#v, error = %v", query, err)
	}

	update, updateGas, err := vm.Sudo(
		checksum, env, marshal(t, map[string]any{
			"update_state": map[string]any{"client_message": header},
		}), store, hostAPI(), querier, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil || update == nil || update.Ok == nil {
		t.Fatalf("Ethereum update response = %#v, error = %v", update, err)
	}
	var updateResult struct {
		Heights []struct {
			RevisionNumber uint64 `json:"revision_number"`
			RevisionHeight uint64 `json:"revision_height"`
		} `json:"heights"`
	}
	if err := json.Unmarshal(update.Ok.Data, &updateResult); err != nil {
		t.Fatal(err)
	}
	if len(updateResult.Heights) != 1 || updateResult.Heights[0].RevisionNumber != 0 {
		t.Fatalf("Ethereum update heights = %#v", updateResult.Heights)
	}
	requireDataOnly(t, update, update.Ok.Data)

	path := append([]byte(recv.Packet.SourceClient), byte(1))
	path = binary.BigEndian.AppendUint64(path, recv.Packet.Sequence)
	value := ethereumPacketCommitment(recv.Packet)
	before := snapshot(store)
	membership, membershipGas, err := vm.Sudo(
		checksum, env, marshal(t, map[string]any{
			"verify_membership": map[string]any{
				"height": map[string]uint64{
					"revision_number": 0, "revision_height": recv.ProofHeight.RevisionHeight,
				},
				"delay_time_period": uint64(0), "delay_block_period": uint64(0),
				"proof": recv.ProofCommitment, "merkle_path": map[string]any{
					"key_path": []string{base64.StdEncoding.EncodeToString(path)},
				},
				"value": value,
			},
		}), store, hostAPI(), querier, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, membership, nil)
	if snapshot(store) != before {
		t.Fatal("successful Ethereum membership verification changed storage")
	}
	return verifyGas, updateGas, membershipGas
}

func decodeEthereumFixtureMessages(
	t *testing.T,
	relayerTxBody string,
) ([]byte, channeltypesv2.MsgRecvPacket) {
	t.Helper()
	encoded, err := hex.DecodeString(relayerTxBody)
	if err != nil {
		t.Fatal(err)
	}
	var body sdktx.TxBody
	if err := body.Unmarshal(encoded); err != nil {
		t.Fatal(err)
	}
	var header []byte
	var recv channeltypesv2.MsgRecvPacket
	for _, message := range body.Messages {
		switch message.TypeUrl {
		case "/ibc.core.client.v1.MsgUpdateClient":
			var update clienttypes.MsgUpdateClient
			if err := update.Unmarshal(message.Value); err != nil {
				t.Fatal(err)
			}
			if update.ClientMessage == nil {
				t.Fatal("fixture update has no client message")
			}
			var wasmMessage ibcwasmtypes.ClientMessage
			if err := wasmMessage.Unmarshal(update.ClientMessage.Value); err != nil {
				t.Fatal(err)
			}
			header = append([]byte(nil), wasmMessage.Data...)
		case "/ibc.core.channel.v2.MsgRecvPacket":
			if err := recv.Unmarshal(message.Value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(header) == 0 || recv.Packet.SourceClient == "" {
		t.Fatal("fixture is missing its Ethereum update or receive message")
	}
	return header, recv
}

func ethereumPacketCommitment(packet channeltypesv2.Packet) []byte {
	applications := make([]byte, 0, len(packet.Payloads)*sha256.Size)
	for _, payload := range packet.Payloads {
		fields := make([]byte, 0, 5*sha256.Size)
		for _, field := range [][]byte{
			[]byte(payload.SourcePort), []byte(payload.DestinationPort), []byte(payload.Version),
			[]byte(payload.Encoding), payload.Value,
		} {
			digest := sha256.Sum256(field)
			fields = append(fields, digest[:]...)
		}
		digest := sha256.Sum256(fields)
		applications = append(applications, digest[:]...)
	}
	destination := sha256.Sum256([]byte(packet.DestinationClient))
	timeoutBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(timeoutBytes, packet.TimeoutTimestamp)
	timeout := sha256.Sum256(timeoutBytes)
	application := sha256.Sum256(applications)
	preimage := append([]byte{2}, destination[:]...)
	preimage = append(preimage, timeout[:]...)
	preimage = append(preimage, application[:]...)
	commitment := sha256.Sum256(preimage)
	return commitment[:]
}

func validateFrozenEthereumArtifact(
	t *testing.T,
	vm *wasmvm.VM,
	checksum wasmvm.Checksum,
	clientJSON, consensusJSON json.RawMessage,
) {
	t.Helper()
	var client map[string]any
	if err := json.Unmarshal(clientJSON, &client); err != nil {
		t.Fatal(err)
	}
	client["is_frozen"] = true
	var consensus struct {
		Slot uint64 `json:"slot"`
	}
	if err := json.Unmarshal(consensusJSON, &consensus); err != nil {
		t.Fatal(err)
	}
	store := memoryStore{}
	result, _, err := vm.Instantiate(
		checksum, hostEnv(), hostInfo(), marshal(t, map[string]any{
			"client_state": marshal(t, client), "consensus_state": []byte(consensusJSON),
			"checksum": []byte(checksum),
		}), store, hostAPI(), rejectingQuerier{}, zeroGasMeter{}, hostGasLimit,
		ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	requireDataOnly(t, result, nil)
	status, _, err := vm.Query(
		checksum, hostEnv(), []byte(`{"status":{}}`), store, hostAPI(), rejectingQuerier{},
		zeroGasMeter{}, hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
	)
	if err != nil || status.Err != "" || !bytes.Equal(status.Ok, []byte(`{"status":"Frozen"}`)) {
		t.Fatalf("frozen status response = %#v, error = %v", status, err)
	}
	before := snapshot(store)
	for _, message := range [][]byte{
		[]byte(`{"update_state":{"client_message":""}}`),
		[]byte(`{"update_state_on_misbehaviour":{"client_message":""}}`),
		marshal(t, map[string]any{"verify_membership": map[string]any{
			"height":            map[string]uint64{"revision_number": 0, "revision_height": consensus.Slot},
			"delay_time_period": uint64(0), "delay_block_period": uint64(0),
			"proof": []byte("not-json"), "merkle_path": map[string]any{"key_path": []string{}},
			"value": []byte{},
		}}),
		marshal(t, map[string]any{"verify_non_membership": map[string]any{
			"height":            map[string]uint64{"revision_number": 0, "revision_height": consensus.Slot},
			"delay_time_period": uint64(0), "delay_block_period": uint64(0),
			"proof": []byte("not-json"), "merkle_path": map[string]any{"key_path": []string{}},
		}}),
	} {
		result, _, err := vm.Sudo(
			checksum, hostEnv(), message, store, hostAPI(), rejectingQuerier{}, zeroGasMeter{},
			hostGasLimit, ibcwasmtypes.CostJSONDeserialization,
		)
		if err != nil {
			t.Fatal(err)
		}
		requireContractError(t, result, "client is frozen")
		if snapshot(store) != before {
			t.Fatal("frozen Ethereum failure changed contract storage")
		}
	}
}

func loadArtifact(t *testing.T, path string) (*wasmvm.VM, wasmvm.Checksum, uint64) {
	t.Helper()
	code, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(code))*10 > uint64(ibcwasmtypes.MaxWasmSize)*9 {
		t.Fatalf("%s is %d bytes, above 90%% of ibc-go MaxWasmSize (%d)", path, len(code), ibcwasmtypes.MaxWasmSize)
	}
	vm, err := wasmvm.NewVM(t.TempDir(), []string{"iterator", "stargate"},
		ibcwasmtypes.ContractMemoryLimit, false, ibcwasmtypes.MemoryCacheSize)
	if err != nil {
		t.Fatal(err)
	}
	checksum, compileGas, err := vm.StoreCode(code, hostGasLimit)
	if err != nil {
		vm.Cleanup()
		t.Fatal(err)
	}
	return vm, checksum, compileGas
}

func delayMembershipMessage() []byte {
	return []byte(`{"verify_membership":{"height":{"revision_number":9,"revision_height":0},"delay_time_period":7,"delay_block_period":3,"proof":"bm90LWpzb24=","merkle_path":{"key_path":[]},"value":""}}`)
}

func delayNonMembershipMessage() []byte {
	return []byte(`{"verify_non_membership":{"height":{"revision_number":9,"revision_height":0},"delay_time_period":7,"delay_block_period":3,"proof":"bm90LWpzb24=","merkle_path":{"key_path":[]}}}`)
}

func TestPinnedHostVersions(t *testing.T) {
	if got := fmt.Sprint(ibcwasmtypes.MaxWasmSize); got != "3145728" {
		t.Fatalf("unexpected ibc-go MaxWasmSize: %s", got)
	}
}

package services

import (
	"attestor/types/attestation"
	"encoding/json"
	relayerclient "relayer/client"
	"testing"

	ibcwasmtypes "github.com/cosmos/ibc-go/modules/light-clients/08-wasm/v10/types"
	"github.com/ethereum/go-ethereum/common"
)

func testL2Params() L2ClientParams {
	return L2ClientParams{
		WasmChecksum:  "0xabcd",
		RollupProfile: json.RawMessage(`{"common":{"l2_router":"0x1111111111111111111111111111111111111111"},"dispute_game_factory":"0x22"}`),
		Bootstrap: relayerclient.L2BootstrapState{
			Height:            123,
			BlockHash:         common.HexToHash("0xcccc"),
			ParentHash:        common.HexToHash("0xdddd"),
			StateRoot:         common.HexToHash("0xaaaa"),
			RouterStorageRoot: common.HexToHash("0xbbbb"),
			TimestampSeconds:  1700000000,
		},
		Attestors: attestation.AttestorConfig{
			PublicKeys: [][]byte{{0x82, 0x88, 0xe3, 0xdd, 0x74, 0x09, 0xf1, 0x95, 0xfd, 0x52, 0xdb, 0x2d, 0x3c, 0xba, 0x5d, 0x72, 0xca, 0x67, 0x09, 0xbf, 0x1d, 0x94, 0x12, 0x1b, 0xf3, 0x74, 0x88, 0x01, 0xb4, 0x0f, 0x6f, 0x5c}},
			Threshold:  1,
		},
	}
}

func TestBuildL2WasmClientState(t *testing.T) {
	t.Run("client state shape", func(t *testing.T) {
		cs, consensus, err := BuildL2WasmClientState(testL2Params())
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		wasmCS, ok := cs.(*ibcwasmtypes.ClientState)
		if !ok {
			t.Fatalf("client state is not a wasm ClientState: %T", cs)
		}
		if wasmCS.LatestHeight.RevisionHeight != 123 {
			t.Fatalf("latest height = %d, want 123", wasmCS.LatestHeight.RevisionHeight)
		}

		// The client-state Data must be ClientState<Profile>: latest_height, frozen_height
		// (null), the immutable attestor set, and the rollup profile embedded verbatim
		// as `profile`.
		var clientState l2ClientStateJSON
		if err := json.Unmarshal(wasmCS.Data, &clientState); err != nil {
			t.Fatalf("client-state Data is not JSON: %v", err)
		}
		if clientState.LatestHeight != 123 {
			t.Fatalf("latest_height = %d, want 123", clientState.LatestHeight)
		}
		if clientState.FrozenHeight != nil {
			t.Fatalf("frozen_height should be null, got %v", *clientState.FrozenHeight)
		}
		if string(clientState.Profile) != string(testL2Params().RollupProfile) {
			t.Fatalf("profile not embedded verbatim: %s", clientState.Profile)
		}
		if len(clientState.Attestors.PublicKeys) != 1 || clientState.Attestors.Threshold != 1 {
			t.Fatalf("attestors missing from client state: %+v", clientState.Attestors)
		}
		var clientStateFields map[string]json.RawMessage
		if err := json.Unmarshal(wasmCS.Data, &clientStateFields); err != nil {
			t.Fatalf("client-state Data is not a JSON object: %v", err)
		}
		if _, found := clientStateFields["state_version"]; found {
			t.Fatal("state_version must not be emitted for fresh clients")
		}

		// Consensus from the bootstrap roots: ibc_storage_root = router_storage_root,
		// timestamp_nanos = timestamp_seconds * 1e9.
		wasmCons, ok := consensus.(*ibcwasmtypes.ConsensusState)
		if !ok {
			t.Fatalf("consensus is not a wasm ConsensusState: %T", consensus)
		}
		var cons l2ConsensusStateJSON
		if err := json.Unmarshal(wasmCons.Data, &cons); err != nil {
			t.Fatalf("consensus Data is not JSON: %v", err)
		}
		if cons.TimestampNanos != 1700000000*1_000_000_000 {
			t.Fatalf("timestamp_nanos = %d, want seconds*1e9", cons.TimestampNanos)
		}
		if cons.IBCStorageRoot != common.HexToHash("0xbbbb").Hex() {
			t.Fatalf("ibc_storage_root = %s, want router storage root", cons.IBCStorageRoot)
		}
		if cons.StateRoot != common.HexToHash("0xaaaa").Hex() {
			t.Fatalf("state_root = %s, want bootstrap state root", cons.StateRoot)
		}
		if cons.L2Height != 123 || cons.L2BlockHash != common.HexToHash("0xcccc").Hex() || cons.ParentHash != common.HexToHash("0xdddd").Hex() {
			t.Fatalf("bootstrap block identity missing: %+v", cons)
		}
	})

	t.Run("requires rollup profile", func(t *testing.T) {
		p := testL2Params()
		p.RollupProfile = nil
		if _, _, err := BuildL2WasmClientState(p); err == nil {
			t.Fatal("missing rollup profile must error")
		}
	})

	t.Run("requires valid attestors", func(t *testing.T) {
		p := testL2Params()
		p.Attestors = attestation.AttestorConfig{}
		if _, _, err := BuildL2WasmClientState(p); err == nil {
			t.Fatal("missing attestor set must error")
		}
	})

	// The consensus state faces the same deny_unknown_fields as the client state, and it
	// lost more fields in the rebuild — the settlement provenance (l1_origin_*,
	// evidence_hash, rollup_commitment) and the finality taxonomy (finality_level,
	// proposal_status, finality_reached_at). Re-adding any of them, or dropping one the
	// contract does read, fails only at MsgCreateClient on a live chain; assert the exact
	// set here instead. Mirrors l2-client `ConsensusState` in state.rs.
	t.Run("consensus state emits only the fields the client reads", func(t *testing.T) {
		_, consensus, err := BuildL2WasmClientState(testL2Params())
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(consensus.(*ibcwasmtypes.ConsensusState).Data, &fields); err != nil {
			t.Fatalf("decode consensus state: %v", err)
		}
		want := map[string]bool{
			"state_root": true, "ibc_storage_root": true, "timestamp_nanos": true,
			"l2_height": true, "l2_block_hash": true, "parent_hash": true,
			"first_accepted_at": true,
		}
		for k := range fields {
			if !want[k] {
				t.Fatalf("consensus state carries %q, which the contract does not read", k)
			}
		}
		for k := range want {
			if _, ok := fields[k]; !ok {
				t.Fatalf("consensus state is missing %q", k)
			}
		}
	})

	// The client state carries no policy fields any more, so the wire form is exactly
	// the four the contract reads. A stray key would be rejected by the Rust side's
	// deny_unknown_fields, which is only visible on chain — assert it here instead.
	t.Run("emits only the fields the client reads", func(t *testing.T) {
		cs, _, err := BuildL2WasmClientState(testL2Params())
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(cs.(*ibcwasmtypes.ClientState).Data, &fields); err != nil {
			t.Fatalf("decode client state: %v", err)
		}
		want := map[string]bool{"latest_height": true, "frozen_height": true, "profile": true, "attestors": true}
		for k := range fields {
			if !want[k] {
				t.Fatalf("client state carries %q, which the contract does not read", k)
			}
		}
		for k := range want {
			if _, ok := fields[k]; !ok {
				t.Fatalf("client state is missing %q", k)
			}
		}
	})
}

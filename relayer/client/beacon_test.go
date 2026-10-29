package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"relayer/rpcmock"
	"strings"
	"testing"
	"time"
)

// assertReportsNodeError checks that a non-200 becomes an error carrying both the
// status and the node's message — never a zero value, which the caller would read
// as "finality is at slot 0" or "there are no updates".
//
// The stub body deliberately does NOT mention the status code. An earlier version
// used `{"code":404,...}`, so the assertion passed whether or not the code printed
// the status: the number came from the echoed body either way.
func assertReportsNodeError(t *testing.T, path string, call func(ctx context.Context, url string) error) {
	t.Helper()
	node := rpcmock.NewBeaconNode(t)
	node.Fail(path, http.StatusNotFound, `{"message":"period not retained by this node"}`)

	err := call(beaconTestCtx(t), node.URL())
	if err == nil {
		t.Fatal("a 404 was reported as success; the caller reads the zero value as real data")
	}
	// 404 on a light-client period means "this node no longer retains it", which
	// calls for a different endpoint — not the retry a network failure calls for.
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should carry the HTTP status, got: %v", err)
	}
	if !strings.Contains(err.Error(), "period not retained") {
		t.Fatalf("error should carry the node's message, got: %v", err)
	}
}

// assertCallsEndpoint runs call against a stub serving body at path, and checks
// that path is the one requested. A copy-pasted URL is the mistake this class of
// code invites; the node answers 404 and the relayer stalls with no clue why.
func assertCallsEndpoint(t *testing.T, path, body string, call func(ctx context.Context, url string) error) {
	t.Helper()
	node := rpcmock.NewBeaconNode(t)
	node.Respond(path, body)

	if err := call(beaconTestCtx(t), node.URL()); err != nil {
		t.Fatalf("call failed against a stub serving %s: %v", path, err)
	}
	requests := node.Requests()
	if len(requests) != 1 {
		t.Fatalf("made %d requests, want 1: %v", len(requests), requests)
	}
	if reqPath, _, _ := strings.Cut(requests[0], "?"); reqPath != path {
		t.Fatalf("called %q, want %q", reqPath, path)
	}
}

func beaconTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestGetBeaconBlockRoot(t *testing.T) {
	const path = "/eth/v1/beacon/blocks/head/root"
	call := func(ctx context.Context, url string) error {
		_, err := GetBeaconBlockRoot(ctx, url, "head")
		return err
	}

	t.Run("reaches its endpoint", func(t *testing.T) {
		assertCallsEndpoint(t, path, `{"data":{"root":"0xdeadbeef"}}`, call)
	})
	t.Run("reports a node error", func(t *testing.T) {
		assertReportsNodeError(t, path, call)
	})
}

func TestGetBeaconSpec(t *testing.T) {
	const path = "/eth/v1/config/spec"
	call := func(ctx context.Context, url string) error {
		_, err := GetBeaconSpec(ctx, url)
		return err
	}

	t.Run("reaches its endpoint", func(t *testing.T) {
		assertCallsEndpoint(t, path, `{"data":{"CONFIG_NAME":"mainnet"}}`, call)
	})
	t.Run("reports a node error", func(t *testing.T) {
		assertReportsNodeError(t, path, call)
	})

	t.Run("unwraps the data envelope", func(t *testing.T) {
		node := rpcmock.NewBeaconNode(t)
		node.Respond(path, `{"data":{"CONFIG_NAME":"mainnet","SECONDS_PER_SLOT":"12"}}`)

		spec, err := GetBeaconSpec(beaconTestCtx(t), node.URL())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if spec.ConfigName != "mainnet" || spec.SecondsPerSlot != "12" {
			t.Fatalf("decoded as CONFIG_NAME=%q SECONDS_PER_SLOT=%q; the envelope was not unwrapped",
				spec.ConfigName, spec.SecondsPerSlot)
		}
	})
}

func TestGetBeaconGenesis(t *testing.T) {
	const path = "/eth/v1/beacon/genesis"
	call := func(ctx context.Context, url string) error {
		_, err := GetBeaconGenesis(ctx, url)
		return err
	}

	t.Run("reaches its endpoint", func(t *testing.T) {
		assertCallsEndpoint(t, path, `{"data":{"genesis_time":"1606824023"}}`, call)
	})
	t.Run("reports a node error", func(t *testing.T) {
		assertReportsNodeError(t, path, call)
	})

	t.Run("unwraps the data envelope", func(t *testing.T) {
		// Without the unwrap, decoding still succeeds and every field comes back
		// zero — which is why this asserts on a value, not on err == nil.
		node := rpcmock.NewBeaconNode(t)
		node.Respond(path, `{"data":{"genesis_time":"1606824023","genesis_validators_root":"0x4b363db9"}}`)

		genesis, err := GetBeaconGenesis(beaconTestCtx(t), node.URL())
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if genesis.GenesisTime != "1606824023" || genesis.GenesisValidatorsRoot != "0x4b363db9" {
			t.Fatalf("decoded as %+v; the data envelope was not unwrapped", genesis)
		}
	})
}

func TestGetLightClientUpdates(t *testing.T) {
	const path = "/eth/v1/beacon/light_client/updates"
	call := func(ctx context.Context, url string) error {
		_, err := GetLightClientUpdates(ctx, url, 5, 2)
		return err
	}

	t.Run("reaches its endpoint", func(t *testing.T) {
		assertCallsEndpoint(t, path, `[{"data":{"signature_slot":"10"}}]`, call)
	})
	t.Run("reports a node error", func(t *testing.T) {
		assertReportsNodeError(t, path, call)
	})

	t.Run("sends the requested period range", func(t *testing.T) {
		// A wrong range silently fetches the wrong sync committee, which fails
		// much later as a verification error with nothing pointing back here.
		node := rpcmock.NewBeaconNode(t)
		node.Respond(path, `[]`)

		if _, err := GetLightClientUpdates(beaconTestCtx(t), node.URL(), 42, 3); err != nil {
			t.Fatalf("call: %v", err)
		}
		query := node.LastQuery(path)
		if got := query.Get("start_period"); got != "42" {
			t.Errorf("start_period = %q, want 42", got)
		}
		if got := query.Get("count"); got != "3" {
			t.Errorf("count = %q, want 3", got)
		}
	})

	t.Run("unwraps each element in order", func(t *testing.T) {
		// The caller applies these sequentially to cross sync-committee periods;
		// an out-of-order batch fails verification on the second one.
		node := rpcmock.NewBeaconNode(t)
		node.Respond(path,
			`[{"data":{"signature_slot":"100"}},{"data":{"signature_slot":"200"}},{"data":{"signature_slot":"300"}}]`)

		updates, err := GetLightClientUpdates(beaconTestCtx(t), node.URL(), 0, 3)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		want := []string{"100", "200", "300"}
		if len(updates) != len(want) {
			t.Fatalf("got %d updates, want %d", len(updates), len(want))
		}
		for i, w := range want {
			if updates[i].SignatureSlot != w {
				t.Fatalf("updates[%d].SignatureSlot = %q, want %q (order must be preserved)",
					i, updates[i].SignatureSlot, w)
			}
		}
	})

	t.Run("empty range is not an error", func(t *testing.T) {
		// It means the node has no update for that period, which the caller
		// handles by waiting rather than by failing.
		node := rpcmock.NewBeaconNode(t)
		node.Respond(path, `[]`)

		updates, err := GetLightClientUpdates(beaconTestCtx(t), node.URL(), 0, 1)
		if err != nil {
			t.Fatalf("an empty period range must not be an error, got %v", err)
		}
		if len(updates) != 0 {
			t.Fatalf("got %d updates from an empty response", len(updates))
		}
	})
}

func TestGetFinalityUpdate(t *testing.T) {
	const path = "/eth/v1/beacon/light_client/finality_update"
	call := func(ctx context.Context, url string) error {
		_, err := GetFinalityUpdate(ctx, url)
		return err
	}

	t.Run("reaches its endpoint", func(t *testing.T) {
		assertCallsEndpoint(t, path, `{"version":"deneb","data":{"signature_slot":"1234"}}`, call)
	})
	t.Run("reports a node error", func(t *testing.T) {
		assertReportsNodeError(t, path, call)
	})
	t.Run("honours context cancellation", func(t *testing.T) {
		// This call sits on the ETH→Cosmos direction, driven by a single
		// goroutine: an unanswered request there wedges the whole direction.
		node := rpcmock.NewBeaconNode(t)
		node.Respond(path, `{"data":{}}`)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := GetFinalityUpdate(ctx, node.URL()); err == nil {
			t.Fatal("a cancelled context did not stop the request")
		}
	})
}

// TestSyncCommittee_TreeHash verifies sszTreeHashBLSPubkeys against the known Rust test vector
// from packages/ethereum/types/src/consensus/sync_committee.rs test_sync_committee_tree_hash_root.
//
// The Rust test uses 32 pubkeys and checks SyncCommittee::tree_hash_root().
// SyncCommittee is a container with 2 fields: [pubkeys_root, aggregate_pubkey_root]
// container_root = sha256(pubkeys_root || aggregate_pubkey_root)
// expected container_root = 0x5361eb179f7499edbf09e514d317002f1d365d72e14a56c931e9edaccca3ff29
func TestSyncCommittee_TreeHash(t *testing.T) {
	pubkeys := []string{
		"0x81ea9f74ef7d935b807474e38954ae3934856219a23e074954b2e860c5a3c400f9aedb42cd27cb4ceb697ca36d1e58cb",
		"0x84d08d58c31bcd3cddf93e13d6f50203897384afa34644bff1135efe8e01c81c6a91ca6c234bb1e51ca32e41b828aaf9",
		"0xa759f6bcca8f35fcaadc406cc4b828c016c0ed23882987a79f52f2933b5cedefe24e31df6fd0d38e8a802dbafd750d01",
		"0x8d028a021c5c31a1aa1e18eda74cfaf0fba1c454c17c2e0fc730dd07a19d0c77f7a905d54017292f3e800ca06b6977cd",
		"0xb27ad13afc8ff30e087797b344c8382bb0a84447549f1b0274059ddd652276e7b148ba8808a10cc45746762957d4efbe",
		"0xa804e4fa8d1391a9d078aa93985a12503b84ce4f6f1f9e70ab7fca421e1cf972538666299d4c1bfc39327b469b2db7a8",
		"0x996323af7e545fb6363ace53f1538c7ddc3eb0d985b2479da3ee4ace10cbc393b518bf02d1a2ddb2f5bdf09b473933ea",
		"0x96947de9e6068c22a7716656a2755a9551b0b66c2d1a741bf84a088fe1e840e992dc39861bf8ba3e8d5b6d21e8f57e64",
		"0xae5302796cfeca685eaf37ffd5baeb32121f2f07415bee26cc0051ee513ff3932d2c365e3d9f87b0949a5980445cb64c",
		"0x996d10c3026b9344532b06c70a596f972a1e779a1f6106d3da9f6ba376bbf7ec82d2f52629e5dbf3f7d03b00f6b862af",
		"0xa35c6004f387430c3797ab0157af7b824c8fe106241c7cdeb897d900c0f9e4bb945ff2a6b88cbd10e35ec48aaa554ecb",
		"0xabd12678c73463ecea5867a80caf256d5c5e6ba53ff188b143a4d5be83365ad257edf39eaa1ba8753c4cdf4c632ff99e",
		"0x81fa222737fe818b43f55f209f42adaee135b2801d02709617fc88c2871852358260ace97cf323e761b5cc18bc7325b3",
		"0xab64f900c770e2b99de6b86b4390bbd1579bd48dccec55800adbcf52e006f22128e9971bbf3a92cc0105b0974849935a",
		"0x930743bfc7e18d3bd7351eaa74f477505268c1e4e1fd1ca3ccccdefb2595517343bbb8f5589c435c3c39323a4c0080f8",
		"0xab72cbc6575c3179680a58c0ecd5de46d2678ccbafc016746348ee5688edcb21b4e15bd37c70c508e3ea73103c2d566b",
		"0x84dc37ca3cd621d3da0fbdd11ca84021e0cd81a73d772dd6fcf19775b72eb64af4e573213378ccee0915dde92ac83ba6",
		"0x8d46e9aa0c1986056e407efc7013b7f271027d3c98ce96667faa98074ab0588a61681faf78644c11819a459a95689dab",
		"0xb5e898a1fc06d51c695712928f44646d15451340d1b3e480a40f03250160bc07d3b6691ec94361dd524d59d9df7f76d3",
		"0xa4ee6d37dc259cbb5237e4265429a9fd8ab5643af81628cc101e0d8b4a333ef2618a37df89ea3f92b5ea4333d8cda393",
		"0x8aa5bbee21e98c7b9e7a4c8ea45aa99f89e22992fa4fc2d73869d77da4cc8a05b25b61931ff521986677dd7f7159e8e6",
		"0x91709ee06497b9ac049325853d64947290189a8c2322e3a500d91e23ea02dc158b6db63ae558b3b7670357a151cd6071",
		"0x8fda66b8607af873f4c2c8218dd3ffc7940d411047eb199b5cd010156af4845d21dd2e65b0e44cfffb5e78271e9bb29d",
		"0xb72cb106b7bc1ecae219e0ae1830a509ed18a042b56a2779f4033419de69ba8ae8017090caed1f5377bfa68506157360",
		"0x896a51e0b0de0f29029af38b796db1f1e6d0f9f9085ade40a313a60cb723fa3d58f6587175570086c4fbf0fe5331f1c8",
		"0xaaf6c1251e73fb600624937760fef218aace5b253bf068ed45398aeb29d821e4d2899343ddcbbe37cb3f6cf500dff26c",
		"0x9918433b8f0bc5e126da3fdef8d7b71456492dae6d2d07f2e10c7a7f852046f84ed0ce6d3bfec42200670db27dcf3037",
		"0xa03c2a82374e04b2e0594c4ce14fb3f225b46f13188f0d8002a523c7dcfb939ae4856053c2c9c695374d7c3685df1ca5",
		"0x8d8985e5dd341c9035b37bf7391c5944c28131b47c7d5359d18fca598010ba9a63e27c55e6b421a807038c320564db17",
		"0xb24391aa97bfff29adc935d06a2b6d583433caf82f92de1980e0192d3b270323bdbf24b86dc61520a40c419dde3df4b3",
		"0xaf61f263addfb41c46d66e60ecfb598a5942f648f58718b6b4e4c92019fdb12328efbff98703134bcf28e9c1fab4bb60",
		"0xb63f327df68581cdc02a66c1c65e906a06a1a3a8d7a6e38f7b6da944e8e6cc2db85fced5327d8c12945ceb33018272ca",
	}
	aggregatePubkey := "0xa7b9141877f397e9d2a36cd86407387bbcec6d557b30ccd9e62adca217e458d7495b581e048fa1084218cadf8f45b9ff"

	// Decode pubkeys
	pks := make([][]byte, len(pubkeys))
	for i, pk := range pubkeys {
		b, err := hex.DecodeString(pk[2:]) // strip "0x"
		if err != nil {
			t.Fatalf("decode pubkey %d: %v", i, err)
		}
		pks[i] = b
	}

	// Compute pubkeys_root using our Go implementation
	pubkeysRoot := sszTreeHashBLSPubkeys(pks)

	// Compute aggregate_pubkey_root: tree_hash of [u8; 48]
	// = sha256(bytes[0..32] || bytes[32..48] || zeros[16])
	aggBytes, _ := hex.DecodeString(aggregatePubkey[2:])
	var aggChunk0, aggChunk1 [32]byte
	copy(aggChunk0[:], aggBytes[:32])
	copy(aggChunk1[:], aggBytes[32:]) // 16 bytes + 16 zeros
	h := sha256.New()
	h.Write(aggChunk0[:])
	h.Write(aggChunk1[:])
	aggRoot := [32]byte{}
	copy(aggRoot[:], h.Sum(nil))

	// Container hash: sha256(pubkeys_root || aggregate_pubkey_root)
	h2 := sha256.New()
	h2.Write(pubkeysRoot[:])
	h2.Write(aggRoot[:])
	containerRoot := h2.Sum(nil)

	expected := "5361eb179f7499edbf09e514d317002f1d365d72e14a56c931e9edaccca3ff29"
	got := hex.EncodeToString(containerRoot)
	if got != expected {
		t.Errorf("SyncCommittee tree hash mismatch:\n  got:  %s\n  want: %s", got, expected)
		t.Errorf("pubkeys_root: %s", hex.EncodeToString(pubkeysRoot[:]))
		t.Errorf("aggRoot:      %s", hex.EncodeToString(aggRoot[:]))
	}
}

// --- beacon REST client ---
//
// Every one of these was untested. The relayer's whole view of Ethereum finality
// arrives through them, and nothing pinned which endpoint each calls, what it does
// with a non-200, or which fields survive decoding.
//
// The shared helpers below keep each unit's test to its own behaviour.

func TestToForkParameters(t *testing.T) {
	t.Run("valid spec", func(t *testing.T) {
		spec := &BeaconSpec{
			GenesisForkVersion:   "0x00000000",
			AltairForkVersion:    "0x01000000",
			AltairForkEpoch:      "74240",
			BellatrixForkVersion: "0x02000000",
			BellatrixForkEpoch:   "144896",
			CapellaForkVersion:   "0x03000000",
			CapellaForkEpoch:     "194048",
			DenebForkVersion:     "0x04000000",
			DenebForkEpoch:       "269568",
			ElectraForkVersion:   "0x05000000",
			ElectraForkEpoch:     "364544",
		}

		fp, err := spec.ToForkParameters()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fp.GenesisForkVersion != "0x00000000" {
			t.Errorf("GenesisForkVersion: got %q", fp.GenesisForkVersion)
		}
		if fp.Altair.Epoch != 74240 {
			t.Errorf("Altair.Epoch: got %d, want 74240", fp.Altair.Epoch)
		}
		if fp.Altair.Version != "0x01000000" {
			t.Errorf("Altair.Version: got %q", fp.Altair.Version)
		}
		if fp.Bellatrix.Epoch != 144896 {
			t.Errorf("Bellatrix.Epoch: got %d", fp.Bellatrix.Epoch)
		}
		if fp.Capella.Epoch != 194048 {
			t.Errorf("Capella.Epoch: got %d", fp.Capella.Epoch)
		}
		if fp.Deneb.Epoch != 269568 {
			t.Errorf("Deneb.Epoch: got %d", fp.Deneb.Epoch)
		}
		if fp.Electra.Epoch != 364544 {
			t.Errorf("Electra.Epoch: got %d", fp.Electra.Epoch)
		}
	})

	t.Run("fulu populated as its own fork slot", func(t *testing.T) {
		// Electra and Fulu each keep their real version+epoch; the light client
		// picks the active version per header, so no folding into Electra.
		spec := &BeaconSpec{
			GenesisForkVersion: "0x00000000",
			AltairForkVersion:  "0x01000000", AltairForkEpoch: "0",
			BellatrixForkVersion: "0x02000000", BellatrixForkEpoch: "0",
			CapellaForkVersion: "0x03000000", CapellaForkEpoch: "0",
			DenebForkVersion: "0x04000000", DenebForkEpoch: "0",
			ElectraForkVersion: "0x05000000", ElectraForkEpoch: "100",
			FuluForkVersion: "0x06000000", FuluForkEpoch: "200",
		}
		fp, err := spec.ToForkParameters()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fp.Electra.Version != "0x05000000" || fp.Electra.Epoch != 100 {
			t.Errorf("Electra: got %q@%d, want 0x05000000@100", fp.Electra.Version, fp.Electra.Epoch)
		}
		if fp.Fulu.Version != "0x06000000" || fp.Fulu.Epoch != 200 {
			t.Errorf("Fulu: got %q@%d, want 0x06000000@200", fp.Fulu.Version, fp.Fulu.Epoch)
		}
	})

	t.Run("fulu not scheduled defaults to never", func(t *testing.T) {
		// A pre-Fulu chain omits FULU_FORK_* — the slot must map to epoch ^uint64(0)
		// so compute_fork_version never selects it and Electra stays active.
		spec := &BeaconSpec{
			GenesisForkVersion: "0x00000000",
			AltairForkEpoch:    "0", BellatrixForkEpoch: "0", CapellaForkEpoch: "0",
			DenebForkEpoch: "0", ElectraForkVersion: "0x05000000", ElectraForkEpoch: "100",
		}
		fp, err := spec.ToForkParameters()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fp.Fulu.Epoch != ^uint64(0) {
			t.Errorf("Fulu.Epoch: got %d, want ^uint64(0) (not scheduled)", fp.Fulu.Epoch)
		}
		// The Rust client's Version is a fixed 4-byte hex; an empty version would
		// fail to deserialize the client state. A missing FULU_FORK_VERSION must
		// serialize to a valid zero version, not "".
		if fp.Fulu.Version != "0x00000000" {
			t.Errorf("Fulu.Version: got %q, want 0x00000000 (valid 4-byte hex)", fp.Fulu.Version)
		}
		// Round-trip: the marshaled fork parameters must carry a non-empty version
		// for every fork, so the Rust B32 deserialization accepts it.
		raw, err := json.Marshal(fp)
		if err != nil {
			t.Fatalf("marshal fork parameters: %v", err)
		}
		var back ForkParameters
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("unmarshal fork parameters: %v", err)
		}
		if back.Fulu.Version == "" {
			t.Errorf("marshaled fulu version is empty; Rust B32 would reject it: %s", raw)
		}
	})

	t.Run("invalid altair epoch", func(t *testing.T) {
		spec := &BeaconSpec{AltairForkEpoch: "notanumber"}
		_, err := spec.ToForkParameters()
		if err == nil {
			t.Fatal("expected error for invalid epoch")
		}
	})

	t.Run("invalid bellatrix epoch", func(t *testing.T) {
		spec := &BeaconSpec{
			AltairForkEpoch:    "0",
			BellatrixForkEpoch: "bad",
		}
		_, err := spec.ToForkParameters()
		if err == nil {
			t.Fatal("expected error for invalid epoch")
		}
	})
}

func TestToSummarizedSyncCommittee(t *testing.T) {
	// BLS sync-committee pubkeys are exactly 48 bytes (96 hex chars).
	// ToSummarizedSyncCommittee validates the length before SSZ-hashing.
	pk48a := strings.Repeat("aa", 48)
	pk48b := strings.Repeat("bb", 48)

	t.Run("valid pubkeys with 0x prefix", func(t *testing.T) {
		sc := &SyncCommittee{
			Pubkeys:         []string{"0x" + pk48a, "0x" + pk48b},
			AggregatePubkey: "0xdeadbeef",
		}
		result, err := sc.ToSummarizedSyncCommittee()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.PubkeysHash == "" {
			t.Error("expected non-empty PubkeysHash")
		}
		if result.AggregatePubkey != "0xdeadbeef" {
			t.Errorf("AggregatePubkey: got %q, want %q", result.AggregatePubkey, "0xdeadbeef")
		}
	})

	t.Run("valid pubkeys without prefix", func(t *testing.T) {
		sc := &SyncCommittee{
			Pubkeys:         []string{pk48a},
			AggregatePubkey: "aggregate",
		}
		result, err := sc.ToSummarizedSyncCommittee()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.PubkeysHash == "" {
			t.Error("expected non-empty PubkeysHash")
		}
	})

	t.Run("wrong-length pubkey rejected", func(t *testing.T) {
		sc := &SyncCommittee{
			Pubkeys: []string{"0xaabbccdd"}, // 4 bytes, not 48
		}
		if _, err := sc.ToSummarizedSyncCommittee(); err == nil {
			t.Fatal("expected error for non-48-byte pubkey")
		}
	})

	t.Run("invalid hex pubkey", func(t *testing.T) {
		sc := &SyncCommittee{
			Pubkeys: []string{"0xnothex"},
		}
		_, err := sc.ToSummarizedSyncCommittee()
		if err == nil {
			t.Fatal("expected error for invalid hex")
		}
	})

	t.Run("empty pubkeys", func(t *testing.T) {
		sc := &SyncCommittee{
			Pubkeys:         []string{},
			AggregatePubkey: "agg",
		}
		result, err := sc.ToSummarizedSyncCommittee()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.AggregatePubkey != "agg" {
			t.Errorf("AggregatePubkey: got %q", result.AggregatePubkey)
		}
	})
}

func TestComputeTimestampAtSlot(t *testing.T) {
	cs := &EthereumClientState{
		GenesisSlot:    0,
		GenesisTime:    1_700_000_000,
		SecondsPerSlot: 12,
	}

	tests := []struct {
		name string
		slot uint64
		want uint64
	}{
		{name: "genesis slot", slot: 0, want: 1_700_000_000},
		{name: "later slot", slot: 160, want: 1_700_001_920},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cs.ComputeTimestampAtSlot(tc.slot)
			if got != tc.want {
				t.Fatalf("slot %d: got timestamp %d, want %d", tc.slot, got, tc.want)
			}
		})
	}
}

func TestComputeSyncCommitteePeriodAtSlot(t *testing.T) {
	cs := &EthereumClientState{
		SlotsPerEpoch:                32,
		EpochsPerSyncCommitteePeriod: 256,
	}

	tests := []struct {
		name string
		slot uint64
		want uint64
	}{
		{name: "slot 0", slot: 0, want: 0},
		{name: "last slot in first period", slot: 8191, want: 0},
		{name: "first slot in second period", slot: 8192, want: 1},
		{name: "slot 100000", slot: 100000, want: 100000 / 32 / 256},
		{name: "slot 16384", slot: 16384, want: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cs.ComputeSyncCommitteePeriodAtSlot(tc.slot)
			if got != tc.want {
				t.Errorf("slot %d: got period %d, want %d", tc.slot, got, tc.want)
			}
		})
	}
}

func TestSszNextPowerOfTwo(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 1}, {1, 1}, {2, 2}, {3, 4}, {5, 8}, {512, 512}, {513, 1024}} {
		if got := sszNextPowerOfTwo(tc.in); got != tc.want {
			t.Errorf("sszNextPowerOfTwo(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// SSZ merkleization pads the leaf list to a power of two with zero chunks and
// hashes pairs upward; the root for a sync committee is what the light client
// compares against the beacon state, so the shape is pinned here on small trees
// whose expected roots are computed by hand.
func TestSszMerkleize(t *testing.T) {
	pair := func(a, b [32]byte) [32]byte {
		h := sha256.New()
		h.Write(a[:])
		h.Write(b[:])
		var out [32]byte
		copy(out[:], h.Sum(nil))
		return out
	}
	var a, b, c, zero [32]byte
	a[0], b[0], c[0] = 1, 2, 3

	if got := sszMerkleize(nil); got != zero {
		t.Fatalf("empty list merkleizes to %x, want the zero root", got)
	}
	if got := sszMerkleize([][32]byte{a}); got != a {
		t.Fatalf("one chunk is already the root (no padding, no hash), got %x", got)
	}
	if got, want := sszMerkleize([][32]byte{a, b}), pair(a, b); got != want {
		t.Fatalf("two chunks: got %x, want hash(a, b) = %x", got, want)
	}
	if got, want := sszMerkleize([][32]byte{a, b, c}), pair(pair(a, b), pair(c, zero)); got != want {
		t.Fatalf("three chunks: got %x, want hash(hash(a,b), hash(c,zero)) = %x", got, want)
	}
}

func TestCountSyncCommitteeParticipants(t *testing.T) {
	for _, tc := range []struct {
		bits string
		want uint64
	}{{"0x", 0}, {"", 0}, {"0xff", 8}, {"0x0f", 4}, {"FF", 8}, {"0x1", 1}, {"0x80", 1}, {"0xffffffff", 32}, {"0xzz", 0}} {
		if got := CountSyncCommitteeParticipants(tc.bits); got != tc.want {
			t.Errorf("CountSyncCommitteeParticipants(%q) = %d, want %d", tc.bits, got, tc.want)
		}
	}
}

func TestGetBeaconBlock(t *testing.T) {
	const path = "/eth/v2/beacon/blocks/head"
	call := func(ctx context.Context, url string) error {
		block, err := GetBeaconBlock(ctx, url, "head")
		if err != nil {
			return err
		}
		if block.Message.Slot != "4242" {
			return fmt.Errorf("slot = %q, want the served 4242", block.Message.Slot)
		}
		return nil
	}
	assertCallsEndpoint(t, path, `{"version":"deneb","data":{"message":{"slot":"4242","proposer_index":"7","parent_root":"0x00","state_root":"0x01","body":{}},"signature":"0x02"}}`, call)
	assertReportsNodeError(t, path, func(ctx context.Context, url string) error {
		_, err := GetBeaconBlock(ctx, url, "head")
		return err
	})
}

func TestGetLightClientBootstrap(t *testing.T) {
	const root = "0x1111111111111111111111111111111111111111111111111111111111111111"
	const path = "/eth/v1/beacon/light_client/bootstrap/" + root
	call := func(ctx context.Context, url string) error {
		bootstrap, err := GetLightClientBootstrap(ctx, url, root)
		if err != nil {
			return err
		}
		if bootstrap.Data.CurrentSyncCommittee.AggregatePubkey != "0xaa" {
			return fmt.Errorf("aggregate pubkey = %q, want the served 0xaa", bootstrap.Data.CurrentSyncCommittee.AggregatePubkey)
		}
		return nil
	}
	assertCallsEndpoint(t, path, `{"data":{"header":{"beacon":{"slot":"64"}},"current_sync_committee":{"pubkeys":["0x01"],"aggregate_pubkey":"0xaa"},"current_sync_committee_branch":["0x0","0x0","0x0","0x0","0x0","0x0"]}}`, call)
	assertReportsNodeError(t, path, func(ctx context.Context, url string) error {
		_, err := GetLightClientBootstrap(ctx, url, root)
		return err
	})
}

// httpGet is the transport under every beacon call: a body that is not the
// JSON the caller asked for must come back as an error, not as a zero value
// the caller then treats as real data.
func TestHttpGet_RejectsANonJSONBody(t *testing.T) {
	node := rpcmock.NewBeaconNode(t)
	node.Respond("/eth/v1/beacon/genesis", `<html>upstream error</html>`)
	if _, err := GetBeaconGenesis(beaconTestCtx(t), node.URL()); err == nil {
		t.Fatal("an HTML body decoded as a beacon genesis")
	}
}

package arbitrum

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	attestortypes "attestor/types"

	"github.com/ethereum/go-ethereum/common"
)

func TestL1IngestHeadValidationAndDefault(t *testing.T) {
	for _, head := range []L1IngestHead{"", L1IngestFinalized, L1IngestSafe, L1IngestLatest} {
		config := validDaemonConfig(t)
		config.L1IngestHead = head
		if err := config.Validate(); err != nil {
			t.Fatalf("head %q: %v", head, err)
		}
		got, err := head.Normalize()
		if err != nil {
			t.Fatal(err)
		}
		if head == "" && got != L1IngestFinalized {
			t.Fatal("omitted head must default to finalized")
		}
	}
	for _, head := range []L1IngestHead{"unsafe", "SAFE", " safe", "unknown"} {
		config := validDaemonConfig(t)
		config.L1IngestHead = head
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "l1_ingest_head") {
			t.Fatalf("head %q accepted: %v", head, err)
		}
	}
}

func TestLoadDaemonConfigResolvesStatePathAndLoadsNitroEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "attestor.json")
	data := `{
		"grpc_listen_address":"127.0.0.1:50051",
		"metrics_listen_address":"127.0.0.1:9090",
		"runtime_poll_interval":"2s",
		"attestation_head":"unsafe",
		"l1_ingest_head":"safe",
		"disable_derived_roots":true,
		"derived_attestation_gap_blocks":12,
		"max_derived_roots":34,
		"src_chain":"arbitrum-one",
		"l1_rpc_url":"https://ethereum.example",
		"l1_chain_id":1,
		"l2_chain_id":42161,
		"attestation_signing_key":"env:ARBITRUM_TEST_ATTESTOR_SIGNING_KEY",
		"rollup_core_address":"0x0000000000000000000000000000000000000001",
		"assertions_mapping_slot":"0x0000000000000000000000000000000000000000000000000000000000000076",
		"assertion_status_offset":25,
		"assertion_start_block":123,
		"assertion_poll_interval":"3s",
		"assertion_call_timeout":"90s",
		"assertion_max_block_range":1000,
		"attestor_state_path":"data/attested-roots.json",
		"nitro_rpc_url":"https://arbitrum.example",
		"nitro_ws_url":"wss://arbitrum.example"
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	config, err := LoadDaemonConfig(path)
	if err != nil {
		t.Fatalf("load daemon config: %v", err)
	}
	if config.L1IngestHead != L1IngestSafe {
		t.Fatalf("L1 ingest head: %s", config.L1IngestHead)
	}
	if config.AttestorStatePath != filepath.Join(dir, "data", "attested-roots.json") {
		t.Fatalf("attestor state path: got %q", config.AttestorStatePath)
	}
	if config.NitroRPCURL != "https://arbitrum.example" {
		t.Fatalf("Nitro RPC URL: got %q", config.NitroRPCURL)
	}
	if config.NitroWSURL != "wss://arbitrum.example" {
		t.Fatalf("Nitro WebSocket URL: got %q", config.NitroWSURL)
	}
	attestationHead, err := config.NormalizedAttestationHead()
	if err != nil {
		t.Fatalf("attestation head: %v", err)
	}
	if attestationHead != RunModeUnsafe || !config.DisableDerivedRoots || config.DerivedAttestationGap() != 12 || config.DerivedRootLimit() != 34 {
		t.Fatalf("derived attestation config: head=%s gap=%d max=%d", attestationHead, config.DerivedAttestationGap(), config.DerivedRootLimit())
	}
	runtimePollInterval, err := config.RuntimePollDuration()
	if err != nil {
		t.Fatalf("runtime poll interval: %v", err)
	}
	if runtimePollInterval != 2*time.Second {
		t.Fatalf("runtime poll interval: got %s want %s", runtimePollInterval, 2*time.Second)
	}
	assertionPollInterval, err := config.AssertionPollDuration()
	if err != nil {
		t.Fatalf("assertion poll interval: %v", err)
	}
	if assertionPollInterval != 3*time.Second {
		t.Fatalf("assertion poll interval: got %s want %s", assertionPollInterval, 3*time.Second)
	}
	assertionCallTimeout, err := config.AssertionCallTimeoutDuration()
	if err != nil {
		t.Fatalf("assertion call timeout: %v", err)
	}
	if assertionCallTimeout != 90*time.Second {
		t.Fatalf("assertion call timeout: got %s want %s", assertionCallTimeout, 90*time.Second)
	}
	if config.AssertionBlockRange() != 1000 {
		t.Fatalf("assertion block range: got %d want 1000", config.AssertionBlockRange())
	}
}

func TestDaemonConfigRequiresNitroHTTPAndWebSocketEndpoints(t *testing.T) {
	config := validDaemonConfig(t)
	config.NitroRPCURL = "wss://arbitrum.example"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "nitro_rpc_url") {
		t.Fatalf("validate Nitro RPC URL: got %v", err)
	}

	config = validDaemonConfig(t)
	config.NitroWSURL = "https://arbitrum.example"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "nitro_ws_url") {
		t.Fatalf("validate Nitro WebSocket URL: got %v", err)
	}
}

func TestLoadDaemonConfigRejectsManagedNitroFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "attestor.json")
	data := `{
		"nitro_rpc_url":"https://arbitrum.example",
		"nitro_ws_url":"wss://arbitrum.example",
		"nitro_binary_path":"./nitro"
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := LoadDaemonConfig(path)
	if err == nil || !strings.Contains(err.Error(), "nitro_binary_path is no longer supported") {
		t.Fatalf("load mixed endpoint/process config: got %v", err)
	}
}

func TestDaemonConfigRejectsInvalidGRPCAddress(t *testing.T) {
	config := validDaemonConfig(t)
	config.GRPCListenAddress = "127.0.0.1"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "grpc_listen_address") {
		t.Fatalf("validate gRPC address: got %v", err)
	}
}

func TestDaemonConfigRejectsInvalidMetricsAddress(t *testing.T) {
	config := validDaemonConfig(t)
	config.MetricsListenAddress = "127.0.0.1"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "metrics_listen_address") {
		t.Fatalf("validate metrics address: got %v", err)
	}
}

func TestDaemonConfigRuntimePollInterval(t *testing.T) {
	config := validDaemonConfig(t)
	interval, err := config.RuntimePollDuration()
	if err != nil {
		t.Fatalf("default runtime poll interval: %v", err)
	}
	if interval != 15*time.Second {
		t.Fatalf("default runtime poll interval: got %s want %s", interval, 15*time.Second)
	}

	config.RuntimePollInterval = "0s"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "runtime_poll_interval") {
		t.Fatalf("validate runtime poll interval: got %v", err)
	}
}

func TestDaemonConfigRuntimeBackfillDefaults(t *testing.T) {
	config := validDaemonConfig(t)
	if config.BackfillMaxBlocks() != 2_048 {
		t.Fatalf("default backfill max blocks: got %d want 2048", config.BackfillMaxBlocks())
	}
	if config.BackfillConcurrency() != 8 {
		t.Fatalf("default backfill concurrency: got %d want 8", config.BackfillConcurrency())
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("validate config without backfill knobs: %v", err)
	}

	config.RuntimeBackfillMaxBlocks = 512
	config.RuntimeBackfillConcurrency = 4
	if config.BackfillMaxBlocks() != 512 || config.BackfillConcurrency() != 4 {
		t.Fatalf(
			"explicit backfill knobs: got %d/%d want 512/4",
			config.BackfillMaxBlocks(),
			config.BackfillConcurrency(),
		)
	}
}

func TestDaemonConfigAttestationHeadDefaultsAndValidation(t *testing.T) {
	config := validDaemonConfig(t)
	head, err := config.NormalizedAttestationHead()
	if err != nil {
		t.Fatalf("default attestation head: %v", err)
	}
	if head != RunModeFinalized || config.DerivedAttestationGap() != 150 || config.DerivedRootLimit() != 1_000 {
		t.Fatalf("derived defaults: head=%s gap=%d max=%d", head, config.DerivedAttestationGap(), config.DerivedRootLimit())
	}

	config.AttestationHead = RunMode("confirmed")
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "attestation_head") {
		t.Fatalf("validate attestation head: got %v", err)
	}
}

func TestDaemonConfigAssertionDefaults(t *testing.T) {
	config := validDaemonConfig(t)
	interval, err := config.AssertionPollDuration()
	if err != nil {
		t.Fatalf("default assertion poll interval: %v", err)
	}
	if interval != 12*time.Second {
		t.Fatalf("default assertion poll interval: got %s want %s", interval, 12*time.Second)
	}
	if config.AssertionBlockRange() != 2_000 {
		t.Fatalf("default assertion block range: got %d want 2000", config.AssertionBlockRange())
	}

	config.AssertionPollInterval = "0s"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "assertion_poll_interval") {
		t.Fatalf("validate assertion poll interval: got %v", err)
	}

	callTimeout, err := validDaemonConfig(t).AssertionCallTimeoutDuration()
	if err != nil {
		t.Fatalf("default assertion call timeout: %v", err)
	}
	if callTimeout != attestortypes.DefaultCallTimeout {
		t.Fatalf("default assertion call timeout: got %s want %s", callTimeout, attestortypes.DefaultCallTimeout)
	}
}

func TestDaemonConfigRequiresBoLDStorageLayout(t *testing.T) {
	config := validDaemonConfig(t)
	config.AssertionsMappingSlot = ""
	if err := config.Validate(); err == nil ||
		!strings.Contains(err.Error(), "assertions_mapping_slot") {
		t.Fatalf("validate missing assertions mapping slot: got %v", err)
	}
}

func TestDaemonConfigRequiresAttestationSigningKey(t *testing.T) {
	config := validDaemonConfig(t)
	config.AttestationSigningKey = ""
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "attestation_signing_key") {
		t.Fatalf("validate missing signing key: got %v", err)
	}
}

func TestDaemonConfigValidationFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*DaemonConfig)
		want   string
	}{
		{name: "missing gRPC listener", mutate: func(c *DaemonConfig) { c.GRPCListenAddress = "" }, want: "grpc_listen_address"},
		{name: "missing metrics listener", mutate: func(c *DaemonConfig) { c.MetricsListenAddress = "" }, want: "metrics_listen_address"},
		{name: "invalid runtime interval", mutate: func(c *DaemonConfig) { c.RuntimePollInterval = "0s" }, want: "runtime_poll_interval"},
		{name: "invalid attestation head", mutate: func(c *DaemonConfig) { c.AttestationHead = "unknown" }, want: "attestation_head"},
		{name: "missing source chain", mutate: func(c *DaemonConfig) { c.SrcChain = "" }, want: "src_chain"},
		{name: "missing L1 RPC", mutate: func(c *DaemonConfig) { c.L1RPCURL = "" }, want: "l1_rpc_url"},
		{name: "invalid L1 RPC scheme", mutate: func(c *DaemonConfig) { c.L1RPCURL = "ftp://ethereum.example" }, want: "l1_rpc_url"},
		{name: "L1 RPC lacks host", mutate: func(c *DaemonConfig) { c.L1RPCURL = "https:///" }, want: "l1_rpc_url"},
		{name: "zero L1 chain ID", mutate: func(c *DaemonConfig) { c.L1ChainID = 0 }, want: "l1_chain_id"},
		{name: "zero L2 chain ID", mutate: func(c *DaemonConfig) { c.L2ChainID = 0 }, want: "l2_chain_id"},
		{name: "blank signing key", mutate: func(c *DaemonConfig) { c.AttestationSigningKey = " \t" }, want: "attestation_signing_key"},
		{name: "invalid RollupCore address", mutate: func(c *DaemonConfig) { c.RollupCoreAddress = "0xnot-an-address" }, want: "rollup_core_address"},
		{name: "zero RollupCore address", mutate: func(c *DaemonConfig) { c.RollupCoreAddress = "0x0000000000000000000000000000000000000000" }, want: "rollup_core_address"},
		{name: "invalid mapping slot", mutate: func(c *DaemonConfig) { c.AssertionsMappingSlot = "0x12" }, want: "assertions_mapping_slot"},
		{name: "status offset outside word", mutate: func(c *DaemonConfig) { c.AssertionStatusOffset = 32 }, want: "assertion_status_offset"},
		{name: "zero assertion start", mutate: func(c *DaemonConfig) { c.AssertionStartBlock = 0 }, want: "assertion_start_block"},
		{name: "invalid assertion poll", mutate: func(c *DaemonConfig) { c.AssertionPollInterval = "-1s" }, want: "assertion_poll_interval"},
		{name: "negative assertion call timeout", mutate: func(c *DaemonConfig) { c.AssertionCallTimeout = "-1s" }, want: "assertion_call_timeout"},
		{name: "zero assertion call timeout", mutate: func(c *DaemonConfig) { c.AssertionCallTimeout = "0s" }, want: "assertion_call_timeout"},
		{name: "unparsable assertion call timeout", mutate: func(c *DaemonConfig) { c.AssertionCallTimeout = "soon" }, want: "assertion_call_timeout"},
		{name: "relative state path", mutate: func(c *DaemonConfig) { c.AttestorStatePath = "state.json" }, want: "attestor_state_path"},
		{name: "invalid Nitro HTTP URL", mutate: func(c *DaemonConfig) { c.NitroRPCURL = "wss://nitro.example" }, want: "nitro_rpc_url"},
		{name: "invalid Nitro websocket URL", mutate: func(c *DaemonConfig) { c.NitroWSURL = "https://nitro.example" }, want: "nitro_ws_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := validDaemonConfig(t)
			tc.mutate(&config)
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestArbitrumSepoliaConfigPinsBoLDDeployment(t *testing.T) {
	config, err := LoadDaemonConfig("config.arbitrum-sepolia.json")
	if err != nil {
		t.Fatalf("load Arbitrum Sepolia config: %v", err)
	}
	if config.L1ChainID != 11_155_111 ||
		config.L2ChainID != 421_614 ||
		config.AttestationHead != RunModeFinalized ||
		!config.DisableDerivedRoots ||
		config.DerivedAttestationGap() != 150 ||
		config.NitroRPCURL != "https://arbitrum-sepolia-rpc.publicnode.com" ||
		config.NitroWSURL != "wss://arbitrum-sepolia-rpc.publicnode.com" ||
		common.HexToAddress(config.RollupCoreAddress) != common.HexToAddress(
			"0x042B2E6C5E99d4c521bd49beeD5E99651D9B0Cf4",
		) ||
		common.HexToHash(config.AssertionsMappingSlot) != common.HexToHash("0x75") ||
		config.AssertionStatusOffset != 25 ||
		config.AssertionStartBlock != 11_379_731 {
		t.Fatalf("unexpected Arbitrum Sepolia config: %+v", config)
	}
}

func validDaemonConfig(t *testing.T) DaemonConfig {
	t.Helper()
	dir := t.TempDir()
	return DaemonConfig{
		GRPCListenAddress:     "127.0.0.1:50051",
		MetricsListenAddress:  "127.0.0.1:9090",
		SrcChain:              "arbitrum-one",
		L1RPCURL:              "https://ethereum.example",
		L1ChainID:             1,
		L2ChainID:             42161,
		AttestationSigningKey: "env:ARBITRUM_TEST_ATTESTOR_SIGNING_KEY",
		RollupCoreAddress:     "0x0000000000000000000000000000000000000001",
		AssertionsMappingSlot: "0x" + strings.Repeat("0", 62) + "76",
		AssertionStatusOffset: 25,
		AssertionStartBlock:   1,
		AttestorStatePath:     filepath.Join(dir, "attested-roots.json"),
		NitroRPCURL:           "https://arbitrum.example",
		NitroWSURL:            "wss://arbitrum.example",
	}
}

// A URL that fails to parse is quoted by the parse error; it must not be
// echoed with its key into the startup log.
func TestDaemonConfigValidateDoesNotEchoTheKeyOfAnUnparsableURL(t *testing.T) {
	for name, set := range map[string]func(*DaemonConfig){
		"L1 RPC":    func(c *DaemonConfig) { c.L1RPCURL = "http://[::1/v2/SECRET-KEY" },
		"Nitro RPC": func(c *DaemonConfig) { c.NitroRPCURL = "http://[::1/v2/SECRET-KEY" },
		"Nitro WS":  func(c *DaemonConfig) { c.NitroWSURL = "ws://[::1/ws/SECRET-KEY" },
	} {
		t.Run(name, func(t *testing.T) {
			config := validDaemonConfig(t)
			set(&config)
			err := config.Validate()
			if err == nil {
				t.Fatal("Validate accepted an unparsable URL")
			}
			if strings.Contains(err.Error(), "SECRET-KEY") {
				t.Fatalf("Validate error %q echoes the key", err)
			}
		})
	}
}

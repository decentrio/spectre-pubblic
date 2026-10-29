package opstack

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	attestortypes "attestor/types"

	"github.com/ethereum/go-ethereum/common"
)

func validConfig() Config {
	return Config{
		SrcChain:           "op-mainnet",
		L1RpcUrl:           "https://ethereum.example",
		L1ChainID:          1,
		OpNodeRpcUrl:       "https://op-node.example",
		L2ChainID:          10,
		DisputeGameFactory: common.HexToAddress("0x0000000000000000000000000000000000000001"),
		StatePath:          "attestor-state.json",
	}
}

func TestConfigValidateAppliesSafeDefaults(t *testing.T) {
	config := validConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if config.AttestationHead != HeadFinalized ||
		config.PollInterval != defaultPollInterval ||
		config.CallTimeout != attestortypes.DefaultCallTimeout ||
		config.BootstrapLookbackBlocks != defaultBootstrapLookback ||
		config.DerivedGapBlocks != defaultDerivedGapBlocks ||
		config.MaxDerivedRoots != defaultMaxDerivedRoots {
		t.Fatalf("defaults = %+v", config)
	}
}

func TestConfigValidateAllowsAbsoluteIPCEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setURL func(*Config, string)
	}{
		{name: "L1 RPC", setURL: func(c *Config, endpoint string) { c.L1RpcUrl = endpoint }},
		{name: "op-node RPC", setURL: func(c *Config, endpoint string) { c.OpNodeRpcUrl = endpoint }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := validConfig()
			tc.setURL(&config, filepath.Join(t.TempDir(), "geth.ipc"))
			if err := config.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}

	config := validConfig()
	config.L1WsUrl = filepath.Join(t.TempDir(), "geth.ipc")
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "absolute URL") {
		t.Fatalf("Validate L1 websocket IPC error = %v, want absolute URL error", err)
	}
}

func TestConfigValidateFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "empty source chain", mutate: func(c *Config) { c.SrcChain = "" }, want: "src_chain"},
		{name: "missing L1 RPC", mutate: func(c *Config) { c.L1RpcUrl = "" }, want: "l1_rpc_url"},
		{name: "relative L1 RPC", mutate: func(c *Config) { c.L1RpcUrl = "geth.ipc" }, want: "valid URL"},
		{name: "unsupported L1 RPC scheme", mutate: func(c *Config) { c.L1RpcUrl = "ftp://ethereum.example" }, want: "one of"},
		{name: "missing op-node RPC", mutate: func(c *Config) { c.OpNodeRpcUrl = "" }, want: "op_node_rpc_url"},
		{name: "relative op-node RPC", mutate: func(c *Config) { c.OpNodeRpcUrl = "op-node" }, want: "valid URL"},
		{name: "invalid L1 websocket scheme", mutate: func(c *Config) { c.L1WsUrl = "https://ethereum.example" }, want: "one of"},
		{name: "relative L1 websocket", mutate: func(c *Config) { c.L1WsUrl = "events" }, want: "valid URL"},
		{name: "zero factory", mutate: func(c *Config) { c.DisputeGameFactory = common.Address{} }, want: "dispute_game_factory"},
		{name: "missing L1 chain ID", mutate: func(c *Config) { c.L1ChainID = 0 }, want: "l1_chain_id"},
		{name: "missing L2 chain ID", mutate: func(c *Config) { c.L2ChainID = 0 }, want: "l2_chain_id"},
		{name: "invalid head", mutate: func(c *Config) { c.AttestationHead = Head("confirmed") }, want: "attestation_head"},
		{name: "missing state path", mutate: func(c *Config) { c.StatePath = "" }, want: "state_path"},
		{name: "negative poll interval", mutate: func(c *Config) { c.PollInterval = -time.Second }, want: "poll_interval"},
		{name: "negative call timeout", mutate: func(c *Config) { c.CallTimeout = -time.Second }, want: "call_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := validConfig()
			tc.mutate(&config)
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A URL that fails to parse is quoted by the parse error; it must not be
// echoed with its key into the startup log.
func TestConfigValidateDoesNotEchoTheKeyOfAnUnparsableURL(t *testing.T) {
	for name, set := range map[string]func(*Config){
		"L1 RPC":     func(c *Config) { c.L1RpcUrl = "http://[::1/v2/SECRET-KEY" },
		"op-node":    func(c *Config) { c.OpNodeRpcUrl = "http://[::1/v2/SECRET-KEY" },
		"L1 hint ws": func(c *Config) { c.L1WsUrl = "ws://[::1/ws/SECRET-KEY" },
	} {
		t.Run(name, func(t *testing.T) {
			config := validConfig()
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

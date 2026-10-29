package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"relayer/chain"
	"strings"
	"testing"
)

// One relayer process serves exactly one source → destination pair. Before this
// guard, `start` looped over every configured module and ran them in one process,
// which is what made a shared nonce cache and a shared error path necessary in
// the first place. A7 moved the guard to parse
// time: loadConfig takes a census of the modules and refuses a second path
// before any module is parsed in full, so nothing downstream ever holds a list.
//
// The census is taken from the raw modules, so the cases below carry only the
// identity each module is named by; that a fully valid single-path file loads
// is TestLoadConfig/shipped examples carry one path each's job.
func TestRejectExtraRelayPaths(t *testing.T) {
	c2e := func(ids ...string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, `{"name":"cosmos_to_eth","src_chain":"cosmos","dst_chain":"ethereum","config":{"ics26_client_id":"`+id+`"}}`)
		}
		return out
	}
	c2l2 := func(ids ...string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, `{"name":"cosmos_to_l2","src_chain":"cosmos","dst_chain":"opstack","config":{"ics26_client_id":"`+id+`"}}`)
		}
		return out
	}
	l2src := func(chains ...string) []string {
		out := make([]string, 0, len(chains))
		for _, c := range chains {
			out = append(out, `{"name":"l2_to_cosmos","src_chain":"`+c+`","dst_chain":"cosmos","config":{"attestor_src_chain":"`+c+`"}}`)
		}
		return out
	}
	join := func(groups ...[]string) []string {
		var out []string
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}

	tests := []struct {
		name    string
		modules []string
		// wantNames are the identities the error must name so the operator knows
		// which paths to split apart; empty means the config must be accepted.
		wantNames []string
	}{
		{
			name:    "one cosmos_to_eth",
			modules: c2e("chain-a"),
		},
		{
			name:    "one cosmos_to_l2 with its return leg is one pair",
			modules: join(c2l2("08-wasm-3"), l2src("opstack")),
		},
		{
			name:    "forward-only cosmos_to_l2",
			modules: c2l2("08-wasm-3"),
		},
		{
			name:    "two cosmos_to_eth sources",
			modules: c2e("chain-a", "chain-b"),
			// Naming both is the point: "config has 2 sources" leaves the operator
			// guessing which two, and file order is not what they edited by.
			wantNames: []string{"chain-a", "chain-b"},
		},
		{
			name:      "two cosmos_to_l2 destinations",
			modules:   c2l2("08-wasm-3", "08-wasm-4"),
			wantNames: []string{"08-wasm-3", "08-wasm-4"},
		},
		{
			name:      "two l2_to_cosmos sources",
			modules:   l2src("opstack", "arbitrum"),
			wantNames: []string{"opstack", "arbitrum"},
		},
		{
			name:      "cosmos_to_eth alongside cosmos_to_l2 is two pairs",
			modules:   join(c2e("chain-a"), c2l2("08-wasm-3")),
			wantNames: []string{"chain-a", "08-wasm-3"},
		},
		{
			name:      "cosmos_to_eth alongside an l2_to_cosmos return leg is two pairs",
			modules:   join(c2e("chain-a"), l2src("opstack")),
			wantNames: []string{"chain-a", "opstack"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var jc jsonConfig
			if err := json.Unmarshal([]byte(`{"modules":[`+strings.Join(tt.modules, ",")+`]}`), &jc); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			census, err := censusRelayPaths(jc.Modules)
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			err = rejectExtraRelayPaths("config.json", census)
			if len(tt.wantNames) == 0 {
				if err != nil {
					t.Fatalf("single-pair config rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("start accepted more than one relay path in one process")
			}
			for _, name := range tt.wantNames {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error must name path %q so the operator knows what to split; got: %v", name, err)
				}
			}
			// Telling the operator what is wrong without telling them the way out
			// leaves them editing the config at random.
			if !strings.Contains(err.Error(), "--config") {
				t.Errorf("error must show the split command; got: %v", err)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	// TestLoadConfig/L2 source confirms an l2_to_cosmos module parses into
	// appConfig.L2ToCosmosConfig (no longer the fail-loud "not wired" error).
	t.Run("L2 source", func(t *testing.T) {
		raw := `{"modules":[{"name":"op","src_chain":"opstack","dst_chain":"cosmos","config":{
		"l1_rpc_url":"http://l1","l2_rpc_url":"http://l2","tm_rpc_url":"http://tm",
		"attestor_endpoints":[{"address":"127.0.0.1:3001","attestor_index":0}],"attestor_src_chain":"op-sepolia",
		"attestors":{"public_keys":["gojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w="],"threshold":1},
		"eth_beacon_api_url":"http://beacon",
		"l2_wasm_client_id":"08-wasm-1","l2_ics26_client_id":"client-0","head_kind":"safe",
		"rollup_profile":{"common":{"l2_chain_id":10,"l2_router":"0x1111111111111111111111111111111111111111"}}}}]}`
		var jc jsonConfig
		if err := json.Unmarshal([]byte(raw), &jc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		m := jc.Modules[0]
		dir, _, err := classifyModule(m)
		if err != nil || dir != dirL2ToCosmos {
			t.Fatalf("classify = %v, %v; want dirL2ToCosmos", dir, err)
		}
		var one l2ToCosmosConfig
		if err := json.Unmarshal(m.Config, &one); err != nil {
			t.Fatalf("parse l2 config: %v", err)
		}
		one.kind = chain.ChainType(m.SrcChain)
		if err := one.validate(); err != nil {
			t.Fatalf("validate parsed l2 config: %v", err)
		}
		if one.kind != chain.OPStack || one.AttestorSrcChain != "op-sepolia" {
			t.Fatalf("parsed config mismatch: %+v", one)
		}
	})

	// The shipped examples encode the split the guard now enforces: one runnable file
	// per path, plus a catalogue that carries every module and is not meant to be
	// started. Without this test the two drift silently — the catalogue is the file
	// whose name most invites `--config config.example.json`.
	t.Run("shipped examples carry one path each", func(t *testing.T) {
		t.Parallel()

		for file := range pathExamples {
			t.Run(file, func(t *testing.T) {
				t.Parallel()
				cfg, err := loadConfig(filepath.Join("..", file))
				if err != nil {
					t.Fatalf("loadConfig(%s): %v", file, err)
				}
				if err := requireRelayPath(cfg, file); err != nil {
					t.Fatalf("%s is a runnable example but start rejects it: %v", file, err)
				}
			})
		}

		// Refused at load, not merely at start: every command that reads a config
		// shares loadConfig, and none of them can do anything sensible with a file
		// that names several paths.
		t.Run("catalogue is not loadable", func(t *testing.T) {
			t.Parallel()
			_, err := loadConfig(filepath.Join("..", "config.example.json"))
			if err == nil {
				t.Fatal("the catalogue lists every path; loadConfig must send the operator to a per-path file instead")
			}
			if !strings.Contains(err.Error(), "--config") {
				t.Errorf("the rejection must show the way out; got: %v", err)
			}
		})
	})

	// TestLoadConfig/path examples match catalogue keeps the per-path examples byte-identical to
	// the catalogue they are cut from. Without this they drift: the file these
	// replaced (config.arbitrum-only.json) had gone stale on log_scan_chunk and
	// include_provisional and carried the addresses of a long-finished devnet run,
	// and nothing in the repo referenced it so nothing caught that.
	t.Run("path examples match catalogue", func(t *testing.T) {
		t.Parallel()

		catalogue := readModules(t, "config.example.json")

		for file, wantModules := range pathExamples {
			t.Run(file, func(t *testing.T) {
				t.Parallel()

				got := readModules(t, file)
				if len(got) != len(wantModules) {
					t.Fatalf("%s has %d modules, want %d — it must carry exactly one relay pair",
						file, len(got), len(wantModules))
				}
				for _, name := range wantModules {
					mine, ok := got[name]
					if !ok {
						t.Errorf("%s is missing module %q", file, name)
						continue
					}
					theirs, ok := catalogue[name]
					if !ok {
						t.Errorf("config.example.json has no module %q to compare against", name)
						continue
					}
					var a, b any
					if err := json.Unmarshal(mine, &a); err != nil {
						t.Fatalf("parse %s module %q: %v", file, name, err)
					}
					if err := json.Unmarshal(theirs, &b); err != nil {
						t.Fatalf("parse catalogue module %q: %v", name, err)
					}
					if !reflect.DeepEqual(a, b) {
						t.Errorf("%s module %q has drifted from config.example.json.\n"+
							"Regenerate it rather than hand-editing:\n"+
							"  cd relayer && jq '{modules: [.modules[] | select(.name|test(\"<filter>\"))], batch: .batch}' \\\n"+
							"    config.example.json > %s", file, name, file)
					}
				}
			})
		}
	})

	// TestLoadConfig/path examples load is the point of these files: unlike the catalogue, each
	// one is a config the relayer can actually start from.
	t.Run("path examples load", func(t *testing.T) {
		t.Parallel()

		for file := range pathExamples {
			t.Run(file, func(t *testing.T) {
				t.Parallel()
				if _, err := loadConfig(filepath.Join("..", file)); err != nil {
					t.Fatalf("loadConfig(%s) error = %v", file, err)
				}
			})
		}
	})

	t.Run("example rollup profiles use only fresh V2 schema", func(t *testing.T) {
		t.Parallel()

		examples := map[string]int{
			"config.example.json":          3,
			"config.op.example.json":       1,
			"config.base.example.json":     1,
			"config.arbitrum.example.json": 1,
			"op-l2-config.example.json":    1,
			"base-l2-config.example.json":  1,
			"arb-l2-config.example.json":   1,
		}
		wantKeys := map[string]struct{}{
			"l2_chain_id": {}, "l2_router": {}, "commitment_slot": {},
			"profile_version": {}, "l2_header_fork": {},
		}
		allowedVersions := map[string]struct{}{
			"op_attestor_v1": {}, "base_attestor_v1": {}, "arbitrum_attestor_v1": {},
		}

		var collectProfiles func(any, *[]map[string]any)
		collectProfiles = func(value any, profiles *[]map[string]any) {
			switch value := value.(type) {
			case map[string]any:
				if version, ok := value["profile_version"].(string); ok {
					if _, allowed := allowedVersions[version]; allowed {
						*profiles = append(*profiles, value)
					}
				}
				for _, child := range value {
					collectProfiles(child, profiles)
				}
			case []any:
				for _, child := range value {
					collectProfiles(child, profiles)
				}
			}
		}

		for file, wantCount := range examples {
			file, wantCount := file, wantCount
			t.Run(file, func(t *testing.T) {
				t.Parallel()
				raw, err := os.ReadFile(filepath.Join("..", file))
				if err != nil {
					t.Fatal(err)
				}
				var document any
				if err := json.Unmarshal(raw, &document); err != nil {
					t.Fatal(err)
				}
				var profiles []map[string]any
				collectProfiles(document, &profiles)
				if len(profiles) != wantCount {
					t.Fatalf("found %d authenticated rollup profiles, want %d", len(profiles), wantCount)
				}
				for _, profile := range profiles {
					if len(profile) != len(wantKeys) {
						t.Fatalf("profile has %d fields, want exactly five: %#v", len(profile), profile)
					}
					for key := range profile {
						if _, ok := wantKeys[key]; !ok {
							t.Fatalf("profile contains stale/unknown field %q", key)
						}
					}
				}
			})
		}
	})

	// TestLoadConfig/legacy src chain fallback: a legacy cosmos_to_eth module with no
	// ics26_client_id falls back to src_chain (back-compat).
	t.Run("legacy src chain fallback", func(t *testing.T) {
		t.Parallel()
		path := writeTempConfig(t, `{"modules":[
		{"name":"cosmos_to_eth","src_chain":"my-client-0","config":{"tm_rpc_url":"http://localhost:26657","eth_rpc_url":"http://localhost:8545","ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3"}}
	]}`)
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if got := cfg.CosmosToEthConfig.ICS26ClientID; got != "my-client-0" {
			t.Fatalf("ics26_client_id = %q, want the src_chain fallback my-client-0", got)
		}
	})

	// TestLoadConfig/canonical no fallback: a canonical module does NOT overload
	// src_chain (a chain kind) as the ics26 fallback.
	t.Run("canonical no fallback", func(t *testing.T) {
		t.Parallel()
		path := writeTempConfig(t, `{"modules":[
		{"name":"c2e","src_chain":"cosmos","dst_chain":"ethereum","config":{"ics26_client_id":"real-id","tm_rpc_url":"http://localhost:26657","eth_rpc_url":"http://localhost:8545","ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3"}}
	]}`)
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if got := cfg.CosmosToEthConfig.ICS26ClientID; got != "real-id" {
			t.Fatalf("ics26_client_id = %q, want real-id (no src_chain overload)", got)
		}
	})

	// TestLoadConfig/unknown module fails loud: a misspelled module is a hard error, not
	// a silent drop.
	t.Run("unknown module fails loud", func(t *testing.T) {
		t.Parallel()
		path := writeTempConfig(t, `{"modules":[{"name":"cosmos_to_ethh","config":{}}]}`)
		if _, err := loadConfig(path); err == nil {
			t.Fatal("expected loadConfig to fail on an unrecognized module, got nil")
		}
	})

	// TestLoadConfig/L2 to cosmos not wired: the optimistic L2→Cosmos direction is not on
	// this branch, so it still fails loud.
	t.Run("L2 to cosmos not wired", func(t *testing.T) {
		t.Parallel()
		path := writeTempConfig(t, `{"modules":[{"name":"op","src_chain":"opstack","dst_chain":"cosmos","config":{}}]}`)
		if _, err := loadConfig(path); err == nil {
			t.Fatal("expected loadConfig to fail on an unwired l2_to_cosmos direction, got nil")
		}
	})

	// TestLoadConfig/cosmos to L2 parses: a cosmos→L2 module parses into CosmosToL2Config
	// (same schema as cosmos_to_eth), pointed at the L2 endpoint.
	t.Run("cosmos to L2 parses", func(t *testing.T) {
		t.Parallel()
		path := writeTempConfig(t, `{"modules":[
		{"name":"arb-dst","src_chain":"cosmos","dst_chain":"arbitrum","config":{"ics26_client_id":"arb-client-0","tm_rpc_url":"http://localhost:26657","eth_rpc_url":"http://localhost:8547","ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3","spectre_client":"0x80741a37e3644612f0465145c9709a90b6d77ee3"}}
	]}`)
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if !cfg.HasCosmosToL2 {
			t.Fatal("HasCosmosToL2 = false, want the module recorded")
		}
		if got := cfg.CosmosToL2Config.ICS26ClientID; got != "arb-client-0" {
			t.Fatalf("ics26_client_id = %q, want arb-client-0", got)
		}
		if cfg.HasCosmosToEth {
			t.Fatal("HasCosmosToEth should be false for a cosmos_to_l2-only config")
		}
	})

	t.Run("cosmos to L2 validation uses module name", func(t *testing.T) {
		t.Parallel()
		path := writeTempConfig(t, `{"modules":[
		{"name":"cosmos-to-op","src_chain":"cosmos","dst_chain":"opstack","config":{"ics26_client_id":"op-client-0","tm_rpc_url":"http://localhost:26657","eth_rpc_url":"not-a-url","ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3"}}
	]}`)
		_, err := loadConfig(path)
		if err == nil {
			t.Fatal("expected invalid cosmos_to_l2 URL to fail")
		}
		if !strings.Contains(err.Error(), "cosmos-to-op.eth_rpc_url") {
			t.Fatalf("loadConfig error = %v, want cosmos-to-op.eth_rpc_url", err)
		}
		if strings.Contains(err.Error(), "cosmos_to_eth.eth_rpc_url") {
			t.Fatalf("loadConfig error = %v, should not label cosmos_to_l2 as cosmos_to_eth", err)
		}
	})

	// TestLoadConfig/cosmos to L2 duplicate client ID: two cosmos→L2 destinations must not
	// share an ics26_client_id.
	t.Run("cosmos to L2 duplicate client ID", func(t *testing.T) {
		t.Parallel()
		path := writeTempConfig(t, `{"modules":[
		{"name":"arb-dst","src_chain":"cosmos","dst_chain":"arbitrum","config":{"ics26_client_id":"dup","tm_rpc_url":"http://localhost:26657","eth_rpc_url":"http://localhost:8547","ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3","spectre_client":"0x80741a37e3644612f0465145c9709a90b6d77ee3"}},
		{"name":"op-dst","src_chain":"cosmos","dst_chain":"opstack","config":{"ics26_client_id":"dup","tm_rpc_url":"http://localhost:26657","eth_rpc_url":"http://localhost:8548","ics26_address":"0x80741a37e3644612f0465145c9709a90b6d77ee3","spectre_client":"0x80741a37e3644612f0465145c9709a90b6d77ee3"}}
	]}`)
		if _, err := loadConfig(path); err == nil {
			t.Fatal("expected loadConfig to fail on duplicate cosmos_to_l2 ics26_client_id, got nil")
		}
	})

	t.Run("rejects deprecated batch config", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(configPath, []byte(`{"batch_config":{"batch_size":1},"modules":[]}`), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}

		_, err := loadConfig(configPath)
		if err == nil {
			t.Fatal("expected deprecated batch_config error")
		}
		if !strings.Contains(err.Error(), "batch_config is deprecated") {
			t.Fatalf("loadConfig error = %v, want deprecated batch_config error", err)
		}
	})

	// TestLoadConfig/ignores legacy server block pins the compatibility half of dropping
	// the dead `server` block: operators' existing config.json files still carry it,
	// and they must keep loading. encoding/json ignores unknown members and nothing
	// here sets DisallowUnknownFields, so this holds — the test exists to keep it that
	// way, because turning on strict decoding later would break every deployed config.
	t.Run("ignores legacy server block", func(t *testing.T) {
		t.Parallel()

		raw := `{
	  "server": {"address": "127.0.0.1", "log_level": "info", "port": 3000},
	  "batch": {"batch_period_seconds": 3, "batch_size": 5},
	  "modules": [{"name":"c2e","src_chain":"cosmos","dst_chain":"ethereum","config":{
	    "tm_rpc_url":"http://tm","eth_rpc_url":"http://eth",
	    "ics26_address":"0x1111111111111111111111111111111111111111",
	    "ics26_client_id":"cosmoshub-1"}}]}`

		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}

		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatalf("loadConfig rejected a config carrying the legacy server block: %v", err)
		}
		if cfg.CosmosToEthConfig.ICS26ClientID != "cosmoshub-1" {
			t.Errorf("ics26_client_id = %q, want cosmoshub-1", cfg.CosmosToEthConfig.ICS26ClientID)
		}
		if cfg.BatchConfig.BatchSize != 5 {
			t.Errorf("batch_size = %d, want 5 — the rest of the config must still parse",
				cfg.BatchConfig.BatchSize)
		}
	})

	t.Run("example", func(t *testing.T) {
		t.Parallel()

		// The per-path Ethereum example, not the catalogue: config.example.json lists
		// every module across every path and loadConfig refuses it for that reason
		// (TestLoadConfig/shipped examples carry one path each pins that).
		cfg, err := loadConfig(filepath.Join("..", "config.ethereum.example.json"))
		if err != nil {
			t.Fatalf("loadConfig(config.ethereum.example.json) error = %v", err)
		}
		if cfg.CosmosToEthConfig.TmRpcUrl == "" {
			t.Fatal("cosmos_to_eth.tm_rpc_url not parsed")
		}
		if cfg.CosmosToEthConfig.ICS26ClientID == "" {
			t.Fatal("ics26_client_id not parsed")
		}
		if cfg.EthToCosmosConfig.BeaconUrl == "" {
			t.Fatal("eth_to_cosmos.eth_beacon_api_url not parsed")
		}
	})

	// TestLoadConfig/ignores pruned fields confirms old configs carrying the now-removed
	// eth_to_cosmos fields and dst_chain still load (unknown JSON keys are ignored),
	// preserving backward compatibility.
	t.Run("ignores pruned fields", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		legacy := `{
		"modules": [
			{"name": "cosmos_to_eth", "src_chain": "test", "dst_chain": "0x1",
			 "config": {"tm_rpc_url": "http://localhost:26657", "eth_rpc_url": "http://localhost:8545", "eth_ws_url": "ws://localhost:8546", "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3"}},
			{"name": "eth_to_cosmos", "dst_chain": "test",
			 "config": {"eth_beacon_api_url": "http://localhost:5052", "tm_rpc_url": "http://localhost:26657", "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "signer_address": "cosmos1abc"}}
		]
	}`
		if err := os.WriteFile(configPath, []byte(legacy), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := loadConfig(configPath)
		if err != nil {
			t.Fatalf("loadConfig(legacy) error = %v", err)
		}
		if cfg.EthToCosmosConfig.BeaconUrl != "http://localhost:5052" {
			t.Fatalf("beacon url = %q, want parsed", cfg.EthToCosmosConfig.BeaconUrl)
		}
	})

	// A second cosmos_to_eth module is a second relay path, and loadConfig refuses
	// it before parsing either in full -- naming both, so the operator knows which
	// two to split, and showing the split command. This is the loadConfig-level
	// half of TestRejectExtraRelayPaths: that the census is wired in at all.
	t.Run("rejects a second cosmos source", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		cfgJSON := `{
		"modules": [
			{"name": "cosmos_to_eth", "src_chain": "chain-a",
			 "config": {"tm_rpc_url": "http://localhost:26657", "eth_rpc_url": "http://localhost:8545", "eth_ws_url": "ws://localhost:8546", "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "ics26_client_id": "chain-a"}},
			{"name": "cosmos_to_eth", "src_chain": "chain-b",
			 "config": {"tm_rpc_url": "http://localhost:36657", "eth_rpc_url": "http://localhost:8545", "eth_ws_url": "ws://localhost:8546", "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "ics26_client_id": "chain-b"}},
			{"name": "eth_to_cosmos", "config": {"eth_beacon_api_url": "http://localhost:5052"}}
		]
	}`
		if err := os.WriteFile(configPath, []byte(cfgJSON), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		_, err := loadConfig(configPath)
		if err == nil {
			t.Fatal("loadConfig accepted two cosmos_to_eth modules; one process serves one path")
		}
		for _, want := range []string{"chain-a", "chain-b", "--config"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error must contain %q so the operator knows what to split and how; got: %v", want, err)
			}
		}
	})

	// Two cosmos_to_eth modules sharing an ics26_client_id used to be refused as a
	// duplicate id (their ETH event streams, filtered by that id, would cross-feed).
	// They are still refused -- as two paths, which subsumes the duplicate case.
	t.Run("rejects duplicate client ID", func(t *testing.T) {
		t.Parallel()

		configPath := filepath.Join(t.TempDir(), "config.json")
		cfgJSON := `{
		"modules": [
			{"name": "cosmos_to_eth", "config": {"tm_rpc_url": "http://localhost:26657", "eth_rpc_url": "http://localhost:8545", "eth_ws_url": "ws://localhost:8546", "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "ics26_client_id": "dup"}},
			{"name": "cosmos_to_eth", "config": {"tm_rpc_url": "http://localhost:36657", "eth_rpc_url": "http://localhost:8545", "eth_ws_url": "ws://localhost:8546", "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "ics26_client_id": "dup"}}
		]
	}`
		if err := os.WriteFile(configPath, []byte(cfgJSON), configFilePerm); err != nil {
			t.Fatalf("write config: %v", err)
		}
		_, err := loadConfig(configPath)
		if err == nil || !strings.Contains(err.Error(), "more than one relay path") {
			t.Fatalf("loadConfig error = %v, want the second path refused", err)
		}
	})

	// The client-creation commands share loadConfig but never read eth_ws_url
	// (build_source.go, reached only from `start`, is its sole consumer). Rejecting a
	// missing websocket at load time blocked create-clients-cosmos on a field it does
	// not use — the config below must load, and only `start` may refuse it.
	t.Run("accepts missing eth ws for client creation", func(t *testing.T) {
		const cfg = `{
		"modules": [
			{"name": "cosmos_to_eth", "src_chain": "cosmos", "dst_chain": "ethereum",
			 "config": {"tm_rpc_url": "http://localhost:26657", "eth_rpc_url": "http://localhost:8545",
			            "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "ics26_client_id": "chain-a"}},
			{"name": "eth_to_cosmos", "src_chain": "ethereum", "dst_chain": "cosmos",
			 "config": {"eth_beacon_api_url": "http://localhost:5052"}}
		]
	}`
		if _, err := loadConfig(writeTempConfig(t, cfg)); err != nil {
			t.Fatalf("loadConfig rejected a config create-clients-cosmos can run: %v", err)
		}
	})

	// A configured ETH→Cosmos direction needs a beacon REST endpoint for finality
	// data and Ethereum light-client updates. Treating an empty value as if the
	// module did not exist makes the relayer silently omit that direction.
	t.Run("rejects eth to cosmos without beacon URL", func(t *testing.T) {
		const cfg = `{
		"modules": [
			{"name": "eth_to_cosmos", "src_chain": "ethereum", "dst_chain": "cosmos", "config": {}}
		]
	}`
		_, err := loadConfig(writeTempConfig(t, cfg))
		if err == nil {
			t.Fatal("loadConfig accepted eth_to_cosmos without eth_beacon_api_url")
		}
		if !strings.Contains(err.Error(), "eth_beacon_api_url") {
			t.Fatalf("error should name the missing field, got: %v", err)
		}
	})

	// The same config with a websocket must load AND start.
	t.Run("accepts eth to cosmos with ws", func(t *testing.T) {
		const cfg = `{
		"modules": [
			{"name": "cosmos_to_eth", "src_chain": "cosmos", "dst_chain": "ethereum",
			 "config": {"tm_rpc_url": "http://localhost:26657", "eth_rpc_url": "http://localhost:8545",
			            "eth_ws_url": "ws://localhost:8546",
			            "ics26_address": "0x80741a37e3644612f0465145c9709a90b6d77ee3", "ics26_client_id": "chain-a"}},
			{"name": "eth_to_cosmos", "src_chain": "ethereum", "dst_chain": "cosmos",
			 "config": {"eth_beacon_api_url": "http://localhost:5052"}}
		]
	}`
		appCfg, err := loadConfig(writeTempConfig(t, cfg))
		if err != nil {
			t.Fatalf("loadConfig rejected a complete config: %v", err)
		}
		if err := validateRelayStartupConfig(appCfg); err != nil {
			t.Fatalf("start rejected a complete config: %v", err)
		}
	})
}

// --- state directory ---------------------------------------------------------

// The path is what the log prefix carries, so it has to be derivable from config
// alone and stable for the life of the process. A1 makes that true by refusing a
// config with more than one path.
func TestRelayPathID(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *appConfig
		want string
	}{
		{
			name: "cosmos to eth carries the source client id",
			cfg:  &appConfig{HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{ICS26ClientID: "cosmoshub-1"}},
			want: "cosmos<->eth/cosmoshub-1",
		},
		{
			// The rollup names itself: an operator running an OP and an Arbitrum
			// path needs to tell the two apart, and "l2" does not.
			name: "cosmos to l2 names the rollup from the return leg",
			cfg: &appConfig{
				HasCosmosToL2: true, CosmosToL2Config: cosmosToEthConfig{ICS26ClientID: "cosmoshub-1"},
				HasL2ToCosmos: true, L2ToCosmosConfig: l2ToCosmosConfig{AttestorSrcChain: "arbitrum"},
			},
			want: "cosmos<->arbitrum/cosmoshub-1",
		},
		{
			name: "cosmos to l2 without a return leg still identifies the path",
			cfg:  &appConfig{HasCosmosToL2: true, CosmosToL2Config: cosmosToEthConfig{ICS26ClientID: "cosmoshub-1"}},
			want: "cosmos<->l2/cosmoshub-1",
		},
		{
			name: "return leg only",
			cfg:  &appConfig{HasL2ToCosmos: true, L2ToCosmosConfig: l2ToCosmosConfig{AttestorSrcChain: "opstack"}},
			want: "opstack->cosmos",
		},
		{
			// Never empty: an empty prefix would silently drop the identity from
			// every line rather than being obviously wrong.
			name: "nothing configured still yields a label",
			cfg:  &appConfig{},
			want: "relayer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayPathID(tc.cfg); got != tc.want {
				t.Fatalf("relayPathID = %q, want %q", got, tc.want)
			}
		})
	}

	// Two processes relaying cosmos->eth from different source clients are a valid
	// deployment, and their lines are merged by journald or Loki with no file
	// boundary left. The prefix is the only thing that tells them apart, so it must
	// actually differ.
	t.Run("distinguishes two cosmos sources", func(t *testing.T) {
		a := relayPathID(&appConfig{HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{ICS26ClientID: "cosmoshub-1"}})
		b := relayPathID(&appConfig{HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{ICS26ClientID: "osmosis-1"}})
		if a == b {
			t.Fatalf("both sources produced %q; merged logs cannot be told apart", a)
		}
	})
}

// --- aggregation -------------------------------------------------------------

// pathExamples maps each runnable per-path example to the module names it must
// carry. config.example.json is the catalogue of every module across every path;
// these are the files an operator actually copies to config.json.
var pathExamples = map[string][]string{
	"config.ethereum.example.json": {"cosmos-to-eth", "eth-to-cosmos"},
	"config.op.example.json":       {"cosmos-to-op", "op-to-cosmos"},
	"config.arbitrum.example.json": {"cosmos-to-arbitrum", "arbitrum-to-cosmos"},
	"config.base.example.json":     {"cosmos-to-base", "base-to-cosmos"},
}

func readModules(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed struct {
		Modules []struct {
			Name string `json:"name"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	// Re-unmarshal into raw messages so comparison is on the full module object,
	// not just the fields this test happens to name.
	var full struct {
		Modules []json.RawMessage `json:"modules"`
	}
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatalf("parse %s modules: %v", path, err)
	}
	out := make(map[string]json.RawMessage, len(full.Modules))
	for i, m := range full.Modules {
		out[parsed.Modules[i].Name] = m
	}
	return out
}

func TestClassifyModule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		module     configModule
		wantDir    moduleDirection
		wantLegacy bool
		wantErr    bool
	}{
		{
			name:       "legacy cosmos_to_eth by name",
			module:     configModule{Name: "cosmos_to_eth", SrcChain: "cosmoshub-1"},
			wantDir:    dirCosmosToEth,
			wantLegacy: true,
		},
		{
			name:       "legacy eth_to_cosmos by name",
			module:     configModule{Name: "eth_to_cosmos"},
			wantDir:    dirEthToCosmos,
			wantLegacy: true,
		},
		{
			name:    "canonical cosmos->ethereum",
			module:  configModule{Name: "my-label", SrcChain: "cosmos", DstChain: "ethereum"},
			wantDir: dirCosmosToEth,
		},
		{
			name:    "canonical ethereum->cosmos",
			module:  configModule{Name: "eth-side", SrcChain: "ethereum", DstChain: "cosmos"},
			wantDir: dirEthToCosmos,
		},
		{
			name:    "canonical opstack->cosmos",
			module:  configModule{Name: "op", SrcChain: "opstack", DstChain: "cosmos"},
			wantDir: dirL2ToCosmos,
		},
		{
			name:    "canonical arbitrum->cosmos",
			module:  configModule{Name: "arb", SrcChain: "arbitrum", DstChain: "cosmos"},
			wantDir: dirL2ToCosmos,
		},
		{
			name:    "canonical cosmos->opstack",
			module:  configModule{Name: "op-dst", SrcChain: "cosmos", DstChain: "opstack"},
			wantDir: dirCosmosToL2,
		},
		{
			name:    "unrecognized legacy name fails loud",
			module:  configModule{Name: "typo_to_eth"},
			wantErr: true,
		},
		{
			// solana is not a known chain kind, so this is not canonical and the
			// unrecognized name fails loud.
			name:    "unknown chain kind fails loud",
			module:  configModule{Name: "x", SrcChain: "solana", DstChain: "cosmos"},
			wantErr: true,
		},
		{
			// both valid kinds but the pair is unsupported.
			name:    "unsupported canonical pair fails loud",
			module:  configModule{Name: "x", SrcChain: "arbitrum", DstChain: "ethereum"},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, legacy, err := classifyModule(tc.module)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got dir=%q", dir)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dir != tc.wantDir {
				t.Fatalf("dir = %q, want %q", dir, tc.wantDir)
			}
			if legacy != tc.wantLegacy {
				t.Fatalf("isLegacy = %v, want %v", legacy, tc.wantLegacy)
			}
		})
	}
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// selectCosmosSource is the promotion half of what --source used to do: the
// create-clients and submit-misbehaviour flows read CosmosToEthConfig, and a
// cosmos_to_l2 module is the same struct pointed at the L2, so a file declaring
// only that module has it promoted into the field those flows read. There is no
// disambiguation half any more -- loadConfig refuses a file with two paths.
func TestSelectCosmosSource(t *testing.T) {
	t.Parallel()

	t.Run("a cosmos_to_eth module is read as is", func(t *testing.T) {
		single := &appConfig{
			HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{ICS26ClientID: "solo", TmRpcUrl: "s"},
		}
		if got := selectCosmosSource(single); got.CosmosToEthConfig.TmRpcUrl != "s" {
			t.Fatalf("selected wrong source: %+v", got.CosmosToEthConfig)
		}
	})

	// create-clients-eth also targets cosmos_to_l2 destinations (the L2-side
	// SpectreClient is created by the same flow).
	t.Run("a sole cosmos_to_l2 module is promoted", func(t *testing.T) {
		l2Only := &appConfig{
			HasCosmosToL2: true, CosmosToL2Config: cosmosToEthConfig{ICS26ClientID: "l2-solo", TmRpcUrl: "l2s"},
		}
		got := selectCosmosSource(l2Only)
		if got.CosmosToEthConfig.TmRpcUrl != "l2s" {
			t.Fatalf("selected wrong dest: %+v", got.CosmosToEthConfig)
		}
		// A copy, not a write into the caller's config: the L2 module must not
		// start looking like a cosmos_to_eth one to anything that reads the flag.
		if l2Only.HasCosmosToEth || l2Only.CosmosToEthConfig.TmRpcUrl != "" {
			t.Fatalf("selectCosmosSource mutated its input: %+v", l2Only)
		}
	})

	t.Run("nothing declared is returned unchanged", func(t *testing.T) {
		empty := &appConfig{}
		if got := selectCosmosSource(empty); got != empty {
			t.Fatal("a config with no Cosmos→EVM module must come back as is, for the command's own preflight to report")
		}
	})
}

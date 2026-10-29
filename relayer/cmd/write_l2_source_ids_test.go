package main

import (
	"encoding/json"
	"os"
	"testing"
)

// l2SourceConfigJSON mirrors the shape config.example.json ships: an l2_to_cosmos
// module carrying a placeholder client id next to a cosmos_to_l2 module that must
// stay untouched. The rollup_profile is the attestor-trusted shape — no
// ethereum_client member, because the L2 client authenticates nothing through one.
const l2SourceConfigJSON = `{
  "modules": [
    {
      "name": "cosmos-to-op",
      "src_chain": "cosmos",
      "dst_chain": "opstack",
      "config": {
        "cosmos_wasm_client_id": "08-wasm-0",
        "ics26_client_id": "cosmoshub-2"
      }
    },
    {
      "name": "op-to-cosmos",
      "src_chain": "opstack",
      "dst_chain": "cosmos",
      "config": {
        "l2_wasm_client_id": "08-wasm-1",
        "l2_ics26_client_id": "cosmoshub-2",
        "head_kind": "safe",
        "rollup_profile": {
          "common": {
            "l2_router": "0xrouter",
            "profile_version": "op_attestor_v1",
            "attestor_public_key": "0x1111111111111111111111111111111111111111111111111111111111111111",
            "attestation_head": "safe"
          }
        }
      }
    }
  ]
}`

// parsed decodes the written config so assertions read fields rather than bytes.
type parsedConfig struct {
	Modules []struct {
		Name     string `json:"name"`
		SrcChain string `json:"src_chain"`
		DstChain string `json:"dst_chain"`
		Config   struct {
			CosmosWasmClientID string `json:"cosmos_wasm_client_id"`
			L2WasmClientID     string `json:"l2_wasm_client_id"`
			L2ICS26ClientID    string `json:"l2_ics26_client_id"`
			HeadKind           string `json:"head_kind"`
			RollupProfile      struct {
				Common struct {
					L2Router       string `json:"l2_router"`
					ProfileVersion string `json:"profile_version"`
				} `json:"common"`
			} `json:"rollup_profile"`
		} `json:"config"`
	} `json:"modules"`
}

// writeTempConfig lives in dispatch_test.go.

func readParsed(t *testing.T, path string) parsedConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg parsedConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse written config: %v\n%s", err, data)
	}
	return cfg
}

func TestWriteL2SourceClientIDs(t *testing.T) {
	// The reason this write-back exists: l2_wasm_client_id is assigned by ibc-go's global
	// client sequence, so the config can only ship a placeholder, and a stale one makes the
	// relayer query a different client than the packets were committed against.
	t.Run("writes the wasm client ID", func(t *testing.T) {
		path := writeTempConfig(t, l2SourceConfigJSON)

		if err := writeL2SourceClientIDs(path, "cosmoshub-2", "08-wasm-5"); err != nil {
			t.Fatalf("writeL2SourceClientIDs: %v", err)
		}

		if got := readParsed(t, path).Modules[1].Config.L2WasmClientID; got != "08-wasm-5" {
			t.Errorf("l2_wasm_client_id = %q, want 08-wasm-5", got)
		}
	})

	t.Run("preserves siblings", func(t *testing.T) {
		path := writeTempConfig(t, l2SourceConfigJSON)

		if err := writeL2SourceClientIDs(path, "cosmoshub-2", "08-wasm-5"); err != nil {
			t.Fatalf("writeL2SourceClientIDs: %v", err)
		}

		cfg := readParsed(t, path)
		// The cosmos_to_l2 module shares the same client id; it must not be swept up by
		// the l2_to_cosmos write.
		if got := cfg.Modules[0].Config.CosmosWasmClientID; got != "08-wasm-0" {
			t.Errorf("cosmos_to_l2 cosmos_wasm_client_id = %q, want it untouched (08-wasm-0)", got)
		}
		l2 := cfg.Modules[1].Config
		if l2.HeadKind != "safe" {
			t.Errorf("head_kind = %q, want it untouched (safe)", l2.HeadKind)
		}
		if l2.RollupProfile.Common.L2Router != "0xrouter" {
			t.Errorf("l2_router = %q, want it untouched", l2.RollupProfile.Common.L2Router)
		}
		if l2.RollupProfile.Common.ProfileVersion != "op_attestor_v1" {
			t.Errorf("profile_version = %q, want it untouched", l2.RollupProfile.Common.ProfileVersion)
		}
	})

	// An empty id is a caller bug, not "nothing to persist": the only caller has just
	// created the client, so an empty value means the id was lost somewhere upstream and
	// silently leaving the placeholder in place is the failure this write-back prevents.
	t.Run("rejects empty wasm client ID", func(t *testing.T) {
		path := writeTempConfig(t, l2SourceConfigJSON)

		if err := writeL2SourceClientIDs(path, "cosmoshub-2", ""); err == nil {
			t.Fatal("writeL2SourceClientIDs accepted an empty l2_wasm_client_id, want an error")
		}

		if got := readParsed(t, path).Modules[1].Config.L2WasmClientID; got != "08-wasm-1" {
			t.Errorf("l2_wasm_client_id = %q, want the pre-existing 08-wasm-1 left alone", got)
		}
	})

	// A module that ships no placeholder at all must still end up with the id, otherwise
	// the operator is back to hand-copying it.
	t.Run("creates missing member", func(t *testing.T) {
		path := writeTempConfig(t, `{
  "modules": [
    {
      "src_chain": "opstack",
      "dst_chain": "cosmos",
      "config": {
        "l2_ics26_client_id": "cosmoshub-2",
        "rollup_profile": {}
      }
    }
  ]
}`)

		if err := writeL2SourceClientIDs(path, "cosmoshub-2", "08-wasm-5"); err != nil {
			t.Fatalf("writeL2SourceClientIDs: %v", err)
		}

		if got := readParsed(t, path).Modules[0].Config.L2WasmClientID; got != "08-wasm-5" {
			t.Errorf("l2_wasm_client_id = %q, want 08-wasm-5", got)
		}
	})

	// Several L2 sources share one config; the write must land on the addressed one.
	t.Run("targets matching source", func(t *testing.T) {
		path := writeTempConfig(t, `{
  "modules": [
    {
      "src_chain": "opstack",
      "dst_chain": "cosmos",
      "config": {
        "l2_ics26_client_id": "cosmoshub-2",
        "l2_wasm_client_id": "08-wasm-0"
      }
    },
    {
      "src_chain": "arbitrum",
      "dst_chain": "cosmos",
      "config": {
        "l2_ics26_client_id": "cosmoshub-3",
        "l2_wasm_client_id": "08-wasm-0"
      }
    }
  ]
}`)

		if err := writeL2SourceClientIDs(path, "cosmoshub-3", "08-wasm-7"); err != nil {
			t.Fatalf("writeL2SourceClientIDs: %v", err)
		}

		cfg := readParsed(t, path)
		if got := cfg.Modules[0].Config.L2WasmClientID; got != "08-wasm-0" {
			t.Errorf("opstack l2_wasm_client_id = %q, want it untouched", got)
		}
		if got := cfg.Modules[1].Config.L2WasmClientID; got != "08-wasm-7" {
			t.Errorf("arbitrum l2_wasm_client_id = %q, want 08-wasm-7", got)
		}
	})

	// A missing target must fail loudly: silently writing nothing is how a stale id
	// survives into a running deployment.
	t.Run("fails on unknown source", func(t *testing.T) {
		path := writeTempConfig(t, l2SourceConfigJSON)

		err := writeL2SourceClientIDs(path, "cosmoshub-9", "08-wasm-5")
		if err == nil {
			t.Fatal("writeL2SourceClientIDs succeeded for an unknown l2_ics26_client_id, want an error")
		}
	})

	// Classification must follow src_chain/dst_chain, not the free-form module name.
	t.Run("ignores module name", func(t *testing.T) {
		path := writeTempConfig(t, `{
  "modules": [
    {
      "name": "whatever-label",
      "src_chain": "opstack",
      "dst_chain": "cosmos",
      "config": {
        "l2_ics26_client_id": "cosmoshub-2",
        "l2_wasm_client_id": "08-wasm-0"
      }
    }
  ]
}`)

		if err := writeL2SourceClientIDs(path, "cosmoshub-2", "08-wasm-4"); err != nil {
			t.Fatalf("writeL2SourceClientIDs: %v", err)
		}
		if got := readParsed(t, path).Modules[0].Config.L2WasmClientID; got != "08-wasm-4" {
			t.Errorf("l2_wasm_client_id = %q, want 08-wasm-4", got)
		}
	})

	// Regression for a High found in review: a cosmos_to_eth and a cosmos_to_l2 module may
	// legitimately share an ics26_client_id (config.example.json ships exactly that), and
	// the direction-agnostic writer resolved the ambiguity by file order — writing the L2
	// client id into the Ethereum module and leaving the L2 one untouched.
	t.Run("wasm client ID respects module direction", func(t *testing.T) {
		const shared = `{
  "modules": [
    {"src_chain": "cosmos", "dst_chain": "ethereum",
     "config": {"ics26_client_id": "cosmoshub-1", "cosmos_wasm_client_id": "eth-original"}},
    {"src_chain": "cosmos", "dst_chain": "opstack",
     "config": {"ics26_client_id": "cosmoshub-1", "cosmos_wasm_client_id": "l2-original"}}
  ]
}`

		t.Run("l2 write lands on the cosmos_to_l2 module", func(t *testing.T) {
			path := writeTempConfig(t, shared)
			if err := writeL2WasmClientID(path, "cosmoshub-1", "08-wasm-7"); err != nil {
				t.Fatalf("writeL2WasmClientID: %v", err)
			}
			cfg := readParsed(t, path)
			if got := cfg.Modules[0].Config.CosmosWasmClientID; got != "eth-original" {
				t.Errorf("cosmos_to_eth cosmos_wasm_client_id = %q, want it untouched", got)
			}
			if got := cfg.Modules[1].Config.CosmosWasmClientID; got != "08-wasm-7" {
				t.Errorf("cosmos_to_l2 cosmos_wasm_client_id = %q, want 08-wasm-7", got)
			}
		})

		t.Run("eth write lands on the cosmos_to_eth module", func(t *testing.T) {
			path := writeTempConfig(t, shared)
			if err := writeEthWasmClientID(path, "cosmoshub-1", "08-wasm-4"); err != nil {
				t.Fatalf("writeEthWasmClientID: %v", err)
			}
			cfg := readParsed(t, path)
			if got := cfg.Modules[0].Config.CosmosWasmClientID; got != "08-wasm-4" {
				t.Errorf("cosmos_to_eth cosmos_wasm_client_id = %q, want 08-wasm-4", got)
			}
			if got := cfg.Modules[1].Config.CosmosWasmClientID; got != "l2-original" {
				t.Errorf("cosmos_to_l2 cosmos_wasm_client_id = %q, want it untouched", got)
			}
		})
	})
}

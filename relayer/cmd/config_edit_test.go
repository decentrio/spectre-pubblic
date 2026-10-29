package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestAssertConfigMemberWritable_AgreesWithTheWrite pins the property that makes the
// precheck trustworthy: it accepts exactly the configs the real write accepts. A
// re-implemented lookup could drift and let the spend through again.
func TestAssertConfigMemberWritable_AgreesWithTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name, body, sourceID string
		wantWritable         bool
	}{
		{"no cosmos_to_eth module", ethToCosmosPlusL2Config, "arb-client-0", false},
		{"unknown source id", validCosmosToEthConfig, "no-such-client", false},
		{"matching cosmos_to_eth module", validCosmosToEthConfig, "chain-a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFixture(t, tc.body)
			checkErr := assertConfigMemberWritable(path, tc.sourceID, "cosmos_wasm_client_id", dirCosmosToEth)
			writeErr := writeEthWasmClientID(path, tc.sourceID, "08-wasm-0")
			if (checkErr == nil) != (writeErr == nil) {
				t.Fatalf("precheck and write disagree: check=%v write=%v", checkErr, writeErr)
			}
			if (checkErr == nil) != tc.wantWritable {
				t.Fatalf("writable = %v (%v), want %v", checkErr == nil, checkErr, tc.wantWritable)
			}
		})
	}
}

// TestUnrecordedClientError_NamesTheClientAndTheRecovery covers the case the precheck
// cannot prevent — the destination existed but the write still failed (permissions, a
// concurrent edit). The id only exists in this message, so it has to carry both the id
// and the one recovery that does not create a second client.
func TestUnrecordedClientError_NamesTheClientAndTheRecovery(t *testing.T) {
	cause := errors.New("permission denied")
	err := unrecordedClientError("/etc/relayer/config.json", "08-wasm-7", cause)

	for _, want := range []string{
		"08-wasm-7",                // the id, which exists nowhere else
		"/etc/relayer/config.json", // where it should have gone
		"cosmos_wasm_client_id",    // the field to set by hand
		"create-clients-eth",       // the command that resumes the flow
		"Do NOT re-run",            // the reaction that would orphan it
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q:\n%s", want, err)
		}
	}
	if !errors.Is(err, cause) {
		t.Error("wrapped cause is not retrievable with errors.Is")
	}
}

func TestReplaceConfigMember_SpectreClientAddress(t *testing.T) {
	t.Parallel()

	const addr = "0x1234567890abcdef1234567890abcdef12345678"

	tests := []struct {
		name   string
		input  string
		assert func(t *testing.T, out []byte, cosmosToEth map[string]any)
	}{
		{
			name: "replaces existing spectre_client",
			input: `{
				"modules": [
					{"name": "cosmos_to_eth", "config": {"spectre_client": "0xold", "other": "keep"}},
					{"name": "eth_to_cosmos", "config": {"spectre_client": "0xother"}}
				]
			}`,
			assert: func(t *testing.T, out []byte, cosmosToEth map[string]any) {
				if got := cosmosToEth["other"]; got != "keep" {
					t.Fatalf("other field = %v, want keep", got)
				}
				ethToCosmos := moduleConfigByName(t, out, "eth_to_cosmos")
				if got := ethToCosmos["spectre_client"]; got != "0xother" {
					t.Fatalf("eth_to_cosmos spectre_client = %v, want 0xother", got)
				}
			},
		},
		{
			name: "inserts spectre_client when absent",
			input: `{
				"modules": [
					{"name": "cosmos_to_eth", "config": {"eth_rpc_url": "http://localhost", "nested": {"keep": true}}}
				]
			}`,
			assert: func(t *testing.T, _ []byte, cosmosToEth map[string]any) {
				if got := cosmosToEth["eth_rpc_url"]; got != "http://localhost" {
					t.Fatalf("eth_rpc_url = %v, want http://localhost", got)
				}
				nested, ok := cosmosToEth["nested"].(map[string]any)
				if !ok {
					t.Fatalf("nested = %T, want object", cosmosToEth["nested"])
				}
				if got := nested["keep"]; got != true {
					t.Fatalf("nested.keep = %v, want true", got)
				}
			},
		},
		{
			name: "preserves nested config objects",
			input: `{
				"modules": [
					{"name": "cosmos_to_eth", "config": {"nested": {"config": {"spectre_client": "nested"}, "items": [{"k": "v,}]"}]}, "spectre_client": "0xold"}}
				]
			}`,
			assert: func(t *testing.T, _ []byte, cosmosToEth map[string]any) {
				nested, ok := cosmosToEth["nested"].(map[string]any)
				if !ok {
					t.Fatalf("nested = %T, want object", cosmosToEth["nested"])
				}
				inner, ok := nested["config"].(map[string]any)
				if !ok {
					t.Fatalf("nested.config = %T, want object", nested["config"])
				}
				if got := inner["spectre_client"]; got != "nested" {
					t.Fatalf("nested.config.spectre_client = %v, want nested", got)
				}
			},
		},
		{
			name: "handles escaped strings and keys",
			input: `{
				"modules": [
					{"name": "cosmos\u005fto\u005feth", "config": {"note": "quote: \" and slash: \\", "spectre\u005fclient": "0xold"}}
				]
			}`,
			assert: func(t *testing.T, _ []byte, cosmosToEth map[string]any) {
				if got := cosmosToEth["note"]; got != `quote: " and slash: \` {
					t.Fatalf("note = %q, want escaped string preserved", got)
				}
			},
		},
		{
			name:  "handles unusual whitespace",
			input: "{\n\t\"modules\"\t:\t[\n\t { \n \"name\" : \"cosmos_to_eth\" ,\n \"config\" : { \n \"foo\" : [1, {\"bar\" : \"baz\"}] \n } \n }\n]\n}",
			assert: func(t *testing.T, _ []byte, cosmosToEth map[string]any) {
				foo, ok := cosmosToEth["foo"].([]any)
				if !ok {
					t.Fatalf("foo = %T, want array", cosmosToEth["foo"])
				}
				if len(foo) != 2 {
					t.Fatalf("foo length = %d, want 2", len(foo))
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, err := replaceConfigMember([]byte(tc.input), "spectre_client", addr)
			if err != nil {
				t.Fatalf("replaceConfigMember() error = %v", err)
			}

			cosmosToEth := moduleConfigByName(t, out, "cosmos_to_eth")
			if got := cosmosToEth["spectre_client"]; got != addr {
				t.Fatalf("spectre_client = %v, want %s", got, addr)
			}
			tc.assert(t, out, cosmosToEth)
		})
	}
}

func TestReplaceConfigMember_WasmClientID(t *testing.T) {
	t.Parallel()

	const id = "08-wasm-445"

	t.Run("inserts cosmos_wasm_client_id when absent", func(t *testing.T) {
		t.Parallel()
		in := `{
			"modules": [
				{"name": "cosmos_to_eth", "config": {"spectre_client": "0xabc"}}
			]
		}`
		out, err := replaceConfigMember([]byte(in), "cosmos_wasm_client_id", id)
		if err != nil {
			t.Fatalf("replaceConfigMember() error = %v", err)
		}
		cfg := moduleConfigByName(t, out, "cosmos_to_eth")
		if got := cfg["cosmos_wasm_client_id"]; got != id {
			t.Fatalf("cosmos_wasm_client_id = %v, want %s", got, id)
		}
		if got := cfg["spectre_client"]; got != "0xabc" {
			t.Fatalf("spectre_client = %v, want preserved 0xabc", got)
		}
	})

	t.Run("replaces existing cosmos_wasm_client_id", func(t *testing.T) {
		t.Parallel()
		in := `{
			"modules": [
				{"name": "cosmos_to_eth", "config": {"cosmos_wasm_client_id": "08-wasm-0", "keep": "yes"}}
			]
		}`
		out, err := replaceConfigMember([]byte(in), "cosmos_wasm_client_id", id)
		if err != nil {
			t.Fatalf("replaceConfigMember() error = %v", err)
		}
		cfg := moduleConfigByName(t, out, "cosmos_to_eth")
		if got := cfg["cosmos_wasm_client_id"]; got != id {
			t.Fatalf("cosmos_wasm_client_id = %v, want %s", got, id)
		}
		if got := cfg["keep"]; got != "yes" {
			t.Fatalf("keep = %v, want yes", got)
		}
	})
}

func moduleConfigByName(t *testing.T, data []byte, name string) map[string]any {
	t.Helper()

	var cfg struct {
		Modules []struct {
			Name   string         `json:"name"`
			Config map[string]any `json:"config"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, string(data))
	}
	for _, m := range cfg.Modules {
		if m.Name == name {
			if m.Config == nil {
				t.Fatalf("module %q config is nil", name)
			}
			return m.Config
		}
	}
	t.Fatalf("module %q not found", name)
	return nil
}

// TestReplaceConfigMemberForSource confirms a write-back targets only the named
// source's module and leaves the other source untouched.
func TestReplaceConfigMemberForSource(t *testing.T) {
	t.Parallel()

	in := `{"modules":[` +
		`{"name":"cosmos_to_eth","config":{"ics26_client_id":"chain-a","spectre_client":"0xAAA"}},` +
		`{"name":"cosmos_to_eth","config":{"ics26_client_id":"chain-b","spectre_client":"0xBBB"}}` +
		`]}`
	out, err := replaceConfigMemberForSource([]byte(in), "chain-b", "spectre_client", "0xNEW")
	if err != nil {
		t.Fatalf("replaceConfigMemberForSource() error = %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"chain-a","spectre_client":"0xAAA"`) {
		t.Fatalf("chain-a should be untouched, got: %s", got)
	}
	if !strings.Contains(got, `"chain-b","spectre_client":"0xNEW"`) {
		t.Fatalf("chain-b should be updated, got: %s", got)
	}

	// TestReplaceConfigMemberForSource/unknown confirms an unknown source errors
	// rather than silently writing the wrong module.
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()

		in := `{"modules":[{"name":"cosmos_to_eth","config":{"ics26_client_id":"chain-a"}}]}`
		_, err := replaceConfigMemberForSource([]byte(in), "chain-x", "spectre_client", "0x1")
		if err == nil || !strings.Contains(err.Error(), "chain-x") {
			t.Fatalf("error = %v, want unknown-source error", err)
		}
	})

	// TestReplaceConfigMemberForSource/via src chain confirms the module-level src_chain
	// is used as the source id when config.ics26_client_id is absent (matching how
	// loadConfig defaults the id).
	t.Run("via src chain", func(t *testing.T) {
		t.Parallel()

		in := `{"modules":[{"name":"cosmos_to_eth","src_chain":"chain-a","config":{"spectre_client":"0xAAA"}}]}`
		out, err := replaceConfigMemberForSource([]byte(in), "chain-a", "spectre_client", "0xNEW")
		if err != nil {
			t.Fatalf("replaceConfigMemberForSource() error = %v", err)
		}
		if !strings.Contains(string(out), `"spectre_client":"0xNEW"`) {
			t.Fatalf("src_chain match failed, got: %s", string(out))
		}
	})

	// TestReplaceConfigMemberForSource/canonical is the regression for the canonical
	// schema: the module carries a free-form name label ("cosmos-to-eth") and is
	// classified by src_chain/dst_chain, exactly as loadConfig/classifyModule does.
	// The old write-back matched name == "cosmos_to_eth" and so failed to find a
	// canonical module after create-clients mutated chain state. It must also skip the
	// eth->cosmos module even when it shares the source id.
	t.Run("canonical", func(t *testing.T) {
		t.Parallel()

		in := `{"modules":[` +
			`{"name":"eth-to-cosmos","src_chain":"ethereum","dst_chain":"cosmos","config":{"ics26_client_id":"chain-a","spectre_client":"0xETH"}},` +
			`{"name":"cosmos-to-eth","src_chain":"cosmos","dst_chain":"ethereum","config":{"ics26_client_id":"chain-a","spectre_client":"0xAAA"}}` +
			`]}`
		out, err := replaceConfigMemberForSource([]byte(in), "chain-a", "spectre_client", "0xNEW")
		if err != nil {
			t.Fatalf("replaceConfigMemberForSource() error = %v", err)
		}
		got := string(out)
		if !strings.Contains(got, `"ics26_client_id":"chain-a","spectre_client":"0xNEW"`) {
			t.Fatalf("canonical cosmos->eth module not updated, got: %s", got)
		}
		// The eth->cosmos module (same source id, wrong direction) must be untouched.
		if !strings.Contains(got, `"spectre_client":"0xETH"`) {
			t.Fatalf("eth->cosmos module must be skipped, got: %s", got)
		}
	})
}

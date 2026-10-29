package main

import (
	attestorpb "attestor/types/attestor"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"relayer/chain"
	"relayer/chain/l2rollup"
	"relayer/services"
	"strings"
	"testing"
	"time"

	channeltypesv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/ethereum/go-ethereum/common"
)

func validL2Config() l2ToCosmosConfig {
	return l2ToCosmosConfig{
		L2RpcUrl:          "http://127.0.0.1:9545",
		TmRpcUrl:          "http://127.0.0.1:26657",
		AttestorEndpoints: []l2AttestorEndpointConfig{{Address: "127.0.0.1:3001", AttestorIndex: 0}},
		AttestorSrcChain:  "op-sepolia",
		Attestors:         validL2Attestors(),
		L2WasmClientID:    "08-wasm-1",
		L2ICS26ClientID:   "client-0",
		HeadKind:          "safe",
		RollupProfile:     json.RawMessage(`{"common":{"l2_chain_id":10,"l2_router":"0x1111111111111111111111111111111111111111"}}`),
		kind:              chain.OPStack,
	}
}

func TestL2ToCosmosConfig_Validate(t *testing.T) {
	if err := validL2Config().validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := map[string]func(*l2ToCosmosConfig){
		"missing l2_rpc_url":        func(c *l2ToCosmosConfig) { c.L2RpcUrl = "" },
		"blank attestor source":     func(c *l2ToCosmosConfig) { c.AttestorSrcChain = " \t" },
		"missing attestor endpoint": func(c *l2ToCosmosConfig) { c.AttestorEndpoints = nil },
		"missing wasm client id":    func(c *l2ToCosmosConfig) { c.L2WasmClientID = "" },
		"empty profile":             func(c *l2ToCosmosConfig) { c.RollupProfile = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validL2Config()
			mutate(&c)
			if err := c.validate(); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

// TestL2ToCosmosConfig_ValidateForClientCreation covers #309: create-clients-cosmos is the
// command that CREATES the L2 wasm client and writes its id back into the config,
// so requiring that id to already be present made a fresh config unusable — the
// operator had to invent a plausible one, which the command then overwrote. Only
// that id is exempt; everything else is still needed to reach the chain at all.
func TestL2ToCosmosConfig_ValidateForClientCreation(t *testing.T) {
	c := validL2Config()
	c.L2WasmClientID = ""
	if err := c.validateForClientCreation(); err != nil {
		t.Fatalf("empty l2_wasm_client_id rejected during client creation: %v", err)
	}

	// The exemption is narrow: it must not spill onto the other required fields.
	for name, mutate := range map[string]func(*l2ToCosmosConfig){
		"missing l2_rpc_url":        func(c *l2ToCosmosConfig) { c.L2RpcUrl = "" },
		"missing attestor endpoint": func(c *l2ToCosmosConfig) { c.AttestorEndpoints = nil },
		"missing l2_ics26_client":   func(c *l2ToCosmosConfig) { c.L2ICS26ClientID = "" },
		"missing tm_rpc_url":        func(c *l2ToCosmosConfig) { c.TmRpcUrl = "" },
		"missing attestor_src":      func(c *l2ToCosmosConfig) { c.AttestorSrcChain = "" },
		"blank attestor_src":        func(c *l2ToCosmosConfig) { c.AttestorSrcChain = " \t" },
		"empty profile":             func(c *l2ToCosmosConfig) { c.RollupProfile = nil },
		"bad head_kind":             func(c *l2ToCosmosConfig) { c.HeadKind = "nonsense" },
	} {
		t.Run(name, func(t *testing.T) {
			c := validL2Config()
			c.L2WasmClientID = ""
			mutate(&c)
			if err := c.validateForClientCreation(); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}

	// A config that is already complete stays valid on this path too.
	if err := validL2Config().validateForClientCreation(); err != nil {
		t.Fatalf("complete config rejected: %v", err)
	}
}

// TestLoadConfigForClientCreation_AcceptsUncreatedWasmClient is the end-to-end of
// the above: the whole config file loads, rather than failing at the first module.
func TestLoadConfigForClientCreation_AcceptsUncreatedWasmClient(t *testing.T) {
	raw := `{"modules":[
		{"name":"cosmos-to-arb","src_chain":"cosmos","dst_chain":"arbitrum","config":{
			"cosmos_wasm_client_id":"","eth_rpc_url":"http://l2","tm_rpc_url":"http://tm",
			"ics26_address":"0x1111111111111111111111111111111111111111",
			"ics26_client_id":"arb-client-0"}},
		{"name":"arb-to-cosmos","src_chain":"arbitrum","dst_chain":"cosmos","config":{
			"l2_rpc_url":"http://l2","tm_rpc_url":"http://tm",
			"attestor_endpoints":[{"address":"127.0.0.1:3002","attestor_index":0}],"attestor_src_chain":"arbitrum-sepolia",
			"attestors":{"public_keys":["gojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w="],"threshold":1},
			"l2_wasm_client_id":"","l2_ics26_client_id":"arb-client-0","head_kind":"unsafe",
			"rollup_profile":{"common":{"l2_chain_id":10,"l2_router":"0x1111111111111111111111111111111111111111"}}}}]}`

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := loadConfig(path); err == nil {
		t.Fatal("loadConfig accepted an empty l2_wasm_client_id; the relay path needs it")
	}

	cfg, err := loadConfigForClientCreation(path)
	if err != nil {
		t.Fatalf("loadConfigForClientCreation rejected a pre-creation config: %v", err)
	}
	if !cfg.HasL2ToCosmos {
		t.Fatal("HasL2ToCosmos = false, want the module recorded")
	}
	if got := cfg.L2ToCosmosConfig.L2ICS26ClientID; got != "arb-client-0" {
		t.Errorf("l2_ics26_client_id = %q, want arb-client-0", got)
	}

	// Closing the loop: the id the command creates lands in the field that was
	// empty, and the config then loads on the strict path. Without this the
	// relaxation above would just move the failure one step later.
	if err := writeL2SourceClientIDs(path, "arb-client-0", "08-wasm-0"); err != nil {
		t.Fatalf("write back the created client id: %v", err)
	}
	reloaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig after write-back: %v", err)
	}
	if got := reloaded.L2ToCosmosConfig.L2WasmClientID; got != "08-wasm-0" {
		t.Errorf("l2_wasm_client_id = %q, want 08-wasm-0", got)
	}
}

func TestParseHeadKind(t *testing.T) {
	cases := map[string]l2rollup.HeadKind{
		"":          l2rollup.Safe,
		"safe":      l2rollup.Safe,
		"unsafe":    l2rollup.Unsafe,
		"finalized": l2rollup.Finalized,
	}
	for in, want := range cases {
		got, err := parseHeadKind(in)
		if err != nil {
			t.Fatalf("parseHeadKind(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("parseHeadKind(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseHeadKind("nonsense"); err == nil {
		t.Fatal("expected error for invalid head_kind")
	}
}

func TestResolveIncludeProvisional(t *testing.T) {
	trueValue, falseValue := true, false
	cases := []struct {
		name        string
		headKind    l2rollup.HeadKind
		override    *bool
		wantInclude bool
		wantWarning string
	}{
		{name: "unsafe default", headKind: l2rollup.Unsafe, wantInclude: true},
		{name: "safe default", headKind: l2rollup.Safe, wantInclude: true},
		{name: "finalized default", headKind: l2rollup.Finalized, wantInclude: false},
		{name: "finalized explicit false", headKind: l2rollup.Finalized, override: &falseValue, wantInclude: false},
		{
			name:        "finalized explicit true warns that the height gate accepts provisional roots",
			headKind:    l2rollup.Finalized,
			override:    &trueValue,
			wantInclude: true,
			wantWarning: "l2->cosmos finalized head_kind with include_provisional=true accepts provisional attestor roots for relayability; state-root verification remains finalized",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotInclude, gotWarning := resolveIncludeProvisional(tc.headKind, tc.override)
			if gotInclude != tc.wantInclude || gotWarning != tc.wantWarning {
				t.Fatalf("resolveIncludeProvisional() = (%v, %q), want (%v, %q)", gotInclude, gotWarning, tc.wantInclude, tc.wantWarning)
			}
		})
	}
}

type timeoutRecoveryFrontier struct {
	root       *attestorpb.AttestedRoot
	gotChain   string
	gotInclude bool
}

func (f *timeoutRecoveryFrontier) AttestedUpTo(_ context.Context, srcChain string, includeProvisional bool) (*attestorpb.AttestedRoot, bool, error) {
	f.gotChain = srcChain
	f.gotInclude = includeProvisional
	return f.root, true, nil
}

type timeoutRecoveryMessage struct{ at time.Time }

func (m timeoutRecoveryMessage) EncodeClientMessage() ([]byte, error) {
	return []byte(`{"type":"header"}`), nil
}
func (m timeoutRecoveryMessage) TrustedAt() time.Time { return m.at }

type timeoutRecoveryHeaderBuilder struct{ requested uint64 }

func (b *timeoutRecoveryHeaderBuilder) Name() string { return "test-attestor" }
func (b *timeoutRecoveryHeaderBuilder) BuildHeader(_ context.Context, request l2rollup.HeaderRequest) (l2rollup.ClientMessage, uint64, error) {
	b.requested = request.Height
	return timeoutRecoveryMessage{at: time.Unix(1_700_000_000, 0)}, request.Height, nil
}

func TestNewL2TimeoutRecovery_UsesAttestedFrontier(t *testing.T) {
	cfg := validL2Config()
	frontier := &timeoutRecoveryFrontier{root: &attestorpb.AttestedRoot{L2BlockNumber: 88}}
	builder := &timeoutRecoveryHeaderBuilder{}
	recovery := newL2TimeoutRecovery(cfg, builder, frontier, nil, common.Address{}, false)

	update, err := recovery.BuildUpdate(context.Background())
	if err != nil {
		t.Fatalf("build timeout update: %v", err)
	}
	if frontier.gotChain != cfg.AttestorSrcChain || frontier.gotInclude {
		t.Fatalf("frontier query = chain=%q include_provisional=%t, want %q false", frontier.gotChain, frontier.gotInclude, cfg.AttestorSrcChain)
	}
	if builder.requested != 88 || update.Height != 88 || update.Timestamp != 1_700_000_000 {
		t.Fatalf("attested update = requested=%d height=%d timestamp=%d, want 88/88/1700000000", builder.requested, update.Height, update.Timestamp)
	}
	if recovery.ClientID != cfg.L2WasmClientID || string(update.Payload) != `{"type":"header"}` || recovery.BuildNonMembershipProof == nil {
		t.Fatalf("recovery = %+v, want paired wasm client, header payload, and L2 proof callback", recovery)
	}
}

func TestL2RouterFromProfile(t *testing.T) {
	addr, err := l2RouterFromProfile(json.RawMessage(`{"common":{"l2_router":"0xAbC1111111111111111111111111111111111111"}}`))
	if err != nil {
		t.Fatalf("valid router rejected: %v", err)
	}
	// Address parsing is case-insensitive; the canonical hex is lowercased internally.
	if !strings.EqualFold(addr.Hex(), "0xAbC1111111111111111111111111111111111111") {
		t.Fatalf("router = %s, want 0xAbC1111111111111111111111111111111111111", addr.Hex())
	}

	for _, bad := range []string{`{"common":{"l2_router":"0xnothex"}}`, `{"common":{}}`, `{`} {
		if _, err := l2RouterFromProfile(json.RawMessage(bad)); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}

	// TestL2RouterFromProfile/example L2 router matches pair pins the one cross-module invariant the example
	// has to demonstrate: a return module's rollup_profile.common.l2_router is the
	// SAME contract as its forward pair's ics26_address. deploy_l2_contracts.sh
	// writes both from one deploy, so an example where they disagree teaches the
	// mistake that used to break the return direction silently.
	t.Run("example L2 router matches pair", func(t *testing.T) {
		t.Parallel()

		raw, err := os.ReadFile(filepath.Join("..", "config.example.json"))
		if err != nil {
			t.Fatalf("read catalogue: %v", err)
		}
		var parsed struct {
			Modules []struct {
				Name   string `json:"name"`
				Config struct {
					ICS26Address    string `json:"ics26_address"`
					ICS26ClientID   string `json:"ics26_client_id"`
					L2ICS26ClientID string `json:"l2_ics26_client_id"`
					RollupProfile   *struct {
						Common struct {
							L2Router string `json:"l2_router"`
						} `json:"common"`
					} `json:"rollup_profile"`
				} `json:"config"`
			} `json:"modules"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("parse catalogue: %v", err)
		}

		routerByClientID := map[string]string{}
		for _, m := range parsed.Modules {
			if m.Config.ICS26ClientID != "" && m.Config.ICS26Address != "" {
				routerByClientID[m.Config.ICS26ClientID] = m.Config.ICS26Address
			}
		}

		checked := 0
		for _, m := range parsed.Modules {
			if m.Config.RollupProfile == nil {
				continue
			}
			want, ok := routerByClientID[m.Config.L2ICS26ClientID]
			if !ok {
				t.Errorf("module %q has l2_ics26_client_id %q with no forward pair",
					m.Name, m.Config.L2ICS26ClientID)
				continue
			}
			if got := m.Config.RollupProfile.Common.L2Router; got != want {
				t.Errorf("module %q l2_router = %s, want its pair's ics26_address %s",
					m.Name, got, want)
			}
			checked++
		}
		if checked == 0 {
			t.Fatal("no l2_to_cosmos modules were checked; the catalogue lost its L2 pairs")
		}
	})
}

func newChainIDRPCServer(t *testing.T, chainIDHex string) *httptest.Server {
	t.Helper()
	type rpcReq struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Method  string `json:"method"`
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode rpc request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Method != "eth_chainId" {
			t.Errorf("rpc method = %q, want eth_chainId", req.Method)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  chainIDHex,
		})
	}))
}

func TestFindL2TimeoutReturnPath(t *testing.T) {
	sourceRPC := newChainIDRPCServer(t, "0xa")
	t.Cleanup(sourceRPC.Close)

	src := validL2Config()
	src.L2RpcUrl = sourceRPC.URL
	src.TmRpcUrl = "http://cosmos-a"
	src.RollupProfile = json.RawMessage(`{"common":{"l2_chain_id":10,"l2_router":"0x1111111111111111111111111111111111111111"}}`)
	matchingDest := cosmosToEthConfig{
		EthRpcUrl:     "http://write-l2-a",
		TmRpcUrl:      "http://cosmos-a",
		ICS26Address:  "0x1111111111111111111111111111111111111111",
		ICS26ClientID: "cosmos-on-l2",
	}

	tests := []struct {
		name    string
		paths   []l2TimeoutReturnPathConfig
		wantErr string
	}{
		{
			name:  "matches by l2 chain id cosmos rpc and router",
			paths: []l2TimeoutReturnPathConfig{{name: "cosmos-to-op", cfg: matchingDest, l2ChainID: "10"}},
		},
		{
			name: "no match fails",
			paths: []l2TimeoutReturnPathConfig{{cfg: cosmosToEthConfig{
				EthRpcUrl:    "http://write-l2-b",
				TmRpcUrl:     "http://cosmos-a",
				ICS26Address: "0x1111111111111111111111111111111111111111",
			}, name: "cosmos-to-base", l2ChainID: "11"}},
			wantErr: "candidates: cosmos-to-base: l2_chain_id=11",
		},
		{
			name: "duplicate match fails",
			paths: []l2TimeoutReturnPathConfig{
				{name: "cosmos-to-op-a", cfg: matchingDest, l2ChainID: "10"},
				{name: "cosmos-to-op-b", cfg: matchingDest, l2ChainID: "10"},
			},
			wantErr: "multiple cosmos_to_l2 return paths match",
		},
		{
			name: "bad destination router fails",
			paths: []l2TimeoutReturnPathConfig{{cfg: cosmosToEthConfig{
				EthRpcUrl:    "http://write-l2-a",
				TmRpcUrl:     "http://cosmos-a",
				ICS26Address: "0xnothex",
			}, name: "cosmos-to-bad", l2ChainID: "10"}},
			wantErr: "cosmos-to-bad.ics26_address",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := findL2TimeoutReturnPath(context.Background(), src, tc.paths)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("findL2TimeoutReturnPath() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("findL2TimeoutReturnPath() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestL2PendingTrackerHooks(t *testing.T) {
	svc := services.New(nil, nil, services.DefaultConfig())
	track, untrack := l2PendingTrackerHooks(svc)

	packet := channeltypesv2.Packet{SourceClient: "client-0", Sequence: 11}
	raw, err := packet.Marshal()
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}

	track(raw, 789)
	pending := svc.BatchBuilder.L2PendingTracker.GetAll()
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	if pending[0].BlockNumber != 789 {
		t.Fatalf("pending block = %d, want 789", pending[0].BlockNumber)
	}

	untrack(raw)
	if got := svc.BatchBuilder.L2PendingTracker.Len(); got != 0 {
		t.Fatalf("pending len after untrack = %d, want 0", got)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

type testChainIDReader struct {
	chainID *big.Int
	err     error
}

func (r testChainIDReader) ChainID(context.Context) (*big.Int, error) {
	return r.chainID, r.err
}

type testL2ChainIDReader struct {
	chainID *big.Int
	err     error
}

func (r testL2ChainIDReader) L2ChainID(context.Context) (*big.Int, error) {
	return r.chainID, r.err
}

func TestValidateChainID(t *testing.T) {
	for _, tc := range []struct {
		name string
		read testChainIDReader
		want bool
	}{
		{name: "matching", read: testChainIDReader{chainID: big.NewInt(10)}},
		{name: "wrong", read: testChainIDReader{chainID: big.NewInt(11)}, want: true},
		{name: "query error", read: testChainIDReader{err: errors.New("rpc down")}, want: true},
		{name: "nil", read: testChainIDReader{}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChainID(context.Background(), tc.read, 10, "test endpoint")
			if (err != nil) != tc.want {
				t.Fatalf("validateChainID error = %v, want error=%t", err, tc.want)
			}
		})
	}
}

func TestValidateL2ChainID(t *testing.T) {
	for _, tc := range []struct {
		name string
		read testL2ChainIDReader
		want bool
	}{
		{name: "matching", read: testL2ChainIDReader{chainID: big.NewInt(10)}},
		{name: "wrong", read: testL2ChainIDReader{chainID: big.NewInt(11)}, want: true},
		{name: "query error", read: testL2ChainIDReader{err: errors.New("rpc down")}, want: true},
		{name: "nil", read: testL2ChainIDReader{}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateL2ChainID(context.Background(), tc.read, 10)
			if (err != nil) != tc.want {
				t.Fatalf("validateL2ChainID error = %v, want error=%t", err, tc.want)
			}
		})
	}
}

func TestLoadConfigExample(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(filepath.Join("..", "config.example.json"))
	if err != nil {
		t.Fatalf("loadConfig(config.example.json) error = %v", err)
	}
	if got := len(cfg.Sources); got != 1 {
		t.Fatalf("Sources len = %d, want 1", got)
	}
	op := cfg.Sources[0]
	if op.SrcChain != "op-mainnet" {
		t.Fatalf("op_source src_chain = %q, want op-mainnet", op.SrcChain)
	}
	if op.OpNodeRpcUrl == "" || op.DisputeGameFactory == "" || op.StatePath == "" {
		t.Fatalf("op_source fields not parsed: %+v", op)
	}
	if op.AttestationHead != "finalized" {
		t.Fatalf("op_source attestation_head = %q, want finalized", op.AttestationHead)
	}
	if cfg.Server.GrpcPort == 0 {
		t.Fatal("server.grpc_port not parsed")
	}
}

func TestLoadConfigRejectsDuplicateSources(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, body string) string {
		t.Helper()
		configPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return configPath
	}

	t.Run("duplicate src_chain", func(t *testing.T) {
		configPath := write(t, `{"modules":[`+
			`{"name":"op_source","src_chain":"op-a","config":{"state_path":"a.json"}},`+
			`{"name":"op_source","src_chain":"op-a","config":{"state_path":"b.json"}}`+
			`]}`)
		if _, err := loadConfig(configPath); err == nil || !strings.Contains(err.Error(), "duplicate op_source src_chain") {
			t.Fatalf("error = %v, want duplicate src_chain error", err)
		}
	})
	t.Run("duplicate state_path", func(t *testing.T) {
		configPath := write(t, `{"modules":[`+
			`{"name":"op_source","src_chain":"op-a","config":{"state_path":"same.json"}},`+
			`{"name":"op_source","src_chain":"op-b","config":{"state_path":"same.json"}}`+
			`]}`)
		if _, err := loadConfig(configPath); err == nil || !strings.Contains(err.Error(), "duplicate op_source state_path") {
			t.Fatalf("error = %v, want duplicate state_path error", err)
		}
	})
	t.Run("missing src_chain", func(t *testing.T) {
		configPath := write(t, `{"modules":[`+
			`{"name":"op_source","config":{"state_path":"a.json"}}`+
			`]}`)
		if _, err := loadConfig(configPath); err == nil || !strings.Contains(err.Error(), "src_chain") {
			t.Fatalf("error = %v, want missing src_chain error", err)
		}
	})
	t.Run("other modules ignored", func(t *testing.T) {
		configPath := write(t, `{"modules":[`+
			`{"name":"cosmos_to_eth","src_chain":"hub","config":{"tm_rpc_url":"http://x"}}`+
			`]}`)
		cfg, err := loadConfig(configPath)
		if err != nil || len(cfg.Sources) != 0 {
			t.Fatalf("(%+v, %v), want zero sources and no error", cfg, err)
		}
	})
}

func TestLoadConfigFailureMatrix(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("missing config error = %v", err)
	}
	if _, err := loadConfig(write(t, "{")); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("malformed JSON error = %v", err)
	}
	if _, err := loadConfig(write(t, `{"modules":[{"name":"op_source","src_chain":"op","config":true}]}`)); err == nil || !strings.Contains(err.Error(), "parse op_source") {
		t.Fatalf("malformed op_source config error = %v", err)
	}
	if _, err := loadConfig(write(t, `{"modules":[{"name":"op_source","src_chain":"op","config":{"l1_ws_url":":"}}]}`)); err == nil || !strings.Contains(err.Error(), "l1_ws_url") {
		t.Fatalf("malformed L1 websocket URL error = %v", err)
	}
}

func TestValidateHexAddressFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", want: "empty"},
		{name: "malformed", value: "not-an-address", want: "invalid hex address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateHexAddress(tc.value, "factory"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateHexAddress error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := validateHexAddress("0x0000000000000000000000000000000000000001", "factory"); err != nil {
		t.Fatalf("validateHexAddress valid address: %v", err)
	}
}

func TestBuildOpAttestorRejectsMissingSigningKeyBeforeNetworkDial(t *testing.T) {
	_, _, _, err := buildOpAttestor(context.Background(), zap.NewNop(), opSourceConfig{
		SrcChain:              "op-mainnet",
		DisputeGameFactory:    "0x0000000000000000000000000000000000000001",
		L2ChainID:             10,
		AttestationSigningKey: "",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "attestation signer") {
		t.Fatalf("buildOpAttestor missing signing key error = %v", err)
	}
}

func TestBuildOpAttestorRejectsInvalidInputBeforeNetworkDial(t *testing.T) {
	validSigningKey := strings.Repeat("11", 32)
	for _, tc := range []struct {
		name   string
		mutate func(*opSourceConfig)
		want   string
	}{
		{
			name:   "invalid factory address",
			mutate: func(c *opSourceConfig) { c.DisputeGameFactory = "not-an-address" },
			want:   "invalid hex address",
		},
		{
			name:   "missing L1 RPC",
			mutate: func(c *opSourceConfig) { c.L1RpcUrl = "" },
			want:   "l1_rpc_url",
		},
		{
			name:   "missing op-node RPC",
			mutate: func(c *opSourceConfig) { c.OpNodeRpcUrl = "" },
			want:   "op_node_rpc_url",
		},
		{
			name:   "missing state path",
			mutate: func(c *opSourceConfig) { c.StatePath = "" },
			want:   "state_path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := opSourceConfig{
				SrcChain:              "op-mainnet",
				L1RpcUrl:              "https://ethereum.example",
				L1ChainID:             1,
				OpNodeRpcUrl:          "https://op-node.example",
				DisputeGameFactory:    "0x0000000000000000000000000000000000000001",
				L2ChainID:             10,
				AttestationSigningKey: validSigningKey,
				StatePath:             "attestor-state.json",
			}
			tc.mutate(&config)
			_, _, _, err := buildOpAttestor(context.Background(), zap.NewNop(), config, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildOpAttestor error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The configured op_node_rpc_url is a standard op-node endpoint. It exposes
// optimism_* methods, not the execution client's eth_* namespace. Exercise
// the complete startup wiring against such an endpoint so an eth_chainId
// regression fails before reaching deployment.
func TestBuildOpAttestorValidatesL2IdentityThroughOpNode(t *testing.T) {
	l1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode L1 RPC request: %v", err)
			return
		}
		var result any
		switch req.Method {
		case "eth_chainId":
			result = "0x1"
		case "eth_getCode":
			result = "0x6000"
		default:
			t.Errorf("unexpected L1 RPC method %q", req.Method)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer l1.Close()

	for _, tc := range []struct {
		name        string
		rollupL2ID  uint64
		wantErrText string
	}{
		{name: "matching L2 identity", rollupL2ID: 10},
		{name: "mismatched L2 identity", rollupL2ID: 11, wantErrText: "op-node L2 chain ID is 11, expected 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := make(map[string]int)
			opNode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode op-node RPC request: %v", err)
					return
				}
				mu.Lock()
				calls[req.Method]++
				mu.Unlock()

				if req.Method != "optimism_rollupConfig" {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": req.ID,
						"error": map[string]any{"code": -32601, "message": "method not found"},
					})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"l2_chain_id": tc.rollupL2ID},
				})
			}))
			defer opNode.Close()

			_, _, cleanup, err := buildOpAttestor(context.Background(), zap.NewNop(), opSourceConfig{
				SrcChain:              "op-mainnet",
				L1RpcUrl:              l1.URL,
				L1ChainID:             1,
				OpNodeRpcUrl:          opNode.URL,
				L2ChainID:             10,
				DisputeGameFactory:    "0x0000000000000000000000000000000000000001",
				AttestationSigningKey: strings.Repeat("11", 32),
				StatePath:             filepath.Join(t.TempDir(), "state.json"),
			}, nil)
			if tc.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("buildOpAttestor error = %v, want %q", err, tc.wantErrText)
				}
			} else if err != nil {
				t.Fatalf("buildOpAttestor: %v", err)
			} else {
				defer cleanup()
			}

			mu.Lock()
			defer mu.Unlock()
			if calls["optimism_rollupConfig"] != 1 {
				t.Fatalf("optimism_rollupConfig calls = %d, want 1 (all calls: %+v)", calls["optimism_rollupConfig"], calls)
			}
			if calls["eth_chainId"] != 0 {
				t.Fatalf("op-node received eth_chainId; calls: %+v", calls)
			}
		})
	}
}

package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	routerContract "relayer/bindings/ICS26Router"
	spectreContract "relayer/bindings/SpectreClient"
	relayerclient "relayer/client"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	statusTestRouter     = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	statusTestConfigured = common.HexToAddress("0x00000000000000000000000000000000000000bb")
	statusTestReplaced   = common.HexToAddress("0x00000000000000000000000000000000000000cc")
)

// fakeClientStatusRPC answers the eth_calls CosmosClientStatus makes: the
// router's getClient, getClientState on whichever SpectreClient address is
// asked, and getPinnedSnapshot as a pre-#541 client (a revert). frozen maps a SpectreClient address to its isFrozen flag.
func fakeClientStatusRPC(t *testing.T, registered common.Address, frozen map[common.Address]bool) EVMEndpoint {
	t.Helper()
	routerABI, err := routerContract.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	spectreABI, err := spectreContract.ContractSpectreClientMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("unexpected request %s", body)
			return
		}
		// The expiry check reads the head first, so the snapshot can be read at
		// that same block.
		if req.Method == "eth_getBlockByNumber" {
			resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": statusTestHead})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(resp)
			return
		}
		if req.Method != "eth_call" {
			t.Errorf("unexpected request %s", body)
			return
		}
		var call struct {
			To    common.Address `json:"to"`
			Input hexutil.Bytes  `json:"input"`
			Data  hexutil.Bytes  `json:"data"`
		}
		if err := json.Unmarshal(req.Params[0], &call); err != nil {
			t.Errorf("decode call: %v", err)
			return
		}
		input := call.Input
		if len(input) == 0 {
			input = call.Data
		}
		// A pre-#541 SpectreClient has no getPinnedSnapshot and reverts on the
		// unknown selector, so the status falls back to the frozen flag alone.
		if strings.HasPrefix(string(input), string(spectreABI.Methods["getPinnedSnapshot"].ID)) {
			resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": 3, "message": "execution reverted"}})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(resp)
			return
		}
		var out []byte
		switch {
		case call.To == statusTestRouter && strings.HasPrefix(string(input), string(routerABI.Methods["getClient"].ID)):
			out, err = routerABI.Methods["getClient"].Outputs.Pack(registered)
		case strings.HasPrefix(string(input), string(spectreABI.Methods["getClientState"].ID)):
			isFrozen, known := frozen[call.To]
			if !known {
				t.Errorf("getClientState on unexpected address %s", call.To.Hex())
				return
			}
			var state []byte
			state, err = relayerclient.EncodeClientState(relayerclient.ClientState{ChainId: "test", IsFrozen: isFrozen})
			if err == nil {
				out, err = spectreABI.Methods["getClientState"].Outputs.Pack(state)
			}
		default:
			t.Errorf("unexpected eth_call to %s", call.To.Hex())
			return
		}
		if err != nil {
			t.Errorf("pack response: %v", err)
			return
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": hexutil.Encode(out)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return EVMEndpoint{Client: client, Contracts: EVMContracts{Router: statusTestRouter, SpectreClient: statusTestConfigured}}
}

// statusTestHead is the minimal block go-ethereum's header decoder accepts.
var statusTestHead = map[string]any{
	"parentHash":       common.Hash{}.Hex(),
	"sha3Uncles":       "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
	"miner":            common.Address{}.Hex(),
	"stateRoot":        common.Hash{}.Hex(),
	"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
	"receiptsRoot":     "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
	"logsBloom":        "0x" + strings.Repeat("0", 512),
	"difficulty":       "0x0",
	"number":           "0x64",
	"gasLimit":         "0x1c9c380",
	"gasUsed":          "0x0",
	"timestamp":        "0x6500a3c0",
	"extraData":        "0x",
	"mixHash":          common.Hash{}.Hex(),
	"nonce":            "0x0000000000000000",
	"baseFeePerGas":    "0x1",
}

func TestCosmosClientStatus(t *testing.T) {
	ctx := context.Background()

	t.Run("registered client is the configured one", func(t *testing.T) {
		for _, tc := range []struct {
			frozen bool
			want   string
		}{{false, "Active"}, {true, "Frozen"}} {
			evm := fakeClientStatusRPC(t, statusTestConfigured, map[common.Address]bool{statusTestConfigured: tc.frozen})
			got, err := CosmosClientStatus(ctx, evm, "client-0")
			if err != nil || got != tc.want {
				t.Fatalf("frozen=%v: got %q, %v; want %q", tc.frozen, got, err, tc.want)
			}
		}
	})

	// After migrateClient the router routes the id to a replacement. The old,
	// configured contract stays frozen forever; reading it alone reported Frozen
	// forever. The status must name the migration and both addresses instead --
	// and must not read the old contract's freeze flag as the answer.
	t.Run("router migrated the client away from the configured address", func(t *testing.T) {
		evm := fakeClientStatusRPC(t, statusTestReplaced, map[common.Address]bool{
			statusTestConfigured: true,
			statusTestReplaced:   false,
		})
		got, err := CosmosClientStatus(ctx, evm, "client-0")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(got, "Migrated") ||
			!strings.Contains(got, statusTestReplaced.Hex()) || !strings.Contains(got, statusTestConfigured.Hex()) {
			t.Fatalf("want a Migrated status naming both addresses, got %q", got)
		}
	})

	// No router configured: there is nothing to resolve through, so the
	// configured contract is the client.
	t.Run("no router falls back to the configured address", func(t *testing.T) {
		evm := fakeClientStatusRPC(t, statusTestReplaced, map[common.Address]bool{statusTestConfigured: true})
		evm.Contracts.Router = common.Address{}
		got, err := CosmosClientStatus(ctx, evm, "client-0")
		if err != nil || got != "Frozen" {
			t.Fatalf("got %q, %v; want Frozen", got, err)
		}
	})
}

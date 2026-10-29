package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	spectreContract "relayer/bindings/SpectreClient"
	updateClientContract "relayer/bindings/UpdateClient"
	relayerclient "relayer/client"
	"testing"
	"time"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
)

func TestBuildCosmosClientUpdateMsg_RejectsAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := &Worker{}
	_, err := worker.BuildCosmosClientUpdateMsg(ctx, CosmosEndpoint{}, EVMEndpoint{}, time.Second, "2/3", "", 0, "2/3", false, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestWaitForCosmosCatchUp_AbortsOnCancelledContext verifies the catch-up wait
// returns promptly when the context is already cancelled, instead of polling the
// Cosmos client (which is never reached — a zero endpoint would panic if it were).
func TestWaitForCosmosCatchUp_AbortsOnCancelledContext(t *testing.T) {
	w := &Worker{}
	stdCtx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() {
		// A zero endpoint set is fine: the cancelled stdCtx short-circuits
		// before ctx.CosmosClient() is ever called.
		w.WaitForCosmosCatchUp(stdCtx, CosmosEndpoint{}, &relayerclient.EthereumClientState{}, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForCosmosCatchUp did not abort on a cancelled context")
	}
}

// The on-chain height is uint64 and the Tendermint RPC takes int64. The guard is
// the conversion boundary; without it a height above 2^63-1 wraps to a negative
// number, and a negative height reads to the RPC as "latest" rather than as an
// error.
func TestClientStateRevisionHeightInt64(t *testing.T) {
	const maxInt64 = uint64(1)<<63 - 1

	tests := []struct {
		name    string
		height  uint64
		want    int64
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"ordinary height", 12345, 12345, false},
		{"largest representable", maxInt64, int64(maxInt64), false},
		{"one past int64", maxInt64 + 1, 0, true},
		{"max uint64", ^uint64(0), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := clientStateRevisionHeightInt64(relayerclient.ClientState{
				LatestHeight: updateClientContract.IICS02ClientMsgsHeight{RevisionHeight: tt.height},
			})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("height %d was accepted; it does not fit in an int64", tt.height)
				}
				return
			}
			if err != nil {
				t.Fatalf("height %d rejected: %v", tt.height, err)
			}
			if got != tt.want {
				t.Fatalf("height = %d, want %d", got, tt.want)
			}
		})
	}
}

// fakeCaughtUpRPCs serves a source chain whose latest height equals the
// SpectreClient's on-chain latest height — the state a packet update leaves
// behind when it lands just before the periodic rotation fires.
func fakeCaughtUpRPCs(t *testing.T, height int64) (CosmosEndpoint, EVMEndpoint) {
	t.Helper()
	cosmosServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "status" {
			t.Errorf("unexpected cosmos request method=%q err=%v", req.Method, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"sync_info":{"latest_block_height":"%d"}}}`, req.ID, height)
	}))
	t.Cleanup(cosmosServer.Close)
	cosmosClient, err := rpchttp.New(cosmosServer.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}

	spectreABI, err := spectreContract.ContractSpectreClientMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	evmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "eth_call" {
			t.Errorf("unexpected evm request method=%q err=%v", req.Method, err)
			return
		}
		state, err := relayerclient.EncodeClientState(relayerclient.ClientState{
			ChainId:      "test",
			LatestHeight: updateClientContract.IICS02ClientMsgsHeight{RevisionHeight: uint64(height)},
		})
		if err != nil {
			t.Errorf("encode client state: %v", err)
			return
		}
		out, err := spectreABI.Methods["getClientState"].Outputs.Pack(state)
		if err != nil {
			t.Errorf("pack client state: %v", err)
			return
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": hexutil.Encode(out)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(evmServer.Close)
	ethClient, err := ethclient.Dial(evmServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ethClient.Close)
	return CosmosEndpoint{Client: cosmosClient}, EVMEndpoint{Client: ethClient}
}

// A packet update that reaches the latest height just before the periodic
// rotation must not turn the rotation into a silent success: that would wait a
// full rotation interval with the pinned snapshot unrefreshed, and it can expire
// first (#541). The forced refresh must fail retryably instead. The Worker has
// no TxHandler, so reaching the send path would panic.
func TestRefreshCosmosClient_ForcedRotationAtLatestHeightIsRetryable(t *testing.T) {
	cosmos, evm := fakeCaughtUpRPCs(t, 100)
	w := &Worker{}

	block, err := w.RefreshCosmosClient(context.Background(), cosmos, evm, "client-0", time.Second, "2/3", "", "2/3")
	if !errors.Is(err, ErrForcedRotationNotReady) {
		t.Fatalf("error = %v, want ErrForcedRotationNotReady", err)
	}
	if errors.Is(err, ErrPermanentRelayFailure) || errors.Is(err, ErrClientNotActive) {
		t.Fatalf("error = %v must stay transient, not permanent or a client hold", err)
	}
	if block != nil {
		t.Fatalf("light block = %+v, want nil so the caller does not record a fresh update", block)
	}
}

func TestForcedRotationBlockedAtHeight(t *testing.T) {
	if !forcedRotationBlockedAtHeight(true, 100, 100) {
		t.Fatal("a forced rotation at the trusted height must be reported as blocked")
	}
	if forcedRotationBlockedAtHeight(false, 100, 100) {
		t.Fatal("an unforced update at the trusted height is simply up to date")
	}
	if forcedRotationBlockedAtHeight(true, 100, 101) {
		t.Fatal("a forced rotation with a newer target can proceed")
	}
}

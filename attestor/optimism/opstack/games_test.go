package opstack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// The Base Sepolia fixture data below is real, live-captured on-chain state —
// not synthetic — ported from PR #241 (hieu/base), which discovered the
// FaultDisputeGame interface rename by calling a real game with `cast`:
//
//	factory:          0xd6E6dBf4F7EA0ac412fD8b65ED297e64BB7a06E1 (Base Sepolia DisputeGameFactoryProxy)
//	game index:       22032
//	game proxy:       0x4185E517C8D3252B4BD3aCa0bb9dA01F54ef61fd
//	gameType:         621
//	rootClaim:        0x537ef77634016d186211e650a6d4ddc3dfd4fbee8587b9dc53a1bf8ac1cfc0ce
//	l2SequenceNumber: 44459063 (0x2a66437); l2BlockNumber() reverts with no data
const (
	fixtureFactoryAddr = "0xd6E6dBf4F7EA0ac412fD8b65ED297e64BB7a06E1"
	fixtureGameProxy   = "0x4185E517C8D3252B4BD3aCa0bb9dA01F54ef61fd"

	// selector 0xbb8aa1fc = gameAtIndex(uint256), called with index 22032:
	// gameType_ = 621 (0x26d), timestamp_ = 0x6a602cfc, proxy_ = the game proxy.
	fixtureGameAtIndexResult = "0x000000000000000000000000000000000000000000000000000000000000026d" +
		"000000000000000000000000000000000000000000000000000000006a602cfc" +
		"0000000000000000000000004185e517c8d3252b4bd3aca0bb9da01f54ef61fd"
	// selector 0xbcef3b55 = rootClaim()
	fixtureRootClaimResult = "0x537ef77634016d186211e650a6d4ddc3dfd4fbee8587b9dc53a1bf8ac1cfc0ce"
	// selector 0x99735e32 = l2SequenceNumber()
	fixtureL2SeqNumberResult = "0x0000000000000000000000000000000000000000000000000000000002a66437"

	fixtureGameType      = uint32(621)
	fixtureL1Timestamp   = uint64(0x6a602cfc)
	fixtureL2SeqNumber   = uint64(44459063)
	fixtureGameIndex     = uint64(22032)
	syntheticBlockResult = "0x0000000000000000000000000000000000000000000000000000000002a66437"
)

const (
	selGameAtIndex      = "bb8aa1fc"
	selRootClaim        = "bcef3b55"
	selL2SequenceNumber = "99735e32"
	selL2BlockNumber    = "8b85902b"
)

// newSelectorRPC returns an httptest server answering eth_call by matching
// the calldata's 4-byte selector: selectors in results answer with the given
// bytes, selectors in reverts answer a data-less "execution reverted" error
// (what a live Base Sepolia game returns for l2BlockNumber()). eth_getCode
// answers non-empty so NewFactoryGameSource's code check passes. No live
// network access, fully deterministic.
//
// t.Fatalf must never be called from the handler: httptest serves each
// request on its own goroutine, and calling it there aborts that goroutine
// without failing the test properly (the client just sees a dropped
// connection). Log with t.Errorf (goroutine-safe) and return a JSON-RPC
// error instead. (Harness pattern ported from PR #241.)
func newSelectorRPC(t *testing.T, results map[string]string, reverts map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string            `json:"jsonrpc"`
			ID      json.RawMessage   `json:"id"`
			Method  string            `json:"method"`
			Params  []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("mock rpc: decode request: %v", err)
			return
		}

		writeResult := func(result string) {
			resp := struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  string          `json:"result"`
			}{JSONRPC: "2.0", ID: req.ID, Result: result}
			_ = json.NewEncoder(w).Encode(resp)
		}
		writeError := func(code int, msg string) {
			resp := struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Error   struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}{JSONRPC: "2.0", ID: req.ID}
			resp.Error.Code = code
			resp.Error.Message = msg
			_ = json.NewEncoder(w).Encode(resp)
		}

		switch req.Method {
		case "eth_getCode":
			writeResult("0x6080")
		case "eth_call":
			// go-ethereum's ethclient sends the calldata as "input"
			// (the modern field name); accept "data" too for safety.
			var call struct {
				Input string `json:"input"`
				Data  string `json:"data"`
			}
			if len(req.Params) == 0 || json.Unmarshal(req.Params[0], &call) != nil {
				t.Errorf("mock rpc: malformed eth_call params")
				writeError(-32602, "malformed eth_call params")
				return
			}
			calldata := call.Input
			if calldata == "" {
				calldata = call.Data
			}
			if len(calldata) < 10 {
				t.Errorf("mock rpc: eth_call without calldata")
				writeError(-32602, "eth_call without calldata")
				return
			}
			selector := strings.ToLower(calldata[2:10])
			if reverts[selector] {
				writeError(3, "execution reverted")
				return
			}
			result, ok := results[selector]
			if !ok {
				t.Errorf("mock rpc: unexpected eth_call selector %s", selector)
				writeError(-32601, "unexpected selector "+selector)
				return
			}
			writeResult(result)
		default:
			t.Errorf("mock rpc: unexpected method %s", req.Method)
			writeError(-32601, "unexpected method "+req.Method)
		}
	}))
}

func newFixtureGameSource(t *testing.T, results map[string]string, reverts map[string]bool) *FactoryGameSource {
	t.Helper()
	server := newSelectorRPC(t, results, reverts)
	t.Cleanup(server.Close)
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatalf("dial mock rpc: %v", err)
	}
	t.Cleanup(client.Close)
	games, err := NewFactoryGameSource(context.Background(), client, common.HexToAddress(fixtureFactoryAddr))
	if err != nil {
		t.Fatalf("NewFactoryGameSource: %v", err)
	}
	return games
}

// TestGameAtIndex_SuperRootsInterface uses the live-captured Base Sepolia
// fixture: the game answers l2SequenceNumber() and reverts on
// l2BlockNumber(), exactly like the real game proxy at index 22032.
func TestGameAtIndex_SuperRootsInterface(t *testing.T) {
	games := newFixtureGameSource(t,
		map[string]string{
			selGameAtIndex:      fixtureGameAtIndexResult,
			selRootClaim:        fixtureRootClaimResult,
			selL2SequenceNumber: fixtureL2SeqNumberResult,
		},
		map[string]bool{selL2BlockNumber: true},
	)

	got, err := games.GameAtIndex(context.Background(), fixtureGameIndex)
	if err != nil {
		t.Fatalf("GameAtIndex: %v", err)
	}
	if got.GameIndex != fixtureGameIndex || got.GameType != fixtureGameType || got.L1Timestamp != fixtureL1Timestamp {
		t.Errorf("game entry = index %d type %d ts %d, want index %d type %d ts %d",
			got.GameIndex, got.GameType, got.L1Timestamp, fixtureGameIndex, fixtureGameType, fixtureL1Timestamp)
	}
	if got.GameAddress != common.HexToAddress(fixtureGameProxy) {
		t.Errorf("GameAddress = %s, want %s", got.GameAddress, fixtureGameProxy)
	}
	if got.RootClaim != [32]byte(common.HexToHash(fixtureRootClaimResult)) {
		t.Errorf("RootClaim = %x, want %s", got.RootClaim, fixtureRootClaimResult)
	}
	if got.L2BlockNumber != fixtureL2SeqNumber {
		t.Errorf("L2BlockNumber = %d, want %d (via l2SequenceNumber)", got.L2BlockNumber, fixtureL2SeqNumber)
	}
}

// TestGameAtIndex_LegacyInterfaceFallback is a SYNTHETIC case (no live
// capture): a pre-rename game answers l2BlockNumber() and reverts on
// l2SequenceNumber() — the op-contracts/v1.8.0 shape OP Mainnet's type-8
// games still expose.
func TestGameAtIndex_LegacyInterfaceFallback(t *testing.T) {
	games := newFixtureGameSource(t,
		map[string]string{
			selGameAtIndex:   fixtureGameAtIndexResult,
			selRootClaim:     fixtureRootClaimResult,
			selL2BlockNumber: syntheticBlockResult,
		},
		map[string]bool{selL2SequenceNumber: true},
	)

	got, err := games.GameAtIndex(context.Background(), fixtureGameIndex)
	if err != nil {
		t.Fatalf("GameAtIndex: %v", err)
	}
	if got.L2BlockNumber != fixtureL2SeqNumber {
		t.Errorf("L2BlockNumber = %d, want %d (via legacy l2BlockNumber fallback)", got.L2BlockNumber, fixtureL2SeqNumber)
	}
}

// TestGameAtIndex_NeitherHeightSelector: both height selectors revert — the
// error must surface both attempts, never a silent zero height.
func TestGameAtIndex_NeitherHeightSelector(t *testing.T) {
	games := newFixtureGameSource(t,
		map[string]string{
			selGameAtIndex: fixtureGameAtIndexResult,
			selRootClaim:   fixtureRootClaimResult,
		},
		map[string]bool{selL2SequenceNumber: true, selL2BlockNumber: true},
	)

	_, err := games.GameAtIndex(context.Background(), fixtureGameIndex)
	if err == nil {
		t.Fatal("GameAtIndex should fail when neither height selector is available")
	}
	if !strings.Contains(err.Error(), "l2SequenceNumber") || !strings.Contains(err.Error(), "l2BlockNumber") {
		t.Errorf("error should mention both selectors, got: %v", err)
	}
}

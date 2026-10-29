package opstack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// newFakeOpNodeWithCommitment serves optimism_outputAtBlock with the full reply
// shape a real op-node sends — blockRef and stateRoot alongside outputRoot.
// blockNumberOverride, when non-zero, makes the node answer about a different
// block than the one requested.
func newFakeOpNodeWithCommitment(t *testing.T, blockHash, stateRoot common.Hash, blockNumberOverride uint64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad rpc request: %v", err)
			return
		}
		if req.Method != "optimism_outputAtBlock" {
			t.Errorf("unexpected rpc method %q", req.Method)
			return
		}
		var blockHex string
		if err := json.Unmarshal(req.Params[0], &blockHex); err != nil {
			t.Errorf("bad block param: %v", err)
			return
		}
		number, err := strconv.ParseUint(strings.TrimPrefix(blockHex, "0x"), 16, 64)
		if err != nil {
			t.Errorf("parse block param %q: %v", blockHex, err)
			return
		}
		if blockNumberOverride != 0 {
			number = blockNumberOverride
		}
		result := map[string]any{
			"outputRoot": "0x" + strings.Repeat("cd", 32),
			"blockRef":   map[string]any{"number": number, "hash": blockHash.Hex()},
			"stateRoot":  stateRoot.Hex(),
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("failed to encode rpc response: %v", err)
		}
	}))
}

// The identity the relayer's binding check compares against has to survive the
// round trip: state root and block hash come from the same reply as the output
// root, so a parse that drops them would silently answer about nothing.
func TestCommitmentAtReadsBlockIdentity(t *testing.T) {
	wantHash := common.HexToHash("0xaa11")
	wantState := common.HexToHash("0xbb22")
	srv := newFakeOpNodeWithCommitment(t, wantHash, wantState, 0)
	defer srv.Close()

	client, err := DialReplica(context.Background(), srv.URL, testRPCTimeout)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer client.Close()

	got, err := client.CommitmentAt(context.Background(), 4096)
	if err != nil {
		t.Fatalf("CommitmentAt: %v", err)
	}
	if got.BlockNumber != 4096 {
		t.Errorf("block number = %d, want 4096", got.BlockNumber)
	}
	if got.BlockHash != wantHash {
		t.Errorf("block hash = %s, want %s", got.BlockHash, wantHash)
	}
	if got.StateRoot != wantState {
		t.Errorf("state root = %s, want %s", got.StateRoot, wantState)
	}
}

// A reply about a different block must not be compared as if it were about the
// requested one — that would report a divergence for a perfectly good header.
func TestCommitmentAtRejectsAnAnswerAboutAnotherBlock(t *testing.T) {
	srv := newFakeOpNodeWithCommitment(t, common.HexToHash("0xaa"), common.HexToHash("0xbb"), 999)
	defer srv.Close()

	client, err := DialReplica(context.Background(), srv.URL, testRPCTimeout)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer client.Close()

	if _, err := client.CommitmentAt(context.Background(), 4096); err == nil {
		t.Fatal("CommitmentAt accepted a reply about block 999, want an error")
	}
}

// OutputAtBlock feeds the attestation loop and must stay tolerant of a reply
// without blockRef, which is what the pre-existing fixtures send.
func TestOutputAtBlockDoesNotRequireBlockRef(t *testing.T) {
	srv := newFakeOpNode(t, 1, 2, "0x"+strings.Repeat("ab", 32))
	defer srv.Close()

	client, err := DialReplica(context.Background(), srv.URL, testRPCTimeout)
	if err != nil {
		t.Fatalf("DialReplica: %v", err)
	}
	defer client.Close()

	if _, err := client.OutputAtBlock(context.Background(), 100); err != nil {
		t.Fatalf("OutputAtBlock: %v", err)
	}
}

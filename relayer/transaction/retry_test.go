package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"relayer/chain"
	"relayer/services"
	"strings"
	"testing"

	errortypes "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
)

func revertProbeClient(t *testing.T, blockGas uint64, callError string, onCall func(uint64)) *ethclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any              `json:"id"`
			Method string           `json:"method"`
			Params []map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "eth_getBlockByNumber":
			resp["result"] = map[string]any{
				"parentHash":       "0x0000000000000000000000000000000000000000000000000000000000000000",
				"sha3Uncles":       "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
				"miner":            "0x0000000000000000000000000000000000000000",
				"stateRoot":        "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"receiptsRoot":     "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"logsBloom":        "0x" + strings.Repeat("0", 512),
				"difficulty":       "0x0", "number": "0x1", "gasLimit": fmt.Sprintf("0x%x", blockGas),
				"gasUsed": "0x0", "timestamp": "0x0", "extraData": "0x",
				"mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"nonce":   "0x0000000000000000",
			}
		case "eth_call":
			if onCall != nil && len(req.Params) > 0 {
				if raw, ok := req.Params[0]["gas"].(string); ok {
					gas, err := hexutil.DecodeUint64(raw)
					if err == nil {
						onCall(gas)
					}
				}
			}
			if callError == "" {
				resp["result"] = "0x"
			} else {
				resp["error"] = map[string]any{"code": -32000, "message": callError}
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatalf("dial mock RPC: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestClassifyEthRevert_DoesNotTrustRejectedProbe(t *testing.T) {
	const gasLimit = uint64(16_000_000)
	client := revertProbeClient(t, 30_000_000, "gas required exceeds allowance", nil)
	endpoint := services.EVMEndpoint{Client: client}
	got := classifyEthRevert(context.Background(), endpoint, ethereum.CallMsg{}, big.NewInt(1), nil, gasLimit/4, gasLimit)
	if got != revertLogical {
		t.Fatalf("rejected diagnostic probe classified as %v, want logical", got)
	}
}

func TestClassifyEthRevert_ClampsProbeToBlockGas(t *testing.T) {
	const (
		gasLimit = uint64(16_000_000)
		blockGas = uint64(30_000_000)
	)
	var observed uint64
	client := revertProbeClient(t, blockGas, "", func(gas uint64) { observed = gas })
	endpoint := services.EVMEndpoint{Client: client}
	if got := classifyEthRevert(context.Background(), endpoint, ethereum.CallMsg{}, big.NewInt(1), nil, gasLimit/4, gasLimit); got != revertOutOfGas {
		t.Fatalf("successful higher-gas replay = %v, want out of gas", got)
	}
	if observed != blockGas {
		t.Fatalf("probe gas = %d, want clamped block gas %d", observed, blockGas)
	}
}

// Classifying an INCLUDED Cosmos out-of-gas as transient is only safe if the
// retry differs from the attempt that failed. It does not by itself: a transient
// failure charges no retry budget (chargesRetryBudget requires permanent), is
// never marked Solo, and is re-queued at the HEAD of the queue. So a repeatable
// simulation under-estimate re-broadcasts the identical tx forever — burning a
// sequence and fees each round and blocking every packet behind it.
//
// planCosmosOutOfGas is what makes each retry a different attempt. These tests
// pin that every path shrinks something and that the shrinking bottoms out.

// oogPlan drives the planner the way the two send paths do: build the ladder,
// then ask chain.Climb for the next rung. Reading the result through Climb
// rather than through the ladder directly is deliberate — Climb is where the
// bottom becomes permanent, so a test that skipped it would not be testing the
// path the relayer takes.
func oogPlan(t *testing.T, msgCount, headroomStep int, base, maxBlockGas uint64, allowSplit bool) (chain.Attempt, chain.Retry, bool) {
	t.Helper()
	cause := &services.CosmosTxFailure{Stage: "DeliverTx", Err: services.ErrPermanentRelayFailure}
	plan, at := planCosmosOutOfGas(cause, msgCount, headroomStep, base, maxBlockGas, allowSplit)
	return chain.Climb(plan, at)
}

func TestPlanCosmosOutOfGas(t *testing.T) {
	t.Run("splits a batch before touching headroom", func(t *testing.T) {
		// Splitting is preferred while it is available: the halves are simulated
		// separately, which fixes the cause (one estimate covering too much work)
		// rather than papering over it with a bigger multiplier.
		for _, msgCount := range []int{2, 3, 16} {
			next, _, ok := oogPlan(t, msgCount, 0, 1_000_000, 30_000_000, true)
			if !ok || next.What != knobBatchSize {
				t.Fatalf("msgCount=%d planned %v (ok=%v), want a batch split", msgCount, next, ok)
			}
			if next.To >= uint64(msgCount) {
				t.Fatalf("msgCount=%d planned %d messages; a split that does not shrink is the same attempt", msgCount, next.To)
			}
		}
		// Even at the last headroom step, a multi-message batch still splits. The
		// gas counter is not the batch counter, and reading it as one would exhaust a
		// ladder that still has messages to halve.
		last := len(cosmosGasHeadroom) - 1
		if next, _, ok := oogPlan(t, 4, last, 1_000_000, 30_000_000, true); !ok || next.What != knobBatchSize {
			t.Fatalf("multi-message at the last headroom step planned %v (ok=%v), want a batch split", next, ok)
		}
	})

	t.Run("escalates a single message", func(t *testing.T) {
		var seen []uint64
		for step := 0; step < len(cosmosGasHeadroom)-1; step++ {
			next, _, ok := oogPlan(t, 1, step, 1_000_000, 30_000_000, true)
			if !ok || next.What != knobCosmosGas {
				t.Fatalf("1 msg at step=%d planned %v (ok=%v), want a gas escalation", step, next, ok)
			}
			seen = append(seen, next.To)
		}
		// Each rung must be a different number, or "escalating" re-sends the tx that
		// just failed.
		for i := 1; i < len(seen); i++ {
			if seen[i] <= seen[i-1] {
				t.Fatalf("gas rungs %v do not increase; the retry would repeat the failed attempt", seen)
			}
		}
	})

	// The two floors. Without them "transient" is unbounded, which is the defect.
	t.Run("bottoms out as permanent", func(t *testing.T) {
		last := len(cosmosGasHeadroom) - 1
		next, bottom, ok := oogPlan(t, 1, last, 1_000_000, 30_000_000, true)
		if ok {
			t.Fatalf("single message at the last headroom step planned %v; the ladder must end, or the retry loop never does", next)
		}
		// The bottom must stay usable as the error it came from. A classification
		// that swallowed the cause would make every existing check downstream —
		// errors.Is on the relay failure, errors.As on the tx failure — start
		// answering no.
		if !errors.Is(bottom, services.ErrPermanentRelayFailure) {
			t.Fatal("the bottom no longer reads as a permanent relay failure; relay/batch.go would re-queue a dead packet forever")
		}
		if !chain.IsPermanent(bottom) {
			t.Fatal("the bottom is not tagged permanent for the engine")
		}
		var txFail *services.CosmosTxFailure
		if !errors.As(bottom, &txFail) || txFail.Stage != "DeliverTx" {
			t.Fatalf("the originating tx failure is no longer reachable through the bottom: %v", bottom)
		}

		// At the block ceiling a larger factor buys nothing: the next attempt would be
		// the same transaction. That is permanent however many steps are left.
		if next, _, ok := oogPlan(t, 1, 0, 30_000_000, 30_000_000, true); ok {
			t.Fatalf("single message already at the block gas limit planned %v, want permanent", next)
		}
		if next, _, ok := oogPlan(t, 1, 0, 31_000_000, 30_000_000, true); ok {
			t.Fatalf("single message above the block gas limit planned %v, want permanent", next)
		}
	})

	// A chain that does not report a block max (MaxGas unset) must not be read as
	// "the ceiling is 0, everything is permanent" — that would dead-letter valid
	// packets on the first out-of-gas.
	t.Run("with no block limit still escalates", func(t *testing.T) {
		next, _, ok := oogPlan(t, 1, 0, 1_000_000, 0, true)
		if !ok || next.What != knobCosmosGas {
			t.Fatalf("no block max reported planned %v (ok=%v), want a gas escalation", next, ok)
		}
	})

	// An atomic batch keeps its all-or-nothing shape: it must never be split, only
	// given more gas, however many messages it carries.
	t.Run("terminates and preserves atomicity", func(t *testing.T) {
		if next, _, ok := oogPlan(t, 2, 0, 1, 10, true); !ok || next.What != knobBatchSize {
			t.Fatalf("splittable batch planned %v (ok=%v), want a split", next, ok)
		}
		if next, _, ok := oogPlan(t, 2, 0, 1, 10, false); !ok || next.What != knobCosmosGas {
			t.Fatalf("atomic batch planned %v (ok=%v), want a gas escalation and never a split", next, ok)
		}
		if next, _, ok := oogPlan(t, 1, len(cosmosGasHeadroom)-1, 10, 10, false); ok {
			t.Fatalf("at the ceiling planned %v, want permanent", next)
		}
	})
}

// A batch that is just under the block limit is still worth one attempt AT the
// limit: that is more gas than the attempt that failed, so it is a real change.
// Cutting the ladder off instead would dead-letter a packet one rung early.
func TestCosmosGasLadder_ClampsToTheBlockLimitBeforeStopping(t *testing.T) {
	const blockMax = uint64(30_000_000)
	base := uint64(float64(blockMax)/cosmosGasHeadroom[1]) + 1 // step 1 lands just over the limit
	next, ok := CosmosGasLadder(base, blockMax)(0)
	if !ok {
		t.Fatal("a rung that only needs clamping was reported as the bottom; the packet dies one attempt early")
	}
	if next.To != blockMax {
		t.Fatalf("clamped rung = %d, want the block limit %d", next.To, blockMax)
	}
	// And the rung after it is genuinely the bottom: clamped to the same number,
	// so retrying would re-send the identical transaction.
	if next, ok := CosmosGasLadder(base, blockMax)(1); ok {
		t.Fatalf("a second clamped rung %v is the same transaction again", next)
	}
}

// C2's shape, applied to each ladder on its own: three calls, three different
// answers, and an end. A ladder is only a ladder if walking it changes something
// every time and stops.
func TestLaddersChangeSomethingEachRungAndEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ladder chain.ChangeFunc
	}{
		{"cosmos gas", CosmosGasLadder(1_000_000, 0)},
		{"evm gas", EVMGasLadder(1_000_000)},
		{"cosmos batch", CosmosBatchLadder(32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[uint64]bool{}
			ended := false
			for attempt := 0; attempt < 16; attempt++ {
				next, ok := tc.ladder(attempt)
				if !ok {
					ended = true
					break
				}
				if next.What == "" {
					t.Fatalf("rung %d has no knob name; an operator reading the log cannot tell what changed", attempt)
				}
				if seen[next.To] {
					t.Fatalf("rung %d repeats the value %d; that retry is the attempt that just failed", attempt, next.To)
				}
				seen[next.To] = true
			}
			if !ended {
				t.Fatal("the ladder never reported a bottom; the retry loop would not terminate")
			}
			if len(seen) < 2 {
				t.Fatalf("only %d distinct rungs; a one-rung ladder cannot make a second retry differ", len(seen))
			}
		})
	}
}

// The batch ladder stops at one message rather than zero: a single message that
// still runs out of gas is not a batching problem, and planCosmosOutOfGas hands
// it to the gas ladder instead.
func TestCosmosBatchLadder_StopsAtOneMessage(t *testing.T) {
	if next, ok := CosmosBatchLadder(1)(0); ok {
		t.Fatalf("a single message planned a split to %v; there is nothing to split", next)
	}
	for attempt := 0; attempt < 16; attempt++ {
		next, ok := CosmosBatchLadder(8)(attempt)
		if !ok {
			break
		}
		if next.To < 1 {
			t.Fatalf("rung %d planned a batch of %d messages", attempt, next.To)
		}
	}
}

// Drive the planner the way the send path does and show the sequence of actions
// terminates from every starting point. This is the property the whole change
// exists for; the individual cases above only pin its pieces.
func TestCosmosOutOfGas_RetriesTerminate(t *testing.T) {
	const blockMax = uint64(30_000_000)
	for _, msgCount := range []int{1, 2, 5, 32} {
		msgs, step := msgCount, 0
		// Generous bound: any run longer than this is a loop, not a ladder.
		const limit = 64
		for i := 0; ; i++ {
			if i > limit {
				t.Fatalf("no termination from msgCount=%d after %d steps", msgCount, limit)
			}
			next, _, ok := oogPlan(t, msgs, step, 1_000_000, blockMax, true)
			if !ok {
				if msgs != 1 {
					t.Fatalf("terminated as permanent while %d messages could still be split", msgs)
				}
				break
			}
			switch next.What {
			case knobBatchSize:
				// Worst case for termination: the failing half is the larger one.
				msgs -= msgs / 2
				step = 0 // halves are simulated afresh
			default:
				step++
			}
		}
	}
}

// The ladder must actually climb. A flat or descending entry would make an
// "escalation" re-send the same transaction, restoring the loop while looking
// like it had been fixed.
func TestCosmosGasHeadroom_IsStrictlyIncreasing(t *testing.T) {
	if len(cosmosGasHeadroom) < 2 {
		t.Fatal("a single-entry ladder cannot escalate; the retry would repeat the failed attempt")
	}
	if cosmosGasHeadroom[0] != 1.3 {
		t.Fatalf("first step = %v, want the historical 1.3 so unaffected batches are unchanged",
			cosmosGasHeadroom[0])
	}
	for i := 1; i < len(cosmosGasHeadroom); i++ {
		if cosmosGasHeadroom[i] <= cosmosGasHeadroom[i-1] {
			t.Fatalf("step %d (%v) does not exceed step %d (%v)",
				i, cosmosGasHeadroom[i], i-1, cosmosGasHeadroom[i-1])
		}
	}
}

// The predicate comes from the SDK's registered error rather than the literals
// "sdk" and 11, so it tracks whatever the chain actually sends.
func TestCosmosOutOfGas_MatchesTheSDKError(t *testing.T) {
	cs, code := errortypes.ErrOutOfGas.Codespace(), errortypes.ErrOutOfGas.ABCICode()
	if !cosmosOutOfGas(cs, code) {
		t.Fatalf("cosmosOutOfGas(%q, %d) = false, want true for the SDK's own ErrOutOfGas", cs, code)
	}
	if cosmosOutOfGas("wasm", code) {
		t.Fatal("right code in another codespace must not be read as out of gas")
	}
	if cosmosOutOfGas(cs, errortypes.ErrInsufficientFee.ABCICode()) {
		t.Fatal("another sdk code must not be read as out of gas")
	}
	if cosmosOutOfGas("", 0) {
		t.Fatal("a success must not be read as out of gas")
	}
}

// The ladder must be anchored on the gas BEFORE applyCosmosGasHeadroom, because
// the retry it describes recomputes its own limit from headroomStep+1. Anchoring
// on the scaled finalGasLimit applied the factor twice, so the rung the ladder
// named was not the gas the retry would use -- and, worse, once finalGasLimit had
// been clamped to maxBlockGas every rung flattened onto the clamp and the ladder
// reported itself exhausted with real headroom steps still to go.
func TestCosmosGasLadder_AnchorsOnUnscaledGas(t *testing.T) {
	const (
		simulated = uint64(1_000_000)
		blockMax  = uint64(10_000_000)
	)

	t.Run("each rung is the gas the next attempt actually computes", func(t *testing.T) {
		for step := 0; step+1 < len(cosmosGasHeadroom); step++ {
			next, ok := CosmosGasLadder(simulated, blockMax)(step)
			if !ok {
				t.Fatalf("step %d: ladder exhausted with headroom left", step)
			}
			// What sendCosmosTxBatchAtHeadroom will compute when it retries.
			want := applyCosmosGasHeadroom(simulated, step+1)
			if next.To != want {
				t.Fatalf("step %d: ladder says %d, the retry will use %d; the number in the log is a different one from the number on the wire",
					step, next.To, want)
			}
		}
	})

	// The reported regression, with the arithmetic that shows it. Simulated 1M
	// against a 1.5M block limit: production sets finalGasLimit = min(1M*1.3,
	// 1.5M) = 1.3M. Anchored THERE, rung 1 is min(1.3M*2.0, 1.5M) = 1.5M and rung
	// 0 is min(1.3M*1.3, 1.5M) = 1.5M -- equal, so the ladder stops and the
	// failure is reported permanent. Anchored on the unscaled 1M, rung 0 is 1.3M
	// and rung 1 is 1.5M: the attempt at exactly the block ceiling is still
	// available, and it is worth making because it is more gas than the one that
	// just failed.
	t.Run("the block-ceiling attempt survives", func(t *testing.T) {
		const tightMax = uint64(1_500_000)
		scaled := applyCosmosGasHeadroom(simulated, 0) // what finalGasLimit holds
		if _, ok := CosmosGasLadder(scaled, tightMax)(0); ok {
			t.Fatal("the fixture no longer reproduces the bug: anchored on the scaled limit the ladder should stop here")
		}
		next, ok := CosmosGasLadder(simulated, tightMax)(0)
		if !ok {
			t.Fatal("anchored on the unscaled gas the block-ceiling attempt must still be offered")
		}
		if next.To != tightMax {
			t.Fatalf("rung = %d, want the block ceiling %d", next.To, tightMax)
		}
	})

	// And the clamp still ends the ladder when the unscaled base genuinely reaches
	// it -- this fix must not turn the terminating condition off.
	t.Run("the clamp still terminates the ladder", func(t *testing.T) {
		attempts := 0
		ladder := CosmosGasLadder(blockMax/2, blockMax)
		for step := 0; step < 10; step++ {
			if _, ok := ladder(step); !ok {
				break
			}
			attempts++
		}
		if attempts == 0 || attempts >= 10 {
			t.Fatalf("ladder yielded %d rungs; it must climb and then stop", attempts)
		}
	})
}

func TestProbeErrorClassesAreDisjoint(t *testing.T) {
	for _, text := range []string{"out of gas", "gas required exceeds allowance", "exceeds block gas limit", "intrinsic gas too low", "dial tcp: timeout"} {
		err := errors.New(text)
		count := 0
		for _, class := range []bool{isExecutionOutOfGas(err), isGasAllowanceRejection(err), isProbeTransportError(err)} {
			if class {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%q matched %d classes, want exactly one", text, count)
		}
	}
}

func TestApplyCosmosGasHeadroom_FallbackCosmosGasLimitIncreasesAcrossOOGRetries(t *testing.T) {
	const fallback = uint64(200_000)
	previous := uint64(0)
	for step := range cosmosGasHeadroom {
		got := applyCosmosGasHeadroom(fallback, step)
		if got <= previous {
			t.Fatalf("fallback gas at step %d = %d, want greater than previous %d", step, got, previous)
		}
		previous = got
	}
}

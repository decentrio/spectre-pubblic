package transaction

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"relayer/services"
	"strings"
	"testing"
	"time"

	errorsmod "cosmossdk.io/errors"
	abci "github.com/cometbft/cometbft/abci/types"
	cmtbytes "github.com/cometbft/cometbft/libs/bytes"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkbech32 "github.com/cosmos/cosmos-sdk/types/bech32"
	errortypes "github.com/cosmos/cosmos-sdk/types/errors"
	txservice "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"
	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
)

func TestCloneCosmosSDKMsgWithSigner_DoesNotMutateOriginal(t *testing.T) {
	original := &clienttypes.MsgUpdateClient{ClientId: "08-wasm-0"}

	cloned, err := cloneCosmosSDKMsgWithSigner(original, "cosmos1signer", -1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cloned.(*clienttypes.MsgUpdateClient)
	if got.Signer != "cosmos1signer" {
		t.Fatalf("cloned signer = %q, want cosmos1signer", got.Signer)
	}
	if original.Signer != "" {
		t.Fatalf("original signer mutated to %q", original.Signer)
	}
}

func TestCloneCosmosSDKMsgWithSigner_PreservesExistingSigner(t *testing.T) {
	original := &clienttypes.MsgUpdateClient{ClientId: "08-wasm-0", Signer: "cosmos1original"}

	cloned, err := cloneCosmosSDKMsgWithSigner(original, "cosmos1default", -1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cloned.(*clienttypes.MsgUpdateClient)
	if got.Signer != "cosmos1original" {
		t.Fatalf("cloned signer = %q, want existing signer", got.Signer)
	}
	if original.Signer != "cosmos1original" {
		t.Fatalf("original signer mutated to %q", original.Signer)
	}
}

// TestWaitForTxResult_CancelledCtxAbortsPromptly is the regression for the #252 P2
// finding: the poll loop must abort on shutdown instead of sleeping past
// cancellation until the local timeout — which, held under cosmosMu in
// SendCosmosTxBatch, would stall shutdown for the whole inclusion window. A
// pre-cancelled ctx returns on the first iteration before any CosmosClient poll, so
// no live client is needed.
func TestWaitForTxResult_CancelledCtxAbortsPromptly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := &Handler{}
	start := time.Now()
	_, err := h.waitForTxResult(ctx, services.CosmosEndpoint{}, []byte{0x01}, cosmosInclusionTimeout)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want wrapped context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waitForTxResult took %s; must abort promptly on cancel, not run to the inclusion timeout", elapsed)
	}
}

type cosmosOOGRPC struct {
	t *testing.T

	checkTxOOG   int
	deliverTxOOG int
	// simulation, when set, is the Simulate query's response; nil keeps the
	// generic "simulation unavailable" failure the OOG tests rely on.
	simulation *abci.ResponseQuery
	broadcasts int
	txQueries  int
	gasLimits  []uint64
	feeAmounts []int64
}

type cosmosRPCRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type cosmosRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

func (m *cosmosOOGRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.t.Helper()
	var req cosmosRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		m.t.Errorf("decode RPC request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch req.Method {
	case "consensus_params":
		m.writeError(w, req.ID, -32603, "block max unavailable")
	case "abci_query":
		if m.simulation != nil {
			m.writeResult(w, req.ID, &coretypes.ResultABCIQuery{Response: *m.simulation})
			return
		}
		m.writeResult(w, req.ID, &coretypes.ResultABCIQuery{
			Response: abci.ResponseQuery{Code: 1, Codespace: "sdk", Log: "simulation unavailable"},
		})
	case "broadcast_tx_sync":
		m.broadcasts++
		m.captureGas(req.Params)
		if m.broadcasts <= m.checkTxOOG {
			m.writeResult(w, req.ID, &coretypes.ResultBroadcastTx{
				Code:      errortypes.ErrOutOfGas.ABCICode(),
				Codespace: errortypes.ErrOutOfGas.Codespace(),
				Log:       "out of gas in CheckTx",
			})
			return
		}
		m.writeResult(w, req.ID, &coretypes.ResultBroadcastTx{
			Hash: cmtbytes.HexBytes(testCosmosTxHash(m.broadcasts)),
		})
	case "tx":
		m.txQueries++
		result := abci.ExecTxResult{}
		if m.txQueries <= m.deliverTxOOG {
			result.Code = errortypes.ErrOutOfGas.ABCICode()
			result.Codespace = errortypes.ErrOutOfGas.Codespace()
			result.Log = "out of gas in DeliverTx"
		}
		m.writeResult(w, req.ID, &coretypes.ResultTx{
			Hash:     cmtbytes.HexBytes(testCosmosTxHash(m.broadcasts)),
			Height:   int64(m.txQueries),
			TxResult: result,
		})
	default:
		m.writeError(w, req.ID, -32601, "method not found")
	}
}

func testCosmosTxHash(n int) []byte {
	hash := make([]byte, 32)
	hash[len(hash)-1] = byte(n)
	return hash
}

func (m *cosmosOOGRPC) captureGas(params json.RawMessage) {
	var named map[string]json.RawMessage
	if err := json.Unmarshal(params, &named); err != nil {
		m.t.Fatalf("decode broadcast params: %v", err)
	}
	var encoded string
	if err := json.Unmarshal(named["tx"], &encoded); err != nil {
		m.t.Fatalf("decode broadcast tx parameter: %v", err)
	}
	txBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		m.t.Fatalf("decode broadcast tx bytes: %v", err)
	}
	var raw txservice.TxRaw
	if err := proto.Unmarshal(txBytes, &raw); err != nil {
		m.t.Fatalf("decode TxRaw: %v", err)
	}
	var authInfo txservice.AuthInfo
	if err := proto.Unmarshal(raw.AuthInfoBytes, &authInfo); err != nil {
		m.t.Fatalf("decode AuthInfo: %v", err)
	}
	if authInfo.Fee == nil {
		m.t.Fatal("broadcast transaction has no fee")
	}
	m.gasLimits = append(m.gasLimits, authInfo.Fee.GasLimit)
	if len(authInfo.Fee.Amount) != 1 {
		m.t.Fatalf("broadcast fee coins = %v, want exactly one", authInfo.Fee.Amount)
	}
	m.feeAmounts = append(m.feeAmounts, authInfo.Fee.Amount[0].Amount.Int64())
}

func (m *cosmosOOGRPC) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	m.t.Helper()
	raw, err := cmtjson.Marshal(result)
	if err != nil {
		m.t.Fatalf("marshal RPC result: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cosmosRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  raw,
	}); err != nil {
		m.t.Errorf("encode RPC response: %v", err)
	}
}

func (m *cosmosOOGRPC) writeError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	m.t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cosmosRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: map[string]any{
			"code":    code,
			"message": message,
		},
	}); err != nil {
		m.t.Errorf("encode RPC error: %v", err)
	}
}

func newCosmosOOGEndpoint(t *testing.T, mock *cosmosOOGRPC) services.CosmosEndpoint {
	t.Helper()
	server := httptest.NewServer(mock)
	t.Cleanup(server.Close)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatalf("create CometBFT client: %v", err)
	}
	return services.CosmosEndpoint{Client: client}
}

func cosmosOOGTestMessage() sdk.Msg {
	return &clienttypes.MsgUpdateClient{
		ClientId: "08-wasm-0",
		ClientMessage: &codectypes.Any{
			TypeUrl: "/test.ClientMessage",
			Value:   []byte{1},
		},
	}
}

func configureCosmosOOGTest(t *testing.T) {
	t.Helper()
	t.Setenv("COSMOS_PRIVATE_KEY", testCosmosPrivKeyHex)
	t.Setenv("COSMOS_CHAIN_ID", "test-chain")
	t.Setenv("COSMOS_GAS_LIMIT", "200000")
	t.Setenv("COSMOS_FEE_DENOM", "stake")
	t.Setenv("COSMOS_FEE_AMOUNT", "")
}

func assertGasStrictlyIncreases(t *testing.T, limits []uint64) {
	t.Helper()
	if len(limits) < 2 {
		t.Fatalf("broadcast gas limits = %v, want at least two attempts", limits)
	}
	for i := 1; i < len(limits); i++ {
		if limits[i] <= limits[i-1] {
			t.Fatalf("broadcast gas limits = %v; attempt %d did not increase", limits, i+1)
		}
	}
}

func TestSendCosmosTxBatchWithSplitting(t *testing.T) {
	t.Run("fallback gas increases after deliver tx out of gas", func(t *testing.T) {
		configureCosmosOOGTest(t)
		mock := &cosmosOOGRPC{t: t, deliverTxOOG: 1}
		endpoint := newCosmosOOGEndpoint(t, mock)

		h := &Handler{}
		nextSequence, succeeded, err := h.sendCosmosTxBatchWithSplitting(
			context.Background(), endpoint, []sdk.Msg{cosmosOOGTestMessage()}, 7, 10, true,
		)
		if err != nil {
			t.Fatalf("send after DeliverTx OOG: %v", err)
		}
		if nextSequence != 12 || succeeded != 1 {
			t.Fatalf("result = sequence:%d succeeded:%d, want 12:1", nextSequence, succeeded)
		}
		assertGasStrictlyIncreases(t, mock.gasLimits)
	})

	t.Run("check tx out of gas retries with more gas and retains sequence", func(t *testing.T) {
		configureCosmosOOGTest(t)
		mock := &cosmosOOGRPC{t: t, checkTxOOG: 1}
		endpoint := newCosmosOOGEndpoint(t, mock)

		h := &Handler{}
		nextSequence, succeeded, err := h.sendCosmosTxBatchWithSplitting(
			context.Background(), endpoint, []sdk.Msg{cosmosOOGTestMessage()}, 7, 10, true,
		)
		if err != nil {
			t.Fatalf("send after CheckTx OOG: %v", err)
		}
		if nextSequence != 11 || succeeded != 1 {
			t.Fatalf("result = sequence:%d succeeded:%d, want 11:1; CheckTx must not consume sequence 10", nextSequence, succeeded)
		}
		assertGasStrictlyIncreases(t, mock.gasLimits)
	})

	t.Run("check tx out of gas scales configured fee with gas", func(t *testing.T) {
		configureCosmosOOGTest(t)
		t.Setenv("COSMOS_FEE_AMOUNT", "1000")
		mock := &cosmosOOGRPC{t: t, checkTxOOG: 1}
		endpoint := newCosmosOOGEndpoint(t, mock)

		h := &Handler{}
		_, _, err := h.sendCosmosTxBatchWithSplitting(
			context.Background(), endpoint, []sdk.Msg{cosmosOOGTestMessage()}, 7, 10, true,
		)
		if err != nil {
			t.Fatalf("send after CheckTx OOG: %v", err)
		}
		if len(mock.feeAmounts) != 2 {
			t.Fatalf("broadcast fee amounts = %v, want two attempts", mock.feeAmounts)
		}
		if mock.feeAmounts[0] != 1000 {
			t.Fatalf("initial configured fee = %d, want historical value 1000", mock.feeAmounts[0])
		}
		wantRetryFee := applyCosmosFeeHeadroom(1000, 1)
		if mock.feeAmounts[1] != wantRetryFee || mock.feeAmounts[1] <= mock.feeAmounts[0] {
			t.Fatalf("broadcast fee amounts = %v, want retry fee to grow to %d with gas headroom", mock.feeAmounts, wantRetryFee)
		}
		if mock.feeAmounts[1]*int64(mock.gasLimits[0]) < mock.feeAmounts[0]*int64(mock.gasLimits[1]) {
			t.Fatalf("fee per gas decreased across retry: fees=%v gas=%v", mock.feeAmounts, mock.gasLimits)
		}
		assertGasStrictlyIncreases(t, mock.gasLimits)
	})

	t.Run("check tx out of gas exhausts the headroom ladder", func(t *testing.T) {
		configureCosmosOOGTest(t)
		mock := &cosmosOOGRPC{t: t, checkTxOOG: 100}
		endpoint := newCosmosOOGEndpoint(t, mock)

		h := &Handler{}
		nextSequence, succeeded, err := h.sendCosmosTxBatchWithSplitting(
			context.Background(), endpoint, []sdk.Msg{cosmosOOGTestMessage()}, 7, 10, true,
		)
		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Fatalf("exhausted CheckTx ladder error = %v, want permanent relay failure", err)
		}
		if nextSequence != 10 || succeeded != 0 {
			t.Fatalf("result = sequence:%d succeeded:%d, want 10:0", nextSequence, succeeded)
		}
		if len(mock.gasLimits) != len(cosmosGasHeadroom) {
			t.Fatalf("broadcast attempts = %d, want finite ladder length %d", len(mock.gasLimits), len(cosmosGasHeadroom))
		}
		assertGasStrictlyIncreases(t, mock.gasLimits)
	})
}

func TestDuplicateDropIsSafe_OnlySafeForOneMessage(t *testing.T) {
	cases := []struct {
		name     string
		msgCount int
		want     bool
	}{
		{"single message is the whole transaction", 1, true},
		{"two-message batch", 2, false},
		{"large batch", 25, false},
		{"empty batch", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := duplicateDropIsSafe(tc.msgCount); got != tc.want {
				t.Fatalf("duplicateDropIsSafe(%d) = %v, want %v", tc.msgCount, got, tc.want)
			}
		})
	}
}

// TestIsCosmosDuplicatePacketError covers the deterministic duplicate/redundant
// signals that must be dropped (not re-queued): channelv2 11/12 and, critically,
// channel/22 ErrRedundantTx — the ante-handler rejection whose omission caused an
// infinite re-submit gas drain.
func TestIsCosmosDuplicatePacketError(t *testing.T) {
	cases := []struct {
		name      string
		codespace string
		code      uint32
		want      bool
	}{
		{"channelv2 ack-exists", "channelv2", 11, true},
		{"channelv2 noop", "channelv2", 12, true},
		{"channel redundant-tx", "channel", 22, true},
		{"channel noop (23) not matched", "channel", 23, false},
		{"channelv2 other code", "channelv2", 5, false},
		{"unrelated codespace", "ibc", 22, false},
		{"zero", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCosmosDuplicatePacketError(tc.codespace, tc.code); got != tc.want {
				t.Fatalf("isCosmosDuplicatePacketError(%q,%d) = %v, want %v", tc.codespace, tc.code, got, tc.want)
			}
		})
	}
}

func TestNewCosmosPreDeliverFailure_DuplicateFailuresBeforeDeliverTxArePermanent(t *testing.T) {
	for _, stage := range []string{"Simulation", "CheckTx"} {
		t.Run(stage, func(t *testing.T) {
			err := newCosmosPreDeliverFailure(stage, 22, "channel", "redundant tx", nil)
			if !errors.Is(err, services.ErrPermanentRelayFailure) {
				t.Fatalf("duplicate %s failure is not permanent: %v", stage, err)
			}
		})
	}

	err := newCosmosPreDeliverFailure("CheckTx", 32, "sdk", "account sequence mismatch", nil)
	if errors.Is(err, services.ErrPermanentRelayFailure) {
		t.Fatalf("transient CheckTx failure was marked permanent: %v", err)
	}
}

// TestCosmosClientNotActiveIsNotPermanent pins #402 on the ETH->Cosmos mirror: a
// frozen or expired client is refused by ibc-go with a client-codespace error at
// every stage, and none of them may reach the relay module as a dead packet.
func TestCosmosClientNotActiveIsNotPermanent(t *testing.T) {
	// The codes are ibc-go's registrations, read from ibc-go rather than restated,
	// so an upstream renumbering fails here instead of silently reclassifying.
	for _, registered := range []*errorsmod.Error{clienttypes.ErrClientNotActive, clienttypes.ErrClientFrozen} {
		codespace, code := registered.Codespace(), registered.ABCICode()
		if !isCosmosClientNotActiveError(codespace, code) {
			t.Fatalf("%s (codespace=%s code=%d) not recognised", registered, codespace, code)
		}
		for _, stage := range []string{"Simulation", "CheckTx"} {
			err := newCosmosPreDeliverFailure(stage, code, codespace, registered.Error(), nil)
			if !errors.Is(err, services.ErrClientNotActive) || errors.Is(err, services.ErrPermanentRelayFailure) {
				t.Fatalf("%s %s: want ErrClientNotActive and not permanent, got %v", stage, registered, err)
			}
		}
		if err := classifyCosmosFailure(codespace, code, services.ErrPermanentRelayFailure); err != services.ErrClientNotActive {
			t.Fatalf("DeliverTx %s: want ErrClientNotActive, got %v", registered, err)
		}
	}
	// Anything else at DeliverTx keeps its permanent classification.
	if err := classifyCosmosFailure("channelv2", 3, services.ErrPermanentRelayFailure); err != services.ErrPermanentRelayFailure {
		t.Fatalf("an unrelated DeliverTx failure must stay permanent, got %v", err)
	}
	if isCosmosClientNotActiveError("client", 4) {
		t.Fatal("client not found is a configuration error, not an inactive client")
	}
}

// TestCosmosSimulationRejection_SDKEnvelope pins the shape a frozen client's
// Simulate rejection actually has. The SDK (v0.53.3) answers the Simulate query
// with sdk/6 and keeps client/29 only as text -- the response below is the one
// recorded from a real BaseApp query in the PR #536 review. The code-based
// classifier cannot recognise it, so the relay module's status fallback depends
// on this error staying identifiable as a Simulate rejection.
func TestCosmosSimulationRejection_SDKEnvelope(t *testing.T) {
	configureCosmosOOGTest(t)
	mock := &cosmosOOGRPC{t: t, simulation: &abci.ResponseQuery{
		Code:      errortypes.ErrUnknownRequest.ABCICode(),
		Codespace: errortypes.ErrUnknownRequest.Codespace(),
		Log: "rpc error: code = Unknown desc = 08-wasm-0 is Frozen: client state is not active " +
			"[cosmossdk.io/errors@v1.0.2/errors.go:151] with gas used: '123': unknown request",
	}}
	endpoint := newCosmosOOGEndpoint(t, mock)

	// allowSplit=false is the atomic path folded Cosmos/L2 batches take.
	_, _, err := (&Handler{}).sendCosmosTxBatchWithSplitting(
		context.Background(), endpoint, []sdk.Msg{cosmosOOGTestMessage()}, 7, 10, false,
	)
	if err == nil {
		t.Fatal("a rejected simulation must fail the atomic batch")
	}
	if !services.IsCosmosSimulationRejection(err) {
		t.Fatalf("the rejection must stay identifiable as a Simulate rejection: %v", err)
	}
	var failure *services.CosmosTxFailure
	if !errors.As(err, &failure) || failure.Codespace != "sdk" || failure.Code != 6 {
		t.Fatalf("want the SDK envelope sdk/6 preserved, got %v", err)
	}
	// Documented limitation, not a goal: nothing at this layer can name the client.
	if errors.Is(err, services.ErrClientNotActive) || errors.Is(err, services.ErrPermanentRelayFailure) {
		t.Fatalf("sdk/6 must not be classified by code: %v", err)
	}
	if mock.broadcasts != 0 {
		t.Fatalf("an atomic batch whose simulation was rejected must not broadcast, got %d", mock.broadcasts)
	}

	// A transport failure reaching the node carries no ABCI metadata, so it is not
	// a Simulate rejection -- the module must not spend a status query on it.
	if services.IsCosmosSimulationRejection(errors.New("simulate ABCI query failed: connection refused")) {
		t.Fatal("a transport failure is not a Simulate rejection")
	}
}

func TestSplitCosmosBatchAfterDuplicateWith(t *testing.T) {
	t.Run("around duplicate stops at the first failing half", func(t *testing.T) {
		msgs := make([]sdk.Msg, 4)
		firstHalfErr := errors.New("first half reverted")

		var calls int
		send := func(
			_ context.Context,
			_ services.CosmosEndpoint,
			sub []sdk.Msg,
			_, sequence uint64,
			allowSplit bool,
		) (uint64, int, error) {
			calls++
			if !allowSplit {
				t.Fatal("duplicate halves must retain recursive splitting")
			}
			if calls == 1 {
				return sequence, 0, firstHalfErr
			}
			return sequence + uint64(len(sub)), len(sub), nil
		}

		_, succeeded, err := splitCosmosBatchAfterDuplicateWith(
			send, context.Background(), services.CosmosEndpoint{}, msgs, 7, 100,
		)
		if !errors.Is(err, firstHalfErr) {
			t.Fatalf("want the first half error propagated, got %v", err)
		}
		if calls != 1 {
			t.Fatalf("the second half must not be attempted after the first fails, got %d calls", calls)
		}
		if succeeded != 0 {
			t.Fatalf("nothing was delivered, so the prefix count must be 0, got %d", succeeded)
		}
	})

	t.Run("around duplicate threads sequence and sums on success", func(t *testing.T) {
		msgs := make([]sdk.Msg, 4)
		var seenSequences []uint64

		send := func(
			_ context.Context,
			_ services.CosmosEndpoint,
			sub []sdk.Msg,
			_, sequence uint64,
			allowSplit bool,
		) (uint64, int, error) {
			if !allowSplit {
				t.Fatal("duplicate halves must retain recursive splitting")
			}
			seenSequences = append(seenSequences, sequence)
			return sequence + uint64(len(sub)), len(sub), nil
		}

		finalSequence, succeeded, err := splitCosmosBatchAfterDuplicateWith(
			send, context.Background(), services.CosmosEndpoint{}, msgs, 7, 100,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if succeeded != 4 {
			t.Fatalf("want all 4 messages reported delivered, got %d", succeeded)
		}
		if finalSequence != 104 {
			t.Fatalf("want the final sequence threaded out (104), got %d", finalSequence)
		}
		if len(seenSequences) != 2 || seenSequences[0] != 100 || seenSequences[1] != 102 {
			t.Fatalf("halves must be signed with consecutive sequences [100 102], got %v", seenSequences)
		}
	})

	// The batch rung and the split that carries it out are computed in two
	// different places: CosmosBatchLadder returns a size, and
	// splitCosmosBatchAfterDuplicateWith halves the slice itself. Both were pinned
	// -- the ladder by TestLaddersChangeSomethingEachRungAndEnd, the split by the
	// hard-coded sequences above -- but nothing tied them to each other, so either
	// could be changed alone and stay green. The rung is what the operator reads in
	// "splitting ... of %d to %d"; when the two disagree that line reports a size
	// the retry never submitted, and C1's claim that the call site only describes
	// the ladder stops being true.
	t.Run("submits the size the ladder names", func(t *testing.T) {
		// Odd counts are the interesting ones: integer division is where two
		// independent halvings drift first.
		for _, count := range []int{2, 3, 4, 5, 9, 32} {
			t.Run(fmt.Sprintf("%d messages", count), func(t *testing.T) {
				rung, ok := CosmosBatchLadder(count)(0)
				if !ok {
					t.Fatalf("the ladder reports no rung for %d messages, so the split must not be reached at all", count)
				}

				var firstSubmitted int
				send := func(
					_ context.Context,
					_ services.CosmosEndpoint,
					sub []sdk.Msg,
					_, sequence uint64,
					_ bool,
				) (uint64, int, error) {
					if firstSubmitted == 0 {
						firstSubmitted = len(sub)
					}
					return sequence + uint64(len(sub)), len(sub), nil
				}

				if _, _, err := splitCosmosBatchAfterDuplicateWith(
					send, context.Background(), services.CosmosEndpoint{}, make([]sdk.Msg, count), 7, 100,
				); err != nil {
					t.Fatalf("split: %v", err)
				}

				if firstSubmitted != int(rung.To) {
					t.Fatalf("the ladder named %d messages but the split submitted %d; the log line and the retry disagree",
						rung.To, firstSubmitted)
				}
			})
		}
	})
}

// Devnet-only sample key (relayer/.env); safe to embed in tests.
const testCosmosPrivKeyHex = "a81f9eb900c02f35d28ec80d1da79dde52cc4cc37d6fbaef5768ab59f32cbfdd"

func TestCosmosSignerAddress_Prefix(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string // COSMOS_ADDRESS_PREFIX; "" = unset
		wantPrefix string
	}{
		{name: "default is cosmos", prefix: "", wantPrefix: "cosmos"},
		{name: "realio", prefix: "realio", wantPrefix: "realio"},
		{name: "osmo", prefix: "osmo", wantPrefix: "osmo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COSMOS_PRIVATE_KEY", testCosmosPrivKeyHex)
			t.Setenv("COSMOS_ADDRESS_PREFIX", tc.prefix)

			addr, err := (&Handler{}).CosmosSignerAddress()
			if err != nil {
				t.Fatalf("CosmosSignerAddress: %v", err)
			}
			if !strings.HasPrefix(addr, tc.wantPrefix+"1") {
				t.Fatalf("address %q does not start with %q", addr, tc.wantPrefix+"1")
			}
			hrp, _, err := sdkbech32.DecodeAndConvert(addr)
			if err != nil {
				t.Fatalf("address %q is not valid bech32: %v", addr, err)
			}
			if hrp != tc.wantPrefix {
				t.Fatalf("decoded hrp = %q, want %q", hrp, tc.wantPrefix)
			}
		})
	}
}

// The prefix must only change the encoding, never the underlying account bytes.
func TestCosmosSignerAddress_PrefixSameAccount(t *testing.T) {
	t.Setenv("COSMOS_PRIVATE_KEY", testCosmosPrivKeyHex)

	t.Setenv("COSMOS_ADDRESS_PREFIX", "")
	defaultAddr, err := (&Handler{}).CosmosSignerAddress()
	if err != nil {
		t.Fatalf("CosmosSignerAddress (default): %v", err)
	}

	t.Setenv("COSMOS_ADDRESS_PREFIX", "realio")
	realioAddr, err := (&Handler{}).CosmosSignerAddress()
	if err != nil {
		t.Fatalf("CosmosSignerAddress (realio): %v", err)
	}

	_, defaultBytes, err := sdkbech32.DecodeAndConvert(defaultAddr)
	if err != nil {
		t.Fatalf("decode %q: %v", defaultAddr, err)
	}
	_, realioBytes, err := sdkbech32.DecodeAndConvert(realioAddr)
	if err != nil {
		t.Fatalf("decode %q: %v", realioAddr, err)
	}
	if string(defaultBytes) != string(realioBytes) {
		t.Fatalf("account bytes differ between prefixes: %x vs %x", defaultBytes, realioBytes)
	}
}

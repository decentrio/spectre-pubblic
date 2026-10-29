package transaction

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	contractICS26Router "relayer/bindings/ICS26Router"
	"relayer/keys"
	"relayer/rpcmock"
	"relayer/services"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// abandonedTxFixture is the state the give-up paths are in: a signed payload
// already broadcast at some nonce, and a node that will be asked to displace it.
type abandonedTxFixture struct {
	node    *mockJSONRPC
	client  *ethclient.Client
	key     *ecdsa.PrivateKey
	from    common.Address
	chainID *big.Int
	tx      *types.Transaction
}

func newAbandonedTxFixture(t *testing.T, nonce uint64) abandonedTxFixture {
	t.Helper()

	node := newMockJSONRPC()
	srv := httptest.NewServer(node)
	t.Cleanup(srv.Close)

	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatalf("dial mock RPC: %v", err)
	}
	t.Cleanup(client.Close)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	chainID := big.NewInt(1)
	payload := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(2_000_000_000),
		GasFeeCap: big.NewInt(4_000_000_000),
		Gas:       100_000,
		To:        &common.Address{0x1},
		Data:      []byte{0xde, 0xad, 0xbe, 0xef},
	})
	signed, err := types.SignTx(payload, types.LatestSignerForChainID(chainID), key)
	if err != nil {
		t.Fatalf("sign payload: %v", err)
	}

	return abandonedTxFixture{
		node:    node,
		client:  client,
		key:     key,
		from:    crypto.PubkeyToAddress(key.PublicKey),
		chainID: chainID,
		tx:      signed,
	}
}

func (f abandonedTxFixture) endpoint() services.EVMEndpoint {
	return services.EVMEndpoint{Client: f.client}
}

// A transaction the relayer stops waiting for does not stop existing: it stays
// queued, and the next attempt takes a LOWER nonce, so closing the gap lets the
// payload execute after the relayer already resubmitted it -- the action runs
// twice (#410). Cancelling means putting a no-op at that nonce first.
func TestCancelAbandonedTx(t *testing.T) {
	t.Run("puts a zero-value self-transfer at the abandoned nonce", func(t *testing.T) {
		f := newAbandonedTxFixture(t, 42)

		outcome, err := (&Handler{}).cancelAbandonedTx(
			context.Background(), f.endpoint(), f.key, f.tx, f.from, f.chainID)
		if err != nil {
			t.Fatalf("cancel abandoned tx: %v", err)
		}
		if outcome != cancelBroadcast {
			t.Fatalf("outcome = %v, want cancelBroadcast: the node accepted it, which is all this can claim", outcome)
		}

		sent := f.node.sentTransactions()
		if len(sent) != 1 {
			t.Fatalf("broadcast %d transactions, want exactly the cancellation", len(sent))
		}
		cancel := sent[0]

		// The nonce is the whole point: a cancellation anywhere else leaves the
		// payload executable.
		if cancel.Nonce() != f.tx.Nonce() {
			t.Errorf("cancellation nonce = %d, want the abandoned %d", cancel.Nonce(), f.tx.Nonce())
		}
		if cancel.To() == nil || *cancel.To() != f.from {
			t.Errorf("cancellation to = %v, want a self-transfer to %s", cancel.To(), f.from.Hex())
		}
		if cancel.Value().Sign() != 0 {
			t.Errorf("cancellation value = %s, want zero", cancel.Value())
		}
		if len(cancel.Data()) != 0 {
			t.Errorf("cancellation carries %d bytes of calldata, want none", len(cancel.Data()))
		}

		// Below the original's fee the node keeps the original and the
		// cancellation is the transaction that gets dropped.
		if cancel.GasTipCap().Cmp(f.tx.GasTipCap()) <= 0 {
			t.Errorf("cancellation tip cap %s does not exceed the abandoned %s; it would not displace it",
				cancel.GasTipCap(), f.tx.GasTipCap())
		}
		if cancel.GasFeeCap().Cmp(f.tx.GasFeeCap()) <= 0 {
			t.Errorf("cancellation fee cap %s does not exceed the abandoned %s; it would not displace it",
				cancel.GasFeeCap(), f.tx.GasFeeCap())
		}
	})

	t.Run("a nonce the account is already past needs no cancellation", func(t *testing.T) {
		f := newAbandonedTxFixture(t, 42)
		f.node.sendErrFor = func(*types.Transaction) error { return errors.New("nonce too low") }

		// A consumed nonce says something took it, and the likeliest something is
		// the payload itself -- which would mean the operation SUCCEEDED. Reported
		// by @DongLieu: this used to report success, and the caller then resubmitted
		// a payload that had already executed. It is unresolved, not cancelled.
		outcome, err := (&Handler{}).cancelAbandonedTx(
			context.Background(), f.endpoint(), f.key, f.tx, f.from, f.chainID)
		if outcome != cancelUnresolved {
			t.Fatalf("outcome = %v, want cancelUnresolved: the nonce may have been consumed by the payload", outcome)
		}
		if err == nil {
			t.Fatal("a consumed nonce reported as a clean cancellation; the payload may have executed")
		}
	})

	t.Run("a cancellation already in the pool is not a failure", func(t *testing.T) {
		f := newAbandonedTxFixture(t, 42)
		f.node.sendErrFor = func(*types.Transaction) error { return errors.New("already known") }

		outcome, err := (&Handler{}).cancelAbandonedTx(
			context.Background(), f.endpoint(), f.key, f.tx, f.from, f.chainID)
		if err != nil {
			t.Fatalf("an already-broadcast cancellation reported as a failure: %v", err)
		}
		if outcome != cancelBroadcast {
			t.Fatalf("outcome = %v, want cancelBroadcast", outcome)
		}
	})

	t.Run("any other rejection is reported", func(t *testing.T) {
		f := newAbandonedTxFixture(t, 42)
		f.node.sendErrFor = func(*types.Transaction) error { return errors.New("replacement transaction underpriced") }

		outcome, err := (&Handler{}).cancelAbandonedTx(
			context.Background(), f.endpoint(), f.key, f.tx, f.from, f.chainID)
		if outcome != cancelUnresolved {
			t.Fatalf("outcome = %v, want cancelUnresolved", outcome)
		}
		if err == nil {
			t.Fatal("a rejected cancellation reported as success; the payload can still execute")
		}
		if !strings.Contains(err.Error(), "42") {
			t.Errorf("error = %v, want it to name the nonce left outstanding", err)
		}
	})
}

// onGiveUp is the one exit the wait loop takes when it stops waiting, so the
// classification of the result is decided in a single place.
//
// That classification changed after review. Reported by @DongLieu: a broadcast
// cancellation is acceptance by ONE node, not inclusion, so "the cancellation
// landed" was never a thing this code could know -- and returning a plain
// retryable error let the payload be resubmitted while the original was still
// live. Every ending now carries ErrUnresolvedEVMNonce.
func TestOnGiveUp(t *testing.T) {
	cause := errors.New("transaction wait mined timed out")
	noHashes := []common.Hash(nil)

	t.Run("a broadcast cancellation is still unresolved", func(t *testing.T) {
		f := newAbandonedTxFixture(t, 42)

		err := (&Handler{}).onGiveUp(
			context.Background(), f.endpoint(), f.key, f.tx, f.from, f.chainID, noHashes, cause)
		if !errors.Is(err, cause) {
			t.Fatalf("error = %v, want the wait failure still reachable", err)
		}
		if !errors.Is(err, services.ErrUnresolvedEVMNonce) {
			t.Fatalf("error = %v, want unresolved: one node accepting the replacement does not decide the nonce", err)
		}
		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Fatalf("error = %v, want permanent: nothing derived from this attempt may be retried", err)
		}
	})

	t.Run("a failed cancellation is unresolved too", func(t *testing.T) {
		f := newAbandonedTxFixture(t, 42)
		f.node.sendErrFor = func(*types.Transaction) error { return errors.New("replacement transaction underpriced") }

		err := (&Handler{}).onGiveUp(
			context.Background(), f.endpoint(), f.key, f.tx, f.from, f.chainID, noHashes, cause)
		if !errors.Is(err, services.ErrUnresolvedEVMNonce) {
			t.Fatalf("error = %v, want unresolved", err)
		}
		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Fatalf("error = %v, want permanent", err)
		}
		// Permanent must not cost the diagnosis of what actually went wrong.
		if !errors.Is(err, cause) {
			t.Errorf("error = %v, want the original wait failure still reachable", err)
		}
	})

	t.Run("invalidates the cached nonce whether or not the cancellation landed", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			sendErr error
		}{
			{name: "cancellation landed"},
			{name: "cancellation rejected", sendErr: errors.New("replacement transaction underpriced")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newAbandonedTxFixture(t, 42)
				if tc.sendErr != nil {
					f.node.sendErrFor = func(*types.Transaction) error { return tc.sendErr }
				}

				h := &Handler{}
				h.mu.Lock()
				state := h.senderState(f.chainID.String(), f.from)
				state.nonce = 42
				state.nonceValid = true
				h.mu.Unlock()

				_ = h.onGiveUp(context.Background(), f.endpoint(), f.key, f.tx, f.from, f.chainID, nil, cause)

				h.mu.Lock()
				stillValid := h.senderState(f.chainID.String(), f.from).nonceValid
				h.mu.Unlock()
				if stillValid {
					t.Fatal("cached nonce still valid; the next attempt would reuse a nonce the chain disagrees with")
				}
			})
		}
	})
}

func TestExecuteWithRetryAndResubmission(t *testing.T) {
	// The #410 regression at the exit that reaches it soonest: the pending-nonce
	// probe sees a gap, gives up, and the caller then resubmits the payload at the
	// lower nonce the chain actually needs. Without a cancellation broadcast at the
	// abandoned nonce first, closing the gap executes the payload a second time.
	t.Run("abandoned nonce is cancelled", func(t *testing.T) {
		oldTimeout := ethTxReceiptTimeout
		oldPoll := ethTxReceiptPollInterval
		ethTxReceiptTimeout = 60 * time.Millisecond
		ethTxReceiptPollInterval = 10 * time.Millisecond
		defer func() {
			ethTxReceiptTimeout = oldTimeout
			ethTxReceiptPollInterval = oldPoll
		}()

		node := newMockJSONRPC()
		node.nonce = 7 // what the chain needs; the cache below is far ahead of it
		srv := httptest.NewServer(node)
		defer srv.Close()
		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		from := crypto.PubkeyToAddress(privKey.PublicKey)

		const abandonedNonce = 50
		h := &Handler{}
		h.mu.Lock()
		state := h.senderState("1", from)
		state.nonce = abandonedNonce
		state.nonceValid = true
		h.mu.Unlock()

		// A bound contract call signs AND broadcasts, and the hazard only exists
		// because the payload really did reach the mempool -- so the stand-in has to
		// broadcast too, or the test would be asserting against a chain that never
		// saw it.
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(1), Nonce: auth.Nonce.Uint64(), GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
				Data: []byte{0xde, 0xad, 0xbe, 0xef},
			})
			signed, signErr := auth.Signer(auth.From, tx)
			if signErr != nil {
				return nil, signErr
			}
			if sendErr := client.SendTransaction(auth.Context, signed); sendErr != nil {
				return nil, sendErr
			}
			return signed, nil
		}

		_, _, _, err = h.executeWithRetryAndResubmission(
			context.Background(), services.EVMEndpoint{Client: client}, privKey, 100_000, senderFn,
		)
		if err == nil {
			t.Fatal("execute succeeded; want the nonce-gap failure")
		}

		sent := node.sentTransactions()
		if len(sent) != 2 {
			t.Fatalf("broadcast %d transactions, want the payload and its cancellation", len(sent))
		}
		payload, cancel := sent[0], sent[1]
		if payload.Nonce() != abandonedNonce {
			t.Fatalf("payload nonce = %d, want the cached %d", payload.Nonce(), abandonedNonce)
		}
		if cancel.Nonce() != abandonedNonce {
			t.Fatalf("cancellation nonce = %d, want the abandoned %d: anywhere else and the payload still executes",
				cancel.Nonce(), abandonedNonce)
		}
		if len(cancel.Data()) != 0 || cancel.Value().Sign() != 0 {
			t.Fatalf("the transaction left at nonce %d still carries %d bytes of calldata and value %s; it is the payload, not a no-op",
				abandonedNonce, len(cancel.Data()), cancel.Value())
		}
		if cancel.To() == nil || *cancel.To() != from {
			t.Errorf("cancellation to = %v, want a self-transfer to %s", cancel.To(), from.Hex())
		}
	})

	t.Run("nonce retry", func(t *testing.T) {
		mockRPC := newMockJSONRPC()
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		var attempts int
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("nonce too low")
			}

			tx := types.NewTx(&types.LegacyTx{
				Nonce:    auth.Nonce.Uint64(),
				GasPrice: auth.GasPrice,
				Gas:      auth.GasLimit,
				To:       &common.Address{0x1},
				Value:    big.NewInt(0),
				Data:     []byte{},
			})
			return auth.Signer(auth.From, tx)
		}

		var txHash common.Hash
		senderFnWrapped := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx, err := senderFn(auth)
			if err == nil && tx != nil {
				txHash = tx.Hash()
				mockRPC.mu.Lock()
				mockRPC.receiptResps[txHash] = &types.Receipt{
					Status:      1,
					GasUsed:     21000,
					BlockNumber: big.NewInt(200),
				}
				mockRPC.mu.Unlock()
			}
			return tx, err
		}

		receipt, _, _, err := h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFnWrapped)
		if err != nil {
			t.Fatalf("executeWithRetryAndResubmission failed: %v", err)
		}

		if receipt == nil || receipt.BlockNumber.Uint64() != 200 {
			t.Errorf("expected receipt at block 200, got %v", receipt)
		}

		if attempts != 2 {
			t.Errorf("expected 2 attempts, got %d", attempts)
		}
	})

	t.Run("scopes nonce caches by chain and sender", func(t *testing.T) {
		relayKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate relay key: %v", err)
		}
		incidentKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate incident key: %v", err)
		}
		relayAddress := crypto.PubkeyToAddress(relayKey.PublicKey)
		incidentAddress := crypto.PubkeyToAddress(incidentKey.PublicKey)

		chainOneRPC := newMockJSONRPC()
		chainOneRPC.chainID = 1
		chainOneRPC.nonces[relayAddress] = 100
		chainOneRPC.nonces[incidentAddress] = 0
		chainOneServer := httptest.NewServer(chainOneRPC)
		defer chainOneServer.Close()
		chainOneClient, err := ethclient.Dial(chainOneServer.URL)
		if err != nil {
			t.Fatalf("dial chain one: %v", err)
		}

		chainTwoRPC := newMockJSONRPC()
		chainTwoRPC.chainID = 2
		chainTwoRPC.nonces[relayAddress] = 7
		chainTwoServer := httptest.NewServer(chainTwoRPC)
		defer chainTwoServer.Close()
		chainTwoClient, err := ethclient.Dial(chainTwoServer.URL)
		if err != nil {
			t.Fatalf("dial chain two: %v", err)
		}

		h := &Handler{}
		execute := func(name string, client *ethclient.Client, rpc *mockJSONRPC, privateKey *ecdsa.PrivateKey) uint64 {
			t.Helper()
			endpoint := services.EVMEndpoint{Client: client}
			chainID, err := client.ChainID(context.Background())
			if err != nil {
				t.Fatalf("%s chain ID: %v", name, err)
			}
			var usedNonce uint64
			sender := func(auth *bind.TransactOpts) (*types.Transaction, error) {
				usedNonce = auth.Nonce.Uint64()
				tx := types.NewTx(&types.DynamicFeeTx{
					ChainID: chainID, Nonce: usedNonce, GasTipCap: auth.GasTipCap,
					GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
				})
				signed, signErr := auth.Signer(auth.From, tx)
				if signErr == nil {
					rpc.mu.Lock()
					rpc.receiptResps[signed.Hash()] = &types.Receipt{Status: 1, GasUsed: 21_000, BlockNumber: big.NewInt(1)}
					rpc.mu.Unlock()
				}
				return signed, signErr
			}
			if _, _, _, err := h.executeWithRetryAndResubmission(context.Background(), endpoint, privateKey, 100_000, sender); err != nil {
				t.Fatalf("%s execute: %v", name, err)
			}
			return usedNonce
		}

		if got := execute("relay chain one", chainOneClient, chainOneRPC, relayKey); got != 100 {
			t.Fatalf("relay chain-one nonce = %d, want 100", got)
		}
		if got := execute("incident chain one", chainOneClient, chainOneRPC, incidentKey); got != 0 {
			t.Fatalf("incident chain-one nonce = %d, want 0", got)
		}
		if got := execute("relay chain two", chainTwoClient, chainTwoRPC, relayKey); got != 7 {
			t.Fatalf("relay chain-two nonce = %d, want 7", got)
		}
		if chainOneRPC.nonceCalls[relayAddress] != 1 || chainOneRPC.nonceCalls[incidentAddress] != 1 {
			t.Fatalf("chain-one pending nonce calls relay/incident = %d/%d, want 1/1",
				chainOneRPC.nonceCalls[relayAddress], chainOneRPC.nonceCalls[incidentAddress])
		}
	})

	t.Run("already known", func(t *testing.T) {
		mockRPC := newMockJSONRPC()
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx := types.NewTx(&types.LegacyTx{
				Nonce:    auth.Nonce.Uint64(),
				GasPrice: auth.GasPrice,
				Gas:      auth.GasLimit,
				To:       &common.Address{0x1},
				Value:    big.NewInt(0),
				Data:     []byte{},
			})
			signedTx, err := auth.Signer(auth.From, tx)
			if err != nil {
				return nil, err
			}

			mockRPC.mu.Lock()
			mockRPC.receiptResps[signedTx.Hash()] = &types.Receipt{
				Status:      1,
				GasUsed:     21000,
				BlockNumber: big.NewInt(300),
			}
			mockRPC.mu.Unlock()

			return signedTx, errors.New("already known")
		}

		receipt, _, _, err := h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
		if err != nil {
			t.Fatalf("executeWithRetryAndResubmission failed: %v", err)
		}

		if receipt == nil || receipt.BlockNumber.Uint64() != 300 {
			t.Errorf("expected receipt at block 300, got %v", receipt)
		}
	})

	t.Run("broadcast context deadline", func(t *testing.T) {
		oldBroadcastTimeout := ethTxBroadcastTimeout
		ethTxBroadcastTimeout = 30 * time.Millisecond
		defer func() {
			ethTxBroadcastTimeout = oldBroadcastTimeout
		}()

		mockRPC := newMockJSONRPC()
		mockRPC.nonce = 10
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		var sawDeadline bool
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			if auth.Context == nil {
				return nil, errors.New("auth context is nil")
			}
			if _, ok := auth.Context.Deadline(); !ok {
				return nil, errors.New("auth context has no deadline")
			}
			sawDeadline = true

			select {
			case <-auth.Context.Done():
				return nil, auth.Context.Err()
			case <-time.After(500 * time.Millisecond):
				return nil, errors.New("auth context did not expire")
			}
		}

		start := time.Now()
		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
		if err == nil {
			t.Fatal("expected broadcast context deadline error, got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context deadline exceeded, got %v", err)
		}
		if !sawDeadline {
			t.Fatal("sender did not observe a deadline on auth.Context")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("broadcast deadline took too long: %s", elapsed)
		}

		if nonceCacheIsValid(h, "1", privKey) {
			t.Error("expected nonce cache invalidated after broadcast deadline")
		}
	})

	t.Run("wait error nonce invalidation", func(t *testing.T) {
		oldTimeout := ethTxReceiptTimeout
		oldPollInterval := ethTxReceiptPollInterval
		ethTxReceiptTimeout = 100 * time.Millisecond
		ethTxReceiptPollInterval = 10 * time.Millisecond
		defer func() {
			ethTxReceiptTimeout = oldTimeout
			ethTxReceiptPollInterval = oldPollInterval
		}()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req jsonrpcReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			var result interface{}
			var rpcErr *jsonrpcError

			switch req.Method {
			case "eth_chainId":
				result = "0x1"
			case "eth_gasPrice":
				result = "0x3b9aca00" // 1 Gwei
			case "eth_getTransactionCount":
				result = "0xa" // 10
			case "eth_sendRawTransaction":
				result = "0x0000000000000000000000000000000000000000000000000000000000000000"
			case "eth_getTransactionReceipt":
				// Fail the receipt lookup with an RPC error
				rpcErr = &jsonrpcError{
					Code:    -32000,
					Message: "node syncing / receipt unavailable due to internal error",
				}
			default:
				rpcErr = &jsonrpcError{
					Code:    -32601,
					Message: "method not found",
				}
			}

			resp := jsonrpcResp{
				Jsonrpc: "2.0",
				Id:      req.Id,
				Result:  result,
				Error:   rpcErr,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx := types.NewTx(&types.LegacyTx{
				Nonce:    auth.Nonce.Uint64(),
				GasPrice: auth.GasPrice,
				Gas:      auth.GasLimit,
				To:       &common.Address{0x1},
				Value:    big.NewInt(0),
				Data:     []byte{},
			})
			return auth.Signer(auth.From, tx)
		}

		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
		if err == nil {
			t.Fatal("expected error from executeWithRetryAndResubmission due to receipt wait failure, got nil")
		}

		if nonceCacheIsValid(h, "1", privKey) {
			t.Error("expected nonce cache to be invalid after receipt wait error")
		}
	})

	t.Run("gas floors on stuck nonce", func(t *testing.T) {
		mockRPC := newMockJSONRPC()
		mockRPC.nonce = 10
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		chainIdInt, err := client.ChainID(context.Background())
		if err != nil {
			t.Fatalf("chain ID: %v", err)
		}

		h := &Handler{}

		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID:   chainIdInt,
				Nonce:     auth.Nonce.Uint64(),
				GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap,
				Gas:       auth.GasLimit,
				To:        &common.Address{0x1},
				Value:     big.NewInt(0),
				Data:      []byte{},
			})
			return auth.Signer(auth.From, tx)
		}

		var tx1 *types.Transaction
		senderFnWrapped1 := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx, err := senderFn(auth)
			if err == nil {
				tx1 = tx
				mockRPC.mu.Lock()
				mockRPC.receiptResps[tx.Hash()] = &types.Receipt{
					Status:      1,
					GasUsed:     21000,
					BlockNumber: big.NewInt(100),
				}
				mockRPC.mu.Unlock()
			}
			return tx, err
		}

		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFnWrapped1)
		if err != nil {
			t.Fatalf("first execution failed: %v", err)
		}

		if tx1 == nil {
			t.Fatal("first transaction was not captured")
		}

		// Invalidate nonce cache manually (simulating timeout that occurred)
		h.mu.Lock()
		h.senderState("1", crypto.PubkeyToAddress(privKey.PublicKey)).nonceValid = false
		h.mu.Unlock()

		// 2. Second execution: re-submitting at same nonce
		var tx2 *types.Transaction
		senderFnWrapped2 := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx, err := senderFn(auth)
			if err == nil {
				tx2 = tx
				mockRPC.mu.Lock()
				mockRPC.receiptResps[tx.Hash()] = &types.Receipt{
					Status:      1,
					GasUsed:     21000,
					BlockNumber: big.NewInt(101),
				}
				mockRPC.mu.Unlock()
			}
			return tx, err
		}

		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFnWrapped2)
		if err != nil {
			t.Fatalf("second execution failed: %v", err)
		}

		if tx2 == nil {
			t.Fatal("second transaction was not captured")
		}

		// Verify gas floors were applied (15% bump)
		expectedTipCap := new(big.Int).Mul(tx1.GasTipCap(), big.NewInt(115))
		expectedTipCap.Div(expectedTipCap, big.NewInt(100))

		expectedFeeCap := new(big.Int).Mul(tx1.GasFeeCap(), big.NewInt(115))
		expectedFeeCap.Div(expectedFeeCap, big.NewInt(100))

		if tx2.GasTipCap().Cmp(expectedTipCap) != 0 {
			t.Errorf("expected bumped tip cap %v, got %v", expectedTipCap, tx2.GasTipCap())
		}

		if tx2.GasFeeCap().Cmp(expectedFeeCap) != 0 {
			t.Errorf("expected bumped fee cap %v, got %v", expectedFeeCap, tx2.GasFeeCap())
		}
	})

	t.Run("sender fn returns nil", func(t *testing.T) {
		mockRPC := newMockJSONRPC()
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		// senderFn returns (nil, nil)
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			return nil, nil
		}

		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		expectedErr := "senderFn returned nil transaction without error"
		if !strings.Contains(err.Error(), expectedErr) {
			t.Errorf("expected error to contain %q, got %q", expectedErr, err.Error())
		}

		if nonceCacheIsValid(h, "1", privKey) {
			t.Error("expected nonce cache to be invalid after nil transaction error")
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		mockRPC := newMockJSONRPC()
		mockRPC.nonce = 10
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		numRequests := 10
		var mu sync.Mutex
		noncesUsed := make([]uint64, 0, numRequests)
		var wg sync.WaitGroup

		for i := 0; i < numRequests; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
					// Simulate random latency to trigger concurrency races if they exist
					time.Sleep(time.Duration(10+time.Now().UnixNano()%30) * time.Millisecond)

					tx := types.NewTx(&types.LegacyTx{
						Nonce:    auth.Nonce.Uint64(),
						GasPrice: auth.GasPrice,
						Gas:      auth.GasLimit,
						To:       &common.Address{0x1},
						Value:    big.NewInt(0),
						Data:     []byte{},
					})
					signedTx, signErr := auth.Signer(auth.From, tx)
					if signErr != nil {
						return nil, signErr
					}
					mockRPC.mu.Lock()
					mockRPC.receiptResps[signedTx.Hash()] = &types.Receipt{
						Status:      1,
						GasUsed:     21000,
						BlockNumber: big.NewInt(100),
					}
					mockRPC.mu.Unlock()

					mu.Lock()
					noncesUsed = append(noncesUsed, auth.Nonce.Uint64())
					mu.Unlock()

					return signedTx, nil
				}

				_, _, _, err := h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
				if err != nil {
					t.Errorf("executeWithRetryAndResubmission failed: %v", err)
					return
				}
			}()
		}

		wg.Wait()

		if len(noncesUsed) != numRequests {
			t.Fatalf("expected %d nonces, got %d", numRequests, len(noncesUsed))
		}

		seen := make(map[uint64]bool)
		for _, nonce := range noncesUsed {
			if nonce < 10 || nonce >= 20 {
				t.Errorf("nonce %d out of bounds [10, 19]", nonce)
			}
			if seen[nonce] {
				t.Errorf("duplicate nonce used: %d", nonce)
			}
			seen[nonce] = true
		}
	})

	t.Run("revert permanent failure", func(t *testing.T) {
		mockRPC := newMockJSONRPC()
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx := types.NewTx(&types.LegacyTx{
				Nonce:    auth.Nonce.Uint64(),
				GasPrice: auth.GasPrice,
				Gas:      auth.GasLimit,
				To:       &common.Address{0x1},
				Value:    big.NewInt(0),
				Data:     []byte{},
			})
			signedTx, signErr := auth.Signer(auth.From, tx)
			if signErr != nil {
				return nil, signErr
			}
			mockRPC.mu.Lock()
			mockRPC.receiptResps[signedTx.Hash()] = &types.Receipt{
				Status:      0, // Reverted!
				GasUsed:     21000,
				BlockNumber: big.NewInt(100),
			}
			mockRPC.mu.Unlock()
			return signedTx, nil
		}

		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Errorf("expected ErrPermanentRelayFailure, got %v", err)
		}
	})

	t.Run("out of gas escalates in process", func(t *testing.T) {
		oldPollInterval := ethTxReceiptPollInterval
		ethTxReceiptPollInterval = time.Millisecond
		defer func() { ethTxReceiptPollInterval = oldPollInterval }()

		mockRPC := newMockJSONRPC()
		mockRPC.callSucceeds = true
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()
		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("dial mock RPC: %v", err)
		}
		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}

		var gasLimits, nonces []uint64
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			gasLimits = append(gasLimits, auth.GasLimit)
			nonces = append(nonces, auth.Nonce.Uint64())
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(1), Nonce: auth.Nonce.Uint64(), GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
			})
			signed, signErr := auth.Signer(auth.From, tx)
			if signErr == nil {
				status := uint64(1)
				if len(gasLimits) == 1 {
					status = 0
				}
				mockRPC.mu.Lock()
				mockRPC.receiptResps[signed.Hash()] = &types.Receipt{Status: status, GasUsed: auth.GasLimit, BlockNumber: big.NewInt(1)}
				mockRPC.mu.Unlock()
			}
			return signed, signErr
		}

		receipt, _, _, err := (&Handler{}).executeWithRetryAndResubmission(
			context.Background(), services.EVMEndpoint{Client: client}, privKey, 100_000, senderFn, knobEthGasLimit,
		)
		if err != nil {
			t.Fatalf("execute after OOG: %v", err)
		}
		if receipt == nil || receipt.Status != 1 {
			t.Fatalf("final receipt = %v, want success", receipt)
		}
		if len(gasLimits) != 2 || gasLimits[1] <= gasLimits[0] {
			t.Fatalf("gas limits = %v, want one larger in-process retry", gasLimits)
		}

		// Larger is not enough: it has to be the rung the ladder named. The call site
		// computes next.To and then hands the callee baseGasLimit plus the step,
		// which re-derives the same quantity -- so next.To reaches the log line and
		// nothing else. Unpinned, the two derivations drift and "retrying with gas
		// limit %d" reports a limit no transaction ever carried.
		rung, ok := EVMGasLadder(100_000)(0)
		if !ok {
			t.Fatal("the gas ladder reports no first rung, so the escalation above came from somewhere else")
		}
		if gasLimits[1] != rung.To {
			t.Fatalf("the ladder named gas limit %d but the retry submitted %d", rung.To, gasLimits[1])
		}
		if len(nonces) != 2 || nonces[1] != nonces[0]+1 {
			t.Fatalf("nonces = %v, want included OOG to consume one nonce", nonces)
		}
	})

	t.Run("out of gas ladder exhaustion is permanent", func(t *testing.T) {
		oldPollInterval := ethTxReceiptPollInterval
		ethTxReceiptPollInterval = time.Millisecond
		defer func() { ethTxReceiptPollInterval = oldPollInterval }()

		mockRPC := newMockJSONRPC()
		mockRPC.callSucceeds = true
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()
		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("dial mock RPC: %v", err)
		}
		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}

		var gasLimits []uint64
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			gasLimits = append(gasLimits, auth.GasLimit)
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(1), Nonce: auth.Nonce.Uint64(), GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
			})
			signed, signErr := auth.Signer(auth.From, tx)
			if signErr == nil {
				mockRPC.mu.Lock()
				mockRPC.receiptResps[signed.Hash()] = &types.Receipt{Status: 0, GasUsed: auth.GasLimit, BlockNumber: big.NewInt(1)}
				mockRPC.mu.Unlock()
			}
			return signed, signErr
		}

		_, _, _, err = (&Handler{}).executeWithRetryAndResubmission(
			context.Background(), services.EVMEndpoint{Client: client}, privKey, 100_000, senderFn, knobEthGasLimit,
		)
		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Fatalf("exhausted OOG ladder error = %v, want permanent relay failure", err)
		}
		if len(gasLimits) != len(evmGasHeadroomBasisPoints) {
			t.Fatalf("gas attempts = %v, want finite ladder length %d", gasLimits, len(evmGasHeadroomBasisPoints))
		}
		for i := 1; i < len(gasLimits); i++ {
			if gasLimits[i] <= gasLimits[i-1] {
				t.Fatalf("gas attempts = %v, want strictly increasing limits", gasLimits)
			}
		}
	})

	t.Run("out of gas block ceiling is permanent", func(t *testing.T) {
		oldPollInterval := ethTxReceiptPollInterval
		ethTxReceiptPollInterval = time.Millisecond
		defer func() { ethTxReceiptPollInterval = oldPollInterval }()

		mockRPC := newMockJSONRPC()
		mockRPC.callSucceeds = true
		mockRPC.blockGas = 120_000
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()
		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("dial mock RPC: %v", err)
		}
		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}

		var gasLimits []uint64
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			gasLimits = append(gasLimits, auth.GasLimit)
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(1), Nonce: auth.Nonce.Uint64(), GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
			})
			signed, signErr := auth.Signer(auth.From, tx)
			if signErr == nil {
				mockRPC.mu.Lock()
				mockRPC.receiptResps[signed.Hash()] = &types.Receipt{Status: 0, GasUsed: auth.GasLimit, BlockNumber: big.NewInt(1)}
				mockRPC.mu.Unlock()
			}
			return signed, signErr
		}

		_, _, _, err = (&Handler{}).executeWithRetryAndResubmission(
			context.Background(), services.EVMEndpoint{Client: client}, privKey, 100_000, senderFn, knobEthGasLimit,
		)
		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Fatalf("block-ceiling OOG error = %v, want permanent relay failure", err)
		}
		if !strings.Contains(err.Error(), "block gas ceiling") {
			t.Fatalf("block-ceiling OOG error = %v, want the block-ceiling termination reason", err)
		}
		if want := []uint64{100_000, 120_000}; !slices.Equal(gasLimits, want) {
			t.Fatalf("gas attempts = %v, want %v without re-broadcasting the clamped limit", gasLimits, want)
		}
	})

	t.Run("refreshes block gas ceiling", func(t *testing.T) {
		oldPollInterval := ethTxReceiptPollInterval
		ethTxReceiptPollInterval = time.Millisecond
		defer func() { ethTxReceiptPollInterval = oldPollInterval }()

		mockRPC := newMockJSONRPC()
		mockRPC.callSucceeds = true
		mockRPC.blockGases["0x1"] = 120_000
		mockRPC.blockGases["0x2"] = 150_000
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()
		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("dial mock RPC: %v", err)
		}
		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}

		var gasLimits []uint64
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			gasLimits = append(gasLimits, auth.GasLimit)
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(1), Nonce: auth.Nonce.Uint64(), GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
			})
			signed, signErr := auth.Signer(auth.From, tx)
			if signErr == nil {
				status := uint64(0)
				if len(gasLimits) == 3 {
					status = 1
				}
				mockRPC.mu.Lock()
				mockRPC.receiptResps[signed.Hash()] = &types.Receipt{
					Status: status, GasUsed: auth.GasLimit, BlockNumber: big.NewInt(int64(len(gasLimits))),
				}
				mockRPC.mu.Unlock()
			}
			return signed, signErr
		}

		receipt, _, _, err := (&Handler{}).executeWithRetryAndResubmission(
			context.Background(), services.EVMEndpoint{Client: client}, privKey, 100_000, senderFn, knobEthGasLimit,
		)
		if err != nil {
			t.Fatalf("execute after block gas ceiling increased: %v", err)
		}
		if receipt == nil || receipt.Status != 1 {
			t.Fatalf("final receipt = %v, want success", receipt)
		}
		if want := []uint64{100_000, 120_000, 150_000}; !slices.Equal(gasLimits, want) {
			t.Fatalf("gas attempts = %v, want refreshed ceilings %v", gasLimits, want)
		}
	})

	t.Run("block ceiling wins at final ladder step", func(t *testing.T) {
		oldPollInterval := ethTxReceiptPollInterval
		ethTxReceiptPollInterval = time.Millisecond
		defer func() { ethTxReceiptPollInterval = oldPollInterval }()

		mockRPC := newMockJSONRPC()
		mockRPC.callSucceeds = true
		mockRPC.blockGas = 300_000
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()
		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("dial mock RPC: %v", err)
		}
		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}

		var gasLimits []uint64
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			gasLimits = append(gasLimits, auth.GasLimit)
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(1), Nonce: auth.Nonce.Uint64(), GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
			})
			signed, signErr := auth.Signer(auth.From, tx)
			if signErr == nil {
				mockRPC.mu.Lock()
				mockRPC.receiptResps[signed.Hash()] = &types.Receipt{Status: 0, GasUsed: auth.GasLimit, BlockNumber: big.NewInt(1)}
				mockRPC.mu.Unlock()
			}
			return signed, signErr
		}

		finalStep := len(evmGasHeadroomBasisPoints) - 1
		_, _, _, err = (&Handler{}).executeWithRetryAndResubmissionAtGasStep(
			context.Background(), services.EVMEndpoint{Client: client}, privKey,
			100_000, finalStep, 300_000, senderFn, knobEthGasLimit,
		)
		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Fatalf("final-step block-ceiling OOG error = %v, want permanent relay failure", err)
		}
		if !strings.Contains(err.Error(), "block gas ceiling") {
			t.Fatalf("final-step block-ceiling OOG error = %v, want ceiling to win over ladder exhaustion", err)
		}
		if want := []uint64{300_000}; !slices.Equal(gasLimits, want) {
			t.Fatalf("gas attempts = %v, want %v without another final-step broadcast", gasLimits, want)
		}
	})

	t.Run("transient wait failure", func(t *testing.T) {
		oldTimeout := ethTxReceiptTimeout
		oldPollInterval := ethTxReceiptPollInterval
		ethTxReceiptTimeout = 100 * time.Millisecond
		ethTxReceiptPollInterval = 10 * time.Millisecond
		defer func() {
			ethTxReceiptTimeout = oldTimeout
			ethTxReceiptPollInterval = oldPollInterval
		}()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req jsonrpcReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			var result interface{}
			var rpcErr *jsonrpcError

			switch req.Method {
			case "eth_chainId":
				result = "0x1"
			case "eth_gasPrice":
				result = "0x3b9aca00" // 1 Gwei
			case "eth_getTransactionCount":
				result = "0xa" // 10
			case "eth_sendRawTransaction":
				result = "0x0000000000000000000000000000000000000000000000000000000000000000"
			case "eth_getTransactionReceipt":
				// Fail the receipt lookup with an RPC error (transient error)
				rpcErr = &jsonrpcError{
					Code:    -32000,
					Message: "node syncing / receipt unavailable due to internal error",
				}
			default:
				rpcErr = &jsonrpcError{
					Code:    -32601,
					Message: "method not found",
				}
			}

			resp := jsonrpcResp{
				Jsonrpc: "2.0",
				Id:      req.Id,
				Result:  result,
				Error:   rpcErr,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx := types.NewTx(&types.LegacyTx{
				Nonce:    auth.Nonce.Uint64(),
				GasPrice: auth.GasPrice,
				Gas:      auth.GasLimit,
				To:       &common.Address{0x1},
				Value:    big.NewInt(0),
				Data:     []byte{},
			})
			return auth.Signer(auth.From, tx)
		}

		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		// This used to assert the opposite -- that giving up on the wait leaves a
		// RETRYABLE failure -- and that assertion is the bug @DongLieu reported on
		// #485. The transaction the relayer stopped waiting for does not stop
		// existing: the cancellation reaches one node's pool, which is acceptance and
		// not inclusion, so the original may still mine. Retrying the payload at a
		// fresh nonce while that is true runs the action twice.
		//
		// So the ending is unresolved, and unresolved is not retryable.
		if !errors.Is(err, services.ErrUnresolvedEVMNonce) {
			t.Errorf("error = %v, want it to carry ErrUnresolvedEVMNonce: the abandoned nonce is undecided", err)
		}
		if !errors.Is(err, services.ErrPermanentRelayFailure) {
			t.Errorf("error = %v, want permanent: nothing derived from this attempt may be sent again", err)
		}
	})

	t.Run("suggest gas price error nonce invalidation", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req jsonrpcReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			var result interface{}
			var rpcErr *jsonrpcError

			switch req.Method {
			case "eth_chainId":
				result = "0x1"
			case "eth_getTransactionCount":
				result = "0xa" // 10
			case "eth_gasPrice":
				// Fail gas price suggestion
				rpcErr = &jsonrpcError{
					Code:    -32000,
					Message: "cannot suggest gas price",
				}
			default:
				rpcErr = &jsonrpcError{
					Code:    -32601,
					Message: "method not found",
				}
			}

			resp := jsonrpcResp{
				Jsonrpc: "2.0",
				Id:      req.Id,
				Result:  result,
				Error:   rpcErr,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer srv.Close()

		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("failed to dial: %v", err)
		}

		ctx := services.EVMEndpoint{Client: client}

		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key: %v", err)
		}

		h := &Handler{}

		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			tx := types.NewTx(&types.LegacyTx{
				Nonce:    auth.Nonce.Uint64(),
				GasPrice: auth.GasPrice,
				Gas:      auth.GasLimit,
				To:       &common.Address{0x1},
				Value:    big.NewInt(0),
				Data:     []byte{},
			})
			return auth.Signer(auth.From, tx)
		}

		_, _, _, err = h.executeWithRetryAndResubmission(context.Background(), ctx, privKey, 100000, senderFn)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if nonceCacheIsValid(h, "1", privKey) {
			t.Error("expected nonce cache to be invalid after SuggestGasPrice failure")
		}
	})

	// TestExecuteWithRetryAndResubmission/stuck future nonce stops bumping is the #320
	// follow-up: a transaction sent at a nonce the account has not reached cannot mine
	// no matter how much gas it carries, so the wait loop must stop instead of spending
	// its whole bump budget resubmitting it.
	//
	// The setup is the collision the issue describes, minus the cause: the cache is
	// pre-seeded high (as a poisoned cross-chain cache used to leave it), while the
	// chain still needs a much lower nonce. The mock never returns a receipt, so the
	// first wait times out and the pending-nonce probe sees the gap.
	t.Run("stuck future nonce stops bumping", func(t *testing.T) {
		oldTimeout := ethTxReceiptTimeout
		oldPoll := ethTxReceiptPollInterval
		ethTxReceiptTimeout = 60 * time.Millisecond
		ethTxReceiptPollInterval = 10 * time.Millisecond
		defer func() {
			ethTxReceiptTimeout = oldTimeout
			ethTxReceiptPollInterval = oldPoll
		}()

		mockRPC := newMockJSONRPC()
		mockRPC.nonce = 7 // what the chain actually needs
		srv := httptest.NewServer(mockRPC)
		defer srv.Close()
		client, err := ethclient.Dial(srv.URL)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		privKey, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		from := crypto.PubkeyToAddress(privKey.PublicKey)

		h := &Handler{}
		h.mu.Lock()
		state := h.senderState("1", from)
		state.nonce = 50 // as a cross-chain collision used to leave it
		state.nonceValid = true
		h.mu.Unlock()

		var sent []uint64
		senderFn := func(auth *bind.TransactOpts) (*types.Transaction, error) {
			sent = append(sent, auth.Nonce.Uint64())
			tx := types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(1), Nonce: auth.Nonce.Uint64(), GasTipCap: auth.GasTipCap,
				GasFeeCap: auth.GasFeeCap, Gas: auth.GasLimit, To: &common.Address{0x1},
			})
			return auth.Signer(auth.From, tx)
		}

		_, _, _, err = h.executeWithRetryAndResubmission(
			context.Background(), services.EVMEndpoint{Client: client}, privKey, 100_000, senderFn,
		)
		if err == nil {
			t.Fatal("execute succeeded; want a failure naming the nonce gap")
		}
		if !strings.Contains(err.Error(), "cannot mine") {
			t.Fatalf("error = %v; want the nonce-gap diagnosis, not a generic wait timeout", err)
		}

		// One submission, not maxAttempts of them: the bump budget must not be spent.
		if len(sent) != 1 {
			t.Fatalf("submissions = %v, want exactly one before giving up", sent)
		}

		// The poisoned entry must be invalidated so the retry re-queries the chain.
		h.mu.Lock()
		stillValid := h.senderState("1", from).nonceValid
		h.mu.Unlock()
		if stillValid {
			t.Fatal("cached nonce still valid; the next attempt would reuse the unminable nonce")
		}
	})
}

// These drive the top-level ETH send functions end to end against a stub node.
// The layer below them was already covered -- handler_test.go says so at its
// calldata-packing tests -- but SendEthTx and SendEthTxBatch themselves were
// never called by any test, so their guards, their single-vs-batch split and
// their nonce behaviour were unverified.

type testDataError struct {
	data interface{}
}

func (e testDataError) Error() string {
	return "execution reverted"
}

func (e testDataError) ErrorData() interface{} {
	return e.data
}

// These tests cover the calldata packing layer that SendEthTxBatch relies on.
// We do not exercise the RPC submission path here — that requires a full
// endpoint dependency + ethclient mock and is validated end-to-end via the
// benchmark run described in /Users/ducnt/.claude/plans/...
//
// Selector reference (from `cast sig` against the ICS26Router ABI):
//   recvPacket(...)              → 0x596e00b9
//   ackPacket(...)               → 0xfdbd955d
//   timeoutPacket(...)           → 0x223e357a
//   updateApplicationState(string,bytes) → 0x9c11bece
//   updateConsensusState(string,bytes)   → 0xd395af7f
//   multicall(bytes[])                   → 0xac9650d8

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func samplePacket() contractICS26Router.IICS26RouterMsgsPacket {
	return contractICS26Router.IICS26RouterMsgsPacket{
		Sequence:         42,
		SourceClient:     "cosmoshub-1",
		DestClient:       "08-wasm-0",
		TimeoutTimestamp: 1_700_000_000,
		Payloads: []contractICS26Router.IICS26RouterMsgsPayload{{
			SourcePort: "transfer",
			DestPort:   "transfer",
			Version:    "ics20-2",
			Encoding:   "abi",
			Value:      []byte{0x01, 0x02, 0x03},
		}},
	}
}

func TestSelectorsForBatchedMsgs(t *testing.T) {
	parsedABI, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("GetAbi: %v", err)
	}
	pkt := samplePacket()

	cases := []struct {
		name     string
		msg      any
		method   string
		selector string
	}{
		{
			name: "recvPacket",
			msg: contractICS26Router.IICS26RouterMsgsMsgRecvPacket{
				Packet:        pkt,
				MembershipMsg: []byte{0xaa, 0xbb},
			},
			method:   "recvPacket",
			selector: "596e00b9",
		},
		{
			name: "ackPacket",
			msg: contractICS26Router.IICS26RouterMsgsMsgAckPacket{
				Packet:          pkt,
				Acknowledgement: []byte{0xde, 0xad},
				MembershipMsg:   []byte{0xbe, 0xef},
			},
			method:   "ackPacket",
			selector: "fdbd955d",
		},
		{
			name: "timeoutPacket",
			msg: contractICS26Router.IICS26RouterMsgsMsgTimeoutPacket{
				Packet:           pkt,
				NonMembershipMsg: []byte{0xca, 0xfe},
			},
			method:   "timeoutPacket",
			selector: "223e357a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := parsedABI.Pack(tc.method, tc.msg)
			if err != nil {
				t.Fatalf("pack: %v", err)
			}
			if len(data) < 4 {
				t.Fatalf("calldata too short: %d", len(data))
			}
			gotSelector := hex.EncodeToString(data[:4])
			if gotSelector != tc.selector {
				t.Fatalf("selector mismatch: got %s, want %s", gotSelector, tc.selector)
			}
		})
	}
}

func TestValidatorCacheRaceErrorName(t *testing.T) {
	selector := errorSelector("ValidatorSetCacheMiss(bytes32)")
	data := append(selector[:], make([]byte, 32)...)
	name, ok := validatorCacheRaceErrorName(testDataError{data: "0x" + hex.EncodeToString(data)})
	if !ok {
		t.Fatal("expected ValidatorSetCacheMiss to be classified as a cache race")
	}
	if name != "ValidatorSetCacheMiss" {
		t.Fatalf("error name: got %q want ValidatorSetCacheMiss", name)
	}

	unknown := errorSelector("ProofVerificationFailed()")
	if name, ok := validatorCacheRaceErrorName(testDataError{data: unknown[:]}); ok {
		t.Fatalf("did not expect unknown selector to be classified, got %q", name)
	}
}

// TestClientNotActiveErrorName pins #402: the replayed revert of a call into a
// frozen SpectreClient is recognised, so it is held rather than dropped.
func TestClientNotActiveErrorName(t *testing.T) {
	selector := errorSelector("FrozenClientState()")
	name, ok := clientNotActiveErrorName(testDataError{data: "0x" + hex.EncodeToString(selector[:])})
	if !ok || name != "FrozenClientState" {
		t.Fatalf("FrozenClientState not recognised: name=%q ok=%v", name, ok)
	}
	cacheMiss := errorSelector("ValidatorSetCacheMiss(bytes32)")
	if _, ok := clientNotActiveErrorName(testDataError{data: "0x" + hex.EncodeToString(append(cacheMiss[:], make([]byte, 32)...))}); ok {
		t.Fatal("a cache race is not an inactive client")
	}
	if _, ok := validatorCacheRaceErrorName(testDataError{data: selector[:]}); ok {
		t.Fatal("a frozen client is not a cache race")
	}
}

// TestMulticall_Wraps verifies that wrapping per-msg calldata into a multicall
// produces the expected outer selector + that each inner blob is recoverable
// after Unpack. This is the round-trip that on-chain MulticallUpgradeable
// would do.
func TestMulticall_Wraps(t *testing.T) {
	parsedABI, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("GetAbi: %v", err)
	}
	pkt := samplePacket()

	recv, err := parsedABI.Pack("recvPacket", contractICS26Router.IICS26RouterMsgsMsgRecvPacket{
		Packet:        pkt,
		MembershipMsg: []byte{0x01},
	})
	if err != nil {
		t.Fatalf("pack recv: %v", err)
	}
	ack, err := parsedABI.Pack("ackPacket", contractICS26Router.IICS26RouterMsgsMsgAckPacket{
		Packet:          pkt,
		Acknowledgement: []byte{0x02},
		MembershipMsg:   []byte{0x03},
	})
	if err != nil {
		t.Fatalf("pack ack: %v", err)
	}

	innerCalldata := [][]byte{recv, ack}
	outer, err := parsedABI.Pack("multicall", innerCalldata)
	if err != nil {
		t.Fatalf("pack multicall: %v", err)
	}
	wantSelector := mustDecodeHex(t, "ac9650d8")
	if !bytes.Equal(outer[:4], wantSelector) {
		t.Fatalf("multicall selector mismatch: got %x, want %x", outer[:4], wantSelector)
	}

	// Decode the outer multicall payload and verify we recover the same two
	// inner calldata blobs in order.
	method, err := parsedABI.MethodById(outer[:4])
	if err != nil {
		t.Fatalf("MethodById: %v", err)
	}
	if method.Name != "multicall" {
		t.Fatalf("unexpected method: %s", method.Name)
	}
	unpacked, err := method.Inputs.Unpack(outer[4:])
	if err != nil {
		t.Fatalf("unpack inputs: %v", err)
	}
	if len(unpacked) != 1 {
		t.Fatalf("expected 1 top-level arg, got %d", len(unpacked))
	}
	gotInner, ok := unpacked[0].([][]byte)
	if !ok {
		t.Fatalf("unexpected inner type: %T", unpacked[0])
	}
	if len(gotInner) != 2 {
		t.Fatalf("expected 2 inner calls, got %d", len(gotInner))
	}
	if !bytes.Equal(gotInner[0], recv) {
		t.Fatalf("inner[0] mismatch")
	}
	if !bytes.Equal(gotInner[1], ack) {
		t.Fatalf("inner[1] mismatch")
	}
}

// TestSelectorForUpdateApplicationState pins the selectors for the split
// ICS26Router update entry points, which the cosmos→eth path batches into the
// same multicall as recvPacket/ackPacket/timeoutPacket.
func TestSelectorForUpdateApplicationState(t *testing.T) {
	parsedABI, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("GetAbi: %v", err)
	}
	for _, tc := range []struct{ method, want string }{
		{"updateApplicationState", "9c11bece"},
		{"updateConsensusState", "d395af7f"},
	} {
		data, err := parsedABI.Pack(tc.method, "cosmoshub-1", []byte{0xde, 0xad, 0xbe, 0xef})
		if err != nil {
			t.Fatalf("pack %s: %v", tc.method, err)
		}
		if len(data) < 4 {
			t.Fatalf("%s calldata too short: %d", tc.method, len(data))
		}
		if got := hex.EncodeToString(data[:4]); got != tc.want {
			t.Fatalf("%s selector mismatch: got %s, want %s", tc.method, got, tc.want)
		}
	}
}

// TestMulticall_WithUpdateApplicationState verifies that an updateApplicationState
// + packet combo produces a valid multicall payload — the layout the cosmos→eth
// path folds so the packet proof verifies against the just-applied client state.
func TestMulticall_WithUpdateApplicationState(t *testing.T) {
	parsedABI, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("GetAbi: %v", err)
	}
	pkt := samplePacket()

	updateData, err := parsedABI.Pack("updateApplicationState", "cosmoshub-1", []byte{0x01, 0x02})
	if err != nil {
		t.Fatalf("pack updateApplicationState: %v", err)
	}
	recvData, err := parsedABI.Pack("recvPacket", contractICS26Router.IICS26RouterMsgsMsgRecvPacket{
		Packet:        pkt,
		MembershipMsg: []byte{0x03},
	})
	if err != nil {
		t.Fatalf("pack recvPacket: %v", err)
	}

	outer, err := parsedABI.Pack("multicall", [][]byte{updateData, recvData})
	if err != nil {
		t.Fatalf("pack multicall: %v", err)
	}
	if !bytes.Equal(outer[:4], mustDecodeHex(t, "ac9650d8")) {
		t.Fatalf("multicall selector mismatch: got %x", outer[:4])
	}

	method, err := parsedABI.MethodById(outer[:4])
	if err != nil {
		t.Fatalf("MethodById: %v", err)
	}
	unpacked, err := method.Inputs.Unpack(outer[4:])
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	inner := unpacked[0].([][]byte)
	if len(inner) != 2 {
		t.Fatalf("expected 2 inner calls, got %d", len(inner))
	}
	// First inner must be updateApplicationState — atomicity demands this so
	// packet proofs verify against the just-applied client state.
	if !bytes.Equal(inner[0][:4], mustDecodeHex(t, "9c11bece")) {
		t.Fatalf("inner[0] is not updateApplicationState: %x", inner[0][:4])
	}
	if !bytes.Equal(inner[1][:4], mustDecodeHex(t, "596e00b9")) {
		t.Fatalf("inner[1] is not recvPacket: %x", inner[1][:4])
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		err     error
		isNonce bool
		isKnown bool
	}{
		{errors.New("nonce too low"), true, false},
		{errors.New("old nonce"), true, false},
		{errors.New("nonce has already been used"), true, false},
		{errors.New("already known"), false, true},
		{errors.New("transaction already imported"), false, true},
		{errors.New("already exists"), false, true},
		{errors.New("execution reverted"), false, false},
		{nil, false, false},
	}
	for _, tc := range cases {
		if got := isNonceTooLowError(tc.err); got != tc.isNonce {
			t.Errorf("isNonceTooLowError(%v) = %v; want %v", tc.err, got, tc.isNonce)
		}
		if got := isAlreadyKnownError(tc.err); got != tc.isKnown {
			t.Errorf("isAlreadyKnownError(%v) = %v; want %v", tc.err, got, tc.isKnown)
		}
	}
}

type jsonrpcReq struct {
	Jsonrpc string            `json:"jsonrpc"`
	Id      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type jsonrpcResp struct {
	Jsonrpc string          `json:"jsonrpc"`
	Id      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type mockJSONRPC struct {
	mu           sync.Mutex
	receiptCalls []common.Hash
	receiptResps map[common.Hash]*types.Receipt
	sendErr      error
	chainID      uint64
	nonce        uint64
	nonces       map[common.Address]uint64
	nonceCalls   map[common.Address]int
	gasPrice     *big.Int
	blockGas     uint64
	blockGases   map[string]uint64
	callSucceeds bool

	// sentTxs are the decoded transactions passed to eth_sendRawTransaction, in
	// order. A test that asserts what was broadcast -- not merely that something
	// was -- needs the transaction itself, so the raw payload is decoded here
	// rather than kept as hex.
	sentTxs []*types.Transaction
	// sendErrFor rejects individual transactions, which sendErr (all of them)
	// cannot express: cancelling an abandoned transaction succeeds or fails on
	// its own, while the payload before it was accepted.
	sendErrFor func(*types.Transaction) error
}

// sentTransactions snapshots what the node was asked to broadcast.
func (m *mockJSONRPC) sentTransactions() []*types.Transaction {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*types.Transaction(nil), m.sentTxs...)
}

func newMockJSONRPC() *mockJSONRPC {
	return &mockJSONRPC{
		receiptResps: make(map[common.Hash]*types.Receipt),
		chainID:      1,
		nonces:       make(map[common.Address]uint64),
		nonceCalls:   make(map[common.Address]int),
		gasPrice:     big.NewInt(1000000000), // 1 Gwei
		blockGases:   make(map[string]uint64),
	}
}

func nonceCacheIsValid(h *Handler, chainID string, privateKey *ecdsa.PrivateKey) bool {
	from := crypto.PubkeyToAddress(privateKey.PublicKey)
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.senderStates[evmNonceKey{chainID: chainID, address: from}]
	return state != nil && state.nonceValid
}

func (m *mockJSONRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req jsonrpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var result interface{}
	var rpcErr *jsonrpcError

	switch req.Method {
	case "eth_chainId":
		result = fmt.Sprintf("0x%x", m.chainID)
	case "eth_gasPrice":
		result = fmt.Sprintf("0x%x", m.gasPrice)
	case "eth_maxPriorityFeePerGas":
		result = fmt.Sprintf("0x%x", m.gasPrice)
	case "eth_getTransactionCount":
		var address common.Address
		if len(req.Params) > 0 {
			var rawAddress string
			if err := json.Unmarshal(req.Params[0], &rawAddress); err == nil {
				address = common.HexToAddress(rawAddress)
			}
		}
		m.nonceCalls[address]++
		nonce := m.nonce
		if configured, ok := m.nonces[address]; ok {
			nonce = configured
		}
		result = fmt.Sprintf("0x%x", nonce)
	case "eth_sendRawTransaction":
		sendErr := m.sendErr
		if len(req.Params) > 0 {
			var raw string
			if err := json.Unmarshal(req.Params[0], &raw); err == nil {
				decoded := new(types.Transaction)
				if err := decoded.UnmarshalBinary(common.FromHex(raw)); err == nil {
					m.sentTxs = append(m.sentTxs, decoded)
					if sendErr == nil && m.sendErrFor != nil {
						sendErr = m.sendErrFor(decoded)
					}
				}
			}
		}
		if sendErr != nil {
			rpcErr = &jsonrpcError{
				Code:    -32000,
				Message: sendErr.Error(),
			}
		} else {
			result = "0x0000000000000000000000000000000000000000000000000000000000000000"
		}
	case "eth_getTransactionReceipt":
		if len(req.Params) > 0 {
			var hashStr string
			if err := json.Unmarshal(req.Params[0], &hashStr); err == nil {
				hash := common.HexToHash(hashStr)
				m.receiptCalls = append(m.receiptCalls, hash)
				if receipt, ok := m.receiptResps[hash]; ok && receipt != nil {
					result = map[string]interface{}{
						"transactionHash":   hash.Hex(),
						"transactionIndex":  "0x1",
						"blockHash":         "0x0000000000000000000000000000000000000000000000000000000000000001",
						"blockNumber":       fmt.Sprintf("0x%x", receipt.BlockNumber.Uint64()),
						"gasUsed":           fmt.Sprintf("0x%x", receipt.GasUsed),
						"cumulativeGasUsed": fmt.Sprintf("0x%x", receipt.GasUsed),
						"status":            fmt.Sprintf("0x%x", receipt.Status),
						"logsBloom":         "0x" + strings.Repeat("0", 512),
						"logs":              []interface{}{},
					}
				} else {
					result = nil
				}
			}
		}
	case "eth_getBlockByNumber":
		blockGas := m.blockGas
		if len(req.Params) > 0 {
			var blockTag string
			if err := json.Unmarshal(req.Params[0], &blockTag); err == nil {
				if configured, ok := m.blockGases[blockTag]; ok {
					blockGas = configured
				}
			}
		}
		if blockGas == 0 {
			blockGas = 0xffffff
		}
		result = map[string]interface{}{
			"parentHash":       "0x0000000000000000000000000000000000000000000000000000000000000000",
			"sha3Uncles":       "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
			"miner":            "0x0000000000000000000000000000000000000000",
			"stateRoot":        "0x0000000000000000000000000000000000000000000000000000000000000000",
			"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
			"receiptsRoot":     "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
			"logsBloom":        "0x" + strings.Repeat("0", 512),
			"difficulty":       "0x0",
			"number":           "0x1",
			"gasLimit":         fmt.Sprintf("0x%x", blockGas),
			"gasUsed":          "0x0",
			"timestamp":        "0x0",
			"extraData":        "0x",
			"mixHash":          "0x0000000000000000000000000000000000000000000000000000000000000000",
			"nonce":            "0x0000000000000000",
			"baseFeePerGas":    fmt.Sprintf("0x%x", m.gasPrice),
		}
	case "eth_call":
		if m.callSucceeds {
			result = "0x"
		} else {
			rpcErr = &jsonrpcError{Code: -32601, Message: "method not found"}
		}
	default:
		rpcErr = &jsonrpcError{
			Code:    -32601,
			Message: "method not found",
		}
	}

	resp := jsonrpcResp{
		Jsonrpc: "2.0",
		Id:      req.Id,
		Result:  result,
		Error:   rpcErr,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func TestWaitForReceipts_ReverseOrder(t *testing.T) {
	mockRPC := newMockJSONRPC()
	srv := httptest.NewServer(mockRPC)
	defer srv.Close()

	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	hash1 := common.HexToHash("0x1")
	hash2 := common.HexToHash("0x2")
	hash3 := common.HexToHash("0x3")

	mockRPC.receiptResps[hash3] = &types.Receipt{
		Status:      1,
		GasUsed:     21000,
		BlockNumber: big.NewInt(100),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	receipt, err := waitForReceipts(ctx, client, []common.Hash{hash1, hash2, hash3})
	if err != nil {
		t.Fatalf("waitForReceipts failed: %v", err)
	}

	if receipt == nil || receipt.BlockNumber.Uint64() != 100 {
		t.Errorf("expected receipt at block 100, got %v", receipt)
	}

	mockRPC.mu.Lock()
	defer mockRPC.mu.Unlock()
	if len(mockRPC.receiptCalls) != 1 {
		t.Errorf("expected exactly 1 call to TransactionReceipt, got %d: %v", len(mockRPC.receiptCalls), mockRPC.receiptCalls)
	} else if mockRPC.receiptCalls[0] != hash3 {
		t.Errorf("expected first call to be for %v, got %v", hash3, mockRPC.receiptCalls[0])
	}
}

func TestWaitForReceipts_ErrorPropagation(t *testing.T) {
	oldTimeout := ethTxReceiptTimeout
	oldPollInterval := ethTxReceiptPollInterval
	ethTxReceiptTimeout = 100 * time.Millisecond
	ethTxReceiptPollInterval = 10 * time.Millisecond
	defer func() {
		ethTxReceiptTimeout = oldTimeout
		ethTxReceiptPollInterval = oldPollInterval
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = waitForReceipts(ctx, client, []common.Hash{common.HexToHash("0x1")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded error (since transient errors are ignored), got %v", err)
	}
}

func TestMisbehaviourGasLimit(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    uint64
		wantErr bool
	}{
		{name: "default", want: defaultMisbehaviourGasLimit},
		{name: "override", value: "17000000", want: 17_000_000},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "trailing characters", value: "17000000oops", wantErr: true},
		{name: "overflow", value: "18446744073709551616", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ETH_MISBEHAVIOUR_GAS_LIMIT", tt.value)
			got, err := MisbehaviourGasLimit()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("MisbehaviourGasLimit() = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MisbehaviourGasLimit(): %v", err)
			}
			if got != tt.want {
				t.Fatalf("MisbehaviourGasLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestValidateMisbehaviourPrivateKey(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "invalid", value: "invalid", wantErr: true},
		{name: "valid", value: strings.Repeat("1", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MISBEHAVIOUR_PRIVATE_KEY", tt.value)
			err := ValidateMisbehaviourPrivateKey()
			if tt.wantErr && err == nil {
				t.Fatal("ValidateMisbehaviourPrivateKey() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateMisbehaviourPrivateKey(): %v", err)
			}
		})
	}
}

func TestBumpGasAndResubmit(t *testing.T) {
	mockRPC := newMockJSONRPC()
	srv := httptest.NewServer(mockRPC)
	defer srv.Close()

	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	ctx := services.EVMEndpoint{Client: client}

	h := &Handler{}

	privKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	chainIdInt, err := client.ChainID(context.Background())
	if err != nil {
		t.Fatalf("chain ID: %v", err)
	}
	auth, err := bind.NewKeyedTransactorWithChainID(privKey, chainIdInt)
	if err != nil {
		t.Fatalf("transactor: %v", err)
	}

	// 1. Legacy Transaction
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    0,
		GasPrice: big.NewInt(1000000000), // 1 Gwei
		Gas:      21000,
		To:       &common.Address{0x1},
		Value:    big.NewInt(0),
		Data:     []byte{},
	})

	bumpedTx, err := h.bumpGasAndResubmit(context.Background(), ctx, tx, auth, 1)
	if err != nil {
		t.Fatalf("bumpGasAndResubmit failed: %v", err)
	}

	expectedGasPrice := big.NewInt(1150000000)
	if bumpedTx.GasPrice().Cmp(expectedGasPrice) != 0 {
		t.Errorf("expected gas price %v, got %v", expectedGasPrice, bumpedTx.GasPrice())
	}

	// 2. Dynamic Fee Transaction
	dynamicTx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainIdInt,
		Nonce:     0,
		GasTipCap: big.NewInt(1000000000), // 1 Gwei
		GasFeeCap: big.NewInt(2000000000), // 2 Gwei
		Gas:       21000,
		To:        &common.Address{0x1},
		Value:     big.NewInt(0),
		Data:      []byte{},
	})

	bumpedDynamicTx, err := h.bumpGasAndResubmit(context.Background(), ctx, dynamicTx, auth, 1)
	if err != nil {
		t.Fatalf("bumpGasAndResubmit (dynamic) failed: %v", err)
	}

	expectedTipCap := big.NewInt(1150000000)
	expectedFeeCap := big.NewInt(2300000000)
	if bumpedDynamicTx.GasTipCap().Cmp(expectedTipCap) != 0 {
		t.Errorf("expected tip cap %v, got %v", expectedTipCap, bumpedDynamicTx.GasTipCap())
	}
	if bumpedDynamicTx.GasFeeCap().Cmp(expectedFeeCap) != 0 {
		t.Errorf("expected fee cap %v, got %v", expectedFeeCap, bumpedDynamicTx.GasFeeCap())
	}

	// 3. Access List Transaction
	accessListTx := types.NewTx(&types.AccessListTx{
		ChainID:    chainIdInt,
		Nonce:      0,
		GasPrice:   big.NewInt(1000000000), // 1 Gwei
		Gas:        21000,
		To:         &common.Address{0x1},
		Value:      big.NewInt(0),
		Data:       []byte{},
		AccessList: types.AccessList{},
	})

	bumpedAccessListTx, err := h.bumpGasAndResubmit(context.Background(), ctx, accessListTx, auth, 1)
	if err != nil {
		t.Fatalf("bumpGasAndResubmit (access list) failed: %v", err)
	}

	if bumpedAccessListTx.Type() != types.AccessListTxType {
		t.Errorf("expected AccessListTxType, got %d", bumpedAccessListTx.Type())
	}
	expectedALPrice := big.NewInt(1150000000)
	if bumpedAccessListTx.GasPrice().Cmp(expectedALPrice) != 0 {
		t.Errorf("expected gas price %v, got %v", expectedALPrice, bumpedAccessListTx.GasPrice())
	}
}

// fixedSigner supplies a deterministic key so a test does not depend on the
// process environment. The Handler's signer field is the seam; keySigner only
// falls back to the env reader when it is nil.
type fixedSigner struct{ eth *ecdsa.PrivateKey }

func (s fixedSigner) EthKey() (*ecdsa.PrivateKey, error) { return s.eth, nil }

func (s fixedSigner) CosmosKeyBytes() ([]byte, error) {
	return nil, errors.New("cosmos key not configured in this test")
}

// testCtx bounds every call. A test must not be able to HANG: the send path polls
// for a receipt until its context ends, so on an unbounded context any change that
// stops a transaction from being mined turns a failing test into a stuck one. That
// is worse than a red test — it wedges the whole run with no message.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// fastReceipts shortens the receipt poll so a send completes immediately instead
// of waiting out the production 2s tick. ethTxReceiptPollInterval is a var for
// exactly this; without it these tests add ~11s to every run of this package.
func fastReceipts(t *testing.T) {
	t.Helper()
	prev := ethTxReceiptPollInterval
	ethTxReceiptPollInterval = time.Millisecond
	t.Cleanup(func() { ethTxReceiptPollInterval = prev })
}

func testHandler(t *testing.T) (*Handler, common.Address) {
	t.Helper()
	fastReceipts(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &Handler{signer: fixedSigner{eth: key}}, crypto.PubkeyToAddress(key.PublicKey)
}

// testEndpoint points the router and light-client contracts at distinct non-zero
// addresses; the binding constructors reject the zero address.
func testEndpoint(t *testing.T, node *rpcmock.EVMNode) services.EVMEndpoint {
	t.Helper()
	return services.EVMEndpoint{
		Client: node.Client(t),
		Contracts: services.EVMContracts{
			Router:        common.HexToAddress("0x1111111111111111111111111111111111111111"),
			SpectreClient: common.HexToAddress("0x2222222222222222222222222222222222222222"),
		},
	}
}

func recvPacketMsg() contractICS26Router.IICS26RouterMsgsMsgRecvPacket {
	return contractICS26Router.IICS26RouterMsgsMsgRecvPacket{
		Packet:        samplePacket(),
		MembershipMsg: []byte{0xaa},
	}
}

// --- SendEthTx / SendEthTxBatch, driven against a stub node ---
//
// The layer below these was already covered (see the calldata-packing tests
// above); the functions themselves were not, so their guards, the single-vs-batch
// split and the nonce behaviour went unverified. They own the ETH nonce.

func TestSendEthTx(t *testing.T) {
	t.Run("requires a router client id", func(t *testing.T) {
		// The id names which light client the call applies to. Without it the
		// transaction would target whatever the contract defaults to, so this must
		// fail before anything is signed or broadcast.
		node := rpcmock.NewEVMNode(t)
		h, _ := testHandler(t)

		if err := h.SendEthTx(testCtx(t), testEndpoint(t, node), "", recvPacketMsg()); err == nil {
			t.Fatal("sent a transaction with no router client id")
		}
		if n := len(node.SentRawTxs()); n != 0 {
			t.Fatalf("broadcast %d transactions despite the guard", n)
		}
	})

	t.Run("reports an unsupported message type", func(t *testing.T) {
		// Skipping it silently would leave the caller believing the packet was
		// relayed, and nothing else will retry it.
		node := rpcmock.NewEVMNode(t)
		h, _ := testHandler(t)

		err := h.SendEthTx(testCtx(t), testEndpoint(t, node), "client-0", "not a message")
		if err == nil {
			t.Fatal("accepted a message type it cannot build a transaction from")
		}
		if !strings.Contains(err.Error(), "unsupported message type") {
			t.Fatalf("error should name the problem, got: %v", err)
		}
		if n := len(node.SentRawTxs()); n != 0 {
			t.Fatalf("broadcast %d transactions for an unsupported message", n)
		}
	})

	t.Run("surfaces a rejected broadcast", func(t *testing.T) {
		// Swallowing it would mark the packet relayed while nothing was submitted.
		node := rpcmock.NewEVMNode(t)
		node.MineEverything(rpcmock.RawReceipt{Status: 1, GasUsed: 21_000, BlockNumber: 100})
		node.FailSend(errors.New("insufficient funds for gas * price + value"))
		h, _ := testHandler(t)

		if err := h.SendEthTx(testCtx(t), testEndpoint(t, node), "client-0", recvPacketMsg()); err == nil {
			t.Fatal("a rejected broadcast was reported as success")
		}
	})
}

func TestSendEthTxBatch(t *testing.T) {
	t.Run("empty batch opens no transaction", func(t *testing.T) {
		// A relay cycle that filtered down to nothing must not cost gas.
		node := rpcmock.NewEVMNode(t)
		h, _ := testHandler(t)

		if err := h.SendEthTxBatch(testCtx(t), testEndpoint(t, node), "client-0", nil); err != nil {
			t.Fatalf("empty batch must be a no-op, got %v", err)
		}
		if n := node.MethodCalls("eth_chainId"); n != 0 {
			t.Fatalf("an empty batch reached the node (%d chainId calls)", n)
		}
	})

	t.Run("single message skips the multicall", func(t *testing.T) {
		// The wrapper costs gas and changes how a revert surfaces: a multicall
		// reverts as the aggregate, hiding which packet was the poison.
		node := rpcmock.NewEVMNode(t)
		node.MineEverything(rpcmock.RawReceipt{Status: 1, GasUsed: 21_000, BlockNumber: 100})
		h, _ := testHandler(t)

		err := h.SendEthTxBatch(testCtx(t), testEndpoint(t, node), "client-0", []any{recvPacketMsg()})
		if err != nil {
			t.Fatalf("single-message batch: %v", err)
		}
		sent := node.SentRawTxs()
		if len(sent) != 1 {
			t.Fatalf("broadcast %d transactions for one message, want 1", len(sent))
		}
		// Assert on the ABSENCE of multicall, not the presence of recvPacket: a
		// multicall payload embeds the inner call verbatim, so recvPacket's
		// selector appears either way. The first version of this test checked for
		// it and passed while the shortcut was deleted.
		if strings.Contains(sent[0], routerSelector(t, "multicall")) {
			t.Fatalf("single message was wrapped in a multicall: %s", sent[0])
		}
		if !strings.Contains(sent[0], routerSelector(t, "recvPacket")) {
			t.Fatalf("transaction does not carry a recvPacket call: %s", sent[0])
		}
	})

	t.Run("folds several messages into one transaction", func(t *testing.T) {
		// Sending them separately would be correct but is what batching exists to
		// avoid, and it would break the ordering the relay path depends on.
		node := rpcmock.NewEVMNode(t)
		node.MineEverything(rpcmock.RawReceipt{Status: 1, GasUsed: 50_000, BlockNumber: 100})
		h, _ := testHandler(t)

		msgs := []any{recvPacketMsg(), recvPacketMsg(), recvPacketMsg()}
		if err := h.SendEthTxBatch(testCtx(t), testEndpoint(t, node), "client-0", msgs); err != nil {
			t.Fatalf("multi-message batch: %v", err)
		}
		if sent := node.SentRawTxs(); len(sent) != 1 {
			t.Fatalf("broadcast %d transactions for %d messages, want 1 multicall", len(sent), len(msgs))
		}
	})

	t.Run("does not re-read the nonce per transaction", func(t *testing.T) {
		// Re-reading returns the same value for two transactions submitted before
		// the first is mined, and the second replaces the first instead of
		// following it.
		node := rpcmock.NewEVMNode(t)
		node.MineEverything(rpcmock.RawReceipt{Status: 1, GasUsed: 21_000, BlockNumber: 100})
		h, from := testHandler(t)
		endpoint := testEndpoint(t, node)

		for i := 0; i < 3; i++ {
			if err := h.SendEthTxBatch(testCtx(t), endpoint, "client-0", []any{recvPacketMsg()}); err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
		}
		if queries := node.NonceQueries(from); queries != 1 {
			t.Fatalf("queried the account nonce %d times across 3 sends, want 1 (it is cached)", queries)
		}
		if sent := len(node.SentRawTxs()); sent != 3 {
			t.Fatalf("broadcast %d transactions, want 3", sent)
		}
	})
}

// routerSelector returns an ICS26Router method's 4-byte selector as lowercase hex.
func routerSelector(t *testing.T, method string) string {
	t.Helper()
	parsedABI, err := contractICS26Router.ContractICS26RouterMetaData.GetAbi()
	if err != nil {
		t.Fatalf("load router ABI: %v", err)
	}
	m, ok := parsedABI.Methods[method]
	if !ok {
		t.Fatalf("router ABI has no method %q", method)
	}
	return hex.EncodeToString(m.ID)
}

// --- environment overrides ---
//
// These five knobs were parsed with fmt.Sscanf("%d"), which stops at the first
// non-digit and reports success on the prefix. An operator who set
// COSMOS_GAS_LIMIT=150000x got a limit of 150000, and one who wrote it in hex got
// 0 -- silently, with the symptom arriving later as an out-of-gas revert that
// points nowhere near the config.
//
// ETH_MULTICALL_GAS_LIMIT was worse: its parse error was discarded entirely, so a
// malformed value fell back to the default and nothing said so.

func TestEnvUint64(t *testing.T) {
	const name = "TEST_RELAYER_UINT_KNOB"

	t.Run("unset uses the default", func(t *testing.T) {
		t.Setenv(name, "")
		got, err := envUint64(name, 42)
		if err != nil || got != 42 {
			t.Fatalf("envUint64 = %d, %v; want 42, nil", got, err)
		}
	})

	t.Run("parses a plain decimal", func(t *testing.T) {
		t.Setenv(name, "1500000")
		got, err := envUint64(name, 42)
		if err != nil || got != 1500000 {
			t.Fatalf("envUint64 = %d, %v; want 1500000, nil", got, err)
		}
	})

	t.Run("rejects anything else", func(t *testing.T) {
		for label, raw := range map[string]string{
			"digits then letters": "150000x",
			"hex":                 "0x1e8480",
			"negative":            "-1",
			"decimal point":       "1500.0",
			"whitespace":          " 1500000 ",
			"overflow":            "99999999999999999999999",
			"words":               "lots",
		} {
			t.Run(label, func(t *testing.T) {
				t.Setenv(name, raw)
				got, err := envUint64(name, 42)
				if err == nil {
					t.Fatalf("envUint64(%q) = %d with no error", raw, got)
				}
				// Not the default, and not a truncated value: the caller must be
				// unable to proceed on a number nobody asked for.
				if got != 0 {
					t.Fatalf("envUint64(%q) = %d alongside an error", raw, got)
				}
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name the variable the operator set", err)
				}
			})
		}
	})

	// Every read of the numeric overrides must go through envUint64/envInt64, which
	// is where the strict parse and the negative-fee guard live. A second reader that
	// parses the variable itself puts the panic back within reach -- and that is not
	// hypothetical: the batch fee path did exactly that until review caught it.
	//
	// A structural check rather than a behavioural one, for the same reason the clone
	// test in services/ is structural: exercising the batch path needs a Cosmos
	// client and a signer, and a mutation that reintroduces a raw parse there is
	// invisible to every test that does not. This fails on the change that breaks it
	// rather than on the bug it later causes.
	t.Run("numeric overrides are read only through the helpers", func(t *testing.T) {
		// handler.go became ethereum.go + cosmos.go in #456. The rule is about every
		// place these variables are read, so the test reads both halves rather than
		// following the name that used to hold them.
		var source []byte
		for _, name := range []string{"ethereum.go", "cosmos.go"} {
			part, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			source = append(source, part...)
		}

		for _, name := range []string{"COSMOS_FEE_AMOUNT", "COSMOS_GAS_LIMIT", "ETH_MULTICALL_GAS_LIMIT"} {
			t.Run(name, func(t *testing.T) {
				for i, line := range strings.Split(string(source), "\n") {
					if !strings.Contains(line, `"`+name+`"`) {
						continue
					}
					if strings.Contains(line, "envUint64(") || strings.Contains(line, "envInt64(") {
						continue
					}
					t.Errorf("handler.go:%d reads %s outside envUint64/envInt64, so it does not get the "+
						"strict parse or the negative guard:\n  %s", i+1, name, strings.TrimSpace(line))
				}
			})
		}
	})
}

func TestEnvInt64(t *testing.T) {
	const name = "TEST_RELAYER_INT_KNOB"

	t.Run("unset uses the default and reports it was not set", func(t *testing.T) {
		t.Setenv(name, "")
		got, set, err := envInt64(name, 7)
		if err != nil || got != 7 || set {
			t.Fatalf("envInt64 = %d, %v, %v; want 7, false, nil", got, set, err)
		}
	})

	t.Run("parses a plain decimal", func(t *testing.T) {
		t.Setenv(name, "10000000")
		got, set, err := envInt64(name, 7)
		if err != nil || got != 10000000 || !set {
			t.Fatalf("envInt64 = %d, %v, %v; want 10000000, true, nil", got, set, err)
		}
	})

	// An explicit zero is a real setting -- a chain with no minimum gas price
	// takes a zero fee -- and it must be distinguishable from an unset variable.
	// It was not: the batch fee path branched on `baseFee > 0`, so setting the
	// variable to 0 was read as "unset", the override was dropped, and the
	// default gas-matching fee was paid instead. Only the second return can tell
	// the two apart, since the value is identical.
	t.Run("an explicit zero is reported as set", func(t *testing.T) {
		t.Setenv(name, "0")
		got, set, err := envInt64(name, 7)
		if err != nil {
			t.Fatalf("envInt64 rejected an explicit zero: %v", err)
		}
		if got != 0 {
			t.Fatalf("envInt64 = %d, want 0", got)
		}
		if !set {
			t.Fatal("an explicit 0 reported as unset; callers cannot honour a zero fee")
		}

		unsetValue, unsetSet, err := envInt64(name+"_ABSENT", 0)
		if err != nil {
			t.Fatalf("unset lookup: %v", err)
		}
		if unsetValue != got {
			t.Fatalf("fixture is not comparable: unset gave %d, explicit zero gave %d", unsetValue, got)
		}
		if unsetSet == set {
			t.Fatal("explicit zero and unset are indistinguishable; the value alone cannot decide")
		}
	})

	// A negative fee must be refused HERE, not passed on.
	//
	// This test asserted the opposite in the first version of this PR, on the
	// reasoning that the chain would reject it with a better message. Review
	// showed that reasoning was wrong: every path feeds the amount into
	// sdk.NewCoin, which PANICS on a negative amount. Nothing reaches the chain --
	// the relayer process dies while building the transaction, on an operator
	// typo. The subtest below pins the panic that makes this non-negotiable.
	t.Run("refuses a negative value", func(t *testing.T) {
		t.Setenv(name, "-1")
		got, _, err := envInt64(name, 7)
		if err == nil {
			t.Fatalf("envInt64 accepted %d; sdk.NewCoin panics on a negative amount", got)
		}
		if got != 0 {
			t.Fatalf("envInt64 = %d alongside an error", got)
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name the variable the operator set", err)
		}
	})

	// The reason the guard above cannot be relaxed, pinned so nobody re-derives
	// it: this is what happens downstream to a negative fee that slips through.
	t.Run("a negative amount panics in sdk.NewCoin", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("sdk.NewCoin no longer panics on a negative amount; " +
					"re-check whether envInt64 still needs to reject one")
			}
		}()
		_ = sdk.NewCoin("stake", sdkmath.NewInt(-1))
	})

	t.Run("rejects anything that is not an integer", func(t *testing.T) {
		for label, raw := range map[string]string{
			"digits then letters": "10000x",
			"hex":                 "0x10",
			"decimal point":       "1.5",
		} {
			t.Run(label, func(t *testing.T) {
				t.Setenv(name, raw)
				if _, _, err := envInt64(name, 7); err == nil {
					t.Fatalf("envInt64(%q) returned no error", raw)
				}
			})
		}
	})
}

// hexToUint64 reads gas values out of an RPC trace, so its input is whatever the
// node sent. Sscanf("%x") accepted "1fzz" as 0x1f, turning a corrupted field into
// a plausible gas number rather than an error.
func TestHexToUint64(t *testing.T) {
	t.Run("parses hex with and without the prefix", func(t *testing.T) {
		for raw, want := range map[string]uint64{
			"0x1f":             31,
			"0X1F":             31,
			"1f":               31,
			"0":                0,
			"ffffffffffffffff": ^uint64(0),
			"0x":               0, // prefix only: the trace's way of writing zero
		} {
			got, err := hexToUint64(raw)
			if err != nil {
				t.Errorf("hexToUint64(%q): %v", raw, err)
				continue
			}
			if got != want {
				t.Errorf("hexToUint64(%q) = %d, want %d", raw, got, want)
			}
		}
	})

	t.Run("rejects malformed hex", func(t *testing.T) {
		for label, raw := range map[string]string{
			"empty":            "",
			"hex then junk":    "1fzz",
			"decimal marker":   "1f.0",
			"not hex at all":   "zz",
			"overflows uint64": "10000000000000000",
			"prefix then junk": "0xzz",
		} {
			t.Run(label, func(t *testing.T) {
				got, err := hexToUint64(raw)
				if err == nil {
					t.Fatalf("hexToUint64(%q) = %d with no error", raw, got)
				}
				if got != 0 {
					t.Fatalf("hexToUint64(%q) = %d alongside an error", raw, got)
				}
			})
		}
	})
}

// TestLiftAboveBaseFee covers the pricing failure seen on Arbitrum Sepolia:
//
//	max fee per gas less than block base fee: maxFeePerGas: 21486000, baseFee: 22414000
//
// SuggestGasPrice reflects the chain when it is called; on a chain whose base fee
// is climbing the value is already stale by the time EstimateGas uses it, and the
// node rejects the call rather than queueing it. Local devnets never show this —
// their base fee sits at the floor — so it only appears against a public network.
func TestLiftAboveBaseFee(t *testing.T) {
	cases := []struct {
		name      string
		suggested *big.Int
		baseFee   *big.Int
		want      *big.Int
	}{
		{
			// The exact numbers from the failure.
			name:      "suggestion below base fee is lifted clear",
			suggested: big.NewInt(21_486_000), baseFee: big.NewInt(22_414_000),
			want: big.NewInt(44_828_000), // 2x base fee
		},
		{
			name:      "suggestion already clear is untouched",
			suggested: big.NewInt(100_000_000), baseFee: big.NewInt(22_414_000),
			want: big.NewInt(100_000_000),
		},
		{
			name:      "exactly at the floor is untouched",
			suggested: big.NewInt(44_828_000), baseFee: big.NewInt(22_414_000),
			want: big.NewInt(44_828_000),
		},
		{
			name:      "nil suggestion takes the floor",
			suggested: nil, baseFee: big.NewInt(1_000),
			want: big.NewInt(2_000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := liftAboveBaseFee(tc.suggested, tc.baseFee)
			if got.Cmp(tc.want) != 0 {
				t.Fatalf("liftAboveBaseFee(%v, %v) = %v, want %v", tc.suggested, tc.baseFee, got, tc.want)
			}
		})
	}

	// A chain with no base fee (pre-1559) must pass the suggestion through untouched
	// rather than being handed a nil-derived value.
	t.Run("without base fee", func(t *testing.T) {
		suggested := big.NewInt(5_000)
		if got := liftAboveBaseFee(suggested, nil); got != suggested {
			t.Fatalf("a nil base fee must return the suggestion unchanged, got %v", got)
		}
	})
}

type fakeSigner struct {
	ethCalls    int
	cosmosCalls int
	err         error
}

func (s *fakeSigner) EthKey() (*ecdsa.PrivateKey, error) {
	s.ethCalls++
	return nil, s.err
}

func (s *fakeSigner) CosmosKeyBytes() ([]byte, error) {
	s.cosmosCalls++
	return nil, s.err
}

func TestValidateKeys(t *testing.T) {
	// Startup validation must go THROUGH the seam.
	//
	// It used to read ETH_PRIVATE_KEY and COSMOS_PRIVATE_KEY itself and parse them
	// with keys.RestoreKey, which left a ninth key source outside the seam that
	// RLY-07 introduced -- and one a KMS-backed signer could never satisfy, since it
	// does not hand back a raw private key to parse. An installed signer must be the
	// only thing consulted, with the environment untouched.
	t.Run("uses the installed signer not the environment", func(t *testing.T) {
		t.Setenv("ETH_PRIVATE_KEY", "")
		t.Setenv("COSMOS_PRIVATE_KEY", "")

		want := errors.New("kms unavailable")
		fake := &fakeSigner{err: want}
		h := &Handler{}
		h.SetSigner(fake)

		if err := h.ValidateKeys(); !errors.Is(err, want) {
			t.Fatalf("ValidateKeys() = %v, want the signer's own error %v; a validator that reads the "+
				"environment cannot check a KMS-backed key at all", err, want)
		}
		if fake.ethCalls == 0 {
			t.Fatal("the ETH key was never requested from the signer")
		}
	})

	// Both keys are checked, not just the first: a relayer that starts with a broken
	// Cosmos key fails on its first send instead of at startup, which is the whole
	// point of validating.
	t.Run("checks both keys", func(t *testing.T) {
		priv, err := ethcrypto.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		fake := &okEthBadCosmosSigner{eth: priv}
		h := &Handler{}
		h.SetSigner(fake)

		if err := h.ValidateKeys(); err == nil {
			t.Fatal("ValidateKeys() = nil with an unusable Cosmos key; the failure would surface on the first send")
		}
		if !fake.cosmosAsked {
			t.Fatal("the Cosmos key was never requested")
		}
	})

	t.Run("handler validate keys uses installed signer", func(t *testing.T) {
		t.Setenv("ETH_PRIVATE_KEY", "")
		t.Setenv("COSMOS_PRIVATE_KEY", "")
		eth, err := keys.RestoreKey(testCosmosPrivKeyHex)
		if err != nil {
			t.Fatalf("RestoreKey: %v", err)
		}
		cosmos, err := hex.DecodeString(testCosmosPrivKeyHex)
		if err != nil {
			t.Fatalf("decode Cosmos key: %v", err)
		}
		signer := &testSigner{eth: eth, cosmos: cosmos}
		h := &Handler{}
		h.SetSigner(signer)
		if err := h.ValidateKeys(); err != nil {
			t.Fatalf("ValidateKeys: %v", err)
		}
		if signer.calls != 2 {
			t.Fatalf("signer calls = %d, want 2", signer.calls)
		}
	})
}

type okEthBadCosmosSigner struct {
	eth         *ecdsa.PrivateKey
	cosmosAsked bool
}

func (s *okEthBadCosmosSigner) EthKey() (*ecdsa.PrivateKey, error) { return s.eth, nil }

func (s *okEthBadCosmosSigner) CosmosKeyBytes() ([]byte, error) {
	s.cosmosAsked = true
	return nil, errors.New("COSMOS_PRIVATE_KEY environment variable is required in .env file")
}

func TestEthSignerAddress(t *testing.T) {
	t.Setenv("ETH_PRIVATE_KEY", testCosmosPrivKeyHex)

	address, err := (&Handler{}).EthSignerAddress()
	if err != nil {
		t.Fatalf("EthSignerAddress: %v", err)
	}
	if want := common.HexToAddress("0xd58ed941051839DB5ffe8735905881d3C7460cE1"); address != want {
		t.Fatalf("EthSignerAddress = %s, want %s", address.Hex(), want.Hex())
	}
}

type testSigner struct {
	eth    *ecdsa.PrivateKey
	cosmos []byte
	err    error
	calls  int
}

func (s *testSigner) EthKey() (*ecdsa.PrivateKey, error) {
	s.calls++
	return s.eth, s.err
}

func (s *testSigner) CosmosKeyBytes() ([]byte, error) {
	s.calls++
	return append([]byte(nil), s.cosmos...), s.err
}

func TestSetSigner_NilLeavesDefaultAvailable(t *testing.T) {
	h := &Handler{}
	h.SetSigner(nil)
	if h.signer != nil {
		t.Fatal("nil SetSigner installed a signer")
	}
	if h.keySigner() == nil {
		t.Fatal("default signer is nil after SetSigner(nil)")
	}
}

func TestEthGasLimit_RejectsZeroAndMalformedOverrides(t *testing.T) {
	for _, gasText := range []string{"0", "not-a-number"} {
		if _, err := EthGasLimit(gasText); err == nil {
			t.Fatalf("EthGasLimit(%q) accepted an invalid override", gasText)
		}
	}
	if got, err := EthGasLimit(""); err != nil || got != defaultEthGasLimit {
		t.Fatalf("EthGasLimit(\"\") = (%d, %v), want (%d, nil)", got, err, defaultEthGasLimit)
	}
	if got, err := EthGasLimit("4000000"); err != nil || got != 4_000_000 {
		t.Fatalf("EthGasLimit(valid) = (%d, %v), want (4000000, nil)", got, err)
	}
}

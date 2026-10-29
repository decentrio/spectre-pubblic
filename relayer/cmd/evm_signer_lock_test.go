package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// TestAcquireEVMSignerLocks pins the distinction that makes this guard different from
// the Cosmos signer lock: an EVM nonce belongs to (chain id, address), not to
// an address alone. The same key on two chains must therefore take two locks.
func TestAcquireEVMSignerLocks(t *testing.T) {
	address := common.HexToAddress("0xd58ed941051839DB5ffe8735905881d3C7460cE1")

	setup := func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		restoreDir := signerLockDir
		signerLockDir = func() (string, error) { return dir, nil }
		t.Cleanup(func() { signerLockDir = restoreDir })
	}

	t.Run("the second process with the same key on the same chain is refused", func(t *testing.T) {
		setup(t)
		key := evmSignerLockKey{chainID: "1", address: address}

		release, err := acquireEVMSignerLockKeys([]evmSignerLockKey{key})
		if err != nil {
			t.Fatalf("first process could not take the lock: %v", err)
		}
		defer release()

		_, err = acquireEVMSignerLockKeys([]evmSignerLockKey{key})
		if err == nil {
			t.Fatal("a second process took the same EVM nonce domain")
		}
		path, pathErr := evmSignerLockPathFor(key)
		if pathErr != nil {
			t.Fatalf("evmSignerLockPathFor: %v", pathErr)
		}
		for _, want := range []string{address.Hex(), "1", path, "ETH_PRIVATE_KEY"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("the same key on different chains can both start", func(t *testing.T) {
		setup(t)

		releaseOne, err := acquireEVMSignerLockKeys([]evmSignerLockKey{{chainID: "1", address: address}})
		if err != nil {
			t.Fatalf("first chain could not take the lock: %v", err)
		}
		defer releaseOne()

		releaseTwo, err := acquireEVMSignerLockKeys([]evmSignerLockKey{{chainID: "8453", address: address}})
		if err != nil {
			t.Fatalf("a process using the same key on another EVM chain must start: %v", err)
		}
		defer releaseTwo()
	})
}

func TestEvmSignerLockKeys_CoverEveryEVMSubmitPath(t *testing.T) {
	address := common.HexToAddress("0xd58ed941051839DB5ffe8735905881d3C7460cE1")
	chainIDs := map[string]string{
		"http://eth":        "1",
		"http://l2":         "8453",   // Base
		"http://return-leg": "421614", // Arbitrum Sepolia
	}

	keys, err := evmSignerLockKeysWithChainID(context.Background(), &appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{EthRpcUrl: "http://eth"},
		HasCosmosToL2: true, CosmosToL2Config: cosmosToEthConfig{EthRpcUrl: "http://l2"},
		HasL2ToCosmos: true, L2ToCosmosConfig: l2ToCosmosConfig{L2RpcUrl: "http://return-leg"},
	}, address, func(_ context.Context, endpoint string) (string, error) {
		return chainIDs[endpoint], nil
	})
	if err != nil {
		t.Fatalf("evmSignerLockKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("lock keys = %#v, want the L1, forward L2, and l2_to_cosmos return-leg L2", keys)
	}
	got := make(map[string]bool, len(keys))
	for _, key := range keys {
		if key.address != address {
			t.Fatalf("key address = %s, want %s", key.address.Hex(), address.Hex())
		}
		got[key.chainID] = true
	}
	for _, want := range []string{"1", "8453", "421614"} {
		if !got[want] {
			t.Fatalf("lock keys %v omit chain %s", got, want)
		}
	}
}

func TestEvmSignerLockKeysWithChainID_RefuseAnUnansweredEndpoint(t *testing.T) {
	shortenEVMChainIDRetries(t)

	_, err := evmSignerLockKeysWithChainID(context.Background(), &appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{EthRpcUrl: "http://unavailable"},
	}, common.HexToAddress("0xd58ed941051839DB5ffe8735905881d3C7460cE1"), func(context.Context, string) (string, error) {
		return "", errors.New("endpoint did not answer")
	})
	if err == nil {
		t.Fatal("startup accepted an EVM endpoint without a chain id and would have run without its nonce lock")
	}
	for _, want := range []string{"eth_chainId", "cosmos_to_eth.eth_rpc_url", "without the nonce-domain lock"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not explain %q", err, want)
		}
	}
}

// shortenEVMChainIDRetries keeps the retry schedule's shape (four attempts)
// while removing the real waits from the test.
func shortenEVMChainIDRetries(t *testing.T) {
	t.Helper()
	restore := evmSignerLockChainIDRetryDelays
	evmSignerLockChainIDRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { evmSignerLockChainIDRetryDelays = restore })
}

func TestEvmSignerLockKeysWithChainID_RetryABlip(t *testing.T) {
	shortenEVMChainIDRetries(t)

	calls := 0
	keys, err := evmSignerLockKeysWithChainID(context.Background(), &appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{EthRpcUrl: "http://slow-to-start"},
	}, common.HexToAddress("0xd58ed941051839DB5ffe8735905881d3C7460cE1"), func(context.Context, string) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("connection refused")
		}
		return "1", nil
	})
	if err != nil {
		t.Fatalf("startup stopped on an endpoint that answered on attempt 3: %v", err)
	}
	if calls != 3 {
		t.Fatalf("eth_chainId was asked %d times, want 3", calls)
	}
	if len(keys) != 1 || keys[0].chainID != "1" {
		t.Fatalf("keys = %+v, want one key on chain 1", keys)
	}
}

func TestEvmSignerLockKeysWithChainID_BoundedAttempts(t *testing.T) {
	shortenEVMChainIDRetries(t)

	calls := 0
	_, err := evmSignerLockKeysWithChainID(context.Background(), &appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{EthRpcUrl: "http://unavailable"},
	}, common.HexToAddress("0xd58ed941051839DB5ffe8735905881d3C7460cE1"), func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("endpoint did not answer")
	})
	if err == nil {
		t.Fatal("startup accepted an endpoint that never answered")
	}
	if want := len(evmSignerLockChainIDRetryDelays) + 1; calls != want {
		t.Fatalf("eth_chainId was asked %d times, want %d", calls, want)
	}
	if !strings.Contains(err.Error(), "endpoint did not answer") {
		t.Fatalf("error %q lost the last attempt's cause", err)
	}
}

func TestEvmSignerLockKeysWithChainID_CancelStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := evmSignerLockKeysWithChainID(ctx, &appConfig{
		HasCosmosToEth: true, CosmosToEthConfig: cosmosToEthConfig{EthRpcUrl: "http://unavailable"},
	}, common.HexToAddress("0xd58ed941051839DB5ffe8735905881d3C7460cE1"), func(context.Context, string) (string, error) {
		calls++
		cancel()
		return "", errors.New("endpoint did not answer")
	})
	if err == nil {
		t.Fatal("startup succeeded after its context was cancelled")
	}
	if calls != 1 {
		t.Fatalf("eth_chainId was asked %d times after cancellation, want 1", calls)
	}
}

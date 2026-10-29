package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	relayerclient "relayer/client"
	"relayer/transaction"
)

// An EVM account's nonce is scoped to this pair, exactly like
// transaction.evmNonceKey. Keeping the startup lock key identical to the
// sender-state key is what permits one ETH_PRIVATE_KEY on two different EVM
// chains while still refusing two processes on the same one.
type evmSignerLockKey struct {
	chainID string
	address common.Address
}

type evmSignerLockEndpoint struct {
	label  string
	rpcURL string
}

const evmSignerLockChainIDTimeout = 10 * time.Second

// evmSignerLockChainIDRetryDelays spaces the eth_chainId retries: four attempts
// over about four seconds of waiting. That rides out an RPC blip or an endpoint
// a moment slower to come up than the relayer, while an endpoint that is
// genuinely down still stops the start promptly. Each attempt keeps its own
// evmSignerLockChainIDTimeout, so a hung RPC is still bounded per attempt.
// A var only so tests can shorten the waits.
var evmSignerLockChainIDRetryDelays = []time.Duration{
	500 * time.Millisecond, 1 * time.Second, 2 * time.Second,
}

// acquireEVMSignerLocks takes every EVM nonce-domain lock this relay can use
// and holds it for the process lifetime. flock is local to a machine and inode,
// so it cannot protect two hosts that share ETH_PRIVATE_KEY; it does make the
// normal one-process-per-path deployment fail loudly before either process has
// allocated a nonce.
func acquireEVMSignerLocks(stdCtx context.Context, cfg *appConfig) (release func(), err error) {
	address, err := (&transaction.Handler{}).EthSignerAddress()
	if err != nil {
		return nil, fmt.Errorf("EVM signer lock: derive address from ETH_PRIVATE_KEY: %w", err)
	}

	keys, err := evmSignerLockKeys(stdCtx, cfg, address)
	if err != nil {
		return nil, err
	}
	return acquireEVMSignerLockKeys(keys)
}

// evmSignerLockKeys resolves the real chain id from every configured EVM
// endpoint before locking. A configured value cannot stand in for eth_chainId:
// transactions use the endpoint's answer, and a typo or stale config value
// would make two processes select different files for the same nonce domain.
//
// Unlike ordinary endpoint validation, a non-answer is fatal here. Continuing
// without the answer means continuing without a lock, which recreates the
// invisible nonce race the guard exists to prevent. The relay can be retried
// once the endpoint returns; it must not claim to have started protected when
// it has not.
func evmSignerLockKeys(stdCtx context.Context, cfg *appConfig, address common.Address) ([]evmSignerLockKey, error) {
	return evmSignerLockKeysWithChainID(stdCtx, cfg, address, evmChainIDForSignerLock)
}

// evmSignerLockKeysWithChainID keeps endpoint selection and nonce-domain
// de-duplication testable without a listener. The production caller always
// supplies evmChainIDForSignerLock, which performs the real eth_chainId read.
func evmSignerLockKeysWithChainID(
	stdCtx context.Context,
	cfg *appConfig,
	address common.Address,
	chainIDFor func(context.Context, string) (string, error),
) ([]evmSignerLockKey, error) {
	endpoints := evmSignerLockEndpoints(cfg)
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("EVM signer lock: no configured EVM endpoint to identify ETH_PRIVATE_KEY's nonce domain")
	}

	unique := make(map[evmSignerLockKey]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		chainID, err := evmChainIDWithRetry(stdCtx, endpoint, chainIDFor)
		if err != nil {
			return nil, fmt.Errorf(
				"EVM signer lock: read eth_chainId for %s at %s: %w. Startup stops rather than run without the nonce-domain lock",
				endpoint.label, endpoint.rpcURL, err)
		}
		unique[evmSignerLockKey{chainID: chainID, address: address}] = struct{}{}
	}

	keys := make([]evmSignerLockKey, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	// A stable order prevents two processes that need the same several locks
	// from each taking one first and then needlessly making both starts fail.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].chainID != keys[j].chainID {
			return numericChainIDLess(keys[i].chainID, keys[j].chainID)
		}
		return strings.ToLower(keys[i].address.Hex()) < strings.ToLower(keys[j].address.Hex())
	})
	return keys, nil
}

// evmChainIDWithRetry asks one endpoint for its chain id, retrying on the
// evmSignerLockChainIDRetryDelays schedule. Only the number of attempts is
// softened: the last error is returned unchanged, and the caller still treats
// it as fatal. Each retry is logged so the boot log tells "answered on attempt
// 3" apart from "never answered".
func evmChainIDWithRetry(
	stdCtx context.Context,
	endpoint evmSignerLockEndpoint,
	chainIDFor func(context.Context, string) (string, error),
) (string, error) {
	attempts := len(evmSignerLockChainIDRetryDelays) + 1
	for attempt := 1; ; attempt++ {
		chainID, err := chainIDFor(stdCtx, endpoint.rpcURL)
		if err == nil {
			if attempt > 1 {
				log.Printf("EVM signer lock: eth_chainId for %s at %s answered on attempt %d/%d",
					endpoint.label, endpoint.rpcURL, attempt, attempts)
			}
			return chainID, nil
		}
		if attempt == attempts {
			return "", err
		}
		delay := evmSignerLockChainIDRetryDelays[attempt-1]
		log.Printf("WARN EVM signer lock: eth_chainId for %s at %s failed on attempt %d/%d, retrying in %s: %v",
			endpoint.label, endpoint.rpcURL, attempt, attempts, delay, err)
		select {
		case <-stdCtx.Done():
			return "", err
		case <-time.After(delay):
		}
	}
}

// evmSignerLockEndpoints includes each direction that can submit to an EVM
// chain. l2_to_cosmos is intentionally included: its timeout return path signs
// on that L2 even though its ordinary packet delivery signs on Cosmos.
func evmSignerLockEndpoints(cfg *appConfig) []evmSignerLockEndpoint {
	if cfg == nil {
		return nil
	}

	var endpoints []evmSignerLockEndpoint
	if cfg.HasCosmosToEth {
		endpoints = append(endpoints, evmSignerLockEndpoint{
			label: "cosmos_to_eth.eth_rpc_url", rpcURL: cfg.CosmosToEthConfig.EthRpcUrl,
		})
	}
	if cfg.HasCosmosToL2 {
		endpoints = append(endpoints, evmSignerLockEndpoint{
			label: "cosmos_to_l2.eth_rpc_url", rpcURL: cfg.CosmosToL2Config.EthRpcUrl,
		})
	}
	if cfg.HasL2ToCosmos {
		endpoints = append(endpoints, evmSignerLockEndpoint{
			label: "l2_to_cosmos.l2_rpc_url", rpcURL: cfg.L2ToCosmosConfig.L2RpcUrl,
		})
	}
	return endpoints
}

// evmChainIDForSignerLock uses its own bounded context per endpoint so a hung
// RPC cannot block startup indefinitely. Its error is deliberately retained:
// saying which endpoint failed is the only actionable distinction between a
// transient outage and a process already holding the same signer lock.
func evmChainIDForSignerLock(stdCtx context.Context, rpcURL string) (string, error) {
	ctx, cancel := context.WithTimeout(stdCtx, evmSignerLockChainIDTimeout)
	defer cancel()

	client, err := relayerclient.DialEthRPC(ctx, rpcURL, evmSignerLockChainIDTimeout)
	if err != nil {
		return "", err
	}
	defer client.Close()

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return "", err
	}
	if chainID == nil {
		return "", fmt.Errorf("endpoint returned a nil chain id")
	}
	return chainID.String(), nil
}

// numericChainIDLess compares decimal chain ids without narrowing them to a
// machine integer. The endpoint may serve an id wider than uint64; it remains a
// distinct nonce domain and still needs a deterministic lock order.
func numericChainIDLess(a, b string) bool {
	ai, aOK := new(big.Int).SetString(a, 10)
	bi, bOK := new(big.Int).SetString(b, 10)
	if aOK && bOK {
		return ai.Cmp(bi) < 0
	}
	return a < b
}

func acquireEVMSignerLockKeys(keys []evmSignerLockKey) (release func(), err error) {
	dir, err := signerLockDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("EVM signer lock: create %q: %w", dir, err)
	}

	files := make([]*os.File, 0, len(keys))
	release = func() {
		for i := len(files) - 1; i >= 0; i-- {
			_ = syscall.Flock(int(files[i].Fd()), syscall.LOCK_UN)
			_ = files[i].Close()
		}
	}

	for _, key := range keys {
		path := evmSignerLockPathForDir(dir, key)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			release()
			return nil, fmt.Errorf("EVM signer lock: open %q: %w", path, err)
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = file.Close()
			release()
			return nil, fmt.Errorf(
				"another relayer process on this machine is already signing EVM transactions with %s on chain id %s "+
					"(lock: %s). The two processes share ETH_PRIVATE_KEY's nonce domain, so one would hand out a nonce the other has already used; "+
					"give this process its own ETH_PRIVATE_KEY, stop the other process, or use a different EVM chain",
				key.address.Hex(), key.chainID, path)
		}
		files = append(files, file)
	}
	return release, nil
}

// evmSignerLockPathFor is exposed to the test so a filename change cannot leave
// the test asserting a path the production lock no longer uses.
func evmSignerLockPathFor(key evmSignerLockKey) (string, error) {
	dir, err := signerLockDir()
	if err != nil {
		return "", err
	}
	return evmSignerLockPathForDir(dir, key), nil
}

func evmSignerLockPathForDir(dir string, key evmSignerLockKey) string {
	return filepath.Join(dir, fmt.Sprintf("evm-signer-%s-%s.lock", key.chainID, strings.ToLower(key.address.Hex())))
}

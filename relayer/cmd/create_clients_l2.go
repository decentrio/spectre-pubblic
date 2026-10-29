package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"attestor/types/attestation"
	"github.com/ethereum/go-ethereum/common"
	"go.uber.org/zap"

	relayerclient "relayer/client"
	"relayer/services"
	"relayer/transaction"
)

const flagL2Config = "l2-config"

// l2ClientConfig is one L2 rollup's client-creation config, passed to
// `create-clients-cosmos --l2-config <file>` (repeatable, one per L2 source). It is
// kept separate from the modules[] schema, which the config-schema follow-up will
// restructure. Cosmos connection details come from the main --config (cosmos_to_eth);
// this file only carries the L2-specific deployment identity.
type l2ClientConfig struct {
	// WasmChecksum is the hex checksum of the L2 client's wasm code, already
	// governance-stored on Cosmos.
	WasmChecksum string `json:"wasm_checksum"`
	// L2RPCURL is the L2 execution RPC used to read the bootstrap roots.
	L2RPCURL string `json:"l2_rpc_url"`
	// RollupProfile is the full ICS-08 verifier Profile JSON (the `common` block +
	// the rollup-specific finality fields), embedded verbatim as ClientState.profile.
	// The L2 router address, commitment slot, and chain ids all live inside it
	// (profile.common.*), so they are not repeated here.
	RollupProfile json.RawMessage `json:"rollup_profile"`
	// BootstrapBlock is the L2 block to bootstrap from; 0 (or absent) = latest.
	BootstrapBlock uint64 `json:"bootstrap_block"`
	// CounterpartyClientID is the L2-side client (on the rollup's ICS26Router) that
	// tracks Cosmos, registered inline as this L2 client's counterparty. Optional:
	// leave empty to defer registration until the L2-side client id is known.
	CounterpartyClientID string `json:"counterparty_client_id"`
	// Attestors is the exact immutable Ed25519 signing set stored in the wasm
	// client. Its public_keys are base64-encoded 32-byte keys in strict byte order.
	Attestors attestation.AttestorConfig `json:"attestors"`
}

// profileCommon is the subset of the verifier Profile the command reads to locate
// the L2 router for the bootstrap eth_getProof (single source of truth: the router
// address is not duplicated in l2-config).
type profileCommon struct {
	Common struct {
		L2Router string `json:"l2_router"`
	} `json:"common"`
}

// routerAddress extracts profile.common.l2_router from the rollup profile.
func (c *l2ClientConfig) routerAddress() (string, error) {
	var pc profileCommon
	if err := json.Unmarshal(c.RollupProfile, &pc); err != nil {
		return "", fmt.Errorf("l2-config: parse rollup_profile.common: %w", err)
	}
	if pc.Common.L2Router == "" {
		return "", fmt.Errorf("l2-config: rollup_profile.common.l2_router is required")
	}
	// Fail fast on malformed input: common.HexToAddress silently truncates/zero-pads a
	// bad string, which would later hit a wrong (or zero) address. GetL2BootstrapState
	// still rejects a non-contract (empty storage root), but a clear error here beats a
	// confusing downstream one.
	if !common.IsHexAddress(pc.Common.L2Router) {
		return "", fmt.Errorf("l2-config: rollup_profile.common.l2_router %q is not a valid hex address", pc.Common.L2Router)
	}
	return pc.Common.L2Router, nil
}

func (c *l2ClientConfig) validate() error {
	if c.WasmChecksum == "" {
		return fmt.Errorf("l2-config: wasm_checksum is required")
	}
	if c.L2RPCURL == "" {
		return fmt.Errorf("l2-config: l2_rpc_url is required")
	}
	if len(c.RollupProfile) == 0 {
		return fmt.Errorf("l2-config: rollup_profile is required")
	}
	if _, err := c.routerAddress(); err != nil {
		return err
	}
	// This config creates a client, so the install-time key rules apply: the wasm client would
	// refuse the same set on chain, and an operator should hear it before broadcasting.
	if err := c.Attestors.ValidateForInstall(); err != nil {
		return fmt.Errorf("l2-config: invalid attestors: %w", err)
	}
	return nil
}

func loadL2ClientConfig(path string) (*l2ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read l2-config %s: %w", path, err)
	}
	var cfg l2ClientConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse l2-config %s: %w", path, err)
	}
	if err := rejectRelayerConfigAsL2Config(path, data); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// rejectRelayerConfigAsL2Config catches the relayer's own config.json being passed
// to --l2-config.
//
// The two files share a name in conversation ("the config") but not a schema, and
// the mistake is easy: both live in relayer/, and the flag takes a path. Without
// this the failure is "l2-config: wasm_checksum is required" — a field-level
// complaint that sends the operator hunting for a missing value in a file that was
// never the right one.
func rejectRelayerConfigAsL2Config(path string, data []byte) error {
	var probe struct {
		Modules []json.RawMessage `json:"modules"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil // not object-shaped; the normal parse/validate errors are clearer
	}
	if len(probe.Modules) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s looks like the relayer config (it has a \"modules\" array), not an l2-config. "+
			"--l2-config takes a separate per-rollup file with wasm_checksum, l2_rpc_url and "+
			"rollup_profile at the top level; see relayer/op-l2-config.example.json or "+
			"relayer/base-l2-config.example.json", path)
}

// runCreateClientsL2 bootstraps one L2 rollup wasm light client on Cosmos: it reads
// the initial state roots from the L2 chain, wraps them in the ICS-08 client state
// (ClientState<Profile>) + consensus state, and submits MsgCreateClient. The
// initial consensus state (state roots read here) is the client's trust anchor at
// creation; ongoing updates are verified by the wasm client against the header and
// router proof carried in each ClientMessage.
//
// The profile is submitted verbatim. Earlier revisions anchored every L2 client to an
// Ethereum client on Cosmos and injected that id here; the attestor-trusted profile has
// no ethereum_client member at all, so there is nothing left to inject and no L1 client
// this command needs to exist.
func runCreateClientsL2(logger *zap.Logger, cfg *appConfig, l2cfg *l2ClientConfig) (string, error) {
	// Cosmos dependencies (MsgCreateClient is submitted to Cosmos).
	deps, cosmosClient, err := buildCreateClientsDeps(logger, cfg, "")
	if err != nil {
		return "", err
	}
	defer cosmosClient.Stop()

	// The L2 client's wasm code must already be governance-stored on Cosmos.
	logger.Sugar().Infof("create-clients-cosmos[l2]: validating wasm checksum on Cosmos: %s", l2cfg.WasmChecksum)
	ok, err := cosmosHasWasmChecksum(cosmosClient, l2cfg.WasmChecksum)
	if err != nil {
		return "", fmt.Errorf("validate wasm checksum on Cosmos: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("wasm checksum %s has not been stored on Cosmos", l2cfg.WasmChecksum)
	}

	// Verify the profile names the chain this RPC actually serves, BEFORE reading
	// bootstrap roots from it and before committing the profile on-chain.
	//
	// `start` runs the same check, but only over config.json's l2_to_cosmos
	// modules — a different copy of the profile, read later. This path is where
	// the durable damage happens: the profile handed to CreateL2Client is
	// committed into the created client verbatim, and nothing can repair it
	// afterwards. Without this, a client bootstrapped from chain M could carry a
	// profile declaring chain N, and the later `start` check would at best flag
	// the config copy while the on-chain client stayed wrong.
	//
	// Unreachable is fatal here, unlike in `start` — see l2ChainIDCheck.
	wantChainID, err := validateL2ClientChainID(context.Background(), l2cfg)
	if err != nil {
		return "", err
	}
	logger.Sugar().Infof("create-clients-cosmos[l2]: l2_chain_id verified: %d", wantChainID)

	// Read the trusted bootstrap roots from the L2 chain.
	logger.Sugar().Infof("create-clients-cosmos[l2]: dialing L2 rpc %s", l2cfg.L2RPCURL)
	l2Client, err := relayerclient.DialEthRPC(context.Background(), l2cfg.L2RPCURL, relayerclient.DefaultRPCTimeout)
	if err != nil {
		return "", fmt.Errorf("dial L2 rpc: %w", err)
	}
	defer l2Client.Close()

	router, err := l2cfg.routerAddress() // profile.common.l2_router — single source of truth
	if err != nil {
		return "", err
	}
	var block *big.Int
	if l2cfg.BootstrapBlock != 0 {
		block = new(big.Int).SetUint64(l2cfg.BootstrapBlock)
	}
	bootstrap, err := relayerclient.GetL2BootstrapState(l2Client, common.HexToAddress(router), block)
	if err != nil {
		return "", err
	}
	logger.Sugar().Infof("create-clients-cosmos[l2]: bootstrap at L2 block %d (state_root=%s router_storage_root=%s ts=%d)",
		bootstrap.Height, bootstrap.StateRoot.Hex(), bootstrap.RouterStorageRoot.Hex(), bootstrap.TimestampSeconds)

	worker := services.NewWorker(&transaction.Handler{}, nil)
	clientID, err := worker.CreateL2Client(context.Background(), deps.Cosmos, services.L2ClientParams{
		WasmChecksum:         l2cfg.WasmChecksum,
		RollupProfile:        l2cfg.RollupProfile,
		Bootstrap:            bootstrap,
		CounterpartyClientID: l2cfg.CounterpartyClientID,
		Attestors:            l2cfg.Attestors,
	})
	if err != nil {
		return "", fmt.Errorf("create L2 client on Cosmos: %w", err)
	}
	logger.Sugar().Infof("create-clients-cosmos[l2]: created L2 wasm client on Cosmos: clientID=%s", clientID)
	fmt.Println(clientID)
	return clientID, nil
}

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"relayer/services"
)

const l2TimeoutReturnPathLookupTimeout = 3 * time.Second

type l2TimeoutReturnPath struct {
	svc  *services.Services
	deps services.RelayDeps
}

func (p l2TimeoutReturnPath) validate() error {
	if p.svc == nil {
		return fmt.Errorf("l2_to_cosmos timeout return path is missing services")
	}
	if p.deps.Cosmos.CosmosClient() == nil {
		return fmt.Errorf("l2_to_cosmos timeout return path has nil Cosmos client")
	}
	if p.deps.EVM.EthClient() == nil {
		return fmt.Errorf("l2_to_cosmos timeout return path has nil L2 client")
	}
	if p.deps.EVM.RouterContract() == nil {
		return fmt.Errorf("l2_to_cosmos timeout return path has nil L2 router address")
	}
	if p.deps.EVM.SpectreClientContract() == nil {
		return fmt.Errorf("l2_to_cosmos timeout return path has nil SpectreClient address")
	}
	return nil
}

type l2TimeoutReturnPathConfig struct {
	name      string
	cfg       cosmosToEthConfig
	l2ChainID string
	path      l2TimeoutReturnPath
}

type l2TimeoutReturnPathKey struct {
	l2ChainID string
	tmRPCURL  string
	router    string
}

func l2TimeoutReturnPathConfigForDest(parent context.Context, name string, cfg cosmosToEthConfig, path l2TimeoutReturnPath) (l2TimeoutReturnPathConfig, error) {
	if err := path.validate(); err != nil {
		return l2TimeoutReturnPathConfig{}, err
	}
	ctx, cancel := context.WithTimeout(parent, l2TimeoutReturnPathLookupTimeout)
	defer cancel()
	chainID, err := path.deps.EVM.EthClient().ChainID(ctx)
	if err != nil {
		return l2TimeoutReturnPathConfig{}, fmt.Errorf(
			"read L2 chain id for cosmos_to_l2 return path %q at %s: %w",
			moduleFieldPrefix(name, dirCosmosToL2), cfg.EthRpcUrl, err,
		)
	}
	if chainID == nil {
		return l2TimeoutReturnPathConfig{}, fmt.Errorf(
			"read L2 chain id for cosmos_to_l2 return path %q at %s: nil chain id",
			moduleFieldPrefix(name, dirCosmosToL2), cfg.EthRpcUrl,
		)
	}
	return l2TimeoutReturnPathConfig{
		name:      name,
		cfg:       cfg,
		l2ChainID: chainID.String(),
		path:      path,
	}, nil
}

func l2TimeoutReturnPathKeyForSource(parent context.Context, cfg l2ToCosmosConfig) (l2TimeoutReturnPathKey, error) {
	router, err := l2RouterFromProfile(cfg.RollupProfile)
	if err != nil {
		return l2TimeoutReturnPathKey{}, err
	}
	// Dial and ChainID each get their own full budget rather than sharing one
	// deadline: a slow-but-successful dial would otherwise starve the
	// ChainID call (or vice versa) for no reason — they're independent RPC
	// round trips against the same endpoint.
	dialCtx, dialCancel := context.WithTimeout(parent, l2TimeoutReturnPathLookupTimeout)
	defer dialCancel()
	l2Client, err := ethclient.DialContext(dialCtx, cfg.L2RpcUrl)
	if err != nil {
		return l2TimeoutReturnPathKey{}, fmt.Errorf(
			"dial L2 rpc for l2_to_cosmos return-path match at %s: %w", cfg.L2RpcUrl, err)
	}
	defer l2Client.Close()

	chainIDCtx, chainIDCancel := context.WithTimeout(parent, l2TimeoutReturnPathLookupTimeout)
	defer chainIDCancel()
	chainID, err := l2Client.ChainID(chainIDCtx)
	if err != nil {
		return l2TimeoutReturnPathKey{}, fmt.Errorf(
			"read L2 chain id for l2_to_cosmos return-path match at %s: %w", cfg.L2RpcUrl, err)
	}
	if chainID == nil {
		return l2TimeoutReturnPathKey{}, fmt.Errorf(
			"read L2 chain id for l2_to_cosmos return-path match at %s: nil chain id", cfg.L2RpcUrl)
	}
	return l2TimeoutReturnPathKey{
		l2ChainID: chainID.String(),
		tmRPCURL:  cfg.TmRpcUrl,
		router:    strings.ToLower(router.Hex()),
	}, nil
}

func l2TimeoutReturnPathKeyForDest(path l2TimeoutReturnPathConfig) (l2TimeoutReturnPathKey, error) {
	cfg := path.cfg
	prefix := moduleFieldPrefix(path.name, dirCosmosToL2)
	if !common.IsHexAddress(cfg.ICS26Address) {
		return l2TimeoutReturnPathKey{}, fmt.Errorf("%s.ics26_address %q is not a valid hex address", prefix, cfg.ICS26Address)
	}
	if path.l2ChainID == "" {
		return l2TimeoutReturnPathKey{}, fmt.Errorf("%s return path is missing L2 chain id", prefix)
	}
	return l2TimeoutReturnPathKey{
		l2ChainID: path.l2ChainID,
		tmRPCURL:  cfg.TmRpcUrl,
		router:    strings.ToLower(common.HexToAddress(cfg.ICS26Address).Hex()),
	}, nil
}

type l2TimeoutReturnPathCandidate struct {
	name string
	key  l2TimeoutReturnPathKey
}

func findL2TimeoutReturnPath(parent context.Context, src l2ToCosmosConfig, paths []l2TimeoutReturnPathConfig) (l2TimeoutReturnPath, error) {
	key, err := l2TimeoutReturnPathKeyForSource(parent, src)
	if err != nil {
		return l2TimeoutReturnPath{}, err
	}

	match := -1
	var matches []string
	candidates := make([]l2TimeoutReturnPathCandidate, 0, len(paths))
	for i := range paths {
		candidate, err := l2TimeoutReturnPathKeyForDest(paths[i])
		if err != nil {
			return l2TimeoutReturnPath{}, err
		}
		name := moduleFieldPrefix(paths[i].name, dirCosmosToL2)
		candidates = append(candidates, l2TimeoutReturnPathCandidate{name: name, key: candidate})
		if candidate != key {
			continue
		}
		matches = append(matches, name)
		if match != -1 {
			return l2TimeoutReturnPath{}, fmt.Errorf(
				"multiple cosmos_to_l2 return paths match l2_to_cosmos source\n  want: %s\n  matches: %s\n  candidates: %s",
				formatL2TimeoutReturnPathKey(key), strings.Join(matches, ", "), formatL2TimeoutReturnPathCandidates(candidates),
			)
		}
		match = i
	}
	if match == -1 {
		return l2TimeoutReturnPath{}, fmt.Errorf(
			"no matching cosmos_to_l2 return path for l2_to_cosmos source\n  want: %s\n  candidates: %s",
			formatL2TimeoutReturnPathKey(key), formatL2TimeoutReturnPathCandidates(candidates),
		)
	}
	return paths[match].path, nil
}

func formatL2TimeoutReturnPathKey(key l2TimeoutReturnPathKey) string {
	return fmt.Sprintf("l2_chain_id=%s tm_rpc_url=%s l2_router=%s", key.l2ChainID, key.tmRPCURL, key.router)
}

func formatL2TimeoutReturnPathCandidates(candidates []l2TimeoutReturnPathCandidate) string {
	if len(candidates) == 0 {
		return "<none>"
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, fmt.Sprintf("%s: %s", candidate.name, formatL2TimeoutReturnPathKey(candidate.key)))
	}
	return strings.Join(out, "; ")
}

// destHasReturnLeg reports whether any l2_to_cosmos source resolved to this
// Cosmos→L2 destination, which is the only condition under which a debt the
// destination records can ever be settled.
//
// The comparison is by *services.Services IDENTITY, not by config equality,
// because sharing that instance IS the settlement mechanism: the forward
// direction records into svc's ledger and the return direction clears from the
// same one. Two destinations with identical config are still two ledgers.
//
// Found in review: this used to be `len(l2Sources) > 0`. A source matches
// exactly one destination (by L2 chain id, Cosmos RPC URL and router address),
// so in any multi-L2 deployment the global count marked every forward-only
// destination as settleable. Those paths then persisted ack debt no module
// shared their ledger to clear, and reported it overdue forever -- even for
// packets another relayer had acknowledged.
func destHasReturnLeg(dest *services.Services, settling []*services.Services) bool {
	if dest == nil {
		return false
	}
	for _, svc := range settling {
		if svc == dest {
			return true
		}
	}
	return false
}

// builtL2Dest is one built-but-not-yet-running Cosmos→L2 destination.
//
// It lives at package level rather than inside the start command so that the
// return-leg decision (destHasReturnLeg) can be tested without standing up a
// chain, a prover and an attestor. The review that found the len(l2Sources) > 0
// bug also found that nothing could have caught it, and that was a direct
// consequence of the decision living inside an untestable closure.
type builtL2Dest struct {
	svc     *services.Services
	deps    services.RelayDeps
	cleanup func()
	// timeoutRecovery is populated only after this destination has been matched
	// to the L2 source that owns its attestors and Cosmos-side wasm client.
	timeoutRecovery *services.L2TimeoutRecovery
	// hasReturnLeg gates the owed-ack ledger: unless the l2_to_cosmos source
	// settles for THIS destination, nothing ever clears a debt it records.
	hasReturnLeg bool
}

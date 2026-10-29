package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"relayer/services"
)

// spectreModule is one of the three contracts a SpectreClient is constructed
// with, named by the config key an operator would have to fix.
type spectreModule struct {
	key  string
	addr common.Address
}

// codeReader is the one read the module check makes. *ethclient.Client has it.
type codeReader interface {
	CodeAt(ctx context.Context, account common.Address, blockNumber *big.Int) ([]byte, error)
}

// spectreSourceLabel names the config module whose addresses create-clients-eth
// deploys with: selectCosmosSource promotes a cosmos_to_l2 module into
// CosmosToEthConfig when the file has no cosmos_to_eth one.
func spectreSourceLabel(cfg *appConfig) string {
	if cfg.HasCosmosToEth {
		return "cosmos_to_eth"
	}
	return "cosmos_to_l2"
}

// spectreModulesOf lists the modules in the order the SpectreClient constructor
// takes them. It reads them from the contract set the deploy will pass, not from
// the config the set was built from, so what is checked is what is deployed.
func spectreModulesOf(label string, contracts services.EVMContracts) []spectreModule {
	return []spectreModule{
		{key: label + ".update_client", addr: contracts.UpdateClient},
		{key: label + ".membership", addr: contracts.Membership},
		{key: label + ".misbehaviour", addr: contracts.Misbehaviour},
	}
}

// checkSpectreModulesBeforeDeploy is the check create-clients-eth runs before it
// spends gas on a SpectreClient.
func checkSpectreModulesBeforeDeploy(ctx context.Context, cfg *appConfig, evm services.EVMEndpoint) error {
	return requireSpectreModulesDeployed(
		ctx, evm.EthClient(), cfg.CosmosToEthConfig.EthRpcUrl, spectreModulesOf(spectreSourceLabel(cfg), evm.Contracts))
}

// requireSpectreModulesDeployed refuses to go on unless every module is set and
// holds code on the chain the SpectreClient is about to be deployed to.
//
// A SpectreClient keeps its modules as immutables and reads a call to one of them
// that does not revert as "verified". A call to an address without code succeeds
// and returns nothing, so a module that is missing accepts whatever it is shown:
// without a misbehaviour module, two identical headers and a quorum in the proof
// metadata freeze the client with no proof checked (#568). The constructor now
// rejects such a module; this stops the deploy before it spends the gas, and says
// which config key to fix.
//
// An empty field is the case that matters most: common.HexToAddress("") is the
// zero address, so a module left out of the config reaches the constructor as one.
//
// A node that does not answer fails the check. The startup validation tolerates
// that, because its relay loops go on retrying; this runs once before an
// irreversible deploy, and "could not read" must not be mistaken for "present".
//
// Every problem is collected before returning, so a config with three wrong keys
// takes one run to diagnose rather than three.
func requireSpectreModulesDeployed(ctx context.Context, reader codeReader, rpcURL string, modules []spectreModule) error {
	var problems []error
	for _, m := range modules {
		if m.addr == (common.Address{}) {
			// Not asked about: the node would answer "no code" for the zero address,
			// which hides the real cause.
			problems = append(problems, fmt.Errorf("%s is not set (or is the zero address)", m.key))
			continue
		}
		code, err := codeAt(ctx, reader, m.addr)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("%s: could not read the code at %s from %s: %w", m.key, m.addr.Hex(), rpcURL, err))
		case len(code) == 0:
			problems = append(problems, fmt.Errorf("%s: %s holds no code on %s", m.key, m.addr.Hex(), rpcURL))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to deploy a SpectreClient, whose modules cannot be changed afterwards "+
		"and which treats a call to a module without code as a verified proof:\n%w", errors.Join(problems...))
}

// codeAt reads the code at the latest block, bounded like every other config
// probe so a rate-limited endpoint cannot hang the command.
func codeAt(ctx context.Context, reader codeReader, addr common.Address) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, configProbeTimeout)
	defer cancel()
	return reader.CodeAt(ctx, addr, nil)
}

package evm

import (
	"context"
	"fmt"
	"time"

	"relayer/chain"
	"relayer/services"
)

// BeaconBuilder is the "beacon" chain.Builder for an Ethereum L1
// source: it assembles the sync-committee + storage-proof update that the
// destination's 08-wasm beacon light client verifies (ETH L1 -> Cosmos). It does
// NO relayer-side proving or re-execution — the wasm client verifies BLS +
// Merkle itself. It exposes the JSON headers returned by services.BuildEthClientUpdateHeaders.
//
// Holds *services.Worker plus the beacon endpoint and on-chain client identifiers
// it reads, so it is constructed by the wiring, not the cfg-only registry.
type BeaconBuilder struct {
	worker   *services.Worker
	cosmos   services.CosmosEndpoint
	evm      services.EVMEndpoint
	clientID string
}

// NewBeaconBuilder wires the builder to the worker and the endpoints it reads.
func NewBeaconBuilder(worker *services.Worker, cosmos services.CosmosEndpoint, evm services.EVMEndpoint, clientID string) *BeaconBuilder {
	return &BeaconBuilder{worker: worker, cosmos: cosmos, evm: evm, clientID: clientID}
}

func (b *BeaconBuilder) Name() string { return "beacon" }

// Build runs the existing beacon update pipeline and packages the result as a
// chain.ClientUpdate.
//
// Height is the update's EXECUTION block number (not the beacon SigSlot): the
// RelayModule compares it against packet event heights, which are execution block
// numbers, so the append-only guard and the provability guard stay in one number
// space. Each payload is the JSON header the Cosmos-side wasm client verifies; the
// destination derives its pre-submit timing requirement from those headers.
//
// A "no update needed" result still reports the current execution block with an
// empty payload list, so the module learns the client's real height (seeding the provability
// guard) without submitting a tx. BuildEthClientUpdateHeaders returns the current
// EthClientState even on the no-op path, so this is always available.
func (b *BeaconBuilder) Build(ctx context.Context, _ []byte) (chain.ClientUpdate, error) {
	result, err := b.worker.BuildEthClientUpdateHeaders(ctx, b.cosmos, b.evm, b.clientID)
	if err != nil {
		return chain.ClientUpdate{}, fmt.Errorf("beacon: build eth client update: %w", err)
	}
	if result.EthClientState == nil {
		return chain.ClientUpdate{}, fmt.Errorf("beacon: build returned nil client state")
	}
	execBlock := result.EthClientState.LatestExecutionBlockNumber
	trustedAt := time.Time{}
	if result.ProofTimestamp > 0 {
		trustedAt = time.Unix(int64(result.ProofTimestamp), 0)
	}
	if len(result.Headers) == 0 {
		return chain.ClientUpdate{Height: execBlock, TrustedAt: trustedAt}, nil // client already current — learn height, no tx
	}
	return chain.ClientUpdate{Height: execBlock, Payloads: result.Headers, TrustedAt: trustedAt}, nil
}

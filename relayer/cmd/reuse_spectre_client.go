package main

import (
	"context"
	"fmt"
	"math"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	routerContract "relayer/bindings/ICS26Router"
	spectreContract "relayer/bindings/SpectreClient"
	relayerclient "relayer/client"
	"relayer/services"
)

// spectreClientIsReusable reports whether a SpectreClient already deployed at the
// configured address can be kept, given its own view of the world and the Cosmos
// chain the relayer is actually pointed at. A nil error means reuse it.
//
// The question this answers is NOT "is there a contract here". That was the old
// check, and it is nearly content-free: a contract address derives from
// deployer+nonce, so the same key deploying the same contract on a second chain
// lands identical bytecode at the identical address. Observed on Sepolia — the
// same address held byte-identical SpectreClient code on both Base and OP.
//
// It is also not enough to compare chain ids. A local devnet re-created by
// run_cosmos_node.sh keeps its chain-id ("test-ibc-eth"), so a client pinned to
// the previous incarnation looks correct by name while trusting a consensus
// state from a chain that no longer exists. The trusted height being above the
// live chain's head is a useful early signal, but a replacement chain can grow
// past that height. reusableSpectreClientAt therefore also verifies the stored
// consensus-state hash against one rebuilt from the live chain at that height.
func spectreClientIsReusable(onChain relayerclient.ClientState, cosmosChainID string, cosmosHeight int64) error {
	if onChain.ChainId == "" {
		return fmt.Errorf(
			"the contract there reports an empty chain id, so it is not a SpectreClient tracking %q "+
				"(contract addresses derive from deployer+nonce, so an unrelated deployment can land at the same address)",
			cosmosChainID,
		)
	}
	if onChain.ChainId != cosmosChainID {
		return fmt.Errorf(
			"it tracks Cosmos chain %q but this config relays %q",
			onChain.ChainId, cosmosChainID,
		)
	}
	if onChain.IsFrozen {
		return fmt.Errorf("it is frozen by a misbehaviour submission and can no longer be updated")
	}
	if trusted := onChain.LatestHeight.RevisionHeight; cosmosHeight >= 0 && trusted > uint64(cosmosHeight) {
		return fmt.Errorf(
			"it trusts height %d but %s is only at height %d, so that chain was re-created since the client was deployed "+
				"(same chain id, new genesis) and the pinned validator set no longer exists",
			trusted, cosmosChainID, cosmosHeight,
		)
	}
	return nil
}

// consensusStateHashAt rebuilds the exact ABI-encoded consensus state that a
// SpectreClient stores for a Cosmos height, then applies the same keccak256 hash
// convention as the Solidity client.
func consensusStateHashAt(ctx context.Context, cosmosClient *rpchttp.HTTP, height uint64) ([32]byte, error) {
	if height > math.MaxInt64 {
		return [32]byte{}, fmt.Errorf("trusted height %d exceeds CometBFT's int64 height range", height)
	}

	lightBlock, err := relayerclient.GetLightBlock(ctx, cosmosClient, int64(height))
	if err != nil {
		return [32]byte{}, fmt.Errorf("get light block at height %d: %w", height, err)
	}
	consensusState, err := relayerclient.ConsensusStateFromLightBlock(lightBlock)
	if err != nil {
		return [32]byte{}, fmt.Errorf("rebuild consensus state at height %d: %w", height, err)
	}
	encoded, err := relayerclient.EncodeConsensusState(consensusState)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode consensus state at height %d: %w", height, err)
	}
	return crypto.Keccak256Hash(encoded), nil
}

// consensusStateHashIsReusable rejects a state which has the right chain id
// and a reachable height but was committed by a different Cosmos history. That
// is the case after a same-chain-id devnet reset has grown past the old client.
func consensusStateHashIsReusable(storedHash, liveHash [32]byte, cosmosChainID string, height uint64) error {
	if storedHash != liveHash {
		return fmt.Errorf(
			"it stores consensus state hash %x at trusted height %d, but %s now commits %x there; "+
				"the chain was re-created or diverged, so the client cannot be updated safely",
			storedHash, height, cosmosChainID, liveHash,
		)
	}
	return nil
}

// reusableSpectreClientAt decides whether the configured spectre_client address
// holds a light client this run can keep. It returns a reason string when the
// address must not be reused, so the caller can log why it is deploying again.
func reusableSpectreClientAt(
	evm services.EVMEndpoint,
	cosmosClient *rpchttp.HTTP,
	addr common.Address,
	clientID string,
	wasmClientID string,
) (bool, string) {
	code, err := evm.EthClient().CodeAt(context.Background(), addr, nil)
	if err != nil {
		return false, fmt.Sprintf("could not read code at %s: %v", addr.Hex(), err)
	}
	if len(code) == 0 {
		return false, fmt.Sprintf("nothing is deployed at %s", addr.Hex())
	}

	spectre, err := spectreContract.NewContractSpectreClient(addr, evm.EthClient())
	if err != nil {
		return false, fmt.Sprintf("could not bind a SpectreClient at %s: %v", addr.Hex(), err)
	}
	stateBytes, err := spectre.GetClientState(nil)
	if err != nil {
		return false, fmt.Sprintf(
			"the contract at %s does not answer getClientState (%v), so it is not a SpectreClient", addr.Hex(), err)
	}
	onChain, err := relayerclient.DecodeClientState(stateBytes)
	if err != nil {
		return false, fmt.Sprintf("could not decode the client state at %s: %v", addr.Hex(), err)
	}

	status, err := cosmosClient.Status(context.Background())
	if err != nil {
		return false, fmt.Sprintf("could not read the Cosmos chain id and height to check it against: %v", err)
	}

	if err := spectreClientIsReusable(
		onChain,
		status.NodeInfo.Network,
		status.SyncInfo.LatestBlockHeight,
	); err != nil {
		return false, err.Error()
	}

	storedConsensusStateHash, err := spectre.GetConsensusStateHash(nil, onChain.LatestHeight.RevisionHeight)
	if err != nil {
		return false, fmt.Sprintf(
			"the SpectreClient at %s cannot reveal its stored consensus-state hash at trusted height %d (%v), "+
				"so it cannot be safely verified for reuse",
			addr.Hex(), onChain.LatestHeight.RevisionHeight, err,
		)
	}
	liveConsensusStateHash, err := consensusStateHashAt(
		context.Background(), cosmosClient, onChain.LatestHeight.RevisionHeight,
	)
	if err != nil {
		return false, fmt.Sprintf(
			"could not rebuild the live Cosmos consensus state at trusted height %d to verify the SpectreClient: %v",
			onChain.LatestHeight.RevisionHeight, err,
		)
	}
	if err := consensusStateHashIsReusable(
		storedConsensusStateHash,
		liveConsensusStateHash,
		status.NodeInfo.Network,
		onChain.LatestHeight.RevisionHeight,
	); err != nil {
		return false, err.Error()
	}

	// The client can be healthy and still not be wired to the router this config
	// points at: re-running deploy_l2_contracts.sh mints a NEW ICS26Router, and
	// skipping the deploy here means that router never gets its AddClient. The
	// client id is then unregistered, so nothing relays — and because the skip
	// path returns before AddClient, no amount of re-running fixes it. Checking
	// the registration is what makes "skip the deploy" safe.
	if reason := routerPointsAtClient(evm, clientID, addr, wasmClientID); reason != "" {
		return false, reason
	}
	return true, ""
}

// routerWiring is what the ICS26Router reports about one client id: the
// implementation address it resolves to, and the Cosmos client id registered as
// its counterparty.
type routerWiring struct {
	client       common.Address
	counterparty string
}

// routerWiringIsReusable reports whether the router's registration matches what
// this config expects. Split out from the RPC so the decision is testable — the
// address check shipped untested and was wrong in a way a unit test would have
// caught immediately.
//
// BOTH halves matter, and only one used to be checked:
//
//   - the address answers "is this router wired to the SpectreClient we are
//     about to reuse" — a redeployed router has no registration at all.
//   - the counterparty answers "is it wired to the Cosmos client this config
//     relays". AddClient/MigrateClient register {ClientId: wasmClientID}, and
//     create-clients-cosmos deliberately mints a FRESH wasm client id. So
//     re-running the Cosmos step leaves a healthy SpectreClient, correctly
//     registered at the right address, pointing at a counterparty that no
//     longer exists. Skipping the deploy then returns success on a path where
//     every packet reverts, and re-running never repairs it.
func routerWiringIsReusable(w routerWiring, addr common.Address, wasmClientID string) error {
	if w.client != addr {
		return fmt.Errorf("it has that client id pointing at %s, not %s", w.client.Hex(), addr.Hex())
	}
	if w.counterparty != wasmClientID {
		return fmt.Errorf(
			"it has that client id counterparty-wired to Cosmos client %q, but this config relays %q "+
				"(re-running create-clients-cosmos mints a new wasm client id, and the router still points at the old one)",
			w.counterparty, wasmClientID)
	}
	return nil
}

// routerPointsAtClient returns "" when the ICS26Router in this config has
// clientID registered against addr AND counterparty-wired to wasmClientID, and
// a reason otherwise.
//
// A check that cannot run DECLINES rather than passing. This is a safety check
// on a "skip the deploy" path, so an unverifiable state must not be read as a
// verified-good one: redeploying costs one transaction, while wrongly reusing
// produces a client that looks healthy and relays nothing.
func routerPointsAtClient(evm services.EVMEndpoint, clientID string, addr common.Address, wasmClientID string) string {
	// RouterContract returns a pointer into EVMContracts, so it is never nil —
	// the nil arm of the old check was unreachable. An unconfigured router shows
	// up as the ZERO ADDRESS (ics26_address absent => HexToAddress("")), which is
	// the case actually worth naming.
	router := evm.RouterContract()
	if router == nil || *router == (common.Address{}) {
		return "no ICS26Router address is configured (ics26_address), so the router wiring behind that client cannot be verified"
	}
	if clientID == "" {
		return "ics26_client_id is empty, so the router wiring behind that client cannot be verified"
	}
	if wasmClientID == "" {
		return "no Cosmos wasm client id is known, so the router's counterparty wiring cannot be verified"
	}
	ics26, err := routerContract.NewContractICS26Router(*router, evm.EthClient())
	if err != nil {
		return fmt.Sprintf("could not bind the ICS26Router at %s: %v", router.Hex(), err)
	}
	registered, err := ics26.GetClient(nil, clientID)
	if err != nil {
		// getClient reverts when the id was never added — the router-was-replaced
		// case, and the one worth naming precisely.
		return fmt.Sprintf(
			"the router at %s has no client %q registered, so it was never wired to this SpectreClient "+
				"(a redeployed router needs a fresh AddClient)", router.Hex(), clientID)
	}
	counterparty, err := ics26.GetCounterparty(nil, clientID)
	if err != nil {
		return fmt.Sprintf(
			"the router at %s has no counterparty registered for %q: %v", router.Hex(), clientID, err)
	}
	if err := routerWiringIsReusable(
		routerWiring{client: registered, counterparty: counterparty.ClientId},
		addr, wasmClientID,
	); err != nil {
		return fmt.Sprintf("the router at %s: %v", router.Hex(), err)
	}
	return ""
}

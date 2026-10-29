package cosmos

import (
	"context"
	"fmt"
	"time"

	"relayer/chain"
	"relayer/chain/codec"
	"relayer/services"
)

// Groth16Builder is the "groth16" chain.Builder for a Cosmos source:
// it produces a ZK Tendermint update that the destination's SpectreClient
// verifies. It WRAPS the existing services.BuildCosmosClientUpdateMsg pipeline
// (extract signatures -> select pinned quorum -> GenerateProof -> assemble) with
// zero changes to the prover I/O.
//
// It holds a *services.Worker and the endpoints/configuration needed to read the
// pinned validator set and trusted height from the on-chain SpectreClient. It is
// therefore constructed by the composition root, which owns those — the only way
// adapters are built here.
//
// Note: BuildCosmosClientUpdateMsg fetches its own light block, so Build
// ignores the header argument for the Cosmos path.
type Groth16Builder struct {
	worker            *services.Worker
	cosmos            services.CosmosEndpoint
	evm               services.EVMEndpoint
	fetchTimeout      time.Duration
	rotationThreshold string
	proofType         string
	trustLevel        string
}

// NewGroth16Builder wires the builder to the worker and the endpoints it reads.
func NewGroth16Builder(worker *services.Worker, cosmos services.CosmosEndpoint, evm services.EVMEndpoint, fetchTimeout time.Duration, rotationThreshold, proofType, trustLevel string) *Groth16Builder {
	return &Groth16Builder{worker: worker, cosmos: cosmos, evm: evm, fetchTimeout: fetchTimeout, rotationThreshold: rotationThreshold, proofType: proofType, trustLevel: trustLevel}
}

func (b *Groth16Builder) Name() string { return "groth16" }

// Build runs the existing Cosmos update pipeline and packages the result as a
// chain.ClientUpdate. A "no update needed" result still reports the current
// on-chain trusted Cosmos height with a nil payload, so the module learns the
// client's real height (seeding the provability guard) without submitting a tx.
func (b *Groth16Builder) Build(ctx context.Context, _ []byte) (chain.ClientUpdate, error) {
	return b.build(ctx, false)
}

// BuildRotation is Build with forceRotation=true: the update re-pins the
// validator set (updateConsensusState) and so refreshes the snapshot the
// SpectreClient measures expiry from. The engine calls it from the
// expiry-driven refresh, where an updateApplicationState would succeed without
// moving the expiry at all (#541).
func (b *Groth16Builder) BuildRotation(ctx context.Context, _ []byte) (chain.ClientUpdate, error) {
	return b.build(ctx, true)
}

var _ chain.RotatingBuilder = (*Groth16Builder)(nil)

func (b *Groth16Builder) build(ctx context.Context, forceRotation bool) (chain.ClientUpdate, error) {
	readCtx, cancel := context.WithTimeout(ctx, b.fetchTimeout)
	defer cancel()
	trustedBlock, err := services.FetchOnChainTrustedHeight(readCtx, b.evm)
	if err != nil {
		return chain.ClientUpdate{}, fmt.Errorf("groth16: on-chain trusted height: %w", err)
	}
	if trustedBlock < 0 {
		return chain.ClientUpdate{}, fmt.Errorf("groth16: negative trusted height %d", trustedBlock)
	}
	result, err := b.worker.BuildCosmosClientUpdateMsg(ctx, b.cosmos, b.evm, b.fetchTimeout, b.rotationThreshold, b.proofType, trustedBlock, b.trustLevel, forceRotation, 0)
	if err != nil {
		return chain.ClientUpdate{}, fmt.Errorf("groth16: build cosmos update: %w", err)
	}
	return groth16Update(result, trustedBlock)
}

// groth16Update turns the pipeline's result into the engine's ClientUpdate.
//
// Split out of Build because everything above it needs two live endpoints and
// everything below it is a pure mapping -- including whether the update is an
// intermediate hop, which decides whether the engine climbs again. Left inline,
// that field could stop being carried and nothing would be red.
func groth16Update(result *services.CosmosClientUpdateBuildResult, trustedBlock int64) (chain.ClientUpdate, error) {
	if result == nil {
		return chain.ClientUpdate{}, fmt.Errorf("groth16: builder returned no result")
	}
	if !result.HasMsg || result.LightBlock == nil {
		// Client already current — report the trusted height so the module can seed
		// its provability guard; nil payload means "no tx".
		update := chain.ClientUpdate{Height: uint64(trustedBlock)}
		if result.LightBlock != nil {
			update.TrustedAt = result.LightBlock.SignedHeader.Header.Time
		}
		return update, nil
	}
	if result.LightBlock.BlockHeight < 0 {
		return chain.ClientUpdate{}, fmt.Errorf("groth16: negative block height %d", result.LightBlock.BlockHeight)
	}

	payload, err := codec.EncodeCosmosUpdate(int(result.Kind), result.AppMsg, result.NewValSet)
	if err != nil {
		return chain.ClientUpdate{}, fmt.Errorf("groth16: encode update: %w", err)
	}
	return chain.ClientUpdate{
		Height: uint64(result.LightBlock.BlockHeight), Payloads: [][]byte{payload},
		TrustedAt: result.LightBlock.SignedHeader.Header.Time,
		// Carried through rather than inferred. "The update landed below the height
		// we asked for" is true of a hop AND of an adapter that simply cannot prove
		// that far yet, and looping on the second one spins.
		IsHop: result.IsHop,
	}, nil
}

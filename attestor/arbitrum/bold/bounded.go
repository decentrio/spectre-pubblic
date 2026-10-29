package bold

import (
	"context"
	"math/big"

	arbitrum "attestor/arbitrum"
	attestortypes "attestor/types"

	"github.com/ethereum/go-ethereum/common"
)

// callBound is the bound every upstream call of the run loop is made under, and
// the beat that goes with it: the loop beats as each call returns, so a pass
// that is making progress is alive however long it runs.
func (a *AssertionAttestor) callBound() attestortypes.CallBound {
	return attestortypes.CallBound{
		Timeout: a.config.CallTimeout,
		Beat:    func() { a.heartbeat.Beat(a.now()) },
	}
}

// l1 is the L1 source with every call bounded. The loop reaches the source only
// through it.
func (a *AssertionAttestor) l1() AssertionSource {
	return boundedSource{inner: a.source, bound: a.callBound()}
}

// nitro is the Nitro commitment resolver with every call bounded.
func (a *AssertionAttestor) nitro() CommitmentResolver {
	return boundedResolver{inner: a.runtime, bound: a.callBound()}
}

// boundedSource bounds each call to an AssertionSource. It names the inner
// source in a field and lists every method, rather than embedding the
// interface, so a method added to AssertionSource breaks the build here instead
// of being promoted from the inner source without a bound.
type boundedSource struct {
	inner AssertionSource
	bound attestortypes.CallBound
}

var _ AssertionSource = boundedSource{}

func (b boundedSource) Head(ctx context.Context, head arbitrum.L1IngestHead) (L1Block, error) {
	return attestortypes.Call(ctx, b.bound, "L1 Head", func(ctx context.Context) (L1Block, error) {
		return b.inner.Head(ctx, head)
	})
}

func (b boundedSource) CanonicalHash(ctx context.Context, number uint64) (common.Hash, error) {
	return attestortypes.Call(ctx, b.bound, "L1 CanonicalHash", func(ctx context.Context) (common.Hash, error) {
		return b.inner.CanonicalHash(ctx, number)
	})
}

func (b boundedSource) AssertionStatusAtHash(
	ctx context.Context,
	proposal arbitrum.ProposedAssertion,
	blockHash common.Hash,
) (uint8, error) {
	return attestortypes.Call(ctx, b.bound, "L1 AssertionStatusAtHash", func(ctx context.Context) (uint8, error) {
		return b.inner.AssertionStatusAtHash(ctx, proposal, blockHash)
	})
}

func (b boundedSource) FinalizedBlockNumber(ctx context.Context) (uint64, error) {
	return attestortypes.Call(ctx, b.bound, "L1 FinalizedBlockNumber", b.inner.FinalizedBlockNumber)
}

func (b boundedSource) ChainIDs(ctx context.Context, block uint64) (l1ChainID, l2ChainID *big.Int, err error) {
	err = b.bound.Do(ctx, "L1 ChainIDs", func(ctx context.Context) error {
		var callErr error
		l1ChainID, l2ChainID, callErr = b.inner.ChainIDs(ctx, block)
		return callErr
	})
	return l1ChainID, l2ChainID, err
}

func (b boundedSource) Assertions(
	ctx context.Context,
	from, to uint64,
) (proposals []arbitrum.ProposedAssertion, confirmations []ConfirmedAssertion, err error) {
	err = b.bound.Do(ctx, "L1 Assertions", func(ctx context.Context) error {
		var callErr error
		proposals, confirmations, callErr = b.inner.Assertions(ctx, from, to)
		return callErr
	})
	return proposals, confirmations, err
}

func (b boundedSource) AssertionStatus(
	ctx context.Context,
	proposal arbitrum.ProposedAssertion,
	finalizedL1Block uint64,
) (uint8, error) {
	return attestortypes.Call(ctx, b.bound, "L1 AssertionStatus", func(ctx context.Context) (uint8, error) {
		return b.inner.AssertionStatus(ctx, proposal, finalizedL1Block)
	})
}

// boundedResolver bounds each call to a CommitmentResolver. One call is up to
// three Nitro requests, and the bound covers all of them.
type boundedResolver struct {
	inner CommitmentResolver
	bound attestortypes.CallBound
}

var _ CommitmentResolver = boundedResolver{}

func (b boundedResolver) ResolveCanonicalBlockHash(
	ctx context.Context,
	blockHash common.Hash,
	mode arbitrum.RunMode,
) (arbitrum.BlockCommitment, error) {
	return attestortypes.Call(ctx, b.bound, "Nitro ResolveCanonicalBlockHash", func(ctx context.Context) (arbitrum.BlockCommitment, error) {
		return b.inner.ResolveCanonicalBlockHash(ctx, blockHash, mode)
	})
}

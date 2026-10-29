package opstack

import (
	"context"

	attestortypes "attestor/types"
)

// callBound is the bound every upstream call of the run loop is made under, and
// the beat that goes with it: the loop beats as each call returns, so a pass that
// is making progress is alive however long it runs.
func (a *OpStackAttestor) callBound() attestortypes.CallBound {
	return attestortypes.CallBound{
		Timeout: a.cfg.CallTimeout,
		Beat:    func() { a.heartbeat.Beat(a.now()) },
	}
}

// loopReplica is the replica with every call bounded, for the run loop. Only the
// loop uses it: a beat from a request that a relayer made through HeadAt or
// CommitmentAt would say nothing about whether the loop is making progress, so
// those keep the replica as it was handed to New.
func (a *OpStackAttestor) loopReplica() replica {
	return boundedReplica{inner: a.replica, bound: a.callBound()}
}

// loopGames is the dispute-game source with every call bounded, for the run loop.
// Callers check that a.games is set first.
func (a *OpStackAttestor) loopGames() gameSource {
	return boundedGames{inner: a.games, bound: a.callBound()}
}

// challengeHook calls the challenge hook under the call bound. The hook is the
// one place the loop calls out that is not behind the replica or the game
// source, and it is where a transaction to challenge a bad proposal will be
// sent, so it must not be able to park the loop either. A hook that fails or is
// cut is logged by the caller and does not undo the verdict.
func (a *OpStackAttestor) challengeHook(ctx context.Context, game ProposedRoot, local [32]byte) error {
	return a.callBound().Do(ctx, "challenge hook", func(ctx context.Context) error {
		return a.hook.OnMismatch(ctx, game, local)
	})
}

// boundedReplica bounds each call to a replica. It lists every method rather
// than embedding the interface, so a method added to replica breaks the build
// here instead of being promoted without a bound.
type boundedReplica struct {
	inner replica
	bound attestortypes.CallBound
}

var _ replica = boundedReplica{}

func (b boundedReplica) SyncStatus(ctx context.Context) (SyncStatus, error) {
	return attestortypes.Call(ctx, b.bound, "op-node SyncStatus", b.inner.SyncStatus)
}

func (b boundedReplica) OutputAtBlock(ctx context.Context, l2Block uint64) ([32]byte, error) {
	return attestortypes.Call(ctx, b.bound, "op-node OutputAtBlock", func(ctx context.Context) ([32]byte, error) {
		return b.inner.OutputAtBlock(ctx, l2Block)
	})
}

func (b boundedReplica) CommitmentAt(ctx context.Context, l2Block uint64) (L2Commitment, error) {
	return attestortypes.Call(ctx, b.bound, "op-node CommitmentAt", func(ctx context.Context) (L2Commitment, error) {
		return b.inner.CommitmentAt(ctx, l2Block)
	})
}

// boundedGames bounds each call to a gameSource, and to the finalized view it
// hands out, which is what ingestion and bootstrap read through.
type boundedGames struct {
	inner gameSource
	bound attestortypes.CallBound
}

var _ gameSource = boundedGames{}

func (b boundedGames) GameCount(ctx context.Context) (uint64, error) {
	return attestortypes.Call(ctx, b.bound, "factory GameCount", b.inner.GameCount)
}

func (b boundedGames) GameAtIndex(ctx context.Context, index uint64) (ProposedRoot, error) {
	return attestortypes.Call(ctx, b.bound, "factory GameAtIndex", func(ctx context.Context) (ProposedRoot, error) {
		return b.inner.GameAtIndex(ctx, index)
	})
}

func (b boundedGames) FinalizedView(ctx context.Context) (gameSource, uint64, error) {
	var (
		view             gameSource
		finalizedL1Block uint64
	)
	err := b.bound.Do(ctx, "factory FinalizedView", func(ctx context.Context) error {
		var callErr error
		view, finalizedL1Block, callErr = b.inner.FinalizedView(ctx)
		return callErr
	})
	if err != nil || view == nil {
		return view, finalizedL1Block, err
	}
	return boundedGames{inner: view, bound: b.bound}, finalizedL1Block, nil
}

// Package adapter exposes OP Stack attestors through the domain-neutral core
// ports. It owns no settlement logic or persistence; opstack remains the
// bounded context for factory ingestion, reconciliation, and its JSON state.
package adapter

import (
	"context"
	"fmt"
	"time"

	"attestor/core"
	"attestor/optimism/opstack"
	"attestor/types/attestation"

	"github.com/ethereum/go-ethereum/common"
)

const defaultWatchPoll = 2 * time.Second

// Options controls only adapter behavior, never OP settlement behavior.
type Options struct {
	WatchPoll time.Duration
}

// Adapter bridges one OP source to core ports.
type Adapter struct {
	attestor  *opstack.OpStackAttestor
	signer    attestation.Signer
	watchPoll time.Duration
}

// New builds a bridge for one configured OP source. A missing signer is kept
// observable at VerifyStateRoot time as FailedPrecondition so read-only
// diagnostics can still be composed in tests; production composition always
// provides one.
func New(attestor *opstack.OpStackAttestor, signer attestation.Signer, options Options) (*Adapter, error) {
	if attestor == nil {
		return nil, fmt.Errorf("OP attestor must not be nil")
	}
	if attestor.SrcChain() == "" {
		return nil, fmt.Errorf("OP attestor src_chain must not be empty")
	}
	poll := options.WatchPoll
	if poll == 0 {
		poll = defaultWatchPoll
	}
	if poll < 0 {
		return nil, fmt.Errorf("watch poll interval must not be negative")
	}
	return &Adapter{attestor: attestor, signer: signer, watchPoll: poll}, nil
}

// Run delegates lifecycle ownership to the OP plugin.
func (a *Adapter) Run(ctx context.Context) error {
	return a.attestor.Run(ctx)
}

// AttestedUpTo returns a durable OP feed snapshot.
func (a *Adapter) AttestedUpTo(_ context.Context, policy core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	root, found := a.attestor.Store().HighestAttestedAtOrBelow(^uint64(0), policy.IncludeProvisional)
	if !found {
		return core.AttestedRoot{}, false, nil
	}
	return toCoreRoot(root), true, nil
}

// AttestedRootAtOrBelow returns a durable OP feed snapshot bounded by height.
func (a *Adapter) AttestedRootAtOrBelow(_ context.Context, height uint64, policy core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	root, found := a.attestor.Store().HighestAttestedAtOrBelow(height, policy.IncludeProvisional)
	if !found {
		return core.AttestedRoot{}, false, nil
	}
	return toCoreRoot(root), true, nil
}

// Status reads the last local replica snapshot without triggering refresh. The
// route is ready once a snapshot exists and the run loop that refreshes it is
// still coming back around: a loop parked on a call that never returns leaves
// the snapshot, and so the feed, frozen, and must not read as serving.
func (a *Adapter) Status(_ context.Context) (core.FeedStatus, error) {
	status := core.FeedStatus{
		SrcChain:        a.attestor.SrcChain(),
		AttestationHead: core.RunMode(a.attestor.Head()),
		Pending:         uint64(len(a.attestor.Store().Pending())),
	}
	if snapshot, found := a.attestor.LastSyncStatus(); found {
		status.Ready = a.attestor.RunnerAlive()
		status.ReplicaSeen = true
		status.ReplicaUnsafe = snapshot.UnsafeL2
		status.ReplicaSafe = snapshot.SafeL2
		status.ReplicaFinalized = snapshot.FinalizedL2
	}
	return status, nil
}

// VerifyStateRoot binds a candidate header identity to the OP replica. It
// returns valid=false for an identity mismatch and classified errors for every
// transport/retry distinction the shared gRPC adapter exposes.
func (a *Adapter) VerifyStateRoot(ctx context.Context, request core.BlockIdentityRequest) (core.SignedBlockIdentityVerdict, error) {
	if !request.RunMode.Valid() {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorInvalidArgument, "run_mode must be unsafe, safe or finalized", nil)
	}
	configuredHead := core.RunMode(a.attestor.Head())
	if request.RunMode != configuredHead {
		return core.SignedBlockIdentityVerdict{}, core.NewError(
			core.ErrorFailedPrecondition,
			fmt.Sprintf("requested run_mode %s does not match configured attestation head %s", request.RunMode, configuredHead),
			nil,
		)
	}
	head, err := a.attestor.HeadAt(ctx, opstack.Head(configuredHead))
	if err != nil {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorUnavailable, fmt.Sprintf("read replica %s head", configuredHead), err)
	}
	if request.BlockNumber > head {
		return core.SignedBlockIdentityVerdict{}, core.NewError(
			core.ErrorFailedPrecondition,
			fmt.Sprintf("block %d is above the replica %s head %d", request.BlockNumber, configuredHead, head),
			nil,
		)
	}
	commitment, err := a.attestor.CommitmentAt(ctx, request.BlockNumber)
	if err != nil {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorUnavailable, fmt.Sprintf("query replica block %d", request.BlockNumber), err)
	}
	verdict := core.SignedBlockIdentityVerdict{
		BlockNumber: commitment.BlockNumber,
		BlockHash:   [32]byte(commitment.BlockHash),
		StateRoot:   [32]byte(commitment.StateRoot),
	}
	verdict.Valid = commitment.StateRoot == common.Hash(request.ExpectedStateRoot)
	if request.ExpectedBlockHash != nil {
		verdict.Valid = verdict.Valid && commitment.BlockHash == common.Hash(*request.ExpectedBlockHash)
	}
	if !verdict.Valid {
		return verdict, nil
	}
	if !a.signer.Configured() {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorUnimplemented, "attestation signer is not configured", nil)
	}
	signature, err := a.signer.Sign(request.L2Router[:], request.AttestorSetHash[:], commitment.BlockNumber, commitment.BlockHash[:], commitment.StateRoot[:])
	if err != nil {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorInternal, fmt.Sprintf("sign canonical L2 block %d", commitment.BlockNumber), err)
	}
	verdict.Signature = signature
	return verdict, nil
}

// WatchFrontier adapts the existing OP polling stream semantics. Each update
// is an authoritative frontier; an absent frontier resets the comparison so a
// later lower root is emitted. It does not mutate the OP store.
func (a *Adapter) WatchFrontier(ctx context.Context, request core.WatchRequest) (<-chan core.FrontierUpdate, error) {
	updates := make(chan core.FrontierUpdate, 1)
	go func() {
		defer close(updates)
		var previous core.AttestedRoot
		sent := false
		emit := func() bool {
			current, found, err := a.AttestedUpTo(ctx, request.Policy)
			if err != nil || !found {
				sent = false
				return true
			}
			if sent && sameFrontier(previous, current) {
				return true
			}
			select {
			case updates <- core.FrontierUpdate{Root: current.Clone(), Found: true}:
				previous = current
				sent = true
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !emit() {
			return
		}
		ticker := time.NewTicker(a.watchPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !emit() {
					return
				}
			}
		}
	}()
	return updates, nil
}

func toCoreRoot(root opstack.AttestedRoot) core.AttestedRoot {
	source := root.Source
	if source == "" {
		source = opstack.SourceGame
	}
	entry := core.AttestedRoot{
		L2BlockNumber: root.L2BlockNumber,
		Root:          append([]byte(nil), root.Root[:]...),
		Source:        source,
		Provisional:   root.Provisional,
		AttestedAt:    root.AttestedAt,
	}
	if source == opstack.SourceGame {
		gameIndex := root.GameIndex
		entry.GameIndex = &gameIndex
	}
	return entry
}

func sameFrontier(a, b core.AttestedRoot) bool {
	if a.L2BlockNumber != b.L2BlockNumber || a.Provisional != b.Provisional || len(a.Root) != len(b.Root) {
		return false
	}
	for index := range a.Root {
		if a.Root[index] != b.Root[index] {
			return false
		}
	}
	return true
}

var _ core.Runner = (*Adapter)(nil)
var _ core.FeedReader = (*Adapter)(nil)
var _ core.BlockVerifier = (*Adapter)(nil)
var _ core.StatusReader = (*Adapter)(nil)
var _ core.FrontierWatcher = (*Adapter)(nil)

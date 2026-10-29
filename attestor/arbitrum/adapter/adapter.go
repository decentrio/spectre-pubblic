// Package adapter exposes Arbitrum's Nitro/runtime and BoLD-owned feed through
// domain-neutral core ports. Assertion ingestion, derived-root reconciliation,
// and the persistent Arbitrum schema stay in their existing bounded contexts.
package adapter

import (
	"context"
	"fmt"

	"attestor/arbitrum"
	"attestor/core"
	"attestor/types/attestation"

	"github.com/ethereum/go-ethereum/common"
)

// FeedReader is the narrow Arbitrum-owned feed surface exposed to the bridge.
type FeedReader interface {
	HighestAttested(includeProvisional bool) (arbitrum.AttestedRoot, bool)
	HighestAttestedAtOrBelow(uint64, bool) (arbitrum.AttestedRoot, bool)
}

// Options wires one immutable src_chain route to its shared Nitro runtime and
// independently persisted feed.
type Options struct {
	SrcChain        string
	Runtime         *arbitrum.RuntimeState
	Feed            FeedReader
	AttestationHead arbitrum.RunMode
	Signer          attestation.Signer
	// RunnerAlive reports whether the loop that advances Feed is still coming
	// back around (AssertionAttestor.RunnerAlive). Nil leaves readiness to the
	// Nitro snapshot alone, for compositions with no such loop.
	RunnerAlive func() bool
}

// Adapter is the Arbitrum implementation of the common read/verify ports.
type Adapter struct {
	srcChain    string
	runtime     *arbitrum.RuntimeState
	feed        FeedReader
	head        core.RunMode
	signer      attestation.Signer
	runnerAlive func() bool
}

// New constructs a route bridge. The signer may be absent only for diagnostic
// test composition; positive VerifyStateRoot results then fail closed.
func New(options Options) (*Adapter, error) {
	if options.SrcChain == "" {
		return nil, fmt.Errorf("Arbitrum src_chain must not be empty")
	}
	if options.Runtime == nil {
		return nil, fmt.Errorf("Arbitrum runtime must not be nil")
	}
	if options.Feed == nil {
		return nil, fmt.Errorf("Arbitrum attested-root feed must not be nil")
	}
	head, err := options.AttestationHead.Normalize()
	if err != nil {
		return nil, err
	}
	return &Adapter{
		srcChain:    options.SrcChain,
		runtime:     options.Runtime,
		feed:        options.Feed,
		head:        core.RunMode(head),
		signer:      options.Signer,
		runnerAlive: options.RunnerAlive,
	}, nil
}

func (a *Adapter) AttestedUpTo(_ context.Context, policy core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	root, found := a.feed.HighestAttested(policy.IncludeProvisional)
	if !found {
		return core.AttestedRoot{}, false, nil
	}
	return toCoreRoot(root), true, nil
}

func (a *Adapter) AttestedRootAtOrBelow(_ context.Context, height uint64, policy core.AttestationPolicy) (core.AttestedRoot, bool, error) {
	root, found := a.feed.HighestAttestedAtOrBelow(height, policy.IncludeProvisional)
	if !found {
		return core.AttestedRoot{}, false, nil
	}
	return toCoreRoot(root), true, nil
}

// Status observes only RuntimeState's local snapshot and the feed loop's
// heartbeat. It deliberately does not issue a Nitro call, refresh heads, or
// mutate the feed. The route is ready once the configured head has been
// observed and the loop that advances the feed is still coming back around: a
// loop parked on a call that never returns leaves the feed frozen, and must
// not read as serving.
func (a *Adapter) Status(_ context.Context) (core.FeedStatus, error) {
	snapshot := a.runtime.Snapshot()
	status := core.FeedStatus{
		SrcChain:        a.srcChain,
		AttestationHead: a.head,
		ReplicaSeen:     snapshot.UnsafeObserved || snapshot.SafeObserved || snapshot.FinalizedSeen,
	}
	if snapshot.UnsafeObserved {
		status.ReplicaUnsafe = snapshot.Unsafe.BlockNumber
	}
	if snapshot.SafeObserved {
		status.ReplicaSafe = snapshot.Safe.BlockNumber
	}
	if snapshot.FinalizedSeen {
		status.ReplicaFinalized = snapshot.Finalized.BlockNumber
	}
	switch a.head {
	case core.RunModeUnsafe:
		status.Ready = snapshot.UnsafeObserved
	case core.RunModeSafe:
		status.Ready = snapshot.SafeObserved
	case core.RunModeFinalized:
		status.Ready = snapshot.FinalizedSeen
	}
	if a.runnerAlive != nil && !a.runnerAlive() {
		status.Ready = false
	}
	return status, nil
}

func (a *Adapter) VerifyStateRoot(ctx context.Context, request core.BlockIdentityRequest) (core.SignedBlockIdentityVerdict, error) {
	if !request.RunMode.Valid() {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorInvalidArgument, "run_mode must be unsafe, safe or finalized", nil)
	}
	if request.RunMode != a.head {
		return core.SignedBlockIdentityVerdict{}, core.NewError(
			core.ErrorFailedPrecondition,
			fmt.Sprintf("requested run_mode %s does not match configured attestation head %s", request.RunMode, a.head),
			nil,
		)
	}
	runMode := arbitrum.RunMode(a.head)
	head, err := a.runtime.Head(ctx, runMode)
	if err != nil {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorUnavailable, fmt.Sprintf("query Nitro %s head", a.head), err)
	}
	if request.BlockNumber > head.BlockNumber {
		return core.SignedBlockIdentityVerdict{}, core.NewError(
			core.ErrorFailedPrecondition,
			fmt.Sprintf("block %d is above Nitro %s head %d", request.BlockNumber, a.head, head.BlockNumber),
			nil,
		)
	}
	commitment, err := a.runtime.CommitmentAt(ctx, request.BlockNumber)
	if err != nil {
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorUnavailable, fmt.Sprintf("query Nitro block %d", request.BlockNumber), err)
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
		return core.SignedBlockIdentityVerdict{}, core.NewError(core.ErrorInternal, fmt.Sprintf("sign canonical Nitro block %d", commitment.BlockNumber), err)
	}
	verdict.Signature = signature
	return verdict, nil
}

func toCoreRoot(root arbitrum.AttestedRoot) core.AttestedRoot {
	entry := core.AttestedRoot{
		L2BlockNumber: root.L2BlockNumber,
		Root:          append([]byte(nil), root.Root[:]...),
		Source:        root.Source,
		Provisional:   root.Provisional,
		AttestedAt:    root.AttestedAt,
	}
	if root.AssertionHash != (common.Hash{}) {
		entry.AssertionHash = append([]byte(nil), root.AssertionHash[:]...)
	} else if root.Source == arbitrum.SourceGame {
		gameIndex := root.GameIndex
		entry.GameIndex = &gameIndex
	}
	return entry
}

var _ core.FeedReader = (*Adapter)(nil)
var _ core.BlockVerifier = (*Adapter)(nil)
var _ core.StatusReader = (*Adapter)(nil)

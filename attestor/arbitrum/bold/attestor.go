package bold

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	arbitrum "attestor/arbitrum"
	attestortypes "attestor/types"
	"attestor/types/redact"
	"github.com/ethereum/go-ethereum/common"
)

const (
	defaultAssertionPollInterval = 12 * time.Second
	defaultMaxL1BlockRange       = uint64(2_000)
)

// CommitmentResolver checks whether an assertion's L2 block hash is canonical
// in the configured Nitro view.
type CommitmentResolver interface {
	ResolveCanonicalBlockHash(
		context.Context,
		common.Hash,
		arbitrum.RunMode,
	) (arbitrum.BlockCommitment, error)
}

// AssertionAttestorConfig controls L1 assertion ingestion and reconciliation.
type AssertionAttestorConfig struct {
	L1IngestHead arbitrum.L1IngestHead
	PollInterval time.Duration
	// CallTimeout bounds each upstream call the loop makes, so a call that never
	// returns (a silent WebSocket peer) becomes a failed pass that is logged and
	// counted instead of parking the loop. It bounds the call and not the pass:
	// reconciliation starts from the first pending entry on every pass, and a
	// deadline on the whole pass could be spent on the same prefix each time.
	// Zero selects attestortypes.DefaultCallTimeout.
	CallTimeout     time.Duration
	MaxL1BlockRange uint64
}

// AssertionAttestor turns RollupCore events into a Nitro-checked
// commitment feed.
type AssertionAttestor struct {
	source  AssertionSource
	runtime CommitmentResolver
	store   *arbitrum.AttestedRootStore
	config  AssertionAttestorConfig
	now     func() time.Time
	syncMu  sync.Mutex
	// heartbeat is beaten by the Run goroutine each time its loop comes back
	// around, and read by the sidecar's readiness probe (see RunnerAlive).
	heartbeat attestortypes.Heartbeat
}

// NewAssertionAttestor constructs an assertion ingestion loop.
func NewAssertionAttestor(
	source AssertionSource,
	runtime CommitmentResolver,
	store *arbitrum.AttestedRootStore,
	config AssertionAttestorConfig,
) (*AssertionAttestor, error) {
	if source == nil {
		return nil, errors.New("assertion source must not be nil")
	}
	if runtime == nil {
		return nil, errors.New("Nitro commitment resolver must not be nil")
	}
	if store == nil {
		return nil, errors.New("attested-root store must not be nil")
	}
	head, err := config.L1IngestHead.Normalize()
	if err != nil {
		return nil, err
	}
	config.L1IngestHead = head
	if config.PollInterval == 0 {
		config.PollInterval = defaultAssertionPollInterval
	}
	if config.PollInterval < 0 {
		return nil, errors.New("assertion poll interval must be greater than zero")
	}
	if config.CallTimeout == 0 {
		config.CallTimeout = attestortypes.DefaultCallTimeout
	}
	if config.CallTimeout < 0 {
		return nil, errors.New("assertion call timeout must be greater than zero")
	}
	if config.MaxL1BlockRange == 0 {
		config.MaxL1BlockRange = defaultMaxL1BlockRange
	}
	return &AssertionAttestor{
		source:  source,
		runtime: runtime,
		store:   store,
		config:  config,
		now:     time.Now,
	}, nil
}

// ValidateChainIDs fails closed if either RPC endpoint or RollupCore belongs to
// a different chain than the configured deployment.
func (a *AssertionAttestor) ValidateChainIDs(
	ctx context.Context,
	expectedL1ChainID uint64,
	expectedL2ChainID uint64,
) error {
	finalizedL1Block, err := a.l1().FinalizedBlockNumber(ctx)
	if err != nil {
		return err
	}
	l1ChainID, l2ChainID, err := a.l1().ChainIDs(ctx, finalizedL1Block)
	if err != nil {
		return err
	}
	if l1ChainID == nil || l2ChainID == nil {
		return errors.New("assertion source returned a nil chain ID")
	}
	expectedL1 := new(big.Int).SetUint64(expectedL1ChainID)
	expectedL2 := new(big.Int).SetUint64(expectedL2ChainID)
	if l1ChainID.Cmp(expectedL1) != 0 {
		return fmt.Errorf(
			"L1 chain ID mismatch: connected=%s configured=%s",
			l1ChainID,
			expectedL1,
		)
	}
	if l2ChainID.Cmp(expectedL2) != 0 {
		return fmt.Errorf(
			"RollupCore L2 chain ID mismatch: contract=%s configured=%s",
			l2ChainID,
			expectedL2,
		)
	}
	return nil
}

// Run continuously advances the verified assertion feed until cancellation.
// Transient L1 or Nitro failures leave the last persisted frontier intact and
// are retried on the next tick.
func (a *AssertionAttestor) Run(ctx context.Context) {
	a.RunWithObserver(ctx, nil)
}

// RunnerAlive reports whether Run's loop is making progress: it has come back
// around, or one of its upstream calls has returned, recently enough. A loop
// parked on something that never returns stops beating, so this goes false even
// though nothing has failed and nothing is logged. A loop whose passes keep
// failing still beats: those failures are logged and counted. So does a pass that
// runs for a long time over a long list, because it beats as each call returns.
func (a *AssertionAttestor) RunnerAlive() bool {
	return a.heartbeat.Alive(a.now(), attestortypes.FeedStaleAfter(a.config.CallTimeout, a.config.PollInterval))
}

// RunWithObserver is Run with an optional per-pass callback for composition
// roots. The callback observes only the pass outcome; it cannot alter
// settlement ingestion or the durable feed.
//
// Every upstream call a pass makes runs under config.CallTimeout, so no call can
// park the loop. A call that hits its deadline fails the pass like any other
// error, to be logged and observed, and the next tick retries from the durable
// state.
func (a *AssertionAttestor) RunWithObserver(ctx context.Context, observe func(error)) {
	a.heartbeat.Beat(a.now())
	sync := func() {
		err := a.SyncOnce(ctx)
		if err != nil && ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("Arbitrum assertion attestation failed: %v", redact.Error(err))
		}
		if observe != nil {
			observe(err)
		}
		a.heartbeat.Beat(a.now())
	}
	sync()

	ticker := time.NewTicker(a.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sync()
		}
	}
}

// SyncOnce advances the finalized log cursor, reconciles finalized evidence,
// and optionally rebuilds the complete speculative L1 window.
func (a *AssertionAttestor) SyncOnce(ctx context.Context) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	var finalizedSnapshot L1Block
	var finalizedL1Block uint64
	var err error
	if a.config.L1IngestHead == arbitrum.L1IngestFinalized {
		finalizedL1Block, err = a.l1().FinalizedBlockNumber(ctx)
	} else {
		finalizedSnapshot, err = a.l1().Head(ctx, arbitrum.L1IngestFinalized)
		finalizedL1Block = finalizedSnapshot.Number
	}
	if err != nil {
		return err
	}

	// L1 finality never moves backwards, and the cursor only ever advances to a
	// block at or below a finalized read, so a read below cursor-1 came from a
	// lagging or failed-over backend. Acting on it would skip reconciliation of
	// every entry above it and then prune, as unfinalized, the assertions
	// already ingested between it and the cursor. The cursor does not move back
	// and the speculative window starts at max(finalized+1, cursor), so they
	// would not be read again. Fail the pass before touching anything.
	if cursor := a.store.NextL1Block(); cursor > 0 && finalizedL1Block < cursor-1 {
		return fmt.Errorf(
			"finalized L1 block %d is behind the assertion cursor %d: L1 finality does not move backwards, "+
				"so the L1 backend answered from behind; skipping this pass so the feed is not pruned against it",
			finalizedL1Block,
			cursor,
		)
	}

	// Track both retained and rejected assertions at this finalized snapshot.
	checked := make(map[common.Hash]bool)

	// Validate persisted speculative work before advancing the cursor across
	// its creation blocks. Otherwise a failed recheck followed by a restart
	// could disguise an unchecked early root as finalized-cursor history.
	if a.store.HasUnfinalizedAssertions() {
		if err := a.reconcileProposals(ctx, finalizedL1Block, checked); err != nil {
			return err
		}
		if err := a.reconcileProvisional(ctx, finalizedL1Block, checked); err != nil {
			return err
		}
	}

	for from := a.store.NextL1Block(); from <= finalizedL1Block; {
		to := finalizedL1Block
		if remaining := finalizedL1Block - from; remaining >= a.config.MaxL1BlockRange {
			to = from + a.config.MaxL1BlockRange - 1
		}
		proposals, confirmations, err := a.l1().Assertions(ctx, from, to)
		if err != nil {
			return err
		}
		if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
			for _, proposal := range proposals {
				// Do not resurrect work rejected at this finalized snapshot.
				if retained, done := checked[proposal.AssertionHash]; done && !retained {
					continue
				}
				if err := store.RecordProposal(proposal); err != nil {
					return false, err
				}
			}
			for _, confirmation := range confirmations {
				if err := store.MarkAssertionConfirmed(
					confirmation.AssertionHash,
					confirmation.L2BlockHash,
				); err != nil {
					return false, err
				}
			}
			store.SetNextL1Block(to + 1)
			return true, nil
		}); err != nil {
			return err
		}
		if to == finalizedL1Block {
			break
		}
		from = to + 1
	}

	if err := a.reconcileProposals(ctx, finalizedL1Block, checked); err != nil {
		return err
	}
	if err := a.reconcileProvisional(ctx, finalizedL1Block, checked); err != nil {
		return err
	}
	if a.config.L1IngestHead != arbitrum.L1IngestFinalized {
		return a.ingestUnfinalized(ctx, finalizedSnapshot)
	}
	var revoked uint64
	if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
		revoked = store.PruneUnfinalizedAssertions(finalizedL1Block, nil)
		return revoked != 0, nil
	}); err != nil {
		return err
	}
	a.store.ObserveAssertionIngest(finalizedL1Block, revoked)
	if revoked != 0 {
		log.Printf("Arbitrum unfinalized assertions withdrawn: l1_ingest_head=finalized l1_head=%d count=%d", finalizedL1Block, revoked)
	}
	return nil
}

func (a *AssertionAttestor) reconcileProposals(
	ctx context.Context,
	finalizedL1Block uint64,
	checked map[common.Hash]bool,
) error {
	for _, proposal := range a.store.Proposals() {
		if _, done := checked[proposal.AssertionHash]; proposal.L1BlockNumber > finalizedL1Block || done {
			continue
		}
		// Share the set across proposal and provisional reconciliation so a
		// newly attested proposal is not checked again in the same pass.
		checked[proposal.AssertionHash] = true
		status, err := a.l1().AssertionStatus(
			ctx,
			proposal,
			finalizedL1Block,
		)
		if err != nil {
			return err
		}
		switch status {
		case assertionStatusNone:
			checked[proposal.AssertionHash] = false
			if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
				store.RemoveAssertion(proposal.AssertionHash)
				return true, nil
			}); err != nil {
				return err
			}
			continue
		case assertionStatusConfirmed:
			proposal.Confirmed = true
		case assertionStatusPending:
			if proposal.Confirmed {
				return fmt.Errorf(
					"assertion %s has a finalized confirmation event but pending status",
					proposal.AssertionHash,
				)
			}
		default:
			return fmt.Errorf(
				"assertion %s has unsupported status %d",
				proposal.AssertionHash,
				status,
			)
		}

		commitment, provisional, err := a.resolveProposal(ctx, proposal)
		switch {
		case errors.Is(err, arbitrum.ErrCommitmentNotReady):
			if proposal.Confirmed {
				if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
					return true, store.MarkAssertionConfirmed(proposal.AssertionHash, proposal.L2BlockHash)
				}); err != nil {
					return err
				}
			}
			continue
		case errors.Is(err, arbitrum.ErrCommitmentMismatch):
			checked[proposal.AssertionHash] = false
			if err := a.rejectAssertion(proposal.AssertionHash, proposal.L2BlockHash, err); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		}
		if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
			return true, store.RecordAssertionAttestation(proposal, commitment, a.now(), provisional)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *AssertionAttestor) resolveProposal(
	ctx context.Context,
	proposal arbitrum.ProposedAssertion,
) (arbitrum.BlockCommitment, bool, error) {
	if proposal.Confirmed {
		commitment, err := a.nitro().ResolveCanonicalBlockHash(
			ctx,
			proposal.L2BlockHash,
			arbitrum.RunModeFinalized,
		)
		if err == nil {
			return commitment, false, nil
		}
		if !errors.Is(err, arbitrum.ErrCommitmentNotReady) {
			return arbitrum.BlockCommitment{}, false, err
		}
	}
	commitment, err := a.nitro().ResolveCanonicalBlockHash(
		ctx,
		proposal.L2BlockHash,
		arbitrum.RunModeSafe,
	)
	if err != nil {
		return arbitrum.BlockCommitment{}, false, err
	}
	return commitment, true, nil
}

func (a *AssertionAttestor) reconcileProvisional(
	ctx context.Context,
	finalizedL1Block uint64,
	checked map[common.Hash]bool,
) error {
	for _, entry := range a.store.ProvisionalAssertions() {
		if _, done := checked[entry.AssertionHash]; entry.L1BlockNumber > finalizedL1Block || done {
			continue
		}
		checked[entry.AssertionHash] = true
		status, err := a.l1().AssertionStatus(
			ctx,
			arbitrum.ProposedAssertion{
				AssertionHash: entry.AssertionHash,
				L2BlockHash:   entry.L2BlockHash,
			},
			finalizedL1Block,
		)
		if err != nil {
			return err
		}
		if status == assertionStatusNone {
			checked[entry.AssertionHash] = false
			if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
				store.RemoveAssertion(entry.AssertionHash)
				return true, nil
			}); err != nil {
				return err
			}
			continue
		}
		if status != assertionStatusPending && status != assertionStatusConfirmed {
			return fmt.Errorf(
				"assertion %s has unsupported status %d",
				entry.AssertionHash,
				status,
			)
		}

		_, err = a.nitro().ResolveCanonicalBlockHash(
			ctx,
			entry.L2BlockHash,
			arbitrum.RunModeSafe,
		)
		switch {
		case errors.Is(err, arbitrum.ErrCommitmentNotReady):
			continue
		case errors.Is(err, arbitrum.ErrCommitmentMismatch):
			checked[entry.AssertionHash] = false
			if err := a.rejectAssertion(entry.AssertionHash, entry.L2BlockHash, err); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		}
		if status != assertionStatusConfirmed {
			continue
		}
		_, err = a.nitro().ResolveCanonicalBlockHash(
			ctx,
			entry.L2BlockHash,
			arbitrum.RunModeFinalized,
		)
		switch {
		case errors.Is(err, arbitrum.ErrCommitmentNotReady):
			if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
				return true, store.MarkAssertionConfirmed(entry.AssertionHash, entry.L2BlockHash)
			}); err != nil {
				return err
			}
			continue
		case errors.Is(err, arbitrum.ErrCommitmentMismatch):
			checked[entry.AssertionHash] = false
			if err := a.rejectAssertion(entry.AssertionHash, entry.L2BlockHash, err); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		}
		if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
			if err := store.MarkAssertionConfirmed(entry.AssertionHash, entry.L2BlockHash); err != nil {
				return false, err
			}
			store.PromoteAssertion(entry.AssertionHash)
			return true, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *AssertionAttestor) rejectAssertion(
	assertionHash common.Hash,
	l2BlockHash common.Hash,
	err error,
) error {
	if commitErr := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
		store.RecordAssertionMismatch(assertionHash, l2BlockHash, err.Error(), a.now())
		return true, nil
	}); commitErr != nil {
		return commitErr
	}
	log.Printf(
		"Arbitrum assertion rejected: assertion_hash=%s l2_block_hash=%s reason=%v",
		assertionHash,
		l2BlockHash,
		err,
	)
	return nil
}

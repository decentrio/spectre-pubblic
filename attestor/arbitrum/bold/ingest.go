package bold

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"

	arbitrum "attestor/arbitrum"
	"github.com/ethereum/go-ethereum/common"
)

type speculativeAssertion struct {
	proposal   arbitrum.ProposedAssertion
	commitment arbitrum.BlockCommitment
	ready      bool
	mismatch   error
}

// ingestUnfinalized rebuilds the whole non-finalized window. It never advances
// the durable log cursor and never trusts confirmations from this window.
func (a *AssertionAttestor) ingestUnfinalized(ctx context.Context, finalized L1Block) error {
	head, err := a.l1().Head(ctx, a.config.L1IngestHead)
	if err != nil {
		return err
	}
	if head.Number < finalized.Number {
		return fmt.Errorf("%s L1 head %d is behind finalized snapshot %d", a.config.L1IngestHead, head.Number, finalized.Number)
	}
	var proposals []arbitrum.ProposedAssertion
	if finalized.Number != math.MaxUint64 {
		from := finalized.Number + 1
		if cursor := a.store.NextL1Block(); cursor > from {
			from = cursor
		}
		for from <= head.Number {
			to := head.Number
			if head.Number-from >= a.config.MaxL1BlockRange {
				to = from + a.config.MaxL1BlockRange - 1
			}
			batch, _, err := a.l1().Assertions(ctx, from, to)
			if err != nil {
				return err
			}
			for _, proposal := range batch {
				if proposal.L1BlockNumber < from || proposal.L1BlockNumber > to {
					return fmt.Errorf("assertion %s has L1 block %d outside ingest range %d..%d", proposal.AssertionHash, proposal.L1BlockNumber, from, to)
				}
				proposals = append(proposals, proposal)
			}
			if to == head.Number {
				break
			}
			from = to + 1
		}
	}

	present := make(map[common.Hash]bool)
	results := make([]speculativeAssertion, 0, len(proposals))
	for _, proposal := range proposals {
		status, err := a.l1().AssertionStatusAtHash(ctx, proposal, head.Hash)
		if err != nil {
			return err
		}
		if status == assertionStatusNone {
			continue
		}
		if status != assertionStatusPending && status != assertionStatusConfirmed {
			return fmt.Errorf("assertion %s has unsupported status %d", proposal.AssertionHash, status)
		}
		proposal.Confirmed = false
		commitment, _, err := a.resolveProposal(ctx, proposal)
		result := speculativeAssertion{proposal: proposal, commitment: commitment}
		switch {
		case errors.Is(err, arbitrum.ErrCommitmentNotReady):
			present[proposal.AssertionHash] = true
		case errors.Is(err, arbitrum.ErrCommitmentMismatch):
			result.mismatch = err
		case err != nil:
			return err
		default:
			result.ready = true
			present[proposal.AssertionHash] = true
		}
		results = append(results, result)
	}

	// Numeric log ranges must still belong to the snapshots used above.
	// Check immediately before committing, without holding the save lock.
	for _, snapshot := range []L1Block{finalized, head} {
		hash, err := a.l1().CanonicalHash(ctx, snapshot.Number)
		if err != nil {
			return err
		}
		if hash != snapshot.Hash {
			return fmt.Errorf("L1 snapshot changed at block %d; retry assertion ingestion", snapshot.Number)
		}
	}

	var revoked uint64
	if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
		revoked = store.PruneUnfinalizedAssertions(finalized.Number, present)
		for _, result := range results {
			if store.IsFinalizedAssertion(result.proposal.AssertionHash) {
				log.Printf("Arbitrum speculative replay of finalized assertion skipped: %s", result.proposal.AssertionHash)
				continue
			}
			if result.mismatch != nil {
				store.RecordAssertionMismatch(result.proposal.AssertionHash, result.proposal.L2BlockHash, result.mismatch.Error(), a.now())
				continue
			}
			if err := store.RecordProposal(result.proposal); err != nil {
				return false, err
			}
			if result.ready {
				if err := store.RecordAssertionAttestation(result.proposal, result.commitment, a.now(), true); err != nil {
					return false, err
				}
			}
		}
		return revoked != 0 || len(results) != 0, nil
	}); err != nil {
		return err
	}
	a.store.ObserveAssertionIngest(head.Number, revoked)
	if revoked != 0 {
		log.Printf("Arbitrum unfinalized assertions withdrawn: l1_ingest_head=%s l1_head=%d count=%d", a.config.L1IngestHead, head.Number, revoked)
	}
	return nil
}

// ReconcileStartup durably discards unchecked early assertions before serving.
// The background loop rebuilds them without delaying startup on backfill or RPCs.
func (a *AssertionAttestor) ReconcileStartup(_ context.Context) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
		return store.DropUnfinalizedAssertions() != 0, nil
	}); err != nil {
		return fmt.Errorf("discard persisted unfinalized assertions before serving: %w", err)
	}
	return nil
}

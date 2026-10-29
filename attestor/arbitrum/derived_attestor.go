package arbitrum

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// DerivedRuntime is the Nitro runtime surface used by proposal-independent
// attestations.
type DerivedRuntime interface {
	Snapshot() RuntimeSnapshot
	CommitmentAt(context.Context, uint64) (BlockCommitment, error)
}

// DerivedAttestorConfig mirrors the OP attestor's configured-head and feed
// retention controls.
type DerivedAttestorConfig struct {
	AttestationHead RunMode
	Disabled        bool
	GapBlocks       uint64
	MaxRoots        uint64
}

// DerivedRootAttestor publishes Nitro's configured head independently of the
// BoLD assertion cadence, then rechecks provisional roots after finalization.
type DerivedRootAttestor struct {
	runtime DerivedRuntime
	store   *AttestedRootStore
	config  DerivedAttestorConfig
	now     func() time.Time
}

// NewDerivedRootAttestor constructs the proposal-independent attestation pass.
func NewDerivedRootAttestor(
	runtime DerivedRuntime,
	store *AttestedRootStore,
	config DerivedAttestorConfig,
) (*DerivedRootAttestor, error) {
	if runtime == nil {
		return nil, errors.New("derived attestor runtime must not be nil")
	}
	if store == nil {
		return nil, errors.New("derived attestor store must not be nil")
	}
	mode, err := config.AttestationHead.Normalize()
	if err != nil {
		return nil, err
	}
	config.AttestationHead = mode
	if config.GapBlocks == 0 {
		return nil, errors.New("derived attestation gap must be greater than zero")
	}
	if config.MaxRoots == 0 {
		return nil, errors.New("derived root limit must be greater than zero")
	}
	return &DerivedRootAttestor{
		runtime: runtime,
		store:   store,
		config:  config,
		now:     time.Now,
	}, nil
}

// SyncOnce rechecks finalized provisional roots and self-attests the selected
// Nitro head. A root above finalized is provisional regardless of whether it
// came from the safe or unsafe head.
func (a *DerivedRootAttestor) SyncOnce(ctx context.Context) error {
	snapshot := a.runtime.Snapshot()
	if !snapshot.FinalizedSeen {
		return nil
	}

	var syncErrors []error
	for _, entry := range a.store.DerivedProvisionalAtOrBelow(snapshot.Finalized.BlockNumber) {
		commitment, err := a.runtime.CommitmentAt(ctx, entry.L2BlockNumber)
		if err != nil {
			if isNitroBlockNotFound(err) {
				var removed AttestedRoot
				var found bool
				commitErr := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
					removed, found = store.RemoveProvisionalDerived(entry.L2BlockNumber)
					return found, nil
				})
				if commitErr != nil {
					syncErrors = append(syncErrors, commitErr)
					continue
				}
				if found {
					log.Printf(
						"Arbitrum dropped unavailable provisional derived root: l2_block=%d block_hash=%s state_root=%s",
						removed.L2BlockNumber,
						removed.L2BlockHash,
						removed.Root,
					)
				}
				continue
			}
			syncErrors = append(syncErrors, fmt.Errorf(
				"reverify derived root at L2 block %d: %w",
				entry.L2BlockNumber,
				err,
			))
			log.Printf(
				"Arbitrum could not reverify provisional derived root; retaining it for retry: l2_block=%d err=%v",
				entry.L2BlockNumber,
				err,
			)
			continue
		}
		if commitment.StateRoot == entry.Root && commitment.BlockHash == entry.L2BlockHash {
			var confirmed bool
			commitErr := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				confirmed = store.ConfirmDerived(entry.L2BlockNumber)
				return confirmed, nil
			})
			if commitErr != nil {
				syncErrors = append(syncErrors, commitErr)
			} else if confirmed {
				log.Printf("Arbitrum finalized head confirmed derived root: l2_block=%d state_root=%s", entry.L2BlockNumber, entry.Root)
			}
		} else {
			log.Printf(
				"Arbitrum HEAD DIVERGENCE: l2_block=%d provisional_block_hash=%s provisional_state_root=%s finalized_block_hash=%s finalized_state_root=%s",
				entry.L2BlockNumber,
				entry.L2BlockHash,
				entry.Root,
				commitment.BlockHash,
				commitment.StateRoot,
			)
			if commitment.BlockNumber != entry.L2BlockNumber {
				syncErrors = append(syncErrors, fmt.Errorf(
					"reverify derived root at L2 block %d: Nitro returned commitment for L2 block %d",
					entry.L2BlockNumber,
					commitment.BlockNumber,
				))
				continue
			}
			var corrected bool
			commitErr := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				corrected = store.CorrectDerived(entry.L2BlockNumber, commitment, a.now())
				return corrected, nil
			})
			if commitErr != nil {
				syncErrors = append(syncErrors, commitErr)
			} else if !corrected {
				syncErrors = append(syncErrors, fmt.Errorf(
					"reverify derived root at L2 block %d: provisional entry disappeared before correction",
					entry.L2BlockNumber,
				))
			}
		}
	}

	gate, observed := snapshotHead(snapshot, a.config.AttestationHead)
	if observed {
		var removedRoots []AttestedRoot
		commitErr := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
			removedRoots = store.RemoveProvisionalDerivedAbove(gate.BlockNumber)
			return len(removedRoots) != 0, nil
		})
		if commitErr != nil {
			syncErrors = append(syncErrors, commitErr)
		}
		for _, removed := range removedRoots {
			log.Printf(
				"Arbitrum removed derived root above regressed %s head: l2_block=%d block_hash=%s state_root=%s",
				a.config.AttestationHead,
				removed.L2BlockNumber,
				removed.L2BlockHash,
				removed.Root,
			)
		}
	}

	if !a.config.Disabled {
		if observed && gate.BlockNumber != 0 && a.shouldAttest(gate) {
			provisional := gate.BlockNumber > snapshot.Finalized.BlockNumber
			if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
				return true, store.AppendDerived(gate, a.now(), provisional)
			}); err != nil {
				syncErrors = append(syncErrors, err)
			} else {
				log.Printf(
					"Arbitrum derived root attested: l2_block=%d block_hash=%s state_root=%s head=%s provisional=%t",
					gate.BlockNumber,
					gate.BlockHash,
					gate.StateRoot,
					a.config.AttestationHead,
					provisional,
				)
			}
		}
	}

	var pruned uint64
	if err := a.store.Commit(func(store *AttestedRootStore) (bool, error) {
		pruned = store.PruneDerived(a.config.MaxRoots)
		return pruned != 0, nil
	}); err != nil {
		syncErrors = append(syncErrors, err)
	} else if pruned != 0 {
		log.Printf("Arbitrum pruned %d confirmed derived roots beyond cap %d", pruned, a.config.MaxRoots)
	}
	return errors.Join(syncErrors...)
}

func (a *DerivedRootAttestor) shouldAttest(commitment BlockCommitment) bool {
	last, found := a.store.HighestDerivedBlock()
	if !found {
		return true
	}
	if commitment.BlockNumber == last {
		entry, found := a.store.DerivedAt(last)
		return found && entry.Provisional &&
			(entry.Root != commitment.StateRoot || entry.L2BlockHash != commitment.BlockHash)
	}
	if commitment.BlockNumber < last {
		return false
	}
	return commitment.BlockNumber-last >= a.config.GapBlocks
}

func snapshotHead(snapshot RuntimeSnapshot, mode RunMode) (BlockCommitment, bool) {
	switch mode {
	case RunModeUnsafe:
		return snapshot.Unsafe, snapshot.UnsafeObserved
	case RunModeSafe:
		return snapshot.Safe, snapshot.SafeObserved
	default:
		return snapshot.Finalized, snapshot.FinalizedSeen
	}
}

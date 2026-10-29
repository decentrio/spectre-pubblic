package arbitrum

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const (
	// SourceGame marks an OP commitment selected from a dispute game.
	SourceGame = "game"

	// SourceAssertion marks an Arbitrum commitment selected from a RollupCore
	// assertion and checked against the configured Nitro RPC endpoint.
	SourceAssertion = "assertion"

	// SourceDerived marks a proposal-independent root read directly from the
	// configured Nitro head.
	SourceDerived = "derived"

	attestedRootStateVersion = 1
)

// AttestedRoot is one Nitro-checked commitment exposed to relayers.
// Rollup-specific identifiers are kept beside the generic height/root fields so
// the header builder can resolve the L1 object it must prove.
type AttestedRoot struct {
	L2BlockNumber uint64      `json:"l2_block_number"`
	Root          common.Hash `json:"root"`
	Source        string      `json:"source"`
	GameIndex     uint64      `json:"game_index,omitempty"`
	AssertionHash common.Hash `json:"assertion_hash,omitempty"`
	Provisional   bool        `json:"provisional,omitempty"`
	AttestedAt    time.Time   `json:"attested_at"`

	// Arbitrum-only verification metadata. These fields are persisted so a
	// provisional entry can be rechecked and promoted without trusting the
	// relayer to provide its provenance.
	L2BlockHash        common.Hash `json:"l2_block_hash,omitempty"`
	L1BlockNumber      uint64      `json:"l1_block_number,omitempty"`
	AssertionConfirmed bool        `json:"assertion_confirmed,omitempty"`
}

// ProposedAssertion is a BoLD AssertionCreated event waiting for
// the configured Nitro endpoint to reach and verify its committed L2 block.
type ProposedAssertion struct {
	AssertionHash    common.Hash `json:"assertion_hash"`
	ParentHash       common.Hash `json:"parent_hash"`
	L2BlockHash      common.Hash `json:"l2_block_hash"`
	InboxAccumulator common.Hash `json:"inbox_accumulator"`
	L1BlockNumber    uint64      `json:"l1_block_number"`
	Confirmed        bool        `json:"confirmed,omitempty"`
}

// AssertionMismatch is a terminal local verdict that an L1 assertion did not
// match the canonical block returned by Nitro.
type AssertionMismatch struct {
	AssertionHash common.Hash `json:"assertion_hash"`
	L2BlockHash   common.Hash `json:"l2_block_hash"`
	Reason        string      `json:"reason"`
	RecordedAt    time.Time   `json:"recorded_at"`
}

type persistedAttestedRootState struct {
	Version        uint32              `json:"version"`
	SrcChain       string              `json:"src_chain"`
	SourceIdentity string              `json:"source_identity,omitempty"`
	NextL1Block    uint64              `json:"next_l1_block"`
	Proposals      []ProposedAssertion `json:"proposals,omitempty"`
	Attested       []AttestedRoot      `json:"attested,omitempty"`
	Mismatches     []AssertionMismatch `json:"mismatches,omitempty"`
}

// attestedRootFile is the narrow filesystem surface used by the atomic writer.
// Keeping it injectable lets the failure matrix exercise every durability step
// without relying on a full or failing local filesystem.
type attestedRootFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type attestedRootFileSystem interface {
	MkdirAll(string, os.FileMode) error
	CreateTemp(string, string) (attestedRootFile, error)
	Rename(string, string) error
	Remove(string) error
	SyncDir(string) error
}

type osAttestedRootFileSystem struct{}

func (osAttestedRootFileSystem) MkdirAll(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}

func (osAttestedRootFileSystem) CreateTemp(dir, pattern string) (attestedRootFile, error) {
	return os.CreateTemp(dir, pattern)
}

func (osAttestedRootFileSystem) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func (osAttestedRootFileSystem) Remove(path string) error {
	return os.Remove(path)
}

func (osAttestedRootFileSystem) SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// BindSourceIdentity pins the durable feed to one concrete rollup deployment.
// Existing pre-identity state is upgraded in place; a conflicting identity
// fails closed so stale roots cannot be served after a config change.
func (s *AttestedRootStore) BindSourceIdentity(identity string) error {
	if identity == "" {
		return errors.New("attested-root source identity must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.SourceIdentity == "" {
		s.state.SourceIdentity = identity
		return nil
	}
	if s.state.SourceIdentity != identity {
		return fmt.Errorf(
			"attested-root source identity %q does not match configured %q",
			s.state.SourceIdentity,
			identity,
		)
	}
	return nil
}

// AttestedRootStore persists the assertion ingest cursor and independently
// verified feed. Writers mutate it from the Arbitrum attestation loop; gRPC
// readers use the RWMutex-protected frontier accessors.
type AttestedRootStore struct {
	mu      sync.RWMutex
	saveMu  sync.Mutex
	path    string
	state   persistedAttestedRootState
	fs      attestedRootFileSystem
	metrics *Metrics
}

// NewAttestedRootStore constructs an in-memory feed, primarily for tests.
func NewAttestedRootStore(srcChain string, startL1Block uint64) (*AttestedRootStore, error) {
	if srcChain == "" {
		return nil, errors.New("attested-root store src_chain must not be empty")
	}
	return &AttestedRootStore{
		state: persistedAttestedRootState{
			Version:     attestedRootStateVersion,
			SrcChain:    srcChain,
			NextL1Block: startL1Block,
		},
	}, nil
}

// LoadAttestedRootStore loads a durable feed. A missing file starts at
// startL1Block; corrupt or identity-mismatched files fail closed.
func LoadAttestedRootStore(path, srcChain string, startL1Block uint64) (*AttestedRootStore, error) {
	store, err := NewAttestedRootStore(srcChain, startL1Block)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("attested-root state path must not be empty")
	}
	store.path = path

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create attested-root state directory: %w", err)
		}
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read attested-root state %q: %w", path, err)
	}

	var state persistedAttestedRootState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode attested-root state %q: %w", path, err)
	}
	if state.Version != attestedRootStateVersion {
		return nil, fmt.Errorf(
			"unsupported attested-root state version %d, want %d",
			state.Version,
			attestedRootStateVersion,
		)
	}
	if state.SrcChain != srcChain {
		return nil, fmt.Errorf(
			"attested-root state src_chain %q does not match configured %q",
			state.SrcChain,
			srcChain,
		)
	}
	store.state = state
	return store, nil
}

// SetMetrics enables bounded persistence-failure and feed-state metrics. It is
// intentionally optional so the domain store remains usable in tests and
// tooling without a Prometheus registry.
func (s *AttestedRootStore) SetMetrics(metrics *Metrics) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = metrics
}

// Stats returns a stable, hash-free snapshot for operator metrics.
func (s *AttestedRootStore) Stats() AttestedRootStoreStats {
	if s == nil {
		return AttestedRootStoreStats{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	stats := AttestedRootStoreStats{
		NextL1Block:       s.state.NextL1Block,
		PendingAssertions: uint64(len(s.state.Proposals)),
		MismatchRecords:   uint64(len(s.state.Mismatches)),
	}
	for _, entry := range s.state.Attested {
		if entry.Provisional {
			stats.ProvisionalRoots++
			if entry.Source == SourceAssertion {
				stats.PendingAssertions++
			}
		}
	}
	return stats
}

// Commit applies a mutation to an isolated snapshot, persists that snapshot,
// then publishes it to readers. It serializes production writers so a delayed
// save cannot overwrite a newer assertion or derived-root transition.
func (s *AttestedRootStore) Commit(mutate func(*AttestedRootStore) (bool, error)) error {
	if s == nil {
		return errors.New("attested-root store is nil")
	}
	if mutate == nil {
		return errors.New("attested-root store mutation must not be nil")
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	path := s.path
	fs := s.fileSystem()
	metrics := s.metrics
	chain := s.state.SrcChain
	staged := &AttestedRootStore{
		path:  path,
		state: clonePersistedAttestedRootState(s.state),
		fs:    fs,
	}
	s.mu.RUnlock()
	changed, err := mutate(staged)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	staged.mu.RLock()
	data, err := json.MarshalIndent(staged.state, "", "  ")
	staged.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal attested-root state: %w", err)
	}
	if err := persistAttestedRootState(path, fs, data); err != nil {
		if metrics != nil {
			metrics.ObservePersistenceFailure(chain)
		}
		return err
	}
	s.mu.Lock()
	s.state = staged.state
	s.mu.Unlock()
	return nil
}

// Save atomically replaces the persistent state file from the current
// snapshot. New production mutations must use Commit so readers cannot see a
// successful in-memory mutation whose durable save failed.
func (s *AttestedRootStore) Save() (err error) {
	if s == nil {
		return errors.New("attested-root store is nil")
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	path := s.path
	fs := s.fileSystem()
	metrics := s.metrics
	chain := s.state.SrcChain
	data, err := json.MarshalIndent(s.state, "", "  ")
	s.mu.RUnlock()
	defer func() {
		if err != nil && metrics != nil {
			metrics.ObservePersistenceFailure(chain)
		}
	}()
	if err != nil {
		return fmt.Errorf("marshal attested-root state: %w", err)
	}
	return persistAttestedRootState(path, fs, data)
}

func clonePersistedAttestedRootState(state persistedAttestedRootState) persistedAttestedRootState {
	clone := state
	clone.Proposals = append([]ProposedAssertion(nil), state.Proposals...)
	clone.Attested = append([]AttestedRoot(nil), state.Attested...)
	clone.Mismatches = append([]AssertionMismatch(nil), state.Mismatches...)
	return clone
}

func persistAttestedRootState(path string, fs attestedRootFileSystem, data []byte) error {
	if path == "" {
		return nil
	}

	dir := filepath.Dir(path)
	if err := fs.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create attested-root state directory: %w", err)
	}
	file, err := fs.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary attested-root state: %w", err)
	}
	temporaryPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = fs.Remove(temporaryPath)
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("restrict temporary attested-root state: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary attested-root state: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary attested-root state: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = fs.Remove(temporaryPath)
		return fmt.Errorf("close temporary attested-root state: %w", err)
	}
	if err := fs.Rename(temporaryPath, path); err != nil {
		_ = fs.Remove(temporaryPath)
		return fmt.Errorf("replace attested-root state: %w", err)
	}
	if err := fs.SyncDir(dir); err != nil {
		return fmt.Errorf("sync attested-root state directory: %w", err)
	}
	return nil
}

func (s *AttestedRootStore) fileSystem() attestedRootFileSystem {
	if s.fs != nil {
		return s.fs
	}
	return osAttestedRootFileSystem{}
}

// AppendDerived records Nitro's own commitment at the configured attestation
// head. At most one derived entry is retained for an L2 height.
func (s *AttestedRootStore) AppendDerived(
	commitment BlockCommitment,
	now time.Time,
	provisional bool,
) error {
	if commitment.BlockHash == (common.Hash{}) {
		return errors.New("derived commitment block hash must not be zero")
	}
	if commitment.StateRoot == (common.Hash{}) {
		return errors.New("derived commitment state root must not be zero")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.state.Attested {
		entry := &s.state.Attested[index]
		if entry.Source != SourceDerived || entry.L2BlockNumber != commitment.BlockNumber {
			continue
		}
		if !entry.Provisional &&
			(entry.Root != commitment.StateRoot || entry.L2BlockHash != commitment.BlockHash) {
			return fmt.Errorf(
				"finalized derived commitment at height %d changed",
				commitment.BlockNumber,
			)
		}
		entry.Root = commitment.StateRoot
		entry.L2BlockHash = commitment.BlockHash
		entry.AttestedAt = now.UTC()
		entry.Provisional = provisional
		return nil
	}
	s.state.Attested = append(s.state.Attested, AttestedRoot{
		L2BlockNumber: commitment.BlockNumber,
		Root:          commitment.StateRoot,
		Source:        SourceDerived,
		Provisional:   provisional,
		AttestedAt:    now.UTC(),
		L2BlockHash:   commitment.BlockHash,
	})
	return nil
}

// HighestDerivedBlock returns the highest L2 height with a derived entry,
// including provisional entries.
func (s *AttestedRootStore) HighestDerivedBlock() (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best uint64
	found := false
	for _, entry := range s.state.Attested {
		if entry.Source == SourceDerived && (!found || entry.L2BlockNumber > best) {
			best = entry.L2BlockNumber
			found = true
		}
	}
	return best, found
}

// DerivedAt returns the derived entry at one exact L2 height.
func (s *AttestedRootStore) DerivedAt(height uint64) (AttestedRoot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.state.Attested {
		if entry.Source == SourceDerived && entry.L2BlockNumber == height {
			return entry, true
		}
	}
	return AttestedRoot{}, false
}

// RemoveProvisionalDerivedAbove drops unsafe/safe entries invalidated when the
// selected Nitro head regresses. Confirmed entries are never removed here.
func (s *AttestedRootStore) RemoveProvisionalDerivedAbove(height uint64) []AttestedRoot {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.state.Attested[:0]
	var removed []AttestedRoot
	for _, entry := range s.state.Attested {
		if entry.Source == SourceDerived && entry.Provisional && entry.L2BlockNumber > height {
			removed = append(removed, entry)
			continue
		}
		kept = append(kept, entry)
	}
	s.state.Attested = kept
	return removed
}

// RemoveProvisionalDerived drops one derived entry that can no longer be
// rechecked through the configured Nitro endpoint. Confirmed entries are never
// removed by this recovery path.
func (s *AttestedRootStore) RemoveProvisionalDerived(height uint64) (AttestedRoot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, entry := range s.state.Attested {
		if entry.Source != SourceDerived || !entry.Provisional || entry.L2BlockNumber != height {
			continue
		}
		s.state.Attested = append(s.state.Attested[:index], s.state.Attested[index+1:]...)
		return entry, true
	}
	return AttestedRoot{}, false
}

// DerivedProvisionalAtOrBelow returns the provisional derived entries now
// covered by Nitro's finalized head, oldest first.
func (s *AttestedRootStore) DerivedProvisionalAtOrBelow(finalized uint64) []AttestedRoot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var entries []AttestedRoot
	for _, entry := range s.state.Attested {
		if entry.Source == SourceDerived && entry.Provisional && entry.L2BlockNumber <= finalized {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].L2BlockNumber < entries[j].L2BlockNumber
	})
	return entries
}

// ConfirmDerived clears the provisional flag after the finalized Nitro view
// agrees with the previously observed commitment.
func (s *AttestedRootStore) ConfirmDerived(height uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.state.Attested {
		entry := &s.state.Attested[index]
		if entry.Source == SourceDerived && entry.L2BlockNumber == height && entry.Provisional {
			entry.Provisional = false
			return true
		}
	}
	return false
}

// CorrectDerived replaces a provisional commitment with Nitro's authoritative
// finalized commitment.
func (s *AttestedRootStore) CorrectDerived(height uint64, commitment BlockCommitment, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if commitment.BlockNumber != height {
		return false
	}
	for index := range s.state.Attested {
		entry := &s.state.Attested[index]
		if entry.Source != SourceDerived || entry.L2BlockNumber != height || !entry.Provisional {
			continue
		}
		entry.Root = commitment.StateRoot
		entry.L2BlockHash = commitment.BlockHash
		entry.AttestedAt = now.UTC()
		entry.Provisional = false
		return true
	}
	return false
}

// PruneDerived drops the oldest confirmed derived roots beyond max. It never
// removes assertion-backed or provisional entries.
func (s *AttestedRootStore) PruneDerived(max uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var confirmed uint64
	for _, entry := range s.state.Attested {
		if entry.Source == SourceDerived && !entry.Provisional {
			confirmed++
		}
	}
	if confirmed <= max {
		return 0
	}
	excess := confirmed - max
	kept := s.state.Attested[:0]
	var removed uint64
	for _, entry := range s.state.Attested {
		if removed < excess && entry.Source == SourceDerived && !entry.Provisional {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	s.state.Attested = kept
	return removed
}

// SrcChain returns the immutable source-chain identifier.
func (s *AttestedRootStore) SrcChain() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.SrcChain
}

// NextL1Block is the next finalized L1 block whose assertion logs must be
// ingested.
func (s *AttestedRootStore) NextL1Block() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.NextL1Block
}

// SetNextL1Block advances the finalized-log cursor.
func (s *AttestedRootStore) SetNextL1Block(next uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.NextL1Block = next
}

// RecordProposal idempotently records a hash-checked AssertionCreated event.
func (s *AttestedRootStore) RecordProposal(proposal ProposedAssertion) error {
	if proposal.AssertionHash == (common.Hash{}) {
		return errors.New("proposal assertion hash must not be zero")
	}
	if proposal.L2BlockHash == (common.Hash{}) {
		return errors.New("proposal L2 block hash must not be zero")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.state.Proposals {
		existing := &s.state.Proposals[index]
		if existing.AssertionHash != proposal.AssertionHash {
			continue
		}
		if existing.ParentHash != proposal.ParentHash ||
			existing.L2BlockHash != proposal.L2BlockHash ||
			existing.InboxAccumulator != proposal.InboxAccumulator {
			return fmt.Errorf("assertion %s was observed with conflicting event data", proposal.AssertionHash)
		}
		existing.Confirmed = existing.Confirmed || proposal.Confirmed
		existing.L1BlockNumber = proposal.L1BlockNumber
		return nil
	}
	for index := range s.state.Attested {
		existing := &s.state.Attested[index]
		if existing.AssertionHash == proposal.AssertionHash {
			if existing.L2BlockHash != proposal.L2BlockHash {
				return fmt.Errorf("attested assertion %s changed L2 block hash", proposal.AssertionHash)
			}
			existing.AssertionConfirmed = existing.AssertionConfirmed || proposal.Confirmed
			existing.L1BlockNumber = proposal.L1BlockNumber
			return nil
		}
	}
	s.state.Proposals = append(s.state.Proposals, proposal)
	return nil
}

// MarkAssertionConfirmed records a finalized AssertionConfirmed event.
func (s *AttestedRootStore) MarkAssertionConfirmed(assertionHash, l2BlockHash common.Hash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.state.Proposals {
		proposal := &s.state.Proposals[index]
		if proposal.AssertionHash != assertionHash {
			continue
		}
		if proposal.L2BlockHash != l2BlockHash {
			return fmt.Errorf(
				"confirmed assertion %s block hash %s does not match proposal %s",
				assertionHash,
				l2BlockHash,
				proposal.L2BlockHash,
			)
		}
		proposal.Confirmed = true
		return nil
	}
	for index := range s.state.Attested {
		entry := &s.state.Attested[index]
		if entry.AssertionHash != assertionHash {
			continue
		}
		if entry.L2BlockHash != l2BlockHash {
			return fmt.Errorf(
				"confirmed assertion %s block hash %s does not match attested %s",
				assertionHash,
				l2BlockHash,
				entry.L2BlockHash,
			)
		}
		entry.AssertionConfirmed = true
		return nil
	}
	// A confirmation can legitimately refer to an assertion created before the
	// configured lookback. It is irrelevant to this feed.
	return nil
}

// Proposals returns a stable copy of unresolved assertion proposals.
func (s *AttestedRootStore) Proposals() []ProposedAssertion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProposedAssertion, len(s.state.Proposals))
	copy(out, s.state.Proposals)
	return out
}

// ProvisionalAssertions returns assertion feed entries awaiting promotion.
func (s *AttestedRootStore) ProvisionalAssertions() []AttestedRoot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []AttestedRoot
	for _, entry := range s.state.Attested {
		if entry.Source == SourceAssertion && entry.Provisional {
			out = append(out, entry)
		}
	}
	return out
}

// RecordAssertionAttestation moves a proposal into the verified feed.
func (s *AttestedRootStore) RecordAssertionAttestation(
	proposal ProposedAssertion,
	commitment BlockCommitment,
	now time.Time,
	provisional bool,
) error {
	if commitment.BlockHash != proposal.L2BlockHash {
		return fmt.Errorf(
			"proposal %s commits to %s, local commitment is %s",
			proposal.AssertionHash,
			proposal.L2BlockHash,
			commitment.BlockHash,
		)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeProposalLocked(proposal.AssertionHash)
	for index := range s.state.Attested {
		entry := &s.state.Attested[index]
		if entry.AssertionHash != proposal.AssertionHash {
			continue
		}
		if entry.L2BlockHash != commitment.BlockHash || entry.Root != commitment.StateRoot {
			return fmt.Errorf("assertion %s changed its local commitment", proposal.AssertionHash)
		}
		if !entry.Provisional && provisional {
			return fmt.Errorf("assertion %s cannot be demoted from finalized", proposal.AssertionHash)
		}
		entry.Provisional = provisional
		entry.L1BlockNumber = proposal.L1BlockNumber
		entry.AssertionConfirmed = entry.AssertionConfirmed || proposal.Confirmed
		return nil
	}
	s.state.Attested = append(s.state.Attested, AttestedRoot{
		L2BlockNumber:      commitment.BlockNumber,
		Root:               commitment.StateRoot,
		Source:             SourceAssertion,
		AssertionHash:      proposal.AssertionHash,
		Provisional:        provisional,
		AttestedAt:         now.UTC(),
		L2BlockHash:        commitment.BlockHash,
		L1BlockNumber:      proposal.L1BlockNumber,
		AssertionConfirmed: proposal.Confirmed,
	})
	return nil
}

// PromoteAssertion marks an assertion irreversible after both the L1
// confirmation and Nitro-finalized recheck have succeeded.
func (s *AttestedRootStore) PromoteAssertion(assertionHash common.Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.state.Attested {
		entry := &s.state.Attested[index]
		if entry.AssertionHash == assertionHash {
			entry.Provisional = false
			entry.AssertionConfirmed = true
			return true
		}
	}
	return false
}

// RemoveAssertion removes an assertion from both pending and attested state.
func (s *AttestedRootStore) RemoveAssertion(assertionHash common.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeProposalLocked(assertionHash)
	kept := s.state.Attested[:0]
	for _, entry := range s.state.Attested {
		if entry.AssertionHash != assertionHash {
			kept = append(kept, entry)
		}
	}
	s.state.Attested = kept
}

// IsFinalizedAssertion reports whether an assertion already has irreversible evidence.
func (s *AttestedRootStore) IsFinalizedAssertion(hash common.Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, entry := range s.state.Attested {
		if entry.Source == SourceAssertion && entry.AssertionHash == hash && !entry.Provisional {
			return true
		}
	}
	return false
}

// DropUnfinalizedAssertions removes entries at or beyond the durable cursor.
// Call only on a staged store inside Commit before serving persisted state.
func (s *AttestedRootStore) DropUnfinalizedAssertions() uint64 {
	return s.pruneUnfinalizedAssertionsFrom(s.NextL1Block(), nil)
}

// HasUnfinalizedAssertions identifies work not covered by the finalized cursor.
func (s *AttestedRootStore) HasUnfinalizedAssertions() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, proposal := range s.state.Proposals {
		if proposal.L1BlockNumber >= s.state.NextL1Block {
			return true
		}
	}
	for _, entry := range s.state.Attested {
		if entry.Source == SourceAssertion && entry.Provisional && entry.L1BlockNumber >= s.state.NextL1Block {
			return true
		}
	}
	return false
}

// PruneUnfinalizedAssertions applies a complete speculative snapshot. Call only
// on a staged store inside Commit; derived entries and finalized work survive.
func (s *AttestedRootStore) PruneUnfinalizedAssertions(finalized uint64, present map[common.Hash]bool) uint64 {
	if finalized == ^uint64(0) {
		return 0
	}
	return s.pruneUnfinalizedAssertionsFrom(finalized+1, present)
}

func (s *AttestedRootStore) pruneUnfinalizedAssertionsFrom(from uint64, present map[common.Hash]bool) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := make(map[common.Hash]bool)
	proposals := s.state.Proposals[:0]
	for _, proposal := range s.state.Proposals {
		if proposal.L1BlockNumber >= from && !present[proposal.AssertionHash] {
			removed[proposal.AssertionHash] = true
		} else {
			proposals = append(proposals, proposal)
		}
	}
	s.state.Proposals = proposals
	attested := s.state.Attested[:0]
	for _, entry := range s.state.Attested {
		if entry.Source == SourceAssertion && entry.Provisional && entry.L1BlockNumber >= from && !present[entry.AssertionHash] {
			removed[entry.AssertionHash] = true
		} else {
			attested = append(attested, entry)
		}
	}
	s.state.Attested = attested
	return uint64(len(removed))
}

// ObserveAssertionIngest publishes only successfully committed snapshots.
func (s *AttestedRootStore) ObserveAssertionIngest(head, revoked uint64) {
	s.mu.RLock()
	metrics, chain := s.metrics, s.state.SrcChain
	s.mu.RUnlock()
	if metrics != nil {
		metrics.AssertionIngestHead.WithLabelValues(chain).Set(float64(head))
		metrics.AssertionRevocations.WithLabelValues(chain).Add(float64(revoked))
	}
}

// RecordAssertionMismatch removes a rejected assertion and retains an alerting
// record for operators.
func (s *AttestedRootStore) RecordAssertionMismatch(
	assertionHash, l2BlockHash common.Hash,
	reason string,
	now time.Time,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeProposalLocked(assertionHash)
	kept := s.state.Attested[:0]
	for _, entry := range s.state.Attested {
		if entry.AssertionHash != assertionHash {
			kept = append(kept, entry)
		}
	}
	s.state.Attested = kept
	for _, mismatch := range s.state.Mismatches {
		if mismatch.AssertionHash == assertionHash {
			return
		}
	}
	s.state.Mismatches = append(s.state.Mismatches, AssertionMismatch{
		AssertionHash: assertionHash,
		L2BlockHash:   l2BlockHash,
		Reason:        reason,
		RecordedAt:    now.UTC(),
	})
}

// Mismatches returns a stable copy of terminal mismatch records.
func (s *AttestedRootStore) Mismatches() []AssertionMismatch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AssertionMismatch, len(s.state.Mismatches))
	copy(out, s.state.Mismatches)
	return out
}

// HighestAttested returns the highest qualifying feed entry.
func (s *AttestedRootStore) HighestAttested(includeProvisional bool) (AttestedRoot, bool) {
	return s.HighestAttestedAtOrBelow(^uint64(0), includeProvisional)
}

// HighestAttestedAtOrBelow returns the highest qualifying entry no greater
// than the inclusive height bound.
func (s *AttestedRootStore) HighestAttestedAtOrBelow(
	l2BlockNumber uint64,
	includeProvisional bool,
) (AttestedRoot, bool) {
	if s == nil {
		return AttestedRoot{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best AttestedRoot
	found := false
	for _, entry := range s.state.Attested {
		if entry.L2BlockNumber > l2BlockNumber {
			continue
		}
		if entry.Provisional && !includeProvisional {
			continue
		}
		if !found || betterAttestedRoot(entry, best) {
			best = entry
			found = true
		}
	}
	return best, found
}

func betterAttestedRoot(candidate, current AttestedRoot) bool {
	if candidate.L2BlockNumber != current.L2BlockNumber {
		return candidate.L2BlockNumber > current.L2BlockNumber
	}
	if candidate.Provisional != current.Provisional {
		return !candidate.Provisional
	}
	return bytes.Compare(candidate.AssertionHash[:], current.AssertionHash[:]) < 0
}

func (s *AttestedRootStore) removeProposalLocked(assertionHash common.Hash) {
	kept := s.state.Proposals[:0]
	for _, proposal := range s.state.Proposals {
		if proposal.AssertionHash != assertionHash {
			kept = append(kept, proposal)
		}
	}
	s.state.Proposals = kept
}

package opstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"attestor/optimism"
)

const opstackStateVersion uint32 = 1

// DeploymentIdentity binds an OP state file to the deployment whose factory
// evidence it contains. It is intentionally independent from runtime-only
// endpoints so an operator cannot accidentally reuse a cursor for a different
// chain, factory, or respected-game policy.
type DeploymentIdentity struct {
	SrcChain           string
	L1ChainID          uint64
	L2ChainID          uint64
	DisputeGameFactory common.Address
	RespectedGameType  uint32
}

func (d DeploymentIdentity) validate() error {
	if d.SrcChain == "" {
		return errors.New("OP deployment identity src_chain must not be empty")
	}
	if d.L1ChainID == 0 || d.L2ChainID == 0 {
		return errors.New("OP deployment identity L1 and L2 chain IDs must not be zero")
	}
	if d.DisputeGameFactory == (common.Address{}) {
		return errors.New("OP deployment identity DisputeGameFactory must not be zero")
	}
	return nil
}

func (d DeploymentIdentity) String() string {
	return fmt.Sprintf(
		"l1:%d/l2:%d/factory:%s/game-type:%d",
		d.L1ChainID,
		d.L2ChainID,
		d.DisputeGameFactory.Hex(),
		d.RespectedGameType,
	)
}

// ProposedRoot is one output-root proposal ingested from the DisputeGameFactory.
type ProposedRoot struct {
	GameIndex     uint64         `json:"game_index"`
	GameAddress   common.Address `json:"game_address"`
	GameType      uint32         `json:"game_type"`
	RootClaim     [32]byte       `json:"root_claim"`
	L2BlockNumber uint64         `json:"l2_block_number"`
	L1Timestamp   uint64         `json:"l1_timestamp"`
}

// Feed-entry sources.
const (
	// SourceGame marks a root that was proposed on L1 (dispute game) and
	// confirmed by the attestor's replay.
	SourceGame = "game"
	// SourceDerived marks a root the attestor computed itself from the
	// replica at the configured head — no proposal involved.
	SourceDerived = "derived"
)

// AttestedRoot is one feed entry: a root the attestor's replay affirms.
// Provisional entries were decided on a block the finalized head did not yet
// cover (L1-reorg risk at safe, sequencer trust at unsafe) and hold that
// status until the finalized recheck clears the flag.
type AttestedRoot struct {
	// GameIndex is meaningful only for Source == SourceGame.
	GameIndex     uint64    `json:"game_index,omitempty"`
	L2BlockNumber uint64    `json:"l2_block_number"`
	Root          [32]byte  `json:"root"`
	AttestedAt    time.Time `json:"attested_at"`
	Provisional   bool      `json:"provisional,omitempty"`
	// Source is SourceGame or SourceDerived; empty (legacy state files) means
	// SourceGame.
	Source string `json:"source,omitempty"`
}

func (a AttestedRoot) source() string {
	if a.Source == "" {
		return SourceGame
	}
	return a.Source
}

// MismatchRecord is a terminal verdict: honest replay disagreed with the
// proposed root.
type MismatchRecord struct {
	Game        ProposedRoot `json:"game"`
	LocalRoot   [32]byte     `json:"local_root"`
	RecordedAt  time.Time    `json:"recorded_at"`
	Provisional bool         `json:"provisional,omitempty"`
}

// RecheckEntry tracks a provisional game verdict awaiting finalized-head
// confirmation.
type RecheckEntry struct {
	Game             ProposedRoot `json:"game"`
	ProvisionalMatch bool         `json:"provisional_match"`
	LocalRoot        [32]byte     `json:"local_root"`
}

type persistedState struct {
	Version              uint32 `json:"version"`
	SrcChain             string `json:"src_chain"`
	SourceIdentity       string `json:"source_identity"`
	LastFinalizedL1Block uint64 `json:"last_finalized_l1_block,omitempty"`
	// BootstrapPending is true from the first Save of a new state file until a
	// Bootstrap commits the ingest cursor. It lives in the file, not in memory,
	// so a boot that dies between binding the identity and bootstrapping is
	// retried on restart. Files written before this field existed omit it and
	// are treated as already bootstrapped, which is what they were.
	BootstrapPending bool `json:"bootstrap_pending,omitempty"`
	// NextGameIndex is the ingest cursor: the next factory index to fetch.
	NextGameIndex uint64           `json:"next_game_index"`
	Pending       []ProposedRoot   `json:"pending"`
	Recheck       []RecheckEntry   `json:"recheck,omitempty"`
	Attested      []AttestedRoot   `json:"attested"`
	Mismatches    []MismatchRecord `json:"mismatches,omitempty"`
}

// stateFile is the minimum file surface used by the atomic store writer. It is
// deliberately small so tests can inject failures at every durability step.
type stateFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type stateFileSystem interface {
	CreateTemp(string, string) (stateFile, error)
	Rename(string, string) error
	Remove(string) error
	SyncDir(string) error
}

type osStateFileSystem struct{}

func (osStateFileSystem) CreateTemp(dir, pattern string) (stateFile, error) {
	return os.CreateTemp(dir, pattern)
}

func (osStateFileSystem) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func (osStateFileSystem) Remove(path string) error {
	return os.Remove(path)
}

func (osStateFileSystem) SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// AttestedRootStore is the persistent record of the attestor: ingest cursor,
// undecided games, provisional rechecks, and the append-only attested feed.
// The Run goroutine is the only writer; readers (a future op_to_cosmos module)
// go through the RWMutex-guarded accessors.
type AttestedRootStore struct {
	mu       sync.RWMutex
	commitMu sync.Mutex
	path     string
	state    persistedState
	fs       stateFileSystem
}

// LoadStore reads the state file at path. A missing file yields a fresh store
// (Fresh() == true, awaiting a bootstrap cursor) whose first Save persists that
// pending state, so it survives a restart until Bootstrap commits; an unreadable or corrupt
// file is a hard error — never silently reset, the operator deletes the file
// to re-bootstrap (re-attesting old games is safe, silently skipping is not).
func LoadStore(path string) (*AttestedRootStore, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &AttestedRootStore{
			path:  path,
			state: persistedState{Version: opstackStateVersion, BootstrapPending: true},
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read attestor state file %s: %w", path, err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("attestor state file %s is corrupt (delete it to re-bootstrap): %w", path, err)
	}
	if st.Version != opstackStateVersion {
		return nil, fmt.Errorf(
			"unsupported OP attestor state version %d, want %d (delete it to re-bootstrap)",
			st.Version,
			opstackStateVersion,
		)
	}
	return &AttestedRootStore{path: path, state: st}, nil
}

// BindDeploymentIdentity pins a newly created state file, or verifies an
// existing file, before the command opens gRPC or starts the attestation loop.
// Callers must Save after a first successful bind so a crash cannot leave an
// unbound state file that later gets reused under another deployment.
func (s *AttestedRootStore) BindDeploymentIdentity(identity DeploymentIdentity) error {
	if err := identity.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Version != opstackStateVersion {
		return fmt.Errorf("unsupported OP attestor state version %d", s.state.Version)
	}
	configured := identity.String()
	if s.state.SrcChain == "" && s.state.SourceIdentity == "" {
		s.state.SrcChain = identity.SrcChain
		s.state.SourceIdentity = configured
		return nil
	}
	if s.state.SrcChain != identity.SrcChain {
		return fmt.Errorf("OP attestor state src_chain %q does not match configured %q", s.state.SrcChain, identity.SrcChain)
	}
	if s.state.SourceIdentity != configured {
		return fmt.Errorf("OP attestor state source identity %q does not match configured %q", s.state.SourceIdentity, configured)
	}
	return nil
}

// RecordFinalizedL1Block records the finalized factory view from which the
// current durable cursor was derived. It is diagnostics and restart evidence,
// not a substitute for per-pass pinned calls.
func (s *AttestedRootStore) RecordFinalizedL1Block(block uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if block <= s.state.LastFinalizedL1Block {
		return false
	}
	s.state.LastFinalizedL1Block = block
	return true
}

func (s *AttestedRootStore) LastFinalizedL1Block() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.LastFinalizedL1Block
}

// Save atomically persists the current state: write to a temp file in the same
// directory, fsync, rename. A crash mid-save leaves the previous state intact;
// stale state only causes re-attestation, which is the safe direction.
func (s *AttestedRootStore) Save() error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	s.mu.RLock()
	data, err := json.MarshalIndent(s.state, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("failed to marshal attestor state: %w", err)
	}
	return s.persist(data)
}

// Commit applies a mutation to an isolated copy, persists that copy, then
// atomically publishes it to in-memory readers. The OP runner is the sole
// writer, but this ordering prevents gRPC from observing a cursor/root that
// failed to reach disk.
func (s *AttestedRootStore) Commit(mutate func(*AttestedRootStore) (bool, error)) error {
	if s == nil {
		return errors.New("attestor state store is nil")
	}
	if mutate == nil {
		return errors.New("attestor state mutation must not be nil")
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	s.mu.RLock()
	staged := &AttestedRootStore{
		path:  s.path,
		state: clonePersistedState(s.state),
		fs:    s.fs,
	}
	s.mu.RUnlock()
	changed, err := mutate(staged)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(staged.state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal staged attestor state: %w", err)
	}
	if err := s.persist(data); err != nil {
		return err
	}
	s.mu.Lock()
	s.state = staged.state
	s.mu.Unlock()
	return nil
}

func clonePersistedState(state persistedState) persistedState {
	cloned := state
	cloned.Pending = append([]ProposedRoot(nil), state.Pending...)
	cloned.Recheck = append([]RecheckEntry(nil), state.Recheck...)
	cloned.Attested = append([]AttestedRoot(nil), state.Attested...)
	cloned.Mismatches = append([]MismatchRecord(nil), state.Mismatches...)
	return cloned
}

func (s *AttestedRootStore) persist(data []byte) error {
	dir := filepath.Dir(s.path)
	fs := s.fileSystem()
	tmp, err := fs.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		fs.Remove(tmpName)
		return fmt.Errorf("failed to write temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		fs.Remove(tmpName)
		return fmt.Errorf("failed to sync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		fs.Remove(tmpName)
		return fmt.Errorf("failed to close temp state file: %w", err)
	}
	if err := fs.Rename(tmpName, s.path); err != nil {
		fs.Remove(tmpName)
		return fmt.Errorf("failed to replace state file: %w", err)
	}
	if err := fs.SyncDir(dir); err != nil {
		return fmt.Errorf("failed to sync state directory: %w", err)
	}
	return nil
}

func (s *AttestedRootStore) fileSystem() stateFileSystem {
	if s.fs != nil {
		return s.fs
	}
	return osStateFileSystem{}
}

// Fresh reports whether the store still needs its bootstrap ingest cursor:
// it was created without a state file and no Bootstrap has committed since,
// including across restarts.
func (s *AttestedRootStore) Fresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.BootstrapPending
}

// Bootstrap sets the initial ingest cursor on a fresh store and reports
// whether it changed the durable state.
func (s *AttestedRootStore) Bootstrap(nextGameIndex uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.BootstrapPending && s.state.NextGameIndex == nextGameIndex {
		return false
	}
	s.state.NextGameIndex = nextGameIndex
	s.state.BootstrapPending = false
	return true
}

func (s *AttestedRootStore) NextGameIndex() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.NextGameIndex
}

// AdvanceIngest moves the ingest cursor to next, optionally adding a fetched
// game to the pending set. Callers advance only past successfully-fetched
// indices — never past a fetch failure.
func (s *AttestedRootStore) AdvanceIngest(next uint64, pending *ProposedRoot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.NextGameIndex = next
	if pending != nil {
		s.state.Pending = append(s.state.Pending, *pending)
	}
}

// Pending returns a copy of the undecided games.
func (s *AttestedRootStore) Pending() []ProposedRoot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProposedRoot, len(s.state.Pending))
	copy(out, s.state.Pending)
	return out
}

func (s *AttestedRootStore) removePendingLocked(gameIndex uint64) {
	kept := s.state.Pending[:0]
	for _, g := range s.state.Pending {
		if g.GameIndex != gameIndex {
			kept = append(kept, g)
		}
	}
	s.state.Pending = kept
}

// RecordMatch removes the game from pending and appends its attested root to
// the feed. provisional marks a below-finalized verdict awaiting recheck.
func (s *AttestedRootStore) RecordMatch(game ProposedRoot, now time.Time, provisional bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removePendingLocked(game.GameIndex)
	s.state.Attested = append(s.state.Attested, AttestedRoot{
		GameIndex:     game.GameIndex,
		L2BlockNumber: game.L2BlockNumber,
		Root:          game.RootClaim,
		AttestedAt:    now,
		Provisional:   provisional,
		Source:        SourceGame,
	})
	if provisional {
		s.state.Recheck = append(s.state.Recheck, RecheckEntry{Game: game, ProvisionalMatch: true, LocalRoot: game.RootClaim})
	}
}

// AppendDerived records a self-derived attestation: the replica's own output
// root at l2Block, computed at the configured head. At most one derived entry
// exists per L2 block (callers gate on HighestDerivedBlock).
func (s *AttestedRootStore) AppendDerived(l2Block uint64, root [32]byte, now time.Time, provisional bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Attested = append(s.state.Attested, AttestedRoot{
		L2BlockNumber: l2Block,
		Root:          root,
		AttestedAt:    now,
		Provisional:   provisional,
		Source:        SourceDerived,
	})
}

// HighestDerivedBlock returns the highest L2 block with a derived entry
// (provisional included).
func (s *AttestedRootStore) HighestDerivedBlock() (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best uint64
	found := false
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.source() == SourceDerived && (!found || a.L2BlockNumber > best) {
			best = a.L2BlockNumber
			found = true
		}
	}
	return best, found
}

// DerivedProvisionalAtOrBelow lists provisional derived entries the finalized
// head now covers, oldest first — the reverification work-list.
func (s *AttestedRootStore) DerivedProvisionalAtOrBelow(finalized uint64) []AttestedRoot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []AttestedRoot
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.source() == SourceDerived && a.Provisional && a.L2BlockNumber <= finalized {
			out = append(out, *a)
		}
	}
	return out
}

// ConfirmDerived clears the provisional flag on the derived entry at l2Block.
func (s *AttestedRootStore) ConfirmDerived(l2Block uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.source() == SourceDerived && a.L2BlockNumber == l2Block && a.Provisional {
			a.Provisional = false
			return true
		}
	}
	return false
}

// CorrectDerived replaces the derived entry's root at l2Block with the
// authoritative finalized-head value and confirms it (divergence handling —
// the alarm is the caller's job).
func (s *AttestedRootStore) CorrectDerived(l2Block uint64, root [32]byte, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.source() == SourceDerived && a.L2BlockNumber == l2Block && a.Provisional {
			a.Root = root
			a.AttestedAt = now
			a.Provisional = false
			return true
		}
	}
	return false
}

// PruneDerived drops the oldest confirmed derived entries beyond max,
// returning how many were removed. Provisional derived entries and
// game-verified entries are never pruned.
func (s *AttestedRootStore) PruneDerived(max int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	confirmed := 0
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.source() == SourceDerived && !a.Provisional {
			confirmed++
		}
	}
	excess := confirmed - max
	if excess <= 0 {
		return 0
	}
	// Entries are appended in block order; drop the first `excess` confirmed
	// derived entries.
	kept := s.state.Attested[:0]
	removed := 0
	for _, a := range s.state.Attested {
		if removed < excess && a.source() == SourceDerived && !a.Provisional {
			removed++
			continue
		}
		kept = append(kept, a)
	}
	s.state.Attested = kept
	return removed
}

// RecordMismatch removes the game from pending and records the terminal
// mismatch verdict.
func (s *AttestedRootStore) RecordMismatch(game ProposedRoot, localRoot [32]byte, now time.Time, provisional bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removePendingLocked(game.GameIndex)
	s.state.Mismatches = append(s.state.Mismatches, MismatchRecord{
		Game:        game,
		LocalRoot:   localRoot,
		RecordedAt:  now,
		Provisional: provisional,
	})
	if provisional {
		s.state.Recheck = append(s.state.Recheck, RecheckEntry{Game: game, ProvisionalMatch: false, LocalRoot: localRoot})
	}
}

// RecheckList returns a copy of the provisional game verdicts awaiting
// finalized-head confirmation.
func (s *AttestedRootStore) RecheckList() []RecheckEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RecheckEntry, len(s.state.Recheck))
	copy(out, s.state.Recheck)
	return out
}

func (s *AttestedRootStore) removeRecheckLocked(gameIndex uint64) {
	kept := s.state.Recheck[:0]
	for _, e := range s.state.Recheck {
		if e.Game.GameIndex != gameIndex {
			kept = append(kept, e)
		}
	}
	s.state.Recheck = kept
}

// ConfirmProvisional resolves a recheck whose finalized-head result agreed with the
// provisional verdict: the recheck entry is dropped and, for a match, the feed
// entry's provisional flag is cleared.
func (s *AttestedRootStore) ConfirmProvisional(gameIndex uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRecheckLocked(gameIndex)
	// Match on source too: derived entries carry GameIndex 0, which collides
	// with factory game index 0.
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.source() == SourceGame && a.GameIndex == gameIndex {
			a.Provisional = false
		}
	}
	for i := range s.state.Mismatches {
		if s.state.Mismatches[i].Game.GameIndex == gameIndex {
			s.state.Mismatches[i].Provisional = false
		}
	}
}

// CorrectProvisional resolves a recheck whose finalized-head result CONTRADICTED
// the provisional verdict (sequencer divergence). The provisional records are
// replaced by the authoritative finalized-head verdict: a provisional feed entry is
// removed (the root was not legit) or a provisional mismatch is superseded by
// a confirmed attested root.
func (s *AttestedRootStore) CorrectProvisional(entry RecheckEntry, safeLocalRoot [32]byte, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRecheckLocked(entry.Game.GameIndex)
	if entry.ProvisionalMatch {
		// Provisional match was wrong: pull the feed entry, record the mismatch.
		// Match on source too: derived entries carry GameIndex 0, which
		// collides with factory game index 0.
		kept := s.state.Attested[:0]
		for _, a := range s.state.Attested {
			if a.source() == SourceGame && a.GameIndex == entry.Game.GameIndex {
				continue
			}
			kept = append(kept, a)
		}
		s.state.Attested = kept
		s.state.Mismatches = append(s.state.Mismatches, MismatchRecord{
			Game:       entry.Game,
			LocalRoot:  safeLocalRoot,
			RecordedAt: now,
		})
		return
	}
	// Provisional mismatch was wrong: drop the mismatch record, append the
	// confirmed attested root.
	kept := s.state.Mismatches[:0]
	for _, m := range s.state.Mismatches {
		if m.Game.GameIndex != entry.Game.GameIndex {
			kept = append(kept, m)
		}
	}
	s.state.Mismatches = kept
	s.state.Attested = append(s.state.Attested, AttestedRoot{
		GameIndex:     entry.Game.GameIndex,
		L2BlockNumber: entry.Game.L2BlockNumber,
		Root:          entry.Game.RootClaim,
		AttestedAt:    now,
		Source:        SourceGame,
	})
}

// AttestedUpTo reports the highest attested L2 block. Provisional entries are
// excluded unless includeProvisional is set.
func (s *AttestedRootStore) AttestedUpTo(includeProvisional bool) (attestor.Cursor, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *AttestedRoot
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.Provisional && !includeProvisional {
			continue
		}
		if best == nil || a.L2BlockNumber > best.L2BlockNumber {
			best = a
		}
	}
	if best == nil {
		return attestor.Cursor{}, false
	}
	return attestor.Cursor{Height: best.L2BlockNumber, ID: best.Root}, true
}

// HighestAttestedAtOrBelow returns the attested root with the largest L2 block
// number not exceeding l2Block — the only roots a consumer may relay against.
func (s *AttestedRootStore) HighestAttestedAtOrBelow(l2Block uint64, includeProvisional bool) (AttestedRoot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *AttestedRoot
	for i := range s.state.Attested {
		a := &s.state.Attested[i]
		if a.L2BlockNumber > l2Block {
			continue
		}
		if a.Provisional && !includeProvisional {
			continue
		}
		if best == nil || a.L2BlockNumber > best.L2BlockNumber {
			best = a
		}
	}
	if best == nil {
		return AttestedRoot{}, false
	}
	return *best, true
}

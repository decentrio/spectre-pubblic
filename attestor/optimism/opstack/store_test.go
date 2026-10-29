package opstack

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type testStateFileSystem struct {
	createTemp func(string, string) (stateFile, error)
	rename     func(string, string) error
	remove     func(string) error
	syncDir    func(string) error
}

func (fs testStateFileSystem) CreateTemp(dir, pattern string) (stateFile, error) {
	return fs.createTemp(dir, pattern)
}

func (fs testStateFileSystem) Rename(oldPath, newPath string) error {
	return fs.rename(oldPath, newPath)
}

func (fs testStateFileSystem) Remove(path string) error {
	return fs.remove(path)
}

func (fs testStateFileSystem) SyncDir(path string) error {
	return fs.syncDir(path)
}

type failingStateFile struct {
	stateFile
	writeErr error
	syncErr  error
	closeErr error
}

func (f failingStateFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.stateFile.Write(data)
}

func (f failingStateFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.stateFile.Sync()
}

func (f failingStateFile) Close() error {
	if f.closeErr != nil {
		_ = f.stateFile.Close()
		return f.closeErr
	}
	return f.stateFile.Close()
}

func realStateFileSystem() testStateFileSystem {
	base := osStateFileSystem{}
	return testStateFileSystem{
		createTemp: base.CreateTemp,
		rename:     base.Rename,
		remove:     base.Remove,
		syncDir:    base.SyncDir,
	}
}

func testGame(idx uint64, l2Block uint64, claim [32]byte) ProposedRoot {
	return ProposedRoot{
		GameIndex:     idx,
		GameAddress:   common.BytesToAddress([]byte{byte(idx + 1)}),
		RootClaim:     claim,
		L2BlockNumber: l2Block,
		L1Timestamp:   1000 + idx,
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if !s.Fresh() {
		t.Fatal("missing state file must yield a fresh store")
	}
	s.Bootstrap(7)

	now := time.Unix(4000, 0).UTC()
	g0, g1, g2 := testGame(7, 100, root(0x11)), testGame(8, 200, root(0x12)), testGame(9, 300, root(0x13))
	s.AdvanceIngest(8, &g0)
	s.AdvanceIngest(9, &g1)
	s.AdvanceIngest(10, &g2)
	s.RecordMatch(g0, now, false)
	s.RecordMatch(g1, now, true) // provisional, enters recheck
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	re, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if re.Fresh() {
		t.Fatal("reloaded store must not be fresh")
	}
	if got := re.NextGameIndex(); got != 10 {
		t.Fatalf("reloaded cursor = %d, want 10", got)
	}
	if got := re.Pending(); len(got) != 1 || got[0].GameIndex != 9 {
		t.Fatalf("reloaded pending = %+v, want only game 9", got)
	}
	if got := re.RecheckList(); len(got) != 1 || got[0].Game.GameIndex != 8 {
		t.Fatalf("reloaded recheck = %+v, want only game 8", got)
	}
	if cur, ok := re.AttestedUpTo(false); !ok || cur.Height != 100 {
		t.Fatalf("reloaded confirmed AttestedUpTo = (%+v, %t), want height 100", cur, ok)
	}
	if cur, ok := re.AttestedUpTo(true); !ok || cur.Height != 200 {
		t.Fatalf("reloaded provisional AttestedUpTo = (%+v, %t), want height 200", cur, ok)
	}
}

func TestStoreCorruptFileFailsLoud(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(path); err == nil {
		t.Fatal("corrupt state file must be a hard error, not a silent reset")
	}
}

func TestStoreRejectsUnknownStateVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStore(path); err == nil || !strings.Contains(err.Error(), "delete it to re-bootstrap") {
		t.Fatalf("unknown state version error = %v, want re-bootstrap guidance", err)
	}
}

func TestStoreBindsDeploymentIdentityBeforeServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	identity := DeploymentIdentity{
		SrcChain:           "op-mainnet",
		L1ChainID:          1,
		L2ChainID:          10,
		DisputeGameFactory: common.HexToAddress("0x0000000000000000000000000000000000000001"),
		RespectedGameType:  8,
	}
	if err := store.BindDeploymentIdentity(identity); err != nil {
		t.Fatalf("BindDeploymentIdentity: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save identity: %v", err)
	}
	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := reloaded.BindDeploymentIdentity(identity); err != nil {
		t.Fatalf("rebind same deployment: %v", err)
	}
	identity.L2ChainID = 8453
	if err := reloaded.BindDeploymentIdentity(identity); err == nil {
		t.Fatal("state from a different L2 deployment was accepted")
	}
}

func TestHighestAttestedAtOrBelow(t *testing.T) {
	s, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(4000, 0)
	s.RecordMatch(testGame(0, 100, root(0x21)), now, false)
	s.RecordMatch(testGame(1, 200, root(0x22)), now, true) // provisional
	s.RecordMatch(testGame(2, 300, root(0x23)), now, false)

	if _, ok := s.HighestAttestedAtOrBelow(99, false); ok {
		t.Fatal("nothing attested at or below 99")
	}
	if got, ok := s.HighestAttestedAtOrBelow(100, false); !ok || got.L2BlockNumber != 100 {
		t.Fatalf("at 100: (%+v, %t), want the boundary entry itself", got, ok)
	}
	// Provisional entry at 200 must be skipped without opt-in.
	if got, ok := s.HighestAttestedAtOrBelow(250, false); !ok || got.L2BlockNumber != 100 {
		t.Fatalf("at 250 confirmed-only: (%+v, %t), want height 100", got, ok)
	}
	if got, ok := s.HighestAttestedAtOrBelow(250, true); !ok || got.L2BlockNumber != 200 {
		t.Fatalf("at 250 with provisional: (%+v, %t), want height 200", got, ok)
	}
	if got, ok := s.HighestAttestedAtOrBelow(1000, false); !ok || got.L2BlockNumber != 300 {
		t.Fatalf("at 1000: (%+v, %t), want height 300", got, ok)
	}
}

func TestSaveIsAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Bootstrap(1)
	if err := s.Save(); err != nil {
		t.Fatalf("first save: %v", err)
	}
	s.Bootstrap(2)
	if err := s.Save(); err != nil {
		t.Fatalf("second save: %v", err)
	}
	re, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := re.NextGameIndex(); got != 2 {
		t.Fatalf("cursor = %d, want 2", got)
	}
	// No temp files may survive a successful save.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("state dir contains %v, want only the state file", names)
	}
}

func TestCommitPublishesOnlyAfterDurableSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	store.Bootstrap(1)
	if err := store.Save(); err != nil {
		t.Fatalf("initial Save: %v", err)
	}
	fs := realStateFileSystem()
	fs.rename = func(string, string) error { return errors.New("rename failed") }
	store.fs = fs
	if err := store.Commit(func(staged *AttestedRootStore) (bool, error) {
		return staged.Bootstrap(2), nil
	}); err == nil {
		t.Fatal("Commit succeeded despite failed durable replacement")
	}
	if got := store.NextGameIndex(); got != 1 {
		t.Fatalf("in-memory cursor after failed commit = %d, want durable cursor 1", got)
	}
	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.NextGameIndex(); got != 1 {
		t.Fatalf("durable cursor after failed commit = %d, want 1", got)
	}
}

func TestCommitSkipsPersistenceWhenUnchanged(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	fs := realStateFileSystem()
	fs.createTemp = func(string, string) (stateFile, error) {
		t.Fatal("Commit attempted to persist an unchanged state")
		return nil, nil
	}
	store.fs = fs

	if err := store.Commit(func(*AttestedRootStore) (bool, error) { return false, nil }); err != nil {
		t.Fatalf("Commit unchanged state: %v", err)
	}
	if !store.Fresh() {
		t.Fatal("unchanged Commit published state")
	}
}

func TestCorrectDerivedDoesNotOverwriteConfirmedEntry(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	now := time.Unix(4_000, 0).UTC()
	original := root(0xaa)
	store.AppendDerived(100, original, now, false)

	if store.CorrectDerived(100, root(0xbb), now.Add(time.Second)) {
		t.Fatal("CorrectDerived overwrote an already-confirmed root")
	}
	entry, ok := store.HighestAttestedAtOrBelow(100, false)
	if !ok || entry.Root != original || entry.Provisional {
		t.Fatalf("confirmed derived root changed: %+v ok=%t", entry, ok)
	}
}

func TestConfirmDerivedDoesNotReportChangeForConfirmedEntry(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	store.AppendDerived(100, root(0xaa), time.Unix(4_000, 0).UTC(), false)

	if store.ConfirmDerived(100) {
		t.Fatal("ConfirmDerived reported a change for an already-confirmed root")
	}
}

// Every failure before rename must leave the last durable state intact and
// remove the temporary file. A directory-sync failure occurs after rename, so
// the new state is already visible and the caller must treat the save error as
// an uncertain-but-committed outcome rather than overwrite it blindly.
func TestSaveFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(testStateFileSystem) testStateFileSystem
		wantCursor uint64
	}{
		{
			name: "create temp",
			configure: func(fs testStateFileSystem) testStateFileSystem {
				fs.createTemp = func(string, string) (stateFile, error) {
					return nil, errors.New("create failed")
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "write temp",
			configure: func(fs testStateFileSystem) testStateFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (stateFile, error) {
					file, err := create(dir, pattern)
					return failingStateFile{stateFile: file, writeErr: errors.New("write failed")}, err
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "sync temp",
			configure: func(fs testStateFileSystem) testStateFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (stateFile, error) {
					file, err := create(dir, pattern)
					return failingStateFile{stateFile: file, syncErr: errors.New("sync failed")}, err
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "close temp",
			configure: func(fs testStateFileSystem) testStateFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (stateFile, error) {
					file, err := create(dir, pattern)
					return failingStateFile{stateFile: file, closeErr: errors.New("close failed")}, err
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "rename temp",
			configure: func(fs testStateFileSystem) testStateFileSystem {
				fs.rename = func(string, string) error { return errors.New("rename failed") }
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "sync directory after rename",
			configure: func(fs testStateFileSystem) testStateFileSystem {
				fs.syncDir = func(string) error { return errors.New("directory sync failed") }
				return fs
			},
			wantCursor: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			store, err := LoadStore(path)
			if err != nil {
				t.Fatalf("LoadStore: %v", err)
			}
			store.Bootstrap(1)
			if err := store.Save(); err != nil {
				t.Fatalf("initial Save: %v", err)
			}
			store.Bootstrap(2)
			store.fs = tc.configure(realStateFileSystem())
			if err := store.Save(); err == nil {
				t.Fatal("Save succeeded despite injected failure")
			}

			reloaded, err := LoadStore(path)
			if err != nil {
				t.Fatalf("LoadStore after failed save: %v", err)
			}
			if got := reloaded.NextGameIndex(); got != tc.wantCursor {
				t.Fatalf("durable cursor = %d, want %d", got, tc.wantCursor)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatalf("read state directory: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
				t.Fatalf("state directory contains %v, want only %s", entries, filepath.Base(path))
			}
		})
	}
}

// Regression: derived feed entries persist GameIndex 0, which collides with
// factory game index 0. Resolving game 0's provisional verdict must never
// touch derived entries — confirming must not clear their provisional flag,
// and correcting must not delete them.
func TestResolveGame0DoesNotTouchDerivedEntries(t *testing.T) {
	now := time.Unix(4000, 0).UTC()
	game0 := testGame(0, 50, root(0xbb))

	t.Run("confirm keeps derived provisional", func(t *testing.T) {
		s, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		s.AppendDerived(100, root(0xaa), now, true) // provisional derived root
		s.RecordMatch(game0, now, true)             // provisional verdict for game 0

		s.ConfirmProvisional(0)

		if got, ok := s.HighestAttestedAtOrBelow(100, false); ok && got.source() == SourceDerived {
			t.Fatalf("derived root at 100 lost its provisional flag via game-0 confirmation: %+v", got)
		}
		if got, ok := s.HighestAttestedAtOrBelow(50, false); !ok || got.GameIndex != 0 || got.source() != SourceGame {
			t.Fatalf("game 0's own entry should be confirmed, got %+v ok=%t", got, ok)
		}
	})

	t.Run("correct keeps derived entries", func(t *testing.T) {
		s, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		s.AppendDerived(100, root(0xaa), now, false) // confirmed derived root
		s.RecordMatch(game0, now, true)

		// Finalized recheck contradicts game 0's provisional match.
		s.CorrectProvisional(RecheckEntry{Game: game0, ProvisionalMatch: true}, root(0xcc), now)

		got, ok := s.HighestAttestedAtOrBelow(100, false)
		if !ok || got.source() != SourceDerived || got.L2BlockNumber != 100 {
			t.Fatalf("correcting game 0 must not delete the derived entry at 100, got %+v ok=%t", got, ok)
		}
		if entry, ok := s.HighestAttestedAtOrBelow(50, true); ok && entry.source() == SourceGame {
			t.Fatalf("game 0's wrong provisional entry should have been pulled, got %+v", entry)
		}
	})
}

// ATT-03: the identity Save at startup creates the state file before bootstrap
// has run. A restart in between must still see a store that needs bootstrap.
func TestFreshSurvivesIdentitySaveUntilBootstrapCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	identity := DeploymentIdentity{
		SrcChain:           "op-mainnet",
		L1ChainID:          1,
		L2ChainID:          10,
		DisputeGameFactory: common.HexToAddress("0x0000000000000000000000000000000000000001"),
		RespectedGameType:  8,
	}

	first, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := first.BindDeploymentIdentity(identity); err != nil {
		t.Fatalf("BindDeploymentIdentity: %v", err)
	}
	if err := first.Save(); err != nil {
		t.Fatalf("Save identity: %v", err)
	}

	// First boot died here: bootstrap never committed.
	restarted, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload after identity save: %v", err)
	}
	if !restarted.Fresh() {
		t.Fatal("store lost Fresh after the identity save; bootstrap would be skipped on restart")
	}
	if err := restarted.BindDeploymentIdentity(identity); err != nil {
		t.Fatalf("rebind identity: %v", err)
	}

	// Bootstrap to index 0 is still a durable change while pending.
	if err := restarted.Commit(func(s *AttestedRootStore) (bool, error) { return s.Bootstrap(0), nil }); err != nil {
		t.Fatalf("Commit bootstrap: %v", err)
	}
	if restarted.Fresh() {
		t.Fatal("store still Fresh after Bootstrap committed")
	}

	done, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload after bootstrap: %v", err)
	}
	if done.Fresh() {
		t.Fatal("bootstrap completion was not persisted")
	}
}

func TestFailedBootstrapCommitKeepsStoreFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fs := realStateFileSystem()
	fs.rename = func(string, string) error { return errors.New("rename failed") }
	store.fs = fs

	if err := store.Commit(func(s *AttestedRootStore) (bool, error) { return s.Bootstrap(5), nil }); err == nil {
		t.Fatal("Commit succeeded despite persistence failure")
	}
	if !store.Fresh() {
		t.Fatal("failed bootstrap commit cleared Fresh in memory")
	}
	re, err := LoadStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !re.Fresh() || re.NextGameIndex() != 0 {
		t.Fatalf("failed bootstrap leaked to disk: fresh=%t cursor=%d", re.Fresh(), re.NextGameIndex())
	}
}

// State files written before BootstrapPending existed omit the field. They are
// already bootstrapped and must not be re-bootstrapped (which would reset the
// ingest cursor).
func TestLegacyStateFileWithoutBootstrapFieldIsNotFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"version":1,"src_chain":"op-mainnet","source_identity":"x","next_game_index":42,"pending":[],"attested":[]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	store, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if store.Fresh() {
		t.Fatal("legacy state file was treated as fresh")
	}
	if got := store.NextGameIndex(); got != 42 {
		t.Fatalf("legacy cursor = %d, want 42", got)
	}
}

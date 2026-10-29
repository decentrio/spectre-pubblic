package arbitrum

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type testAttestedRootFileSystem struct {
	mkdirAll   func(string, os.FileMode) error
	createTemp func(string, string) (attestedRootFile, error)
	rename     func(string, string) error
	remove     func(string) error
	syncDir    func(string) error
}

func (fs testAttestedRootFileSystem) MkdirAll(path string, mode os.FileMode) error {
	return fs.mkdirAll(path, mode)
}

func (fs testAttestedRootFileSystem) CreateTemp(dir, pattern string) (attestedRootFile, error) {
	return fs.createTemp(dir, pattern)
}

func (fs testAttestedRootFileSystem) Rename(oldPath, newPath string) error {
	return fs.rename(oldPath, newPath)
}

func (fs testAttestedRootFileSystem) Remove(path string) error {
	return fs.remove(path)
}

func (fs testAttestedRootFileSystem) SyncDir(path string) error {
	return fs.syncDir(path)
}

type failingAttestedRootFile struct {
	attestedRootFile
	chmodErr error
	writeErr error
	syncErr  error
	closeErr error
}

func (f failingAttestedRootFile) Chmod(mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	return f.attestedRootFile.Chmod(mode)
}

func (f failingAttestedRootFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.attestedRootFile.Write(data)
}

func (f failingAttestedRootFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.attestedRootFile.Sync()
}

func (f failingAttestedRootFile) Close() error {
	if f.closeErr != nil {
		_ = f.attestedRootFile.Close()
		return f.closeErr
	}
	return f.attestedRootFile.Close()
}

func realAttestedRootFileSystem() testAttestedRootFileSystem {
	base := osAttestedRootFileSystem{}
	return testAttestedRootFileSystem{
		mkdirAll:   base.MkdirAll,
		createTemp: base.CreateTemp,
		rename:     base.Rename,
		remove:     base.Remove,
		syncDir:    base.SyncDir,
	}
}

func TestAttestedRootStoreFrontiersAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "attested-roots.json")
	store, err := LoadAttestedRootStore(path, "arbitrum-one", 1_000)
	if err != nil {
		t.Fatalf("load new store: %v", err)
	}

	finalized := testStoreProposal(1, 90)
	if err := store.RecordProposal(finalized); err != nil {
		t.Fatalf("record finalized proposal: %v", err)
	}
	if err := store.RecordAssertionAttestation(
		finalized,
		testStoreCommitment(90),
		time.Unix(1_700_000_000, 0),
		false,
	); err != nil {
		t.Fatalf("record finalized attestation: %v", err)
	}

	provisional := testStoreProposal(2, 100)
	if err := store.RecordProposal(provisional); err != nil {
		t.Fatalf("record provisional proposal: %v", err)
	}
	if err := store.RecordAssertionAttestation(
		provisional,
		testStoreCommitment(100),
		time.Unix(1_700_000_100, 0),
		true,
	); err != nil {
		t.Fatalf("record provisional attestation: %v", err)
	}

	root, found := store.HighestAttested(false)
	if !found || root.L2BlockNumber != 90 || root.Provisional {
		t.Fatalf("confirmed frontier: found=%t root=%+v", found, root)
	}
	root, found = store.HighestAttested(true)
	if !found || root.L2BlockNumber != 100 || !root.Provisional {
		t.Fatalf("provisional frontier: found=%t root=%+v", found, root)
	}
	root, found = store.HighestAttestedAtOrBelow(95, true)
	if !found || root.L2BlockNumber != 90 {
		t.Fatalf("frontier at or below 95: found=%t root=%+v", found, root)
	}

	store.SetNextL1Block(1_234)
	if err := store.Save(); err != nil {
		t.Fatalf("save store: %v", err)
	}
	reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 0)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if reloaded.NextL1Block() != 1_234 {
		t.Fatalf("reloaded cursor: got %d want 1234", reloaded.NextL1Block())
	}
	root, found = reloaded.HighestAttested(true)
	if !found || root.AssertionHash != provisional.AssertionHash {
		t.Fatalf("reloaded frontier: found=%t root=%+v", found, root)
	}
}

func TestAttestedRootStoreCommitPublishesOnlyAfterDurableSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "attested-roots.json")
	store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("LoadAttestedRootStore: %v", err)
	}
	if err := store.Commit(func(staged *AttestedRootStore) (bool, error) {
		staged.SetNextL1Block(2)
		return true, nil
	}); err != nil {
		t.Fatalf("initial Commit: %v", err)
	}

	fs := realAttestedRootFileSystem()
	fs.rename = func(string, string) error { return errors.New("rename failed") }
	store.fs = fs
	if err := store.Commit(func(staged *AttestedRootStore) (bool, error) {
		staged.SetNextL1Block(3)
		return true, nil
	}); err == nil {
		t.Fatal("Commit succeeded despite injected durable-save failure")
	}
	if got := store.NextL1Block(); got != 2 {
		t.Fatalf("published cursor = %d, want 2", got)
	}
	reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 0)
	if err != nil {
		t.Fatalf("reload durable state: %v", err)
	}
	if got := reloaded.NextL1Block(); got != 2 {
		t.Fatalf("durable cursor = %d, want 2", got)
	}
}

func TestAttestedRootStoreCommitSerializesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "attested-roots.json")
	store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("LoadAttestedRootStore: %v", err)
	}
	const writers = 32
	var writersDone sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		writersDone.Add(1)
		go func() {
			defer writersDone.Done()
			errs <- store.Commit(func(staged *AttestedRootStore) (bool, error) {
				staged.SetNextL1Block(staged.NextL1Block() + 1)
				return true, nil
			})
		}()
	}
	writersDone.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Commit: %v", err)
		}
	}
	if got := store.NextL1Block(); got != writers+1 {
		t.Fatalf("published cursor = %d, want %d", got, writers+1)
	}
	reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 0)
	if err != nil {
		t.Fatalf("reload durable state: %v", err)
	}
	if got := reloaded.NextL1Block(); got != writers+1 {
		t.Fatalf("durable cursor = %d, want %d", got, writers+1)
	}
}

// Commit must retain the old public snapshot for every failure before rename.
// A directory-sync failure is necessarily uncertain: rename has already made
// the replacement visible, so the durable file can be new even though Commit
// returns an error. The live store still refuses to publish that uncertain
// snapshot and the next reconciliation can replay it idempotently.
func TestAttestedRootStoreCommitFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		configure   func(testAttestedRootFileSystem) testAttestedRootFileSystem
		wantDurable uint64
	}{
		{
			name: "create directory",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir failed") }
				return fs
			},
			wantDurable: 1,
		},
		{
			name: "create temporary",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.createTemp = func(string, string) (attestedRootFile, error) { return nil, errors.New("create failed") }
				return fs
			},
			wantDurable: 1,
		},
		{
			name: "chmod temporary",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, chmodErr: errors.New("chmod failed")}, err
				}
				return fs
			},
			wantDurable: 1,
		},
		{
			name: "write temporary",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, writeErr: errors.New("write failed")}, err
				}
				return fs
			},
			wantDurable: 1,
		},
		{
			name: "sync temporary",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, syncErr: errors.New("sync failed")}, err
				}
				return fs
			},
			wantDurable: 1,
		},
		{
			name: "close temporary",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, closeErr: errors.New("close failed")}, err
				}
				return fs
			},
			wantDurable: 1,
		},
		{
			name: "rename",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.rename = func(string, string) error { return errors.New("rename failed") }
				return fs
			},
			wantDurable: 1,
		},
		{
			name: "sync directory",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.syncDir = func(string) error { return errors.New("directory sync failed") }
				return fs
			},
			wantDurable: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "attested-roots.json")
			store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
			if err != nil {
				t.Fatalf("LoadAttestedRootStore: %v", err)
			}
			if err := store.Save(); err != nil {
				t.Fatalf("initial Save: %v", err)
			}
			store.fs = tc.configure(realAttestedRootFileSystem())
			if err := store.Commit(func(staged *AttestedRootStore) (bool, error) {
				staged.SetNextL1Block(2)
				return true, nil
			}); err == nil {
				t.Fatal("Commit succeeded despite injected persistence failure")
			}
			if got := store.NextL1Block(); got != 1 {
				t.Fatalf("published cursor after failed Commit = %d, want 1", got)
			}
			reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 0)
			if err != nil {
				t.Fatalf("reload after failed Commit: %v", err)
			}
			if got := reloaded.NextL1Block(); got != tc.wantDurable {
				t.Fatalf("durable cursor after failed Commit = %d, want %d", got, tc.wantDurable)
			}
		})
	}
}

func TestAttestedRootStoreCommitIsRaceSafeWithReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "attested-roots.json")
	store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("LoadAttestedRootStore: %v", err)
	}
	stopReaders := make(chan struct{})
	var readersDone sync.WaitGroup
	for range 8 {
		readersDone.Add(1)
		go func() {
			defer readersDone.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					_ = store.NextL1Block()
					_, _ = store.HighestAttested(true)
					_ = store.Stats()
				}
			}
		}()
	}
	for want := uint64(2); want <= 16; want++ {
		if err := store.Commit(func(staged *AttestedRootStore) (bool, error) {
			staged.SetNextL1Block(want)
			return true, nil
		}); err != nil {
			close(stopReaders)
			readersDone.Wait()
			t.Fatalf("Commit cursor %d: %v", want, err)
		}
	}
	close(stopReaders)
	readersDone.Wait()
	reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 0)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := reloaded.NextL1Block(); got != 16 {
		t.Fatalf("durable cursor = %d, want 16", got)
	}
}

func TestAttestedRootStoreRejectsIdentityMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attested-roots.json")
	store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if err := store.BindSourceIdentity("l1:1/l2:42161/rollup:0x1"); err != nil {
		t.Fatalf("bind source identity: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("save store: %v", err)
	}
	if _, err := LoadAttestedRootStore(path, "arbitrum-sepolia", 1); err == nil {
		t.Fatal("expected persisted src_chain mismatch")
	}
	reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("reload identity-bound store: %v", err)
	}
	if err := reloaded.BindSourceIdentity("l1:1/l2:42161/rollup:0x2"); err == nil {
		t.Fatal("expected persisted source identity mismatch")
	}
}

func TestAttestedRootStoreDerivedLifecycle(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	first := testStoreCommitment(100)
	second := testStoreCommitment(200)
	third := testStoreCommitment(300)
	for _, commitment := range []BlockCommitment{first, second, third} {
		if err := store.AppendDerived(commitment, now, commitment.BlockNumber != 100); err != nil {
			t.Fatalf("append derived root: %v", err)
		}
	}

	if height, found := store.HighestDerivedBlock(); !found || height != 300 {
		t.Fatalf("highest derived block: found=%t height=%d", found, height)
	}
	if roots := store.DerivedProvisionalAtOrBelow(250); len(roots) != 1 || roots[0].L2BlockNumber != 200 {
		t.Fatalf("derived recheck list: %+v", roots)
	}
	if !store.ConfirmDerived(200) {
		t.Fatal("derived root was not confirmed")
	}
	corrected := third
	corrected.StateRoot = common.HexToHash("0xcafe")
	corrected.BlockHash = common.HexToHash("0xbeef")
	if !store.CorrectDerived(corrected.BlockNumber, corrected, now.Add(time.Second)) {
		t.Fatal("derived root was not corrected")
	}
	root, found := store.HighestAttested(false)
	if !found || root.L2BlockNumber != 300 || root.Root != corrected.StateRoot || root.L2BlockHash != corrected.BlockHash {
		t.Fatalf("corrected derived frontier: found=%t root=%+v", found, root)
	}
	if removed := store.PruneDerived(2); removed != 1 {
		t.Fatalf("pruned derived roots: got %d want 1", removed)
	}
	root, found = store.HighestAttestedAtOrBelow(100, false)
	if found {
		t.Fatalf("oldest derived root survived pruning: %+v", root)
	}
}

func TestAttestedRootStoreCorrectDerivedRejectsInvalidTargets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	t.Run("mismatched commitment height leaves every root untouched", func(t *testing.T) {
		store, err := NewAttestedRootStore("arbitrum-one", 1)
		if err != nil {
			t.Fatalf("create store: %v", err)
		}
		first := testStoreCommitment(100)
		second := testStoreCommitment(200)
		if err := store.AppendDerived(first, now, true); err != nil {
			t.Fatalf("append first root: %v", err)
		}
		if err := store.AppendDerived(second, now, true); err != nil {
			t.Fatalf("append second root: %v", err)
		}
		wrongHeight := second
		wrongHeight.StateRoot = common.HexToHash("0xcafe")
		wrongHeight.BlockHash = common.HexToHash("0xbeef")

		if store.CorrectDerived(first.BlockNumber, wrongHeight, now.Add(time.Second)) {
			t.Fatal("CorrectDerived accepted a commitment for another height")
		}
		for _, want := range []BlockCommitment{first, second} {
			got, ok := store.DerivedAt(want.BlockNumber)
			if !ok || !got.Provisional || got.Root != want.StateRoot || got.L2BlockHash != want.BlockHash {
				t.Fatalf("root at height %d changed: %+v ok=%t", want.BlockNumber, got, ok)
			}
		}
	})

	t.Run("confirmed root cannot be corrected", func(t *testing.T) {
		store, err := NewAttestedRootStore("arbitrum-one", 1)
		if err != nil {
			t.Fatalf("create store: %v", err)
		}
		original := testStoreCommitment(100)
		if err := store.AppendDerived(original, now, false); err != nil {
			t.Fatalf("append confirmed root: %v", err)
		}
		corrected := original
		corrected.StateRoot = common.HexToHash("0xcafe")
		corrected.BlockHash = common.HexToHash("0xbeef")

		if store.CorrectDerived(original.BlockNumber, corrected, now.Add(time.Second)) {
			t.Fatal("CorrectDerived overwrote an already-confirmed root")
		}
		got, ok := store.DerivedAt(original.BlockNumber)
		if !ok || got.Provisional || got.Root != original.StateRoot || got.L2BlockHash != original.BlockHash {
			t.Fatalf("confirmed root changed: %+v ok=%t", got, ok)
		}
	})
}

func TestAttestedRootStoreConfirmDerivedDoesNotReportChangeForConfirmedEntry(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	commitment := testStoreCommitment(100)
	if err := store.AppendDerived(commitment, time.Unix(1_700_000_000, 0), false); err != nil {
		t.Fatalf("append confirmed root: %v", err)
	}

	if store.ConfirmDerived(commitment.BlockNumber) {
		t.Fatal("ConfirmDerived reported a change for an already-confirmed root")
	}
}

func TestAttestedRootStoreRejectsInvalidAndConflictingAssertions(t *testing.T) {
	if _, err := NewAttestedRootStore("", 1); err == nil {
		t.Fatal("NewAttestedRootStore accepted an empty source chain")
	}
	if _, err := LoadAttestedRootStore("", "arbitrum-one", 1); err == nil {
		t.Fatal("LoadAttestedRootStore accepted an empty state path")
	}

	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.BindSourceIdentity(""); err == nil {
		t.Fatal("BindSourceIdentity accepted an empty identity")
	}
	if err := store.BindSourceIdentity("deployment-a"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	if err := store.BindSourceIdentity("deployment-b"); err == nil {
		t.Fatal("BindSourceIdentity accepted a conflicting identity")
	}
	if err := store.RecordProposal(ProposedAssertion{}); err == nil {
		t.Fatal("RecordProposal accepted zero assertion hash")
	}
	if err := store.RecordProposal(ProposedAssertion{AssertionHash: common.HexToHash("0x01")}); err == nil {
		t.Fatal("RecordProposal accepted zero L2 block hash")
	}

	proposal := testStoreProposal(1, 100)
	if err := store.RecordProposal(proposal); err != nil {
		t.Fatalf("record proposal: %v", err)
	}
	conflict := proposal
	conflict.ParentHash = common.HexToHash("0x99")
	if err := store.RecordProposal(conflict); err == nil {
		t.Fatal("RecordProposal accepted conflicting event data")
	}
	if err := store.MarkAssertionConfirmed(proposal.AssertionHash, common.HexToHash("0x99")); err == nil {
		t.Fatal("MarkAssertionConfirmed accepted a mismatched block hash")
	}
	commitment := BlockCommitment{
		BlockNumber: 100,
		BlockHash:   common.HexToHash("0x99"),
		StateRoot:   common.HexToHash("0x98"),
	}
	if err := store.RecordAssertionAttestation(proposal, commitment, time.Now(), true); err == nil {
		t.Fatal("RecordAssertionAttestation accepted a mismatched local commitment")
	}
	if err := store.AppendDerived(BlockCommitment{}, time.Now(), true); err == nil {
		t.Fatal("AppendDerived accepted an empty commitment")
	}
}

// The durable feed must fail closed at every filesystem step. Before rename,
// reload must still observe the prior snapshot; once rename succeeds, a later
// directory-sync error is uncertain but the new snapshot is already visible.
func TestAttestedRootStoreSaveFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(testAttestedRootFileSystem) testAttestedRootFileSystem
		wantCursor uint64
	}{
		{
			name: "create directory",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.mkdirAll = func(string, os.FileMode) error { return errors.New("mkdir failed") }
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "create temp",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.createTemp = func(string, string) (attestedRootFile, error) {
					return nil, errors.New("create failed")
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "chmod temp",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, chmodErr: errors.New("chmod failed")}, err
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "write temp",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, writeErr: errors.New("write failed")}, err
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "sync temp",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, syncErr: errors.New("sync failed")}, err
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "close temp",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				create := fs.createTemp
				fs.createTemp = func(dir, pattern string) (attestedRootFile, error) {
					file, err := create(dir, pattern)
					return failingAttestedRootFile{attestedRootFile: file, closeErr: errors.New("close failed")}, err
				}
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "rename temp",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.rename = func(string, string) error { return errors.New("rename failed") }
				return fs
			},
			wantCursor: 1,
		},
		{
			name: "sync directory after rename",
			configure: func(fs testAttestedRootFileSystem) testAttestedRootFileSystem {
				fs.syncDir = func(string) error { return errors.New("directory sync failed") }
				return fs
			},
			wantCursor: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "attested-roots.json")
			store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
			if err != nil {
				t.Fatalf("LoadAttestedRootStore: %v", err)
			}
			store.SetNextL1Block(1)
			if err := store.Save(); err != nil {
				t.Fatalf("initial Save: %v", err)
			}
			store.SetNextL1Block(2)
			store.fs = tc.configure(realAttestedRootFileSystem())
			if err := store.Save(); err == nil {
				t.Fatal("Save succeeded despite injected failure")
			}

			reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 0)
			if err != nil {
				t.Fatalf("LoadAttestedRootStore after failed save: %v", err)
			}
			if got := reloaded.NextL1Block(); got != tc.wantCursor {
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

func testStoreProposal(id byte, height uint64) ProposedAssertion {
	return ProposedAssertion{
		AssertionHash:    common.Hash{31: id},
		ParentHash:       common.Hash{30: id},
		L2BlockHash:      testStoreCommitment(height).BlockHash,
		InboxAccumulator: common.Hash{29: id},
		L1BlockNumber:    height + 1_000,
		Confirmed:        height < 100,
	}
}

func testStoreCommitment(height uint64) BlockCommitment {
	return BlockCommitment{
		BlockNumber: height,
		BlockHash:   common.BigToHash(newBigInt(height)),
		StateRoot:   common.BigToHash(newBigInt(height + 10_000)),
	}
}

func newBigInt(value uint64) *big.Int {
	return new(big.Int).SetUint64(value)
}

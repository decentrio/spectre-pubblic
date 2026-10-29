package services

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two state files this writer serves both guard against losing work, so
// "the write returned nil" has to mean the bytes survive a power cut, not
// merely that a reader will not see a half-written file.
func TestWriteStateFile(t *testing.T) {
	t.Run("creates the directory and the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "state.json")
		if err := writeStateFile(path, []byte(`{"version":1}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != `{"version":1}` {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("the file is not world-readable", func(t *testing.T) {
		// It carries relay progress, not secrets, but it sits beside the config
		// and inherits that directory's expectations.
		path := filepath.Join(t.TempDir(), "state.json")
		if err := writeStateFile(path, []byte("{}")); err != nil {
			t.Fatalf("write: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != stateFilePerm {
			t.Fatalf("mode = %o, want %o", info.Mode().Perm(), stateFilePerm)
		}
	})

	t.Run("replacing leaves no temp files behind", func(t *testing.T) {
		// A crashed write must not fill the state directory with debris, and a
		// leftover .tmp is also how a reader could be tempted to guess.
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		for i := 0; i < 3; i++ {
			if err := writeStateFile(path, []byte("{}")); err != nil {
				t.Fatalf("write %d: %v", i, err)
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("temp file left behind: %s", e.Name())
			}
		}
		if len(entries) != 1 {
			t.Fatalf("directory holds %d entries, want just the state file", len(entries))
		}
	})

	t.Run("an unwritable directory is reported, not swallowed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Skipf("cannot make the directory read-only here: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := writeStateFile(filepath.Join(dir, "state.json"), []byte("{}")); err == nil {
			t.Fatal("a write into a read-only directory reported success")
		}
	})

	// The durability step itself. rename is atomic, which is what stops a reader
	// seeing a half-written file -- but atomic is not durable: until the DIRECTORY
	// is synced, some filesystems can lose the rename across a power cut while
	// keeping the temp file that was already fsynced.
	//
	// A unit test cannot pull the power, so this asserts the mechanism instead:
	// fsyncDir must be called on the directory holding the file, and must report a
	// failure rather than returning nil. Deleting the fsyncDir call from
	// writeStateFile makes this fail, which is what the plan asks for.
	t.Run("syncs the directory", func(t *testing.T) {
		t.Run("fsyncDir reaches a real directory", func(t *testing.T) {
			if err := fsyncDir(t.TempDir()); err != nil {
				t.Fatalf("syncing a real directory failed: %v", err)
			}
		})

		t.Run("a directory that cannot be synced is an error", func(t *testing.T) {
			if err := fsyncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
				t.Fatal("syncing a missing directory reported success")
			}
		})

		// The wiring: writeStateFile must go through fsyncDir rather than stopping at
		// the rename. Asserted structurally because the effect is invisible to a
		// process that does not crash -- the same reason it was missing from both
		// writers for so long.
		t.Run("writeStateFile calls it", func(t *testing.T) {
			src, err := os.ReadFile("statefile.go")
			if err != nil {
				t.Fatalf("read statefile.go: %v", err)
			}
			body := string(src)
			start := strings.Index(body, "func writeStateFileWithDirSync(")
			if start < 0 {
				t.Fatal("writeStateFileWithDirSync not found")
			}
			end := strings.Index(body[start:], "\nfunc ")
			if end < 0 {
				end = len(body) - start
			}
			fn := body[start : start+end]
			if !strings.Contains(fn, "syncDir(dir)") {
				t.Fatal("writeStateFileWithDirSync does not sync the directory after renaming; the rename is " +
					"atomic but not durable, and losing it loses a recovery cursor with nothing to show for it")
			}
			// And the production entry point must go through it rather than round it.
			if !strings.Contains(body, "return writeStateFileWithDirSync(path, data, fsyncDir)") {
				t.Fatal("writeStateFile does not delegate to writeStateFileWithDirSync with the real fsyncDir; " +
					"the seam would then describe a path production does not take")
			}
		})
	})

	t.Run("syncs parent directory after rename", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state", "pending.json")
		called := ""
		syncDir := func(dir string) error {
			called = dir
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(data) != "snapshot" {
				return errors.New("directory synced before replacement became visible")
			}
			return nil
		}

		if err := writeStateFileWithDirSync(path, []byte("snapshot"), syncDir); err != nil {
			t.Fatal(err)
		}
		if called != filepath.Dir(path) {
			t.Fatalf("synced directory = %q, want %q", called, filepath.Dir(path))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "snapshot" {
			t.Fatalf("snapshot contents = %q, want replacement data", data)
		}
	})
}

// The first write creates the whole hierarchy, and a new directory is an entry
// in its PARENT -- syncing the leaf does not make that entry durable. Reported by
// @neitdung: a power cut after a reported-successful first write could take
// .spectre-state and everything under it, which is the silent cursor and
// pending-state loss this helper exists to prevent.
//
// A unit test cannot pull the power either, so this pins the input the durability
// step is driven from: which levels this process created, outermost first.
func TestMissingDirs_ListsWhatMustBeMadeDurable(t *testing.T) {
	root := t.TempDir()

	t.Run("every level that does not exist yet, outermost first", func(t *testing.T) {
		target := filepath.Join(root, "a", ".spectre-state", "cosmos-to-eth-0011")
		got := missingDirs(target)
		want := []string{
			filepath.Join(root, "a"),
			filepath.Join(root, "a", ".spectre-state"),
			target,
		}
		if len(got) != len(want) {
			t.Fatalf("missingDirs = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("missingDirs[%d] = %q, want %q (outermost first, so each parent is durable "+
					"before the entry inside it is synced)", i, got[i], want[i])
			}
		}
	})

	t.Run("an existing directory needs nothing", func(t *testing.T) {
		if got := missingDirs(root); len(got) != 0 {
			t.Fatalf("missingDirs(%q) = %v, want none: it already exists", root, got)
		}
	})

	// The wiring, structurally, for the same reason as the test above it: the
	// effect is invisible to a process that does not crash.
	t.Run("writeStateFile syncs each level it created", func(t *testing.T) {
		src, err := os.ReadFile("statefile.go")
		if err != nil {
			t.Fatalf("read statefile.go: %v", err)
		}
		body := string(src)
		start := strings.Index(body, "func writeStateFileWithDirSync(")
		if start < 0 {
			t.Fatal("writeStateFileWithDirSync not found")
		}
		end := strings.Index(body[start:], "\nfunc ")
		if end < 0 {
			end = len(body) - start
		}
		fn := body[start : start+end]
		if !strings.Contains(fn, "MkdirAllDurable(dir,") {
			t.Fatal("writeStateFile does not make the directories it created durable; only the leaf is " +
				"synced, so a first write can report success and then lose the whole state directory")
		}
	})

	t.Run("a first write into a three-level path still succeeds", func(t *testing.T) {
		path := filepath.Join(root, "b", ".spectre-state", "cosmos-to-eth-0011", "cursors.json")
		if err := writeStateFile(path, []byte(`{"ok":true}`)); err != nil {
			t.Fatalf("first write: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != `{"ok":true}` {
			t.Fatalf("content = %q", got)
		}
	})
}

// MkdirAllDurable is the shared helper, and cmd.lockStateDir is its other caller
// -- the one that in practice creates the whole hierarchy, long before any state
// file is written. Reported by @DongLieu: a durability fix living only in
// writeStateFile never runs, because by then there is nothing left to create.
func TestMkdirAllDurable(t *testing.T) {
	root := t.TempDir()

	t.Run("creates every level and reports success", func(t *testing.T) {
		dir := filepath.Join(root, "a", ".spectre-state", "cosmos-to-eth-0011")
		if err := MkdirAllDurable(dir, stateDirPerm); err != nil {
			t.Fatalf("MkdirAllDurable: %v", err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("stat %q: %v", dir, err)
		}
	})

	t.Run("an existing directory is not an error", func(t *testing.T) {
		if err := MkdirAllDurable(root, stateDirPerm); err != nil {
			t.Fatalf("MkdirAllDurable on an existing dir: %v", err)
		}
	})

	t.Run("a path that cannot be created is an error", func(t *testing.T) {
		file := filepath.Join(root, "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := MkdirAllDurable(filepath.Join(file, "under-a-file"), stateDirPerm); err == nil {
			t.Fatal("creating a directory under a regular file reported success")
		}
	})
}

func TestSyncStateDirHierarchy(t *testing.T) {
	// The property MkdirAllDurable cannot have: durability that does not depend on
	// being the first caller to create anything.
	//
	// MkdirAllDurable records what was missing at entry, so once some earlier step
	// has created the hierarchy it syncs nothing. Twice now that earlier step turned
	// out to exist -- lockStateDir before writeStateFile, then validateStartupConfig
	// before lockStateDir -- and each fix moved the sync one caller upstream instead
	// of removing the dependency. This asserts the dependency is gone: the same call
	// syncs the same three levels whether they were just created or have been there
	// all along.
	t.Run("does not depend on who created the directories", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, ".spectre-state", "cosmos-to-eth-0011")

		collect := func() []string {
			var synced []string
			if err := syncStateDirHierarchyWith(stateDir, func(dir string) error {
				synced = append(synced, dir)
				return nil
			}); err != nil {
				t.Fatalf("syncStateDirHierarchyWith: %v", err)
			}
			return synced
		}

		want := []string{stateDir, filepath.Join(root, ".spectre-state"), root}

		// Pre-created by somebody else entirely -- the case that broke the last two
		// fixes. A bare MkdirAll here is the point, not an oversight: it is what
		// cmd.validateStateDir does.
		if err := os.MkdirAll(stateDir, stateDirPerm); err != nil {
			t.Fatalf("pre-create: %v", err)
		}
		preexisting := collect()

		// And again on a hierarchy this call would itself have had to create.
		fresh := filepath.Join(t.TempDir(), ".spectre-state", "cosmos-to-eth-0011")
		var freshSynced []string
		if err := syncStateDirHierarchyWith(fresh, func(dir string) error {
			freshSynced = append(freshSynced, dir)
			return nil
		}); err != nil {
			t.Fatalf("syncStateDirHierarchyWith on a fresh path: %v", err)
		}

		if len(preexisting) != len(want) {
			t.Fatalf("synced %v on a pre-created hierarchy, want the three levels %v", preexisting, want)
		}
		for i := range want {
			if preexisting[i] != want[i] {
				t.Fatalf("level %d = %q, want %q (full walk %v)", i, preexisting[i], want[i], preexisting)
			}
		}
		if len(freshSynced) != len(preexisting) {
			t.Fatalf("a fresh hierarchy synced %d level(s) and a pre-created one %d; "+
				"durability must not depend on who created the directories", len(freshSynced), len(preexisting))
		}
	})

	// The walk stops at the config directory rather than climbing to the filesystem
	// root: the level above is the operator's own, not one this relayer created, and
	// fsyncing every ancestor up to / on a startup path buys nothing.
	t.Run("stops at the config directory", func(t *testing.T) {
		deep := filepath.Join(t.TempDir(), "a", "b", "c", ".spectre-state", "cosmos-to-eth-0011")
		var synced []string
		if err := syncStateDirHierarchyWith(deep, func(dir string) error {
			synced = append(synced, dir)
			return nil
		}); err != nil {
			t.Fatalf("syncStateDirHierarchyWith: %v", err)
		}
		if len(synced) != stateDirOwnedLevels {
			t.Fatalf("synced %d level(s) (%v), want exactly %d", len(synced), synced, stateDirOwnedLevels)
		}
	})

	// A directory that cannot be synced is not a warning: it means the state this
	// process is about to rely on may not survive a power cut, and the caller
	// (lockStateDir) refuses to boot on it.
	t.Run("reports a failed sync", func(t *testing.T) {
		err := syncStateDirHierarchyWith(filepath.Join(t.TempDir(), ".spectre-state", "x"),
			func(string) error { return errors.New("sync refused") })
		if err == nil {
			t.Fatal("a refused directory sync was reported as success")
		}
		if !strings.Contains(err.Error(), "durable") {
			t.Fatalf("error = %v; it must say what was not made durable", err)
		}
	})
}

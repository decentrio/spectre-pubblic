package main

import (
	"os"
	"path/filepath"
	"relayer/services"
	"strconv"
	"strings"
	"testing"
)

// G3: one directory, one owner. relayStateDir removes accidental collisions;
// this covers the deliberate mistake of starting the same command twice.
func TestLockStateDir(t *testing.T) {
	t.Run("the first caller gets it", func(t *testing.T) {
		lock, err := lockStateDir(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatalf("first lock: %v", err)
		}
		t.Cleanup(func() { _ = lock.Close() })
	})

	t.Run("it records who holds it", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		lock, err := lockStateDir(dir)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		t.Cleanup(func() { _ = lock.Close() })

		data, err := os.ReadFile(filepath.Join(dir, stateLockFileName))
		if err != nil {
			t.Fatalf("read lock file: %v", err)
		}
		if !strings.Contains(string(data), strconv.Itoa(os.Getpid())) {
			t.Fatalf("lock file does not name the holding pid: %q", data)
		}
	})

	t.Run("a second holder is refused, naming the directory and the pid", func(t *testing.T) {
		// The acceptance case: `start` run twice with the same config and client
		// id. flock is per open file description, so a second open in this same
		// process is refused exactly as a second process would be.
		//
		// The message has to carry both facts. "Already locked" tells an operator
		// nothing they can act on; the directory says which pair collided and the
		// pid says which process to look at.
		dir := filepath.Join(t.TempDir(), "state")
		first, err := lockStateDir(dir)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		t.Cleanup(func() { _ = first.Close() })

		second, err := lockStateDir(dir)
		if err == nil {
			_ = second.Close()
			t.Fatal("a second holder acquired the same state directory; both would advance the same cursors")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error does not name the directory that collided: %v", err)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
			t.Errorf("error does not name the pid holding it: %v", err)
		}
	})

	t.Run("the refused caller does not disturb the holder", func(t *testing.T) {
		// A failed second start must leave the running relayer untouched -- it must
		// not truncate the lock file, and the holder must still own the lock.
		dir := filepath.Join(t.TempDir(), "state")
		first, err := lockStateDir(dir)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		t.Cleanup(func() { _ = first.Close() })
		before, err := os.ReadFile(filepath.Join(dir, stateLockFileName))
		if err != nil {
			t.Fatalf("read lock: %v", err)
		}

		if _, err := lockStateDir(dir); err == nil {
			t.Fatal("second lock succeeded")
		}

		after, err := os.ReadFile(filepath.Join(dir, stateLockFileName))
		if err != nil {
			t.Fatalf("read lock after: %v", err)
		}
		if string(before) != string(after) {
			t.Fatalf("the refused caller rewrote the holder's lock file:\n before %q\n after  %q", before, after)
		}
	})

	t.Run("different directories do not contend", func(t *testing.T) {
		// Two paths with different client ids must both start. This is the control
		// for the test below: a lock that refused everything would pass that one.
		root := t.TempDir()
		first, err := lockStateDir(filepath.Join(root, "a"))
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		t.Cleanup(func() { _ = first.Close() })
		second, err := lockStateDir(filepath.Join(root, "b"))
		if err != nil {
			t.Fatalf("two separate directories contended: %v", err)
		}
		t.Cleanup(func() { _ = second.Close() })
	})

	t.Run("releasing it hands the directory over", func(t *testing.T) {
		// flock is released by the kernel when a process dies, including on
		// SIGKILL, so a crashed relayer leaves nothing to clean up by hand. Within
		// one process, closing the descriptor is the same event.
		dir := filepath.Join(t.TempDir(), "state")
		first, err := lockStateDir(dir)
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		if err := first.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		second, err := lockStateDir(dir)
		if err != nil {
			t.Fatalf("the directory stayed locked after its holder released it: %v", err)
		}
		t.Cleanup(func() { _ = second.Close() })
	})

	// The order the process actually runs in: lockStateDir creates the hierarchy to
	// claim the lock, and only later does anything write a state file.
	//
	// Reported by @DongLieu on #439. Making writeStateFile sync the levels IT created
	// fixes nothing here, because by then there are none -- lockStateDir made them
	// all. The durability has to live where the creation does, which is why both go
	// through one helper.
	t.Run("creates the hierarchy durably", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, ".spectre-state", "cosmos-to-eth-0011")

		lock, err := lockStateDir(stateDir)
		if err != nil {
			t.Fatalf("lockStateDir: %v", err)
		}
		defer lock.Close()

		for _, dir := range []string{filepath.Join(root, ".spectre-state"), stateDir} {
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat %q: %v", dir, err)
			}
			if !info.IsDir() {
				t.Fatalf("%q is not a directory", dir)
			}
		}

		// The fact that makes the durability lockStateDir's job: nothing is left for
		// a later state write to create, so a fix that only runs on creation must run
		// here or it never runs at all.
		nested := filepath.Join(stateDir, "cursors.json")
		if _, err := os.Stat(filepath.Dir(nested)); err != nil {
			t.Fatalf("the first state write would still have to create %q: %v", filepath.Dir(nested), err)
		}

		// fsync is invisible to a process that does not crash, so the wiring is
		// asserted structurally -- the same reason services/statefile_test.go does it.
		src, err := os.ReadFile("statelock.go")
		if err != nil {
			t.Fatalf("read statelock.go: %v", err)
		}
		body := string(src)
		start := strings.Index(body, "func lockStateDir(")
		if start < 0 {
			t.Fatal("lockStateDir not found")
		}
		end := strings.Index(body[start:], "\nfunc ")
		if end < 0 {
			end = len(body) - start
		}
		fn := body[start : start+end]
		if !strings.Contains(fn, "services.MkdirAllDurable(") {
			t.Error("lockStateDir does not create the state hierarchy durably; a power cut after a " +
				"first start can take .spectre-state and every cursor under it")
		}
		if strings.Contains(fn, "os.MkdirAll(") {
			t.Error("lockStateDir calls os.MkdirAll directly, which creates the directories without " +
				"making their entries durable in their parents")
		}
	})

	// The production order, which the test above does not run.
	//
	// TestLockStateDir/creates the hierarchy durably starts at lockStateDir with an
	// empty tree, so MkdirAllDurable finds every level missing and its sync loop
	// runs. Start does not work that way: validateStartupConfig resolves the same
	// directory and creates it (validateStateDir, cmd/config.go) BEFORE any lock is
	// taken, so by the time lockStateDir runs there is nothing missing and that loop
	// is empty. Reported by @neitdung, and it is the second time a durability fix
	// has been found to sit behind an earlier creator.
	//
	// This drives the real sequence and asserts the outcome does not depend on it.
	t.Run("hierarchy is durable through the real startup order", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, ".spectre-state", "cosmos-to-eth-0011")

		// 1. Validation runs first and creates the whole hierarchy, non-durably.
		if findings := validateStateDir(stateDir); len(findings) != 0 {
			t.Fatalf("validateStateDir on a writable path reported %v", findings)
		}
		// The precondition that makes this test meaningful: after validation there is
		// nothing left for a create-time fix to create. If this ever stops holding,
		// the test is no longer exercising the case it was written for.
		for _, dir := range []string{filepath.Join(root, ".spectre-state"), stateDir} {
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("validateStateDir did not create %q (%v); this test no longer covers "+
					"the order it was written for", dir, err)
			}
		}

		// 2. Then the lock, which is where durability has to happen regardless.
		lock, err := lockStateDir(stateDir)
		if err != nil {
			t.Fatalf("lockStateDir after validation: %v", err)
		}
		defer lock.Close()

		// 3. And a first state write into the directory, through the real store,
		//    which must still work once the lock owns the hierarchy.
		store, err := services.LoadRecoveryState(filepath.Join(stateDir, services.RecoveryCursorsFileName))
		if err != nil {
			t.Fatalf("load recovery state: %v", err)
		}
		if err := store.SaveCheckpoint(services.RecoveryCursors{"08-wasm-0": 42}); err != nil {
			t.Fatalf("first state write: %v", err)
		}

		// fsync is invisible to a process that does not crash, so the unconditional
		// half is asserted structurally -- for the same reason the test above does,
		// and because it is exactly the half that has been silently dead twice.
		src, err := os.ReadFile("statelock.go")
		if err != nil {
			t.Fatalf("read statelock.go: %v", err)
		}
		body := string(src)
		start := strings.Index(body, "func lockStateDir(")
		if start < 0 {
			t.Fatal("lockStateDir not found")
		}
		end := strings.Index(body[start:], "\nfunc ")
		if end < 0 {
			end = len(body) - start
		}
		if fn := body[start : start+end]; !strings.Contains(fn, "services.SyncStateDirHierarchy(") {
			t.Error("lockStateDir relies on MkdirAllDurable alone, which syncs only what it created; " +
				"after validateStateDir has created the hierarchy that is nothing, and a power cut " +
				"after a first start can still take .spectre-state and every cursor under it")
		}
	})
}

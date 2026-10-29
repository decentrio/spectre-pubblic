package services

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	stateFilePerm = 0o600
	stateDirPerm  = 0o700
	// stateDirOwnedLevels is how far up SyncStateDirHierarchy walks: the per-path
	// directory, .spectre-state, and the config directory that holds it. See the
	// diagram on SyncStateDirHierarchy for why the walk stops there.
	stateDirOwnedLevels = 3
)

// writeStateFile replaces path with data, atomically and durably.
//
// The recipe is mkdir-durable / write-temp / fsync-temp / rename / fsync-DIR,
// and the last step is the one both callers used to omit. rename is atomic on
// POSIX, so a reader sees either the old file or the new one and never a
// half-written mixture -- but atomic is not the same as durable. The rename is a
// directory-entry change, and until the DIRECTORY is fsynced some filesystems
// can lose it across a power cut while keeping the temp file that was already
// fsynced. The result is the old cursor, or no cursor, with nothing to indicate
// anything was lost.
//
// The two state files this serves both guard against losing work: a lost
// recovery cursor skips a whole window of source events, and a lost pending
// record hides packets from the timeout scanner, leaving funds escrowed. That is
// why the durability step is not optional here.
//
// The temp file is removed on every failure path so a crashed write cannot leave
// the directory filling with debris.
func writeStateFile(path string, data []byte) error {
	return writeStateFileWithDirSync(path, data, fsyncDir)
}

// writeStateFileWithDirSync keeps the filesystem boundary injectable without a
// package-global test hook. The production path always uses fsyncDir.
func writeStateFileWithDirSync(path string, data []byte, syncDir func(string) error) error {
	dir := filepath.Dir(path)
	if err := MkdirAllDurable(dir, stateDirPerm); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(stateFilePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary state file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state file %q: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync state directory %q: %w", dir, err)
	}
	return nil
}

// MkdirAllDurable creates dir and every missing level above it, and makes each
// new directory's entry durable in its own parent before returning.
//
// A new directory is an entry in its PARENT, and syncing the child does not make
// that entry durable -- the same distinction the rename above relies on, one
// level up. On a first start MkdirAll can create .spectre-state and the per-path
// directory under it, and a power cut afterwards could take the whole hierarchy
// with it: no cursor, no pending records, and nothing to say anything was lost.
//
// It is exported because the process does NOT create that hierarchy here.
// Reported by @DongLieu: cmd.lockStateDir creates the per-path directory at
// startup to claim the lock, long before any state is written, so by the time
// writeStateFile runs there is nothing left for it to create and a fix living
// only here would never execute in production. One helper, used by both, is what
// keeps that from being true again -- the creation and the durability cannot
// drift apart if they are the same call.
//
// It syncs through fsyncDir rather than through the caller's syncDir seam: that
// seam exists to observe the RENAME being made durable, which is a different
// event, and routing directory creation through it would make the seam's test
// assert on whichever call happened to come last.
func MkdirAllDurable(dir string, perm os.FileMode) error {
	if dir == "" || dir == "." {
		return nil
	}
	// Recorded BEFORE the call: afterwards there is no way to tell which levels
	// this process made.
	created := missingDirs(dir)
	if err := os.MkdirAll(dir, perm); err != nil {
		return fmt.Errorf("create state dir %q: %w", dir, err)
	}
	for _, made := range created {
		if err := fsyncDir(filepath.Dir(made)); err != nil {
			return fmt.Errorf("make new state dir %q durable: %w", made, err)
		}
	}
	return nil
}

// SyncStateDirHierarchy makes the state directory and the levels above it
// durable, WHOEVER created them and whenever.
//
// MkdirAllDurable syncs only the levels it created itself. That is right for
// writeStateFile -- it runs on every cursor update and must not fsync the chain
// each time -- and wrong for startup, because it makes durability depend on
// being the first caller to create anything. That dependency has now moved twice
// under review rather than been fixed: the sync lived in writeStateFile until
// @DongLieu found lockStateDir creates the hierarchy first, then in lockStateDir
// until @neitdung found validateStartupConfig creates it first (cmd/config.go,
// before any lock is taken). Making each new creator durable is the strategy
// that failed both times, and a fourth creator would break it again.
//
// This is unconditional instead, so ordering cannot matter. The cost is three
// directory fsyncs, once, on a startup path that goes on to dial chains.
//
// Three and no more, because three is what this layout owns:
//
//	<config dir>/          <- syncing it makes .spectre-state's entry durable
//	  .spectre-state/      <- syncing it makes the per-path directory's entry durable
//	    <kind>-<digest>/   <- syncing it makes the lock and cursor entries durable
//
// The directory above holds the operator's own config file; it is not ours to
// have created, so the walk stops there.
func SyncStateDirHierarchy(dir string) error {
	return syncStateDirHierarchyWith(dir, fsyncDir)
}

// syncStateDirHierarchyWith keeps the filesystem boundary injectable without a
// package-global test hook, the same way writeStateFileWithDirSync does.
func syncStateDirHierarchyWith(dir string, syncDir func(string) error) error {
	if dir == "" || dir == "." {
		return nil
	}
	level := filepath.Clean(dir)
	for i := 0; i < stateDirOwnedLevels; i++ {
		if err := syncDir(level); err != nil {
			return fmt.Errorf("make state dir %q durable: %w", level, err)
		}
		parent := filepath.Dir(level)
		if parent == level {
			break // reached the root; there is no further parent to walk to
		}
		level = parent
	}
	return nil
}

// fsyncDir makes a rename durable. Opening a directory read-only and syncing it
// is the portable way to do that on POSIX; the error is reported rather than
// swallowed, because "the write succeeded but might not survive a crash" is not
// a success the caller can act on.
func fsyncDir(dir string) error {
	if dir == "" {
		dir = "."
	}
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state dir %q for sync: %w", dir, err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("sync state dir %q: %w", dir, err)
	}
	return handle.Close()
}

// missingDirs lists the levels of dir that do not exist yet, OUTERMOST FIRST, so
// a caller that creates them can make each one durable in the order the
// filesystem created it.
//
// Order matters: syncing the parent of an inner directory before the outer one
// exists on disk would sync an entry that is itself not yet durable.
//
// Anything Stat cannot confirm counts as missing. The only caller creates the
// chain immediately afterwards and reports that failure, so a permission error
// surfaces there with a better message than this function could give.
func missingDirs(dir string) []string {
	var reversed []string
	for d := filepath.Clean(dir); ; {
		if _, err := os.Stat(d); err == nil {
			break
		}
		reversed = append(reversed, d)
		parent := filepath.Dir(d)
		if parent == d {
			break // reached the root; there is no further parent to walk to
		}
		d = parent
	}
	missing := make([]string, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		missing = append(missing, reversed[i])
	}
	return missing
}

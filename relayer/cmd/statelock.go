package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"relayer/services"
)

// stateLockFileName marks a state directory as owned by a running process.
const stateLockFileName = ".lock"

// lockStateDir claims exclusive ownership of a state directory for the lifetime
// of this process, or fails immediately naming the process that already holds it.
//
// relayStateDir already removes accidental collisions: two runs collide only if
// they share a config path, a direction AND a client id. What is left is the
// deliberate mistake -- starting `start` twice with the same arguments. Identical
// identity means the same directory, and both processes would then advance the
// same cursors, each unaware of the other. Neither file is corrupted; the cursors
// are simply wrong, and the first symptom is a window of source events nobody
// scanned.
//
// Three choices here, all deliberate:
//
//   - LOCK_NB, never blocking. A second process that waited would hang with no
//     output, and from outside a hung relayer is indistinguishable from a working
//     one. Failing at startup with the reason is the honest behaviour.
//   - Taken at startup, not around each write. A per-write lock still lets two
//     processes coexist and take turns overwriting each other's cursors. What has
//     to be excluded is two OWNERS, not two concurrent writes.
//   - flock is released by the kernel when the process dies, including on
//     SIGKILL. So there are no stale locks to clean up by hand, which is the
//     failure mode of a pid file used as a lock.
//
// The returned closer must live as long as the process. Closing the descriptor
// releases the lock, so callers keep it until exit rather than deferring it into
// a shorter scope.
func lockStateDir(dir string) (*os.File, error) {
	// Durable, not just created. This is where the hierarchy actually comes into
	// existence -- the lock is claimed before anything is relayed, so by the time
	// a state file is written there is nothing left to create. A new directory is
	// an entry in its parent, so without syncing that parent a power cut right
	// after a first start can take .spectre-state and every cursor under it, and
	// the next start finds an empty tracker with nothing to say work was lost.
	if err := services.MkdirAllDurable(dir, 0o700); err != nil {
		return nil, err
	}
	// And sync unconditionally, because MkdirAllDurable above syncs only what IT
	// created -- and by the time this runs, it has usually created nothing.
	// validateStartupConfig (cmd/config.go) resolves the same directory and mkdirs
	// it before any lock is taken, so on a first start the hierarchy already
	// exists here and the loop inside MkdirAllDurable is empty.
	//
	// Reported by @neitdung, and it is the third place this same dependency has
	// surfaced: the sync was in writeStateFile until lockStateDir was found to
	// create the hierarchy first, then here until validation was found to create
	// it first. Chasing the creator is what failed twice. This does not care who
	// created what.
	if err := services.SyncStateDirHierarchy(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, stateLockFileName)

	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock %q: %w", path, err)
	}

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readLockHolder(lock)
		_ = lock.Close()
		return nil, fmt.Errorf(
			"state directory %s is held by another relayer process%s; "+
				"one source→destination pair runs exactly one process, so either stop that one "+
				"or give this path its own config and client id: %w",
			dir, holder, err)
	}

	// Written after the lock is held, so the contents always describe the current
	// owner rather than a process that failed to acquire it.
	if err := writeLockHolder(lock); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

// writeLockHolder records who owns the directory. It is diagnostic only -- the
// lock itself is the flock -- so the file is truncated and rewritten in place
// rather than going through the atomic-replace path: replacing it by rename
// would hand the new inode a different lock.
func writeLockHolder(lock *os.File) error {
	if err := lock.Truncate(0); err != nil {
		return fmt.Errorf("truncate state lock: %w", err)
	}
	holder := fmt.Appendf(nil, "pid %d\nstarted %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if _, err := lock.WriteAt(holder, 0); err != nil {
		return fmt.Errorf("write state lock: %w", err)
	}
	return nil
}

// readLockHolder renders the holder for an error message. Anything unreadable
// yields an empty string: the lock failure is the message, and a diagnostic that
// cannot be read must not replace it with an error of its own.
func readLockHolder(lock *os.File) string {
	data := make([]byte, 256)
	n, _ := lock.ReadAt(data, 0)
	if n == 0 {
		return ""
	}
	fields := strings.Join(strings.Fields(string(data[:n])), " ")
	if fields == "" {
		return ""
	}
	return " (" + fields + ")"
}

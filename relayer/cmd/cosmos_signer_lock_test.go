package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// A2 decided one Cosmos signing key per process, and nothing in the code can
// notice the violation on its own: cosmosMu serializes the account sequence
// WITHIN a process, so two processes sharing a key each believe they own it and
// the loser of every race sees "account sequence mismatch" -- a message naming
// neither the other process nor the shared key.
func TestAcquireCosmosSignerLock(t *testing.T) {
	const address = "cosmos1testsigneraddress"

	// One cache dir per subtest stands in for one machine. Every acquire inside a
	// subtest resolves the SAME directory no matter what else changes, which is
	// the property under test -- see the cross-state-directory case below.
	setup := func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		restoreDir := signerLockDir
		signerLockDir = func() (string, error) { return dir, nil }
		restore := cosmosSignerAddressForLock
		cosmosSignerAddressForLock = func() (string, error) { return address, nil }
		t.Cleanup(func() {
			cosmosSignerAddressForLock = restore
			signerLockDir = restoreDir
		})
	}

	t.Run("the second process with the same key is refused", func(t *testing.T) {
		setup(t)

		release, err := acquireCosmosSignerLock()
		if err != nil {
			t.Fatalf("first process could not take the lock: %v", err)
		}
		defer release()

		_, err = acquireCosmosSignerLock()
		if err == nil {
			t.Fatal("a second process took the same signing key; the sequence race this prevents is invisible until it happens")
		}
		// The operator has to learn which key and which file, or the message
		// sends them looking for an RPC problem.
		lockPath, pathErr := signerLockPathFor(address)
		if pathErr != nil {
			t.Fatalf("signerLockPathFor: %v", pathErr)
		}
		for _, want := range []string{address, lockPath, "COSMOS_PRIVATE_KEY"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not mention %q", err, want)
			}
		}
	})

	// Releasing must actually hand the lock over: a restart that cannot re-take
	// its own lock is worse than no lock at all.
	t.Run("releasing lets the next process in", func(t *testing.T) {
		setup(t)

		release, err := acquireCosmosSignerLock()
		if err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		release()

		release2, err := acquireCosmosSignerLock()
		if err != nil {
			t.Fatalf("the lock was not released: %v", err)
		}
		release2()
	})

	// A different key is a different process's business. Locking per machine
	// rather than per signing address would stop two legitimate relayers.
	t.Run("a different signing address is not blocked", func(t *testing.T) {
		setup(t)

		release, err := acquireCosmosSignerLock()
		if err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		defer release()

		cosmosSignerAddressForLock = func() (string, error) { return "cosmos1othersigner", nil }
		release2, err := acquireCosmosSignerLock()
		if err != nil {
			t.Fatalf("a second process with its OWN key must start: %v", err)
		}
		release2()
	})

	// Reported by @DongLieu on #467. The lock used to be created under
	// filepath.Dir(RELAYER_RECOVERY_STATE_FILE), and docs/E2E.md tells the
	// operator to give each source its own state directory -- so the two
	// processes this guard exists to catch took two different lock files and both
	// started. The guard was off in precisely the setup that needs it.
	t.Run("the same key is refused across different state directories", func(t *testing.T) {
		setup(t)

		t.Setenv("RELAYER_RECOVERY_STATE_FILE", filepath.Join(t.TempDir(), "recovery-cursors.json"))
		release, err := acquireCosmosSignerLock()
		if err != nil {
			t.Fatalf("first process could not take the lock: %v", err)
		}
		defer release()

		// Second process: its own state directory, its own working directory, the
		// same COSMOS_PRIVATE_KEY.
		t.Setenv("RELAYER_RECOVERY_STATE_FILE", filepath.Join(t.TempDir(), "recovery-cursors.json"))
		release2, err := acquireCosmosSignerLock()
		if err == nil {
			release2()
			t.Fatal("a second process with the same key started because its state directory differed; " +
				"the account-sequence race is back and invisible until it happens")
		}
	})

	// Deliberately NOT setup(t): that stubs signerLockDir, and the real one is
	// what has to ignore the state directory. Stubbing it here would assert the
	// stub. Computing a path writes nothing, so the real cache dir is untouched.
	t.Run("the real lock directory does not depend on the state directory", func(t *testing.T) {
		t.Setenv("RELAYER_RECOVERY_STATE_FILE", filepath.Join(t.TempDir(), "a", "recovery-cursors.json"))
		first, err := signerLockPathFor(address)
		if err != nil {
			t.Fatalf("signerLockPathFor: %v", err)
		}

		t.Setenv("RELAYER_RECOVERY_STATE_FILE", filepath.Join(t.TempDir(), "b", "recovery-cursors.json"))
		second, err := signerLockPathFor(address)
		if err != nil {
			t.Fatalf("signerLockPathFor: %v", err)
		}

		if first != second {
			t.Fatalf("lock path moved with the state directory: %q then %q", first, second)
		}
	})

	// No cache directory has to fail, not fall back. Two processes that pick
	// different fallbacks take different locks and both start.
	t.Run("an unresolvable lock directory refuses to start", func(t *testing.T) {
		setup(t)

		signerLockDir = func() (string, error) { return "", errNoCacheDir }
		if _, err := acquireCosmosSignerLock(); err == nil {
			t.Fatal("a process started with nowhere to put its lock; the guard was silently off")
		}
	})
}

var errNoCacheDir = errors.New("no cache dir in this test")

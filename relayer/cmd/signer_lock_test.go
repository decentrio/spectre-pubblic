package main

import (
	"os"
	"testing"
)

// Reported by @DongLieu on #467, and it is the same defect as the one this file
// already fixed once, one level further out. The first version keyed the lock off
// the recovery state directory, which the runbook tells operators to vary. The
// second used os.UserCacheDir, which reads XDG_CACHE_HOME on Unix and HOME on
// macOS -- and start loads .env at start.go:39 before taking the lock at :91, so
// a variable set there (or simply two units running under different HOME) hands
// the two processes different lock files and lets both start.
//
// The lock path must therefore be a function of the user and the machine and
// nothing else. This drives the REAL resolver, not the test seam, because the
// property under test is which inputs it reads.
func TestSignerLockPathFor_IgnoresTheEnvironment(t *testing.T) {
	if os.Getuid() < 0 {
		t.Skip("uid-based path is not used on this platform")
	}
	baseline, err := signerLockDir()
	if err != nil {
		t.Fatalf("signerLockDir: %v", err)
	}

	// Every variable any stdlib directory helper consults.
	for _, envVar := range []string{"HOME", "XDG_CACHE_HOME", "TMPDIR", "XDG_RUNTIME_DIR"} {
		t.Run(envVar, func(t *testing.T) {
			t.Setenv(envVar, t.TempDir())
			got, err := signerLockDir()
			if err != nil {
				t.Fatalf("signerLockDir with %s set: %v", envVar, err)
			}
			if got != baseline {
				t.Fatalf("%s moved the lock directory: %q vs %q. Two processes with different "+
					"%s take different lock files and both start, which is the account-sequence "+
					"race this lock exists to prevent", envVar, got, baseline, envVar)
			}
		})
	}

	// And the whole set at once, which is what a .env file actually does.
	t.Run("all of them at once, as .env would", func(t *testing.T) {
		for _, envVar := range []string{"HOME", "XDG_CACHE_HOME", "TMPDIR", "XDG_RUNTIME_DIR"} {
			t.Setenv(envVar, t.TempDir())
		}
		got, err := signerLockDir()
		if err != nil {
			t.Fatalf("signerLockDir: %v", err)
		}
		if got != baseline {
			t.Fatalf("the lock directory moved to %q, want %q", got, baseline)
		}
	})
}

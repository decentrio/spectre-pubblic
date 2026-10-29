package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// signerLockDirName identifies the shared per-user lock directory. It must not
// be derived from a relay's state or working directory: those are deliberately
// per-path values, so a lock located under either would let two processes take
// different files.
const signerLockDirName = "spectre-relayer"

// signerLockRoot is a literal on purpose: every stdlib helper that would name a
// directory for us reads an environment variable (TMPDIR for os.TempDir,
// XDG_CACHE_HOME/HOME for os.UserCacheDir), and an environment variable is
// exactly what must not decide this path.
const signerLockRoot = "/tmp"

// signerLockDir resolves the one directory every relay process of this user, on
// this machine, agrees on. It is a seam for lock tests, which must not write
// outside their temporary directory.
//
// It reads no environment variable. start loads .env before taking the lock;
// using os.UserCacheDir here would let processes with different HOME or
// XDG_CACHE_HOME values acquire different files and both start.
var signerLockDir = func() (string, error) {
	uid := os.Getuid()
	if uid < 0 {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("signer lock: no directory to place it in: %w", err)
		}
		return filepath.Join(cache, signerLockDirName), nil
	}
	return filepath.Join(signerLockRoot, fmt.Sprintf("%s-%d", signerLockDirName, uid)), nil
}

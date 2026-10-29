package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"relayer/transaction"
)

// A2 decided one Cosmos signing key per relay process. This is the half that
// enforces it; #433 is the half that wrote it down.
//
// The decision needs enforcing because nothing in the code can notice the
// violation. cosmosMu serializes the account sequence WITHIN a process, and
// there is no cross-process equivalent: two relayers signing with the same
// COSMOS_PRIVATE_KEY each believe they own the sequence, and the loser of every
// race gets "account sequence mismatch" — a message that names neither the other
// process nor the shared key, and which looks exactly like a transient RPC
// problem. A6 exists to turn that class of thing into a startup failure.
//
// The lock is advisory (flock) and per signing ADDRESS, not per key: the address
// is derived from the key but is public, so it can appear in a filename and in
// the error text. The key itself never leaves the transaction package.
//
// The address is the WHOLE key. Deliberately nothing else -- not the chain id,
// not the config path, not the state directory. A lock whose name depends on a
// value the operator is told to vary per process is not a lock: the two
// processes it exists to catch each take their own file and both start. That is
// the shape this guard shipped with, and it disabled the guard in exactly the
// documented multi-process setup (docs/E2E.md: "give each source ... its own
// state directory"), which is the only setup where it matters.
//
// The known cost of keying on the address alone: one key relaying two DIFFERENT
// Cosmos chains does not race (the sequence is per chain, per account) but is
// still refused. That is the decision A2 made and CLAUDE.md states -- one key
// per process -- and the error names the fix. Adding the chain id to narrow it
// would reintroduce the failure above the moment COSMOS_CHAIN_ID is unset or
// typo'd.
const signerLockPrefix = "cosmos-signer-"

// acquireCosmosSignerLock takes an exclusive, non-blocking lock naming this
// process's Cosmos signing address, and returns the release func.
//
// flock is per (machine, inode), so the guard reaches exactly as far as one
// machine and one user. Two relayers on two hosts sharing a key still race, and
// no local lock can see that; the error text says "on this machine" so nobody
// reads more into it than it gives.
//
// The lock is held for the life of the process. Releasing it early would let a
// second process in while the first is still signing, which is the whole thing
// being prevented.
func acquireCosmosSignerLock() (release func(), err error) {
	address, err := cosmosSignerAddressForLock()
	if err != nil {
		// Not this function's error to report: validateStartupKeys runs first and
		// says what is wrong with the key. Staying silent here avoids two
		// different messages about one problem.
		return func() {}, nil
	}

	dir, err := signerLockDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("signer lock: create %q: %w", dir, err)
	}
	path := filepath.Join(dir, signerLockPrefix+address+".lock")

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("signer lock: open %q: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf(
			"another relayer process on this machine is already signing Cosmos transactions with %s "+
				"(lock: %s). A2 decided one signing key per process: two processes sharing one key each "+
				"believe they own the account sequence, and the loser of every race sees "+
				"\"account sequence mismatch\", which names neither the other process nor the shared key. "+
				"Give this process its own COSMOS_PRIVATE_KEY, or stop the other one",
			address, path)
	}

	// The file is kept open on purpose: closing it drops the flock, so the
	// descriptor IS the lock.
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}

// signerLockPathFor is the lock file a given address would take. Exported to the
// test rather than recomputed there, so a change to the naming cannot leave the
// test asserting a path nothing uses.
func signerLockPathFor(address string) (string, error) {
	dir, err := signerLockDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, signerLockPrefix+address+".lock"), nil
}

// cosmosSignerAddressForLock is a seam for the test: it needs a deterministic
// address without a real key in the environment.
var cosmosSignerAddressForLock = func() (string, error) {
	address, err := (&transaction.Handler{}).CosmosSignerAddress()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(address), nil
}

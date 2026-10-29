// Package attestor defines the abstraction over "the component that makes sure
// a source chain's state is legit before anything is relayed against it". The
// Groth16 prover plays this role for Cosmos sources today (not yet behind this
// interface — moving it here is a deliberately deferred consolidation); the
// opstack subpackage implements it for OP Stack sources by replaying the L2
// state transition through a verify-mode replica.
package attestor

import (
	"context"
	"time"
)

// Cursor identifies the highest source state an attestor currently affirms.
// ID is an optional content identifier (e.g. an OP Stack output root); it is
// zero when the attestor has no content hash to bind.
type Cursor struct {
	Height uint64
	ID     [32]byte
}

// Attestor is the lifecycle-level interface every source attestor satisfies.
//
// Contract shared by all implementations:
//   - the attestor is the sole authority on whether source state is legit;
//   - ConfirmedUpTo never advances on a failed operation;
//   - a failed or lagging attestor means "affirm nothing", never "affirm".
type Attestor interface {
	// Name identifies the attestor in logs and metrics, e.g. "opstack:op-mainnet".
	Name() string
	// Run blocks until ctx is done, continuously attesting source state.
	Run(ctx context.Context) error
	// ConfirmedUpTo reports the highest fully-attested (non-provisional)
	// source state. ok is false when nothing has been confirmed yet.
	ConfirmedUpTo() (cursor Cursor, ok bool)
}

const (
	failureBackoffMin = time.Minute
	failureBackoffMax = 15 * time.Minute
)

// Backoff mirrors services.routineBackoff (min 1m doubling to max 15m, reset
// on success). It is an intentional local copy: exporting the services one
// would couple attestor to the relay-loop package for three tiny methods.
type Backoff struct {
	current     time.Duration
	nextAttempt time.Time
}

func (b *Backoff) Ready(now time.Time) bool {
	return b.nextAttempt.IsZero() || !now.Before(b.nextAttempt)
}

func (b *Backoff) RecordFailure(now time.Time) time.Duration {
	if b.current == 0 {
		b.current = failureBackoffMin
	} else {
		b.current *= 2
		if b.current > failureBackoffMax {
			b.current = failureBackoffMax
		}
	}
	b.nextAttempt = now.Add(b.current)
	return b.current
}

func (b *Backoff) RecordSuccess() {
	b.current = 0
	b.nextAttempt = time.Time{}
}

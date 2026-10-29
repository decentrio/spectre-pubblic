// Package prover is the relayer's side of the proof service: the vocabulary it submits
// work and reads outcomes in. The prover itself (circuits, engine, queue, store) lives in
// its own repository and process, spectre-prover, and is reached over gRPC by
// relayer/prover/grpcclient; nothing in this module imports gnark.
//
// The types here mirror what the service exposes (ProofJob, ProofResult, the failure
// classes and the sentinel errors) so the relay engine's retry table, in
// services/proofclient.go, reads the same whichever side of a process boundary the
// prover is on.
package prover

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"relayer/prover/witness"
)

// ProofID identifies one proof job. The prover derives it, deterministically, from the
// circuit version and the signatures: submitting the same signatures again returns the
// same id and never proves twice, and a re-submit after a prover restart reproduces it.
type ProofID [32]byte

func (id ProofID) String() string { return hex.EncodeToString(id[:]) }

// Status is the lifecycle of a proof job:
//
//	queued → claimed → proving → done | failed
type Status string

const (
	StatusQueued  Status = "queued"
	StatusClaimed Status = "claimed"
	StatusProving Status = "proving"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// FailureClass tells the relayer what a failed job means for its retry policy — the
// prover itself never retries.
type FailureClass string

const (
	// FailureTransient covers timeouts and flaky backends: submitting the same
	// request again is reasonable.
	FailureTransient FailureClass = "transient"
	// FailureInternal covers circuit/artifact/assignment bugs: retrying is useless, a
	// human needs to look.
	FailureInternal FailureClass = "internal"
)

// ProofJob is what GetProof returns: the job's current status plus its result or
// failure, whichever applies.
type ProofJob struct {
	ID     ProofID
	Status Status
	Result *ProofResult // set when Status == done
	Class  FailureClass // set when Status == failed
	Cause  string       // set when Status == failed
}

// ProofResult is a finished Groth16 batch proof, shaped for the on-chain verifier
// dispatch: the bucket selects Groth16Verifier_N{bucket}, PaddedSigs carries the
// bucket-sized signer metadata that travels with the proof.
//
// Of each PaddedSigs slot only Index, PublicKey and Active are filled: they are what the
// on-chain call carries. The signature, the signed bytes and the voting power stay with
// the prover.
type ProofResult struct {
	Bucket        int
	PaddedSigs    []witness.ValidatorSignature
	Proof         [8]*big.Int
	Commitments   [2]*big.Int
	CommitmentPok [2]*big.Int
}

// ErrProofFailed is returned by WaitProof when a worker stored a terminal failure. The
// class remains machine-readable so one-shot callers can apply the same retry table as
// callers that poll GetProof directly.
type ErrProofFailed struct {
	ID    ProofID
	Class FailureClass
	Cause string
}

func (e *ErrProofFailed) Error() string {
	return fmt.Sprintf("proof %s failed (%s): %s", e.ID, e.Class, e.Cause)
}

// ErrInvalidInput reports a request the prover rejected at submit time. The input is
// wrong, not the system: submitting the same input again fails the same way.
type ErrInvalidInput struct {
	Reason string
	Slot   int // offending slot, -1 when the problem is not slot-specific
}

func (e *ErrInvalidInput) Error() string {
	if e.Slot >= 0 {
		return fmt.Sprintf("invalid input at slot %d: %s", e.Slot, e.Reason)
	}
	return "invalid input: " + e.Reason
}

// Port errors. The client returns these sentinel values (wrapped is fine) so the relay
// engine can classify an outcome without knowing the transport.
var (
	// ErrQueueFull: the prover's bounded queue rejected the job. The caller backs off;
	// nothing was silently dropped.
	ErrQueueFull = errors.New("proof queue full")
	// ErrNotFound: the prover has no entry for this id — never submitted,
	// TTL-collected, or lost to a restart. Re-submitting reproduces the id.
	ErrNotFound = errors.New("proof not found")
	// ErrStoreUnavailable: the prover cannot be reached, or its store cannot, right
	// now. The caller backs off and tries again.
	ErrStoreUnavailable = errors.New("proof store unavailable")
)

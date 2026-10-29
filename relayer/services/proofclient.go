package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"relayer/prover"
	"relayer/prover/witness"
)

// proofPollInterval paces GetProof polling while a prove (seconds to ~10s)
// runs; proofRetryBackoff spaces retries on ErrQueueFull/ErrStoreUnavailable.
const (
	proofPollInterval = 500 * time.Millisecond
	proofRetryBackoff = 2 * time.Second
)

// submitAndAwaitProof drives the async prover API from a synchronous relay
// step: submit, then poll GetProof on the relayer's cadence until the job
// finishes or ctx expires. Retry policy lives here — the prover never
// retries — and follows this error table:
//
//	ErrInvalidInput      → permanent: same input fails the same way
//	ErrQueueFull         → backoff and resubmit
//	ErrStoreUnavailable  → backoff and resubmit
//	failed{transient}    → resubmit (deterministic id, no duplicate prove)
//	failed{internal}     → permanent: circuit/artifact bug, a human must look
//	not_found            → resubmit (TTL collected it, or the prover restarted)
//
// A GetProof that cannot reach the prover (ErrStoreUnavailable) is not in the table
// because it is not an outcome: it keeps polling until ctx expires. The prover is a
// separate process now, so a restart or a dropped connection mid-poll is routine; the
// job id is deterministic and a restarted prover answers not_found, which resubmits.
func submitAndAwaitProof(ctx context.Context, p Prover, sigs []witness.ValidatorSignature) (prover.ProofResult, error) {
	return submitAndAwaitProofWithTiming(ctx, p, sigs, proofPollInterval, proofRetryBackoff)
}

func submitAndAwaitProofWithTiming(
	ctx context.Context,
	p Prover,
	sigs []witness.ValidatorSignature,
	pollInterval time.Duration,
	retryBackoff time.Duration,
) (prover.ProofResult, error) {
	id, err := submitProofWithBackoffTiming(ctx, p, sigs, retryBackoff)
	if err != nil {
		return prover.ProofResult{}, err
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	unreachable := false
	for {
		job, err := p.GetProof(id)
		if unreachable && !errors.Is(err, prover.ErrStoreUnavailable) {
			unreachable = false
			log.Printf("[prover] proof_id=%s prover reachable again", id)
		}
		switch {
		case errors.Is(err, prover.ErrStoreUnavailable):
			// Said once per outage, not once per poll.
			if !unreachable {
				unreachable = true
				log.Printf("WARN [prover] proof_id=%s unreachable while polling, retrying: %v", id, err)
			}
		case errors.Is(err, prover.ErrNotFound):
			log.Printf("WARN [prover] proof_id=%s not found (TTL or prover restart), resubmitting", id)
			if id, err = submitProofWithBackoffTiming(ctx, p, sigs, retryBackoff); err != nil {
				return prover.ProofResult{}, err
			}
		case err != nil:
			return prover.ProofResult{}, fmt.Errorf("get proof %s: %w", id, err)
		case job.Status == prover.StatusDone:
			return *job.Result, nil
		case job.Status == prover.StatusFailed && job.Class == prover.FailureTransient:
			log.Printf("WARN [prover] proof_id=%s failed transiently (%s), resubmitting", id, job.Cause)
			if id, err = submitProofWithBackoffTiming(ctx, p, sigs, retryBackoff); err != nil {
				return prover.ProofResult{}, err
			}
		case job.Status == prover.StatusFailed:
			return prover.ProofResult{}, fmt.Errorf("proof %s failed (%s): %s: %w",
				id, job.Class, job.Cause, ErrPermanentRelayFailure)
		}
		select {
		case <-ctx.Done():
			return prover.ProofResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// submitAndWaitProof is the one-shot CLI twin of submitAndAwaitProof. It uses
// Service.WaitProof instead of importing the relay loop's polling cadence, but
// applies the same retry table by inspecting ErrProofFailed.
func submitAndWaitProof(ctx context.Context, p Prover, sigs []witness.ValidatorSignature) (prover.ProofResult, error) {
	for {
		id, err := submitProofWithBackoff(ctx, p, sigs)
		if err != nil {
			return prover.ProofResult{}, err
		}
		result, err := p.WaitProof(ctx, id)
		if err == nil {
			return result, nil
		}
		var failed *prover.ErrProofFailed
		switch {
		case errors.Is(err, prover.ErrNotFound):
			log.Printf("WARN [prover] proof_id=%s not found (TTL or prover restart), resubmitting", id)
			continue
		case errors.As(err, &failed) && failed.Class == prover.FailureTransient:
			log.Printf("WARN [prover] proof_id=%s failed transiently (%s), resubmitting", id, failed.Cause)
			continue
		case errors.As(err, &failed):
			return prover.ProofResult{}, fmt.Errorf("%v: %w", failed, ErrPermanentRelayFailure)
		default:
			return prover.ProofResult{}, fmt.Errorf("wait proof %s: %w", id, err)
		}
	}
}

func submitProofWithBackoff(ctx context.Context, p Prover, sigs []witness.ValidatorSignature) (prover.ProofID, error) {
	return submitProofWithBackoffTiming(ctx, p, sigs, proofRetryBackoff)
}

func submitProofWithBackoffTiming(
	ctx context.Context,
	p Prover,
	sigs []witness.ValidatorSignature,
	retryBackoff time.Duration,
) (prover.ProofID, error) {
	for {
		id, err := p.SubmitProof(sigs)
		var invalid *prover.ErrInvalidInput
		switch {
		case err == nil:
			return id, nil
		case errors.As(err, &invalid):
			// Deterministic rejection — retrying the same input is useless.
			return prover.ProofID{}, fmt.Errorf("submit proof: %v: %w", err, ErrPermanentRelayFailure)
		case errors.Is(err, prover.ErrQueueFull), errors.Is(err, prover.ErrStoreUnavailable):
			log.Printf("WARN [prover] submit backing off: %v", err)
			select {
			case <-ctx.Done():
				return prover.ProofID{}, ctx.Err()
			case <-time.After(retryBackoff):
			}
		default:
			return prover.ProofID{}, fmt.Errorf("submit proof: %w", err)
		}
	}
}

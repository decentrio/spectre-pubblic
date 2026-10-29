package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"relayer/prover"
	"relayer/prover/witness"
)

type proofClientFake struct {
	submit func(int) (prover.ProofID, error)
	get    func(int, prover.ProofID) (prover.ProofJob, error)
	wait   func(context.Context, prover.ProofID) (prover.ProofResult, error)

	submitCalls int
	getCalls    int
	waitCalls   int
}

func (f *proofClientFake) SubmitProof([]witness.ValidatorSignature) (prover.ProofID, error) {
	f.submitCalls++
	return f.submit(f.submitCalls)
}

func (*proofClientFake) MaxBucket() int { return 4 }

func (f *proofClientFake) GetProof(id prover.ProofID) (prover.ProofJob, error) {
	f.getCalls++
	return f.get(f.getCalls, id)
}

func (f *proofClientFake) WaitProof(ctx context.Context, id prover.ProofID) (prover.ProofResult, error) {
	f.waitCalls++
	if f.wait == nil {
		return prover.ProofResult{}, errors.New("unexpected WaitProof call")
	}
	return f.wait(ctx, id)
}

func TestSubmitAndAwaitProof_RetryTable(t *testing.T) {
	id := prover.ProofID{1}
	done := prover.ProofResult{Bucket: 4}
	tests := []struct {
		name        string
		fake        *proofClientFake
		wantErr     error
		wantSubmits int
	}{
		{
			name: "invalid input is permanent",
			fake: &proofClientFake{
				submit: func(int) (prover.ProofID, error) {
					return prover.ProofID{}, &prover.ErrInvalidInput{Reason: "bad", Slot: -1}
				},
				get: func(int, prover.ProofID) (prover.ProofJob, error) { return prover.ProofJob{}, nil },
			},
			wantErr:     ErrPermanentRelayFailure,
			wantSubmits: 1,
		},
		{
			name: "queue full backs off and resubmits",
			fake: &proofClientFake{
				submit: func(call int) (prover.ProofID, error) {
					if call == 1 {
						return prover.ProofID{}, prover.ErrQueueFull
					}
					return id, nil
				},
				get: func(int, prover.ProofID) (prover.ProofJob, error) {
					return prover.ProofJob{ID: id, Status: prover.StatusDone, Result: &done}, nil
				},
			},
			wantSubmits: 2,
		},
		{
			name: "store unavailable backs off and resubmits",
			fake: &proofClientFake{
				submit: func(call int) (prover.ProofID, error) {
					if call == 1 {
						return prover.ProofID{}, prover.ErrStoreUnavailable
					}
					return id, nil
				},
				get: func(int, prover.ProofID) (prover.ProofJob, error) {
					return prover.ProofJob{ID: id, Status: prover.StatusDone, Result: &done}, nil
				},
			},
			wantSubmits: 2,
		},
		{
			name: "store unavailable while polling keeps polling without resubmitting",
			fake: &proofClientFake{
				submit: func(int) (prover.ProofID, error) { return id, nil },
				get: func(call int, _ prover.ProofID) (prover.ProofJob, error) {
					if call <= 2 {
						return prover.ProofJob{}, prover.ErrStoreUnavailable
					}
					return prover.ProofJob{ID: id, Status: prover.StatusDone, Result: &done}, nil
				},
			},
			wantSubmits: 1,
		},
		{
			name: "not found resubmits",
			fake: &proofClientFake{
				submit: func(int) (prover.ProofID, error) { return id, nil },
				get: func(call int, _ prover.ProofID) (prover.ProofJob, error) {
					if call == 1 {
						return prover.ProofJob{}, prover.ErrNotFound
					}
					return prover.ProofJob{ID: id, Status: prover.StatusDone, Result: &done}, nil
				},
			},
			wantSubmits: 2,
		},
		{
			name: "transient failure resubmits",
			fake: &proofClientFake{
				submit: func(int) (prover.ProofID, error) { return id, nil },
				get: func(call int, _ prover.ProofID) (prover.ProofJob, error) {
					if call == 1 {
						return prover.ProofJob{ID: id, Status: prover.StatusFailed, Class: prover.FailureTransient, Cause: "timeout"}, nil
					}
					return prover.ProofJob{ID: id, Status: prover.StatusDone, Result: &done}, nil
				},
			},
			wantSubmits: 2,
		},
		{
			name: "internal failure is permanent",
			fake: &proofClientFake{
				submit: func(int) (prover.ProofID, error) { return id, nil },
				get: func(int, prover.ProofID) (prover.ProofJob, error) {
					return prover.ProofJob{ID: id, Status: prover.StatusFailed, Class: prover.FailureInternal, Cause: "verify"}, nil
				},
			},
			wantErr:     ErrPermanentRelayFailure,
			wantSubmits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := submitAndAwaitProofWithTiming(ctx, tt.fake, nil, time.Millisecond, time.Millisecond)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil || result.Bucket != done.Bucket {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tt.fake.submitCalls != tt.wantSubmits {
				t.Fatalf("SubmitProof calls = %d, want %d", tt.fake.submitCalls, tt.wantSubmits)
			}
		})
	}
}

func TestNewOneShotWorker_UsesWaitProof(t *testing.T) {
	id := prover.ProofID{1}
	fake := &proofClientFake{
		submit: func(int) (prover.ProofID, error) { return id, nil },
		get: func(int, prover.ProofID) (prover.ProofJob, error) {
			return prover.ProofJob{}, errors.New("unexpected GetProof call")
		},
		wait: func(context.Context, prover.ProofID) (prover.ProofResult, error) {
			return prover.ProofResult{Bucket: 4}, nil
		},
	}
	worker := NewOneShotWorker(nil, fake)
	result, err := worker.generateProof(context.Background(), nil)
	if err != nil || result.Bucket != 4 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if fake.waitCalls != 1 || fake.getCalls != 0 {
		t.Fatalf("WaitProof/GetProof calls = %d/%d, want 1/0", fake.waitCalls, fake.getCalls)
	}
}

func TestSubmitAndWaitProof_TransientFailureResubmits(t *testing.T) {
	id := prover.ProofID{1}
	fake := &proofClientFake{
		submit: func(int) (prover.ProofID, error) { return id, nil },
		get: func(int, prover.ProofID) (prover.ProofJob, error) {
			return prover.ProofJob{}, errors.New("unexpected GetProof call")
		},
	}
	var waits int
	fake.wait = func(context.Context, prover.ProofID) (prover.ProofResult, error) {
		waits++
		if waits == 1 {
			return prover.ProofResult{}, &prover.ErrProofFailed{ID: id, Class: prover.FailureTransient, Cause: "timeout"}
		}
		return prover.ProofResult{Bucket: 4}, nil
	}
	result, err := submitAndWaitProof(context.Background(), fake, nil)
	if err != nil || result.Bucket != 4 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if fake.submitCalls != 2 || fake.waitCalls != 2 {
		t.Fatalf("SubmitProof/WaitProof calls = %d/%d, want 2/2", fake.submitCalls, fake.waitCalls)
	}
}

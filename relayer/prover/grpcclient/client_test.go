package grpcclient

import (
	"context"
	"errors"
	"log"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"relayer/prover"
	"relayer/prover/proverpb"
	"relayer/prover/witness"
)

// fakeProver is a scriptable ProverService.
type fakeProver struct {
	proverpb.UnimplementedProverServiceServer

	mu     sync.Mutex
	info   func(n int) (*proverpb.InfoResponse, error) // n is the 1-based call number
	submit func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error)
	get    func(n int, req *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error)

	infoCalls, getCalls int
	lastSubmit          *proverpb.SubmitProofRequest
}

func (f *fakeProver) Info(context.Context, *proverpb.InfoRequest) (*proverpb.InfoResponse, error) {
	f.mu.Lock()
	f.infoCalls++
	n := f.infoCalls
	f.mu.Unlock()
	return f.info(n)
}

func (f *fakeProver) SubmitProof(_ context.Context, req *proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
	f.mu.Lock()
	f.lastSubmit = req
	f.mu.Unlock()
	return f.submit(req)
}

func (f *fakeProver) GetProof(_ context.Context, req *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
	f.mu.Lock()
	f.getCalls++
	n := f.getCalls
	f.mu.Unlock()
	return f.get(n, req)
}

func infoWith(buckets ...uint32) func(int) (*proverpb.InfoResponse, error) {
	return func(int) (*proverpb.InfoResponse, error) {
		resp := &proverpb.InfoResponse{}
		for _, b := range buckets {
			resp.Buckets = append(resp.Buckets, &proverpb.BucketInfo{Bucket: b, VkSha256: make([]byte, 32)})
		}
		return resp, nil
	}
}

// connect serves f in memory and returns a connected, un-wrapped client connection.
func connect(t *testing.T, f *fakeProver) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	proverpb.RegisterProverServiceServer(gs, f)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///prover.test",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	return conn
}

// newClient builds a Client against f with a fast poll interval.
func newClient(t *testing.T, f *fakeProver) *Client {
	t.Helper()
	if f.info == nil {
		f.info = infoWith(4)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New(ctx, connect(t, f), WithPollInterval(2*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---- New / Info ------------------------------------------------------------

func TestNew_MaxBucketIsTheLargestReportedBucket(t *testing.T) {
	f := &fakeProver{info: infoWith(4, 16, 8)}
	if got := newClient(t, f).MaxBucket(); got != 16 {
		t.Fatalf("MaxBucket = %d, want 16", got)
	}
}

func TestNew_WaitsForAProverThatIsNotUpYet(t *testing.T) {
	f := &fakeProver{info: func(n int) (*proverpb.InfoResponse, error) {
		if n < 3 {
			return nil, status.Error(codes.Unavailable, "starting")
		}
		return infoWith(4)(n)
	}}
	c := newClient(t, f)
	if c.MaxBucket() != 4 {
		t.Fatalf("MaxBucket = %d, want 4", c.MaxBucket())
	}
	if f.infoCalls != 3 {
		t.Errorf("Info called %d times, want 3 (two refusals then success)", f.infoCalls)
	}
}

func TestNew_GivesUpWhenTheContextEndsAndNamesTheAddress(t *testing.T) {
	f := &fakeProver{info: func(int) (*proverpb.InfoResponse, error) { return nil, status.Error(codes.Unavailable, "down") }}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := New(ctx, connect(t, f))
	if err == nil {
		t.Fatal("New succeeded against a prover that never answers")
	}
	if !strings.Contains(err.Error(), "prover.test") || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("error %q should name the address and say the prover did not answer", err)
	}
}

func TestNew_AWrongPeerFailsAtOnceInsteadOfWaiting(t *testing.T) {
	f := &fakeProver{info: func(int) (*proverpb.InfoResponse, error) {
		return nil, status.Error(codes.Unimplemented, "not a prover")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := New(ctx, connect(t, f)); err == nil {
		t.Fatal("New succeeded against a peer without ProverService")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s: a non-retryable code must not be waited out", time.Since(start))
	}
	if f.infoCalls != 1 {
		t.Errorf("Info called %d times, want 1", f.infoCalls)
	}
}

func TestNew_NoBucketsIsAnError(t *testing.T) {
	f := &fakeProver{info: infoWith()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := New(ctx, connect(t, f)); err == nil || !strings.Contains(err.Error(), "no buckets") {
		t.Fatalf("err = %v, want a no-buckets error", err)
	}
}

// ---- SubmitProof -----------------------------------------------------------

func TestSubmitProof_SendsRealSignaturesAndReturnsTheID(t *testing.T) {
	want := prover.ProofID{9, 8, 7}
	f := &fakeProver{submit: func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
		return &proverpb.SubmitProofResponse{ProofId: want[:]}, nil
	}}
	c := newClient(t, f)

	id, err := c.SubmitProof([]witness.ValidatorSignature{
		{Signature: []byte{1}, PublicKey: []byte{2}, SignedBytes: []byte{3}, Index: 6, Power: 1000, Active: true},
		{Signature: []byte{4}, PublicKey: []byte{5}, SignedBytes: []byte{6}, Index: 11, Active: false},
	})
	if err != nil {
		t.Fatalf("SubmitProof: %v", err)
	}
	if id != want {
		t.Fatalf("id = %x, want %x", id, want)
	}
	got := f.lastSubmit.GetSignatures()
	if len(got) != 2 || got[0].Index != 6 || !got[0].Active || got[1].Index != 11 || got[1].Active ||
		got[0].Signature[0] != 1 || got[0].PublicKey[0] != 2 || got[0].SignedBytes[0] != 3 {
		t.Fatalf("server received %v", got)
	}
}

func TestSubmitProof_ErrorsAreTheRelayersOwnSentinels(t *testing.T) {
	cases := []struct {
		name  string
		code  codes.Code
		check func(error) bool
	}{
		{"invalid argument", codes.InvalidArgument, func(e error) bool {
			var inv *prover.ErrInvalidInput
			return errors.As(e, &inv)
		}},
		{"resource exhausted", codes.ResourceExhausted, func(e error) bool { return errors.Is(e, prover.ErrQueueFull) }},
		{"unavailable", codes.Unavailable, func(e error) bool { return errors.Is(e, prover.ErrStoreUnavailable) }},
		{"deadline exceeded", codes.DeadlineExceeded, func(e error) bool { return errors.Is(e, prover.ErrStoreUnavailable) }},
		{"anything else stays unclassified", codes.Internal, func(e error) bool {
			var inv *prover.ErrInvalidInput
			return !errors.As(e, &inv) && !errors.Is(e, prover.ErrQueueFull) &&
				!errors.Is(e, prover.ErrStoreUnavailable) && !errors.Is(e, prover.ErrNotFound)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProver{submit: func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
				return nil, status.Error(tc.code, "boom")
			}}
			_, err := newClient(t, f).SubmitProof(nil)
			if err == nil || !tc.check(err) {
				t.Fatalf("err = %v does not have the expected classification", err)
			}
		})
	}
}

func TestSubmitProof_InvalidInputRecoversTheSlotAndReason(t *testing.T) {
	f := &fakeProver{submit: func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "invalid input at slot 3: signature does not verify")
	}}
	_, err := newClient(t, f).SubmitProof(nil)
	var inv *prover.ErrInvalidInput
	if !errors.As(err, &inv) || inv.Slot != 3 || inv.Reason != "signature does not verify" {
		t.Fatalf("err = %v, want ErrInvalidInput{Slot: 3, Reason: signature does not verify}", err)
	}

	f.submit = func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "invalid input: no signatures provided")
	}
	_, err = newClient(t, f).SubmitProof(nil)
	if !errors.As(err, &inv) || inv.Slot != -1 || !strings.Contains(inv.Reason, "no signatures provided") {
		t.Fatalf("err = %v, want a slot-less ErrInvalidInput", err)
	}
}

func TestSubmitProof_ANegativeIndexIsRefusedBeforeItCanWrapAround(t *testing.T) {
	f := &fakeProver{submit: func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
		t.Fatal("the request must not reach the prover")
		return nil, nil
	}}
	_, err := newClient(t, f).SubmitProof([]witness.ValidatorSignature{{Index: 1}, {Index: -1}})
	var inv *prover.ErrInvalidInput
	if !errors.As(err, &inv) || inv.Slot != 1 {
		t.Fatalf("err = %v, want ErrInvalidInput at slot 1", err)
	}
}

func TestSubmitProof_ABadProofIDLengthIsAnError(t *testing.T) {
	f := &fakeProver{submit: func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
		return &proverpb.SubmitProofResponse{ProofId: []byte{1, 2, 3}}, nil
	}}
	if _, err := newClient(t, f).SubmitProof(nil); err == nil {
		t.Fatal("a 3-byte proof id was accepted")
	}
}

// ---- GetProof --------------------------------------------------------------

func be32(n int64) []byte { return new(big.Int).SetInt64(n).FillBytes(make([]byte, 32)) }

func goodResult(bucket int) *proverpb.ProofResult {
	r := &proverpb.ProofResult{Bucket: uint32(bucket)}
	for i := 0; i < bucket; i++ {
		pk := make([]byte, 32)
		pk[0] = byte(i + 1)
		r.PaddedSigners = append(r.PaddedSigners, &proverpb.PaddedSigner{Index: uint32(i), PublicKey: pk, Active: i < bucket-1})
	}
	for i := int64(0); i < 8; i++ {
		r.Proof = append(r.Proof, be32(100+i))
	}
	r.Commitments = [][]byte{be32(7), append([]byte{0x80}, make([]byte, 31)...)}
	r.CommitmentPok = [][]byte{be32(8), be32(9)}
	return r
}

func getReturning(resp *proverpb.GetProofResponse) func(int, *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
	return func(_ int, req *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
		out := proto.Clone(resp).(*proverpb.GetProofResponse)
		out.ProofId = req.ProofId
		return out, nil
	}
}

func TestGetProof_StatusesMapAndInFlightCarriesNoResult(t *testing.T) {
	for wire, want := range map[proverpb.JobStatus]prover.Status{
		proverpb.JobStatus_JOB_STATUS_QUEUED:  prover.StatusQueued,
		proverpb.JobStatus_JOB_STATUS_CLAIMED: prover.StatusClaimed,
		proverpb.JobStatus_JOB_STATUS_PROVING: prover.StatusProving,
	} {
		f := &fakeProver{get: getReturning(&proverpb.GetProofResponse{Status: wire})}
		job, err := newClient(t, f).GetProof(prover.ProofID{1})
		if err != nil || job.Status != want || job.Result != nil {
			t.Errorf("%v: job=%+v err=%v, want status %s and no result", wire, job, err, want)
		}
	}
}

func TestGetProof_DoneDecodesTheFullProof(t *testing.T) {
	f := &fakeProver{get: getReturning(&proverpb.GetProofResponse{Status: proverpb.JobStatus_JOB_STATUS_DONE, Result: goodResult(4)})}
	job, err := newClient(t, f).GetProof(prover.ProofID{1})
	if err != nil {
		t.Fatalf("GetProof: %v", err)
	}
	r := job.Result
	if job.Status != prover.StatusDone || r == nil || r.Bucket != 4 || len(r.PaddedSigs) != 4 {
		t.Fatalf("job = %+v", job)
	}
	if r.Proof[3].Int64() != 103 {
		t.Errorf("proof[3] = %v, want 103", r.Proof[3])
	}
	if want := new(big.Int).Lsh(big.NewInt(1), 255); r.Commitments[1].Cmp(want) != 0 {
		t.Errorf("commitments[1] = %v, want 2^255: all 32 bytes must be read", r.Commitments[1])
	}
	if r.CommitmentPok[1].Int64() != 9 {
		t.Errorf("commitment_pok[1] = %v, want 9", r.CommitmentPok[1])
	}
	if s := r.PaddedSigs[2]; s.Index != 2 || s.PublicKey[0] != 3 || !s.Active {
		t.Errorf("padded signer 2 = %+v", s)
	}
	if r.PaddedSigs[3].Active {
		t.Error("the last padded signer was marked inactive on the wire")
	}
}

func TestGetProof_NeverReturnsAMalformedProof(t *testing.T) {
	mutate := func(f func(r *proverpb.ProofResult)) *proverpb.GetProofResponse {
		r := goodResult(4)
		f(r)
		return &proverpb.GetProofResponse{Status: proverpb.JobStatus_JOB_STATUS_DONE, Result: r}
	}
	cases := map[string]*proverpb.GetProofResponse{
		"done without a result":  {Status: proverpb.JobStatus_JOB_STATUS_DONE},
		"zero bucket":            mutate(func(r *proverpb.ProofResult) { r.Bucket = 0 }),
		"too few padded signers": mutate(func(r *proverpb.ProofResult) { r.PaddedSigners = r.PaddedSigners[:3] }),
		"short public key":       mutate(func(r *proverpb.ProofResult) { r.PaddedSigners[1].PublicKey = []byte{1} }),
		"seven proof elements":   mutate(func(r *proverpb.ProofResult) { r.Proof = r.Proof[:7] }),
		"one commitment":         mutate(func(r *proverpb.ProofResult) { r.Commitments = r.Commitments[:1] }),
		"three pok elements":     mutate(func(r *proverpb.ProofResult) { r.CommitmentPok = append(r.CommitmentPok, be32(1)) }),
		"31-byte element":        mutate(func(r *proverpb.ProofResult) { r.Proof[5] = make([]byte, 31) }),
		"33-byte element":        mutate(func(r *proverpb.ProofResult) { r.CommitmentPok[0] = make([]byte, 33) }),
		"unspecified job status": {Status: proverpb.JobStatus_JOB_STATUS_UNSPECIFIED},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeProver{get: getReturning(resp)}
			job, err := newClient(t, f).GetProof(prover.ProofID{1})
			if err == nil {
				t.Fatalf("accepted: %+v", job)
			}
			if errors.Is(err, prover.ErrNotFound) || errors.Is(err, prover.ErrStoreUnavailable) {
				t.Fatalf("a protocol violation must not read as a retryable condition: %v", err)
			}
		})
	}
}

func TestGetProof_AnAnswerForAnotherProofIsRefused(t *testing.T) {
	f := &fakeProver{get: func(int, *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
		return &proverpb.GetProofResponse{ProofId: make([]byte, 32), Status: proverpb.JobStatus_JOB_STATUS_QUEUED}, nil
	}}
	if _, err := newClient(t, f).GetProof(prover.ProofID{1}); err == nil {
		t.Fatal("accepted a response for a different proof id")
	}
}

func TestGetProof_FailedIsAJobNotAnError(t *testing.T) {
	for _, tc := range []struct {
		wire proverpb.FailureClass
		want prover.FailureClass
	}{
		{proverpb.FailureClass_FAILURE_CLASS_TRANSIENT, prover.FailureTransient},
		{proverpb.FailureClass_FAILURE_CLASS_INTERNAL, prover.FailureInternal},
		// Unknown must read as the non-retryable class.
		{proverpb.FailureClass_FAILURE_CLASS_UNSPECIFIED, prover.FailureInternal},
		{proverpb.FailureClass(99), prover.FailureInternal},
	} {
		f := &fakeProver{get: getReturning(&proverpb.GetProofResponse{
			Status: proverpb.JobStatus_JOB_STATUS_FAILED, FailureClass: tc.wire, FailureCause: "deadline",
		})}
		job, err := newClient(t, f).GetProof(prover.ProofID{1})
		if err != nil || job.Status != prover.StatusFailed || job.Class != tc.want || job.Cause != "deadline" {
			t.Errorf("wire class %v: job=%+v err=%v, want failed/%s", tc.wire, job, err, tc.want)
		}
	}
}

func TestGetProof_StoreErrorsAreTheRelayersOwnSentinels(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		is   error
	}{
		{codes.NotFound, prover.ErrNotFound},
		{codes.Unavailable, prover.ErrStoreUnavailable},
		{codes.DeadlineExceeded, prover.ErrStoreUnavailable},
	} {
		f := &fakeProver{get: func(int, *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
			return nil, status.Error(tc.code, "x")
		}}
		if _, err := newClient(t, f).GetProof(prover.ProofID{1}); !errors.Is(err, tc.is) {
			t.Errorf("%v: err = %v, want errors.Is %v", tc.code, err, tc.is)
		}
	}
}

// ---- WaitProof -------------------------------------------------------------

func TestWaitProof_PollsThroughToDone(t *testing.T) {
	f := &fakeProver{get: func(n int, req *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
		resp := &proverpb.GetProofResponse{ProofId: req.ProofId, Status: proverpb.JobStatus_JOB_STATUS_QUEUED}
		switch {
		case n == 2:
			resp.Status = proverpb.JobStatus_JOB_STATUS_PROVING
		case n >= 3:
			resp.Status, resp.Result = proverpb.JobStatus_JOB_STATUS_DONE, goodResult(4)
		}
		return resp, nil
	}}
	res, err := newClient(t, f).WaitProof(context.Background(), prover.ProofID{1})
	if err != nil || res.Bucket != 4 {
		t.Fatalf("WaitProof = %+v, %v", res, err)
	}
	if f.getCalls != 3 {
		t.Errorf("GetProof called %d times, want 3", f.getCalls)
	}
}

func TestWaitProof_FailedCarriesItsClass(t *testing.T) {
	f := &fakeProver{get: getReturning(&proverpb.GetProofResponse{
		Status: proverpb.JobStatus_JOB_STATUS_FAILED, FailureClass: proverpb.FailureClass_FAILURE_CLASS_TRANSIENT, FailureCause: "timeout",
	})}
	_, err := newClient(t, f).WaitProof(context.Background(), prover.ProofID{1})
	var failed *prover.ErrProofFailed
	if !errors.As(err, &failed) || failed.Class != prover.FailureTransient || failed.Cause != "timeout" {
		t.Fatalf("err = %v, want ErrProofFailed{transient, timeout}", err)
	}
}

func TestWaitProof_NotFoundSurfacesSoTheCallerCanResubmit(t *testing.T) {
	f := &fakeProver{get: func(int, *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
		return nil, status.Error(codes.NotFound, "gone")
	}}
	if _, err := newClient(t, f).WaitProof(context.Background(), prover.ProofID{1}); !errors.Is(err, prover.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestWaitProof_WaitsOutAProverThatIsBrieflyUnreachable(t *testing.T) {
	f := &fakeProver{get: func(n int, req *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
		if n <= 2 {
			return nil, status.Error(codes.Unavailable, "restarting")
		}
		return &proverpb.GetProofResponse{ProofId: req.ProofId, Status: proverpb.JobStatus_JOB_STATUS_DONE, Result: goodResult(4)}, nil
	}}
	if _, err := newClient(t, f).WaitProof(context.Background(), prover.ProofID{1}); err != nil {
		t.Fatalf("WaitProof: %v", err)
	}
}

func TestWaitProof_ContextEndsThePoll(t *testing.T) {
	f := &fakeProver{get: getReturning(&proverpb.GetProofResponse{Status: proverpb.JobStatus_JOB_STATUS_PROVING})}
	c := newClient(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.WaitProof(ctx, prover.ProofID{1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestWaitProof_AnUnreachableProverDoesNotOutliveTheContext(t *testing.T) {
	f := &fakeProver{get: func(int, *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}}
	c := newClient(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.WaitProof(ctx, prover.ProofID{1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded", err)
	}
}

// syncBuffer is a log sink the test can read while the client writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWaitProof_AnOutageIsLoggedOncePerOutageNotOncePerPoll(t *testing.T) {
	var sink syncBuffer
	log.SetOutput(&sink)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	f := &fakeProver{get: func(n int, req *proverpb.GetProofRequest) (*proverpb.GetProofResponse, error) {
		if n <= 20 {
			return nil, status.Error(codes.Unavailable, "restarting")
		}
		return &proverpb.GetProofResponse{ProofId: req.ProofId, Status: proverpb.JobStatus_JOB_STATUS_DONE, Result: goodResult(4)}, nil
	}}
	if _, err := newClient(t, f).WaitProof(context.Background(), prover.ProofID{1}); err != nil {
		t.Fatalf("WaitProof: %v", err)
	}
	out := sink.String()
	if got := strings.Count(out, "unreachable while waiting"); got != 1 {
		t.Errorf("outage warning written %d times over 20 failed polls, want 1\n%s", got, out)
	}
	if !strings.Contains(out, "reachable again") {
		t.Errorf("recovery was not logged:\n%s", out)
	}
}

// ---- Close -----------------------------------------------------------------

func TestClose_StopsLaterCallsInsteadOfPanicking(t *testing.T) {
	var calls atomic.Int64
	f := &fakeProver{submit: func(*proverpb.SubmitProofRequest) (*proverpb.SubmitProofResponse, error) {
		calls.Add(1)
		return &proverpb.SubmitProofResponse{ProofId: make([]byte, 32)}, nil
	}}
	c := newClient(t, f)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.SubmitProof(nil); err == nil {
		t.Fatal("SubmitProof succeeded on a closed client")
	}
	if calls.Load() != 0 {
		t.Errorf("the closed client still reached the prover (%d calls)", calls.Load())
	}
}

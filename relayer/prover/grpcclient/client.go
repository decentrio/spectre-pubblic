// Package grpcclient binds the prover service's gRPC ProverService to the
// services.Prover interface the relay engine proves through. It does not import services:
// cmd/prover.go is where the client is checked against that interface, and keeping the
// package a leaf lets the services tests drive a real client. The prover lives in its
// own repository and process (spectre-prover); this is the relayer's only way to it.
//
// It is a thin transport layer. What a proof job is, and how a failure should be
// retried, is the relayer's own vocabulary (relayer/prover) and the retry table in
// services/proofclient.go; this package only translates the wire into it, so a prover
// outcome reads the same as it did when the prover ran in-process:
//
//	INVALID_ARGUMENT     prover.ErrInvalidInput        permanent
//	RESOURCE_EXHAUSTED   prover.ErrQueueFull           back off and resubmit
//	UNAVAILABLE and      prover.ErrStoreUnavailable    back off and resubmit
//	DEADLINE_EXCEEDED
//	NOT_FOUND            prover.ErrNotFound            resubmit
//	a FAILED job         prover.ProofJob{Status: failed, Class, Cause}, not an error
//
// A proof is never trusted until it is well formed: GetProof refuses to return a result
// whose element counts or widths are wrong, because the relayer would send it on-chain.
package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"relayer/prover"
	"relayer/prover/proverpb"
	"relayer/prover/witness"
)

const (
	proofIDLen      = 32
	fieldElementLen = 32
	pubKeyLen       = 32

	defaultCallTimeout  = 30 * time.Second
	defaultPollInterval = 200 * time.Millisecond

	// infoAttemptTimeout bounds one Info attempt while waiting for the prover to come up.
	infoAttemptTimeout = 3 * time.Second
	infoRetryBackoff   = 500 * time.Millisecond
	infoRetryBackoffHi = 2 * time.Second

	// reconnectMaxDelay caps gRPC's reconnect backoff. Its default ceiling is two
	// minutes, so a prover that was down for a while would stay unreachable that long
	// after it came back, with every poll failing fast against a channel that is
	// merely waiting out its backoff. A restarted prover needs tens of seconds to load
	// its keys anyway; what matters is not adding minutes on top of that.
	reconnectMaxDelay = 3 * time.Second
)

// Client implements services.Prover over a ProverService connection. It is safe for
// concurrent use; it starts no goroutines of its own.
type Client struct {
	conn   *grpc.ClientConn
	rpc    proverpb.ProverServiceClient
	target string

	// ctx is cancelled by Close, so calls that carry no context of their own (the
	// services.Prover methods) stop with the client.
	ctx    context.Context
	cancel context.CancelFunc

	callTimeout  time.Duration
	pollInterval time.Duration

	// maxBucket is set once by New and only read afterwards. If the prover is later
	// restarted with a different bucket set, a running relayer keeps this value until
	// it is restarted too.
	maxBucket int
}

// Option tunes a Client.
type Option func(*Client)

// WithCallTimeout bounds each SubmitProof and GetProof call. Default 30s.
func WithCallTimeout(d time.Duration) Option { return func(c *Client) { c.callTimeout = d } }

// WithPollInterval paces WaitProof's polling. Default 200ms.
func WithPollInterval(d time.Duration) Option { return func(c *Client) { c.pollInterval = d } }

// Dial connects to the prover at addr (host:port) and waits, until ctx is done, for it
// to answer Info. The transport is plaintext, like the attestor sidecar's: the prover is
// a co-located trusted process. The caller must Close the client.
func Dial(ctx context.Context, addr string, opts ...Option) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  500 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   reconnectMaxDelay,
			},
			MinConnectTimeout: 5 * time.Second,
		}))
	if err != nil {
		return nil, fmt.Errorf("prover: dial %s: %w", addr, err)
	}
	c, err := New(ctx, conn, opts...)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// New builds a Client on an existing connection and waits, until ctx is done, for the
// prover to answer Info. The client takes ownership of conn: Close closes it.
//
// Waiting is bounded retry on UNAVAILABLE and DEADLINE_EXCEEDED only (the prover is not
// up yet). Any other code means the peer is not a working prover, which no amount of
// waiting changes, so it fails at once.
func New(ctx context.Context, conn *grpc.ClientConn, opts ...Option) (*Client, error) {
	c := &Client{
		conn:         conn,
		rpc:          proverpb.NewProverServiceClient(conn),
		target:       conn.Target(),
		callTimeout:  defaultCallTimeout,
		pollInterval: defaultPollInterval,
	}
	for _, o := range opts {
		o(c)
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	info, err := c.waitForInfo(ctx)
	if err != nil {
		c.cancel()
		return nil, err
	}
	if len(info.GetBuckets()) == 0 {
		c.cancel()
		return nil, fmt.Errorf("prover at %s reports no buckets: it has no circuit artifacts loaded", c.target)
	}
	for _, b := range info.GetBuckets() {
		if int(b.GetBucket()) > c.maxBucket {
			c.maxBucket = int(b.GetBucket())
		}
		digest := b.GetVkSha256()
		if len(digest) >= 8 {
			log.Printf("[prover] connected addr=%s bucket=%d vk_digest=%x", c.target, b.GetBucket(), digest[:8])
		} else {
			log.Printf("[prover] connected addr=%s bucket=%d", c.target, b.GetBucket())
		}
	}
	return c, nil
}

func (c *Client) waitForInfo(ctx context.Context) (*proverpb.InfoResponse, error) {
	backoff := infoRetryBackoff
	for attempt := 1; ; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, infoAttemptTimeout)
		info, err := c.rpc.Info(callCtx, &proverpb.InfoRequest{})
		cancel()
		if err == nil {
			return info, nil
		}
		if code := status.Code(err); code != codes.Unavailable && code != codes.DeadlineExceeded {
			return nil, fmt.Errorf("prover at %s: Info: %w", c.target, err)
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("prover at %s did not answer (last error: %w); is it running?", c.target, err)
		}
		log.Printf("WARN [prover] %s not reachable yet (attempt %d): %v", c.target, attempt, err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("prover at %s did not answer (last error: %w); is it running?", c.target, err)
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > infoRetryBackoffHi {
			backoff = infoRetryBackoffHi
		}
	}
}

// Close releases the connection and stops calls that are still in flight.
func (c *Client) Close() error {
	c.cancel()
	return c.conn.Close()
}

// MaxBucket is the largest bucket the prover could prove when this client connected: the
// ceiling signature selection must respect.
func (c *Client) MaxBucket() int { return c.maxBucket }

// SubmitProof sends the real signatures; the prover pads them to a bucket, derives the
// deterministic proof id, and enqueues. A duplicate submit returns the same id.
func (c *Client) SubmitProof(sigs []witness.ValidatorSignature) (prover.ProofID, error) {
	req := &proverpb.SubmitProofRequest{Signatures: make([]*proverpb.ValidatorSignature, len(sigs))}
	for i, s := range sigs {
		// The wire index is unsigned. A value that does not fit would wrap into some
		// other validator's slot, so refuse it here as the input error it is.
		if s.Index < 0 || int64(s.Index) > math.MaxUint32 {
			return prover.ProofID{}, &prover.ErrInvalidInput{Reason: fmt.Sprintf("validator index %d out of range", s.Index), Slot: i}
		}
		req.Signatures[i] = &proverpb.ValidatorSignature{
			Signature:   s.Signature,
			PublicKey:   s.PublicKey,
			SignedBytes: s.SignedBytes,
			Index:       uint32(s.Index),
			Active:      s.Active,
		}
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.callTimeout)
	defer cancel()
	resp, err := c.rpc.SubmitProof(ctx, req)
	if err != nil {
		return prover.ProofID{}, fmt.Errorf("prover: SubmitProof: %w", mapStatus(err))
	}
	if len(resp.GetProofId()) != proofIDLen {
		return prover.ProofID{}, fmt.Errorf("prover: SubmitProof returned a %d-byte proof id, want %d", len(resp.GetProofId()), proofIDLen)
	}
	var id prover.ProofID
	copy(id[:], resp.GetProofId())
	return id, nil
}

// GetProof reads a job's state. A failed job is returned as a ProofJob, not as an error.
func (c *Client) GetProof(id prover.ProofID) (prover.ProofJob, error) {
	return c.getProof(c.ctx, id)
}

func (c *Client) getProof(parent context.Context, id prover.ProofID) (prover.ProofJob, error) {
	ctx, cancel := context.WithTimeout(parent, c.callTimeout)
	defer cancel()
	resp, err := c.rpc.GetProof(ctx, &proverpb.GetProofRequest{ProofId: id[:]})
	if err != nil {
		return prover.ProofJob{}, fmt.Errorf("prover: GetProof %s: %w", id, mapStatus(err))
	}
	if string(resp.GetProofId()) != string(id[:]) {
		return prover.ProofJob{}, fmt.Errorf("prover: GetProof %s answered for proof id %x", id, resp.GetProofId())
	}
	job := prover.ProofJob{ID: id}
	switch resp.GetStatus() {
	case proverpb.JobStatus_JOB_STATUS_QUEUED:
		job.Status = prover.StatusQueued
	case proverpb.JobStatus_JOB_STATUS_CLAIMED:
		job.Status = prover.StatusClaimed
	case proverpb.JobStatus_JOB_STATUS_PROVING:
		job.Status = prover.StatusProving
	case proverpb.JobStatus_JOB_STATUS_DONE:
		result, err := resultFromWire(resp.GetResult())
		if err != nil {
			return prover.ProofJob{}, fmt.Errorf("prover: GetProof %s: malformed proof: %w", id, err)
		}
		job.Status = prover.StatusDone
		job.Result = &result
	case proverpb.JobStatus_JOB_STATUS_FAILED:
		job.Status = prover.StatusFailed
		job.Cause = resp.GetFailureCause()
		switch resp.GetFailureClass() {
		case proverpb.FailureClass_FAILURE_CLASS_TRANSIENT:
			job.Class = prover.FailureTransient
		default:
			// Internal, and anything the relayer does not recognise: never read an
			// unknown class as transient, or a job that cannot succeed is resubmitted
			// forever.
			job.Class = prover.FailureInternal
		}
	default:
		return prover.ProofJob{}, fmt.Errorf("prover: GetProof %s returned unknown job status %v", id, resp.GetStatus())
	}
	return job, nil
}

// WaitProof polls GetProof until the job finishes or ctx is done. The job keeps running
// when it gives up. A prover that is briefly unreachable is waited out, since a restart
// ends in NOT_FOUND, which the caller answers by resubmitting.
func (c *Client) WaitProof(ctx context.Context, id prover.ProofID) (prover.ProofResult, error) {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	unreachable := false
	for {
		job, err := c.getProof(ctx, id)
		if err != nil && ctx.Err() != nil {
			// The caller's context ended, possibly mid-RPC: that is the answer, not
			// "prover unavailable".
			return prover.ProofResult{}, ctx.Err()
		}
		switch {
		case errors.Is(err, prover.ErrStoreUnavailable):
			// Said once per outage, not once per poll: at the poll interval an outage
			// would otherwise write several lines a second.
			if !unreachable {
				unreachable = true
				log.Printf("WARN [prover] proof_id=%s prover unreachable while waiting, retrying: %v", id, err)
			}
		case err != nil:
			return prover.ProofResult{}, err
		default:
			if unreachable {
				unreachable = false
				log.Printf("[prover] proof_id=%s prover reachable again", id)
			}
			switch job.Status {
			case prover.StatusDone:
				return *job.Result, nil
			case prover.StatusFailed:
				return prover.ProofResult{}, &prover.ErrProofFailed{ID: id, Class: job.Class, Cause: job.Cause}
			}
		}
		select {
		case <-ctx.Done():
			return prover.ProofResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// invalidInputMsg recovers the slot from the prover's "invalid input at slot N: reason".
var invalidInputMsg = regexp.MustCompile(`^invalid input at slot (\d+): (.*)$`)

// mapStatus translates transport codes into the relayer's prover error taxonomy. A code
// with no meaning here keeps its status error, unclassified, so the caller treats it as
// an unexpected failure rather than a known retry case.
func mapStatus(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		// Not a gRPC status: the context ended or the transport broke under us.
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", prover.ErrStoreUnavailable, err)
		}
		return err
	}
	switch st.Code() {
	case codes.InvalidArgument:
		invalid := &prover.ErrInvalidInput{Reason: st.Message(), Slot: -1}
		if m := invalidInputMsg.FindStringSubmatch(st.Message()); m != nil {
			if slot, convErr := strconv.Atoi(m[1]); convErr == nil {
				invalid.Slot, invalid.Reason = slot, m[2]
			}
		}
		return invalid
	case codes.ResourceExhausted:
		return fmt.Errorf("%w: %s", prover.ErrQueueFull, st.Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s", prover.ErrStoreUnavailable, st.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s", prover.ErrNotFound, st.Message())
	default:
		return err
	}
}

// resultFromWire decodes a finished proof, refusing anything the verifier contract could
// not take: a wrong element count, a wrong width, or a padded-signer list that does not
// match the bucket.
func resultFromWire(r *proverpb.ProofResult) (prover.ProofResult, error) {
	if r == nil {
		return prover.ProofResult{}, errors.New("job is done but carries no result")
	}
	bucket := int(r.GetBucket())
	if bucket <= 0 {
		return prover.ProofResult{}, fmt.Errorf("bucket %d", r.GetBucket())
	}
	if got := len(r.GetPaddedSigners()); got != bucket {
		return prover.ProofResult{}, fmt.Errorf("%d padded signers for bucket %d", got, bucket)
	}
	out := prover.ProofResult{Bucket: bucket, PaddedSigs: make([]witness.ValidatorSignature, bucket)}
	for i, s := range r.GetPaddedSigners() {
		if len(s.GetPublicKey()) != pubKeyLen {
			return prover.ProofResult{}, fmt.Errorf("padded signer %d has a %d-byte public key, want %d", i, len(s.GetPublicKey()), pubKeyLen)
		}
		out.PaddedSigs[i] = witness.ValidatorSignature{
			Index:     int(s.GetIndex()),
			PublicKey: append([]byte(nil), s.GetPublicKey()...),
			Active:    s.GetActive(),
		}
	}
	if err := elementsFromWire("proof", r.GetProof(), out.Proof[:]); err != nil {
		return prover.ProofResult{}, err
	}
	if err := elementsFromWire("commitments", r.GetCommitments(), out.Commitments[:]); err != nil {
		return prover.ProofResult{}, err
	}
	if err := elementsFromWire("commitment_pok", r.GetCommitmentPok(), out.CommitmentPok[:]); err != nil {
		return prover.ProofResult{}, err
	}
	return out, nil
}

func elementsFromWire(name string, in [][]byte, out []*big.Int) error {
	if len(in) != len(out) {
		return fmt.Errorf("%s has %d elements, want %d", name, len(in), len(out))
	}
	for i, b := range in {
		if len(b) != fieldElementLen {
			return fmt.Errorf("%s[%d] is %d bytes, want %d", name, i, len(b), fieldElementLen)
		}
		out[i] = new(big.Int).SetBytes(b)
	}
	return nil
}

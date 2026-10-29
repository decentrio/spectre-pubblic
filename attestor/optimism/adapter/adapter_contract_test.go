package adapter

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"attestor/core"
	"attestor/optimism/opstack"
	"attestor/types/attestation"

	"github.com/ethereum/go-ethereum/common"
)

type contractReplica struct {
	status        opstack.SyncStatus
	commitment    opstack.L2Commitment
	statusErr     error
	commitmentErr error
}

func (r contractReplica) SyncStatus(context.Context) (opstack.SyncStatus, error) {
	return r.status, r.statusErr
}

func (r contractReplica) OutputAtBlock(context.Context, uint64) ([32]byte, error) {
	return r.commitment.OutputRoot, r.commitmentErr
}

func (r contractReplica) CommitmentAt(context.Context, uint64) (opstack.L2Commitment, error) {
	return r.commitment, r.commitmentErr
}

func newContractAdapter(t *testing.T, replica contractReplica, signer attestation.Signer) *Adapter {
	t.Helper()
	store, err := opstack.LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	attestor := opstack.New(
		opstack.Config{SrcChain: "op-contract", AttestationHead: opstack.HeadFinalized},
		nil,
		replica,
		store,
		nil,
		nil,
		nil,
	)
	bridge, err := New(attestor, signer, Options{WatchPoll: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return bridge
}

func TestCorePortContract(t *testing.T) {
	commitment := opstack.L2Commitment{
		BlockNumber: 7,
		BlockHash:   common.HexToHash("0x7"),
		StateRoot:   common.HexToHash("0x70"),
	}
	signer, err := attestation.NewSigner(10, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	bridge := newContractAdapter(t, contractReplica{
		status:     opstack.SyncStatus{UnsafeL2: 10, SafeL2: 9, FinalizedL2: 8},
		commitment: commitment,
	}, signer)

	if _, found, err := bridge.AttestedUpTo(context.Background(), core.AttestationPolicy{}); err != nil || found {
		t.Fatalf("empty durable feed = (_, %t, %v), want not found", found, err)
	}
	if _, found, err := bridge.AttestedRootAtOrBelow(context.Background(), 7, core.AttestationPolicy{IncludeProvisional: true}); err != nil || found {
		t.Fatalf("empty bounded durable feed = (_, %t, %v), want not found", found, err)
	}
	initialStatus, err := bridge.Status(context.Background())
	if err != nil || initialStatus.Ready || initialStatus.SrcChain != "op-contract" {
		t.Fatalf("initial status = (%+v, %v), want unready local snapshot", initialStatus, err)
	}

	var stateRoot [32]byte
	copy(stateRoot[:], commitment.StateRoot[:])
	var blockHash [32]byte
	copy(blockHash[:], commitment.BlockHash[:])
	request := core.BlockIdentityRequest{
		BlockNumber:       commitment.BlockNumber,
		ExpectedStateRoot: stateRoot,
		ExpectedBlockHash: &blockHash,
		RunMode:           core.RunModeFinalized,
	}
	request.L2Router[19] = 1
	request.AttestorSetHash[31] = 2
	verdict, err := bridge.VerifyStateRoot(context.Background(), request)
	if err != nil || !verdict.Valid || len(verdict.Signature) != ed25519.SignatureSize {
		t.Fatalf("matching verdict = (%+v, %v), want signed valid", verdict, err)
	}
	message, err := attestation.SigningBytes(10, request.L2Router[:], request.AttestorSetHash[:], commitment.BlockNumber, commitment.BlockHash[:], commitment.StateRoot[:])
	if err != nil {
		t.Fatalf("SigningBytes: %v", err)
	}
	if !ed25519.Verify(signer.PublicKey(), message, verdict.Signature) {
		t.Fatal("positive verdict signature does not bind canonical identity")
	}
	for _, mismatched := range []core.RunMode{core.RunModeUnsafe, core.RunModeSafe} {
		request.RunMode = mismatched
		if _, err := bridge.VerifyStateRoot(context.Background(), request); core.ErrorKindOf(err) != core.ErrorFailedPrecondition {
			t.Fatalf("mismatched run mode %s error kind = %v, want FailedPrecondition (err %v)", mismatched, core.ErrorKindOf(err), err)
		}
	}

	request.RunMode = core.RunModeFinalized
	request.ExpectedStateRoot = [32]byte{1}
	verdict, err = bridge.VerifyStateRoot(context.Background(), request)
	if err != nil || verdict.Valid || len(verdict.Signature) != 0 {
		t.Fatalf("mismatch verdict = (%+v, %v), want unsigned valid=false", verdict, err)
	}
	request.RunMode = core.RunMode("unknown")
	if _, err := bridge.VerifyStateRoot(context.Background(), request); core.ErrorKindOf(err) != core.ErrorInvalidArgument {
		t.Fatalf("invalid run mode error kind = %v, want InvalidArgument (err %v)", core.ErrorKindOf(err), err)
	}
	request.RunMode = core.RunModeUnsafe
	request.ExpectedStateRoot = stateRoot
	if _, err := bridge.VerifyStateRoot(context.Background(), request); core.ErrorKindOf(err) != core.ErrorFailedPrecondition {
		t.Fatalf("mismatched configured-head error kind = %v, want FailedPrecondition (err %v)", core.ErrorKindOf(err), err)
	}
	request.RunMode = core.RunModeFinalized
	request.BlockNumber = 99
	if _, err := bridge.VerifyStateRoot(context.Background(), request); core.ErrorKindOf(err) != core.ErrorFailedPrecondition {
		t.Fatalf("above-head error kind = %v, want FailedPrecondition (err %v)", core.ErrorKindOf(err), err)
	}

	// A missing signer is a CONFIGURATION error and never clears on its own; the
	// above-head case a few lines up is the one that does. They shared a kind
	// until now, which made the consumer retry a misconfigured daemon forever.
	unsigned := newContractAdapter(t, contractReplica{status: opstack.SyncStatus{FinalizedL2: 8}, commitment: commitment}, attestation.Signer{})
	request.BlockNumber = commitment.BlockNumber
	request.ExpectedStateRoot = stateRoot
	if _, err := unsigned.VerifyStateRoot(context.Background(), request); core.ErrorKindOf(err) != core.ErrorUnimplemented {
		t.Fatalf("unsigned positive error kind = %v, want Unimplemented (err %v)", core.ErrorKindOf(err), err)
	}

	unavailable := newContractAdapter(t, contractReplica{statusErr: errors.New("op-node unavailable")}, signer)
	if _, err := unavailable.VerifyStateRoot(context.Background(), request); core.ErrorKindOf(err) != core.ErrorUnavailable {
		t.Fatalf("replica error kind = %v, want Unavailable (err %v)", core.ErrorKindOf(err), err)
	}
}

func TestCorePortWatchCancellationDoesNotWrite(t *testing.T) {
	bridge := newContractAdapter(t, contractReplica{}, attestation.Signer{})
	ctx, cancel := context.WithCancel(context.Background())
	updates, err := bridge.WatchFrontier(ctx, core.WatchRequest{})
	if err != nil {
		t.Fatalf("WatchFrontier: %v", err)
	}
	cancel()
	select {
	case _, open := <-updates:
		if open {
			t.Fatal("watch emitted an update from an empty feed after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not stop after cancellation")
	}
}

func TestNewRejectsInvalidComposition(t *testing.T) {
	if _, err := New(nil, attestation.Signer{}, Options{}); err == nil {
		t.Fatal("New accepted nil OP attestor")
	}
	store, err := opstack.LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	emptyChain := opstack.New(opstack.Config{}, nil, contractReplica{}, store, nil, nil, nil)
	if _, err := New(emptyChain, attestation.Signer{}, Options{}); err == nil {
		t.Fatal("New accepted empty src_chain")
	}
}

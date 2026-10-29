package arbitrum

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

func TestDerivedRootAttestorPublishesConfiguredUnsafeHead(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	runtime := &derivedTestRuntime{
		snapshot: RuntimeSnapshot{
			Unsafe:         testStoreCommitment(120),
			UnsafeObserved: true,
			Safe:           testStoreCommitment(110),
			SafeObserved:   true,
			Finalized:      testStoreCommitment(100),
			FinalizedSeen:  true,
		},
		commitments: map[uint64]BlockCommitment{},
	}
	attestor, err := NewDerivedRootAttestor(runtime, store, DerivedAttestorConfig{
		AttestationHead: RunModeUnsafe,
		GapBlocks:       10,
		MaxRoots:        10,
	})
	if err != nil {
		t.Fatalf("create derived attestor: %v", err)
	}
	attestor.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	if err := attestor.SyncOnce(context.Background()); err != nil {
		t.Fatalf("attest unsafe head: %v", err)
	}
	root, found := store.HighestAttested(true)
	if !found || root.Source != SourceDerived || root.L2BlockNumber != 120 || !root.Provisional {
		t.Fatalf("unsafe derived frontier: found=%t root=%+v", found, root)
	}
	if _, found := store.HighestAttested(false); found {
		t.Fatal("unsafe derived root entered confirmed frontier before finalization")
	}
}

func TestDerivedRootAttestorFinalizedRecheckCorrectsDivergence(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	provisional := testStoreCommitment(100)
	if err := store.AppendDerived(provisional, time.Unix(1, 0), true); err != nil {
		t.Fatalf("append provisional root: %v", err)
	}
	finalized := provisional
	finalized.BlockHash = common.HexToHash("0xf1")
	finalized.StateRoot = common.HexToHash("0xf2")
	runtime := &derivedTestRuntime{
		snapshot: RuntimeSnapshot{
			Unsafe:         testStoreCommitment(120),
			UnsafeObserved: true,
			Safe:           testStoreCommitment(110),
			SafeObserved:   true,
			Finalized:      finalized,
			FinalizedSeen:  true,
		},
		commitments: map[uint64]BlockCommitment{100: finalized},
	}
	attestor, err := NewDerivedRootAttestor(runtime, store, DerivedAttestorConfig{
		AttestationHead: RunModeUnsafe,
		Disabled:        true,
		GapBlocks:       10,
		MaxRoots:        10,
	})
	if err != nil {
		t.Fatalf("create derived attestor: %v", err)
	}
	attestor.now = func() time.Time { return time.Unix(2, 0) }

	if err := attestor.SyncOnce(context.Background()); err != nil {
		t.Fatalf("recheck derived root: %v", err)
	}
	root, found := store.HighestAttested(false)
	if !found || root.Provisional || root.Root != finalized.StateRoot || root.L2BlockHash != finalized.BlockHash {
		t.Fatalf("finalized corrected root: found=%t root=%+v", found, root)
	}
}

func TestDerivedRootAttestorReportsWrongHeightDivergence(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	provisional := testStoreCommitment(100)
	if err := store.AppendDerived(provisional, time.Unix(1, 0), true); err != nil {
		t.Fatalf("append provisional root: %v", err)
	}
	wrongHeight := testStoreCommitment(101)
	wrongHeight.BlockHash = common.HexToHash("0xf1")
	wrongHeight.StateRoot = common.HexToHash("0xf2")
	runtime := &derivedTestRuntime{
		snapshot: RuntimeSnapshot{
			Finalized:      wrongHeight,
			FinalizedSeen:  true,
			Unsafe:         wrongHeight,
			UnsafeObserved: true,
		},
		commitments: map[uint64]BlockCommitment{100: wrongHeight},
	}
	attestor, err := NewDerivedRootAttestor(runtime, store, DerivedAttestorConfig{
		AttestationHead: RunModeUnsafe,
		Disabled:        true,
		GapBlocks:       1,
		MaxRoots:        10,
	})
	if err != nil {
		t.Fatalf("create derived attestor: %v", err)
	}

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	err = attestor.SyncOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "returned commitment for L2 block 101") {
		t.Fatalf("SyncOnce error = %v, want wrong-height recheck failure", err)
	}
	if !strings.Contains(logs.String(), "Arbitrum HEAD DIVERGENCE") {
		t.Fatalf("wrong-height divergence was not logged: %q", logs.String())
	}
	root, found := store.DerivedAt(100)
	if !found || !root.Provisional || root.Root != provisional.StateRoot {
		t.Fatalf("wrong-height response mutated provisional root: found=%t root=%+v", found, root)
	}
}

func TestDerivedRootAttestorTracksUnsafeReorgBeforeFinalization(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	oldHead := testStoreCommitment(120)
	if err := store.AppendDerived(oldHead, time.Unix(1, 0), true); err != nil {
		t.Fatalf("append old head: %v", err)
	}
	newHead := oldHead
	newHead.BlockHash = common.HexToHash("0xa1")
	newHead.StateRoot = common.HexToHash("0xa2")
	runtime := &derivedTestRuntime{snapshot: RuntimeSnapshot{
		Unsafe:         newHead,
		UnsafeObserved: true,
		Finalized:      testStoreCommitment(100),
		FinalizedSeen:  true,
	}}
	attestor, err := NewDerivedRootAttestor(runtime, store, DerivedAttestorConfig{
		AttestationHead: RunModeUnsafe,
		GapBlocks:       10,
		MaxRoots:        10,
	})
	if err != nil {
		t.Fatalf("create derived attestor: %v", err)
	}
	if err := attestor.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync reorged unsafe head: %v", err)
	}
	root, found := store.HighestAttested(true)
	if !found || root.Root != newHead.StateRoot || root.L2BlockHash != newHead.BlockHash || !root.Provisional {
		t.Fatalf("reorged unsafe root: found=%t root=%+v", found, root)
	}
}

func TestDerivedRootAttestorDropsUnavailableEntryAndPersistsRemainingWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attested-roots.json")
	store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	for _, height := range []uint64{90, 100} {
		if err := store.AppendDerived(testStoreCommitment(height), time.Unix(1, 0), true); err != nil {
			t.Fatalf("append provisional root at %d: %v", height, err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatalf("save initial store: %v", err)
	}

	runtime := &derivedTestRuntime{
		snapshot: RuntimeSnapshot{
			Unsafe:         testStoreCommitment(120),
			UnsafeObserved: true,
			Finalized:      testStoreCommitment(100),
			FinalizedSeen:  true,
		},
		commitments: map[uint64]BlockCommitment{100: testStoreCommitment(100)},
		failures:    map[uint64]error{90: ethereum.NotFound},
	}
	attestor, err := NewDerivedRootAttestor(runtime, store, DerivedAttestorConfig{
		AttestationHead: RunModeUnsafe,
		GapBlocks:       10,
		MaxRoots:        10,
	})
	if err != nil {
		t.Fatalf("create derived attestor: %v", err)
	}
	if err := attestor.SyncOnce(context.Background()); err != nil {
		t.Fatalf("sync after unavailable historical block: %v", err)
	}

	reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if _, found := reloaded.DerivedAt(90); found {
		t.Fatal("unavailable provisional root was retained")
	}
	if root, found := reloaded.DerivedAt(100); !found || root.Provisional {
		t.Fatalf("later finalized root was not confirmed: found=%t root=%+v", found, root)
	}
	if root, found := reloaded.DerivedAt(120); !found || !root.Provisional {
		t.Fatalf("new unsafe root was not persisted: found=%t root=%+v", found, root)
	}
}

func TestDerivedRootAttestorPersistsProgressAcrossTransientRecheckFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attested-roots.json")
	store, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	for _, height := range []uint64{90, 100} {
		if err := store.AppendDerived(testStoreCommitment(height), time.Unix(1, 0), true); err != nil {
			t.Fatalf("append provisional root at %d: %v", height, err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatalf("save initial store: %v", err)
	}

	runtime := &derivedTestRuntime{
		snapshot: RuntimeSnapshot{
			Unsafe:         testStoreCommitment(120),
			UnsafeObserved: true,
			Finalized:      testStoreCommitment(100),
			FinalizedSeen:  true,
		},
		commitments: map[uint64]BlockCommitment{100: testStoreCommitment(100)},
		failures:    map[uint64]error{90: errors.New("temporary Nitro RPC failure")},
	}
	attestor, err := NewDerivedRootAttestor(runtime, store, DerivedAttestorConfig{
		AttestationHead: RunModeUnsafe,
		GapBlocks:       10,
		MaxRoots:        10,
	})
	if err != nil {
		t.Fatalf("create derived attestor: %v", err)
	}
	if err := attestor.SyncOnce(context.Background()); err == nil {
		t.Fatal("transient recheck failure was not reported")
	}

	reloaded, err := LoadAttestedRootStore(path, "arbitrum-one", 1)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if root, found := reloaded.DerivedAt(90); !found || !root.Provisional {
		t.Fatalf("transiently unavailable root was not retained: found=%t root=%+v", found, root)
	}
	if root, found := reloaded.DerivedAt(100); !found || root.Provisional {
		t.Fatalf("later finalized root was not persisted: found=%t root=%+v", found, root)
	}
	if root, found := reloaded.DerivedAt(120); !found || !root.Provisional {
		t.Fatalf("new unsafe root was not persisted: found=%t root=%+v", found, root)
	}
}

func TestNewDerivedRootAttestorFailureMatrix(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	runtime := &derivedTestRuntime{}
	for _, tc := range []struct {
		name string
		make func() (DerivedRuntime, *AttestedRootStore, DerivedAttestorConfig)
		want string
	}{
		{
			name: "nil runtime",
			make: func() (DerivedRuntime, *AttestedRootStore, DerivedAttestorConfig) {
				return nil, store, DerivedAttestorConfig{GapBlocks: 1, MaxRoots: 1}
			},
			want: "runtime",
		},
		{
			name: "nil store",
			make: func() (DerivedRuntime, *AttestedRootStore, DerivedAttestorConfig) {
				return runtime, nil, DerivedAttestorConfig{GapBlocks: 1, MaxRoots: 1}
			},
			want: "store",
		},
		{
			name: "invalid attestation head",
			make: func() (DerivedRuntime, *AttestedRootStore, DerivedAttestorConfig) {
				return runtime, store, DerivedAttestorConfig{AttestationHead: "invalid", GapBlocks: 1, MaxRoots: 1}
			},
			want: "must be one of",
		},
		{
			name: "zero gap",
			make: func() (DerivedRuntime, *AttestedRootStore, DerivedAttestorConfig) {
				return runtime, store, DerivedAttestorConfig{AttestationHead: RunModeFinalized, GapBlocks: 0, MaxRoots: 1}
			},
			want: "gap",
		},
		{
			name: "zero root limit",
			make: func() (DerivedRuntime, *AttestedRootStore, DerivedAttestorConfig) {
				return runtime, store, DerivedAttestorConfig{AttestationHead: RunModeFinalized, GapBlocks: 1, MaxRoots: 0}
			},
			want: "limit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, store, config := tc.make()
			_, err := NewDerivedRootAttestor(runtime, store, config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewDerivedRootAttestor error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDerivedRootAttestorFailsClosedWhenHeadCommitmentIsInvalid(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 1)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	runtime := &derivedTestRuntime{snapshot: RuntimeSnapshot{
		Finalized:      BlockCommitment{BlockNumber: 100},
		FinalizedSeen:  true,
		Unsafe:         BlockCommitment{BlockNumber: 101},
		UnsafeObserved: true,
	}}
	attestor, err := NewDerivedRootAttestor(runtime, store, DerivedAttestorConfig{
		AttestationHead: RunModeUnsafe,
		GapBlocks:       1,
		MaxRoots:        1,
	})
	if err != nil {
		t.Fatalf("NewDerivedRootAttestor: %v", err)
	}
	if err := attestor.SyncOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "block hash") {
		t.Fatalf("SyncOnce error = %v, want invalid commitment", err)
	}
	if _, found := store.HighestAttested(true); found {
		t.Fatal("invalid Nitro head entered attested feed")
	}
}

type derivedTestRuntime struct {
	snapshot    RuntimeSnapshot
	commitments map[uint64]BlockCommitment
	failures    map[uint64]error
}

func (r *derivedTestRuntime) Snapshot() RuntimeSnapshot { return r.snapshot }

func (r *derivedTestRuntime) CommitmentAt(
	_ context.Context,
	height uint64,
) (BlockCommitment, error) {
	if err := r.failures[height]; err != nil {
		return BlockCommitment{}, err
	}
	return r.commitments[height], nil
}

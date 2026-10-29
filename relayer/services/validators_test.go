package services

import (
	"context"
	"errors"
	relayerclient "relayer/client"
	"relayer/prover/witness"
	"testing"
)

func TestFindHighestFeasibleHop_SearchRejectsAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := findHighestFeasibleHop(ctx, cosmosClientDeps{}, 1, 10, "chain-1", pinnedCosmosValidatorSet{}, 4, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSelectSignaturesForPinnedSet_ChoosesPinnedQuorum(t *testing.T) {
	pubkey0 := [32]byte{0x01}
	pubkey1 := [32]byte{0x02}
	pubkey2 := [32]byte{0x03}
	pubkey3 := [32]byte{0x04}

	candidates := []witness.ValidatorSignature{
		{Index: 0, PublicKey: pubkey0[:], Power: 40, Active: true},
		{Index: 1, PublicKey: pubkey1[:], Power: 40, Active: true},
		{Index: 2, PublicKey: pubkey2[:], Power: 30, Active: true},
		{Index: 3, PublicKey: pubkey3[:], Power: 30, Active: true},
	}
	pinned := pinnedCosmosValidatorSet{
		indices:      []uint32{0, 1, 2},
		pubkeys:      [][32]byte{pubkey2, pubkey3, pubkey0},
		votingPowers: []uint64{40, 40, 20},
		totalPower:   100,
	}

	selected, err := selectSignaturesForPinnedSet(candidates, pinned, 4)
	if err != nil {
		t.Fatalf("select signatures: %v", err)
	}

	got := make([]int, len(selected))
	for i, sig := range selected {
		got[i] = sig.Index
	}
	want := []int{2, 3}
	if len(got) != len(want) {
		t.Fatalf("selected indices: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selected indices: got %v want %v", got, want)
		}
	}
}

func TestPinnedOverlapPower(t *testing.T) {
	pubkey0 := [32]byte{0x01}
	pubkey1 := [32]byte{0x02}
	pubkey2 := [32]byte{0x03}

	candidates := []witness.ValidatorSignature{
		{Index: 0, PublicKey: pubkey0[:], Power: 40, Active: true},
		{Index: 1, PublicKey: pubkey1[:], Power: 40, Active: true}, // not pinned: contributes 0
	}
	pinned := pinnedCosmosValidatorSet{
		indices:      []uint32{0, 1},
		pubkeys:      [][32]byte{pubkey0, pubkey2},
		votingPowers: []uint64{30, 70},
		totalPower:   100,
	}

	if got := pinnedOverlapPower(candidates, pinned); got != 30 {
		t.Fatalf("overlap = %d, want 30 (pinned power of pubkey0 only)", got)
	}
	if got := pinnedOverlapPower(nil, pinned); got != 0 {
		t.Fatalf("overlap with no candidates = %d, want 0", got)
	}
}

func TestShouldRotatePinnedSet(t *testing.T) {
	threshold := relayerclient.TrustThreshold{Numerator: 5, Denominator: 6}

	if !shouldRotatePinnedSet(true, 100, 100, threshold) {
		t.Fatal("forceRotation should always rotate")
	}
	if shouldRotatePinnedSet(false, 90, 100, threshold) {
		t.Fatal("overlap 90/100 above 5/6 should not rotate")
	}
	// boundary: overlap/total exactly 5/6 (100*6 == 120*5)
	if !shouldRotatePinnedSet(false, 100, 120, threshold) {
		t.Fatal("overlap exactly at 5/6 should rotate")
	}
	if !shouldRotatePinnedSet(false, 70, 100, threshold) {
		t.Fatal("overlap 70/100 below 5/6 should rotate")
	}

	// threshold 1/1 rotates on any overlap (overlap <= total always holds)
	full := relayerclient.TrustThreshold{Numerator: 1, Denominator: 1}
	if !shouldRotatePinnedSet(false, 100, 100, full) {
		t.Fatal("threshold 1/1 should rotate even at full overlap")
	}
}

// A static validator set keeps nextValidatorsHash equal to the pinned hash
// forever. The forced rotation must still take the updateConsensusState path,
// or the pinned snapshot timestamp never moves and the client expires one
// trusting period after creation (#541).
func TestRotationCandidate(t *testing.T) {
	pinned := [32]byte{0xaa}
	changed := [32]byte{0xbb}

	if !rotationCandidate(true, pinned, pinned) {
		t.Fatal("forced rotation with an unchanged valset must still re-pin")
	}
	if !rotationCandidate(true, changed, pinned) {
		t.Fatal("forced rotation with a changed valset must re-pin")
	}
	if !rotationCandidate(false, changed, pinned) {
		t.Fatal("a changed valset must be considered for rotation")
	}
	if rotationCandidate(false, pinned, pinned) {
		t.Fatal("an unforced update with an unchanged valset must stay on updateApplicationState")
	}
}

func TestSelectSignaturesForPinnedSet_RejectsInsufficientOverlap(t *testing.T) {
	pubkey0 := [32]byte{0x01}
	pubkey1 := [32]byte{0x02}

	candidates := []witness.ValidatorSignature{
		{Index: 0, PublicKey: pubkey0[:], Power: 40, Active: true},
	}
	pinned := pinnedCosmosValidatorSet{
		indices:      []uint32{0, 1},
		pubkeys:      [][32]byte{pubkey0, pubkey1},
		votingPowers: []uint64{40, 60},
		totalPower:   100,
	}

	if _, err := selectSignaturesForPinnedSet(candidates, pinned, 4); err == nil {
		t.Fatal("expected insufficient pinned quorum error")
	}
}

func TestBinarySearchHighestFeasible(t *testing.T) {
	// TestBinarySearchHighestFeasible/converges is the RLY-01 multi-hop core: given
	// a synthetic monotonic feasibility predicate (feasible(h) == h <= threshold),
	// the search must find exactly the highest height in (trusted, latest] that
	// clears it, including the boundary cases (threshold at trusted+1, at
	// latest-1, and "no height clears it at all").
	t.Run("converges", func(t *testing.T) {
		cases := []struct {
			name            string
			trusted, latest int64
			threshold       int64
			wantHeight      int64
			wantOk          bool
		}{
			{"mid-range threshold", 100, 200, 150, 150, true},
			{"threshold at trusted+1", 100, 200, 101, 101, true},
			{"threshold at latest-1", 100, 200, 199, 199, true},
			{"no height clears it", 100, 200, 100, 0, false},
			{"single-block gap, target already infeasible", 100, 101, 101, 0, false},
			{"large gap", 1_000, 1_000_000, 654_321, 654_321, true},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				feasible := func(h int64) (bool, error) { return h <= tc.threshold, nil }
				height, ok, err := binarySearchHighestFeasible(tc.trusted, tc.latest, feasible)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if ok != tc.wantOk {
					t.Fatalf("ok = %v, want %v", ok, tc.wantOk)
				}
				if ok && height != tc.wantHeight {
					t.Fatalf("height = %d, want %d", height, tc.wantHeight)
				}
			})
		}
	})

	t.Run("does not reprobe known infeasible edges", func(t *testing.T) {
		calls := map[int64]int{}
		feasible := func(h int64) (bool, error) {
			calls[h]++
			return false, nil
		}
		height, ok, err := binarySearchHighestFeasible(100, 102, feasible)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok || height != 0 {
			t.Fatalf("height=%d ok=%v, want no feasible intermediate", height, ok)
		}
		if calls[102] != 0 {
			t.Fatalf("latest height was re-probed %d times", calls[102])
		}
		if calls[101] != 1 {
			t.Fatalf("trusted+1 was probed %d times, want exactly once", calls[101])
		}

		calls = map[int64]int{}
		height, ok, err = binarySearchHighestFeasible(100, 101, feasible)
		if err != nil {
			t.Fatalf("unexpected single-block error: %v", err)
		}
		if ok || height != 0 {
			t.Fatalf("single-block height=%d ok=%v, want no feasible intermediate", height, ok)
		}
		if len(calls) != 0 {
			t.Fatalf("single-block gap re-probed known failed target: calls=%v", calls)
		}
	})

	// TestBinarySearchHighestFeasible/propagates error verifies a genuine probe
	// error (e.g. an RPC failure fetching a candidate light block) is returned to
	// the caller rather than being misread as "infeasible at this height" — the
	// two must never be conflated, since one means "churn beat quorum here" and
	// the other means "we don't actually know."
	t.Run("propagates error", func(t *testing.T) {
		wantErr := errors.New("boom: rpc unavailable")
		feasible := func(h int64) (bool, error) {
			if h == 150 { // the first midpoint probed for (100, 200]
				return false, wantErr
			}
			return h <= 190, nil
		}
		_, _, err := binarySearchHighestFeasible(100, 200, feasible)
		if err == nil {
			t.Fatal("expected error to propagate")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected wrapped %v, got %v", wantErr, err)
		}
	})
}

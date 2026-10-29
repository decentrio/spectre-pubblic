package services

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	relayerclient "relayer/client"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testSlotsPerEpoch = 32

// fakeBeacon serves the two endpoints resolveBootstrapCheckpoint uses. A slot is
// "servable" only if it is listed in servable; every other slot 404s the same way a
// real node does for a skipped checkpoint. Requested slots are recorded so a test can
// assert exactly which boundaries were walked.
//
// noBootstrap models the other, more common failure: the block exists (so the root
// lookup succeeds) but the node has not retained a light-client bootstrap for it.
// The two endpoints fail independently on a real node, so the fake must too.
type fakeBeacon struct {
	servable    map[uint64]bool
	noBootstrap map[uint64]bool

	mu            sync.Mutex
	rootsAsked    []uint64
	bootstrapsFor []uint64
}

// rootFor is a deterministic stand-in for a beacon block root, and encodes the slot so
// the bootstrap handler can map a root back to the slot it belongs to.
func rootFor(slot uint64) string { return fmt.Sprintf("0x%064x", slot) }

func (f *fakeBeacon) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/eth/v1/beacon/blocks/", func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/eth/v1/beacon/blocks/"), "/root")
		slot, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			http.Error(w, "bad slot", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.rootsAsked = append(f.rootsAsked, slot)
		f.mu.Unlock()
		// Skipped slots have no block at all, so even the root lookup fails.
		if !f.servable[slot] {
			http.Error(w, "NOT_FOUND: no block at slot", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"data":{"root":%q}}`, rootFor(slot))
	})

	mux.HandleFunc("/eth/v1/beacon/light_client/bootstrap/", func(w http.ResponseWriter, r *http.Request) {
		root := strings.TrimPrefix(r.URL.Path, "/eth/v1/beacon/light_client/bootstrap/")
		slot, err := strconv.ParseUint(strings.TrimPrefix(root, "0x"), 16, 64)
		if err != nil {
			http.Error(w, "bad root", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.bootstrapsFor = append(f.bootstrapsFor, slot)
		f.mu.Unlock()
		// The block exists but its bootstrap was pruned — the walk must treat this the
		// same as a missing block and step back.
		if f.noBootstrap[slot] {
			http.Error(w, "NOT_FOUND: no light client bootstrap for this block root", http.StatusNotFound)
			return
		}
		// The aggregate pubkey carries the slot so the test can tell which checkpoint
		// the returned bootstrap actually came from.
		fmt.Fprintf(w, `{"data":{"current_sync_committee":{"pubkeys":[],"aggregate_pubkey":%q}}}`, rootFor(slot))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeBeacon) asked() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.rootsAsked...)
}

func TestResolveBootstrapCheckpoint(t *testing.T) {
	// TestResolveBootstrapCheckpoint/uses epoch boundary: a finalized header reported
	// mid-epoch must be rounded DOWN to its epoch boundary, because that is the slot the
	// beacon indexes bootstraps by.
	t.Run("uses epoch boundary", func(t *testing.T) {
		beacon := &fakeBeacon{servable: map[uint64]bool{4096: true}}
		url := beacon.start(t)

		slot, root, bootstrap, err := resolveBootstrapCheckpoint(url, "4127", testSlotsPerEpoch)
		if err != nil {
			t.Fatalf("resolveBootstrapCheckpoint: %v", err)
		}
		if slot != "4096" {
			t.Errorf("checkpoint slot = %q, want 4096 (the epoch boundary below 4127)", slot)
		}
		if root != rootFor(4096) {
			t.Errorf("root = %q, want %q", root, rootFor(4096))
		}
		if got := bootstrap.Data.CurrentSyncCommittee.AggregatePubkey; got != rootFor(4096) {
			t.Errorf("bootstrap came from the wrong checkpoint: aggregate_pubkey = %q", got)
		}
		if asked := beacon.asked(); len(asked) != 1 || asked[0] != 4096 {
			t.Errorf("asked for roots %v, want exactly [4096] — no step-back was needed", asked)
		}
	})

	// TestResolveBootstrapCheckpoint/steps back over skipped boundary is the bug this fix is
	// for: the boundary slot was skipped, so it has no block and no bootstrap. The walk
	// must fall back to the previous boundary rather than failing client creation.
	t.Run("steps back over skipped boundary", func(t *testing.T) {
		// 4096 skipped, 4064 servable.
		beacon := &fakeBeacon{servable: map[uint64]bool{4064: true}}
		url := beacon.start(t)

		slot, root, bootstrap, err := resolveBootstrapCheckpoint(url, "4100", testSlotsPerEpoch)
		if err != nil {
			t.Fatalf("resolveBootstrapCheckpoint: %v", err)
		}
		if slot != "4064" {
			t.Errorf("checkpoint slot = %q, want 4064 (one epoch back from the skipped 4096)", slot)
		}
		if root != rootFor(4064) {
			t.Errorf("root = %q, want %q", root, rootFor(4064))
		}
		if got := bootstrap.Data.CurrentSyncCommittee.AggregatePubkey; got != rootFor(4064) {
			t.Errorf("bootstrap came from the wrong checkpoint: aggregate_pubkey = %q", got)
		}
		if asked := beacon.asked(); len(asked) != 2 || asked[0] != 4096 || asked[1] != 4064 {
			t.Errorf("asked for roots %v, want [4096 4064]", asked)
		}
	})

	// TestResolveBootstrapCheckpoint/steps back when bootstrap pruned covers the failure that
	// motivated the walk in the first place, and the one a real node produces most often:
	// the checkpoint block is there — the root lookup succeeds — but the node no longer
	// retains a light-client bootstrap for it. The step-back must trigger on the bootstrap
	// error, not only on a missing block.
	t.Run("steps back when bootstrap pruned", func(t *testing.T) {
		// Both boundaries have blocks; only 4064 still has a bootstrap.
		beacon := &fakeBeacon{
			servable:    map[uint64]bool{4096: true, 4064: true},
			noBootstrap: map[uint64]bool{4096: true},
		}
		url := beacon.start(t)

		slot, root, bootstrap, err := resolveBootstrapCheckpoint(url, "4096", testSlotsPerEpoch)
		if err != nil {
			t.Fatalf("resolveBootstrapCheckpoint: %v", err)
		}
		if slot != "4064" {
			t.Errorf("checkpoint slot = %q, want 4064 (4096 has a block but no bootstrap)", slot)
		}
		if root != rootFor(4064) {
			t.Errorf("root = %q, want %q", root, rootFor(4064))
		}
		// The returned triple must be self-consistent: the caller checks this bootstrap
		// against the beacon block at the returned slot, so a mismatched pair would only
		// surface later as a confusing block-number error.
		if got := bootstrap.Data.CurrentSyncCommittee.AggregatePubkey; got != rootFor(4064) {
			t.Errorf("bootstrap came from the wrong checkpoint: aggregate_pubkey = %q", got)
		}
		// 4096's root was fetched and its bootstrap attempted before stepping back.
		if asked := beacon.asked(); len(asked) != 2 || asked[0] != 4096 || asked[1] != 4064 {
			t.Errorf("asked for roots %v, want [4096 4064]", asked)
		}
	})

	// TestResolveBootstrapCheckpoint/gives up after cap: a beacon that serves no bootstrap at
	// all must produce one bounded, explanatory failure — not an unbounded walk.
	t.Run("gives up after cap", func(t *testing.T) {
		beacon := &fakeBeacon{servable: map[uint64]bool{}}
		url := beacon.start(t)

		_, _, _, err := resolveBootstrapCheckpoint(url, "4100", testSlotsPerEpoch)
		if err == nil {
			t.Fatal("expected an error when no checkpoint is servable")
		}
		if !strings.Contains(err.Error(), "no servable light-client bootstrap") {
			t.Errorf("error does not explain the failure: %v", err)
		}
		if asked := beacon.asked(); len(asked) != maxBootstrapCheckpointStepBack {
			t.Errorf("asked for %d roots, want the %d-attempt cap: %v",
				len(asked), maxBootstrapCheckpointStepBack, asked)
		}
	})

	// TestResolveBootstrapCheckpoint/stops at genesis epoch guards the unsigned-slot subtraction:
	// inside the genesis epoch there is no earlier boundary, so the walk must stop at slot 0
	// instead of wrapping around to an absurd slot number.
	t.Run("stops at genesis epoch", func(t *testing.T) {
		beacon := &fakeBeacon{servable: map[uint64]bool{}}
		url := beacon.start(t)

		_, _, _, err := resolveBootstrapCheckpoint(url, "17", testSlotsPerEpoch)
		if err == nil {
			t.Fatal("expected an error when no checkpoint is servable")
		}
		asked := beacon.asked()
		if len(asked) != 1 || asked[0] != 0 {
			t.Fatalf("asked for roots %v, want exactly [0] — the walk must not go below genesis", asked)
		}
	})

	// TestResolveBootstrapCheckpoint/rejects bad input covers the two argument errors, both of
	// which would otherwise divide by zero or walk from a garbage slot.
	t.Run("rejects bad input", func(t *testing.T) {
		beacon := &fakeBeacon{servable: map[uint64]bool{}}
		url := beacon.start(t)

		if _, _, _, err := resolveBootstrapCheckpoint(url, "not-a-slot", testSlotsPerEpoch); err == nil {
			t.Error("expected an error for an unparseable finalized slot")
		}
		if _, _, _, err := resolveBootstrapCheckpoint(url, "4100", 0); err == nil {
			t.Error("expected an error for slots_per_epoch = 0")
		}
		if asked := beacon.asked(); len(asked) != 0 {
			t.Errorf("bad input reached the beacon: %v", asked)
		}
	})
}

func TestCosmosCurrentSlotReady(t *testing.T) {
	if cosmosCurrentSlotReady(932, 933) {
		t.Fatal("did not expect current slot below safety margin to be ready")
	}

	if cosmosCurrentSlotReady(935, 933) {
		t.Fatal("did not expect current slot equal to the minimum safety slot to be treated as not ready")
	}

	if !cosmosCurrentSlotReady(936, 933) {
		t.Fatal("expected current slot above safety margin to be ready")
	}
}

func TestCloneEthereumClientState(t *testing.T) {
	// cloneEthereumClientState is a one-line shallow copy: `cloned := *state`. That is
	// a correct deep copy today only because every field of EthereumClientState is a
	// value type. Add one slice, map or pointer -- a list of forks, say -- and the
	// "clone" starts sharing it, so a mutation through the copy reaches the original.
	//
	// The callers rely on independence: ethProofStateFromFinalityUpdate clones the
	// on-chain client state and then rewrites slots on the copy to build a proof
	// state, while the original stays the trusted one.
	//
	// Testing that by mutating fields would prove nothing until the day someone adds
	// a reference field, and would pass right up to it. This checks the property the
	// shallow copy actually depends on, so the failure arrives with the change that
	// breaks it rather than with the bug it later causes.
	t.Run("stays a deep copy", func(t *testing.T) {
		assertNoReferenceFields(t, reflect.TypeOf(relayerclient.EthereumClientState{}), "EthereumClientState")
	})

	t.Run("nil in nil out", func(t *testing.T) {
		if got := cloneEthereumClientState(nil); got != nil {
			t.Fatalf("cloning nil returned %+v; the caller checks for nil to report a missing state", got)
		}
	})

	// The clone must not be the same pointer, or every "proof state" edit would land
	// on the trusted state the caller kept.
	t.Run("returns a new pointer", func(t *testing.T) {
		original := &relayerclient.EthereumClientState{LatestSlot: 100}
		cloned := cloneEthereumClientState(original)
		if cloned == original {
			t.Fatal("clone returned the same pointer")
		}
		cloned.LatestSlot = 200
		if original.LatestSlot != 100 {
			t.Fatalf("writing to the clone changed the original: LatestSlot = %d", original.LatestSlot)
		}
	})
}

func assertNoReferenceFields(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	switch typ.Kind() {
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			assertNoReferenceFields(t, typ.Field(i).Type, path+"."+typ.Field(i).Name)
		}
	case reflect.Slice, reflect.Map, reflect.Ptr, reflect.Chan, reflect.Func, reflect.Interface:
		t.Errorf("%s is a %s, so `cloned := *state` now shares it with the original;"+
			" cloneEthereumClientState must copy it explicitly", path, typ.Kind())
	}
}

// parseSlot reads beacon slots out of JSON the relayer did not produce -- all
// five call sites pass a field straight from a beacon API response.
func TestParseSlot(t *testing.T) {
	t.Run("parses decimal slots", func(t *testing.T) {
		for _, tt := range []struct {
			in   string
			want uint64
		}{
			{"0", 0},
			{"123", 123},
			{"18446744073709551615", ^uint64(0)}, // max uint64
		} {
			got, err := parseSlot(tt.in)
			if err != nil {
				t.Errorf("parseSlot(%q): %v", tt.in, err)
				continue
			}
			if got != tt.want {
				t.Errorf("parseSlot(%q) = %d, want %d", tt.in, got, tt.want)
			}
		}
	})

	// Everything else is rejected. The three middle cases used to be ACCEPTED:
	// fmt.Sscanf("%d") stopped at the first non-digit and reported success on the
	// prefix, so "123abc" became 123, "1e3" became 1, and "0x10" became 0 -- which
	// downstream reads as genesis. A slot decides which state a proof is built
	// against, so a wrong-but-plausible number is the worst outcome available.
	t.Run("rejects anything that is not a plain decimal", func(t *testing.T) {
		for name, in := range map[string]string{
			"empty":               "",
			"negative":            "-5",
			"overflows uint64":    "99999999999999999999999",
			"digits then letters": "123abc",
			"scientific notation": "1e3",
			"hex":                 "0x10",
			"leading whitespace":  "  7",
			"trailing whitespace": "7 ",
			"decimal point":       "7.0",
			"thousands separator": "1,000",
		} {
			t.Run(name, func(t *testing.T) {
				got, err := parseSlot(in)
				if err == nil {
					t.Fatalf("parseSlot(%q) returned %d with no error", in, got)
				}
				// The value must be unusable, not merely accompanied by an error:
				// a caller that forgets to check err must not silently get genesis
				// back as a plausible slot... which is exactly what it would get.
				// Zero is the only honest answer here, so the error is the contract.
				if got != 0 {
					t.Fatalf("parseSlot(%q) = %d alongside an error", in, got)
				}
				// The message must carry BOTH halves: strconv quotes the offending
				// input, and the wrapper says which kind of value it was. Without
				// the wrapper an operator sees a bare parse error and has to guess
				// which field of which beacon response produced it.
				if in != "" && !strings.Contains(err.Error(), in) {
					t.Errorf("error %q does not quote the offending input %q", err, in)
				}
				if !strings.Contains(err.Error(), "beacon slot") {
					t.Errorf("error %q does not say what kind of value failed to parse", err)
				}
			})
		}
	})

	// A slot string the relayer cannot parse must not be reported as slot 0, which
	// downstream reads as "genesis" and would rebuild the client from scratch.
	t.Run("error does not look like genesis", func(t *testing.T) {
		got, err := parseSlot("")
		if err == nil {
			t.Fatal("an empty slot string was accepted")
		}
		if got != 0 {
			t.Fatalf("slot = %d alongside an error; callers must not use it", got)
		}
		if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "unexpected") {
			t.Logf("error text is %q -- fine, but callers must check err, not the value", err)
		}
	})
}

// Which of the two stored committees the client checks against is decided by the
// label on the header, and the client picks it by the SIGNATURE slot's period:
//
//	sync_committee = if signature_period == stored_period { current } else { next }
//
// (ethereum/light-client verify.rs). Getting the label wrong fails on chain with
// "current sync committee (X) does not match with the one in the current state (Y)"
// -- a message that names two aggregate pubkeys and neither period.
func TestActiveCommitteeFor(t *testing.T) {
	committee := &relayerclient.SyncCommittee{AggregatePubkey: "0xaggregate"}

	cases := []struct {
		name            string
		signaturePeriod uint64
		storedPeriod    uint64
		wantCurrent     bool
		wantNext        bool
		wantErr         bool
		why             string
	}{
		{
			name:            "same period is the current committee",
			signaturePeriod: 1343,
			storedPeriod:    1343,
			wantCurrent:     true,
			why:             "the steady state: the signature is in the period the client is finalized in",
		},
		{
			name:            "one period ahead is the next committee",
			signaturePeriod: 1344,
			storedPeriod:    1343,
			wantNext:        true,
			why: "the last ~65 slots of every period: signature_slot has crossed while the client's " +
				"finalized slot has not. Labelling this Current is the stall",
		},
		{
			name:            "two periods ahead is refused",
			signaturePeriod: 1345,
			storedPeriod:    1343,
			wantErr:         true,
			why:             "the client rejects it outright, so building the header only spends gas to be told so",
		},
		{
			name:            "a period behind is refused",
			signaturePeriod: 1342,
			storedPeriod:    1343,
			wantErr:         true,
			why:             "the client has no committee older than its current one to check against",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			active, err := activeCommitteeFor(tc.signaturePeriod, tc.storedPeriod, committee)
			if (err != nil) != tc.wantErr {
				t.Fatalf("activeCommitteeFor(%d, %d) error = %v, want error %v: %s",
					tc.signaturePeriod, tc.storedPeriod, err, tc.wantErr, tc.why)
			}
			if tc.wantErr {
				return
			}
			if (active.Current != nil) != tc.wantCurrent {
				t.Fatalf("Current set = %v, want %v: %s", active.Current != nil, tc.wantCurrent, tc.why)
			}
			if (active.Next != nil) != tc.wantNext {
				t.Fatalf("Next set = %v, want %v: %s", active.Next != nil, tc.wantNext, tc.why)
			}
			// Exactly one, never both: the client reads one field and the other
			// would be silently ignored.
			if (active.Current != nil) == (active.Next != nil) {
				t.Fatal("exactly one of Current/Next must be set")
			}
		})
	}

	// The boundary is a real slot pair, not a rounded one: signature_slot is
	// attested_slot + 1, so a single slot decides the label once every 8192.
	t.Run("the boundary falls on one real slot", func(t *testing.T) {
		const slotsPerPeriod = 8192 // EpochsPerSyncCommitteePeriod(256) * SlotsPerEpoch(32)
		cs := &relayerclient.EthereumClientState{
			EpochsPerSyncCommitteePeriod: 256,
			SlotsPerEpoch:                32,
		}

		// Period 1344 starts at 8192 * 1344 = 11010048. The attested slot one below it
		// is still 1343 while its signature slot is already 1344 -- the one slot where
		// choosing by attested rather than signature gives the wrong answer.
		const lastSlotOf1343 = slotsPerPeriod*1344 - 1

		if got := cs.ComputeSyncCommitteePeriodAtSlot(lastSlotOf1343); got != 1343 {
			t.Fatalf("attested slot %d is in period %d, want 1343", lastSlotOf1343, got)
		}
		if got := cs.ComputeSyncCommitteePeriodAtSlot(lastSlotOf1343 + 1); got != 1344 {
			t.Fatalf("signature slot %d is in period %d, want 1344", lastSlotOf1343+1, got)
		}

		// Reading the period from the attested slot would label this Current against a
		// client stored in 1343 -- which is what the on-chain rejection looked like.
		byAttested, err := activeCommitteeFor(cs.ComputeSyncCommitteePeriodAtSlot(lastSlotOf1343), 1343, &relayerclient.SyncCommittee{})
		if err != nil || byAttested.Current == nil {
			t.Fatalf("attested-slot period must resolve to Current, got %+v err=%v", byAttested, err)
		}
		bySignature, err := activeCommitteeFor(cs.ComputeSyncCommitteePeriodAtSlot(lastSlotOf1343+1), 1343, &relayerclient.SyncCommittee{})
		if err != nil || bySignature.Next == nil {
			t.Fatalf("signature-slot period must resolve to Next, got %+v err=%v", bySignature, err)
		}
	})
}

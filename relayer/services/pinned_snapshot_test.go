package services

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	spectreContract "relayer/bindings/SpectreClient"
	updateClientContract "relayer/bindings/UpdateClient"
	relayerclient "relayer/client"
	"relayer/rpcmock"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

var pinnedSnapshotTestClient = common.HexToAddress("0x00000000000000000000000000000000000000dd")

// pinnedSnapshotNode answers getClientState and getPinnedSnapshot the way a
// SpectreClient does after a few updateApplicationState calls and no rotation:
// latestHeight has moved past the snapshot height.
func pinnedSnapshotNode(t *testing.T, snapshotHeight uint64, snapshotTime time.Time, trustingPeriod uint32) (*rpcmock.EVMNode, EVMEndpoint) {
	t.Helper()
	spectreABI, err := spectreContract.ContractSpectreClientMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	node := rpcmock.NewEVMNode(t)

	state, err := relayerclient.EncodeClientState(relayerclient.ClientState{
		ChainId:        "test-1",
		TrustLevel:     relayerclient.TrustThreshold{Numerator: 2, Denominator: 3},
		LatestHeight:   updateClientContract.IICS02ClientMsgsHeight{RevisionNumber: 1, RevisionHeight: snapshotHeight + 500},
		TrustingPeriod: trustingPeriod,
	})
	if err != nil {
		t.Fatal(err)
	}
	stateOut, err := spectreABI.Methods["getClientState"].Outputs.Pack(state)
	if err != nil {
		t.Fatal(err)
	}
	node.SetCallResultFor(hexutil.Encode(spectreABI.Methods["getClientState"].ID), hexutil.Encode(stateOut))

	snapOut, err := spectreABI.Methods["getPinnedSnapshot"].Outputs.Pack(snapshotHeight, big.NewInt(snapshotTime.UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	node.SetCallResultFor(hexutil.Encode(spectreABI.Methods["getPinnedSnapshot"].ID), hexutil.Encode(snapOut))

	return node, EVMEndpoint{Client: node.Client(t), Contracts: EVMContracts{SpectreClient: pinnedSnapshotTestClient}}
}

func TestFetchPinnedSnapshotTime(t *testing.T) {
	ctx := context.Background()
	snapshotTime := time.Date(2026, 8, 20, 12, 43, 56, 0, time.UTC)

	t.Run("reads the snapshot timestamp", func(t *testing.T) {
		_, evm := pinnedSnapshotNode(t, 6593231, snapshotTime, 57600)
		got, supported, err := fetchPinnedSnapshotTime(ctx, evm)
		if err != nil || !supported || !got.Equal(snapshotTime) {
			t.Fatalf("got %v supported=%v err=%v; want %v supported=true", got, supported, err, snapshotTime)
		}
	})

	// A client deployed before the getter existed reverts on the unknown
	// selector. That is "fall back", not an error the caller has to retry.
	t.Run("revert means a pre-#541 client", func(t *testing.T) {
		node, evm := pinnedSnapshotNode(t, 6593231, snapshotTime, 57600)
		node.FailCall(errors.New("execution reverted"))
		_, supported, err := fetchPinnedSnapshotTime(ctx, evm)
		if err != nil || supported {
			t.Fatalf("supported=%v err=%v; want unsupported with no error", supported, err)
		}
	})

	// A revert that carries data arrives as code 3 with hex data: still a revert.
	t.Run("code-3 revert with data means a pre-#541 client", func(t *testing.T) {
		node, evm := pinnedSnapshotNode(t, 6593231, snapshotTime, 57600)
		node.FailCallWith(3, "execution reverted", "0x08c379a0")
		_, supported, err := fetchPinnedSnapshotTime(ctx, evm)
		if err != nil || supported {
			t.Fatalf("supported=%v err=%v; want unsupported with no error", supported, err)
		}
	})

	// Review of d85c8cd: a throttled endpoint whose error carries diagnostic
	// data was read as a revert, and the expiry silently fell back to the
	// latest-height anchor. Data alone is not a revert signal.
	t.Run("rate limit with data is an error, not a fallback", func(t *testing.T) {
		node, evm := pinnedSnapshotNode(t, 6593231, snapshotTime, 57600)
		node.FailCallWith(-32005, "limit exceeded", map[string]any{"retryAfter": 1})
		_, supported, err := fetchPinnedSnapshotTime(ctx, evm)
		if err == nil || supported {
			t.Fatalf("supported=%v err=%v; want an error so the caller retries", supported, err)
		}
	})

	// A node that never ran the call must not be read as an old contract:
	// downgrading to the latest-height anchor on a blip is the #541 bug.
	t.Run("transport failure is an error", func(t *testing.T) {
		node, evm := pinnedSnapshotNode(t, 6593231, snapshotTime, 57600)
		node.FailCall(errors.New("header not found"))
		if _, _, err := fetchPinnedSnapshotTime(ctx, evm); err == nil {
			t.Fatal("want an error for a non-revert RPC failure")
		}
	})
}

// The #541 shape: latestHeight advanced by app-state updates, pinned set never
// rotated. Expiry must follow the snapshot, and must not need the Cosmos RPC
// (the zero CosmosEndpoint would panic if it were touched).
func TestCosmosClientExpiry_AnchorsOnPinnedSnapshot(t *testing.T) {
	snapshotTime := time.Date(2026, 8, 20, 12, 43, 56, 0, time.UTC)
	const trustingPeriod = 57600 // 16h
	_, evm := pinnedSnapshotNode(t, 6593231, snapshotTime, trustingPeriod)

	expiresAt, period, err := CosmosClientExpiry(context.Background(), CosmosEndpoint{}, evm)
	if err != nil {
		t.Fatal(err)
	}
	if want := snapshotTime.Add(16 * time.Hour); !expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v, want %v (snapshot + trusting period)", expiresAt, want)
	}
	if period != 16*time.Hour {
		t.Fatalf("period = %v, want 16h", period)
	}
}

func TestSnapshotTimestampToTime(t *testing.T) {
	want := time.Date(2026, 8, 20, 12, 43, 56, 0, time.UTC)
	if got, err := snapshotTimestampToTime(big.NewInt(want.UnixNano())); err != nil || !got.Equal(want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
	for name, nanos := range map[string]*big.Int{
		"nil":      nil,
		"zero":     big.NewInt(0),
		"negative": big.NewInt(-1),
		"overflow": new(big.Int).Lsh(big.NewInt(1), 64),
	} {
		if _, err := snapshotTimestampToTime(nanos); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

func TestRotationDueIn(t *testing.T) {
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	anchor := now.Add(-10 * time.Hour)
	if got := rotationDueIn(anchor, 15*time.Hour, now); got != 5*time.Hour {
		t.Fatalf("due = %v, want 5h", got)
	}
	if got := rotationDueIn(anchor, 10*time.Hour, now); got != 0 {
		t.Fatalf("exactly due = %v, want 0", got)
	}
	if got := rotationDueIn(anchor, time.Hour, now); got != 0 {
		t.Fatalf("overdue = %v, want 0 (rotate now)", got)
	}
}

// The #541 client by the chain's clock: Active up to the last second of the
// trusting period, Expired from the boundary on. Expired is what arms the relay
// module's client hold, so the refresh and rotation loops stop paying for
// reverts.
func TestCosmosClientStatus_ExpiresWithPinnedSnapshot(t *testing.T) {
	ctx := context.Background()
	snapshotTime := time.Date(2026, 8, 20, 12, 43, 56, 0, time.UTC)
	node, evm := pinnedSnapshotNode(t, 6593231, snapshotTime, 57600)

	node.SetBlockTime(uint64(snapshotTime.Add(16*time.Hour - time.Second).Unix()))
	if got, err := CosmosClientStatus(ctx, evm, ""); err != nil || got != "Active" {
		t.Fatalf("one second before expiry: got %q, %v; want Active", got, err)
	}

	node.SetBlockTime(uint64(snapshotTime.Add(16 * time.Hour).Unix()))
	got, err := CosmosClientStatus(ctx, evm, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Expired") || !strings.Contains(got, "migrateClient") {
		t.Fatalf("at the trusting-period boundary: got %q; want an Expired status naming migrateClient", got)
	}
}

// Review of 65786fb: the snapshot and the clock came from two independent
// "latest" reads. A rotation landing just before the deadline could fall
// between them -- old snapshot, new block time -- and a live client was
// reported Expired, arming the hold and recommending a migration.
//
// Modelled with one node: the head is block 101, past the old snapshot's
// deadline, and the rotation landed in it. A "latest"-tagged getPinnedSnapshot
// answers the pre-rotation snapshot (the read served before the block), a call
// pinned at 101 answers the rotated one. Read at one block, the client is Active.
func TestCosmosClientStatus_ReadsSnapshotAndClockAtOneBlock(t *testing.T) {
	ctx := context.Background()
	oldSnapshot := time.Date(2026, 8, 20, 12, 43, 56, 0, time.UTC)
	const period = 57600 // 16h
	node, evm := pinnedSnapshotNode(t, 6593231, oldSnapshot, period)

	const head = 101
	headTime := oldSnapshot.Add(16*time.Hour + 12*time.Second) // past the OLD deadline
	node.SetBlockNumber(head)
	node.SetBlockTime(uint64(headTime.Unix()))

	spectreABI, err := spectreContract.ContractSpectreClientMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	rotated := headTime.Add(-12 * time.Second) // the rotation's own header, one block back
	out, err := spectreABI.Methods["getPinnedSnapshot"].Outputs.Pack(uint64(6650000), big.NewInt(rotated.UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	node.SetCallResultForAt(head, hexutil.Encode(spectreABI.Methods["getPinnedSnapshot"].ID), hexutil.Encode(out))

	got, err := CosmosClientStatus(ctx, evm, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Active" {
		t.Fatalf("snapshot and clock read at one block must report the rotated client Active, got %q", got)
	}
}

// Review of bfbbd2a: on a load-balanced RPC the header and the pinned eth_call
// can land on different backends, and one a block behind answers "header not
// found". That is not a revert, and an error status makes
// destinationClientInactive answer "do not know" -- the drop path. A failed
// pinned read falls back to "latest", believed only when it says not expired;
// Expired needs a pinned read to confirm it.
func TestCosmosClientStatus_PinnedReadOnALaggingBackend(t *testing.T) {
	ctx := context.Background()
	const period = 57600 // 16h
	const head = 101
	headTime := time.Date(2026, 8, 21, 4, 44, 8, 0, time.UTC)
	staleSnapshot := headTime.Add(-16*time.Hour - 12*time.Second) // past its deadline at head
	const headerNotFound = "header not found"

	spectreABI, err := spectreContract.ContractSpectreClientMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	snapshotAt := func(node *rpcmock.EVMNode, block uint64, at time.Time) {
		out, err := spectreABI.Methods["getPinnedSnapshot"].Outputs.Pack(uint64(6650000), big.NewInt(at.UnixNano()))
		if err != nil {
			t.Fatal(err)
		}
		node.SetCallResultForAt(block, hexutil.Encode(spectreABI.Methods["getPinnedSnapshot"].ID), hexutil.Encode(out))
	}
	setup := func(latestSnapshot time.Time) (*rpcmock.EVMNode, EVMEndpoint) {
		node, evm := pinnedSnapshotNode(t, 6593231, latestSnapshot, period)
		node.SetBlockNumber(head)
		node.SetBlockTime(uint64(headTime.Unix()))
		return node, evm
	}

	t.Run("healthy client: the latest fallback answers Active", func(t *testing.T) {
		node, evm := setup(headTime.Add(-time.Hour))
		node.FailCallAt(head, 1, -32000, headerNotFound)
		if got, err := CosmosClientStatus(ctx, evm, ""); err != nil || got != "Active" {
			t.Fatalf("got %q, %v; want Active with no error", got, err)
		}
	})

	t.Run("fallback says expired, the confirming pinned read agrees", func(t *testing.T) {
		shortConfirmWait(t)
		node, evm := setup(staleSnapshot)
		node.FailCallAt(head, 1, -32000, headerNotFound)
		got, err := CosmosClientStatus(ctx, evm, "")
		if err != nil || !strings.HasPrefix(got, "Expired") {
			t.Fatalf("got %q, %v; want Expired once a pinned read confirms it", got, err)
		}
	})

	// The lag the fallback can show: the "latest" backend still has the
	// pre-rotation snapshot. The confirming pinned read sees the rotation.
	t.Run("fallback says expired, the confirming pinned read sees the rotation", func(t *testing.T) {
		shortConfirmWait(t)
		node, evm := setup(staleSnapshot)
		node.FailCallAt(head, 1, -32000, headerNotFound)
		snapshotAt(node, head, headTime.Add(-12*time.Second))
		if got, err := CosmosClientStatus(ctx, evm, ""); err != nil || got != "Active" {
			t.Fatalf("got %q, %v; want Active: the fallback's Expired was lag", got, err)
		}
	})

	// The common lag: the backend reaches the block a few retries later. The
	// confirm waits for it at the SAME block, so the answer is a status, not an
	// error -- an error is "do not know", the drop path.
	t.Run("the confirm waits for a lagging backend to reach the block", func(t *testing.T) {
		shortConfirmWait(t)
		node, evm := setup(staleSnapshot)
		node.FailCallAt(head, 3, -32000, headerNotFound) // first read + two confirm retries
		snapshotAt(node, head, headTime.Add(-12*time.Second))
		if got, err := CosmosClientStatus(ctx, evm, ""); err != nil || got != "Active" {
			t.Fatalf("got %q, %v; want Active once the backend catches up", got, err)
		}
	})

	t.Run("an unconfirmed Expired is an error, never Expired", func(t *testing.T) {
		shortConfirmWait(t)
		node, evm := setup(staleSnapshot)
		node.FailCallAt(head, 1000, -32000, headerNotFound) // never catches up
		got, err := CosmosClientStatus(ctx, evm, "")
		if err == nil || got != "" {
			t.Fatalf("got %q, %v; want an error and no status", got, err)
		}
	})

	t.Run("the confirm wait honours cancellation", func(t *testing.T) {
		node, evm := setup(staleSnapshot)
		node.FailCallAt(head, 1000, -32000, headerNotFound)
		cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond) // the real 1s first delay never fires
		defer cancel()
		start := time.Now()
		if got, err := CosmosClientStatus(cctx, evm, ""); err == nil || got != "" {
			t.Fatalf("got %q, %v; want an error and no status", got, err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("cancellation took %s; the wait must end with the context", took)
		}
	})
}

// shortConfirmWait shrinks the confirming read's retry schedule to keep the
// lagging-backend tests fast; the budget still allows several retries.
func shortConfirmWait(t *testing.T) {
	t.Helper()
	prevDelay, prevBudget := pinnedConfirmFirstDelay, pinnedConfirmBudget
	pinnedConfirmFirstDelay, pinnedConfirmBudget = time.Millisecond, 15*time.Millisecond
	t.Cleanup(func() { pinnedConfirmFirstDelay, pinnedConfirmBudget = prevDelay, prevBudget })
}

// pinnedSnapshotExpired must agree with the contract's check to the second, or
// the relayer either holds a client that still accepts updates or pays for a
// revert on one that does not.
func TestPinnedSnapshotExpired(t *testing.T) {
	snapshot := time.Date(2026, 8, 20, 12, 43, 56, 900_000_000, time.UTC)
	const period = 57600
	for _, tc := range []struct {
		name    string
		now     time.Time
		wantAge time.Duration
		expired bool
	}{
		{"fresh", snapshot.Add(time.Hour), time.Hour, false},
		// Sub-second parts are floored on both sides, as nanos / 1e9 is on-chain.
		{"last valid second", snapshot.Add(16*time.Hour - time.Second), 16*time.Hour - time.Second, false},
		{"boundary", snapshot.Add(16 * time.Hour), 16 * time.Hour, true},
		{"#541", snapshot.Add(4042888 * time.Second), 4042888 * time.Second, true},
		{"snapshot ahead of the chain clock", snapshot.Add(-time.Minute), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			age, expired := pinnedSnapshotExpired(snapshot, tc.now, period)
			if age != tc.wantAge || expired != tc.expired {
				t.Fatalf("got age=%v expired=%v; want age=%v expired=%v", age, expired, tc.wantAge, tc.expired)
			}
		})
	}
}

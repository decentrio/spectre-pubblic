package bold

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	arbitrum "attestor/arbitrum"
	attestortypes "attestor/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

type headTestSource struct {
	assertionTestSource
	head            uint64
	events          []arbitrum.ProposedAssertion
	confirmed       bool
	headErr         error
	canonicalCheck  func()
	hashErr         error
	changedAt       uint64
	failRangeFrom   uint64
	statusHashErr   error
	statusNumberErr error
	heads           []arbitrum.L1IngestHead
	statusBlocks    []uint64
	statusHashes    []common.Hash
	ranges          [][2]uint64
}

func blockHash(n uint64) common.Hash { return common.BigToHash(new(big.Int).SetUint64(n)) }

func (s *headTestSource) Head(_ context.Context, h arbitrum.L1IngestHead) (L1Block, error) {
	s.heads = append(s.heads, h)
	if h == arbitrum.L1IngestFinalized {
		return L1Block{Number: s.finalizedBlock, Hash: blockHash(s.finalizedBlock)}, s.finalizedErr
	}
	return L1Block{Number: s.head, Hash: blockHash(s.head)}, s.headErr
}

func (s *headTestSource) CanonicalHash(_ context.Context, n uint64) (common.Hash, error) {
	if s.canonicalCheck != nil {
		s.canonicalCheck()
	}
	if s.changedAt == n {
		return common.HexToHash("0xbad"), nil
	}
	return blockHash(n), s.hashErr
}

func (s *headTestSource) Assertions(_ context.Context, from, to uint64) ([]arbitrum.ProposedAssertion, []ConfirmedAssertion, error) {
	s.ranges = append(s.ranges, [2]uint64{from, to})
	if s.failRangeFrom != 0 && from >= s.failRangeFrom {
		return nil, nil, errors.New("log range failed")
	}
	var proposals []arbitrum.ProposedAssertion
	var confirmations []ConfirmedAssertion
	for _, p := range s.events {
		if p.L1BlockNumber >= from && p.L1BlockNumber <= to {
			proposals = append(proposals, p)
		}
		if s.confirmed && s.head >= from && s.head <= to {
			confirmations = append(confirmations, ConfirmedAssertion{AssertionHash: p.AssertionHash, L2BlockHash: p.L2BlockHash, L1BlockNumber: s.head})
		}
	}
	return proposals, confirmations, nil
}

func (s *headTestSource) status(p arbitrum.ProposedAssertion, n uint64) uint8 {
	for _, event := range s.events {
		if event.AssertionHash == p.AssertionHash && event.L1BlockNumber <= n {
			if s.confirmed && s.head <= n {
				return assertionStatusConfirmed
			}
			return assertionStatusPending
		}
	}
	return assertionStatusNone
}

func (s *headTestSource) AssertionStatus(_ context.Context, p arbitrum.ProposedAssertion, n uint64) (uint8, error) {
	s.statusBlocks = append(s.statusBlocks, n)
	return s.status(p, n), s.statusNumberErr
}

func (s *headTestSource) AssertionStatusAtHash(_ context.Context, p arbitrum.ProposedAssertion, h common.Hash) (uint8, error) {
	s.statusHashes = append(s.statusHashes, h)
	return s.status(p, h.Big().Uint64()), s.statusHashErr
}

type headTestResolver struct {
	finalized bool
	err       error
	modes     []arbitrum.RunMode
}

func (r *headTestResolver) ResolveCanonicalBlockHash(_ context.Context, hash common.Hash, mode arbitrum.RunMode) (arbitrum.BlockCommitment, error) {
	r.modes = append(r.modes, mode)
	if r.err != nil {
		return arbitrum.BlockCommitment{}, r.err
	}
	if mode == arbitrum.RunModeFinalized && !r.finalized {
		return arbitrum.BlockCommitment{}, arbitrum.ErrCommitmentNotReady
	}
	return arbitrum.BlockCommitment{BlockNumber: 500, BlockHash: hash, StateRoot: common.HexToHash("0x777")}, nil
}

func earlyProposal(hash int64, block uint64) arbitrum.ProposedAssertion {
	return arbitrum.ProposedAssertion{AssertionHash: common.BigToHash(big.NewInt(hash)), L2BlockHash: common.BigToHash(big.NewInt(hash + 1000)), L1BlockNumber: block}
}

func newHeadTestLoop(t *testing.T, mode arbitrum.L1IngestHead) (*AssertionAttestor, *headTestSource, *headTestResolver) {
	t.Helper()
	store, err := arbitrum.NewAttestedRootStore("arb", 101)
	if err != nil {
		t.Fatal(err)
	}
	s := &headTestSource{assertionTestSource: assertionTestSource{finalizedBlock: 100}, head: 110, events: []arbitrum.ProposedAssertion{earlyProposal(1, 110)}}
	r := &headTestResolver{}
	a, err := NewAssertionAttestor(s, r, store, AssertionAttestorConfig{L1IngestHead: mode, MaxL1BlockRange: 5})
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Unix(1000, 0) }
	return a, s, r
}

func TestEarlyIngestSurvivesUntilFinalityAndPromotesOnlyAtFinalized(t *testing.T) {
	for _, mode := range []arbitrum.L1IngestHead{arbitrum.L1IngestSafe, arbitrum.L1IngestLatest} {
		t.Run(string(mode), func(t *testing.T) {
			a, s, r := newHeadTestLoop(t, mode)
			s.confirmed = true // Even a confirmation in the speculative window is insufficient.
			if err := a.SyncOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			root, found := a.store.HighestAttested(true)
			if !found || !root.Provisional || root.AssertionConfirmed {
				t.Fatalf("early root = %+v, found=%v", root, found)
			}
			if _, found := a.store.HighestAttested(false); found {
				t.Fatal("early assertion entered finalized frontier")
			}
			if a.store.NextL1Block() != 101 {
				t.Fatal("speculation advanced durable cursor")
			}
			if !reflect.DeepEqual(s.heads, []arbitrum.L1IngestHead{arbitrum.L1IngestFinalized, mode}) {
				t.Fatalf("heads = %v", s.heads)
			}
			if !reflect.DeepEqual(s.statusHashes, []common.Hash{blockHash(110)}) {
				t.Fatalf("hash reads = %v", s.statusHashes)
			}
			a.now = func() time.Time { return time.Unix(2000, 0) }
			if err := a.SyncOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			repeated, found := a.store.HighestAttested(true)
			if !found || repeated.L2BlockNumber < root.L2BlockNumber || repeated.AttestedAt != root.AttestedAt {
				t.Fatal("unchanged entry lost timestamp")
			}
			s.finalizedBlock = 110
			if err := a.SyncOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, found := a.store.HighestAttested(false); found {
				t.Fatal("promoted before Nitro finalized")
			}
			beforePromotion, found := a.store.HighestAttested(true)
			if !found || beforePromotion.L2BlockNumber < repeated.L2BlockNumber {
				t.Fatal("frontier regressed while waiting for Nitro finalization")
			}
			r.finalized = true
			if err := a.SyncOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			promoted, found := a.store.HighestAttested(false)
			if !found || promoted.L2BlockNumber != root.L2BlockNumber || promoted.AttestedAt != root.AttestedAt {
				t.Fatalf("promotion = %+v", promoted)
			}
			frontier, found := a.store.HighestAttested(true)
			if !found || frontier.L2BlockNumber < beforePromotion.L2BlockNumber {
				t.Fatal("frontier regressed on promotion")
			}
			for _, n := range s.statusBlocks {
				if n != 110 {
					t.Fatalf("finalized reconciliation used %d", n)
				}
			}
		})
	}
}

func TestEarlyIngestReorgRepostAndHeadRegression(t *testing.T) {
	a, s, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	ctx := context.Background()
	metrics := arbitrum.NewMetrics(prometheus.NewRegistry())
	a.store.SetMetrics(metrics)
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	original, _ := a.store.HighestAttested(true)
	s.events[0].L1BlockNumber = 115
	s.head = 115
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	reposted, _ := a.store.HighestAttested(true)
	if reposted.L1BlockNumber != 115 || reposted.AttestedAt != original.AttestedAt {
		t.Fatalf("reposted = %+v", reposted)
	}
	s.events = []arbitrum.ProposedAssertion{earlyProposal(2, 113)}
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	replaced, _ := a.store.HighestAttested(true)
	if replaced.AssertionHash != s.events[0].AssertionHash || len(a.store.ProvisionalAssertions()) != 1 {
		t.Fatalf("replacement = %+v", replaced)
	}
	if a.store.NextL1Block() != 101 {
		t.Fatal("reorg skipped durable window")
	}
	if metricValue(metrics.AssertionRevocations.WithLabelValues("arb")) != 1 {
		t.Fatal("replacement revocation not counted")
	}
	s.head = 105
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found := a.store.HighestAttested(true); found {
		t.Fatal("root above regressed head retained")
	}
	if metricValue(metrics.AssertionIngestHead.WithLabelValues("arb")) != 105 {
		t.Fatal("successful head not observed")
	}
	if metricValue(metrics.AssertionRevocations.WithLabelValues("arb")) != 2 {
		t.Fatal("regression revocation not counted")
	}
}

func TestEarlyIngestFailuresDoNotPublishPartialSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*headTestSource, *headTestResolver)
	}{
		{"unsupported head", func(s *headTestSource, _ *headTestResolver) { s.headErr = errors.New("unsupported safe tag") }},
		{"second log chunk", func(s *headTestSource, _ *headTestResolver) { s.failRangeFrom = 106 }},
		{"status at hash", func(s *headTestSource, _ *headTestResolver) { s.statusHashErr = errors.New("hash unavailable") }},
		{"canonical read", func(s *headTestSource, _ *headTestResolver) { s.hashErr = errors.New("RPC unavailable") }},
		{"ingest snapshot changed", func(s *headTestSource, _ *headTestResolver) { s.changedAt = 110 }},
		{"finalized snapshot changed", func(s *headTestSource, _ *headTestResolver) { s.changedAt = 100 }},
		{"Nitro RPC failure", func(_ *headTestSource, r *headTestResolver) { r.err = errors.New("Nitro unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s, r := newHeadTestLoop(t, arbitrum.L1IngestSafe)
			metrics := arbitrum.NewMetrics(prometheus.NewRegistry())
			a.store.SetMetrics(metrics)
			if err := a.SyncOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := a.store.ProvisionalAssertions()
			s.events = []arbitrum.ProposedAssertion{earlyProposal(2, 103), earlyProposal(3, 108)}
			tc.mutate(s, r)
			if err := a.SyncOnce(context.Background()); err == nil {
				t.Fatal("expected failure")
			}
			if !reflect.DeepEqual(before, a.store.ProvisionalAssertions()) || len(a.store.Proposals()) != 0 || a.store.NextL1Block() != 101 {
				t.Fatal("failed speculative pass published partial state")
			}
			if metricValue(metrics.AssertionRevocations.WithLabelValues("arb")) != 0 {
				t.Fatal("failed pass counted withdrawals")
			}
		})
	}
}

func TestEarlyIngestPersistenceAndLocalStartupCleanup(t *testing.T) {
	a, s, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := arbitrum.LoadAttestedRootStore(path, "arb", 101)
	if err != nil {
		t.Fatal(err)
	}
	a.store = store
	ctx := context.Background()
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before := a.store.ProvisionalAssertions()
	reloaded, err := arbitrum.LoadAttestedRootStore(path, "arb", 101)
	if err != nil {
		t.Fatal(err)
	}
	a.store = reloaded
	if !reloaded.HasUnfinalizedAssertions() {
		t.Fatal("persisted speculative root not detected")
	}
	s.headErr = errors.New("head unsupported")
	callsBefore := len(s.heads) + len(s.ranges) + len(s.statusBlocks) + len(s.statusHashes)
	if err := a.ReconcileStartup(ctx); err != nil {
		t.Fatalf("startup depended on unavailable RPC: %v", err)
	}
	if got := len(s.heads) + len(s.ranges) + len(s.statusBlocks) + len(s.statusHashes); got != callsBefore {
		t.Fatal("startup made RPC calls")
	}
	cleaned, err := arbitrum.LoadAttestedRootStore(path, "arb", 101)
	if err != nil || cleaned.HasUnfinalizedAssertions() || cleaned.NextL1Block() != 101 {
		t.Fatalf("startup cleanup was not durable: %v", err)
	}
	s.headErr = nil
	s.events = nil
	if err := a.ReconcileStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if reloaded.HasUnfinalizedAssertions() {
		t.Fatal("restart retained orphaned root")
	}
	s.events = []arbitrum.ProposedAssertion{earlyProposal(1, 110)}
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	} // Force durable rename failure.
	s.events = []arbitrum.ProposedAssertion{earlyProposal(2, 110)}
	if err := a.SyncOnce(ctx); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !reflect.DeepEqual(before, a.store.ProvisionalAssertions()) {
		t.Fatal("published speculative state despite persistence failure")
	}
}

func TestFinalizedModeDefaultsAndPurgesSpeculativeState(t *testing.T) {
	for _, mode := range []arbitrum.L1IngestHead{"", arbitrum.L1IngestFinalized} {
		t.Run(string(mode), func(t *testing.T) {
			a, s, _ := newHeadTestLoop(t, mode)
			if err := a.SyncOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(s.heads) != 0 || len(s.statusHashes) != 0 {
				t.Fatal("default queried speculative views")
			}
			if _, found := a.store.HighestAttested(true); found {
				t.Fatal("default ingested unfinalized event")
			}
		})
	}
	a, _, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	if err := a.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.config.L1IngestHead = arbitrum.L1IngestFinalized
	if err := a.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.store.HasUnfinalizedAssertions() {
		t.Fatal("switching to finalized kept speculative roots")
	}
}

func TestSpeculativeCommitPreservesConcurrentDerivedWrites(t *testing.T) {
	a, _, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := uint64(1); n <= 20; n++ {
			err := a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
				return true, store.AppendDerived(arbitrum.BlockCommitment{BlockNumber: n, BlockHash: blockHash(n), StateRoot: blockHash(n + 1000)}, time.Unix(1000, 0), true)
			})
			if err != nil {
				errs <- err
				return
			}
		}
	}()
	for n := 0; n < 5; n++ {
		if err := a.SyncOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	for n := uint64(1); n <= 20; n++ {
		root, found := a.store.HighestAttestedAtOrBelow(n, true)
		if !found || root.Source != arbitrum.SourceDerived || root.L2BlockNumber != n {
			t.Fatalf("lost derived root %d", n)
		}
	}
}

func TestSyncFailureCannotHideUncheckedRootBehindAdvancedCursor(t *testing.T) {
	a, s, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := arbitrum.LoadAttestedRootStore(path, "arb", 101)
	if err != nil {
		t.Fatal(err)
	}
	a.store = store
	ctx := context.Background()
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	s.finalizedBlock = 110
	s.events = nil // The assertion disappeared before this block finalized.
	s.statusNumberErr = errors.New("finalized status unavailable")
	if err := a.SyncOnce(ctx); err == nil {
		t.Fatal("sync accepted failed finalized recheck")
	}
	reloaded, err := arbitrum.LoadAttestedRootStore(path, "arb", 101)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.NextL1Block() != 101 || !reloaded.HasUnfinalizedAssertions() {
		t.Fatal("failed startup hid unchecked root behind cursor")
	}
	a.store = reloaded
	s.statusNumberErr = nil
	if err := a.ReconcileStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found := a.store.HighestAttested(true); found {
		t.Fatal("orphan survived authoritative finalized check")
	}
	if a.store.NextL1Block() != 101 {
		t.Fatal("startup cleanup advanced the cursor")
	}
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if a.store.NextL1Block() != 111 {
		t.Fatal("background finalized cursor did not resume")
	}
}

func TestEarlyProposalNotReadyIsPersistedAndDroppedAtStartup(t *testing.T) {
	a, s, r := newHeadTestLoop(t, arbitrum.L1IngestSafe)
	r.err = arbitrum.ErrCommitmentNotReady
	ctx := context.Background()
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.store.Proposals()) != 1 || !a.store.HasUnfinalizedAssertions() {
		t.Fatal("unresolved speculative proposal not retained")
	}
	s.events = nil
	if err := a.ReconcileStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.store.Proposals()) != 0 {
		t.Fatal("orphaned unresolved proposal survived startup")
	}
}

func TestRejectedSpeculativeAssertionIsWithdrawnAndNeverPromoted(t *testing.T) {
	a, _, r := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	ctx := context.Background()
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	r.err = arbitrum.ErrCommitmentMismatch
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found := a.store.HighestAttested(true); found {
		t.Fatal("Nitro-mismatched speculative assertion retained")
	}
	if len(a.store.Mismatches()) != 1 || len(a.store.Proposals()) != 0 {
		t.Fatal("missing terminal mismatch record")
	}
}

func TestSpeculativeSnapshotCannotDemoteExistingFinalizedAssertion(t *testing.T) {
	a, s, r := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	proposal := s.events[0]
	proposal.L1BlockNumber = 90
	proposal.Confirmed = true
	commitment, err := r.ResolveCanonicalBlockHash(context.Background(), proposal.L2BlockHash, arbitrum.RunModeSafe)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.RecordAssertionAttestation(proposal, commitment, time.Unix(1000, 0), false); err != nil {
		t.Fatal(err)
	}
	s.events = append(s.events, earlyProposal(2, 109))
	for pass := 0; pass < 2; pass++ {
		if err := a.SyncOnce(context.Background()); err != nil {
			t.Fatalf("speculative replay stalled ingestion: %v", err)
		}
	}
	if len(a.store.ProvisionalAssertions()) != 1 || a.store.ProvisionalAssertions()[0].AssertionHash != s.events[1].AssertionHash {
		t.Fatal("replay prevented unrelated assertion ingestion")
	}
	root, found := a.store.HighestAttested(false)
	if !found || root.Provisional || root.L1BlockNumber != 90 {
		t.Fatalf("finalized assertion changed: %+v", root)
	}
}

func metricValue(metric prometheus.Metric) float64 {
	var out dto.Metric
	if err := metric.Write(&out); err != nil {
		panic(err)
	}
	if out.Gauge != nil {
		return out.GetGauge().GetValue()
	}
	return out.GetCounter().GetValue()
}

func TestCanonicalRPCDoesNotBlockDerivedCommit(t *testing.T) {
	a, s, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.canonicalCheck = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	syncDone := make(chan error, 1)
	go func() { syncDone <- a.SyncOnce(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("canonical RPC was not reached")
	}
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- a.store.Commit(func(store *arbitrum.AttestedRootStore) (bool, error) {
			return true, store.AppendDerived(arbitrum.BlockCommitment{BlockNumber: 1, BlockHash: blockHash(1), StateRoot: blockHash(2)}, time.Unix(1000, 0), true)
		})
	}()
	select {
	case err := <-commitDone:
		close(release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		<-syncDone
		<-commitDone
		t.Fatal("canonical RPC held the save lock")
	}
	if err := <-syncDone; err != nil {
		t.Fatal(err)
	}
	if _, found := a.store.DerivedAt(1); !found {
		t.Fatal("speculative commit lost concurrent derived root")
	}
}

func TestFinalizedReconciliationRunsOncePerAssertionPerPass(t *testing.T) {
	for _, initiallyReady := range []bool{false, true} {
		t.Run(fmt.Sprint(initiallyReady), func(t *testing.T) {
			a, s, r := newHeadTestLoop(t, arbitrum.L1IngestLatest)
			if !initiallyReady {
				r.err = arbitrum.ErrCommitmentNotReady
			}
			ctx := context.Background()
			if err := a.SyncOnce(ctx); err != nil {
				t.Fatal(err)
			}
			r.err = nil
			s.finalizedBlock = 110
			s.statusBlocks = nil
			r.modes = nil
			if err := a.SyncOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if len(s.statusBlocks) != 1 || len(r.modes) != 1 {
				t.Fatalf("duplicate reconciliation: status=%v Nitro=%v", s.statusBlocks, r.modes)
			}
			s.confirmed = true
			r.finalized = true
			if err := a.SyncOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if _, found := a.store.HighestAttested(false); !found {
				t.Fatal("deduplication prevented later promotion")
			}
		})
	}
}

func TestStartupPersistenceFailureRetainsUncheckedState(t *testing.T) {
	a, _, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := arbitrum.LoadAttestedRootStore(path, "arb", 101)
	if err != nil {
		t.Fatal(err)
	}
	a.store = store
	if err := a.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.ReconcileStartup(context.Background()); err == nil {
		t.Fatal("startup accepted non-durable cleanup")
	}
	if !a.store.HasUnfinalizedAssertions() || a.store.NextL1Block() != 101 {
		t.Fatal("failed cleanup published state or advanced cursor")
	}
}

func TestStartupCleanupPreservesFinalizedEvidenceAndDerivedRoots(t *testing.T) {
	a, s, r := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	ctx := context.Background()
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, block := range []uint64{90, 100} {
		proposal := earlyProposal(int64(block), block)
		commitment, err := r.ResolveCanonicalBlockHash(ctx, proposal.L2BlockHash, arbitrum.RunModeSafe)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.store.RecordAssertionAttestation(proposal, commitment, time.Unix(1000, 0), block == 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.AppendDerived(arbitrum.BlockCommitment{BlockNumber: 600, BlockHash: blockHash(600), StateRoot: blockHash(601)}, time.Unix(1000, 0), true); err != nil {
		t.Fatal(err)
	}
	s.headErr = errors.New("RPC unavailable")
	r.err = errors.New("Nitro unavailable")
	if err := a.ReconcileStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.store.IsFinalizedAssertion(earlyProposal(90, 90).AssertionHash) {
		t.Fatal("startup removed finalized root")
	}
	if roots := a.store.ProvisionalAssertions(); len(roots) != 1 || roots[0].L1BlockNumber != 100 {
		t.Fatalf("startup removed provisional root with finalized L1 evidence: %v", roots)
	}
	if _, found := a.store.DerivedAt(600); !found {
		t.Fatal("startup removed derived root")
	}
	if a.store.NextL1Block() != 101 {
		t.Fatal("startup changed cursor")
	}
}

func TestFinalizedReconciliationKeepsRepostedCreationBlock(t *testing.T) {
	a, s, _ := newHeadTestLoop(t, arbitrum.L1IngestLatest)
	ctx := context.Background()
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// The same assertion is reposted before the old creation block finalizes.
	s.events[0].L1BlockNumber = 113
	s.finalizedBlock, s.head = 115, 115
	if err := a.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	root, found := a.store.HighestAttested(true)
	if !found || root.L1BlockNumber != 113 || len(s.statusBlocks) != 1 {
		t.Fatalf("reposted root=%+v status reads=%v", root, s.statusBlocks)
	}
}

// One finalized-L1 read from a lagging backend (behind a load balancer, or just
// after a failover) must not cost the feed the assertions already ingested
// between that read and the cursor. Before the guard it did: both reconcile
// passes skipped them, the prune then dropped them as unfinalized, and since
// the cursor does not move back and the speculative window starts at
// max(finalized+1, cursor), they were never read again.
func TestSyncOnceRefusesAFinalizedReadBehindTheCursor(t *testing.T) {
	// The tracked assertion is either already attested provisionally, or still
	// a bare proposal because Nitro has not reached its block yet. The prune
	// took both.
	for _, variant := range []struct {
		name     string
		nitroLag bool
	}{
		{"attested provisionally", false},
		{"pending on Nitro", true},
	} {
		for _, mode := range []arbitrum.L1IngestHead{arbitrum.L1IngestFinalized, arbitrum.L1IngestSafe, arbitrum.L1IngestLatest} {
			t.Run(variant.name+"/"+string(mode), func(t *testing.T) {
				a, s, r := newHeadTestLoop(t, mode)
				if variant.nitroLag {
					r.err = arbitrum.ErrCommitmentNotReady
				}
				ctx := context.Background()
				if err := a.SyncOnce(ctx); err != nil {
					t.Fatal(err)
				}

				// L1 finalizes past the assertion's block: it is ingested from the
				// finalized logs and the cursor moves past it.
				s.finalizedBlock, s.head = 130, 135
				if err := a.SyncOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if got := a.store.NextL1Block(); got != 131 {
					t.Fatalf("cursor after the finalized pass = %d, want 131", got)
				}
				provisional, proposals := a.store.ProvisionalAssertions(), a.store.Proposals()
				if len(provisional)+len(proposals) != 1 {
					t.Fatalf("setup: %d provisional entries and %d proposals, want exactly one tracked assertion", len(provisional), len(proposals))
				}

				// One read from a backend that is behind.
				s.finalizedBlock = 105
				err := a.SyncOnce(ctx)
				if err == nil || !strings.Contains(err.Error(), "behind the assertion cursor") {
					t.Fatalf("SyncOnce on a regressed finalized read = %v, want the behind-the-cursor error", err)
				}
				if !reflect.DeepEqual(provisional, a.store.ProvisionalAssertions()) ||
					!reflect.DeepEqual(proposals, a.store.Proposals()) ||
					a.store.NextL1Block() != 131 {
					t.Fatal("the refused pass changed the feed or the cursor")
				}

				// L1 answers correctly again. The assertion is still tracked and
				// reaches the finalized frontier.
				s.finalizedBlock, s.head = 140, 140
				s.confirmed = true
				r.err, r.finalized = nil, true
				if err := a.SyncOnce(ctx); err != nil {
					t.Fatal(err)
				}
				root, found := a.store.HighestAttested(false)
				if !found || root.Provisional || root.AssertionHash != s.events[0].AssertionHash {
					t.Fatalf("finalized frontier after recovery = (%+v, found=%t), want the assertion", root, found)
				}
			})
		}
	}
}

// The guard is exactly "behind the cursor": a read at cursor-1 is the normal
// steady state, and a store that has not scanned anything yet has no cursor to
// be behind.
func TestSyncOnceFinalizedReadGuardBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cursor    uint64
		finalized uint64
		wantErr   bool
	}{
		{"at cursor-1 (nothing new)", 101, 100, false},
		{"past the cursor", 101, 120, false},
		{"one block behind cursor-1", 101, 99, true},
		{"far behind", 101, 5, true},
		{"fresh store at genesis", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s, _ := newHeadTestLoop(t, arbitrum.L1IngestFinalized)
			a.store.SetNextL1Block(tc.cursor)
			s.finalizedBlock = tc.finalized
			s.events = nil
			err := a.SyncOnce(context.Background())
			if tc.wantErr != (err != nil) {
				t.Fatalf("SyncOnce(cursor=%d, finalized=%d) error = %v, want error: %t", tc.cursor, tc.finalized, err, tc.wantErr)
			}
		})
	}
}

// parkingSource parks the finalized-block read, from its parkFromCall-th call
// on, until the call's context ends. It stands in for an L1 WebSocket peer that
// keeps answering pings and drops one response.
type parkingSource struct {
	headTestSource
	calls atomic.Int32
	// parkFromCall is the 1-based call from which reads park; 0 parks none.
	parkFromCall int32
}

func (p *parkingSource) FinalizedBlockNumber(ctx context.Context) (uint64, error) {
	call := p.calls.Add(1)
	if p.parkFromCall != 0 && call >= p.parkFromCall {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return p.finalizedBlock, p.finalizedErr
}

func newParkingLoop(t *testing.T, parkFromCall int32, config AssertionAttestorConfig) (*AssertionAttestor, *parkingSource) {
	t.Helper()
	store, err := arbitrum.NewAttestedRootStore("arb", 101)
	if err != nil {
		t.Fatal(err)
	}
	source := &parkingSource{parkFromCall: parkFromCall}
	source.finalizedBlock = 100
	a, err := NewAssertionAttestor(source, &headTestResolver{}, store, config)
	if err != nil {
		t.Fatal(err)
	}
	return a, source
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func stopRun(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// A call that never returns used to park the single feed goroutine forever: no
// error, no log, no metric. The call bound turns it into an ordinary failed pass
// that the observer sees, and names the call that was cut.
func TestHungL1CallIsCutAndReported(t *testing.T) {
	a, _ := newParkingLoop(t, 1, AssertionAttestorConfig{PollInterval: time.Hour, CallTimeout: 50 * time.Millisecond})
	observed := make(chan error, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.RunWithObserver(ctx, func(err error) { observed <- err })
	}()
	defer stopRun(t, cancel, done)

	select {
	case err := <-observed:
		if err == nil || !strings.Contains(err.Error(), "L1 FinalizedBlockNumber exceeded its 50ms deadline") ||
			!errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("observed pass error = %v, want one naming the call and its 50ms deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hung call was never reported: the loop is parked")
	}
}

// A call cut in the middle of the log scan fails the pass, and the chunks that
// were already committed stay committed: the next pass resumes from the cursor.
func TestCallDeadlineKeepsTheChunksAlreadyCommitted(t *testing.T) {
	a, s, _ := newHeadTestLoop(t, arbitrum.L1IngestFinalized)
	a.config.CallTimeout = 50 * time.Millisecond
	s.finalizedBlock = 120
	s.events = nil
	blocking := &chunkBlockingSource{headTestSource: s, blockFrom: 106}
	a.source = blocking

	err := a.SyncOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "L1 Assertions exceeded its 50ms deadline") {
		t.Fatalf("pass with a parked chunk = %v, want the call deadline", err)
	}
	if got := a.store.NextL1Block(); got != 106 {
		t.Fatalf("cursor after the cut pass = %d, want 106: the first chunk (101-105) was committed", got)
	}

	blocking.blockFrom = 0
	if err := a.SyncOnce(context.Background()); err != nil {
		t.Fatalf("pass after the park cleared: %v", err)
	}
	if got := a.store.NextL1Block(); got != 121 {
		t.Fatalf("cursor after the resumed pass = %d, want 121", got)
	}
}

// chunkBlockingSource parks every log read from blockFrom upward.
type chunkBlockingSource struct {
	*headTestSource
	blockFrom uint64
}

func (c *chunkBlockingSource) Assertions(ctx context.Context, from, to uint64) ([]arbitrum.ProposedAssertion, []ConfirmedAssertion, error) {
	if c.blockFrom != 0 && from >= c.blockFrom {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return c.headTestSource.Assertions(ctx, from, to)
}

// hookedSource runs a hook inside each AssertionStatus call, before the status
// is read: the place a slow, hung or clock-advancing endpoint is stood in for.
type hookedSource struct {
	*headTestSource
	hook  func(ctx context.Context, call int) error
	calls atomic.Int32
}

func (h *hookedSource) AssertionStatus(ctx context.Context, p arbitrum.ProposedAssertion, n uint64) (uint8, error) {
	call := int(h.calls.Add(1))
	if h.hook != nil {
		if err := h.hook(ctx, call); err != nil {
			return 0, err
		}
	}
	return h.headTestSource.AssertionStatus(ctx, p, n)
}

// newPendingList builds a loop whose store holds pending assertions that exist
// on finalized L1 but that Nitro has not reached, so they stay in the lists
// reconciliation walks from the first entry on every pass. The one at
// revokedIndex no longer exists on L1 (revokedIndex < 0 for none): reconciling
// the list removes it. The L1 log scan still has 100 blocks to cover.
func newPendingList(t *testing.T, pending, revokedIndex int) (*AssertionAttestor, *hookedSource) {
	t.Helper()
	a, s, r := newHeadTestLoop(t, arbitrum.L1IngestFinalized)
	r.err = arbitrum.ErrCommitmentNotReady
	s.finalizedBlock = 200
	s.events = nil
	for i := 0; i < pending; i++ {
		p := earlyProposal(int64(100+i), uint64(101+i))
		if i != revokedIndex {
			s.events = append(s.events, p)
		}
		if err := a.store.RecordProposal(p); err != nil {
			t.Fatal(err)
		}
	}
	hooked := &hookedSource{headTestSource: s}
	a.source = hooked
	return a, hooked
}

// runFirstPass runs the loop's first pass the way production does, through
// RunWithObserver, and returns its outcome. A deadline on the pass, if one were
// ever added to the loop, would apply here and not to a direct SyncOnce.
func runFirstPass(t *testing.T, a *AssertionAttestor) error {
	t.Helper()
	a.config.PollInterval = time.Hour // only the immediate first pass runs
	observed := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.RunWithObserver(ctx, func(err error) { observed <- err })
	}()
	defer stopRun(t, cancel, done)
	select {
	case err := <-observed:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the loop's first pass never finished")
		return nil
	}
}

// The case a whole-pass deadline got wrong. Reconciliation walks the pending
// entries from the first on every pass, and an entry that is still pending stays
// in the list, so no progress is kept between passes. With a deadline on the
// pass, a list that takes longer than the deadline to walk was cut in the same
// prefix every time: the entries behind it were never confirmed or revoked, and
// the log scan that comes after the reconciliation never ran. Each call here is
// well inside its bound; only their sum is longer than one call's bound.
func TestALongSlowReconcileIsNotCutAndReachesTheLogScan(t *testing.T) {
	const pending = 30
	a, source := newPendingList(t, pending, pending-1)
	a.config.CallTimeout = 100 * time.Millisecond
	source.hook = func(ctx context.Context, _ int) error {
		select {
		case <-time.After(20 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	started := time.Now()
	if err := runFirstPass(t, a); err != nil {
		t.Fatalf("the first pass over a slow but healthy endpoint: %v", err)
	}
	if took := time.Since(started); took <= a.config.CallTimeout {
		t.Fatalf("the pass took %s, no longer than one call's bound %s: the test proves nothing", took, a.config.CallTimeout)
	}
	if got := len(a.store.Proposals()); got != pending-1 {
		t.Fatalf("%d proposals tracked, want %d: the revoked entry at the end of the list was not reached", got, pending-1)
	}
	if got := a.store.NextL1Block(); got != 201 {
		t.Fatalf("cursor = %d, want 201: the log scan after the reconciliation did not run", got)
	}
}

// A call that hangs part-way down the list fails the pass at that entry, like
// any other failing call. What was reconciled before it stays reconciled, and
// the cursor does not move, because the entries behind the failure were not
// checked.
func TestAHungCallMidListFailsThePassAtThatEntry(t *testing.T) {
	const pending = 30
	a, source := newPendingList(t, pending, 1)
	a.config.CallTimeout = 50 * time.Millisecond
	source.hook = func(ctx context.Context, call int) error {
		if call == 4 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}

	err := runFirstPass(t, a)
	if err == nil || !strings.Contains(err.Error(), "L1 AssertionStatus exceeded its 50ms deadline") {
		t.Fatalf("the first pass with a hung call = %v, want the call deadline", err)
	}
	if got := len(a.store.Proposals()); got != pending-1 {
		t.Fatalf("%d proposals tracked, want %d: the entry revoked before the hang was kept", got, pending-1)
	}
	if got := a.store.NextL1Block(); got != 101 {
		t.Fatalf("cursor = %d, want 101: a pass that failed mid-list must not advance past unchecked entries", got)
	}

	source.hook = nil
	if err := a.SyncOnce(context.Background()); err != nil {
		t.Fatalf("pass after the hang cleared: %v", err)
	}
	if got := a.store.NextL1Block(); got != 201 {
		t.Fatalf("cursor after the next pass = %d, want 201", got)
	}
}

// testClock is a clock the test advances by hand; it is read by the run loop's
// goroutine and the test's, so it is atomic.
type testClock struct{ ns atomic.Int64 }

func newTestClock(at time.Time) *testClock {
	c := &testClock{}
	c.ns.Store(at.UnixNano())
	return c
}

func (c *testClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *testClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// RunnerAlive is what tells a parked loop from a healthy one: the Nitro
// snapshot keeps looking fresh over a frozen assertion feed, so health that
// read only that stayed SERVING.
func TestRunnerAliveTellsAParkedLoopFromAHealthyOne(t *testing.T) {
	// The second pass parks in its first call. A long call bound keeps it parked
	// for the whole test, so what is measured is the heartbeat and not the bound.
	a, source := newParkingLoop(t, 2, AssertionAttestorConfig{PollInterval: 10 * time.Millisecond, CallTimeout: time.Hour})
	clock := newTestClock(time.Unix(5000, 0))
	a.now = clock.now
	staleAfter := time.Hour + 20*time.Millisecond

	if a.RunnerAlive() {
		t.Fatal("a loop that never started is alive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	defer stopRun(t, cancel, done)

	// The second pass starts only after the first has finished and the loop
	// has beaten, so a parked second pass proves the loop was alive until then.
	waitUntil(t, "the second pass to park", func() bool { return source.calls.Load() >= 2 })
	if !a.RunnerAlive() {
		t.Fatal("a loop that completed a pass and is inside the next one reads as dead")
	}

	clock.advance(30 * time.Minute)
	if !a.RunnerAlive() {
		t.Fatal("a call that has run for less than the call bound read as a dead loop")
	}
	clock.advance(staleAfter)
	if a.RunnerAlive() {
		t.Fatal("a loop parked beyond the call bound and two ticks still reads as alive")
	}
}

// A pass can legitimately run far longer than the staleness window: the lists it
// walks have no bound. It stays alive because the runner beats as each call
// returns, not only when the loop comes back around.
func TestRunnerStaysAliveThroughALongPassOfSlowCalls(t *testing.T) {
	const pending = 10
	a, source := newPendingList(t, pending, -1)
	clock := newTestClock(time.Unix(5000, 0))
	a.now = clock.now
	a.config.CallTimeout = time.Hour
	a.config.PollInterval = 10 * time.Millisecond
	staleAfter := attestortypes.FeedStaleAfter(a.config.CallTimeout, a.config.PollInterval)

	const perCall = 40 * time.Minute // each call "takes" this long on the test clock
	var sampled []bool
	source.hook = func(context.Context, int) error {
		clock.advance(perCall)
		sampled = append(sampled, a.RunnerAlive()) // while the call is in flight
		return nil
	}

	a.heartbeat.Beat(a.now()) // the loop has started
	startedAt := clock.now()
	if err := a.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if span := clock.now().Sub(startedAt); span <= 2*staleAfter {
		t.Fatalf("the pass spanned %s on the test clock, not longer than twice the %s window: the test proves nothing", span, staleAfter)
	}
	if len(sampled) < pending {
		t.Fatalf("%d calls sampled, want at least %d", len(sampled), pending)
	}
	for call, alive := range sampled {
		if !alive {
			t.Fatalf("the loop read as dead during call %d of a pass whose calls each returned inside the window", call+1)
		}
	}
	if !a.RunnerAlive() {
		t.Fatal("the loop reads as dead after a pass whose last call just returned")
	}
}

// A pass that fails is not a parked loop: it is logged and counted, but the
// loop keeps coming back around.
func TestRunnerStaysAliveWhilePassesFail(t *testing.T) {
	a, source := newParkingLoop(t, 0, AssertionAttestorConfig{PollInterval: 10 * time.Millisecond, CallTimeout: time.Second})
	source.finalizedErr = errors.New("L1 unavailable")
	clock := newTestClock(time.Unix(5000, 0))
	a.now = clock.now
	failures := make(chan struct{}, 64)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.RunWithObserver(ctx, func(err error) {
			if err != nil {
				select {
				case failures <- struct{}{}:
				default:
				}
			}
		})
	}()
	defer stopRun(t, cancel, done)

	<-failures
	clock.advance(time.Minute) // far past staleAfter (about 1s): only a new beat revives it
	waitUntil(t, "the failing loop to beat again", a.RunnerAlive)
}

// The pass failure line is where a transport error is logged at run time, and
// net/http quotes the full endpoint URL in it; an RPC provider's API key is in
// that URL.
func TestPassFailureLogDoesNotQuoteTheEndpointKey(t *testing.T) {
	var output syncBuffer
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a, source := newParkingLoop(t, 0, AssertionAttestorConfig{PollInterval: time.Hour, CallTimeout: time.Second})
	source.finalizedErr = fmt.Errorf("finalized block: %w", &url.Error{Op: "Post", URL: "http://127.0.0.1:8545/rpc?apikey=SECRET-KEY", Err: io.EOF})
	observed := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.RunWithObserver(ctx, func(err error) { observed <- err })
	}()
	defer stopRun(t, cancel, done)

	if err := <-observed; err == nil {
		t.Fatal("the pass did not fail")
	}
	line := output.String()
	if strings.Contains(line, "SECRET-KEY") || strings.Contains(line, "apikey") {
		t.Fatalf("log output %q carries the endpoint key", line)
	}
	if !strings.Contains(line, `Post "http://127.0.0.1:8545": EOF`) {
		t.Fatalf("log output %q should still name the host and the failure", line)
	}
}

// syncBuffer is a bytes.Buffer safe for the log package's writer and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
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

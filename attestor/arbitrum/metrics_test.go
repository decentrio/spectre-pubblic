package arbitrum

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsExposeBoundedOperatorSignals(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	metrics.ObserveRuntime("arbitrum-one", RuntimeSnapshot{
		Unsafe:    BlockCommitment{BlockNumber: 101},
		Safe:      BlockCommitment{BlockNumber: 100},
		Finalized: BlockCommitment{BlockNumber: 99},
	}, []FinalizedConsistency{{
		UnsafeObserved: true,
		UnsafeMatches:  false,
		FinalizedReorg: true,
	}})
	metrics.ObserveStore("arbitrum-one", AttestedRootStoreStats{
		NextL1Block:       500,
		PendingAssertions: 2,
		ProvisionalRoots:  3,
		MismatchRecords:   4,
	})
	metrics.ObserveRuntimeFailure("arbitrum-one")
	metrics.ObserveAssertionFailure("arbitrum-one")
	metrics.ObserveDerivedFailure("arbitrum-one")
	metrics.ObservePersistenceFailure("arbitrum-one")

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	got := make(map[string]bool, len(families))
	for _, family := range families {
		got[family.GetName()] = true
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() != "chain" {
					t.Fatalf("metric %s has unexpected label %q", family.GetName(), label.GetName())
				}
			}
		}
	}
	for _, name := range []string{
		"spectre_arbitrum_attestor_nitro_unsafe_head",
		"spectre_arbitrum_attestor_assertions_pending",
		"spectre_arbitrum_attestor_runtime_refresh_failures_total",
		"spectre_arbitrum_attestor_finalized_divergence_total",
		"spectre_arbitrum_attestor_persistence_failures_total",
	} {
		if !got[name] {
			t.Fatalf("missing metric family %s", name)
		}
	}
}

func TestAttestedRootStoreStatsCountsPendingAndProvisionalWork(t *testing.T) {
	store, err := NewAttestedRootStore("arbitrum-one", 50)
	if err != nil {
		t.Fatalf("NewAttestedRootStore: %v", err)
	}
	proposal := testStoreProposal(1, 100)
	if err := store.RecordProposal(proposal); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	if err := store.RecordAssertionAttestation(proposal, testStoreCommitment(100), now, true); err != nil {
		t.Fatalf("RecordAssertionAttestation: %v", err)
	}
	if err := store.AppendDerived(testStoreCommitment(110), now, true); err != nil {
		t.Fatalf("AppendDerived: %v", err)
	}
	store.RecordAssertionMismatch(testStoreProposal(2, 120).AssertionHash, testStoreCommitment(120).BlockHash, "test", now)

	stats := store.Stats()
	if stats.NextL1Block != 50 || stats.PendingAssertions != 1 || stats.ProvisionalRoots != 2 || stats.MismatchRecords != 1 {
		t.Fatalf("Stats = %+v", stats)
	}
}

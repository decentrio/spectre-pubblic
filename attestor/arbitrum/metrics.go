package arbitrum

import "github.com/prometheus/client_golang/prometheus"

const (
	arbitrumMetricsNamespace = "spectre"
	arbitrumMetricsSubsystem = "arbitrum_attestor"
)

// AttestedRootStoreStats is a bounded, hash-free view of the durable feed for
// operator metrics. It intentionally exposes counts and cursors only: block
// and assertion hashes belong in structured logs, not Prometheus labels.
type AttestedRootStoreStats struct {
	NextL1Block       uint64
	PendingAssertions uint64
	ProvisionalRoots  uint64
	MismatchRecords   uint64
}

// Metrics contains the Arbitrum attestor's bounded-cardinality operator
// signals. A single configured source chain owns every vector instance.
type Metrics struct {
	NitroUnsafeHead      *prometheus.GaugeVec
	NitroSafeHead        *prometheus.GaugeVec
	NitroFinalizedHead   *prometheus.GaugeVec
	AssertionCursor      *prometheus.GaugeVec
	AssertionIngestHead  *prometheus.GaugeVec
	AssertionRevocations *prometheus.CounterVec
	PendingAssertions    *prometheus.GaugeVec
	ProvisionalRoots     *prometheus.GaugeVec
	MismatchRecords      *prometheus.GaugeVec
	RuntimeFailures      *prometheus.CounterVec
	AssertionFailures    *prometheus.CounterVec
	DerivedFailures      *prometheus.CounterVec
	PersistenceFailures  *prometheus.CounterVec
	FinalizedDivergence  *prometheus.CounterVec
	FinalizedReorg       *prometheus.CounterVec
}

// NewMetrics registers the metrics used by the Arbitrum attestor. All metrics
// use only the configured source-chain label, which has deployment-bounded
// cardinality.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	gauge := func(name, help string) *prometheus.GaugeVec {
		metric := prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: arbitrumMetricsNamespace,
			Subsystem: arbitrumMetricsSubsystem,
			Name:      name,
			Help:      help,
		}, []string{"chain"})
		reg.MustRegister(metric)
		return metric
	}
	counter := func(name, help string) *prometheus.CounterVec {
		metric := prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: arbitrumMetricsNamespace,
			Subsystem: arbitrumMetricsSubsystem,
			Name:      name,
			Help:      help,
		}, []string{"chain"})
		reg.MustRegister(metric)
		return metric
	}
	return &Metrics{
		NitroUnsafeHead:      gauge("nitro_unsafe_head", "Latest observed Nitro unsafe L2 height."),
		NitroSafeHead:        gauge("nitro_safe_head", "Latest observed Nitro safe L2 height."),
		NitroFinalizedHead:   gauge("nitro_finalized_head", "Latest observed Nitro finalized L2 height."),
		AssertionCursor:      gauge("assertion_next_l1_block", "Next finalized L1 block whose RollupCore logs must be scanned."),
		AssertionIngestHead:  gauge("assertion_ingest_head", "L1 block of the last successfully committed assertion ingest snapshot."),
		AssertionRevocations: counter("assertion_revocations_total", "Assertion proposals or roots withdrawn before L1 finality."),
		PendingAssertions:    gauge("assertions_pending", "Assertion proposals or provisional assertion roots awaiting an authoritative verdict."),
		ProvisionalRoots:     gauge("roots_provisional", "Attested roots awaiting finalized recheck."),
		MismatchRecords:      gauge("mismatch_records", "Retained terminal assertion mismatch records."),
		RuntimeFailures:      counter("runtime_refresh_failures_total", "Failed Nitro runtime refreshes."),
		AssertionFailures:    counter("assertion_sync_failures_total", "Failed assertion ingestion or reconciliation passes."),
		DerivedFailures:      counter("derived_sync_failures_total", "Failed derived-root reconciliation passes."),
		PersistenceFailures:  counter("persistence_failures_total", "Failed durable attested-root state saves."),
		FinalizedDivergence:  counter("finalized_divergence_total", "Finalized checks that contradicted earlier unsafe or safe observations."),
		FinalizedReorg:       counter("finalized_reorg_total", "Observed finalized-head regressions or root changes."),
	}
}

// ObserveRuntime publishes current Nitro heads and records finality evidence
// contradictions emitted by a successful refresh.
func (m *Metrics) ObserveRuntime(chain string, snapshot RuntimeSnapshot, checks []FinalizedConsistency) {
	if m == nil {
		return
	}
	m.NitroUnsafeHead.WithLabelValues(chain).Set(float64(snapshot.Unsafe.BlockNumber))
	m.NitroSafeHead.WithLabelValues(chain).Set(float64(snapshot.Safe.BlockNumber))
	m.NitroFinalizedHead.WithLabelValues(chain).Set(float64(snapshot.Finalized.BlockNumber))
	for _, check := range checks {
		if check.FinalizedReorg {
			m.FinalizedReorg.WithLabelValues(chain).Inc()
		}
		if (check.UnsafeObserved && !check.UnsafeMatches) || (check.SafeObserved && !check.SafeMatches) {
			m.FinalizedDivergence.WithLabelValues(chain).Inc()
		}
	}
}

// ObserveStore publishes the bounded state needed to distinguish L1 backlog,
// provisional work, and a recorded divergence.
func (m *Metrics) ObserveStore(chain string, stats AttestedRootStoreStats) {
	if m == nil {
		return
	}
	m.AssertionCursor.WithLabelValues(chain).Set(float64(stats.NextL1Block))
	m.PendingAssertions.WithLabelValues(chain).Set(float64(stats.PendingAssertions))
	m.ProvisionalRoots.WithLabelValues(chain).Set(float64(stats.ProvisionalRoots))
	m.MismatchRecords.WithLabelValues(chain).Set(float64(stats.MismatchRecords))
}

func (m *Metrics) ObserveRuntimeFailure(chain string) {
	if m != nil {
		m.RuntimeFailures.WithLabelValues(chain).Inc()
	}
}

func (m *Metrics) ObserveAssertionFailure(chain string) {
	if m != nil {
		m.AssertionFailures.WithLabelValues(chain).Inc()
	}
}

func (m *Metrics) ObserveDerivedFailure(chain string) {
	if m != nil {
		m.DerivedFailures.WithLabelValues(chain).Inc()
	}
}

func (m *Metrics) ObservePersistenceFailure(chain string) {
	if m != nil {
		m.PersistenceFailures.WithLabelValues(chain).Inc()
	}
}

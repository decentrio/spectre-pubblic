package opstack

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricsNamespace = "spectre"
	metricsSubsystem = "op_attestor"
)

// Metrics holds the attestor metric vectors, labeled by source chain so one
// registry serves every configured op_source. A nil *Metrics disables
// recording (unit tests).
type Metrics struct {
	GamesIngested        *prometheus.CounterVec
	RootsMatched         *prometheus.CounterVec
	RootsMismatched      *prometheus.CounterVec
	RootsDerived         *prometheus.CounterVec
	RootsProvisional     *prometheus.CounterVec
	HeadDivergence       *prometheus.CounterVec
	ReplicaErrors        *prometheus.CounterVec
	PassFailures         *prometheus.CounterVec
	AttestationLag       *prometheus.GaugeVec
	ReplicaFinalizedHead *prometheus.GaugeVec
	ReplicaSafeHead      *prometheus.GaugeVec
	ReplicaUnsafeHead    *prometheus.GaugeVec
	PendingGames         *prometheus.GaugeVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	counter := func(name, help string, extraLabels ...string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace, Subsystem: metricsSubsystem, Name: name, Help: help,
		}, append([]string{"chain"}, extraLabels...))
		reg.MustRegister(v)
		return v
	}
	gauge := func(name, help string) *prometheus.GaugeVec {
		v := prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace, Subsystem: metricsSubsystem, Name: name, Help: help,
		}, []string{"chain"})
		reg.MustRegister(v)
		return v
	}
	return &Metrics{
		GamesIngested:        counter("games_ingested_total", "Dispute games ingested from the factory (all game types)."),
		RootsMatched:         counter("roots_matched_total", "Proposed output roots confirmed by honest derivation."),
		RootsMismatched:      counter("roots_mismatched_total", "Proposed output roots contradicted by honest derivation."),
		RootsDerived:         counter("roots_derived_total", "Self-derived output roots attested at the configured head."),
		RootsProvisional:     counter("roots_provisional_total", "Verdicts recorded provisionally (below the finalized head), pending finalized recheck."),
		HeadDivergence:       counter("head_divergence_total", "Finalized rechecks that contradicted a provisional verdict (sequencer divergence at unsafe, L1 reorg at safe)."),
		ReplicaErrors:        counter("replica_errors_total", "Replica interaction failures.", "kind"),
		PassFailures:         counter("pass_failures_total", "Attestation passes that failed, including those that hit the call timeout."),
		AttestationLag:       gauge("attestation_lag_blocks", "Oldest pending proposal's L2 block minus the gating head (0 when nothing pending is ahead)."),
		ReplicaFinalizedHead: gauge("replica_finalized_head", "Replica finalized L2 head (derived from finalized L1 — tx finality)."),
		ReplicaSafeHead:      gauge("replica_safe_head", "Replica safe L2 head (derived from L1)."),
		ReplicaUnsafeHead:    gauge("replica_unsafe_head", "Replica unsafe L2 head (sequencer gossip)."),
		PendingGames:         gauge("pending_games", "Ingested respected-type games awaiting a verdict."),
	}
}

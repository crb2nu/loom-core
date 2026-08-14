package gates

import (
	"github.com/crb2nu/loom/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	GateMetricSpecConformance = "spec_conformance"
	GateMetricScope           = "scope"
	GateMetricPRSelfReview    = "pr_self_review"
	GateMetricNonemptyDiff    = "nonempty_diff"
	GateMetricBranchPushed    = "branch_pushed"
)

// GateFlakeMetrics exports pass/fail totals and verdict transitions for the
// deterministic gates whose inputs have stable digests. Metric labels use a
// closed vocabulary, while the digest is retained only as process-local state
// and is never exported as a label.
type GateFlakeMetrics struct {
	EvaluationsTotal  *prometheus.CounterVec
	VerdictFlipsTotal *prometheus.CounterVec

	transitions telemetry.VerdictTransitionTracker
}

// NewGateFlakeMetrics constructs and optionally registers the gate metrics.
// A nil registerer is useful in tests.
func NewGateFlakeMetrics(reg prometheus.Registerer) *GateFlakeMetrics {
	m := &GateFlakeMetrics{
		EvaluationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loom_mills_deterministic_gate_evaluations_total",
			Help: "Deterministic Mills gate evaluations partitioned by gate and pass/fail verdict.",
		}, []string{"gate", "verdict"}),
		VerdictFlipsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loom_mills_deterministic_gate_verdict_flips_total",
			Help: "Verdict transitions for repeated deterministic Mills gate inputs.",
		}, []string{"gate", "from_verdict", "to_verdict"}),
	}
	if reg != nil {
		reg.MustRegister(m.EvaluationsTotal, m.VerdictFlipsTotal)
	}
	return m
}

// RecordGateEvaluation implements telemetry.GateEvaluationSink. Unsupported
// gates and non-binary outcomes are intentionally ignored: they cannot create
// metric series or grow prior-verdict state.
func (m *GateFlakeMetrics) RecordGateEvaluation(e telemetry.GateEvaluation) {
	if m == nil || !isFlakeTelemetryGate(e.GateID) || !isBinaryGateVerdict(e.Verdict) {
		return
	}
	m.EvaluationsTotal.WithLabelValues(e.GateID, string(e.Verdict)).Inc()
	identity := e.GateID + "\x00" + e.InputDigest
	previous, flipped := m.transitions.Record(identity, e.Verdict)
	if flipped {
		m.VerdictFlipsTotal.WithLabelValues(e.GateID, string(previous), string(e.Verdict)).Inc()
	}
}

func isFlakeTelemetryGate(gateID string) bool {
	switch gateID {
	case GateMetricSpecConformance, GateMetricScope, GateMetricPRSelfReview, GateMetricNonemptyDiff, GateMetricBranchPushed:
		return true
	default:
		return false
	}
}

func isBinaryGateVerdict(verdict telemetry.GateVerdict) bool {
	return verdict == telemetry.GateVerdictPass || verdict == telemetry.GateVerdictFail
}

var _ telemetry.GateEvaluationSink = (*GateFlakeMetrics)(nil)

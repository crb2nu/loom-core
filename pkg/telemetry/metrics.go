package telemetry

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// EscalationClassExternalDependency is the bounded KPI/Prometheus label for
	// failures carrying deterministic external-dependency classifier metadata.
	EscalationClassExternalDependency             = "external_dependency"
	RetryIncidentClassExternalDependency          = "external_dependency_incident"
	RetryIncidentClassUnknown                     = "unknown"
	RetryDispositionWaitForRecovery               = "wait_for_dependency_recovery"
	ClassificationClassExternalDependencyIncident = "external_dependency_incident"
	ClassificationClassRepositoryRegression       = "repository_regression"
	ClassificationClassUnknown                    = "unknown"

	gateLabelDocsGuardrail = "docs_guardrail"
	gateLabelScope         = "scope"
	gateLabelOther         = "other"
	verdictLabelUnknown    = "unknown"
)

var (
	defaultGateMetricsOnce           sync.Once
	defaultGateMetrics               *GateMetrics
	defaultTerminalStateMetricsOnce  sync.Once
	defaultTerminalStateMetrics      *TerminalStateMetrics
	defaultClassificationMetricsOnce sync.Once
	defaultClassificationMetrics     *ClassificationMetrics
	defaultCrossRepoStampMetricsOnce sync.Once
	defaultCrossRepoStampMetrics     *CrossRepoStampDeliveryMetrics
	defaultOverseerSoakMetricsOnce   sync.Once
	defaultOverseerSoakMetrics       *OverseerSoakMetrics
)

const terminalStateLabelUnknown = "unknown"

const (
	CrossRepoStampDeliverySuccess = "success"
	CrossRepoStampDeliveryDenial  = "denial"
	CrossRepoStampDeliveryFailure = "failure"
)

// OverseerSoakMetrics observes persisted S2 dry-run decisions. Both labels are
// booleans rendered by this package, limiting the metric to four possible
// series and preventing decision subjects or policy details from becoming
// labels.
type OverseerSoakMetrics struct {
	DecisionsTotal *prometheus.CounterVec
}

// NewOverseerSoakMetrics constructs and optionally registers the overseer S2
// soak counter. Passing an isolated registry keeps unit tests independent.
func NewOverseerSoakMetrics(reg prometheus.Registerer) *OverseerSoakMetrics {
	m := &OverseerSoakMetrics{
		DecisionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: OverseerDryRunDecisionsMetric,
			Help: "Persisted overseer S2 dry-run decisions partitioned by whether they would act and diverge from approved policy.",
		}, []string{"would_have_acted", "diverged"}),
	}
	if reg != nil {
		if err := reg.Register(m.DecisionsTotal); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
					m.DecisionsTotal = existing
				}
			} else {
				panic(err)
			}
		}
	}
	return m
}

// DefaultOverseerSoakMetrics returns the process-wide overseer soak sink.
func DefaultOverseerSoakMetrics() *OverseerSoakMetrics {
	defaultOverseerSoakMetricsOnce.Do(func() {
		defaultOverseerSoakMetrics = NewOverseerSoakMetrics(prometheus.DefaultRegisterer)
	})
	return defaultOverseerSoakMetrics
}

// RecordDryRunDecision increments one of the counter's four bounded series.
func (m *OverseerSoakMetrics) RecordDryRunDecision(wouldHaveActed, diverged bool) {
	if m == nil || m.DecisionsTotal == nil {
		return
	}
	m.DecisionsTotal.WithLabelValues(
		boolMetricLabel(wouldHaveActed), boolMetricLabel(diverged),
	).Inc()
}

func boolMetricLabel(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// CrossRepoStampDeliveryMetrics observes the result of the fail-closed stamp
// delivery boundary. Outcome is the only label and is constrained to three
// values so project identities and errors can never become metric labels.
type CrossRepoStampDeliveryMetrics struct {
	DeliveriesTotal *prometheus.CounterVec
}

// NewCrossRepoStampDeliveryMetrics constructs and optionally registers stamp
// delivery metrics. Passing an isolated registry keeps unit tests independent.
func NewCrossRepoStampDeliveryMetrics(reg prometheus.Registerer) *CrossRepoStampDeliveryMetrics {
	m := &CrossRepoStampDeliveryMetrics{
		DeliveriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mills_crossrepo_stamp_deliveries_total",
			Help: "Cross-repository stamp delivery attempts partitioned by bounded outcome.",
		}, []string{"outcome"}),
	}
	if reg != nil {
		if err := reg.Register(m.DeliveriesTotal); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
					m.DeliveriesTotal = existing
				}
			} else {
				panic(err)
			}
		}
	}
	return m
}

// DefaultCrossRepoStampDeliveryMetrics returns the process-wide delivery sink.
func DefaultCrossRepoStampDeliveryMetrics() *CrossRepoStampDeliveryMetrics {
	defaultCrossRepoStampMetricsOnce.Do(func() {
		defaultCrossRepoStampMetrics = NewCrossRepoStampDeliveryMetrics(prometheus.DefaultRegisterer)
	})
	return defaultCrossRepoStampMetrics
}

// RecordCrossRepoStampDelivery increments one bounded outcome series. Unknown
// input is conservatively reported as failure rather than creating a series.
func (m *CrossRepoStampDeliveryMetrics) RecordCrossRepoStampDelivery(outcome string) {
	if m == nil || m.DeliveriesTotal == nil {
		return
	}
	switch outcome {
	case CrossRepoStampDeliverySuccess, CrossRepoStampDeliveryDenial, CrossRepoStampDeliveryFailure:
	default:
		outcome = CrossRepoStampDeliveryFailure
	}
	m.DeliveriesTotal.WithLabelValues(outcome).Inc()
}

// ClassificationMetrics observes the normalized class produced by Mills
// failure classifiers. Class is constrained to the policy taxonomy so
// arbitrary classifier output cannot create unbounded Prometheus series.
type ClassificationMetrics struct {
	ClassificationsTotal *prometheus.CounterVec
}

// NewClassificationMetrics constructs classification metrics and optionally
// registers them. A nil registerer is useful for classifier unit tests.
func NewClassificationMetrics(reg prometheus.Registerer) *ClassificationMetrics {
	m := &ClassificationMetrics{
		ClassificationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loom_mills_incident_classifications_total",
			Help: "Mills incident classifications produced, partitioned by normalized class.",
		}, []string{"class"}),
	}
	if reg != nil {
		if err := reg.Register(m.ClassificationsTotal); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
					m.ClassificationsTotal = existing
				}
			} else {
				panic(err)
			}
		}
	}
	return m
}

// DefaultClassificationMetrics returns the process-wide classification sink.
func DefaultClassificationMetrics() *ClassificationMetrics {
	defaultClassificationMetricsOnce.Do(func() {
		defaultClassificationMetrics = NewClassificationMetrics(prometheus.DefaultRegisterer)
	})
	return defaultClassificationMetrics
}

// RecordClassification increments the counter after bounding its class label.
func (m *ClassificationMetrics) RecordClassification(class string) {
	if m == nil || m.ClassificationsTotal == nil {
		return
	}
	switch class {
	case ClassificationClassExternalDependencyIncident, ClassificationClassRepositoryRegression:
	default:
		class = ClassificationClassUnknown
	}
	m.ClassificationsTotal.WithLabelValues(class).Inc()
}

// TerminalStateMetrics observes rejected attempts to replace a durable Mills
// pipeline outcome. requested_state is normalized to a closed label set.
type TerminalStateMetrics struct {
	ConflictsTotal *prometheus.CounterVec
}

// NewTerminalStateMetrics constructs the terminal-state conflict metric and
// optionally registers it.
func NewTerminalStateMetrics(reg prometheus.Registerer) *TerminalStateMetrics {
	m := &TerminalStateMetrics{
		ConflictsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loom_mills_terminal_state_conflicts_total",
			Help: "Rejected writes to an already resolved Mills pipeline terminal state.",
		}, []string{"requested_state"}),
	}
	if reg != nil {
		if err := reg.Register(m.ConflictsTotal); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
					m.ConflictsTotal = existing
				}
			} else {
				panic(err)
			}
		}
	}
	return m
}

// DefaultTerminalStateMetrics returns the process-wide terminal conflict sink.
func DefaultTerminalStateMetrics() *TerminalStateMetrics {
	defaultTerminalStateMetricsOnce.Do(func() {
		defaultTerminalStateMetrics = NewTerminalStateMetrics(prometheus.DefaultRegisterer)
	})
	return defaultTerminalStateMetrics
}

// RecordTerminalStateConflict increments the counter after bounding its label.
func (m *TerminalStateMetrics) RecordTerminalStateConflict(requestedState string) {
	if m == nil || m.ConflictsTotal == nil {
		return
	}
	switch requestedState {
	case "done", "escalated":
	default:
		requestedState = terminalStateLabelUnknown
	}
	m.ConflictsTotal.WithLabelValues(requestedState).Inc()
}

// RetryMetrics is the bounded-label Prometheus surface for retry guardrails.
type RetryMetrics struct {
	CapRefusalsTotal *prometheus.CounterVec
}

// NewRetryMetrics constructs retry guardrail metrics and optionally registers
// them. A nil registerer is useful for policy unit tests.
func NewRetryMetrics(reg prometheus.Registerer) *RetryMetrics {
	m := &RetryMetrics{
		CapRefusalsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loom_mills_retry_cap_refusals_total",
			Help: "Paid pipeline retries refused by the retry-exhaustion guardrail.",
		}, []string{"incident_class", "disposition"}),
	}
	if reg != nil {
		if err := reg.Register(m.CapRefusalsTotal); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
					m.CapRefusalsTotal = existing
				}
			} else {
				panic(err)
			}
		}
	}
	return m
}

// RecordRetryCapRefusal increments the guardrail counter after normalizing
// caller input to the metric's closed, low-cardinality label set.
func (m *RetryMetrics) RecordRetryCapRefusal(incidentClass, disposition string) {
	if m == nil || m.CapRefusalsTotal == nil {
		return
	}
	if incidentClass != RetryIncidentClassExternalDependency {
		incidentClass = RetryIncidentClassUnknown
	}
	if disposition != RetryDispositionWaitForRecovery {
		disposition = RetryDispositionWaitForRecovery
	}
	m.CapRefusalsTotal.WithLabelValues(incidentClass, disposition).Inc()
}

// GateMetrics observes deterministic gate evaluations and counts verdict
// transitions for the same gate and semantic input. Its labels deliberately
// exclude run IDs and input digests, and unknown gate/verdict values collapse
// to fixed fallback buckets.
type GateMetrics struct {
	VerdictFlipsTotal *prometheus.CounterVec

	mu   sync.Mutex
	last map[string]GateVerdict
}

// NewGateMetrics constructs the deterministic-gate metric sink.
func NewGateMetrics(reg prometheus.Registerer) *GateMetrics {
	m := &GateMetrics{
		VerdictFlipsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loom_mills_gate_verdict_flips_total",
			Help: "Verdict transitions for repeated deterministic gate inputs.",
		}, []string{"gate", "from_verdict", "to_verdict"}),
		last: make(map[string]GateVerdict),
	}
	if reg != nil {
		if err := reg.Register(m.VerdictFlipsTotal); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
					m.VerdictFlipsTotal = existing
				}
			} else {
				panic(err)
			}
		}
	}
	return m
}

// DefaultGateMetrics returns the process-wide sink registered with the default
// Prometheus registry. Sharing one sink preserves prior verdicts across gate
// registry instances and pipeline runs.
func DefaultGateMetrics() *GateMetrics {
	defaultGateMetricsOnce.Do(func() {
		defaultGateMetrics = NewGateMetrics(prometheus.DefaultRegisterer)
	})
	return defaultGateMetrics
}

// RecordGateEvaluation implements GateEvaluationSink.
func (m *GateMetrics) RecordGateEvaluation(e GateEvaluation) {
	if m == nil || m.VerdictFlipsTotal == nil {
		return
	}
	gate := boundedGateLabel(e.GateID)
	verdict := boundedVerdictLabel(e.Verdict)
	key := e.GateID + "\x00" + e.InputDigest

	m.mu.Lock()
	previous, exists := m.last[key]
	m.last[key] = GateVerdict(verdict)
	m.mu.Unlock()
	if exists && string(previous) != verdict {
		m.VerdictFlipsTotal.WithLabelValues(
			gate, boundedVerdictLabel(previous), verdict,
		).Inc()
	}
}

func boundedGateLabel(gate string) string {
	switch gate {
	case gateLabelDocsGuardrail, gateLabelScope:
		return gate
	default:
		return gateLabelOther
	}
}

func boundedVerdictLabel(verdict GateVerdict) string {
	switch verdict {
	case GateVerdictPass, GateVerdictFail, GateVerdictSkip:
		return string(verdict)
	default:
		return verdictLabelUnknown
	}
}

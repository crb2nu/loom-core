package telemetry

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// RetryCostUSDField and AutoRequeuesField preserve the durable Mills field
	// names while FailureClassField adds bounded classifier attribution.
	RetryCostUSDField = "retry_cost_usd"
	AutoRequeuesField = "auto_requeues"
	FailureClassField = "failure_class"

	EmbedderRequestFailuresMetric    = "embedder_request_failures_total"
	EmbedderLatencyMetric            = "embedder_latency_seconds"
	IntakeFailclosedRejectionsMetric = "intake_failclosed_rejections_total"
	GateVerdictParseMetric           = "gate_verdict_parse_total"
	EscalationRequeueEligibleMetric  = "mills_escalation_requeue_eligible_total"
	EscalationRequeueBlockedMetric   = "mills_escalation_requeue_blocked_total"
	OverseerDryRunDecisionsMetric    = "loom_mills_overseer_dry_run_decisions_total"

	EmbedderProviderMorph     = "morph"
	EmbedderProviderFlexInfer = "flexinfer"
	EmbedderProviderOllama    = "ollama"
	EmbedderProviderUnknown   = "unknown"
)

// OverseerDryRunDecisionRecorder observes successfully persisted overseer
// dry-run decisions. The two booleans are a closed vocabulary: the dry-run
// verdict (would act or would not act) and whether it diverged from production.
type OverseerDryRunDecisionRecorder interface {
	RecordOverseerDryRunDecision(context.Context, bool, bool)
}

var overseerDryRunDecisionRecorder = struct {
	sync.RWMutex
	recorder OverseerDryRunDecisionRecorder
}{recorder: prometheusOverseerDryRunDecisionRecorder{}}

// RecordOverseerDryRunDecision increments the persisted-decision counter.
// Callers must invoke it only after the corresponding decision is durable.
func RecordOverseerDryRunDecision(ctx context.Context, wouldHaveActed, diverged bool) {
	overseerDryRunDecisionRecorder.RLock()
	recorder := overseerDryRunDecisionRecorder.recorder
	overseerDryRunDecisionRecorder.RUnlock()
	if recorder != nil {
		recorder.RecordOverseerDryRunDecision(ctx, wouldHaveActed, diverged)
	}
}

// SetOverseerDryRunDecisionRecorderForTest replaces the process-wide recorder
// and returns a function that restores it.
func SetOverseerDryRunDecisionRecorderForTest(recorder OverseerDryRunDecisionRecorder) func() {
	overseerDryRunDecisionRecorder.Lock()
	previous := overseerDryRunDecisionRecorder.recorder
	overseerDryRunDecisionRecorder.recorder = recorder
	overseerDryRunDecisionRecorder.Unlock()
	return func() {
		overseerDryRunDecisionRecorder.Lock()
		overseerDryRunDecisionRecorder.recorder = previous
		overseerDryRunDecisionRecorder.Unlock()
	}
}

type prometheusOverseerDryRunDecisionRecorder struct{}

func (prometheusOverseerDryRunDecisionRecorder) RecordOverseerDryRunDecision(_ context.Context, wouldHaveActed, diverged bool) {
	DefaultOverseerSoakMetrics().RecordDryRunDecision(wouldHaveActed, diverged)
}

const (
	EscalationRequeueClassTransient      = "transient"
	EscalationRequeueClassTransientQuota = "transient_quota"
	EscalationRequeueClassInfrastructure = "infrastructure"
	EscalationRequeueClassUnknown        = "unknown"

	EscalationRequeueBlockClassification    = "classification"
	EscalationRequeueBlockBudgetUnavailable = "budget_unavailable"
	EscalationRequeueBlockBudgetExhausted   = "budget_exhausted"
	EscalationRequeueBlockPersistence       = "persistence"
	EscalationRequeueBlockUnknown           = "unknown"
)

// EscalationRequeueRecorder observes bounded escalation routing decisions.
// Implementations receive only normalized class and reason values.
type EscalationRequeueRecorder interface {
	RecordEscalationRequeueEligible(context.Context, string)
	RecordEscalationRequeueBlocked(context.Context, string)
}

var escalationRequeueRecorder = struct {
	sync.RWMutex
	recorder EscalationRequeueRecorder
}{recorder: newOTelEscalationRequeueRecorder()}

func RecordEscalationRequeueEligible(ctx context.Context, class string) {
	escalationRequeueRecorder.RLock()
	recorder := escalationRequeueRecorder.recorder
	escalationRequeueRecorder.RUnlock()
	if recorder != nil {
		recorder.RecordEscalationRequeueEligible(ctx, normalizeEscalationRequeueClass(class))
	}
}

func RecordEscalationRequeueBlocked(ctx context.Context, reason string) {
	escalationRequeueRecorder.RLock()
	recorder := escalationRequeueRecorder.recorder
	escalationRequeueRecorder.RUnlock()
	if recorder != nil {
		recorder.RecordEscalationRequeueBlocked(ctx, normalizeEscalationRequeueBlockReason(reason))
	}
}

func SetEscalationRequeueRecorderForTest(recorder EscalationRequeueRecorder) func() {
	escalationRequeueRecorder.Lock()
	previous := escalationRequeueRecorder.recorder
	escalationRequeueRecorder.recorder = recorder
	escalationRequeueRecorder.Unlock()
	return func() {
		escalationRequeueRecorder.Lock()
		escalationRequeueRecorder.recorder = previous
		escalationRequeueRecorder.Unlock()
	}
}

type otelEscalationRequeueRecorder struct {
	eligible metric.Int64Counter
	blocked  metric.Int64Counter
}

func newOTelEscalationRequeueRecorder() *otelEscalationRequeueRecorder {
	meter := otel.GetMeterProvider().Meter("github.com/crb2nu/loom/pkg/telemetry")
	eligible, _ := meter.Int64Counter(EscalationRequeueEligibleMetric,
		metric.WithDescription("Total escalation requeue allowances claimed by bounded failure class."))
	blocked, _ := meter.Int64Counter(EscalationRequeueBlockedMetric,
		metric.WithDescription("Total escalation requeue decisions blocked by bounded reason."))
	return &otelEscalationRequeueRecorder{eligible: eligible, blocked: blocked}
}

func (r *otelEscalationRequeueRecorder) RecordEscalationRequeueEligible(ctx context.Context, class string) {
	r.eligible.Add(ctx, 1, metric.WithAttributes(attribute.String("failure_class", class)))
}

func (r *otelEscalationRequeueRecorder) RecordEscalationRequeueBlocked(ctx context.Context, reason string) {
	r.blocked.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func normalizeEscalationRequeueClass(class string) string {
	switch class {
	case EscalationRequeueClassTransient, EscalationRequeueClassTransientQuota, EscalationRequeueClassInfrastructure:
		return class
	default:
		return EscalationRequeueClassUnknown
	}
}

func normalizeEscalationRequeueBlockReason(reason string) string {
	switch reason {
	case EscalationRequeueBlockClassification, EscalationRequeueBlockBudgetUnavailable,
		EscalationRequeueBlockBudgetExhausted, EscalationRequeueBlockPersistence:
		return reason
	default:
		return EscalationRequeueBlockUnknown
	}
}

const (
	GateVerdictParseOutcomeParsed     = "parsed"
	GateVerdictParseOutcomeParseError = "parse_error"
	GateVerdictParseGateUnknown       = "unknown"
)

// GateVerdictParseRecorder observes whether a supported Mills gate produced a
// structured verdict. Implementations must keep gate and outcome bounded.
type GateVerdictParseRecorder interface {
	RecordGateVerdictParse(ctx context.Context, gate, outcome string)
}

var gateVerdictParseRecorder = struct {
	sync.RWMutex
	recorder GateVerdictParseRecorder
}{recorder: newOTelGateVerdictParseRecorder()}

// RecordGateVerdictParse increments the parse counter using only the three
// hardened gate names and the parsed/parse_error outcome vocabulary.
func RecordGateVerdictParse(ctx context.Context, gate, outcome string) {
	gateVerdictParseRecorder.RLock()
	recorder := gateVerdictParseRecorder.recorder
	gateVerdictParseRecorder.RUnlock()
	if recorder != nil {
		recorder.RecordGateVerdictParse(ctx, normalizeGateVerdictParseGate(gate), normalizeGateVerdictParseOutcome(outcome))
	}
}

// SetGateVerdictParseRecorderForTest replaces the process-wide recorder and
// returns a function that restores it.
func SetGateVerdictParseRecorderForTest(recorder GateVerdictParseRecorder) func() {
	gateVerdictParseRecorder.Lock()
	previous := gateVerdictParseRecorder.recorder
	gateVerdictParseRecorder.recorder = recorder
	gateVerdictParseRecorder.Unlock()
	return func() {
		gateVerdictParseRecorder.Lock()
		gateVerdictParseRecorder.recorder = previous
		gateVerdictParseRecorder.Unlock()
	}
}

type otelGateVerdictParseRecorder struct {
	counter metric.Int64Counter
}

func newOTelGateVerdictParseRecorder() *otelGateVerdictParseRecorder {
	meter := otel.GetMeterProvider().Meter("github.com/crb2nu/loom/pkg/telemetry")
	counter, _ := meter.Int64Counter(
		GateVerdictParseMetric,
		metric.WithDescription("Total Mills gate verdict parse attempts by bounded gate and outcome."),
	)
	return &otelGateVerdictParseRecorder{counter: counter}
}

func (r *otelGateVerdictParseRecorder) RecordGateVerdictParse(ctx context.Context, gate, outcome string) {
	r.counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("gate", gate),
		attribute.String("outcome", outcome),
	))
}

func normalizeGateVerdictParseGate(gate string) string {
	switch gate {
	case "spec_conformance", "pr_self_review", "scope":
		return gate
	default:
		return GateVerdictParseGateUnknown
	}
}

func normalizeGateVerdictParseOutcome(outcome string) string {
	if outcome == GateVerdictParseOutcomeParseError {
		return outcome
	}
	return GateVerdictParseOutcomeParsed
}

// EmbedderRequestRecorder observes one logical embed-client request. Provider
// is normalized at the recording boundary; request data and errors are never
// part of the metric contract.
type EmbedderRequestRecorder interface {
	RecordEmbedderRequest(ctx context.Context, provider string, elapsed time.Duration, failed bool)
}

var embedderRequestRecorder = struct {
	sync.RWMutex
	recorder EmbedderRequestRecorder
}{recorder: newOTelEmbedderRequestRecorder()}

// RecordEmbedderRequest records latency for every request and increments the
// failure counter only when failed is true.
func RecordEmbedderRequest(ctx context.Context, provider string, elapsed time.Duration, failed bool) {
	embedderRequestRecorder.RLock()
	recorder := embedderRequestRecorder.recorder
	embedderRequestRecorder.RUnlock()
	if recorder != nil {
		recorder.RecordEmbedderRequest(ctx, normalizeEmbedderProvider(provider), elapsed, failed)
	}
}

// SetEmbedderRequestRecorderForTest replaces the process-wide recorder and
// returns a function that restores the previous recorder.
func SetEmbedderRequestRecorderForTest(recorder EmbedderRequestRecorder) func() {
	embedderRequestRecorder.Lock()
	previous := embedderRequestRecorder.recorder
	embedderRequestRecorder.recorder = recorder
	embedderRequestRecorder.Unlock()
	return func() {
		embedderRequestRecorder.Lock()
		embedderRequestRecorder.recorder = previous
		embedderRequestRecorder.Unlock()
	}
}

func normalizeEmbedderProvider(provider string) string {
	switch provider {
	case EmbedderProviderMorph, EmbedderProviderFlexInfer, EmbedderProviderOllama:
		return provider
	default:
		return EmbedderProviderUnknown
	}
}

type otelEmbedderRequestRecorder struct {
	failures metric.Int64Counter
	latency  metric.Float64Histogram
}

func newOTelEmbedderRequestRecorder() *otelEmbedderRequestRecorder {
	meter := otel.GetMeterProvider().Meter("github.com/crb2nu/loom/pkg/telemetry")
	return newOTelEmbedderRequestRecorderWithMeter(meter)
}

func newOTelEmbedderRequestRecorderWithMeter(meter metric.Meter) *otelEmbedderRequestRecorder {
	failures, _ := meter.Int64Counter(
		EmbedderRequestFailuresMetric,
		metric.WithDescription("Total failed embedder requests by bounded provider."),
	)
	latency, _ := meter.Float64Histogram(
		EmbedderLatencyMetric,
		metric.WithDescription("Embedder request latency in seconds by bounded provider and outcome."),
		metric.WithUnit("s"),
	)
	return &otelEmbedderRequestRecorder{failures: failures, latency: latency}
}

func (r *otelEmbedderRequestRecorder) RecordEmbedderRequest(ctx context.Context, provider string, elapsed time.Duration, failed bool) {
	outcome := "success"
	if failed {
		outcome = "failure"
	}
	providerAttr := attribute.String("embedder.provider", provider)
	r.latency.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
		providerAttr,
		attribute.String("embedder.outcome", outcome),
	))
	if failed {
		r.failures.Add(ctx, 1, metric.WithAttributes(providerAttr))
	}
}

// RetryAccountingTotals is the existing aggregate retry accounting surface.
type RetryAccountingTotals struct {
	RetryCostUSD float64 `json:"retry_cost_usd"`
	AutoRequeues uint64  `json:"auto_requeues"`
}

// RetryAccountingByClass adds one bounded failure-class dimension. Producers
// must construct Class from the canonical Mills pipeline failure taxonomy;
// raw signatures, dependency names, and error messages are never labels.
type RetryAccountingByClass struct {
	Class        string  `json:"failure_class"`
	RetryCostUSD float64 `json:"retry_cost_usd"`
	AutoRequeues uint64  `json:"auto_requeues"`
}

// RetryAccountingSnapshot keeps aggregate telemetry additive for existing
// consumers while exposing independent attribution for each observed class.
type RetryAccountingSnapshot struct {
	Total   RetryAccountingTotals    `json:"total"`
	ByClass []RetryAccountingByClass `json:"by_class"`
}

var (
	councilIntentsMissingTotal      atomic.Uint64
	intakeFailclosedRejectionsTotal atomic.Uint64
	terminalStateConflictsTotal     atomic.Uint64
	defaultTerminalConflictOnce     sync.Once
	defaultTerminalConflictRecorder *TerminalStateConflictRecorder
)

var intakeFailclosedRejectionsCounter = func() metric.Int64Counter {
	meter := otel.GetMeterProvider().Meter("github.com/crb2nu/loom/pkg/telemetry")
	counter, _ := meter.Int64Counter(
		IntakeFailclosedRejectionsMetric,
		metric.WithDescription("Total cross-repository intake items rejected by the fail-closed repository allowlist."),
	)
	return counter
}()

// RecordIntakeFailclosedRejection records one cross-repository intake item
// rejected before admission or dispatch. The metric has no labels, keeping its
// cardinality fixed and avoiding repository names in telemetry.
func RecordIntakeFailclosedRejection(ctx context.Context) {
	intakeFailclosedRejectionsTotal.Add(1)
	intakeFailclosedRejectionsCounter.Add(ctx, 1)
}

// IntakeFailclosedRejectionsTotal returns the process-local number of
// fail-closed cross-repository intake rejections.
func IntakeFailclosedRejectionsTotal() uint64 {
	return intakeFailclosedRejectionsTotal.Load()
}

// TerminalStateConflictRecorder records rejected attempts to replace one
// durable Mills terminal outcome with another. The process-local counter has
// no labels, so its cardinality is fixed; the Prometheus sink retains its
// existing closed requested-state vocabulary.
type TerminalStateConflictRecorder struct {
	metrics *TerminalStateMetrics
}

// DefaultTerminalStateConflictRecorder returns the process-wide conflict
// recorder used by the Mills store.
func DefaultTerminalStateConflictRecorder() *TerminalStateConflictRecorder {
	defaultTerminalConflictOnce.Do(func() {
		defaultTerminalConflictRecorder = &TerminalStateConflictRecorder{
			metrics: DefaultTerminalStateMetrics(),
		}
	})
	return defaultTerminalConflictRecorder
}

// RecordTerminalStateConflict increments each bounded telemetry surface once
// for one rejected conflicting write.
func (r *TerminalStateConflictRecorder) RecordTerminalStateConflict(requestedState string) {
	if r == nil {
		return
	}
	terminalStateConflictsTotal.Add(1)
	if r.metrics != nil {
		r.metrics.RecordTerminalStateConflict(requestedState)
	}
}

// TerminalStateConflictsTotal returns the process-local number of rejected
// conflicting Mills terminal-state writes.
func TerminalStateConflictsTotal() uint64 {
	return terminalStateConflictsTotal.Load()
}

// RecordCouncilIntentsMissing records a Council preflight that found the
// canonical roadmap-intent store empty. It is safe to call concurrently.
func RecordCouncilIntentsMissing() {
	councilIntentsMissingTotal.Add(1)
}

// CouncilIntentsMissingTotal returns the process-local number of Council
// preflights that found no canonical roadmap intents.
func CouncilIntentsMissingTotal() uint64 {
	return councilIntentsMissingTotal.Load()
}

// GateVerdict is the bounded result vocabulary for a gate evaluation.
type GateVerdict string

const (
	GateVerdictPass GateVerdict = "pass"
	GateVerdictFail GateVerdict = "fail"
	GateVerdictSkip GateVerdict = "skip"
)

// GateFailureCategory is the closed failure taxonomy projected into KPI
// snapshots. Error messages and other unbounded diagnostic text must never be
// used as categories.
type GateFailureCategory string

const (
	GateFailureCategoryFail                GateFailureCategory = "fail"
	GateFailureCategoryUnknownGate         GateFailureCategory = "unknown-gate"
	GateFailureCategoryInfrastructureError GateFailureCategory = "infrastructure-error"
	GateFailureCategoryExternalDependency  GateFailureCategory = "external_dependency"
)

// GateParseStatus is the bounded parsing outcome for a gate evaluation.
type GateParseStatus string

const (
	GateParseStatusParsed     GateParseStatus = "parsed"
	GateParseStatusParseError GateParseStatus = "parse_error"
)

// GateResultEvent is the bounded event emitted once for each docs_guardrail or
// scope evaluation. It intentionally excludes inputs and reasons, which may be
// large or contain repository data.
type GateResultEvent struct {
	GateID      string          `json:"gate_id"`
	Verdict     GateVerdict     `json:"verdict"`
	ParseStatus GateParseStatus `json:"parse_status"`
}

// GateResultEventSink receives structured gate-result events.
type GateResultEventSink interface {
	RecordGateResultEvent(GateResultEvent)
}

// GateResultEventSinkFunc adapts a function to GateResultEventSink.
type GateResultEventSinkFunc func(GateResultEvent)

// RecordGateResultEvent implements GateResultEventSink.
func (f GateResultEventSinkFunc) RecordGateResultEvent(event GateResultEvent) {
	if f != nil {
		f(event)
	}
}

// GateEvaluation is the gate_verdict audit record emitted for one gate run.
// Its essential tuple is {gate_id, input_digest, verdict}: GateID plus
// InputDigest identifies evaluations that must agree even when they belong to
// different pipeline runs. Inputs themselves are never emitted.
type GateEvaluation struct {
	GateID          string              `json:"gate_id"`
	RunID           string              `json:"run_id"`
	InputDigest     string              `json:"input_digest"`
	Verdict         GateVerdict         `json:"verdict"`
	FailureCategory GateFailureCategory `json:"failure_category,omitempty"`
	Reason          string              `json:"reason"`
	DurationMS      int64               `json:"duration_ms"`
}

// GateEvaluationSinkFunc adapts a function to GateEvaluationSink.
type GateEvaluationSinkFunc func(GateEvaluation)

// RecordGateEvaluation implements GateEvaluationSink.
func (f GateEvaluationSinkFunc) RecordGateEvaluation(event GateEvaluation) {
	if f != nil {
		f(event)
	}
}

// GateEvaluationSink receives gate evaluation telemetry.
type GateEvaluationSink interface {
	RecordGateEvaluation(GateEvaluation)
}

// MultiGateEvaluationSink forwards an evaluation to each non-nil sink.
// It lets the default registry retain its existing Prometheus observer while
// also projecting failures into KPI snapshots.
type MultiGateEvaluationSink []GateEvaluationSink

// RecordGateEvaluation implements GateEvaluationSink.
func (sinks MultiGateEvaluationSink) RecordGateEvaluation(event GateEvaluation) {
	for _, sink := range sinks {
		if sink != nil {
			sink.RecordGateEvaluation(event)
		}
	}
}

const gateFailureRecentRunLimit = 8

// GateFailureAggregate is one deterministic per-gate/category KPI row. RunIDs
// are retained as values for diagnosis, never as aggregation keys or metric
// labels. Only the most recent eight are retained to bound memory.
type GateFailureAggregate struct {
	GateID       string              `json:"gate_id"`
	Category     GateFailureCategory `json:"category"`
	Count        uint64              `json:"count"`
	RecentRunIDs []string            `json:"recent_run_ids,omitempty"`
}

// GateFailureKPISnapshot is the additive KPI projection of gate failures.
type GateFailureKPISnapshot struct {
	Failures []GateFailureAggregate `json:"gate_failures"`
}

type gateFailureKey struct {
	gateID   string
	category GateFailureCategory
}

// GateFailureCollector aggregates bounded failure telemetry concurrently.
type GateFailureCollector struct {
	mu       sync.Mutex
	failures map[gateFailureKey]GateFailureAggregate
}

// RecordGateEvaluation implements GateEvaluationSink. Passes and skips are
// intentionally excluded; malformed categories collapse to the appropriate
// bounded fallback based on verdict.
func (c *GateFailureCollector) RecordGateEvaluation(event GateEvaluation) {
	if c == nil || event.Verdict == GateVerdictPass || event.Verdict == GateVerdictSkip {
		return
	}
	category := normalizeGateFailureCategory(event.FailureCategory, event.Verdict)
	key := gateFailureKey{gateID: event.GateID, category: category}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures == nil {
		c.failures = make(map[gateFailureKey]GateFailureAggregate)
	}
	aggregate := c.failures[key]
	aggregate.GateID = event.GateID
	aggregate.Category = category
	aggregate.Count++
	if event.RunID != "" {
		aggregate.RecentRunIDs = append(aggregate.RecentRunIDs, event.RunID)
		if excess := len(aggregate.RecentRunIDs) - gateFailureRecentRunLimit; excess > 0 {
			aggregate.RecentRunIDs = append([]string(nil), aggregate.RecentRunIDs[excess:]...)
		}
	}
	c.failures[key] = aggregate
}

func normalizeGateFailureCategory(category GateFailureCategory, verdict GateVerdict) GateFailureCategory {
	switch category {
	case GateFailureCategoryFail, GateFailureCategoryUnknownGate, GateFailureCategoryInfrastructureError,
		GateFailureCategoryExternalDependency:
		return category
	}
	if verdict == GateVerdictError {
		return GateFailureCategoryInfrastructureError
	}
	return GateFailureCategoryFail
}

// Snapshot returns rows sorted by gate then category for byte-stable KPI JSON.
func (c *GateFailureCollector) Snapshot() GateFailureKPISnapshot {
	if c == nil {
		return GateFailureKPISnapshot{Failures: []GateFailureAggregate{}}
	}
	c.mu.Lock()
	rows := make([]GateFailureAggregate, 0, len(c.failures))
	for _, aggregate := range c.failures {
		aggregate.RecentRunIDs = append([]string(nil), aggregate.RecentRunIDs...)
		rows = append(rows, aggregate)
	}
	c.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].GateID != rows[j].GateID {
			return rows[i].GateID < rows[j].GateID
		}
		return rows[i].Category < rows[j].Category
	})
	return GateFailureKPISnapshot{Failures: rows}
}

var defaultGateFailureCollector GateFailureCollector

// DefaultGateFailureCollector returns the process-wide KPI collector.
func DefaultGateFailureCollector() *GateFailureCollector {
	return &defaultGateFailureCollector
}

// VerdictTransitionTracker remembers the last verdict for each caller-defined
// stable identity. Callers must bound identities before recording them; the
// tracker deliberately knows nothing about metric labels or gate vocabulary.
//
// Record reports the immediately preceding verdict and whether this record
// changed it. Updating the remembered verdict and detecting the transition
// happen under one lock, so concurrent evaluations cannot lose or invent a
// flip.
type VerdictTransitionTracker struct {
	mu   sync.Mutex
	last map[string]GateVerdict
}

// Record stores verdict as the latest value for identity.
func (t *VerdictTransitionTracker) Record(identity string, verdict GateVerdict) (previous GateVerdict, flipped bool) {
	if t == nil {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = make(map[string]GateVerdict)
	}
	previous, exists := t.last[identity]
	t.last[identity] = verdict
	return previous, exists && previous != verdict
}

// GateFlake reports two evaluations of the same deterministic input that
// produced different verdicts.
type GateFlake struct {
	GateID      string      `json:"gate_id"`
	InputDigest string      `json:"input_digest"`
	FirstRunID  string      `json:"first_run_id"`
	First       GateVerdict `json:"first_verdict"`
	RunID       string      `json:"run_id"`
	Verdict     GateVerdict `json:"verdict"`
}

// GateDeterminismHarness is an in-memory telemetry sink suitable for tests,
// canaries, and replay jobs. It is safe for concurrent gate evaluation.
type GateDeterminismHarness struct {
	mu      sync.Mutex
	records []GateEvaluation
	first   map[string]GateEvaluation
	flakes  []GateFlake
}

// RecordGateEvaluation stores an evaluation and flags a flake when an earlier
// verdict exists for the same gate-ID/input-digest pair.
func (h *GateDeterminismHarness) RecordGateEvaluation(e GateEvaluation) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, e)
	if h.first == nil {
		h.first = make(map[string]GateEvaluation)
	}
	key := e.GateID + "\x00" + e.InputDigest
	first, exists := h.first[key]
	if !exists {
		h.first[key] = e
		return
	}
	if first.Verdict != e.Verdict {
		h.flakes = append(h.flakes, GateFlake{
			GateID: e.GateID, InputDigest: e.InputDigest,
			FirstRunID: first.RunID, First: first.Verdict,
			RunID: e.RunID, Verdict: e.Verdict,
		})
	}
}

// Records returns a snapshot of all recorded evaluations.
func (h *GateDeterminismHarness) Records() []GateEvaluation {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]GateEvaluation(nil), h.records...)
}

// Flakes returns a snapshot of all divergent evaluations.
func (h *GateDeterminismHarness) Flakes() []GateFlake {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]GateFlake(nil), h.flakes...)
}

package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	AutonomyReasonKPIUnavailable        = "kpi_unavailable"
	AutonomyReasonKPIStale              = "kpi_stale"
	AutonomyReasonKPIUnhealthy          = "kpi_unhealthy"
	AutonomyReasonInfrastructureRed     = "infrastructure_red"
	defaultBreakerKPIWindowSeconds      = 86400
	defaultBreakerMaxKPIAge             = 2 * time.Hour
	breakerMetricPolicyEnabled          = "policy_enabled"
	breakerMetricActivePipelineRuns     = "active_pipeline_runs"
	breakerMetricQueueDepth             = "queue_depth"
	breakerMetricPipelineMergedReal     = "pipeline_merged_real"
	breakerMetricRegressionRate         = "regression_rate"
	breakerMetricGatePassRate           = "gate_pass_rate"
	infrastructureStatusGreen           = "green"
	infrastructureModeReal              = "real"
	infrastructureModeHealthy           = "healthy"
	infrastructureModeReady             = "ready"
	infrastructureModeOK                = "ok"
	infrastructureModeConfiguredHealthy = "configured_healthy"
)

// BreakerKPIReader is the KPI snapshot read side used by the autonomy circuit
// breaker. It matches *store.KPIDAO.
type BreakerKPIReader interface {
	Latest(ctx context.Context, windowSeconds int) (*store.KPISnapshot, error)
}

// BreakerPolicyProvider is the policy read side used by the autonomy circuit
// breaker. It matches *mills.PolicyManager.
type BreakerPolicyProvider interface {
	Current() *mills.Policy
}

// InfrastructureHealthSource returns deterministic infrastructure readiness
// rows for dependencies that must be healthy before Mills performs autonomous
// writes.
type InfrastructureHealthSource interface {
	Health(ctx context.Context) ([]InfrastructureHealth, error)
}

// InfrastructureHealth is the pipeline-package version of the operator's
// capability matrix row. Required rows must be healthy for autonomy to continue.
type InfrastructureHealth struct {
	ID                  string
	Status              string
	Mode                string
	Message             string
	RequiredForAutonomy bool
	IncidentID          string
	IncidentActive      bool
}

// BreakerOperationalStateInput adapts a breaker verdict and infrastructure
// evidence into the canonical Mills operational state model.
type BreakerOperationalStateInput struct {
	Decision       council.AutonomyGateDecision
	ActiveWork     int
	Infrastructure []InfrastructureHealth
}

// InfrastructureHealthFunc adapts a function into an InfrastructureHealthSource.
type InfrastructureHealthFunc func(ctx context.Context) ([]InfrastructureHealth, error)

func (f InfrastructureHealthFunc) Health(ctx context.Context) ([]InfrastructureHealth, error) {
	if f == nil {
		return nil, nil
	}
	return f(ctx)
}

// BreakerThresholds tunes deterministic KPI policy evaluation. Zero-valued
// numeric thresholds are disabled except WindowSeconds and MaxKPIAge, which
// default to the operator's 1d KPI window and a bounded freshness check.
type BreakerThresholds struct {
	WindowSeconds         int
	MaxKPIAge             time.Duration
	MinRealMerges         int
	MaxRegressionRate     float64
	MinGatePassRate       float64
	MaxActivePipelineRuns int
	MaxQueueDepth         int
}

func (t BreakerThresholds) normalized() BreakerThresholds {
	if t.WindowSeconds <= 0 {
		t.WindowSeconds = defaultBreakerKPIWindowSeconds
	}
	if t.MaxKPIAge <= 0 {
		t.MaxKPIAge = defaultBreakerMaxKPIAge
	}
	return t
}

// BreakerEvaluator evaluates policy, KPI freshness/thresholds, and required
// infrastructure health into the same autonomy decision shape used by the
// council and pipeline gates.
type BreakerEvaluator struct {
	Policy         BreakerPolicyProvider
	KPI            BreakerKPIReader
	Infrastructure InfrastructureHealthSource
	Thresholds     BreakerThresholds
	Clock          func() time.Time
}

func (e *BreakerEvaluator) CheckAutonomy(ctx context.Context) council.AutonomyGateDecision {
	if e == nil {
		return council.AutonomyGateDecision{Allowed: true}
	}
	now := time.Now().UTC()
	if e.Clock != nil {
		now = e.Clock().UTC()
	}
	input := BreakerEvaluationInput{
		Policy:     currentBreakerPolicy(e.Policy),
		KPI:        e.KPI,
		Infra:      e.Infrastructure,
		Thresholds: e.Thresholds,
		Now:        now,
	}
	return EvaluateAutonomyBreaker(ctx, input)
}

// BreakerEvaluationInput is a pure evaluation bundle for tests and adapters.
type BreakerEvaluationInput struct {
	Policy     *mills.Policy
	KPI        BreakerKPIReader
	Infra      InfrastructureHealthSource
	Thresholds BreakerThresholds
	Now        time.Time
}

// EvaluateAutonomyBreaker is deterministic and fail-closed: any configured
// policy, KPI, or required infrastructure problem returns a blocked decision
// with stable reason codes and sorted blockers.
func EvaluateAutonomyBreaker(ctx context.Context, in BreakerEvaluationInput) council.AutonomyGateDecision {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	thresholds := in.Thresholds.normalized()
	var blockers []string
	reason := ""

	if in.Policy == nil || !in.Policy.IsEnabled() {
		blockers = append(blockers, "policy.enabled=false")
		reason = council.AutonomyReasonPolicyDisabled
	}

	snap, kpiReason, kpiBlockers := evaluateBreakerKPI(ctx, in.KPI, thresholds, now)
	if kpiReason != "" && reason == "" {
		reason = kpiReason
	}
	blockers = append(blockers, kpiBlockers...)

	if snap != nil {
		if enabled, ok := boolMetric(snap.Metrics, breakerMetricPolicyEnabled); ok && !enabled {
			blockers = append(blockers, "kpi policy_enabled=false")
			if reason == "" {
				reason = council.AutonomyReasonPolicyDisabled
			}
		}
	}

	infraReason, infraBlockers := evaluateBreakerInfrastructure(ctx, in.Infra)
	if infraReason != "" && reason == "" {
		reason = infraReason
	}
	blockers = append(blockers, infraBlockers...)

	if len(blockers) == 0 {
		return council.AutonomyGateDecision{Allowed: true}
	}
	if reason == "" {
		reason = council.AutonomyReasonBlocked
	}
	sort.Strings(blockers)
	return council.NormalizeAutonomyDecision(council.AutonomyGateDecision{
		Allowed:  false,
		Code:     reason,
		Blockers: blockers,
	})
}

func currentBreakerPolicy(p BreakerPolicyProvider) *mills.Policy {
	if p == nil {
		return nil
	}
	return p.Current()
}

func evaluateBreakerKPI(ctx context.Context, reader BreakerKPIReader, thresholds BreakerThresholds, now time.Time) (*store.KPISnapshot, string, []string) {
	if reader == nil {
		return nil, AutonomyReasonKPIUnavailable, []string{"kpi reader unavailable"}
	}
	snap, err := reader.Latest(ctx, thresholds.WindowSeconds)
	if err != nil {
		return nil, AutonomyReasonKPIUnavailable, []string{fmt.Sprintf("kpi latest unavailable for window=%ds: %v", thresholds.WindowSeconds, err)}
	}
	if snap == nil {
		return nil, AutonomyReasonKPIUnavailable, []string{fmt.Sprintf("kpi latest unavailable for window=%ds", thresholds.WindowSeconds)}
	}
	age := now.Sub(snap.SnapshotAt)
	if age < 0 {
		age = 0
	}
	if age > thresholds.MaxKPIAge {
		return snap, AutonomyReasonKPIStale, []string{fmt.Sprintf("kpi snapshot stale: age=%s max=%s", age.Round(time.Second), thresholds.MaxKPIAge.Round(time.Second))}
	}

	var blockers []string
	if thresholds.MinRealMerges > 0 {
		if v, ok := numberMetric(snap.Metrics, breakerMetricPipelineMergedReal); !ok || int(v) < thresholds.MinRealMerges {
			blockers = append(blockers, fmt.Sprintf("kpi %s below threshold: got=%s min=%d", breakerMetricPipelineMergedReal, metricValueString(snap.Metrics, breakerMetricPipelineMergedReal), thresholds.MinRealMerges))
		}
	}
	if thresholds.MaxRegressionRate > 0 {
		if v, ok := numberMetric(snap.Metrics, breakerMetricRegressionRate); ok && v > thresholds.MaxRegressionRate {
			blockers = append(blockers, fmt.Sprintf("kpi %s above threshold: got=%.4g max=%.4g", breakerMetricRegressionRate, v, thresholds.MaxRegressionRate))
		}
	}
	if thresholds.MinGatePassRate > 0 {
		if v, ok := numberMetric(snap.Metrics, breakerMetricGatePassRate); !ok || v < thresholds.MinGatePassRate {
			blockers = append(blockers, fmt.Sprintf("kpi %s below threshold: got=%s min=%.4g", breakerMetricGatePassRate, metricValueString(snap.Metrics, breakerMetricGatePassRate), thresholds.MinGatePassRate))
		}
	}
	if thresholds.MaxActivePipelineRuns > 0 {
		if v, ok := numberMetric(snap.Metrics, breakerMetricActivePipelineRuns); ok && int(v) > thresholds.MaxActivePipelineRuns {
			blockers = append(blockers, fmt.Sprintf("kpi %s above threshold: got=%d max=%d", breakerMetricActivePipelineRuns, int(v), thresholds.MaxActivePipelineRuns))
		}
	}
	if thresholds.MaxQueueDepth > 0 {
		if v, ok := numberMetric(snap.Metrics, breakerMetricQueueDepth); ok && int(v) > thresholds.MaxQueueDepth {
			blockers = append(blockers, fmt.Sprintf("kpi %s above threshold: got=%d max=%d", breakerMetricQueueDepth, int(v), thresholds.MaxQueueDepth))
		}
	}
	if len(blockers) > 0 {
		return snap, AutonomyReasonKPIUnhealthy, blockers
	}
	return snap, "", nil
}

func evaluateBreakerInfrastructure(ctx context.Context, src InfrastructureHealthSource) (string, []string) {
	if src == nil {
		return "", nil
	}
	rows, err := src.Health(ctx)
	if err != nil {
		return AutonomyReasonInfrastructureRed, []string{"infrastructure health unavailable: " + err.Error()}
	}
	var blockers []string
	for _, row := range rows {
		if !row.RequiredForAutonomy || infrastructureHealthy(row) {
			continue
		}
		id := strings.TrimSpace(row.ID)
		if id == "" {
			id = "unknown"
		}
		msg := strings.TrimSpace(row.Message)
		if msg == "" {
			msg = fmt.Sprintf("status=%s mode=%s", strings.TrimSpace(row.Status), strings.TrimSpace(row.Mode))
		}
		blockers = append(blockers, fmt.Sprintf("%s: %s", id, msg))
	}
	if len(blockers) > 0 {
		return AutonomyReasonInfrastructureRed, blockers
	}
	return "", nil
}

func EvaluateBreakerOperationalState(in BreakerOperationalStateInput) telemetry.MillsOperationalStateReport {
	var degraded []string
	var incidents []telemetry.DependencyIncident
	for _, row := range in.Infrastructure {
		id := strings.TrimSpace(row.ID)
		if id == "" {
			id = "unknown"
		}
		status := strings.ToLower(strings.TrimSpace(row.Status))
		mode := strings.ToLower(strings.TrimSpace(row.Mode))
		if status == "yellow" || status == "degraded" || mode == "degraded" || row.IncidentActive {
			degraded = append(degraded, id)
		}
		if row.IncidentActive {
			incidents = append(incidents, telemetry.DependencyIncident{
				ID:         row.IncidentID,
				Dependency: id,
				Summary:    row.Message,
			})
		}
	}
	return telemetry.EvaluateMillsOperationalState(telemetry.MillsOperationalStateInput{
		ActiveWork:                in.ActiveWork,
		AutonomyAllowed:           in.Decision.Allowed,
		AutonomyBlockers:          in.Decision.Blockers,
		HealthAllowed:             true,
		DegradedDependencies:      degraded,
		ActiveDependencyIncidents: incidents,
	})
}

func infrastructureHealthy(row InfrastructureHealth) bool {
	status := strings.ToLower(strings.TrimSpace(row.Status))
	mode := strings.ToLower(strings.TrimSpace(row.Mode))
	return status == infrastructureStatusGreen &&
		(mode == infrastructureModeReal ||
			mode == infrastructureModeHealthy ||
			mode == infrastructureModeReady ||
			mode == infrastructureModeOK ||
			mode == infrastructureModeConfiguredHealthy)
}

func numberMetric(metrics map[string]any, key string) (float64, bool) {
	if metrics == nil {
		return 0, false
	}
	switch v := metrics[key].(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint64:
		return float64(v), true
	default:
		return 0, false
	}
}

func boolMetric(metrics map[string]any, key string) (bool, bool) {
	if metrics == nil {
		return false, false
	}
	v, ok := metrics[key].(bool)
	return v, ok
}

func metricValueString(metrics map[string]any, key string) string {
	if metrics == nil {
		return "missing"
	}
	if v, ok := metrics[key]; ok {
		return fmt.Sprintf("%v", v)
	}
	return "missing"
}

package gates

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// GateClassification identifies the safety condition that stopped a Mills
// workflow. The vocabulary is deliberately closed so callers can safely use it
// for routing, metrics, and escalation policy.
type GateClassification string

const (
	GateClassificationOK            GateClassification = "ok"
	GateClassificationStorageHealth GateClassification = "storage_health"
	GateClassificationLocalConfig   GateClassification = "local_config"
)

// StorageHealthEvaluator supplies fresh storage dependency evidence. A storage
// evaluator must not return cached evidence without its original timestamps:
// EvaluateHealthSnapshot rejects stale or incomplete evidence fail-closed.
type StorageHealthEvaluator interface {
	EvaluateStorageHealth(ctx context.Context) (HealthSnapshot, error)
}

// LocalConfigPreflight checks configuration required before Mills can safely
// start autonomous work. Safe=false is a deterministic configuration block,
// while a returned error means the check itself could not be completed.
type LocalConfigPreflight interface {
	PreflightLocalConfig(ctx context.Context) (LocalConfigResult, error)
}

// LocalConfigResult is the local configuration check result.
type LocalConfigResult struct {
	Safe    bool
	Reasons []string
}

// GateResult is the classified, fail-closed outcome of the admission gates.
// PipelineClass maps the result onto the pipeline's existing bounded error
// classes without making this low-level package depend on pipeline.
type GateResult struct {
	Allowed        bool
	FailClosed     bool
	Classification GateClassification
	PipelineClass  string
	Reasons        []string
	Health         HealthDecision
}

// HealthDecision adapts the result to the pipeline's existing preflight
// contract. The class marker is intentionally included in Reasons: the runner
// persists it into escalation metadata as [class=infra] or [class=config].
func (r GateResult) HealthDecision() HealthDecision {
	decision := r.Health
	decision.Allowed = r.Allowed
	decision.FailClosed = r.FailClosed
	if r.Allowed {
		decision.Status = "pass"
		return decision
	}
	decision.Status = "block"
	decision.Reasons = append([]string(nil), r.Reasons...)
	if r.PipelineClass != "" {
		decision.Reasons = append([]string{fmt.Sprintf("[class=%s]", r.PipelineClass)}, decision.Reasons...)
	}
	return decision
}

// GateRunner evaluates storage health followed by local configuration. It
// fails closed for missing evaluators, evaluator errors, unsafe results, and
// incomplete health evidence so autonomous progression never relies on an
// assumed-safe dependency or configuration.
type GateRunner struct {
	StorageHealth StorageHealthEvaluator
	LocalConfig   LocalConfigPreflight
	Now           func() time.Time
}

// Run evaluates the admission gates in order. Local config is deliberately
// not evaluated after an unsafe storage result: the workflow is already
// blocked and avoiding extra work keeps the failure evidence unambiguous.
func (r GateRunner) Run(ctx context.Context) GateResult {
	now := func() time.Time {
		if r.Now != nil {
			return r.Now()
		}
		return time.Now().UTC()
	}
	if r.StorageHealth == nil {
		return blockedGateResult(GateClassificationStorageHealth, "infra", "storage health evaluator is not configured")
	}

	snapshot, err := r.StorageHealth.EvaluateStorageHealth(ctx)
	if err != nil {
		return blockedGateResult(GateClassificationStorageHealth, "infra", fmt.Sprintf("storage health evaluation failed: %v", err))
	}
	// Judge the evidence at an instant sampled AFTER it was collected. Reading
	// the clock first makes every real evaluator's timestamps land in the
	// future relative to it, and EvaluateHealthSnapshot treats a negative age
	// as stale — so a live probe would fail closed with "age 0s exceeds 5m0s".
	// Only fakes returning fixed past timestamps survive the other ordering.
	health := EvaluateHealthSnapshot(snapshot, now())
	if !health.Allowed {
		return GateResult{
			Allowed: false, FailClosed: true, Classification: GateClassificationStorageHealth,
			PipelineClass: "infra", Reasons: append([]string(nil), health.Reasons...), Health: health,
		}
	}

	if r.LocalConfig == nil {
		return blockedGateResult(GateClassificationLocalConfig, "config", "local config preflight is not configured")
	}
	config, err := r.LocalConfig.PreflightLocalConfig(ctx)
	if err != nil {
		return blockedGateResult(GateClassificationLocalConfig, "config", fmt.Sprintf("local config preflight failed: %v", err))
	}
	if !config.Safe {
		reasons := normalizedReasons(config.Reasons, "local config preflight reported an unsafe configuration")
		return GateResult{
			Allowed: false, FailClosed: true, Classification: GateClassificationLocalConfig,
			PipelineClass: "config", Reasons: reasons, Health: health,
		}
	}

	return GateResult{Allowed: true, Classification: GateClassificationOK, Health: health}
}

func blockedGateResult(classification GateClassification, pipelineClass, reason string) GateResult {
	return GateResult{
		Allowed: false, FailClosed: true, Classification: classification,
		PipelineClass: pipelineClass, Reasons: []string{reason},
	}
}

func normalizedReasons(reasons []string, fallback string) []string {
	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if reason = strings.TrimSpace(reason); reason != "" {
			out = append(out, reason)
		}
	}
	if len(out) == 0 {
		return []string{fallback}
	}
	return out
}

// Package policy contains small, deterministic policy primitives shared by
// higher-level Loom controllers.
package policy

import (
	"fmt"

	"github.com/crb2nu/loom/internal/loomconcurrency"
)

const (
	// DefaultExternalIncidentThreshold is the maximum number of external
	// dependency incident clusters allowed for one ref in the rolling window
	// before auto-merge is suppressed.
	DefaultExternalIncidentThreshold = 3

	// DefaultCouncilRequireRoadmapIntents fails closed: a council brief marked
	// intents_missing blocks the run unless policy explicitly opts out. An
	// empty canonical intent store means the council would plan against no
	// stated intent at all, which is worse than not planning this tick.
	DefaultCouncilRequireRoadmapIntents = true

	// DefaultPipelineConcurrencyLimit preserves the effective concurrency used
	// before the policy knob existed. Raising it is a separate rollout decision.
	DefaultPipelineConcurrencyLimit = loomconcurrency.DefaultLimit
)

// PipelineConcurrencyPolicy controls concurrency in pipeline supervision. A
// nil limit resolves to the compiled, behavior-neutral default. Explicit
// values, including zero, must pass Validate before being applied.
type PipelineConcurrencyPolicy struct {
	// MaxConcurrentPipelines bounds simultaneous pipeline scheduler work. Nil
	// preserves the compiled conservative default for older policy documents.
	MaxConcurrentPipelines *int `json:"max_concurrent_pipelines,omitempty" yaml:"max_concurrent_pipelines,omitempty"`

	// Limit is the legacy spelling retained while existing callers migrate.
	// New policy documents must use max_concurrent_pipelines.
	Limit *int `json:"concurrency_limit,omitempty" yaml:"concurrency_limit,omitempty"`
}

// EffectiveLimit returns the configured limit or the compiled default.
func (p PipelineConcurrencyPolicy) EffectiveLimit() int {
	if p.MaxConcurrentPipelines != nil {
		return *p.MaxConcurrentPipelines
	}
	if p.Limit == nil {
		return DefaultPipelineConcurrencyLimit
	}
	return *p.Limit
}

// Validate rejects explicit values outside the supported concurrency range.
func (p PipelineConcurrencyPolicy) Validate() error {
	if p.MaxConcurrentPipelines != nil && p.Limit != nil {
		return fmt.Errorf("max_concurrent_pipelines and legacy concurrency_limit cannot both be set")
	}
	limit := p.MaxConcurrentPipelines
	field := "max_concurrent_pipelines"
	if limit == nil {
		limit = p.Limit
		field = "concurrency_limit"
	}
	if limit == nil {
		return nil
	}
	if err := loomconcurrency.Validate(*limit); err != nil {
		return fmt.Errorf("%s %d: %w", field, *limit, err)
	}
	return nil
}

// ExternalIncidentPolicy controls the per-ref external incident auto-merge
// guardrail. A non-positive threshold is treated as unset and uses the
// conservative default.
type ExternalIncidentPolicy struct {
	Threshold int `json:"threshold,omitempty" yaml:"threshold,omitempty"`
}

// ExternalIncidentThreshold returns the configured threshold or its default.
func (p ExternalIncidentPolicy) ExternalIncidentThreshold() int {
	if p.Threshold <= 0 {
		return DefaultExternalIncidentThreshold
	}
	return p.Threshold
}

// CouncilIntentPolicy controls whether an empty canonical roadmap-intent store
// blocks council run scheduling. A nil pointer is treated as unset and uses the
// conservative (fail-closed) default.
type CouncilIntentPolicy struct {
	RequireRoadmapIntents *bool `json:"require_roadmap_intents,omitempty" yaml:"require_roadmap_intents,omitempty"`
}

// RequireRoadmapIntentsEnabled returns the configured value or its default.
func (p CouncilIntentPolicy) RequireRoadmapIntentsEnabled() bool {
	if p.RequireRoadmapIntents == nil {
		return DefaultCouncilRequireRoadmapIntents
	}
	return *p.RequireRoadmapIntents
}

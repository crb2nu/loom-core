// Package policy contains small, deterministic policy primitives shared by
// higher-level Loom controllers.
package policy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/loomconcurrency"
)

const DefaultMainRedExternalHold = 2 * time.Hour
const MaxMainRedExternalHold = 24 * time.Hour

type MainRedExternalHoldPolicy struct {
	HoldMinutes int `json:"hold_minutes,omitempty" yaml:"hold_minutes,omitempty"`
}

func (p MainRedExternalHoldPolicy) Duration() time.Duration {
	if p.HoldMinutes <= 0 {
		return DefaultMainRedExternalHold
	}
	if p.HoldMinutes > int(MaxMainRedExternalHold/time.Minute) {
		return MaxMainRedExternalHold
	}
	return time.Duration(p.HoldMinutes) * time.Minute
}

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

	// MinConcurrency and MaxConcurrency are the inclusive hard bounds accepted
	// by pipeline concurrency policy fields.
	MinConcurrency = loomconcurrency.MinLimit
	MaxConcurrency = loomconcurrency.MaxLimit
)

// StampTargetPolicy is the explicit source-to-target allowlist for stamp
// writes. Same-project writes are always permitted; every cross-project pair
// must be present or authorization fails closed.
type StampTargetPolicy struct {
	AllowedTargets map[string][]string `json:"allowed_targets,omitempty" yaml:"allowed_targets,omitempty"`
}

// Allows reports whether source may write a stamp targeting target. Project
// names are trimmed for comparison, while empty identities are always denied.
func (p StampTargetPolicy) Allows(source, target string) bool {
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	if source == "" || target == "" {
		return false
	}
	if source == target {
		return true
	}
	for configuredSource, targets := range p.AllowedTargets {
		if strings.TrimSpace(configuredSource) != source {
			continue
		}
		for _, allowed := range targets {
			if strings.TrimSpace(allowed) == target {
				return true
			}
		}
	}
	return false
}

// AuthorizeStamp implements the cross-repository stamp authorization contract.
func (p StampTargetPolicy) AuthorizeStamp(_ context.Context, source, target string) error {
	if !p.Allows(source, target) {
		return errors.New("policy: stamp source/target relationship is not allowed")
	}
	return nil
}

// PipelineConcurrencyPolicy controls concurrency in pipeline supervision. A
// nil limit resolves to the compiled, behavior-neutral default. Explicit
// values, including zero, must pass ResolveLimit before being applied.
type PipelineConcurrencyPolicy struct {
	// MaxConcurrency is a compatibility spelling retained for policy documents
	// written during the field-name regression.
	MaxConcurrency *int `json:"max_concurrency,omitempty" yaml:"max_concurrency,omitempty"`

	// MaxConcurrentPipelines bounds simultaneous pipeline scheduler work. This
	// is the canonical field used by the Mills policy and production ConfigMap.
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
	if p.MaxConcurrency != nil {
		return *p.MaxConcurrency
	}
	if p.Limit == nil {
		return DefaultPipelineConcurrencyLimit
	}
	return *p.Limit
}

// ResolveLimit validates and resolves the policy to an effective limit.
func (p PipelineConcurrencyPolicy) ResolveLimit() (int, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	limit := p.EffectiveLimit()
	return loomconcurrency.ResolvePolicyLimit(&limit)
}

// ResolvePipelineConcurrencyLimit validates and resolves the canonical Mills
// policy field. Keeping this defaulting boundary in the policy package lets
// hot-path consumers apply reloaded values without duplicating fallback rules.
func ResolvePipelineConcurrencyLimit(configured *int) (int, error) {
	return (PipelineConcurrencyPolicy{MaxConcurrentPipelines: configured}).ResolveLimit()
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

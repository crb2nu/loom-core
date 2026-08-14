package council

import (
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	DegradedPolicyModeNormal   = "normal"
	DegradedPolicyModeDegraded = "degraded"
	DegradedPolicyModeBlocked  = "blocked"

	DegradedPolicyCodeNormal                 = "normal"
	DegradedPolicyCodeAutonomyBlocked        = "autonomy_blocked"
	DegradedPolicyCodeEmbedderFallbackVector = "embedder_fallback_vector"
	DegradedPolicyCodeEmbedderQueryDegraded  = "embedder_query_degraded"
	DegradedPolicyCodeEmbedderUnavailable    = "embedder_unavailable"
	DegradedPolicyCodeEmbedderDegraded       = "embedder_degraded"
)

// DegradedModePolicy defines how Mills should react when embedding telemetry
// shows a fallback or degraded search path. Zero values default fail-closed for
// query degradation and open breakers, while allowing document fallback vectors.
type DegradedModePolicy struct {
	AllowDocumentFallbackVectors    bool
	BlockOnQueryDegradation         bool
	BlockOnOpenCircuitWithoutVector bool
}

// DefaultDegradedModePolicy returns the production policy used by callers that
// only need the standard embedder fallback semantics.
func DefaultDegradedModePolicy() DegradedModePolicy {
	return DegradedModePolicy{
		AllowDocumentFallbackVectors:    true,
		BlockOnQueryDegradation:         true,
		BlockOnOpenCircuitWithoutVector: true,
	}
}

// DegradedModeSignal is a stable, package-local projection of degraded
// dependency evidence. It intentionally mirrors telemetry.EmbeddingFallbackEvent
// without making pipeline code depend on an OTel sink implementation.
type DegradedModeSignal struct {
	Source         string    `json:"source,omitempty"`
	Path           string    `json:"path,omitempty"`
	Outcome        string    `json:"outcome,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Model          string    `json:"model,omitempty"`
	FallbackVector bool      `json:"fallback_vector,omitempty"`
	ObservedAt     time.Time `json:"observed_at,omitempty"`
	Message        string    `json:"message,omitempty"`
}

// DegradedModePolicyInput is the evidence bundle evaluated by the degraded
// policy. Autonomy may be nil when the caller has no breaker verdict.
type DegradedModePolicyInput struct {
	Policy   DegradedModePolicy
	Autonomy *AutonomyGateDecision
	Signals  []DegradedModeSignal
}

// DegradedModeDecision is the structured verdict consumed by council and
// pipeline paths before autonomous work continues.
type DegradedModeDecision struct {
	Allowed      bool                 `json:"allowed"`
	Mode         string               `json:"mode"`
	Code         string               `json:"code"`
	Reasons      []string             `json:"reasons,omitempty"`
	Blockers     []string             `json:"blockers,omitempty"`
	Signals      []DegradedModeSignal `json:"signals,omitempty"`
	FallbackUsed bool                 `json:"fallback_used,omitempty"`
}

// EvaluateDegradedModePolicy folds autonomy, provider-overload, circuit-breaker,
// and fallback-vector evidence into a single deterministic decision.
func EvaluateDegradedModePolicy(in DegradedModePolicyInput) DegradedModeDecision {
	policy := normalizeDegradedModePolicy(in.Policy)
	if in.Autonomy != nil {
		autonomy := NormalizeAutonomyDecision(*in.Autonomy)
		if !autonomy.Allowed {
			return NormalizeDegradedModeDecision(DegradedModeDecision{
				Allowed:  false,
				Mode:     DegradedPolicyModeBlocked,
				Code:     firstNonEmptyDegradedPolicy(autonomy.Code, DegradedPolicyCodeAutonomyBlocked),
				Reasons:  []string{"autonomy circuit breaker is blocked"},
				Blockers: autonomy.Blockers,
			})
		}
	}

	signals := cleanDegradedSignals(in.Signals)
	if len(signals) == 0 {
		return DegradedModeDecision{Allowed: true, Mode: DegradedPolicyModeNormal, Code: DegradedPolicyCodeNormal}
	}

	var reasons []string
	var blockers []string
	var providerOrCircuit bool
	var queryDegraded bool
	var fallbackVector bool
	var fallbackError bool

	for _, sig := range signals {
		reason := strings.ToLower(strings.TrimSpace(sig.Reason))
		outcome := strings.ToLower(strings.TrimSpace(sig.Outcome))
		path := strings.ToLower(strings.TrimSpace(sig.Path))
		switch reason {
		case telemetry.EmbeddingReasonProviderOverload, telemetry.EmbeddingReasonCircuitOpen, telemetry.EmbeddingReasonFailureThresholdExceeded:
			providerOrCircuit = true
		}
		switch outcome {
		case telemetry.EmbeddingOutcomeShortCircuit, telemetry.EmbeddingOutcomeThresholdOpen:
			providerOrCircuit = true
		case telemetry.EmbeddingOutcomeFallbackError:
			fallbackError = true
		}
		if sig.FallbackVector || (path == telemetry.EmbeddingPathDocuments && outcome == telemetry.EmbeddingOutcomeFallbackSuccess) {
			fallbackVector = true
		}
		if path == telemetry.EmbeddingPathQuery && outcome == telemetry.EmbeddingOutcomeDegraded {
			queryDegraded = true
		}
		reasons = append(reasons, degradedSignalReason(sig))
	}

	if queryDegraded && policy.BlockOnQueryDegradation {
		blockers = append(blockers, "embedding query degraded to keyword search")
		return NormalizeDegradedModeDecision(DegradedModeDecision{
			Allowed: false, Mode: DegradedPolicyModeBlocked, Code: DegradedPolicyCodeEmbedderQueryDegraded,
			Reasons: reasons, Blockers: blockers, Signals: signals, FallbackUsed: fallbackVector,
		})
	}
	if fallbackError {
		blockers = append(blockers, "embedding fallback provider failed")
		return NormalizeDegradedModeDecision(DegradedModeDecision{
			Allowed: false, Mode: DegradedPolicyModeBlocked, Code: DegradedPolicyCodeEmbedderUnavailable,
			Reasons: reasons, Blockers: blockers, Signals: signals, FallbackUsed: fallbackVector,
		})
	}
	if providerOrCircuit && !fallbackVector && policy.BlockOnOpenCircuitWithoutVector {
		blockers = append(blockers, "embedding provider unavailable with no fallback vector")
		return NormalizeDegradedModeDecision(DegradedModeDecision{
			Allowed: false, Mode: DegradedPolicyModeBlocked, Code: DegradedPolicyCodeEmbedderUnavailable,
			Reasons: reasons, Blockers: blockers, Signals: signals,
		})
	}
	if fallbackVector && !policy.AllowDocumentFallbackVectors {
		blockers = append(blockers, "embedding fallback vectors disabled by policy")
		return NormalizeDegradedModeDecision(DegradedModeDecision{
			Allowed: false, Mode: DegradedPolicyModeBlocked, Code: DegradedPolicyCodeEmbedderUnavailable,
			Reasons: reasons, Blockers: blockers, Signals: signals, FallbackUsed: true,
		})
	}

	code := DegradedPolicyCodeEmbedderDegraded
	if fallbackVector {
		code = DegradedPolicyCodeEmbedderFallbackVector
	}
	return NormalizeDegradedModeDecision(DegradedModeDecision{
		Allowed: true, Mode: DegradedPolicyModeDegraded, Code: code,
		Reasons: reasons, Signals: signals, FallbackUsed: fallbackVector,
	})
}

func NormalizeDegradedModeDecision(d DegradedModeDecision) DegradedModeDecision {
	d.Reasons = cleanBlockers(d.Reasons)
	d.Blockers = cleanBlockers(d.Blockers)
	d.Signals = cleanDegradedSignals(d.Signals)
	if d.Allowed {
		if strings.TrimSpace(d.Mode) == "" {
			d.Mode = DegradedPolicyModeNormal
		}
		if strings.TrimSpace(d.Code) == "" {
			d.Code = DegradedPolicyCodeNormal
		}
		d.Code = normalizeReasonCode(d.Code)
		return d
	}
	d.Mode = DegradedPolicyModeBlocked
	if strings.TrimSpace(d.Code) == "" {
		d.Code = DegradedPolicyCodeEmbedderUnavailable
	}
	d.Code = normalizeReasonCode(d.Code)
	return d
}

func normalizeDegradedModePolicy(p DegradedModePolicy) DegradedModePolicy {
	def := DefaultDegradedModePolicy()
	if !p.AllowDocumentFallbackVectors && !p.BlockOnQueryDegradation && !p.BlockOnOpenCircuitWithoutVector {
		return def
	}
	if !p.BlockOnQueryDegradation {
		p.BlockOnQueryDegradation = def.BlockOnQueryDegradation
	}
	if !p.BlockOnOpenCircuitWithoutVector {
		p.BlockOnOpenCircuitWithoutVector = def.BlockOnOpenCircuitWithoutVector
	}
	return p
}

func cleanDegradedSignals(in []DegradedModeSignal) []DegradedModeSignal {
	out := make([]DegradedModeSignal, 0, len(in))
	seen := map[string]bool{}
	for _, sig := range in {
		sig.Source = strings.TrimSpace(sig.Source)
		sig.Path = strings.ToLower(strings.TrimSpace(sig.Path))
		sig.Outcome = strings.ToLower(strings.TrimSpace(sig.Outcome))
		sig.Reason = strings.ToLower(strings.TrimSpace(sig.Reason))
		sig.Provider = strings.TrimSpace(sig.Provider)
		sig.Model = strings.TrimSpace(sig.Model)
		sig.Message = strings.TrimSpace(sig.Message)
		key := sig.Source + "\x00" + sig.Path + "\x00" + sig.Outcome + "\x00" + sig.Reason + "\x00" + sig.Provider + "\x00" + sig.Model + "\x00" + sig.Message
		if sig.Path == "" && sig.Outcome == "" && sig.Reason == "" && sig.Message == "" {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sig)
	}
	return out
}

func degradedSignalReason(sig DegradedModeSignal) string {
	parts := []string{"embedding degraded"}
	if sig.Path != "" {
		parts = append(parts, "path="+sig.Path)
	}
	if sig.Outcome != "" {
		parts = append(parts, "outcome="+sig.Outcome)
	}
	if sig.Reason != "" {
		parts = append(parts, "reason="+sig.Reason)
	}
	if sig.Provider != "" {
		parts = append(parts, "provider="+sig.Provider)
	}
	if sig.Model != "" {
		parts = append(parts, "model="+sig.Model)
	}
	if sig.Message != "" {
		parts = append(parts, sig.Message)
	}
	return strings.Join(parts, " ")
}

func firstNonEmptyDegradedPolicy(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

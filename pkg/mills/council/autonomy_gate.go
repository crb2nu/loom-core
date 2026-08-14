package council

import (
	"context"
	"strings"
)

const (
	AutonomyReasonBlocked        = "autonomy_blocked"
	AutonomyReasonPolicyDisabled = "policy_disabled"
	AutonomyReasonCapabilityRed  = "capability_red"
)

// AutonomyGateDecision is the stable, machine-readable autonomy circuit
// breaker verdict shared by council and pipeline execution paths.
type AutonomyGateDecision struct {
	Allowed  bool
	Code     string
	Blockers []string
}

// AutonomyGate evaluates whether autonomous continuation is currently allowed.
type AutonomyGate interface {
	CheckAutonomy(ctx context.Context) AutonomyGateDecision
}

// AutonomyGateFunc adapts a function into AutonomyGate.
type AutonomyGateFunc func(ctx context.Context) AutonomyGateDecision

func (f AutonomyGateFunc) CheckAutonomy(ctx context.Context) AutonomyGateDecision {
	if f == nil {
		return AutonomyGateDecision{Allowed: true}
	}
	return NormalizeAutonomyDecision(f(ctx))
}

// NormalizeAutonomyDecision keeps the gate fail-closed and guarantees a stable
// reason code for audit payloads and escalation text.
func NormalizeAutonomyDecision(d AutonomyGateDecision) AutonomyGateDecision {
	if d.Allowed {
		return AutonomyGateDecision{Allowed: true}
	}
	d.Blockers = cleanBlockers(d.Blockers)
	if strings.TrimSpace(d.Code) == "" {
		d.Code = codeForBlockers(d.Blockers)
	} else {
		d.Code = normalizeReasonCode(d.Code)
	}
	return d
}

func cleanBlockers(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, b := range in {
		b = strings.TrimSpace(b)
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	return out
}

func codeForBlockers(blockers []string) string {
	for _, b := range blockers {
		lower := strings.ToLower(b)
		if strings.Contains(lower, "policy") || strings.Contains(lower, "kill-switch") || strings.Contains(lower, "kill switch") {
			return AutonomyReasonPolicyDisabled
		}
	}
	if len(blockers) > 0 {
		return AutonomyReasonCapabilityRed
	}
	return AutonomyReasonBlocked
}

func normalizeReasonCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	code = strings.NewReplacer(" ", "_", "-", "_", ".", "_", "/", "_").Replace(code)
	if code == "" {
		return AutonomyReasonBlocked
	}
	return code
}

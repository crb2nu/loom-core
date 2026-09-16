package council

import (
	"context"
	"errors"
	"strings"

	"github.com/crb2nu/loom/pkg/transport"
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

// TransientCapabilities returns bounded metric identities only when every blocker
// is a substrate failure: a transport error (the dependency is down) or a
// deadline / call-slot expiry (the dependency is busy — the 2026-09-13 hub
// probe `wait for call slot: context deadline exceeded` under load). Unknown or
// mixed configuration blockers fail closed.
func (d AutonomyGateDecision) TransientCapabilities() []string {
	d = NormalizeAutonomyDecision(d)
	if d.Allowed || d.Code != AutonomyReasonCapabilityRed || len(d.Blockers) == 0 {
		return nil
	}
	var capabilities []string
	seen := map[string]bool{}
	for _, blocker := range d.Blockers {
		if err := errors.New(blocker); !transport.IsError(err) && !transport.IsTimeout(err) {
			return nil
		}
		name, _, _ := strings.Cut(blocker, ":")
		switch name {
		case "sqlite_store", "policy_loaded", "admin_auth", "repo_root", "flexinfer", "gitlab", "hud_spawn", "mcp_hub_session", "dispatcher_write_stages", "council_participants", "branch_contract", "kpi_writer":
		default:
			name = "unknown"
		}
		if !seen[name] {
			capabilities = append(capabilities, name)
			seen[name] = true
		}
	}
	return capabilities
}

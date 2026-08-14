package gates

import (
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestEvaluateHealthSnapshot_AllCriticalHealthyAllows(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	got := EvaluateHealthSnapshot(HealthSnapshot{
		ObservedAt: now.Add(-time.Minute),
		Components: []HealthComponent{
			{Name: "mcp-hub", State: HealthStateHealthy, Critical: true, CheckedAt: now.Add(-time.Minute)},
			{Name: "grafana", State: HealthStateDown, Critical: false, CheckedAt: now.Add(-time.Minute)},
		},
	}, now)
	if !got.Allowed || got.FailClosed || got.Status != "pass" {
		t.Fatalf("decision = %+v, want allowed pass", got)
	}
	if got.OperationalState != telemetry.MillsOperationalStateIdleHealthy {
		t.Fatalf("operational state = %q, want idle_healthy", got.OperationalState)
	}
}

func TestEvaluateHealthSnapshot_FailsClosedOnMissingEvidence(t *testing.T) {
	got := EvaluateHealthSnapshot(HealthSnapshot{}, time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	if got.Allowed || !got.FailClosed {
		t.Fatalf("decision = %+v, want fail-closed block", got)
	}
	if got.OperationalState != telemetry.MillsOperationalStateIdleBlocked {
		t.Fatalf("operational state = %q, want idle_blocked", got.OperationalState)
	}
	if !contains(got.Reasons, "no infrastructure health evidence") {
		t.Fatalf("reasons = %+v", got.Reasons)
	}
}

func TestEvaluateHealthSnapshot_SurfaceActiveDependencyIncidentAsDegraded(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	got := EvaluateHealthSnapshot(HealthSnapshot{
		ObservedAt: now,
		Components: []HealthComponent{{
			Name: "gitlab", State: HealthStateDegraded, Critical: true, CheckedAt: now,
			Error: "api 503", IncidentID: "gitlab-503", IncidentActive: true,
		}},
	}, now)
	if got.Allowed || got.Status != "block" {
		t.Fatalf("decision = %+v, want blocked degraded", got)
	}
	if got.OperationalState != telemetry.MillsOperationalStateDegraded {
		t.Fatalf("operational state = %q, want degraded", got.OperationalState)
	}
	if len(got.DegradedDependencies) != 1 || got.DegradedDependencies[0] != "gitlab" {
		t.Fatalf("degraded dependencies = %v, want gitlab", got.DegradedDependencies)
	}
	if len(got.ActiveIncidents) != 1 || got.ActiveIncidents[0].ID != "gitlab-503" {
		t.Fatalf("active incidents = %+v, want gitlab-503", got.ActiveIncidents)
	}
}

func TestEvaluateHealthSnapshot_BlocksUnhealthyCritical(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	got := EvaluateHealthSnapshot(HealthSnapshot{
		ObservedAt: now,
		Components: []HealthComponent{{
			Name: "qdrant", State: HealthStateDown, Critical: true, CheckedAt: now,
			Error: "connection refused", Remediation: "restart qdrant",
		}},
	}, now)
	if got.Allowed || got.Status != "block" {
		t.Fatalf("decision = %+v, want blocked", got)
	}
	if !contains(got.Reasons, "critical dependency qdrant is down") {
		t.Fatalf("reasons = %+v", got.Reasons)
	}
	if len(got.Remediations) != 1 || got.Remediations[0] != "restart qdrant" {
		t.Fatalf("remediations = %+v", got.Remediations)
	}
}

func TestEvaluateHealthSnapshot_FailsClosedOnStaleEvidence(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	got := EvaluateHealthSnapshot(HealthSnapshot{
		ObservedAt: now.Add(-10 * time.Minute),
		MaxAge:     time.Minute,
		Components: []HealthComponent{{Name: "gitlab", State: HealthStateHealthy, Critical: true, CheckedAt: now.Add(-10 * time.Minute)}},
	}, now)
	if got.Allowed || !got.FailClosed {
		t.Fatalf("decision = %+v, want fail-closed stale block", got)
	}
	if !contains(got.Reasons, "stale") {
		t.Fatalf("reasons = %+v", got.Reasons)
	}
}

func TestEvaluateHealthSnapshot_FailsClosedOnStaleCriticalComponent(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	got := EvaluateHealthSnapshot(HealthSnapshot{
		ObservedAt: now,
		MaxAge:     time.Minute,
		Components: []HealthComponent{{Name: "mcp-hub", State: HealthStateHealthy, Critical: true, CheckedAt: now.Add(-10 * time.Minute)}},
	}, now)
	if got.Allowed || !got.FailClosed {
		t.Fatalf("decision = %+v, want fail-closed stale component block", got)
	}
	if !contains(got.Reasons, "critical dependency mcp-hub health check is stale") {
		t.Fatalf("reasons = %+v", got.Reasons)
	}
}

func contains(values []string, needle string) bool {
	for _, v := range values {
		if strings.Contains(v, needle) {
			return true
		}
	}
	return false
}

package gates

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestDefaultStorageHealthPolicy_DocumentedThresholds(t *testing.T) {
	policy := DefaultStorageHealthPolicy()
	if policy.WarningUsedPercent != 80 || policy.CriticalUsedPercent != 90 {
		t.Fatalf("policy = %+v, want 80%% warning and 90%% critical", policy)
	}
}

func TestEvaluateStorageHealthPolicy_UsesMostSevereCapacitySignal(t *testing.T) {
	tests := []struct {
		name           string
		snapshot       StorageHealthSnapshot
		wantState      StorageHealthState
		wantSeverity   IncidentSeverity
		writesAllowed  bool
		manualRecovery bool
	}{
		{
			name:      "normal below warning threshold",
			snapshot:  StorageHealthSnapshot{CapacityUsedPercent: 79.9, InodeUsedPercent: 20},
			wantState: StorageHealthStateNormal, wantSeverity: IncidentSeverityNone, writesAllowed: true,
		},
		{
			name:      "inode warning wins over capacity",
			snapshot:  StorageHealthSnapshot{CapacityUsedPercent: 30, InodeUsedPercent: 80},
			wantState: StorageHealthStateWarning, wantSeverity: IncidentSeverityWarning,
		},
		{
			name:      "capacity critical boundary",
			snapshot:  StorageHealthSnapshot{CapacityUsedPercent: 90, InodeUsedPercent: 50},
			wantState: StorageHealthStateCritical, wantSeverity: IncidentSeverityCritical,
		},
		{
			name:      "full inode filesystem is exhausted",
			snapshot:  StorageHealthSnapshot{CapacityUsedPercent: 70, InodeUsedPercent: 100},
			wantState: StorageHealthStateExhausted, wantSeverity: IncidentSeverityCritical, manualRecovery: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateStorageHealthPolicy(StorageHealthPolicy{}, tt.snapshot)
			if got.State != tt.wantState || got.AutonomousWritesAllowed != tt.writesAllowed {
				t.Fatalf("verdict = %+v, want state %q and writes allowed %t", got, tt.wantState, tt.writesAllowed)
			}
			if got.Classification.Severity != tt.wantSeverity || got.Classification.RequiresManualRecovery != tt.manualRecovery {
				t.Fatalf("classification = %+v, want severity %q and manual recovery %t", got.Classification, tt.wantSeverity, tt.manualRecovery)
			}
			if tt.wantState == StorageHealthStateNormal {
				if got.Classification.Class != IncidentClassNone {
					t.Fatalf("classification class = %q, want none", got.Classification.Class)
				}
			} else if got.Classification.Class != IncidentClassStorage || got.Classification.Dependency != "storage" || got.Classification.RetryAllowed {
				t.Fatalf("classification = %+v, want non-retryable storage incident", got.Classification)
			}
		})
	}
}

func TestEvaluateStorageHealthPolicy_IntegrityFailureOverridesThresholds(t *testing.T) {
	got := EvaluateStorageHealthPolicy(StorageHealthPolicy{WarningUsedPercent: 70, CriticalUsedPercent: 85}, StorageHealthSnapshot{
		CapacityUsedPercent: 10,
		WriteError:          "no space left on device",
	})
	if got.State != StorageHealthStateExhausted || got.Classification.Class != IncidentClassStorage {
		t.Fatalf("verdict = %+v, want exhausted storage incident", got)
	}
	if !got.Classification.RequiresManualRecovery || got.AutonomousWritesAllowed {
		t.Fatalf("verdict = %+v, want manual recovery with writes blocked", got)
	}
}

func TestEvaluateStorageHealthPolicy_InvalidPolicyUsesDefaults(t *testing.T) {
	got := EvaluateStorageHealthPolicy(StorageHealthPolicy{WarningUsedPercent: 95, CriticalUsedPercent: 80}, StorageHealthSnapshot{CapacityUsedPercent: 85})
	if got.State != StorageHealthStateWarning {
		t.Fatalf("verdict = %+v, want warning from default 80%% threshold", got)
	}
}

func TestEvaluateTelemetryHealthPolicy_HealthyTelemetryPasses(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	got := EvaluateTelemetryHealthPolicy(TelemetryHealthPolicy{}, TelemetryHealthSnapshot{
		ObservedAt:           now.Add(-time.Minute),
		PipelineFailureRate:  0.05,
		GateFlakeRate:        0.01,
		JudgeUnparseableRate: 0.01,
		RetryBurnRate:        0.10,
		QueueDepth:           2,
	}, now)

	if !got.Pass || got.Degraded || got.Code != TelemetryHealthCodeOK {
		t.Fatalf("verdict = %+v, want healthy pass", got)
	}
	if got.OperationalState != telemetry.MillsOperationalStateIdleHealthy {
		t.Fatalf("operational state = %q, want idle_healthy", got.OperationalState)
	}
	if len(got.Checks) != 5 {
		t.Fatalf("checks = %d, want 5", len(got.Checks))
	}
}

func TestEvaluateTelemetryHealthPolicy_ThresholdBreachMarksDegraded(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	got := EvaluateTelemetryHealthPolicy(TelemetryHealthPolicy{
		MaxPipelineFailureRate: 0.20,
		MaxQueueDepth:          10,
	}, TelemetryHealthSnapshot{
		ObservedAt:          now,
		PipelineFailureRate: 0.40,
		QueueDepth:          12,
	}, now)

	if got.Pass || !got.Degraded || got.Code != TelemetryHealthCodeThresholdBreach {
		t.Fatalf("verdict = %+v, want threshold degraded", got)
	}
	if got.Severity != TelemetryHealthSeverityCritical {
		t.Fatalf("severity = %q, want critical", got.Severity)
	}
	if got.OperationalState != telemetry.MillsOperationalStateDegraded {
		t.Fatalf("operational state = %q, want degraded", got.OperationalState)
	}
	if len(got.DegradedDependencies) != 1 || got.DegradedDependencies[0] != "telemetry" {
		t.Fatalf("degraded dependencies = %v, want telemetry", got.DegradedDependencies)
	}
	if !contains(got.Reasons, "pipeline failure rate exceeds telemetry health threshold") {
		t.Fatalf("reasons = %v", got.Reasons)
	}
	if got.Metrics["pipeline_failure_rate.threshold"] != 0.20 {
		t.Fatalf("pipeline threshold metric = %v", got.Metrics["pipeline_failure_rate.threshold"])
	}
}

func TestEvaluateTelemetryHealthPolicy_BackendIncidentWinsCodeAndCarriesIncident(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	got := EvaluateTelemetryHealthPolicy(TelemetryHealthPolicy{
		MaxGateFlakeRate: 0.05,
	}, TelemetryHealthSnapshot{
		ObservedAt:    now,
		GateFlakeRate: 0.20,
		BackendIncidents: []TelemetryBackendIncident{
			{ID: "gitlab-503", Backend: "gitlab", Summary: "api 503", Active: true},
			{ID: "old-qdrant", Backend: "qdrant", Summary: "resolved", Active: false},
			{ID: "gitlab-503", Backend: "gitlab", Summary: "duplicate", Active: true},
		},
	}, now)

	if got.Pass || !got.Degraded || got.Code != TelemetryHealthCodeBackendIncident {
		t.Fatalf("verdict = %+v, want backend incident degraded", got)
	}
	if got.Severity != TelemetryHealthSeverityCritical {
		t.Fatalf("severity = %q, want critical", got.Severity)
	}
	if len(got.ActiveIncidents) != 1 {
		t.Fatalf("active incidents = %+v, want one deduped incident", got.ActiveIncidents)
	}
	incident := got.ActiveIncidents[0]
	if incident.ID != "gitlab-503" || incident.Dependency != "gitlab" || incident.Summary != "api 503" {
		t.Fatalf("incident = %+v, want gitlab-503/gitlab/api 503", incident)
	}
	if len(got.DegradedDependencies) != 1 || got.DegradedDependencies[0] != "gitlab" {
		t.Fatalf("degraded dependencies = %v, want gitlab", got.DegradedDependencies)
	}
}

func TestEvaluateTelemetryHealthPolicy_StaleEvidenceMarksDegraded(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	got := EvaluateTelemetryHealthPolicy(TelemetryHealthPolicy{MaxTelemetryAge: time.Minute}, TelemetryHealthSnapshot{
		ObservedAt:          now.Add(-5 * time.Minute),
		PipelineFailureRate: 0.50,
	}, now)

	if got.Pass || !got.Degraded || got.Code != TelemetryHealthCodeStaleTelemetry {
		t.Fatalf("verdict = %+v, want stale degraded", got)
	}
	if got.OperationalState != telemetry.MillsOperationalStateDegraded {
		t.Fatalf("operational state = %q, want degraded", got.OperationalState)
	}
	if !contains(got.Reasons, "telemetry health evidence is stale") {
		t.Fatalf("reasons = %v", got.Reasons)
	}
	if got.Metrics["telemetry_age_seconds"] != 300 {
		t.Fatalf("telemetry age metric = %v, want 300", got.Metrics["telemetry_age_seconds"])
	}
}

func TestEvaluateTelemetryHealthPolicy_DefaultsNormalizeInvalidPolicy(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	got := EvaluateTelemetryHealthPolicy(TelemetryHealthPolicy{
		MaxPipelineFailureRate:  2,
		MaxGateFlakeRate:        -1,
		MaxJudgeUnparseableRate: 0,
		MaxRetryBurnRate:        1,
		MaxQueueDepth:           -5,
	}, TelemetryHealthSnapshot{
		ObservedAt:           now,
		PipelineFailureRate:  0.26,
		GateFlakeRate:        0.11,
		JudgeUnparseableRate: 0.06,
		RetryBurnRate:        0.36,
		QueueDepth:           26,
	}, now)

	if got.Pass || got.Code != TelemetryHealthCodeThresholdBreach {
		t.Fatalf("verdict = %+v, want default threshold breach", got)
	}
	if got.Metrics["pipeline_failure_rate.threshold"] != DefaultTelemetryHealthPolicy().MaxPipelineFailureRate {
		t.Fatalf("default pipeline threshold metric = %v", got.Metrics["pipeline_failure_rate.threshold"])
	}
	if got.Metrics["queue_depth.threshold"] != float64(DefaultTelemetryHealthPolicy().MaxQueueDepth) {
		t.Fatalf("default queue threshold metric = %v", got.Metrics["queue_depth.threshold"])
	}
}

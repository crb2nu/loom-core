package council

import (
	"encoding/json"
	"testing"

	"github.com/crb2nu/loom/pkg/telemetry"
)

func TestEvaluateCouncilStatusSurfacesDegradedReasonCodes(t *testing.T) {
	got := EvaluateCouncilStatus(CouncilStatusInput{
		AutonomyAllowed:      true,
		HealthAllowed:        true,
		DegradedDependencies: []string{"gitlab", " gitlab ", "prometheus"},
		ActiveDependencyIncidents: []telemetry.DependencyIncident{
			{ID: "inc-2", Dependency: "gitlab", Summary: "pipeline API returning 503"},
			{ID: "inc-2", Dependency: "gitlab", Summary: "pipeline API returning 503"},
		},
	})

	if got.OperationalState != telemetry.MillsOperationalStateDegraded {
		t.Fatalf("OperationalState = %q, want degraded; status=%+v", got.OperationalState, got)
	}
	if !got.DegradedMode {
		t.Fatal("DegradedMode = false, want true")
	}
	wantReasons := []DegradedReason{
		{Code: DegradedReasonDependencyDegraded, Dependency: "gitlab"},
		{Code: DegradedReasonDependencyDegraded, Dependency: "prometheus"},
		{Code: DegradedReasonIncidentActive, Dependency: "gitlab", IncidentID: "inc-2", Message: "pipeline API returning 503"},
	}
	if len(got.DegradedReasons) != len(wantReasons) {
		t.Fatalf("DegradedReasons = %+v, want %+v", got.DegradedReasons, wantReasons)
	}
	for i := range wantReasons {
		if got.DegradedReasons[i] != wantReasons[i] {
			t.Fatalf("DegradedReasons[%d] = %+v, want %+v", i, got.DegradedReasons[i], wantReasons[i])
		}
	}
	if got.PolicyVerdict.Pass {
		t.Fatalf("PolicyVerdict.Pass = true, want false: %+v", got.PolicyVerdict)
	}
	if got.PolicyVerdict.Code != CouncilStatusCodeTelemetryDegraded {
		t.Fatalf("PolicyVerdict.Code = %q, want %q", got.PolicyVerdict.Code, CouncilStatusCodeTelemetryDegraded)
	}
	if got.PolicyVerdict.Severity != PolicySeverityWarning || got.PolicyVerdict.Action != PolicyActionEscalate {
		t.Fatalf("PolicyVerdict = %+v, want warning/escalate", got.PolicyVerdict)
	}
	if got.PolicyVerdict.Metrics["degraded_dependencies"] != 2 || got.PolicyVerdict.Metrics["active_incidents"] != 1 {
		t.Fatalf("PolicyVerdict.Metrics = %+v, want dependency and incident counts", got.PolicyVerdict.Metrics)
	}
}

func TestCouncilStatusFromOperationalStateHealthyPasses(t *testing.T) {
	got := CouncilStatusFromOperationalState(telemetry.MillsOperationalStateReport{
		State: telemetry.MillsOperationalStateIdleHealthy,
	})

	if got.DegradedMode {
		t.Fatal("DegradedMode = true, want false")
	}
	if len(got.DegradedReasons) != 0 {
		t.Fatalf("DegradedReasons = %+v, want none", got.DegradedReasons)
	}
	if !got.PolicyVerdict.Pass || got.PolicyVerdict.Code != CouncilStatusCodeOK {
		t.Fatalf("PolicyVerdict = %+v, want passing ok verdict", got.PolicyVerdict)
	}
}

func TestCouncilStatusFromOperationalStateFallsBackToTelemetryReasons(t *testing.T) {
	got := CouncilStatusFromOperationalState(telemetry.MillsOperationalStateReport{
		State:   telemetry.MillsOperationalStateDegraded,
		Reasons: []string{" dependency incident active ", "dependency incident active"},
	})

	if len(got.DegradedReasons) != 1 {
		t.Fatalf("DegradedReasons = %+v, want one fallback reason", got.DegradedReasons)
	}
	if got.DegradedReasons[0] != (DegradedReason{Code: DegradedReasonTelemetryReason, Message: "dependency incident active"}) {
		t.Fatalf("fallback reason = %+v", got.DegradedReasons[0])
	}
	if len(got.PolicyVerdict.Reasons) != 1 || got.PolicyVerdict.Reasons[0] != "dependency incident active" {
		t.Fatalf("PolicyVerdict.Reasons = %+v", got.PolicyVerdict.Reasons)
	}
}

func TestCouncilStatusJSONCarriesDegradedModeFields(t *testing.T) {
	got := EvaluateCouncilStatus(CouncilStatusInput{
		AutonomyAllowed:      true,
		HealthAllowed:        true,
		DegradedDependencies: []string{"gitlab"},
	})

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["operational_state"] != string(telemetry.MillsOperationalStateDegraded) {
		t.Fatalf("operational_state = %#v", decoded["operational_state"])
	}
	if decoded["degraded_mode"] != true {
		t.Fatalf("degraded_mode = %#v", decoded["degraded_mode"])
	}
	if _, ok := decoded["degraded_reasons"].([]any); !ok {
		t.Fatalf("degraded_reasons missing from JSON: %s", raw)
	}
}

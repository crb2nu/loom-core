package pipeline

import "testing"

func TestEvaluatePreflight_AllowsWhenRequiredChecksPass(t *testing.T) {
	got := EvaluatePreflight(ActionPipelineExecute, DefaultPreflightPolicy(), []PreflightCheck{
		{Name: "worktree_clean", Passed: true, Required: true},
		{Name: "scope_loaded", Passed: true, Required: true},
		{Name: "policy_loaded", Passed: true, Required: true},
		{Name: "otel_reachable", Passed: false},
	})
	if !got.Allowed {
		t.Fatalf("allowed = false, reasons=%v", got.Reasons)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("warnings = %v, want one optional warning", got.Warnings)
	}
}

func TestEvaluatePreflight_FailsClosedWhenRequiredCheckMissing(t *testing.T) {
	got := EvaluatePreflight(ActionCouncilPlan, DefaultPreflightPolicy(), []PreflightCheck{
		{Name: "policy_loaded", Passed: true, Required: true},
	})
	if got.Allowed {
		t.Fatalf("allowed = true, want blocked")
	}
	if !got.FailClosed {
		t.Fatalf("failClosed = false, want true")
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != `missing required preflight "roadmap_extracted"` {
		t.Fatalf("reasons = %v", got.Reasons)
	}
}

func TestEvaluatePreflight_BlocksFailedRequiredCheck(t *testing.T) {
	got := EvaluatePreflight(ActionMerge, DefaultPreflightPolicy(), []PreflightCheck{
		{Name: "tests_green", Passed: true, Required: true},
		{Name: "mr_ready", Passed: false, Required: true, Message: "pipeline still running"},
		{Name: "policy_loaded", Passed: true, Required: true},
	})
	if got.Allowed {
		t.Fatalf("allowed = true, want blocked")
	}
	if got.FailClosed {
		t.Fatalf("failClosed = true, want ordinary preflight block")
	}
}

func TestEvaluatePreflight_FailsClosedWithoutPolicy(t *testing.T) {
	got := EvaluatePreflight(Action("unknown"), DefaultPreflightPolicy(), nil)
	if got.Allowed || !got.FailClosed {
		t.Fatalf("result = %+v, want fail-closed block", got)
	}
}

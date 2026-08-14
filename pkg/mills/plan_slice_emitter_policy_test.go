package mills

import (
	"testing"
	"time"
)

// TestPlanSliceEmitterPolicyParse validates the intake.plan_slice_emitter
// block parses and the accessors resolve configured values + defaults.
func TestPlanSliceEmitterPolicyParse(t *testing.T) {
	pm := canaryPolicyMgr(t, `version: 2
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }
pipeline:
  retry: { max_attempts: 1, cooldown_seconds: 0 }
intake:
  plan_slice_emitter:
    enabled: true
    namespace: mills/eligible
    project: services/loom-core
    ready_phase: pending
    priority: P1
    poll_interval_seconds: 120
    tick_timeout_seconds: 45
`)
	p := pm.Current()
	if !p.PlanSliceEmitterEnabled() {
		t.Fatal("PlanSliceEmitterEnabled() = false, want true")
	}
	if got := p.PlanSliceEmitterNamespace(); got != "mills/eligible" {
		t.Errorf("Namespace = %q, want mills/eligible", got)
	}
	if got := p.PlanSliceEmitterProject(); got != "services/loom-core" {
		t.Errorf("Project = %q, want services/loom-core", got)
	}
	if got := p.PlanSliceEmitterReadyPhase(); got != "pending" {
		t.Errorf("ReadyPhase = %q, want pending", got)
	}
	if got := p.PlanSliceEmitterPriority(); got != "P1" {
		t.Errorf("Priority = %q, want P1", got)
	}
	if got := p.PlanSliceEmitterPollInterval().Seconds(); got != 120 {
		t.Errorf("PollInterval = %vs, want 120s", got)
	}
	if got := p.PlanSliceEmitterTickTimeout().Seconds(); got != 45 {
		t.Errorf("TickTimeout = %vs, want 45s", got)
	}
}

// TestPlanSliceEmitterPolicyDefaults checks the fail-closed + default
// behavior when the block is omitted.
func TestPlanSliceEmitterPolicyDefaults(t *testing.T) {
	pm := canaryPolicyMgr(t, `version: 2
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }
pipeline:
  retry: { max_attempts: 1, cooldown_seconds: 0 }
`)
	p := pm.Current()
	if p.PlanSliceEmitterEnabled() {
		t.Error("PlanSliceEmitterEnabled() = true on omitted block, want false")
	}
	if got := p.PlanSliceEmitterNamespace(); got != "" {
		t.Errorf("Namespace = %q, want empty (fail-closed)", got)
	}
	if got := p.PlanSliceEmitterReadyPhase(); got != "pending" {
		t.Errorf("ReadyPhase default = %q, want pending", got)
	}
	if got := p.PlanSliceEmitterLabel(); got != "mills-from-plan-slice" {
		t.Errorf("Label default = %q, want mills-from-plan-slice", got)
	}
	if got := p.PlanSliceEmitterPriority(); got != "P2" {
		t.Errorf("Priority default = %q, want P2", got)
	}
	if got := p.PlanSliceEmitterTickTimeout(); got != 2*time.Minute {
		t.Errorf("TickTimeout default = %v, want 2m", got)
	}
	// nil-safe
	var np *Policy
	if np.PlanSliceEmitterEnabled() {
		t.Error("nil Policy PlanSliceEmitterEnabled() = true, want false")
	}
	if got := np.PlanSliceEmitterTickTimeout(); got != 2*time.Minute {
		t.Errorf("nil Policy TickTimeout = %v, want 2m", got)
	}
}

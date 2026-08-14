package mills

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// crossRepoItem builds a runnable queued item (slice + budget) with the given
// target project, so it reaches the cross-repo gate in tryStart.
func crossRepoItem(id, target string) *store.BacklogItem {
	return &store.BacklogItem{
		ID: id, Title: "cross-repo", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
		TargetProject: target,
		Slices:        []store.Slice{{Name: "x", Files: []string{"a.go"}}},
		Budget:        store.Budget{MaxCostUSD: 1},
	}
}

// TestReconciler_CrossRepoDisabledSkips: a cross-repo item is skipped
// fail-closed while cross_repo execution is disabled — it never starts a run
// against the home repo.
func TestReconciler_CrossRepoDisabledSkips(t *testing.T) {
	env := newRecEnv(t, nil) // CrossRepo.Enabled defaults false
	env.rec.HomeProject = "services/loom-core"
	ctx := context.Background()

	if err := env.store.Backlog.Put(ctx, crossRepoItem("MILLS-XREPO", "services/loom-flightdeck")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Skipped != 1 || res.Started != 0 {
		t.Errorf("expected skipped=1 started=0, got %+v", res)
	}
	if env.starter.calls() != 0 {
		t.Errorf("starter must not run for a gated cross-repo item, got %d", env.starter.calls())
	}
}

// TestReconciler_HomeItemNotGated: an item targeting the home repo (by bare
// name, ignoring the bucket prefix) runs even while cross_repo is disabled.
func TestReconciler_HomeItemNotGated(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ctx := context.Background()

	if err := env.store.Backlog.Put(ctx, crossRepoItem("MILLS-HOME", "loom-core")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 {
		t.Errorf("expected started=1 for a home-targeted item, got %+v", res)
	}
	if env.starter.calls() != 1 {
		t.Errorf("starter should run for a home item, got %d", env.starter.calls())
	}
}

// TestReconciler_CrossRepoEnabledRuns: with cross_repo enabled, a cross-repo
// item passes the gate and starts a run. Driven through tryStart directly with
// an explicit enabled policy — the test's YAML fixture writer
// (writePolicyYAMLForTest) ignores mutated sub-policies, so a policyMutator
// can't flip cross_repo.enabled in the loaded policy.
func TestReconciler_CrossRepoEnabledRuns(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.HomeProject = "services/loom-core"
	ctx := context.Background()

	pol := Default()
	on := true
	pol.Enabled = &on
	pol.CrossRepo.Enabled = true

	item := crossRepoItem("MILLS-XREPO-ON", "services/loom-flightdeck")
	if err := env.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dec, run, _, err := env.rec.tryStart(ctx, item, pol)
	if err != nil {
		t.Fatalf("tryStart: %v", err)
	}
	if dec != decisionStarted || run == nil {
		t.Errorf("expected decisionStarted with a run when cross_repo enabled, got dec=%v run=%v", dec, run)
	}
	if env.starter.calls() != 1 {
		t.Errorf("starter should run when cross_repo enabled, got %d", env.starter.calls())
	}
}

// TestReconciler_CrossRepoGateInertWithoutHomeProject: when HomeProject is
// unset, the gate is disabled (pre-cross-repo behavior) — a TargetProject item
// runs normally.
func TestReconciler_CrossRepoGateInertWithoutHomeProject(t *testing.T) {
	env := newRecEnv(t, nil) // CrossRepo disabled, HomeProject unset
	ctx := context.Background()

	if err := env.store.Backlog.Put(ctx, crossRepoItem("MILLS-NOHOME", "services/loom-flightdeck")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := env.rec.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 {
		t.Errorf("expected started=1 when HomeProject unset (gate inert), got %+v", res)
	}
}

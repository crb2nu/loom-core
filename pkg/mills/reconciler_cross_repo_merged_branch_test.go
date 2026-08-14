package mills

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// recordingBranchClientFactory stands in for GhostSparkMergedBranchForProject:
// it records every project it was asked to build a client for and answers from
// a fixed project→client table (nil for unknown projects).
type recordingBranchClientFactory struct {
	mu      sync.Mutex
	clients map[string]*fakeMergedBranchClient
	asked   []string
}

func (f *recordingBranchClientFactory) forProject(project string) MergedBranchMRClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, project)
	c, ok := f.clients[project]
	if !ok {
		return nil
	}
	return c
}

func (f *recordingBranchClientFactory) askedProjects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// bindEscalationTarget appends the escalation-time target binding the pipeline
// runner writes when a run escalates with its item.
func bindEscalationTarget(t *testing.T, env *recTestEnv, runID, backlogID, targetProject string) {
	t.Helper()
	appended, err := AppendEscalationTargetBinding(
		context.Background(), env.store.Events, "pipeline",
		&store.PipelineRun{ID: runID},
		&store.BacklogItem{ID: backlogID, TargetProject: targetProject},
	)
	if err != nil || !appended {
		t.Fatalf("bind escalation target %s→%s: appended=%v err=%v", runID, targetProject, appended, err)
	}
}

// The cross-repo kill test (procmodel saga, 2026-08-07): an item targeting a
// foreign repo escalated before the mr stage, its branch was later merged by
// hand in that repo (services/procmodel!2), and the item could only be closed
// by a manual backlog upsert — which also skipped the run-verdict supersede.
// With the escalation-time binding recorded, the merged-branch pass must close
// the item through the same choke point as every other pass: item merged, MR
// identity + project recorded, and the run's verdict superseded to
// merged_after_escalation.
func TestCrossRepoMergedBranchSparkClosesBoundItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-2 * time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-XREPO-BOUND", started, "services/procmodel")
	bindEscalationTarget(t, env, "PIPE-"+item.ID, item.ID, "services/procmodel")

	home := &fakeMergedBranchClient{}
	enableMergedBranchPass(env, home)
	cross := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-XREPO-BOUND/the-slice": {iid: 2, when: started.Add(time.Hour)},
	}}
	factory := &recordingBranchClientFactory{clients: map[string]*fakeMergedBranchClient{
		"services/procmodel": cross,
	}}
	env.rec.GhostSparkMergedBranchForProject = factory.forProject

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 1 || res.BranchMerged != 1 || res.BranchBindingSkipped != 0 {
		t.Fatalf("sweep result = %+v, want one cross-repo branch close", res)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogMerged {
		t.Fatalf("item state = %s, want merged", got.State)
	}
	if got := factory.askedProjects(); len(got) != 1 || got[0] != "services/procmodel" {
		t.Fatalf("factory asked projects = %v, want exactly the bound project", got)
	}
	if n := len(home.askedBranches()); n != 0 {
		t.Fatalf("home client answered %d lookups for a cross-repo item, want 0", n)
	}

	closedEvent, err := env.store.Events.FirstBySubjectKind(
		ctx, "pipeline_run", "PIPE-"+item.ID, "reconciler.ghost_spark_closed",
	)
	if err != nil {
		t.Fatalf("ghost_spark_closed event: %v", err)
	}
	if got, _ := closedEvent.Payload["project"].(string); got != "services/procmodel" {
		t.Fatalf("close event project = %q, want the bound project (an MR IID is per-project)", got)
	}

	// Trustworthy Verdicts: the close must supersede the run's escalated
	// verdict — a manual upsert skipping this is exactly what left the
	// procmodel runs reading escalated class=code forever.
	verdict, err := env.store.Events.FirstBySubjectKind(
		ctx, "pipeline_run", "PIPE-"+item.ID, RunVerdictKindGhostSparkMerged,
	)
	if err != nil {
		t.Fatalf("run verdict supersede event: %v", err)
	}
	if got, _ := verdict.Payload["class"].(string); got != RunVerdictClassMergedAfterEscalation {
		t.Fatalf("verdict class = %q, want %s", got, RunVerdictClassMergedAfterEscalation)
	}
	if got, _ := verdict.Payload["outcome"].(string); got != "merged_branch" {
		t.Fatalf("verdict outcome = %q, want merged_branch", got)
	}
	if got, _ := verdict.Payload["project"].(string); got != "services/procmodel" {
		t.Fatalf("verdict project = %q, want the bound project", got)
	}

	// Idempotent: the item has left the escalated set.
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.Merged != 0 || res2.BranchInspected != 0 {
		t.Fatalf("second sweep = %+v, want no-op", res2)
	}
}

// Without a binding event (every escalation that predates emission), a
// cross-repo item is left alone even when its branch DID merge in the target
// project: mutable target_project alone must never route the lookup.
func TestCrossRepoMergedBranchSparkRequiresBinding(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-XREPO-NOBIND", started, "services/procmodel")

	enableMergedBranchPass(env, &fakeMergedBranchClient{})
	cross := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-XREPO-NOBIND/the-slice": {iid: 9, when: started.Add(time.Minute)},
	}}
	factory := &recordingBranchClientFactory{clients: map[string]*fakeMergedBranchClient{
		"services/procmodel": cross,
	}}
	env.rec.GhostSparkMergedBranchForProject = factory.forProject

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 || res.BranchBindingSkipped != 1 {
		t.Fatalf("sweep = %+v, want a binding skip and no close", res)
	}
	if n := len(factory.askedProjects()); n != 0 {
		t.Fatalf("factory consulted %d times without a binding, want 0", n)
	}
	if n := len(cross.askedBranches()); n != 0 {
		t.Fatalf("issued %d cross-repo lookups without a binding, want 0", n)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}

	// The binding is immutable, so re-reading it next tick cannot change the
	// answer — the skip defers the recheck cooldown instead of re-scanning.
	res2, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res2.BranchBindingSkipped != 0 {
		t.Fatalf("second sweep = %+v, want the candidate cooled down", res2)
	}
}

// A binding that no longer matches the item's current target means the item
// was retargeted AFTER it escalated. Evidence from the frozen project cannot
// close work now claimed for another repo — and the mutated field must not
// route a lookup anywhere.
func TestCrossRepoMergedBranchSparkSkipsRetargetedItem(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-XREPO-RETARGET", started, "services/procmodel")
	bindEscalationTarget(t, env, "PIPE-"+item.ID, item.ID, "services/procmodel")

	mutated, err := env.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	mutated.TargetProject = "services/flexdeck"
	if err := env.store.Backlog.Put(ctx, mutated); err != nil {
		t.Fatalf("retarget item: %v", err)
	}

	enableMergedBranchPass(env, &fakeMergedBranchClient{})
	merged := map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-XREPO-RETARGET/the-slice": {iid: 4, when: started.Add(time.Minute)},
	}
	factory := &recordingBranchClientFactory{clients: map[string]*fakeMergedBranchClient{
		"services/procmodel": {merged: merged},
		"services/flexdeck":  {merged: merged},
	}}
	env.rec.GhostSparkMergedBranchForProject = factory.forProject

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 || res.BranchBindingSkipped != 1 {
		t.Fatalf("sweep = %+v, want a binding-mismatch skip", res)
	}
	if n := len(factory.askedProjects()); n != 0 {
		t.Fatalf("factory consulted %d times on a retargeted item, want 0", n)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// The reverse retarget: an item that escalated targeting a FOREIGN repo and
// was then pointed home must not be closed on home-branch evidence — the work
// it escalated with never lived there.
func TestMergedBranchSparkSkipsHomeItemWithForeignBinding(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-HOME-RETARGET", started, "")
	bindEscalationTarget(t, env, "PIPE-"+item.ID, item.ID, "services/procmodel")

	home := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-HOME-RETARGET/the-slice": {iid: 6, when: started.Add(time.Minute)},
	}}
	enableMergedBranchPass(env, home)

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 || res.BranchBindingSkipped != 1 {
		t.Fatalf("sweep = %+v, want a foreign-binding skip", res)
	}
	if n := len(home.askedBranches()); n != 0 {
		t.Fatalf("issued %d home lookups against a foreign binding, want 0", n)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// Guard 3 holds cross-repo exactly as it does at home: a merge that predates
// the escalated attempt is stale evidence from an earlier attempt.
func TestCrossRepoMergedBranchSparkIgnoresMergeOlderThanAttempt(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-XREPO-STALE", started, "services/procmodel")
	bindEscalationTarget(t, env, "PIPE-"+item.ID, item.ID, "services/procmodel")

	enableMergedBranchPass(env, &fakeMergedBranchClient{})
	cross := &fakeMergedBranchClient{merged: map[string]struct {
		iid  int64
		when time.Time
	}{
		"feat/MILLS-XREPO-STALE/the-slice": {iid: 5, when: started.Add(-2 * time.Hour)},
	}}
	factory := &recordingBranchClientFactory{clients: map[string]*fakeMergedBranchClient{
		"services/procmodel": cross,
	}}
	env.rec.GhostSparkMergedBranchForProject = factory.forProject

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 {
		t.Fatalf("merged = %d, want 0 — a pre-attempt merge must not close a live escalation", res.Merged)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// Nil GhostSparkMergedBranchForProject keeps the pass home-only, byte-for-byte
// pre-binding behavior — even when a binding exists. Rollback stays a config
// change, and the fast path skips before any store read (no binding-skip
// counter, no cooldown entry).
func TestCrossRepoMergedBranchSparkWithoutFactoryLeavesItemAlone(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	started := env.now.Add(-time.Hour)
	item := seedGateEscalatedItem(t, env, "MILLS-XREPO-NOFACTORY", started, "services/procmodel")
	bindEscalationTarget(t, env, "PIPE-"+item.ID, item.ID, "services/procmodel")

	home := &fakeMergedBranchClient{}
	enableMergedBranchPass(env, home)

	res, err := env.rec.SweepGhostSparks(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Merged != 0 || res.BranchBindingSkipped != 0 || res.BranchInspected != 0 {
		t.Fatalf("sweep = %+v, want the pre-binding home-only skip", res)
	}
	if got, _ := env.store.Backlog.Get(ctx, item.ID); got.State != store.BacklogEscalated {
		t.Fatalf("item state = %s, want escalated", got.State)
	}
}

// First-writer semantics: a retried escalation cannot rewrite the frozen
// project, and resolution reads the original binding.
func TestAppendEscalationTargetBindingFirstWriter(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	run := &store.PipelineRun{ID: "PIPE-BINDING-ONCE"}

	appended, err := AppendEscalationTargetBinding(ctx, env.store.Events, "pipeline",
		run, &store.BacklogItem{ID: "BL-1", TargetProject: " services/procmodel "})
	if err != nil || !appended {
		t.Fatalf("first append: appended=%v err=%v", appended, err)
	}
	appended, err = AppendEscalationTargetBinding(ctx, env.store.Events, "pipeline",
		run, &store.BacklogItem{ID: "BL-1", TargetProject: "services/other"})
	if err != nil || appended {
		t.Fatalf("second append must be a no-op: appended=%v err=%v", appended, err)
	}

	bound, found, err := escalationTargetBinding(ctx, env.store.Events, run.ID)
	if err != nil || !found || bound != "services/procmodel" {
		t.Fatalf("binding = (%q, %v, %v), want the trimmed first write", bound, found, err)
	}
	if _, found, err = escalationTargetBinding(ctx, env.store.Events, "PIPE-NEVER-BOUND"); err != nil || found {
		t.Fatalf("unbound run resolution = (found=%v, err=%v), want not found", found, err)
	}
}

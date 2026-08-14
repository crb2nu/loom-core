package mills

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeWorkflowSelector satisfies WorkflowSelector for the S7 routing tests.
type fakeWorkflowSelector struct {
	sel    *store.WorkflowSelection
	reason string
	err    error
	calls  int
}

func (f *fakeWorkflowSelector) Resolve(_ context.Context, _ *store.BacklogItem, _ bool) (*store.WorkflowSelection, string, error) {
	f.calls++
	return f.sel, f.reason, f.err
}

func testWorkflowSelection() *store.WorkflowSelection {
	return &store.WorkflowSelection{
		Engine:             store.WorkflowEngineImperative,
		Template:           "implement-gate",
		TemplateVersion:    "v1",
		InterpreterVersion: "starlark-go@test",
		ParamsJSON:         `{"registry_template":{"content_hash":"abc","params":{"budget_usd":1}}}`,
	}
}

func seedQueuedWorkflowItem(t *testing.T, env *recTestEnv, id string) *store.BacklogItem {
	t.Helper()
	item := &store.BacklogItem{
		ID:        id,
		Title:     "workflow routing " + id,
		State:     store.BacklogQueued,
		Priority:  store.P1,
		CreatedBy: "test",
		Budget:    store.Budget{MaxCostUSD: 1},
	}
	if err := env.store.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	return item
}

// TestReconciler_WorkflowSelectionRoutesToImperativeLane: a frozen selection
// claims through ClaimWorkflowStart — an imperative run exists, the item is
// running, and the DAG lane (pipeline run + starter + dispatch) is untouched.
func TestReconciler_WorkflowSelectionRoutesToImperativeLane(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.WorkflowSelector = &fakeWorkflowSelector{sel: testWorkflowSelection()}
	item := seedQueuedWorkflowItem(t, env, "MILLS-WF-ROUTE")

	res, err := env.rec.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 {
		t.Fatalf("started = %d, want 1", res.Started)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("pipeline starter invoked %d times for an imperative selection", env.starter.calls())
	}
	runs, err := env.store.Pipeline.ListByBacklog(context.Background(), item.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("pipeline runs = %d err=%v, want none", len(runs), err)
	}
	imperative, err := env.store.Workflow.ListRunningImperativeRuns(context.Background())
	if err != nil || len(imperative) != 1 || imperative[0].BacklogID != item.ID {
		t.Fatalf("imperative runs = %v err=%v, want one for the item", imperative, err)
	}
	fresh, err := env.store.Backlog.Get(context.Background(), item.ID)
	if err != nil || fresh.State != store.BacklogRunning {
		t.Fatalf("item = %+v err=%v, want running", fresh, err)
	}
}

// TestReconciler_WorkflowStartAttributesSquad: an imperative start carries the
// same squad attribution the DAG lane gets, keyed subject_kind=workflow_run —
// the regression this guards: workflow-selected items were never squad-routed,
// so routing data silently thinned as the imperative lane grew.
func TestReconciler_WorkflowStartAttributesSquad(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.WorkflowSelector = &fakeWorkflowSelector{sel: testWorkflowSelection()}
	router := &fakeSquadRouter{out: SquadDecision{
		SquadName: "wf-squad", PathClass: "docs/**", Confidence: 0.7,
	}}
	env.rec.SquadRouter = router
	item := seedQueuedWorkflowItem(t, env, "MILLS-WF-SQUAD")

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	imperative, err := env.store.Workflow.ListRunningImperativeRuns(context.Background())
	if err != nil || len(imperative) != 1 {
		t.Fatalf("imperative runs = %v err=%v, want one", imperative, err)
	}
	if router.calls != 1 || router.last == nil || router.last.ID != item.ID {
		t.Fatalf("router calls=%d last=%+v, want one Pick for the item", router.calls, router.last)
	}
	ev, err := env.store.Events.FirstBySubjectKind(
		context.Background(), "workflow_run", imperative[0].ID, "reconciler.squad_routed",
	)
	if err != nil {
		t.Fatalf("workflow_run attribution event: %v", err)
	}
	if got := ev.Payload["squad_name"]; got != "wf-squad" {
		t.Fatalf("squad_name = %v, want wf-squad", got)
	}
	if got := ev.Payload["lane"]; got != "workflow" {
		t.Fatalf("lane = %v, want workflow", got)
	}
}

// TestReconciler_WorkflowSelectionHoldSkipsFailClosed: a hold reason (disabled
// workflows / invalid selection) skips the item — no DAG fallback, no claim.
func TestReconciler_WorkflowSelectionHoldSkipsFailClosed(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.WorkflowSelector = &fakeWorkflowSelector{reason: "workflow runtime is disabled by policy"}
	item := seedQueuedWorkflowItem(t, env, "MILLS-WF-HOLD")

	res, err := env.rec.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 0 || res.Skipped == 0 {
		t.Fatalf("tick result = %+v, want a skip and no starts", res)
	}
	if env.starter.calls() != 0 {
		t.Fatalf("pipeline starter invoked %d times for a held selection", env.starter.calls())
	}
	fresh, err := env.store.Backlog.Get(context.Background(), item.ID)
	if err != nil || fresh.State != store.BacklogQueued {
		t.Fatalf("item = %+v err=%v, want still queued (held)", fresh, err)
	}
}

// TestReconciler_NoSelectionKeepsDAGPath: a selector returning (nil, "", nil)
// leaves the DAG pipeline path byte-identical.
func TestReconciler_NoSelectionKeepsDAGPath(t *testing.T) {
	env := newRecEnv(t, nil)
	selector := &fakeWorkflowSelector{}
	env.rec.WorkflowSelector = selector
	item := seedQueuedWorkflowItem(t, env, "MILLS-WF-DAG")

	res, err := env.rec.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Started != 1 || selector.calls != 1 {
		t.Fatalf("tick result = %+v selector calls=%d, want 1 start via DAG", res, selector.calls)
	}
	runs, err := env.store.Pipeline.ListByBacklog(context.Background(), item.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("pipeline runs = %d err=%v, want exactly 1 (DAG lane)", len(runs), err)
	}
}

// TestReconciler_ImperativeTerminalSettleFreesItemForReview: after the claimed
// run terminalizes through the lifecycle CAS, the item is escalated (not
// re-queued, not stuck running) and a later tick does not restart it.
func TestReconciler_ImperativeTerminalSettleFreesItemForReview(t *testing.T) {
	env := newRecEnv(t, nil)
	env.rec.WorkflowSelector = &fakeWorkflowSelector{sel: testWorkflowSelection()}
	item := seedQueuedWorkflowItem(t, env, "MILLS-WF-SETTLE-TICK")

	if _, err := env.rec.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	imperative, err := env.store.Workflow.ListRunningImperativeRuns(context.Background())
	if err != nil || len(imperative) != 1 {
		t.Fatalf("imperative runs = %v err=%v", imperative, err)
	}
	run := imperative[0]
	now := time.Now().UTC()
	run.State = store.WorkflowRunDone
	run.EndedAt = &now
	won, err := env.store.Workflow.CompareAndSetWorkflowRunLifecycle(context.Background(), run, store.WorkflowRunRunning)
	if err != nil || !won {
		t.Fatalf("terminal CAS: won=%t err=%v", won, err)
	}

	fresh, err := env.store.Backlog.Get(context.Background(), item.ID)
	if err != nil || fresh.State != store.BacklogEscalated {
		t.Fatalf("item = %+v err=%v, want escalated after settle", fresh, err)
	}
	res, err := env.rec.Tick(context.Background())
	if err != nil {
		t.Fatalf("post-settle tick: %v", err)
	}
	if res.Started != 0 {
		t.Fatalf("post-settle tick started %d runs, want 0 (escalated items wait for a human)", res.Started)
	}
}

package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/worker"
	"github.com/crb2nu/loom/pkg/mills/workflow/registry"
)

// TestRegistryAgentTypeParity pins the leaf registry package's inlined
// agent-type tokens to the worker package's wire constants (the leaf inlines
// them to stay import-cycle-free).
func TestRegistryAgentTypeParity(t *testing.T) {
	if registry.AgentTypeClaudeCode != worker.AgentTypeClaudeCode {
		t.Fatalf("registry claude-code token %q != worker %q", registry.AgentTypeClaudeCode, worker.AgentTypeClaudeCode)
	}
	if registry.AgentTypeCodex != worker.AgentTypeCodex {
		t.Fatalf("registry codex token %q != worker %q", registry.AgentTypeCodex, worker.AgentTypeCodex)
	}
}

func TestResolveItemSelectionSemantics(t *testing.T) {
	r := NewDefaultRegistry()

	// No selection => DAG (nil, nil), regardless of the workflows gate.
	for _, enabled := range []bool{true, false} {
		sel, err := ResolveItemSelection(r, enabled, &store.BacklogItem{ID: "plain"})
		if sel != nil || err != nil {
			t.Fatalf("no-selection resolution = (%v, %v), want (nil, nil)", sel, err)
		}
	}

	// Explicit selection while disabled => ErrWorkflowsDisabled (defer, never
	// silently run the DAG over the author's choice).
	item := &store.BacklogItem{ID: "item-1", Policy: store.ItemPolicy{
		WorkflowTemplate: "implement-gate", WorkflowTemplateVersion: "v1",
	}}
	if _, err := ResolveItemSelection(r, false, item); !errors.Is(err, ErrWorkflowsDisabled) {
		t.Fatalf("disabled resolution error = %v, want ErrWorkflowsDisabled", err)
	}

	// Unknown template fails closed.
	bad := &store.BacklogItem{ID: "item-2", Policy: store.ItemPolicy{
		WorkflowTemplate: "no-such-template", WorkflowTemplateVersion: "v1",
	}}
	if _, err := ResolveItemSelection(r, true, bad); !errors.Is(err, ErrUnknownTemplate) {
		t.Fatalf("unknown-template resolution error = %v, want ErrUnknownTemplate", err)
	}

	// Valid selection freezes the full identity with clamped params.
	item.Policy.WorkflowParams = map[string]float64{"budget_usd": 99}
	sel, err := ResolveItemSelection(r, true, item)
	if err != nil {
		t.Fatalf("valid resolution: %v", err)
	}
	if sel.Engine != store.WorkflowEngineImperative || sel.Template != "implement-gate" ||
		sel.TemplateVersion != "v1" || sel.InterpreterVersion != HostInterpreterVersion {
		t.Fatalf("frozen identity wrong: %+v", sel)
	}
	if !strings.Contains(sel.ParamsJSON, `"content_hash"`) || !strings.Contains(sel.ParamsJSON, `"budget_usd":5`) {
		t.Fatalf("frozen params missing hash or clamped budget: %s", sel.ParamsJSON)
	}
}

// TestRegistryRunExecutesEndToEnd drives a frozen registry run through the
// real interpreter + DAO journal with a fake runner: the rendered program's
// two effects (agent + gate) journal exactly once and the run reaches done;
// a second advance replays with zero new live effects.
func TestRegistryRunExecutesEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dao := st.Workflow
	r := NewDefaultRegistry()

	sel, err := ResolveItemSelection(r, true, &store.BacklogItem{ID: "", Policy: store.ItemPolicy{
		WorkflowTemplate: "implement-gate", WorkflowTemplateVersion: "v1",
		WorkflowParams: map[string]float64{"budget_usd": 0.25},
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	run, err := CreateRunFromSelection(ctx, dao, "wf-reg-e2e", "", sel)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	runner := &fakeRunner{}
	wi := NewWorkflowInterpreter(dao, runner, nil)
	if err := wi.Run(ctx, run); err != nil {
		t.Fatalf("first advance: %v", err)
	}
	fresh, err := dao.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != store.WorkflowRunDone {
		t.Fatalf("run state = %q, want done", fresh.State)
	}
	if runner.runCount != 1 {
		t.Fatalf("live spawns = %d, want 1", runner.runCount)
	}
	if runner.lastReq.BudgetUSD != 0.25 {
		t.Fatalf("spawn budget = %v, want the frozen clamped 0.25", runner.lastReq.BudgetUSD)
	}

	// Replay: a fresh interpreter re-derives the identical script from the
	// frozen row and short-circuits on the journal — no new live effects.
	wi2 := NewWorkflowInterpreter(dao, runner, nil)
	if err := wi2.Run(ctx, fresh); err != nil {
		t.Fatalf("replay advance: %v", err)
	}
	if runner.runCount != 1 {
		t.Fatalf("replay fired a live spawn: count = %d", runner.runCount)
	}
}

// TestFrozenRunIsNeverRerouted proves the S7 selection guard: execution
// derives the program from the run row alone, so editing the backlog item's
// selection after the run started cannot change what replays.
func TestFrozenRunIsNeverRerouted(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dao := st.Workflow
	r := NewDefaultRegistry()

	item := &store.BacklogItem{ID: "", Policy: store.ItemPolicy{
		WorkflowTemplate: "implement-gate", WorkflowTemplateVersion: "v1",
		WorkflowParams: map[string]float64{"budget_usd": 0.5},
	}}
	sel, err := ResolveItemSelection(r, true, item)
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRunFromSelection(ctx, dao, "wf-reg-frozen", "", sel)
	if err != nil {
		t.Fatal(err)
	}
	before, err := r.ScriptFromRun(run)
	if err != nil {
		t.Fatal(err)
	}

	// The author edits the item after the run started: different params,
	// different template. The frozen run's derived script must not move.
	item.Policy.WorkflowParams["budget_usd"] = 5
	item.Policy.WorkflowTemplate = "no-such-template"
	after, err := r.ScriptFromRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("frozen run re-derived a different script after item edits:\n%q\n%q", before, after)
	}
}

// TestInterpreterRefusesNonImperativeEngine proves the engine discriminator:
// a DAG run can never be advanced by the imperative interpreter.
func TestInterpreterRefusesNonImperativeEngine(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	wi := NewWorkflowInterpreter(st.Workflow, &fakeRunner{}, nil)
	err := wi.Run(ctx, &store.WorkflowRun{
		ID: "wf-dag-guard", Engine: store.WorkflowEngineDAG,
		Template: "mills-default-pipeline", State: store.WorkflowRunRunning,
	})
	if err == nil || !strings.Contains(err.Error(), "non-imperative") {
		t.Fatalf("dag-engine advance error = %v, want refusal", err)
	}
}

// TestInterpreterTerminalizesUnknownTemplate: a run frozen to a template the
// binary no longer knows terminalizes (error state) instead of zombie-looping.
func TestInterpreterTerminalizesUnknownTemplate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dao := st.Workflow
	now := time.Now().UTC()
	run := &store.WorkflowRun{
		ID: "wf-reg-unknown", Engine: store.WorkflowEngineImperative,
		Template: "retired-template", TemplateVersion: "v1",
		InterpreterVersion: HostInterpreterVersion,
		State:              store.WorkflowRunRunning, StartedAt: &now,
	}
	if err := dao.CreateWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	wi := NewWorkflowInterpreter(dao, &fakeRunner{}, nil)
	if err := wi.Run(ctx, run); err == nil {
		t.Fatal("unknown template advance succeeded")
	}
	fresh, err := dao.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != store.WorkflowRunError {
		t.Fatalf("run state = %q, want error (terminalized)", fresh.State)
	}
}

// TestRegistryRunGetsSpecAwareWorkPrompt: an S7 registry run's spawn prompt is
// a real work prompt derived from the backlog item — never the canary
// protocol — and degrades to a generic work prompt when the lookup fails.
func TestRegistryRunGetsSpecAwareWorkPrompt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dao := st.Workflow
	r := NewDefaultRegistry()

	sel, err := ResolveItemSelection(r, true, &store.BacklogItem{ID: "", Policy: store.ItemPolicy{
		WorkflowTemplate: "implement-gate", WorkflowTemplateVersion: "v1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRunFromSelection(ctx, dao, "wf-reg-prompt", "", sel)
	if err != nil {
		t.Fatal(err)
	}
	run.BacklogID = "MILLS-PROMPT-1"

	wi := NewWorkflowInterpreter(dao, &fakeRunner{}, nil)
	wi.SetBacklogItemLookup(func(_ context.Context, id string) (string, string, error) {
		if id != "MILLS-PROMPT-1" {
			t.Fatalf("lookup got id %q", id)
		}
		return "Add retry to the fetcher", ".loom/42-spec.md", nil
	})
	prompt := wi.agentPrompt(ctx, run, "implement")
	for _, want := range []string{"Add retry to the fetcher", ".loom/42-spec.md", "Do NOT create merge requests", "commit and push"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("work prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "MILLS_CANARY_OK") || strings.Contains(prompt, "canary protocol") {
		t.Fatalf("registry run received the canary protocol prompt: %s", prompt)
	}

	// Lookup failure degrades to a generic work prompt, still never canary.
	wi.SetBacklogItemLookup(func(context.Context, string) (string, string, error) {
		return "", "", context.DeadlineExceeded
	})
	degraded := wi.agentPrompt(ctx, run, "implement")
	if !strings.Contains(degraded, "Implement the assigned backlog item") || strings.Contains(degraded, "MILLS_CANARY_OK") {
		t.Fatalf("degraded prompt wrong: %s", degraded)
	}
}

// deadlineTestGate implements PolicyGate + RunDeadlineGate for the sweep test.
type deadlineTestGate struct{ maxAge time.Duration }

func (g deadlineTestGate) WorkflowsEnabled() bool   { return true }
func (g deadlineTestGate) MaxRunAge() time.Duration { return g.maxAge }

// TestSchedulerDeadlineTerminalizesWedgedRun: a running imperative run older
// than the policy bound is terminalized as error by the scheduler tick — and
// because the settle rides the lifecycle CAS, a claim-started run's item is
// escalated and its reservation released in the same commit.
func TestSchedulerDeadlineTerminalizesWedgedRun(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dao := st.Workflow
	r := NewDefaultRegistry()

	sel, err := ResolveItemSelection(r, true, &store.BacklogItem{ID: "", Policy: store.ItemPolicy{
		WorkflowTemplate: "implement-gate", WorkflowTemplateVersion: "v1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRunFromSelection(ctx, dao, "wf-reg-wedged", "", sel)
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the start beyond the bound.
	old := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE workflow_runs SET started_at = ? WHERE id = ?`,
		old.Format(time.RFC3339Nano), run.ID); err != nil {
		t.Fatal(err)
	}

	interp := NewWorkflowInterpreter(dao, &fakeRunner{}, nil)
	sched := NewWorkflowScheduler(dao, interp, deadlineTestGate{maxAge: time.Hour}, nil)
	sched.tick(ctx)

	fresh, err := dao.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != store.WorkflowRunError {
		t.Fatalf("wedged run state = %q, want error (deadline terminalized)", fresh.State)
	}

	// A run INSIDE the bound is untouched by the sweep path (it advances).
	run2, err := CreateRunFromSelection(ctx, dao, "wf-reg-fresh", "", sel)
	if err != nil {
		t.Fatal(err)
	}
	sched.tick(ctx)
	fresh2, err := dao.GetWorkflowRun(ctx, run2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh2.State != store.WorkflowRunDone {
		t.Fatalf("in-bound run state = %q, want done (advanced normally)", fresh2.State)
	}
}

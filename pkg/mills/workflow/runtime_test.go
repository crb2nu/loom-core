package workflow

// runtime_test.go is the IN-PROCESS verification for the S6-min imperative
// runtime + scheduler. It is NOT a substitute for the deployed S1c dual-crash
// kill-test (.loom/134 §S1c): it exercises the journal short-circuit + resume
// re-attach against a real temp-file SQLite store and a fake WorkerRunner, but
// it does not crash a real operator pod or a real mobile-hud. The deployed S1c
// remains the authoritative gate.
//
// Coverage:
//   - completion: the canary reaches state='done' and the fake agent ran once.
//   - crash-resume: drive partway, drop the interpreter/scheduler, re-create
//     sharing the SAME store, re-run the tick → the fake's live agent-run count
//     is UNCHANGED for the already-completed step and the run completes once.
//   - paused_at: a paused run is not advanced by the scheduler.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/worker"
)

// fakeRunner is a worker.WorkerRunner that counts live Run invocations and
// returns a deterministic WorkerResult. It also implements WorkerResumer so the
// runtime promotes it (Resume is counted separately). failNext, when set, makes
// the next Run return an error AFTER recording a spawn id (to simulate an
// interrupted dispatch that left a recoverable pending row).
type fakeRunner struct {
	mu          sync.Mutex
	runCount    int
	resumeCount int
	lastKey     string
	failNext    bool
}

func (f *fakeRunner) Run(_ context.Context, req worker.WorkerRequest) (worker.WorkerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCount++
	f.lastKey = req.IdempotencyKey
	res := worker.WorkerResult{
		SpawnID:      worker.DeriveSpawnID(req.IdempotencyKey),
		CostUSD:      0.5,
		CostSource:   worker.CostSourceReal,
		FilesChanged: []string{"a.go"},
		LinesAdded:   3,
	}
	if f.failNext {
		f.failNext = false
		return res, errors.New("simulated dispatch interruption")
	}
	return res, nil
}

func (f *fakeRunner) Resume(_ context.Context, key string) (worker.WorkerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCount++
	return worker.WorkerResult{
		SpawnID:    worker.DeriveSpawnID(key),
		CostUSD:    0.5,
		CostSource: worker.CostSourceReal,
	}, nil
}

func (f *fakeRunner) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCount
}

func (f *fakeRunner) resumes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumeCount
}

// alwaysOn is a PolicyGate that always enables workflows (the canary window).
type alwaysOn struct{}

func (alwaysOn) WorkflowsEnabled() bool { return true }

func newRuntimeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: t.TempDir() + "/mills.db"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedBacklog inserts a minimal backlog item so the workflow_runs.backlog_id FK
// resolves (the canary references a real backlog item, mirroring production).
func seedBacklog(t *testing.T, st *store.Store, id string) {
	t.Helper()
	err := st.Backlog.Put(context.Background(), &store.BacklogItem{
		ID:       id,
		Title:    "canary " + id,
		State:    store.BacklogQueued,
		Priority: store.P2,
	})
	if err != nil {
		t.Fatalf("seed backlog %s: %v", id, err)
	}
}

// TestRuntime_CanaryCompletes drives one tick to completion and asserts the
// canary reaches done with exactly one live agent spawn.
func TestRuntime_CanaryCompletes(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	fr := &fakeRunner{}

	seedBacklog(t, st, "backlog-1")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-1", "backlog-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	interp := NewWorkflowInterpreter(st.Workflow, fr, nil)
	sched := NewWorkflowScheduler(st.Workflow, interp, alwaysOn{}, nil)
	sched.tick(ctx)

	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.WorkflowRunDone {
		t.Fatalf("expected run state done, got %q", got.State)
	}
	if n := fr.runs(); n != 1 {
		t.Fatalf("expected exactly 1 live agent spawn, got %d", n)
	}

	// Both effects (agent + gate) recorded as success steps.
	steps, err := st.Workflow.ListByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	var agentSteps, successSteps int
	for _, s := range steps {
		if s.Status == store.WorkflowStepSuccess {
			successSteps++
		}
		if s.EventType == store.WorkflowEventSpawnRequested {
			agentSteps++
			if s.SpawnID == "" {
				t.Fatalf("agent step missing spawn_id")
			}
			if s.CostSource != store.WorkflowCostReal {
				t.Fatalf("agent step cost_source = %q, want real", s.CostSource)
			}
		}
	}
	if successSteps != 2 {
		t.Fatalf("expected 2 success steps (agent+gate), got %d", successSteps)
	}
	if agentSteps != 1 {
		t.Fatalf("expected 1 agent step, got %d", agentSteps)
	}
}

// TestRuntime_CrashResume drives partway (agent step recorded, run interrupted
// before gate completes), drops the interpreter+scheduler, then re-creates a
// fresh interpreter+scheduler sharing the SAME store and re-runs. The already-
// completed agent step must NOT re-spawn (journal short-circuit), and the run
// must complete exactly once.
func TestRuntime_CrashResume(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)

	seedBacklog(t, st, "backlog-2")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-2", "backlog-2")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// --- PASS 1: an executor that completes the agent step, then forces an
	// error on the gate step so the run is interrupted mid-flight. The agent
	// success row is durably committed; the gate never reaches success.
	fr1 := &fakeRunner{}
	interp1 := NewWorkflowInterpreter(st.Workflow, fr1, nil)
	// Install an executor that runs the agent for real but trips on the gate so
	// PASS 1 ends with the agent committed and the gate un-run.
	journal := NewDAOJournalFromDAO(ctx, st.Workflow)
	host := NewEffectHost(run.ID, journal)
	base := interp1.effectExec(ctx, run)
	host.SetEffectExec(func(stepKey, primKind string, args map[string]any, seq int64) (EffectResult, error) {
		if primKind == "gate" {
			return EffectResult{}, errors.New("simulated crash before gate completes")
		}
		return base(stepKey, primKind, args, seq)
	})
	if err := host.Run(canaryScript); err == nil {
		t.Fatalf("expected PASS 1 to error on the gate step")
	}
	if n := fr1.runs(); n != 1 {
		t.Fatalf("PASS 1: expected 1 live agent spawn, got %d", n)
	}
	// Run is still 'running' (interp1.Run was not used; the run was never
	// transitioned). Confirm the agent step is durably success.
	agentStep := findAgentStep(t, ctx, st, run.ID)
	if agentStep.Status != store.WorkflowStepSuccess {
		t.Fatalf("PASS 1: agent step not durably success: %q", agentStep.Status)
	}

	// --- PASS 2: drop interp1/host (the "crash"); a FRESH interpreter +
	// scheduler over the SAME store re-runs the tick. The completed agent step
	// short-circuits from the journal (no new spawn); the gate now succeeds; the
	// run completes exactly once.
	fr2 := &fakeRunner{}
	interp2 := NewWorkflowInterpreter(st.Workflow, fr2, nil)
	sched2 := NewWorkflowScheduler(st.Workflow, interp2, alwaysOn{}, nil)
	sched2.tick(ctx)

	if n := fr2.runs(); n != 0 {
		t.Fatalf("PASS 2: completed agent step re-spawned (journal short-circuit failed): live=%d want 0", n)
	}
	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.WorkflowRunDone {
		t.Fatalf("PASS 2: expected run done, got %q", got.State)
	}

	// A third tick is fully idempotent: run is terminal, list excludes it.
	sched2.tick(ctx)
	if n := fr2.runs(); n != 0 {
		t.Fatalf("PASS 3: terminal run re-advanced: live=%d want 0", n)
	}
}

// TestRuntime_ResumeReattaches drives the agent step to a durable 'pending'
// state carrying a spawn_id (an interrupted dispatch), then re-runs sharing the
// same store. The fresh interpreter must RE-ATTACH via WorkerResumer.Resume
// keyed on the deterministic idempotency key rather than issuing a fresh Run —
// this is the exactly-once resume contract (S2b/S2c) the runtime drives.
func TestRuntime_ResumeReattaches(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	seedBacklog(t, st, "backlog-5")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-5", "backlog-5")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// PASS 1: the runner fails its first Run but returns a spawn id, so the
	// runtime records a pending agent step carrying that spawn_id.
	fr1 := &fakeRunner{failNext: true}
	interp1 := NewWorkflowInterpreter(st.Workflow, fr1, nil)
	sched1 := NewWorkflowScheduler(st.Workflow, interp1, alwaysOn{}, nil)
	sched1.tick(ctx)

	if n := fr1.runs(); n != 1 {
		t.Fatalf("PASS 1: expected 1 Run attempt, got %d", n)
	}
	agentStep := findAgentStep(t, ctx, st, run.ID)
	if agentStep.Status != store.WorkflowStepPending {
		t.Fatalf("PASS 1: expected pending agent step, got %q", agentStep.Status)
	}
	if agentStep.SpawnID == "" {
		t.Fatalf("PASS 1: expected pending step to carry a spawn_id for re-attach")
	}

	// PASS 2: a fresh interpreter sharing the SAME store re-attaches via Resume
	// (not a fresh Run) because the step is pending-with-spawn-id.
	fr2 := &fakeRunner{}
	interp2 := NewWorkflowInterpreter(st.Workflow, fr2, nil)
	sched2 := NewWorkflowScheduler(st.Workflow, interp2, alwaysOn{}, nil)
	sched2.tick(ctx)

	if n := fr2.runs(); n != 0 {
		t.Fatalf("PASS 2: re-dispatched a fresh Run instead of re-attaching: Run=%d want 0", n)
	}
	if n := fr2.resumes(); n != 1 {
		t.Fatalf("PASS 2: expected exactly 1 Resume re-attach, got %d", n)
	}
	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.WorkflowRunDone {
		t.Fatalf("PASS 2: expected run done after resume, got %q", got.State)
	}
}

// TestRuntime_PausedNotAdvanced asserts the scheduler does not advance a run
// whose paused_at is set, even though it is engine=imperative. This is the §5
// between-step fast-stop safety property.
func TestRuntime_PausedNotAdvanced(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	fr := &fakeRunner{}

	seedBacklog(t, st, "backlog-3")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-3", "backlog-3")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Pause the run: set paused_at while keeping it state='running' so it still
	// matches ListRunningImperativeRuns and the between-step paused_at check is
	// what stops it (not the state filter).
	now := time.Now().UTC()
	run.PausedAt = &now
	if err := st.Workflow.PutWorkflowRun(ctx, run); err != nil {
		t.Fatalf("pause run: %v", err)
	}

	interp := NewWorkflowInterpreter(st.Workflow, fr, nil)
	sched := NewWorkflowScheduler(st.Workflow, interp, alwaysOn{}, nil)
	sched.tick(ctx)

	if n := fr.runs(); n != 0 {
		t.Fatalf("paused run advanced (paused_at not honored): live=%d want 0", n)
	}
	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State == store.WorkflowRunDone {
		t.Fatalf("paused run reached done; should not advance")
	}
}

// TestRuntime_DisabledFlagNoOp asserts the scheduler is inert when the policy
// gate reports workflows disabled (the default-OFF flag).
func TestRuntime_DisabledFlagNoOp(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	fr := &fakeRunner{}

	seedBacklog(t, st, "backlog-4")
	if _, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-4", "backlog-4"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	interp := NewWorkflowInterpreter(st.Workflow, fr, nil)
	sched := NewWorkflowScheduler(st.Workflow, interp, PolicyGateFunc(func() bool { return false }), nil)
	sched.tick(ctx)

	if n := fr.runs(); n != 0 {
		t.Fatalf("disabled flag still ran effects: live=%d want 0", n)
	}
}

// TestDeriveStepIdempotencyKey_Deterministic confirms the idempotency key is
// stable across re-derivation (drives exactly-once on resume) and unique per
// (run, step, args).
func TestDeriveStepIdempotencyKey_Deterministic(t *testing.T) {
	args := map[string]any{"_0": "implement", "model": "claude-code", "budget_usd": int64(1)}
	k1 := DeriveStepIdempotencyKey("run-1", "root/agent~abc#0", args)
	k2 := DeriveStepIdempotencyKey("run-1", "root/agent~abc#0", args)
	if k1 != k2 {
		t.Fatalf("idempotency key not stable: %q != %q", k1, k2)
	}
	// Different step → different key.
	if DeriveStepIdempotencyKey("run-1", "root/agent~abc#1", args) == k1 {
		t.Fatalf("distinct steps produced identical idempotency keys")
	}
	// Different run → different key.
	if DeriveStepIdempotencyKey("run-2", "root/agent~abc#0", args) == k1 {
		t.Fatalf("distinct runs produced identical idempotency keys")
	}
}

func findAgentStep(t *testing.T, ctx context.Context, st *store.Store, runID string) *store.WorkflowStep {
	t.Helper()
	steps, err := st.Workflow.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	for _, s := range steps {
		if s.EventType == store.WorkflowEventSpawnRequested {
			return s
		}
	}
	t.Fatalf("no agent step found for run %s", runID)
	return nil
}

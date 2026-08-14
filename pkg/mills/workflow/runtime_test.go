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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	lastReq     worker.WorkerRequest
	failNext    bool
	resumeErr   error // when set, Resume returns this error
}

type blockingConcurrentRunner struct {
	mu        sync.Mutex
	started   chan string
	release   chan struct{}
	active    int
	maxActive int
}

func newBlockingConcurrentRunner() *blockingConcurrentRunner {
	return &blockingConcurrentRunner{
		started: make(chan string, 8),
		release: make(chan struct{}),
	}
}

func (r *blockingConcurrentRunner) Run(ctx context.Context, req worker.WorkerRequest) (worker.WorkerResult, error) {
	r.mu.Lock()
	r.active++
	if r.active > r.maxActive {
		r.maxActive = r.active
	}
	r.mu.Unlock()
	r.started <- req.BacklogID
	select {
	case <-r.release:
	case <-ctx.Done():
		return worker.WorkerResult{}, ctx.Err()
	}
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return worker.WorkerResult{SpawnID: worker.DeriveSpawnID(req.IdempotencyKey)}, nil
}

func (r *blockingConcurrentRunner) maxConcurrency() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxActive
}

func (f *fakeRunner) Run(_ context.Context, req worker.WorkerRequest) (worker.WorkerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCount++
	f.lastKey = req.IdempotencyKey
	f.lastReq = req
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
	if f.resumeErr != nil {
		return worker.WorkerResult{SpawnID: worker.DeriveSpawnID(key)}, f.resumeErr
	}
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

func TestWorkflowScheduler_AdvancesIndependentRunsConcurrently(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	runner := newBlockingConcurrentRunner()
	for i := 1; i <= 2; i++ {
		backlogID := fmt.Sprintf("backlog-concurrent-%d", i)
		seedBacklog(t, st, backlogID)
		if _, err := CreateImperativeRun(ctx, st.Workflow, fmt.Sprintf("wf-concurrent-%d", i), backlogID); err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
	}
	sched := NewWorkflowScheduler(st.Workflow, NewWorkflowInterpreter(st.Workflow, runner, nil), alwaysOn{}, nil)
	sched.MaxConcurrentRuns = 2
	tickDone := make(chan struct{})
	go func() {
		sched.tick(ctx)
		close(tickDone)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-runner.started:
		case <-time.After(300 * time.Millisecond):
			close(runner.release)
			<-tickDone
			t.Fatalf("only %d workflow run(s) started before the first was released", i)
		}
	}
	close(runner.release)
	select {
	case <-tickDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent workflow tick did not finish")
	}
	if got := runner.maxConcurrency(); got != 2 {
		t.Fatalf("max concurrent workflow runs = %d, want 2", got)
	}
}

func TestRuntime_ManualFailIsTerminalFenceForLateCompletion(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	seedBacklog(t, st, "backlog-manual-fence")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-manual-fence", "backlog-manual-fence")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	interp := NewWorkflowInterpreter(st.Workflow, &fakeRunner{}, nil)
	runtimeLoaded := make(chan struct{})
	releaseRuntime := make(chan struct{})
	interp.beforeTransitionCAS = func() {
		close(runtimeLoaded)
		<-releaseRuntime
	}
	runtimeDone := make(chan struct{})
	go func() {
		interp.transition(ctx, run, store.WorkflowRunDone)
		close(runtimeDone)
	}()
	select {
	case <-runtimeLoaded:
	case <-time.After(time.Second):
		t.Fatal("runtime did not reach pre-CAS interleaving point")
	}

	failed, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run for manual fail: %v", err)
	}
	failed.State = store.WorkflowRunError
	now := time.Now().UTC()
	failed.EndedAt = &now
	updated, err := st.Workflow.CompareAndSetWorkflowRunLifecycle(ctx, failed, store.WorkflowRunRunning)
	if err != nil || !updated {
		t.Fatalf("manual fail CAS: updated=%t err=%v", updated, err)
	}
	close(releaseRuntime)
	select {
	case <-runtimeDone:
	case <-time.After(time.Second):
		t.Fatal("runtime transition did not finish")
	}
	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if got.State != store.WorkflowRunError || run.State != store.WorkflowRunError {
		t.Fatalf("late completion overwrote manual fail: stored=%q in_memory=%q", got.State, run.State)
	}
}

// TestRuntime_AgentSpawnContext is the regression test for the S1c blocker:
// execAgent built a WorkerRequest with no Project/Branch, so the live HUD spawn
// API (which hard-requires both) rejected every agent() spawn with
// "SpawnRequest.Project required" and no pod was ever created. The in-process
// fake runner didn't validate fields, so the bug only surfaced deployed.
//
// It asserts (a) the configured default project flows through, (b) a per-run
// deterministic branch is set, and (c) an unconfigured interpreter still falls
// back to a non-empty "loom-core" project rather than the empty value that
// tripped validation.
func TestRuntime_AgentSpawnContext(t *testing.T) {
	ctx := context.Background()

	t.Run("configured defaults flow to the spawn request", func(t *testing.T) {
		st := newRuntimeStore(t)
		fr := &fakeRunner{}
		seedBacklog(t, st, "backlog-ctx")
		run, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-ctx", "backlog-ctx")
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		interp := NewWorkflowInterpreter(st.Workflow, fr, nil)
		interp.SetSpawnDefaults("services/loom-core", "main")
		NewWorkflowScheduler(st.Workflow, interp, alwaysOn{}, nil).tick(ctx)

		fr.mu.Lock()
		req := fr.lastReq
		fr.mu.Unlock()
		if req.Project != "services/loom-core" {
			t.Fatalf("spawn Project = %q, want services/loom-core", req.Project)
		}
		if req.BaseBranch != "main" {
			t.Fatalf("spawn BaseBranch = %q, want main", req.BaseBranch)
		}
		if want := "mills-wf/" + run.ID; req.Branch != want {
			t.Fatalf("spawn Branch = %q, want %q", req.Branch, want)
		}
		if req.CompletionHoldSeconds != CanaryHoldSeconds {
			t.Fatalf("spawn CompletionHoldSeconds = %d, want %d", req.CompletionHoldSeconds, CanaryHoldSeconds)
		}
	})

	t.Run("unset project falls back to loom-core (never empty)", func(t *testing.T) {
		st := newRuntimeStore(t)
		fr := &fakeRunner{}
		seedBacklog(t, st, "backlog-fallback")
		if _, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-fb", "backlog-fallback"); err != nil {
			t.Fatalf("create run: %v", err)
		}
		// No SetSpawnDefaults call: zero-value interpreter.
		interp := NewWorkflowInterpreter(st.Workflow, fr, nil)
		NewWorkflowScheduler(st.Workflow, interp, alwaysOn{}, nil).tick(ctx)

		fr.mu.Lock()
		req := fr.lastReq
		fr.mu.Unlock()
		if req.Project != "loom-core" {
			t.Fatalf("fallback spawn Project = %q, want loom-core", req.Project)
		}
		if req.Branch == "" {
			t.Fatalf("spawn Branch must never be empty (HUD spawn API requires it)")
		}
	})
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

func TestRuntime_PendingWithoutSpawnIDLogsDeterministicRedispatch(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	seedBacklog(t, st, "backlog-proof-log")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-proof-log", "backlog-proof-log")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	args := map[string]any{"_0": "implement", "model": "claude-code", "budget_usd": 0.5}
	stepKey := "root/agent-proof#0"
	if _, err := st.Workflow.AppendStep(ctx, &store.WorkflowStep{
		RunID: run.ID, StepKey: stepKey, EventType: store.WorkflowEventSpawnRequested,
		CallHash: canonicalCallHash("agent", args), Status: store.WorkflowStepPending,
	}); err != nil {
		t.Fatalf("seed pending step: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	fr := &fakeRunner{}
	interp := NewWorkflowInterpreter(st.Workflow, fr, logger)
	if _, err := interp.execAgent(ctx, run, stepKey, args); err != nil {
		t.Fatalf("execAgent: %v", err)
	}
	wantID := worker.DeriveSpawnID(DeriveStepIdempotencyKey(run.ID, stepKey, args))
	if text := logs.String(); !strings.Contains(text, "re-dispatching pending spawn with deterministic key") || !strings.Contains(text, wantID) {
		t.Fatalf("missing attributed deterministic redispatch log: %s", text)
	}
}

// TestRuntime_TerminalSpawnFailureFailsRun reproduces the 2026-07-09 wf-canary
// zombie loop: a pending-with-spawn-id step whose spawn reached a terminal
// non-completed status (Resume returns ErrSpawnTerminalFailure — the HUD
// "pod not found during reconciliation" shape). The run must transition to
// state='error' (terminal) instead of staying 'running' and re-attaching to
// the same dead spawn on every tick forever.
func TestRuntime_TerminalSpawnFailureFailsRun(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	seedBacklog(t, st, "backlog-6")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-6", "backlog-6")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// PASS 1: interrupted dispatch leaves a durable pending step + spawn_id.
	fr1 := &fakeRunner{failNext: true}
	sched1 := NewWorkflowScheduler(st.Workflow, NewWorkflowInterpreter(st.Workflow, fr1, nil), alwaysOn{}, nil)
	sched1.tick(ctx)
	if step := findAgentStep(t, ctx, st, run.ID); step.Status != store.WorkflowStepPending || step.SpawnID == "" {
		t.Fatalf("PASS 1: expected pending step with spawn_id, got status=%q spawn_id=%q", step.Status, step.SpawnID)
	}

	// PASS 2: resume re-attaches, but the spawn is terminally failed. The
	// error is wrapped through the resume-spawn chain exactly like production
	// (execAgent wraps with %w; the Starlark EvalError preserves Unwrap).
	fr2 := &fakeRunner{resumeErr: fmt.Errorf("hud spawn spawn-dead status=failed: pod not found during reconciliation: %w", worker.ErrSpawnTerminalFailure)}
	sched2 := NewWorkflowScheduler(st.Workflow, NewWorkflowInterpreter(st.Workflow, fr2, nil), alwaysOn{}, nil)
	sched2.tick(ctx)

	if n := fr2.resumes(); n != 1 {
		t.Fatalf("PASS 2: expected 1 Resume attempt, got %d", n)
	}
	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.WorkflowRunError {
		t.Fatalf("PASS 2: expected run state 'error' after terminal spawn failure, got %q", got.State)
	}

	// PASS 3: the run is terminal, so the scheduler never touches it again —
	// this is the zombie-loop regression guard.
	sched2.tick(ctx)
	if n := fr2.resumes(); n != 1 {
		t.Fatalf("PASS 3: terminal run re-advanced (zombie loop): resumes=%d want 1", n)
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

func TestCreateImperativeRunDoesNotReviveExistingTerminalID(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	seedBacklog(t, st, "backlog-stable-id")
	run, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-stable-id", "backlog-stable-id")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	now := time.Now().UTC()
	run.State = store.WorkflowRunDone
	run.EndedAt = &now
	if err := st.Workflow.PutWorkflowRun(ctx, run); err != nil {
		t.Fatalf("terminalize: %v", err)
	}
	if _, err := CreateImperativeRun(ctx, st.Workflow, run.ID, run.BacklogID); !errors.Is(err, store.ErrWorkflowRunExists) {
		t.Fatalf("duplicate create error = %v, want ErrWorkflowRunExists", err)
	}
	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil || got.State != store.WorkflowRunDone {
		t.Fatalf("duplicate create revived terminal run: state=%q err=%v", got.State, err)
	}
}

func TestCreateImperativeRunPersistsPortableCanaryAgentType(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)

	legacy, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-legacy-agent", "")
	if err != nil {
		t.Fatalf("create legacy-default run: %v", err)
	}
	if got, err := CanaryAgentTypeFromRun(legacy); err != nil || got != worker.AgentTypeClaudeCode {
		t.Fatalf("legacy-default agent type = %q, err=%v; want %q", got, err, worker.AgentTypeClaudeCode)
	}
	if legacy.WorkflowParams == "" {
		t.Fatal("new legacy-default run did not persist its agent choice")
	}

	codex, err := CreateImperativeRunWithAgentType(ctx, st.Workflow, "wf-canary-codex-agent", "", worker.AgentTypeCodex)
	if err != nil {
		t.Fatalf("create codex run: %v", err)
	}
	restored, err := st.Workflow.GetWorkflowRun(ctx, codex.ID)
	if err != nil {
		t.Fatalf("restore codex run: %v", err)
	}
	if got, err := CanaryAgentTypeFromRun(restored); err != nil || got != worker.AgentTypeCodex {
		t.Fatalf("restored agent type = %q, err=%v; want %q", got, err, worker.AgentTypeCodex)
	}
}

func TestRuntime_PortableAgentChoiceIsStableAcrossResume(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	run, err := CreateImperativeRunWithAgentType(ctx, st.Workflow, "wf-canary-codex-resume", "", worker.AgentTypeCodex)
	if err != nil {
		t.Fatalf("create codex run: %v", err)
	}

	firstRunner := &fakeRunner{failNext: true}
	NewWorkflowScheduler(st.Workflow, NewWorkflowInterpreter(st.Workflow, firstRunner, nil), alwaysOn{}, nil).tick(ctx)
	firstRunner.mu.Lock()
	firstReq := firstRunner.lastReq
	firstRunner.mu.Unlock()
	if firstReq.AgentType != worker.AgentTypeCodex || firstReq.Model != worker.AgentTypeCodex {
		t.Fatalf("first spawn used agent/model %q/%q, want codex/codex", firstReq.AgentType, firstReq.Model)
	}
	pending := findAgentStep(t, ctx, st, run.ID)
	wantHash := canonicalCallHash("agent", map[string]any{
		"_0": "implement", "model": worker.AgentTypeCodex, "budget_usd": int64(1),
	})
	if pending.CallHash != wantHash {
		t.Fatalf("codex call hash = %q, want %q", pending.CallHash, wantHash)
	}

	restored, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("restore run: %v", err)
	}
	secondRunner := &fakeRunner{}
	if err := NewWorkflowInterpreter(st.Workflow, secondRunner, nil).Run(ctx, restored); err != nil {
		t.Fatalf("resume restored codex run: %v", err)
	}
	if secondRunner.runs() != 0 || secondRunner.resumes() != 1 {
		t.Fatalf("resume dispatch counts: runs=%d resumes=%d; want 0/1", secondRunner.runs(), secondRunner.resumes())
	}
	if restored.State != store.WorkflowRunDone {
		t.Fatalf("restored run state = %q, want done", restored.State)
	}
}

func TestPortableCanaryAgentTypeValidationFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	for _, params := range []string{`{}`, `null`, `{"agent_type":""}`} {
		if got, err := CanaryAgentTypeFromRun(&store.WorkflowRun{ID: "wf-invalid-params", WorkflowParams: params}); err == nil {
			t.Fatalf("invalid persisted params %s resolved to %q", params, got)
		}
	}
	if _, err := CreateImperativeRunWithAgentType(ctx, st.Workflow, "wf-canary-invalid-agent", "", "gemini"); err == nil {
		t.Fatal("unsupported agent type was accepted")
	}
	if _, err := st.Workflow.GetWorkflowRun(ctx, "wf-canary-invalid-agent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unsupported agent type created a run: %v", err)
	}

	now := time.Now().UTC()
	corrupt := &store.WorkflowRun{
		ID: "wf-canary-corrupt-agent", Engine: store.WorkflowEngineImperative,
		Template: "workflow-canary", TemplateVersion: CanaryTemplateVersion, InterpreterVersion: HostInterpreterVersion,
		WorkflowParams: `{"agent_type":"gemini"}`, State: store.WorkflowRunRunning, StartedAt: &now,
	}
	if err := st.Workflow.CreateWorkflowRun(ctx, corrupt); err != nil {
		t.Fatalf("seed corrupt run: %v", err)
	}
	runner := &fakeRunner{}
	if err := NewWorkflowInterpreter(st.Workflow, runner, nil).Run(ctx, corrupt); err == nil {
		t.Fatal("runtime accepted unsupported persisted agent type")
	}
	if runner.runs() != 0 {
		t.Fatalf("runtime dispatched %d effects for unsupported persisted agent type", runner.runs())
	}
	stored, err := st.Workflow.GetWorkflowRun(ctx, corrupt.ID)
	if err != nil {
		t.Fatalf("reload invalid run: %v", err)
	}
	if stored.State != store.WorkflowRunError {
		t.Fatalf("invalid immutable agent choice left a retrying run in state %q", stored.State)
	}
}

func TestAgentPromptDelegatesCrashHoldToDriverWithoutMutableEffects(t *testing.T) {
	wi := &WorkflowInterpreter{}
	run := &store.WorkflowRun{
		ID:              "wf-canary-hold",
		BacklogID:       "MILLS-42",
		Template:        CanaryTemplateName,
		TemplateVersion: CanaryTemplateVersion,
	}
	prompt := wi.agentPrompt(context.Background(), run, "implement")
	for _, required := range []string{
		"wf-canary-hold", "MILLS-42", "do not edit files or invoke tools",
		"spawn driver owns the bounded completion hold", "MILLS_CANARY_OK",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("canary prompt missing %q: %s", required, prompt)
		}
	}
	for _, forbidden := range []string{"sleep 90", "shell tool exactly once", "yielded or still-running"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("canary prompt retains model-owned hold instruction %q: %s", forbidden, prompt)
		}
	}
}

func TestAgentPromptPreservesLegacyV1ReplayHold(t *testing.T) {
	wi := &WorkflowInterpreter{}
	run := &store.WorkflowRun{
		ID:              "wf-canary-v1-replay",
		Template:        CanaryTemplateName,
		TemplateVersion: legacyCanaryTemplateVersion,
	}
	prompt := wi.agentPrompt(context.Background(), run, "implement")
	for _, required := range []string{
		fmt.Sprintf("sleep %d", CanaryHoldSeconds),
		"shell tool exactly once",
		"wait until it actually exits",
		"MILLS_CANARY_OK",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("legacy v1 prompt missing %q: %s", required, prompt)
		}
	}
	if strings.Contains(prompt, "spawn driver owns") {
		t.Fatalf("legacy v1 prompt unexpectedly uses v2 driver contract: %s", prompt)
	}
	if got := canaryCompletionHoldSeconds(run); got != 0 {
		t.Fatalf("legacy v1 driver hold = %d, want 0", got)
	}
}

func TestCanaryCompletionHoldSecondsIsTemplateVersionScoped(t *testing.T) {
	current := &store.WorkflowRun{Template: CanaryTemplateName, TemplateVersion: CanaryTemplateVersion}
	if got := canaryCompletionHoldSeconds(current); got != CanaryHoldSeconds {
		t.Fatalf("current canary hold = %d, want %d", got, CanaryHoldSeconds)
	}
	for _, run := range []*store.WorkflowRun{
		nil,
		{Template: CanaryTemplateName, TemplateVersion: legacyCanaryTemplateVersion},
		{Template: "workflow-production", TemplateVersion: CanaryTemplateVersion},
	} {
		if got := canaryCompletionHoldSeconds(run); got != 0 {
			t.Fatalf("non-current canary hold = %d, want 0 for %#v", got, run)
		}
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

// ---------------------------------------------------------------------------
// S6-full merging canary: the single journaled merge effect must land exactly
// once across replay and crash-resume, and must fail closed when unwired.
// ---------------------------------------------------------------------------

// fakeMerger simulates the operator's idempotent canary merger. landed models
// GitLab's durable merged state: once true, every further call converges via
// the AlreadyMerged fast-path instead of merging again. failAfterLanding
// simulates the crash-adjacent partial: the merge lands remotely but the
// executor errors before the journal can record success.
type fakeMerger struct {
	mu               sync.Mutex
	calls            int
	effectiveMerges  int
	landed           bool
	failAfterLanding bool
}

func (f *fakeMerger) MergeCanary(_ context.Context, runID string) (CanaryMergeOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.landed {
		return CanaryMergeOutcome{MRIID: 77, SourceBranch: "mills-wf-merge/" + runID, AlreadyMerged: true}, nil
	}
	f.landed = true
	f.effectiveMerges++
	if f.failAfterLanding {
		f.failAfterLanding = false
		return CanaryMergeOutcome{}, fmt.Errorf("simulated crash after merge landed")
	}
	return CanaryMergeOutcome{MRIID: 77, MergeCommitSHA: "abc123", SourceBranch: "mills-wf-merge/" + runID}, nil
}

func (f *fakeMerger) stats() (calls, effective int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.effectiveMerges
}

func createMergingRun(t *testing.T, st *store.Store, id string) *store.WorkflowRun {
	t.Helper()
	seedBacklog(t, st, "backlog-"+id)
	run, err := CreateImperativeRunWithOptions(context.Background(), st.Workflow, id, "backlog-"+id, "", true)
	if err != nil {
		t.Fatalf("create merging run: %v", err)
	}
	if run.TemplateVersion != CanaryMergingTemplateVersion {
		t.Fatalf("merging run stamped version %q, want %q", run.TemplateVersion, CanaryMergingTemplateVersion)
	}
	return run
}

func countMergeSuccessRows(t *testing.T, st *store.Store, runID string) int {
	t.Helper()
	steps, err := st.Workflow.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	n := 0
	for _, s := range steps {
		if s.Status == store.WorkflowStepSuccess && strings.Contains(s.StepKey, "merge~") {
			n++
		}
	}
	return n
}

func TestRuntime_MergingCanaryMergesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	fr := &fakeRunner{}
	fm := &fakeMerger{}
	run := createMergingRun(t, st, "wf-canary-merge-1")

	interp := NewWorkflowInterpreter(st.Workflow, fr, nil)
	interp.SetMergeExecutor(fm)
	NewWorkflowScheduler(st.Workflow, interp, alwaysOn{}, nil).tick(ctx)

	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.WorkflowRunDone {
		t.Fatalf("expected done, got %q", got.State)
	}
	if calls, effective := fm.stats(); calls != 1 || effective != 1 {
		t.Fatalf("merge executor calls=%d effective=%d, want 1/1", calls, effective)
	}
	if n := countMergeSuccessRows(t, st, run.ID); n != 1 {
		t.Fatalf("merge success rows = %d, want exactly 1", n)
	}

	// Replay of the completed journal must short-circuit in read-through
	// without re-entering the executor.
	fresh, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if err := interp.Run(ctx, fresh); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if calls, effective := fm.stats(); calls != 1 || effective != 1 {
		t.Fatalf("replay re-entered the merge executor: calls=%d effective=%d", calls, effective)
	}
	if n := countMergeSuccessRows(t, st, run.ID); n != 1 {
		t.Fatalf("replay duplicated the merge success row: %d", n)
	}
}

// The crash-adjacent partial: the merge lands remotely but the process dies
// before the success row is recorded (executor error leaves the pending row).
// The resume must re-enter the idempotent executor, converge on the already-
// landed merge, and record exactly one success — PASS-3 in-process.
func TestRuntime_MergingCanaryCrashBetweenMergeAndRecordMergesOnce(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	fr := &fakeRunner{}
	fm := &fakeMerger{failAfterLanding: true}
	run := createMergingRun(t, st, "wf-canary-merge-2")

	interp := NewWorkflowInterpreter(st.Workflow, fr, nil)
	interp.SetMergeExecutor(fm)
	sched := NewWorkflowScheduler(st.Workflow, interp, alwaysOn{}, nil)
	sched.tick(ctx)

	mid, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if mid.State == store.WorkflowRunDone {
		t.Fatal("run completed despite simulated crash in the merge effect")
	}
	if n := countMergeSuccessRows(t, st, run.ID); n != 0 {
		t.Fatalf("interrupted merge recorded %d success rows, want 0", n)
	}

	// "Restart": a fresh interpreter (cold caches) resumes the same journal.
	interp2 := NewWorkflowInterpreter(st.Workflow, fr, nil)
	interp2.SetMergeExecutor(fm)
	NewWorkflowScheduler(st.Workflow, interp2, alwaysOn{}, nil).tick(ctx)

	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.WorkflowRunDone {
		t.Fatalf("resumed run state = %q, want done", got.State)
	}
	calls, effective := fm.stats()
	if effective != 1 {
		t.Fatalf("PASS-3 violated: effective merges = %d, want exactly 1 (calls=%d)", effective, calls)
	}
	if calls != 2 {
		t.Fatalf("resume should re-enter the idempotent executor exactly once more: calls=%d", calls)
	}
	if n := countMergeSuccessRows(t, st, run.ID); n != 1 {
		t.Fatalf("merge success rows = %d, want exactly 1", n)
	}
}

func TestRuntime_MergeWithoutExecutorFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)
	run := createMergingRun(t, st, "wf-canary-merge-3")

	interp := NewWorkflowInterpreter(st.Workflow, &fakeRunner{}, nil)
	NewWorkflowScheduler(st.Workflow, interp, alwaysOn{}, nil).tick(ctx)

	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State == store.WorkflowRunDone {
		t.Fatal("merge() without a configured executor must not pass")
	}
	if n := countMergeSuccessRows(t, st, run.ID); n != 0 {
		t.Fatalf("unwired merge recorded %d success rows, want 0", n)
	}
}

func TestCanaryMergingIdentity(t *testing.T) {
	ctx := context.Background()
	st := newRuntimeStore(t)

	run := createMergingRun(t, st, "wf-canary-merge-4")
	script, err := canaryScriptFromRun(run)
	if err != nil {
		t.Fatalf("derive merging script: %v", err)
	}
	if !strings.Contains(script, "merge('canary')") {
		t.Fatalf("merging script missing merge effect:\n%s", script)
	}

	seedBacklog(t, st, "backlog-plain")
	plain, err := CreateImperativeRun(ctx, st.Workflow, "wf-canary-plain-1", "backlog-plain")
	if err != nil {
		t.Fatalf("create plain run: %v", err)
	}
	plainScript, err := canaryScriptFromRun(plain)
	if err != nil {
		t.Fatalf("derive plain script: %v", err)
	}
	if strings.Contains(plainScript, "merge(") {
		t.Fatalf("non-merging script gained a merge effect:\n%s", plainScript)
	}

	// Tampered identity fails closed: v3 version without merging params...
	tampered := *run
	tampered.WorkflowParams = `{"agent_type":"claude-code"}`
	if _, err := CanaryMergingFromRun(&tampered); err == nil {
		t.Fatal("v3 run without merging params must fail closed")
	}
	// ...and a v2 version carrying merging params.
	tampered2 := *plain
	tampered2.WorkflowParams = `{"agent_type":"claude-code","merging":true}`
	if _, err := CanaryMergingFromRun(&tampered2); err == nil {
		t.Fatal("v2 run with merging params must fail closed")
	}
}

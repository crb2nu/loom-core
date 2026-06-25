package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeDispatcher records every Dispatch call and returns canned outputs
// keyed by stage id. A FailFor map causes the dispatcher to error N times
// for a given stage before falling through to a Success output.
type fakeDispatcher struct {
	mu      sync.Mutex
	canned  map[string]StageOutput
	calls   []string
	failFor map[string]int // stage id -> remaining failures
	err     error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, stage.ID)
	if f.err != nil {
		return StageOutput{}, f.err
	}
	if f.failFor != nil {
		if n := f.failFor[stage.ID]; n > 0 {
			f.failFor[stage.ID] = n - 1
			return StageOutput{}, fmt.Errorf("dispatch fail: %s", stage.ID)
		}
	}
	if out, ok := f.canned[stage.ID]; ok {
		return out, nil
	}
	return StageOutput{CostUSD: 0.01}, nil
}

func (f *fakeDispatcher) callsList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

type resumeAwareDispatcher struct {
	gotResume string
}

func (d *resumeAwareDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	d.gotResume = resumeSpawnIDFromContext(ctx)
	if d.gotResume == "" {
		return StageOutput{}, fmt.Errorf("missing resume spawn id for %s", stage.ID)
	}
	return StageOutput{SpawnID: d.gotResume}, nil
}

type acceptedThenInterruptedDispatcher struct{}

func (d *acceptedThenInterruptedDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	record := stageAcceptRecorderFromContext(ctx)
	if record == nil {
		return StageOutput{}, fmt.Errorf("missing accept recorder for %s", stage.ID)
	}
	if err := record("spawn-accepted"); err != nil {
		return StageOutput{}, err
	}
	return StageOutput{}, fmt.Errorf("poll interrupted")
}

type blockingDispatcher struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingDispatcher) Dispatch(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	d.once.Do(func() { close(d.started) })
	<-d.release
	return StageOutput{SpawnID: "spawn-blocking"}, nil
}

func (d *blockingDispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// alwaysFailGate is a gate that always returns Pass=false; used to drive
// the gate-fail retry path.
type alwaysFailGate struct{ name string }

func (g *alwaysFailGate) Name() string { return g.name }
func (g *alwaysFailGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	return gates.Outcome{Pass: false, Reasons: []string{"forced fail"}, JudgedBy: "go"}, nil
}

// alwaysPassGate trivially passes.
type alwaysPassGate struct{ name string }

func (g *alwaysPassGate) Name() string { return g.name }
func (g *alwaysPassGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	return gates.Outcome{Pass: true, JudgedBy: "go"}, nil
}

// flakyJudgeGate fails its first failFor calls with a Pass=false outcome
// (mimicking the LLMGate soft-fail path used for unparseable judge
// output), then passes. This is the M2.5 contract: a gate that returns
// Outcome.Pass=false with err=nil must cause the runner to rewind to
// RetryFrom and re-attempt the upstream stage.
type flakyJudgeGate struct {
	name      string
	failFor   int
	calls     int
	failJudge string // JudgedBy on failure (e.g., "flexinfer:unparseable")
}

func (g *flakyJudgeGate) Name() string { return g.name }
func (g *flakyJudgeGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	g.calls++
	if g.calls <= g.failFor {
		judged := g.failJudge
		if judged == "" {
			judged = "flexinfer:unparseable"
		}
		return gates.Outcome{
			Pass:     false,
			JudgedBy: judged,
			Reasons:  []string{"simulated unparseable judge response"},
		}, nil
	}
	return gates.Outcome{Pass: true, JudgedBy: "flexinfer:qwen-3-8b"}, nil
}

func newRunnerEnv(t *testing.T) (*store.Store, *store.PipelineRun, *store.BacklogItem) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "h.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	item := &store.BacklogItem{
		ID:       "BL-TEST-1",
		Title:    "test backlog item",
		State:    store.BacklogQueued,
		Priority: store.P2,
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	run := &store.PipelineRun{
		ID:        "PIPE-BL-TEST-1-0",
		BacklogID: item.ID,
		Template:  "mills-default-pipeline",
		State:     store.PipelineQueued,
		Attempts:  1,
		StartedAt: now,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return st, run, item
}

func newPassingGates(t *testing.T) *gates.Registry {
	t.Helper()
	r := gates.NewRegistry()
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		r.Register(&alwaysPassGate{name: name})
	}
	return r
}

func TestRunner_DriveHappyPath(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			CostUSD:        0.10,
			FilesChanged:   []string{"foo.go"},
			LinesAdded:     5,
			LinesRemoved:   1,
			DiffPatch:      []byte("diff --git a/foo.go b/foo.go\n+x\n"),
			CommitMessages: []string{"feat: x"},
		},
		"mr":    {CostUSD: 0.05, MRIID: 42},
		"merge": {CostUSD: 0.03, MergedSHA: "abcdef"},
	}}
	r := New(st, newPassingGates(t), disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) }
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Errorf("state = %s, want done", got.State)
	}
	gotItem, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if gotItem.State != store.BacklogMerged {
		t.Errorf("backlog state = %s, want merged", gotItem.State)
	}
	if got.MRIID == nil || *got.MRIID != 42 {
		t.Errorf("mr_iid = %v, want 42", got.MRIID)
	}
	// Expected non-gate stage calls in order (every non-gate stage exactly once).
	want := []string{"plan_slice", "research", "implement", "tests", "pr_self_review", "mr", "ci_watch", "merge", "cleanup"}
	gotCalls := disp.callsList()
	if len(gotCalls) != len(want) {
		t.Fatalf("calls = %v, want %v", gotCalls, want)
	}
	for i := range want {
		if gotCalls[i] != want[i] {
			t.Errorf("calls[%d] = %s, want %s", i, gotCalls[i], want[i])
		}
	}
	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != len(want) {
		t.Errorf("stage_results rows = %d, want %d", len(stages), len(want))
	}
	for _, sr := range stages {
		if sr.Outcome == nil || *sr.Outcome != store.StageOutcomeSuccess {
			t.Errorf("stage %s outcome = %v, want success", sr.Stage, sr.Outcome)
		}
	}
}

func TestRunner_GateFailRetriesUpstreamThenEscalates(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	// Register one gate that always fails so post_implement_gate fails.
	gr := gates.NewRegistry()
	gr.Register(&alwaysFailGate{name: "diff_size"})

	r := New(st, gr, disp, nil)
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Errorf("state = %s, want escalated", got.State)
	}
	gotItem, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if gotItem.State != store.BacklogEscalated {
		t.Errorf("backlog state = %s, want escalated", gotItem.State)
	}
	// Implement should have been called maxAttempts (3) times before
	// escalation; plan_slice + research run once each.
	implementCalls := 0
	for _, c := range disp.callsList() {
		if c == "implement" {
			implementCalls++
		}
	}
	if implementCalls != 3 {
		t.Errorf("implement calls = %d, want 3 (maxAttempts)", implementCalls)
	}
	gateRows, err := st.Pipeline.ListGates(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list gates: %v", err)
	}
	if len(gateRows) == 0 {
		t.Errorf("expected gate_outcomes rows, got 0")
	}
	for _, g := range gateRows {
		if g.Outcome != store.GateOutcomeFail {
			t.Errorf("gate %s outcome = %s, want fail", g.GateName, g.Outcome)
		}
	}
}

// M2.5: post_review_gate's LLM-judged gates (spec_conformance,
// pr_self_review) must trigger the runner's retry path when they return
// Outcome.Pass=false with err=nil — even if the underlying cause is a
// judge parse miss. Verifies the contract that LLMGate.Evaluate now
// soft-fails on unparseable judge output so the existing rewind-to-
// RetryFrom path fires (pr_self_review re-runs on attempt 2).
//
// Live reproducer (2026-05-16 canary PIPE-MILLS-CANARY-M1D-VERIFY-2):
// gemma4-26b returned free-text instead of a score envelope on the first
// spec_conformance call. Before this slice, the runner escalated on the
// first failure. After this slice, the runner rewinds to pr_self_review
// (RetryFrom from DefaultStages line 79), bumps the attempt counter,
// and retries — exactly what the gate-fail path in runner.go line 281
// already does for pure-Go gate failures.
func TestRunner_LLMGateUnparseableOutcomeTriggersRetry(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			CostUSD:        0.10,
			FilesChanged:   []string{"foo.go"},
			LinesAdded:     5,
			LinesRemoved:   1,
			DiffPatch:      []byte("diff --git a/foo.go b/foo.go\n+x\n"),
			CommitMessages: []string{"feat: x"},
		},
		"mr":    {CostUSD: 0.05, MRIID: 42},
		"merge": {CostUSD: 0.03, MergedSHA: "abcdef"},
	}}
	gr := gates.NewRegistry()
	// Register pure-Go gates as no-op passes so post_implement_gate +
	// post_tests_gate clear unimpeded.
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		gr.Register(&alwaysPassGate{name: name})
	}
	// spec_conformance fails once with the unparseable JudgedBy then
	// passes on retry — the contract under test.
	flaky := &flakyJudgeGate{name: "spec_conformance", failFor: 1}
	gr.Register(flaky)
	// pr_self_review trivially passes; it shares post_review_gate with
	// spec_conformance and must not block when its sibling soft-fails.
	gr.Register(&alwaysPassGate{name: "pr_self_review"})

	r := New(st, gr, disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) }
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Errorf("state = %s, want done (run must complete via gate-retry path)", got.State)
	}
	// pr_self_review is the post_review_gate RetryFrom target. After one
	// soft-fail it should be re-dispatched, so the dispatcher sees it twice.
	prCalls := 0
	for _, c := range disp.callsList() {
		if c == "pr_self_review" {
			prCalls++
		}
	}
	if prCalls != 2 {
		t.Errorf("pr_self_review dispatches = %d, want 2 (initial + retry after gate soft-fail)", prCalls)
	}
	// spec_conformance gate evaluated twice (fail, then pass).
	if flaky.calls != 2 {
		t.Errorf("spec_conformance evaluations = %d, want 2 (fail then pass after retry)", flaky.calls)
	}
}

// recordingJudge counts Judge invocations for the M8 integration test.
type recordingJudge struct {
	mu       sync.Mutex
	calls    int
	response gates.RubricVerdict
}

func (r *recordingJudge) Judge(_ context.Context, _ string, _ gates.StageInput) (gates.RubricVerdict, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.response, nil
}

func (r *recordingJudge) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestRunner_CanaryItemSkipsLLMGatesAndReachesMerge is the M8
// end-to-end contract: a backlog item labeled `mills-canary` carries
// through the entire pipeline (plan_slice → research → implement →
// tests → pr_self_review → mr → ci_watch → merge → cleanup) WITHOUT
// the LLM-judged gates ever calling FlexInfer. Each LLM gate persists
// a `gate_outcomes` row with `judged_by="skipped:canary"` so operators
// reviewing the run can grep that exact token to see why no model was
// consulted.
//
// Live evidence the skip is needed: PIPE-MILLS-CANARY-M6-164007-1779036007
// (2026-05-17) returned `score=0.00 below threshold=0.70 | Example:
// file.py:10 - debug print found` from gemma4-26b on a markdown-only
// diff. After M2.5 + M6 the canary retries 3× and escalates honestly
// instead of crashing — but it never reaches merge because the verdict
// is model-quality bounded. M8 short-circuits exactly the LLM gates
// for canaries so the rest of the pipeline plumbing gets exercised.
func TestRunner_CanaryItemSkipsLLMGatesAndReachesMerge(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	// Re-stamp the backlog item with the canary label; newRunnerEnv
	// seeds an unlabeled item by default. This mirrors what
	// cmd/loom/cmd_mills_pipelines.go does when triggering a canary.
	ctx := context.Background()
	item.Labels = []string{gates.CanaryLabel, "safe-fixture"}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed canary item: %v", err)
	}
	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			CostUSD:        0.10,
			FilesChanged:   []string{"testdata/mills-canary/heartbeat.md"},
			LinesAdded:     1,
			LinesRemoved:   1,
			DiffPatch:      []byte("diff --git a/testdata/mills-canary/heartbeat.md b/testdata/mills-canary/heartbeat.md\n@@\n-Run: x\n+Run: y\n"),
			CommitMessages: []string{"chore(canary): bump heartbeat run id"},
		},
		"mr":    {CostUSD: 0.05, MRIID: 99},
		"merge": {CostUSD: 0.03, MergedSHA: "deadbeef"},
	}}

	// Register pure-Go gates as no-op passes so post_implement_gate +
	// post_tests_gate clear unimpeded (these still run for canaries).
	gr := gates.NewRegistry()
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		gr.Register(&alwaysPassGate{name: name})
	}
	// Wire the real spec_conformance + pr_self_review gates. The
	// recordingJudge would emit a fail verdict if consulted; the M8
	// skip path must short-circuit before it's called.
	judge := &recordingJudge{response: gates.RubricVerdict{Score: 0.0, Model: "should-not-be-called"}}
	gates.RegisterLLMGates(gr, judge)

	r := New(st, gr, disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC) }
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Fatalf("state = %s, want done (canary must reach merge with LLM gates skipped)", got.State)
	}

	// Hard contract: the judge must NEVER be consulted for a canary.
	// If this fires we're paying for and waiting on a model roundtrip
	// we explicitly designed the canary to avoid.
	if calls := judge.callCount(); calls != 0 {
		t.Errorf("judge.calls = %d, want 0 (canary must not consult FlexInfer)", calls)
	}

	gateRows, err := st.Pipeline.ListGates(ctx, run.ID)
	if err != nil {
		t.Fatalf("list gates: %v", err)
	}

	// Audit-trail contract: both LLM gates persisted a row with
	// judged_by="skipped:canary" so operators can grep for it.
	skipped := map[string]int{}
	for _, gr := range gateRows {
		if gr.JudgedBy == gates.CanarySkipJudgedBy {
			skipped[gr.GateName]++
			if gr.Outcome != store.GateOutcomePass {
				t.Errorf("canary-skipped gate %s outcome = %v, want pass", gr.GateName, gr.Outcome)
			}
		}
	}
	if skipped["spec_conformance"] == 0 {
		t.Errorf("expected at least one spec_conformance row with judged_by=%q; got rows %+v", gates.CanarySkipJudgedBy, gateRows)
	}
	if skipped["pr_self_review"] == 0 {
		t.Errorf("expected at least one pr_self_review row with judged_by=%q; got rows %+v", gates.CanarySkipJudgedBy, gateRows)
	}
}

func TestRunner_StageErrorRetriesThenSucceeds(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	// First implement attempt fails; second succeeds.
	disp := &fakeDispatcher{
		failFor: map[string]int{"implement": 1},
	}
	r := New(st, newPassingGates(t), disp, nil)
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Errorf("state = %s, want done", got.State)
	}
	implementCalls := 0
	for _, c := range disp.callsList() {
		if c == "implement" {
			implementCalls++
		}
	}
	if implementCalls != 2 {
		t.Errorf("implement calls = %d, want 2 (1 fail + 1 retry success)", implementCalls)
	}
	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	implementRows := 0
	successRows := 0
	errorRows := 0
	for _, sr := range stages {
		if sr.Stage == "implement" {
			implementRows++
			if sr.Outcome != nil && *sr.Outcome == store.StageOutcomeSuccess {
				successRows++
			}
			if sr.Outcome != nil && *sr.Outcome == store.StageOutcomeError {
				errorRows++
				if !strings.Contains(sr.LogTail, "dispatch fail: implement") {
					t.Errorf("implement error log tail = %q, want dispatch error", sr.LogTail)
				}
			}
		}
	}
	if implementRows != 2 {
		t.Errorf("implement stage_results rows = %d, want 2", implementRows)
	}
	if successRows != 1 {
		t.Errorf("implement success rows = %d, want 1", successRows)
	}
	if errorRows != 1 {
		t.Errorf("implement error rows = %d, want 1", errorRows)
	}
}

func TestRunner_ResumesFromCurrentStage(t *testing.T) {
	st, run, item := newRunnerEnv(t)

	// Pre-populate a pretend prior implement success.
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	for _, s := range []string{"plan_slice", "research", "implement"} {
		out := store.StageOutcomeSuccess
		end := now
		artifacts := map[string]any{"stage_id": s}
		if s == "implement" {
			artifacts["files_changed"] = []any{"foo.go"}
		}
		if err := st.Pipeline.PutStage(context.Background(), &store.StageResult{
			PipelineRunID: run.ID,
			Stage:         s,
			Attempt:       1,
			StartedAt:     now,
			EndedAt:       &end,
			Outcome:       &out,
			Artifacts:     artifacts,
		}); err != nil {
			t.Fatalf("seed stage %s: %v", s, err)
		}
	}
	run.CurrentStage = "tests"
	run.State = store.PipelineTesting
	if err := st.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("seed run head: %v", err)
	}

	disp := &fakeDispatcher{}
	r := New(st, newPassingGates(t), disp, nil)
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	calls := disp.callsList()
	// Resume should skip plan_slice/research/implement.
	for _, c := range calls {
		if c == "plan_slice" || c == "research" || c == "implement" {
			t.Errorf("resume should not re-run %s", c)
		}
	}
	// And tests should be the first call this Drive made.
	if len(calls) == 0 || calls[0] != "tests" {
		t.Errorf("first call after resume = %v, want tests-first", calls)
	}
}

func TestRunner_ResumesPendingStageSpawnAttempt(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	run.CurrentStage = "plan_slice"
	run.State = store.PipelinePlanning
	if err := st.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("seed run head: %v", err)
	}
	if err := st.Pipeline.PutStage(context.Background(), &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "plan_slice",
		Attempt:       1,
		StartedAt:     now,
		SpawnID:       "spawn-existing",
		Artifacts:     map[string]any{"stage_id": "plan_slice"},
	}); err != nil {
		t.Fatalf("seed pending stage: %v", err)
	}

	disp := &resumeAwareDispatcher{}
	r := New(st, nil, disp, nil)
	r.Stages = []Stage{{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}}
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if disp.gotResume != "spawn-existing" {
		t.Fatalf("resume spawn id = %q, want spawn-existing", disp.gotResume)
	}
	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stage rows = %d, want existing attempt updated in-place", len(stages))
	}
	if stages[0].Attempt != 1 || stages[0].SpawnID != "spawn-existing" {
		t.Fatalf("stage row = %+v", stages[0])
	}
	if stages[0].Outcome == nil || *stages[0].Outcome != store.StageOutcomeSuccess {
		t.Fatalf("stage outcome = %v, want success", stages[0].Outcome)
	}
}

func TestRunner_KeepsAcceptedSpawnPendingOnInterruptedPoll(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r := New(st, nil, &acceptedThenInterruptedDispatcher{}, nil)
	r.Stages = []Stage{{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}}

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stage rows = %d, want pending accepted spawn", len(stages))
	}
	if stages[0].SpawnID != "spawn-accepted" {
		t.Fatalf("spawn id = %q, want accepted spawn preserved", stages[0].SpawnID)
	}
	if stages[0].Outcome != nil || stages[0].EndedAt != nil {
		t.Fatalf("stage should remain pending, got outcome=%v ended=%v", stages[0].Outcome, stages[0].EndedAt)
	}
	gotRun, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.State != store.PipelinePlanning || gotRun.CurrentStage != "plan_slice" {
		t.Fatalf("run head = state %s current %q, want planning/plan_slice", gotRun.State, gotRun.CurrentStage)
	}
}

// stalledSpawnDispatcher models a spawn whose pod stays alive but never
// reaches a terminal status, so every poll exhausts the client PollDeadline
// and returns ErrSpawnPollTimeout with the (accepted) spawn id set. On a
// resume it re-attaches to the supplied spawn id; on a fresh dispatch it mints
// a new id and records it via the accept recorder, mirroring HUDSpawnClient.
type stalledSpawnDispatcher struct {
	mu          sync.Mutex
	stage       string
	calls       int
	freshSpawns int
	spawnIDs    map[string]bool
}

func (d *stalledSpawnDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if stage.ID != d.stage {
		return StageOutput{CostUSD: 0.01}, nil
	}
	d.calls++
	spawnID := resumeSpawnIDFromContext(ctx)
	if spawnID == "" {
		d.freshSpawns++
		spawnID = fmt.Sprintf("spawn-hung-%d", d.freshSpawns)
		if rec := stageAcceptRecorderFromContext(ctx); rec != nil {
			if err := rec(spawnID); err != nil {
				return StageOutput{}, err
			}
		}
	}
	if d.spawnIDs == nil {
		d.spawnIDs = map[string]bool{}
	}
	d.spawnIDs[spawnID] = true
	// Non-terminal poll that ran out the client deadline (hung-but-alive pod).
	return StageOutput{SpawnID: spawnID},
		fmt.Errorf("hud spawn: poll timeout after 30m0s: %w", ErrSpawnPollTimeout)
}

func (d *stalledSpawnDispatcher) snapshot() (calls, fresh, distinct int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, d.freshSpawns, len(d.spawnIDs)
}

// TestRunner_StalledSpawnEscalatesInsteadOfLoopingPending pins the 2026-06-25
// regression: a spawn that keeps timing out at its PollDeadline while its pod
// stays alive used to re-park the stage pending on every reconciler re-drive,
// never burning the retry budget — the run looped pending forever (~13h
// observed) until the orphan pod was deleted by hand. The fix converts a
// recurring poll-timeout into a failed (transient-class) attempt so a fresh
// spawn is re-dispatched and the run ultimately escalates.
func TestRunner_StalledSpawnEscalatesInsteadOfLoopingPending(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &stalledSpawnDispatcher{stage: "plan_slice"}
	r := New(st, nil, disp, nil)
	r.Stages = []Stage{{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}}
	// Disable auto-retry so the transient hard cap escalates the item
	// directly (deterministic terminal state for the assertion).
	r.Policy = newPolicyMgrWithRetryCap(t, 0)

	// Simulate the reconciler re-driving the in-flight run each tick.
	const maxTicks = 100
	escalated := false
	ticks := 0
	for ; ticks < maxTicks; ticks++ {
		if err := r.Drive(context.Background(), run, item); err != nil {
			t.Fatalf("drive tick %d: %v", ticks, err)
		}
		got, err := st.Pipeline.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("get run tick %d: %v", ticks, err)
		}
		if got.State == store.PipelineEscalated {
			escalated = true
			break
		}
		if got.State != store.PipelinePlanning {
			t.Fatalf("tick %d: unexpected run state %s", ticks, got.State)
		}
	}
	if !escalated {
		t.Fatalf("run never escalated after %d re-drives; stalled spawn looped pending (the bug)", maxTicks)
	}

	gotItem, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if gotItem.State != store.BacklogEscalated {
		t.Errorf("backlog state = %s, want escalated", gotItem.State)
	}

	calls, fresh, distinct := disp.snapshot()
	// The stall must trigger fresh re-spawns, not just re-attach the same
	// dead spawn forever. With maxAttempts=3 + default transientRetryCap=5
	// the hard cap is 8 attempts, each a distinct fresh spawn.
	if fresh < 2 {
		t.Errorf("fresh spawns = %d; want >=2 (stall should re-dispatch, not only re-attach)", fresh)
	}
	if distinct < 2 {
		t.Errorf("distinct spawn ids = %d; want >=2", distinct)
	}
	t.Logf("escalated after %d ticks: %d dispatch calls, %d fresh spawns, %d distinct ids", ticks+1, calls, fresh, distinct)
}

// TestRunner_SinglePollTimeoutStillParksPending guards the other side: one
// poll-timeout (a spawn that may simply be slow-but-progressing) must keep the
// existing resume-safe "check again next tick" behavior — it parks pending and
// does NOT burn the retry budget or escalate.
func TestRunner_SinglePollTimeoutStillParksPending(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &stalledSpawnDispatcher{stage: "plan_slice"}
	r := New(st, nil, disp, nil)
	r.Stages = []Stage{{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}}

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelinePlanning {
		t.Fatalf("run state = %s; want planning (single timeout must not escalate)", got.State)
	}
	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stage rows = %d, want one pending row", len(stages))
	}
	if stages[0].Outcome != nil || stages[0].EndedAt != nil {
		t.Fatalf("stage should remain pending, got outcome=%v ended=%v", stages[0].Outcome, stages[0].EndedAt)
	}
	if n := pendingPollTimeouts(stages[0]); n != 1 {
		t.Errorf("pending poll-timeout counter = %d, want 1", n)
	}
}

func TestRunner_StartGoroutineReachesTerminal(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	r := New(st, newPassingGates(t), disp, nil)
	if err := r.Start(context.Background(), run, item); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.Pipeline.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("getrun: %v", err)
		}
		if got.State == store.PipelineDone || got.State == store.PipelineEscalated {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Start: run did not reach terminal state in time")
}

func TestRunner_StartSuppressesDuplicateActiveRun(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &blockingDispatcher{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	r := New(st, newPassingGates(t), disp, nil)
	r.Stages = []Stage{{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}}
	if err := r.Start(context.Background(), run, item); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-disp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch did not start")
	}
	if err := r.Start(context.Background(), run, item); err != nil {
		t.Fatalf("duplicate start: %v", err)
	}
	close(disp.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := st.Pipeline.GetRun(context.Background(), run.ID); err == nil && got.State == store.PipelineDone {
			if calls := disp.callCount(); calls != 1 {
				t.Fatalf("dispatch calls = %d, want 1", calls)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run did not finish after releasing dispatcher")
}

func TestRunner_StartEscalatesFatalDriveError(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	run.State = store.PipelinePlanning
	run.CurrentStage = "not-in-dag"
	if err := st.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("seed bad run head: %v", err)
	}

	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	if err := r.Start(context.Background(), run, item); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.Pipeline.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("getrun: %v", err)
		}
		if got.State == store.PipelineEscalated {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := st.Pipeline.GetRun(context.Background(), run.ID)
	t.Fatalf("state = %s, want escalated", got.State)
}

func TestRunner_StartRejectsBadConfig(t *testing.T) {
	r := &Runner{}
	if err := r.Start(context.Background(), &store.PipelineRun{ID: "x"}, &store.BacklogItem{ID: "y"}); err == nil {
		t.Errorf("expected error for unconfigured runner")
	}
	st, run, item := newRunnerEnv(t)
	r2 := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	if err := r2.Start(context.Background(), nil, item); !errors.Is(err, errors.New("pipeline: run.ID required")) && err == nil {
		t.Errorf("expected error for nil run, got nil")
	}
	if err := r2.Start(context.Background(), run, nil); err == nil {
		t.Errorf("expected error for nil item")
	}
}

// emptyTextErr is an error type whose Error() returns "". Real-world
// example: a context that was canceled before the spawn HTTP client
// captured any body text, then wrapped through a custom error type. The
// runner's log_tail fallback must still produce searchable text — this
// is the path that historically left 33 of 33 plan_slice rows with
// outcome='error' AND empty log_tail.
type emptyTextErr struct{}

func (emptyTextErr) Error() string { return "" }

// silentErrorDispatcher returns (StageOutput{}, emptyTextErr{}) for every
// stage call. Models the failure mode where the spawn HUD client errored
// without any usable telemetry or text — the case slice M1a must cover.
type silentErrorDispatcher struct{}

func (silentErrorDispatcher) Dispatch(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, _ Stage, _ map[string]StageOutput) (StageOutput, error) {
	return StageOutput{}, emptyTextErr{}
}

// TestRunner_PersistsSpawnFailureContextWhenWorkerReturnsEmptyError is
// the regression guard for slice M1a: even when the dispatcher errors
// without any text and without spawn telemetry, the persisted
// stage_results row for the error attempt must have a non-empty log_tail
// that names the stage + attempt so future triage has signal.
func TestRunner_PersistsSpawnFailureContextWhenWorkerReturnsEmptyError(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), silentErrorDispatcher{}, nil)
	// Narrow the DAG to just plan_slice to keep the assertions tight and
	// mirror the live failure mode (plan_slice is where the 33 empty
	// rows landed).
	r.Stages = []Stage{{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}}

	// Drive returns the escalation error from the runner; we only care
	// that stage_results captured signal regardless of run-state outcome.
	_ = r.Drive(context.Background(), run, item)

	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) == 0 {
		t.Fatalf("expected at least one stage_results row, got 0")
	}
	sawError := false
	for _, sr := range stages {
		if sr.Outcome == nil || *sr.Outcome != store.StageOutcomeError {
			continue
		}
		sawError = true
		if strings.TrimSpace(sr.LogTail) == "" {
			t.Errorf("stage %s attempt %d: log_tail empty for error row (the bug)", sr.Stage, sr.Attempt)
		}
		// Identifiable context: stage id + attempt number must be in
		// the tail so a SQL grep on stage_results.log_tail can find
		// the run.
		if !strings.Contains(sr.LogTail, "stage=plan_slice") {
			t.Errorf("log_tail %q missing stage marker", sr.LogTail)
		}
		if !strings.Contains(sr.LogTail, fmt.Sprintf("attempt=%d", sr.Attempt)) {
			t.Errorf("log_tail %q missing attempt marker for attempt %d", sr.LogTail, sr.Attempt)
		}
		// Synthetic worker-no-text fallback must apply — emptyTextErr
		// returns "" from Error() so neither the worker tail nor the
		// derr.Error() fallback can populate the row on their own.
		if !strings.Contains(sr.LogTail, "no error text returned by worker") {
			t.Errorf("log_tail %q missing worker-empty fallback marker", sr.LogTail)
		}
	}
	if !sawError {
		t.Fatalf("expected at least one stage_results row with outcome=error, got: %+v", stages)
	}
}

// TestRunner_PersistsSpawnFailureContextOnPendingPath covers the pending
// branch: the spawn was accepted (OnAccepted ran) but the dispatcher
// returned an error before the spawn reached a terminal status. The
// pending stage_results row must carry the error context too — without
// it the next operator restart sees a pending row with no clue what
// went wrong on the previous attempt.
func TestRunner_PersistsSpawnFailureContextOnPendingPath(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	r := New(st, nil, &acceptedThenInterruptedDispatcher{}, nil)
	r.Stages = []Stage{{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}}

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stage rows = %d, want exactly one pending row", len(stages))
	}
	row := stages[0]
	if row.Outcome != nil {
		t.Fatalf("pending row should have nil outcome, got %v", row.Outcome)
	}
	if row.SpawnID != "spawn-accepted" {
		t.Fatalf("pending row spawn_id = %q, want spawn-accepted", row.SpawnID)
	}
	if strings.TrimSpace(row.LogTail) == "" {
		t.Fatalf("pending row log_tail empty; should carry spawn/stage context for resume triage")
	}
	for _, want := range []string{"stage=plan_slice", "attempt=1", "spawn=spawn-accepted", "poll interrupted"} {
		if !strings.Contains(row.LogTail, want) {
			t.Errorf("pending row log_tail %q missing %q", row.LogTail, want)
		}
	}
}

// TestBuildFailureLogTail_Precedence pins the contract that the helper
// keeps existing worker text when present, falls back to err.Error(),
// and otherwise synthesizes a worker-empty marker.
func TestBuildFailureLogTail_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		err      error
		stage    string
		attempt  int
		spawn    string
		want     []string // substrings the result must contain
		notWant  []string // substrings the result must NOT contain
	}{
		{
			name:     "worker tail wins",
			existing: "  HUD spawn telemetry: stop_reason=max_turns  ",
			err:      errors.New("hud spawn xyz status=failed"),
			stage:    "plan_slice", attempt: 1, spawn: "spawn-1",
			want: []string{"stage=plan_slice", "attempt=1", "spawn=spawn-1", "HUD spawn telemetry"},
		},
		{
			name:     "err.Error fills empty tail",
			existing: "",
			err:      errors.New("hud spawn xyz status=failed"),
			stage:    "implement", attempt: 2, spawn: "spawn-2",
			want: []string{"stage=implement", "attempt=2", "spawn=spawn-2", "hud spawn xyz status=failed"},
		},
		{
			name:     "synthetic when both empty",
			existing: "",
			err:      emptyTextErr{},
			stage:    "pr_self_review", attempt: 3, spawn: "",
			want:    []string{"stage=pr_self_review", "attempt=3", "no error text returned by worker"},
			notWant: []string{"spawn="},
		},
		{
			name:     "nil error with empty tail still gets synthetic",
			existing: "   ",
			err:      nil,
			stage:    "tests", attempt: 1, spawn: "spawn-z",
			want: []string{"stage=tests", "attempt=1", "spawn=spawn-z", "no error text returned by worker"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFailureLogTail(tc.existing, tc.err, tc.stage, tc.attempt, tc.spawn)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("got empty result for %q", tc.name)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("result %q missing %q", got, w)
				}
			}
			for _, n := range tc.notWant {
				if strings.Contains(got, n) {
					t.Errorf("result %q should not contain %q", got, n)
				}
			}
		})
	}
}

// ----- Slice 2c: transient-vs-code retry classification -----

// classedFailDispatcher returns a specific error string on the next N
// calls to a stage, then succeeds. Lets tests drive the runner through
// the new error_class classifier with real kill-test fixtures.
type classedFailDispatcher struct {
	mu    sync.Mutex
	stage string
	errs  []string // pop from front; empty slice = success thereafter
	calls int
}

func (d *classedFailDispatcher) Dispatch(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if stage.ID != d.stage {
		// Other stages just succeed.
		return StageOutput{CostUSD: 0.01}, nil
	}
	d.calls++
	if len(d.errs) == 0 {
		return StageOutput{CostUSD: 0.01}, nil
	}
	msg := d.errs[0]
	d.errs = d.errs[1:]
	return StageOutput{}, errors.New(msg)
}

// TestRunner_TransientErrorsDoNotConsumeBudget pins the headline
// Slice 2c behavior: a stage that fails with transient errors (k8s pod
// GC, websocket close, broken pipe) is retried for free up to the cap
// and still reaches `done` even though the raw fail count exceeded
// MaxAttempts. Without this slice, 4 transient flakes would have hit
// MaxAttempts=3 and escalated.
func TestRunner_TransientErrorsDoNotConsumeBudget(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &classedFailDispatcher{
		stage: "implement",
		errs: []string{
			"pod not found during reconciliation",                             // transient (k8s GC)
			"websocket: close 1006 (abnormal closure): unexpected EOF",        // transient (mcp)
			"write tcp 10.42.4.85:45000->10.43.248.41:80: write: broken pipe", // transient (mcp)
			"flexinfer chat: context deadline exceeded",                       // transient (flexinfer)
		},
	}
	if err := New(st, newPassingGates(t), disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State == store.PipelineEscalated {
		t.Errorf("state = escalated; transient retries should not consume MaxAttempts budget")
	}
	if disp.calls < 5 {
		t.Errorf("implement called %d times; want at least 5 (4 transient retries + 1 success)", disp.calls)
	}
}

// TestRunner_ConfigErrorEscalatesWithoutRetry pins DEBT-073(b): a
// merge-stage 405 is a terminal config error — the runner escalates on
// the first occurrence instead of burning MaxAttempts on identical
// retries (escalations #148/#150 showed 3 verbatim retries per run).
func TestRunner_ConfigErrorEscalatesWithoutRetry(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &classedFailDispatcher{
		stage: "merge",
		errs: []string{
			`gitlab: PUT /projects/services%2Floom-core/merge_requests/598/merge: status 405: {"message":"405 Method Not Allowed"}`,
			// Never reached — present so a regression to retrying
			// surfaces as calls > 1 rather than a pass-by-success.
			`gitlab: PUT /projects/services%2Floom-core/merge_requests/598/merge: status 405: {"message":"405 Method Not Allowed"}`,
		},
	}
	if err := New(st, newPassingGates(t), disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Errorf("state = %s; want escalated on first config-class error", got.State)
	}
	if disp.calls != 1 {
		t.Errorf("merge called %d times; want 1 (config errors must not retry)", disp.calls)
	}
}

// TestRunner_CodeErrorsExhaustBudgetEvenWithTransientHistory pins the
// other half: a stage that mixes transient and real-code failures
// escalates after MaxAttempts *code* failures, regardless of how many
// transient retries it burned. Without proper class accounting a
// stage could mask a real bug by interleaving transient flakes.
func TestRunner_CodeErrorsExhaustBudgetEvenWithTransientHistory(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &classedFailDispatcher{
		stage: "implement",
		// Two transients followed by three real code failures.
		// Default MaxAttempts=3 → escalate after the 3rd code fail.
		errs: []string{
			"websocket: close 1006",
			"pod not found during reconciliation",
			"go test FAIL: TestFoo not equal",
			"go test FAIL: TestFoo not equal",
			"go test FAIL: TestFoo not equal",
		},
	}
	if err := New(st, newPassingGates(t), disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Errorf("state = %s; want escalated (3 code fails should exhaust budget)", got.State)
	}
	if disp.calls != 5 {
		t.Errorf("implement called %d times; want 5 (2 transient + 3 code)", disp.calls)
	}
}

// TestRunner_TransientHardCapPreventsRunaway pins the hard cap on
// total attempts (MaxAttempts + TransientRetryCap, default 3+5=8): a
// permanently-flaking transient escalates instead of looping forever.
func TestRunner_TransientHardCapPreventsRunaway(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	// 20 transient errors >> hard cap. Runner should stop at the cap.
	flakes := make([]string, 20)
	for i := range flakes {
		flakes[i] = "websocket: close 1006 (abnormal closure)"
	}
	disp := &classedFailDispatcher{stage: "implement", errs: flakes}
	if err := New(st, newPassingGates(t), disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Errorf("state = %s; want escalated (hard cap should kick in)", got.State)
	}
	// Default cap is 3+5=8. Allow ±1 for the boundary (the escalate
	// check is `>= cap`).
	if disp.calls < 7 || disp.calls > 9 {
		t.Errorf("implement called %d times; want ~8 (MaxAttempts=3 + TransientRetryCap=5)", disp.calls)
	}
}

// TestRunner_InfraErrorsCountAgainstBudget pins that buildah / sandbox
// infrastructure failures (ClassInfra) count against MaxAttempts so a
// truly broken sandbox doesn't get free retries forever. Slice 2e will
// reduce the rate, but until then ClassInfra must consume the budget.
func TestRunner_InfraErrorsCountAgainstBudget(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &classedFailDispatcher{
		stage: "implement",
		errs: []string{
			"image build failed: create buildah pod: pods \"buildah-build-spawn-runtime-codex-x\" already exists",
			"image build failed: buildah build failed: build pod failed: exit_code=243 reason=Error",
			"ensure sandbox: generate dockerfile: no language detected",
		},
	}
	if err := New(st, newPassingGates(t), disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Errorf("state = %s; want escalated (3 infra failures should exhaust budget)", got.State)
	}
	if disp.calls != 3 {
		t.Errorf("implement called %d times; want 3 (MaxAttempts, no free retries for Infra)", disp.calls)
	}
}

// ----- Slice 3d: bounded escalation auto-retry -----

// TestRunner_TransientCapEscalation_AutoRetries pins the headline
// behaviour: a pipeline that escalates because of the transient hard
// cap gets re-queued (backlog stays Queued, run goes Escalated) up to
// EscalationAutoRetryCap times.
func TestRunner_TransientCapEscalation_AutoRetries(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	// Use a policy mutator? newRunnerEnv doesn't accept one. Instead
	// rely on the default policy + a runner Policy override. The
	// recurring transient → escalate path needs MaxAttempts=3 +
	// EscalationAutoRetryCap > 0.
	disp := &classedFailDispatcher{stage: "implement"}
	// 100 transient errors >> hard cap → escalate by total-attempts.
	for i := 0; i < 100; i++ {
		disp.errs = append(disp.errs, "websocket: close 1006 (abnormal closure)")
	}

	r := New(st, newPassingGates(t), disp, nil)
	// Inject a Policy that opts into auto-retry.
	r.Policy = newPolicyMgrWithRetryCap(t, 2)

	var autoRetries int32
	r.OnAutoRetry = func(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem) error {
		atomic.AddInt32(&autoRetries, 1)
		return nil
	}

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	// First run should be escalated (transient cap), but backlog
	// item should be queued (auto-retried, not human-escalated).
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Errorf("run state = %s, want escalated", got.State)
	}
	gotItem, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("getbacklog: %v", err)
	}
	if gotItem.State != store.BacklogQueued {
		t.Errorf("backlog state = %s, want queued (auto-retry should not escalate the item)", gotItem.State)
	}
	if got := atomic.LoadInt32(&autoRetries); got != 1 {
		t.Errorf("OnAutoRetry fired %d times, want 1", got)
	}
}

// TestRunner_TransientCapEscalation_StopsAtCap pins the second half:
// after EscalationAutoRetryCap auto-retries already exist, the next
// transient escalation actually escalates the backlog item.
func TestRunner_TransientCapEscalation_StopsAtCap(t *testing.T) {
	st, run, item := newRunnerEnv(t)

	// Seed two prior escalated runs so the cap (2) is exhausted. The
	// pipeline_runs UNIQUE constraint is (backlog_id, attempts), and
	// newRunnerEnv already seeded a row with attempts=1; use 100/101
	// here to dodge that without bumping the live run's attempts.
	ctx := context.Background()
	for i, attempts := range []int{100, 101} {
		prior := &store.PipelineRun{
			ID:        fmt.Sprintf("PIPE-%s-prior-%d", item.ID, i),
			BacklogID: item.ID,
			Template:  "mills-default-pipeline",
			State:     store.PipelineEscalated,
			Attempts:  attempts,
			StartedAt: run.StartedAt.Add(-time.Duration(i+1) * time.Hour),
		}
		if err := st.Pipeline.PutRun(ctx, prior); err != nil {
			t.Fatalf("seed prior %d (attempts=%d): %v", i, attempts, err)
		}
	}

	disp := &classedFailDispatcher{stage: "implement"}
	for i := 0; i < 100; i++ {
		disp.errs = append(disp.errs, "websocket: close 1006")
	}
	r := New(st, newPassingGates(t), disp, nil)
	r.Policy = newPolicyMgrWithRetryCap(t, 2)

	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	gotItem, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("getbacklog: %v", err)
	}
	if gotItem.State != store.BacklogEscalated {
		t.Errorf("backlog state = %s, want escalated (cap of 2 exhausted)", gotItem.State)
	}
}

// TestRunner_CodeClassEscalation_DoesNotAutoRetry: even with cap > 0,
// a real code-class failure (test fail, build fail, etc.) escalates
// to human — auto-retry is only for transient-cap escalations.
func TestRunner_CodeClassEscalation_DoesNotAutoRetry(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &classedFailDispatcher{stage: "implement", errs: []string{
		"go test FAIL: TestFoo not equal",
		"go test FAIL: TestFoo not equal",
		"go test FAIL: TestFoo not equal",
	}}
	r := New(st, newPassingGates(t), disp, nil)
	r.Policy = newPolicyMgrWithRetryCap(t, 5) // generous cap, irrelevant for code-class

	var autoRetries int32
	r.OnAutoRetry = func(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem) error {
		atomic.AddInt32(&autoRetries, 1)
		return nil
	}

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	gotItem, _ := st.Backlog.Get(context.Background(), item.ID)
	if gotItem.State != store.BacklogEscalated {
		t.Errorf("backlog state = %s, want escalated (code-class should not auto-retry)", gotItem.State)
	}
	if got := atomic.LoadInt32(&autoRetries); got != 0 {
		t.Errorf("OnAutoRetry fired %d times for code-class, want 0", got)
	}
}

func TestIsTransientEscalationReason(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"stage implement errored after 8 total attempts (cap 8) [class=transient]: websocket close", true},
		{"stage implement errored after 6 total attempts (cap 6) [class=transient_quota]: 429", true},
		{"stage implement errored after 3 attempts [class=code]: go test FAIL", false},
		{"stage tests errored after 3 attempts [class=infra]: buildah pod conflict", false},
		{"gate diff_size failed; implement exceeded 3 attempts", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isTransientEscalationReason(tc.in); got != tc.want {
			t.Errorf("isTransientEscalationReason(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// newPolicyMgrWithRetryCap builds a real PolicyManager whose policy
// has EscalationAutoRetryCap set. We use a tmpdir + YAML file because
// PolicyManager is fsnotify-driven and there's no public constructor
// that takes a *Policy directly. The Validate() golden version field
// requires version >= 1; we use 1 to keep the fixture minimal.
func newPolicyMgrWithRetryCap(t *testing.T, cap int) *mills.PolicyManager {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	body := fmt.Sprintf(`version: 1
enabled: true
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 1 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 1 }
pipeline:
  retry:
    max_attempts: 3
    cooldown_seconds: 0
    escalation_auto_retry_cap: %d
`, cap)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	pm, err := mills.NewPolicyManager(context.Background(), path, mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })
	return pm
}

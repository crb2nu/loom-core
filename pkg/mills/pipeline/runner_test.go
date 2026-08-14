package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeDispatcher records every Dispatch call and returns canned outputs
// keyed by stage id. A FailFor map causes the dispatcher to error N times
// for a given stage before falling through to a Success output.
type fakeDispatcher struct {
	mu      sync.Mutex
	canned  map[string]StageOutput
	calls   []string
	failFor map[string]int   // stage id -> remaining failures
	errFor  map[string]error // stage id -> persistent error to return
	err     error
	// seenFence records the MR head-transition fence the runner threaded onto
	// the context for each stage (#374). Only ci_watch and merge are fenced.
	seenFence map[string]int64
}

type failingIncidentWriter struct {
	calls int
}

func (w *failingIncidentWriter) Put(context.Context, *store.IncidentRecord) (bool, error) {
	w.calls++
	return false, errors.New("incident database unavailable")
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, stage.ID)
	if f.seenFence == nil {
		f.seenFence = map[string]int64{}
	}
	f.seenFence[stage.ID] = headTransitionSeqFromContext(ctx)
	if f.err != nil {
		return StageOutput{}, f.err
	}
	if f.errFor != nil {
		if serr := f.errFor[stage.ID]; serr != nil {
			return StageOutput{}, serr
		}
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

type mergeRecoveryFenceDispatcher struct {
	attempted []bool
	calls     int
}

func (d *mergeRecoveryFenceDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, _ Stage, _ map[string]StageOutput) (StageOutput, error) {
	d.calls++
	d.attempted = append(d.attempted, mergeRecoveryPipelineCreateAttemptedFromContext(ctx))
	if d.calls == 1 {
		if err := RecordMergeRecoveryPipelineCreate(ctx); err != nil {
			return StageOutput{}, err
		}
		return StageOutput{}, errors.New("operator interrupted after pipeline-create fence")
	}
	return StageOutput{}, nil
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

// terminalFailGate fails with Outcome.Terminal set — the runner must
// escalate immediately instead of rewinding to RetryFrom.
type terminalFailGate struct{ name string }

func (g *terminalFailGate) Name() string { return g.name }
func (g *terminalFailGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	return gates.Outcome{Pass: false, Reasons: []string{"deterministic fail"}, JudgedBy: "go", Terminal: true}, nil
}

// alwaysPassGate trivially passes.
type alwaysPassGate struct{ name string }

func (g *alwaysPassGate) Name() string { return g.name }
func (g *alwaysPassGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	return gates.Outcome{Pass: true, JudgedBy: "go"}, nil
}

// skipGate returns an advisory skip (Pass=true, Skip=true) — the shape the
// scope gate now uses for a slice-less item. The pipeline must proceed and the
// runner must persist gate_outcomes.outcome='skip'.
type skipGate struct{ name string }

func (g *skipGate) Name() string { return g.name }
func (g *skipGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	return gates.Outcome{Pass: true, Skip: true, Reasons: []string{"advisory skip"}, JudgedBy: "go"}, nil
}

// flakyJudgeGate fails its first failFor calls with a Pass=false outcome
// (mimicking the LLMGate soft-fail path used for unparseable judge
// output), then passes. This is the M2.5 contract: a gate that returns
// Outcome.Pass=false with err=nil must cause the runner to rewind to
// RetryFrom and re-attempt the upstream stage.
type flakyJudgeGate struct {
	name       string
	failFor    int
	calls      int
	failJudge  string // JudgedBy on failure (e.g., "flexinfer:unparseable")
	failReason string // Reasons[0] on failure; defaults to a generic message
}

func (g *flakyJudgeGate) Name() string { return g.name }
func (g *flakyJudgeGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	g.calls++
	if g.calls <= g.failFor {
		judged := g.failJudge
		if judged == "" {
			judged = "flexinfer:unparseable"
		}
		reason := g.failReason
		if reason == "" {
			reason = "simulated unparseable judge response"
		}
		return gates.Outcome{
			Pass:     false,
			JudgedBy: judged,
			Reasons:  []string{reason},
		}, nil
	}
	return gates.Outcome{Pass: true, JudgedBy: "flexinfer:qwen-3-8b"}, nil
}

// issue348JudgeReason is the EXACT failing-gate reason text from GitLab issue
// #348 (services/loom-core): the post_review_gate judge returned an empty score
// envelope (raw="") on finished work. Used as a fixture so the runner's
// external-dependency classification is exercised against real bytes.
const issue348JudgeReason = `judge response could not be parsed into a score envelope: rubric judge: parse: no parseable score envelope in response: rubric judge: unparseable response; raw=""`

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

func TestRunner_CIWatchFlakeRescueFenceSurvivesStoreRoundTrip(t *testing.T) {
	st, run, _ := newRunnerEnv(t)
	jobs := []FailedJob{{ID: 17, Name: "test:reliability", FailureReason: "script_failure"}}
	if err := st.Pipeline.PutStage(context.Background(), &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "ci_watch",
		Attempt:       1,
		StartedAt:     time.Now(),
		Artifacts: map[string]any{
			"stage_id":                          "ci_watch",
			ciWatchFlakeRescueAttemptedArtifact: true,
			ciWatchFlakeRescueFirstJobsArtifact: jobs,
		},
	}); err != nil {
		t.Fatalf("persist rescue fence: %v", err)
	}

	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	attempted, restored, err := r.ciWatchFlakeRescueAttempted(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("restore rescue fence: %v", err)
	}
	if !attempted || len(restored) != 1 || restored[0] != jobs[0] {
		t.Fatalf("restored attempted=%v jobs=%+v, want true %+v", attempted, restored, jobs)
	}
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

func TestCostRollupAttributesCumulativeCostOncePerSpawn(t *testing.T) {
	tests := []struct {
		name       string
		outputs    []StageOutput
		restartAt  int
		wantRows   []float64
		wantRunUSD float64
	}{
		{
			name: "unchanged cumulative cost on reattach",
			outputs: []StageOutput{
				{SpawnID: "spawn-shared", CostUSD: 2.62},
				{SpawnID: "spawn-shared", CostUSD: 2.62},
			},
			wantRows:   []float64{2.62, 0},
			wantRunUSD: 2.62,
		},
		{
			name: "growth after zero-cost reattach uses total prior attribution",
			outputs: []StageOutput{
				{SpawnID: "spawn-shared", CostUSD: 2.62},
				{SpawnID: "spawn-shared", CostUSD: 2.62},
				{SpawnID: "spawn-shared", CostUSD: 3.00},
			},
			wantRows:   []float64{2.62, 0, 0.38},
			wantRunUSD: 3.00,
		},
		{
			name: "distinct spawns retain full cost",
			outputs: []StageOutput{
				{SpawnID: "spawn-one", CostUSD: 1.25},
				{SpawnID: "spawn-two", CostUSD: 0.75},
			},
			wantRows:   []float64{1.25, 0.75},
			wantRunUSD: 2.00,
		},
		{
			name: "empty spawn ids retain full cost",
			outputs: []StageOutput{
				{CostUSD: 0.30},
				{CostUSD: 0.20},
			},
			wantRows:   []float64{0.30, 0.20},
			wantRunUSD: 0.50,
		},
		{
			name: "lower cumulative report floors attribution at zero",
			outputs: []StageOutput{
				{SpawnID: "spawn-shared", CostUSD: 1.50},
				{SpawnID: "spawn-shared", CostUSD: 1.25},
			},
			wantRows:   []float64{1.50, 0},
			wantRunUSD: 1.50,
		},
		{
			name: "restart derives attribution from persisted rows",
			outputs: []StageOutput{
				{SpawnID: "spawn-shared", CostUSD: 1.50},
				{SpawnID: "spawn-shared", CostUSD: 1.90},
			},
			restartAt:  1,
			wantRows:   []float64{1.50, 0.40},
			wantRunUSD: 1.90,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, run, item := newRunnerEnv(t)
			dispatcher := &fakeDispatcher{canned: map[string]StageOutput{}}
			runner := New(st, nil, dispatcher, nil)
			stage := Stage{ID: "implement", Type: "llm", State: store.PipelineImplementing}

			for i, output := range tt.outputs {
				if tt.restartAt == i {
					runner = New(st, nil, dispatcher, nil)
					var err error
					run, err = st.Pipeline.GetRun(context.Background(), run.ID)
					if err != nil {
						t.Fatalf("reload run after restart: %v", err)
					}
				}
				dispatcher.canned[stage.ID] = output
				if _, err := runner.runStage(context.Background(), run, item, stage, nil, i+1, nil); err != nil {
					t.Fatalf("run attempt %d: %v", i+1, err)
				}
			}

			storedRun, err := st.Pipeline.GetRun(context.Background(), run.ID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			rows, err := st.Pipeline.ListStages(context.Background(), run.ID)
			if err != nil {
				t.Fatalf("list stages: %v", err)
			}
			if len(rows) != len(tt.wantRows) {
				t.Fatalf("stage rows = %d, want %d", len(rows), len(tt.wantRows))
			}
			var ledgerTotal float64
			for i, row := range rows {
				ledgerTotal += row.CostUSD
				if math.Abs(row.CostUSD-tt.wantRows[i]) > 1e-9 {
					t.Errorf("row %d cost = %.12f, want %.12f", i+1, row.CostUSD, tt.wantRows[i])
				}
			}
			if math.Abs(storedRun.CostUSD-tt.wantRunUSD) > 1e-9 {
				t.Errorf("run cost = %.12f, want %.12f", storedRun.CostUSD, tt.wantRunUSD)
			}
			if math.Abs(ledgerTotal-storedRun.CostUSD) > 1e-9 {
				t.Errorf("stage ledger total = %.12f, run cost = %.12f", ledgerTotal, storedRun.CostUSD)
			}
		})
	}
}

func TestRunner_GateFailRetriesUpstreamThenEscalates(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	item.TargetProject = "services/procmodel"
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("set target project: %v", err)
	}
	disp := &fakeDispatcher{}
	// Register one gate that always fails so post_implement_gate fails.
	gr := gates.NewRegistry()
	gr.Register(&alwaysFailGate{name: "diff_size"})
	harness := &telemetry.GateDeterminismHarness{}
	gr.SetTelemetrySink(harness)

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
	// The escalation freezes the item's TargetProject onto the run's event
	// subject — the ghost-spark merged-branch sweep authorizes cross-repo
	// lookups against this binding, never the mutable backlog field.
	binding, err := st.Events.FirstBySubjectKind(
		context.Background(), "pipeline_run", run.ID, mills.EscalationTargetBindingKind,
	)
	if err != nil {
		t.Fatalf("escalation target binding event: %v", err)
	}
	if got, _ := binding.Payload["target_project"].(string); got != "services/procmodel" {
		t.Errorf("binding target_project = %q, want services/procmodel", got)
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
	records := harness.Records()
	if len(records) != 3 {
		t.Fatalf("gate telemetry records = %d, want 3", len(records))
	}
	for i, record := range records {
		if record.RunID != run.ID {
			t.Errorf("gate telemetry record[%d].RunID = %q, want %q", i, record.RunID, run.ID)
		}
	}
}

// TestRunner_CIWatchTerminalFailureEscalatesWithoutRetry guards escalation
// #292 (2026-07-08): a ci_watch whose pipeline reached a terminal non-success
// state re-polls the SAME dead pipeline on retry (attempts 2 and 3 completed
// in 2.1s / 1.3s with zero new signal). The runner must escalate on first
// sight, keeping the classified error class (code) in the reason marker.
func TestRunner_CIWatchTerminalFailureEscalatesWithoutRetry(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{
		canned: map[string]StageOutput{
			"implement": {
				CostUSD:        0.10,
				FilesChanged:   []string{"foo.go"},
				LinesAdded:     5,
				DiffPatch:      []byte("diff --git a/foo.go b/foo.go\n+x\n"),
				CommitMessages: []string{"feat: x"},
			},
			"mr": {CostUSD: 0.05, MRIID: 42},
		},
		errFor: map[string]error{
			"ci_watch": fmt.Errorf("ci pipeline failed for mr 42: %w", ErrCIPipelineTerminal),
		},
	}
	esc := &reasonCapturingEscalator{}
	r := New(st, newPassingGates(t), disp, nil)
	r.Escalator = esc
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
	ciCalls := 0
	for _, c := range disp.callsList() {
		if c == "ci_watch" {
			ciCalls++
		}
	}
	if ciCalls != 1 {
		t.Errorf("ci_watch calls = %d, want 1 (terminal pipeline state must not be re-watched)", ciCalls)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	reason := esc.reasons[0]
	if !strings.Contains(reason, "[class=code]") {
		t.Errorf("reason missing [class=code] marker: %q", reason)
	}
	if !strings.Contains(reason, "not retried") {
		t.Errorf("reason missing not-retried note: %q", reason)
	}
}

func TestRunner_CIWatchRunnerSystemFailureEscalatesRetryably(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{
		canned: map[string]StageOutput{
			"implement": {FilesChanged: []string{"foo.go"}, DiffPatch: []byte("diff --git a/foo.go b/foo.go\n+x\n")},
			"mr":        {MRIID: 42},
		},
		errFor: map[string]error{
			"ci_watch": &CIPipelineTerminalError{Status: "failed", MRIID: 42, FailedJobReasons: []string{"runner_system_failure"}},
		},
	}
	esc := &reasonCapturingEscalator{}
	r := New(st, newPassingGates(t), disp, nil)
	r.Escalator = esc
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.EscalationClass != string(ClassTransient) || got.EscalationRetryable == nil || !*got.EscalationRetryable {
		t.Fatalf("escalation class=%q retryable=%v, want transient/true", got.EscalationClass, got.EscalationRetryable)
	}
	if len(esc.reasons) != 1 || !strings.Contains(esc.reasons[0], "retryable CI runner-system failure") {
		t.Fatalf("escalation reasons = %v, want runner-system failure guidance", esc.reasons)
	}
}

// A terminal gate verdict (Outcome.Terminal) is a function of the item's own
// state — re-running RetryFrom cannot flip it. The runner must escalate on
// the FIRST failure instead of burning maxAttempts implement retries
// (escalations #272–#278: 7 slice-less bootstrapped-plan items each spent 3
// attempts + ~$0.60 on the same deterministic no-slices scope fail).
func TestRunner_TerminalGateFailEscalatesWithoutRetry(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	gr := gates.NewRegistry()
	gr.Register(&terminalFailGate{name: "scope"})
	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
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
	implementCalls := 0
	for _, c := range disp.callsList() {
		if c == "implement" {
			implementCalls++
		}
	}
	if implementCalls != 1 {
		t.Errorf("implement calls = %d, want 1 (terminal fail must not retry)", implementCalls)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	reason := esc.reasons[0]
	if !strings.Contains(reason, "[class=config]") {
		t.Errorf("reason missing [class=config] marker: %q", reason)
	}
	if !strings.Contains(reason, "scope: deterministic fail") {
		t.Errorf("reason missing failing gate detail: %q", reason)
	}
}

// TestRunner_SkipGatePersistsSkipAndProceeds pins the advisory-skip contract:
// a gate returning Skip (e.g. scope on a slice-less item) must NOT escalate the
// run — the pipeline proceeds — and the runner must persist the gate row as
// outcome='skip', not 'fail' (so gate_pass_rate can exclude it). Live
// 2026-07-16: a slice-less item false-failed scope and escalated a fine diff.
func TestRunner_SkipGatePersistsSkipAndProceeds(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	gr := gates.NewRegistry()
	gr.Register(&skipGate{name: "scope"})
	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Errorf("state = %s, want done (a skip must not block or escalate)", got.State)
	}
	if len(esc.reasons) != 0 {
		t.Errorf("a skip must not escalate, got %v", esc.reasons)
	}
	rows, err := st.Pipeline.ListGates(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list gates: %v", err)
	}
	sawSkip := false
	for _, gr := range rows {
		if gr.GateName != "scope" {
			continue
		}
		if gr.Outcome != store.GateOutcomeSkip {
			t.Errorf("scope gate persisted outcome=%s, want skip", gr.Outcome)
		}
		sawSkip = true
	}
	if !sawSkip {
		t.Errorf("expected a persisted scope gate_outcomes row, got %+v", rows)
	}
}

// gateInputFor must stamp ProjectBootstrapped from the bootstrapped_projects
// registry so the scope gate can relax its no-slices fail for runtime-minted
// repos. Matching is by RepoBase: the registry stores the full
// PathWithNamespace while items may carry a bare TargetProject.
func TestRunner_GateInputStampsProjectBootstrapped(t *testing.T) {
	st, _, item := newRunnerEnv(t)
	ctx := context.Background()
	if err := st.Bootstrap.Insert(ctx, &store.BootstrappedProject{
		Project:   "millwork/procmodel",
		PlanID:    "plan-1",
		CreatedBy: "test",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed bootstrap: %v", err)
	}
	r := New(st, gates.NewRegistry(), &fakeDispatcher{}, nil)

	item.TargetProject = "procmodel" // bare name still matches the registry row
	in := r.gateInputFor(ctx, Stage{ID: "post_implement_gate"}, item, nil, nil)
	if !in.ProjectBootstrapped {
		t.Errorf("expected ProjectBootstrapped=true for registered project")
	}

	item.TargetProject = "services/loom-flightdeck"
	in = r.gateInputFor(ctx, Stage{ID: "post_implement_gate"}, item, nil, nil)
	if in.ProjectBootstrapped {
		t.Errorf("expected ProjectBootstrapped=false for unregistered project")
	}

	item.TargetProject = ""
	in = r.gateInputFor(ctx, Stage{ID: "post_implement_gate"}, item, nil, nil)
	if in.ProjectBootstrapped {
		t.Errorf("expected ProjectBootstrapped=false for home-repo item")
	}
}

// gateInputFor must stamp TestsPassed from the presence of a successful
// "tests" stage output (prior[] rows are only written after a stage completes
// without error), so LLM judges grade with the deterministic build verdict
// instead of fabricating compile failures (escalation #304).
func TestRunner_GateInputStampsTestsPassed(t *testing.T) {
	st, _, item := newRunnerEnv(t)
	ctx := context.Background()
	r := New(st, gates.NewRegistry(), &fakeDispatcher{}, nil)

	in := r.gateInputFor(ctx, Stage{ID: "post_review_gate"}, item, nil,
		map[string]StageOutput{"tests": {}})
	if !in.TestsPassed {
		t.Errorf("expected TestsPassed=true when a tests stage output exists")
	}

	in = r.gateInputFor(ctx, Stage{ID: "post_review_gate"}, item, nil,
		map[string]StageOutput{"implement": {}})
	if in.TestsPassed {
		t.Errorf("expected TestsPassed=false without a tests stage output")
	}

	in = r.gateInputFor(ctx, Stage{ID: "post_review_gate"}, item, nil, nil)
	if in.TestsPassed {
		t.Errorf("expected TestsPassed=false with no prior outputs")
	}
}

// gateInputFor must lift the cumulative-git-capture provenance the spawn
// client stamps on the implement stage so nonempty_diff can tell a branch
// that carries no work from one the pipeline simply could not see
// (issue #224). Legacy rows with no artifact keep the empty defaults.
func TestRunner_GateInputStampsGitCaptureProvenance(t *testing.T) {
	st, _, item := newRunnerEnv(t)
	ctx := context.Background()
	r := New(st, gates.NewRegistry(), &fakeDispatcher{}, nil)

	in := r.gateInputFor(ctx, Stage{ID: "post_implement_gate"}, item, nil,
		map[string]StageOutput{"implement": {Artifacts: map[string]any{
			GitCaptureArtifactKey: map[string]any{
				"status": "fetch_failed",
				"reason": "branch ref fetch failed (exit 128)",
			},
		}}})
	if in.GitCaptureStatus != "fetch_failed" {
		t.Errorf("GitCaptureStatus = %q, want fetch_failed", in.GitCaptureStatus)
	}
	if in.GitCaptureReason != "branch ref fetch failed (exit 128)" {
		t.Errorf("GitCaptureReason = %q", in.GitCaptureReason)
	}

	in = r.gateInputFor(ctx, Stage{ID: "post_implement_gate"}, item, nil,
		map[string]StageOutput{"implement": {}})
	if in.GitCaptureStatus != "" || in.GitCaptureReason != "" {
		t.Errorf("legacy row must leave capture provenance empty; got %q/%q", in.GitCaptureStatus, in.GitCaptureReason)
	}
}

// M2.5 + issue #348: post_review_gate's LLM-judged gates (spec_conformance,
// pr_self_review) must not escalate finished work when the judge returns an
// unparseable/empty score envelope (Outcome.Pass=false, JudgedBy=
// flexinfer:unparseable, err=nil). The M2.5 contract recovered this by
// rewinding to the pr_self_review RetryFrom stage and RESPAWNING the agent.
// Issue #348 showed that respawn is wasteful: an unreadable judge verdict is a
// model-provider miss, not a defect in the (already-successful) diff, so
// respawning the costly agent burns the code-class budget on finished work.
//
// The recovery is now a FREE re-judge: the runner re-runs ONLY the gate (a
// fresh judge call that re-invokes the client's larger-budget recovery) with no
// agent respawn. This test pins that superseding contract — the run completes,
// spec_conformance is re-judged (fail then pass), and pr_self_review is
// dispatched EXACTLY once (never respawned).
//
// Live reproducer (2026-05-16 canary PIPE-MILLS-CANARY-M1D-VERIFY-2):
// gemma4-26b returned free-text instead of a score envelope on the first
// spec_conformance call.
func TestRunner_LLMGateUnparseableOutcomeRecoversViaFreeRejudge(t *testing.T) {
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
	// passes on the free re-judge — the contract under test.
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
		t.Errorf("state = %s, want done (run must complete via free re-judge)", got.State)
	}
	// pr_self_review is the post_review_gate RetryFrom target. The free re-judge
	// must NOT respawn it, so the dispatcher sees it exactly once.
	prCalls := 0
	for _, c := range disp.callsList() {
		if c == "pr_self_review" {
			prCalls++
		}
	}
	if prCalls != 1 {
		t.Errorf("pr_self_review dispatches = %d, want 1 (free re-judge must not respawn the agent)", prCalls)
	}
	// spec_conformance gate evaluated twice (fail, then pass on the free re-judge).
	if flaky.calls != 2 {
		t.Errorf("spec_conformance evaluations = %d, want 2 (fail then pass after free re-judge)", flaky.calls)
	}
}

// TestRunner_JudgeEmptyEnvelope_FreeRejudgeRecoversWithoutRespawn is the
// issue #348 recovery path: the post_review_gate judge returns an ungradeable
// score envelope (raw="", JudgedBy=flexinfer:unparseable) on the first evals,
// then grades cleanly. The runner must recover it with FREE re-judges — a fresh
// judge call per re-run, NO agent respawn of the (already-successful,
// $-costly) pr_self_review RetryFrom stage — so the run completes without
// burning the code-class attempt budget on finished work.
func TestRunner_JudgeEmptyEnvelope_FreeRejudgeRecoversWithoutRespawn(t *testing.T) {
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
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		gr.Register(&alwaysPassGate{name: name})
	}
	// spec_conformance returns the real #348 empty-envelope failure twice (the
	// two FREE re-judges), then grades. maxJudgeUnparseableRetries=2 → the 3rd
	// eval must be reached and pass.
	flaky := &flakyJudgeGate{
		name:       "spec_conformance",
		failFor:    2,
		failJudge:  store.JudgedByUnparseable,
		failReason: issue348JudgeReason,
	}
	gr.Register(flaky)
	gr.Register(&alwaysPassGate{name: "pr_self_review"})

	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	r.Clock = func() time.Time { return time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC) }
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Errorf("state = %s, want done (free re-judge must recover the empty envelope)", got.State)
	}
	if len(esc.reasons) != 0 {
		t.Errorf("empty-envelope recovery must not escalate, got %v", esc.reasons)
	}
	// spec_conformance re-judged 3× (fail, fail, pass) — two FREE re-judges.
	if flaky.calls != 3 {
		t.Errorf("spec_conformance evaluations = %d, want 3 (2 free re-judges then a clean grade)", flaky.calls)
	}
	// The RetryFrom stage (pr_self_review) must be dispatched EXACTLY once: the
	// free re-judge re-runs only the gate, never respawning the agent.
	prCalls := 0
	for _, c := range disp.callsList() {
		if c == "pr_self_review" {
			prCalls++
		}
	}
	if prCalls != 1 {
		t.Errorf("pr_self_review dispatches = %d, want 1 (free re-judge must not respawn the agent)", prCalls)
	}
}

// TestRunner_JudgeEmptyEnvelope_PersistentEscalatesExternalDependency is the
// issue #348 terminal path: the judge NEVER emits a gradeable envelope (raw=""
// on every eval). After the bounded free re-judges are exhausted the run must
// escalate as an EXTERNAL model-provider dependency incident (class=config,
// retryable=false) — NOT as a code defect — and it must NOT respawn the costly
// pr_self_review RetryFrom stage (which would burn the code-class budget on
// finished work, the $2 waste in #348).
func TestRunner_JudgeEmptyEnvelope_PersistentEscalatesExternalDependency(t *testing.T) {
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
		"mr": {CostUSD: 0.05, MRIID: 42},
	}}
	gr := gates.NewRegistry()
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		gr.Register(&alwaysPassGate{name: name})
	}
	// spec_conformance never grades — persistent empty envelope.
	flaky := &flakyJudgeGate{
		name:       "spec_conformance",
		failFor:    1000,
		failJudge:  store.JudgedByUnparseable,
		failReason: issue348JudgeReason,
	}
	gr.Register(flaky)
	gr.Register(&alwaysPassGate{name: "pr_self_review"})

	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	r.Clock = func() time.Time { return time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC) }
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
	// Exactly 1 initial eval + maxJudgeUnparseableRetries (2) free re-judges = 3.
	if flaky.calls != 1+maxJudgeUnparseableRetries {
		t.Errorf("spec_conformance evaluations = %d, want %d (initial + %d free re-judges)",
			flaky.calls, 1+maxJudgeUnparseableRetries, maxJudgeUnparseableRetries)
	}
	// pr_self_review must be dispatched EXACTLY once — the persistent empty
	// envelope must never respawn the agent and burn the code-class budget.
	prCalls := 0
	for _, c := range disp.callsList() {
		if c == "pr_self_review" {
			prCalls++
		}
	}
	if prCalls != 1 {
		t.Errorf("pr_self_review dispatches = %d, want 1 (external-dependency escalation must not respawn)", prCalls)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	reason := esc.reasons[0]
	// Classified as external dependency (config), NOT code.
	if !strings.Contains(reason, "[class=config]") {
		t.Errorf("reason missing [class=config] marker: %q", reason)
	}
	if strings.Contains(reason, "[class=code]") {
		t.Errorf("reason must NOT be code-classed: %q", reason)
	}
	if escalationClassLabel(ClassConfig, reason, "") != "config" {
		t.Errorf("escalationClassLabel = %q, want config", escalationClassLabel(ClassConfig, reason, ""))
	}
	if !strings.Contains(reason, "external_dependency.model_provider.judge_ungradeable_envelope") {
		t.Errorf("reason missing model-provider incident id: %q", reason)
	}
	if !strings.Contains(reason, "llm_judge_provider") {
		t.Errorf("reason missing judge-provider dependency: %q", reason)
	}
	// Escalation metadata must stamp the external dependency (via the reason).
	md := escalationMetadataFromEvidence(ClassConfig, reason, "")
	if md.ExternalDependencyID != "external_dependency.model_provider.judge_ungradeable_envelope" ||
		md.ExternalDependency != "llm_judge_provider" {
		t.Errorf("escalation metadata external dependency = id %q dep %q, want model-provider incident",
			md.ExternalDependencyID, md.ExternalDependency)
	}
	if md.Retryable == nil || *md.Retryable {
		t.Errorf("external-dependency escalation must be retryable=false, got %v", md.Retryable)
	}
}

// erroringJudgeGate ERRORS (no verdict at all) on its first failFor calls, then
// passes. This is the LLMGate transport contract: a judge call that fails at the
// network/provider layer returns err (not a Pass=false outcome), so the runner —
// not the gate — decides the class and the retry.
type erroringJudgeGate struct {
	name    string
	failFor int
	calls   int
	err     error
}

func (g *erroringJudgeGate) Name() string { return g.name }
func (g *erroringJudgeGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	g.calls++
	if g.calls <= g.failFor {
		return gates.Outcome{}, g.err
	}
	return gates.Outcome{Pass: true, JudgedBy: "flexinfer:qwen-3-8b"}, nil
}

// issue378JudgeTransportError is the live #378 shape: the spec_conformance judge
// call failed against LiteLLM/OpenRouter with a provider 400 after the fallback
// chain was exhausted. No verdict was ever produced on the diff.
const issue378JudgeTransportError = `spec_conformance: judge: rubric judge: flexinfer chat: every candidate model rejected the request (tried [or/kimi-k3]): flexinfer chat: status 400: {"error":{"message":"litellm.BadRequestError: OpenrouterException - Provider returned error. Available Model Group Fallbacks=None"}}`

// TestClassifyGateError_NeverBlamesTheDiff pins the gate-error class contract:
// a gate that errored produced NO verdict, so it can never be code-class. Each
// case is a live judge-transport failure shape from issue #378.
func TestClassifyGateError_NeverBlamesTheDiff(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		want     ErrorClass
		external bool
	}{
		{
			name: "litellm provider 400 (chain exhausted)",
			err:  errors.New(issue378JudgeTransportError),
			want: ClassInfra,
		},
		{
			name: "all candidate models rate limited / parked",
			err:  fmt.Errorf("spec_conformance: judge: flexinfer chat: all candidate models unavailable (tried [a b]): %w (last: status 429)", ErrModelUnavailable),
			want: ClassTransient,
		},
		{
			name: "judge call timed out",
			err:  errors.New("spec_conformance: judge: flexinfer chat: context deadline exceeded"),
			want: ClassTransient,
		},
		{
			name:     "gitlab credential failure surfaced through a gate",
			err:      errors.New("scope: gitlab: GET /projects/47: status 401: unauthorized"),
			want:     ClassConfig,
			external: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, incident, external := classifyGateError(tt.err)
			if got != tt.want {
				t.Errorf("class = %q, want %q", got, tt.want)
			}
			if got == ClassCode {
				t.Error("a gate error must never be code-class: the diff was never graded")
			}
			if external != tt.external {
				t.Errorf("external = %v, want %v (incident %+v)", external, tt.external, incident)
			}
			if tt.external && incident.Dependency == "" {
				t.Error("an external incident must name its dependency")
			}
		})
	}
}

// TestRunner_GateTransportError_FreeRejudgeRecoversWithoutRespawn is the #378
// recovery path: the post_review_gate judge CALL fails (litellm 400) and then
// succeeds. The runner must recover it with a FREE re-judge — gate-only re-run,
// no respawn of the already-successful pr_self_review stage — instead of
// escalating on first sight as it did before.
func TestRunner_GateTransportError_FreeRejudgeRecoversWithoutRespawn(t *testing.T) {
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
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		gr.Register(&alwaysPassGate{name: name})
	}
	flaky := &erroringJudgeGate{name: "spec_conformance", failFor: 1, err: errors.New(issue378JudgeTransportError)}
	gr.Register(flaky)
	gr.Register(&alwaysPassGate{name: "pr_self_review"})

	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	r.Clock = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Errorf("state = %s, want done (free re-judge must recover the transport failure)", got.State)
	}
	if len(esc.reasons) != 0 {
		t.Errorf("transport recovery must not escalate, got %v", esc.reasons)
	}
	if flaky.calls != 2 {
		t.Errorf("spec_conformance evaluations = %d, want 2 (error then a clean grade)", flaky.calls)
	}
	prCalls := 0
	for _, c := range disp.callsList() {
		if c == "pr_self_review" {
			prCalls++
		}
	}
	if prCalls != 1 {
		t.Errorf("pr_self_review dispatches = %d, want 1 (free re-judge must not respawn the agent)", prCalls)
	}
}

// TestRunner_GateTransportError_PersistentEscalatesClassified is the #378
// terminal path: the judge call fails on every attempt. After the bounded free
// re-judges the run escalates CLASSIFIED — [class=infra], retryable, original
// provider error preserved — so SweepAutoRequeue can pick it up instead of
// skipping it as "unclassified" (the defect: the reason carried no class marker
// at all and the item parked permanently).
func TestRunner_GateTransportError_PersistentEscalatesClassified(t *testing.T) {
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
		"mr": {CostUSD: 0.05, MRIID: 42},
	}}
	gr := gates.NewRegistry()
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		gr.Register(&alwaysPassGate{name: name})
	}
	flaky := &erroringJudgeGate{name: "spec_conformance", failFor: 1000, err: errors.New(issue378JudgeTransportError)}
	gr.Register(flaky)
	gr.Register(&alwaysPassGate{name: "pr_self_review"})

	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	r.Clock = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
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
	if flaky.calls != 1+maxJudgeTransportRetries {
		t.Errorf("spec_conformance evaluations = %d, want %d (initial + %d free re-judges)",
			flaky.calls, 1+maxJudgeTransportRetries, maxJudgeTransportRetries)
	}
	prCalls := 0
	for _, c := range disp.callsList() {
		if c == "pr_self_review" {
			prCalls++
		}
	}
	if prCalls != 1 {
		t.Errorf("pr_self_review dispatches = %d, want 1 (a failed judge call must not respawn the agent)", prCalls)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	reason := esc.reasons[0]
	if !strings.Contains(reason, "[class=infra]") {
		t.Errorf("reason missing [class=infra] marker: %q", reason)
	}
	if strings.Contains(reason, "[class=code]") {
		t.Errorf("a failed judge CALL must never be code-classed: %q", reason)
	}
	// The whole point: escalationClassFromReason must resolve a class so
	// SweepAutoRequeue sees a retryable fault instead of "unclassified".
	if escalationClassLabel(ClassInfra, reason, "") != "infra" {
		t.Errorf("escalationClassLabel = %q, want infra", escalationClassLabel(ClassInfra, reason, ""))
	}
	// Operators diagnose from the provider's own words.
	if !strings.Contains(reason, "litellm.BadRequestError") {
		t.Errorf("reason must preserve the original judge error text: %q", reason)
	}
	md := escalationMetadataFromEvidence(ClassInfra, reason, "")
	if md.Retryable == nil || !*md.Retryable {
		t.Errorf("infra escalation must be retryable=true, got %v", md.Retryable)
	}
	if md.FailureClass != string(FailureInfrastructure) {
		t.Errorf("failure class = %q, want %q", md.FailureClass, FailureInfrastructure)
	}
}

// TestRunner_GateExternalIncidentError_EscalatesOnFirstSight guards the
// non-free branch: a gate error that matches a known external dependency
// incident (here a GitLab credential failure) is terminal for this run — no
// free re-judges — and escalates as [class=config] with the incident named.
func TestRunner_GateExternalIncidentError_EscalatesOnFirstSight(t *testing.T) {
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
		"mr": {CostUSD: 0.05, MRIID: 42},
	}}
	gr := gates.NewRegistry()
	for _, name := range []string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"} {
		gr.Register(&alwaysPassGate{name: name})
	}
	flaky := &erroringJudgeGate{
		name:    "spec_conformance",
		failFor: 1000,
		err:     errors.New("spec_conformance: judge: gitlab: GET /projects/47/repository: status 401: unauthorized"),
	}
	gr.Register(flaky)
	gr.Register(&alwaysPassGate{name: "pr_self_review"})

	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	r.Clock = func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) }
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if flaky.calls != 1 {
		t.Errorf("spec_conformance evaluations = %d, want 1 (a known incident must not burn re-judges)", flaky.calls)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	reason := esc.reasons[0]
	if !strings.Contains(reason, "[class=config]") {
		t.Errorf("reason missing [class=config] marker: %q", reason)
	}
	md := escalationMetadataFromEvidence("", reason, "")
	if md.ExternalDependency != "gitlab" {
		t.Errorf("external dependency = %q, want gitlab", md.ExternalDependency)
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

// reconciledFailedSpawnDispatcher models the second observation of a wedged
// spawn: the original poll timed out, then the spawn controller reconciled the
// same SpawnID to a terminal failure.
type reconciledFailedSpawnDispatcher struct{}

func (reconciledFailedSpawnDispatcher) Dispatch(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, _ Stage, _ map[string]StageOutput) (StageOutput, error) {
	return StageOutput{
		SpawnID:   "spawn-wedged",
		Artifacts: map[string]any{"status": "failed"},
	}, fmt.Errorf("hud spawn spawn-wedged status=failed: spawn deadline exceeded during reconciliation: %w", ErrSpawnTerminalFailure)
}

func TestRunner_DedupesPollTimeoutAndReconciledSpawnFailure(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	firstOutcome := store.StageOutcomeError
	if err := st.Pipeline.PutStage(context.Background(), &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "plan_slice",
		Attempt:       1,
		StartedAt:     run.StartedAt,
		EndedAt:       &run.StartedAt,
		Outcome:       &firstOutcome,
		SpawnID:       "spawn-wedged",
		LogTail:       "stage=plan_slice attempt=1 spawn=spawn-wedged: hud spawn: poll timeout after 30m0s: " + ErrSpawnPollTimeout.Error(),
	}); err != nil {
		t.Fatalf("seed poll-timeout attempt: %v", err)
	}

	r := New(st, nil, reconciledFailedSpawnDispatcher{}, nil)
	stage := Stage{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning}
	_, err := r.runStage(context.Background(), run, item, stage, nil, 2, nil)
	var deduped *dedupedStageAttemptError
	if !errors.As(err, &deduped) {
		t.Fatalf("runStage error = %v, want dedupedStageAttemptError", err)
	}
	if deduped.attempt != 1 {
		t.Errorf("deduped attempt = %d, want 1", deduped.attempt)
	}

	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stage rows = %d, want one row for spawn-wedged", len(stages))
	}
	if stages[0].Attempt != 1 || stages[0].SpawnID != "spawn-wedged" {
		t.Errorf("stage row = attempt %d spawn %q, want attempt 1 / spawn-wedged", stages[0].Attempt, stages[0].SpawnID)
	}
	if !strings.Contains(stages[0].LogTail, "spawn deadline exceeded during reconciliation") {
		t.Errorf("updated log tail %q missing reconciliation failure", stages[0].LogTail)
	}
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
	if n := pendingPollFailures(stages[0]); n != 1 {
		t.Errorf("pending poll-failure counter = %d, want 1", n)
	}
}

// reapedPodDispatcher models a spawn whose pod was GC'd/reaped: every poll
// (fresh dispatch or resume re-attach) returns a NON-timeout error with the
// spawn id set and a non-terminal status — the shape HUDSpawnClient produces
// when the spawn detail endpoint reports the pod is gone ("pod not found
// during reconciliation", the k8s-spawn-pod-GC transient bucket). Before the
// fix this hit runStage's non-timeout branch, which re-parked pending with an
// UNCHANGED counter forever: the run wedged active-but-idle across restarts,
// re-attaching to the same dead spawn every reconciler tick, never escalating.
type reapedPodDispatcher struct {
	mu       sync.Mutex
	stage    string
	calls    int
	fresh    int
	spawnIDs map[string]bool
}

func (d *reapedPodDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if stage.ID != d.stage {
		return StageOutput{CostUSD: 0.01}, nil
	}
	d.calls++
	spawnID := resumeSpawnIDFromContext(ctx)
	if spawnID == "" {
		d.fresh++
		spawnID = fmt.Sprintf("spawn-reaped-%d", d.fresh)
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
	// Non-timeout error (a 404/GC-shaped failure), spawn id set, non-terminal.
	return StageOutput{SpawnID: spawnID},
		fmt.Errorf("hud spawn %s: pod not found during reconciliation", spawnID)
}

func (d *reapedPodDispatcher) snapshot() (calls, fresh, distinct int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, d.fresh, len(d.spawnIDs)
}

// TestRunner_ReattachErrorEscalatesInsteadOfLoopingPending pins the 2026-07-01
// wedge: an operator restart resumes a run stuck mid-implement, whose spawn pod
// was reaped. The resumed drive re-attaches to the dead spawn and its poll
// returns a NON-timeout error. Before the fix, runStage re-parked pending with
// an unchanged counter on every non-timeout error, so the reconciler re-drove
// the run each tick, it re-attached to the same dead spawn, re-parked, and the
// run sat active-but-idle forever ("pipeline start skipped; run already active
// in this operator" every minute, no spawn pod, no escalation). The fix counts
// consecutive non-terminal poll failures — timeout OR error — and, past the
// stall tolerance, converts to an errored attempt so a fresh spawn is
// re-dispatched and the run ultimately escalates.
func TestRunner_ReattachErrorEscalatesInsteadOfLoopingPending(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	// Seed the mid-implement wedge exactly as it survives a restart: a
	// non-terminal run head + a pending stage_results row (outcome NULL,
	// spawn_id set) that ListInFlight/Runner.Drive re-attach to on resume.
	run.CurrentStage = "implement"
	run.State = store.PipelineImplementing
	if err := st.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("seed run head: %v", err)
	}
	if err := st.Pipeline.PutStage(context.Background(), &store.StageResult{
		PipelineRunID: run.ID, Stage: "implement", Attempt: 1,
		StartedAt: run.StartedAt, SpawnID: "spawn-reaped-orphan",
		Artifacts: map[string]any{"stage_id": "implement"},
	}); err != nil {
		t.Fatalf("seed pending stage: %v", err)
	}

	disp := &reapedPodDispatcher{stage: "implement"}
	r := New(st, nil, disp, nil)
	r.Stages = []Stage{{ID: "implement", Type: "spawn", State: store.PipelineImplementing}}
	// Disable escalation auto-retry so the hard cap escalates the item
	// directly (deterministic terminal state for the assertion).
	r.Policy = newPolicyMgrWithRetryCap(t, 0)

	// Simulate the operator restart's resume followed by the reconciler
	// re-driving the in-flight run each tick (ResumeInFlightRuns and
	// pickupInFlightRuns both just call Starter.Start → Runner.Drive).
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
		if got.State != store.PipelineImplementing {
			t.Fatalf("tick %d: unexpected run state %s", ticks, got.State)
		}
		run = got // mirror the reconciler re-reading the row each tick
	}
	if !escalated {
		t.Fatalf("run never escalated after %d re-drives; reaped-pod re-attach looped pending forever (the wedge)", maxTicks)
	}

	gotItem, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if gotItem.State != store.BacklogEscalated {
		t.Errorf("backlog state = %s, want escalated", gotItem.State)
	}

	calls, fresh, distinct := disp.snapshot()
	if fresh < 2 {
		t.Errorf("fresh spawns = %d; want >=2 (stall must re-dispatch, not only re-attach the dead spawn)", fresh)
	}
	if distinct < 2 {
		t.Errorf("distinct spawn ids = %d; want >=2", distinct)
	}
	t.Logf("escalated after %d ticks: %d dispatch calls, %d fresh spawns, %d distinct ids", ticks+1, calls, fresh, distinct)
}

// escalatingDispatcher simulates the manual /escalate handler firing (writing a
// terminal escalated row) while this Drive goroutine is blocked inside a stage
// dispatch — the race behind the 2026-07-01 "escalate isn't durable" wedge. It
// escalates the run out-of-band exactly once, then returns a successful stage
// output as if the spawn had completed.
type escalatingDispatcher struct {
	st    *store.Store
	runID string
	fired bool
}

func (d *escalatingDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	if !d.fired {
		d.fired = true
		cur, err := d.st.Pipeline.GetRun(ctx, d.runID)
		if err == nil {
			cur.State = store.PipelineEscalated
			ended := cur.StartedAt
			cur.EndedAt = &ended
			_ = d.st.Pipeline.PutRun(ctx, cur)
		}
	}
	return StageOutput{SpawnID: "spawn-completed", CostUSD: 0.02}, nil
}

// TestRunner_OutOfBandEscalationSurvivesResumeDrive pins fix direction 2: a run
// escalated out-of-band (by the /escalate handler) while a resumed Drive
// goroutine is mid-stage must STAY escalated. Before the fix the goroutine's
// stale in-memory run.State clobbered the escalated row back to the stage
// state when it persisted the stage rollup, so the next restart's resume
// re-activated the run — the run the operator had explicitly escalated.
func TestRunner_OutOfBandEscalationSurvivesResumeDrive(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	run.CurrentStage = "implement"
	run.State = store.PipelineImplementing
	if err := st.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("seed run head: %v", err)
	}

	disp := &escalatingDispatcher{st: st, runID: run.ID}
	r := New(st, nil, disp, nil)
	r.Stages = []Stage{
		{ID: "implement", Type: "spawn", State: store.PipelineImplementing},
		{ID: "mr", Type: "spawn", State: store.PipelineMR},
	}

	// errRunTerminated is swallowed to a clean nil stop inside Drive.
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("run state = %s, want escalated (resume drive clobbered the out-of-band escalation)", got.State)
	}

	// The mr stage must never have run — the drive bailed at the implement
	// stage the moment it saw the terminal head.
	stages, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	for _, s := range stages {
		if s.Stage == "mr" {
			t.Fatalf("mr stage ran after out-of-band escalation; drive did not bail")
		}
	}
}

// TestPipelinePause_InFlightDriveCannotUnpause pins the pause counterpart to
// the external-escalation regression above. A dispatcher may finish after an
// operator pauses the run, but its stale rollup must never resurrect work.
func TestPipelinePause_InFlightDriveCannotUnpause(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	run.CurrentStage = "implement"
	run.State = store.PipelineImplementing
	if err := st.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("seed run head: %v", err)
	}

	r := New(st, nil, nil, nil)
	r.Stages = []Stage{{ID: "implement", Type: "spawn", State: store.PipelineImplementing}}

	// The dispatcher writes the same terminal outcome the HTTP pause handler
	// persists while Drive is blocked in a live stage dispatch.
	dispatcher := pipelinePauseDispatcher{st: st, runID: run.ID}
	r.Dispatcher = &dispatcher
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelinePaused {
		t.Fatalf("run state = %s, want paused (in-flight drive resurrected it)", got.State)
	}
}

func TestPipelinePause_PausedRunExcludedFromStartupResumeSweep(t *testing.T) {
	st, run, _ := newRunnerEnv(t)
	ctx := context.Background()
	run.CurrentStage = "implement"
	run.State = store.PipelinePaused
	now := time.Now().UTC()
	run.EndedAt = &now
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("pause run: %v", err)
	}

	inFlight, err := st.Pipeline.ListInFlight(ctx)
	if err != nil {
		t.Fatalf("list in-flight: %v", err)
	}
	for _, got := range inFlight {
		if got.ID == run.ID {
			t.Fatalf("paused run %s appeared in startup resume sweep: %+v", run.ID, inFlight)
		}
	}
	active, err := st.Pipeline.CountActive(ctx)
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 0 {
		t.Fatalf("active count = %d, want 0 for paused-only store", active)
	}
}

func TestPipelinePause_ResumeQueuesBacklogExactlyOnce(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	ctx := context.Background()
	item.State = store.BacklogPaused
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("pause backlog: %v", err)
	}
	run.State = store.PipelinePaused
	now := time.Now().UTC()
	run.EndedAt = &now
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("pause run: %v", err)
	}
	observed := *run

	if err := st.Pipeline.ResumePausedRunWithBacklog(ctx, &observed, store.BacklogPaused); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	queued, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get queued backlog: %v", err)
	}
	if observed.State != store.PipelineQueued || queued.State != store.BacklogQueued {
		t.Fatalf("resume states: run=%s backlog=%s, want queued/queued", observed.State, queued.State)
	}
	if err := st.Pipeline.ResumePausedRun(ctx, run); err == nil {
		t.Fatal("second resume with stale paused run succeeded, want conflict")
	}
	got, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if got.State != store.BacklogQueued {
		t.Fatalf("backlog state after duplicate resume = %s, want queued", got.State)
	}
}

type pipelinePauseDispatcher struct {
	st    *store.Store
	runID string
}

func (d *pipelinePauseDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, _ Stage, _ map[string]StageOutput) (StageOutput, error) {
	cur, err := d.st.Pipeline.GetRun(ctx, d.runID)
	if err != nil {
		return StageOutput{}, err
	}
	now := time.Now().UTC()
	cur.State, cur.EndedAt = store.PipelinePaused, &now
	if err := d.st.Pipeline.PutRun(ctx, cur); err != nil {
		return StageOutput{}, err
	}
	return StageOutput{SpawnID: "spawn-completed", CostUSD: 0.02}, nil
}

// TestRunner_DriveBailsWhenRunEscalatedOutOfBand covers the loop-top guard: a
// run already terminal in the store before Drive touches its next stage must
// be a no-op — the dispatcher is never invoked and the terminal state stands.
func TestRunner_DriveBailsWhenRunEscalatedOutOfBand(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	run.CurrentStage = "implement"
	run.State = store.PipelineEscalated
	ended := run.StartedAt
	run.EndedAt = &ended
	if err := st.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("seed run head: %v", err)
	}

	disp := &fakeDispatcher{}
	r := New(st, nil, disp, nil)
	r.Stages = []Stage{{ID: "implement", Type: "spawn", State: store.PipelineImplementing}}

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if calls := disp.callsList(); len(calls) != 0 {
		t.Fatalf("dispatcher invoked %v on an already-escalated run; loop-top guard failed", calls)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("run state = %s, want escalated (unchanged)", got.State)
	}
}

func TestRunner_StartGoroutineReachesTerminal(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	r := New(st, newPassingGates(t), disp, nil)
	if err := r.Start(context.Background(), run, item); err != nil {
		t.Fatalf("start: %v", err)
	}
	r.Wait() // deterministic join — no polling, no leaked goroutine
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone && got.State != store.PipelineEscalated {
		t.Fatalf("Start: run did not reach terminal state, got %s", got.State)
	}
}

func TestRunner_StartDoesNotEscalateAfterDoneTerminalWrite(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	r := New(st, newPassingGates(t), disp, nil)
	esc := &reasonCapturingEscalator{}
	r.Escalator = esc

	// Force markDone to persist the pipeline row as done and then fail while
	// synchronizing the aggregate backlog state. Start must observe the durable
	// terminal run and stop instead of trying to resolve the same run again as
	// escalated.
	current, err := st.Backlog.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("load backlog: %v", err)
	}
	current.State = store.BacklogEscalated
	if err := st.Backlog.Put(context.Background(), current); err != nil {
		t.Fatalf("pre-resolve backlog: %v", err)
	}

	if err := r.Start(context.Background(), run, item); err != nil {
		t.Fatalf("start: %v", err)
	}
	r.Wait()
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Fatalf("run state = %s, want done", got.State)
	}
	if len(esc.reasons) != 0 {
		t.Fatalf("escalator fired after done terminal write: %v", esc.reasons)
	}
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
	r.Wait() // join the single live drive goroutine deterministically
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil || got.State != store.PipelineDone {
		t.Fatalf("run did not finish after releasing dispatcher: state=%v err=%v", got.State, err)
	}
	if calls := disp.callCount(); calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1 (duplicate Start must be suppressed)", calls)
	}
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
	r.Wait() // deterministic join — the fatal drive error escalates in-goroutine
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %s, want escalated", got.State)
	}
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

// A resumed legacy run without persisted ci_sha is a terminal authorization
// failure. The dispatcher refuses to call GitLab, and the runner must escalate
// once instead of spending the merge retry budget on identical failures.
func TestRunner_MissingCISHAEscalatesWithoutRetry(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{errFor: map[string]error{
		"merge": fmt.Errorf("merge: no ci_sha from successful ci_watch: %w", ErrMergeAuthorizationStale),
	}}
	if err := New(st, newPassingGates(t), disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != store.PipelineEscalated || got.EscalationClass != string(ClassConfig) {
		t.Fatalf("run state/class = %s/%q, want escalated/%q", got.State, got.EscalationClass, ClassConfig)
	}
	mergeCalls := 0
	for _, stage := range disp.callsList() {
		if stage == "merge" {
			mergeCalls++
		}
	}
	if mergeCalls != 1 {
		t.Fatalf("merge called %d times, want 1", mergeCalls)
	}
}

// A persistently locked MR is transient, even when GitLab reports the lock
// alongside a 405. The runner may retry it for recovery, but must still stop
// at the transient hard cap instead of looping forever.
func TestRunner_MergeLockedRetriesToTransientHardCap(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{errFor: map[string]error{
		"merge": fmt.Errorf("gitlab merge status 405 while locked: %w", ErrMergeRequestLocked),
	}}
	if err := New(st, newPassingGates(t), disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != store.PipelineEscalated || got.EscalationClass != string(ClassTransient) {
		t.Fatalf("run state/class = %s/%q, want escalated/%q", got.State, got.EscalationClass, ClassTransient)
	}
	mergeCalls := 0
	for _, stage := range disp.callsList() {
		if stage == "merge" {
			mergeCalls++
		}
	}
	if mergeCalls < 7 || mergeCalls > 9 {
		t.Fatalf("merge called %d times, want ~8 (bounded by the transient hard cap)", mergeCalls)
	}
}

func TestRunner_KnownExternalIncidentEscalatesWithoutRetry(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &classedFailDispatcher{
		stage: "tests",
		errs: []string{
			"devbox quality_gate: registry cache export failed: error writing manifest blob: blob upload unknown",
			// Never reached: known external incidents should be surfaced once
			// instead of burning blind retries on the same upstream outage.
			"devbox quality_gate: registry cache export failed: error writing manifest blob: blob upload unknown",
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
		t.Errorf("state = %s; want escalated on first known external incident", got.State)
	}
	if disp.calls != 1 {
		t.Errorf("tests called %d times; want 1 (external incidents must not retry blindly)", disp.calls)
	}
	if got.EscalationClass != string(ClassConfig) ||
		got.FailureClass != string(FailureConfiguration) ||
		got.ExternalDependencyID != "external_dependency.blob_storage.manifest_write" ||
		got.ExternalDependency != "container_registry_blob_storage" ||
		got.EscalationRetryable == nil ||
		*got.EscalationRetryable {
		t.Fatalf("escalation metadata = class=%q failure=%q external_id=%q external=%q retryable=%v; want terminal external blob-storage incident",
			got.EscalationClass, got.FailureClass, got.ExternalDependencyID, got.ExternalDependency, got.EscalationRetryable)
	}
}

func TestRunner_ExternalIncidentPersistsAndDeduplicates(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{errFor: map[string]error{
		"ci_watch": errors.New("gitlab pipeline status 503: service unavailable"),
	}}
	r := New(st, newPassingGates(t), disp, nil)
	r.Stages = []Stage{{ID: "ci_watch", Type: "shell"}}
	previousMetrics := classificationMetrics
	classificationMetrics = telemetry.NewClassificationMetrics(nil)
	t.Cleanup(func() { classificationMetrics = previousMetrics })

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}

	evidence := "gitlab pipeline status 503: service unavailable"
	incident, ok := mcperror.ClassifyExternalCIIncident(evidence)
	if !ok {
		t.Fatal("fixture did not classify as an external incident")
	}
	// A second observation through the common persistence seam must update the
	// row produced by Drive rather than insert a second fingerprint.
	r.recordExternalIncident(context.Background(), incident)

	records, err := st.Incidents.ListSince(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatalf("list incidents: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("incident rows = %d, want 1", len(records))
	}
	if records[0].OccurrenceCount != 2 {
		t.Fatalf("occurrence count = %d, want 2", records[0].OccurrenceCount)
	}
	if got := testutil.ToFloat64(classificationMetrics.ClassificationsTotal.WithLabelValues(
		telemetry.ClassificationClassExternalDependencyIncident,
	)); got != 2 {
		t.Fatalf("classification metric = %v, want 2", got)
	}
}

func TestRunner_IncidentStoreFailureDoesNotFailStageHandling(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{errFor: map[string]error{
		"ci_watch": errors.New("gitlab pipeline status 503: service unavailable"),
	}}
	writer := &failingIncidentWriter{}
	r := New(st, newPassingGates(t), disp, nil)
	r.Stages = []Stage{{ID: "ci_watch", Type: "shell"}}
	r.IncidentWriter = writer

	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive failed because incident persistence failed: %v", err)
	}
	if writer.calls != 1 {
		t.Fatalf("incident writes = %d, want 1", writer.calls)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("run state = %s, want escalated", got.State)
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
	beforeMetric := testutil.ToFloat64(mills.AutoRequeuesTotal.WithLabelValues(string(ClassTransient)))
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
	if got := testutil.ToFloat64(mills.AutoRequeuesTotal.WithLabelValues(string(ClassTransient))) - beforeMetric; got != 1 {
		t.Errorf("auto_requeues KPI delta = %v, want 1", got)
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
	gotRun, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if gotRun.RetryExhausted == nil || !*gotRun.RetryExhausted {
		t.Fatalf("retry_exhausted = %v, want true", gotRun.RetryExhausted)
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

func TestIsTransientEscalationClass(t *testing.T) {
	cases := []struct {
		in   ErrorClass
		want bool
	}{
		{ClassTransient, true},
		{ClassTransientQuota, true},
		{ClassCode, false},
		{ClassInfra, false},
		{ClassConfig, false},
		{ErrorClass(""), false},
		{ErrorClass("wat"), false},
	}
	for _, tc := range cases {
		if got := isTransientEscalationClass(tc.in); got != tc.want {
			t.Errorf("isTransientEscalationClass(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The gate reads the declared class, so prose can no longer steer it in either
// direction. These are the two divergences the old substring matcher had.
func TestIsTransientEscalationClass_IgnoresReasonProse(t *testing.T) {
	// A ClassCode gate exhaustion whose failDetail quotes a nested transient
	// marker must NOT auto-retry against its own verdict.
	if isTransientEscalationClass(ClassCode) {
		t.Error("ClassCode must not be retryable even when the reason embeds [class=transient]")
	}
	// A genuinely transient escalation whose reason carries no marker at all
	// (adapter.go's \"integrator drive failed: …\") MUST auto-retry.
	if !isTransientEscalationClass(ClassTransient) {
		t.Error("ClassTransient must auto-retry even when the reason has no [class=…] marker")
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

// scriptedGate returns a fixed sequence of outcomes, clamping to the last
// entry once the script is exhausted. Used to model a gate whose verdict
// changes across retry rounds (e.g. scope fails on attempt 1, then the
// do-nothing retry fails nonempty_diff instead).
type scriptedGate struct {
	name     string
	outcomes []gates.Outcome
	calls    int
}

func (g *scriptedGate) Name() string { return g.name }
func (g *scriptedGate) Evaluate(_ context.Context, _ gates.StageInput) (gates.Outcome, error) {
	i := g.calls
	if i >= len(g.outcomes) {
		i = len(g.outcomes) - 1
	}
	g.calls++
	return g.outcomes[i], nil
}

// reasonCapturingEscalator records every escalation reason the runner emits.
type reasonCapturingEscalator struct {
	mu      sync.Mutex
	reasons []string
}

func (e *reasonCapturingEscalator) Handle(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reasons = append(e.reasons, reason)
	return nil
}

// retryCtxCapturingDispatcher records the StageRetryContext (or nil) the
// runner threads onto each implement dispatch.
type retryCtxCapturingDispatcher struct {
	mu      sync.Mutex
	implRCs []*StageRetryContext
}

func (d *retryCtxCapturingDispatcher) Dispatch(ctx context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, _ map[string]StageOutput) (StageOutput, error) {
	if stage.ID == "implement" {
		d.mu.Lock()
		d.implRCs = append(d.implRCs, StageRetryContextFromContext(ctx))
		d.mu.Unlock()
	}
	return StageOutput{
		CostUSD:        0.01,
		FilesChanged:   []string{"foo.go"},
		LinesAdded:     3,
		DiffPatch:      []byte("diff --git a/foo.go b/foo.go\n+x\n"),
		CommitMessages: []string{"feat: x"},
	}, nil
}

func (d *retryCtxCapturingDispatcher) captured() []*StageRetryContext {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*StageRetryContext, len(d.implRCs))
	copy(out, d.implRCs)
	return out
}

// Regression for the 2026-07-01 plan-linked retry wedge
// (PIPE-pattern-stamp-go-rest-service-{widget,gadget}-…): attempt 1 fails a
// real gate (scope), the fresh-clone retry does no work (the plan slice
// already looked implemented) and fails nonempty_diff, and the run escalates
// after burning all attempts — with an escalation reason that only named the
// gate stage, masking the ORIGINAL failure. The reason must now carry the
// FIRST failing gate + reasons alongside the latest knock-on failure.
func TestRunner_GateFailEscalationCarriesFirstFailingGate(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{}
	gr := gates.NewRegistry()
	pass := gates.Outcome{Pass: true, JudgedBy: "go"}
	gr.Register(&scriptedGate{name: "scope", outcomes: []gates.Outcome{
		{Pass: false, Reasons: []string{"file docs/x.md outside slice scope"}, JudgedBy: "go"},
		pass,
	}})
	gr.Register(&scriptedGate{name: "nonempty_diff", outcomes: []gates.Outcome{
		pass,
		{Pass: false, Reasons: []string{"no files changed by implement stage"}, JudgedBy: "go"},
	}})
	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %s, want escalated", got.State)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	reason := esc.reasons[0]
	// The latest failure (the retry's empty diff) is present...
	if !strings.Contains(reason, "nonempty_diff: no files changed by implement stage") {
		t.Errorf("reason missing latest failure detail: %q", reason)
	}
	// ...but the FIRST failing gate — the original defect — must be too.
	if !strings.Contains(reason, "first failure (gate post_implement_gate): scope: file docs/x.md outside slice scope") {
		t.Errorf("reason missing first failing gate: %q", reason)
	}
}

// A gate-fail rewind must hand the re-dispatched stage a StageRetryContext
// naming the failed gate, so the fresh spawn's prompt can instruct it to
// redo the discarded work instead of trusting the (stale) plan-slice status.
func TestRunner_GateFailRetryThreadsRetryContextToDispatcher(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &retryCtxCapturingDispatcher{}
	gr := gates.NewRegistry()
	gr.Register(&scriptedGate{name: "diff_size", outcomes: []gates.Outcome{
		{Pass: false, Reasons: []string{"diff too large: 900 lines"}, JudgedBy: "go"},
		{Pass: true, JudgedBy: "go"},
	}})
	r := New(st, gr, disp, nil)
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Fatalf("state = %s, want done", got.State)
	}
	rcs := disp.captured()
	if len(rcs) != 2 {
		t.Fatalf("implement dispatches = %d, want 2", len(rcs))
	}
	if rcs[0] != nil {
		t.Errorf("attempt 1 should carry no retry context, got %+v", rcs[0])
	}
	rc := rcs[1]
	if rc == nil {
		t.Fatal("attempt 2 missing retry context")
	}
	if rc.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", rc.Attempt)
	}
	if rc.GateStage != "post_implement_gate" {
		t.Errorf("GateStage = %q, want post_implement_gate", rc.GateStage)
	}
	if !strings.Contains(rc.FirstFailure, "diff_size: diff too large: 900 lines") {
		t.Errorf("FirstFailure = %q, want diff_size detail", rc.FirstFailure)
	}
}

// TestGateRetryFreshSpawn proves the runner and canonical dispatcher compose:
// a gate rewind issues a second spawn create with a fresh attempt key.
func TestGateRetryFreshSpawn(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	spawn := &fakeSpawn{resp: SpawnResponse{
		SpawnID: "spawn-retry", CostUSD: 0.01,
		FilesChanged: []string{"pkg/retry.go"}, LinesAdded: 1,
		DiffPatch: []byte("diff --git a/pkg/retry.go b/pkg/retry.go\n+retry\n"),
	}}
	worker := &SpawnWorker{Client: spawn, PromptFor: func(JobContext) string { return "retry" }}
	disp := NewDispatcher(map[string]Worker{"implement": worker}, &NoOpWorker{})
	gr := gates.NewRegistry()
	gr.Register(&scriptedGate{name: "diff_size", outcomes: []gates.Outcome{
		{Pass: false, Reasons: []string{"diff too large"}, JudgedBy: "go"},
		{Pass: true, JudgedBy: "go"},
	}})
	if err := New(st, gr, disp, nil).Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(spawn.calls) != 2 {
		t.Fatalf("implement spawn creates = %d, want two", len(spawn.calls))
	}
	keys := []string{spawn.calls[0].IdempotencyKey, spawn.calls[1].IdempotencyKey}
	if keys[0] == keys[1] {
		t.Fatalf("retry reused spawn key %q", keys[0])
	}
	if !strings.HasSuffix(keys[0], ":1") || !strings.HasSuffix(keys[1], ":2") {
		t.Fatalf("spawn keys = %v, want attempt suffixes :1 and :2", keys)
	}
}

// An operator restart between a gate failure and the retry's dispatch must
// not lose the retry context: Drive reseeds it from the persisted
// gate_outcomes, so the resumed retry spawn is still told it is a retry.
func TestRunner_ResumeRehydratesRetryContextFromGateOutcomes(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	ctx := context.Background()
	// Attempt 1's implement succeeded as a stage...
	started := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	oc := store.StageOutcomeSuccess
	if err := st.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "implement",
		Attempt:       1,
		StartedAt:     started,
		EndedAt:       &ended,
		Outcome:       &oc,
	}); err != nil {
		t.Fatalf("seed stage: %v", err)
	}
	// ...but the post_implement scope gate failed, and the operator died
	// before the retry dispatched.
	if err := st.Pipeline.PutGate(ctx, &store.GateOutcome{
		PipelineRunID: run.ID,
		AfterStage:    "post_implement_gate",
		GateName:      "scope",
		Outcome:       store.GateOutcomeFail,
		Reasons:       []string{"file docs/x.md outside slice scope"},
		JudgedBy:      "go",
		EvaluatedAt:   ended,
	}); err != nil {
		t.Fatalf("seed gate: %v", err)
	}
	run.CurrentStage = "implement"
	run.State = store.PipelineImplementing
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("persist run: %v", err)
	}

	disp := &retryCtxCapturingDispatcher{}
	r := New(st, newPassingGates(t), disp, nil)
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	rcs := disp.captured()
	if len(rcs) == 0 {
		t.Fatal("implement never dispatched on resume")
	}
	rc := rcs[0]
	if rc == nil {
		t.Fatal("resumed retry dispatch missing rehydrated retry context")
	}
	if rc.GateStage != "post_implement_gate" {
		t.Errorf("GateStage = %q, want post_implement_gate", rc.GateStage)
	}
	if !strings.Contains(rc.FirstFailure, "scope: file docs/x.md outside slice scope") {
		t.Errorf("FirstFailure = %q, want persisted scope failure", rc.FirstFailure)
	}
	if rc.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2 (seeded from persisted attempt 1)", rc.Attempt)
	}
}

// attemptScriptedDispatcher returns scripted per-attempt outputs for one
// stage (clamping to the last entry once exhausted, default output for every
// other stage) and snapshots the prior map handed to a watched stage so tests
// can assert what downstream stages actually see after a retry.
type attemptScriptedDispatcher struct {
	mu           sync.Mutex
	stageID      string
	outputs      []StageOutput
	calls        int
	watchStage   string
	watchedPrior map[string]StageOutput
}

func (d *attemptScriptedDispatcher) Dispatch(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem, stage Stage, prior map[string]StageOutput) (StageOutput, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if stage.ID == d.watchStage && d.watchedPrior == nil {
		snap := make(map[string]StageOutput, len(prior))
		for k, v := range prior {
			snap[k] = v
		}
		d.watchedPrior = snap
	}
	if stage.ID == d.stageID {
		i := d.calls
		d.calls++
		if i >= len(d.outputs) {
			i = len(d.outputs) - 1
		}
		return d.outputs[i], nil
	}
	return StageOutput{CostUSD: 0.01}, nil
}

// Regression for the implement-retry empty-diff wedge (escalations
// #218/#221–#224/#226/#228/#231/#232, 2026-06-30 → 2026-07-01): attempt 1
// pushes real work, a gate fails, and the fresh-clone retry finds the branch
// already up to date — so it reports an empty per-attempt diff ("Everything
// up-to-date"). The runner must carry the prior attempt's recorded work
// forward for gating instead of letting nonempty_diff escalate a run whose
// finished work is sitting on the branch.
func TestRunner_RetryEmptyDiffCarriesForwardPriorWork(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	patch := []byte("diff --git a/pkg/x/x.go b/pkg/x/x.go\n+real work\n")
	disp := &attemptScriptedDispatcher{
		stageID: "implement",
		outputs: []StageOutput{
			{CostUSD: 0.5, FilesChanged: []string{"pkg/x/x.go"}, LinesAdded: 10, DiffPatch: patch, CommitMessages: []string{"feat: x"}},
			{CostUSD: 0.1}, // do-nothing retry: the branch already has the work
		},
		watchStage: "mr",
	}
	gr := gates.NewRegistry()
	gr.Register(&gates.NonEmptyDiff{})
	// scope fails on the real attempt-1 output and passes on the retry
	// round — the shape of the transient scope failures in the live
	// escalations (no-slices hydration gap, since fixed).
	gr.Register(&scriptedGate{name: "scope", outcomes: []gates.Outcome{
		{Pass: false, Reasons: []string{"file docs/x.md outside slice scope"}, JudgedBy: "go"},
		{Pass: true, JudgedBy: "go"},
	}})
	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineDone {
		t.Fatalf("state = %s, want done (escalations: %v)", got.State, esc.reasons)
	}
	if disp.calls != 2 {
		t.Errorf("implement dispatched %d times, want 2 (one real attempt + one do-nothing retry)", disp.calls)
	}
	impl, ok := disp.watchedPrior["implement"]
	if !ok {
		t.Fatal("mr stage saw no implement output in prior")
	}
	if string(impl.DiffPatch) != string(patch) {
		t.Errorf("mr stage saw DiffPatch %q, want attempt-1 patch carried forward", impl.DiffPatch)
	}
	if len(impl.FilesChanged) != 1 || impl.FilesChanged[0] != "pkg/x/x.go" {
		t.Errorf("mr stage saw FilesChanged %v, want [pkg/x/x.go] carried forward", impl.FilesChanged)
	}
	if len(impl.CommitMessages) != 1 || impl.CommitMessages[0] != "feat: x" {
		t.Errorf("mr stage saw CommitMessages %v, want [feat: x] carried forward", impl.CommitMessages)
	}
}

// A run where the agent does zero work on EVERY attempt must still escalate —
// the carry-forward only bridges retries over previously recorded work; it
// must not weaken the empty-MR guard nonempty_diff exists for.
func TestRunner_AllAttemptsEmptyStillEscalates(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &attemptScriptedDispatcher{
		stageID: "implement",
		outputs: []StageOutput{{CostUSD: 0.01}}, // empty on every attempt
	}
	gr := gates.NewRegistry()
	gr.Register(&gates.NonEmptyDiff{})
	esc := &reasonCapturingEscalator{}
	r := New(st, gr, disp, nil)
	r.Escalator = esc
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	got, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("getrun: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("state = %s, want escalated (no attempt ever produced work)", got.State)
	}
}

// MR provenance and the full CI authorization tuple must survive the SQL JSON
// round-trip and a new Runner. A mutable backlog reroute after restart must not
// move merge to a different project.
func TestLoadPriorOutputs_PersistedCIIdentityRoutesMergeAfterRestart(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	ctx := context.Background()
	success := store.StageOutcomeSuccess
	mrIID := int64(215)
	if err := st.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "mr",
		Attempt:       1,
		StartedAt:     time.Date(2026, 7, 23, 11, 59, 0, 0, time.UTC),
		Outcome:       &success,
		Artifacts: mergeArtifacts("mr", StageOutput{MRIID: mrIID, Artifacts: map[string]any{
			"mr_project":       testCIProject,
			"mr_source_branch": testCISource,
			"mr_target_branch": testCITarget,
		}}),
	}); err != nil {
		t.Fatalf("persist mr provenance: %v", err)
	}
	if err := st.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "ci_watch",
		Attempt:       1,
		StartedAt:     time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		Outcome:       &success,
		Artifacts:     mergeArtifacts("ci_watch", StageOutput{Artifacts: testCIArtifacts("persisted-tested-head")}),
	}); err != nil {
		t.Fatalf("persist ci_watch: %v", err)
	}

	restarted := New(st, nil, &fakeDispatcher{}, nil)
	prior, err := restarted.loadPriorOutputs(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload prior outputs: %v", err)
	}
	pollReq, err := mrPollRequestFrom(JobContext{Prior: prior}, mrIID)
	if err != nil {
		t.Fatalf("restore MR provenance: %v", err)
	}
	if pollReq.Project != testCIProject || pollReq.SourceBranch != testCISource || pollReq.TargetBranch != testCITarget {
		t.Fatalf("restored MR provenance = %q:%q→%q", pollReq.Project, pollReq.SourceBranch, pollReq.TargetBranch)
	}

	run.MRIID = &mrIID
	item.TargetProject = "services/loom-flightdeck" // mutated after CI
	home := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "persisted-identity-merge"}}
	target := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "wrong-project-merge"}}
	w := &GitLabWorker{
		Client: home,
		ForProject: func(project string) GitLabClient {
			switch project {
			case testCIProject:
				return home
			case "services/loom-flightdeck":
				return target
			default:
				return nil
			}
		},
	}
	out, err := w.Run(ctx, JobContext{Run: run, Item: item, Stage: Stage{ID: "merge"}, Prior: prior})
	if err != nil || out.MergedSHA != "persisted-identity-merge" {
		t.Fatalf("merge from restored identity = %+v, %v", out, err)
	}
	if len(home.mergeCalls) != 1 || len(target.mergeCalls) != 0 {
		t.Fatalf("home/target merge calls = %d/%d, want 1/0", len(home.mergeCalls), len(target.mergeCalls))
	}
	got := home.mergeCalls[0]
	if got.Project != testCIProject || got.SourceBranch != testCISource || got.TargetBranch != testCITarget || got.ExpectedSHA != "persisted-tested-head" {
		t.Fatalf("restored merge authorization = %+v", got)
	}
}

func TestMergeRecoveryPipelineCreateFenceSurvivesRestart(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	dispatcher := &mergeRecoveryFenceDispatcher{}
	stage := Stage{ID: "merge", Type: "shell", State: store.PipelineMerging}
	first := New(st, nil, dispatcher, nil)
	if _, err := first.runStage(context.Background(), run, item, stage, nil, 1, nil); err == nil {
		t.Fatal("first merge stage should simulate interruption")
	}

	// A new Runner/process must recover the fence from SQLite even though the
	// first attempt ended in error and never produced a successful output.
	restarted := New(st, nil, dispatcher, nil)
	if _, err := restarted.runStage(context.Background(), run, item, stage, nil, 2, nil); err != nil {
		t.Fatalf("restarted merge stage: %v", err)
	}
	if len(dispatcher.attempted) != 2 || dispatcher.attempted[0] || !dispatcher.attempted[1] {
		t.Fatalf("fence observations = %v, want [false true]", dispatcher.attempted)
	}
	rows, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	fenced := 0
	for _, row := range rows {
		if row.Stage == "merge" {
			if attempted, _ := row.Artifacts[mergeRecoveryPipelineCreateAttemptedArtifact].(bool); attempted {
				fenced++
			}
		}
	}
	if fenced != 2 {
		t.Fatalf("durably fenced merge attempts = %d, want 2", fenced)
	}
}

// Resume-safety: loadPriorOutputs walks stage_results oldest-first and later
// rows overwrite earlier ones — so a persisted do-nothing retry row (empty
// artifacts) must not clobber the attempt-1 row's recorded diff, or a Drive
// resumed after an operator restart re-enters the same wedge the in-memory
// carry-forward fixes.
func TestLoadPriorOutputs_EmptyRetryRowKeepsPriorDiff(t *testing.T) {
	st, run, _ := newRunnerEnv(t)
	ctx := context.Background()
	success := store.StageOutcomeSuccess
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := st.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "implement",
		Attempt:       1,
		StartedAt:     now,
		Outcome:       &success,
		Artifacts: map[string]any{
			"files_changed": []any{"pkg/x/x.go"},
			"diff_patch":    "diff --git a/pkg/x/x.go b/pkg/x/x.go\n+real work\n",
			"lines_added":   float64(10),
		},
	}); err != nil {
		t.Fatalf("seed attempt 1: %v", err)
	}
	if err := st.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         "implement",
		Attempt:       2,
		StartedAt:     now.Add(time.Minute),
		Outcome:       &success,
	}); err != nil {
		t.Fatalf("seed attempt 2: %v", err)
	}
	r := New(st, nil, &fakeDispatcher{}, nil)
	prior, err := r.loadPriorOutputs(ctx, run.ID)
	if err != nil {
		t.Fatalf("loadPriorOutputs: %v", err)
	}
	impl, ok := prior["implement"]
	if !ok {
		t.Fatal("no implement output rehydrated")
	}
	if len(impl.FilesChanged) != 1 || impl.FilesChanged[0] != "pkg/x/x.go" {
		t.Errorf("FilesChanged = %v, want [pkg/x/x.go] preserved from attempt 1", impl.FilesChanged)
	}
	if !strings.Contains(string(impl.DiffPatch), "+real work") {
		t.Errorf("DiffPatch = %q, want attempt-1 diff preserved", impl.DiffPatch)
	}
	if impl.LinesAdded != 10 {
		t.Errorf("LinesAdded = %d, want 10 preserved from attempt 1", impl.LinesAdded)
	}
}

// carryForwardDiff unit matrix: carry only when the retry reports no work
// and the previous attempt reported some.
func TestCarryForwardDiff(t *testing.T) {
	work := StageOutput{
		FilesChanged:   []string{"a.go"},
		LinesAdded:     3,
		LinesRemoved:   1,
		DiffPatch:      []byte("diff"),
		CommitMessages: []string{"feat: a"},
	}
	empty := StageOutput{CostUSD: 0.1, SpawnID: "spawn-2"}

	got, carried := carryForwardDiff(work, empty)
	if !carried {
		t.Fatal("expected carry when retry is empty and prev has work")
	}
	if got.SpawnID != "spawn-2" || got.CostUSD != 0.1 {
		t.Errorf("retry bookkeeping fields must be kept: %+v", got)
	}
	if len(got.FilesChanged) != 1 || string(got.DiffPatch) != "diff" || got.LinesAdded != 3 || got.LinesRemoved != 1 {
		t.Errorf("diff evidence not carried: %+v", got)
	}
	if len(got.CommitMessages) != 1 || got.CommitMessages[0] != "feat: a" {
		t.Errorf("commit messages not carried: %+v", got)
	}

	if _, carried := carryForwardDiff(empty, empty); carried {
		t.Error("no carry when prev has no work")
	}
	if got, carried := carryForwardDiff(work, work); carried || len(got.FilesChanged) != 1 {
		t.Error("no carry when retry has its own work")
	}
	// A diff-only prev (telemetry parsed no paths) still counts as work.
	diffOnly := StageOutput{DiffPatch: []byte("diff")}
	if _, carried := carryForwardDiff(diffOnly, empty); !carried {
		t.Error("expected carry from diff-only prev")
	}
}

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// ----- S3: ci_watch watch-extension resilience -----

// pollStep is one scripted PollPipeline result for sequencedGitLab.
type pollStep struct {
	resp PollPipelineResponse
	err  error
}

// sequencedGitLab is a GitLabClient whose PollPipeline returns scripted results
// in order. Once the script is exhausted it repeats the final step, so a
// persistent-timeout scenario can be written as a single step.
type sequencedGitLab struct {
	steps     []pollStep
	pollCalls int
	retried   []int64
	retryErr  error
	deadline  time.Duration
	deadlines []time.Time
}

func (f *sequencedGitLab) RetryJob(_ context.Context, id int64) error {
	f.retried = append(f.retried, id)
	return f.retryErr
}

func (f *sequencedGitLab) PipelinePollDeadline() time.Duration { return f.deadline }

func (f *sequencedGitLab) CreateMR(context.Context, CreateMRRequest) (CreateMRResponse, error) {
	return CreateMRResponse{}, nil
}

func (f *sequencedGitLab) PollPipeline(ctx context.Context, _ PollPipelineRequest) (PollPipelineResponse, error) {
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	i := f.pollCalls
	f.pollCalls++
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	return f.steps[i].resp, f.steps[i].err
}

func (f *sequencedGitLab) Merge(context.Context, MergeRequestArgs) (MergeResponse, error) {
	return MergeResponse{}, nil
}

func (f *sequencedGitLab) Cleanup(context.Context, CleanupRequest) (CleanupResponse, error) {
	return CleanupResponse{}, nil
}

// ciWatchTimeoutStep builds a poll session that timed out with the pipeline
// still running — the shape clients.GitLabClient.PollPipeline returns when the
// per-session deadline fires before a terminal state (wraps ErrPipelinePollTimeout).
func ciWatchTimeoutStep(url string, cost float64) pollStep {
	return pollStep{
		resp: PollPipelineResponse{
			Status:      "timeout",
			CostUSD:     cost,
			PipelineURL: url,
			LastStatus:  "running",
			LogTail:     "[t] pipeline 1 (push) status=running " + url + "\n",
		},
		err: fmt.Errorf("gitlab: pipeline poll timed out after 30m0s (pipeline: %s): %w", url, ErrPipelinePollTimeout),
	}
}

func ciWatchJobContext(t *testing.T, maxMinutes string) JobContext {
	t.Helper()
	jc := sampleJobContext("ci_watch")
	iid := int64(42)
	addMRProvenance(&jc, iid, testCIProject, testCISource, testCITarget)
	if maxMinutes != "" {
		jc.Env["MILLS_CI_WATCH_MAX_MINUTES"] = maxMinutes
	}
	return jc
}

func TestCIWatchFlakeEligibility(t *testing.T) {
	eligible := []string{"test:reliability", "test:unit"}
	for _, tt := range []struct {
		name string
		jobs []FailedJob
		want bool
	}{
		{"eligible", []FailedJob{{ID: 1, Name: "test:reliability", FailureReason: "script_failure"}}, true},
		{"mixed", []FailedJob{{ID: 1, Name: "test:unit", FailureReason: "script_failure"}, {ID: 2, Name: "lint", FailureReason: "script_failure"}}, false},
		{"runner", []FailedJob{{ID: 1, Name: "test:unit", FailureReason: "runner_system_failure"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := flakyRetryEligible(tt.jobs, eligible); got != tt.want {
				t.Fatalf("eligible=%v want %v", got, tt.want)
			}
		})
	}
}

func TestGitLabWorker_CIWatchRetriesEligibleFailureThenGreen(t *testing.T) {
	failed := testCIPollResponse("failed", "head")
	failed.FailedJobReasons = []string{"script_failure"}
	failed.FailedJobs = []FailedJob{{ID: 17, Name: "test:reliability", FailureReason: "script_failure"}}
	green := testCIPollResponse("success", "head")
	gl := &sequencedGitLab{steps: []pollStep{{resp: failed}, {resp: green}}, deadline: time.Minute}
	w := &GitLabWorker{Client: gl, FlakyJobs: func() []string { return []string{"test:reliability"} }}
	ctx := withCIWatchFlakeRescueRecorder(context.Background(), func([]FailedJob) error { return nil })
	if _, err := w.Run(ctx, ciWatchJobContext(t, "")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(gl.retried) != 1 || gl.retried[0] != 17 {
		t.Fatalf("retried=%v, want [17]", gl.retried)
	}
	if gl.pollCalls != 2 {
		t.Fatalf("poll calls=%d, want 2", gl.pollCalls)
	}
	if gl.deadlines[0].IsZero() || !gl.deadlines[0].Equal(gl.deadlines[1]) {
		t.Fatalf("rescue poll deadlines = %v, want same non-zero absolute deadline", gl.deadlines)
	}
}

func TestGitLabWorker_CIWatchRescueFailureEscalatesWithBothObservations(t *testing.T) {
	first := testCIPollResponse("failed", "head")
	first.FailedJobReasons = []string{"script_failure"}
	first.FailedJobs = []FailedJob{{ID: 17, Name: "test:reliability", FailureReason: "script_failure"}}
	second := testCIPollResponse("failed", "head")
	second.FailedJobReasons = []string{"script_failure"}
	second.FailedJobs = []FailedJob{{ID: 18, Name: "test:unit", FailureReason: "script_failure"}}
	gl := &sequencedGitLab{steps: []pollStep{{resp: first}, {resp: second}}, deadline: time.Minute}
	w := &GitLabWorker{Client: gl, FlakyJobs: func() []string { return []string{"test:reliability", "test:unit"} }}
	ctx := withCIWatchFlakeRescueRecorder(context.Background(), func([]FailedJob) error { return nil })
	_, err := w.Run(ctx, ciWatchJobContext(t, ""))
	if err == nil || !strings.Contains(err.Error(), "test:reliability failed, auto-retried once, failed again (test:unit)") {
		t.Fatalf("error = %v, want both failure observations", err)
	}
	if got := len(gl.retried); got != 1 {
		t.Fatalf("retry calls = %d, want 1", got)
	}
}

func TestGitLabWorker_CIWatchIneligibleAndRetryAPIFailure(t *testing.T) {
	failed := testCIPollResponse("failed", "head")
	failed.FailedJobReasons = []string{"script_failure"}
	failed.FailedJobs = []FailedJob{{ID: 17, Name: "lint", FailureReason: "script_failure"}}
	gl := &sequencedGitLab{steps: []pollStep{{resp: failed}}, deadline: time.Minute}
	w := &GitLabWorker{Client: gl, FlakyJobs: func() []string { return []string{"test:unit"} }}
	if _, err := w.Run(context.Background(), ciWatchJobContext(t, "")); !errors.Is(err, ErrCIPipelineTerminal) {
		t.Fatalf("ineligible error = %v, want terminal", err)
	}
	if len(gl.retried) != 0 {
		t.Fatalf("ineligible retry calls = %v, want none", gl.retried)
	}

	failed.FailedJobs[0].Name = "test:unit"
	gl = &sequencedGitLab{steps: []pollStep{{resp: failed}}, retryErr: errors.New("retry denied"), deadline: time.Minute}
	w.Client = gl
	ctx := withCIWatchFlakeRescueRecorder(context.Background(), func([]FailedJob) error { return nil })
	if _, err := w.Run(ctx, ciWatchJobContext(t, "")); err == nil || !strings.Contains(err.Error(), "retry denied") {
		t.Fatalf("retry API error = %v", err)
	}
}

// TestGitLabWorker_CIWatch_ExtendsThenSucceeds pins the headline S3 behavior: a
// slow-but-healthy pipeline that stays running past the first 30m poll deadline
// must not kill the run — runCI extends the watch and succeeds when the pipeline
// finally goes green during a later extension.
func TestGitLabWorker_CIWatch_ExtendsThenSucceeds(t *testing.T) {
	const url = "https://gitlab.example/services/loom-core/-/pipelines/4242"
	gl := &sequencedGitLab{steps: []pollStep{
		ciWatchTimeoutStep(url, 0.01),
		ciWatchTimeoutStep(url, 0.01),
		{resp: func() PollPipelineResponse {
			resp := testCIPollResponse("success", "tested-head")
			resp.CostUSD = 0.02
			resp.PipelineURL = url
			return resp
		}()},
	}}
	w := &GitLabWorker{Client: gl}

	out, err := w.Run(context.Background(), ciWatchJobContext(t, "90")) // 90m/30m → 2 extensions
	if err != nil {
		t.Fatalf("expected success after extensions, got %v", err)
	}
	if out.Artifacts["ci_status"] != "success" {
		t.Errorf("ci_status = %v, want success", out.Artifacts["ci_status"])
	}
	if gl.pollCalls != 3 {
		t.Errorf("poll calls = %d, want 3 (base watch + 2 extensions)", gl.pollCalls)
	}
	if got, want := out.CostUSD, 0.04; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("cost = %v, want %v (accumulated across sessions)", got, want)
	}
	for _, note := range []string{"extending watch (1/2)", "extending watch (2/2)"} {
		if !strings.Contains(out.LogTail, note) {
			t.Errorf("log tail missing %q so the HUD drawer can't show the extension:\n%s", note, out.LogTail)
		}
	}
}

// TestGitLabWorker_CIWatch_StallsAtHardCap pins the cap path: a pipeline that
// stays running past the hard cap yields a CIWatchStalledError carrying the
// stuck pipeline URL, unwrapping to ErrPipelinePollTimeout (ClassInfra), rather
// than a generic stage error.
func TestGitLabWorker_CIWatch_StallsAtHardCap(t *testing.T) {
	const url = "https://gitlab.example/services/loom-core/-/pipelines/4242"
	gl := &sequencedGitLab{steps: []pollStep{ciWatchTimeoutStep(url, 0.01)}} // always timeout
	w := &GitLabWorker{Client: gl}

	out, err := w.Run(context.Background(), ciWatchJobContext(t, "90")) // 2 extensions → 3 sessions
	if err == nil {
		t.Fatal("expected a stall error at the hard cap")
	}
	var stall *CIWatchStalledError
	if !errors.As(err, &stall) {
		t.Fatalf("error must be a *CIWatchStalledError; got %v", err)
	}
	if stall.PipelineURL != url {
		t.Errorf("stall PipelineURL = %q, want %q", stall.PipelineURL, url)
	}
	if stall.MaxMinutes != 90 {
		t.Errorf("stall MaxMinutes = %d, want 90", stall.MaxMinutes)
	}
	if !errors.Is(err, ErrPipelinePollTimeout) {
		t.Errorf("stall must unwrap to ErrPipelinePollTimeout so Classify tags it infra; got %v", err)
	}
	if Classify(err) != ClassInfra {
		t.Errorf("Classify(stall) = %s, want infra", Classify(err))
	}
	if gl.pollCalls != 3 {
		t.Errorf("poll calls = %d, want 3 (base watch + 2 extensions before cap)", gl.pollCalls)
	}
	if !strings.Contains(out.LogTail, "extending watch (2/2)") {
		t.Errorf("log tail should record every extension before the cap:\n%s", out.LogTail)
	}
}

// TestGitLabWorker_CIWatch_NormalFailureUnchanged guards that the extension loop
// does not alter the pre-S3 behavior for a pipeline that reaches a terminal
// non-success state within the first window: it still wraps ErrCIPipelineTerminal
// on the first poll (no extra watching).
func TestGitLabWorker_CIWatch_NormalFailureUnchanged(t *testing.T) {
	gl := &sequencedGitLab{steps: []pollStep{
		{resp: func() PollPipelineResponse {
			resp := testCIPollResponse("failed", "failed-head")
			resp.LogTail = "job x failed"
			return resp
		}()},
	}}
	w := &GitLabWorker{Client: gl}

	out, err := w.Run(context.Background(), ciWatchJobContext(t, "90"))
	if err == nil {
		t.Fatal("expected error for a failed pipeline")
	}
	if !errors.Is(err, ErrCIPipelineTerminal) {
		t.Fatalf("terminal failure must wrap ErrCIPipelineTerminal; got %v", err)
	}
	var stall *CIWatchStalledError
	if errors.As(err, &stall) {
		t.Fatal("a terminal failure must not be treated as a watch stall")
	}
	if gl.pollCalls != 1 {
		t.Errorf("poll calls = %d, want 1 (terminal state must not be re-watched)", gl.pollCalls)
	}
	if out.Artifacts["ci_status"] != "failed" {
		t.Errorf("ci_status = %v, want failed", out.Artifacts["ci_status"])
	}
}

// TestGitLabWorker_CIWatch_HeadSHAUnavailableIsNotExtended pins the 2026-07-26
// wedge: three ci_watch escalations whose whole LogTail was "MR 1239 head sha
// pending". A headless MR has no pipeline to wait for, so the watch-extension
// path (built for slow-but-real pipelines) must not apply — one poll, one
// terminal error, no 90m burn, and never reported as a pipeline stall.
func TestGitLabWorker_CIWatch_HeadSHAUnavailableIsNotExtended(t *testing.T) {
	gl := &sequencedGitLab{steps: []pollStep{{
		resp: PollPipelineResponse{LogTail: "[t] MR 42 head sha pending (5m0s, state=opened merge_status=cannot_be_merged)\n"},
		err: fmt.Errorf("gitlab: mr 42 reported no head sha after 5m0s (bound 5m0s): state=opened merge_status=cannot_be_merged has_conflicts=true: %w",
			ErrMRHeadSHAUnavailable),
	}}}
	w := &GitLabWorker{Client: gl}

	out, err := w.Run(context.Background(), ciWatchJobContext(t, "90"))
	if err == nil {
		t.Fatal("expected an error for an MR that never reports a head sha")
	}
	if !errors.Is(err, ErrMRHeadSHAUnavailable) {
		t.Fatalf("error must wrap ErrMRHeadSHAUnavailable; got %v", err)
	}
	if errors.Is(err, ErrPipelinePollTimeout) {
		t.Fatal("a headless MR must not masquerade as a pipeline poll timeout")
	}
	var stall *CIWatchStalledError
	if errors.As(err, &stall) {
		t.Fatal("a headless MR must not be reported as a running-pipeline stall")
	}
	if gl.pollCalls != 1 {
		t.Errorf("poll calls = %d, want 1 (no watch extensions for a headless MR)", gl.pollCalls)
	}
	if !strings.Contains(out.LogTail, "head sha pending") {
		t.Errorf("log tail should carry the pending history, got %q", out.LogTail)
	}
	if got := Classify(err); got != ClassConfig {
		t.Errorf("Classify = %q, want %q (terminal MR state, escalate without burning attempts)", got, ClassConfig)
	}
}

// TestGitLabWorker_CIWatch_BranchPipelineUnavailableIsNotExtended is the
// with-a-head twin of the headless case above: the MR has a head SHA but the
// project never builds a push pipeline for it. Extending the watch cannot
// conjure a pipeline that workflow rules exclude, so the stage must stop at the
// first verdict rather than replaying "branch pipeline pending" for 90 minutes
// and then reporting a stall on a pipeline that never existed.
func TestGitLabWorker_CIWatch_BranchPipelineUnavailableIsNotExtended(t *testing.T) {
	gl := &sequencedGitLab{steps: []pollStep{{
		resp: PollPipelineResponse{LogTail: "[t] MR 42 branch pipeline pending for cafef00d (10m0s)\n"},
		err: fmt.Errorf("gitlab: mr 42 head cafef00d has no push pipeline after 10m0s (bound 10m0s): state=opened merge_status=can_be_merged; pipelines for this sha: merge_request_event/success@feat/x: %w",
			ErrBranchPipelineUnavailable),
	}}}
	w := &GitLabWorker{Client: gl}

	out, err := w.Run(context.Background(), ciWatchJobContext(t, "90"))
	if err == nil {
		t.Fatal("expected an error for a head with no push pipeline")
	}
	if !errors.Is(err, ErrBranchPipelineUnavailable) {
		t.Fatalf("error must wrap ErrBranchPipelineUnavailable; got %v", err)
	}
	if errors.Is(err, ErrPipelinePollTimeout) {
		t.Fatal("a missing branch pipeline must not masquerade as a pipeline poll timeout")
	}
	var stall *CIWatchStalledError
	if errors.As(err, &stall) {
		t.Fatal("a pipeline that never existed must not be reported as a running-pipeline stall")
	}
	if gl.pollCalls != 1 {
		t.Errorf("poll calls = %d, want 1 (no watch extensions)", gl.pollCalls)
	}
	if !strings.Contains(out.LogTail, "branch pipeline pending") {
		t.Errorf("log tail should carry the pending history, got %q", out.LogTail)
	}
	if got := Classify(err); got != ClassConfig {
		t.Errorf("Classify = %q, want %q (project CI configuration, escalate without burning attempts)", got, ClassConfig)
	}
}

// TestGitLabWorker_CIWatch_ClosedMRSurfacesLogTail: a closed MR is a terminal
// config stop, and the poll history that preceded it must reach the stage
// output — non-timeout error returns used to arrive with an empty tail.
func TestGitLabWorker_CIWatch_ClosedMRSurfacesLogTail(t *testing.T) {
	gl := &sequencedGitLab{steps: []pollStep{{
		resp: PollPipelineResponse{
			LogTail:    "[t] pipeline 12 (push) status=running https://gitlab.example/p/-/pipelines/12\n[t] MR 42 is closed; abandoning ci watch\n",
			LastStatus: "running",
		},
		err: fmt.Errorf("gitlab: mr 42 is closed during ci watch: %w", ErrMergeRequestClosed),
	}}}
	w := &GitLabWorker{Client: gl}

	out, err := w.Run(context.Background(), ciWatchJobContext(t, "90"))
	if !errors.Is(err, ErrMergeRequestClosed) {
		t.Fatalf("error must wrap ErrMergeRequestClosed; got %v", err)
	}
	if gl.pollCalls != 1 {
		t.Errorf("poll calls = %d, want 1 (a closed MR must not be re-watched)", gl.pollCalls)
	}
	if !strings.Contains(out.LogTail, "abandoning ci watch") {
		t.Errorf("log tail should explain why the watch stopped, got %q", out.LogTail)
	}
	if got := Classify(err); got != ClassConfig {
		t.Errorf("Classify = %q, want %q", got, ClassConfig)
	}
}

func TestCIWatchExtensionBudget(t *testing.T) {
	cases := []struct {
		max  string
		want int
	}{
		{"", 2},   // default 90m / 30m → 3 sessions → 2 extensions
		{"90", 2}, // explicit default
		{"30", 0}, // single session, no extension
		{"60", 1},
		{"91", 3},  // ceil: partial session gets a full window
		{"0", 2},   // invalid → default
		{"abc", 2}, // unparseable → default
	}
	for _, tc := range cases {
		env := map[string]string{}
		if tc.max != "" {
			env["MILLS_CI_WATCH_MAX_MINUTES"] = tc.max
		}
		if got := ciWatchExtensionBudget(env); got != tc.want {
			t.Errorf("ciWatchExtensionBudget(max=%q) = %d, want %d", tc.max, got, tc.want)
		}
	}
}

// TestRunner_CIWatchStallEscalatesRetryableExternalDependency pins the runner
// side of S3: a ci_watch stall (pipeline still running at the hard cap)
// escalates ONCE — no attempt-budget burn — and is recorded as a RETRYABLE
// external-dependency incident keyed on the stuck pipeline URL, so a later
// requeue can recover it once CI drains.
// TestRunner_CIWatchHeadSHAUnavailableEscalatesOnFirstSight: the run must stop
// at the first headless-MR verdict with remediation the operator can act on,
// instead of re-watching it MaxAttempts times (each re-watch replays the same
// bounded wait and returns the identical answer).
func TestRunner_CIWatchHeadSHAUnavailableEscalatesOnFirstSight(t *testing.T) {
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
			"ci_watch": fmt.Errorf("gitlab: mr 42 reported no head sha after 5m0s (bound 5m0s): state=opened merge_status=cannot_be_merged has_conflicts=true: %w",
				ErrMRHeadSHAUnavailable),
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
		t.Errorf("ci_watch calls = %d, want 1 (a headless MR must not be re-watched)", ciCalls)
	}
	if got.EscalationClass != string(ClassConfig) {
		t.Errorf("escalation class = %q, want %q", got.EscalationClass, ClassConfig)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	if !strings.Contains(esc.reasons[0], "no head sha") ||
		!strings.Contains(esc.reasons[0], "rebase or repush") {
		t.Errorf("reason should name the headless MR and its remediation: %q", esc.reasons[0])
	}
}

// TestRunner_CIWatchBranchPipelineUnavailableEscalatesOnFirstSight: the run must
// stop at the first "no push pipeline" verdict with CI-configuration
// remediation, instead of re-watching it MaxAttempts times — each re-watch
// replays the same bounded wait and returns the identical answer.
func TestRunner_CIWatchBranchPipelineUnavailableEscalatesOnFirstSight(t *testing.T) {
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
			"ci_watch": fmt.Errorf("gitlab: mr 42 head cafef00d has no push pipeline after 10m0s (bound 10m0s): state=opened merge_status=can_be_merged; pipelines for this sha: merge_request_event/success@feat/x: %w",
				ErrBranchPipelineUnavailable),
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
		t.Errorf("ci_watch calls = %d, want 1 (a project that never builds pushes must not be re-watched)", ciCalls)
	}
	if got.EscalationClass != string(ClassConfig) {
		t.Errorf("escalation class = %q, want %q", got.EscalationClass, ClassConfig)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	if !strings.Contains(esc.reasons[0], "no push pipeline") ||
		!strings.Contains(esc.reasons[0], "workflow rules") {
		t.Errorf("reason should name the missing pipeline and its remediation: %q", esc.reasons[0])
	}
}

func TestRunner_CIWatchStallEscalatesRetryableExternalDependency(t *testing.T) {
	const url = "https://gitlab.example/services/loom-core/-/pipelines/4242"
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
			"ci_watch": &CIWatchStalledError{PipelineURL: url, MaxMinutes: 90, MRIID: 42, LastStatus: "running"},
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
		t.Errorf("ci_watch calls = %d, want 1 (a stall must not re-watch or burn attempts)", ciCalls)
	}
	if got.EscalationClass != string(ClassInfra) ||
		got.FailureClass != string(FailureInfrastructure) ||
		got.ExternalDependencyID != url ||
		got.ExternalDependency != ciWatchExternalDependency ||
		got.EscalationRetryable == nil ||
		!*got.EscalationRetryable {
		t.Fatalf("escalation metadata = class=%q failure=%q external_id=%q external=%q retryable=%v; want retryable external CI-dependency stall keyed on the pipeline URL",
			got.EscalationClass, got.FailureClass, got.ExternalDependencyID, got.ExternalDependency, got.EscalationRetryable)
	}
	if len(esc.reasons) != 1 {
		t.Fatalf("escalations = %d, want 1 (%v)", len(esc.reasons), esc.reasons)
	}
	if !strings.Contains(esc.reasons[0], "[class=infra]") || !strings.Contains(esc.reasons[0], "not retried") {
		t.Errorf("reason should mark the infra class and not-retried intent: %q", esc.reasons[0])
	}
}

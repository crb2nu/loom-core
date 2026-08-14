package pipeline

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// ----- Fakes -----

type fakeSpawn struct {
	calls    []SpawnRequest
	resp     SpawnResponse
	err      error
	gotEnvs  []map[string]string
	gotWdirs []string
}

func (f *fakeSpawn) Run(_ context.Context, req SpawnRequest) (SpawnResponse, error) {
	f.calls = append(f.calls, req)
	f.gotEnvs = append(f.gotEnvs, req.Env)
	f.gotWdirs = append(f.gotWdirs, req.WorkingDir)
	if f.err != nil {
		return SpawnResponse{}, f.err
	}
	return f.resp, nil
}

type fakeWeaver struct {
	calls []WeaverRequest
	resp  WeaverResponse
	err   error
}

func (f *fakeWeaver) Research(_ context.Context, req WeaverRequest) (WeaverResponse, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return WeaverResponse{}, f.err
	}
	return f.resp, nil
}

type fakeDevbox struct {
	calls []DevboxRequest
	resp  DevboxResponse
	err   error
}

func (f *fakeDevbox) QualityGate(_ context.Context, req DevboxRequest) (DevboxResponse, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return DevboxResponse{}, f.err
	}
	return f.resp, nil
}

type fakeGitLab struct {
	createCalls  []CreateMRRequest
	pollCalls    []PollPipelineRequest
	mergeCalls   []MergeRequestArgs
	cleanupCalls []CleanupRequest
	createResp   CreateMRResponse
	pollResp     PollPipelineResponse
	mergeResp    MergeResponse
	cleanupResp  CleanupResponse
}

func (f *fakeGitLab) CreateMR(_ context.Context, req CreateMRRequest) (CreateMRResponse, error) {
	f.createCalls = append(f.createCalls, req)
	return f.createResp, nil
}
func (f *fakeGitLab) PollPipeline(_ context.Context, req PollPipelineRequest) (PollPipelineResponse, error) {
	f.pollCalls = append(f.pollCalls, req)
	return f.pollResp, nil
}
func (f *fakeGitLab) Merge(_ context.Context, req MergeRequestArgs) (MergeResponse, error) {
	f.mergeCalls = append(f.mergeCalls, req)
	return f.mergeResp, nil
}
func (f *fakeGitLab) Cleanup(_ context.Context, req CleanupRequest) (CleanupResponse, error) {
	f.cleanupCalls = append(f.cleanupCalls, req)
	return f.cleanupResp, nil
}

func sampleJobContext(stageID string, opts ...func(*JobContext)) JobContext {
	run := &store.PipelineRun{
		ID:              "PIPE-X-1",
		BacklogID:       "BL-X",
		ParentSessionID: "claude-code-session-9",
		WorktreePath:    "/tmp/wt",
	}
	item := &store.BacklogItem{
		ID:    "BL-X",
		Title: "x",
		Budget: store.Budget{
			MaxCostUSD:         5,
			MaxTurns:           50,
			MaxPipelineMinutes: 30,
		},
	}
	stage := Stage{ID: stageID, Type: "agent_spawn"}
	jc := JobContext{
		Run:    run,
		Item:   item,
		Stage:  stage,
		Prior:  map[string]StageOutput{},
		Budget: item.Budget,
		Env:    BuildMillsEnv(run, item, stage),
	}
	for _, o := range opts {
		o(&jc)
	}
	return jc
}

const (
	testCIProject = "services/loom-core"
	testCISource  = "feat/BL-X"
	testCITarget  = "main"
)

func addMRProvenance(jc *JobContext, iid int64, project, source, target string) {
	jc.Run.MRIID = &iid
	jc.Prior["mr"] = StageOutput{
		MRIID: iid,
		Artifacts: map[string]any{
			"mr_project":       project,
			"mr_source_branch": source,
			"mr_target_branch": target,
		},
	}
}

func testCIPollResponse(status, sha string) PollPipelineResponse {
	return PollPipelineResponse{
		Status:       status,
		Project:      testCIProject,
		SourceBranch: testCISource,
		TargetBranch: testCITarget,
		SHA:          sha,
	}
}

func testCIArtifacts(sha string) map[string]any {
	return map[string]any{
		"ci_project":       testCIProject,
		"ci_source_branch": testCISource,
		"ci_target_branch": testCITarget,
		"ci_sha":           sha,
	}
}

func TestBuildMillsEnv_AlwaysIncludesIDs(t *testing.T) {
	jc := sampleJobContext("implement")
	env := jc.Env
	want := map[string]string{
		"LOOM_MILLS_RUN_ID":      "PIPE-X-1",
		"LOOM_MILLS_BACKLOG_ID":  "BL-X",
		"LOOM_MILLS_STAGE":       "implement",
		"LOOM_PARENT_SESSION_ID": "claude-code-session-9",
		"LOOM_MILLS_WORKTREE":    "/tmp/wt",
		"LOOM_MILLS_BRANCH":      "feat/BL-X",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
}

func TestBuildMillsEnv_OmitsOptionalWhenAbsent(t *testing.T) {
	jc := sampleJobContext("plan_slice", func(jc *JobContext) {
		jc.Run.ParentSessionID = ""
		jc.Run.WorktreePath = ""
		jc.Env = BuildMillsEnv(jc.Run, jc.Item, jc.Stage)
	})
	if _, ok := jc.Env["LOOM_PARENT_SESSION_ID"]; ok {
		t.Errorf("LOOM_PARENT_SESSION_ID should be omitted when empty")
	}
	if _, ok := jc.Env["LOOM_MILLS_WORKTREE"]; ok {
		t.Errorf("LOOM_MILLS_WORKTREE should be omitted when empty")
	}
}

func TestSpawnWorker_PropagatesBudgetEnvAndPrompt(t *testing.T) {
	sp := &fakeSpawn{resp: SpawnResponse{
		SpawnID:      "spawn-1",
		CostUSD:      0.42,
		FilesChanged: []string{"a.go"},
		LinesAdded:   3,
	}}
	w := &SpawnWorker{Client: sp, PromptFor: func(jc JobContext) string {
		return "plan slices for " + jc.Item.ID
	}}
	out, err := w.Run(context.Background(), sampleJobContext("plan_slice"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.SpawnID != "spawn-1" || out.CostUSD != 0.42 {
		t.Errorf("output passthrough wrong: %+v", out)
	}
	if len(sp.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(sp.calls))
	}
	got := sp.calls[0]
	if got.Prompt != "plan slices for BL-X" {
		t.Errorf("prompt = %q", got.Prompt)
	}
	if got.BudgetUSD != 5 || got.BudgetTurns != 50 || got.BudgetMinutes != 30 {
		t.Errorf("budget propagation wrong: %+v", got)
	}
	if got.WorkingDir != "/tmp/wt" {
		t.Errorf("workdir = %q", got.WorkingDir)
	}
	if got.ParentSessionID != "claude-code-session-9" {
		t.Errorf("parent session = %q", got.ParentSessionID)
	}
	if got.Env["LOOM_MILLS_RUN_ID"] != "PIPE-X-1" {
		t.Errorf("env not propagated to spawn")
	}
	if got.Branch != "feat/BL-X" {
		t.Errorf("branch = %q", got.Branch)
	}
}

func TestSpawnWorker_NilClientErrors(t *testing.T) {
	w := &SpawnWorker{}
	if _, err := w.Run(context.Background(), sampleJobContext("plan_slice")); err == nil {
		t.Error("expected error for nil client")
	}
}

// TestSpawnWorker_GitCaptureFallbacks pins the WorkingDir + BaseBranch
// plumbing the spawn client's cumulative git capture depends on
// (issue #224): the run's worktree wins when allocated, the
// operator-local RepoRoot backstops the standard path where
// Run.WorktreePath is never populated, and BaseBranch always resolves
// to a usable base ref instead of silently disabling the capture.
func TestSpawnWorker_GitCaptureFallbacks(t *testing.T) {
	cases := []struct {
		name           string
		worktreePath   string
		repoRoot       string
		baseBranch     string
		wantWorkingDir string
		wantBaseBranch string
	}{
		{
			name:           "run worktree wins over repo root",
			worktreePath:   "/tmp/wt",
			repoRoot:       "/var/lib/loom-mills/loom-core",
			wantWorkingDir: "/tmp/wt",
			wantBaseBranch: "main",
		},
		{
			name:           "repo root backstops missing worktree",
			worktreePath:   "",
			repoRoot:       "/var/lib/loom-mills/loom-core",
			wantWorkingDir: "/var/lib/loom-mills/loom-core",
			wantBaseBranch: "main",
		},
		{
			name:           "explicit base branch passes through",
			worktreePath:   "",
			repoRoot:       "/var/lib/loom-mills/loom-core",
			baseBranch:     "release",
			wantWorkingDir: "/var/lib/loom-mills/loom-core",
			wantBaseBranch: "release",
		},
		{
			name:           "no worktree and no repo root leaves capture off",
			worktreePath:   "",
			repoRoot:       "",
			wantWorkingDir: "",
			wantBaseBranch: "main",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-1"}}
			w := &SpawnWorker{Client: sp, BaseBranch: tc.baseBranch, RepoRoot: tc.repoRoot}
			jc := sampleJobContext("implement", func(jc *JobContext) {
				jc.Run.WorktreePath = tc.worktreePath
			})
			if _, err := w.Run(context.Background(), jc); err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(sp.calls) != 1 {
				t.Fatalf("spawn calls = %d, want 1", len(sp.calls))
			}
			got := sp.calls[0]
			if got.WorkingDir != tc.wantWorkingDir {
				t.Errorf("WorkingDir = %q, want %q", got.WorkingDir, tc.wantWorkingDir)
			}
			if got.BaseBranch != tc.wantBaseBranch {
				t.Errorf("BaseBranch = %q, want %q", got.BaseBranch, tc.wantBaseBranch)
			}
		})
	}
}

// TestSpawnWorker_KeysEveryStageSpawn pins the restart-survival contract for
// spawn-dispatched stages: every dispatch carries a deterministic
// idempotency key derived from (run, stage, attempt). An UNKEYED spawn is
// fail-fasted by HUD restart recovery ("agent turn driver lost across
// mobile-hud restart; unkeyed spawn cannot be re-driven" —
// internal/hud/spawn.go classifyInterruptedSpawn), which killed every
// in-flight Mills stage whenever mobile-hud rolled (PIPE-bl-mills-operator-
// stop-lever-20260726, pr_self_review). The attempt number MUST be part of
// the key: spawnWithKey re-attaches to any durable state with the same key,
// including a terminal one, so a retry that reused its predecessor's key
// would instantly adopt the old failure.
func TestSpawnWorker_KeysEveryStageSpawn(t *testing.T) {
	sp := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-1"}}
	w := &SpawnWorker{Client: sp}

	jc := sampleJobContext("pr_self_review", func(jc *JobContext) { jc.Attempt = 2 })
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := sp.calls[0].IdempotencyKey, "mills-stage:PIPE-X-1:pr_self_review:2"; got != want {
		t.Errorf("IdempotencyKey = %q, want %q", got, want)
	}

	// A retry (new attempt) must mint a DIFFERENT key so it never re-attaches
	// to the previous attempt's terminal spawn.
	jc.Attempt = 3
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run attempt 3: %v", err)
	}
	if sp.calls[1].IdempotencyKey == sp.calls[0].IdempotencyKey {
		t.Errorf("attempts 2 and 3 share key %q — a retry would adopt the old failure", sp.calls[0].IdempotencyKey)
	}

	// Different stages of the same run must not collide either.
	other := sampleJobContext("implement", func(jc *JobContext) { jc.Attempt = 2 })
	if _, err := w.Run(context.Background(), other); err != nil {
		t.Fatalf("run implement: %v", err)
	}
	if sp.calls[2].IdempotencyKey == sp.calls[0].IdempotencyKey {
		t.Errorf("stages pr_self_review and implement share key %q", sp.calls[0].IdempotencyKey)
	}

	// Without a run identity there is no valid key namespace: stay unkeyed
	// rather than sharing one sentinel key across unrelated dispatches.
	if key := stageIdempotencyKey(JobContext{Stage: Stage{ID: "implement"}, Attempt: 1}); key != "" {
		t.Errorf("nil-run key = %q, want empty (legacy unkeyed)", key)
	}
	if key := stageIdempotencyKey(JobContext{Run: &store.PipelineRun{}, Stage: Stage{ID: "implement"}, Attempt: 1}); key != "" {
		t.Errorf("empty-run-id key = %q, want empty (legacy unkeyed)", key)
	}
}

// TestDispatcher_ThreadsAttemptOntoJobContext pins the runner→worker attempt
// plumbing stageIdempotencyKey depends on: the attempt the runner stashes on
// the stage context surfaces as JobContext.Attempt, and an unstamped context
// yields zero.
func TestDispatcher_ThreadsAttemptOntoJobContext(t *testing.T) {
	var got []int
	w := workerFn(func(_ context.Context, jc JobContext) (StageOutput, error) {
		got = append(got, jc.Attempt)
		return StageOutput{}, nil
	})
	d := NewDispatcher(map[string]Worker{"implement": w}, nil)
	jc := sampleJobContext("implement")

	if _, err := d.Dispatch(WithStageAttempt(context.Background(), 4), jc.Run, jc.Item, jc.Stage, jc.Prior); err != nil {
		t.Fatalf("dispatch stamped: %v", err)
	}
	if _, err := d.Dispatch(context.Background(), jc.Run, jc.Item, jc.Stage, jc.Prior); err != nil {
		t.Fatalf("dispatch unstamped: %v", err)
	}
	if len(got) != 2 || got[0] != 4 || got[1] != 0 {
		t.Errorf("attempts seen = %v, want [4 0]", got)
	}
}

func TestDispatcher_SpawnMetadataAttempt(t *testing.T) {
	sp := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-1"}}
	d := NewDispatcher(map[string]Worker{
		"implement": &SpawnWorker{Client: sp},
	}, nil)
	jc := sampleJobContext("implement")

	if _, err := d.Dispatch(WithStageAttempt(context.Background(), 4), jc.Run, jc.Item, jc.Stage, jc.Prior); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(sp.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(sp.calls))
	}
	if got, want := sp.calls[0].Env["LOOM_MILLS_ATTEMPT"], "4"; got != want {
		t.Errorf("LOOM_MILLS_ATTEMPT = %q, want %q", got, want)
	}
}

func TestStageIdempotencyKeyRejectsNonPositiveAttempts(t *testing.T) {
	base := JobContext{Run: &store.PipelineRun{ID: "PIPE-1"}, Stage: Stage{ID: "implement"}}
	for _, attempt := range []int{0, -1} {
		base.Attempt = attempt
		if got := stageIdempotencyKey(base); got != "" {
			t.Errorf("attempt %d key = %q, want empty", attempt, got)
		}
	}
	base.Attempt = 3
	if got, want := stageIdempotencyKey(base), "mills-stage:PIPE-1:implement:3"; got != want {
		t.Errorf("positive-attempt key = %q, want %q", got, want)
	}
}

func TestWeaverWorker_RecordsResearchNotes(t *testing.T) {
	wv := &fakeWeaver{resp: WeaverResponse{
		SpawnID: "weaver-1", CostUSD: 0.1, Notes: "found prior art",
	}}
	w := &WeaverWorker{Client: wv, PromptFor: func(jc JobContext) string { return "research " + jc.Item.ID }}
	out, err := w.Run(context.Background(), sampleJobContext("research"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Artifacts["research_notes"] != "found prior art" {
		t.Errorf("research_notes = %v", out.Artifacts["research_notes"])
	}
	if len(wv.calls) != 1 || wv.calls[0].Prompt != "research BL-X" {
		t.Errorf("weaver call wrong: %+v", wv.calls)
	}
}

func TestWeaverWorker_ModelUnavailableSoftSkips(t *testing.T) {
	// Research is advisory: when every candidate model is 503-parked the stage
	// must complete SUCCESS (nil error → runner records outcome success) with an
	// explicit skip note, not error and burn retries/escalate.
	wv := &fakeWeaver{err: fmt.Errorf("flexinfer chat: all candidate models unavailable (tried [m]): %w", ErrModelUnavailable)}
	w := &WeaverWorker{Client: wv, PromptFor: func(jc JobContext) string { return "research " + jc.Item.ID }}
	out, err := w.Run(context.Background(), sampleJobContext("research"))
	if err != nil {
		t.Fatalf("model-unavailable research must soft-skip, got error: %v", err)
	}
	if out.Artifacts[researchSkippedArtifactKey] != true {
		t.Errorf("expected %s artifact flag, got %+v", researchSkippedArtifactKey, out.Artifacts)
	}
	note, _ := out.Artifacts["research_notes"].(string)
	if !strings.Contains(note, "research skipped: model unavailable") {
		t.Errorf("research_notes should carry the skip note, got %q", note)
	}
	if !strings.Contains(out.LogTail, "research skipped: model unavailable") {
		t.Errorf("log_tail should carry the skip note, got %q", out.LogTail)
	}
}

func TestWeaverWorker_NonModelErrorStillFails(t *testing.T) {
	// A genuine (non model-unavailable) research error must still fail the stage.
	wv := &fakeWeaver{err: errors.New("weaver: boom")}
	w := &WeaverWorker{Client: wv, PromptFor: func(jc JobContext) string { return "research " + jc.Item.ID }}
	if _, err := w.Run(context.Background(), sampleJobContext("research")); err == nil {
		t.Fatal("expected a non-model error to fail the research stage")
	}
}

func TestDevboxWorker_FailedGateReturnsError(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{
		Passed:  false,
		CostUSD: 0.05,
		Checks:  []DevboxCheck{{Name: "lint", Passed: false, Output: "boom"}},
	}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	out, err := w.Run(context.Background(), sampleJobContext("tests"))
	if err == nil {
		t.Error("expected error on failed quality gate")
	}
	if out.CostUSD != 0.05 {
		t.Errorf("cost not propagated on fail: %v", out.CostUSD)
	}
	if len(db.calls) != 1 {
		t.Errorf("devbox calls = %d", len(db.calls))
	}
	// The error must name the failing check and carry its output so
	// Classify sees infra/transient needles and the escalation is
	// actionable (escalation #289: "devbox quality gate failed: 1 checks"
	// gave the operator nothing).
	for _, want := range []string{"lint", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate error missing %q: %v", want, err)
		}
	}
}

// TestDevboxWorker_EmptyCheckSetIsTransientInfra guards the tests-stage fix
// (live 2026-07-16: "0/0 checks marked failed; gate reported not passed" ×4
// escalated as code). A not-passed verdict with ZERO executed checks is an
// infrastructure contract violation, not a test failure: the error must wrap
// ErrDevboxGateNoChecks so Classify tags it ClassTransient (free retry), and it
// must carry the gate JSON tail for actionable escalation.
func TestDevboxWorker_EmptyCheckSetIsTransientInfra(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{
		Passed:  false,
		CostUSD: 0.02,
		LogTail: `{"language":"go","passed":false,"checks":[]}`,
		Checks:  nil, // zero executed checks
	}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	out, err := w.Run(context.Background(), sampleJobContext("tests"))
	if err == nil {
		t.Fatal("expected error when the gate reports not-passed with no checks")
	}
	if !errors.Is(err, ErrDevboxGateNoChecks) {
		t.Errorf("error must wrap ErrDevboxGateNoChecks, got %v", err)
	}
	if cls := Classify(err); cls != ClassTransient {
		t.Errorf("empty-check gate should classify transient, got %s", cls)
	}
	// The gate JSON tail is embedded (quote-escaped by %q) so the escalation is
	// actionable; assert on tail content that survives escaping.
	for _, want := range []string{"language", "checks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should carry the gate JSON tail (%q): %v", want, err)
		}
	}
	if out.CostUSD != 0.02 {
		t.Errorf("cost not propagated: %v", out.CostUSD)
	}
	// A non-empty failing check set is still a real (non-transient) test failure.
	db2 := &fakeDevbox{resp: DevboxResponse{Passed: false, Checks: []DevboxCheck{{Name: "test", Passed: false, Output: "assertion failed"}}}}
	w2 := &DevboxWorker{Client: db2, Project: "loom-core", AgentID: "claude-code"}
	_, err2 := w2.Run(context.Background(), sampleJobContext("tests"))
	if err2 == nil {
		t.Fatal("expected error for a real failing check")
	}
	if errors.Is(err2, ErrDevboxGateNoChecks) {
		t.Errorf("a populated failing check set must NOT be the no-checks infra error: %v", err2)
	}
}

func TestSummarizeFailedChecks(t *testing.T) {
	checks := []DevboxCheck{
		{Name: "fmt", Passed: true},
		{Name: "lint", Passed: false, ExitCode: 1, Output: "pkg/x.go:1: undefined: y\n"},
		{Name: "test", Passed: false, ExitCode: 2, Output: `exec error: unable to upgrade connection: container not found ("devbox")`},
	}
	got := summarizeFailedChecks(checks)
	for _, want := range []string{
		"2/3 checks failed",
		"lint[exit=1]",
		"undefined: y",
		"test[exit=2]",
		`container not found ("devbox")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "fmt[") {
		t.Errorf("summary must not list passing checks: %s", got)
	}
	// A long output keeps its TAIL (where compilers print the error), capped.
	long := DevboxCheck{Name: "test", Passed: false, Output: strings.Repeat("x", 1000) + " FINAL ERROR"}
	tail := summarizeFailedChecks([]DevboxCheck{long})
	if !strings.Contains(tail, "FINAL ERROR") {
		t.Errorf("long output must keep its tail: %s", tail)
	}
	if len(tail) > 400 {
		t.Errorf("per-check tail not capped: len=%d", len(tail))
	}
	// Passed=false with no failing check recorded still yields a stable message.
	if got := summarizeFailedChecks([]DevboxCheck{{Name: "fmt", Passed: true}}); !strings.Contains(got, "gate reported not passed") {
		t.Errorf("empty-failure fallback wrong: %s", got)
	}
}

// TestGitLabWorker_CIWatchTerminalFailureWrapsSentinel guards escalation #292
// (2026-07-08): a pipeline that reached a terminal non-success state is
// deterministic — re-watching re-polls the same dead pipeline — so runCI must
// wrap ErrCIPipelineTerminal for the runner's escalate-on-first-sight branch.
func TestGitLabWorker_CIWatchTerminalFailureWrapsSentinel(t *testing.T) {
	failed := testCIPollResponse("failed", "failed-head")
	failed.LogTail = "job x failed"
	gl := &fakeGitLab{pollResp: failed}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("ci_watch")
	iid := int64(999)
	addMRProvenance(&jc, iid, testCIProject, testCISource, testCITarget)
	_, err := w.Run(context.Background(), jc)
	if err == nil {
		t.Fatal("expected error for failed pipeline")
	}
	if !errors.Is(err, ErrCIPipelineTerminal) {
		t.Fatalf("error must wrap ErrCIPipelineTerminal: %v", err)
	}
	if Classify(err) != ClassCode {
		t.Fatalf("terminal CI failure keeps class=code, got %s", Classify(err))
	}
	// A successful poll must not error.
	gl.pollResp = testCIPollResponse("success", "successful-head")
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("success poll: %v", err)
	}
}

// TestDevboxWorker_CanaryScopesChecks asserts that a backlog item
// labeled "mills-canary" narrows the gate to fmt-only. Prior to this
// scoping, every canary ran `go vet ./...` on the entire codebase even
// though the canary only modifies a Markdown fixture — and any
// transient go-toolchain failure in the sandbox (module cache, network
// to the proxy) would escalate the canary on infra it was not meant
// to exercise.
func TestDevboxWorker_CanaryScopesChecks(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "fmt", Passed: true}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Item.Labels = []string{"mills-canary", "safe-fixture"}
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("expected 1 devbox call, got %d", len(db.calls))
	}
	got := db.calls[0].Checks
	if len(got) != 1 || got[0] != "fmt" {
		t.Fatalf("canary checks = %v, want [fmt]", got)
	}
}

// TestDevboxWorker_NonCanaryScopesToFmt asserts that a non-canary backlog
// item ALSO sends the sandbox-safe Checks=[fmt] scope. Whole-module
// go vet/test can't run in the loom-core-only git-clone sandbox (go.work
// siblings + fi-accel cgo); GitLab CI via ci_watch is the authoritative
// lint/test/build gate. Regression for MILLS-DEBT-TICKLABEL-20260624, which
// escalated at the tests stage on "FAIL lint (79ms)".
func TestDevboxWorker_NonCanaryScopesToFmt(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "fmt", Passed: true}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Item.Labels = []string{"feature", "p1"}
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := db.calls[0].Checks
	if len(got) != 1 || got[0] != "fmt" {
		t.Fatalf("non-canary checks = %v, want [fmt]", got)
	}
}

func TestDevboxWorker_ForwardsAllowlistedDeclaredTestsAndRecordsSkipped(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "fmt", Passed: true}, {Name: "test:0", Passed: true}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Item.Success.Tests = []string{"go test ./cmd/loom -run Mills", "make test", "go test ./pkg/mills/..."}
		jc.Env = map[string]string{"GIT_TOKEN": "token"}
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := db.calls[0].TestCommands; !reflect.DeepEqual(got, []string{"go test ./cmd/loom -run Mills", "go test ./pkg/mills/..."}) {
		t.Fatalf("test commands = %v", got)
	}
	if db.calls[0].Env["GIT_TOKEN"] != "token" {
		t.Fatalf("request env not forwarded: %#v", db.calls[0].Env)
	}
	if got := out.Artifacts[skippedDeclaredTestsArtifactKey]; !reflect.DeepEqual(got, []string{"make test"}) {
		t.Fatalf("skipped tests artifact = %#v", got)
	}
}

func TestDevboxWorker_DeclaredTestFailureIsCodeClass(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: false, Checks: []DevboxCheck{{Name: "fmt", Passed: true}, {Name: "test:0", Passed: false, ExitCode: 1, Output: "assertion failed"}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Item.Success.Tests = []string{"go test ./pkg/mills/..."}
	})
	_, err := w.Run(context.Background(), jc)
	if err == nil {
		t.Fatal("expected declared-test failure")
	}
	if got := Classify(err); got != ClassCode {
		t.Fatalf("failure class = %s, want code", got)
	}
	if !strings.Contains(err.Error(), "test:0") || !strings.Contains(err.Error(), "assertion failed") {
		t.Fatalf("failure detail missing: %v", err)
	}
}

func TestDevboxWorker_PassPropagatesArtifacts(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{
		Passed:  true,
		CostUSD: 0.02,
		Checks:  []DevboxCheck{{Name: "test", Passed: true}},
	}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	out, err := w.Run(context.Background(), sampleJobContext("tests"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Artifacts["passed"] != true {
		t.Errorf("passed flag missing")
	}
}

func TestGitLabWorker_CreateMR_RecordsIID(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{
		MRIID: 99, URL: "https://gl/mr/99", Project: testCIProject,
		SourceBranch: testCISource, TargetBranch: testCITarget, CostUSD: 0.01,
	}}
	w := &GitLabWorker{Client: gl, MRTitle: func(jc JobContext) string { return "feat: x" }}
	out, err := w.Run(context.Background(), sampleJobContext("mr"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.MRIID != 99 {
		t.Errorf("mr_iid = %d", out.MRIID)
	}
	if out.Artifacts["mr_url"] != "https://gl/mr/99" {
		t.Errorf("mr_url not recorded")
	}
	for key, want := range map[string]any{
		"mr_project":       testCIProject,
		"mr_source_branch": testCISource,
		"mr_target_branch": testCITarget,
	} {
		if got := out.Artifacts[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if len(gl.createCalls) != 1 || gl.createCalls[0].Title != "feat: x" {
		t.Errorf("createMR call wrong: %+v", gl.createCalls)
	}
	if gl.createCalls[0].SourceBranch != "feat/BL-X" {
		t.Errorf("source branch = %q", gl.createCalls[0].SourceBranch)
	}
}

func TestGitLabWorker_SourceBranchCallbackCannotOverrideContract(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	w := &GitLabWorker{Client: gl, SourceBranch: func(JobContext) string { return "fix/retry-specific" }}
	if _, err := w.Run(context.Background(), sampleJobContext("mr")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := gl.createCalls[0].SourceBranch; got != "feat/BL-X" {
		t.Fatalf("source branch = %q, want immutable contract branch", got)
	}
}

// Pre-2026-05-25 empty-MR fix: runMR must push the source branch
// before CreateMR, so GitLab actually has commits to point the MR at.
// Pin both the call ordering and the push args (working dir + branch).
type recordingPusher struct {
	calls       []recordingPushCall
	returnError error
}
type recordingPushCall struct {
	WorkingDir string
	Branch     string
}

func (p *recordingPusher) Push(_ context.Context, workingDir, branch string) error {
	p.calls = append(p.calls, recordingPushCall{WorkingDir: workingDir, Branch: branch})
	return p.returnError
}

func TestGitLabWorker_CreateMR_PushesBranchBeforeCreatingMR(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	pusher := &recordingPusher{}
	w := &GitLabWorker{Client: gl, BranchPusher: pusher}
	if _, err := w.Run(context.Background(), sampleJobContext("mr")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(pusher.calls) != 1 {
		t.Fatalf("pusher called %d times, want 1", len(pusher.calls))
	}
	if pusher.calls[0].WorkingDir != "/tmp/wt" {
		t.Errorf("push workingDir = %q, want /tmp/wt (from sampleJobContext)", pusher.calls[0].WorkingDir)
	}
	if pusher.calls[0].Branch != "feat/BL-X" {
		t.Errorf("push branch = %q, want feat/BL-X (from BranchContractFor)", pusher.calls[0].Branch)
	}
	if len(gl.createCalls) != 1 {
		t.Errorf("CreateMR called %d times after push, want 1", len(gl.createCalls))
	}
}

func TestGitLabWorker_CreateMR_PushFailureBubblesUp(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	pusher := &recordingPusher{returnError: errors.New("push refused")}
	w := &GitLabWorker{Client: gl, BranchPusher: pusher}
	_, err := w.Run(context.Background(), sampleJobContext("mr"))
	if err == nil {
		t.Fatal("expected error when pusher fails")
	}
	if len(gl.createCalls) != 0 {
		t.Errorf("CreateMR fired despite push failure (calls=%d)", len(gl.createCalls))
	}
}

func TestGitLabWorker_CreateMR_NoPusherStillWorks(t *testing.T) {
	// Legacy behavior: GitLabWorker with no BranchPusher should still
	// open the MR. We just won't push — keeps the old test fixtures green.
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	w := &GitLabWorker{Client: gl}
	if _, err := w.Run(context.Background(), sampleJobContext("mr")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(gl.createCalls) != 1 {
		t.Errorf("CreateMR not called, len=%d", len(gl.createCalls))
	}
}

func TestGitLabWorker_CreateMR_SkipsPushWhenWorktreeMissing(t *testing.T) {
	// A run with no worktree path (e.g. resumed state where worktree
	// allocation hasn't happened yet) shouldn't attempt the push — the
	// CommandRunner would fail with cryptic errors. Skip cleanly.
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	pusher := &recordingPusher{}
	w := &GitLabWorker{Client: gl, BranchPusher: pusher}
	jc := sampleJobContext("mr", func(jc *JobContext) {
		jc.Run.WorktreePath = ""
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(pusher.calls) != 0 {
		t.Errorf("pusher called %d times despite missing worktree, want 0", len(pusher.calls))
	}
	if len(gl.createCalls) != 1 {
		t.Errorf("CreateMR not called, len=%d", len(gl.createCalls))
	}
}

// Slice 2a: when AutoMergeFor returns true, the CreateMRRequest carries
// AutoMerge=true through to the GitLab client.
func TestGitLabWorker_CreateMR_PassesAutoMergeFromCallback(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	w := &GitLabWorker{
		Client:       gl,
		AutoMergeFor: func(jc JobContext) bool { return true },
	}
	if _, err := w.Run(context.Background(), sampleJobContext("mr")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !gl.createCalls[0].AutoMerge {
		t.Errorf("CreateMRRequest.AutoMerge = false, want true (callback returned true)")
	}
}

// And the negative: callback returns false → no auto-merge.
func TestGitLabWorker_CreateMR_AutoMergeOffByDefault(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	w := &GitLabWorker{Client: gl} // no AutoMergeFor wired
	if _, err := w.Run(context.Background(), sampleJobContext("mr")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gl.createCalls[0].AutoMerge {
		t.Errorf("CreateMRRequest.AutoMerge = true with no callback + no item.Policy.AutoMerge")
	}
}

// Item.Policy.AutoMerge alone (no callback) still flips the flag —
// importer/council can opt an item in even when the operator hasn't
// wired the policy.LabelOverrideFor callback.
func TestGitLabWorker_CreateMR_AutoMergeFromItemPolicy(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("mr", func(jc *JobContext) {
		jc.Item.Policy.AutoMerge = true
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !gl.createCalls[0].AutoMerge {
		t.Errorf("AutoMerge = false; item.Policy.AutoMerge should have flipped it")
	}
}

func TestGitLabWorker_CreateMR_FanOutParentUsesIntegrationBranch(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("mr", func(jc *JobContext) {
		jc.Item.Slices = []store.Slice{
			{Name: "alpha", ParallelWith: []string{"beta"}},
			{Name: "beta", ParallelWith: []string{"alpha"}},
		}
		jc.Env = BuildMillsEnv(jc.Run, jc.Item, jc.Stage)
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("mr: %v", err)
	}
	if gl.createCalls[0].SourceBranch != "integrate/BL-X" {
		t.Errorf("source branch = %q", gl.createCalls[0].SourceBranch)
	}
}

func TestGitLabWorker_CIWatch_FailingPipelineErrors(t *testing.T) {
	gl := &fakeGitLab{pollResp: testCIPollResponse("failed", "failed-head")}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("ci_watch", func(jc *JobContext) {
		addMRProvenance(jc, 99, testCIProject, testCISource, testCITarget)
	})
	out, err := w.Run(context.Background(), jc)
	if err == nil {
		t.Error("expected error on failed pipeline")
	}
	if out.Artifacts["ci_status"] != "failed" {
		t.Errorf("ci_status not recorded")
	}
}

func TestGitLabWorker_CIWatch_PersistsTestedIdentity(t *testing.T) {
	gl := &fakeGitLab{pollResp: testCIPollResponse("success", "tested-head")}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("ci_watch", func(jc *JobContext) {
		addMRProvenance(jc, 99, testCIProject, testCISource, testCITarget)
	})

	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for key, want := range testCIArtifacts("tested-head") {
		if got := out.Artifacts[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestGitLabWorker_CIWatch_NoMRIDErrors(t *testing.T) {
	gl := &fakeGitLab{pollResp: PollPipelineResponse{Status: "success"}}
	w := &GitLabWorker{Client: gl}
	if _, err := w.Run(context.Background(), sampleJobContext("ci_watch")); err == nil {
		t.Error("expected error when no mr_iid present")
	}
}

func TestGitLabWorker_CIWatch_MissingMRProvenanceBlocksClient(t *testing.T) {
	gl := &fakeGitLab{pollResp: testCIPollResponse("success", "must-not-authorize")}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("ci_watch", func(jc *JobContext) {
		mr := int64(99)
		jc.Run.MRIID = &mr
	})
	_, err := w.Run(context.Background(), jc)
	if err == nil || !errors.Is(err, ErrMergeAuthorizationStale) {
		t.Fatalf("missing MR provenance error = %v", err)
	}
	if len(gl.pollCalls) != 0 {
		t.Fatalf("PollPipeline called %d times without MR provenance", len(gl.pollCalls))
	}
}

func TestGitLabWorker_Merge_PropagatesSHA(t *testing.T) {
	gl := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "abc123", CostUSD: 0.01}}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("merge", func(jc *JobContext) {
		mr := int64(99)
		jc.Run.MRIID = &mr
		jc.Prior["ci_watch"] = StageOutput{Artifacts: testCIArtifacts("tested-head")}
		jc.MergeRecoveryPipelineCreateAttempted = true
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.MergedSHA != "abc123" {
		t.Errorf("merged_sha = %q", out.MergedSHA)
	}
	if got := gl.mergeCalls[0].ExpectedSHA; got != "tested-head" {
		t.Errorf("merge expected sha = %q, want tested-head", got)
	}
	if got := gl.mergeCalls[0]; got.Project != testCIProject || got.SourceBranch != testCISource || got.TargetBranch != testCITarget {
		t.Errorf("merge authorization = %q:%q→%q", got.Project, got.SourceBranch, got.TargetBranch)
	}
	if !gl.mergeCalls[0].RecoveryPipelineCreateAttempted {
		t.Error("durable recovery pipeline-create fence was not propagated")
	}
}

func TestGitLabWorker_Merge_MissingCISHABlocksClient(t *testing.T) {
	gl := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "must-not-merge"}}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("merge", func(jc *JobContext) {
		mr := int64(99)
		jc.Run.MRIID = &mr
	})

	_, err := w.Run(context.Background(), jc)
	if err == nil {
		t.Fatal("expected missing ci_sha to fail closed")
	}
	if got := Classify(err); got != ClassConfig {
		t.Fatalf("Classify(missing ci_sha) = %s, want %s: %v", got, ClassConfig, err)
	}
	if len(gl.mergeCalls) != 0 {
		t.Fatalf("merge client called %d times without ci_sha", len(gl.mergeCalls))
	}
}

func TestGitLabWorker_Merge_IncompleteCIIdentityBlocksClient(t *testing.T) {
	for _, missing := range []string{"ci_project", "ci_source_branch", "ci_target_branch", "ci_sha"} {
		t.Run(missing, func(t *testing.T) {
			gl := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "must-not-merge"}}
			w := &GitLabWorker{Client: gl}
			artifacts := testCIArtifacts("tested-head")
			delete(artifacts, missing)
			jc := sampleJobContext("merge", func(jc *JobContext) {
				mr := int64(99)
				jc.Run.MRIID = &mr
				jc.Prior["ci_watch"] = StageOutput{Artifacts: artifacts}
			})
			_, err := w.Run(context.Background(), jc)
			if err == nil || !errors.Is(err, ErrMergeAuthorizationStale) {
				t.Fatalf("missing %s error = %v", missing, err)
			}
			if missing == "ci_sha" && !strings.Contains(err.Error(), "no ci_sha") {
				t.Fatalf("missing ci_sha diagnostic changed: %v", err)
			}
			if len(gl.mergeCalls) != 0 {
				t.Fatalf("Merge called %d times with missing %s", len(gl.mergeCalls), missing)
			}
		})
	}
}

func TestGitLabWorker_Cleanup_UsesPersistedMRProvenance(t *testing.T) {
	gl := &fakeGitLab{}
	w := &GitLabWorker{Client: gl, SourceBranch: func(jc JobContext) string { return "feat/mutated" }}
	jc := sampleJobContext("cleanup", func(jc *JobContext) {
		addMRProvenance(jc, 99, testCIProject, "feat/persisted", testCITarget)
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(gl.cleanupCalls) != 1 || gl.cleanupCalls[0].BranchName != "feat/persisted" {
		t.Errorf("cleanup did not use persisted branch: %+v", gl.cleanupCalls)
	}
}

func TestGitLabWorker_Cleanup_IncompleteMRProvenanceSkipsDeletion(t *testing.T) {
	for _, missing := range []string{"mr_project", "mr_source_branch", "mr_target_branch"} {
		t.Run(missing, func(t *testing.T) {
			gl := &fakeGitLab{}
			w := &GitLabWorker{Client: gl}
			jc := sampleJobContext("cleanup", func(jc *JobContext) {
				addMRProvenance(jc, 99, testCIProject, testCISource, testCITarget)
				delete(jc.Prior["mr"].Artifacts, missing)
			})
			out, err := w.Run(context.Background(), jc)
			if err != nil {
				t.Fatalf("missing %s cleanup error = %v", missing, err)
			}
			if !strings.Contains(out.LogTail, "skipped branch deletion") {
				t.Fatalf("missing %s cleanup log = %q", missing, out.LogTail)
			}
			if len(gl.cleanupCalls) != 0 {
				t.Fatalf("Cleanup called %d times with missing %s", len(gl.cleanupCalls), missing)
			}
		})
	}
}

func TestGitLabWorker_MRRequiresSourceBranch(t *testing.T) {
	gl := &fakeGitLab{}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("mr", func(jc *JobContext) {
		jc.Item.ID = ""
		jc.Env = BuildMillsEnv(jc.Run, jc.Item, jc.Stage)
	})
	if _, err := w.Run(context.Background(), jc); err == nil {
		t.Fatal("expected error when source branch is unavailable")
	}
	if len(gl.createCalls) != 0 {
		t.Errorf("CreateMR should not be called: %+v", gl.createCalls)
	}
}

func TestGitLabWorker_UnknownStageErrors(t *testing.T) {
	gl := &fakeGitLab{}
	w := &GitLabWorker{Client: gl}
	if _, err := w.Run(context.Background(), sampleJobContext("nope")); err == nil {
		t.Error("expected error for unknown stage")
	}
}

// ----- Dispatcher routing -----

func TestDispatcher_RoutesToRegistered(t *testing.T) {
	called := ""
	wA := workerFn(func(_ context.Context, jc JobContext) (StageOutput, error) {
		called = jc.Stage.ID
		return StageOutput{CostUSD: 0.1}, nil
	})
	d := NewDispatcher(map[string]Worker{"plan_slice": wA}, nil)
	jc := sampleJobContext("plan_slice")
	out, err := d.Dispatch(context.Background(), jc.Run, jc.Item, jc.Stage, jc.Prior)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if called != "plan_slice" || out.CostUSD != 0.1 {
		t.Errorf("dispatcher did not route correctly: called=%s out=%+v", called, out)
	}
}

func TestDispatcher_NoFallbackUnmappedErrors(t *testing.T) {
	d := NewDispatcher(nil, nil)
	jc := sampleJobContext("plan_slice")
	if _, err := d.Dispatch(context.Background(), jc.Run, jc.Item, jc.Stage, jc.Prior); err == nil {
		t.Error("expected error for unmapped stage with no fallback")
	}
}

func TestDispatcher_FallbackHandlesUnmapped(t *testing.T) {
	called := false
	fb := workerFn(func(_ context.Context, _ JobContext) (StageOutput, error) {
		called = true
		return StageOutput{}, nil
	})
	d := NewDispatcher(nil, fb)
	jc := sampleJobContext("plan_slice")
	if _, err := d.Dispatch(context.Background(), jc.Run, jc.Item, jc.Stage, jc.Prior); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !called {
		t.Error("fallback should have run")
	}
}

func TestDispatcher_RegisterReplacesRoute(t *testing.T) {
	d := NewDispatcher(nil, nil)
	d.Register("implement", workerFn(func(_ context.Context, _ JobContext) (StageOutput, error) {
		return StageOutput{}, errors.New("first")
	}))
	d.Register("implement", workerFn(func(_ context.Context, _ JobContext) (StageOutput, error) {
		return StageOutput{CostUSD: 99}, nil
	}))
	jc := sampleJobContext("implement")
	out, err := d.Dispatch(context.Background(), jc.Run, jc.Item, jc.Stage, jc.Prior)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if out.CostUSD != 99 {
		t.Errorf("Register did not replace route")
	}
}

func TestDefaultRoutes_WiresAllStages(t *testing.T) {
	routes := DefaultRoutes(&fakeSpawn{}, &fakeWeaver{}, &fakeDevbox{}, &fakeGitLab{}, "loom-core", "claude-code", nil, nil, nil)
	for _, want := range []string{"plan_slice", "research", "implement", "tests", "pr_self_review", "mr", "ci_watch", "merge", "cleanup"} {
		if _, ok := routes[want]; !ok {
			t.Errorf("DefaultRoutes missing %s", want)
		}
	}
}

// workerFn adapts a function into the Worker interface.
type workerFn func(ctx context.Context, jc JobContext) (StageOutput, error)

func (f workerFn) Run(ctx context.Context, jc JobContext) (StageOutput, error) { return f(ctx, jc) }

// TestSpawnWorker_Substrate_FromPolicy covers Slice 2b's contract: the
// SpawnWorker reads SubstrateFor at every Run, populates
// SpawnRequest.Substrate, and stays nil-safe so a worker without a
// SubstrateFor closure preserves pre-Slice-2b behavior (empty
// Substrate = spawn-service default backend).
//
// Spec: .loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md
func TestSpawnWorker_Substrate_FromPolicy(t *testing.T) {
	cases := []struct {
		name         string
		substrateFor func(stage string) string
		stage        string
		want         string
	}{
		{name: "nil_closure_yields_empty", substrateFor: nil, stage: "implement", want: ""},
		{name: "policy_default_yields_k8s",
			substrateFor: func(string) string { return "k8s" },
			stage:        "implement",
			want:         "k8s"},
		{name: "policy_harvester_vm_for_implement",
			substrateFor: func(stage string) string {
				if stage == "implement" {
					return "harvester-vm"
				}
				return "k8s"
			},
			stage: "implement",
			want:  "harvester-vm"},
		{name: "policy_keeps_plan_slice_on_k8s",
			substrateFor: func(stage string) string {
				if stage == "implement" {
					return "harvester-vm"
				}
				return "k8s"
			},
			stage: "plan_slice",
			want:  "k8s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spawn := &fakeSpawn{}
			w := &SpawnWorker{
				Client:       spawn,
				PromptFor:    func(JobContext) string { return "noop" },
				SubstrateFor: tc.substrateFor,
			}
			jc := sampleJobContext(tc.stage, func(j *JobContext) {
				// SpawnWorker requires a non-empty source branch; the default
				// fixture provides feat/BL-X via the branch contract.
			})
			if _, err := w.Run(context.Background(), jc); err != nil {
				t.Fatalf("Run: unexpected error: %v", err)
			}
			if got := len(spawn.calls); got != 1 {
				t.Fatalf("expected 1 spawn call, got %d", got)
			}
			if got := spawn.calls[0].Substrate; got != tc.want {
				t.Errorf("SpawnRequest.Substrate: got %q want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultRoutes_PropagatesSubstrateForToSpawnWorkers confirms the
// three spawn-driven stages constructed by DefaultRoutes carry the
// caller-supplied substrateFor closure. Without this, a downstream
// caller wiring a real policy closure would silently send empty
// Substrate values on every stage.
func TestDefaultRoutes_PropagatesSubstrateForToSpawnWorkers(t *testing.T) {
	subFor := func(stage string) string { return "harvester-vm" }
	routes := DefaultRoutes(&fakeSpawn{}, &fakeWeaver{}, &fakeDevbox{}, &fakeGitLab{}, "loom-core", "claude-code", nil, subFor, nil)
	for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
		sw, ok := routes[stage].(*SpawnWorker)
		if !ok {
			t.Fatalf("route %q: expected *SpawnWorker, got %T", stage, routes[stage])
		}
		if sw.SubstrateFor == nil {
			t.Errorf("route %q: SubstrateFor was not propagated", stage)
			continue
		}
		if got := sw.SubstrateFor(stage); got != "harvester-vm" {
			t.Errorf("route %q: SubstrateFor returned %q, want %q", stage, got, "harvester-vm")
		}
	}
}

// TestSpawnWorker_Agent_FromPolicy covers Slice W2's dispatcher contract as
// widened by per-item agent routing: the SpawnWorker reads RouteFor at every Run
// and, on a non-empty Agent, uses it as SpawnRequest.Model (the field the spawn
// client maps to agent_type). It stays byte-identical when RouteFor is nil OR
// returns a zero decision — the worker's static Model wins — so an operator that
// configures no routing keeps prior behavior.
func TestSpawnWorker_Agent_FromPolicy(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		routeFor func(context.Context, string, *store.BacklogItem) mills.AgentDecision
		stage    string
		want     string
	}{
		{name: "nil_closure_keeps_model", model: "claude-code", routeFor: nil, stage: "implement", want: "claude-code"},
		{name: "empty_return_keeps_model",
			model: "claude-code",
			routeFor: func(context.Context, string, *store.BacklogItem) mills.AgentDecision {
				return mills.AgentDecision{}
			},
			stage: "implement",
			want:  "claude-code"},
		{name: "policy_overrides_pr_self_review",
			model:    "claude-code",
			routeFor: stageAgentRouter(map[string]string{"pr_self_review": "gemini"}, "claude-code"),
			stage:    "pr_self_review",
			want:     "gemini"},
		{name: "policy_keeps_implement_on_default",
			model:    "claude-code",
			routeFor: stageAgentRouter(map[string]string{"pr_self_review": "gemini"}, "claude-code"),
			stage:    "implement",
			want:     "claude-code"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spawn := &fakeSpawn{}
			w := &SpawnWorker{
				Client:    spawn,
				Model:     tc.model,
				PromptFor: func(JobContext) string { return "noop" },
				RouteFor:  tc.routeFor,
			}
			if _, err := w.Run(context.Background(), sampleJobContext(tc.stage)); err != nil {
				t.Fatalf("Run: unexpected error: %v", err)
			}
			if got := len(spawn.calls); got != 1 {
				t.Fatalf("expected 1 spawn call, got %d", got)
			}
			if got := spawn.calls[0].Model; got != tc.want {
				t.Errorf("SpawnRequest.Model: got %q want %q", got, tc.want)
			}
		})
	}
}

// stageAgentRouter builds a RouteFor closure mimicking the operator's
// stage_agents-only resolution: a per-stage agent map with a default fallback
// and no model pin.
func stageAgentRouter(byStage map[string]string, fallback string) func(context.Context, string, *store.BacklogItem) mills.AgentDecision {
	return func(_ context.Context, stage string, _ *store.BacklogItem) mills.AgentDecision {
		if a, ok := byStage[stage]; ok {
			return mills.AgentDecision{Agent: a, DecidedBy: mills.AgentDecidedByStageAgents}
		}
		return mills.AgentDecision{Agent: fallback, DecidedBy: mills.AgentDecidedByDefault}
	}
}

// TestDefaultRoutes_PropagatesRouteForToSpawnWorkers confirms the three
// spawn-driven stages carry the caller-supplied routeFor closure while the
// non-spawn stages (research/tests) do not gain a spurious routing hook.
func TestDefaultRoutes_PropagatesRouteForToSpawnWorkers(t *testing.T) {
	routeFor := stageAgentRouter(nil, "gemini")
	routes := DefaultRoutes(&fakeSpawn{}, &fakeWeaver{}, &fakeDevbox{}, &fakeGitLab{}, "loom-core", "claude-code", nil, nil, routeFor)
	for _, stage := range []string{"plan_slice", "implement", "pr_self_review"} {
		sw, ok := routes[stage].(*SpawnWorker)
		if !ok {
			t.Fatalf("route %q: expected *SpawnWorker, got %T", stage, routes[stage])
		}
		if sw.RouteFor == nil {
			t.Errorf("route %q: RouteFor was not propagated", stage)
			continue
		}
		if got := sw.RouteFor(context.Background(), stage, nil).Agent; got != "gemini" {
			t.Errorf("route %q: RouteFor returned %q, want %q", stage, got, "gemini")
		}
	}
}

// TestSpawnWorker_Model_FromPolicy is the model half of the RouteFor contract:
// a non-empty decision Model sets SpawnRequest.AgentModel (the field the spawn
// client maps to the HUD spawn API's `model`) without disturbing the resolved
// agent. It stays byte-identical when RouteFor is nil OR leaves Model empty —
// AgentModel stays "" so the spawn server keeps its vendor default.
func TestSpawnWorker_Model_FromPolicy(t *testing.T) {
	routerFor := func(stage, model string) func(context.Context, string, *store.BacklogItem) mills.AgentDecision {
		return func(_ context.Context, got string, _ *store.BacklogItem) mills.AgentDecision {
			d := mills.AgentDecision{Agent: "codex", DecidedBy: mills.AgentDecidedByStageAgents}
			if got == stage {
				d.Model = model
			}
			return d
		}
	}
	cases := []struct {
		name     string
		routeFor func(context.Context, string, *store.BacklogItem) mills.AgentDecision
		stage    string
		want     string
	}{
		{name: "nil_closure_leaves_empty", routeFor: nil, stage: "implement", want: ""},
		{name: "empty_return_leaves_empty",
			routeFor: stageAgentRouter(nil, "codex"),
			stage:    "implement",
			want:     ""},
		{name: "policy_sets_implement_model",
			routeFor: routerFor("implement", "gpt-5.6-terra"),
			stage:    "implement",
			want:     "gpt-5.6-terra"},
		{name: "policy_sets_plan_slice_model",
			routeFor: routerFor("plan_slice", "gpt-5.6-sol"),
			stage:    "plan_slice",
			want:     "gpt-5.6-sol"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spawn := &fakeSpawn{}
			w := &SpawnWorker{
				Client:    spawn,
				Model:     "codex",
				PromptFor: func(JobContext) string { return "noop" },
				RouteFor:  tc.routeFor,
			}
			if _, err := w.Run(context.Background(), sampleJobContext(tc.stage)); err != nil {
				t.Fatalf("Run: unexpected error: %v", err)
			}
			if got := len(spawn.calls); got != 1 {
				t.Fatalf("expected 1 spawn call, got %d", got)
			}
			if got := spawn.calls[0].AgentModel; got != tc.want {
				t.Errorf("SpawnRequest.AgentModel: got %q want %q", got, tc.want)
			}
			// The agent/vendor selection (Model) must be untouched by the
			// model half of the decision.
			if got := spawn.calls[0].Model; got != "codex" {
				t.Errorf("SpawnRequest.Model (agent): got %q want %q", got, "codex")
			}
		})
	}
}

// TestDevboxScopeFor_FmtOnly is a regression guard for the fix that resolved the
// MILLS-DEBT-TICKLABEL-20260624-191214 escalation (mills A3 / W1.2). That run's
// in-pod tests stage ran `go vet ./...` in the devbox sandbox, which false-
// failed across 3 attempts ("PASS fmt / FAIL lint (79ms)", exit 0 yet not
// passed) and escalated a backlog item whose code was actually correct — the
// identical task merged unchanged the next day once the gate was scoped to
// `fmt`. The fix (gitCloneTestsScope) restricts the sandbox gate to `fmt` for
// ALL items; GitLab CI, enforced by the ci_watch stage, remains the
// authoritative lint/test/build gate before merge. If a future change widens
// the sandbox scope back to lint/test, this test fails before the flaky
// escalation can recur.
func TestDevboxScopeFor_FmtOnly(t *testing.T) {
	cases := []struct {
		name string
		item *store.BacklogItem
	}{
		{"nil item", nil},
		{"canary fixture", &store.BacklogItem{ID: "MILLS-CANARY-X", Labels: []string{"mills-canary", "safe-fixture"}}},
		{"debt code item", &store.BacklogItem{ID: "MILLS-DEBT-X", Labels: []string{"debt"}}},
		{"unlabeled item", &store.BacklogItem{ID: "ITEM-X"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := devboxScopeFor(tc.item)
			if len(got) != 1 || got[0] != "fmt" {
				t.Fatalf("devboxScopeFor(%s) = %v, want [fmt] — the sandbox lint/test gate false-fails; CI is the authoritative gate", tc.name, got)
			}
		})
	}
	// Lock the underlying constant so widening the scope is a deliberate,
	// reviewed edit rather than an accidental regression.
	if len(gitCloneTestsScope) != 1 || gitCloneTestsScope[0] != "fmt" {
		t.Fatalf("gitCloneTestsScope = %v, want [fmt]", gitCloneTestsScope)
	}
}

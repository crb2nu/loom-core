package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
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

type fakeAdoptionProbe struct {
	out   *StageOutput
	err   error
	calls int
}

type fakeCollisionAdoptionProbe struct {
	fakeAdoptionProbe
	refs    []string
	retired []string
}

func (f *fakeCollisionAdoptionProbe) PreflightRefCollisions(_ context.Context, _ string, contract string, item *store.BacklogItem, hasOpenMR func(string) (bool, error)) ([]string, error) {
	for _, collision := range BranchRefCollisions(contract, f.refs, item) {
		if !collision.SameItemLegacyForm {
			return nil, fmt.Errorf("foreign collision %s", collision.Ref)
		}
		open, err := hasOpenMR(collision.Ref)
		if err != nil {
			return nil, err
		}
		if open {
			return nil, fmt.Errorf("open MR owns %s", collision.Ref)
		}
		f.retired = append(f.retired, collision.Ref)
	}
	return slices.Clone(f.retired), nil
}

type fakeBranchComparer struct {
	out    StageOutput
	exists bool
	err    error
	calls  [][2]string
}

func (f *fakeBranchComparer) CompareBranch(_ context.Context, baseBranch, branch string) (StageOutput, bool, error) {
	f.calls = append(f.calls, [2]string{baseBranch, branch})
	return f.out, f.exists, f.err
}

func (f *fakeAdoptionProbe) Probe(context.Context, string, string, string) (*StageOutput, error) {
	f.calls++
	return f.out, f.err
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
		return f.resp, f.err
	}
	return f.resp, nil
}

type fakeDevbox struct {
	calls []DevboxRequest
	resp  DevboxResponse
	// respQueue, when non-empty, is consumed one response per call before
	// falling back to resp (two-call oracle tests).
	respQueue []DevboxResponse
	err       error
}

func (f *fakeDevbox) QualityGate(_ context.Context, req DevboxRequest) (DevboxResponse, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return DevboxResponse{}, f.err
	}
	resp := f.resp
	if len(f.respQueue) > 0 {
		resp = f.respQueue[0]
		f.respQueue = f.respQueue[1:]
	}
	if resp.TestedSHA == "" {
		resp.TestedSHA = req.Env["LOOM_MILLS_EXPECTED_SHA"]
	}
	return resp, nil
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

func (f *fakeGitLab) BranchHeadSHA(context.Context, string, string) (string, error) {
	return "", errors.New("branch head unavailable")
}

type branchLookupGitLab struct {
	*fakeGitLab
	branchSHA string
	branchOK  bool
	branchErr error
	lookups   []string
}

type headGitLab struct {
	*fakeGitLab
	sha      string
	err      error
	projects []string
	branches []string
}

func (f *headGitLab) BranchHeadSHA(_ context.Context, project, branch string) (string, error) {
	f.projects = append(f.projects, project)
	f.branches = append(f.branches, branch)
	return f.sha, f.err
}

func (f *branchLookupGitLab) GetBranch(_ context.Context, branch string) (string, bool, error) {
	f.lookups = append(f.lookups, branch)
	return f.branchSHA, f.branchOK, f.branchErr
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
	jc.Env["LOOM_MILLS_EXPECTED_SHA"] = "1111111111111111111111111111111111111111"
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
		"LOOM_MILLS_ATTEMPT":     "0",
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

func TestSpawnWorker_ImplementBranchAdoptionDecision(t *testing.T) {
	tests := []struct {
		name      string
		probe     *fakeAdoptionProbe
		wantSpawn int
		wantErr   bool
		wantAdopt string
	}{
		{name: "missing branch", probe: &fakeAdoptionProbe{}, wantSpawn: 1},
		{name: "empty diff", probe: &fakeAdoptionProbe{}, wantSpawn: 1},
		{name: "nonempty diff", probe: &fakeAdoptionProbe{out: &StageOutput{FilesChanged: []string{"done.go"}, DiffPatch: []byte("diff"), Artifacts: map[string]any{"adopted_branch": "mills/BL-X/implement"}}}, wantAdopt: "mills/BL-X/implement"},
		{name: "probe failure fails closed", probe: &fakeAdoptionProbe{err: errors.New("origin unavailable")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-new"}}
			w := &SpawnWorker{Client: sp, AdoptionProbe: tt.probe, BaseBranch: "main"}
			out, err := w.Run(context.Background(), sampleJobContext("implement"))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(sp.calls) != tt.wantSpawn {
				t.Fatalf("spawn calls = %d, want %d", len(sp.calls), tt.wantSpawn)
			}
			if tt.wantAdopt != "" && out.Artifacts["adopted_branch"] != tt.wantAdopt {
				t.Fatalf("adopted_branch = %v", out.Artifacts["adopted_branch"])
			}
		})
	}
}

func TestSpawnWorker_ImplementRefCollisionPreflight(t *testing.T) {
	nestedJob := func() JobContext {
		return sampleJobContext("implement", func(jc *JobContext) {
			jc.Item.Slices = []store.Slice{{Name: "api"}}
			jc.Env = BuildMillsEnv(jc.Run, jc.Item, jc.Stage)
		})
	}
	t.Run("retires stale same-item legacy ref records event and spawns contract branch", func(t *testing.T) {
		probe := &fakeCollisionAdoptionProbe{refs: []string{"feat/BL-X"}}
		spawn := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-1"}}
		var events []string
		worker := &SpawnWorker{
			Client: spawn, AdoptionProbe: probe,
			OpenMRForBranch: func(context.Context, string, string) (bool, error) { return false, nil },
			RecordBranchRefRetired: func(_ context.Context, _, contract, ref string) error {
				events = append(events, contract+"<-"+ref)
				return nil
			},
		}
		if _, err := worker.Run(context.Background(), nestedJob()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(spawn.calls) != 1 || spawn.calls[0].Branch != "feat/BL-X/api" {
			t.Fatalf("spawn calls = %#v, want canonical contract branch", spawn.calls)
		}
		if !reflect.DeepEqual(events, []string{"feat/BL-X/api<-feat/BL-X"}) {
			t.Fatalf("events = %#v", events)
		}
	})

	for _, tc := range []struct {
		name string
		refs []string
		open bool
	}{
		{"foreign collision", []string{"feat/BL-X/api/backup"}, false},
		{"open MR owns legacy", []string{"feat/BL-X"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawn := &fakeSpawn{}
			worker := &SpawnWorker{Client: spawn, AdoptionProbe: &fakeCollisionAdoptionProbe{refs: tc.refs}, OpenMRForBranch: func(context.Context, string, string) (bool, error) { return tc.open, nil }}
			_, err := worker.Run(context.Background(), nestedJob())
			if err == nil || !strings.Contains(err.Error(), "[branch_contract.ref_collision]") {
				t.Fatalf("error = %v, want collision reason", err)
			}
			if got := Classify(err); got != ClassConfig {
				t.Fatalf("error class = %s, want terminal config", got)
			}
			if len(spawn.calls) != 0 {
				t.Fatalf("spawn calls = %d, want zero", len(spawn.calls))
			}
		})
	}
}

func TestGitBranchAdoptionProbe_UnresolvedDeepensOnceThenDegrades(t *testing.T) {
	for _, wording := range []string{"no merge base", "unknown revision origin/main...origin/feat/BL-X"} {
		t.Run(wording, func(t *testing.T) {
			var deepen, mergeBases int
			probe := GitBranchAdoptionProbe{runGit: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(joined, "ls-remote"):
					return []byte("abc123 refs/heads/feat/BL-X\n"), nil
				case strings.HasPrefix(joined, "fetch --no-tags"):
					return nil, nil
				case joined == "rev-parse --is-shallow-repository":
					return []byte("true\n"), nil
				case strings.HasPrefix(joined, "fetch --deepen=2000"):
					deepen++
					return nil, nil
				case strings.HasPrefix(joined, "merge-base"):
					mergeBases++
					return nil, fmt.Errorf("git merge-base: exit status 128: %s", wording)
				default:
					t.Fatalf("unexpected git command %q", joined)
					return nil, nil
				}
			}}
			out, err := probe.Probe(context.Background(), "/repo", "main", "feat/BL-X")
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if deepen != 1 || mergeBases != 2 {
				t.Fatalf("deepen=%d mergeBases=%d, want one remediation and one retry", deepen, mergeBases)
			}
			if _, ok := out.Artifacts[AdoptionProbeUnresolvedArtifact]; !ok {
				t.Fatalf("missing unresolved artifact: %#v", out.Artifacts)
			}
		})
	}
}

func TestGitBranchAdoptionProbe_RefCollisionPreflight(t *testing.T) {
	t.Run("deletes stale legacy ref", func(t *testing.T) {
		var commands []string
		probe := GitBranchAdoptionProbe{runGit: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			command := strings.Join(args, " ")
			commands = append(commands, command)
			switch command {
			case "ls-remote --refs --heads origin":
				return []byte("abc refs/heads/feat/BL-X\ndef refs/heads/main\n"), nil
			case "push origin :refs/heads/feat/BL-X":
				return nil, nil
			default:
				return nil, fmt.Errorf("unexpected command %q", command)
			}
		}}
		retired, err := probe.PreflightRefCollisions(context.Background(), "/repo", "feat/BL-X/api", &store.BacklogItem{ID: "BL-X"}, func(ref string) (bool, error) {
			if ref != "feat/BL-X" {
				t.Fatalf("MR lookup ref = %q", ref)
			}
			return false, nil
		})
		if err != nil {
			t.Fatalf("PreflightRefCollisions: %v", err)
		}
		if !reflect.DeepEqual(retired, []string{"feat/BL-X"}) {
			t.Fatalf("retired = %#v", retired)
		}
		if len(commands) != 2 {
			t.Fatalf("commands = %#v", commands)
		}
	})

	t.Run("foreign collision never pushes", func(t *testing.T) {
		var pushes int
		probe := GitBranchAdoptionProbe{runGit: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "push" {
				pushes++
			}
			return []byte("abc refs/heads/feat/BL-X/api/backup\n"), nil
		}}
		_, err := probe.PreflightRefCollisions(context.Background(), "/repo", "feat/BL-X/api", &store.BacklogItem{ID: "BL-X"}, func(string) (bool, error) {
			t.Fatal("foreign collision must not query MR ownership")
			return false, nil
		})
		if err == nil || pushes != 0 {
			t.Fatalf("err = %v, pushes = %d", err, pushes)
		}
	})
}

// A working directory with no origin (bare E2E scratch dirs, synthetic spawn
// worktrees) makes adoption impossible, not ambiguous: the probe must degrade
// to a normal implementation instead of erroring the stage.
func TestGitBranchAdoptionProbe_NotAGitRepoDegrades(t *testing.T) {
	probe := GitBranchAdoptionProbe{runGit: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.HasPrefix(strings.Join(args, " "), "ls-remote") {
			return nil, errors.New("git ls-remote --refs origin refs/heads/feat/BL-X: exit status 128: fatal: 'origin' does not appear to be a git repository")
		}
		t.Fatalf("unexpected git command %q", strings.Join(args, " "))
		return nil, nil
	}}
	out, err := probe.Probe(context.Background(), "/tmp/scratch", "main", "feat/BL-X")
	if err != nil {
		t.Fatalf("probe must degrade, got error: %v", err)
	}
	if out == nil {
		t.Fatal("probe must return the unresolved degrade output, got nil")
	}
	artifact, ok := out.Artifacts[AdoptionProbeUnresolvedArtifact].(map[string]any)
	if !ok {
		t.Fatalf("missing unresolved artifact: %#v", out.Artifacts)
	}
	if artifact["remediation"] != "workdir_not_git_repository" {
		t.Fatalf("remediation = %v, want workdir_not_git_repository", artifact["remediation"])
	}
}

func TestGitBranchAdoptionProbe_DeepenRetryCanAdopt(t *testing.T) {
	var mergeBases int
	probe := GitBranchAdoptionProbe{runGit: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "ls-remote"):
			return []byte("abc123 refs/heads/feat/BL-X\n"), nil
		case strings.HasPrefix(joined, "fetch"):
			return nil, nil
		case joined == "rev-parse --is-shallow-repository":
			return []byte("false\n"), nil
		case strings.HasPrefix(joined, "merge-base"):
			mergeBases++
			if mergeBases == 1 {
				return nil, errors.New("git merge-base: exit status 128: no merge base")
			}
			return []byte("base123\n"), nil
		case strings.HasPrefix(joined, "diff --name-only"):
			return []byte("fixed.go\n"), nil
		case strings.HasPrefix(joined, "diff --no-ext-diff"):
			return []byte("patch"), nil
		case strings.HasPrefix(joined, "diff --numstat"):
			return []byte("1\t0\tfixed.go\n"), nil
		case strings.HasPrefix(joined, "log --format=%s"):
			return []byte("fix: existing work\n"), nil
		default:
			t.Fatalf("unexpected git command %q", joined)
			return nil, nil
		}
	}}
	out, err := probe.Probe(context.Background(), "/repo", "main", "feat/BL-X")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := out.Artifacts["adopted_branch"]; got != "feat/BL-X" {
		t.Fatalf("adopted_branch = %v", got)
	}
	if _, unresolved := out.Artifacts[AdoptionProbeUnresolvedArtifact]; unresolved {
		t.Fatalf("successful retry marked unresolved: %#v", out.Artifacts)
	}
}

func TestGitBranchAdoptionProbe_StaleShallowForkNeverLeaksMainPaths(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitTestRun(t, "", nil, "init", "--bare", remote)
	var stream strings.Builder
	stream.WriteString("blob\nmark :1\ndata 5\nroot\ncommit refs/heads/main\nmark :2\nauthor Test <test@example.com> 1 +0000\ncommitter Test <test@example.com> 1 +0000\ndata 4\nroot\nM 100644 :1 shared.txt\n\n")
	stream.WriteString("blob\nmark :3\ndata 7\nbranch\ncommit refs/heads/feat/stale\nmark :4\nauthor Test <test@example.com> 2 +0000\ncommitter Test <test@example.com> 2 +0000\ndata 6\nbranch\nfrom :2\nM 100644 :3 branch-only.txt\n\n")
	parent, mark := 2, 5
	for i := 0; i < 2005; i++ {
		blob, commit := mark, mark+1
		fmt.Fprintf(&stream, "blob\nmark :%d\ndata %d\nmain-%d\ncommit refs/heads/main\nmark :%d\nauthor Test <test@example.com> %d +0000\ncommitter Test <test@example.com> %d +0000\ndata 4\nmain\nfrom :%d\nM 100644 :%d main-only.txt\n\n", blob, len(fmt.Sprintf("main-%d\n", i)), i, commit, i+3, i+3, parent, blob)
		parent, mark = commit, mark+2
	}
	gitTestRun(t, remote, strings.NewReader(stream.String()), "fast-import", "--quiet")
	clone := filepath.Join(t.TempDir(), "clone")
	gitTestRun(t, "", nil, "clone", "--depth=1", "--branch=main", "file://"+remote, clone)
	out, err := (GitBranchAdoptionProbe{}).Probe(context.Background(), clone, "main", "feat/stale")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if out == nil || out.Artifacts[AdoptionProbeUnresolvedArtifact] == nil {
		t.Fatalf("stale shallow fork must degrade explicitly: %#v", out)
	}
	if len(out.FilesChanged) != 0 || out.LinesAdded != 0 || out.LinesRemoved != 0 || len(out.DiffPatch) != 0 {
		t.Fatalf("degraded adoption leaked gate evidence: files=%v +%d/-%d patch=%q", out.FilesChanged, out.LinesAdded, out.LinesRemoved, out.DiffPatch)
	}
}

func gitTestRun(t *testing.T, dir string, stdin *strings.Reader, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestSpawnWorker_UnresolvedAdoptionProbeContinuesSpawn(t *testing.T) {
	probe := &fakeAdoptionProbe{out: &StageOutput{Artifacts: map[string]any{
		AdoptionProbeUnresolvedArtifact: map[string]any{"error": "no merge base"},
	}}}
	spawn := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-new"}}
	out, err := (&SpawnWorker{Client: spawn, AdoptionProbe: probe}).Run(context.Background(), sampleJobContext("implement"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(spawn.calls))
	}
	if _, ok := out.Artifacts[AdoptionProbeUnresolvedArtifact]; !ok {
		t.Fatalf("unresolved evidence not carried into spawn output: %#v", out.Artifacts)
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

func TestSpawnWorker_RemoteBranchDiffFallback(t *testing.T) {
	captureUnavailable := map[string]any{
		GitCaptureArtifactKey: map[string]any{
			"status": "skipped_no_capture_context", "reason": "capture coordinates unset: working_dir",
		},
	}
	tests := []struct {
		name       string
		compare    *fakeBranchComparer
		wantFiles  []string
		wantStatus string
		wantReason string
	}{
		{
			name: "differing remote branch populates cumulative evidence",
			compare: &fakeBranchComparer{exists: true, out: StageOutput{
				FilesChanged: []string{"pkg/mills/pipeline/dispatcher.go"}, DiffPatch: []byte("diff --git a/x b/x\n+fallback\n"),
				LinesAdded: 1, CommitMessages: []string{"fix(mills): compare remote branch"},
			}},
			wantFiles: []string{"pkg/mills/pipeline/dispatcher.go"}, wantStatus: "captured",
		},
		{
			name:    "missing remote branch preserves capture unavailable",
			compare: &fakeBranchComparer{exists: false}, wantStatus: "skipped_no_capture_context",
			wantReason: "capture coordinates unset: working_dir",
		},
		{
			name:    "compare failure remains fail closed",
			compare: &fakeBranchComparer{err: errors.New("gitlab unavailable")}, wantStatus: "skipped_no_capture_context",
			wantReason: "remote branch compare failed: gitlab unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-1", Artifacts: cloneArtifacts(captureUnavailable)}}
			w := &SpawnWorker{
				Client: sp,
				CompareForProject: func(project string) BranchCompareClient {
					if project != "loom-core" {
						t.Fatalf("compare project = %q", project)
					}
					return tt.compare
				},
			}
			jc := sampleJobContext("implement", func(jc *JobContext) { jc.Run.WorktreePath = "" })
			out, err := w.Run(context.Background(), jc)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !reflect.DeepEqual(out.FilesChanged, tt.wantFiles) {
				t.Fatalf("FilesChanged = %v, want %v", out.FilesChanged, tt.wantFiles)
			}
			status, reason := gitCaptureFromArtifacts(out.Artifacts)
			if status != tt.wantStatus || (tt.wantReason != "" && reason != tt.wantReason) {
				t.Fatalf("capture = (%q, %q), want (%q, %q)", status, reason, tt.wantStatus, tt.wantReason)
			}
			if len(tt.compare.calls) != 1 || tt.compare.calls[0] != [2]string{"main", "feat/BL-X"} {
				t.Fatalf("compare calls = %v", tt.compare.calls)
			}
			outcome, gateErr := (&gates.NonEmptyDiff{}).Evaluate(context.Background(), gates.StageInput{
				FilesChanged: out.FilesChanged, DiffPatch: out.DiffPatch, GitCaptureStatus: status, GitCaptureReason: reason,
			})
			if gateErr != nil {
				t.Fatalf("nonempty_diff: %v", gateErr)
			}
			if len(tt.wantFiles) > 0 && !outcome.Pass {
				t.Fatalf("nonempty_diff failed after remote compare: %v", outcome.Reasons)
			}
			if len(tt.wantFiles) == 0 && outcome.Pass {
				t.Fatal("nonempty_diff passed without observable branch evidence")
			}
		})
	}
}

func cloneArtifacts(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for key, value := range src {
		if nested, ok := value.(map[string]any); ok {
			copyNested := make(map[string]any, len(nested))
			for nestedKey, nestedValue := range nested {
				copyNested[nestedKey] = nestedValue
			}
			out[key] = copyNested
			continue
		}
		out[key] = value
	}
	return out
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

func TestWeaverWorker_FailurePreservesResearchProvenance(t *testing.T) {
	wv := &fakeWeaver{
		resp: WeaverResponse{
			CostUSD: 0.17, LogTail: "POST https://models.invalid/v1/chat: status 404",
			Model: "research-model", Backend: "flexinfer",
			Citation: map[string]any{"source": "dispatcher.go"},
			Usage:    llmusage.Usage{PromptTokens: 1200, CompletionTokens: 80},
		},
		err: errors.New("flexinfer chat: status 404: model endpoint not found"),
	}
	out, err := (&WeaverWorker{Client: wv}).Run(context.Background(), sampleJobContext("research"))
	if err == nil {
		t.Fatal("expected research failure")
	}
	if out.Model != "research-model" || out.Backend != "flexinfer" || out.CostUSD != 0.17 {
		t.Fatalf("failure metadata lost: %+v", out)
	}
	if out.Artifacts[researchPromptTokensArtifactKey] != 1200 || out.Artifacts[researchCompletionTokensArtifactKey] != 80 {
		t.Fatalf("failure token artifacts lost: %+v", out.Artifacts)
	}
	if out.Artifacts["citation"] == nil || !strings.Contains(out.LogTail, "status 404") {
		t.Fatalf("failure evidence lost: %+v", out)
	}
}

func TestDevboxWorker_RealVerdictReturnsFailedOutput(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{
		Passed:  false,
		CostUSD: 0.05,
		Checks:  []DevboxCheck{{Name: "lint", Passed: false, ExitCode: 1, Output: "boom"}},
	}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	out, err := w.Run(context.Background(), sampleJobContext("tests"))
	if err != nil {
		t.Fatalf("real verdict returned error: %v", err)
	}
	if out.CostUSD != 0.05 {
		t.Errorf("cost not propagated on fail: %v", out.CostUSD)
	}
	if len(db.calls) != 1 {
		t.Errorf("devbox calls = %d", len(db.calls))
	}
	if out.Artifacts["passed"] != false || out.Artifacts["checks"] == nil || out.Artifacts["failed_summary"] == "" {
		t.Fatalf("failed artifacts = %+v", out.Artifacts)
	}
}

func TestDevboxWorker_LintsOnlyTouchedPackagesBeforeDeclaredTests(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "fmt", Passed: true}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Prior["implement"] = StageOutput{FilesChanged: []string{"pkg/z/z.go", "README.md", "pkg/a/a_test.go", "pkg/a/other.go"}}
		jc.Item.Success.Tests = []string{"GOWORK=off go test ./pkg/mills/pipeline/..."}
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatal(err)
	}
	got := db.calls[0]
	wantPackages := []string{"./pkg/a", "./pkg/z"}
	if !reflect.DeepEqual(got.LintPackages, wantPackages) {
		t.Fatalf("lint packages = %v, want %v", got.LintPackages, wantPackages)
	}
	if len(got.TestCommands) != 3 || got.TestCommands[0] != "CGO_ENABLED=0 GOWORK=off golangci-lint run --config .golangci.yml --allow-parallel-runners './pkg/a' './pkg/z' 2>&1" || got.TestCommands[2] != "GOWORK=off go test -count=1 './pkg/a' './pkg/z'" {
		t.Fatalf("test commands = %v", got.TestCommands)
	}
}

func TestDevboxWorker_AppendsTouchedPackageTests(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:1", Passed: true, Output: "ok"}}}}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Prior["implement"] = StageOutput{FilesChanged: []string{"pkg/mills/pipeline/dispatcher.go"}}
	})
	out, err := (&DevboxWorker{Client: db}).Run(context.Background(), jc)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := "GOWORK=off go test -count=1 './pkg/mills/pipeline'"
	if got := db.calls[0].TestCommands; len(got) != 2 || got[1] != wantCommand {
		t.Fatalf("test commands = %v", got)
	}
	if got := out.Artifacts[touchedTestPackagesArtifactKey]; !reflect.DeepEqual(got, []string{"./pkg/mills/pipeline"}) {
		t.Fatalf("touched packages artifact = %#v", got)
	}
	checks := out.Artifacts["checks"].([]DevboxCheck)
	if !strings.HasPrefix(checks[0].Output, "Command: "+wantCommand+"\n") {
		t.Fatalf("check output = %q", checks[0].Output)
	}
}

func TestDevboxWorker_DeclaredTestCoveringTouchedPackageIsNotDuplicated(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "fmt", Passed: true}}}}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Prior["implement"] = StageOutput{FilesChanged: []string{"pkg/mills/pipeline/dispatcher.go"}}
		jc.Item.Success.Tests = []string{"go test ./pkg/mills/..."}
	})
	out, err := (&DevboxWorker{Client: db}).Run(context.Background(), jc)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.calls[0].TestCommands; len(got) != 2 || got[1] != "GOWORK=off go test -count=1 ./pkg/mills/..." {
		t.Fatalf("test commands = %v", got)
	}
	if got := out.Artifacts[touchedTestPackagesArtifactKey]; len(got.([]string)) != 0 {
		t.Fatalf("touched packages artifact = %#v", got)
	}
}

func TestDevboxWorker_NonGoChangeAddsNoTouchedPackageTest(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "fmt", Passed: true}}}}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Prior["implement"] = StageOutput{FilesChanged: []string{"docs/MILLS.md"}}
	})
	out, err := (&DevboxWorker{Client: db}).Run(context.Background(), jc)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.calls[0].TestCommands; len(got) != 0 {
		t.Fatalf("test commands = %v", got)
	}
	if got := out.Artifacts[touchedTestPackagesArtifactKey]; len(got.([]string)) != 0 {
		t.Fatalf("touched packages artifact = %#v", got)
	}
}

func TestDevboxWorker_CollapsesMoreThanTwentyFiveTouchedPackages(t *testing.T) {
	changed := make([]string, 26)
	for i := range changed {
		changed[i] = fmt.Sprintf("pkg/mills/p%02d/file.go", i)
	}
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:1", Passed: true}}}}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Prior["implement"] = StageOutput{FilesChanged: changed}
	})
	out, err := (&DevboxWorker{Client: db}).Run(context.Background(), jc)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := "GOWORK=off go test -count=1 './pkg/mills/...'"
	if got := db.calls[0].TestCommands; len(got) != 2 || got[1] != wantCommand {
		t.Fatalf("test commands = %v", got)
	}
	if got := out.Artifacts[touchedTestPackagesArtifactKey]; !reflect.DeepEqual(got, []string{"./pkg/mills/..."}) {
		t.Fatalf("touched packages artifact = %#v", got)
	}
	checks := out.Artifacts["checks"].([]DevboxCheck)
	if !strings.HasPrefix(checks[0].Output, "Command: "+wantCommand+"\n") {
		t.Fatalf("check output = %q", checks[0].Output)
	}
}

func TestTouchedPackageTestCommand_QuotesPackagePaths(t *testing.T) {
	command, effective := touchedPackageTestCommand([]string{"./pkg/it's complicated"}, nil)
	if want := "GOWORK=off go test -count=1 './pkg/it'\"'\"'s complicated'"; command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}
	if !reflect.DeepEqual(effective, []string{"./pkg/it's complicated"}) {
		t.Fatalf("effective packages = %#v", effective)
	}
}

func TestDevboxWorker_ForwardsBranchHeadAndStampsTestedSHA(t *testing.T) {
	const sha = "2222222222222222222222222222222222222222"
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:0", Passed: true}}, TestedSHA: sha}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "mills"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Env["LOOM_MILLS_EXPECTED_SHA"] = sha
		jc.Item.Success.Tests = []string{"GOWORK=off go test ./pkg/mills/pipeline/..."}
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatal(err)
	}
	got := db.calls[0]
	if got.Env["LOOM_MILLS_BRANCH"] != "feat/BL-X" || got.Env["LOOM_MILLS_EXPECTED_SHA"] != sha {
		t.Fatalf("checkout coordinates = %#v", got.Env)
	}
	if got.AgentID != devboxAgentID("mills", "PIPE-X-1", "") {
		t.Fatalf("agent id = %q, want bounded per-run sandbox", got.AgentID)
	}
	if out.Artifacts["tested_sha"] != sha {
		t.Fatalf("tested_sha artifact = %v", out.Artifacts["tested_sha"])
	}
}

func TestSpawnWorker_ReviewReportsPushedCommitsAndResultingHead(t *testing.T) {
	w := &SpawnWorker{ResolveBranchHead: func(_ context.Context, project, branch string) (string, error) {
		if project != "services/loom-core" || branch == "" {
			t.Fatalf("resolver coordinates = %q %q", project, branch)
		}
		return "bbbbbbbb", nil
	}}
	jc := JobContext{Stage: Stage{ID: "pr_self_review"}, Prior: map[string]StageOutput{
		"implement": {CommitMessages: []string{"feat: implementation"}},
	}}
	out, err := w.withReviewPushArtifact(context.Background(), jc, "services/loom-core", "feat/item", StageOutput{
		CommitMessages: []string{"feat: implementation", "fix: review finding"},
	}, nil)
	if err != nil {
		t.Fatalf("withReviewPushArtifact: %v", err)
	}
	pushed := out.Artifacts["pushed_commits"].(map[string]any)
	if pushed["count"] != 1 || pushed["head_sha"] != "bbbbbbbb" {
		t.Fatalf("pushed_commits = %#v", pushed)
	}
}

func TestSpawnWorker_ReviewHeadUnresolvedIsRecordedNotFatal(t *testing.T) {
	prev := reviewHeadResolveBackoff
	reviewHeadResolveBackoff = 0
	t.Cleanup(func() { reviewHeadResolveBackoff = prev })
	calls := 0
	w := &SpawnWorker{ResolveBranchHead: func(context.Context, string, string) (string, error) {
		calls++
		return "", errors.New("gitlab: resolve branch head: status 502")
	}}
	jc := JobContext{Stage: Stage{ID: "pr_self_review"}}
	out, err := w.withReviewPushArtifact(context.Background(), jc, "services/loom-core", "feat/item", StageOutput{}, nil)
	if err != nil {
		t.Fatalf("a head lookup failure must not fail the finished review stage: %v", err)
	}
	pushed := out.Artifacts["pushed_commits"].(map[string]any)
	if pushed["head_sha"] != "" || pushed["count"] != 0 {
		t.Fatalf("pushed_commits = %#v, want an unresolved head", pushed)
	}
	if msg, _ := pushed["head_error"].(string); !strings.Contains(msg, "502") {
		t.Fatalf("head_error = %q, want the GitLab error", msg)
	}
	if calls != reviewHeadResolveAttempts {
		t.Fatalf("resolve attempts = %d, want %d", calls, reviewHeadResolveAttempts)
	}
}

func TestSpawnWorker_ReviewArtifactOnlyForReviewStage(t *testing.T) {
	w := &SpawnWorker{ResolveBranchHead: func(context.Context, string, string) (string, error) {
		t.Error("implement must not resolve a review head")
		return "", nil
	}}
	jc := JobContext{Stage: Stage{ID: "implement"}}
	out, err := w.withReviewPushArtifact(context.Background(), jc, "services/loom-core", "feat/item", StageOutput{}, nil)
	if err != nil || out.Artifacts != nil {
		t.Fatalf("out=%+v err=%v, want untouched output", out, err)
	}
}

func TestDevboxWorker_PinsPostReviewRetestToReviewHead(t *testing.T) {
	const adopted = "1111111111111111111111111111111111111111"
	const reviewHead = "2222222222222222222222222222222222222222"
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:0", Passed: true}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "mills"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		delete(jc.Env, "LOOM_MILLS_EXPECTED_SHA")
		jc.Item.Success.Tests = []string{"GOWORK=off go test ./pkg/mills/pipeline/..."}
		jc.Prior["implement"] = StageOutput{Artifacts: map[string]any{"adopted_head_sha": adopted}}
		jc.RetryContext = &StageRetryContext{GateStage: "post_review_gate", ExpectedHeadSHA: reviewHead}
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.calls[0].Env["LOOM_MILLS_EXPECTED_SHA"]; got != reviewHead {
		t.Fatalf("expected sha = %q, want the review head %q over the adopted pin %q", got, reviewHead, adopted)
	}
	if out.Artifacts["tested_sha"] != reviewHead {
		t.Fatalf("tested_sha artifact = %v, want %s", out.Artifacts["tested_sha"], reviewHead)
	}
}

func TestDevboxWorker_ResolvesSpawnHeadFromTargetGitLabProject(t *testing.T) {
	const sha = "2222222222222222222222222222222222222222"
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:0", Passed: true}}}}
	gl := &headGitLab{fakeGitLab: &fakeGitLab{}, sha: sha}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Env["LOOM_MILLS_EXPECTED_SHA"] = ""
		jc.Run.WorktreePath = ""
		jc.Item.TargetProject = "libs/fi-fhir"
	})
	if _, err := (&DevboxWorker{Client: db, GitLab: gl, Project: "services/loom-core"}).Run(context.Background(), jc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gl.projects, []string{"libs/fi-fhir"}) || !reflect.DeepEqual(gl.branches, []string{"feat/BL-X"}) {
		t.Fatalf("lookups = projects %v branches %v", gl.projects, gl.branches)
	}
	if len(db.calls) != 1 || db.calls[0].Env["LOOM_MILLS_BRANCH"] != "feat/BL-X" || db.calls[0].Env["LOOM_MILLS_EXPECTED_SHA"] != sha {
		t.Fatalf("request = %#v", db.calls)
	}
}

func TestDevboxWorker_AdoptedHeadWinsOverGitLab(t *testing.T) {
	const adopted = "3333333333333333333333333333333333333333"
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:0", Passed: true}}}}
	gl := &headGitLab{fakeGitLab: &fakeGitLab{}, sha: "4444444444444444444444444444444444444444"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Env["LOOM_MILLS_EXPECTED_SHA"] = ""
		jc.Prior["implement"] = StageOutput{Artifacts: map[string]any{"adopted_head_sha": adopted}}
	})
	if _, err := (&DevboxWorker{Client: db, GitLab: gl}).Run(context.Background(), jc); err != nil {
		t.Fatal(err)
	}
	if len(gl.branches) != 0 || db.calls[0].Env["LOOM_MILLS_EXPECTED_SHA"] != adopted {
		t.Fatalf("gitlab lookups = %v, request = %#v", gl.branches, db.calls[0])
	}
}

func TestDevboxWorker_TestedSHAMismatchIsInfra(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, TestedSHA: "3333333333333333333333333333333333333333"}}
	jc := sampleJobContext("tests")
	_, err := (&DevboxWorker{Client: db}).Run(context.Background(), jc)
	if !errors.Is(err, ErrDevboxCheckoutInfra) {
		t.Fatalf("error = %v", err)
	}
	if got := Classify(err); got != ClassInfra {
		t.Fatalf("class = %s, want infra", got)
	}
}

func TestDevboxWorker_LintFailureSurfacesOutputAndWarningPassIsPreserved(t *testing.T) {
	jc := sampleJobContext("tests", func(jc *JobContext) { jc.Prior["implement"] = StageOutput{FilesChanged: []string{"pkg/a/a.go"}} })
	db := &fakeDevbox{resp: DevboxResponse{Passed: false, LogTail: "lint failed", Checks: []DevboxCheck{{Name: gates.LintParityCheckName, Passed: false, ExitCode: 1, Output: "QueryRow must be QueryRowContext (noctx)"}}}}
	out, err := (&DevboxWorker{Client: db}).Run(context.Background(), jc)
	if err != nil || !strings.Contains(out.LogTail, "noctx") {
		t.Fatalf("out=%+v err=%v", out, err)
	}

	checks := out.Artifacts["checks"].([]DevboxCheck)
	if len(checks) != 1 || !strings.Contains(checks[0].Output, "noctx") || checks[0].Degraded {
		t.Fatalf("finding not preserved in artifact: %+v", checks)
	}

	warning := "golangci-lint unavailable; CI-parity lint skipped"
	db.resp = DevboxResponse{Passed: true, LogTail: warning, Checks: []DevboxCheck{{Name: gates.LintParityCheckName, Passed: true, Output: warning}}}
	out, err = (&DevboxWorker{Client: db}).Run(context.Background(), jc)
	if err != nil || !strings.Contains(out.LogTail, warning) {
		t.Fatalf("warning pass out=%+v err=%v", out, err)
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

func TestDevboxWorker_FailureSummaryIsAppendedToLogTail(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream string
	}{
		{name: "populated", upstream: "upstream gate log"},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeDevbox{resp: DevboxResponse{
				Passed:  false,
				LogTail: tc.upstream,
				Checks:  []DevboxCheck{{Name: "test:0", ExitCode: 1, Output: "line one\nline two"}},
			}}
			out, err := (&DevboxWorker{Client: db, Project: "loom-core"}).Run(context.Background(), sampleJobContext("tests"))
			if err != nil {
				t.Fatalf("real verdict returned error: %v", err)
			}
			if tc.upstream != "" && !strings.HasPrefix(out.LogTail, tc.upstream+"\n") {
				t.Fatalf("upstream log not preserved: %q", out.LogTail)
			}
			for _, want := range []string{"test:0[exit=1]", "line one | line two"} {
				if !strings.Contains(out.LogTail, want) {
					t.Errorf("log tail missing %q: %q", want, out.LogTail)
				}
			}
		})
	}
}

func TestDevboxWorker_ExitZeroPhantomIsTransient(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{
		Passed: false,
		Checks: []DevboxCheck{{Name: "test:0", Passed: false, ExitCode: 0}},
	}}
	out, err := (&DevboxWorker{Client: db, Project: "loom-core"}).Run(context.Background(), sampleJobContext("tests"))
	if err == nil || Classify(err) != ClassTransient {
		t.Fatalf("phantom verdict error/class = %v/%s", err, Classify(err))
	}
	if !strings.Contains(out.LogTail, "test:0[exit=0]") {
		t.Fatalf("phantom verdict missing from log: %q", out.LogTail)
	}

	db.resp.Checks[0].Output = "legacy producer attached output"
	if _, err = (&DevboxWorker{Client: db, Project: "loom-core"}).Run(context.Background(), sampleJobContext("tests")); err == nil || Classify(err) != ClassTransient {
		t.Fatalf("exit-zero contradiction with output error/class = %v/%s", err, Classify(err))
	}

	db.resp.Checks[0] = DevboxCheck{Name: "test:0", Passed: true, ExitCode: 0}
	if _, err = (&DevboxWorker{Client: db, Project: "loom-core"}).Run(context.Background(), sampleJobContext("tests")); err == nil || Classify(err) != ClassTransient {
		t.Fatalf("aggregate contradiction error/class = %v/%s", err, Classify(err))
	}

	long := strings.Repeat("x", 5000) + " FINAL ERROR"
	db.resp.Checks[0] = DevboxCheck{Name: "test:0", ExitCode: 1, Output: long}
	out, err = (&DevboxWorker{Client: db, Project: "loom-core"}).Run(context.Background(), sampleJobContext("tests"))
	if err != nil || out.Artifacts["passed"] != false {
		t.Fatalf("ordinary code failure output: %+v, %v", out, err)
	}
	if !strings.Contains(out.LogTail, "FINAL ERROR") || len(out.LogTail) > 2200 {
		t.Fatalf("oversized output not tail-bounded: len=%d tail=%q", len(out.LogTail), out.LogTail)
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
		jc.Env = map[string]string{"GIT_TOKEN": "token", "GOPRIVATE": "gitlab.flexinfer.ai/*", "UNRELATED_SECRET": "drop-me", "LOOM_MILLS_EXPECTED_SHA": "1111111111111111111111111111111111111111"}
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := db.calls[0].TestCommands; !reflect.DeepEqual(got, []string{"GOWORK=off go test -count=1 ./cmd/loom -run Mills", "GOWORK=off go test -count=1 ./pkg/mills/..."}) {
		t.Fatalf("test commands = %v", got)
	}
	if db.calls[0].Env["GIT_TOKEN"] != "token" {
		t.Fatalf("request env not forwarded: %#v", db.calls[0].Env)
	}
	if db.calls[0].Env["GOPRIVATE"] != "gitlab.flexinfer.ai/*" {
		t.Fatalf("GOPRIVATE not forwarded: %#v", db.calls[0].Env)
	}
	if _, ok := db.calls[0].Env["UNRELATED_SECRET"]; ok {
		t.Fatalf("unapproved env forwarded: %#v", db.calls[0].Env)
	}
	if got := out.Artifacts[skippedDeclaredTestsArtifactKey]; !reflect.DeepEqual(got, []string{"make test"}) {
		t.Fatalf("skipped tests artifact = %#v", got)
	}
}

func TestDevboxWorker_ConfiguredGitEnvOverridesJobAndRedactsOutput(t *testing.T) {
	const token = "sentinel-private-token"
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, LogTail: "used " + token, Checks: []DevboxCheck{{Name: "test:0", Passed: true, Output: "url=https://oauth2:" + token + "@gitlab.flexinfer.ai"}}}}
	w := &DevboxWorker{Client: db, GitToken: token, GoPrivate: "gitlab.flexinfer.ai/*"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Env = map[string]string{"GIT_TOKEN": "stale", "GOPRIVATE": "stale.example/*", "LOOM_MILLS_EXPECTED_SHA": "2222222222222222222222222222222222222222"}
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.calls[0].Env; got["GIT_TOKEN"] != token || got["GOPRIVATE"] != "gitlab.flexinfer.ai/*" {
		t.Fatalf("request env = %#v", got)
	}
	if strings.Contains(out.LogTail, token) || strings.Contains(out.Artifacts["checks"].([]DevboxCheck)[0].Output, token) {
		t.Fatal("token leaked through tests-stage output")
	}
}

// The workspace convention declares tests as "GOWORK=off go test ./pkg/..." —
// entries with leading env assignments must execute (they previously fell to
// the skip bucket), and bare `go test` entries must be normalized to GOWORK=off
// so the sandbox's go.work (whose toolchain floor the sandbox Go may not meet)
// never resolves. Regression for the 2026-08-14 instant "FAIL test:0" class.
func TestDeclaredDevboxTests_EnvPrefixesExecuteAndBareGetsGoworkOff(t *testing.T) {
	item := &store.BacklogItem{Success: store.SuccessCriteria{Tests: []string{
		"GOWORK=off go test ./pkg/a/...",
		"FOO=1 GOWORK=off go test ./pkg/b/...",
		"FOO=1 go test ./pkg/c/...",
		"go test ./pkg/d/...",
		"make test",
		"gotestsum ./...",
	}}}
	allowed, skipped := declaredDevboxTests(item)
	wantAllowed := []string{
		"GOWORK=off go test -count=1 ./pkg/a/...",
		"FOO=1 GOWORK=off go test -count=1 ./pkg/b/...",
		"GOWORK=off FOO=1 go test -count=1 ./pkg/c/...",
		"GOWORK=off go test -count=1 ./pkg/d/...",
	}
	if !reflect.DeepEqual(allowed, wantAllowed) {
		t.Fatalf("allowed = %#v, want %#v", allowed, wantAllowed)
	}
	if !reflect.DeepEqual(skipped, []string{"make test", "gotestsum ./..."}) {
		t.Fatalf("skipped = %#v", skipped)
	}
}

func TestNormalizeDeclaredGoTest_CountOneInjection(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
		ok      bool
	}{
		{name: "bare", command: "go test ./...", want: "go test -count=1 ./...", ok: true},
		{name: "single env", command: "CGO_ENABLED=0 go test ./...", want: "CGO_ENABLED=0 go test -count=1 ./...", ok: true},
		{name: "multiple env", command: "FOO=a BAR=b go test ./pkg/...", want: "FOO=a BAR=b go test -count=1 ./pkg/...", ok: true},
		{name: "already present", command: "FOO=a go test ./... -count=1", want: "FOO=a go test ./... -count=1", ok: true},
		{name: "non go", command: "FOO=a make test", want: "FOO=a make test", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeDeclaredGoTest(tt.command)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("normalizeDeclaredGoTest(%q) = (%q, %v), want (%q, %v)", tt.command, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDevboxWorker_DeclaredTestFailureIsCodeClass(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: false, Checks: []DevboxCheck{{Name: "fmt", Passed: true}, {Name: "test:0", Passed: false, ExitCode: 1, Output: "assertion failed"}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Item.Success.Tests = []string{"go test ./pkg/mills/..."}
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil || out.Artifacts["passed"] != false {
		t.Fatalf("declared-test verdict = %+v, %v", out, err)
	}
	summary, _ := out.Artifacts["failed_summary"].(string)
	if !strings.Contains(summary, "test:0") || !strings.Contains(summary, "assertion failed") {
		t.Fatalf("failure detail missing: %+v", out.Artifacts)
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

func TestGitLabWorker_CreateMR_BlocksMissingOriginBranchWithoutWorktree(t *testing.T) {
	base := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	gl := &branchLookupGitLab{fakeGitLab: base}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("mr", func(jc *JobContext) { jc.Run.WorktreePath = "" })
	_, err := w.Run(context.Background(), jc)
	if err == nil || !strings.Contains(err.Error(), "implement spawn never pushed") || !strings.Contains(err.Error(), "feat/BL-X") {
		t.Fatalf("error = %v, want missing pushed branch error", err)
	}
	if len(base.createCalls) != 0 {
		t.Fatalf("CreateMR called %d times, want 0", len(base.createCalls))
	}
}

func TestGitLabWorker_CreateMR_BranchLookupErrorFailsOpen(t *testing.T) {
	base := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	gl := &branchLookupGitLab{fakeGitLab: base, branchErr: errors.New("gitlab unavailable")}
	w := &GitLabWorker{Client: gl}
	jc := sampleJobContext("mr", func(jc *JobContext) { jc.Run.WorktreePath = "" })
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(base.createCalls) != 1 {
		t.Fatalf("CreateMR called %d times, want 1", len(base.createCalls))
	}
}

func TestGitLabWorker_CreateMR_WorktreePushSkipsOriginLookup(t *testing.T) {
	base := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	gl := &branchLookupGitLab{fakeGitLab: base}
	pusher := &recordingPusher{}
	w := &GitLabWorker{Client: gl, BranchPusher: pusher}
	if _, err := w.Run(context.Background(), sampleJobContext("mr")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(gl.lookups) != 0 {
		t.Fatalf("GetBranch called with worktree present: %v", gl.lookups)
	}
	if len(pusher.calls) != 1 || len(base.createCalls) != 1 {
		t.Fatalf("push calls=%d create calls=%d, want 1 each", len(pusher.calls), len(base.createCalls))
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

func TestGitLabWorker_CreateMR_CallbackSuppressesItemAutoMerge(t *testing.T) {
	gl := &fakeGitLab{createResp: CreateMRResponse{MRIID: 99}}
	w := &GitLabWorker{Client: gl, AutoMergeFor: func(JobContext) bool { return false }}
	jc := sampleJobContext("mr", func(jc *JobContext) { jc.Item.Policy.AutoMerge = true })
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatal(err)
	}
	if gl.createCalls[0].AutoMerge {
		t.Fatal("callback false must suppress item auto-merge")
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
	gl := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "abc123", CostUSD: 0.01, Remediations: []string{"merge remediation: bounded retry"}}}
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
	if !strings.Contains(out.LogTail, "merge remediation: bounded retry") {
		t.Errorf("merge remediation missing from audit log: %q", out.LogTail)
	}
	if got, ok := out.Artifacts["merge_remediations"].([]string); !ok || len(got) != 1 {
		t.Errorf("merge remediation artifact = %#v", out.Artifacts["merge_remediations"])
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

// B3: a failed check that ALSO fails against bare main classifies as infra;
// one that passes at baseline keeps the code-class failure. The oracle call
// must omit the checkout keys and re-run only the failed subset.
func TestDevboxWorker_BaselineOracle(t *testing.T) {
	failedRun := DevboxResponse{Passed: false, Checks: []DevboxCheck{
		{Name: "test:0", Passed: false, ExitCode: 1, Output: "boom"},
		{Name: "fmt", Passed: true},
	}}
	newJC := func() JobContext {
		return sampleJobContext("tests", func(jc *JobContext) {
			jc.Item.Success.Tests = []string{"go test ./pkg/mills/..."}
			jc.Env = map[string]string{"GIT_TOKEN": "token", "LOOM_MILLS_EXPECTED_SHA": "3333333333333333333333333333333333333333"}
		})
	}

	// Baseline also fails → environment verdict, infra class.
	db := &fakeDevbox{respQueue: []DevboxResponse{
		failedRun,
		{Passed: false, Checks: []DevboxCheck{{Name: "test:0", Passed: false, ExitCode: 1, Output: "boom on main too"}}},
	}}
	heartbeats := 0
	now := time.Now()
	lastActivity := now
	client := &observedGateDevbox{fakeDevbox: db, beforeCall: func() {
		if lastActivity != now {
			t.Fatal("baseline gate started without a fresh activity window")
		}
		now = now.Add(time.Hour)
	}}
	w := &DevboxWorker{Client: client, Project: "loom-core", AgentID: "op", BaselineOracle: func() bool { return true }}
	ctx := context.WithValue(context.Background(), stageHeartbeatKey{}, func() { heartbeats++; lastActivity = now })
	out, err := w.Run(ctx, newJC())
	if heartbeats != 2 {
		t.Fatalf("candidate and baseline heartbeats = %d", heartbeats)
	}
	if err == nil || !errors.Is(err, ErrDevboxBaselineAlsoFails) {
		t.Fatalf("want baseline-also-fails, got %v", err)
	}
	if Classify(err) != ClassInfra {
		t.Fatalf("classify = %s, want infra", Classify(err))
	}
	if out.Artifacts["baseline_verdict"] != "environment" {
		t.Fatalf("artifacts = %#v", out.Artifacts)
	}
	if len(db.calls) != 2 {
		t.Fatalf("oracle must issue exactly one extra call, got %d", len(db.calls))
	}
	oracle := db.calls[1]
	if _, ok := oracle.Env["LOOM_MILLS_BRANCH"]; ok {
		t.Fatalf("oracle must run bare main (no branch key): %#v", oracle.Env)
	}
	if _, ok := oracle.Env["LOOM_MILLS_EXPECTED_SHA"]; ok {
		t.Fatalf("oracle must run bare main (no sha key): %#v", oracle.Env)
	}
	if !reflect.DeepEqual(oracle.Checks, []string{"test:0"}) {
		t.Fatalf("oracle checks = %v, want explicit extra-command scope", oracle.Checks)
	}
	if !reflect.DeepEqual(oracle.TestCommands, []string{"GOWORK=off go test -count=1 ./pkg/mills/..."}) {
		t.Fatalf("oracle must re-run only the failed subset: %#v", oracle)
	}

	// Baseline passes → change implicated, classification unchanged.
	db2 := &fakeDevbox{respQueue: []DevboxResponse{
		failedRun,
		{Passed: true, Checks: []DevboxCheck{{Name: "test:0", Passed: true}}},
	}}
	w2 := &DevboxWorker{Client: db2, Project: "loom-core", AgentID: "op", BaselineOracle: func() bool { return true }}
	out2, err2 := w2.Run(context.Background(), newJC())
	if err2 != nil {
		t.Fatalf("baseline-pass must return change verdict output, got %v", err2)
	}
	if out2.Artifacts["baseline_verdict"] != "change_implicated" {
		t.Fatalf("artifacts = %#v", out2.Artifacts)
	}

	// Oracle disabled → single call, with the genuine verdict returned as output.
	db3 := &fakeDevbox{resp: failedRun}
	w3 := &DevboxWorker{Client: db3, Project: "loom-core", AgentID: "op"}
	if out3, err := w3.Run(context.Background(), newJC()); err != nil || out3.Artifacts["passed"] != false {
		t.Fatalf("failure verdict = %+v, %v", out3, err)
	}
	if len(db3.calls) != 1 {
		t.Fatalf("disabled oracle must not add calls, got %d", len(db3.calls))
	}
}

func TestDevboxWorker_BaselineOracleScopesParityCommand(t *testing.T) {
	const parityCommand = "CGO_ENABLED=0 GOWORK=off golangci-lint run --config .golangci.yml './pkg/mills/gates' 2>&1"
	db := &fakeDevbox{resp: DevboxResponse{
		Passed: true,
		Checks: []DevboxCheck{{Name: gates.LintParityCheckName, Passed: true}},
	}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "op"}
	jc := sampleJobContext("tests")

	_, _, ok := w.runBaselineOracle(context.Background(), jc, []DevboxCheck{{
		Name: gates.LintParityCheckName, Passed: false, ExitCode: 7, Output: "0 issues.",
	}}, []string{parityCommand})
	if !ok || len(db.calls) != 1 {
		t.Fatalf("oracle call missing: ok=%v calls=%d", ok, len(db.calls))
	}
	request := db.calls[0]
	if !reflect.DeepEqual(request.Checks, []string{gates.LintParityCheckName}) {
		t.Fatalf("checks = %v, want explicit parity-only selector", request.Checks)
	}
	if !reflect.DeepEqual(request.TestCommands, []string{parityCommand}) {
		t.Fatalf("test commands = %v, want unchanged env-prefixed parity command", request.TestCommands)
	}
	for _, unwanted := range []string{"fmt", "lint", "test"} {
		if slices.Contains(request.Checks, unwanted) {
			t.Fatalf("default check %q contaminated parity oracle: %v", unwanted, request.Checks)
		}
	}
}

func TestDevboxWorker_UnresolvableSHAFailsClosed(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:0", Passed: true}}}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "op"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Item.Success.Tests = []string{"go test ./pkg/mills/..."}
		jc.Env = map[string]string{"GIT_TOKEN": "token"}
		// No LOOM_MILLS_EXPECTED_SHA and no worktree: production spawn shape.
		jc.Run.WorktreePath = ""
	})
	_, err := w.Run(context.Background(), jc)
	if !errors.Is(err, ErrDevboxCheckoutInfra) || !strings.Contains(err.Error(), "pushed head unresolvable for feat/BL-X") {
		t.Fatalf("error = %v", err)
	}
	if len(db.calls) != 0 {
		t.Fatalf("quality gate called despite unresolved head: %#v", db.calls)
	}
}

// 2026-08-20 incident (second layer): the devbox agent id must stay inside
// the Kubernetes 63-char label bound — the full run id join produced
// "loom-mills-operator-PIPE-psl-plan-council-…" and every sandbox pod was
// rejected as metadata.labels-invalid.
func TestDevboxAgentIDStaysWithinLabelBound(t *testing.T) {
	runID := "PIPE-psl-plan-council-unify-the-mill-staff-hud-group-and-document-s2-soak-exit-cri-2-01a0205d-19c0-74fa-97a2-698e4c672a6c"
	id := devboxAgentID("loom-mills-operator", runID, "")
	if len(id) > 63 {
		t.Fatalf("agent id %q exceeds the label bound (%d chars)", id, len(id))
	}
	if !strings.HasPrefix(id, "loom-mills-operator-") || !strings.HasSuffix(id, "698e4c672a6c") {
		t.Fatalf("agent id %q must keep base + unique run token", id)
	}
	baseline := devboxAgentID("loom-mills-operator", runID, "baseline")
	if len(baseline) > 63 || !strings.HasSuffix(baseline, "-baseline") {
		t.Fatalf("baseline agent id %q invalid (%d chars)", baseline, len(baseline))
	}
	// Two runs must not collide on the same sandbox identity.
	other := devboxAgentID("loom-mills-operator", "PIPE-x-01a0205d-0a8a-7c27-a051-7f55ca0f78ff", "")
	if other == id {
		t.Fatalf("distinct runs must derive distinct agent ids")
	}
}

type timedDevbox struct {
	fakeDevbox
	timeout time.Duration
}

func (d *timedDevbox) EffectiveGateTimeout() time.Duration { return d.timeout }

func TestDispatcher_SynchronousCallTimeoutFallback(t *testing.T) {
	w := &DevboxWorker{Client: &timedDevbox{timeout: 90 * time.Minute}}
	d := NewDispatcher(nil, w)
	if got := d.SynchronousCallTimeout("tests"); got != 90*time.Minute {
		t.Fatalf("timeout = %s", got)
	}
	if got := (*Dispatcher)(nil).SynchronousCallTimeout("tests"); got != 0 {
		t.Fatalf("nil timeout = %s", got)
	}
}

type observedGateDevbox struct {
	*fakeDevbox
	beforeCall func()
}

func (d *observedGateDevbox) QualityGate(ctx context.Context, req DevboxRequest) (DevboxResponse, error) {
	d.beforeCall()
	return d.fakeDevbox.QualityGate(ctx, req)
}

func TestDevboxWorker_CancelledGateDoesNotHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), stageHeartbeatKey{}, func() { t.Fatal("cancelled gate refreshed activity") }))
	client := &observedGateDevbox{fakeDevbox: &fakeDevbox{}, beforeCall: cancel}
	w := &DevboxWorker{Client: client}
	_, _ = w.qualityGate(ctx, DevboxRequest{})
}

func TestDevboxWorker_ParityEmptyOutputInfra(t *testing.T) {
	for _, baseline := range []bool{false, true} {
		t.Run(fmt.Sprint(baseline), func(t *testing.T) {
			empty := DevboxResponse{Checks: []DevboxCheck{{Name: gates.LintParityCheckName, ExitCode: 1}}}
			db := &fakeDevbox{resp: empty}
			if baseline {
				db.respQueue = []DevboxResponse{{Checks: []DevboxCheck{{Name: gates.LintParityCheckName, ExitCode: 1, Output: "real noctx finding"}}}, empty}
			}
			jc := sampleJobContext("tests", func(jc *JobContext) { jc.Prior["implement"] = StageOutput{FilesChanged: []string{"pkg/a/a.go"}} })
			out, err := (&DevboxWorker{Client: db, BaselineOracle: func() bool { return baseline }}).Run(t.Context(), jc)
			if err == nil || Classify(err) != ClassInfra || IsTerminal(Classify(err)) || !strings.Contains(err.Error(), gates.LintParityNoOutput) {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			key := "checks"
			if baseline {
				key = "baseline_checks"
			}
			checks := out.Artifacts[key].([]DevboxCheck)
			if !checks[0].Degraded || checks[0].Warning != gates.LintParityInfraWarning || checks[0].FailureSignature != gates.LintParityNoOutput {
				t.Fatalf("checks=%+v", checks)
			}
		})
	}
}

type stoppingDevbox struct {
	fakeDevbox
	sequence []string
	stopErr  error
}

func (f *stoppingDevbox) Stop(_ context.Context, _, agent string) error {
	f.sequence = append(f.sequence, "stop:"+agent)
	return f.stopErr
}
func (f *stoppingDevbox) QualityGate(ctx context.Context, req DevboxRequest) (DevboxResponse, error) {
	f.sequence = append(f.sequence, "gate:"+req.AgentID)
	return f.fakeDevbox.QualityGate(ctx, req)
}
func TestDevboxWorker_ResumeStopsBeforeGate(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			db := &stoppingDevbox{fakeDevbox: fakeDevbox{resp: DevboxResponse{Passed: true}}}
			if fail {
				db.stopErr = errors.New("termination failed")
			}
			jc := sampleJobContext("tests")
			out, err := (&DevboxWorker{Client: db, AgentID: "operator"}).Run(context.WithValue(context.Background(), resumeTestsKey{}, true), jc)
			if (err != nil) != fail {
				t.Fatalf("error = %v", err)
			}
			want := []string{"stop:" + devboxAgentID("operator", jc.Run.ID, "")}
			if !fail {
				want = append(want, "stop:"+devboxAgentID("operator", jc.Run.ID, "baseline"), "gate:"+devboxAgentID("operator", jc.Run.ID, ""), "stop:"+devboxAgentID("operator", jc.Run.ID, ""))
			}
			if !reflect.DeepEqual(db.sequence, want) {
				t.Fatalf("sequence %v, want %v", db.sequence, want)
			}
			if out.Artifacts["resume_sandbox_cleanup"] == nil {
				t.Fatal("missing cleanup evidence")
			}
		})
	}
}

// releasingDevbox records every gate and release call, in order, so the tests
// stage can be checked for releasing each minted sandbox exactly once.
type releasingDevbox struct {
	fakeDevbox
	t           *testing.T
	events      []string
	stopErr     error
	baselineErr error              // the second (oracle) gate call fails with this
	cancel      context.CancelFunc // cancels the stage context inside the gate
}

func (f *releasingDevbox) QualityGate(ctx context.Context, req DevboxRequest) (DevboxResponse, error) {
	f.events = append(f.events, "gate:"+req.AgentID)
	if f.cancel != nil {
		f.cancel()
	}
	if f.baselineErr != nil && len(f.calls) == 1 {
		return DevboxResponse{}, f.baselineErr
	}
	return f.fakeDevbox.QualityGate(ctx, req)
}

func (f *releasingDevbox) Stop(ctx context.Context, project, agentID string) error {
	if ctx.Err() != nil {
		f.t.Errorf("cleanup context canceled: %v", ctx.Err())
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 55*time.Second {
		f.t.Error("cleanup must have bounded deadline")
	}
	if project != "services/other" {
		f.t.Errorf("cleanup project = %q", project)
	}
	f.events = append(f.events, "stop:"+agentID)
	return f.stopErr
}

// bl-devbox-sandbox-quota-headroom-20260913: every sandbox the tests stage
// mints is released exactly once, as soon as its gate returns — the run
// sandbox before the baseline oracle mints a second one — whatever the gate
// verdict, including transport errors and a cancelled stage context. A
// sandbox that was never minted (the baseline identity without an oracle
// run) is never stopped.
func TestDevboxWorker_ReleasesRunSandboxes(t *testing.T) {
	// A non-zero exit keeps the failed check out of the contradictory-verdict
	// guard, which would otherwise return before the baseline oracle runs.
	failedRun := DevboxResponse{Passed: false, Checks: []DevboxCheck{{Name: "test:0", Passed: false, ExitCode: 1, Output: "boom"}}}
	passedRun := DevboxResponse{Passed: true, Checks: []DevboxCheck{{Name: "test:0", Passed: true}}}
	cases := []struct {
		name        string
		resp        DevboxResponse
		gateErr     error // main gate transport error
		baselineErr error // oracle gate transport error
		stopErr     error
		cancel      bool // the gate cancels the stage context
		oracle      bool
		wantErr     bool
		wantErrIs   error
		wantLog     string
	}{
		{name: "pass", resp: passedRun},
		// A genuine code verdict is successful stage transport (no error).
		{name: "fail", resp: failedRun},
		{name: "transport", gateErr: errors.New("gate transport failure"), wantErr: true},
		{name: "cancel", gateErr: errors.New("gate transport failure"), cancel: true, wantErr: true},
		{name: "cleanup-error", resp: passedRun, stopErr: errors.New("stop unavailable"), wantLog: "mills devbox sandbox release failed"},
		{name: "baseline", resp: failedRun, oracle: true, wantErr: true, wantErrIs: ErrDevboxBaselineAlsoFails},
		{name: "baseline-error", resp: failedRun, oracle: true, baselineErr: errors.New("baseline unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db := &releasingDevbox{t: t, fakeDevbox: fakeDevbox{resp: tc.resp, err: tc.gateErr}, stopErr: tc.stopErr, baselineErr: tc.baselineErr}
			if tc.cancel {
				db.cancel = cancel
			}
			jc := sampleJobContext("tests", func(jc *JobContext) {
				jc.Item.TargetProject = "services/other"
				jc.Item.Success.Tests = []string{"go test ./pkg/mills/..."}
			})
			w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "loom-mills-operator", BaselineOracle: func() bool { return tc.oracle }}
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			_, err := w.Run(ctx, jc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("Run error = %v, want %v", err, tc.wantErrIs)
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("logs = %q, want %q", logs.String(), tc.wantLog)
			}
			mainID := devboxAgentID(w.AgentID, jc.Run.ID, "")
			baselineID := devboxAgentID(w.AgentID, jc.Run.ID, "baseline")
			want := []string{"gate:" + mainID, "stop:" + mainID}
			if tc.oracle {
				want = append(want, "gate:"+baselineID, "stop:"+baselineID)
			}
			if !reflect.DeepEqual(db.events, want) {
				t.Fatalf("events = %v, want %v", db.events, want)
			}
		})
	}
}

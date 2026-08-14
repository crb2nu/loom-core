package pipeline

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestEffectiveProject(t *testing.T) {
	home := "loom-core"
	if got := effectiveProject(nil, home); got != home {
		t.Errorf("nil item: got %q, want home %q", got, home)
	}
	if got := effectiveProject(&store.BacklogItem{}, home); got != home {
		t.Errorf("empty target: got %q, want home %q", got, home)
	}
	if got := effectiveProject(&store.BacklogItem{TargetProject: "  services/flexdeck "}, home); got != "services/flexdeck" {
		t.Errorf("target set: got %q, want services/flexdeck", got)
	}
}

// TestSpawnWorker_TargetProjectRoutesCrossRepo: a per-item TargetProject
// overrides the worker's home Project, and a cross-repo item does NOT reuse the
// operator-local RepoRoot as the git-capture working dir (that clone is the home
// repo). Empty target keeps home behavior.
func TestSpawnWorker_TargetProjectRoutesCrossRepo(t *testing.T) {
	cases := []struct {
		name           string
		target         string
		wantProject    string
		wantWorkingDir string
	}{
		{
			name:           "home item uses home project + operator repo root",
			target:         "",
			wantProject:    "loom-core",
			wantWorkingDir: "/var/lib/loom-mills/loom-core",
		},
		{
			name:           "cross-repo item routes project + skips operator repo root",
			target:         "services/loom-flightdeck",
			wantProject:    "services/loom-flightdeck",
			wantWorkingDir: "", // operator-local clone is the home repo; skip capture
		},
		{
			name:           "home target by name still uses operator repo root",
			target:         "loom-core",
			wantProject:    "loom-core",
			wantWorkingDir: "/var/lib/loom-mills/loom-core",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := &fakeSpawn{resp: SpawnResponse{SpawnID: "spawn-1"}}
			w := &SpawnWorker{Client: sp, Project: "loom-core", RepoRoot: "/var/lib/loom-mills/loom-core"}
			jc := sampleJobContext("implement", func(jc *JobContext) {
				jc.Run.WorktreePath = "" // force the RepoRoot fallback path
				jc.Item.TargetProject = tc.target
			})
			if _, err := w.Run(context.Background(), jc); err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(sp.calls) != 1 {
				t.Fatalf("spawn calls = %d, want 1", len(sp.calls))
			}
			if sp.calls[0].Project != tc.wantProject {
				t.Errorf("Project = %q, want %q", sp.calls[0].Project, tc.wantProject)
			}
			if sp.calls[0].WorkingDir != tc.wantWorkingDir {
				t.Errorf("WorkingDir = %q, want %q", sp.calls[0].WorkingDir, tc.wantWorkingDir)
			}
		})
	}
}

func TestDevboxWorker_TargetProjectRoutesCrossRepo(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	jc := sampleJobContext("tests", func(jc *JobContext) {
		jc.Item.TargetProject = "services/loom-flightdeck"
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(db.calls) != 1 {
		t.Fatalf("devbox calls = %d, want 1", len(db.calls))
	}
	if db.calls[0].Project != "services/loom-flightdeck" {
		t.Errorf("Project = %q, want services/loom-flightdeck", db.calls[0].Project)
	}
}

func TestDevboxWorker_EmptyTargetUsesHome(t *testing.T) {
	db := &fakeDevbox{resp: DevboxResponse{Passed: true}}
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "claude-code"}
	if _, err := w.Run(context.Background(), sampleJobContext("tests")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if db.calls[0].Project != "loom-core" {
		t.Errorf("Project = %q, want home loom-core", db.calls[0].Project)
	}
}

// TestGitLabWorker_ForProjectRoutesCrossRepo: a cross-repo item's mr stage runs
// against the ForProject-scoped client, not the home client. A home item (no
// TargetProject) always uses the home client (ForProject not consulted).
func TestGitLabWorker_ForProjectRoutesCrossRepo(t *testing.T) {
	home := &fakeGitLab{createResp: CreateMRResponse{MRIID: 1}}
	target := &fakeGitLab{createResp: CreateMRResponse{MRIID: 2}}
	w := &GitLabWorker{
		Client: home,
		ForProject: func(project string) GitLabClient {
			if project == "services/loom-flightdeck" {
				return target
			}
			return nil
		},
	}

	// Cross-repo item → target client.
	jc := sampleJobContext("mr", func(jc *JobContext) {
		jc.Item.TargetProject = "services/loom-flightdeck"
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run cross-repo: %v", err)
	}
	if len(target.createCalls) != 1 {
		t.Errorf("target client CreateMR calls = %d, want 1", len(target.createCalls))
	}
	if len(home.createCalls) != 0 {
		t.Errorf("home client must not be used for cross-repo item, got %d calls", len(home.createCalls))
	}

	// Home item → home client (ForProject short-circuits on empty target).
	homeJC := sampleJobContext("mr")
	if _, err := w.Run(context.Background(), homeJC); err != nil {
		t.Fatalf("run home: %v", err)
	}
	if len(home.createCalls) != 1 {
		t.Errorf("home client CreateMR calls = %d, want 1", len(home.createCalls))
	}
	if len(target.createCalls) != 1 {
		t.Errorf("target client must not gain calls for a home item, got %d", len(target.createCalls))
	}
}

// A backlog reroute after MR creation must not move ci_watch to a same-IID MR
// in the new project. Routing comes from MR-stage provenance, not the mutable
// item.TargetProject.
func TestGitLabWorker_CIWatchUsesPersistedProjectAfterItemReroute(t *testing.T) {
	home := &fakeGitLab{pollResp: testCIPollResponse("success", "persisted-project-head")}
	target := &fakeGitLab{pollResp: PollPipelineResponse{
		Status:       "success",
		Project:      "services/loom-flightdeck",
		SourceBranch: testCISource,
		TargetBranch: testCITarget,
		SHA:          "same-iid-other-project",
	}}
	w := &GitLabWorker{
		Client: home,
		ForProject: func(project string) GitLabClient {
			if project == testCIProject {
				return home
			}
			if project == "services/loom-flightdeck" {
				return target
			}
			return nil
		},
	}
	jc := sampleJobContext("ci_watch", func(jc *JobContext) {
		jc.Item.TargetProject = "services/loom-flightdeck"
		addMRProvenance(jc, 215, testCIProject, testCISource, testCITarget)
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("ci_watch after item reroute: %v", err)
	}
	if len(home.pollCalls) != 1 || len(target.pollCalls) != 0 {
		t.Fatalf("home/target poll calls = %d/%d, want 1/0", len(home.pollCalls), len(target.pollCalls))
	}
	if got := home.pollCalls[0].Project; got != testCIProject {
		t.Fatalf("poll authorization project = %q, want persisted %q", got, testCIProject)
	}
}

func TestGitLabWorker_MergeUsesPersistedProjectAfterItemReroute(t *testing.T) {
	home := &fakeGitLab{mergeResp: MergeResponse{MergedSHA: "authorized-merge"}}
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
	jc := sampleJobContext("merge", func(jc *JobContext) {
		jc.Item.TargetProject = "services/loom-flightdeck"
		mr := int64(215)
		jc.Run.MRIID = &mr
		jc.Prior["ci_watch"] = StageOutput{Artifacts: testCIArtifacts("persisted-project-head")}
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil || out.MergedSHA != "authorized-merge" {
		t.Fatalf("merge after item reroute = %+v, %v", out, err)
	}
	if len(home.mergeCalls) != 1 || len(target.mergeCalls) != 0 {
		t.Fatalf("home/target merge calls = %d/%d, want 1/0", len(home.mergeCalls), len(target.mergeCalls))
	}
}

func TestGitLabWorker_CleanupUsesPersistedProjectAfterItemReroute(t *testing.T) {
	home := &fakeGitLab{}
	target := &fakeGitLab{}
	w := &GitLabWorker{
		Client: home,
		SourceBranch: func(JobContext) string {
			return "feat/mutated"
		},
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
	jc := sampleJobContext("cleanup", func(jc *JobContext) {
		jc.Item.TargetProject = "services/loom-flightdeck"
		addMRProvenance(jc, 215, testCIProject, "feat/persisted", testCITarget)
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("cleanup after item reroute: %v", err)
	}
	if len(home.cleanupCalls) != 1 || len(target.cleanupCalls) != 0 {
		t.Fatalf("home/target cleanup calls = %d/%d, want 1/0", len(home.cleanupCalls), len(target.cleanupCalls))
	}
	if got := home.cleanupCalls[0].BranchName; got != "feat/persisted" {
		t.Fatalf("cleanup branch = %q, want persisted branch", got)
	}
	if got := home.cleanupCalls[0].Project; got != testCIProject {
		t.Fatalf("cleanup project = %q, want persisted %q", got, testCIProject)
	}
}

// TestGitLabWorker_NilForProjectFallsBackToClient: ForProject unset → the home
// Client is used even for an item that carries a TargetProject (safe default;
// the reconciler gate is what actually blocks such an item when cross-repo is
// disabled).
func TestGitLabWorker_NilForProjectFallsBackToClient(t *testing.T) {
	home := &fakeGitLab{createResp: CreateMRResponse{MRIID: 1}}
	w := &GitLabWorker{Client: home} // ForProject nil
	jc := sampleJobContext("mr", func(jc *JobContext) {
		jc.Item.TargetProject = "services/loom-flightdeck"
	})
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(home.createCalls) != 1 {
		t.Errorf("home client CreateMR calls = %d, want 1", len(home.createCalls))
	}
}

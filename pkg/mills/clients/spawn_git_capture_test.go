package clients

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// Regression suite for issue #224: the cumulative branch-vs-base git
// capture was "dead in production" and — critically — UNDIAGNOSABLE,
// because every skip and every failure returned silently and reached the
// gates as the same zero-files/empty-diff state a genuinely empty branch
// produces. These tests pin the observability contract (every capture
// outcome is recorded on the response artifacts AND logged) and the
// resume path that dropped the capture coordinates outright.

// captureLogger returns a logger writing into buf at Debug level so both
// the warn and debug capture events are assertable.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func gitCaptureArtifact(t *testing.T, resp pipeline.SpawnResponse) map[string]any {
	t.Helper()
	raw, ok := resp.Artifacts[pipeline.GitCaptureArtifactKey]
	if !ok {
		t.Fatalf("artifacts missing %q slot; got %#v", pipeline.GitCaptureArtifactKey, resp.Artifacts)
	}
	art, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%q artifact is %T, want map[string]any", pipeline.GitCaptureArtifactKey, raw)
	}
	return art
}

func gitCaptureStatus(t *testing.T, resp pipeline.SpawnResponse) string {
	t.Helper()
	status, _ := gitCaptureArtifact(t, resp)["status"].(string)
	return status
}

func gitCaptureReason(t *testing.T, resp pipeline.SpawnResponse) string {
	t.Helper()
	reason, _ := gitCaptureArtifact(t, resp)["reason"].(string)
	return reason
}

// TestGitCapture_SkipWithoutCoordinatesIsEvented: a stage dispatched
// without WorkingDir must still say WHY nothing was captured. Before this,
// the missing-coordinates branch was a bare `return`, so an operator
// deployment that never wired RepoRoot looked identical to one where the
// branch legitimately carried no work.
func TestGitCapture_SkipWithoutCoordinatesIsEvented(t *testing.T) {
	var logs bytes.Buffer
	gr := &spawnTelGitRunner{}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-no-wd"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-no-wd", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	c.cfg.Logger = captureLogger(&logs)

	req := sampleSpawnReq()
	req.WorkingDir = ""
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := gitCaptureStatus(t, resp); got != gitCaptureStatusNoContext {
		t.Errorf("status = %q, want %q", got, gitCaptureStatusNoContext)
	}
	if reason := gitCaptureReason(t, resp); !strings.Contains(reason, "working_dir") {
		t.Errorf("reason %q should name the missing coordinate", reason)
	}
	if len(gr.callArgs()) != 0 {
		t.Errorf("git runner must not run without coordinates; saw %v", gr.callArgs())
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "cumulative git capture unavailable") {
		t.Errorf("skip must emit a WARN event; log was %q", out)
	}
}

// TestGitCapture_FetchFailureRecordsReasonAndNeverFailsStage pins the
// most likely production failure: the operator-local clone cannot refresh
// the branch/base refs (missing ref, credential-helper misfire, shallow
// clone). The stage must still succeed on spawn telemetry, but the git
// exit code and stderr must be recorded so the cause is answerable from
// the persisted stage row.
func TestGitCapture_FetchFailureRecordsReasonAndNeverFailsStage(t *testing.T) {
	var logs bytes.Buffer
	branchFetch := "fetch --depth=100 origin +refs/heads/mills/BL-X/plan_slice:refs/remotes/origin/mills/BL-X/plan_slice"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{branchFetch: ""},
		stderrs: map[string]string{branchFetch: "fatal: couldn't find remote ref refs/heads/mills/BL-X/plan_slice"},
		exits:   map[string]int{branchFetch: 128},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-fetch-fail"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-fetch-fail", Status: "completed",
				Telemetry: &hudSpawnTelemetry{
					TotalCostUSD: 0.25,
					FileChanges:  []hudFileChange{{Path: "pkg/a.go", LinesAdded: 2}},
				},
			}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	c.cfg.Logger = captureLogger(&logs)

	req := sampleSpawnReq()
	req.WorkingDir = "/var/lib/loom-mills/loom-core"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("a failed capture must never fail the stage: %v", err)
	}
	if got := gitCaptureStatus(t, resp); got != gitCaptureStatusFetchFailed {
		t.Fatalf("status = %q, want %q", got, gitCaptureStatusFetchFailed)
	}
	reason := gitCaptureReason(t, resp)
	if !strings.Contains(reason, "exit 128") || !strings.Contains(reason, "couldn't find remote ref") {
		t.Errorf("reason %q must carry the git exit code and stderr", reason)
	}
	// Telemetry survives untouched — the capture is additive, never
	// destructive, when it cannot trust the refs.
	if len(resp.FilesChanged) != 1 || resp.FilesChanged[0] != "pkg/a.go" {
		t.Errorf("spawn telemetry paths should survive a failed capture; got %v", resp.FilesChanged)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") {
		t.Errorf("fetch failure must emit a WARN event; log was %q", out)
	}
}

// TestGitCapture_PopulatedCaptureAttachesFilesAndDiff is the positive
// regression pin: when the refs refresh, the cumulative diff, the
// cumulative path list, and the numstat totals all land on the response —
// this is the evidence nonempty_diff/scope/diff_size gates judge.
func TestGitCapture_PopulatedCaptureAttachesFilesAndDiff(t *testing.T) {
	var logs bytes.Buffer
	base, head := "origin/main", "origin/mills/BL-X/plan_slice"
	diffOut := "diff --git a/pkg/mills/clients/spawn.go b/pkg/mills/clients/spawn.go\n@@\n-old\n+new\n"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff " + base + "..." + head:                      diffOut,
			"diff --name-only " + base + "..." + head:          "pkg/mills/clients/spawn.go\nchangelog.d/x.fixed.md\n",
			"diff --numstat " + base + "..." + head:            "4\t1\tpkg/mills/clients/spawn.go\n1\t0\tchangelog.d/x.fixed.md\n",
			"log --pretty=format:%B%x00 " + base + ".." + head: "fix(mills): resurrect capture\x00",
		},
	}
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-captured"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{
				SpawnID: "spawn-captured", Status: "completed",
				Telemetry: &hudSpawnTelemetry{TotalCostUSD: 0.5},
			}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	c.cfg.Logger = captureLogger(&logs)

	req := sampleSpawnReq()
	req.WorkingDir = "/var/lib/loom-mills/loom-core"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := gitCaptureStatus(t, resp); got != gitCaptureStatusCaptured {
		t.Fatalf("status = %q, want %q (reason=%q)", got, gitCaptureStatusCaptured, gitCaptureReason(t, resp))
	}
	if !strings.Contains(string(resp.DiffPatch), "pkg/mills/clients/spawn.go") {
		t.Errorf("DiffPatch not attached; got %q", string(resp.DiffPatch))
	}
	if len(resp.FilesChanged) != 2 {
		t.Errorf("FilesChanged = %v, want both cumulative paths", resp.FilesChanged)
	}
	if resp.LinesAdded != 5 || resp.LinesRemoved != 1 {
		t.Errorf("line totals = +%d/-%d, want +5/-1", resp.LinesAdded, resp.LinesRemoved)
	}
	if len(resp.CommitMessages) != 1 {
		t.Errorf("CommitMessages = %v, want the branch commit", resp.CommitMessages)
	}
	art := gitCaptureArtifact(t, resp)
	if art["base_ref"] != base || art["head_ref"] != head {
		t.Errorf("artifact refs = %v/%v, want %s/%s", art["base_ref"], art["head_ref"], base, head)
	}
}

// TestGitCapture_EmptyBranchIsDistinctFromSkip: refs refreshed, git
// answered "nothing changed". That is a legitimate empty branch and must
// NOT be reported with the same status as a capture that never ran —
// separating the two is the whole point of issue #224.
func TestGitCapture_EmptyBranchIsDistinctFromSkip(t *testing.T) {
	gr := &spawnTelGitRunner{} // every command succeeds with empty stdout
	ft := &hudFakeTransport{
		post: func(_ *http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-empty"}
		},
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-empty", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr

	req := sampleSpawnReq()
	req.WorkingDir = "/var/lib/loom-mills/loom-core"
	resp, err := c.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := gitCaptureStatus(t, resp); got != gitCaptureStatusEmpty {
		t.Errorf("status = %q, want %q", got, gitCaptureStatusEmpty)
	}
}

// TestResumeWithContext_RunsGitCapture is the core issue-#224 fix: the
// operator re-attaches to in-flight spawns on every pod rollout, and the
// bare Resume entrypoint had no branch/base/checkout in scope, so the
// capture was structurally dead for every stage that finished across a
// roll. ResumeWithContext threads the same coordinates the initial
// dispatch used.
func TestResumeWithContext_RunsGitCapture(t *testing.T) {
	base, head := "origin/main", "origin/feat/BL-X"
	gr := &spawnTelGitRunner{
		stdouts: map[string]string{
			"diff " + base + "..." + head:             "diff --git a/pkg/x.go b/pkg/x.go\n@@\n+resumed\n",
			"diff --name-only " + base + "..." + head: "pkg/x.go\n",
		},
	}
	ft := &hudFakeTransport{
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-resume-ctx", Status: "completed", Telemetry: &hudSpawnTelemetry{TotalCostUSD: 0.1}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr

	resp, err := c.ResumeWithContext(context.Background(), "spawn-resume-ctx", pipeline.SpawnResumeContext{
		Project:    "loom-core",
		WorkingDir: "/var/lib/loom-mills/loom-core",
		BaseBranch: "main",
		Branch:     "feat/BL-X",
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := gitCaptureStatus(t, resp); got != gitCaptureStatusCaptured {
		t.Fatalf("status = %q, want %q (reason=%q)", got, gitCaptureStatusCaptured, gitCaptureReason(t, resp))
	}
	if !strings.Contains(string(resp.DiffPatch), "resumed") {
		t.Errorf("resumed spawn must carry the cumulative diff; got %q", string(resp.DiffPatch))
	}
	if len(resp.FilesChanged) != 1 || resp.FilesChanged[0] != "pkg/x.go" {
		t.Errorf("FilesChanged = %v, want the cumulative branch path", resp.FilesChanged)
	}
	if resumed, _ := gitCaptureArtifact(t, resp)["resumed"].(bool); !resumed {
		t.Errorf("artifact should mark the capture as taken on a resume")
	}
}

// TestResume_RecordsResumeSkipReason: the legacy coordinate-less Resume
// still skips, but now says so with a resume-specific status instead of
// vanishing.
func TestResume_RecordsResumeSkipReason(t *testing.T) {
	var logs bytes.Buffer
	gr := &spawnTelGitRunner{}
	ft := &hudFakeTransport{
		get: func(_ *http.Request) (int, any) {
			return 200, hudSpawnState{SpawnID: "spawn-resume-bare", Status: "completed", Telemetry: &hudSpawnTelemetry{}}
		},
	}
	c := newHUDStub(t, ft)
	c.cfg.GitRunner = gr
	c.cfg.Logger = captureLogger(&logs)

	resp, err := c.Resume(context.Background(), "spawn-resume-bare")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := gitCaptureStatus(t, resp); got != gitCaptureStatusResumeNoContext {
		t.Errorf("status = %q, want %q", got, gitCaptureStatusResumeNoContext)
	}
	if len(gr.callArgs()) != 0 {
		t.Errorf("bare Resume must not shell out to git; saw %v", gr.callArgs())
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") {
		t.Errorf("bare Resume skip must emit a WARN event; log was %q", out)
	}
}

// TestHUDSpawnClient_SatisfiesContextResumeClient keeps the dispatcher's
// preferred interface wired: a type-assertion miss would silently fall
// back to the coordinate-less Resume and re-kill the capture.
func TestHUDSpawnClient_SatisfiesContextResumeClient(t *testing.T) {
	var _ pipeline.SpawnContextResumeClient = (*HUDSpawnClient)(nil)
	var _ pipeline.SpawnResumeClient = (*HUDSpawnClient)(nil)
}

func TestRedactGitOutput_StripsCredentialUserinfo(t *testing.T) {
	in := "fatal: Authentication failed for 'https://oauth2:glpat-SECRETVALUE@gitlab.flexinfer.ai/services/loom-core.git/'"
	got := redactGitOutput(in)
	if strings.Contains(got, "glpat-SECRETVALUE") {
		t.Fatalf("token leaked into recorded reason: %q", got)
	}
	if !strings.Contains(got, "***@gitlab.flexinfer.ai") {
		t.Errorf("host should survive redaction; got %q", got)
	}
}

func TestRedactGitOutput_LeavesPlainStderrIntact(t *testing.T) {
	in := "  fatal: couldn't find remote ref refs/heads/feat/x\n"
	if got, want := redactGitOutput(in), "fatal: couldn't find remote ref refs/heads/feat/x"; got != want {
		t.Errorf("redactGitOutput = %q, want %q", got, want)
	}
}

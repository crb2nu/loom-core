package pipeline

import (
	"context"
	"errors"
	"testing"
)

// fakeContextResumeSpawn implements SpawnClient AND
// SpawnContextResumeClient so the dispatcher's preferred resume path is
// exercised end to end.
type fakeContextResumeSpawn struct {
	fakeSpawn
	gotSpawnID string
	gotCtx     SpawnResumeContext
	resumeResp SpawnResponse
	resumeErr  error
	calls      int
}

func (f *fakeContextResumeSpawn) ResumeWithContext(_ context.Context, spawnID string, rc SpawnResumeContext) (SpawnResponse, error) {
	f.calls++
	f.gotSpawnID = spawnID
	f.gotCtx = rc
	return f.resumeResp, f.resumeErr
}

// fakeLegacyResumeSpawn implements only the coordinate-less
// SpawnResumeClient, standing in for an older backend.
type fakeLegacyResumeSpawn struct {
	fakeSpawn
	gotSpawnID string
	calls      int
}

func (f *fakeLegacyResumeSpawn) Resume(_ context.Context, spawnID string) (SpawnResponse, error) {
	f.calls++
	f.gotSpawnID = spawnID
	return SpawnResponse{SpawnID: spawnID}, nil
}

// TestSpawnWorker_ResumeCarriesGitCaptureContext is the dispatcher half of
// the issue-#224 fix. The operator re-attaches to in-flight spawns on
// every pod rollout (reconciler pickupInFlightRuns → Runner.Start →
// resume), and the deployment rolls on every Flux image update. While
// resume dropped the checkout/branch coordinates, the cumulative
// branch-vs-base capture could not run for ANY stage that finished across
// a roll — the finished-work-invisible-to-the-gate shape that burns
// retries on nonempty_diff.
func TestSpawnWorker_ResumeCarriesGitCaptureContext(t *testing.T) {
	sp := &fakeContextResumeSpawn{resumeResp: SpawnResponse{SpawnID: "spawn-resumed", CostUSD: 0.2}}
	w := &SpawnWorker{
		Client:     sp,
		Project:    "loom-core",
		BaseBranch: "main",
		RepoRoot:   "/var/lib/loom-mills/loom-core",
	}
	jc := sampleJobContext("implement", func(jc *JobContext) {
		jc.ResumeSpawnID = "spawn-resumed"
		jc.Run.WorktreePath = "" // standard non-integrator path
	})
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sp.calls != 1 {
		t.Fatalf("ResumeWithContext calls = %d, want 1", sp.calls)
	}
	if sp.gotSpawnID != "spawn-resumed" {
		t.Errorf("spawn id = %q, want spawn-resumed", sp.gotSpawnID)
	}
	want := SpawnResumeContext{
		Project:    "loom-core",
		WorkingDir: "/var/lib/loom-mills/loom-core",
		BaseBranch: "main",
		Branch:     "feat/BL-X",
	}
	if sp.gotCtx != want {
		t.Errorf("resume context = %+v, want %+v", sp.gotCtx, want)
	}
	if out.SpawnID != "spawn-resumed" {
		t.Errorf("output passthrough wrong: %+v", out)
	}
}

// TestSpawnWorker_ResumeContextUsesRunWorktreeWhenAllocated mirrors the
// dispatch-time precedence: an allocated run worktree outranks the
// operator-local clone as the capture dir on resume too.
func TestSpawnWorker_ResumeContextUsesRunWorktreeWhenAllocated(t *testing.T) {
	sp := &fakeContextResumeSpawn{resumeResp: SpawnResponse{SpawnID: "spawn-wt"}}
	w := &SpawnWorker{Client: sp, Project: "loom-core", RepoRoot: "/var/lib/loom-mills/loom-core"}
	jc := sampleJobContext("implement", func(jc *JobContext) { jc.ResumeSpawnID = "spawn-wt" })
	if _, err := w.Run(context.Background(), jc); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sp.gotCtx.WorkingDir != "/tmp/wt" {
		t.Errorf("WorkingDir = %q, want the run worktree", sp.gotCtx.WorkingDir)
	}
	if sp.gotCtx.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want the resolved default", sp.gotCtx.BaseBranch)
	}
}

// TestSpawnWorker_ResumeErrorStillReturnsPartialOutput: a resumed spawn
// that ends terminal-failed must return its partial telemetry alongside
// the error, exactly as the pre-fix path did.
func TestSpawnWorker_ResumeErrorStillReturnsPartialOutput(t *testing.T) {
	boom := errors.New("terminal spawn failure")
	sp := &fakeContextResumeSpawn{
		resumeResp: SpawnResponse{SpawnID: "spawn-bad", CostUSD: 0.4},
		resumeErr:  boom,
	}
	w := &SpawnWorker{Client: sp, RepoRoot: "/repo"}
	jc := sampleJobContext("implement", func(jc *JobContext) { jc.ResumeSpawnID = "spawn-bad" })
	out, err := w.Run(context.Background(), jc)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the resume error", err)
	}
	if out.SpawnID != "spawn-bad" || out.CostUSD != 0.4 {
		t.Errorf("partial output lost on resume error: %+v", out)
	}
}

// TestSpawnWorker_ResumeFallsBackToLegacyResumeClient keeps backends that
// implement only SpawnResumeClient working (no capture, as before).
func TestSpawnWorker_ResumeFallsBackToLegacyResumeClient(t *testing.T) {
	sp := &fakeLegacyResumeSpawn{}
	w := &SpawnWorker{Client: sp, RepoRoot: "/repo"}
	jc := sampleJobContext("implement", func(jc *JobContext) { jc.ResumeSpawnID = "spawn-legacy" })
	out, err := w.Run(context.Background(), jc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sp.calls != 1 || sp.gotSpawnID != "spawn-legacy" {
		t.Errorf("legacy Resume not used: calls=%d id=%q", sp.calls, sp.gotSpawnID)
	}
	if out.SpawnID != "spawn-legacy" {
		t.Errorf("output passthrough wrong: %+v", out)
	}
}

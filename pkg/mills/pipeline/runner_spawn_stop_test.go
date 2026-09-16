package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type spawnStopFunc func(context.Context, string) error

func (f spawnStopFunc) Stop(ctx context.Context, id string) error { return f(ctx, id) }

func pendingSpawnRow(t *testing.T, st *store.Store, runID string) *store.StageResult {
	t.Helper()
	rows, err := st.Pipeline.ListStages(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Outcome != nil || rows[0].EndedAt != nil {
		t.Fatalf("expected one pending accepted worker, got %+v", rows)
	}
	return rows[0]
}

func TestRunner_StalledSpawnNeedsConfirmedStop(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &stalledSpawnDispatcher{stage: "implement"}
	stage := Stage{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing}
	r := New(st, nil, disp, nil)
	for tick := 0; tick < 3; tick++ {
		rows, err := st.Pipeline.ListStages(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		var pending *store.StageResult
		if len(rows) > 0 {
			pending = rows[0]
		}
		_, err = r.runStage(context.Background(), run, item, stage, nil, 1, pending)
		if !errors.Is(err, errStagePending) {
			t.Fatalf("tick %d: unsafe to retry without confirmed stop: %v", tick, err)
		}
		// A restarted runner must retain the same accepted worker.
		r = New(st, nil, disp, nil)
	}
	rows, err := st.Pipeline.ListStages(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Outcome != nil || rows[0].EndedAt != nil {
		t.Fatalf("accepted worker must remain pending: %+v", rows)
	}
	if _, fresh, _ := disp.snapshot(); fresh != 1 {
		t.Fatalf("fresh workers = %d, want one", fresh)
	}
}

func TestRunner_StalledSpawnWaitsForStopBeforeReplacement(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &stalledSpawnDispatcher{stage: "implement"}
	r := New(st, nil, disp, nil)
	r.Stages = []Stage{{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing}}
	r.RetryWait = func(context.Context, time.Duration) error { return nil }
	entered, release := make(chan string, 1), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.SpawnStopper = spawnStopFunc(func(stopCtx context.Context, id string) error {
		if _, ok := stopCtx.Deadline(); !ok {
			t.Error("stop must have a deadline")
		}
		entered <- id
		select {
		case <-release:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	})
	if err := r.Drive(ctx, run, item); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
		t.Fatal("first timeout must only park, not stop")
	default:
	}
	done := make(chan error, 1)
	go func() { done <- r.Drive(ctx, run, item) }()
	select {
	case id := <-entered:
		if id != "spawn-hung-1" {
			t.Errorf("stopped %q, want original worker", id)
		}
	case err := <-done:
		t.Fatalf("drive returned before stop acknowledgement: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if row := pendingSpawnRow(t, st, run.ID); row.Artifacts[spawnStopPendingArtifact] != true {
		t.Error("stop intent must be persisted before the remote stop")
	}
	if _, fresh, _ := disp.snapshot(); fresh != 1 {
		t.Errorf("replacement started before stop acknowledgement: %d workers", fresh)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, fresh, _ := disp.snapshot(); fresh != 2 {
		t.Fatalf("fresh workers after confirmed stop = %d, want 2", fresh)
	}
	rows, err := st.Pipeline.ListStages(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Artifacts["spawn_stop_confirmed"] != true || rows[0].Outcome == nil {
		t.Fatalf("missing stop evidence on retired attempt: %+v", rows)
	}
}

func TestRunner_StalledSpawnStopFailureResumesFence(t *testing.T) {
	for _, stopErr := range []error{errors.New("cleanup failed"), context.DeadlineExceeded} {
		t.Run(stopErr.Error(), func(t *testing.T) {
			st, run, item := newRunnerEnv(t)
			disp := &stalledSpawnDispatcher{stage: "implement"}
			stage := Stage{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing}
			r := New(st, nil, disp, nil)
			if _, err := r.runStage(t.Context(), run, item, stage, nil, 1, nil); !errors.Is(err, errStagePending) {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				// Reconstruct after each failure to prove the durable intent is
				// sufficient; no process-local state permits another writer.
				r = New(st, nil, disp, nil)
				r.SpawnStopper = spawnStopFunc(func(ctx context.Context, id string) error {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > stalledSpawnStopTimeout {
						t.Error("stop deadline is unbounded")
					}
					if id != "spawn-hung-1" {
						t.Errorf("stop id = %s", id)
					}
					return stopErr
				})
				if _, err := r.runStage(t.Context(), run, item, stage, nil, 1, pendingSpawnRow(t, st, run.ID)); !errors.Is(err, errStagePending) {
					t.Fatalf("failed stop must park: %v", err)
				}
			}
			if calls, fresh, _ := disp.snapshot(); calls != 2 || fresh != 1 {
				t.Fatalf("unconfirmed stop re-polled or replaced worker: calls=%d fresh=%d", calls, fresh)
			}
			r = New(st, nil, disp, nil)
			r.SpawnStopper = spawnStopFunc(func(context.Context, string) error { return nil })
			if _, err := r.runStage(t.Context(), run, item, stage, nil, 1, pendingSpawnRow(t, st, run.ID)); !errors.Is(err, ErrSpawnPollTimeout) {
				t.Fatalf("confirmed stop must release original failure for retry: %v", err)
			}
			rows, err := st.Pipeline.ListStages(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Artifacts["spawn_stop_confirmed"] != true || rows[0].Artifacts[spawnStopPendingArtifact] != false {
				t.Fatalf("missing durable stop evidence: %+v", rows)
			}
		})
	}
}

func TestRunner_StalledSpawnCancellationKeepsStopIntent(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &stalledSpawnDispatcher{stage: "implement"}
	stage := Stage{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing}
	r := New(st, nil, disp, nil)
	if _, err := r.runStage(t.Context(), run, item, stage, nil, 1, nil); !errors.Is(err, errStagePending) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.SpawnStopper = spawnStopFunc(func(context.Context, string) error {
		cancel() // rollout after cleanup, before recording the acknowledgement
		return nil
	})
	if _, err := r.runStage(ctx, run, item, stage, nil, 1, pendingSpawnRow(t, st, run.ID)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stop = %v", err)
	}
	if row := pendingSpawnRow(t, st, run.ID); row.Artifacts[spawnStopPendingArtifact] != true {
		t.Fatal("lost stop intent during cancellation")
	}
	if _, fresh, _ := disp.snapshot(); fresh != 1 {
		t.Fatalf("unexpected replacement: %d", fresh)
	}
}

func TestRunner_StalledSpawnStopPreservesExternalPause(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	disp := &stalledSpawnDispatcher{stage: "implement"}
	stage := Stage{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing}
	r := New(st, nil, disp, nil)
	if _, err := r.runStage(t.Context(), run, item, stage, nil, 1, nil); !errors.Is(err, errStagePending) {
		t.Fatal(err)
	}
	r.SpawnStopper = spawnStopFunc(func(ctx context.Context, _ string) error {
		paused, err := st.Pipeline.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		paused.State = store.PipelinePaused
		return st.Pipeline.PutRun(ctx, paused)
	})
	if _, err := r.runStage(t.Context(), run, item, stage, nil, 1, pendingSpawnRow(t, st, run.ID)); !errors.Is(err, errRunTerminated) {
		t.Fatalf("external pause during stop must end drive: %v", err)
	}
	pendingSpawnRow(t, st, run.ID)
	got, err := st.Pipeline.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.PipelinePaused {
		t.Fatalf("external pause overwritten: %s", got.State)
	}
}

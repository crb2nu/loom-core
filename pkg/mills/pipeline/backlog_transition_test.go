package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestRunnerMarkDone_PreservesMetadataEditedDuringRunForHooks(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	ctx := context.Background()
	fresh, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load metadata editor snapshot: %v", err)
	}
	fresh.Title = "fresh title from concurrent editor"
	fresh.Labels = []string{"fresh", "metadata"}
	fresh.Policy.AutoMerge = true
	if err := st.Backlog.Put(ctx, fresh); err != nil {
		t.Fatalf("persist concurrent metadata: %v", err)
	}

	runner := New(st, nil, nil, nil)
	hookCalled := false
	runner.OnMerged = func(_ context.Context, _ *store.PipelineRun, got *store.BacklogItem) error {
		hookCalled = true
		if got.Title != fresh.Title || !got.Policy.AutoMerge || len(got.Labels) != 2 {
			t.Errorf("OnMerged received stale metadata: %+v", got)
		}
		return nil
	}
	if err := runner.markDone(ctx, run, item); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if !hookCalled {
		t.Fatal("OnMerged was not called")
	}
	if item.State != store.BacklogMerged || item.Title != fresh.Title ||
		!item.Policy.AutoMerge || item.Revision <= fresh.Revision {
		t.Fatalf("runner item was not refreshed after state transition: %+v", item)
	}
}

func TestRunnerAutoRetry_PreservesMetadataEditedDuringRunForHook(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	ctx := context.Background()
	running, err := st.Backlog.TransitionState(
		ctx, item.ID, item.ClaimVersion, item.State, store.BacklogRunning,
	)
	if err != nil {
		t.Fatalf("mark backlog running: %v", err)
	}
	*item = *running

	fresh, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load metadata editor snapshot: %v", err)
	}
	fresh.Title = "fresh auto-retry title"
	fresh.Labels = []string{"retry", "fresh"}
	fresh.TargetProject = "services/fresh-target"
	if err := st.Backlog.Put(ctx, fresh); err != nil {
		t.Fatalf("persist concurrent metadata: %v", err)
	}

	runner := New(st, nil, nil, nil)
	runner.Policy = newPolicyMgrWithRetryCap(t, 2)
	hookCalled := false
	runner.OnAutoRetry = func(_ context.Context, _ *store.PipelineRun, got *store.BacklogItem) error {
		hookCalled = true
		if got.Title != fresh.Title || got.TargetProject != fresh.TargetProject || len(got.Labels) != 2 {
			t.Errorf("OnAutoRetry received stale metadata: %+v", got)
		}
		return nil
	}
	if handled := runner.maybeAutoRetry(
		ctx, run, item, ClassTransient,
		"stage implement errored after 9 total attempts (cap 9) [class=transient]: timeout",
	); !handled {
		t.Fatal("transient escalation was not auto-retried")
	}
	if !hookCalled {
		t.Fatal("OnAutoRetry was not called")
	}
	if item.State != store.BacklogQueued || item.Title != fresh.Title ||
		item.TargetProject != fresh.TargetProject || item.Revision <= fresh.Revision {
		t.Fatalf("auto-retry item was not refreshed after state transition: %+v", item)
	}
}

func TestRunnerSliceChild_DoesNotTransitionSharedBacklogOrFireParentHook(t *testing.T) {
	st, parent, item := newIntegratorEnv(t)
	ctx := context.Background()
	parentID := parent.ID
	child := &store.PipelineRun{
		ID:               parent.ID + "-slice-child",
		BacklogID:        parent.BacklogID,
		AggregateVersion: parent.AggregateVersion,
		Template:         parent.Template,
		State:            store.PipelineImplementing,
		CurrentStage:     "implement",
		Attempts:         1001,
		StartedAt:        parent.StartedAt,
		ParentRunID:      &parentID,
	}
	if err := st.Pipeline.PutRun(ctx, child); err != nil {
		t.Fatalf("seed slice child: %v", err)
	}

	runner := New(st, nil, nil, nil)
	hookCalled := false
	runner.OnMerged = func(context.Context, *store.PipelineRun, *store.BacklogItem) error {
		hookCalled = true
		return nil
	}
	subItem := *item
	if err := runner.markDone(ctx, child, &subItem); err != nil {
		t.Fatalf("mark slice child done: %v", err)
	}
	if hookCalled {
		t.Fatal("slice child fired parent OnMerged hook")
	}
	stored, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load shared backlog: %v", err)
	}
	if stored.State != item.State || stored.Revision != item.Revision {
		t.Fatalf("slice child mutated shared backlog: before=%+v after=%+v", item, stored)
	}
}

func TestRunnerSliceChildEscalation_DoesNotTransitionSharedBacklogOrFireParentHook(t *testing.T) {
	st, parent, item := newIntegratorEnv(t)
	ctx := context.Background()
	parentID := parent.ID
	child := &store.PipelineRun{
		ID:               parent.ID + "-slice-escalated",
		BacklogID:        parent.BacklogID,
		AggregateVersion: parent.AggregateVersion,
		Template:         parent.Template,
		State:            store.PipelineImplementing,
		CurrentStage:     "implement",
		Attempts:         1002,
		StartedAt:        parent.StartedAt,
		ParentRunID:      &parentID,
	}
	if err := st.Pipeline.PutRun(ctx, child); err != nil {
		t.Fatalf("seed slice child: %v", err)
	}

	runner := New(st, nil, nil, nil)
	hookCalled := false
	runner.OnEscalated = func(context.Context, *store.PipelineRun, *store.BacklogItem) error {
		hookCalled = true
		return nil
	}
	subItem := *item
	if err := runner.escalateWithItem(ctx, child, &subItem, ClassCode, "[class=code]: compile failure"); err != nil {
		t.Fatalf("escalate slice child: %v", err)
	}
	if hookCalled {
		t.Fatal("slice child fired parent OnEscalated hook")
	}
	stored, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load shared backlog: %v", err)
	}
	if stored.State != item.State || stored.Revision != item.Revision {
		t.Fatalf("slice child mutated shared backlog: before=%+v after=%+v", item, stored)
	}
}

func TestIntegratorSliceChildren_InheritParentAggregateVersion(t *testing.T) {
	st, _, item := newIntegratorEnv(t)
	ctx := context.Background()
	parent := &store.PipelineRun{
		ID: "PIPE-BL-PAR-VERSIONED", BacklogID: item.ID, AggregateVersion: 5,
		Template: "mills-default-pipeline", State: store.PipelineQueued,
		Attempts: 2, StartedAt: time.Now().UTC(),
	}
	if err := st.Pipeline.PutRun(ctx, parent); err != nil {
		t.Fatalf("seed versioned parent: %v", err)
	}
	integrator := NewIntegrator(
		st,
		&recordingSubRunner{store: st},
		&fakeAllocator{},
		&fakeMerger{sha: "integrated"},
	)
	if err := integrator.Run(ctx, parent, item); err != nil {
		t.Fatalf("run integrator: %v", err)
	}
	runs, err := st.Pipeline.ListByBacklog(ctx, item.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	children := 0
	for _, run := range runs {
		if run.ParentRunID == nil || *run.ParentRunID != parent.ID {
			continue
		}
		children++
		if run.AggregateVersion != parent.AggregateVersion {
			t.Fatalf("child %s aggregate=%d want parent aggregate %d",
				run.ID, run.AggregateVersion, parent.AggregateVersion)
		}
	}
	if children != len(item.Slices) {
		t.Fatalf("versioned slice children=%d want %d", children, len(item.Slices))
	}
}

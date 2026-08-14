package store

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func putCASHardeningBacklog(t *testing.T, st *Store, id string) *BacklogItem {
	t.Helper()
	item := &BacklogItem{
		ID:        id,
		Title:     id,
		State:     BacklogQueued,
		Priority:  P1,
		CreatedBy: "test",
	}
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("put backlog %s: %v", id, err)
	}
	return item
}

func TestBacklogPut_CanonicalizesInsertVersionsAndRejectsOverflow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := &BacklogItem{
		ID:           "MILLS-CAS-CANONICAL",
		Title:        "canonical versions",
		State:        BacklogQueued,
		Priority:     P1,
		CreatedBy:    "test",
		ClaimVersion: 17,
		Revision:     9,
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("canonical insert: %v", err)
	}
	if item.ClaimVersion != 0 || item.Revision != 1 {
		t.Fatalf("insert versions = claim %d revision %d, want 0/1", item.ClaimVersion, item.Revision)
	}

	item.Title = "guarded update"
	item.ClaimVersion = 23
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("guarded update: %v", err)
	}
	if item.ClaimVersion != 0 || item.Revision != 2 {
		t.Fatalf("update versions = claim %d revision %d, want 0/2", item.ClaimVersion, item.Revision)
	}
	stored, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("load canonical item: %v", err)
	}
	if stored.Title != "guarded update" || stored.ClaimVersion != 0 || stored.Revision != 2 {
		t.Fatalf("stored canonical item: %+v", stored)
	}

	for _, tc := range []struct {
		name string
		item *BacklogItem
	}{
		{
			name: "revision",
			item: &BacklogItem{ID: "MILLS-CAS-REV-OVERFLOW", Title: "overflow", State: BacklogQueued,
				Priority: P1, CreatedBy: "test", Revision: math.MaxInt64},
		},
		{
			name: "claim_version",
			item: &BacklogItem{ID: "MILLS-CAS-CLAIM-OVERFLOW", Title: "overflow", State: BacklogQueued,
				Priority: P1, CreatedBy: "test", ClaimVersion: math.MaxInt64},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.Backlog.Put(ctx, tc.item); err == nil {
				t.Fatal("overflow version was accepted")
			}
			if _, err := st.Backlog.Get(ctx, tc.item.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("overflow insert persisted: %v", err)
			}
		})
	}
}

func TestPipelinePutRun_RequiresExactAggregateVersion(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-PIPELINE")
	versioned := &PipelineRun{
		ID:               "PIPE-CAS-VERSIONED",
		BacklogID:        item.ID,
		AggregateVersion: 7,
		Template:         "test",
		State:            PipelineImplementing,
		Attempts:         1,
		StartedAt:        time.Now().UTC(),
		CostUSD:          0.5,
	}
	if err := st.Pipeline.PutRun(ctx, versioned); err != nil {
		t.Fatalf("put versioned run: %v", err)
	}
	stale := *versioned
	stale.AggregateVersion = 0
	stale.State = PipelineDone
	stale.CostUSD = 2
	if err := st.Pipeline.PutRun(ctx, &stale); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("zero-version terminal write error = %v, want ErrStaleWrite", err)
	}
	stored, err := st.Pipeline.GetRun(ctx, versioned.ID)
	if err != nil {
		t.Fatalf("load versioned run: %v", err)
	}
	if stored.State != PipelineImplementing || stored.AggregateVersion != 7 || stored.CostUSD != 0.5 {
		t.Fatalf("zero-version write changed versioned run: %+v", stored)
	}

	legacy := &PipelineRun{
		ID:        "PIPE-CAS-LEGACY",
		BacklogID: item.ID,
		Template:  "legacy",
		State:     PipelineImplementing,
		Attempts:  2,
		StartedAt: time.Now().UTC(),
	}
	if err := st.Pipeline.PutRun(ctx, legacy); err != nil {
		t.Fatalf("put legacy run: %v", err)
	}
	legacy.State = PipelineTesting
	legacy.CostUSD = 1
	if err := st.Pipeline.PutRun(ctx, legacy); err != nil {
		t.Fatalf("update legacy run: %v", err)
	}
}

func TestBacklogTransitionState_PreservesConcurrentMetadataAndIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-STATE")
	item.State = BacklogRunning
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	longLived := *item
	item.Title = "metadata edited during run"
	item.Labels = []string{"fresh", "policy"}
	item.Policy.AutoMerge = true
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("metadata edit: %v", err)
	}
	metadataRevision := item.Revision

	transitioned, err := st.Backlog.TransitionState(
		ctx, longLived.ID, longLived.ClaimVersion, longLived.State, BacklogMerged,
	)
	if err != nil {
		t.Fatalf("state-only transition: %v", err)
	}
	if transitioned.State != BacklogMerged || transitioned.Revision != metadataRevision+1 ||
		transitioned.Title != item.Title || !transitioned.Policy.AutoMerge || len(transitioned.Labels) != 2 {
		t.Fatalf("transition clobbered concurrent metadata: %+v", transitioned)
	}

	repeated, err := st.Backlog.TransitionState(
		ctx, longLived.ID, longLived.ClaimVersion, longLived.State, BacklogMerged,
	)
	if err != nil {
		t.Fatalf("idempotent transition: %v", err)
	}
	if repeated.Revision != transitioned.Revision || repeated.State != BacklogMerged {
		t.Fatalf("idempotent transition changed row: before=%+v after=%+v", transitioned, repeated)
	}
	if _, err := st.Backlog.TransitionState(
		ctx, longLived.ID, longLived.ClaimVersion+1, BacklogMerged, BacklogQueued,
	); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("wrong aggregate transition error=%v want ErrStaleWrite", err)
	}
}

func TestBacklogTransitionStateWithEvent_RollsBackTogether(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-EVENT-ROLLBACK")
	item.State = BacklogEscalated
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("mark escalated: %v", err)
	}
	revision := item.Revision

	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_atomic_requeue_event
		BEFORE INSERT ON events
		WHEN NEW.kind = 'test.atomic_requeue'
		BEGIN SELECT RAISE(ABORT, 'injected event failure'); END;
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	_, err := st.Backlog.TransitionStateWithEvent(
		ctx, item.ID, item.ClaimVersion, BacklogEscalated, BacklogQueued,
		&Event{Actor: "test", Kind: "test.atomic_requeue", SubjectKind: "backlog_item", SubjectID: item.ID},
	)
	if err == nil {
		t.Fatal("event insert failure unexpectedly committed")
	}

	stored, err := st.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("reload backlog: %v", err)
	}
	if stored.State != BacklogEscalated || stored.Revision != revision {
		t.Fatalf("failed event did not roll back state: %+v", stored)
	}
	count, err := st.Events.CountBySubjectKind(ctx, "backlog_item", item.ID, "test.atomic_requeue")
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed transaction persisted %d events, want 0", count)
	}
}

func TestBacklogTransitionStateWithEventOnce_CancellationIsAtomic(t *testing.T) {
	points := []transitionStateWithEventOnceFaultPoint{
		transitionEventOnceAfterBacklog,
		transitionEventOnceAfterEvent,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			st := newTestStore(t)
			seedCtx := context.Background()
			item := putCASHardeningBacklog(t, st, "MILLS-CAS-ONCE-"+string(point))
			item.State = BacklogEscalated
			if err := st.Backlog.Put(seedCtx, item); err != nil {
				t.Fatalf("mark escalated: %v", err)
			}
			originalRevision := item.Revision
			event := &Event{
				Actor: "reconciler", Kind: "reconciler.ghost_spark_closed",
				SubjectKind: "pipeline_run", SubjectID: "PIPE-" + item.ID,
			}

			ctx, cancel := context.WithCancel(context.Background())
			_, inserted, err := st.Backlog.transitionStateWithEventOnce(
				ctx, item.ID, item.ClaimVersion, BacklogEscalated, BacklogMerged, event,
				func(got transitionStateWithEventOnceFaultPoint) error {
					if got != point {
						return nil
					}
					cancel()
					return nil
				},
			)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("transition error = %v, want context canceled", err)
			}
			if inserted || event.ID != 0 {
				t.Fatalf("rolled-back event: inserted=%v id=%d, want false/0", inserted, event.ID)
			}
			stored, getErr := st.Backlog.Get(context.Background(), item.ID)
			if getErr != nil {
				t.Fatalf("reload backlog: %v", getErr)
			}
			if stored.State != BacklogEscalated || stored.Revision != originalRevision {
				t.Fatalf("canceled transaction changed backlog: %+v", stored)
			}
			count, countErr := st.Events.CountBySubjectKind(
				context.Background(), event.SubjectKind, event.SubjectID, event.Kind,
			)
			if countErr != nil || count != 0 {
				t.Fatalf("events after cancellation = %d, err=%v, want 0", count, countErr)
			}

			updated, inserted, err := st.Backlog.TransitionStateWithEventOnce(
				context.Background(), item.ID, item.ClaimVersion,
				BacklogEscalated, BacklogMerged, event,
			)
			if err != nil || !inserted || updated.State != BacklogMerged || event.ID == 0 {
				t.Fatalf("retry result: item=%+v inserted=%v event_id=%d err=%v", updated, inserted, event.ID, err)
			}
			revisionAfterCommit := updated.Revision
			repeated, inserted, err := st.Backlog.TransitionStateWithEventOnce(
				context.Background(), item.ID, item.ClaimVersion,
				BacklogEscalated, BacklogMerged, event,
			)
			if err != nil || inserted || repeated.Revision != revisionAfterCommit {
				t.Fatalf("idempotent repeat: item=%+v inserted=%v err=%v", repeated, inserted, err)
			}
			count, countErr = st.Events.CountBySubjectKind(
				context.Background(), event.SubjectKind, event.SubjectID, event.Kind,
			)
			if countErr != nil || count != 1 {
				t.Fatalf("events after retry/repeat = %d, err=%v, want 1", count, countErr)
			}
		})
	}
}

func TestBacklogTransitionStateWithEventOnce_ConcurrentFirstWriter(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-ONCE-CONCURRENT")
	item.State = BacklogEscalated
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("mark escalated: %v", err)
	}
	originalRevision := item.Revision

	type result struct {
		item     *BacklogItem
		inserted bool
		eventID  int64
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, actor := range []string{"reconciler-a", "reconciler-b"} {
		wg.Add(1)
		go func(actor string) {
			defer wg.Done()
			<-start
			event := &Event{
				Actor: actor, Kind: "reconciler.ghost_spark_closed",
				SubjectKind: "pipeline_run", SubjectID: "PIPE-" + item.ID,
			}
			updated, inserted, err := st.Backlog.TransitionStateWithEventOnce(
				ctx, item.ID, item.ClaimVersion, BacklogEscalated, BacklogMerged, event,
			)
			results <- result{item: updated, inserted: inserted, eventID: event.ID, err: err}
		}(actor)
	}
	close(start)
	wg.Wait()
	close(results)

	insertedCount := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent transition: %v", got.err)
		}
		if got.item == nil || got.item.State != BacklogMerged {
			t.Fatalf("concurrent transition item = %+v, want merged", got.item)
		}
		if got.inserted {
			insertedCount++
			if got.eventID == 0 {
				t.Fatal("first writer returned a zero event id")
			}
		} else if got.eventID != 0 {
			t.Fatalf("non-writer event id = %d, want 0", got.eventID)
		}
	}
	if insertedCount != 1 {
		t.Fatalf("concurrent first writers = %d, want 1", insertedCount)
	}
	stored, err := st.Backlog.Get(ctx, item.ID)
	if err != nil || stored.State != BacklogMerged || stored.Revision != originalRevision+1 {
		t.Fatalf("stored backlog after race = %+v, err=%v", stored, err)
	}
	count, err := st.Events.CountBySubjectKind(
		ctx, "pipeline_run", "PIPE-"+item.ID, "reconciler.ghost_spark_closed",
	)
	if err != nil || count != 1 {
		t.Fatalf("events after race = %d, err=%v, want 1", count, err)
	}
}

func TestPipelinePutRun_RowRevisionRejectsStaleProgress(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-ROW-REVISION")
	run := &PipelineRun{
		ID: "PIPE-CAS-ROW-REVISION", BacklogID: item.ID, AggregateVersion: 3,
		Template: "test", State: PipelineImplementing, CurrentStage: "implement",
		Attempts: 1, StartedAt: time.Now().UTC(), CostUSD: 0.5,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if run.Revision != 1 {
		t.Fatalf("insert revision=%d want 1", run.Revision)
	}
	stale := *run
	run.State = PipelineTesting
	run.CurrentStage = "test"
	run.CostUSD = 1.25
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("advance run: %v", err)
	}
	if run.Revision != 2 {
		t.Fatalf("advanced revision=%d want 2", run.Revision)
	}
	stale.State = PipelinePlanning
	stale.CurrentStage = "plan"
	stale.CostUSD = 0.1
	if err := st.Pipeline.PutRun(ctx, &stale); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale regression error=%v want ErrStaleWrite", err)
	}
	stored, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("load progressed run: %v", err)
	}
	if stored.Revision != 2 || stored.State != PipelineTesting || stored.CurrentStage != "test" || stored.CostUSD != 1.25 {
		t.Fatalf("stale writer regressed progress: %+v", stored)
	}
}

func TestPipelinePutRun_ConcurrentRowRevisionExactlyOneWinner(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-ROW-RACE")
	base := &PipelineRun{
		ID: "PIPE-CAS-ROW-RACE", BacklogID: item.ID, AggregateVersion: 4,
		Template: "test", State: PipelineImplementing, CurrentStage: "implement",
		Attempts: 1, StartedAt: time.Now().UTC(),
	}
	if err := st.Pipeline.PutRun(ctx, base); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	left, right := *base, *base
	left.State, left.CurrentStage, left.CostUSD = PipelineTesting, "test", 1
	right.State, right.CurrentStage, right.CostUSD = PipelineReviewing, "review", 2

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []*PipelineRun{&left, &right} {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- st.Pipeline.PutRun(ctx, candidate)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes, staleWrites := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrStaleWrite):
			staleWrites++
		default:
			t.Fatalf("unexpected writer error: %v", err)
		}
	}
	if successes != 1 || staleWrites != 1 {
		t.Fatalf("writer outcomes successes=%d stale=%d want 1/1", successes, staleWrites)
	}
	stored, err := st.Pipeline.GetRun(ctx, base.ID)
	if err != nil {
		t.Fatalf("load winner: %v", err)
	}
	if stored.Revision != 2 ||
		!((stored.State == PipelineTesting && stored.CurrentStage == "test" && stored.CostUSD == 1) ||
			(stored.State == PipelineReviewing && stored.CurrentStage == "review" && stored.CostUSD == 2)) {
		t.Fatalf("stored row is not one complete winner: %+v", stored)
	}
}

func TestPipelineCreateSubrun_RejectsInvalidVersionDepthAndCost(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-SUBRUN")
	parent := &PipelineRun{
		ID:        "PIPE-CAS-PARENT",
		BacklogID: item.ID,
		Template:  "test",
		State:     PipelineImplementing,
		Attempts:  1,
		StartedAt: time.Now().UTC(),
	}
	if err := st.Pipeline.PutRun(ctx, parent); err != nil {
		t.Fatalf("put parent: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*PipelineRun)
	}{
		{name: "aggregate_version", mutate: func(run *PipelineRun) { run.AggregateVersion = -1 }},
		{name: "depth", mutate: func(run *PipelineRun) { run.Depth = -1 }},
		{name: "negative_cost", mutate: func(run *PipelineRun) { run.CostUSD = -0.01 }},
		{name: "nan_cost", mutate: func(run *PipelineRun) { run.CostUSD = math.NaN() }},
		{name: "infinite_cost", mutate: func(run *PipelineRun) { run.CostUSD = math.Inf(1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := &PipelineRun{
				ID:          "PIPE-CAS-CHILD-" + tc.name,
				BacklogID:   item.ID,
				Template:    "test",
				State:       PipelineQueued,
				Attempts:    2,
				StartedAt:   time.Now().UTC(),
				ParentRunID: &parent.ID,
				Depth:       1,
			}
			tc.mutate(run)
			if err := st.Pipeline.CreateSubrun(ctx, run); err == nil {
				t.Fatal("invalid subrun was accepted")
			}
			if _, err := st.Pipeline.GetRun(ctx, run.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("invalid subrun persisted: %v", err)
			}
		})
	}

	valid := &PipelineRun{
		ID:          "PIPE-CAS-CHILD-VALID",
		BacklogID:   item.ID,
		Template:    "test",
		State:       PipelineQueued,
		Attempts:    2,
		StartedAt:   time.Now().UTC(),
		ParentRunID: &parent.ID,
		Depth:       1,
	}
	if err := st.Pipeline.CreateSubrun(ctx, valid); err != nil {
		t.Fatalf("valid legacy subrun: %v", err)
	}
	if valid.Revision != 1 {
		t.Fatalf("created subrun revision=%d want 1", valid.Revision)
	}
}

func TestPipelineTerminal_NormalizesAndPreservesEndedAt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	item := putCASHardeningBacklog(t, st, "MILLS-CAS-TERMINAL")
	claim, err := st.ClaimPipelineStart(ctx, ClaimPipelineStartRequest{
		BacklogID:            item.ID,
		ExpectedClaimVersion: item.ClaimVersion,
		ExpectedRevision:     item.Revision,
		Template:             "test",
		Now:                  time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("claim pipeline: %v", err)
	}
	if claim.Run.Revision != 1 {
		t.Fatalf("claimed run revision=%d want 1", claim.Run.Revision)
	}
	claim.Run.State = PipelineDone
	claim.Run.EndedAt = nil
	claim.Run.CostUSD = 1.5
	if err := st.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("terminal put without ended_at: %v", err)
	}
	if claim.Run.EndedAt == nil {
		t.Fatal("terminal put did not normalize ended_at")
	}
	firstEndedAt := *claim.Run.EndedAt

	repeat := *claim.Run
	repeat.EndedAt = nil
	repeat.CostUSD = 2
	if err := st.Pipeline.PutRun(ctx, &repeat); err != nil {
		t.Fatalf("repeated terminal put: %v", err)
	}
	if repeat.EndedAt == nil || !repeat.EndedAt.Equal(firstEndedAt) {
		t.Fatalf("repeated terminal ended_at = %v, want %v", repeat.EndedAt, firstEndedAt)
	}
	stored, err := st.Pipeline.GetRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("load terminal pipeline: %v", err)
	}
	workflow, err := st.Workflow.GetWorkflowRun(ctx, claim.Run.ID)
	if err != nil {
		t.Fatalf("load terminal workflow: %v", err)
	}
	if stored.EndedAt == nil || workflow.EndedAt == nil ||
		!stored.EndedAt.Equal(firstEndedAt) || !workflow.EndedAt.Equal(firstEndedAt) {
		t.Fatalf("terminal timestamps diverged: pipeline=%v workflow=%v want=%v",
			stored.EndedAt, workflow.EndedAt, firstEndedAt)
	}
	if stored.CostUSD != 2 || workflow.CostUSD != 2 {
		t.Fatalf("terminal costs = pipeline %v workflow %v, want 2", stored.CostUSD, workflow.CostUSD)
	}
}

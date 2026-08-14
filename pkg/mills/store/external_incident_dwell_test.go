package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalIncidentDwellPersistsAndCompletesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mills.db")
	ctx := context.Background()
	st, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	item := &BacklogItem{ID: "ITEM-DWELL", Title: "dwell", State: BacklogEscalated, Priority: P1, CreatedBy: "test"}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatal(err)
	}
	retryable := true
	run := &PipelineRun{
		ID: "RUN-DWELL", BacklogID: item.ID, Template: "test", State: PipelineEscalated,
		StartedAt: start.Add(-time.Minute), ExternalDependencyID: "external_dependency.gitlab",
		ExternalDependency: "gitlab", EscalationClass: "infra", FailureClass: "infrastructure",
		EscalationRetryable: &retryable,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	deadline := start.Add(30 * time.Minute)
	if _, err := st.Pipeline.BeginExternalIncidentDwell(ctx, run.ID, run.ExternalDependencyID, run.ExternalDependency, start, deadline); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Pipeline.GetExternalIncidentDwell(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(start) || !got.DeadlineAt.Equal(deadline) || got.DependencyID != run.ExternalDependencyID || got.Dependency != "gitlab" {
		t.Fatalf("reopened dwell = %+v", got)
	}
	final, won, err := st.Pipeline.CompleteExternalIncidentDwell(ctx, run.ID, ExternalIncidentDwellTimeout, deadline)
	if err != nil || !won {
		t.Fatalf("timeout completion: dwell=%+v won=%v err=%v", final, won, err)
	}
	late, won, err := st.Pipeline.CompleteExternalIncidentDwell(ctx, run.ID, ExternalIncidentDwellFastKill, deadline.Add(time.Minute))
	if err != nil || won || late.CompletionReason != ExternalIncidentDwellTimeout {
		t.Fatalf("late fast-kill diverged: dwell=%+v won=%v err=%v", late, won, err)
	}
	if final.ElapsedDuration < 30*time.Minute-time.Second || final.CompletedAt == nil {
		t.Fatalf("completed dwell = %+v", final)
	}
	persisted, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.FailureClass != "infrastructure" || persisted.ExternalDependencyID != run.ExternalDependencyID || persisted.EscalationRetryable == nil || !*persisted.EscalationRetryable {
		t.Fatalf("incident metadata changed: %+v", persisted)
	}
}

func TestExternalIncidentDwellFastKillWinsBeforeTimeout(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if err := st.Backlog.Put(ctx, &BacklogItem{ID: "ITEM-KILL", Title: "kill", State: BacklogEscalated, Priority: P1, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{ID: "RUN-KILL", BacklogID: "ITEM-KILL", Template: "test", State: PipelineEscalated, StartedAt: start, ExternalDependency: "gitlab"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pipeline.BeginExternalIncidentDwell(ctx, "RUN-KILL", "", "gitlab", start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, won, err := st.Pipeline.CompleteExternalIncidentDwell(ctx, "RUN-KILL", ExternalIncidentDwellFastKill, start.Add(30*time.Minute))
	if err != nil || !won {
		t.Fatalf("fast kill: won=%v err=%v", won, err)
	}
	final, won, err := st.Pipeline.CompleteExternalIncidentDwell(ctx, "RUN-KILL", ExternalIncidentDwellTimeout, start.Add(time.Hour))
	if err != nil || won || final.CompletionReason != ExternalIncidentDwellFastKill {
		t.Fatalf("late timeout diverged: dwell=%+v won=%v err=%v", final, won, err)
	}
}

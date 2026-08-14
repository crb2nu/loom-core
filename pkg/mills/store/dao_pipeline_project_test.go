package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedProjectTestRun(t *testing.T, st *Store, runID string) {
	t.Helper()
	ctx := context.Background()
	backlogID := "BL-" + runID
	if err := st.Backlog.Put(ctx, &BacklogItem{
		ID: backlogID, Title: runID, State: BacklogQueued, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("put backlog %s: %v", backlogID, err)
	}
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{
		ID: runID, BacklogID: backlogID, Template: "mills-default-pipeline",
		State: PipelineRunning(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put run %s: %v", runID, err)
	}
}

func TestPipelineAuthorizedProject(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedProjectTestRun(t, st, "PIPE-PROJECT")
	success := StageOutcomeSuccess
	for attempt, stage := range []struct {
		name string
		key  string
	}{
		{"mr", "mr_project"},
		{"ci_watch", "ci_project"},
		{"merge", "merged_project"},
		{"cleanup", "cleanup_project"},
	} {
		ended := time.Now().UTC()
		if err := st.Pipeline.PutStage(ctx, &StageResult{
			PipelineRunID: "PIPE-PROJECT",
			Stage:         stage.name,
			Attempt:       attempt + 1,
			StartedAt:     ended.Add(-time.Second),
			EndedAt:       &ended,
			Outcome:       &success,
			Artifacts:     map[string]any{stage.key: "services/loom-core"},
		}); err != nil {
			t.Fatalf("put %s stage: %v", stage.name, err)
		}
	}
	got, err := st.Pipeline.AuthorizedProject(ctx, "PIPE-PROJECT")
	if err != nil || got != "services/loom-core" {
		t.Fatalf("AuthorizedProject = %q, %v", got, err)
	}
}

func TestPipelineAuthorizedProjectRejectsMissingOrConflictingProvenance(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.Pipeline.AuthorizedProject(ctx, "PIPE-LEGACY"); !errors.Is(err, ErrPipelineProjectUnavailable) {
		t.Fatalf("legacy project error = %v", err)
	}
	seedProjectTestRun(t, st, "PIPE-CONFLICT")
	success := StageOutcomeSuccess
	ended := time.Now().UTC()
	for attempt, row := range []struct {
		stage   string
		key     string
		project string
	}{
		{"mr", "mr_project", "services/a"},
		{"ci_watch", "ci_project", "services/b"},
	} {
		if err := st.Pipeline.PutStage(ctx, &StageResult{
			PipelineRunID: "PIPE-CONFLICT",
			Stage:         row.stage,
			Attempt:       attempt + 1,
			StartedAt:     ended.Add(-time.Second),
			EndedAt:       &ended,
			Outcome:       &success,
			Artifacts:     map[string]any{row.key: row.project},
		}); err != nil {
			t.Fatalf("put %s stage: %v", row.stage, err)
		}
	}
	if _, err := st.Pipeline.AuthorizedProject(ctx, "PIPE-CONFLICT"); !errors.Is(err, ErrPipelineProjectUnavailable) {
		t.Fatalf("conflicting project error = %v", err)
	}
}

package squads

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedRoutedEvent is a small helper that emits the attribution event
// shape the reconciler writes at routing time. Mirrors the mills package's
// reconciler.routeToSquad payload precisely so the recorder's lookup
// finds it via the indexed (subject_kind, subject_id) tuple.
func seedRoutedEvent(t *testing.T, st *store.Store, runID string, payload map[string]any) {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	payload["run_id"] = runID
	payload["outcome"] = "ok"
	if err := st.Events.Append(context.Background(), &store.Event{
		Actor:       "reconciler",
		Kind:        SquadRoutedEventKind,
		SubjectKind: SquadRoutedSubjectKind,
		SubjectID:   runID,
		Payload:     payload,
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

// seedRunForRecorder seeds the council + backlog + pipeline_run rows the
// recorder's OnMerged needs to satisfy FK constraints + give the
// squad_outcomes row a real run id to attach to.
func seedRunForRecorder(t *testing.T, st *store.Store, runID, backlogID string, attempt int) (*store.PipelineRun, *store.BacklogItem) {
	t.Helper()
	ctx := context.Background()
	council := "COUNCIL-RECORDER"
	if err := st.Council.Put(ctx, &store.CouncilRun{
		ID: council, Trigger: store.CouncilTriggerCron,
		StartedAt: time.Now().UTC(), Outcome: store.CouncilOutcomeSuccess,
	}); err != nil {
		t.Fatalf("seed council: %v", err)
	}
	item := &store.BacklogItem{
		ID: backlogID, Title: "recorder test", State: store.BacklogMerged,
		Priority: store.P2, CreatedBy: "test", CouncilRunID: &council,
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	end := time.Now().UTC()
	start := end.Add(-7 * time.Minute)
	run := &store.PipelineRun{
		ID:        runID,
		BacklogID: backlogID,
		Template:  "mills-default-pipeline",
		State:     store.PipelineDone,
		Attempts:  attempt,
		StartedAt: start,
		EndedAt:   &end,
		CostUSD:   0.42,
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return run, item
}

func TestOutcomeRecorder_HappyPath(t *testing.T) {
	st := newTestStore(t)
	if err := st.Squads.PutSquad(context.Background(), &store.Squad{
		Name: "hud-frontend", Enabled: true,
	}); err != nil {
		t.Fatalf("seed squad row: %v", err)
	}
	run, item := seedRunForRecorder(t, st, "PIPE-OK-1", "MILLS-OK-1", 1)
	seedRoutedEvent(t, st, run.ID, map[string]any{
		"backlog_id":  item.ID,
		"squad_name":  "hud-frontend",
		"path_class":  "internal/hud/frontend/**",
		"confidence":  0.74,
		"sample_size": 12.0,
	})

	rec := NewOutcomeRecorder(st)
	if err := rec.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged: %v", err)
	}
	rows, err := st.Squads.ListOutcomes(context.Background(), "hud-frontend", 10)
	if err != nil {
		t.Fatalf("list outcomes: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 outcome row, got %d", len(rows))
	}
	got := rows[0]
	if got.PipelineRunID != run.ID {
		t.Errorf("pipeline_run_id: got %q want %q", got.PipelineRunID, run.ID)
	}
	if got.Outcome != store.SquadOutcomeMergedClean {
		t.Errorf("outcome: got %v want merged_clean", got.Outcome)
	}
	if got.PathClass != "internal/hud/frontend/**" {
		t.Errorf("path_class: %q", got.PathClass)
	}
	if got.CostUSD != run.CostUSD {
		t.Errorf("cost_usd: got %v want %v", got.CostUSD, run.CostUSD)
	}
	if got.DurationSeconds <= 0 {
		t.Errorf("duration_seconds: got %d want >0", got.DurationSeconds)
	}
}

func TestOutcomeRecorder_NoEventIsNoOp(t *testing.T) {
	st := newTestStore(t)
	run, item := seedRunForRecorder(t, st, "PIPE-NOEVT-1", "MILLS-NOEVT-1", 1)

	rec := NewOutcomeRecorder(st)
	if err := rec.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged: %v", err)
	}
	// No squad event seeded → no outcome row written.
	rows, _ := st.Squads.ListOutcomes(context.Background(), "hud-frontend", 10)
	if len(rows) != 0 {
		t.Errorf("expected zero outcomes for unrouted run, got %d", len(rows))
	}
}

func TestOutcomeRecorder_FallbackSquadIsNoOp(t *testing.T) {
	st := newTestStore(t)
	run, item := seedRunForRecorder(t, st, "PIPE-FB-1", "MILLS-FB-1", 1)
	seedRoutedEvent(t, st, run.ID, map[string]any{
		"squad_name": FallbackName,
		"path_class": "",
		"confidence": 0.0,
	})

	rec := NewOutcomeRecorder(st)
	if err := rec.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged: %v", err)
	}
	// _default routing must not create attribution rows.
	rows, _ := st.Squads.ListOutcomes(context.Background(), FallbackName, 10)
	if len(rows) != 0 {
		t.Errorf("expected zero outcomes for fallback routing, got %d", len(rows))
	}
}

func TestOutcomeRecorder_DoubleFireIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	if err := st.Squads.PutSquad(context.Background(), &store.Squad{
		Name: "hud-frontend", Enabled: true,
	}); err != nil {
		t.Fatalf("seed squad row: %v", err)
	}
	run, item := seedRunForRecorder(t, st, "PIPE-DBL-1", "MILLS-DBL-1", 1)
	seedRoutedEvent(t, st, run.ID, map[string]any{
		"squad_name": "hud-frontend", "path_class": "internal/hud/frontend/**",
	})

	rec := NewOutcomeRecorder(st)
	if err := rec.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("first OnMerged: %v", err)
	}
	if err := rec.OnMerged(context.Background(), run, item); err != nil {
		t.Errorf("second OnMerged should be idempotent (UNIQUE on pipeline_run_id swallowed); got %v", err)
	}
	rows, _ := st.Squads.ListOutcomes(context.Background(), "hud-frontend", 10)
	if len(rows) != 1 {
		t.Errorf("expected exactly 1 outcome after double-fire, got %d", len(rows))
	}
}

func TestOutcomeRecorder_NilStoreReturnsError(t *testing.T) {
	rec := NewOutcomeRecorder(nil)
	if err := rec.OnMerged(context.Background(), &store.PipelineRun{ID: "x"}, nil); err == nil {
		t.Error("nil store should error")
	}
}

func TestOutcomeRecorder_OnEscalatedRecordsFailed(t *testing.T) {
	st := newTestStore(t)
	if err := st.Squads.PutSquad(context.Background(), &store.Squad{
		Name: "hud-frontend", Enabled: true,
	}); err != nil {
		t.Fatalf("seed squad row: %v", err)
	}
	run, item := seedRunForRecorder(t, st, "PIPE-ESC-1", "MILLS-ESC-1", 1)
	seedRoutedEvent(t, st, run.ID, map[string]any{
		"squad_name": "hud-frontend", "path_class": "internal/hud/frontend/**",
	})

	rec := NewOutcomeRecorder(st)
	if err := rec.OnEscalated(context.Background(), run, item); err != nil {
		t.Fatalf("OnEscalated: %v", err)
	}
	rows, err := st.Squads.ListOutcomes(context.Background(), "hud-frontend", 10)
	if err != nil {
		t.Fatalf("list outcomes: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 outcome row, got %d", len(rows))
	}
	if rows[0].Outcome != store.SquadOutcomeFailed {
		t.Errorf("outcome: got %v want failed", rows[0].Outcome)
	}
	if rows[0].PipelineRunID != run.ID {
		t.Errorf("pipeline_run_id: got %q want %q", rows[0].PipelineRunID, run.ID)
	}
}

func TestOutcomeRecorder_OnEscalatedNoEventIsNoOp(t *testing.T) {
	st := newTestStore(t)
	run, item := seedRunForRecorder(t, st, "PIPE-ESC-2", "MILLS-ESC-2", 1)

	rec := NewOutcomeRecorder(st)
	if err := rec.OnEscalated(context.Background(), run, item); err != nil {
		t.Fatalf("OnEscalated: %v", err)
	}
	rows, _ := st.Squads.ListOutcomes(context.Background(), "hud-frontend", 10)
	if len(rows) != 0 {
		t.Errorf("expected zero outcomes for unrouted escalation, got %d", len(rows))
	}
}

func TestOutcomeRecorder_OnMergedAppendsMemory(t *testing.T) {
	st := newTestStore(t)
	if err := st.Squads.PutSquad(context.Background(), &store.Squad{
		Name: "hud-frontend", Enabled: true,
	}); err != nil {
		t.Fatalf("seed squad row: %v", err)
	}
	run, item := seedRunForRecorder(t, st, "PIPE-MEM-1", "MILLS-MEM-1", 1)
	seedRoutedEvent(t, st, run.ID, map[string]any{
		"squad_name": "hud-frontend", "path_class": "internal/hud/frontend/**",
	})

	rec := NewOutcomeRecorder(st)
	if err := rec.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("OnMerged: %v", err)
	}
	mems, err := st.Squads.MemoryRecall(context.Background(), "hud-frontend", store.SquadMemoryMerge, 10)
	if err != nil {
		t.Fatalf("memory recall: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1 merge memory entry, got %d", len(mems))
	}
	m := mems[0]
	if m.Title != "merge: "+item.Title {
		t.Errorf("title: got %q", m.Title)
	}
	if m.Importance != mergeMemoryImportance {
		t.Errorf("importance: got %v want %v", m.Importance, mergeMemoryImportance)
	}
	foundRun := false
	for _, ref := range m.Refs {
		if ref == run.ID {
			foundRun = true
		}
	}
	if !foundRun {
		t.Errorf("refs missing run id: %v", m.Refs)
	}

	// Double-fire must not duplicate the entry (outcome dedup short-circuits).
	if err := rec.OnMerged(context.Background(), run, item); err != nil {
		t.Fatalf("second OnMerged: %v", err)
	}
	mems, _ = st.Squads.MemoryRecall(context.Background(), "hud-frontend", store.SquadMemoryMerge, 10)
	if len(mems) != 1 {
		t.Errorf("expected memory to stay at 1 entry after double-fire, got %d", len(mems))
	}
}

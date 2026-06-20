package pm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeReader struct {
	tasks     []TaskBrief
	decisions []DecisionBrief
	tasksErr  error
	decErr    error
}

func (f *fakeReader) TasksByProject(context.Context, string) ([]TaskBrief, error) {
	return f.tasks, f.tasksErr
}
func (f *fakeReader) DecisionsByProject(context.Context, string) ([]DecisionBrief, error) {
	return f.decisions, f.decErr
}

func TestProjectStatus_Rollup(t *testing.T) {
	store := newFakeStore()
	store.risks["r1"] = Risk{ID: "r1", Project: "services/loom-core", Status: StatusIdentified}
	store.risks["r2"] = Risk{ID: "r2", Project: "services/loom-core", Status: StatusClosed}
	store.risks["r3"] = Risk{ID: "r3", Project: "services/other", Status: StatusIdentified}

	reader := &fakeReader{
		tasks: []TaskBrief{
			{ID: "t1", Title: "a", Status: "pending"},
			{ID: "t2", Title: "b", Status: "in_progress"},
			{ID: "t3", Title: "c", Status: "blocked"},
			{ID: "t4", Title: "d", Status: "completed"},
		},
		decisions: []DecisionBrief{{ID: "d1", Title: "chose X"}},
	}

	svc := NewService(store, &fakeEmbedder{dim: VectorSize}, Config{}, nil)
	svc.SetProjectReader(reader)

	res, err := svc.ProjectStatus(context.Background(), "services/loom-core")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if res.Partial {
		t.Errorf("unexpected partial: %v", res.PartialReasons)
	}
	if res.OpenTasks != 3 {
		t.Errorf("open_tasks=%d, want 3 (pending+in_progress+blocked)", res.OpenTasks)
	}
	if res.InProgress != 1 {
		t.Errorf("in_progress=%d, want 1", res.InProgress)
	}
	if res.Blocked != 1 {
		t.Errorf("blocked=%d, want 1", res.Blocked)
	}
	if len(res.Tasks) != 3 {
		t.Errorf("tasks list=%d, want 3 (completed excluded)", len(res.Tasks))
	}
	if res.OpenRisks != 1 {
		t.Errorf("open_risks=%d, want 1 (closed excluded)", res.OpenRisks)
	}
	if len(res.Risks) != 2 {
		t.Errorf("risks=%d, want 2 (project-filtered)", len(res.Risks))
	}
	if len(res.Decisions) != 1 {
		t.Errorf("decisions=%d, want 1", len(res.Decisions))
	}
}

func TestProjectStatus_RequiresProject(t *testing.T) {
	svc := NewService(newFakeStore(), &fakeEmbedder{dim: VectorSize}, Config{}, nil)
	if _, err := svc.ProjectStatus(context.Background(), "  "); err == nil {
		t.Error("expected error for empty project")
	}
}

func TestProjectStatus_NilReaderIsPartial(t *testing.T) {
	store := newFakeStore()
	store.risks["r1"] = Risk{ID: "r1", Project: "p", Status: StatusIdentified}
	svc := NewService(store, &fakeEmbedder{dim: VectorSize}, Config{}, nil) // no reader

	res, err := svc.ProjectStatus(context.Background(), "p")
	if err != nil {
		t.Fatalf("ProjectStatus: %v", err)
	}
	if !res.Partial {
		t.Error("expected partial=true with nil reader")
	}
	if res.OpenRisks != 1 {
		t.Errorf("risks must still load with nil reader; open_risks=%d", res.OpenRisks)
	}
	if len(res.Tasks) != 0 {
		t.Errorf("tasks must be empty with nil reader; got %d", len(res.Tasks))
	}
}

// TestQdrantProjectReader_ScrollsByCollection exercises the production reader's
// scroll + payload mapping against a collection-aware fake Qdrant: the tasks
// collection yields TaskBriefs, the context collection yields DecisionBriefs.
func TestQdrantProjectReader_ScrollsByCollection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var pts []map[string]any
		switch {
		case strings.Contains(r.URL.Path, "tasks_coll"):
			pts = []map[string]any{
				{"id": "t1", "payload": map[string]any{"id": "t1", "title": "do x", "status": "pending", "priority": "high", "session_id": "s1"}},
			}
		case strings.Contains(r.URL.Path, "ctx_coll"):
			pts = []map[string]any{
				{"id": "d1", "payload": map[string]any{"id": "d1", "title": "chose A", "timestamp": "2026-06-20T00:00:00Z", "entry_type": "decision"}},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"points": pts, "next_page_offset": nil},
		})
	}))
	t.Cleanup(srv.Close)

	reader := NewQdrantProjectReader(Config{
		QdrantURL:         srv.URL,
		QdrantDistance:    "Cosine",
		TasksCollection:   "tasks_coll",
		ContextCollection: "ctx_coll",
	})
	ctx := context.Background()

	tasks, err := reader.TasksByProject(ctx, "services/loom-core")
	if err != nil {
		t.Fatalf("TasksByProject: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t1" || tasks[0].Status != "pending" || tasks[0].Priority != "high" || tasks[0].SessionID != "s1" {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}

	decs, err := reader.DecisionsByProject(ctx, "services/loom-core")
	if err != nil {
		t.Fatalf("DecisionsByProject: %v", err)
	}
	if len(decs) != 1 || decs[0].ID != "d1" || decs[0].Title != "chose A" || decs[0].DecidedAt == "" {
		t.Fatalf("unexpected decisions: %+v", decs)
	}
}

func TestProjectStatus_SourceErrorIsolated(t *testing.T) {
	store := newFakeStore()
	reader := &fakeReader{
		tasksErr:  errors.New("qdrant down"),
		decisions: []DecisionBrief{{ID: "d1", Title: "x"}},
	}
	svc := NewService(store, &fakeEmbedder{dim: VectorSize}, Config{}, nil)
	svc.SetProjectReader(reader)

	res, err := svc.ProjectStatus(context.Background(), "p")
	if err != nil {
		t.Fatalf("ProjectStatus must not fail when one source errors: %v", err)
	}
	if !res.Partial {
		t.Error("expected partial=true when the tasks source errors")
	}
	if len(res.Decisions) != 1 {
		t.Error("decisions must still load when tasks errors (per-source isolation)")
	}
}

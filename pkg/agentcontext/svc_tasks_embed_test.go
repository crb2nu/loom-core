package agentcontext

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
)

// failingEmbedder simulates an embedder outage (e.g. the gte-qwen2-1.5b HTTP 500
// observed 2026-06-20): every embed call returns an error.
type failingEmbedder struct{}

func (failingEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	return nil, errors.New("embed outage")
}
func (failingEmbedder) EmbedDocuments(context.Context, []string) ([][]float64, error) {
	return nil, errors.New("embed outage")
}
func (failingEmbedder) Name() string  { return "failing" }
func (failingEmbedder) Model() string { return "none" }

// TestTaskAdd_EmbedFailureStillPersists is the Slice-2a regression guard: a task
// MUST still persist (with a deterministic fallback vector) when the embedder is
// down. Without the best-effort decoupling, an embedder outage hard-fails
// agent_task_add, drains agent_tasks_v1 to empty, and blanks the flexdeck
// /projects task lane for every agent.
// See .loom/plan-task-integration-pm-2026-06-20.md (Slice 2a).
func TestTaskAdd_EmbedFailureStillPersists(t *testing.T) {
	// Qdrant stub: EnsureCollection probes the collection via GET; report it
	// exists at 1536 so the write proceeds to upsert.
	mux := http.NewServeMux()
	mux.HandleFunc("/collections/"+CollTasks, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, map[string]any{
			"result": map[string]any{
				"config": map[string]any{
					"params": map[string]any{
						"vectors": map[string]any{"size": 1536, "distance": "Cosine"},
					},
				},
			},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	qdrant := NewQdrantClient(httpclient.NewDefault(), server.URL, "", CollTasks, "Cosine")

	vectorSize := 1536
	ts := NewTaskSvc(qdrant, failingEmbedder{}, Config{}, slog.Default(), &vectorSize)
	ts.getSession = func(_ context.Context, sessionID string) (*Session, error) {
		return &Session{
			ID:        sessionID,
			AgentID:   "claude-code-test",
			Namespace: "services/loom-core/fix/x",
			Project:   "services/loom-core",
		}, nil
	}
	var captured []Point
	ts.upsertBatched = func(_ context.Context, _ *QdrantClient, points []Point) error {
		captured = append(captured, points...)
		return nil
	}

	res, err := ts.Add(context.Background(), map[string]any{
		"session_id": "s1",
		"tasks": []any{
			map[string]any{"title": "do the thing", "context": "with details"},
		},
	})
	if err != nil {
		t.Fatalf("Add returned transport error: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("Add returned a tool error despite embedding being best-effort: %+v", res.Content)
	}

	if len(captured) != 1 {
		t.Fatalf("captured %d points, want 1 (task must persist when embed fails)", len(captured))
	}
	p := captured[0]

	// Fallback vector: correct dimension and non-zero (Qdrant rejects all-zero
	// vectors under cosine distance).
	if len(p.Vector) != 1536 {
		t.Fatalf("fallback vector dim = %d, want 1536", len(p.Vector))
	}
	nonZero := false
	for _, c := range p.Vector {
		if c != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Errorf("fallback vector is all-zero; Qdrant rejects that under cosine distance")
	}

	// Project must be stamped so the task federates into the PM view.
	if got, _ := p.Payload["project"].(string); got != "services/loom-core" {
		t.Errorf("persisted project = %q, want services/loom-core", got)
	}
	if got, _ := p.Payload["status"].(string); got != string(TaskStatusPending) {
		t.Errorf("persisted status = %q, want pending", got)
	}
}

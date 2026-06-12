package agentcontext

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/crb2nu/loom/pkg/httpclient"
)

type tasksQdrantStub struct {
	mu    sync.Mutex
	tasks map[string]map[string]any
}

func newTasksQdrantStub(t *testing.T, seeded ...map[string]any) (*QdrantClient, *tasksQdrantStub) {
	t.Helper()

	stub := &tasksQdrantStub{
		tasks: make(map[string]map[string]any, len(seeded)),
	}
	for _, payload := range seeded {
		id, _ := payload["id"].(string)
		if id == "" {
			t.Fatalf("seeded task payload missing id: %v", payload)
		}
		stub.tasks[id] = clonePayload(payload)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/collections/"+CollTasks+"/points/scroll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode scroll body: %v", err)
		}
		filter, _ := body["filter"].(map[string]any)

		stub.mu.Lock()
		points := make([]map[string]any, 0, len(stub.tasks))
		for id, payload := range stub.tasks {
			if !matchesPayloadFilter(filter, payload) {
				continue
			}
			points = append(points, map[string]any{
				"id":      toPointID(id),
				"payload": clonePayload(payload),
			})
		}
		stub.mu.Unlock()

		writeJSON(t, w, map[string]any{
			"result": map[string]any{
				"points":           points,
				"next_page_offset": nil,
			},
		})
	})
	mux.HandleFunc("/collections/"+CollTasks+"/points/payload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode set-payload body: %v", err)
		}
		patch, _ := body["payload"].(map[string]any)
		rawIDs, _ := body["points"].([]any)
		wantUUIDs := make(map[string]bool, len(rawIDs))
		for _, raw := range rawIDs {
			if s, ok := raw.(string); ok {
				wantUUIDs[s] = true
			}
		}
		stub.mu.Lock()
		for id, existing := range stub.tasks {
			if !wantUUIDs[toPointID(id)] {
				continue
			}
			for k, v := range patch {
				existing[k] = v
			}
		}
		stub.mu.Unlock()
		writeJSON(t, w, map[string]any{"status": "ok"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := NewQdrantClient(httpclient.NewDefault(), server.URL, "", CollTasks, "Cosine")
	return client, stub
}

func (s *tasksQdrantStub) taskField(t *testing.T, id, field string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, ok := s.tasks[id]
	if !ok {
		t.Fatalf("task %q not found in stub", id)
	}
	value, _ := payload[field].(string)
	return value
}

func seedTaskPayload(id, sessionID string, status TaskStatus) map[string]any {
	return map[string]any{
		"id":         id,
		"session_id": sessionID,
		"status":     string(status),
		"resolution": "",
	}
}

// Regression test for backlog tasks being auto-blocked on session end:
// a planning session that creates pending tasks and ends immediately must
// leave those tasks pending. Only in_progress tasks get blocked.
func TestMarkSessionTasksStale_PendingTasksSurviveSessionEnd(t *testing.T) {
	qdrant, stub := newTasksQdrantStub(t,
		seedTaskPayload("t-pending-1", "s1", TaskStatusPending),
		seedTaskPayload("t-pending-2", "s1", TaskStatusPending),
		seedTaskPayload("t-in-progress", "s1", TaskStatusInProgress),
		seedTaskPayload("t-completed", "s1", TaskStatusCompleted),
		seedTaskPayload("t-other-session", "s2", TaskStatusInProgress),
	)

	vectorSize := 0
	ts := NewTaskSvc(qdrant, nil, Config{}, slog.Default(), &vectorSize)

	count := ts.MarkSessionTasksStale(context.Background(), "s1")
	if count != 1 {
		t.Fatalf("MarkSessionTasksStale = %d, want 1 (only the in_progress task)", count)
	}

	for _, id := range []string{"t-pending-1", "t-pending-2"} {
		if got := stub.taskField(t, id, "status"); got != string(TaskStatusPending) {
			t.Errorf("%s status = %q, want pending (backlog tasks must outlive the session)", id, got)
		}
		if got := stub.taskField(t, id, "resolution"); got != "" {
			t.Errorf("%s resolution = %q, want empty", id, got)
		}
	}

	if got := stub.taskField(t, "t-in-progress", "status"); got != string(TaskStatusBlocked) {
		t.Errorf("t-in-progress status = %q, want blocked", got)
	}
	if got := stub.taskField(t, "t-in-progress", "resolution"); got != "session ended — task incomplete" {
		t.Errorf("t-in-progress resolution = %q, want session-ended resolution", got)
	}

	if got := stub.taskField(t, "t-completed", "status"); got != string(TaskStatusCompleted) {
		t.Errorf("t-completed status = %q, want completed", got)
	}
	if got := stub.taskField(t, "t-other-session", "status"); got != string(TaskStatusInProgress) {
		t.Errorf("t-other-session status = %q, want in_progress (different session)", got)
	}
}

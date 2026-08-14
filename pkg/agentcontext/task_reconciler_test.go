package agentcontext

import (
	"context"
	"log/slog"
	"testing"
)

// Regression test for backlog tasks being orphan-deleted after session
// pruning: session end leaves pending tasks alone (see
// MarkSessionTasksStale), but the reconciler's orphan pass deleted ANY
// non-completed task whose session record was gone (e.g. after
// agent_session_prune). Pending backlog tasks must survive; only in-flight
// work (in_progress/blocked) is GC'd as orphaned.
func TestReconcile_PendingTaskSurvivesSessionPrune(t *testing.T) {
	qdrant, stub := newTasksQdrantStub(t,
		seedTaskPayload("t-backlog-1", "s-pruned", TaskStatusPending),
		seedTaskPayload("t-backlog-2", "s-pruned", TaskStatusPending),
		seedTaskPayload("t-orphan-wip", "s-pruned", TaskStatusInProgress),
		seedTaskPayload("t-orphan-blocked", "s-pruned", TaskStatusBlocked),
		seedTaskPayload("t-live-pending", "s-live", TaskStatusPending),
	)

	vectorSize := 0
	ts := NewTaskSvc(qdrant, nil, Config{}, slog.Default(), &vectorSize)

	r := NewTaskReconciler(DefaultTaskReconcilerConfig(), ts, slog.Default())
	r.getSession = func(_ context.Context, sessionID string) (*Session, error) {
		if sessionID == "s-live" {
			return &Session{ID: sessionID}, nil
		}
		return nil, nil // session record pruned
	}

	stats, err := r.TriggerReconcile(context.Background())
	if err != nil {
		t.Fatalf("TriggerReconcile: %v", err)
	}

	if stats.OrphansCleanedUp != 2 {
		t.Errorf("OrphansCleanedUp = %d, want 2 (only in_progress + blocked)", stats.OrphansCleanedUp)
	}

	for _, id := range []string{"t-backlog-1", "t-backlog-2"} {
		if !stub.hasTask(id) {
			t.Errorf("%s was deleted; pending backlog tasks must survive session pruning", id)
			continue
		}
		if got := stub.taskField(t, id, "status"); got != string(TaskStatusPending) {
			t.Errorf("%s status = %q, want pending", id, got)
		}
	}
	if !stub.hasTask("t-live-pending") {
		t.Errorf("t-live-pending was deleted; tasks with a live session must survive")
	}

	for _, id := range []string{"t-orphan-wip", "t-orphan-blocked"} {
		if stub.hasTask(id) {
			t.Errorf("%s still exists; orphaned in-flight tasks should be deleted", id)
		}
	}
}

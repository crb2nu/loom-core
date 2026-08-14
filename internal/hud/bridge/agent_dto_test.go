package bridge

import (
	"encoding/json"
	"testing"
)

func TestSessionInfoUnmarshalJSON_BackfillsProject(t *testing.T) {
	var session SessionInfo
	if err := json.Unmarshal([]byte(`{"id":"sess-1","agent_id":"codex-1","namespace":"loom-core/feat/orchestration","status":"active"}`), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if session.Project != "loom-core" {
		t.Fatalf("session.Project = %q, want loom-core", session.Project)
	}
}

func TestContextEntryUnmarshalJSON_PreservesSessionID(t *testing.T) {
	var entry ContextEntry
	if err := json.Unmarshal([]byte(`{
		"id":"ctx-1",
		"entry_type":"decision",
		"agent_id":"codex-1",
		"session_id":"session-1",
		"namespace":"loom-core/main",
		"title":"Keep one session identity",
		"timestamp":"2026-07-14T19:00:00Z"
	}`), &entry); err != nil {
		t.Fatalf("unmarshal context entry: %v", err)
	}
	if entry.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", entry.SessionID)
	}
}

func TestTaskInfoUnmarshalJSON_BackfillsProjectFromPipeline(t *testing.T) {
	var task TaskInfo
	if err := json.Unmarshal([]byte(`{"id":"task-1","session_id":"sess-1","title":"Watch CI","status":"pending","pipeline_ref":{"id":42,"project":"services/loom-core","ref":"main"}}`), &task); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if task.Project != "services/loom-core" {
		t.Fatalf("task.Project = %q, want services/loom-core", task.Project)
	}
}

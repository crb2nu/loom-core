package clients

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"
)

func contextAddStub(t *testing.T, body any) []byte {
	t.Helper()
	bodyJSON, _ := json.Marshal(body)
	res := mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: string(bodyJSON)}}}
	out, _ := json.Marshal(res)
	return out
}

// callParams returns the arguments of the last tools/call the fake transport saw.
func callParams(t *testing.T, ft *fakeTransport) mcp.CallToolParams {
	t.Helper()
	var params mcp.CallToolParams
	for _, m := range ft.sentMessages() {
		if m.Method == "tools/call" {
			_ = json.Unmarshal(m.Params, &params)
		}
	}
	return params
}

// singleEntry pulls the one entry out of an agent_context_add argument map.
func singleEntry(t *testing.T, params mcp.CallToolParams) map[string]any {
	t.Helper()
	entries, ok := params.Arguments["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected exactly one entry, got %#v", params.Arguments["entries"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("entry is not an object: %#v", entries[0])
	}
	return entry
}

func TestContextRecorder_AddContextEntry_HappyPath(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": contextAddStub(t, map[string]any{
				"ok": true, "count": 1, "entry_ids": []string{"ce-1"},
			}),
		},
	}
	hub := newTestHubClient(t, ft)
	rec := NewContextRecorder(hub, "session-op-1")

	err := rec.AddContextEntry(context.Background(), "", "decision",
		"Escalated MILLS-7: gate failed", "stage: implement\nfailure_class: infrastructure",
		[]string{"mills", "escalation"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	params := callParams(t, ft)
	if params.Name != "agent_context_add" {
		t.Errorf("tool name = %q, want agent_context_add", params.Name)
	}
	if params.Arguments["session_id"] != "session-op-1" {
		t.Errorf("session_id = %v, want the operator session", params.Arguments["session_id"])
	}
	entry := singleEntry(t, params)
	if entry["entry_type"] != "decision" {
		t.Errorf("entry_type = %v", entry["entry_type"])
	}
	if entry["title"] != "Escalated MILLS-7: gate failed" {
		t.Errorf("title = %v", entry["title"])
	}
	if entry["durability"] != "session" {
		t.Errorf("durability = %v, want session default", entry["durability"])
	}
	if entry["visibility"] != "shared" {
		t.Errorf("visibility = %v, want shared default", entry["visibility"])
	}
	tags, _ := entry["tags"].([]any)
	if len(tags) != 2 || tags[0] != "mills" || tags[1] != "escalation" {
		t.Errorf("tags = %#v", entry["tags"])
	}
}

func TestContextRecorder_UsesDynamicSessionID(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": contextAddStub(t, map[string]any{"ok": true, "count": 1}),
		},
	}
	hub := newTestHubClient(t, ft)
	// Boot-time session was empty (hub outage); the maintainer supplies it later.
	rec := NewContextRecorder(hub, "")
	rec.SessionIDFunc = func() string { return "session-late" }

	if err := rec.AddContextEntry(context.Background(), "", "finding", "t", "c", nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := callParams(t, ft).Arguments["session_id"]; got != "session-late" {
		t.Errorf("session_id = %v, want session-late", got)
	}
}

func TestContextRecorder_ExplicitSessionIDWins(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": contextAddStub(t, map[string]any{"ok": true, "count": 1}),
		},
	}
	hub := newTestHubClient(t, ft)
	rec := NewContextRecorder(hub, "session-op-1")

	if err := rec.AddContextEntry(context.Background(), "session-other", "note", "t", "c", nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := callParams(t, ft).Arguments["session_id"]; got != "session-other" {
		t.Errorf("session_id = %v, want session-other", got)
	}
}

func TestContextRecorder_TruncatesOversizedContent(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": contextAddStub(t, map[string]any{"ok": true, "count": 1}),
		},
	}
	hub := newTestHubClient(t, ft)
	rec := NewContextRecorder(hub, "session-op-1")
	rec.MaxContentBytes = 16

	if err := rec.AddContextEntry(context.Background(), "", "finding", "t",
		strings.Repeat("x", 500), nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	content, _ := singleEntry(t, callParams(t, ft))["content"].(string)
	if !strings.HasPrefix(content, strings.Repeat("x", 16)) {
		t.Errorf("content not capped at MaxContentBytes: %q", content)
	}
	if !strings.Contains(content, "truncated") {
		t.Errorf("truncated content missing elision marker: %q", content)
	}
}

func TestContextRecorder_ValidatesInput(t *testing.T) {
	hub := newTestHubClient(t, &fakeTransport{})
	cases := map[string]*ContextRecorder{
		"nil hub": {},
	}
	for name, rec := range cases {
		if err := rec.AddContextEntry(context.Background(), "s", "decision", "t", "c", nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	rec := NewContextRecorder(hub, "")
	if err := rec.AddContextEntry(context.Background(), "", "decision", "t", "c", nil); err == nil {
		t.Error("expected error when no session id is resolvable")
	}
	rec = NewContextRecorder(hub, "session-op-1")
	if err := rec.AddContextEntry(context.Background(), "", "", "t", "c", nil); err == nil {
		t.Error("expected error when entry_type empty")
	}
	if err := rec.AddContextEntry(context.Background(), "", "decision", "  ", "c", nil); err == nil {
		t.Error("expected error when title empty")
	}
}

func TestContextRecorder_ServiceFailureBubbles(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": contextAddStub(t, map[string]any{"ok": false, "count": 0}),
		},
	}
	hub := newTestHubClient(t, ft)
	rec := NewContextRecorder(hub, "session-op-1")
	if err := rec.AddContextEntry(context.Background(), "", "decision", "t", "c", nil); err == nil {
		t.Error("expected error when service reports ok=false")
	}
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHandleListMergeRequestDiffs(t *testing.T) {
	var gotPath string
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Page", "2")
		w.Header().Set("X-Per-Page", "2")
		w.Header().Set("X-Total", "4")
		_, _ = io.WriteString(w, `[
			{"old_path":"a.go","new_path":"a.go","diff":"1234567890"},
			{"old_path":"b.go","new_path":"b.go","diff":"abcdefghij"}
		]`)
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handleListMergeRequestDiffs(context.Background(), map[string]any{
		"project":              "group/project",
		"merge_request_iid":    7,
		"page":                 2,
		"per_page":             2,
		"unidiff":              true,
		"max_total_diff_bytes": 12,
	})
	if err != nil {
		t.Fatalf("list merge request diffs: %v", err)
	}
	if gotPath != "/projects/group%2Fproject/merge_requests/7/diffs" {
		t.Fatalf("escaped path = %q", gotPath)
	}
	for _, want := range []string{"page=2", "per_page=2", "unidiff=true"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q does not contain %q", gotQuery, want)
		}
	}

	parsed := mustParseJSON(t, result)
	if parsed["count"] != float64(2) {
		t.Fatalf("count = %v, want 2", parsed["count"])
	}
	if parsed["diff_bytes_returned"] != float64(12) {
		t.Fatalf("diff_bytes_returned = %v, want 12", parsed["diff_bytes_returned"])
	}
	if parsed["truncated_files"] != float64(2) {
		t.Fatalf("truncated_files = %v, want 2", parsed["truncated_files"])
	}
	diffs := parsed["diffs"].([]any)
	for i, raw := range diffs {
		diff := raw.(map[string]any)
		if diff["diff_truncated"] != true {
			t.Errorf("diff %d missing diff_truncated marker: %v", i, diff)
		}
		if diff["original_diff_bytes"] != float64(10) {
			t.Errorf("diff %d original_diff_bytes = %v", i, diff["original_diff_bytes"])
		}
	}
	pagination := parsed["pagination"].(map[string]any)
	if pagination["total"] != float64(4) {
		t.Fatalf("pagination total = %v, want 4", pagination["total"])
	}
}

func TestHandleListMergeRequestDiffsValidatesInputs(t *testing.T) {
	gl := &gitlabServer{token: "x", apiURL: "http://unused"}

	for name, args := range map[string]map[string]any{
		"invalid iid": {
			"project": "group/project", "merge_request_iid": 0,
		},
		"invalid diff budget": {
			"project": "group/project", "merge_request_iid": 1, "max_total_diff_bytes": 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := gl.handleListMergeRequestDiffs(context.Background(), args)
			if err != nil {
				t.Fatalf("validation returned error: %v", err)
			}
			if result == nil || !result.IsError {
				t.Fatal("expected MCP validation error result")
			}
		})
	}
}

func TestTruncateMergeRequestDiffTextPreservesUTF8(t *testing.T) {
	got := truncateMergeRequestDiffText("ééé", 1)
	if got != "" {
		t.Fatalf("tiny budget result = %q, want empty", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("tiny budget result is invalid UTF-8: %q", got)
	}

	got = truncateMergeRequestDiffText("ééé", 5)
	if len(got) > 5 || !utf8.ValidString(got) {
		t.Fatalf("bounded result = %q (%d bytes), want valid UTF-8 <= 5 bytes", got, len(got))
	}
}

func TestHandleListMergeRequestDiscussionsFiltersResolutionState(t *testing.T) {
	var gotPath string
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Page", "1")
		w.Header().Set("X-Total", "3")
		_, _ = io.WriteString(w, `[
			{"id":"open","notes":[{"id":1,"resolvable":true,"resolved":false}]},
			{"id":"done","notes":[{"id":2,"resolvable":true,"resolved":true}]},
			{"id":"note","notes":[{"id":3,"resolvable":false,"system":true}]}
		]`)
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handleListMergeRequestDiscussions(context.Background(), map[string]any{
		"project":           "group/project",
		"merge_request_iid": 9,
		"discussion_state":  "unresolved",
		"page":              1,
		"per_page":          50,
	})
	if err != nil {
		t.Fatalf("list merge request discussions: %v", err)
	}
	if gotPath != "/projects/group%2Fproject/merge_requests/9/discussions" {
		t.Fatalf("escaped path = %q", gotPath)
	}
	for _, want := range []string{"page=1", "per_page=50"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q does not contain %q", gotQuery, want)
		}
	}

	parsed := mustParseJSON(t, result)
	if parsed["count"] != float64(1) {
		t.Fatalf("count = %v, want 1", parsed["count"])
	}
	discussions := parsed["discussions"].([]any)
	if discussions[0].(map[string]any)["id"] != "open" {
		t.Fatalf("filtered discussions = %v", discussions)
	}
	summary := parsed["page_summary"].(map[string]any)
	for field, want := range map[string]float64{
		"fetched": 3, "unresolved": 1, "resolved": 1, "unresolvable": 1,
	} {
		if summary[field] != want {
			t.Errorf("page_summary.%s = %v, want %v", field, summary[field], want)
		}
	}
}

func TestHandleListMergeRequestDiscussionsRejectsInvalidState(t *testing.T) {
	gl := &gitlabServer{token: "x", apiURL: "http://unused"}
	result, err := gl.handleListMergeRequestDiscussions(context.Background(), map[string]any{
		"project": "group/project", "merge_request_iid": 1, "discussion_state": "blocked",
	})
	if err != nil {
		t.Fatalf("validation returned error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatal("expected MCP validation error result")
	}
}

func TestHandleResolveMergeRequestDiscussion(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"thread/one","notes":[{"resolved":false}]}`)
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handleResolveMergeRequestDiscussion(context.Background(), map[string]any{
		"project":           "group/project",
		"merge_request_iid": 3,
		"discussion_id":     "thread/one",
		"resolved":          false,
	})
	if err != nil {
		t.Fatalf("resolve merge request discussion: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %s, want PUT", gotMethod)
	}
	if gotPath != "/projects/group%2Fproject/merge_requests/3/discussions/thread%2Fone" {
		t.Fatalf("escaped path = %q", gotPath)
	}
	if gotBody["resolved"] != false {
		t.Fatalf("resolved body = %v, want false", gotBody["resolved"])
	}
	parsed := mustParseJSON(t, result)
	if parsed["id"] != "thread/one" {
		t.Fatalf("discussion id = %v", parsed["id"])
	}
}

func TestHandleResolveMergeRequestDiscussionRequiresResolved(t *testing.T) {
	gl := &gitlabServer{token: "x", apiURL: "http://unused"}
	result, err := gl.handleResolveMergeRequestDiscussion(context.Background(), map[string]any{
		"project": "group/project", "merge_request_iid": 1, "discussion_id": "thread",
	})
	if err != nil {
		t.Fatalf("validation returned error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatal("expected MCP validation error result")
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mcperror"
)

func TestHandleMergeMergeRequest_FleetQueuesWithoutMergePUT(t *testing.T) {
	var mergePUTs, enqueues int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/merge_requests/7"):
			fmt.Fprint(w, `{"iid":7,"sha":"head-7","source_branch":"feat/x","target_branch":"main"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/mills/merge-queue/enqueue":
			atomic.AddInt32(&enqueues, 1)
			if r.Header.Get("Authorization") != "Bearer queue-token" {
				t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["producer"] != "mcp_gitlab" || body["observed_sha"] != "head-7" {
				t.Errorf("payload = %#v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"outcome":"enqueued","state":"queued"}`)
		case r.Method == http.MethodPut:
			atomic.AddInt32(&mergePUTs, 1)
			t.Error("fleet mode must not PUT a GitLab merge")
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	gl := newTestServer(ts)
	gl.mergeQueueURL = ts.URL
	gl.mergeQueueToken = "queue-token"
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{"project": "group/project", "merge_request_iid": 7, "sha": "head-7"})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if mergePUTs != 0 || enqueues != 1 {
		t.Fatalf("merge PUTs=%d enqueues=%d", mergePUTs, enqueues)
	}
}

func TestHandleMergeMergeRequest_FleetRejectsStaleSHA(t *testing.T) {
	var enqueues int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"sha":"new","source_branch":"feat/x","target_branch":"main"}`)
			return
		}
		atomic.AddInt32(&enqueues, 1)
	}))
	defer ts.Close()
	gl := newTestServer(ts)
	gl.mergeQueueURL = ts.URL
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{"project": "p", "merge_request_iid": 1, "sha": "old"})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if enqueues != 0 {
		t.Fatal("stale SHA was enqueued")
	}
}

// shrinkAutoMergeDelays makes the auto-merge retry timings test-fast.
func shrinkAutoMergeDelays(t *testing.T) {
	t.Helper()
	oldAttempts := autoMergeRetryAttempts
	oldWait := autoMergeHeadPipelineWait
	oldPoll := autoMergePollInterval
	oldBackoff := autoMergeRetryBackoffMax
	autoMergeHeadPipelineWait = 30 * time.Millisecond
	autoMergePollInterval = 5 * time.Millisecond
	autoMergeRetryBackoffMax = time.Millisecond
	t.Cleanup(func() {
		autoMergeRetryAttempts = oldAttempts
		autoMergeHeadPipelineWait = oldWait
		autoMergePollInterval = oldPoll
		autoMergeRetryBackoffMax = oldBackoff
	})
}

func TestHandleMergeMergeRequest_AutoMerge405_RetriesAfterPipelineAppears(t *testing.T) {
	shrinkAutoMergeDelays(t)

	var putCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			if atomic.AddInt32(&putCount, 1) == 1 {
				w.WriteHeader(http.StatusMethodNotAllowed)
				fmt.Fprint(w, `{"message":"405 Method Not Allowed"}`)
				return
			}
			fmt.Fprint(w, `{"iid":5,"state":"opened","merge_when_pipeline_succeeds":true}`)
		case r.Method == http.MethodGet:
			fmt.Fprint(w, `{"iid":5,"head_pipeline":{"id":777,"status":"running"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{
		"project":           "p",
		"merge_request_iid": float64(5),
		"auto_merge":        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success after retry, got error result: %+v", result)
	}
	if got := atomic.LoadInt32(&putCount); got != 2 {
		t.Fatalf("expected 2 merge attempts, got %d", got)
	}
	out := mustParseJSON(t, result)
	if out["merge_when_pipeline_succeeds"] != true {
		t.Fatalf("expected merge_when_pipeline_succeeds=true, got %v", out)
	}
}

func TestHandleMergeMergeRequest_AutoMerge405_PersistentReturnsActionableError(t *testing.T) {
	shrinkAutoMergeDelays(t)

	var putCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			atomic.AddInt32(&putCount, 1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprint(w, `{"message":"405 Method Not Allowed"}`)
		case r.Method == http.MethodGet:
			// No head pipeline yet.
			fmt.Fprint(w, `{"iid":5,"head_pipeline":null}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{
		"project":           "p",
		"merge_request_iid": float64(5),
		"auto_merge":        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got %+v", result)
	}
	if got := atomic.LoadInt32(&putCount); got != int32(1+autoMergeRetryAttempts) {
		t.Fatalf("expected %d merge attempts, got %d", 1+autoMergeRetryAttempts, got)
	}
	text := result.Content[0].Text
	for _, want := range []string{"AUTO_MERGE_NOT_READY", "poll_pipeline", "no head pipeline"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error text missing %q: %s", want, text)
		}
	}
}

func TestHandleMergeMergeRequest_AutoMerge406_ReportsHeadPipeline(t *testing.T) {
	shrinkAutoMergeDelays(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			w.WriteHeader(http.StatusNotAcceptable)
			fmt.Fprint(w, `{"message":"Branch cannot be merged"}`)
		case r.Method == http.MethodGet:
			fmt.Fprint(w, `{"iid":5,"head_pipeline":{"id":888,"status":"success"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{
		"project":           "p",
		"merge_request_iid": float64(5),
		"auto_merge":        true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got %+v", result)
	}
	text := result.Content[0].Text
	for _, want := range []string{"HTTP 406", `head pipeline 888 is "success"`, "rebase"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error text missing %q: %s", want, text)
		}
	}
}

func TestHandleMergeMergeRequest_ImmediateMerge405_NoRetry(t *testing.T) {
	shrinkAutoMergeDelays(t)

	var putCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		atomic.AddInt32(&putCount, 1)
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, `{"message":"405 Method Not Allowed"}`)
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{
		"project":           "p",
		"merge_request_iid": float64(5),
	})
	if err == nil {
		t.Fatalf("expected raw API error without auto_merge, got result %+v", result)
	}
	if got := atomic.LoadInt32(&putCount); got != 1 {
		t.Fatalf("expected exactly 1 merge attempt, got %d", got)
	}
}

func TestAPIStatusCode(t *testing.T) {
	if got := apiStatusCode(nil); got != 0 {
		t.Fatalf("nil error: expected 0, got %d", got)
	}
	if got := apiStatusCode(errors.New("plain")); got != 0 {
		t.Fatalf("plain error: expected 0, got %d", got)
	}
	if got := apiStatusCode(mcperror.New("X", "no details")); got != 0 {
		t.Fatalf("no details: expected 0, got %d", got)
	}
	if got := apiStatusCode(mcperror.APIError("GitLab", 405, "nope")); got != 405 {
		t.Fatalf("api error: expected 405, got %d", got)
	}
}

func TestHandlePipelineSummary_NotFoundReturnsActionableError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"404 Not Found"}`)
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handlePipelineSummary(context.Background(), map[string]any{
		"project":     "p",
		"pipeline_id": float64(99),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got %+v", result)
	}
	text := result.Content[0].Text
	for _, want := range []string{"PIPELINE_NOT_FOUND", "list_pipelines", "poll_pipeline"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error text missing %q: %s", want, text)
		}
	}
}

func TestHandlePipelineSummary_JobsFailureReturnsPartialResult(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/jobs"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"404 Not Found"}`)
		case strings.Contains(r.URL.Path, "/pipelines/"):
			fmt.Fprint(w, `{"id":99,"status":"running"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	gl := newTestServer(ts)
	result, err := gl.handlePipelineSummary(context.Background(), map[string]any{
		"project":     "p",
		"pipeline_id": float64(99),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected partial success, got error result: %+v", result)
	}
	out := mustParseJSON(t, result)
	if out["partial"] != true {
		t.Fatalf("expected partial=true, got %v", out)
	}
	if jobsErr, _ := out["jobs_error"].(string); jobsErr == "" {
		t.Fatalf("expected jobs_error to be set, got %v", out)
	}
	pipeline, _ := out["pipeline"].(map[string]any)
	if pipeline == nil || pipeline["status"] != "running" {
		t.Fatalf("expected pipeline payload, got %v", out)
	}
}

// A policy-disabled queue must fall back to the pre-queue direct merge —
// mirroring the cluster merge stage's own disable semantics. Every other
// queue non-success (full lane, unreachable) stays a hard error elsewhere.
func TestHandleMergeMergeRequest_FleetDisabledFallsBackToDirectMerge(t *testing.T) {
	var mergePUTs, enqueues int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/merge_requests/9"):
			fmt.Fprint(w, `{"iid":9,"sha":"head-9","source_branch":"feat/y","target_branch":"main"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/mills/merge-queue/enqueue":
			atomic.AddInt32(&enqueues, 1)
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"outcome":"disabled"}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			atomic.AddInt32(&mergePUTs, 1)
			fmt.Fprint(w, `{"iid":9,"state":"merged"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	gl := newTestServer(ts)
	gl.mergeQueueURL = ts.URL
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{"project": "p", "merge_request_iid": 9})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if enqueues != 1 || mergePUTs != 1 {
		t.Fatalf("enqueues=%d mergePUTs=%d, want 1 and 1", enqueues, mergePUTs)
	}
}

// A full lane must NOT silently degrade into an unserialized direct merge.
func TestHandleMergeMergeRequest_FleetFullLaneStaysAnError(t *testing.T) {
	var mergePUTs int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			fmt.Fprint(w, `{"iid":11,"sha":"head-11","source_branch":"feat/z","target_branch":"main"}`)
		case http.MethodPost:
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"outcome":"full"}`)
		case http.MethodPut:
			atomic.AddInt32(&mergePUTs, 1)
			t.Error("full lane must not direct-merge")
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	gl := newTestServer(ts)
	gl.mergeQueueURL = ts.URL
	result, err := gl.handleMergeMergeRequest(context.Background(), map[string]any{"project": "p", "merge_request_iid": 11})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("expected error result, got result=%+v err=%v", result, err)
	}
	if mergePUTs != 0 {
		t.Fatalf("mergePUTs=%d, want 0", mergePUTs)
	}
}

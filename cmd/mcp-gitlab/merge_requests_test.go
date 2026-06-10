package main

import (
	"context"
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

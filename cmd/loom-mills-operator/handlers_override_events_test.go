package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// findOverrideEvent returns the operator.override.<action> event recorded for a
// subject, or nil. Assertions read through the store rather than the response
// body: the label is only useful if it is durable.
func findOverrideEvent(t *testing.T, op *operator, subjectKind, subjectID, action string) *store.Event {
	t.Helper()
	events, err := op.store.Events.ListBySubject(context.Background(), subjectKind, subjectID, 200)
	if err != nil {
		t.Fatalf("list events for %s/%s: %v", subjectKind, subjectID, err)
	}
	for _, e := range events {
		if e.Kind == "operator.override."+action {
			return e
		}
	}
	return nil
}

// assertOverrideEvent checks actor, kind, and the full payload contract for one
// recorded override. wantReason == "" asserts the reason key is absent.
func assertOverrideEvent(t *testing.T, op *operator, subjectKind, subjectID, action, wantReason string) {
	t.Helper()
	e := findOverrideEvent(t, op, subjectKind, subjectID, action)
	if e == nil {
		t.Fatalf("no operator.override.%s event for %s/%s", action, subjectKind, subjectID)
	}
	if e.Actor != "operator.manual" {
		t.Errorf("%s actor = %q, want operator.manual", action, e.Actor)
	}
	if e.SubjectKind != subjectKind || e.SubjectID != subjectID {
		t.Errorf("%s subject = %s/%s, want %s/%s", action, e.SubjectKind, e.SubjectID, subjectKind, subjectID)
	}
	if got := e.Payload["action"]; got != action {
		t.Errorf("%s payload action = %v, want %q", action, got, action)
	}
	if got := e.Payload["subject_kind"]; got != subjectKind {
		t.Errorf("%s payload subject_kind = %v, want %q", action, got, subjectKind)
	}
	if got := e.Payload["subject_id"]; got != subjectID {
		t.Errorf("%s payload subject_id = %v, want %q", action, got, subjectID)
	}
	reason, present := e.Payload["reason"]
	switch {
	case wantReason == "" && present:
		t.Errorf("%s payload carries reason %v, want none", action, reason)
	case wantReason != "" && reason != wantReason:
		t.Errorf("%s payload reason = %v, want %q", action, reason, wantReason)
	}
}

// TestOverrideEvents_WorkflowLifecycle proves the three workflow-run manual
// mutations each land a labeled event: query-param reason on pause, no reason
// on resume, JSON-body reason on fail.
func TestOverrideEvents_WorkflowLifecycle(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)
	setAdminToken(wfLifecycleTestToken)
	t.Cleanup(func() { setAdminToken("") })

	req := httptest.NewRequest(http.MethodPost, "/api/mills/workflow/runs/WF-LIVE/pause?reason=wedged+step", nil)
	req.Header.Set("Authorization", "Bearer "+wfLifecycleTestToken)
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: got %d body=%s", rec.Code, rec.Body.String())
	}
	assertOverrideEvent(t, op, "workflow_run", "WF-LIVE", "pause", "wedged step")

	if rec := postWorkflowLifecycle(t, op, "WF-LIVE", "resume", "", true); rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d body=%s", rec.Code, rec.Body.String())
	}
	assertOverrideEvent(t, op, "workflow_run", "WF-LIVE", "resume", "")

	if rec := postWorkflowLifecycle(t, op, "WF-LIVE", "fail", `{"reason":"zombie spawn"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("fail: got %d body=%s", rec.Code, rec.Body.String())
	}
	assertOverrideEvent(t, op, "workflow_run", "WF-LIVE", "fail", "zombie spawn")
}

// seedPausedPipelineRun stores a backlog item + paused run pair that
// POST /resume can legally transition.
func seedPausedPipelineRun(t *testing.T, op *operator, itemID, runID string) {
	t.Helper()
	ctx := context.Background()
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: itemID, Title: itemID, State: store.BacklogPaused,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog %s: %v", itemID, err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: runID, BacklogID: itemID, Template: "mills-default-pipeline",
		State: store.PipelinePaused, Attempts: 1, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
}

func TestOverrideEvents_PipelineResume(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	seedPausedPipelineRun(t, op, "MILLS-OVR-RESUME", "PIPE-OVR-RESUME")

	req := httptest.NewRequest(http.MethodPost,
		"/api/mills/pipeline/runs/PIPE-OVR-RESUME/resume?reason=infra+recovered", nil)
	req.Header.Set("Authorization", "Bearer secret-abc")
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d body=%s", rec.Code, rec.Body.String())
	}
	assertOverrideEvent(t, op, "pipeline_run", "PIPE-OVR-RESUME", "resume", "infra recovered")
}

func TestOverrideEvents_PipelineForceEscalate(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-OVR-ESC", Title: "escalate me", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-OVR-ESC", BacklogID: "MILLS-OVR-ESC", Template: "mills-default-pipeline",
		State: store.PipelinePlanning, CurrentStage: "plan_slice", Attempts: 1,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/PIPE-OVR-ESC/escalate",
		strings.NewReader(`{"reason":"needs a human"}`))
	req.Header.Set("Authorization", "Bearer secret-abc")
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("escalate: got %d body=%s", rec.Code, rec.Body.String())
	}
	assertOverrideEvent(t, op, "pipeline_run", "PIPE-OVR-ESC", "force_escalate", "needs a human")
}

// TestOverrideEvents_PipelineRequeue covers the manual re-run of an escalated
// item. The reconciler writes its own "reconciler.requeued" event; this asserts
// the human decision is additionally labeled under the manual actor.
func TestOverrideEvents_PipelineRequeue(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-OVR-REQUEUE", Title: "escalated; human re-runs me",
		State: store.BacklogEscalated, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	rec := mills.NewReconciler(op.store, op.policy, op.budget, &recordingPipelineStarter{})
	rec.AutonomyGate = func(context.Context) (bool, []string) { return true, nil }
	op.withReconciler(rec)

	req := httptest.NewRequest(http.MethodPost,
		"/api/mills/pipeline/runs/MILLS-OVR-REQUEUE/start?requeue=1&reason=flaky+CI", nil)
	req.Header.Set("Authorization", "Bearer secret-abc")
	resp := httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("requeue start: got %d body=%s", resp.Code, resp.Body.String())
	}
	assertOverrideEvent(t, op, "backlog_item", "MILLS-OVR-REQUEUE", "requeue", "flaky CI")
}

// TestOverrideEvents_AppendFailureDoesNotFailMutation drops the events table so
// every append errors, then proves the override still applies: the label is
// best-effort, the operator's action is not.
func TestOverrideEvents_AppendFailureDoesNotFailMutation(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()
	seedPausedPipelineRun(t, op, "MILLS-OVR-BROKEN", "PIPE-OVR-BROKEN")

	if _, err := op.store.DB().ExecContext(ctx, `DROP TABLE events`); err != nil {
		t.Fatalf("drop events table: %v", err)
	}
	if err := op.store.Events.Append(ctx, &store.Event{Actor: "probe", Kind: "probe"}); err == nil {
		t.Fatal("append still succeeds after dropping events; the failure is not simulated")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/PIPE-OVR-BROKEN/resume", nil)
	req.Header.Set("Authorization", "Bearer secret-abc")
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume with broken events store: got %d body=%s", rec.Code, rec.Body.String())
	}
	run, err := op.store.Pipeline.GetRun(ctx, "PIPE-OVR-BROKEN")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != store.PipelineQueued {
		t.Fatalf("run state = %q, want %q", run.State, store.PipelineQueued)
	}
}

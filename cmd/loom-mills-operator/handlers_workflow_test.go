package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

// seedWorkflowFixtures inserts two imperative runs into the operator's store:
//   - WF-LIVE: a running run with a live spawn step (effect_count=1, real cost)
//     and a replay/cache-hit step (success, effect_count=0).
//   - WF-QUAR: a quarantined run with one step.
//
// Returns the seeded run ids so callers can assert ordering / detail.
func seedWorkflowFixtures(t *testing.T, op *operator) {
	t.Helper()
	ctx := context.Background()
	dao := op.store.Workflow

	// Seed the backlog items the runs reference so the workflow_runs.backlog_id
	// FK to backlog_items resolves.
	for _, id := range []string{"MILLS-WF-1", "MILLS-WF-2"} {
		if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: id, Title: id, State: store.BacklogQueued,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog %s: %v", id, err)
		}
	}

	t0 := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)

	live := &store.WorkflowRun{
		ID: "WF-LIVE", BacklogID: "MILLS-WF-1",
		Engine: store.WorkflowEngineImperative, Template: "workflow-canary",
		TemplateVersion: "v0", InterpreterVersion: "h1",
		WorkflowParams: `{"agent_type":"codex"}`,
		State:          store.WorkflowRunRunning, StartedAt: &t0, CostUSD: 0.42,
	}
	if err := dao.PutWorkflowRun(ctx, live); err != nil {
		t.Fatalf("put live run: %v", err)
	}
	// A live spawn step: ran an effect this execution.
	if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-LIVE", StepKey: "spawn:0", EventType: store.WorkflowEventSpawnResult,
		CallHash: "h-spawn", Status: store.WorkflowStepSuccess, SpawnID: "spawn-abc",
		StartedAt: &t0, EndedAt: &t1, CostUSD: 0.42,
		CostSource: store.WorkflowCostReal, EffectCount: 1,
	}); err != nil {
		t.Fatalf("append live step: %v", err)
	}
	// A cache-hit step: success but no live effect (replay short-circuit).
	if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-LIVE", StepKey: "gate:0", EventType: store.WorkflowEventGateEval,
		CallHash: "h-gate", Status: store.WorkflowStepSuccess,
		StartedAt: &t1, EndedAt: &t2, CostUSD: 0,
		CostSource: store.WorkflowCostUnavailable, EffectCount: 0,
	}); err != nil {
		t.Fatalf("append cache-hit step: %v", err)
	}

	quar := &store.WorkflowRun{
		ID: "WF-QUAR", BacklogID: "MILLS-WF-2",
		Engine: store.WorkflowEngineImperative, Template: "workflow-canary",
		TemplateVersion: "v0", InterpreterVersion: "h1",
		State: store.WorkflowRunQuarantined, StartedAt: &t1, CostUSD: 0.1,
	}
	if err := dao.PutWorkflowRun(ctx, quar); err != nil {
		t.Fatalf("put quarantined run: %v", err)
	}
	if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-QUAR", StepKey: "spawn:0", EventType: store.WorkflowEventSpawnRequested,
		CallHash: "h-q", Status: store.WorkflowStepSuccess,
		StartedAt: &t1, EndedAt: &t2, CostUSD: 0.1,
		CostSource: store.WorkflowCostReal, EffectCount: 1,
	}); err != nil {
		t.Fatalf("append quar step: %v", err)
	}
}

// TestHandleWorkflowRunsList_Shape verifies the list endpoint returns the
// summary shape, newest-first, bounded by limit.
func TestHandleWorkflowRunsList_Shape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	type runRow struct {
		ID        string  `json:"id"`
		BacklogID string  `json:"backlog_id"`
		AgentType string  `json:"agent_type"`
		Engine    string  `json:"engine"`
		Template  string  `json:"template"`
		State     string  `json:"state"`
		CostUSD   float64 `json:"cost_usd"`
		StepCount int     `json:"step_count"`
	}
	var body struct {
		Runs []runRow `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(body.Runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %s", len(body.Runs), rec.Body.String())
	}
	// Newest-first: WF-QUAR started at t1 > WF-LIVE at t0, so it sorts first.
	if body.Runs[0].ID != "WF-QUAR" {
		t.Errorf("ordering: want WF-QUAR first, got %q", body.Runs[0].ID)
	}
	byID := map[string]runRow{}
	for _, r := range body.Runs {
		byID[r.ID] = r
	}
	live, ok := byID["WF-LIVE"]
	if !ok {
		t.Fatal("WF-LIVE missing from list")
	}
	if live.Engine != "imperative" || live.Template != "workflow-canary" ||
		live.State != "running" || live.BacklogID != "MILLS-WF-1" ||
		live.AgentType != "codex" || live.CostUSD != 0.42 {
		t.Errorf("WF-LIVE summary fields wrong: %+v", live)
	}
	// The list now carries a real per-run step_count (the backend follow-up to
	// the Work › Workflows fix), so the Mills table renders the count instead
	// of a permanent "—". WF-LIVE has 2 journaled steps; WF-QUAR has 1.
	if live.StepCount != 2 {
		t.Errorf("WF-LIVE step_count: want 2, got %d", live.StepCount)
	}
	if quar := byID["WF-QUAR"]; quar.StepCount != 1 {
		t.Errorf("WF-QUAR step_count: want 1, got %d", quar.StepCount)
	}
}

// TestHandleWorkflowRunsList_LimitValidation rejects a non-positive limit.
func TestHandleWorkflowRunsList_LimitValidation(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs?limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0: want 400, got %d", rec.Code)
	}
}

// TestHandleWorkflowRunGet_NestedStepsAndBadges verifies the detail endpoint
// returns the run + nested steps and the server-derived badge per step.
func TestHandleWorkflowRunGet_NestedStepsAndBadges(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs/WF-LIVE", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Run struct {
			ID                 string `json:"id"`
			State              string `json:"state"`
			AgentType          string `json:"agent_type"`
			TemplateVersion    string `json:"template_version"`
			InterpreterVersion string `json:"interpreter_version"`
			StepCount          int    `json:"step_count"`
		} `json:"run"`
		Steps []struct {
			StepKey     string `json:"step_key"`
			Status      string `json:"status"`
			CostSource  string `json:"cost_source"`
			EffectCount int    `json:"effect_count"`
			Badge       string `json:"badge"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body.Run.ID != "WF-LIVE" || body.Run.AgentType != "codex" ||
		body.Run.TemplateVersion != "v0" || body.Run.InterpreterVersion != "h1" ||
		body.Run.StepCount != 2 || len(body.Steps) != 2 {
		t.Fatalf("run/steps shape wrong: %+v", body)
	}
	byKey := map[string]string{} // step_key -> badge
	for _, s := range body.Steps {
		byKey[s.StepKey] = s.Badge
	}
	// spawn:0 ran a live effect (effect_count=1, success) → "live".
	if byKey["spawn:0"] != "live" {
		t.Errorf("spawn:0 badge: want live, got %q", byKey["spawn:0"])
	}
	// gate:0 succeeded with effect_count=0 → cache_hit (replay).
	if byKey["gate:0"] != "cache_hit" {
		t.Errorf("gate:0 badge: want cache_hit, got %q", byKey["gate:0"])
	}
}

// TestHandleWorkflowRunGet_QuarantinedBadge verifies that every step under a
// quarantined run is badged "quarantined" regardless of its own status.
func TestHandleWorkflowRunGet_QuarantinedBadge(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs/WF-QUAR", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Steps []struct {
			Badge string `json:"badge"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(body.Steps))
	}
	// The step is success+effect_count=1 (would be "live"), but the run is
	// quarantined so the badge is "quarantined".
	if body.Steps[0].Badge != "quarantined" {
		t.Errorf("quarantined run step badge: want quarantined, got %q", body.Steps[0].Badge)
	}
}

// TestHandleWorkflowRunGet_NotFound returns 404 for an unknown run id.
func TestHandleWorkflowRunGet_NotFound(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

// ----- Workflow run lifecycle mutation tests ---------------------------------

// wfLifecycleTestToken authenticates the admin-gated lifecycle endpoints.
const wfLifecycleTestToken = "wf-lifecycle-test-token"

// postWorkflowLifecycle drives one POST /api/mills/workflow/runs/{id}/{verb}
// through the real mux (so requireAdmin + routing are exercised) and returns
// the recorder.
func postWorkflowLifecycle(t *testing.T, op *operator, id, verb, body string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/mills/workflow/runs/"+id+"/"+verb, rdr)
	if authed {
		req.Header.Set("Authorization", "Bearer "+wfLifecycleTestToken)
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	return rec
}

// getWorkflowRunState reads the run back through the store for assertions.
func getWorkflowRunState(t *testing.T, op *operator, id string) *store.WorkflowRun {
	t.Helper()
	run, err := op.store.Workflow.GetWorkflowRun(context.Background(), id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

// TestHandleWorkflowRunLifecycle_PauseResumeFail walks the full state machine:
// running → paused → running → error, asserting the persisted fields and the
// response view at each hop, then confirms every invalid transition 409s.
func TestHandleWorkflowRunLifecycle_PauseResumeFail(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)
	setAdminToken(wfLifecycleTestToken)
	t.Cleanup(func() { setAdminToken("") })

	// pause: running → paused.
	rec := postWorkflowLifecycle(t, op, "WF-LIVE", "pause", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	run := getWorkflowRunState(t, op, "WF-LIVE")
	if run.State != store.WorkflowRunPaused || run.PausedAt == nil {
		t.Fatalf("pause: state=%q paused_at=%v", run.State, run.PausedAt)
	}

	// pause again: 409 (not running).
	if rec := postWorkflowLifecycle(t, op, "WF-LIVE", "pause", "", true); rec.Code != http.StatusConflict {
		t.Fatalf("double pause: want 409, got %d", rec.Code)
	}

	// resume: paused → running, PausedAt cleared, ResumedAt stamped.
	rec = postWorkflowLifecycle(t, op, "WF-LIVE", "resume", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	run = getWorkflowRunState(t, op, "WF-LIVE")
	if run.State != store.WorkflowRunRunning || run.PausedAt != nil || run.ResumedAt == nil {
		t.Fatalf("resume: state=%q paused_at=%v resumed_at=%v", run.State, run.PausedAt, run.ResumedAt)
	}

	// resume again: 409 (not paused).
	if rec := postWorkflowLifecycle(t, op, "WF-LIVE", "resume", "", true); rec.Code != http.StatusConflict {
		t.Fatalf("double resume: want 409, got %d", rec.Code)
	}

	// fail: running → error, EndedAt stamped; reason body accepted.
	rec = postWorkflowLifecycle(t, op, "WF-LIVE", "fail", `{"reason":"zombie spawn"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		EndedAt string `json:"ended_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode fail view: %v body=%s", err, rec.Body.String())
	}
	if view.ID != "WF-LIVE" || view.State != "error" || view.EndedAt == "" {
		t.Fatalf("fail view: %+v", view)
	}
	run = getWorkflowRunState(t, op, "WF-LIVE")
	if run.State != store.WorkflowRunError || run.EndedAt == nil {
		t.Fatalf("fail: state=%q ended_at=%v", run.State, run.EndedAt)
	}

	// Every mutation on a terminal run: 409.
	for _, verb := range []string{"pause", "resume", "fail"} {
		if rec := postWorkflowLifecycle(t, op, "WF-LIVE", verb, "", true); rec.Code != http.StatusConflict {
			t.Errorf("%s on terminal run: want 409, got %d", verb, rec.Code)
		}
	}
}

// TestHandleWorkflowRunLifecycle_FailPausedRun asserts a paused run can be
// failed directly (the pause-then-fail mitigation flow).
func TestHandleWorkflowRunLifecycle_FailPausedRun(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)
	setAdminToken(wfLifecycleTestToken)
	t.Cleanup(func() { setAdminToken("") })

	if rec := postWorkflowLifecycle(t, op, "WF-LIVE", "pause", "", true); rec.Code != http.StatusOK {
		t.Fatalf("pause: want 200, got %d", rec.Code)
	}
	if rec := postWorkflowLifecycle(t, op, "WF-LIVE", "fail", "", true); rec.Code != http.StatusOK {
		t.Fatalf("fail paused: want 200, got %d", rec.Code)
	}
	if run := getWorkflowRunState(t, op, "WF-LIVE"); run.State != store.WorkflowRunError {
		t.Fatalf("state: want error, got %q", run.State)
	}
}

// TestHandleWorkflowRunLifecycle_AuthAndNotFound pins the admin gate
// (fail-closed without a bearer token) and the 404 for an unknown run id.
func TestHandleWorkflowRunLifecycle_AuthAndNotFound(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)
	setAdminToken(wfLifecycleTestToken)
	t.Cleanup(func() { setAdminToken("") })

	// No bearer token → 401, and the run is untouched.
	if rec := postWorkflowLifecycle(t, op, "WF-LIVE", "pause", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pause: want 401, got %d", rec.Code)
	}
	if run := getWorkflowRunState(t, op, "WF-LIVE"); run.State != store.WorkflowRunRunning {
		t.Fatalf("unauthenticated pause mutated the run: %q", run.State)
	}

	// Unknown id → 404.
	if rec := postWorkflowLifecycle(t, op, "WF-NOPE", "fail", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: want 404, got %d", rec.Code)
	}
}

func TestWorkflowCanaryRequiresBarrierAndIsSingleton(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	requestBody := `{"run_id":"wf-canary-stable-request","agent_type":"codex"}`

	rec := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(rec, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary", strings.NewReader(requestBody)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("open global admission status=%d body=%s", rec.Code, rec.Body.String())
	}

	off := false
	op.policy.Current().Enabled = &off
	op.policy.Current().Workflows.Enabled = true
	op.policy.Current().Workflows.SubstrateK8sOnly = true
	rec = httptest.NewRecorder()
	op.handleWorkflowCanaryStart(rec, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary", strings.NewReader(requestBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("barrier canary status=%d body=%s", rec.Code, rec.Body.String())
	}
	var launched struct {
		AgentType string `json:"agent_type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &launched); err != nil || launched.AgentType != "codex" {
		t.Fatalf("launch agent_type=%q err=%v body=%s", launched.AgentType, err, rec.Body.String())
	}
	persisted, err := op.store.Workflow.GetWorkflowRun(context.Background(), "wf-canary-stable-request")
	if err != nil {
		t.Fatalf("load launched canary: %v", err)
	}
	if got, err := workflow.CanaryAgentTypeFromRun(persisted); err != nil || got != "codex" {
		t.Fatalf("persisted agent_type=%q err=%v", got, err)
	}

	// A lost response is safe to retry with the caller-owned id. The handler
	// returns the original run without re-opening or overwriting its lifecycle.
	retry := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(retry, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary", strings.NewReader(requestBody)))
	if retry.Code != http.StatusOK || retry.Header().Get("X-Mills-Idempotent-Replay") != "true" {
		t.Fatalf("idempotent retry status=%d replay=%q body=%s",
			retry.Code, retry.Header().Get("X-Mills-Idempotent-Replay"), retry.Body.String())
	}

	mismatch := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(mismatch, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary",
		strings.NewReader(`{"run_id":"wf-canary-stable-request","agent_type":"claude-code"}`)))
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatched agent retry status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}

	second := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(second, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary",
		strings.NewReader(`{"run_id":"wf-canary-competing-request"}`)))
	if second.Code != http.StatusConflict {
		t.Fatalf("competing canary status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestWorkflowCanaryRejectsUnsupportedAgentType(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(rec, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary",
		strings.NewReader(`{"run_id":"wf-canary-unsupported-agent","agent_type":"gemini"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported agent status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := op.store.Workflow.GetWorkflowRun(context.Background(), "wf-canary-unsupported-agent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unsupported agent created a run: %v", err)
	}
}

func TestWorkflowCanaryRejectsInvalidOrMismatchedStableID(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	bad := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(bad, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary",
		strings.NewReader(`{"run_id":"other-run"}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status=%d body=%s", bad.Code, bad.Body.String())
	}

	now := time.Now().UTC()
	if err := op.store.Workflow.PutWorkflowRun(context.Background(), &store.WorkflowRun{
		ID: "wf-canary-collision", Engine: store.WorkflowEngineDAG, Template: "other",
		TemplateVersion: "v1", InterpreterVersion: "other", State: store.WorkflowRunRunning, StartedAt: &now,
	}); err != nil {
		t.Fatalf("seed collision: %v", err)
	}
	collision := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(collision, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary",
		strings.NewReader(`{"run_id":"wf-canary-collision"}`)))
	if collision.Code != http.StatusConflict {
		t.Fatalf("collision status=%d body=%s", collision.Code, collision.Body.String())
	}
}

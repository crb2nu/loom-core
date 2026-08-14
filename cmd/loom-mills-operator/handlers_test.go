package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type verdictMRStateStub struct {
	state string
	err   error
	iids  []int64
}

func (s *verdictMRStateStub) MRState(_ context.Context, iid int64) (string, error) {
	s.iids = append(s.iids, iid)
	return s.state, s.err
}

type verdictProjectResolverStub struct {
	project string
	err     error
}

func (s verdictProjectResolverStub) AuthorizedProject(context.Context, string) (string, error) {
	return s.project, s.err
}

func seedVerdictRun(t *testing.T, op *operator, id string, state store.PipelineState) {
	t.Helper()
	ended := time.Now().UTC()
	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{
		ID: "BL-" + id, Title: "verdict fixture", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed verdict backlog: %v", err)
	}
	if err := op.store.Pipeline.PutRun(context.Background(), &store.PipelineRun{
		ID: id, BacklogID: "BL-" + id, Template: "test", State: state,
		StartedAt: ended.Add(-time.Hour), EndedAt: &ended, EscalationClass: "code",
	}); err != nil {
		t.Fatalf("seed verdict run: %v", err)
	}
}

func postRunVerdict(op *operator, id, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/"+id+"/verdict", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	return rec
}

func TestHandlePipelineRunVerdict_AdminValidationAndGitLabFailures(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("verdict-secret")
	defer setAdminToken("")
	seedVerdictRun(t, op, "PIPE-ESC", store.PipelineEscalated)
	seedVerdictRun(t, op, "PIPE-DONE", store.PipelineDone)
	valid := `{"class":"merged_after_escalation","outcome":"manual_rescue","mr_iid":1575,"note":"merged via queue"}`

	if rec := postRunVerdict(op, "PIPE-ESC", valid, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing admin token status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "PIPE-ESC", valid, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong admin token status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "PIPE-ESC", `{`, "verdict-secret"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "PIPE-ESC", valid+` {}`, "verdict-secret"); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "PIPE-ESC", `{"class":"done","outcome":"x","mr_iid":1}`, "verdict-secret"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid fields status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "missing", valid, "verdict-secret"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing run status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "PIPE-DONE", valid, "verdict-secret"); rec.Code != http.StatusConflict {
		t.Fatalf("non-escalated status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "PIPE-ESC", valid, "verdict-secret"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired GitLab status=%d body=%s", rec.Code, rec.Body.String())
	}

	failed := &verdictMRStateStub{err: errors.New("gitlab down")}
	op.withVerdictMRVerification(func(string) mills.MRStateClient { return failed }, verdictProjectResolverStub{project: "services/loom-core"}, "")
	if rec := postRunVerdict(op, "PIPE-ESC", valid, "verdict-secret"); rec.Code != http.StatusBadGateway {
		t.Fatalf("GitLab error status=%d body=%s", rec.Code, rec.Body.String())
	}
	unmerged := &verdictMRStateStub{state: "opened"}
	op.withVerdictMRVerification(func(string) mills.MRStateClient { return unmerged }, verdictProjectResolverStub{project: "services/loom-core"}, "")
	if rec := postRunVerdict(op, "PIPE-ESC", valid, "verdict-secret"); rec.Code != http.StatusConflict {
		t.Fatalf("unmerged status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlePipelineRunVerdict_RejectsDifferentRecordedMR(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("verdict-secret")
	defer setAdminToken("")
	seedVerdictRun(t, op, "PIPE-WITH-MR", store.PipelineEscalated)
	mrIID := int64(1569)
	run, err := op.store.Pipeline.GetRun(context.Background(), "PIPE-WITH-MR")
	if err != nil {
		t.Fatal(err)
	}
	run.MRIID = &mrIID
	if err := op.store.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	body := `{"class":"merged_after_escalation","outcome":"manual_rescue","mr_iid":1575}`
	if rec := postRunVerdict(op, run.ID, body, "verdict-secret"); rec.Code != http.StatusConflict {
		t.Fatalf("mismatched MR status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlePipelineRunVerdict_SuccessReplayAndRunGet(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("verdict-secret")
	defer setAdminToken("")
	seedVerdictRun(t, op, "PIPE-RESCUED", store.PipelineEscalated)
	merged := &verdictMRStateStub{state: "merged"}
	// Legacy run: durable MR-stage provenance is unavailable, so verification
	// intentionally falls back to the configured home project.
	op.withVerdictMRVerification(func(project string) mills.MRStateClient {
		if project != "services/loom-core" {
			t.Fatalf("verification project=%q", project)
		}
		return merged
	}, verdictProjectResolverStub{err: store.ErrPipelineProjectUnavailable}, "services/loom-core")
	body := `{"class":"merged_after_escalation","outcome":"manual_rescue","mr_iid":1575,"note":"merged via queue"}`
	if rec := postRunVerdict(op, "PIPE-RESCUED", body, "verdict-secret"); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"appended":true`) {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postRunVerdict(op, "PIPE-RESCUED", body, "verdict-secret"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"appended":false`) {
		t.Fatalf("replay status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/PIPE-RESCUED", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("run GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"class":"merged_after_escalation"`, `"source":"operator_override"`, `"outcome":"manual_rescue"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("run GET missing %s: %s", want, rec.Body.String())
		}
	}
	if len(merged.iids) != 2 || merged.iids[0] != 1575 {
		t.Fatalf("MR verification calls=%v", merged.iids)
	}
}

// ----- Read-only handlers -----

func TestHandlePolicy_ReturnsCurrent(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/policy", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"Version":1`, `"Pipeline"`, `"Council"`} {
		if !strings.Contains(body, want) {
			t.Errorf("policy missing %q: %s", want, body)
		}
	}
}

func TestHandlePipelineRuns_EmptyIsJSONArrayNotNull(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	// Must be `[]`, never `null` — a bare null forces every client to special-
	// case it and broke the mobile app's pipeline list.
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("empty active runs: got %q, want %q", got, "[]")
	}
	var runs []*store.PipelineRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("expected 0 runs, got %d", len(runs))
	}
}

func TestHandleKPIs_NoSnapshotIs404(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/kpis?window=1d", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 with empty kpi_snapshots, got %d", rec.Code)
	}
}

func TestHandleKPIs_ReturnsSnapshot(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	if err := op.store.KPI.RecordSnapshot(context.Background(), &store.KPISnapshot{
		WindowSeconds: 86400,
		Metrics:       map[string]any{"cost_per_merged": 1.23},
	}); err != nil {
		t.Fatalf("seed kpi: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/kpis?window=1d", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"cost_per_merged":1.23`) {
		t.Errorf("body missing metric: %s", rec.Body.String())
	}
}

func TestHandleKPIs_BadWindowIs400(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/kpis?window=42h", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad window, got %d", rec.Code)
	}
}

func TestHandleBacklog_ListAndGet(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	item := &store.BacklogItem{
		ID: "MILLS-T1", Title: "first", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := op.store.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/backlog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MILLS-T1") {
		t.Errorf("list body missing item: %s", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/mills/backlog/MILLS-T1", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("get: %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `"Title":"first"`) {
		t.Errorf("get body missing title: %s", rec2.Body.String())
	}
}

func TestHandleBacklog_GetMissingIs404(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/backlog/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleBacklog_CreateRoundTrip(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	body := `{"ID":"MILLS-CREATE-1","Title":"smoke item","Labels":["docs"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body))
	op.handleBacklogCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}

	got, err := op.store.Backlog.Get(context.Background(), "MILLS-CREATE-1")
	if err != nil {
		t.Fatalf("post-create get: %v", err)
	}
	if got.Title != "smoke item" {
		t.Errorf("title = %q, want smoke item", got.Title)
	}
	if got.State != store.BacklogQueued {
		t.Errorf("state = %q, want queued (default)", got.State)
	}
	if got.Priority != store.P3 {
		t.Errorf("priority = %q, want P3 (default)", got.Priority)
	}
	if got.CreatedBy != "api" {
		t.Errorf("created_by = %q, want api (default)", got.CreatedBy)
	}
	if got.Revision != 1 {
		t.Errorf("revision = %d, want 1", got.Revision)
	}
}

func TestHandleBacklog_UpdateRequiresCurrentRevision(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	ctx := context.Background()
	item := &store.BacklogItem{
		ID: "MILLS-CAS-HTTP", Title: "original", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
	}
	if err := op.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed: %v", err)
	}

	staleBody := `{"ID":"MILLS-CAS-HTTP","Title":"stale overwrite","Revision":0}`
	stale := httptest.NewRecorder()
	op.handleBacklogCreate(stale, httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(staleBody)))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale update: status=%d body=%s, want 409", stale.Code, stale.Body.String())
	}
	if !strings.Contains(stale.Body.String(), `"error":"stale-backlog-write"`) {
		t.Fatalf("stale update body=%s", stale.Body.String())
	}

	freshBody := `{"ID":"MILLS-CAS-HTTP","Title":"fresh update","Revision":1}`
	fresh := httptest.NewRecorder()
	op.handleBacklogCreate(fresh, httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(freshBody)))
	if fresh.Code != http.StatusCreated {
		t.Fatalf("fresh update: status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	got, err := op.store.Backlog.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "fresh update" || got.Revision != 2 {
		t.Fatalf("fresh update not persisted: %+v", got)
	}
}

// TestHandleBacklog_CreateDedupesRecentCanary locks in the 24h dedupe
// window on the operator: a second mills-canary POST within the
// window must return 409 + the canonical body shape that the CLI
// parses. Items missing the label, items older than 24h, and items in
// state=merged must not block.
func TestHandleBacklog_CreateDedupesRecentCanary(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	ctx := context.Background()

	// Recent escalated canary — should block.
	priorEscalated := &store.BacklogItem{
		ID:        "MILLS-CANARY-PRIOR",
		Title:     "prior",
		Labels:    []string{"mills-canary", "safe-fixture"},
		State:     store.BacklogEscalated,
		Priority:  store.P3,
		CreatedBy: "test",
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour),
		UpdatedAt: time.Now().UTC().Add(-2 * time.Hour),
	}
	if err := op.store.Backlog.Put(ctx, priorEscalated); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	body := `{"ID":"MILLS-CANARY-NEW","Title":"new canary","Labels":["mills-canary"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body))
	op.handleBacklogCreate(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dedupe: status = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 409 body: %v body=%s", err, rec.Body.String())
	}
	if resp["error"] != "canary-deduped" {
		t.Errorf("error = %q, want canary-deduped", resp["error"])
	}
	if resp["existing_id"] != "MILLS-CANARY-PRIOR" {
		t.Errorf("existing_id = %q, want MILLS-CANARY-PRIOR", resp["existing_id"])
	}
	if resp["existing_state"] != string(store.BacklogEscalated) {
		t.Errorf("existing_state = %q, want %s", resp["existing_state"], store.BacklogEscalated)
	}

	// Confirm the new row was *not* persisted (server-side guard works).
	if _, err := op.store.Backlog.Get(ctx, "MILLS-CANARY-NEW"); err == nil {
		t.Errorf("dedupe should have skipped the insert; MILLS-CANARY-NEW found in store")
	}
}

// TestHandleBacklog_CreateDedupeAllowsMergedAndOldCanaries proves the
// negative cases: a merged canary in the window, an in-flight canary
// older than the window, and an item without the canary label must
// all be permissible to follow with a new canary enqueue.
func TestHandleBacklog_CreateDedupeAllowsMergedAndOldCanaries(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Merged canary, fresh — not a blocker.
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-CANARY-MERGED", Title: "merged", Labels: []string{"mills-canary"},
		State: store.BacklogMerged, Priority: store.P3, CreatedBy: "test",
		CreatedAt: now.Add(-3 * time.Hour),
	}); err != nil {
		t.Fatalf("seed merged: %v", err)
	}
	// In-flight queued canary older than the dedupe window — not a
	// blocker (queued represents transient progress, the dedupe window
	// gives up on it as wedge-worthy after 24h and lets a new one in).
	// NOTE: the previous version of this seed used BacklogEscalated,
	// which is now blocked-forever; see TestHandleBacklog_
	// CreateDedupeBlocksEscalatedRegardlessOfAge for that case.
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-CANARY-STALE", Title: "stale", Labels: []string{"mills-canary"},
		State: store.BacklogQueued, Priority: store.P3, CreatedBy: "test",
		CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	// Non-canary in-flight row — must never trigger dedupe.
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-OTHER", Title: "other work", Labels: []string{"feature"},
		State: store.BacklogRunning, Priority: store.P2, CreatedBy: "test",
		CreatedAt: now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	body := `{"ID":"MILLS-CANARY-FRESH","Title":"fresh","Labels":["mills-canary"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body))
	op.handleBacklogCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := op.store.Backlog.Get(ctx, "MILLS-CANARY-FRESH"); err != nil {
		t.Errorf("fresh canary should persist: %v", err)
	}
}

// TestHandleBacklog_CreateDedupeBlocksEscalatedRegardlessOfAge pins
// the post-MR semantic: an escalated canary blocks new canary enqueues
// forever, not just within the 24h window. Escalation means "human
// must act before the next canary makes sense" — without this carve-
// out, a stuck escalation from >24h ago lets a new canary slip
// through every cycle, accumulating into the 30+ MILLS-CANARY-* /
// escalated rows visible in the HUD Backlog tab on 2026-05-24.
func TestHandleBacklog_CreateDedupeBlocksEscalatedRegardlessOfAge(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()

	// Escalated canary far outside the dedupe window — must STILL block.
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-CANARY-WEDGED", Title: "wedged", Labels: []string{"mills-canary"},
		State: store.BacklogEscalated, Priority: store.P3, CreatedBy: "test",
		CreatedAt: time.Now().UTC().Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed wedged: %v", err)
	}

	body := `{"ID":"MILLS-CANARY-NEW-AGAIN","Title":"new","Labels":["mills-canary"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body))
	op.handleBacklogCreate(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for escalated canary regardless of age, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["existing_id"] != "MILLS-CANARY-WEDGED" {
		t.Errorf("existing_id = %q, want MILLS-CANARY-WEDGED", resp["existing_id"])
	}
}

// TestHandleBacklog_CreateDedupeForceQuery confirms ?force=1 bypasses
// the guard so operators can rescue a wedged queue intentionally.
func TestHandleBacklog_CreateDedupeForceQuery(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	ctx := context.Background()
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-CANARY-PRIOR-F", Title: "prior", Labels: []string{"mills-canary"},
		State: store.BacklogEscalated, Priority: store.P3, CreatedBy: "test",
		CreatedAt: time.Now().UTC().Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	body := `{"ID":"MILLS-CANARY-FORCED","Title":"forced","Labels":["mills-canary"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog?force=1", strings.NewReader(body))
	op.handleBacklogCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("force should bypass dedupe; got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := op.store.Backlog.Get(ctx, "MILLS-CANARY-FORCED"); err != nil {
		t.Errorf("forced canary should persist: %v", err)
	}
}

// TestHandleBacklog_CreateRepostingSameIDIsNotDedupe covers the guarded update
// path: re-POSTing the same canary id is a retry of one row, not a duplicate,
// and succeeds when the caller echoes the current revision.
func TestHandleBacklog_CreateRepostingSameIDIsNotDedupe(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	ctx := context.Background()
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-CANARY-RETRY", Title: "retry", Labels: []string{"mills-canary"},
		State: store.BacklogEscalated, Priority: store.P3, CreatedBy: "test",
		CreatedAt: time.Now().UTC().Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{"ID":"MILLS-CANARY-RETRY","Title":"retry updated","Labels":["mills-canary"],"Revision":1}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body))
	op.handleBacklogCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("same-id repost should succeed (upsert); got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBacklog_CreateRequiresIDAndTitle(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	cases := []struct{ name, body string }{
		{"empty body", `{}`},
		{"missing id", `{"Title":"only title"}`},
		{"missing title", `{"ID":"X"}`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(tc.body))
		op.handleBacklogCreate(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", tc.name, rec.Code)
		}
	}
}

func TestHandleCouncilRuns_ListAndGet(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	if err := op.store.Council.Put(context.Background(), &store.CouncilRun{
		ID: "COUNCIL-T", Trigger: store.CouncilTriggerCron,
		StartedAt: time.Now().UTC(), Outcome: store.CouncilOutcomeSuccess,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/council/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "COUNCIL-T") {
		t.Errorf("list missing run: %s", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/mills/council/runs/COUNCIL-T", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("get: %d", rec2.Code)
	}
}

// TestHandleCouncilRunDebate covers the slice-5.3 debate transcript
// endpoint: 200 + populated array when debate ran, 200 + [] when the
// run had no debate, 404 when the run id itself is unknown.
func TestHandleCouncilRunDebate(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()

	// Seed a council run + 3-row debate transcript matching the
	// slice 5.2 fixture's converge-on-round-1 shape.
	if err := op.store.Council.Put(ctx, &store.CouncilRun{
		ID: "COUNCIL-DEBATE", Trigger: store.CouncilTriggerIncident,
		StartedAt: time.Now().UTC(), Outcome: store.CouncilOutcomeSuccess,
	}); err != nil {
		t.Fatalf("seed council: %v", err)
	}
	rounds := []*store.CouncilDebateRound{
		{CouncilRunID: "COUNCIL-DEBATE", RoundIndex: 0, Role: store.DebateRoleEditorProposes, CostUSD: 0.42, Summary: "draft v0"},
		{CouncilRunID: "COUNCIL-DEBATE", RoundIndex: 1, Role: store.DebateRoleReviewerCritiques, CostUSD: 0.40, Summary: "critiques"},
		{CouncilRunID: "COUNCIL-DEBATE", RoundIndex: 1, Role: store.DebateRoleModeratorDecision, CostUSD: 0.05, Summary: "converged"},
	}
	for i, r := range rounds {
		if err := op.store.Debate.AppendRound(ctx, r); err != nil {
			t.Fatalf("seed round %d: %v", i, err)
		}
	}

	// Seed a single-pass run so the API can show 200 + [] for runs
	// that don't have debate.
	if err := op.store.Council.Put(ctx, &store.CouncilRun{
		ID: "COUNCIL-NODEBATE", Trigger: store.CouncilTriggerCron,
		StartedAt: time.Now().UTC(), Outcome: store.CouncilOutcomeSuccess,
	}); err != nil {
		t.Fatalf("seed nodebate: %v", err)
	}

	cases := []struct {
		name      string
		runID     string
		wantCode  int
		wantRows  int
		wantNotIn string // substring that must NOT appear in body
	}{
		{"with_debate", "COUNCIL-DEBATE", http.StatusOK, 3, ""},
		{"no_debate_returns_empty", "COUNCIL-NODEBATE", http.StatusOK, 0, "draft v0"},
		{"unknown_run_404", "COUNCIL-MISSING", http.StatusNotFound, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/api/mills/council/runs/"+tc.runID+"/debate", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code: got %d want %d body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			var got []*store.CouncilDebateRound
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != tc.wantRows {
				t.Errorf("rows: got %d want %d", len(got), tc.wantRows)
			}
			if tc.wantNotIn != "" && strings.Contains(rec.Body.String(), tc.wantNotIn) {
				t.Errorf("body should not contain %q: %s", tc.wantNotIn, rec.Body.String())
			}
		})
	}
}

func TestHandlePipelineRuns_GetWithStagesAndGates(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-P", Title: "p", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-T1", BacklogID: "MILLS-P", Template: "mills-default-pipeline",
		State: store.PipelineImplementing, Attempts: 1, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	out := store.StageOutcomeSuccess
	end := time.Now().UTC().Add(time.Second)
	if err := op.store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: "PIPE-T1", Stage: "implement", Attempt: 1,
		StartedAt: time.Now().UTC(), EndedAt: &end, Outcome: &out,
	}); err != nil {
		t.Fatalf("seed stage: %v", err)
	}
	if err := op.store.Pipeline.PutGate(ctx, &store.GateOutcome{
		PipelineRunID: "PIPE-T1", AfterStage: "implement", GateName: "diff_size",
		Outcome: store.GateOutcomePass, JudgedBy: "go",
	}); err != nil {
		t.Fatalf("seed gate: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/PIPE-T1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["run"] == nil {
		t.Errorf("run missing")
	}
	stages, ok := resp["stages"].([]any)
	if !ok || len(stages) != 1 {
		t.Errorf("stages: %v", resp["stages"])
	}
	gates, ok := resp["gates"].([]any)
	if !ok || len(gates) != 1 {
		t.Errorf("gates: %v", resp["gates"])
	}
}

// TestHandlePipelineRuns_GetEmptyStagesGatesAreArrays is the regression test
// for the live-run drawer wedge: a run with no stage attempts or gate
// outcomes yet (every freshly-started run) must encode stages/gates as `[]`,
// not `null` — `null` crashed the HUD drawer's gate sort and froze the panel.
func TestHandlePipelineRuns_GetEmptyStagesGatesAreArrays(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-E", Title: "e", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-E1", BacklogID: "MILLS-E", Template: "mills-default-pipeline",
		State: store.PipelineQueued, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/PIPE-E1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stages, ok := resp["stages"].([]any); !ok || len(stages) != 0 {
		t.Errorf("stages must be an empty JSON array, got: %v", resp["stages"])
	}
	if gates, ok := resp["gates"].([]any); !ok || len(gates) != 0 {
		t.Errorf("gates must be an empty JSON array, got: %v", resp["gates"])
	}
}

// TestHandlePipelineRuns_TerminalHistory covers the run-history read mode:
// GET /api/mills/pipeline/runs?state=terminal returns finished runs (and
// excludes in-flight ones), and bad since/limit params are rejected 400.
func TestHandlePipelineRuns_TerminalHistory(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-H", Title: "h", State: store.BacklogRunning,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	now := time.Now().UTC()
	seed := func(id string, st store.PipelineState, attempt int, started time.Time) {
		t.Helper()
		if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
			ID: id, BacklogID: "MILLS-H", Template: "t", State: st,
			Attempts: attempt, StartedAt: started,
		}); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	seed("H-ACTIVE", store.PipelineImplementing, 1, now.Add(-10*time.Minute))
	seed("H-DONE", store.PipelineDone, 2, now.Add(-2*time.Hour))
	seed("H-ESC", store.PipelineEscalated, 3, now.Add(-1*time.Hour))
	seed("H-PAUSED", store.PipelinePaused, 4, now.Add(-30*time.Minute))

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs?state=terminal", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var runs []store.PipelineRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 terminal runs, got %d (%+v)", len(runs), runs)
	}
	// Newest-first: paused (-30m), escalated (-1h), done (-2h); active absent.
	if runs[0].ID != "H-PAUSED" || runs[1].ID != "H-ESC" || runs[2].ID != "H-DONE" {
		t.Fatalf("order = [%s %s %s], want [H-PAUSED H-ESC H-DONE]", runs[0].ID, runs[1].ID, runs[2].ID)
	}

	// Bad since → 400.
	recBad := httptest.NewRecorder()
	op.httpMux().ServeHTTP(recBad, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs?state=terminal&since=notatime", nil))
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("bad since: got %d want 400", recBad.Code)
	}
	// Bad limit → 400.
	recLim := httptest.NewRecorder()
	op.httpMux().ServeHTTP(recLim, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs?state=terminal&limit=0", nil))
	if recLim.Code != http.StatusBadRequest {
		t.Errorf("bad limit: got %d want 400", recLim.Code)
	}
}

type recordingPipelineStarter struct {
	calls int
	runID string
}

func (s *recordingPipelineStarter) Start(_ context.Context, run *store.PipelineRun, _ *store.BacklogItem) error {
	s.calls++
	s.runID = run.ID
	return nil
}

func TestHandlePipelineStart_StartsQueuedBacklogItem(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID:        "MILLS-START-1",
		Title:     "start me",
		State:     store.BacklogQueued,
		Priority:  store.P2,
		CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	starter := &recordingPipelineStarter{}
	rec := mills.NewReconciler(op.store, op.policy, op.budget, starter)
	rec.AutonomyGate = func(context.Context) (bool, []string) { return true, nil }
	op.withReconciler(rec)

	req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/MILLS-START-1/start", nil)
	req.Header.Set("Authorization", "Bearer secret-abc")
	resp := httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("start: got %d body=%s", resp.Code, resp.Body.String())
	}
	var body pipelineStartResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Decision != "started" || body.RunID == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if starter.calls != 1 || starter.runID != body.RunID {
		t.Fatalf("starter calls=%d runID=%q body=%q", starter.calls, starter.runID, body.RunID)
	}
	item, err := op.store.Backlog.Get(ctx, "MILLS-START-1")
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if item.State != store.BacklogRunning {
		t.Fatalf("backlog state: got %q want %q", item.State, store.BacklogRunning)
	}
}

// TestHandlePipelineStart_RequeueQueryReRunsEscalatedItem pins the
// ?requeue=1 contract end-to-end: an escalated item 409s on a plain
// start (with the state in `reason`) and starts on a requeue start.
func TestHandlePipelineStart_RequeueQueryReRunsEscalatedItem(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID:        "MILLS-REQUEUE-1",
		Title:     "escalated; human re-runs me",
		State:     store.BacklogEscalated,
		Priority:  store.P2,
		CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	starter := &recordingPipelineStarter{}
	rec := mills.NewReconciler(op.store, op.policy, op.budget, starter)
	rec.AutonomyGate = func(context.Context) (bool, []string) { return true, nil }
	op.withReconciler(rec)

	// Plain start: 409 with the state surfaced.
	req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/MILLS-REQUEUE-1/start", nil)
	req.Header.Set("Authorization", "Bearer secret-abc")
	resp := httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict {
		t.Fatalf("plain start: got %d body=%s", resp.Code, resp.Body.String())
	}
	var conflict pipelineStartResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if conflict.Reason != "state is escalated" {
		t.Fatalf("conflict reason: got %q", conflict.Reason)
	}

	// Requeue start: item flips queued → running with a fresh run.
	req = httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/MILLS-REQUEUE-1/start?requeue=1", nil)
	req.Header.Set("Authorization", "Bearer secret-abc")
	resp = httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("requeue start: got %d body=%s", resp.Code, resp.Body.String())
	}
	var body pipelineStartResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Decision != "started" || body.RunID == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
	item, err := op.store.Backlog.Get(ctx, "MILLS-REQUEUE-1")
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if item.State != store.BacklogRunning {
		t.Fatalf("backlog state: got %q want %q", item.State, store.BacklogRunning)
	}
}

func TestHandlePipelineEscalate_MarksRunAndBacklog(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID:        "MILLS-ESC-1",
		Title:     "escalate me",
		State:     store.BacklogRunning,
		Priority:  store.P2,
		CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-ESC-1", BacklogID: "MILLS-ESC-1", Template: "mills-default-pipeline",
		State: store.PipelinePlanning, CurrentStage: "plan_slice", Attempts: 1, StartedAt: time.Now().UTC(),
		ExternalDependencyID: "external_dependency.gitlab", ExternalDependency: "gitlab",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	started := time.Now().UTC().Add(-time.Minute)
	if _, err := op.store.Pipeline.BeginExternalIncidentDwell(ctx, "PIPE-ESC-1", "external_dependency.gitlab", "gitlab", started, started.Add(time.Hour)); err != nil {
		t.Fatalf("begin external incident dwell: %v", err)
	}
	op.withReconciler(mills.NewReconciler(op.store, op.policy, op.budget, nil))

	req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/PIPE-ESC-1/escalate",
		strings.NewReader(`{"reason":"test cleanup"}`))
	req.Header.Set("Authorization", "Bearer secret-abc")
	resp := httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("escalate: got %d body=%s", resp.Code, resp.Body.String())
	}
	run, err := op.store.Pipeline.GetRun(ctx, "PIPE-ESC-1")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != store.PipelineEscalated || run.EndedAt == nil {
		t.Fatalf("run after escalate = %+v", run)
	}
	item, err := op.store.Backlog.Get(ctx, "MILLS-ESC-1")
	if err != nil {
		t.Fatalf("get backlog: %v", err)
	}
	if item.State != store.BacklogEscalated {
		t.Fatalf("backlog state = %q, want %q", item.State, store.BacklogEscalated)
	}
	dwell, err := op.store.Pipeline.GetExternalIncidentDwell(ctx, run.ID)
	if err != nil {
		t.Fatalf("get external incident dwell: %v", err)
	}
	if dwell.CompletionReason != store.ExternalIncidentDwellFastKill || dwell.CompletedAt == nil {
		t.Fatalf("external incident dwell after escalation = %+v", dwell)
	}
}

func TestHandlePipelinePauseAndResume(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	spawnStops := &recordingSpawnStopper{}
	op.withSpawnClient(spawnStops)
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{ID: "MILLS-PAUSE-1", Title: "pause me", State: store.BacklogRunning, Priority: store.P2, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{ID: "PIPE-PAUSE-1", BacklogID: "MILLS-PAUSE-1", Template: "mills-default-pipeline", State: store.PipelinePlanning, Attempts: 1, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	outcome := store.StageOutcomeSuccess
	if err := op.store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: "PIPE-PAUSE-1",
		Stage:         "implement",
		Attempt:       1,
		StartedAt:     time.Now().UTC(),
		Outcome:       &outcome,
		SpawnID:       "spawn-live-1",
	}); err != nil {
		t.Fatal(err)
	}
	pause := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/PIPE-PAUSE-1/pause", strings.NewReader(`{"reason":"duplicate run"}`))
	pause.Header.Set("Authorization", "Bearer secret-abc")
	resp := httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, pause)
	if resp.Code != http.StatusOK {
		t.Fatalf("pause: got %d body=%s", resp.Code, resp.Body.String())
	}
	run, err := op.store.Pipeline.GetRun(ctx, "PIPE-PAUSE-1")
	if err != nil || run.State != store.PipelinePaused {
		t.Fatalf("paused run: %#v err=%v", run, err)
	}
	item, err := op.store.Backlog.Get(ctx, "MILLS-PAUSE-1")
	if err != nil || item.State != store.BacklogPaused {
		t.Fatalf("paused backlog: %#v err=%v", item, err)
	}
	if got := spawnStops.ids; len(got) != 1 || got[0] != "spawn-live-1" {
		t.Fatalf("spawn stops = %v, want [spawn-live-1]", got)
	}
	stages, err := op.store.Pipeline.ListStages(ctx, "PIPE-PAUSE-1")
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	foundReason := false
	for _, stage := range stages {
		if stage.Stage == "operator_pause" && stage.Artifacts["reason"] == "duplicate run" {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("operator pause reason artifact missing: %+v", stages)
	}

	resume := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/PIPE-PAUSE-1/resume", nil)
	resume.Header.Set("Authorization", "Bearer secret-abc")
	resp = httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, resume)
	if resp.Code != http.StatusOK {
		t.Fatalf("resume: got %d body=%s", resp.Code, resp.Body.String())
	}
	run, err = op.store.Pipeline.GetRun(ctx, "PIPE-PAUSE-1")
	if err != nil || run.State != store.PipelineQueued {
		t.Fatalf("queued run: %#v err=%v", run, err)
	}
	// Exactly-once: a second resume cannot replay the queued transition.
	resp = httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, resume)
	if resp.Code != http.StatusConflict {
		t.Fatalf("second resume: got %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestHandlePipelineResume_BacklogConflictLeavesRunPaused(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")
	ctx := context.Background()
	item := &store.BacklogItem{ID: "MILLS-PAUSE-CONFLICT", Title: "pause conflict", State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test"}
	if err := op.store.Backlog.Put(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := op.store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID:            item.ID,
		ExpectedClaimVersion: item.ClaimVersion,
		ExpectedRevision:     item.Revision,
		Template:             "mills-default-pipeline",
		RunID:                "PIPE-PAUSE-CONFLICT-CURRENT",
		Now:                  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("claim current aggregate: %v", err)
	}
	now := time.Now().UTC()
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-PAUSE-CONFLICT", BacklogID: "MILLS-PAUSE-CONFLICT",
		Template: "mills-default-pipeline", State: store.PipelinePaused,
		Attempts: 2, StartedAt: now.Add(-time.Minute), EndedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}

	resume := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/PIPE-PAUSE-CONFLICT/resume", nil)
	resume.Header.Set("Authorization", "Bearer secret-abc")
	resp := httptest.NewRecorder()
	op.httpMux().ServeHTTP(resp, resume)
	if resp.Code != http.StatusConflict {
		t.Fatalf("resume: got %d body=%s", resp.Code, resp.Body.String())
	}
	run, err := op.store.Pipeline.GetRun(ctx, "PIPE-PAUSE-CONFLICT")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != store.PipelinePaused {
		t.Fatalf("run state after failed resume = %s, want paused", run.State)
	}
}

type recordingSpawnStopper struct {
	ids []string
}

func (s *recordingSpawnStopper) Stop(_ context.Context, id string) error {
	s.ids = append(s.ids, id)
	return nil
}

func TestHandleEvalScores_EmptyOK(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/eval/scores", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with empty eval table, got %d", rec.Code)
	}
}

// TestHandleEvalScores_JSONShape pins the JSON field names emitted by
// /api/mills/eval/scores. The HUD EvalPanel reads SubjectKind /
// SubjectID / Rubric / JudgedBy / EvaluatedAt to derive the Loop
// letter and render rows; renaming or dropping any of these in
// pkg/mills/store.EvalScore would silently blank the panel.
func TestHandleEvalScores_JSONShape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	if err := op.store.Eval.RecordScore(context.Background(), &store.EvalScore{
		SubjectKind: store.EvalSubjectCrossRun,
		SubjectID:   "2026-05-10..2026-05-17",
		Rubric:      "loop_c_stale_plans",
		Score:       0.875,
		JudgedBy:    "loop_c_cross_run",
		EvaluatedAt: time.Now().UTC(),
		Notes:       "shape-contract",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/eval/scores?limit=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(rows) == 0 {
		t.Fatalf("expected at least one row, got %s", rec.Body.String())
	}
	row := rows[0]
	for _, want := range []string{"ID", "SubjectKind", "SubjectID", "Rubric", "Score", "JudgedBy", "EvaluatedAt", "Notes"} {
		if _, ok := row[want]; !ok {
			t.Errorf("eval JSON missing field %q (HUD EvalPanel depends on it); row=%v", want, row)
		}
	}
	if got, _ := row["SubjectKind"].(string); got != "cross_run" {
		t.Errorf("SubjectKind = %q, want \"cross_run\"", got)
	}
	if got, _ := row["JudgedBy"].(string); got != "loop_c_cross_run" {
		t.Errorf("JudgedBy = %q, want \"loop_c_cross_run\"", got)
	}
}

// ----- Admin-token gate -----

func TestRequireAdmin_NoTokenConfigured_Rejects(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("") // explicit fail-closed default

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/mills/council/run", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with no admin token configured, got %d", rec.Code)
	}
}

func TestRequireAdmin_MissingHeader_Rejects(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/mills/council/run", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 missing header, got %d", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("expected WWW-Authenticate Bearer hint, got %q", got)
	}
}

func TestRequireAdmin_WrongToken_Rejects(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")

	req := httptest.NewRequest(http.MethodPost, "/api/mills/council/run", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 wrong token, got %d", rec.Code)
	}
}

func TestRequireAdmin_CorrectTokenReachesHandler(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("secret-abc")
	defer setAdminToken("")

	req := httptest.NewRequest(http.MethodPost, "/api/mills/council/run", nil)
	req.Header.Set("Authorization", "Bearer secret-abc")
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	// newTestOperator doesn't wire a council runner, so the handler
	// short-circuits with 503. The point of this test is that the auth
	// gate *let the request through* to the handler — anything other
	// than 401 proves that. We assert 503 specifically so we'd notice
	// if the gate accidentally became authorisation-only-no-handler.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (no runner wired in tests), got %d body=%s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "council runner not configured") {
		t.Errorf("body should explain the 503: %s", rec.Body.String())
	}
}

func TestRequireAdmin_EveryMutatingEndpointIsGated(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	setAdminToken("") // fail-closed

	endpoints := []struct{ method, path string }{
		{http.MethodPost, "/api/mills/council/run"},
		{http.MethodPost, "/api/mills/council/dryrun"},
		{http.MethodPost, "/api/mills/pipeline/runs/MILLS-X/start"},
		{http.MethodPost, "/api/mills/pipeline/runs/PIPE-X/pause"},
		{http.MethodPost, "/api/mills/pipeline/runs/PIPE-X/resume"},
		{http.MethodPost, "/api/mills/pipeline/runs/PIPE-X/escalate"},
		{http.MethodPost, "/api/mills/backlog/sync"},
		{http.MethodPost, "/api/mills/eval/run-cross"},
	}
	for _, ep := range endpoints {
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, httptest.NewRequest(ep.method, ep.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d", ep.method, ep.path, rec.Code)
		}
	}
}

func TestSubtleEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "abcd", false},
		{"", "", true},
	}
	for _, c := range cases {
		if got := subtleEqual(c.a, c.b); got != c.want {
			t.Errorf("subtleEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw      string
		fallback int
		want     int
	}{
		{"", 50, 50},
		{"abc", 50, 50},
		{"-1", 50, 50},
		{"0", 50, 50},
		{"25", 50, 25},
		{"5000", 50, 1000},
	}
	for _, c := range cases {
		if got := parseLimit(c.raw, c.fallback); got != c.want {
			t.Errorf("parseLimit(%q, %d) = %d, want %d", c.raw, c.fallback, got, c.want)
		}
	}
}

func TestHandlePipelineRunsList_FiltersByMRIID(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-AUDIT", Title: "audit", State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	mr := int64(777)
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-AUDIT", BacklogID: "MILLS-AUDIT", Template: "mills-default-pipeline",
		State: store.PipelineMerging, CurrentStage: "merge", Attempts: 1,
		MRIID: &mr, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Match: terminal run surfaces via mr_iid even though it's not "active".
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs?mr_iid=777", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var runs []store.PipelineRun
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "PIPE-AUDIT" {
		t.Fatalf("mr_iid filter = %+v, want one run PIPE-AUDIT", runs)
	}

	// Unknown iid → 200 + [].
	rec = httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs?mr_iid=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown status: %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("unknown iid body = %q, want []", got)
	}

	// Non-integer iid → 400.
	rec = httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs?mr_iid=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad iid status: %d, want 400", rec.Code)
	}
}

package hud

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// newPatternTestApp builds a minimal App with a discard logger and a nil agent
// bridge, exercising the bridge-unavailable paths without a live daemon.
func newPatternTestApp() *App {
	return &App{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// agent is intentionally nil.
	}
}

// TestHandlePatternList_NilBridgeReturnsEmptyCatalog covers the front-door
// page's "no daemon yet" path: the list endpoint must serve an empty but
// well-formed catalog instead of 500ing when a.agent is nil.
func TestHandlePatternList_NilBridgeReturnsEmptyCatalog(t *testing.T) {
	app := newPatternTestApp()

	req := httptest.NewRequest(http.MethodGet, "/api/patterns?status=approved", nil)
	rec := httptest.NewRecorder()
	app.handlePatternList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Patterns []any `json:"patterns"`
		Count    int   `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if got.Count != 0 {
		t.Errorf("count: got %d want 0", got.Count)
	}
	if got.Patterns == nil {
		t.Error("patterns should be a non-nil empty array (frontend indexes without nil checks)")
	}
}

// TestHandlePatternStamp_InvalidBody asserts a malformed JSON body is a 400,
// not a 502/500 — the parse error is reachable without a live bridge.
func TestHandlePatternStamp_InvalidBody(t *testing.T) {
	app := newPatternTestApp()

	req := httptest.NewRequest(http.MethodPost, "/api/patterns/stamp", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	app.handlePatternStamp(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandlePatternStamp_MissingPatternID asserts an empty pattern_id is a 400
// with a descriptive error, before any bridge call.
func TestHandlePatternStamp_MissingPatternID(t *testing.T) {
	app := newPatternTestApp()

	body := `{"pattern_id": "  ", "materials": {"service_name": "foo"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/patterns/stamp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	app.handlePatternStamp(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got["error"], "pattern_id") {
		t.Errorf("error = %q, want it to mention pattern_id", got["error"])
	}
}

// TestHandlePatternStamp_MissingMaterials asserts an empty/absent materials
// object is rejected as a 400 before any bridge call.
func TestHandlePatternStamp_MissingMaterials(t *testing.T) {
	app := newPatternTestApp()

	body := `{"pattern_id": "pattern-go-rest-service"}`
	req := httptest.NewRequest(http.MethodPost, "/api/patterns/stamp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	app.handlePatternStamp(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got["error"], "materials") {
		t.Errorf("error = %q, want it to mention materials", got["error"])
	}
}

// TestHandlePatternStamp_NilBridgeAfterValidation asserts that once the body
// validates, a nil bridge surfaces a 503 rather than panicking.
func TestHandlePatternStamp_NilBridgeAfterValidation(t *testing.T) {
	app := newPatternTestApp()

	body := `{"pattern_id": "pattern-go-rest-service", "materials": {"service_name": "foo"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/patterns/stamp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	app.handlePatternStamp(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandlePatternStamp_EnqueueRequiresAdmin asserts the S1 e2e enqueue path is
// admin-gated: enqueue:true with no admin token configured returns 403 BEFORE
// the (nil) bridge would 503 — i.e. the privileged projection is gated first.
func TestHandlePatternStamp_EnqueueRequiresAdmin(t *testing.T) {
	app := newPatternTestApp() // no admin token configured

	body := `{"pattern_id":"pattern-go-rest-service","materials":{"service_name":"widget"},"enqueue":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/patterns/stamp", strings.NewReader(body))
	rec := httptest.NewRecorder()
	app.handlePatternStamp(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (admin-gated enqueue) (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestBuildStampBacklogItem covers the stamp->BacklogItem projection: id derived
// from the plan id, PlanID carried (canonical), the provenance label, and a
// title that names the pattern + primary material.
func TestBuildStampBacklogItem(t *testing.T) {
	result := &bridge.PatternStampResult{
		PlanID:    "plan-stamp-go-rest-service-widget",
		PatternID: "pattern-go-rest-service",
		Materials: map[string]any{"service_name": "widget"},
	}
	item := buildStampBacklogItem(result)

	if item.ID != "pattern-stamp-go-rest-service-widget" {
		t.Errorf("id = %q, want pattern-stamp-go-rest-service-widget", item.ID)
	}
	if item.PlanID != result.PlanID {
		t.Errorf("plan_id = %q, want %q", item.PlanID, result.PlanID)
	}
	if !strings.Contains(item.Title, "widget") || !strings.Contains(item.Title, "pattern-go-rest-service") {
		t.Errorf("title = %q, want it to mention the pattern + service_name", item.Title)
	}
	if len(item.Labels) != 1 || item.Labels[0] != "mills-pattern-stamp" {
		t.Errorf("labels = %v, want [mills-pattern-stamp]", item.Labels)
	}
	if item.CreatedBy != "pattern-stamp" {
		t.Errorf("created_by = %q, want pattern-stamp", item.CreatedBy)
	}
}

// TestBuildStampBacklogItem_FallbackID: a missing plan id must not yield the
// bare "pattern-stamp-" prefix — it falls back to the pattern id.
func TestBuildStampBacklogItem_FallbackID(t *testing.T) {
	item := buildStampBacklogItem(&bridge.PatternStampResult{
		PlanID:    "",
		PatternID: "pattern-go-rest-service",
	})
	if item.ID != "pattern-stamp-pattern-go-rest-service" {
		t.Errorf("fallback id = %q, want pattern-stamp-pattern-go-rest-service", item.ID)
	}
}

// TestBuildStampBacklogItem_MaterializesSlices: the projection must carry the
// stamp's expanded slices (name + files) into BacklogItem.Slices — the
// operator's scope gate reads them directly and fails closed on a slice-less
// item (live escalation PIPE-pattern-stamp-go-rest-service-widget-1782942288,
// 2026-07-01: "backlog item has no slices; no scope to enforce").
func TestBuildStampBacklogItem_MaterializesSlices(t *testing.T) {
	item := buildStampBacklogItem(&bridge.PatternStampResult{
		PlanID:    "plan-stamp-go-rest-service-widget",
		PatternID: "pattern-go-rest-service",
		Slices: []map[string]any{
			{
				"name":  "scaffold widget service",
				"goal":  "Create the service.",
				"files": []any{"examples/widget/cmd/widget/main.go", "examples/widget/go.mod", 7, ""},
			},
			{"name": "fileless slice"},
		},
	})

	if len(item.Slices) != 2 {
		t.Fatalf("slices = %d, want 2", len(item.Slices))
	}
	got := item.Slices[0]
	if got.Name != "scaffold widget service" {
		t.Errorf("slice name = %q", got.Name)
	}
	want := []string{"examples/widget/cmd/widget/main.go", "examples/widget/go.mod"}
	if len(got.Files) != len(want) || got.Files[0] != want[0] || got.Files[1] != want[1] {
		t.Errorf("slice files = %v, want %v (non-strings and blanks dropped)", got.Files, want)
	}
	if item.Slices[1].Name != "fileless slice" || len(item.Slices[1].Files) != 0 {
		t.Errorf("fileless slice mangled: %+v", item.Slices[1])
	}
}

func TestPatternInstancesNilAgentDegrades(t *testing.T) {
	a := &App{}
	rr := httptest.NewRecorder()
	a.handlePatternInstances(rr, httptest.NewRequest(http.MethodGet, "/api/patterns/p/instances", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	instances, ok := body["instances"].([]any)
	if body["degraded"] != true || !ok || instances == nil {
		t.Fatalf("body=%v", body)
	}
}

func TestPatternInstancesRouteForwardsIDAndCORS(t *testing.T) {
	var gotID any
	caller := &engramAPICaller{callTool: func(name string, args map[string]any) (json.RawMessage, error) {
		if name != "agent_context__agent_pattern_get" {
			t.Fatalf("tool=%s", name)
		}
		gotID = args["pattern_id"]
		return apiMCPResult(`{"pattern":{"instances":[{"stamped_at":"2026-08-08T12:00:00Z","plan_id":"plan-1","target_project":"services/a"}]}}`), nil
	}}
	a := apiTestApp(caller)
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/patterns/pattern-go/instances", nil))
	if rr.Code != http.StatusOK || gotID != "pattern-go" || rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("status=%d id=%v cors=%q body=%s", rr.Code, gotID, rr.Header().Get("Access-Control-Allow-Origin"), rr.Body.String())
	}
	var body struct {
		Instances []bridge.PatternInstanceInfo `json:"instances"`
		Degraded  bool                         `json:"degraded"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Degraded || len(body.Instances) != 1 || body.Instances[0].PlanID != "plan-1" {
		t.Fatalf("body=%+v", body)
	}
}

func TestPatternInstancesBridgeErrorReturnsBadGateway(t *testing.T) {
	caller := &engramAPICaller{callTool: func(string, map[string]any) (json.RawMessage, error) { return nil, errors.New("bridge offline") }}
	a := apiTestApp(caller)
	rr := httptest.NewRecorder()
	a.handlePatternInstances(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

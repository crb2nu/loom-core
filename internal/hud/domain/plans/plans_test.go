package plans

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// --- mock caller / deps ---

type testCaller struct {
	fn func(name string, args map[string]any) (json.RawMessage, error)
}

func (c *testCaller) Call(string, any) (json.RawMessage, error) { return nil, nil }
func (c *testCaller) CallWithTimeout(string, any, time.Duration) (json.RawMessage, error) {
	return nil, nil
}
func (c *testCaller) CallTool(name string, args map[string]any) (json.RawMessage, error) {
	return c.fn(name, args)
}
func (c *testCaller) CallToolWithTimeout(name string, args map[string]any, _ time.Duration) (json.RawMessage, error) {
	return c.fn(name, args)
}
func (c *testCaller) CircuitOpen() bool { return false }
func (c *testCaller) Close() error      { return nil }

type mockDeps struct{ agent *bridge.AgentBridge }

func (d *mockDeps) WriteJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
func (d *mockDeps) WriteError(w http.ResponseWriter, status int, msg string, _ error) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
func (d *mockDeps) Logger() *slog.Logger       { return slog.Default() }
func (d *mockDeps) Agent() *bridge.AgentBridge { return d.agent }

func newDomain(fn func(name string, args map[string]any) (json.RawMessage, error)) *PlansDomain {
	return New(&mockDeps{agent: bridge.NewAgentBridge(&testCaller{fn: fn})})
}

// --- tests ---

func TestPlanList_SuccessAndFilters(t *testing.T) {
	var gotArgs map[string]any
	d := newDomain(func(name string, args map[string]any) (json.RawMessage, error) {
		if !strings.HasSuffix(name, "agent_plan_list") {
			t.Fatalf("unexpected tool %s", name)
		}
		gotArgs = args
		return json.RawMessage(`{"plans":[{"id":"plan-a-1","title":"A","phase":"in_progress"}]}`), nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/plans?project=p/x&phase=in_progress", nil)
	rec := httptest.NewRecorder()
	d.handlePlanList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Available bool `json:"available"`
		Count     int  `json:"count"`
		Plans     []struct {
			ID    string `json:"id"`
			Phase string `json:"phase"`
		} `json:"plans"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp) //nolint:errcheck
	if !resp.Available || resp.Count != 1 || resp.Plans[0].ID != "plan-a-1" {
		t.Fatalf("bad response: %s", rec.Body.String())
	}
	if gotArgs["project"] != "p/x" || gotArgs["phase"] != "in_progress" {
		t.Fatalf("filters not passed through: %v", gotArgs)
	}
	if _, ok := gotArgs["namespace"]; ok {
		t.Fatalf("empty namespace should not be sent: %v", gotArgs)
	}
}

func TestPlanList_DegradesWhenToolUnknown(t *testing.T) {
	d := newDomain(func(string, map[string]any) (json.RawMessage, error) {
		return nil, fmt.Errorf("unknown tool: agent_plan_list")
	})
	req := httptest.NewRequest(http.MethodGet, "/api/plans", nil)
	rec := httptest.NewRecorder()
	d.handlePlanList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("undeployed daemon should degrade to 200, got %d", rec.Code)
	}
	var resp struct {
		Available bool `json:"available"`
		Count     int  `json:"count"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp) //nolint:errcheck
	if resp.Available || resp.Count != 0 {
		t.Fatalf("expected available=false count=0, got %s", rec.Body.String())
	}
}

func TestPlanList_RealErrorIsBadGateway(t *testing.T) {
	d := newDomain(func(string, map[string]any) (json.RawMessage, error) {
		return nil, fmt.Errorf("qdrant connection refused")
	})
	req := httptest.NewRequest(http.MethodGet, "/api/plans", nil)
	rec := httptest.NewRecorder()
	d.handlePlanList(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("genuine error should be 502, got %d", rec.Code)
	}
}

var errIllegal = fmt.Errorf("illegal transition in_progress -> merged")

func TestPlanCreate_Success(t *testing.T) {
	var gotName string
	var gotArgs map[string]any
	d := newDomain(func(name string, args map[string]any) (json.RawMessage, error) {
		gotName, gotArgs = name, args
		return json.RawMessage(`{"ok":true,"plan_id":"plan-new-1","phase":"draft","slice_count":0}`), nil
	})
	req := httptest.NewRequest(http.MethodPost, "/api/plans", strings.NewReader(`{"title":"New plan","project":"p/x"}`))
	rec := httptest.NewRecorder()
	d.handlePlanCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(gotName, "agent_plan_create") || gotArgs["title"] != "New plan" || gotArgs["project"] != "p/x" {
		t.Fatalf("create call wrong: %s %v", gotName, gotArgs)
	}
	var resp struct {
		PlanID string `json:"plan_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp) //nolint:errcheck
	if resp.PlanID != "plan-new-1" {
		t.Fatalf("plan_id = %q", resp.PlanID)
	}
}

func TestPlanCompose_Success(t *testing.T) {
	var gotName string
	var gotArgs map[string]any
	d := newDomain(func(name string, args map[string]any) (json.RawMessage, error) {
		gotName, gotArgs = name, args
		return json.RawMessage(`{"ok":true,"plan_id":"plan-merged-1","phase":"draft","slice_count":2}`), nil
	})
	body := `{
		"title": "Merged: process → model",
		"source_plan_ids": ["plan-mule-1", "plan-flyer-1"],
		"project": "services/loom-core",
		"priority": "p2",
		"slices": [
			{"name": "Ingest & normalize", "goal": "normalize events", "files": ["ingest/events.py"], "source": "mule"},
			{"name": "Derivatives engine", "goal": "rates of change", "source": "flyer"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/plans/compose", strings.NewReader(body))
	rec := httptest.NewRecorder()
	d.handlePlanCompose(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(gotName, "agent_plan_create") {
		t.Fatalf("expected agent_plan_create, got %s", gotName)
	}
	// Phase must be draft; the merged slices + a provenance spec_doc must reach
	// the store.
	if gotArgs["phase"] != "draft" {
		t.Fatalf("compose must create a draft, got phase=%v", gotArgs["phase"])
	}
	if gotArgs["priority"] != "P2" {
		t.Fatalf("priority not normalized/passed: %v", gotArgs["priority"])
	}
	if gotArgs["agent_id"] != "hud:plan-merge-editor" {
		t.Fatalf("compose agent_id = %v", gotArgs["agent_id"])
	}
	rawSlices, ok := gotArgs["slices"].([]map[string]any)
	if !ok || len(rawSlices) != 2 {
		t.Fatalf("expected 2 merged slices, got %#v", gotArgs["slices"])
	}
	if rawSlices[0]["name"] != "Ingest & normalize" || rawSlices[0]["goal"] != "normalize events" {
		t.Fatalf("first slice not carried through: %#v", rawSlices[0])
	}
	spec, _ := gotArgs["spec_doc"].(string)
	if !strings.Contains(spec, "plan-mule-1") || !strings.Contains(spec, "plan-flyer-1") {
		t.Fatalf("spec_doc must name the source ids, got: %q", spec)
	}
	var resp struct {
		Status        string   `json:"status"`
		PlanID        string   `json:"plan_id"`
		SourcePlanIDs []string `json:"source_plan_ids"`
		SliceCount    int      `json:"slice_count"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp) //nolint:errcheck
	if resp.Status != "composed" || resp.PlanID != "plan-merged-1" || resp.SliceCount != 2 {
		t.Fatalf("bad compose response: %s", rec.Body.String())
	}
	if len(resp.SourcePlanIDs) != 2 || resp.SourcePlanIDs[0] != "plan-mule-1" {
		t.Fatalf("source ids not echoed: %v", resp.SourcePlanIDs)
	}
}

func TestPlanCompose_RejectsFewerThanTwoSources(t *testing.T) {
	d := newDomain(func(string, map[string]any) (json.RawMessage, error) {
		t.Fatal("should not call tool with fewer than two sources")
		return nil, nil
	})
	body := `{"title":"T","source_plan_ids":["only-one"],"slices":[{"name":"s1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/plans/compose", strings.NewReader(body))
	rec := httptest.NewRecorder()
	d.handlePlanCompose(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("single source should be 400, got %d", rec.Code)
	}
}

func TestPlanCreate_RequiresTitle(t *testing.T) {
	d := newDomain(func(string, map[string]any) (json.RawMessage, error) {
		t.Fatal("should not call tool without title")
		return nil, nil
	})
	rec := httptest.NewRecorder()
	d.handlePlanCreate(rec, httptest.NewRequest(http.MethodPost, "/api/plans", strings.NewReader(`{"title":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty title should be 400, got %d", rec.Code)
	}
}

func TestPlanAdvance_SuccessAndIllegal(t *testing.T) {
	d := newDomain(func(name string, args map[string]any) (json.RawMessage, error) {
		if !strings.HasSuffix(name, "agent_plan_lifecycle_advance") || args["to_phase"] != "in_review" {
			t.Fatalf("advance call wrong: %s %v", name, args)
		}
		return json.RawMessage(`{"ok":true,"plan_id":"plan-a-1","from_phase":"in_progress","to_phase":"in_review"}`), nil
	})
	req := httptest.NewRequest(http.MethodPost, "/api/plans/plan-a-1/advance", strings.NewReader(`{"to_phase":"in_review"}`))
	req.SetPathValue("id", "plan-a-1")
	rec := httptest.NewRecorder()
	d.handlePlanAdvance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	d2 := newDomain(func(string, map[string]any) (json.RawMessage, error) {
		return nil, errIllegal
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/plans/plan-a-1/advance", strings.NewReader(`{"to_phase":"merged"}`))
	req2.SetPathValue("id", "plan-a-1")
	rec2 := httptest.NewRecorder()
	d2.handlePlanAdvance(rec2, req2)
	if rec2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("illegal transition should be 422, got %d", rec2.Code)
	}
}

func TestPlanGet_SuccessAndMissingID(t *testing.T) {
	d := newDomain(func(name string, args map[string]any) (json.RawMessage, error) {
		if args["plan_id"] != "plan-a-1" {
			t.Fatalf("plan_id not passed: %v", args)
		}
		return json.RawMessage(`{"plan":{"id":"plan-a-1","title":"A","phase":"merged","slices":[{"id":"plan-a-1#1","name":"s1","phase":"merged"}]}}`), nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/plans/plan-a-1", nil)
	req.SetPathValue("id", "plan-a-1")
	rec := httptest.NewRecorder()
	d.handlePlanGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Plan struct {
			ID     string `json:"id"`
			Slices []any  `json:"slices"`
		} `json:"plan"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp) //nolint:errcheck
	if resp.Plan.ID != "plan-a-1" || len(resp.Plan.Slices) != 1 {
		t.Fatalf("bad plan get: %s", rec.Body.String())
	}

	// Missing id → 400.
	rec2 := httptest.NewRecorder()
	d.handlePlanGet(rec2, httptest.NewRequest(http.MethodGet, "/api/plans/", nil))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing id should be 400, got %d", rec2.Code)
	}
}

// TestPlanSetPriority covers the HUD's beam-reorder knob: set (with
// normalization), clear (empty string must still send the priority key so the
// store clears it), and reject junk without calling the tool.
func TestPlanSetPriority(t *testing.T) {
	var gotName string
	var gotArgs map[string]any
	d := newDomain(func(name string, args map[string]any) (json.RawMessage, error) {
		gotName, gotArgs = name, args
		return json.RawMessage(`{"ok":true,"plan_id":"plan-a-1","phase":"in_progress"}`), nil
	})

	// Set: lowercase normalizes to the bucket.
	req := httptest.NewRequest(http.MethodPost, "/api/plans/plan-a-1/priority", strings.NewReader(`{"priority":"p0"}`))
	req.SetPathValue("id", "plan-a-1")
	rec := httptest.NewRecorder()
	d.handlePlanSetPriority(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(gotName, "agent_plan_update") || gotArgs["plan_id"] != "plan-a-1" || gotArgs["priority"] != "P0" {
		t.Fatalf("update call wrong: %s %v", gotName, gotArgs)
	}

	// Clear: empty string must be SENT (present key), not omitted.
	req = httptest.NewRequest(http.MethodPost, "/api/plans/plan-a-1/priority", strings.NewReader(`{"priority":""}`))
	req.SetPathValue("id", "plan-a-1")
	rec = httptest.NewRecorder()
	d.handlePlanSetPriority(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d body=%s", rec.Code, rec.Body.String())
	}
	if pr, ok := gotArgs["priority"]; !ok || pr != "" {
		t.Fatalf("clear must send priority=\"\": %v", gotArgs)
	}

	// Junk → 400 without a tool call.
	d2 := newDomain(func(string, map[string]any) (json.RawMessage, error) {
		t.Fatal("should not call tool with invalid priority")
		return nil, nil
	})
	req = httptest.NewRequest(http.MethodPost, "/api/plans/plan-a-1/priority", strings.NewReader(`{"priority":"urgent"}`))
	req.SetPathValue("id", "plan-a-1")
	rec = httptest.NewRecorder()
	d2.handlePlanSetPriority(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("junk priority should be 400, got %d", rec.Code)
	}
}

// TestPlanCreate_PassesPriority pins priority passthrough on create (the
// "add a plan to the beam already prioritized" path) and rejects junk.
func TestPlanCreate_PassesPriority(t *testing.T) {
	var gotArgs map[string]any
	d := newDomain(func(name string, args map[string]any) (json.RawMessage, error) {
		gotArgs = args
		return json.RawMessage(`{"ok":true,"plan_id":"plan-new-1","phase":"draft","slice_count":0}`), nil
	})
	req := httptest.NewRequest(http.MethodPost, "/api/plans", strings.NewReader(`{"title":"T","priority":"p1"}`))
	rec := httptest.NewRecorder()
	d.handlePlanCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotArgs["priority"] != "P1" {
		t.Fatalf("priority not normalized/passed: %v", gotArgs)
	}

	d2 := newDomain(func(string, map[string]any) (json.RawMessage, error) {
		t.Fatal("should not call tool with invalid priority")
		return nil, nil
	})
	rec2 := httptest.NewRecorder()
	d2.handlePlanCreate(rec2, httptest.NewRequest(http.MethodPost, "/api/plans", strings.NewReader(`{"title":"T","priority":"asap"}`)))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("junk priority on create should be 400, got %d", rec2.Code)
	}
}

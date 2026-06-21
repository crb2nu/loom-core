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

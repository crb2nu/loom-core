package shuttle

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	orch "github.com/crb2nu/loom/internal/hud/shuttle"
)

// mockDeps implements the Deps interface for testing.
type mockDeps struct {
	monitor *orch.ShuttleMonitor
}

func (m *mockDeps) WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (m *mockDeps) WriteError(w http.ResponseWriter, status int, msg string, _ error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (m *mockDeps) ShuttleMonitor() *orch.ShuttleMonitor { return m.monitor }

func newTestDomain() (*ShuttleDomain, *mockDeps) {
	engine := orch.NewEngine(nil)
	mon := orch.NewShuttleMonitor(engine, nil, nil)

	// Pre-populate the monitor snapshot with test data.
	mon.Update(orch.ShuttleSnapshot{
		Capacities: []orch.CapacityInfo{
			{AgentID: "agent-1", ActiveTasks: 2, AvailableSlots: 3},
			{AgentID: "agent-2", ActiveTasks: 0, AvailableSlots: 5},
		},
		Recommendations: []orch.DispatchRecommendation{
			{TaskID: "task-1", RecommendedAgent: "agent-2", Score: 0.85, Reason: "capacity=0.40"},
		},
		PendingTasks: 3,
		ActiveAgents: 2,
	})

	deps := &mockDeps{monitor: mon}
	return New(deps), deps
}

func TestHandleStatus(t *testing.T) {
	d, _ := newTestDomain()

	req := httptest.NewRequest("GET", "/api/shuttle/status", nil)
	w := httptest.NewRecorder()

	d.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var snap orch.ShuttleSnapshot
	if err := json.NewDecoder(w.Body).Decode(&snap); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(snap.Capacities) != 2 {
		t.Errorf("expected 2 capacities, got %d", len(snap.Capacities))
	}
	if snap.PendingTasks != 3 {
		t.Errorf("expected 3 pending tasks, got %d", snap.PendingTasks)
	}
	if len(snap.Recommendations) != 1 {
		t.Errorf("expected 1 recommendation, got %d", len(snap.Recommendations))
	}
}

func TestHandleStatus_MonitorUnavailable(t *testing.T) {
	d := New(&mockDeps{monitor: nil})

	req := httptest.NewRequest("GET", "/api/shuttle/status", nil)
	w := httptest.NewRecorder()

	d.handleStatus(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHandleStatus_EmptyState(t *testing.T) {
	engine := orch.NewEngine(nil)
	mon := orch.NewShuttleMonitor(engine, nil, nil)
	d := New(&mockDeps{monitor: mon})

	req := httptest.NewRequest("GET", "/api/shuttle/status", nil)
	w := httptest.NewRecorder()

	d.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var snap orch.ShuttleSnapshot
	if err := json.NewDecoder(w.Body).Decode(&snap); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if snap.PendingTasks != 0 {
		t.Errorf("expected 0 pending tasks, got %d", snap.PendingTasks)
	}
}

func TestRegisterRoutes(t *testing.T) {
	d, _ := newTestDomain()
	mux := http.NewServeMux()
	mw := func(h http.HandlerFunc) http.HandlerFunc { return h }
	d.RegisterRoutes(mux, mw)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/shuttle/status", nil))
	if w.Code == http.StatusNotFound {
		t.Error("route GET /api/shuttle/status not registered")
	}
}

func TestName(t *testing.T) {
	d, _ := newTestDomain()
	if d.Name() != "shuttle" {
		t.Fatalf("want shuttle, got %s", d.Name())
	}
}

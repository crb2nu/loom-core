package mobile

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/internal/hud/coordination"
	"github.com/crb2nu/loom/internal/hud/monitor"
)

func TestHandleMobileDashboard_SpawnsSummary(t *testing.T) {
	deps := newTestMockDeps()
	deps.monitors = Monitors{
		Fleet:  &monitor.FleetMonitor{},
		Health: &monitor.HealthMonitor{},
	}
	deps.monitors.Fleet.Update(monitor.FleetSnapshot{
		DaemonRunning:  true,
		ServerCount:    10,
		ActiveSessions: 1,
		Spawns: []monitor.SpawnInfo{
			{
				SpawnID:         "spawn-001",
				AgentID:         "spawn-claude-code-001",
				Status:          "running",
				AgentType:       "claude-code",
				Project:         "loom-core",
				TurnCount:       3,
				TotalCostUSD:    0.15,
				InputTokens:     500,
				OutputTokens:    200,
				ToolCallCount:   5,
				FileChangeCount: 2,
			},
			{
				SpawnID:   "spawn-002",
				AgentID:   "spawn-codex-002",
				Status:    "building",
				AgentType: "codex",
				Project:   "flexdeck",
			},
			{
				SpawnID:   "spawn-003",
				AgentID:   "spawn-claude-code-003",
				Status:    "completed",
				AgentType: "claude-code",
				Project:   "loom-core",
			},
		},
	})
	d := New(deps)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mobile/v1/dashboard", d.handleMobileDashboard)

	req := newAuthRequest("GET", "/api/mobile/v1/dashboard")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var env Envelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatal("expected ok=true")
	}

	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data map, got %T", env.Data)
	}

	// Verify spawns section exists.
	spawnsRaw, ok := data["spawns"]
	if !ok {
		t.Fatal("expected spawns key in dashboard response")
	}
	spawns, ok := spawnsRaw.(map[string]any)
	if !ok {
		t.Fatalf("expected spawns to be a map, got %T", spawnsRaw)
	}

	// 2 active (running + building), 3 total.
	if got := spawns["active"]; got != float64(2) {
		t.Errorf("spawns.active = %v, want 2", got)
	}
	if got := spawns["total"]; got != float64(3) {
		t.Errorf("spawns.total = %v, want 3", got)
	}

	// Verify items are included.
	items, ok := spawns["items"].([]any)
	if !ok {
		t.Fatalf("expected spawns.items to be a slice, got %T", spawns["items"])
	}
	if len(items) != 3 {
		t.Errorf("spawns.items length = %d, want 3", len(items))
	}

	// Verify telemetry fields are present in the first spawn item.
	firstItem, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("expected first item to be a map, got %T", items[0])
	}
	if got := firstItem["turn_count"]; got != float64(3) {
		t.Errorf("first item turn_count = %v, want 3", got)
	}
	if got := firstItem["total_cost_usd"]; got != float64(0.15) {
		t.Errorf("first item total_cost_usd = %v, want 0.15", got)
	}
	if got := firstItem["tool_call_count"]; got != float64(5) {
		t.Errorf("first item tool_call_count = %v, want 5", got)
	}
}

func TestHandleMobileDashboard_SpawnsEmpty(t *testing.T) {
	deps := newTestMockDeps()
	deps.monitors = Monitors{
		Fleet:  &monitor.FleetMonitor{},
		Health: &monitor.HealthMonitor{},
	}
	deps.monitors.Fleet.Update(monitor.FleetSnapshot{
		DaemonRunning: true,
	})
	d := New(deps)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mobile/v1/dashboard", d.handleMobileDashboard)

	req := newAuthRequest("GET", "/api/mobile/v1/dashboard")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var env Envelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}

	data := env.Data.(map[string]any)
	spawns := data["spawns"].(map[string]any)

	if got := spawns["active"]; got != float64(0) {
		t.Errorf("spawns.active = %v, want 0", got)
	}
	if got := spawns["total"]; got != float64(0) {
		t.Errorf("spawns.total = %v, want 0", got)
	}
}

func TestHandleMobileDashboard_BlockedSessions(t *testing.T) {
	deps := newTestMockDeps()
	deps.monitors = Monitors{Fleet: &monitor.FleetMonitor{}, Health: &monitor.HealthMonitor{}}
	deps.monitors.Fleet.Update(monitor.FleetSnapshot{DaemonRunning: true})
	deps.blocked = []BlockedSessionInfo{
		{SessionID: "s1", AgentID: "claude-code", Reason: "permission", ToolName: "Bash", WaitedSeconds: 42},
	}
	d := New(deps)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mobile/v1/dashboard", d.handleMobileDashboard)
	req := newAuthRequest("GET", "/api/mobile/v1/dashboard")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var env Envelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data := env.Data.(map[string]any)

	if got := data["blocked_count"]; got != float64(1) {
		t.Errorf("blocked_count = %v, want 1", got)
	}
	blocked, ok := data["blocked"].([]any)
	if !ok || len(blocked) != 1 {
		t.Fatalf("blocked = %v, want one entry", data["blocked"])
	}
	row := blocked[0].(map[string]any)
	if row["session_id"] != "s1" || row["reason"] != "permission" || row["tool_name"] != "Bash" {
		t.Errorf("blocked[0] = %v", row)
	}
	if got := row["waited_seconds"]; got != float64(42) {
		t.Errorf("waited_seconds = %v, want 42", got)
	}
}

// An empty blocked set serializes as [] (not null) with a zero count.
func TestHandleMobileDashboard_BlockedEmpty(t *testing.T) {
	deps := newTestMockDeps()
	deps.monitors = Monitors{Fleet: &monitor.FleetMonitor{}, Health: &monitor.HealthMonitor{}}
	deps.monitors.Fleet.Update(monitor.FleetSnapshot{DaemonRunning: true})
	d := New(deps)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mobile/v1/dashboard", d.handleMobileDashboard)
	req := newAuthRequest("GET", "/api/mobile/v1/dashboard")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var env Envelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data := env.Data.(map[string]any)
	if got := data["blocked_count"]; got != float64(0) {
		t.Errorf("blocked_count = %v, want 0", got)
	}
	if blocked, ok := data["blocked"].([]any); !ok || len(blocked) != 0 {
		t.Errorf("blocked = %v, want []", data["blocked"])
	}
}

// dashboardAttentionLanes serves the dashboard against deps and returns the
// decoded coordination.attention_lanes slice.
func dashboardAttentionLanes(t *testing.T, deps *mockDeps) []map[string]any {
	t.Helper()
	d := New(deps)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/mobile/v1/dashboard", d.handleMobileDashboard)
	req := newAuthRequest("GET", "/api/mobile/v1/dashboard")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var env Envelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	coord, ok := env.Data.(map[string]any)["coordination"].(map[string]any)
	if !ok {
		t.Fatalf("expected coordination map in dashboard response")
	}
	raw, ok := coord["attention_lanes"].([]any)
	if !ok {
		t.Fatalf("expected attention_lanes slice, got %T", coord["attention_lanes"])
	}
	lanes := make([]map[string]any, 0, len(raw))
	for i, entry := range raw {
		lane, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("lane[%d] is %T, want map", i, entry)
		}
		lanes = append(lanes, lane)
	}
	return lanes
}

// TestHandleMobileDashboard_AttentionLaneSeverityOrder pins the merged lane
// ordering contract: coordination and mrwatch lanes interleave by severity
// (critical > warning > info), so a critical MR conflict claims the hero card
// (iOS anchors it on lane[0]) instead of sorting below every info-level
// coordination lane as under the old append-mrwatch-last flow.
func TestHandleMobileDashboard_AttentionLaneSeverityOrder(t *testing.T) {
	deps := newTestMockDeps()
	deps.monitors = Monitors{Fleet: &monitor.FleetMonitor{}, Health: &monitor.HealthMonitor{}}
	deps.monitors.Fleet.Update(monitor.FleetSnapshot{
		DaemonRunning: true,
		Coordination: coordination.Snapshot{
			// -> one info-severity "merge" lane from the coordination side.
			Summary: coordination.Summary{MergeReadyBranches: 1},
		},
	})
	deps.mrwatchAttention = []MergeAttentionItem{
		{Repo: "services/loom-core", IID: 41, Branch: "feat/a", State: "ci-failed", Lane: "merge", Severity: "warning"},
		{Repo: "services/loom-core", IID: 42, Branch: "feat/b", State: "conflict", Lane: "conflict", Severity: "critical"},
	}

	lanes := dashboardAttentionLanes(t, deps)
	if len(lanes) != 3 {
		t.Fatalf("expected 3 lanes, got %d: %v", len(lanes), lanes)
	}

	hero := lanes[0]
	if hero["severity"] != "critical" || hero["type"] != "conflict" || hero["id"] != "mr:services/loom-core!42" {
		t.Errorf("hero lane = severity %v type %v id %v, want the critical mrwatch conflict first", hero["severity"], hero["type"], hero["id"])
	}
	wantSeverities := []string{"critical", "warning", "info"}
	for i, want := range wantSeverities {
		if got := lanes[i]["severity"]; got != want {
			t.Errorf("lane[%d] severity = %v, want %v", i, got, want)
		}
	}
}

// TestHandleMobileDashboard_AttentionLaneGlobalCap pins the single global cap:
// both sources together never exceed 8 lanes (the old per-source caps allowed
// 16), the cap is applied AFTER the severity sort so it sheds the least-urgent
// lanes, and the stable sort keeps coordination lanes ahead of mrwatch lanes
// of equal severity.
func TestHandleMobileDashboard_AttentionLaneGlobalCap(t *testing.T) {
	deps := newTestMockDeps()
	deps.monitors = Monitors{Fleet: &monitor.FleetMonitor{}, Health: &monitor.HealthMonitor{}}
	deps.monitors.Fleet.Update(monitor.FleetSnapshot{
		DaemonRunning: true,
		Coordination: coordination.Snapshot{
			Summary: coordination.Summary{
				MergeReadyBranches: 1, // -> info lane: must be shed by the cap
				ConflictFiles:      2, // -> critical lane: must survive and lead
			},
		},
	})
	for i := 0; i < 10; i++ {
		deps.mrwatchAttention = append(deps.mrwatchAttention, MergeAttentionItem{
			Repo:     "services/loom-core",
			IID:      int64(100 + i),
			Branch:   "feat/x",
			State:    "ci-failed",
			Lane:     "merge",
			Severity: "warning",
		})
	}

	lanes := dashboardAttentionLanes(t, deps)
	if len(lanes) != 8 {
		t.Fatalf("expected the global cap to hold at 8 lanes, got %d", len(lanes))
	}
	if lanes[0]["id"] != "file-conflicts" || lanes[0]["severity"] != "critical" {
		t.Errorf("lane[0] = id %v severity %v, want the coordination file-conflicts critical lane", lanes[0]["id"], lanes[0]["severity"])
	}
	for i, lane := range lanes {
		if lane["severity"] == "info" {
			t.Errorf("lane[%d] is info-severity; the cap should shed info lanes before warnings", i)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/overseer"
)

// noopAgent is a scripted Agent for handler tests.
type noopAgent struct {
	name   string
	result overseer.TickResult
}

func (a *noopAgent) Name() string { return a.name }
func (a *noopAgent) Tick(context.Context) (overseer.TickResult, error) {
	return a.result, nil
}

func withTestOverseers(op *operator) *overseer.Harness {
	h := &overseer.Harness{Agent: &noopAgent{name: "groomer", result: overseer.TickResult{Inspected: 2}}}
	op.overseers = map[string]overseerEntry{"groomer": {
		Harness: h,
		// Mirror main.go's wiring: policy gates for the status view.
		Enabled: func() bool { pol := op.policy.Current(); return pol != nil && pol.GroomerEnabled() },
		DryRun: func() bool {
			pol := op.policy.Current()
			return pol == nil || mills.DryRunOn(pol.Overseers.Groomer.DryRun)
		},
	}}
	return h
}

func TestHandleOverseersStatus_Shape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	withTestOverseers(op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/overseers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Enabled bool `json:"enabled"`
		Agents  []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			DryRun  bool   `json:"dry_run"`
			Paused  bool   `json:"paused"`
		} `json:"agents"`
		RecentActions map[string]json.RawMessage `json:"recent_actions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Agents) != 1 || resp.Agents[0].Name != "groomer" {
		t.Fatalf("agents = %+v", resp.Agents)
	}
	// Fixture policy has no overseers block: everything reads disabled and
	// dry-run (the fail-safe defaults).
	if resp.Enabled || resp.Agents[0].Enabled {
		t.Fatalf("zero-policy overseers reported enabled: %+v", resp)
	}
	if !resp.Agents[0].DryRun {
		t.Fatal("dry_run must default true")
	}
}

// TestHandleOverseersStatus_RegisteredAgentAccessors proves the status view
// reads each registration's own accessors — the regression this guards: a
// hardcoded name switch silently reported enabled=false/dry_run=false for
// any agent it didn't know about.
func TestHandleOverseersStatus_RegisteredAgentAccessors(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	lease := &overseer.Suppression{Reason: "test"}
	op.overseers = map[string]overseerEntry{"doffer": {
		Harness:     &overseer.Harness{Agent: &noopAgent{name: "doffer"}},
		Enabled:     func() bool { return true },
		DryRun:      func() bool { return false },
		Suppression: func() *overseer.Suppression { return lease },
	}}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/overseers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Agents []struct {
			Name        string                `json:"name"`
			Enabled     bool                  `json:"enabled"`
			DryRun      bool                  `json:"dry_run"`
			Suppression *overseer.Suppression `json:"suppression"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Agents) != 1 || resp.Agents[0].Name != "doffer" {
		t.Fatalf("agents = %+v", resp.Agents)
	}
	a := resp.Agents[0]
	if !a.Enabled || a.DryRun {
		t.Fatalf("accessor passthrough broken: enabled=%v dry_run=%v (want true/false)", a.Enabled, a.DryRun)
	}
	if a.Suppression == nil || a.Suppression.Reason != "test" {
		t.Fatalf("suppression passthrough broken: %+v", a.Suppression)
	}
}

// setTestAdminToken installs an admin token for the test and restores the
// prior value on cleanup (the token lives in a process-global atomic, not
// the environment).
func setTestAdminToken(t *testing.T, token string) {
	t.Helper()
	prev, _ := adminToken.Load().(string)
	setAdminToken(token)
	t.Cleanup(func() { setAdminToken(prev) })
}

func TestHandleOverseerPauseResume_AuthAndEffect(t *testing.T) {
	setTestAdminToken(t, "secret")
	op, cleanup := newTestOperator(t)
	defer cleanup()
	h := withTestOverseers(op)
	mux := op.httpMux()

	// No bearer: fail-closed 401.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/mills/overseers/groomer/pause", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pause = %d, want 401", rec.Code)
	}

	// Authorized pause flips the harness.
	req := httptest.NewRequest(http.MethodPost, "/api/mills/overseers/groomer/pause", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause = %d body=%s", rec.Code, rec.Body.String())
	}
	if !h.Paused() {
		t.Fatal("harness not paused")
	}

	// Resume clears it.
	req = httptest.NewRequest(http.MethodPost, "/api/mills/overseers/groomer/resume", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume = %d body=%s", rec.Code, rec.Body.String())
	}
	if h.Paused() {
		t.Fatal("harness still paused")
	}

	// Unknown agent 404s.
	req = httptest.NewRequest(http.MethodPost, "/api/mills/overseers/ghost/pause", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent = %d, want 404", rec.Code)
	}
}

func TestHandleOverseerTick_RunsAgent(t *testing.T) {
	setTestAdminToken(t, "secret")
	op, cleanup := newTestOperator(t)
	defer cleanup()
	withTestOverseers(op)

	req := httptest.NewRequest(http.MethodPost, "/api/mills/overseers/groomer/tick", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tick = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result overseer.TickResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.Inspected != 2 {
		t.Fatalf("result = %+v", resp.Result)
	}
}

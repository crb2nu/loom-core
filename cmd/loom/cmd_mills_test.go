package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOperator returns an httptest.Server emulating the loom-mills-operator
// /api/mills/status surface. Tests configure the body via the `status` arg;
// pass nil to simulate a 500.
func fakeOperator(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mills/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMillsStatus_HumanReadable(t *testing.T) {
	srv := fakeOperator(t, http.StatusOK, map[string]any{
		"db_ok":          true,
		"policy_enabled": true,
		"policy_version": 1,
		"slice":          "1.2-skeleton",
	})

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\noutput=%q", err, out.String())
	}
	for _, want := range []string{"policy:", "(v1)", "store:", "ok", "queue depth:", "—", "operator slice:", "1.2-skeleton"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q\n%s", want, out.String())
		}
	}
}

// TestMillsStatus_HumanReadableFull pins the enriched render against the
// full slice-2.4 payload: build sha, autonomy verdict, health gates, the
// capability matrix rollup, both budget tiers, relative ages and the council
// yield block.
func TestMillsStatus_HumanReadableFull(t *testing.T) {
	prevNow := millsStatusNow
	millsStatusNow = func() time.Time { return time.Date(2026, 9, 1, 22, 10, 0, 0, time.UTC) }
	t.Cleanup(func() { millsStatusNow = prevNow })

	srv := fakeOperator(t, http.StatusOK, map[string]any{
		"build_sha":            "9a61db1d",
		"db_ok":                true,
		"policy_enabled":       true,
		"policy_version":       2,
		"autonomy_ready":       false,
		"autonomy_blockers":    []string{"hud_spawn is red", "council_participants uses fake agents"},
		"queue_depth":          1,
		"active_pipeline_runs": 0,
		"last_council_at":      "2026-09-01T18:00:16.24599927Z",
		"last_merge_at":        "2026-09-01T14:01:17.830746597Z",
		"health_gates":         map[string]any{"allowed": true, "fail_closed": false, "status": "pass"},
		"health_gates_mode":    "observe",
		"capabilities": []map[string]any{
			{"id": "sqlite_store", "status": "green", "mode": "real", "required_for_autonomy": true},
			{"id": "hud_spawn", "status": "red", "mode": "stub", "required_for_autonomy": true},
			{"id": "kpi_writer", "status": "yellow", "mode": "real", "required_for_autonomy": false},
		},
		"budget": map[string]any{
			"pipeline": map[string]any{"spent_usd": 2.6389, "cap_usd": 75, "runs": 11, "runs_cap": 60},
			"council":  map[string]any{"spent_usd": 2.7743, "cap_usd": 50, "runs": 4, "runs_cap": 0},
		},
		"council_yield": map[string]any{
			"runs_since_last_delta":     5,
			"cost_since_last_delta_usd": 3.48,
			"last_delta_at":             "2026-08-30T12:00:00Z",
			"last_delta_run_id":         "COUNCIL-2026-08-30-120000-abcd",
			"sample_size":               6,
		},
		"slice": "2.4-rest-surface",
	})

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\noutput=%q", err, out.String())
	}
	for _, want := range []string{
		"build:            9a61db1d",
		"policy:           on (v2)",
		"autonomy:         BLOCKED: hud_spawn is red; council_participants uses fake agents",
		"health gates:     pass (observe)",
		"capabilities:     1/3 green; hud_spawn=red(stub)*, kpi_writer=yellow",
		"budget (24h):     pipeline $2.64/$75 (11/60 runs) · council $2.77/$50 (4 runs)",
		"queue depth:      1",
		"active pipelines: 0",
		"last council run: 2026-09-01T18:00:16.24599927Z (4h ago)",
		"council yield:    5 runs without a backlog delta ($3.48); last delta 2026-08-30T12:00:00Z (2d ago)",
		"last merge:       2026-09-01T14:01:17.830746597Z (8h ago)",
		"operator slice:   2.4-rest-surface",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q\n%s", want, out.String())
		}
	}
}

func TestRenderMillsStatusHelpers(t *testing.T) {
	now := time.Date(2026, 9, 1, 22, 0, 0, 0, time.UTC)
	ready := true
	if got := renderMillsAutonomy(millsStatus{AutonomyReady: &ready}); got != "ready" {
		t.Errorf("autonomy ready = %q", got)
	}
	if got := renderMillsAutonomy(millsStatus{}); got != "—" {
		t.Errorf("autonomy absent = %q, want —", got)
	}
	blocked := false
	if got := renderMillsAutonomy(millsStatus{AutonomyReady: &blocked}); got != "BLOCKED" {
		t.Errorf("autonomy blocked without reasons = %q", got)
	}

	hg := &millsHealthGates{Allowed: false, FailClosed: true, Status: "block", Reasons: []string{"mills-store unhealthy", "second"}}
	if got := renderMillsHealthGates(millsStatus{HealthGates: hg, HealthGatesMode: "enforce"}); got != "block (enforce) — mills-store unhealthy" {
		t.Errorf("health gates block = %q", got)
	}
	if got := renderMillsHealthGates(millsStatus{}); got != "—" {
		t.Errorf("health gates absent = %q", got)
	}

	if got := renderMillsCapabilities(nil); got != "—" {
		t.Errorf("capabilities absent = %q", got)
	}
	if got := renderMillsCapabilities([]millsCapability{{ID: "a", Status: "green"}, {ID: "b", Status: "green"}}); got != "2/2 green" {
		t.Errorf("capabilities all green = %q", got)
	}

	if got := renderMillsBudget(nil); got != "—" {
		t.Errorf("budget absent = %q", got)
	}
	if got := renderMillsBudget(map[string]millsBudgetTier{"spin": {SpentUSD: 120, CapUSD: 0, Runs: 3}}); got != "spin $120 (3 runs)" {
		t.Errorf("budget unknown tier = %q", got)
	}

	cases := []struct {
		name string
		y    *millsCouncilYield
		want string
	}{
		{"absent", nil, "—"},
		{"no runs", &millsCouncilYield{}, "no finished council runs"},
		{"yielding", &millsCouncilYield{SampleSize: 3}, "last run produced backlog deltas"},
		{"one run", &millsCouncilYield{RunsSinceLastDelta: 1, CostSinceLastDeltaUSD: 0.7, LastDeltaAt: "2026-09-01T21:30:00Z", SampleSize: 2}, "1 run without a backlog delta ($0.70); last delta 2026-09-01T21:30:00Z (30m ago)"},
		{"sample exhausted", &millsCouncilYield{RunsSinceLastDelta: 50, CostSinceLastDeltaUSD: 35, SampleSize: 50}, "50 runs without a backlog delta ($35); none in the last 50 examined"},
	}
	for _, c := range cases {
		if got := renderMillsCouncilYield(c.y, now); got != c.want {
			t.Errorf("%s: council yield = %q, want %q", c.name, got, c.want)
		}
	}

	if got := renderMillsTimestamp("not-a-time", now); got != "not-a-time" {
		t.Errorf("unparseable timestamp = %q, want passthrough", got)
	}
	if got := renderMillsTimestamp("2026-09-01T22:00:30Z", now); got != "2026-09-01T22:00:30Z (just now)" {
		t.Errorf("future/skewed timestamp = %q", got)
	}
	if got := fmtMillsAge(72 * time.Hour); got != "3d ago" {
		t.Errorf("age 72h = %q", got)
	}
}

func TestMillsStatus_RawJSON(t *testing.T) {
	srv := fakeOperator(t, http.StatusOK, map[string]any{
		"db_ok":          true,
		"policy_enabled": false,
		"policy_version": 1,
	})

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL, "--json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), `"db_ok":true`) {
		t.Errorf("expected raw JSON, got: %s", out.String())
	}
}

func TestMillsStatus_PolicyOff(t *testing.T) {
	srv := fakeOperator(t, http.StatusOK, map[string]any{
		"db_ok":          true,
		"policy_enabled": false,
		"policy_version": 2,
	})

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "policy:           off (v2)") {
		t.Errorf("expected policy off, got: %s", out.String())
	}
}

func TestMillsStatus_DBFailure(t *testing.T) {
	srv := fakeOperator(t, http.StatusOK, map[string]any{
		"db_ok":          false,
		"policy_enabled": true,
		"policy_version": 1,
	})

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "store:            FAIL") {
		t.Errorf("expected store=FAIL, got: %s", out.String())
	}
}

func TestMillsStatus_OperatorError(t *testing.T) {
	srv := fakeOperator(t, http.StatusInternalServerError, nil)

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "operator returned 500") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMillsStatus_ConnectionRefused(t *testing.T) {
	cmd := newMillsCmd()
	// 127.0.0.1:1 is a reserved port; nothing listens there.
	cmd.SetArgs([]string{"status", "--operator-url", "http://127.0.0.1:1", "--timeout", "200ms"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected connection error")
	}
	if !strings.Contains(err.Error(), "kubectl port-forward") {
		t.Errorf("expected port-forward hint, got: %v", err)
	}
}

func TestMillsStatus_HonorsEnv(t *testing.T) {
	srv := fakeOperator(t, http.StatusOK, map[string]any{
		"db_ok":          true,
		"policy_enabled": true,
		"policy_version": 1,
	})
	t.Setenv("LOOM_MILLS_OPERATOR_URL", srv.URL)

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "Loom Mills @ "+srv.URL) {
		t.Errorf("env URL not applied; output: %s", out.String())
	}
}

func TestMillsStatus_AuthHeaderForwarded(t *testing.T) {
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mills/status", func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"db_ok":true,"policy_enabled":true,"policy_version":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("LOOM_MILLS_TOKEN", "tok-abc")
	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL})
	cmd.SetContext(context.Background())
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if seen != "Bearer tok-abc" {
		t.Errorf("Authorization header: got %q want Bearer tok-abc", seen)
	}
}

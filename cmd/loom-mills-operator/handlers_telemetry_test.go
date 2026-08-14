package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestHandleTelemetryStages_JSONShape drives the operator mux end-to-end and
// pins the exact JSON contract (snake_case keys) the HUD telemetry panel builds
// against, plus the aggregated values from a seeded run.
func TestHandleTelemetryStages_JSONShape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: "MILLS-TEL", Title: "tel", State: store.BacklogRunning, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{
		ID: "PIPE-TEL", BacklogID: "MILLS-TEL", Template: "t", State: store.PipelineEscalated,
		Attempts: 1, StartedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	putStage := func(stage string, attempt int, dur time.Duration, outcome store.StageOutcome, cost float64, logTail string) {
		t.Helper()
		start := now.Add(-2 * time.Hour).Add(time.Duration(attempt) * time.Minute)
		sr := &store.StageResult{
			PipelineRunID: "PIPE-TEL", Stage: stage, Attempt: attempt,
			StartedAt: start, CostUSD: cost, LogTail: logTail,
		}
		if dur > 0 {
			end := start.Add(dur)
			sr.EndedAt = &end
		}
		oc := outcome
		sr.Outcome = &oc
		if err := op.store.Pipeline.PutStage(ctx, sr); err != nil {
			t.Fatalf("seed stage %s a%d: %v", stage, attempt, err)
		}
	}
	putStage("implement", 1, 100*time.Second, store.StageOutcomeSuccess, 1.50, "wrote files")
	putStage("implement", 2, 200*time.Second, store.StageOutcomeError, 2.50, "hud spawn: POST failed")
	if err := op.store.Pipeline.PutGate(ctx, &store.GateOutcome{
		PipelineRunID: "PIPE-TEL", AfterStage: "pr_self_review", GateName: "pr_self_review",
		Outcome: store.GateOutcomeFail, JudgedBy: "flexinfer:unparseable", EvaluatedAt: now.Add(-90 * time.Minute),
	}); err != nil {
		t.Fatalf("seed gate: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/telemetry/stages?window=7d", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}

	// Decode into a loosely-typed map first to assert the exact snake_case keys
	// the frontend fixture depends on.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	for _, key := range []string{"window_seconds", "generated_at", "runs", "stages", "gates", "escalation_funnel", "failure_classes", "model_economics"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing top-level key %q; body=%s", key, rec.Body.String())
		}
	}

	var resp struct {
		WindowSeconds int `json:"window_seconds"`
		Runs          struct {
			Total            int     `json:"total"`
			Done             int     `json:"done"`
			Escalated        int     `json:"escalated"`
			RetryBurnCostUSD float64 `json:"retry_burn_cost_usd"`
			RetryBurnSeconds int64   `json:"retry_burn_seconds"`
		} `json:"runs"`
		Stages []struct {
			Stage         string  `json:"stage"`
			Attempts      int     `json:"attempts"`
			Errors        int     `json:"errors"`
			ErrorRate     float64 `json:"error_rate"`
			P50Seconds    int64   `json:"p50_seconds"`
			P90Seconds    int64   `json:"p90_seconds"`
			MaxSeconds    int64   `json:"max_seconds"`
			TotalSeconds  int64   `json:"total_seconds"`
			CostUSD       float64 `json:"cost_usd"`
			RetryAttempts int     `json:"retry_attempts"`
			RetryCostUSD  float64 `json:"retry_cost_usd"`
		} `json:"stages"`
		Gates []struct {
			Gate        string `json:"gate"`
			Evaluations int    `json:"evaluations"`
			Fails       int    `json:"fails"`
			Unparseable int    `json:"unparseable"`
		} `json:"gates"`
		EscalationFunnel []struct {
			LastStage string `json:"last_stage"`
			Outcome   string `json:"outcome"`
			Count     int    `json:"count"`
		} `json:"escalation_funnel"`
		FailureClasses []struct {
			Stage string `json:"stage"`
			Class string `json:"class"`
			Count int    `json:"count"`
		} `json:"failure_classes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("typed decode: %v", err)
	}

	if resp.WindowSeconds != 7*86400 {
		t.Errorf("window_seconds = %d, want %d", resp.WindowSeconds, 7*86400)
	}
	if resp.Runs.Total != 1 || resp.Runs.Escalated != 1 || resp.Runs.Done != 0 {
		t.Errorf("runs = %+v, want total=1 escalated=1 done=0", resp.Runs)
	}
	if resp.Runs.RetryBurnCostUSD != 2.50 {
		t.Errorf("retry_burn_cost_usd = %v, want 2.50", resp.Runs.RetryBurnCostUSD)
	}
	if resp.Runs.RetryBurnSeconds != 200 {
		t.Errorf("retry_burn_seconds = %d, want 200", resp.Runs.RetryBurnSeconds)
	}

	if len(resp.Stages) != 1 {
		t.Fatalf("stages len = %d, want 1: %+v", len(resp.Stages), resp.Stages)
	}
	impl := resp.Stages[0]
	if impl.Stage != "implement" || impl.Attempts != 2 || impl.Errors != 1 {
		t.Errorf("stage = %+v, want implement/2/1", impl)
	}
	if impl.ErrorRate != 0.5 {
		t.Errorf("error_rate = %v, want 0.5", impl.ErrorRate)
	}
	if impl.P50Seconds != 100 || impl.MaxSeconds != 200 || impl.TotalSeconds != 300 {
		t.Errorf("durations = p50 %d max %d total %d, want 100/200/300", impl.P50Seconds, impl.MaxSeconds, impl.TotalSeconds)
	}
	if impl.CostUSD != 4.00 || impl.RetryAttempts != 1 || impl.RetryCostUSD != 2.50 {
		t.Errorf("cost/retry = %v/%d/%v, want 4.00/1/2.50", impl.CostUSD, impl.RetryAttempts, impl.RetryCostUSD)
	}

	if len(resp.Gates) != 1 || resp.Gates[0].Unparseable != 1 || resp.Gates[0].Fails != 1 {
		t.Errorf("gates = %+v, want one gate with 1 fail / 1 unparseable", resp.Gates)
	}
	if len(resp.EscalationFunnel) != 1 {
		t.Fatalf("funnel len = %d, want 1: %+v", len(resp.EscalationFunnel), resp.EscalationFunnel)
	}
	if resp.EscalationFunnel[0].LastStage != "implement" || resp.EscalationFunnel[0].Outcome != "error" {
		t.Errorf("funnel[0] = %+v, want implement/error", resp.EscalationFunnel[0])
	}
	if len(resp.FailureClasses) != 1 || resp.FailureClasses[0].Class != "spawn_infra" {
		t.Errorf("failure_classes = %+v, want one spawn_infra row", resp.FailureClasses)
	}
}

func TestHandleTelemetryStages_BadWindowIs400(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/telemetry/stages?window=42h", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad window, got %d", rec.Code)
	}
}

// TestHandleTelemetryStages_EmptyArraysNotNull guards the never-null contract on
// the wire: an empty store must encode stages/gates/escalation_funnel/
// failure_classes as [], never null.
func TestHandleTelemetryStages_EmptyArraysNotNull(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/telemetry/stages?window=1d", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"stages", "gates", "escalation_funnel", "failure_classes", "model_economics"} {
		arr, ok := resp[key].([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("%s must be an empty JSON array, got: %v", key, resp[key])
		}
	}
}

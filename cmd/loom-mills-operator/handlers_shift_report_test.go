package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/shiftreport"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestShiftReportHandlerWindowAndDeterministicShape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	now := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	op.shiftNow = func() time.Time { return now }
	graded := now.Add(-30 * time.Minute)
	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{ID: "BL-SHIFT", Title: "Shift ledger", State: store.BacklogMerged, Priority: store.P2, CreatedBy: "test", UpdatedAt: graded}); err != nil {
		t.Fatal(err)
	}
	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{ID: "BL-SHIFT-SPARK", Title: "Broken loom", State: store.BacklogEscalated, Priority: store.P2, CreatedBy: "test", UpdatedAt: graded}); err != nil {
		t.Fatal(err)
	}
	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{ID: "BL-SHIFT-HELD", Title: "Held thread", State: store.BacklogQueued, Priority: store.P2, CreatedBy: "test", UpdatedAt: graded}); err != nil {
		t.Fatal(err)
	}
	ended := now.Add(-time.Hour)
	if err := op.store.Pipeline.PutRun(context.Background(), &store.PipelineRun{ID: "RUN-SHIFT", BacklogID: "BL-SHIFT", Template: "implement", State: store.PipelineDone, Attempts: 1, StartedAt: now.Add(-2 * time.Hour), EndedAt: &ended, CostUSD: 2}); err != nil {
		t.Fatal(err)
	}
	sparked := now.Add(-45 * time.Minute)
	if err := op.store.Pipeline.PutRun(context.Background(), &store.PipelineRun{ID: "RUN-SHIFT-SPARK", BacklogID: "BL-SHIFT-SPARK", Template: "implement", State: store.PipelineEscalated, Attempts: 2, StartedAt: now.Add(-2 * time.Hour), EndedAt: &sparked, CostUSD: 1, FailureSignature: "sig-h"}); err != nil {
		t.Fatal(err)
	}
	// Paused = held thread: terminal for the read model, but not cloth — it
	// must not surface as a bolt, a spark, or a taste denominator.
	held := now.Add(-20 * time.Minute)
	if err := op.store.Pipeline.PutRun(context.Background(), &store.PipelineRun{ID: "RUN-SHIFT-HELD", BacklogID: "BL-SHIFT-HELD", Template: "implement", State: store.PipelinePaused, Attempts: 6, StartedAt: now.Add(-2 * time.Hour), EndedAt: &held, CostUSD: 50}); err != nil {
		t.Fatal(err)
	}
	if _, err := op.store.Backlog.GradeRun(context.Background(), "RUN-SHIFT", "keep", "", "test", graded); err != nil {
		t.Fatal(err)
	}
	for _, snap := range []*store.KPISnapshot{{SnapshotAt: now.Add(-25 * time.Hour), WindowSeconds: 86400, Metrics: map[string]any{"escalation_rate": .2, "merged_runs": 1.0}}, {SnapshotAt: now.Add(-time.Minute), WindowSeconds: 86400, Metrics: map[string]any{"escalation_rate": .3, "merged_runs": 2.0, "cost_per_merged_pipeline_usd": 6.0, "scope_max_queue_age_seconds": 21601.0, "scope_starvation_reservations": 1.0}}} {
		if err := op.store.KPI.RecordSnapshot(context.Background(), snap); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/shift-report?window=24h", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got shiftreport.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.GeneratedAt != now || got.WindowSeconds != 86400 || len(got.Bolts) != 1 || len(got.Sparks) != 1 {
		t.Fatalf("shape=%+v", got)
	}
	for _, line := range got.NarrativeLines {
		if strings.Contains(line, "BL-SHIFT-HELD") {
			t.Fatalf("paused run leaked into narrative: %q", line)
		}
	}
	if got.KPIDelta.EscalationRate.Now != .3 || got.KPIDelta.EscalationRate.Prev != .2 {
		t.Fatalf("delta=%+v", got.KPIDelta)
	}
	if !got.ThroughputGuardrail.Breached || len(got.ThroughputGuardrail.Reasons) != 4 {
		t.Fatalf("guardrail=%+v", got.ThroughputGuardrail)
	}
	if got.Taste.GradedThisShift != 1 || got.Taste.BoltsThisShift != 1 || got.Taste.LastGradeAt == nil {
		t.Fatalf("taste=%+v", got.Taste)
	}
	if !strings.Contains(got.Markdown, "sig-h") {
		t.Fatalf("spark signature missing from markdown:\n%s", got.Markdown)
	}
	first := rec.Body.String()
	rec = httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/shift-report?window=24h", nil))
	if rec.Body.String() != first {
		t.Fatal("identical store state produced different response")
	}
	// The handler resolves thresholds per request, so a hot-reloaded policy
	// changes the advisory verdict without rebuilding the operator.
	op.policy = newAgentPolicyManager(t, `version: 2
budgets:
  throughput_guardrail:
    max_escalation_rate: 0.5
    max_cost_per_merged_pipeline_usd: 10
    max_scope_queue_age_seconds: 30000
    max_starved_queues: 2
`)
	rec = httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/shift-report?window=24h", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ThroughputGuardrail.Breached || len(got.ThroughputGuardrail.Reasons) != 0 {
		t.Fatalf("reconfigured guardrail=%+v", got.ThroughputGuardrail)
	}
}

func TestShiftReportHandlerDeploymentStatesAndMetrics(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	t.Cleanup(func() { mills.FinishingBoltsPending.Set(0); mills.FinishingBoltsUnknown.Set(0) })
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	op.shiftNow = func() time.Time { return now }
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	git("commit", "--allow-empty", "-m", "live")
	liveSHA := git("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "build")
	buildSHA := git("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "pending")
	pendingSHA := git("rev-parse", "HEAD")
	op.withRepoRoot(repo)
	oldVersion := version
	version = buildSHA
	t.Cleanup(func() { version = oldVersion })

	for i, row := range []struct{ id, sha string }{{"BL-LIVE", liveSHA}, {"BL-PENDING", pendingSHA}} {
		if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{ID: row.id, Title: row.id, State: store.BacklogMerged, Priority: store.P2, CreatedBy: "test", UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		ended := now.Add(-time.Duration(i+1) * time.Hour)
		runID := "RUN-" + row.id
		if err := op.store.Pipeline.PutRun(context.Background(), &store.PipelineRun{ID: runID, BacklogID: row.id, Template: "implement", State: store.PipelineDone, Attempts: 1, StartedAt: ended.Add(-time.Hour), EndedAt: &ended}); err != nil {
			t.Fatal(err)
		}
		outcome := store.StageOutcomeSuccess
		if err := op.store.Pipeline.PutStage(context.Background(), &store.StageResult{PipelineRunID: runID, Stage: "mr", Attempt: 1, StartedAt: ended, EndedAt: &ended, Outcome: &outcome, Artifacts: map[string]any{"merged_sha": row.sha, "files_changed": []any{"pkg/mills/example.go"}}}); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/shift-report", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var report shiftreport.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, card := range report.Bolts {
		if card.Deploy != nil {
			states[card.BacklogID] = card.Deploy.State
		}
	}
	if states["BL-LIVE"] != "live" || states["BL-PENDING"] != "pending" {
		t.Fatalf("states = %#v", states)
	}
	if got := testutil.ToFloat64(mills.FinishingBoltsPending); got != 1 {
		t.Fatalf("pending gauge = %v", got)
	}
	if got := testutil.ToFloat64(mills.FinishingBoltsUnknown); got != 0 {
		t.Fatalf("unknown gauge = %v", got)
	}
	if filepath.Clean(op.repoRoot) != filepath.Clean(repo) {
		t.Fatalf("repo root = %q", op.repoRoot)
	}
}

func TestShiftReportHandlerRejectsInvalidWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	for _, q := range []string{"wat", "0s", "-1h"} {
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/shift-report?window="+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("window %q status=%d", q, rec.Code)
		}
	}
}

func TestShiftReportHandlerStoreError(t *testing.T) {
	op, cleanup := newTestOperator(t)
	cleanup()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/shift-report", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestThroughputSignalsMissingZeroAndCanonicalKeys(t *testing.T) {
	if got := throughputSignals(nil); got != (mills.ThroughputSignals{}) {
		t.Fatalf("missing snapshot = %+v", got)
	}
	snapshot := &store.KPISnapshot{Metrics: map[string]any{
		"escalation_rate":               0.0,
		"cost_per_merged_pipeline_usd":  0.0,
		"scope_max_queue_age_seconds":   0.0,
		"scope_starvation_reservations": 0.0,
		"cost_per_merged_change_usd":    999.0,
	}}
	got := throughputSignals(snapshot)
	if !got.HasEscalationRate || !got.HasCostPerMergedPipelineUSD || !got.HasScopeMaxQueueAgeSeconds || !got.HasStarvedQueues {
		t.Fatalf("reported zeros lost presence: %+v", got)
	}
	if verdict := mills.EvaluateThroughputGuardrail(got, mills.ThroughputGuardrailThresholds{}); verdict.Breached {
		t.Fatalf("zeros or unrelated cost alias caused breach: %+v", verdict)
	}
	delete(snapshot.Metrics, "cost_per_merged_pipeline_usd")
	if got := throughputSignals(snapshot); got.HasCostPerMergedPipelineUSD {
		t.Fatalf("used noncanonical cost alias: %+v", got)
	}
}

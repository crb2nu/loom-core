package overseer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type soakTelemetryStub struct{ persistErr error }

func (s soakTelemetryStub) RecordOverseerSoakDecision(context.Context, time.Time, bool, bool) error {
	return s.persistErr
}

func (soakTelemetryStub) OverseerSoakTelemetry(context.Context, time.Time) ([]store.OverseerSoakDailyCounters, error) {
	return nil, nil
}

type dryRunDecisionRecorderStub struct {
	calls    int
	wouldAct bool
	diverged bool
}

func (r *dryRunDecisionRecorderStub) RecordOverseerDryRunDecision(_ context.Context, wouldAct, diverged bool) {
	r.calls++
	r.wouldAct = wouldAct
	r.diverged = diverged
}

func TestRecordDryRunDecisionEmitsOnceAfterPersistence(t *testing.T) {
	recorder := &dryRunDecisionRecorderStub{}
	restore := telemetry.SetOverseerDryRunDecisionRecorderForTest(recorder)
	t.Cleanup(restore)

	if err := RecordDryRunDecision(context.Background(), soakTelemetryStub{}, time.Now(), true, true); err != nil {
		t.Fatalf("record dry-run decision: %v", err)
	}
	if recorder.calls != 1 || !recorder.wouldAct || !recorder.diverged {
		t.Fatalf("metric calls = %d (%v, %v), want one (true, true)", recorder.calls, recorder.wouldAct, recorder.diverged)
	}
}

func TestRecordDryRunDecisionDoesNotEmitWhenPersistenceFails(t *testing.T) {
	recorder := &dryRunDecisionRecorderStub{}
	restore := telemetry.SetOverseerDryRunDecisionRecorderForTest(recorder)
	t.Cleanup(restore)

	wantErr := errors.New("persist failed")
	err := RecordDryRunDecision(context.Background(), soakTelemetryStub{persistErr: wantErr}, time.Now(), false, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if recorder.calls != 0 {
		t.Fatalf("metric calls = %d, want zero", recorder.calls)
	}
}

func TestEvaluateS2Soak(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	passing := func() *guard.PromotionReport {
		return &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: 3, TotalDryRun: 3}
	}
	tests := []struct {
		name        string
		report      *guard.PromotionReport
		divergences int
		promotable  bool
		wantReason  string
	}{
		{name: "empty", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, ZeroEvidence: true}, wantReason: "promotion evidence is empty"},
		{name: "sub seven day", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-6 * 24 * time.Hour), WindowEnd: now, TotalActions: 1, TotalDryRun: 1}, wantReason: "closed soak window"},
		{name: "passing", report: passing(), promotable: true},
		{name: "divergent", report: passing(), divergences: 1, wantReason: "divergence threshold exceeded"},
		{name: "would have acted", report: passing(), promotable: true},
		{name: "missing evidence", wantReason: "missing or unreadable"},
		{name: "wrong actor prefix", report: &guard.PromotionReport{ActorPrefix: "overseer", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: 1, TotalDryRun: 1}, wantReason: "actor_prefix"},
		{name: "inconsistent totals", report: &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-S2SoakMinimumDuration), WindowEnd: now, TotalActions: 2, TotalDryRun: 1}, wantReason: "promotion evidence is inconsistent"},
		{name: "negative reviewed divergences", report: passing(), divergences: -1, wantReason: "promotion evidence is inconsistent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateS2Soak(tc.report, tc.divergences)
			if got.Promotable != tc.promotable || got.FailClosed == tc.promotable {
				t.Fatalf("verdict = promotable %v fail_closed %v, want promotable %v: %v", got.Promotable, got.FailClosed, tc.promotable, got.FailureReasons)
			}
			if tc.wantReason != "" && !containsReason(got.FailureReasons, tc.wantReason) {
				t.Fatalf("failure_reasons = %v, want reason containing %q", got.FailureReasons, tc.wantReason)
			}
			if tc.name == "passing" && got.ElapsedDays != 7 {
				t.Fatalf("elapsed_days = %d, want exact threshold of 7", got.ElapsedDays)
			}
			if tc.name == "would have acted" && got.WouldHaveActed != 3 {
				t.Fatalf("would_have_acted = %d, want 3", got.WouldHaveActed)
			}
		})
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}

func TestEvaluateS2SoakExecutedActionIsDivergence(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	report := &guard.PromotionReport{ActorPrefix: "overseer.", WindowStart: now.Add(-7 * 24 * time.Hour), WindowEnd: now, TotalActions: 2, TotalDryRun: 1, TotalExecuted: 1}
	got := EvaluateS2Soak(report, 0)
	if got.Promotable || got.Divergences != 1 {
		t.Fatalf("verdict = %+v, want one divergence and fail-closed", got)
	}
}

func TestSoakMetricsJSONNames(t *testing.T) {
	b, err := json.Marshal(SoakMetrics{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{S2SoakElapsedDaysMetric, S2SoakDryRunDecisionsMetric, S2SoakWouldHaveActedMetric, S2SoakDivergencesMetric} {
		if !strings.Contains(string(b), `"`+name+`"`) {
			t.Fatalf("JSON %s missing metric %q", b, name)
		}
	}
}

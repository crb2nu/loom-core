package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestReportHandlersDoNotReadEvents(t *testing.T) {
	files := []string{"handlers_judge_calibration.go", "handlers_promotion.go", "handlers_config_outcomes.go", "handlers_overseers.go", "handlers_signature_candidates.go", "handlers_regression_attribution.go"}
	for _, name := range files {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, forbidden := range []string{".Events.", "BuildJudgeCalibrationReport", "BuildPromotionReport", "BuildConfigOutcomeReport", "ListByActorSince", "ListSince"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains request-time event read %q", name, forbidden)
			}
		}
	}
}

func TestWriteReportRollupExactHitDoesNotRebuild(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	want := json.RawMessage(`{"cached":true}`)
	at := time.Now().UTC().Add(-time.Minute)
	if err := op.store.Reports.Put(context.Background(), &store.ReportRollupSnapshot{ReportName: "promotion", WindowSeconds: 24 * 3600, ReportKey: "council.", SnapshotAt: at, Payload: want}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	op.writeReportRollup(rec, httptest.NewRequest(http.MethodGet, "/", nil), "promotion", 24*time.Hour, "council.")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != string(want) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Loom-Report-Snapshot-At") != at.Format(time.RFC3339Nano) {
		t.Fatalf("snapshot header=%q", rec.Header().Get("X-Loom-Report-Snapshot-At"))
	}
}

func TestWriteReportRollupMissBuildsPersistsAndServes(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedPromotionEvents(t, op)
	rec := httptest.NewRecorder()
	op.writeReportRollup(rec, httptest.NewRequest(http.MethodGet, "/", nil), "promotion", 24*time.Hour, "council.")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Loom-Report-Snapshot-At") == "" {
		t.Fatalf("status=%d header=%q body=%s", rec.Code, rec.Header().Get("X-Loom-Report-Snapshot-At"), rec.Body.String())
	}
	if _, err := op.store.Reports.Latest(context.Background(), "promotion", 24*3600, "council."); err != nil {
		t.Fatalf("persisted snapshot: %v", err)
	}
}

func TestBuildReportRollupTimeoutLeavesSnapshotUnavailable(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	if _, err := op.buildReportRollup(context.Background(), "promotion", 24*time.Hour, "council.", time.Nanosecond); err == nil {
		t.Fatal("expected timeout error")
	}
	if _, err := op.store.Reports.Latest(context.Background(), "promotion", 24*3600, "council."); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("latest error=%v want not found", err)
	}
}

func TestReportRollupWriterWiresAllSurfaces(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	got := map[string]int{}
	for _, spec := range newReportRollupWriter(op).Specs {
		got[spec.Name] = int(spec.Window.Seconds())
	}
	want := map[string]int{"judge_calibration": int(judgeCalibrationDefaultWindow.Seconds()), "promotion": int(promotionReportDefaultWindow.Seconds()), "config_outcomes": int(configOutcomesDefaultWindow.Seconds()), "overseers": int(overseerRecentActionsWindow.Seconds()), "signature_candidates": int(signatureCandidatesDefaultWindow.Seconds()), "regressions": int(regressionsDefaultWindow.Seconds())}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("specs=%v want=%v", got, want)
	}
}

func TestOverseerRollupLabelsSoakCancellation(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now().UTC()
	_, err := op.buildOverseersRollup(ctx, now.Add(-24*time.Hour), now)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "overseer soak telemetry") {
		t.Fatalf("soak cancellation must identify the exhausted phase: %v", err)
	}
}

func TestOverseerRollupRetainsFailClosedEvidence(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	check := func(promotable bool) {
		t.Helper()
		result, err := op.buildOverseersRollup(ctx, now.Add(-24*time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		got := result.(overseersStatusResponse).Soak
		if got == nil || got.Promotable != promotable || got.FailClosed == promotable {
			t.Fatalf("promotable=%v, got %+v", promotable, got)
		}
	}
	check(false) // Missing days must not count as zero-disagreement evidence.
	for day := 1; day <= 7; day++ {
		if err := op.store.RecordOverseerSoakDecision(ctx, now.AddDate(0, 0, -day), true, false); err != nil {
			t.Fatal(err)
		}
	}
	check(true)
	if err := op.store.Events.Append(ctx, &store.Event{
		Actor: "overseer", Kind: "overseer.soak.daily", SubjectKind: "utc_day",
		SubjectID: "2026-09-12", Payload: map[string]any{"decisions": 0},
	}); err != nil {
		t.Fatal(err)
	}
	check(false) // Malformed appended evidence must still close the gate.
}

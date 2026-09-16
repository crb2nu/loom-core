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

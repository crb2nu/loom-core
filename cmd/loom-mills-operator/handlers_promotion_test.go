package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedPromotionEvents writes one dry-run and one committed groomer action
// plus a council action, so actor-prefix selection is observable.
func seedPromotionEvents(t *testing.T, op *operator) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, e := range []*store.Event{
		{OccurredAt: now.Add(-time.Hour), Actor: "overseer.groomer", Kind: "overseer.groomer.retire.dryrun", SubjectKind: "backlog_item", SubjectID: "A"},
		{OccurredAt: now.Add(-2 * time.Hour), Actor: "overseer.groomer", Kind: "overseer.groomer.retire", SubjectKind: "backlog_item", SubjectID: "B"},
		{OccurredAt: now.Add(-3 * time.Hour), Actor: "council.mutator", Kind: "council.mutator.apply", SubjectKind: "policy", SubjectID: "p1"},
	} {
		if err := op.store.Events.Append(ctx, e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
}

func getPromotionReport(t *testing.T, op *operator, query string) (*httptest.ResponseRecorder, guard.PromotionReport) {
	t.Helper()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/promotion-report"+query, nil))
	var report guard.PromotionReport
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, report
}

func TestHandlePromotionReport_Defaults(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedPromotionEvents(t, op)

	rec, report := getPromotionReport(t, op, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if report.ActorPrefix != promotionReportDefaultActor {
		t.Errorf("actor_prefix = %q, want %q", report.ActorPrefix, promotionReportDefaultActor)
	}
	if got := report.WindowEnd.Sub(report.WindowStart); got != promotionReportDefaultWindow {
		t.Errorf("window = %s, want %s", got, promotionReportDefaultWindow)
	}
	// The default prefix must exclude the council actor.
	if report.TotalActions != 2 || report.TotalDryRun != 1 || report.TotalExecuted != 1 {
		t.Fatalf("totals = %+v", report)
	}
	if report.ZeroEvidence {
		t.Error("zero_evidence set on a window holding two actions")
	}
}

func TestHandlePromotionReport_ActorAndWindowParams(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedPromotionEvents(t, op)

	rec, report := getPromotionReport(t, op, "?actor=council.&window=24h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if report.ActorPrefix != "council." || report.WindowEnd.Sub(report.WindowStart) != 24*time.Hour {
		t.Fatalf("params not honored: %+v", report)
	}
	if report.TotalActions != 1 || report.TotalExecuted != 1 {
		t.Fatalf("totals = %+v", report)
	}
	if len(report.PerActor) != 1 || report.PerActor[0].Actor != "council.mutator" {
		t.Fatalf("per_actor = %+v", report.PerActor)
	}

	// A window that predates the fixture must report zero evidence rather
	// than an empty table a reviewer reads as a clean soak.
	rec, report = getPromotionReport(t, op, "?window=1s")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !report.ZeroEvidence || report.TotalActions != 0 {
		t.Fatalf("narrow window = %+v, want zero evidence", report)
	}
}

func TestHandlePromotionReport_RejectsBadWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	for _, q := range []string{"?window=week", "?window=0s", "?window=-4h"} {
		rec, _ := getPromotionReport(t, op, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (body=%s)", q, rec.Code, rec.Body.String())
		}
	}
}

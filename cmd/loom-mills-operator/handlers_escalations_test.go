package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const relaunchCandidatesPath = "/api/mills/escalations/relaunch-candidates"

// seedRelaunchCandidate stores one backlog item plus one pipeline run whose
// escalation metadata the projection reads.
func seedRelaunchCandidate(
	t *testing.T,
	op *operator,
	itemID string,
	itemState store.BacklogState,
	run *store.PipelineRun,
) {
	t.Helper()
	ctx := context.Background()
	if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
		ID: itemID, Title: "title " + itemID,
		State: itemState, Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed backlog %s: %v", itemID, err)
	}
	if run != nil {
		run.BacklogID = itemID
		if run.Template == "" {
			run.Template = "mills-default-pipeline"
		}
		if err := op.store.Pipeline.PutRun(ctx, run); err != nil {
			t.Fatalf("seed run %s: %v", run.ID, err)
		}
	}
}

// An empty candidate set must encode as the JSON literal `[]`, never `null` —
// a bare null forces every client to special-case it (the same defect that
// broke the HUD drawer for in-flight runs).
func TestHandleEscalationRelaunchCandidates_EmptyIsJSONArrayNotNull(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, relaunchCandidatesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want the JSON literal []", got)
	}
}

func TestHandleEscalationRelaunchCandidates_ProjectsLatestRetryableEscalations(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	retryable := true
	notRetryable := false
	// Recent end times so the default now-7d window includes them.
	endedA := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	endedB := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	// Included: escalated item whose latest run is retryable.
	seedRelaunchCandidate(t, op, "BL-RETRY-A", store.BacklogEscalated, &store.PipelineRun{
		ID: "PIPE-RETRY-A", State: store.PipelineEscalated, Attempts: 1,
		StartedAt: endedA.Add(-time.Hour), EndedAt: &endedA,
		EscalationClass: "infra", FailureClass: "infrastructure",
		EscalationRetryable: &retryable,
	})
	// Excluded: latest run explicitly not retryable.
	seedRelaunchCandidate(t, op, "BL-NORETRY-B", store.BacklogEscalated, &store.PipelineRun{
		ID: "PIPE-NORETRY-B", State: store.PipelineEscalated, Attempts: 1,
		StartedAt: endedB.Add(-time.Hour), EndedAt: &endedB,
		EscalationClass: "config", FailureClass: "configuration",
		EscalationRetryable: &notRetryable,
	})
	// Excluded: retryable run but the item is not escalated.
	seedRelaunchCandidate(t, op, "BL-RUNNING-C", store.BacklogRunning, &store.PipelineRun{
		ID: "PIPE-RUNNING-C", State: store.PipelineEscalated, Attempts: 1,
		StartedAt: endedA.Add(-time.Hour), EndedAt: &endedA,
		EscalationRetryable: &retryable,
	})

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, relaunchCandidatesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Decode into generic maps to pin the ACTUAL serialization: PascalCase
	// store-row fields, exactly the declared projection.
	var body []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(body) != 1 {
		t.Fatalf("candidates = %d (%s), want 1", len(body), rec.Body.String())
	}
	row := body[0]
	for _, key := range []string{"ID", "Title", "EscalationClass", "FailureClass", "EndedAt"} {
		if _, ok := row[key]; !ok {
			t.Errorf("row is missing PascalCase key %q: %v", key, row)
		}
	}
	if row["ID"] != "BL-RETRY-A" || row["Title"] != "title BL-RETRY-A" {
		t.Errorf("identity = {%v %v}, want {BL-RETRY-A title BL-RETRY-A}", row["ID"], row["Title"])
	}
	if row["EscalationClass"] != "infra" || row["FailureClass"] != "infrastructure" {
		t.Errorf("classes = {%v %v}, want {infra infrastructure}", row["EscalationClass"], row["FailureClass"])
	}
	if row["EndedAt"] == nil {
		t.Errorf("EndedAt = nil, want %v", endedA)
	}
}

func TestHandleEscalationRelaunchCandidates_SinceAndLimitBoundTheList(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	retryable := true
	endedOld := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	endedNew := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	// Outside the default now-7d window.
	seedRelaunchCandidate(t, op, "BL-OLD", store.BacklogEscalated, &store.PipelineRun{
		ID: "PIPE-OLD", State: store.PipelineEscalated, Attempts: 1,
		StartedAt: endedOld.Add(-time.Hour), EndedAt: &endedOld,
		EscalationRetryable: &retryable,
	})
	seedRelaunchCandidate(t, op, "BL-NEW", store.BacklogEscalated, &store.PipelineRun{
		ID: "PIPE-NEW", State: store.PipelineEscalated, Attempts: 1,
		StartedAt: endedNew.Add(-time.Hour), EndedAt: &endedNew,
		EscalationRetryable: &retryable,
	})

	// Default window: only the recent candidate.
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, relaunchCandidatesPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var defaulted []*store.RelaunchCandidate
	if err := json.Unmarshal(rec.Body.Bytes(), &defaulted); err != nil {
		t.Fatalf("decode default window: %v", err)
	}
	if len(defaulted) != 1 || defaulted[0].ID != "BL-NEW" {
		t.Fatalf("default-window candidates = %+v, want only BL-NEW", defaulted)
	}

	// Explicit since widens the window to include the old candidate; newest
	// ended first.
	since := endedOld.Add(-time.Hour).Format(time.RFC3339)
	rec = httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, relaunchCandidatesPath+"?since="+since, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var widened []*store.RelaunchCandidate
	if err := json.Unmarshal(rec.Body.Bytes(), &widened); err != nil {
		t.Fatalf("decode widened window: %v", err)
	}
	if len(widened) != 2 || widened[0].ID != "BL-NEW" || widened[1].ID != "BL-OLD" {
		t.Fatalf("widened candidates = %+v, want [BL-NEW BL-OLD]", widened)
	}

	// limit bounds the widened list.
	rec = httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, relaunchCandidatesPath+"?since="+since+"&limit=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var limited []*store.RelaunchCandidate
	if err := json.Unmarshal(rec.Body.Bytes(), &limited); err != nil {
		t.Fatalf("decode limited: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "BL-NEW" {
		t.Fatalf("limited candidates = %+v, want only BL-NEW", limited)
	}
}

func TestHandleEscalationRelaunchCandidates_MalformedParamsAre400(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	for _, query := range []string{"?since=yesterday", "?limit=zero", "?limit=-5", "?limit=0"} {
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, relaunchCandidatesPath+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%s", query, rec.Code, rec.Body.String())
		}
	}
}

// The relaunch-candidates list is a read surface: no admin token required and
// no mutation verb exposed on the route.
func TestHandleEscalationRelaunchCandidates_IsAnOpenReadOnlyRoute(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, httptest.NewRequest(method, relaunchCandidatesPath, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s on the relaunch-candidates route returned 200; the projection must be read-only", method)
		}
	}
}

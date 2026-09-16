package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedProvenanceItem inserts one backlog item the events endpoint can hang a
// ledger off of.
func seedProvenanceItem(t *testing.T, op *operator, id string) {
	t.Helper()
	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{
		ID:       id,
		Title:    "provenance fixture",
		State:    store.BacklogQueued,
		Priority: store.P2,
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
}

// TestBacklogItemEvents_ReturnsLedgerNewestFirst proves the endpoint reads the
// item's own subject ledger and preserves the DAO's newest-first order — the
// order the journey strip renders top-down.
func TestBacklogItemEvents_ReturnsLedgerNewestFirst(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()
	seedProvenanceItem(t, op, "BL-JOURNEY")

	base := time.Now().UTC().Add(-time.Hour)
	for i, ev := range []struct {
		kind   string
		actor  string
		offset time.Duration
	}{
		{"reconciler.bootstrap_escalated", "reconciler", 0},
		{"operator.override.requeue", operatorOverrideActor, 10 * time.Minute},
		{"reconciler.auto_requeued", "reconciler", 20 * time.Minute},
	} {
		if err := op.store.Events.Append(ctx, &store.Event{
			OccurredAt:  base.Add(ev.offset),
			Actor:       ev.actor,
			Kind:        ev.kind,
			SubjectKind: backlogItemSubjectKind,
			SubjectID:   "BL-JOURNEY",
			Payload:     map[string]any{"seq": i},
		}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	// A same-id event under a DIFFERENT subject kind must not leak in: run and
	// item ledgers are distinct timelines that happen to share id spaces.
	if err := op.store.Events.Append(ctx, &store.Event{
		OccurredAt:  base,
		Actor:       "pipeline",
		Kind:        "pipeline.stage_done",
		SubjectKind: "pipeline_run",
		SubjectID:   "BL-JOURNEY",
	}); err != nil {
		t.Fatalf("append foreign-subject event: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/backlog/BL-JOURNEY/events", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		BacklogID string         `json:"backlog_id"`
		Events    []*store.Event `json:"events"`
		Partial   bool           `json:"partial"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BacklogID != "BL-JOURNEY" {
		t.Fatalf("backlog_id: got %q want BL-JOURNEY", got.BacklogID)
	}
	if !got.Partial {
		t.Fatal("partial must stay true: the ledger records only event-writing transitions")
	}
	if len(got.Events) != 3 {
		t.Fatalf("events: got %d want 3 (foreign subject_kind must be excluded)", len(got.Events))
	}
	if got.Events[0].Kind != "reconciler.auto_requeued" {
		t.Fatalf("newest-first: got %q first, want reconciler.auto_requeued", got.Events[0].Kind)
	}
	if got.Events[2].Kind != "reconciler.bootstrap_escalated" {
		t.Fatalf("oldest-last: got %q last, want reconciler.bootstrap_escalated", got.Events[2].Kind)
	}
}

// TestBacklogItemEvents_EmptyLedgerEncodesAsArray pins the wire contract: a
// real item with no recorded events yields `[]`, never `null`.
func TestBacklogItemEvents_EmptyLedgerEncodesAsArray(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedProvenanceItem(t, op, "BL-QUIET")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/backlog/BL-QUIET/events", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got["events"]) != "[]" {
		t.Fatalf("empty ledger: got %s want []", got["events"])
	}
}

// TestBacklogItemEvents_UnknownItemIs404 keeps "no such item" distinguishable
// from "item with an empty ledger". Collapsing them sends an operator hunting
// for a missing event writer when the real problem was a typo'd id.
func TestBacklogItemEvents_UnknownItemIs404(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/backlog/BL-NOPE/events", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestBacklogItemEvents_RejectsBadLimit proves ?limit= is validated rather than
// silently coerced — a nonsense limit is a caller bug worth reporting.
func TestBacklogItemEvents_RejectsBadLimit(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedProvenanceItem(t, op, "BL-LIMIT")

	for _, bad := range []string{"0", "-5", "abc"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/mills/backlog/BL-LIMIT/events?limit="+bad, nil)
		op.httpMux().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s: got %d want 400", bad, rec.Code)
		}
	}
}

// writeWaiverManifest writes a minimal but VALID fleet-gate manifest — the
// loader validates, so a waiver must reference a declared benchmark and exceed
// the global threshold or LoadManifest rejects the fixture.
func writeWaiverManifest(t *testing.T, root, until string) {
	t.Helper()
	dir := filepath.Join(root, "scripts", "ci")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := map[string]any{
		"schema_version": "loom.fleet-reliability.suite/v1",
		"suite_version":  7,
		"thresholds": map[string]any{
			"time_percent": 25, "bytes_percent": 15, "allocations_percent": 15,
		},
		"test_groups": []any{},
		"benchmarks":  []string{"BenchmarkFleetMillsEventAppend"},
		"waivers": []any{map[string]any{
			"benchmark":        "BenchmarkFleetMillsEventAppend",
			"max_time_percent": 60,
			"until":            until,
			"reason":           "fixture waiver",
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fleet_reliability_suite_v1.json"), raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// TestFleetGateWaivers_ComputesInclusiveExpiry is the endpoint's reason to
// exist: the countdown. Until is INCLUSIVE, so a waiver expiring today must
// report 0 days and NOT be expired — off-by-one here would tell the operator a
// live waiver is dead.
func TestFleetGateWaivers_ComputesInclusiveExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		untilDays int
		wantDays  int
		wantGone  bool
	}{
		{"expires today is still active", 0, 0, false},
		{"future waiver counts down", 30, 30, false},
		{"yesterday is expired", -1, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, cleanup := newTestOperator(t)
			defer cleanup()
			root := t.TempDir()
			until := time.Now().UTC().AddDate(0, 0, tc.untilDays).Format("2006-01-02")
			writeWaiverManifest(t, root, until)
			op.withRepoRoot(root)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/mills/fleet-gate/waivers", nil)
			op.httpMux().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
			}
			var got waiversResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got.Waivers) != 1 {
				t.Fatalf("waivers: got %d want 1", len(got.Waivers))
			}
			w := got.Waivers[0]
			if w.DaysRemaining != tc.wantDays {
				t.Fatalf("days_remaining: got %d want %d", w.DaysRemaining, tc.wantDays)
			}
			if w.Expired != tc.wantGone {
				t.Fatalf("expired: got %v want %v", w.Expired, tc.wantGone)
			}
			// The global threshold is what makes a raised cap legible.
			if got.GlobalTimePercent != 25 {
				t.Fatalf("global_time_percent: got %v want 25", got.GlobalTimePercent)
			}
			if got.SuiteVersion != 7 {
				t.Fatalf("suite_version: got %d want 7", got.SuiteVersion)
			}
		})
	}
}

// TestFleetGateWaivers_ServesTheCommittedManifest runs the endpoint against the
// REAL scripts/ci/fleet_reliability_suite_v1.json in this checkout rather than a
// fixture.
//
// The fixture tests prove the arithmetic; this one proves the endpoint and the
// committed manifest still agree. The waiver file is edited by hand under time
// pressure (that is what a waiver IS), so a schema drift that silently 503s the
// card is exactly the failure this catches — and a silently missing card is
// indistinguishable from "no active waivers", which is the whole cliff the
// feature exists to prevent.
func TestFleetGateWaivers_ServesTheCommittedManifest(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	// cmd/loom-mills-operator → repo root.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, fleetGateManifestRelPath)); statErr != nil {
		t.Skipf("manifest not present at %s: %v", root, statErr)
	}
	op.withRepoRoot(root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/fleet-gate/waivers", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("committed manifest must serve 200: got %d; body=%s", rec.Code, rec.Body.String())
	}
	var got waiversResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.GlobalTimePercent <= 0 {
		t.Fatalf("global_time_percent must be populated, got %v", got.GlobalTimePercent)
	}
	// Every waiver must carry the fields the card renders; a blank benchmark or
	// reason would draw an unexplained pill.
	for _, w := range got.Waivers {
		if strings.TrimSpace(w.Benchmark) == "" || strings.TrimSpace(w.Reason) == "" {
			t.Fatalf("waiver missing benchmark/reason: %+v", w)
		}
		if _, perr := time.Parse("2006-01-02", w.Until); perr != nil {
			t.Fatalf("waiver %q has an unparseable until %q: %v", w.Benchmark, w.Until, perr)
		}
	}
}

// TestFleetGateWaivers_DegradesWithoutRepoRoot proves the card's feed fails
// soft: an unconfigured checkout is 503-with-reason, not a 500, because the
// panel hosting this card must keep rendering.
func TestFleetGateWaivers_DegradesWithoutRepoRoot(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withRepoRoot("")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/fleet-gate/waivers", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestFleetGateWaivers_DegradesOnMissingManifest covers the stale/incomplete
// checkout: RepoRoot set but the file absent must also be a soft 503.
func TestFleetGateWaivers_DegradesOnMissingManifest(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withRepoRoot(t.TempDir())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/fleet-gate/waivers", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBacklogItemEvents_TransactionReservationDeferral(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedProvenanceItem(t, op, "BL-DEFERRED")
	if err := op.store.Events.Append(context.Background(), &store.Event{
		OccurredAt: time.Now().UTC(), Actor: "reconciler", Kind: "reconciler.deferred",
		SubjectKind: backlogItemSubjectKind, SubjectID: "BL-DEFERRED",
		Payload: map[string]any{"outcome": "scope_reservation_transaction", "item": "BL-DEFERRED", "blocked_by": "older", "witness": "pkg/shared.go"},
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/backlog/BL-DEFERRED/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Events []*store.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 {
		t.Fatalf("events=%+v", got.Events)
	}
	e := got.Events[0]
	if e.Payload["outcome"] != "scope_reservation_transaction" || e.SubjectID != "BL-DEFERRED" || e.Payload["blocked_by"] != "older" || e.Payload["witness"] != "pkg/shared.go" {
		t.Fatalf("event=%+v", e)
	}
}

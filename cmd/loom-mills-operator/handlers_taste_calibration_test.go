package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestTasteCalibrationEndpointRegistered(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/taste/calibration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var report store.OutcomeCalibration
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Buckets == nil {
		t.Fatalf("buckets must encode as []: %s", rec.Body.String())
	}
	bad := httptest.NewRecorder()
	op.httpMux().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/mills/taste/calibration?window=nope", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad window status=%d", bad.Code)
	}
}

func TestTasteCalibrationRealizedQualityAndNullContract(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()
	fixtures := []struct {
		id    string
		state store.BacklogState
		grade string
		score float64
	}{
		{"keep", store.BacklogMerged, "keep", .21},
		{"meh", store.BacklogMerged, "meh", .29},
		{"graded-escalation", store.BacklogEscalated, "keep", .25},
		{"regret", store.BacklogMerged, "regret", .81},
		{"ungraded", store.BacklogMerged, "", .55},
	}
	for _, f := range fixtures {
		if err := op.store.Backlog.Put(ctx, &store.BacklogItem{ID: f.id, Title: f.id, State: f.state, Priority: store.P2, Grade: f.grade, CreatedBy: "test", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		pipelineState := store.PipelineDone
		if f.state == store.BacklogEscalated {
			pipelineState = store.PipelineEscalated
		}
		ended := now
		if err := op.store.Pipeline.PutRun(ctx, &store.PipelineRun{ID: "run-" + f.id, BacklogID: f.id, Template: "test", State: pipelineState, StartedAt: now, EndedAt: &ended}); err != nil {
			t.Fatal(err)
		}
		if _, err := op.store.Outcomes.WriteTerminal(ctx, f.id, f.score); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/taste/calibration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var report store.OutcomeCalibration
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	byLower := make(map[float64]store.CalibrationBucket, len(report.Buckets))
	for _, bucket := range report.Buckets {
		byLower[bucket.Lower] = bucket
	}
	assertMetric := func(name string, got *float64, want float64) {
		t.Helper()
		if got == nil || math.Abs(*got-want) > 1e-9 {
			t.Errorf("%s=%v want %v", name, got, want)
		}
	}
	assertMetric("0.2 realized quality", byLower[.2].RealizedQuality, .75)
	assertMetric("0.2 taste error", byLower[.2].TasteCalibrationError, .5)
	assertMetric("0.8 realized quality", byLower[.8].RealizedQuality, 0)
	assertMetric("0.8 taste error", byLower[.8].TasteCalibrationError, .81)
	if bucket := byLower[.5]; bucket.RealizedQuality != nil || bucket.TasteCalibrationError != nil {
		t.Errorf("ungraded bucket must use null metrics: %+v", bucket)
	}
	var raw struct {
		Buckets []map[string]json.RawMessage `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	foundNull := false
	for _, bucket := range raw.Buckets {
		var lower float64
		_ = json.Unmarshal(bucket["lower"], &lower)
		if lower == .5 {
			foundNull = string(bucket["realized_quality"]) == "null" && string(bucket["taste_calibration_error"]) == "null"
		}
	}
	if !foundNull {
		t.Fatalf("ungraded metrics did not encode as JSON null: %s", rec.Body.String())
	}
}

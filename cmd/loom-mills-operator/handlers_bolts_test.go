package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestBoltHandlerWindowValidationAndEmptyShape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	bad := httptest.NewRecorder()
	op.httpMux().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/mills/bolts?window=nope", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad window status=%d", bad.Code)
	}
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/bolts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got boltsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.WindowSeconds != 86400 || got.Bolts == nil {
		t.Fatalf("response=%+v", got)
	}
}

func TestAggregatesJSONTags(t *testing.T) {
	b, err := json.Marshal(store.TasteAggregates{OverallGraded14d: 1, OverallMerged14d: 2, OverallCoverage14d: .5})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	for _, key := range []string{"overall_graded_14d", "overall_merged_14d", "overall_coverage_14d"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %s in %s", key, b)
		}
	}
}

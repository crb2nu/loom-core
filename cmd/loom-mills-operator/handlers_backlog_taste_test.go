package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTasteAggregatesIsOpenRead(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/taste/aggregates", nil)
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"plans":[],"overall_graded_14d":0,"overall_merged_14d":0,"overall_coverage_14d":0}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("body=%s want=%s", rec.Body.String(), want)
	}
}

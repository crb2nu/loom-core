package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestBacklogList_EmptyEncodesAsArray pins the mill-floor B2 contract: an
// empty backlog serializes as `[]`, never `null`. A bare `null` body forced
// every client to special-case it and crashed the HUD warp view on an empty
// beam.
func TestBacklogList_EmptyEncodesAsArray(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/backlog", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Fatalf("empty backlog body: got %q want %q", body, "[]")
	}
}

// TestBacklogList_InnerArraysNeverNull proves the coercion reaches the inner
// arrays: a seeded item whose Labels/Dependencies/Slices and per-slice
// Files/Tests are unset must never serialize any of them as `null`.
func TestBacklogList_InnerArraysNeverNull(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	// Seed one item that carries a slice with no files/tests declared and no
	// labels/dependencies. The store may or may not round-trip nil→[] on its
	// own; the encode-boundary coercion must guarantee the wire shape either
	// way.
	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{
		ID:       "BACK-NIL-ARRAYS",
		Title:    "nil arrays fixture",
		State:    store.BacklogQueued,
		Priority: store.P2,
		Slices:   []store.Slice{{Name: "only-name"}},
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/backlog", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, needle := range []string{
		`"Labels":null`, `"Dependencies":null`, `"Slices":null`,
		`"files":null`, `"tests":null`,
	} {
		if strings.Contains(raw, needle) {
			t.Errorf("wire contains %s (nil array not coerced): body=%s", needle, raw)
		}
	}
	// And positively assert the coerced empty arrays are present.
	for _, needle := range []string{
		`"Labels":[]`, `"Dependencies":[]`, `"files":[]`, `"tests":[]`,
	} {
		if !strings.Contains(raw, needle) {
			t.Errorf("wire missing %s: body=%s", needle, raw)
		}
	}
}

// TestBacklogGet_InnerArraysNeverNull applies the same guarantee to the
// single-item endpoint that backs the Warps drawer.
func TestBacklogGet_InnerArraysNeverNull(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{
		ID:       "BACK-GET-NIL",
		Title:    "single item nil arrays",
		State:    store.BacklogQueued,
		Priority: store.P1,
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/backlog/BACK-GET-NIL", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, needle := range []string{`"Labels":null`, `"Dependencies":null`, `"Slices":null`} {
		if strings.Contains(raw, needle) {
			t.Errorf("wire contains %s (nil array not coerced): body=%s", needle, raw)
		}
	}
}

// TestCoerceBacklogArrays_Unit exercises the pure coercion directly, including
// the deliberate carve-out: omitempty fields (Slice.ParallelWith,
// SuccessCriteria.*, ItemPolicy.ProtectedPathsTouched) stay omitted, not `[]`.
func TestCoerceBacklogArrays_Unit(t *testing.T) {
	item := &store.BacklogItem{
		ID:     "unit",
		Slices: []store.Slice{{Name: "s1"}}, // nil Files/Tests/ParallelWith
	}
	coerceBacklogArrays(item)

	if item.Labels == nil || item.Dependencies == nil || item.Slices == nil {
		t.Fatal("top-level arrays not coerced to empty")
	}
	if item.Slices[0].Files == nil || item.Slices[0].Tests == nil {
		t.Fatal("per-slice Files/Tests not coerced to empty")
	}
	// ParallelWith is omitempty — leaving it nil keeps it off the wire.
	if item.Slices[0].ParallelWith != nil {
		t.Error("ParallelWith should be left nil (omitempty), not coerced")
	}

	b, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(b)
	// Only the array fields must be free of `null`; nil pointer fields
	// (GitLabIssueIID, CouncilRunID) legitimately marshal as null and are out
	// of scope for array coercion.
	for _, needle := range []string{
		`"Labels":null`, `"Dependencies":null`, `"Slices":null`,
		`"files":null`, `"tests":null`,
	} {
		if strings.Contains(raw, needle) {
			t.Errorf("coerced item still marshals %s: %s", needle, raw)
		}
	}
	if strings.Contains(raw, `"parallel_with"`) {
		t.Errorf("omitempty ParallelWith leaked onto the wire: %s", raw)
	}
}

// TestCoerceBacklogList_NilBecomesEmpty covers the top-level nil list.
func TestCoerceBacklogList_NilBecomesEmpty(t *testing.T) {
	out := coerceBacklogList(nil)
	if out == nil {
		t.Fatal("nil list not coerced to empty slice")
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); got != "[]" {
		t.Fatalf("nil list marshals to %q want []", got)
	}
}

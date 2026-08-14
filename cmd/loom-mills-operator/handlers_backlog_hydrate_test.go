package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

// fakePlanReader serves canned plan slices, mimicking the hub transport
// gotcha: the LIST view returns slices WITHOUT files (TOON tabular drops
// array columns); the detail view carries them.
type fakePlanReader struct {
	slices    []clients.PlanSliceSummary // list view (no files)
	details   map[string]clients.PlanSliceSummary
	listErr   error
	listCalls int
}

func (f *fakePlanReader) ListSlices(_ context.Context, planID string) ([]clients.PlanSliceSummary, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.slices, nil
}

func (f *fakePlanReader) GetSlice(_ context.Context, sliceID string) (clients.PlanSliceSummary, error) {
	return f.details[sliceID], nil
}

// TestHandleBacklog_CreateHydratesPlanSliceScope locks in the intake-time
// hydration that unblocked the pattern-stamp kill-test: a plan-linked item
// arriving with a file-less slice (the HUD stamp projection loses `files`
// crossing the hub — live escalations 2026-07-01, widget + gadget runs) gets
// its slice scope materialized from the plan store before persisting, so the
// post-implement scope gate has an allowlist to enforce.
func TestHandleBacklog_CreateHydratesPlanSliceScope(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.planReader = &fakePlanReader{
		slices: []clients.PlanSliceSummary{{ID: "plan-x#1", PlanID: "plan-x", Name: "scaffold gadget service"}},
		details: map[string]clients.PlanSliceSummary{
			"plan-x#1": {ID: "plan-x#1", Name: "scaffold gadget service",
				Files: []string{"examples/gadget/cmd/gadget/main.go", "examples/gadget/go.mod"}},
		},
	}

	body := `{"ID":"STAMP-H1","Title":"Pattern stamp: gadget","PlanID":"plan-x","Slices":[{"name":"scaffold gadget service","files":[],"tests":[]}]}`
	rec := httptest.NewRecorder()
	op.handleBacklogCreate(rec, httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}

	got, err := op.store.Backlog.Get(context.Background(), "STAMP-H1")
	if err != nil {
		t.Fatalf("post-create get: %v", err)
	}
	if len(got.Slices) != 1 || len(got.Slices[0].Files) != 2 {
		b, _ := json.Marshal(got.Slices)
		t.Fatalf("slices not hydrated: %s", b)
	}
	if got.Slices[0].Name != "scaffold gadget service" {
		t.Errorf("slice name = %q", got.Slices[0].Name)
	}
}

// TestHandleBacklog_CreateHydrationSkips locks in the guardrails: no reader,
// no PlanID, or already-materialized scope must leave the item untouched, and
// a plan-store failure must not fail the request.
func TestHandleBacklog_CreateHydrationSkips(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	// Already-materialized scope: reader must not be consulted.
	reader := &fakePlanReader{}
	op.planReader = reader
	body := `{"ID":"STAMP-H2","Title":"t","PlanID":"plan-y","Slices":[{"name":"s","files":["a.go"],"tests":[]}]}`
	rec := httptest.NewRecorder()
	op.handleBacklogCreate(rec, httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}
	if reader.listCalls != 0 {
		t.Errorf("reader consulted despite materialized scope (%d calls)", reader.listCalls)
	}

	// Plan-store failure: best-effort, request still succeeds, item unchanged.
	op.planReader = &fakePlanReader{listErr: context.DeadlineExceeded}
	body = `{"ID":"STAMP-H3","Title":"t","PlanID":"plan-z"}`
	rec = httptest.NewRecorder()
	op.handleBacklogCreate(rec, httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with failing reader: %d body=%s", rec.Code, rec.Body.String())
	}
	got, err := op.store.Backlog.Get(context.Background(), "STAMP-H3")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Slices) != 0 {
		t.Errorf("slices = %+v, want none (hydration failed best-effort)", got.Slices)
	}
}

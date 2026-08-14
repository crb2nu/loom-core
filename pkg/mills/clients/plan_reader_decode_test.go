package clients

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeBodyHub returns one canned (body, err) pair for every tool call —
// including the tool-ERROR shape where CallTool surfaces BOTH the raw
// text body and a non-nil error (IsError=true results).
type fakeBodyHub struct {
	body string
	err  error
}

func (f *fakeBodyHub) CallTool(context.Context, string, string, map[string]any) (string, error) {
	return f.body, f.err
}

// TestPlanClient_ListSlices_ToolErrorBodyDoesNotFabricateSlicelessPlan is
// the plan-reader twin of the devbox #322 regression: a tool ERROR whose
// plain-text body contains a colon TOON-decoded into an unrelated object,
// unmarshaled into a zero-value sliceListEnvelope, and ListSlices dropped
// the CallTool error — reading "plan has no slices" (the sliceless-item
// scope-gate cascade) instead of surfacing the real failure.
func TestPlanClient_ListSlices_ToolErrorBodyDoesNotFabricateSlicelessPlan(t *testing.T) {
	const errBody = "plan get: not found: plan-2026-07-x"
	hub := &fakeBodyHub{body: errBody, err: errors.New(errBody)}
	pc := &PlanClient{Hub: hub}
	slices, err := pc.ListSlices(context.Background(), "plan-2026-07-x")
	if err == nil {
		t.Fatalf("expected an error, not a fabricated empty slice list: %+v", slices)
	}
	if !strings.Contains(err.Error(), "not found: plan-2026-07-x") {
		t.Errorf("error should carry the real tool failure text: %v", err)
	}
}

func TestPlanClient_ListPlans_ToolErrorBodyDoesNotFabricateEmptyList(t *testing.T) {
	const errBody = "plan list: store unavailable: connection refused"
	hub := &fakeBodyHub{body: errBody, err: errors.New(errBody)}
	pc := &PlanClient{Hub: hub}
	plans, err := pc.ListPlans(context.Background(), "loom-core", "", "")
	if err == nil {
		t.Fatalf("expected an error, not a fabricated empty plan list: %+v", plans)
	}
	if !strings.Contains(err.Error(), "store unavailable") {
		t.Errorf("error should carry the real tool failure text: %v", err)
	}
}

func TestPlanClient_GetSlice_ToolErrorBodyDoesNotFabricateZeroSlice(t *testing.T) {
	const errBody = "slice get: not found: p#7"
	hub := &fakeBodyHub{body: errBody, err: errors.New(errBody)}
	pc := &PlanClient{Hub: hub}
	sl, err := pc.GetSlice(context.Background(), "p#7")
	if err == nil {
		t.Fatalf("expected an error, not a zero-value slice: %+v", sl)
	}
	if !strings.Contains(err.Error(), "not found: p#7") {
		t.Errorf("error should carry the real tool failure text: %v", err)
	}
}

// A colon-bearing plain-text body with NO transport error (e.g. an
// IsError result a hub layer flattened to text) must fail the strict
// probe rather than decode as a zero-value envelope.
func TestDecodeListBody_RejectsColonBearingPlainText(t *testing.T) {
	var env sliceListEnvelope
	err := decodeListBody("ensure hub: dial tcp 10.0.0.5:443: connection refused", &env)
	if err == nil {
		t.Fatalf("expected probe rejection, decoded: %+v", env)
	}
	if !strings.Contains(err.Error(), `"ok"`) {
		t.Errorf("error should name the missing probe field: %v", err)
	}
}

// A syntactically valid JSON object that is not an agent-context envelope
// (no "ok" field) must not decode into a zero-value envelope.
func TestDecodeListBody_RejectsJSONWithoutOKField(t *testing.T) {
	var env planListEnvelope
	if err := decodeListBody(`{"error":"backend unavailable"}`, &env); err == nil {
		t.Fatalf("expected probe rejection, decoded: %+v", env)
	}
}

// The strict probe must not break genuine TOON responses: the hub's
// tabular re-encoding keeps the top-level "ok" boolean.
func TestDecodeListBody_DecodesTOONSliceList(t *testing.T) {
	toonBody := "ok: true\ncount: 2\nslices[2]{id,plan_id,name,phase}:\n  p#1,p,slice-one,pending\n  p#2,p,slice-two,implementing"
	var env sliceListEnvelope
	if err := decodeListBody(toonBody, &env); err != nil {
		t.Fatalf("genuine TOON envelope should decode: %v", err)
	}
	if !env.OK {
		t.Error("OK should be true")
	}
	if len(env.Slices) != 2 || env.Slices[0].ID != "p#1" || env.Slices[1].Name != "slice-two" {
		t.Errorf("slices = %+v, want the 2 tabular rows", env.Slices)
	}
}

// A structured refusal ({"ok": false, ...}) passes the shape probe but
// must surface as an error, not an empty result.
func TestPlanClient_ListSlices_RejectedEnvelopeSurfacesError(t *testing.T) {
	hub := &fakeBodyHub{body: `{"ok":false,"error":"store offline"}`}
	pc := &PlanClient{Hub: hub}
	if _, err := pc.ListSlices(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want rejection error, got: %v", err)
	}
}

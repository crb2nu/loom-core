package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/spin"
)

// spinFakeEditor is a council.Editor that returns a fixed decomposition so the
// spin handler exercises the full path without a live model backend.
type spinFakeEditor struct{}

func (spinFakeEditor) Edit(_ context.Context, _ *council.Brief, _ []council.ReviewerOutput) (*council.EditorOutput, error) {
	return &council.EditorOutput{
		Backend: "flexinfer",
		Model:   "claude-opus",
		CostUSD: 0.11,
		BacklogProposals: []council.BacklogProposal{{
			Title: "Spun feature",
			PlanSlices: []council.PlanSliceSpec{
				{Name: "core", Goal: "do the thing", Files: []string{"pkg/x/core.go"}},
			},
		}},
	}, nil
}

// spinFakeAuthor records what it was asked to author and returns a canned id.
type spinFakeAuthor struct {
	got    spin.DraftPlanInput
	planID string
}

func (a *spinFakeAuthor) AuthorDraftPlan(_ context.Context, in spin.DraftPlanInput) (string, error) {
	a.got = in
	return a.planID, nil
}

// spinnerWithFakes builds a Spinner around one always-available frame backed by
// the fake editor + author, independent of policy so the handler test doesn't
// depend on the seeded policy fixture.
func spinnerWithFakes(author spin.DraftPlanAuthor) *spin.Spinner {
	return &spin.Spinner{
		Enabled: func() bool { return true },
		Frame: func(name string) (spin.Frame, bool) {
			if name == "opus" {
				return spin.Frame{Name: "opus", Model: "claude-opus", Backend: "flexinfer"}, true
			}
			return spin.Frame{}, false
		},
		NewEditor:       func(spin.Frame) (council.Editor, error) { return spinFakeEditor{}, nil },
		Author:          author,
		DefaultPriority: func() string { return "P2" },
	}
}

func TestHandleSpin_503WhenNoSpinner(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	// op.spinner is nil by default.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin",
		strings.NewReader(`{"brief":"b","frame":"opus"}`))
	op.handleSpin(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandleSpin_HappyPath(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	author := &spinFakeAuthor{planID: "plan-spun-1"}
	op.withSpinner(spinnerWithFakes(author))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin",
		strings.NewReader(`{"brief":"Ship a thing","frame":"opus","priority":"P0","project":"services/loom-core","namespace":"mills/spun"}`))
	op.handleSpin(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var res spin.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if res.PlanID != "plan-spun-1" || res.Frame != "opus" || res.SliceCount != 1 {
		t.Errorf("result = %+v", res)
	}
	if res.Priority != "P0" {
		t.Errorf("priority = %q, want P0", res.Priority)
	}
	// The author saw the scope + audit trail.
	if author.got.Project != "services/loom-core" || author.got.Namespace != "mills/spun" || author.got.Frame != "opus" {
		t.Errorf("author input = %+v", author.got)
	}
}

func TestHandleSpin_UnknownFrameIs400(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withSpinner(spinnerWithFakes(&spinFakeAuthor{planID: "x"}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin",
		strings.NewReader(`{"brief":"b","frame":"gpt-nope"}`))
	op.handleSpin(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown frame", rec.Code)
	}
}

func TestHandleSpin_InvalidBodyIs400(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withSpinner(spinnerWithFakes(&spinFakeAuthor{planID: "x"}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin", strings.NewReader(`{"brief":`))
	op.handleSpin(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed body", rec.Code)
	}
}

func TestHandleSpinningRoomFrames_DefaultDisabled(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/mills/spinning-room/frames", nil)
	op.handleSpinningRoomFrames(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Live-policy endpoint must be uncacheable so the HUD picker never shows a
	// stale/empty frame list after a policy edit.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var body struct {
		Enabled         bool                `json:"enabled"`
		Available       bool                `json:"available"`
		DefaultPriority string              `json:"default_priority"`
		Frames          []map[string]string `json:"frames"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Enabled || body.Available {
		t.Errorf("seeded policy has no spinning room: enabled=%v available=%v", body.Enabled, body.Available)
	}
	if body.DefaultPriority != "P2" {
		t.Errorf("default priority = %q, want P2", body.DefaultPriority)
	}
	if len(body.Frames) != 0 {
		t.Errorf("frames = %v, want empty", body.Frames)
	}
}

// competitiveSpinnerWithFakes wires two always-available frames so the handler
// test can exercise the frames[] (competitive) request path.
func competitiveSpinnerWithFakes(author spin.DraftPlanAuthor) *spin.Spinner {
	frames := map[string]spin.Frame{
		"mule": {Name: "mule", Model: "gpt-5.4", Backend: "openai"},
		"ring": {Name: "ring", Model: "gemma4", Backend: "flexinfer"},
	}
	return &spin.Spinner{
		Enabled: func() bool { return true },
		Frame: func(name string) (spin.Frame, bool) {
			f, ok := frames[name]
			return f, ok
		},
		NewEditor:       func(spin.Frame) (council.Editor, error) { return spinFakeEditor{}, nil },
		Author:          author,
		DefaultPriority: func() string { return "P2" },
	}
}

// competitiveFakeAuthor mints a per-frame plan id (concurrency-safe).
type competitiveFakeAuthor struct {
	mu sync.Mutex
	in []spin.DraftPlanInput
}

func (a *competitiveFakeAuthor) AuthorDraftPlan(_ context.Context, in spin.DraftPlanInput) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.in = append(a.in, in)
	return "plan-" + in.Frame, nil
}

func TestHandleSpin_CompetitiveFramesReturnsOneDraftPerFrame(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	author := &competitiveFakeAuthor{}
	op.withSpinner(competitiveSpinnerWithFakes(author))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin",
		strings.NewReader(`{"brief":"Ship a thing","frames":["mule","ring"],"namespace":"mills/spun"}`))
	op.handleSpin(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var cr spin.CompetitiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(cr.Results) != 2 || len(cr.Failures) != 0 {
		t.Fatalf("results/failures = %d/%d (%s)", len(cr.Results), len(cr.Failures), rec.Body.String())
	}
	if cr.Results[0].Frame != "mule" || cr.Results[0].PlanID != "plan-mule" {
		t.Errorf("results[0] = %+v", cr.Results[0])
	}
	if cr.Results[1].Frame != "ring" || cr.Results[1].PlanID != "plan-ring" {
		t.Errorf("results[1] = %+v", cr.Results[1])
	}
}

// A frames list with one entry still gets the composite shape — the response
// contract keys off the field's presence, not the count.
func TestHandleSpin_SingleEntryFramesListIsComposite(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withSpinner(competitiveSpinnerWithFakes(&competitiveFakeAuthor{}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin",
		strings.NewReader(`{"brief":"b","frames":["ring"]}`))
	op.handleSpin(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var cr spin.CompetitiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(cr.Results) != 1 || cr.Results[0].PlanID != "plan-ring" {
		t.Fatalf("results = %+v", cr.Results)
	}
}

func TestHandleSpin_CompetitiveUnknownFrameIs400(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withSpinner(competitiveSpinnerWithFakes(&competitiveFakeAuthor{}))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/mills/spin",
		strings.NewReader(`{"brief":"b","frames":["mule","gpt-nope"]}`))
	op.handleSpin(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for off-policy frame", rec.Code)
	}
}

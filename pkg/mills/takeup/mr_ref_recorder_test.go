package takeup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/intake"
)

// testLogger keeps the recorder's advisory logging out of test output.
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSliceMRStore is a PlanSliceMRStore double: canned list/detail views plus
// recorded writes. It mirrors the LIST view's real behaviour — array columns
// (files, decisions) are dropped by the TOON tabular encoding, so they only
// appear in the detail view.
type fakeSliceMRStore struct {
	slices   []clients.PlanSliceSummary
	details  map[string]clients.PlanSliceSummary
	listErr  error
	getErr   map[string]error
	writeErr error

	mrRefs       map[string]string
	decisions    map[string][]string
	detailFetch  []string
	updateCalls  int
	listCallsFor []string
}

func newFakeSliceMRStore(slices ...clients.PlanSliceSummary) *fakeSliceMRStore {
	return &fakeSliceMRStore{
		slices:    slices,
		details:   map[string]clients.PlanSliceSummary{},
		getErr:    map[string]error{},
		mrRefs:    map[string]string{},
		decisions: map[string][]string{},
	}
}

func (f *fakeSliceMRStore) ListSlices(_ context.Context, planID string) ([]clients.PlanSliceSummary, error) {
	f.listCallsFor = append(f.listCallsFor, planID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	// The list projection never carries arrays.
	out := make([]clients.PlanSliceSummary, 0, len(f.slices))
	for _, sl := range f.slices {
		sl.Files = nil
		sl.Decisions = nil
		out = append(out, sl)
	}
	return out, nil
}

func (f *fakeSliceMRStore) GetSlice(_ context.Context, sliceID string) (clients.PlanSliceSummary, error) {
	f.detailFetch = append(f.detailFetch, sliceID)
	if err := f.getErr[sliceID]; err != nil {
		return clients.PlanSliceSummary{}, err
	}
	if d, ok := f.details[sliceID]; ok {
		return d, nil
	}
	return clients.PlanSliceSummary{ID: sliceID}, nil
}

func (f *fakeSliceMRStore) UpdateSliceMRRef(_ context.Context, sliceID, mrRef string) error {
	f.updateCalls++
	if f.writeErr != nil {
		return f.writeErr
	}
	f.mrRefs[sliceID] = mrRef
	return nil
}

func (f *fakeSliceMRStore) AppendSliceDecision(_ context.Context, sliceID, note string) error {
	f.decisions[sliceID] = append(f.decisions[sliceID], note)
	return nil
}

func slice(id, name string, files ...string) clients.PlanSliceSummary {
	return clients.PlanSliceSummary{ID: id, PlanID: "plan-x", Name: name, Files: files}
}

// TestRecordMRRef_AttributesByEmittedBacklogID pins the dominant path: a psl-*
// item's id is derived from its slice id, so attribution is exact even when a
// plan has several in-flight slices and the run captured no files. This is the
// case that stalled plan-stamp-loom-runbook-loom-runbook (slice #1, MR !1380).
func TestRecordMRRef_AttributesByEmittedBacklogID(t *testing.T) {
	target := slice("plan-x#2", "second")
	st := newFakeSliceMRStore(slice("plan-x#1", "first"), target, slice("plan-x#3", "third"))
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-x",
		intake.BacklogIDForSlice(target.ID), "!1380", nil)
	if err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if got != target.ID {
		t.Fatalf("recorded slice = %q, want %q", got, target.ID)
	}
	if st.mrRefs[target.ID] != "!1380" {
		t.Fatalf("mr_refs = %v, want %s -> !1380", st.mrRefs, target.ID)
	}
	if len(st.decisions) != 0 {
		t.Fatalf("unexpected decision notes: %v", st.decisions)
	}
}

// A retried run opens a NEW MR. The emitted-item linkage is exact, so the
// newer ref must replace the stale one — the reconciler follows whichever MR
// is live now.
func TestRecordMRRef_BacklogIDMatchOverwritesStaleRef(t *testing.T) {
	target := slice("plan-x#1", "only")
	target.MRRef = "!900"
	st := newFakeSliceMRStore(target)
	rec := NewMRRefRecorder(st, testLogger())

	if _, err := rec.RecordMRRef(context.Background(), "plan-x",
		intake.BacklogIDForSlice(target.ID), "!1380", nil); err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if st.mrRefs[target.ID] != "!1380" {
		t.Fatalf("mr_ref = %q, want !1380 (newer MR wins)", st.mrRefs[target.ID])
	}
}

// A plan with one slice has exactly one place the MR can belong, so an item
// whose id isn't emitter-derived (a backfilled plan-mills-<id> plan) still
// resolves — including over a stale ref from an earlier attempt.
func TestRecordMRRef_AttributesSingleSlicePlan(t *testing.T) {
	only := slice("plan-mills-bl-x#1", "the work")
	only.MRRef = "!900" // earlier attempt, superseded
	only.Phase = "implementing"
	st := newFakeSliceMRStore(only)
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-mills-bl-x", "BL-X", "!1380", nil)
	if err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if got != only.ID || st.mrRefs[only.ID] != "!1380" {
		t.Fatalf("recorded %q (%v), want %q -> !1380", got, st.mrRefs, only.ID)
	}
}

// ...but a slice that already MERGED against another MR keeps its provenance:
// take-up skips merged slices, so overwriting the ref would only lose history.
func TestRecordMRRef_SingleSlicePlanKeepsMergedProvenance(t *testing.T) {
	only := slice("plan-mills-bl-x#1", "the work")
	only.MRRef = "!900"
	only.Phase = "merged"
	st := newFakeSliceMRStore(only)
	rec := NewMRRefRecorder(st, testLogger())

	if _, err := rec.RecordMRRef(context.Background(), "plan-mills-bl-x", "BL-X", "!1380", nil); !errors.Is(err, ErrSliceAmbiguous) {
		t.Fatalf("err = %v, want ErrSliceAmbiguous", err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("overwrote a merged slice's ref: %v", st.mrRefs)
	}
}

// An item that isn't emitter-derived (a stamped-pattern or backfilled plan)
// still resolves when only one slice is unrecorded.
func TestRecordMRRef_AttributesBySingleUnrecordedCandidate(t *testing.T) {
	done := slice("plan-x#1", "merged-already")
	done.MRRef = "!1000"
	open := slice("plan-x#2", "in-flight")
	st := newFakeSliceMRStore(done, open)
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-x", "some-other-item", "!1380", nil)
	if err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if got != open.ID || st.mrRefs[open.ID] != "!1380" {
		t.Fatalf("recorded %q (%v), want %q", got, st.mrRefs, open.ID)
	}
	if _, touched := st.mrRefs[done.ID]; touched {
		t.Fatalf("already-recorded slice was overwritten: %v", st.mrRefs)
	}
}

// With several unrecorded candidates, the run's captured paths decide — and
// the declared files must be recovered from the DETAIL view, because the list
// projection drops array columns.
func TestRecordMRRef_AttributesByFileOverlapViaDetailView(t *testing.T) {
	a := slice("plan-x#1", "hud", "internal/hud/api.go")
	b := slice("plan-x#2", "mills", "pkg/mills/pipeline/dispatcher.go")
	st := newFakeSliceMRStore(a, b)
	st.details = map[string]clients.PlanSliceSummary{a.ID: a, b.ID: b}
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-x", "unlinked-item", "!1380",
		[]string{"./pkg/mills/pipeline/dispatcher.go", "pkg/mills/pipeline/dispatcher_test.go"})
	if err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if got != b.ID {
		t.Fatalf("recorded slice = %q, want %q (file overlap)", got, b.ID)
	}
	if len(st.detailFetch) == 0 {
		t.Fatal("expected detail hydration for file-less list projection")
	}
}

// A slice declaring a directory envelope covers the files inside it.
func TestRecordMRRef_FileOverlapMatchesDirectoryEnvelope(t *testing.T) {
	a := slice("plan-x#1", "docs", "docs")
	b := slice("plan-x#2", "code", "pkg/mills/takeup")
	st := newFakeSliceMRStore(a, b)
	st.details = map[string]clients.PlanSliceSummary{a.ID: a, b.ID: b}
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-x", "unlinked-item", "!1380",
		[]string{"pkg/mills/takeup/reconciler.go"})
	if err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if got != b.ID {
		t.Fatalf("recorded slice = %q, want %q", got, b.ID)
	}
}

// Ambiguity must never guess: no mr_ref is written, and every candidate gets a
// note so whichever slice a human opens tells the truth.
func TestRecordMRRef_AmbiguousLeavesNotesAndWritesNoRef(t *testing.T) {
	a := slice("plan-x#1", "one", "pkg/a/a.go")
	b := slice("plan-x#2", "two", "pkg/b/b.go")
	st := newFakeSliceMRStore(a, b)
	st.details = map[string]clients.PlanSliceSummary{a.ID: a, b.ID: b}
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-x", "unlinked-item", "!1380",
		[]string{"pkg/c/c.go"})
	if !errors.Is(err, ErrSliceAmbiguous) {
		t.Fatalf("err = %v, want ErrSliceAmbiguous", err)
	}
	if got != "" {
		t.Fatalf("slice id = %q, want empty on ambiguity", got)
	}
	if st.updateCalls != 0 {
		t.Fatalf("mr_ref written on an ambiguous attribution: %v", st.mrRefs)
	}
	for _, id := range []string{a.ID, b.ID} {
		notes := st.decisions[id]
		if len(notes) != 1 || !strings.Contains(notes[0], "!1380") {
			t.Fatalf("decisions[%s] = %v, want one note naming the MR", id, notes)
		}
	}
}

// Two slices matching the captured paths equally well is still ambiguity.
func TestRecordMRRef_FileOverlapTieIsAmbiguous(t *testing.T) {
	a := slice("plan-x#1", "one", "pkg/shared/x.go")
	b := slice("plan-x#2", "two", "pkg/shared/x.go")
	st := newFakeSliceMRStore(a, b)
	st.details = map[string]clients.PlanSliceSummary{a.ID: a, b.ID: b}
	rec := NewMRRefRecorder(st, testLogger())

	if _, err := rec.RecordMRRef(context.Background(), "plan-x", "unlinked", "!1380",
		[]string{"pkg/shared/x.go"}); !errors.Is(err, ErrSliceAmbiguous) {
		t.Fatalf("err = %v, want ErrSliceAmbiguous on a tie", err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("mr_ref written on a tie: %v", st.mrRefs)
	}
}

// A retried mr stage must not re-append the same ambiguity note.
func TestRecordMRRef_AmbiguousNoteDedupes(t *testing.T) {
	a := slice("plan-x#1", "one", "pkg/a/a.go")
	b := slice("plan-x#2", "two", "pkg/b/b.go")
	st := newFakeSliceMRStore(a, b)
	priorA := a
	priorA.Decisions = []string{"take-up: MR !1380 opened for backlog item unlinked-item — could not be attributed"}
	st.details = map[string]clients.PlanSliceSummary{a.ID: priorA, b.ID: b}
	rec := NewMRRefRecorder(st, testLogger())

	if _, err := rec.RecordMRRef(context.Background(), "plan-x", "unlinked-item", "!1380", nil); !errors.Is(err, ErrSliceAmbiguous) {
		t.Fatalf("err = %v, want ErrSliceAmbiguous", err)
	}
	if got := st.decisions[a.ID]; len(got) != 0 {
		t.Fatalf("decisions[%s] = %v, want none (already flagged)", a.ID, got)
	}
	if got := st.decisions[b.ID]; len(got) != 1 {
		t.Fatalf("decisions[%s] = %v, want one", b.ID, got)
	}
}

// The mr stage retries and adopts existing MRs, so re-recording an equivalent
// ref (any of the three spellings) must be a no-op write.
func TestRecordMRRef_IdempotentAcrossRefSpellings(t *testing.T) {
	existing := slice("plan-x#1", "one")
	existing.MRRef = "https://gitlab.flexinfer.ai/loom/loom-core/-/merge_requests/1380"
	other := slice("plan-x#2", "two")
	st := newFakeSliceMRStore(existing, other)
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-x", "unlinked", "!1380", nil)
	if err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if got != existing.ID {
		t.Fatalf("slice id = %q, want %q", got, existing.ID)
	}
	if st.updateCalls != 0 {
		t.Fatalf("rewrote an equivalent ref: %v", st.mrRefs)
	}
}

// A slice-less plan is benign: nothing to record, and it must not read as a
// fault to the mr stage.
func TestRecordMRRef_NoSlicesIsBenign(t *testing.T) {
	st := newFakeSliceMRStore()
	rec := NewMRRefRecorder(st, testLogger())

	got, err := rec.RecordMRRef(context.Background(), "plan-x", "item", "!1380", nil)
	if err != nil || got != "" {
		t.Fatalf("RecordMRRef = (%q, %v), want (\"\", nil)", got, err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("unexpected write: %v", st.mrRefs)
	}
}

// Every slice already pointing at a different MR is ambiguous, not a licence
// to overwrite a live ref.
func TestRecordMRRef_AllSlicesRecordedIsAmbiguous(t *testing.T) {
	a := slice("plan-x#1", "one")
	a.MRRef = "!1"
	b := slice("plan-x#2", "two")
	b.MRRef = "!2"
	st := newFakeSliceMRStore(a, b)
	rec := NewMRRefRecorder(st, testLogger())

	if _, err := rec.RecordMRRef(context.Background(), "plan-x", "unlinked", "!1380", nil); !errors.Is(err, ErrSliceAmbiguous) {
		t.Fatalf("err = %v, want ErrSliceAmbiguous", err)
	}
	if st.updateCalls != 0 {
		t.Fatalf("overwrote a live ref: %v", st.mrRefs)
	}
}

func TestRecordMRRef_SurfacesStoreErrors(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		st := newFakeSliceMRStore(slice("plan-x#1", "one"))
		st.listErr = fmt.Errorf("hub unreachable")
		rec := NewMRRefRecorder(st, testLogger())
		if _, err := rec.RecordMRRef(context.Background(), "plan-x", "item", "!1380", nil); err == nil {
			t.Fatal("want error when the slice list fails")
		}
	})
	t.Run("write", func(t *testing.T) {
		target := slice("plan-x#1", "one")
		st := newFakeSliceMRStore(target)
		st.writeErr = fmt.Errorf("tool rejected")
		rec := NewMRRefRecorder(st, testLogger())
		_, err := rec.RecordMRRef(context.Background(), "plan-x",
			intake.BacklogIDForSlice(target.ID), "!1380", nil)
		if err == nil || !strings.Contains(err.Error(), "tool rejected") {
			t.Fatalf("err = %v, want the store error surfaced", err)
		}
	})
}

// argRecordingHub is a HubCaller that keeps the args of every call, so a test
// can pin the wire contract rather than just the Go-level seam.
type argRecordingHub struct {
	responses map[string]string
	calls     []hubToolCall
}

type hubToolCall struct {
	tool string
	args map[string]any
}

func (h *argRecordingHub) CallTool(_ context.Context, _, tool string, args map[string]any) (string, error) {
	h.calls = append(h.calls, hubToolCall{tool: tool, args: args})
	if body, ok := h.responses[tool]; ok {
		return body, nil
	}
	return "ok: true", nil
}

func (h *argRecordingHub) lastCall(tool string) (hubToolCall, bool) {
	for i := len(h.calls) - 1; i >= 0; i-- {
		if h.calls[i].tool == tool {
			return h.calls[i], true
		}
	}
	return hubToolCall{}, false
}

// TestRecordMRRef_RealPlanClientWireContract drives the recorder through a
// REAL *clients.PlanClient over a hub returning production-shaped TOON, so the
// whole path is covered: the tabular slice LIST decode (which drops array
// columns) and the agent_plan_slice_update write carrying mr_ref. A Go-level
// fake alone would not catch a wrong tool name or argument key.
func TestRecordMRRef_RealPlanClientWireContract(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "toon") // force the tabular producer

	const (
		planID  = "plan-stamp-loom-runbook-loom-runbook"
		sliceID = "plan-stamp-loom-runbook-loom-runbook#1"
	)
	hub := &argRecordingHub{responses: map[string]string{
		"agent_plan_slice_list": toonBody(t, map[string]any{
			"ok": true,
			"slices": []map[string]any{{
				"id":      sliceID,
				"plan_id": planID,
				"name":    "author the runbook",
				"phase":   "implementing",
			}},
		}),
		"agent_plan_slice_update": toonBody(t, map[string]any{"ok": true}),
	}}
	rec := NewMRRefRecorder(&clients.PlanClient{Hub: hub, AgentID: "mills:test"}, testLogger())

	got, err := rec.RecordMRRef(context.Background(), planID, intake.BacklogIDForSlice(sliceID), "!1380", nil)
	if err != nil {
		t.Fatalf("RecordMRRef: %v", err)
	}
	if got != sliceID {
		t.Fatalf("recorded slice = %q, want %q", got, sliceID)
	}
	call, ok := hub.lastCall("agent_plan_slice_update")
	if !ok {
		t.Fatalf("agent_plan_slice_update never called; calls = %+v", hub.calls)
	}
	if call.args["slice_id"] != sliceID || call.args["mr_ref"] != "!1380" {
		t.Fatalf("update args = %+v, want slice_id=%s mr_ref=!1380", call.args, sliceID)
	}
	if _, unexpected := call.args["phase"]; unexpected {
		t.Fatalf("update args = %+v, want mr_ref only (phase is take-up's job at merge time)", call.args)
	}
}

func TestRecordMRRef_RejectsIncompleteInput(t *testing.T) {
	rec := NewMRRefRecorder(newFakeSliceMRStore(slice("plan-x#1", "one")), testLogger())
	if _, err := rec.RecordMRRef(context.Background(), "  ", "item", "!1", nil); err == nil {
		t.Fatal("want error for an empty plan_id")
	}
	if _, err := rec.RecordMRRef(context.Background(), "plan-x", "item", "  ", nil); err == nil {
		t.Fatal("want error for an empty mr_ref")
	}
	unconfigured := NewMRRefRecorder(nil, testLogger())
	if _, err := unconfigured.RecordMRRef(context.Background(), "plan-x", "item", "!1", nil); err == nil {
		t.Fatal("want error when the plan store is unwired")
	}
}

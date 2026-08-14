package spin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeEditor is a controllable council.Editor: it returns a fixed EditorOutput
// (or error) and records the brief it was handed so tests can assert the
// roving reached the model.
type fakeEditor struct {
	out       *council.EditorOutput
	err       error
	gotBrief  string
	editCalls int
}

func (f *fakeEditor) Edit(_ context.Context, brief *council.Brief, _ []council.ReviewerOutput) (*council.EditorOutput, error) {
	f.editCalls++
	if brief != nil {
		f.gotBrief = brief.Markdown
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

// fakeAuthor records the DraftPlanInput it received and returns a canned id.
type fakeAuthor struct {
	got      DraftPlanInput
	planID   string
	err      error
	authored bool
}

func (a *fakeAuthor) AuthorDraftPlan(_ context.Context, in DraftPlanInput) (string, error) {
	a.authored = true
	a.got = in
	if a.err != nil {
		return "", a.err
	}
	return a.planID, nil
}

// newSpinner wires a Spinner around one frame + a supplied editor/author.
func newSpinner(frame Frame, ed council.Editor, edErr error, author DraftPlanAuthor) *Spinner {
	return &Spinner{
		Enabled: func() bool { return true },
		Frame: func(name string) (Frame, bool) {
			if name == frame.Name {
				return frame, true
			}
			return Frame{}, false
		},
		NewEditor: func(Frame) (council.Editor, error) {
			if edErr != nil {
				return nil, edErr
			}
			return ed, nil
		},
		Author:          author,
		DefaultPriority: func() string { return "P2" },
	}
}

func slicedOutput() *council.EditorOutput {
	return &council.EditorOutput{
		Backend: "flexinfer",
		Model:   "claude-opus",
		CostUSD: 0.42,
		BacklogProposals: []council.BacklogProposal{{
			Title: "Add retry to importer",
			PlanSlices: []council.PlanSliceSpec{
				{Name: "client", Goal: "retry on 5xx", Files: []string{"pkg/x/client.go"}},
				{Name: "tests", Goal: "cover retry", Files: []string{"pkg/x/client_test.go"}},
			},
		}},
	}
}

// TestSpin_ThreadsObjectiveAndTissue proves the spin carries the editor's
// synthesized objective onto the draft plan and preserves each slice's
// connective tissue (depends_on / interface_contracts / acceptance_criteria)
// through the flatten step into the author input.
func TestSpin_ThreadsObjectiveAndTissue(t *testing.T) {
	out := &council.EditorOutput{
		Backend:   "flexinfer",
		Model:     "claude-opus",
		Objective: "Give a couple one shared source of truth; slice 1 lands the store the rest code against.",
		BacklogProposals: []council.BacklogProposal{{
			Title: "Build FamilyForge",
			PlanSlices: []council.PlanSliceSpec{
				{Name: "schema", Goal: "define the store", Files: []string{"pkg/store/schema.go"},
					InterfaceContracts: "publishes FamilyStore schema", AcceptanceCriteria: "round-trips"},
				{Name: "api", Goal: "wire the API", Files: []string{"pkg/api/api.go"}, DependsOn: []string{"schema"}},
			},
		}},
	}
	ed := &fakeEditor{out: out}
	author := &fakeAuthor{planID: "plan-fam-1"}
	s := newSpinner(Frame{Name: "opus", Model: "claude-opus", Backend: "flexinfer"}, ed, nil, author)

	if _, err := s.Spin(context.Background(), Request{Brief: "co-plan our household", Frame: "opus"}); err != nil {
		t.Fatalf("Spin: %v", err)
	}
	if !author.authored {
		t.Fatal("author never called")
	}
	if author.got.Objective != out.Objective {
		t.Errorf("objective threaded = %q, want %q", author.got.Objective, out.Objective)
	}
	if len(author.got.Slices) != 2 {
		t.Fatalf("slices = %d, want 2", len(author.got.Slices))
	}
	if author.got.Slices[0].InterfaceContracts == "" || author.got.Slices[0].AcceptanceCriteria == "" {
		t.Errorf("schema slice tissue lost: %+v", author.got.Slices[0])
	}
	if got := author.got.Slices[1].DependsOn; len(got) != 1 || got[0] != "schema" {
		t.Errorf("api.depends_on = %v, want [schema] preserved through flatten", got)
	}
}

func TestSpin_HappyPath(t *testing.T) {
	ed := &fakeEditor{out: slicedOutput()}
	author := &fakeAuthor{planID: "plan-spun-abc"}
	s := newSpinner(Frame{Name: "opus", Model: "claude-opus", Backend: "flexinfer"}, ed, nil, author)

	res, err := s.Spin(context.Background(), Request{
		Brief:      "Harden the importer against 5xx",
		Frame:      "opus",
		Priority:   "p0",
		Project:    "services/loom-core",
		Namespace:  "mills/spun",
		RespunFrom: "plan-old-sparse-1",
	})
	if err != nil {
		t.Fatalf("Spin: %v", err)
	}
	if res.PlanID != "plan-spun-abc" {
		t.Errorf("plan id = %q", res.PlanID)
	}
	if res.SliceCount != 2 {
		t.Errorf("slice count = %d, want 2", res.SliceCount)
	}
	if res.Frame != "opus" || res.Model != "claude-opus" || res.Backend != "flexinfer" {
		t.Errorf("audit = %+v", res)
	}
	if res.Priority != "P0" {
		t.Errorf("priority = %q, want normalized P0", res.Priority)
	}
	if res.CostUSD != 0.42 {
		t.Errorf("cost = %v", res.CostUSD)
	}

	// The author saw the decomposition, priority, scope, and audit trail.
	if !author.authored {
		t.Fatal("author never called")
	}
	if got := author.got; got.Priority != "P0" || got.Project != "services/loom-core" ||
		got.Namespace != "mills/spun" || got.Frame != "opus" || got.Model != "claude-opus" {
		t.Errorf("author input = %+v", got)
	}
	if author.got.RespunFrom != "plan-old-sparse-1" {
		t.Errorf("respun_from not threaded to author: %q", author.got.RespunFrom)
	}
	if len(author.got.Slices) != 2 || author.got.Slices[0].Name != "client" {
		t.Errorf("author slices = %+v", author.got.Slices)
	}
	if author.got.Title != "Add retry to importer" {
		t.Errorf("title = %q, want lone-proposal title", author.got.Title)
	}
	if author.got.Brief != "Harden the importer against 5xx" {
		t.Errorf("brief not recorded for audit: %q", author.got.Brief)
	}
	// The roving nudge reached the editor.
	if ed.gotBrief == "" || !contains(ed.gotBrief, "Harden the importer") {
		t.Errorf("editor brief = %q", ed.gotBrief)
	}
}

func TestSpin_Disabled(t *testing.T) {
	author := &fakeAuthor{planID: "x"}
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{out: slicedOutput()}, nil, author)
	s.Enabled = func() bool { return false }
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if author.authored {
		t.Fatal("disabled room must not author")
	}
}

func TestSpin_NilEnabled(t *testing.T) {
	s := &Spinner{}
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "f"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("nil Enabled must fail-closed: %v", err)
	}
}

func TestSpin_InvalidRequest(t *testing.T) {
	author := &fakeAuthor{planID: "x"}
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{out: slicedOutput()}, nil, author)

	if _, err := s.Spin(context.Background(), Request{Brief: "  ", Frame: "opus"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("empty brief err = %v", err)
	}
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: ""}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("empty frame err = %v", err)
	}
	if author.authored {
		t.Fatal("invalid request must not author")
	}
}

func TestSpin_UnknownFrame(t *testing.T) {
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{out: slicedOutput()}, nil, &fakeAuthor{planID: "x"})
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "gpt"}); !errors.Is(err, ErrUnknownFrame) {
		t.Fatalf("err = %v, want ErrUnknownFrame", err)
	}
}

func TestSpin_EditorErrorPropagates(t *testing.T) {
	author := &fakeAuthor{planID: "x"}
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{err: errors.New("boom")}, nil, author)
	_, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"})
	if err == nil || !contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want wrapped editor error", err)
	}
	if author.authored {
		t.Fatal("editor failure must not author")
	}
}

func TestSpin_EmptyOutput(t *testing.T) {
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{out: &council.EditorOutput{Empty: true}}, nil, &fakeAuthor{planID: "x"})
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"}); !errors.Is(err, ErrNoOutput) {
		t.Fatalf("err = %v, want ErrNoOutput", err)
	}
}

func TestSpin_NoProposals(t *testing.T) {
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{out: &council.EditorOutput{Model: "m"}}, nil, &fakeAuthor{planID: "x"})
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"}); !errors.Is(err, ErrNoOutput) {
		t.Fatalf("err = %v, want ErrNoOutput for zero proposals", err)
	}
}

// A proposal the editor left flat (no PlanSlices) becomes a single slice whose
// files come from the proposal's fan-out slices.
func TestSpin_FlatProposalSynthesizesSlice(t *testing.T) {
	out := &council.EditorOutput{
		Model:   "gpt-5.4",
		Backend: "openai-responses",
		BacklogProposals: []council.BacklogProposal{{
			Title:      "Fix flaky test",
			Notes:      "deflake the fsnotify swap test",
			Slices:     []store.Slice{{Name: "s1", Files: []string{"a.go", "b.go"}}},
			PlanSlices: nil,
		}},
	}
	author := &fakeAuthor{planID: "plan-flat"}
	s := newSpinner(Frame{Name: "gpt", Model: "gpt-5.4", Backend: "openai-responses"}, &fakeEditor{out: out}, nil, author)

	res, err := s.Spin(context.Background(), Request{Brief: "deflake it", Frame: "gpt"})
	if err != nil {
		t.Fatalf("Spin: %v", err)
	}
	if res.SliceCount != 1 {
		t.Fatalf("slice count = %d, want 1", res.SliceCount)
	}
	sl := author.got.Slices[0]
	if sl.Name != "Fix flaky test" || sl.Goal != "deflake the fsnotify swap test" {
		t.Errorf("synthesized slice = %+v", sl)
	}
	if len(sl.Files) != 2 || sl.Files[0] != "a.go" {
		t.Errorf("synthesized files = %v", sl.Files)
	}
	if res.Model != "gpt-5.4" || res.Backend != "openai-responses" {
		t.Errorf("actual model/backend = %s/%s", res.Model, res.Backend)
	}
}

// When the request omits priority, the Spinner stamps the policy default.
func TestSpin_DefaultPriority(t *testing.T) {
	author := &fakeAuthor{planID: "x"}
	s := newSpinner(Frame{Name: "opus", Model: "m", Backend: "flexinfer"}, &fakeEditor{out: slicedOutput()}, nil, author)
	res, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"})
	if err != nil {
		t.Fatalf("Spin: %v", err)
	}
	if res.Priority != "P2" || author.got.Priority != "P2" {
		t.Errorf("priority = %q / %q, want default P2", res.Priority, author.got.Priority)
	}
}

// The editor's reported model/backend overrides the frame's declared values so
// the audit records what actually ran (e.g. a fallback editor).
func TestSpin_ActualModelOverridesFrame(t *testing.T) {
	out := slicedOutput()
	out.Model = "flexinfer-local" // fell back off the requested gpt-5.4
	out.Backend = "flexinfer"
	author := &fakeAuthor{planID: "x"}
	s := newSpinner(Frame{Name: "gpt", Model: "gpt-5.4", Backend: "openai-responses"}, &fakeEditor{out: out}, nil, author)
	res, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "gpt"})
	if err != nil {
		t.Fatalf("Spin: %v", err)
	}
	if res.Frame != "gpt" {
		t.Errorf("frame = %q, want requested gpt", res.Frame)
	}
	if res.Model != "flexinfer-local" || res.Backend != "flexinfer" {
		t.Errorf("actual = %s/%s, want the editor's real output", res.Model, res.Backend)
	}
}

func TestSpin_AuthorErrorPropagates(t *testing.T) {
	author := &fakeAuthor{err: errors.New("store down")}
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{out: slicedOutput()}, nil, author)
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"}); err == nil || !contains(err.Error(), "store down") {
		t.Fatalf("err = %v, want wrapped author error", err)
	}
}

func TestSpin_EditorBuildErrorPropagates(t *testing.T) {
	s := newSpinner(Frame{Name: "opus", Model: "m"}, nil, errors.New("no backend"), &fakeAuthor{planID: "x"})
	if _, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"}); err == nil || !contains(err.Error(), "no backend") {
		t.Fatalf("err = %v, want wrapped factory error", err)
	}
}

func TestSpin_SliceCapAndDedup(t *testing.T) {
	// 30 slices proposed; cap is 24. Also feed a duplicate name to prove dedup.
	specs := make([]council.PlanSliceSpec, 0, 31)
	specs = append(specs, council.PlanSliceSpec{Name: "dup", Goal: "g"})
	specs = append(specs, council.PlanSliceSpec{Name: "DUP", Goal: "g2"}) // dedup vs "dup"
	for i := 0; i < 30; i++ {
		specs = append(specs, council.PlanSliceSpec{Name: "slice-" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Goal: "g"})
	}
	out := &council.EditorOutput{Model: "m", BacklogProposals: []council.BacklogProposal{{Title: "big", PlanSlices: specs}}}
	author := &fakeAuthor{planID: "x"}
	s := newSpinner(Frame{Name: "opus", Model: "m"}, &fakeEditor{out: out}, nil, author)
	res, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"})
	if err != nil {
		t.Fatalf("Spin: %v", err)
	}
	if res.SliceCount != maxDraftSlices {
		t.Fatalf("slice count = %d, want cap %d", res.SliceCount, maxDraftSlices)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// ----- Competitive spinning (SpinAll) -----

// multiAuthor is a concurrency-safe DraftPlanAuthor that mints a per-frame
// plan id and records every input, with optional per-frame failures.
type multiAuthor struct {
	mu     sync.Mutex
	got    []DraftPlanInput
	errFor map[string]error // keyed by requested frame name
}

func (a *multiAuthor) AuthorDraftPlan(_ context.Context, in DraftPlanInput) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.errFor[in.Frame]; err != nil {
		return "", err
	}
	a.got = append(a.got, in)
	return "plan-" + in.Frame, nil
}

func (a *multiAuthor) byFrame(frame string) (DraftPlanInput, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, in := range a.got {
		if in.Frame == frame {
			return in, true
		}
	}
	return DraftPlanInput{}, false
}

// newMultiSpinner wires a Spinner over a fixed frame set; editors resolve per
// frame (an entry in edErrs makes that frame's editor factory fail).
func newMultiSpinner(frames []Frame, editors map[string]council.Editor, edErrs map[string]error, author DraftPlanAuthor) *Spinner {
	return &Spinner{
		Enabled: func() bool { return true },
		Frame: func(name string) (Frame, bool) {
			for _, f := range frames {
				if f.Name == name {
					return f, true
				}
			}
			return Frame{}, false
		},
		NewEditor: func(f Frame) (council.Editor, error) {
			if err := edErrs[f.Name]; err != nil {
				return nil, err
			}
			return editors[f.Name], nil
		},
		Author:          author,
		DefaultPriority: func() string { return "P2" },
	}
}

func twoFrames() []Frame {
	return []Frame{
		{Name: "mule", Model: "gpt-5.4", Backend: "openai"},
		{Name: "ring", Model: "gemma4", Backend: "flexinfer"},
	}
}

func TestSpinAll_CompetitiveAuthorsOneDraftPerFrame(t *testing.T) {
	author := &multiAuthor{}
	s := newMultiSpinner(twoFrames(), map[string]council.Editor{
		"mule": &fakeEditor{out: slicedOutput()},
		"ring": &fakeEditor{out: slicedOutput()},
	}, nil, author)

	var mu sync.Mutex
	outcomes := map[string]bool{}
	s.OnSpinDone = func(frame string, ok bool) {
		mu.Lock()
		defer mu.Unlock()
		outcomes[frame] = ok
	}

	cr, err := s.SpinAll(context.Background(), Request{
		Brief:  "Harden the importer",
		Frames: []string{"mule", "ring"},
	})
	if err != nil {
		t.Fatalf("SpinAll: %v", err)
	}
	if len(cr.Results) != 2 || len(cr.Failures) != 0 {
		t.Fatalf("results/failures = %d/%d, want 2/0 (%+v)", len(cr.Results), len(cr.Failures), cr)
	}
	// Results come back in request order with per-frame plan ids.
	if cr.Results[0].Frame != "mule" || cr.Results[0].PlanID != "plan-mule" {
		t.Errorf("results[0] = %+v", cr.Results[0])
	}
	if cr.Results[1].Frame != "ring" || cr.Results[1].PlanID != "plan-ring" {
		t.Errorf("results[1] = %+v", cr.Results[1])
	}
	// Each draft recorded its competitor for the reviewer.
	if in, ok := author.byFrame("mule"); !ok || len(in.Competitors) != 1 || in.Competitors[0] != "ring" {
		t.Errorf("mule competitors = %+v", in.Competitors)
	}
	if in, ok := author.byFrame("ring"); !ok || len(in.Competitors) != 1 || in.Competitors[0] != "mule" {
		t.Errorf("ring competitors = %+v", in.Competitors)
	}
	// The metrics hook saw both attempts succeed.
	if !outcomes["mule"] || !outcomes["ring"] {
		t.Errorf("OnSpinDone outcomes = %+v, want ok for both frames", outcomes)
	}
}

func TestSpinAll_PartialFailureStillSucceeds(t *testing.T) {
	author := &multiAuthor{}
	s := newMultiSpinner(twoFrames(), map[string]council.Editor{
		"mule": &fakeEditor{out: slicedOutput()},
		"ring": &fakeEditor{err: errors.New("model down")},
	}, nil, author)

	cr, err := s.SpinAll(context.Background(), Request{Brief: "b", Frames: []string{"mule", "ring"}})
	if err != nil {
		t.Fatalf("SpinAll: %v (partial success must not error)", err)
	}
	if len(cr.Results) != 1 || cr.Results[0].Frame != "mule" {
		t.Fatalf("results = %+v, want just mule", cr.Results)
	}
	if len(cr.Failures) != 1 || cr.Failures[0].Frame != "ring" || !contains(cr.Failures[0].Error, "model down") {
		t.Fatalf("failures = %+v, want ring's error", cr.Failures)
	}
	// The lone survivor still records who it competed against.
	if in, ok := author.byFrame("mule"); !ok || len(in.Competitors) != 1 || in.Competitors[0] != "ring" {
		t.Errorf("mule competitors = %+v", in.Competitors)
	}
}

func TestSpinAll_AllFramesFailedReturnsError(t *testing.T) {
	s := newMultiSpinner(twoFrames(), nil, map[string]error{
		"mule": errors.New("no backend"),
		"ring": errors.New("no backend"),
	}, &multiAuthor{})

	cr, err := s.SpinAll(context.Background(), Request{Brief: "b", Frames: []string{"mule", "ring"}})
	if err == nil {
		t.Fatal("want error when every frame fails")
	}
	if len(cr.Failures) != 2 {
		t.Errorf("failures = %+v, want both frames reported", cr.Failures)
	}
}

func TestSpinAll_UnknownFrameFailsWholeRequest(t *testing.T) {
	author := &multiAuthor{}
	s := newMultiSpinner(twoFrames(), map[string]council.Editor{
		"mule": &fakeEditor{out: slicedOutput()},
	}, nil, author)

	_, err := s.SpinAll(context.Background(), Request{Brief: "b", Frames: []string{"mule", "off-policy"}})
	if !errors.Is(err, ErrUnknownFrame) {
		t.Fatalf("err = %v, want ErrUnknownFrame", err)
	}
	if len(author.got) != 0 {
		t.Errorf("authored %d drafts before validation failed; want 0 (no spend)", len(author.got))
	}
}

func TestSpinAll_MergesAndDedupesFrameField(t *testing.T) {
	author := &multiAuthor{}
	s := newMultiSpinner(twoFrames(), map[string]council.Editor{
		"mule": &fakeEditor{out: slicedOutput()},
		"ring": &fakeEditor{out: slicedOutput()},
	}, nil, author)

	// Frame merges in first; the duplicate "mule" in Frames dedupes away.
	cr, err := s.SpinAll(context.Background(), Request{Brief: "b", Frame: "mule", Frames: []string{"mule", "ring"}})
	if err != nil {
		t.Fatalf("SpinAll: %v", err)
	}
	if len(cr.Results) != 2 {
		t.Fatalf("results = %+v, want 2 after dedupe", cr.Results)
	}
}

func TestSpinAll_CapsFrameCount(t *testing.T) {
	s := newMultiSpinner(twoFrames(), nil, nil, &multiAuthor{})
	_, err := s.SpinAll(context.Background(), Request{
		Brief:  "b",
		Frames: []string{"a", "b", "c", "d"},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest for %d frames", err, 4)
	}
}

func TestSpinAll_SingleFrameNoCompetitors(t *testing.T) {
	author := &multiAuthor{}
	s := newMultiSpinner(twoFrames(), map[string]council.Editor{
		"mule": &fakeEditor{out: slicedOutput()},
	}, nil, author)

	cr, err := s.SpinAll(context.Background(), Request{Brief: "b", Frames: []string{"mule"}})
	if err != nil {
		t.Fatalf("SpinAll: %v", err)
	}
	if len(cr.Results) != 1 {
		t.Fatalf("results = %+v", cr.Results)
	}
	if in, ok := author.byFrame("mule"); !ok || len(in.Competitors) != 0 {
		t.Errorf("competitors = %+v, want none for a group of one", in.Competitors)
	}
}

// ----- Phase timeouts (bounding a hung model / plan-store write) -----

// blockingEditor / blockingAuthor block until their context is cancelled, then
// return the ctx error — modelling a hung frame or a stalled MCP-hub plan write.
type blockingEditor struct{}

func (blockingEditor) Edit(ctx context.Context, _ *council.Brief, _ []council.ReviewerOutput) (*council.EditorOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type blockingAuthor struct{}

func (blockingAuthor) AuthorDraftPlan(ctx context.Context, _ DraftPlanInput) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestSpin_EditorTimeoutFailsFast(t *testing.T) {
	s := &Spinner{
		Enabled:       func() bool { return true },
		Frame:         func(n string) (Frame, bool) { return Frame{Name: n, Model: "m"}, true },
		NewEditor:     func(Frame) (council.Editor, error) { return blockingEditor{}, nil },
		Author:        &fakeAuthor{planID: "x"},
		EditorTimeout: 40 * time.Millisecond,
	}
	start := time.Now()
	_, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"})
	if !errors.Is(err, ErrEditorTimeout) {
		t.Fatalf("err = %v, want ErrEditorTimeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("editor timeout took %s, expected fast fail near 40ms", time.Since(start))
	}
}

func TestSpin_AuthorTimeoutFailsFast(t *testing.T) {
	s := &Spinner{
		Enabled:       func() bool { return true },
		Frame:         func(n string) (Frame, bool) { return Frame{Name: n, Model: "m"}, true },
		NewEditor:     func(Frame) (council.Editor, error) { return &fakeEditor{out: slicedOutput()}, nil },
		Author:        blockingAuthor{},
		AuthorTimeout: 40 * time.Millisecond,
	}
	start := time.Now()
	_, err := s.Spin(context.Background(), Request{Brief: "b", Frame: "opus"})
	if !errors.Is(err, ErrAuthorTimeout) {
		t.Fatalf("err = %v, want ErrAuthorTimeout (hung plan-store write must fail fast)", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("author timeout took %s, expected fast fail near 40ms", time.Since(start))
	}
}

// An outer-ctx cancellation (client disconnect / request budget) is NOT
// reported as a phase timeout — it's the caller's cancellation, surfaced as-is.
func TestSpin_OuterCancelNotMisreportedAsPhaseTimeout(t *testing.T) {
	s := &Spinner{
		Enabled:   func() bool { return true },
		Frame:     func(n string) (Frame, bool) { return Frame{Name: n, Model: "m"}, true },
		NewEditor: func(Frame) (council.Editor, error) { return blockingEditor{}, nil },
		Author:    &fakeAuthor{planID: "x"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Spin(ctx, Request{Brief: "b", Frame: "opus"})
	if errors.Is(err, ErrEditorTimeout) {
		t.Fatalf("outer cancel misreported as ErrEditorTimeout: %v", err)
	}
	if err == nil {
		t.Fatal("want an error on a cancelled outer context")
	}
}

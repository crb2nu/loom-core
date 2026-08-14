// Package spin implements the Mills "Spinning Room" (Live Beam slice 3 / F2 in
// .loom/brainstorm-mills-steering-preparation-line-2026-07-03.md).
//
// Spinning turns prepared fiber into yarn; different frames trade quality
// against speed and cost. Here the operator picks a frame (a model — e.g.
// claude-opus, gpt-5.4, or a local flexinfer model) from the HUD, hands it a
// brief (the roving), and it spins a draft plan + slices directly into the
// agent-context Plan Store (phase=draft, with a warp-beam priority) for review
// before dispatch. Draft plans are deliberately NOT emitted by the plan-slice
// emitter — the operator advances the draft to planned/in_progress once happy,
// and only then does the beam pick it up.
//
// This package reuses the existing council editor clients (the same
// council.Editor a scheduled council run drives) rather than new inference
// plumbing: an EditorFactory resolves a policy frame to a council.Editor, the
// Spinner runs one Edit pass over the brief, and the editor's structured
// decomposition becomes the draft plan's slices.
//
// Governance line (F2 vs GitOps): the SET of frames is policy (Git-reviewed,
// lives in pkg/mills SpinningRoomPolicy); the frame CHOSEN per spin is
// run-scoped and recorded on the resulting draft plan for audit. The Spinner
// stays decoupled from pkg/mills — the operator adapts a policy CouncilAgent
// into a spin.Frame — so this package is unit-testable with a fake editor and
// a fake author.
package spin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// Sentinel errors the operator handler maps onto HTTP status codes.
var (
	// ErrDisabled means policy.spinning_room.enabled is false (or no Spinner is
	// wired). Maps to 503.
	ErrDisabled = errors.New("spin: spinning room disabled")
	// ErrInvalidRequest means the brief or frame was empty. Maps to 400.
	ErrInvalidRequest = errors.New("spin: invalid request")
	// ErrUnknownFrame means the requested frame is not in policy. Maps to 400 —
	// a caller cannot spin on an arbitrary off-policy model.
	ErrUnknownFrame = errors.New("spin: unknown frame")
	// ErrNoOutput means the editor produced nothing usable (empty output or no
	// decomposable slices). Maps to 502.
	ErrNoOutput = errors.New("spin: frame produced no usable plan")
	// ErrEditorTimeout means the frame's model synthesis did not finish within
	// the editor budget. Maps to 504 — a slow/hung model, not a bad request.
	ErrEditorTimeout = errors.New("spin: editor timed out")
	// ErrAuthorTimeout means the draft-plan write over the MCP hub did not
	// complete within the author budget (a hub / agent_context stall). Maps to
	// 504. Distinct from a generic author error so the handler can tell the
	// operator to retry rather than treating it as a bad brief.
	ErrAuthorTimeout = errors.New("spin: draft plan store timed out (mcp hub)")
)

// Phase budgets: a spin bounds each phase so one hung dependency (a slow model,
// or a stalled plan-store write over the MCP hub) fails fast with a clear error
// instead of holding the HTTP request until the outer deadline. Overridable via
// Spinner.{EditorTimeout,AuthorTimeout}.
const (
	defaultEditorTimeout = 6 * time.Minute
	defaultAuthorTimeout = 45 * time.Second
)

// maxDraftSlices bounds one spun draft plan so a runaway decomposition can't
// author a hundred-slice plan. Excess slices are dropped with a log line
// (no silent truncation).
const maxDraftSlices = 24

// maxCompetitiveFrames bounds one competitive spin: each frame drives a live
// model synthesis, so an uncapped request multiplies spend linearly (the F2
// risk the brainstorm flagged). Three covers "frontier vs local vs diversity"
// without letting a single POST fan out across every policy frame.
const maxCompetitiveFrames = 3

// Frame is one allowed spinning frame: a model + backend the operator may spin
// on, keyed by a human-selectable Name. Mirrors the {name, model, backend}
// shape of pkg/mills CouncilAgent, decoupled so this package doesn't import the
// policy package.
type Frame struct {
	Name    string
	Model   string
	Backend string
}

// Request is one spin: turn Brief into a draft plan on Frame at Priority in a
// Project/Namespace. Brief and one frame (Frame or Frames) are required; the
// rest are optional (Priority falls back to policy default at the author
// layer, Project/Namespace scope the plan for the emitter/take-up reconciler).
//
// Frames enables a COMPETITIVE spin (F2's "spin the same roving on two frames
// and keep the better yarn"): SpinAll runs every listed frame over the same
// brief concurrently and authors one draft plan per frame, each recording its
// competitors, so the operator compares siblings and advances the winner.
// Frame and Frames merge (Frame first, deduped); Spin ignores Frames.
type Request struct {
	Brief     string
	Frame     string
	Frames    []string
	Priority  string
	Project   string
	Namespace string
	// RespunFrom, when set, is the source plan_id this spin redoes ("respin").
	// It's recorded on the resulting draft so the HUD can link the fresh draft
	// back to the plan it supersedes; empty for an ordinary spin.
	RespunFrom string
}

// Result is what a successful spin returns: the new draft plan's id, the
// requested frame, the model/backend that actually produced it (which may
// differ from the frame when a fallback editor kicked in), the slice count, and
// the approximate cost.
type Result struct {
	PlanID     string  `json:"plan_id"`
	Frame      string  `json:"frame"`
	Model      string  `json:"model"`
	Backend    string  `json:"backend"`
	SliceCount int     `json:"slice_count"`
	CostUSD    float64 `json:"cost_usd_approx"`
	Priority   string  `json:"priority,omitempty"`
	Title      string  `json:"title"`
}

// FrameFailure is one frame's failure inside a competitive spin. The error is
// a string (not an error) because the whole struct is the HTTP response body.
type FrameFailure struct {
	Frame string `json:"frame"`
	Error string `json:"error"`
}

// CompetitiveResult is what SpinAll returns: one Result per frame that
// authored a draft, plus per-frame failures for the frames that didn't. A
// partial success is a success — the operator still has yarn to judge.
type CompetitiveResult struct {
	Results  []Result       `json:"results"`
	Failures []FrameFailure `json:"failures,omitempty"`
}

// DraftPlanInput authors a draft (phase=draft) plan with slices + priority. The
// audit fields (Frame/Model/Backend/Brief) are recorded on the plan so a
// reviewer can see which frame spun it and from what roving.
type DraftPlanInput struct {
	Title     string
	Project   string
	Namespace string
	Priority  string
	// Objective is the plan-level end-state + through-line the frame synthesized
	// (EditorOutput.Objective). Recorded on the draft plan so a single spun plan
	// reads as one coherent goal, not just a slice list. Empty when the frame
	// omitted it — the plan renders cleanly without one.
	Objective string
	// RespunFrom is the source plan_id this draft redoes (respin); recorded on
	// the new plan so the HUD links it back to the plan it supersedes. Empty for
	// an ordinary spin.
	RespunFrom string
	Frame      string // requested frame name (audit)
	Model      string // model that actually produced the plan (audit)
	Backend    string // backend that actually produced the plan (audit)
	Brief      string // the roving text (audit)
	// Competitors names the OTHER frames the same brief was spun on in a
	// competitive spin, so a reviewer knows sibling drafts exist to compare
	// before advancing one. Empty for a plain single-frame spin.
	Competitors []string
	Slices      []council.PlanSliceSpec
}

// DraftPlanAuthor writes a draft plan into the Plan Store and returns its id.
// *clients.PlanClient satisfies it (pkg/mills/clients/plan_spin.go); tests use
// a fake. Declared on the consumer side so this package stays decoupled from
// the MCP hub client.
type DraftPlanAuthor interface {
	AuthorDraftPlan(ctx context.Context, in DraftPlanInput) (string, error)
}

// EditorFactory resolves a policy frame to a council.Editor. It returns an
// error when no inference backend can serve the frame (e.g. no flexinfer client
// and no OpenAI key), so a spin fails loudly rather than silently faking.
type EditorFactory func(frame Frame) (council.Editor, error)

// Spinner turns a brief into a draft plan via a policy-chosen frame. All
// collaborators are injected so the whole flow is unit-testable without a live
// operator, MCP hub, or model backend.
type Spinner struct {
	// Enabled gates the whole room (policy.spinning_room.enabled). Required;
	// a nil Enabled is treated as disabled (fail-closed).
	Enabled func() bool
	// Frame resolves an allowed frame by name. ok=false => ErrUnknownFrame.
	Frame func(name string) (Frame, bool)
	// NewEditor builds the council editor for a resolved frame.
	NewEditor EditorFactory
	// Author persists the resulting draft plan.
	Author DraftPlanAuthor
	// DefaultPriority stamps the plan when the request omits one. Optional;
	// the author layer applies its own fallback when empty.
	DefaultPriority func() string
	// EditorTimeout bounds one frame's model synthesis. 0 ⇒ defaultEditorTimeout.
	EditorTimeout time.Duration
	// AuthorTimeout bounds the draft-plan write over the MCP hub. 0 ⇒
	// defaultAuthorTimeout. Keeps a hung hub write from holding the request.
	AuthorTimeout time.Duration
	// OnSpinDone, when set, observes every frame spin attempt that reached
	// the editor (ok = a draft was authored). Pre-spend rejections (disabled
	// room, unknown frame, empty brief) are NOT reported, so a metrics hook
	// never mints label values from arbitrary request strings.
	OnSpinDone func(frame string, ok bool)
	// Now is injectable for deterministic tests. Defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

func (s *Spinner) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Spinner) editorTimeout() time.Duration {
	if s.EditorTimeout > 0 {
		return s.EditorTimeout
	}
	return defaultEditorTimeout
}

func (s *Spinner) authorTimeout() time.Duration {
	if s.AuthorTimeout > 0 {
		return s.AuthorTimeout
	}
	return defaultAuthorTimeout
}

func (s *Spinner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// Spin runs one single-frame spin end-to-end: gate on policy, resolve + build
// the frame's editor, run a single Edit pass over the brief, decompose the
// output into draft slices, and author a phase=draft plan carrying the
// priority + audit trail. Returns a typed sentinel error (see the Err* vars)
// the handler maps to a status code, or the wrapped editor/author error
// otherwise. Ignores req.Frames — competitive spins go through SpinAll.
func (s *Spinner) Spin(ctx context.Context, req Request) (Result, error) {
	if err := s.gate(); err != nil {
		return Result{}, err
	}

	brief := strings.TrimSpace(req.Brief)
	frameName := strings.TrimSpace(req.Frame)
	if brief == "" {
		return Result{}, fmt.Errorf("%w: brief is required", ErrInvalidRequest)
	}
	if frameName == "" {
		return Result{}, fmt.Errorf("%w: frame is required", ErrInvalidRequest)
	}

	frame, ok := s.Frame(frameName)
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownFrame, frameName)
	}
	return s.spinOne(ctx, brief, req, frame, nil)
}

// SpinAll is the competitive entry point (F2's "spin the same roving on two
// frames and keep the better yarn"): it merges req.Frame + req.Frames into a
// deduped, order-preserving frame list, resolves EVERY frame against policy
// before spending anything, then spins each frame over the same brief
// concurrently, authoring one draft plan per frame. Each draft records its
// competitors so the reviewer knows sibling drafts exist.
//
// Returns a nil error whenever at least one frame authored a draft (per-frame
// failures ride along in CompetitiveResult.Failures); returns the first
// frame's error only when every frame failed, so the handler's sentinel
// mapping keeps working for total failures.
func (s *Spinner) SpinAll(ctx context.Context, req Request) (CompetitiveResult, error) {
	if err := s.gate(); err != nil {
		return CompetitiveResult{}, err
	}

	brief := strings.TrimSpace(req.Brief)
	if brief == "" {
		return CompetitiveResult{}, fmt.Errorf("%w: brief is required", ErrInvalidRequest)
	}
	names := mergeFrameNames(req)
	if len(names) == 0 {
		return CompetitiveResult{}, fmt.Errorf("%w: frame is required", ErrInvalidRequest)
	}
	if len(names) > maxCompetitiveFrames {
		return CompetitiveResult{}, fmt.Errorf("%w: at most %d frames per competitive spin, got %d",
			ErrInvalidRequest, maxCompetitiveFrames, len(names))
	}

	// Resolve everything up front: an off-policy frame fails the whole request
	// before any model spend, mirroring Spin's fail-loud stance.
	frames := make([]Frame, 0, len(names))
	for _, name := range names {
		frame, ok := s.Frame(name)
		if !ok {
			return CompetitiveResult{}, fmt.Errorf("%w: %q", ErrUnknownFrame, name)
		}
		frames = append(frames, frame)
	}

	results := make([]*Result, len(frames))
	errs := make([]error, len(frames))
	var wg sync.WaitGroup
	for i, frame := range frames {
		wg.Add(1)
		go func(i int, frame Frame) {
			defer wg.Done()
			res, err := s.spinOne(ctx, brief, req, frame, competitorsOf(names, frame.Name))
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = &res
		}(i, frame)
	}
	wg.Wait()

	var out CompetitiveResult
	for i, frame := range frames {
		switch {
		case results[i] != nil:
			out.Results = append(out.Results, *results[i])
		case errs[i] != nil:
			out.Failures = append(out.Failures, FrameFailure{Frame: frame.Name, Error: errs[i].Error()})
		}
	}
	if len(out.Results) == 0 {
		for _, err := range errs {
			if err != nil {
				return out, err
			}
		}
		return out, fmt.Errorf("%w: no frame produced a draft", ErrNoOutput)
	}
	return out, nil
}

// gate is the shared entry check: room enabled + collaborators wired.
func (s *Spinner) gate() error {
	if s == nil || s.Enabled == nil || !s.Enabled() {
		return ErrDisabled
	}
	if s.Frame == nil || s.NewEditor == nil || s.Author == nil {
		return fmt.Errorf("spin: spinner not fully configured")
	}
	return nil
}

// mergeFrameNames merges req.Frame (first) + req.Frames into a trimmed,
// deduped, order-preserving name list.
func mergeFrameNames(req Request) []string {
	seen := map[string]bool{}
	var names []string
	for _, name := range append([]string{req.Frame}, req.Frames...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// competitorsOf returns every name in the group except self — the sibling
// audit trail stamped on each competitive draft. Nil for a group of one.
func competitorsOf(names []string, self string) []string {
	if len(names) < 2 {
		return nil
	}
	out := make([]string, 0, len(names)-1)
	for _, n := range names {
		if n != self {
			out = append(out, n)
		}
	}
	return out
}

// spinOne runs one already-resolved frame and reports the attempt's outcome
// to OnSpinDone (the operator's metrics hook).
func (s *Spinner) spinOne(ctx context.Context, brief string, req Request, frame Frame, competitors []string) (Result, error) {
	res, err := s.spinFrame(ctx, brief, req, frame, competitors)
	if s.OnSpinDone != nil {
		s.OnSpinDone(frame.Name, err == nil)
	}
	return res, err
}

// spinFrame runs one already-resolved frame over a validated brief: build the
// frame's editor, run a single Edit pass, decompose into draft slices, and
// author a phase=draft plan carrying the priority + audit trail (including
// competitors when part of a competitive spin).
func (s *Spinner) spinFrame(ctx context.Context, brief string, req Request, frame Frame, competitors []string) (Result, error) {
	editor, err := s.NewEditor(frame)
	if err != nil {
		return Result{}, fmt.Errorf("spin: build editor for frame %q: %w", frame.Name, err)
	}
	if editor == nil {
		return Result{}, fmt.Errorf("spin: no editor for frame %q", frame.Name)
	}

	editStart := s.now()
	editCtx, cancelEdit := context.WithTimeout(ctx, s.editorTimeout())
	out, err := editor.Edit(editCtx, &council.Brief{Markdown: buildRoving(brief)}, nil)
	cancelEdit()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return Result{}, fmt.Errorf("%w: frame %q did not finish within %s", ErrEditorTimeout, frame.Name, s.editorTimeout())
		}
		return Result{}, fmt.Errorf("spin: frame %q edit: %w", frame.Name, err)
	}
	s.logger().Info("spin editor produced output",
		"frame", frame.Name, "empty", out == nil || out.Empty,
		"edit_seconds", int(s.now().Sub(editStart).Seconds()))
	if out == nil || out.Empty {
		return Result{}, fmt.Errorf("%w (frame %q)", ErrNoOutput, frame.Name)
	}

	slices := s.draftSlices(out)
	if len(slices) == 0 {
		return Result{}, fmt.Errorf("%w: no decomposable slices (frame %q)", ErrNoOutput, frame.Name)
	}

	priority := strings.ToUpper(strings.TrimSpace(req.Priority))
	if priority == "" && s.DefaultPriority != nil {
		priority = strings.ToUpper(strings.TrimSpace(s.DefaultPriority()))
	}

	model := firstNonEmpty(out.Model, frame.Model)
	backend := firstNonEmpty(out.Backend, frame.Backend)
	title := draftTitle(brief, out)

	// Bound the plan-store write on its own budget: it goes over the MCP hub to
	// agent_context, which can stall independently of the model call. A stall
	// here fails fast with ErrAuthorTimeout instead of holding the request for
	// the whole outer budget (the live failure mode: a hung agent_context write
	// hanging every spin until the server write deadline dropped the response).
	authStart := s.now()
	authCtx, cancelAuth := context.WithTimeout(ctx, s.authorTimeout())
	planID, err := s.Author.AuthorDraftPlan(authCtx, DraftPlanInput{
		Title:       title,
		Project:     strings.TrimSpace(req.Project),
		Namespace:   strings.TrimSpace(req.Namespace),
		Priority:    priority,
		Objective:   strings.TrimSpace(out.Objective),
		RespunFrom:  strings.TrimSpace(req.RespunFrom),
		Frame:       frame.Name,
		Model:       model,
		Backend:     backend,
		Brief:       brief,
		Competitors: competitors,
		Slices:      slices,
	})
	cancelAuth()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return Result{}, fmt.Errorf("%w: no response within %s (retry)", ErrAuthorTimeout, s.authorTimeout())
		}
		return Result{}, fmt.Errorf("spin: author draft plan: %w", err)
	}

	s.logger().Info("spinning room spun draft plan",
		"plan_id", planID, "frame", frame.Name, "model", model, "backend", backend,
		"slices", len(slices), "priority", priority, "namespace", req.Namespace,
		"competitors", strings.Join(competitors, ","),
		"author_seconds", int(s.now().Sub(authStart).Seconds()),
		"spun_at", s.now().Format(time.RFC3339))

	return Result{
		PlanID:     planID,
		Frame:      frame.Name,
		Model:      model,
		Backend:    backend,
		SliceCount: len(slices),
		CostUSD:    out.CostUSD,
		Priority:   priority,
		Title:      title,
	}, nil
}

// draftSlices flattens the editor's backlog proposals into a deduped slice set.
// A proposal the editor decomposed carries PlanSlices verbatim; a proposal it
// left flat becomes a single slice (name = proposal title, files from its
// fan-out slices). Deduped by lower-cased name; capped at maxDraftSlices with a
// log line so a runaway decomposition never silently balloons the draft.
func (s *Spinner) draftSlices(out *council.EditorOutput) []council.PlanSliceSpec {
	var slices []council.PlanSliceSpec
	seen := map[string]bool{}
	add := func(sp council.PlanSliceSpec) bool {
		name := strings.TrimSpace(sp.Name)
		if name == "" {
			return false
		}
		key := strings.ToLower(name)
		if seen[key] {
			return false
		}
		seen[key] = true
		sp.Name = name
		slices = append(slices, sp)
		return true
	}

	dropped := 0
	for _, p := range out.BacklogProposals {
		specs := p.PlanSlices
		if len(specs) == 0 {
			// Flat proposal → one slice describing the whole item.
			specs = []council.PlanSliceSpec{{
				Name:  strings.TrimSpace(p.Title),
				Goal:  firstNonEmpty(strings.TrimSpace(p.Notes), strings.TrimSpace(p.SpecAnchor), strings.TrimSpace(p.Title)),
				Files: proposalFiles(p),
			}}
		}
		for _, sp := range specs {
			if len(slices) >= maxDraftSlices {
				dropped++
				continue
			}
			add(sp)
		}
	}
	if dropped > 0 {
		s.logger().Warn("spinning room capped draft slices",
			"kept", len(slices), "dropped", dropped, "cap", maxDraftSlices)
	}
	return slices
}

// proposalFiles flattens the file sets of a flat proposal's fan-out slices into
// one deduped list so an emitted slice still carries a concrete scope.
func proposalFiles(p council.BacklogProposal) []string {
	seen := map[string]bool{}
	var files []string
	for _, sl := range p.Slices {
		for _, f := range sl.Files {
			f = strings.TrimSpace(f)
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			files = append(files, f)
		}
	}
	return files
}

// draftTitle picks a plan title: a lone proposal's own title when the editor
// produced exactly one, else a title derived from the brief's first line.
func draftTitle(brief string, out *council.EditorOutput) string {
	if len(out.BacklogProposals) == 1 {
		if t := strings.TrimSpace(out.BacklogProposals[0].Title); t != "" {
			return truncate(t, 120)
		}
	}
	first := brief
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(strings.TrimLeft(first, "#- "))
	if first == "" {
		return "Spun draft plan"
	}
	return truncate(first, 120)
}

// buildRoving wraps the operator's raw brief in a light planning header that
// nudges the editor to decompose into independently-shippable slices. The
// editor consumes brief.Markdown verbatim.
func buildRoving(brief string) string {
	var b strings.Builder
	b.WriteString("# Spinning Room brief (roving)\n\n")
	b.WriteString(brief)
	b.WriteString("\n\n---\n")
	b.WriteString("Produce a draft implementation plan for the brief above, decomposed into " +
		"independently-shippable slices. Each slice must name the files it touches and state a " +
		"concrete goal, so each can ship as its own merge request.\n")
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}

package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// runnerEnv wires every council component with deterministic fakes so
// the test exercises the same flow the operator's live wiring does.
type runnerEnv struct {
	store    *store.Store
	policy   *mills.PolicyManager
	repoRoot string
	now      time.Time
	runner   *Runner
}

const validPolicyYAML = `
version: 1
budgets:
  council:  { max_usd_per_run: 5, max_usd_per_day: 50 }
  pipeline: { max_usd_per_run: 5, max_usd_per_day: 50 }
council:
  schedule_cron: "0 5 * * *"
  artifacts_branch: "council/{date}"
  artifacts_merge_strategy: "fast-merge-loom-only"
  ensemble:
    editor: { model: claude-opus, backend: claude-code }
    reviewers:
      - { name: security,  model: gpt-5-codex, backend: codex }
      - { name: tech-debt, model: qwen3.5-9b,  backend: flexinfer }
pipeline:
  default_template: mills-default-pipeline
  retry: { max_attempts: 3, cooldown_seconds: 60 }
human_handoff:
  on_escalation_create_handoff: true
  on_escalation_create_issue: true
`

// Regression: the production 35B reviewer backend can queue parallel lens
// calls beyond the old 30-second deadline. The deadline must cover at least
// two such slots so a three-lens dispatch can still reach majority quorum.
func TestCouncilReviewerTimeoutCoversQueuedLocalInference(t *testing.T) {
	if councilReviewerTimeout < 90*time.Second {
		t.Fatalf("councilReviewerTimeout = %s, want at least 90s", councilReviewerTimeout)
	}
	if councilReviewerTimeout > 2*time.Minute {
		t.Fatalf("councilReviewerTimeout = %s, want a bounded deadline", councilReviewerTimeout)
	}
}

// runnerSampleTitles supplies distinct multi-token titles so the slice
// 6.2 dedup logic doesn't collapse the fixture (single-char
// disambiguators all normalize to the same token set after stopwords +
// single-char drops).
var runnerSampleTitles = []string{
	"exercise reconciler pipeline starter",
	"exercise gate library expansion",
	"exercise eval loop attribution",
	"exercise integrator fan-out",
	"exercise escalation runbook",
	"exercise weaver subagent dispatch",
}

func sampleProposals(n int) []council.BacklogProposal {
	out := make([]council.BacklogProposal, n)
	for i := 0; i < n; i++ {
		title := "exercise sample council " + string(rune('A'+i))
		if i < len(runnerSampleTitles) {
			title = runnerSampleTitles[i]
		}
		out[i] = council.BacklogProposal{
			Title:    title,
			Labels:   []string{"debt"},
			Priority: store.P2,
			Slices: []store.Slice{{
				Name:  "core",
				Files: []string{"pkg/foo/bar.go"},
			}},
			Success: store.SuccessCriteria{Tests: []string{"go test ./pkg/foo/..."}},
			Budget:  store.Budget{MaxCostUSD: 1.0},
		}
	}
	return out
}

func newRunnerEnv(t *testing.T, proposals []council.BacklogProposal) *runnerEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "h.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(validPolicyYAML), 0o644); err != nil {
		t.Fatalf("policy write: %v", err)
	}
	pm, err := mills.NewPolicyManager(context.Background(), policyPath, mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		t.Fatalf("policy mgr: %v", err)
	}
	t.Cleanup(func() { _ = pm.Close() })

	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".loom"), 0o755); err != nil {
		t.Fatalf("mkdir loom: %v", err)
	}
	// Seed a roadmap intent so the eval Loop A judge's roadmap_alignment
	// criterion has something to match.
	if err := st.Roadmap.Upsert(context.Background(), &store.RoadmapIntent{
		Theme: "Tier 1", Priority: 1, Summary: "exercise the council",
		LastSeenInRoadmapSHA: "test",
	}); err != nil {
		t.Fatalf("seed roadmap: %v", err)
	}

	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	dispatcher := &council.Dispatcher{Reviewers: map[string]council.Reviewer{
		"security":  &council.FakeReviewer{Notes: "audit ok", CostUSD: 0.10},
		"tech-debt": &council.FakeReviewer{Notes: "lean", CostUSD: 0.05},
	}}
	editor := &council.FakeEditor{
		Backend: "claude-code", Model: "claude-opus", CostUSD: 0.42, Notes: "fake",
		BacklogCreated: len(proposals),
	}
	wrappedEditor := &editorWithProposals{base: editor, proposals: proposals}

	writer := &council.ArtifactWriter{RepoRoot: repo, Now: func() time.Time { return now }}
	mutator := &council.BacklogMutator{Store: st, Now: func() time.Time { return now }}
	judge := &eval.Judge{Criteria: eval.DefaultRubric(&eval.FakeLLMJudge{Score: 1.0})}

	r := &Runner{
		Store:     st,
		Policy:    pm,
		Budget:    mills.NewBudget(pm, mills.NewStoreBudgetReader(st)),
		Reviewers: dispatcher,
		Editor:    wrappedEditor,
		Writer:    writer,
		Mutator:   mutator,
		Judge:     judge,
		RepoRoot:  repo,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return now },
	}
	return &runnerEnv{store: st, policy: pm, repoRoot: repo, now: now, runner: r}
}

// editorWithProposals decorates a base Editor so each Edit call adds the
// caller-supplied BacklogProposals to the output. Keeps proposal
// generation out of council.FakeEditor's contract.
type editorWithProposals struct {
	base      council.Editor
	proposals []council.BacklogProposal
}

func (e *editorWithProposals) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	out, err := e.base.Edit(ctx, brief, reviews)
	if err != nil {
		return nil, err
	}
	out.BacklogProposals = append(out.BacklogProposals, e.proposals...)
	return out, nil
}

// Revise delegates to the wrapped editor when it implements Reviser so
// the debate-mode runner test can drive the same fixture as the
// single-pass tests. Adds the same canned proposals onto the revised
// output (matching Edit's behaviour), so the mutator stage downstream
// sees a non-empty BacklogProposals on the final revise.
func (e *editorWithProposals) Revise(ctx context.Context, prior *council.EditorOutput, critiques []council.ReviewerOutput, focusAreas []string) (*council.EditorOutput, error) {
	rv, ok := e.base.(council.Reviser)
	if !ok {
		// Fall back to a propose-only impl: reuse Edit so tests
		// covering not-Reviser editors still resolve.
		return e.base.Edit(ctx, &council.Brief{Markdown: "(revise)"}, critiques)
	}
	out, err := rv.Revise(ctx, prior, critiques, focusAreas)
	if err != nil {
		return nil, err
	}
	out.BacklogProposals = append(out.BacklogProposals, e.proposals...)
	return out, nil
}

// ----- happy path -----

func TestRun_HappyPathPersistsEverything(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(2))
	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual,
		Reason:  "test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Verdict.Partial {
		t.Errorf("happy path should pass judge, got verdict=%+v", res.Verdict)
	}
	if len(res.Mutation.CreatedItems) != 2 {
		t.Errorf("created items: got %d want 2", len(res.Mutation.CreatedItems))
	}
	if len(res.Write.ArtifactRefs) != 3 {
		t.Errorf("artifact refs: got %d want 3", len(res.Write.ArtifactRefs))
	}

	// Council run row persisted with the populated deltas.
	got, err := env.store.Council.Get(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("council get: %v", err)
	}
	if got.Outcome != store.CouncilOutcomeSuccess {
		t.Errorf("outcome: %v", got.Outcome)
	}
	if len(got.BacklogDeltas.Created) != 2 {
		t.Errorf("persisted backlog deltas: %+v", got.BacklogDeltas)
	}
	if got.Notes != "test; fake" {
		t.Errorf("notes: %q", got.Notes)
	}
	if total := got.CostFrontierUSD + got.CostLocalUSD; abs(total-(0.42+0.10+0.05)) > 1e-6 {
		t.Errorf("persisted cost: got %v want reviewer + editor total", total)
	}

	// Eval row recorded.
	scores, _ := env.store.Eval.LatestPerSubject(context.Background(),
		store.EvalSubjectCouncilRun, res.RunID)
	if len(scores) != 1 {
		t.Errorf("expected 1 eval score, got %d", len(scores))
	}

	// Files materialised on disk.
	for _, ref := range res.Write.ArtifactRefs {
		if _, err := os.Stat(filepath.Join(env.repoRoot, ref.Path)); err != nil {
			t.Errorf("artifact missing: %s (%v)", ref.Path, err)
		}
	}
	for _, p := range res.Mutation.CreatedYAMLPath {
		if _, err := os.Stat(filepath.Join(env.repoRoot, p)); err != nil {
			t.Errorf("backlog yaml missing: %s (%v)", p, err)
		}
	}

	// Cost is reviewer cost + editor cost.
	want := 0.42 + 0.10 + 0.05
	if abs(res.CostUSDApprox-want) > 1e-6 {
		t.Errorf("CostUSDApprox: got %v want %v", res.CostUSDApprox, want)
	}
}

// ----- partial path -----

func TestRun_PartialSkipsBacklog(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(2))
	// Swap the judge for one that returns a hard 0 to force partial.
	env.runner.Judge = &eval.Judge{
		Criteria: []eval.Criterion{alwaysZeroCriterion{}},
	}
	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerCron,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Verdict.Partial {
		t.Errorf("expected partial verdict")
	}
	if !res.Mutation.Skipped {
		t.Errorf("mutator should have skipped, got %+v", res.Mutation)
	}
	if res.Mutation.TotalProposed != 2 {
		t.Errorf("TotalProposed should be populated for audit: %d", res.Mutation.TotalProposed)
	}

	q, _ := env.store.Backlog.ListByState(context.Background(), store.BacklogQueued)
	if len(q) != 0 {
		t.Errorf("backlog should be untouched on partial run: %d items", len(q))
	}

	// Persisted council run reflects partial.
	got, _ := env.store.Council.Get(context.Background(), res.RunID)
	if got.Outcome != store.CouncilOutcomePartial {
		t.Errorf("outcome should be partial, got %v", got.Outcome)
	}
}

func TestRun_ReviewerQuorumFailureForcesPartialAndSkipsBacklog(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(2))
	providerErr := errors.New("review provider unavailable")
	env.runner.Reviewers.Reviewers["security"] = &council.FakeReviewer{ReturnErr: providerErr}
	env.runner.Reviewers.Reviewers["tech-debt"] = &council.FakeReviewer{ReturnErr: providerErr}

	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerCron,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Verdict.Partial {
		t.Fatalf("verdict = %+v, want quorum failure to force partial", res.Verdict)
	}
	if !res.Mutation.Skipped || res.Mutation.SkipReason != "reviewer quorum failure; mutations dropped" {
		t.Fatalf("mutation = %+v, want skipped partial mutation", res.Mutation)
	}
	if len(res.Mutation.CreatedItems) != 0 {
		t.Fatalf("created items = %d, want none without reviewer quorum", len(res.Mutation.CreatedItems))
	}

	got, err := env.store.Council.Get(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("council get: %v", err)
	}
	if got.Outcome != store.CouncilOutcomePartial {
		t.Fatalf("outcome = %v, want partial", got.Outcome)
	}
	if !strings.Contains(got.Notes, "reviewer quorum failure") {
		t.Fatalf("notes = %q, want reviewer quorum failure", got.Notes)
	}

	q, err := env.store.Backlog.ListByState(context.Background(), store.BacklogQueued)
	if err != nil {
		t.Fatalf("list queued backlog: %v", err)
	}
	if len(q) != 0 {
		t.Fatalf("queued backlog = %d, want no mutation without quorum", len(q))
	}
}

// TestRun_EmptyEditorMarksOutcomeError pins the regression where an
// editor that returned no usable content used to produce a council_run
// row with outcome=success and a "No model output returned." placeholder
// document. After the empty-flag wiring the runner demotes the run to
// outcome=error and annotates the notes so operators see the failure on
// the Council tab.
func TestRun_EmptyEditorMarksOutcomeError(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	// Decorate the existing editor so it stamps Empty=true on the
	// output. Everything else stays normal so the writer still produces
	// a council_run row — we just want to verify the outcome demotion.
	env.runner.Editor = &emptyMarkingEditor{base: env.runner.Editor}
	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerCron,
		Reason:  "scheduled",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := env.store.Council.Get(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("Council.Get: %v", err)
	}
	if got.Outcome != store.CouncilOutcomeError {
		t.Errorf("Outcome = %v, want error", got.Outcome)
	}
	if !strings.Contains(got.Notes, "empty response") {
		t.Errorf("Notes = %q, want it to mention empty response", got.Notes)
	}
}

// emptyMarkingEditor decorates a base Editor to set Empty=true on every
// returned output, simulating a model that took the API call but produced
// no usable text.
type emptyMarkingEditor struct{ base council.Editor }

func (e *emptyMarkingEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	out, err := e.base.Edit(ctx, brief, reviews)
	if err != nil {
		return nil, err
	}
	out.Empty = true
	return out, nil
}

// ----- dryrun path -----

func TestRun_DryrunWritesScratchDir(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual,
		Dryrun:  true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Canonical store untouched: no council_runs, no backlog_items, no eval row.
	q, _ := env.store.Backlog.ListByState(context.Background(), store.BacklogQueued)
	if len(q) != 0 {
		t.Errorf("dryrun should not write backlog, got %d items", len(q))
	}
	if _, err := env.store.Council.Get(context.Background(), res.RunID); err == nil {
		t.Errorf("dryrun should not persist council_runs row")
	}

	// Scratch dir under .loom/dryrun/<runID>/ contains the artifacts.
	scratch := filepath.Join(env.repoRoot, ".loom", "dryrun", res.RunID, ".loom")
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatalf("scratch dir missing: %v", err)
	}
	if len(entries) == 0 {
		t.Errorf("scratch dir empty: %s", scratch)
	}
	mdCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mdCount++
		}
	}
	if mdCount != 3 {
		t.Errorf("expected 3 markdown artifacts in scratch, got %d", mdCount)
	}
}

// ----- guards -----

func TestRun_PolicyDisabledRejects(t *testing.T) {
	env := newRunnerEnv(t, nil)
	off := false
	env.policy.Current().Enabled = &off
	_, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerManual})
	if err == nil {
		t.Errorf("expected error when policy disabled")
	}
}

func TestRun_EmptyTriggerDefaultsToManual(t *testing.T) {
	env := newRunnerEnv(t, nil)

	res, err := env.runner.Run(context.Background(), RunInput{})
	if err != nil {
		t.Fatalf("run with omitted trigger: %v", err)
	}
	got, err := env.store.Council.Get(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("get council run: %v", err)
	}
	if got.Trigger != store.CouncilTriggerManual {
		t.Fatalf("trigger = %q, want %q", got.Trigger, store.CouncilTriggerManual)
	}
}

func TestRun_MissingDepsErrors(t *testing.T) {
	if _, err := (&Runner{}).Run(context.Background(), RunInput{}); err == nil {
		t.Error("expected error with no config")
	}
}

// ----- admission split (Admit / Execute) -----

// The async trigger returns 202 with a pollable run id, which is only honest if
// Admit's council_runs row is durable BEFORE Execute does any long work.
func TestAdmit_CommitsRowBeforeExecute(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	adm, err := env.runner.Admit(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual,
		Reason:  "admission test",
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if adm == nil || adm.RunID == "" || adm.Run == nil {
		t.Fatalf("admission = %+v, want a committed run", adm)
	}

	// Durable before Execute is ever called — this is what GET
	// /api/mills/council/runs/{id} resolves right after the 202.
	got, err := env.store.Council.Get(context.Background(), adm.RunID)
	if err != nil {
		t.Fatalf("get admitted council run: %v", err)
	}
	if got.Outcome != store.CouncilOutcomeRunning || got.EndedAt != nil {
		t.Fatalf("admitted run = %+v, want provisional running row", got)
	}
	if got.Trigger != store.CouncilTriggerManual {
		t.Fatalf("trigger = %q, want %q", got.Trigger, store.CouncilTriggerManual)
	}

	res, err := env.runner.Execute(context.Background(), adm)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.RunID != adm.RunID {
		t.Fatalf("result run id = %q, want the admitted id %q", res.RunID, adm.RunID)
	}
	final, err := env.store.Council.Get(context.Background(), adm.RunID)
	if err != nil {
		t.Fatalf("get executed council run: %v", err)
	}
	if final.Outcome != store.CouncilOutcomeSuccess || final.EndedAt == nil {
		t.Fatalf("executed run = %+v, want terminal success", final)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

// A denied admission must leave no trace: the provisional insert that takes the
// SQLite writer slot is rolled back before the denial commits, so the minted id
// stays unresolvable rather than becoming a permanent 'running' ghost.
func TestAdmit_BudgetDeniedDoesNotCommit(t *testing.T) {
	env := newRunnerEnv(t, nil)
	// Concurrency is enforced only by the admission transaction (the read-only
	// preflight has no view of active runs), so this drives the store-side
	// CouncilBudgetExceededError rather than the preflight denial.
	env.policy.Current().Budgets.Council.MaxConcurrentRuns = 1

	held, err := env.runner.Admit(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}

	denied, err := env.runner.Admit(context.Background(), RunInput{Trigger: store.CouncilTriggerManual})
	if err == nil {
		t.Fatal("second admit succeeded, want concurrency denial")
	}
	var exceeded *store.CouncilBudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("admit error = %v, want *store.CouncilBudgetExceededError", err)
	}
	if denied == nil || denied.RunID == "" {
		t.Fatalf("denied admission = %+v, want the minted run id back", denied)
	}
	if denied.Run != nil {
		t.Fatalf("denied admission committed a run: %+v", denied.Run)
	}
	if _, getErr := env.store.Council.Get(context.Background(), denied.RunID); !errors.Is(getErr, store.ErrNotFound) {
		t.Fatalf("denied run lookup = %v, want ErrNotFound", getErr)
	}

	// Drain the held admission so the env ends with no active reservation.
	if _, err := env.runner.Execute(context.Background(), held); err != nil {
		t.Fatalf("execute held admission: %v", err)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_BudgetDeniedBeforeParticipantCalls(t *testing.T) {
	env := newRunnerEnv(t, nil)
	ended := env.now.Add(-30 * time.Minute)
	if err := env.store.Council.Put(context.Background(), &store.CouncilRun{
		ID:              "COUNCIL-PRIOR-SPEND",
		Trigger:         store.CouncilTriggerCron,
		StartedAt:       env.now.Add(-time.Hour),
		EndedAt:         &ended,
		Outcome:         store.CouncilOutcomeError,
		CostFrontierUSD: 50,
	}); err != nil {
		t.Fatalf("seed prior spend: %v", err)
	}
	reviewer := &countingReviewer{}
	env.runner.Reviewers = &council.Dispatcher{Reviewers: map[string]council.Reviewer{
		"security": reviewer, "tech-debt": reviewer,
	}}

	_, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("run error = %v, want budget denial", err)
	}
	if got := reviewer.calls.Load(); got != 0 {
		t.Fatalf("reviewer calls = %d, want 0 before denied admission", got)
	}
}

func TestRun_DryrunBudgetDeniedBeforeParticipantCalls(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Budget.Now = func() time.Time { return env.now }
	ended := env.now.Add(-30 * time.Minute)
	if err := env.store.Council.Put(context.Background(), &store.CouncilRun{
		ID: "COUNCIL-PRIOR-DRYRUN-SPEND", Trigger: store.CouncilTriggerCron,
		StartedAt: env.now.Add(-time.Hour), EndedAt: &ended,
		Outcome: store.CouncilOutcomeError, CostFrontierUSD: 50,
	}); err != nil {
		t.Fatalf("seed prior spend: %v", err)
	}
	reviewer := &countingReviewer{}
	env.runner.Reviewers = &council.Dispatcher{Reviewers: map[string]council.Reviewer{
		"security": reviewer, "tech-debt": reviewer,
	}}

	_, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual, Dryrun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "budget denied") {
		t.Fatalf("dryrun error = %v, want budget denial", err)
	}
	if got := reviewer.calls.Load(); got != 0 {
		t.Fatalf("reviewer calls = %d, want 0 before denied dryrun", got)
	}
}

func TestRun_BudgetReadFailureFailsClosed(t *testing.T) {
	env := newRunnerEnv(t, nil)
	reviewer := &countingReviewer{}
	env.runner.Reviewers = &council.Dispatcher{Reviewers: map[string]council.Reviewer{
		"security": reviewer, "tech-debt": reviewer,
	}}
	if err := env.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	_, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err == nil || !strings.Contains(err.Error(), "budget check") {
		t.Fatalf("run error = %v, want budget-store failure", err)
	}
	if got := reviewer.calls.Load(); got != 0 {
		t.Fatalf("reviewer calls = %d, want 0 before failed admission", got)
	}
}

func TestRun_EvalErrorPersistsErrorOutcomeAndCost(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	evalErr := errors.New("evaluator unavailable")
	env.runner.Judge = &eval.Judge{Criteria: eval.DefaultRubric(&eval.FakeLLMJudge{
		Err: evalErr, CostUSD: 0.25, Backend: "litellm",
	})}

	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerCron,
		Reason:  "scheduled",
	})
	if !errors.Is(err, evalErr) {
		t.Fatalf("run error = %v, want evaluator error", err)
	}
	if res == nil {
		t.Fatal("run result is nil")
	}
	got, getErr := env.store.Council.Get(context.Background(), res.RunID)
	if getErr != nil {
		t.Fatalf("get failed council run: %v", getErr)
	}
	if got.Outcome != store.CouncilOutcomeError || got.EndedAt == nil {
		t.Fatalf("persisted run = %+v, want terminal error", got)
	}
	wantCost := 0.42 + 0.10 + 0.05 + 0.25
	if total := got.CostFrontierUSD + got.CostLocalUSD; abs(total-wantCost) > 1e-6 {
		t.Fatalf("persisted cost = %v, want %v", total, wantCost)
	}
	if abs(got.CostFrontierUSD-0.77) > 1e-6 || abs(got.CostLocalUSD-0.05) > 1e-6 {
		t.Fatalf("cost attribution frontier/local = %.2f/%.2f, want 0.77/0.05",
			got.CostFrontierUSD, got.CostLocalUSD)
	}
	if sidecarTotal := res.Editor.Sidecar.CostUSD.Frontier + res.Editor.Sidecar.CostUSD.Local; abs(sidecarTotal-0.57) > 1e-6 {
		t.Fatalf("generation sidecar cost = %v, want pre-eval spend 0.57", sidecarTotal)
	}
	if !strings.Contains(got.Notes, evalErr.Error()) {
		t.Fatalf("notes = %q, want evaluator failure", got.Notes)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_EvalErrorDryrunDoesNotPersist(t *testing.T) {
	env := newRunnerEnv(t, nil)
	evalErr := errors.New("dryrun evaluator failed")
	env.runner.Judge = &eval.Judge{Criteria: eval.DefaultRubric(&eval.FakeLLMJudge{
		Err: evalErr, CostUSD: 0.25, Backend: "litellm",
	})}

	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual,
		Dryrun:  true,
	})
	if !errors.Is(err, evalErr) {
		t.Fatalf("run error = %v, want evaluator error", err)
	}
	if res == nil {
		t.Fatal("run result is nil")
	}
	if _, getErr := env.store.Council.Get(context.Background(), res.RunID); !errors.Is(getErr, store.ErrNotFound) {
		t.Fatalf("dryrun persisted council row: %v", getErr)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_ReleasesReservationOnEveryExit(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Writer = &council.ArtifactWriter{RepoRoot: filepath.Join(env.repoRoot, "missing")}
	_, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err == nil || !strings.Contains(err.Error(), "artifacts") {
		t.Fatalf("run error = %v, want artifact failure", err)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_CancellationStillFinalizesAndReleasesReservation(t *testing.T) {
	env := newRunnerEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	env.runner.Editor = cancelingEditor{cancel: cancel}

	res, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerCron})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context canceled", err)
	}
	if res == nil {
		t.Fatal("run result is nil")
	}
	got, getErr := env.store.Council.Get(context.Background(), res.RunID)
	if getErr != nil {
		t.Fatalf("get canceled council run: %v", getErr)
	}
	if got.Outcome != store.CouncilOutcomeError || got.EndedAt == nil {
		t.Fatalf("canceled run = %+v, want terminal error", got)
	}
	if !strings.Contains(got.Notes, context.Canceled.Error()) {
		t.Fatalf("canceled run notes = %q, want cancellation", got.Notes)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_PanicStillPersistsErrorAndReleasesReservation(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Editor = panickingEditor{}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	}()
	if recovered == nil {
		t.Fatal("editor panic did not propagate")
	}
	runs, err := env.store.Council.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("list council runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Outcome != store.CouncilOutcomeError || runs[0].EndedAt == nil {
		t.Fatalf("panic run = %+v, want one terminal error", runs)
	}
	if !strings.Contains(runs[0].Notes, "panic") {
		t.Fatalf("panic run notes = %q, want panic marker", runs[0].Notes)
	}
	if total := runs[0].CostFrontierUSD + runs[0].CostLocalUSD; abs(total-5) > 1e-6 {
		t.Fatalf("panic run cost = %v, want full $5 reservation for interrupted provider work", total)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_UnpricedEditorConsumesConservativeReservation(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Editor = unpricedEditor{base: env.runner.Editor}

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := env.store.Council.Get(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("get council run: %v", err)
	}
	if total := got.CostFrontierUSD + got.CostLocalUSD; abs(total-5) > 1e-6 {
		t.Fatalf("unpriced total = %v, want full $5 reservation", total)
	}
	if !strings.Contains(got.Notes, "unpriced provider spend") {
		t.Fatalf("unpriced notes = %q, want conservative-accounting marker", got.Notes)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_UnpricedEditorDryrunReportsConservativeReservation(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Editor = unpricedEditor{base: env.runner.Editor}

	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual, Dryrun: true,
	})
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	if abs(res.CostUSDApprox-5) > 1e-6 {
		t.Fatalf("dryrun unpriced cost = %v, want full $5 preflight estimate", res.CostUSDApprox)
	}
	if _, err := env.store.Council.Get(context.Background(), res.RunID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dryrun persisted council row: %v", err)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_UnpricedReviewerConsumesConservativeReservation(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Reviewers.Reviewers["security"] = unpricedErrorReviewer{}

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := env.store.Council.Get(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("get council run: %v", err)
	}
	if total := got.CostFrontierUSD + got.CostLocalUSD; abs(total-5) > 1e-6 {
		t.Fatalf("unpriced reviewer total = %v, want full $5 reservation", total)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestRun_PersistenceFailureDoesNotMaskEvalFailure(t *testing.T) {
	env := newRunnerEnv(t, nil)
	evalErr := errors.New("original evaluator failure")
	env.runner.Judge = &eval.Judge{Criteria: []eval.Criterion{
		&closingErrorCriterion{store: env.store, err: evalErr},
	}}

	_, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if !errors.Is(err, evalErr) {
		t.Fatalf("joined error = %v, want original evaluator failure preserved", err)
	}
	if err == nil || !strings.Contains(err.Error(), "finalize council run") {
		t.Fatalf("joined error = %v, want finalization failure too", err)
	}
}

type countingReviewer struct{ calls atomic.Int32 }

func (r *countingReviewer) Review(_ context.Context, _ *council.Brief, lens council.ReviewerLens) (council.ReviewerOutput, error) {
	r.calls.Add(1)
	return council.ReviewerOutput{Lens: lens, Markdown: "ok"}, nil
}

type cancelingEditor struct{ cancel context.CancelFunc }

func (e cancelingEditor) Edit(ctx context.Context, _ *council.Brief, _ []council.ReviewerOutput) (*council.EditorOutput, error) {
	e.cancel()
	return nil, ctx.Err()
}

type panickingEditor struct{}

func (panickingEditor) Edit(context.Context, *council.Brief, []council.ReviewerOutput) (*council.EditorOutput, error) {
	panic("editor exploded")
}

type unpricedEditor struct{ base council.Editor }

func (e unpricedEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	out, err := e.base.Edit(ctx, brief, reviews)
	if out != nil {
		out.CostUSD = 0
		out.Sidecar.CostUSD = council.SidecarCost{}
		out.CostUnpriced = true
	}
	return out, err
}

type unpricedErrorReviewer struct{}

func (unpricedErrorReviewer) Review(_ context.Context, _ *council.Brief, lens council.ReviewerLens) (council.ReviewerOutput, error) {
	err := errors.New("remote reviewer response had no usage")
	return council.ReviewerOutput{Lens: lens, CostUnpriced: true}, err
}

type closingErrorCriterion struct {
	store *store.Store
	err   error
}

func (*closingErrorCriterion) Name() string    { return "closing_error" }
func (*closingErrorCriterion) Weight() float64 { return 1 }
func (c *closingErrorCriterion) Score(_ context.Context, _ eval.Input) (eval.CriterionResult, error) {
	_ = c.store.Close()
	return eval.CriterionResult{Name: c.Name(), Weight: c.Weight()}, c.err
}

func assertNoActiveCouncilReservations(t *testing.T, st *store.Store) {
	t.Helper()
	var active int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM council_budget_reservations WHERE state = 'active'`,
	).Scan(&active); err != nil {
		t.Fatalf("count active council reservations: %v", err)
	}
	if active != 0 {
		t.Fatalf("active council reservations = %d, want 0", active)
	}
}

// ----- Phase 5.2: debate persistence + cost rollup -----

// TestRun_DebatePersistsTranscriptAndRollsUpCost is the slice-5.2
// acceptance gate: when policy.Debate.Enabled enables the trigger AND
// a Moderator is wired, the runner (a) drives Council Debate Mode end
// to end, (b) stamps the per-round transcript onto the persisted
// council_debate_rounds table, and (c) rolls the *full* debate spend
// into council_runs.cost_frontier_usd so CouncilDAO.SumCostSince
// reflects the daily cap. Single-pass cost tracking is unchanged.
func TestRun_DebatePersistsTranscriptAndRollsUpCost(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))

	// Flip debate on for the manual trigger (V2-D5 default).
	pol := env.policy.Current()
	pol.Debate = mills.DebatePolicy{
		Enabled:            mills.DebateTriggers{Manual: true},
		MaxUSD:             8.0,
		MaxRounds:          3,
		EarlyExitThreshold: 0.8,
	}

	// Wire the moderator: converge after the first decision so the
	// debate emits Round 0 propose + Round 1 critique + Round 1
	// moderator_decision (converged=true) and exits without a
	// follow-up revise. That gives us 3 transcript rows under the
	// per-run MaxUSD cap.
	env.runner.Moderator = &council.FakeModerator{
		ConvergeAfterRound: 0,
		FocusAreas:         nil,
		CostUSD:            0.05,
	}

	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual,
		Reason:  "phase 5.2 persistence test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// (1) Sidecar.Debate populated.
	if res.Editor == nil || res.Editor.Sidecar.Debate == nil {
		t.Fatalf("sidecar.debate not populated")
	}
	sb := res.Editor.Sidecar.Debate
	if !sb.Enabled {
		t.Errorf("sidecar.debate.enabled: got %v want true", sb.Enabled)
	}
	if sb.EarlyExitReason != "converged" {
		t.Errorf("early_exit_reason: got %q want %q", sb.EarlyExitReason, "converged")
	}
	if len(sb.Rounds) != 3 {
		t.Fatalf("transcript rows: got %d want 3 (R0 propose, R1 critique, R1 moderator)", len(sb.Rounds))
	}

	// (2) Persistence: every transcript row should land in
	// council_debate_rounds.
	persisted, err := env.store.Debate.ListByRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("list-by-run: %v", err)
	}
	if len(persisted) != 3 {
		t.Errorf("persisted rows: got %d want 3", len(persisted))
	}
	wantRoles := []store.DebateRole{
		store.DebateRoleEditorProposes,
		store.DebateRoleReviewerCritiques,
		store.DebateRoleModeratorDecision,
	}
	for i, want := range wantRoles {
		if i >= len(persisted) {
			break
		}
		if persisted[i].Role != want {
			t.Errorf("persisted[%d].Role: got %q want %q", i, persisted[i].Role, want)
		}
	}

	// (3) Cost rollup: SumCostSince via DebateDAO must equal the
	// transcript total, and the persisted council_runs row's
	// frontier cost must equal the same total.
	now := env.now
	debateSpent, err := env.store.Debate.SumCostSince(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("debate sum-since: %v", err)
	}
	if !approxEqual(debateSpent, sb.TotalCostUSD, 0.001) {
		t.Errorf("DebateDAO.SumCostSince: got %.4f want %.4f", debateSpent, sb.TotalCostUSD)
	}
	cRun, err := env.store.Council.Get(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("get council run: %v", err)
	}
	if !approxEqual(cRun.CostFrontierUSD, sb.TotalCostUSD, 0.001) {
		t.Errorf("council_runs.cost_frontier_usd: got %.4f want %.4f (full debate cost should roll up)",
			cRun.CostFrontierUSD, sb.TotalCostUSD)
	}
	if cRun.CostLocalUSD != 0 {
		t.Errorf("council_runs.cost_local_usd: got %.4f want 0 (debate Local refined in slice 5.3)", cRun.CostLocalUSD)
	}
}

// TestRun_DebateDryrunSkipsPersistence pins that the dryrun path does
// NOT write debate rounds even when debate ran successfully — the
// scratch-dir invariant of dryrun is "no canonical writes".
func TestRun_DebateDryrunSkipsPersistence(t *testing.T) {
	env := newRunnerEnv(t, nil)
	pol := env.policy.Current()
	pol.Debate = mills.DebatePolicy{
		Enabled:   mills.DebateTriggers{Manual: true},
		MaxUSD:    8.0,
		MaxRounds: 3,
	}
	env.runner.Moderator = &council.FakeModerator{ConvergeAfterRound: 0, CostUSD: 0.05}

	res, err := env.runner.Run(context.Background(), RunInput{
		Trigger: store.CouncilTriggerManual,
		Dryrun:  true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Editor == nil || res.Editor.Sidecar.Debate == nil {
		t.Fatalf("dryrun should still populate Sidecar.Debate (transcript visible to caller)")
	}
	persisted, err := env.store.Debate.ListByRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("list-by-run: %v", err)
	}
	if len(persisted) != 0 {
		t.Errorf("dryrun should not persist debate rounds; got %d rows", len(persisted))
	}
}

func approxEqual(a, b, eps float64) bool {
	if a == b {
		return true
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// ----- helpers -----

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

type alwaysZeroCriterion struct{}

func (alwaysZeroCriterion) Name() string    { return "always_zero" }
func (alwaysZeroCriterion) Weight() float64 { return 1.0 }
func (alwaysZeroCriterion) Score(_ context.Context, _ eval.Input) (eval.CriterionResult, error) {
	return eval.CriterionResult{Name: "always_zero", Weight: 1.0, Score: 0}, nil
}

// ----- stage budgets (G2/G3/G5) -----

// blockingEditor models a wedged provider call: it blocks until its context is
// done and then reports why, which is exactly the shape of the 2026-07-16
// incident's editor phase.
type blockingEditor struct {
	entered chan struct{}
	once    sync.Once
}

func (e *blockingEditor) Edit(ctx context.Context, _ *council.Brief, _ []council.ReviewerOutput) (*council.EditorOutput, error) {
	if e.entered != nil {
		e.once.Do(func() { close(e.entered) })
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// blockingCriterion wedges the judge stage the same way blockingEditor wedges
// the editor.
type blockingCriterion struct{}

func (blockingCriterion) Name() string    { return "blocking" }
func (blockingCriterion) Weight() float64 { return 1.0 }
func (blockingCriterion) Score(ctx context.Context, _ eval.Input) (eval.CriterionResult, error) {
	<-ctx.Done()
	return eval.CriterionResult{Name: "blocking", Weight: 1.0}, ctx.Err()
}

// Acceptance criterion 6: an editor that outlives its stage budget terminalizes
// the run and the persisted row names the stage and its budget.
func TestExecute_EditorStageBudgetTerminalizesRun(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Editor = &blockingEditor{entered: make(chan struct{})}
	env.runner.StageBudgets = StageBudgets{Editor: 60 * time.Millisecond}

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err == nil || !strings.Contains(err.Error(), "stage editor exceeded 60ms budget") {
		t.Fatalf("run error = %v, want editor stage-budget failure", err)
	}
	got, getErr := env.store.Council.Get(context.Background(), res.RunID)
	if getErr != nil {
		t.Fatalf("get council run: %v", getErr)
	}
	if got.Outcome != store.CouncilOutcomeError || got.EndedAt == nil {
		t.Fatalf("run = %+v, want terminal error", got)
	}
	if !strings.Contains(got.Notes, "stage editor exceeded 60ms budget") {
		t.Fatalf("notes = %q, want the failing stage + budget named", got.Notes)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

func TestExecute_JudgeStageBudgetTerminalizesRun(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Judge = &eval.Judge{Criteria: []eval.Criterion{blockingCriterion{}}}
	env.runner.StageBudgets = StageBudgets{Judge: 60 * time.Millisecond}

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	if err == nil || !strings.Contains(err.Error(), "stage judge exceeded 60ms budget") {
		t.Fatalf("run error = %v, want judge stage-budget failure", err)
	}
	got, getErr := env.store.Council.Get(context.Background(), res.RunID)
	if getErr != nil {
		t.Fatalf("get council run: %v", getErr)
	}
	if got.Outcome != store.CouncilOutcomeError || got.EndedAt == nil {
		t.Fatalf("run = %+v, want terminal error", got)
	}
	if !strings.Contains(got.Notes, "stage judge") {
		t.Fatalf("notes = %q, want the judge stage named", got.Notes)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

// The overall cap wins even when a single stage's own budget is generous, so a
// pathological sum of stages can't hold the reservation open.
func TestExecute_OverallBudgetCapsTheWholePass(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Editor = &blockingEditor{entered: make(chan struct{})}
	env.runner.StageBudgets = StageBudgets{Overall: 150 * time.Millisecond, Editor: time.Hour}

	started := time.Now()
	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerCron})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "exceeded its overall budget") {
		t.Fatalf("run error = %v, want overall-budget failure", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("run took %s, want the overall cap to fire promptly", elapsed)
	}
	got, getErr := env.store.Council.Get(context.Background(), res.RunID)
	if getErr != nil {
		t.Fatalf("get council run: %v", getErr)
	}
	if got.Outcome != store.CouncilOutcomeError || got.EndedAt == nil {
		t.Fatalf("run = %+v, want terminal error", got)
	}
	if !strings.Contains(got.Notes, "overall budget") {
		t.Fatalf("notes = %q, want the overall cap named", got.Notes)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

// Slice 2's completion criterion: the SCHEDULED path is bounded. The scheduler
// calls the runner on its own root context with no deadline anywhere
// (cmd/loom-mills-operator/main.go's councilRunFn closure, reproduced here), so
// only the overall cap inside Execute can stop a wedged participant — and no
// change in pkg/mills/council_scheduler.go is required.
func TestExecute_OverallBudgetBoundsSchedulerPath(t *testing.T) {
	env := newRunnerEnv(t, nil)
	env.runner.Editor = &blockingEditor{entered: make(chan struct{})}
	env.runner.StageBudgets = StageBudgets{Overall: 150 * time.Millisecond, Editor: time.Hour}

	var runFn mills.CouncilRunFn = func(ctx context.Context, trigger store.CouncilTrigger, reason string) error {
		_, err := env.runner.Run(ctx, RunInput{Trigger: trigger, Reason: reason})
		return err
	}

	started := time.Now()
	err := runFn(context.Background(), store.CouncilTriggerCron, "scheduler")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("scheduled run returned nil error, want the overall cap to fire")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("scheduled run took %s, want the overall cap to bound it", elapsed)
	}
	runs, listErr := env.store.Council.List(context.Background(), 10)
	if listErr != nil {
		t.Fatalf("list council runs: %v", listErr)
	}
	if len(runs) != 1 || runs[0].Outcome != store.CouncilOutcomeError || runs[0].EndedAt == nil {
		t.Fatalf("scheduled run = %+v, want one terminal error row", runs)
	}
	assertNoActiveCouncilReservations(t, env.store)
}

// Acceptance criterion 7: every stage emits a start and an end line carrying
// duration_ms, and no log record ever contains a credential value.
func TestExecute_StageLogsCarryDurationAndNoCredentials(t *testing.T) {
	const secret = "s3cr3t-council-credential"
	t.Setenv("FLEXINFER_TOKEN", secret)
	t.Setenv("LOOM_MILLS_LITELLM_KEY", secret)
	t.Setenv("GITLAB_TOKEN", secret)

	env := newRunnerEnv(t, sampleProposals(1))
	var logs strings.Builder
	env.runner.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerManual}); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := logs.String()
	for _, stage := range []string{"brief", "reviewers", "editor", "artifacts", "judge", "persist", "mutator"} {
		if !strings.Contains(out, `msg="council stage start" run_id=`) {
			t.Fatalf("no stage start lines emitted: %s", out)
		}
		if !strings.Contains(out, "stage="+stage) {
			t.Errorf("stage %q never logged; output:\n%s", stage, out)
		}
	}
	if strings.Count(out, `msg="council stage end"`) < 7 {
		t.Errorf("stage end lines = %d, want one per stage:\n%s",
			strings.Count(out, `msg="council stage end"`), out)
	}
	if !strings.Contains(out, "duration_ms=") {
		t.Errorf("stage end lines carry no duration_ms:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("credential value leaked into council logs:\n%s", out)
	}
}

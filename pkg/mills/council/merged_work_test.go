package council

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/textsim"
)

// mergedWorkAnchor matches the clock newMutatorEnv pins on the mutator, so
// MergedAt ages in these fixtures are relative to what Apply reads as "now".
var mergedWorkAnchor = time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

// stubMergedWork is the council-side MergedWorkSource fake: `merged` seeds the
// corpus, `err` forces the fail-open branch, and the counters prove the
// snapshot is taken exactly once per Apply (or not at all when gated off).
type stubMergedWork struct {
	merged []MergedWork
	err    error
	calls  int
	since  time.Time
}

type semanticOverride textsim.Similarity

func (s semanticOverride) Score(context.Context, string, string) textsim.Similarity {
	return textsim.Similarity(s)
}

type countingScorer struct {
	result textsim.Similarity
	calls  int
}

func (s *countingScorer) Score(context.Context, string, string) textsim.Similarity {
	s.calls++
	return s.result
}

func (s *stubMergedWork) ListMergedWork(_ context.Context, since time.Time) ([]MergedWork, error) {
	s.calls++
	s.since = since
	if s.err != nil {
		return nil, s.err
	}
	return s.merged, nil
}

// mergedWorkProposal is the fixture proposal every test in this file re-mints.
// Its title is what the council would author from a stale brief; the merged MR
// below is the work that already shipped it.
func mergedWorkProposal() *EditorOutput {
	return &EditorOutput{BacklogProposals: []BacklogProposal{{
		Title:    "Add a Grafana panel and alert for the embedder",
		Priority: store.P2,
		Budget:   store.Budget{MaxCostUSD: 1},
	}}}
}

// shippedMR wears both decorations a mills-shipped MR carries: the editor's
// conventional-commit prefix and the plan-slice emitter's " — <slug>" suffix.
// Raw TitleJaccard against the proposal above is 0.625 — under the 0.7 hard
// threshold — so this fixture only suppresses if normalization happens.
func shippedMR(age time.Duration) MergedWork {
	return MergedWork{
		IID:      1419,
		Title:    "feat(hud): add embedder Grafana panel and alert — embedder-alerting",
		WebURL:   "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/1419",
		MergedAt: mergedWorkAnchor.Add(-age),
	}
}

// TestApply_MergedWork_SuppressesShippedProposal is the slice's acceptance
// test: a proposal restating an MR that merged hours ago is dropped, and the
// normalization is load-bearing — the same pair compared raw scores under the
// hard threshold.
func TestApply_MergedWork_SuppressesShippedProposal(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	src := &stubMergedWork{merged: []MergedWork{shippedMR(6 * time.Hour)}}
	m.MergedWork = src

	res, err := m.Apply(context.Background(), "COUNCIL-X", mergedWorkProposal(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.CreatedItems) != 0 {
		t.Fatalf("created=%d want 0", len(res.CreatedItems))
	}
	if len(res.MergedWorkSkipped) != 1 {
		t.Fatalf("merged_work_skipped=%d want 1", len(res.MergedWorkSkipped))
	}
	skip := res.MergedWorkSkipped[0]
	if skip.MergedIID != 1419 {
		t.Errorf("merged_iid = %d want 1419", skip.MergedIID)
	}
	if skip.Basis != mergedWorkBasisHard {
		t.Errorf("basis = %q want %q (score %v)", skip.Basis, mergedWorkBasisHard, skip.JaccardScore)
	}
	if skip.JaccardScore < 0.7 {
		t.Errorf("score = %v want >= 0.7 (normalization should have closed the gap)", skip.JaccardScore)
	}
	// Normalization is load-bearing here, not incidental: compared raw, this
	// pair scores under the hard threshold and the proposal would have landed.
	if raw := textsim.TitleJaccard(skip.ProposalTitle, skip.MergedTitle); raw >= 0.7 {
		t.Errorf("fixture no longer proves normalization matters: raw score = %v, want < 0.7", raw)
	}
	// One snapshot per Apply, over the default 14d window.
	if src.calls != 1 {
		t.Errorf("ListMergedWork calls = %d want 1", src.calls)
	}
	if want := mergedWorkAnchor.Add(-defaultMergedWorkLookback); !src.since.Equal(want) {
		t.Errorf("lookback cutoff = %s want %s", src.since, want)
	}
	// Nothing reached the canonical store.
	all, _ := st.Backlog.List(context.Background())
	if len(all) != 0 {
		t.Errorf("backlog size = %d want 0", len(all))
	}
	if got := res.Summary(); !strings.Contains(got, "merged_work_skipped=1") {
		t.Errorf("summary = %q want it to report merged_work_skipped=1", got)
	}
}

func TestApply_MergedWork_SemanticGrounding(t *testing.T) {
	t.Run("semantic scorer can reject lexical hit", func(t *testing.T) {
		m, _, _ := newMutatorEnv(t)
		m.MergedWork = &stubMergedWork{merged: []MergedWork{shippedMR(6 * time.Hour)}}
		m.MergedWorkSemantic = semanticOverride(textsim.Similarity{
			Semantic: 0, Combined: 0, SemanticAvailable: true,
		})

		res, err := m.Apply(context.Background(), "COUNCIL-T", mergedWorkProposal(), MutationOptions{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(res.MergedWorkSkipped) != 0 || len(res.CreatedItems) != 1 {
			t.Fatalf("created=%d skipped=%d; configured semantic score did not control the decision", len(res.CreatedItems), len(res.MergedWorkSkipped))
		}
	})

	t.Run("semantic scorer can ground a lexical miss", func(t *testing.T) {
		// The configured scorer is authoritative even when the lexical score
		// misses the gray band.
		m, _, _ := newMutatorEnv(t)
		m.MergedWork = &stubMergedWork{merged: []MergedWork{{
			IID: 42, Title: "Repair webhook retries", MergedAt: mergedWorkAnchor.Add(-time.Hour),
		}}}
		m.MergedWorkSemantic = semanticOverride(textsim.Similarity{
			Semantic: 1, Combined: 1, SemanticAvailable: true,
		})

		res, err := m.Apply(context.Background(), "COUNCIL-P", mergedWorkProposal(), MutationOptions{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(res.CreatedItems) != 0 || len(res.MergedWorkSkipped) != 1 {
			t.Fatalf("created=%d skipped=%d; configured semantic score did not ground the match", len(res.CreatedItems), len(res.MergedWorkSkipped))
		}
	})

	t.Run("live 2026-09-02 pair: HUD panel unification is not fi-fhir spawn admission", func(t *testing.T) {
		candidates := []MergedWork{{
			IID: 1798, Title: "feat(mills): admit libs/fi-fhir to HUD spawning", MergedAt: mergedWorkAnchor.Add(-time.Hour),
		}}
		scorer := semanticOverride(textsim.Similarity{
			Semantic: 0.8397933101064353, Combined: 0.8397933101064353, SemanticAvailable: true,
		})
		if hit := findMergedWorkGrounded(context.Background(), scorer, "Unify Mill Staff HUD panels into a single group (S4 finish)", candidates, 0.7, mergedWorkAnchor); hit == nil {
			t.Fatal("configured semantic score should control the hard band")
		}
	})

	t.Run("semantic promotes a lexical gray-band pair into a hard hit", func(t *testing.T) {
		// !978's phrasing against !970's merged title: lexical 0.6 (gray band).
		// The MR is OLD, so the gray band alone would admit the proposal; a
		// semantic score above the threshold lifts the plausible pair to hard.
		candidates := []MergedWork{{
			IID: 970, Title: "Add external CI incident classification for GitLab pipeline failures", MergedAt: mergedWorkAnchor.Add(-30 * 24 * time.Hour),
		}}
		title := "Add GitLab CI external dependency incident classification to Mills"
		lexical := textsim.WorkTitleJaccard(title, candidates[0].Title)
		if lexical < textsim.GrayBandFloor || lexical >= 0.7 {
			t.Fatalf("fixture lexical score %.2f is not in the gray band", lexical)
		}
		scorer := semanticOverride(textsim.Similarity{Semantic: 0.9, Combined: 0.9, SemanticAvailable: true})
		hit := findMergedWorkGrounded(context.Background(), scorer, title, candidates, 0.7, mergedWorkAnchor)
		if hit == nil || hit.basis != mergedWorkBasisHard {
			t.Fatalf("semantic corroboration of a gray-band pair should be a hard hit, got %+v", hit)
		}
	})

	t.Run("unrelated work remains admitted", func(t *testing.T) {
		candidates := []MergedWork{{
			IID: 42, Title: "Repair webhook retries", MergedAt: mergedWorkAnchor.Add(-time.Hour),
		}}
		scorer := semanticOverride(textsim.Similarity{
			Semantic: 0, Combined: 0, SemanticAvailable: true,
		})
		if hit := findMergedWorkGrounded(context.Background(), scorer, mergedWorkProposal().BacklogProposals[0].Title, candidates, 0.7, mergedWorkAnchor); hit != nil {
			t.Fatalf("unrelated work was grounded: %+v", hit)
		}
	})

	t.Run("unavailable backend scores at most one candidate", func(t *testing.T) {
		candidates := []MergedWork{
			{IID: 1, Title: "Repair webhook retries", MergedAt: mergedWorkAnchor.Add(-time.Hour)},
			{IID: 2, Title: "Harden council quorum", MergedAt: mergedWorkAnchor.Add(-time.Hour)},
			{IID: 3, Title: "Split HUD spawn files", MergedAt: mergedWorkAnchor.Add(-time.Hour)},
		}
		scorer := &countingScorer{}
		findMergedWorkGrounded(context.Background(), scorer, mergedWorkProposal().BacklogProposals[0].Title, candidates, 0.7, mergedWorkAnchor)
		if scorer.calls != 1 {
			t.Fatalf("scorer called %d times; want 1 — an unavailable backend must not add one timeout per merged-work candidate", scorer.calls)
		}
	})

	t.Run("semantic ranking wins and every candidate is observed", func(t *testing.T) {
		candidates := []MergedWork{
			{IID: 1, Title: "alpha beta gamma", MergedAt: mergedWorkAnchor.Add(-time.Hour)},
			{IID: 2, Title: "unrelated title", MergedAt: mergedWorkAnchor.Add(-time.Hour)},
		}
		scorer := &sequenceScorer{results: []textsim.Similarity{
			{Semantic: .71, Combined: .71, SemanticAvailable: true},
			{Semantic: .95, Combined: .95, SemanticAvailable: true},
		}}
		var observed []mergedWorkScore
		hit := findMergedWorkGroundedObserved(context.Background(), scorer, "alpha beta gamma", candidates, .7, mergedWorkAnchor, func(s mergedWorkScore) { observed = append(observed, s) })
		if hit == nil || hit.work.IID != 2 || len(observed) != 2 {
			t.Fatalf("hit=%+v observed=%d; semantic ranking must win and all candidates must be observed", hit, len(observed))
		}
		for _, score := range observed {
			if !score.semanticAvailable || score.lexicalScore < 0 || score.semanticScore == 0 {
				t.Fatalf("incomplete dual-score observation: %+v", score)
			}
		}
	})

	t.Run("exact score boundaries are unchanged", func(t *testing.T) {
		candidate := []MergedWork{{IID: 1, Title: "candidate", MergedAt: mergedWorkAnchor.Add(-time.Hour)}}
		for _, tc := range []struct {
			name  string
			score float64
			want  string
		}{
			{"hard", .7, mergedWorkBasisHard},
			{"gray floor", textsim.GrayBandFloor, mergedWorkBasisGray},
			{"below gray", textsim.GrayBandFloor - .001, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				hit := findMergedWorkGrounded(context.Background(), semanticOverride(textsim.Similarity{Semantic: tc.score, Combined: tc.score, SemanticAvailable: true}), "proposal", candidate, .7, mergedWorkAnchor)
				if tc.want == "" && hit != nil {
					t.Fatalf("got %+v, want no hit", hit)
				}
				if tc.want != "" && (hit == nil || hit.basis != tc.want) {
					t.Fatalf("got %+v, want basis %s", hit, tc.want)
				}
			})
		}
	})
}

type sequenceScorer struct {
	results []textsim.Similarity
	calls   int
}

func (s *sequenceScorer) Score(context.Context, string, string) textsim.Similarity {
	r := s.results[s.calls]
	s.calls++
	return r
}

func TestApply_MergedWork_LogsBothScoresAndFallback(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(strconv.FormatBool(available), func(t *testing.T) {
			m, _, _ := newMutatorEnv(t)
			m.MergedWork = &stubMergedWork{merged: []MergedWork{shippedMR(time.Hour)}}
			m.MergedWorkSemantic = semanticOverride(textsim.Similarity{Semantic: .8, Combined: .8, SemanticAvailable: available})
			var logs bytes.Buffer
			m.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			_, err := m.Apply(context.Background(), "COUNCIL-LOG", mergedWorkProposal(), MutationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got := logs.String()
			for _, field := range []string{"lexical_score", "semantic_score", "semantic_available", "configured_score"} {
				if !strings.Contains(got, field) {
					t.Errorf("log missing %q: %s", field, got)
				}
			}
		})
	}
}

// TestApply_MergedWork_GrayBand mirrors the backlog gray band onto merged work:
// a reworded restatement scoring in [GrayBandFloor, threshold) is suppressed
// when the MR merged inside the recency window, and allowed through when the MR
// is older — a loose lookalike of long-shipped work is legitimate follow-up.
func TestApply_MergedWork_GrayBand(t *testing.T) {
	// !978's phrasing against !970's merged title: the live 0.6 pair.
	reMint := &EditorOutput{BacklogProposals: []BacklogProposal{{
		Title:    "Add GitLab CI external dependency incident classification to Mills",
		Priority: store.P2,
		Budget:   store.Budget{MaxCostUSD: 1},
	}}}
	shipped := func(age time.Duration) MergedWork {
		return MergedWork{
			IID:      970,
			Title:    "feat(mills): add external CI incident classification for GitLab pipeline failures",
			MergedAt: mergedWorkAnchor.Add(-age),
		}
	}

	t.Run("recent merge blocks", func(t *testing.T) {
		m, _, _ := newMutatorEnv(t)
		m.MergedWork = &stubMergedWork{merged: []MergedWork{shipped(2 * 24 * time.Hour)}}
		res, err := m.Apply(context.Background(), "COUNCIL-Y", reMint, MutationOptions{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(res.CreatedItems) != 0 || len(res.MergedWorkSkipped) != 1 {
			t.Fatalf("created=%d merged_work_skipped=%d want 0/1", len(res.CreatedItems), len(res.MergedWorkSkipped))
		}
		skip := res.MergedWorkSkipped[0]
		if skip.Basis != mergedWorkBasisGray {
			t.Errorf("basis = %q want %q", skip.Basis, mergedWorkBasisGray)
		}
		if skip.JaccardScore >= 0.7 || skip.JaccardScore < 0.55 {
			t.Errorf("score %v should sit in the gray band [0.55, 0.7)", skip.JaccardScore)
		}
	})

	t.Run("stale merge is legitimate follow-up", func(t *testing.T) {
		m, _, _ := newMutatorEnv(t)
		// Inside the 14d fetch window, outside the 7d gray-band recency gate.
		m.MergedWork = &stubMergedWork{merged: []MergedWork{shipped(10 * 24 * time.Hour)}}
		res, err := m.Apply(context.Background(), "COUNCIL-T", reMint, MutationOptions{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if len(res.CreatedItems) != 1 || len(res.MergedWorkSkipped) != 0 {
			t.Fatalf("created=%d merged_work_skipped=%d want 1/0", len(res.CreatedItems), len(res.MergedWorkSkipped))
		}
	})
}

// TestApply_MergedWork_FailsOpenOnFetchError pins the resilience contract: a
// GitLab outage must never block the council. The proposal lands ungrounded.
func TestApply_MergedWork_FailsOpenOnFetchError(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	src := &stubMergedWork{err: errors.New("gitlab: 503 service unavailable")}
	m.MergedWork = src

	res, err := m.Apply(context.Background(), "COUNCIL-C", mergedWorkProposal(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply must not fail on a merged-work fetch error: %v", err)
	}
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created=%d want 1 (fail-open)", len(res.CreatedItems))
	}
	if len(res.MergedWorkSkipped) != 0 {
		t.Errorf("merged_work_skipped=%d want 0", len(res.MergedWorkSkipped))
	}
	if src.calls != 1 {
		t.Errorf("ListMergedWork calls = %d want 1", src.calls)
	}
	all, _ := st.Backlog.List(context.Background())
	if len(all) != 1 {
		t.Errorf("backlog size = %d want 1", len(all))
	}
}

// TestApply_MergedWork_DisabledBypassesFetch proves the policy flag is a real
// bypass and not just a suppression filter: with grounding off the mutator does
// not even take the snapshot, so a GitLab-side cost is not paid either.
func TestApply_MergedWork_DisabledBypassesFetch(t *testing.T) {
	m, _, _ := newMutatorEnv(t)
	src := &stubMergedWork{merged: []MergedWork{shippedMR(1 * time.Hour)}}
	m.MergedWork = src

	res, err := m.Apply(context.Background(), "COUNCIL-P", mergedWorkProposal(),
		MutationOptions{MergedWorkGroundingDisabled: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.CreatedItems) != 1 || len(res.MergedWorkSkipped) != 0 {
		t.Fatalf("created=%d merged_work_skipped=%d want 1/0", len(res.CreatedItems), len(res.MergedWorkSkipped))
	}
	if src.calls != 0 {
		t.Errorf("ListMergedWork calls = %d want 0 when grounding is disabled", src.calls)
	}
}

// TestApply_MergedWork_HonoursLookbackOverride proves the policy window reaches
// the fetch cutoff rather than being silently defaulted.
func TestApply_MergedWork_HonoursLookbackOverride(t *testing.T) {
	m, _, _ := newMutatorEnv(t)
	src := &stubMergedWork{}
	m.MergedWork = src

	if _, err := m.Apply(context.Background(), "COUNCIL-H", mergedWorkProposal(),
		MutationOptions{MergedWorkLookback: 48 * time.Hour}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := mergedWorkAnchor.Add(-48 * time.Hour); !src.since.Equal(want) {
		t.Errorf("lookback cutoff = %s want %s", src.since, want)
	}
}

// TestApply_MergedWork_AuditsUnderDistinctAction pins the audit contract the
// promotion report reads: merged-work suppressions land as their own action
// (not folded into dedup_skip) and name the merge request that caused them.
func TestApply_MergedWork_AuditsUnderDistinctAction(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	m.MergedWork = &stubMergedWork{merged: []MergedWork{shippedMR(6 * time.Hour)}}
	m.Recorder = &guard.ActionRecorder{
		Events: st.Events,
		Actor:  "council.mutator",
		DryRun: func() bool { return false },
	}

	res, err := m.Apply(context.Background(), "COUNCIL-A", mergedWorkProposal(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.MergedWorkSkipped) != 1 {
		t.Fatalf("merged_work_skipped=%d want 1", len(res.MergedWorkSkipped))
	}

	events, err := st.Events.ListByActorSince(context.Background(), "council.mutator", time.Time{}, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var found *store.Event
	for _, e := range events {
		if e.Kind == "council.mutator.merged_work_skip" {
			found = e
			break
		}
		if e.Kind == "council.mutator.dedup_skip" {
			t.Errorf("merged-work suppression recorded as dedup_skip; the promotion report cannot count it separately")
		}
	}
	if found == nil {
		t.Fatalf("no council.mutator.merged_work_skip event in %d events", len(events))
	}
	if found.SubjectKind != "merge_request" || found.SubjectID != "!1419" {
		t.Errorf("subject = %s/%s want merge_request/!1419", found.SubjectKind, found.SubjectID)
	}
	if got, _ := found.Payload["basis"].(string); got != mergedWorkBasisHard {
		t.Errorf("payload basis = %q want %q", got, mergedWorkBasisHard)
	}
	if got, _ := found.Payload["run_id"].(string); got != "COUNCIL-A" {
		t.Errorf("payload run_id = %q want COUNCIL-A", got)
	}
}

// TestMergedWorkRef covers the audit-subject rendering both ways: GitLab
// shorthand when an iid is known, web url when only that survived.
func TestMergedWorkRef(t *testing.T) {
	if got := (MergedWork{IID: 1424}).Ref(); got != "!1424" {
		t.Errorf("Ref() = %q want !1424", got)
	}
	url := "https://gitlab.flexinfer.ai/x/-/merge_requests/7"
	if got := (MergedWork{WebURL: url}).Ref(); got != url {
		t.Errorf("Ref() = %q want %q", got, url)
	}
}

// TestFindMergedWork_ThresholdEscapeHatch confirms the documented "dedup
// disabled" threshold (> 1) turns grounding off too, so the escape hatch stays
// one switch rather than two.
func TestFindMergedWork_ThresholdEscapeHatch(t *testing.T) {
	corpus := []MergedWork{shippedMR(time.Hour)}
	if hit := findMergedWork("Add a Grafana panel and alert for the embedder", corpus, 1.5, mergedWorkAnchor); hit != nil {
		t.Errorf("threshold > 1 must disable grounding, got hit %+v", hit)
	}
	if hit := findMergedWork("", corpus, 0.7, mergedWorkAnchor); hit != nil {
		t.Errorf("empty title must never match, got hit %+v", hit)
	}
}

// groundingEmbedder exercises the production semantic scorer through council
// selection, including responses that must preserve the lexical decision.
type groundingEmbedder struct {
	vectors [][]float64
	err     error
}

func (e groundingEmbedder) EmbedDocuments(context.Context, []string) ([][]float64, error) {
	return e.vectors, e.err
}

func TestMergedWorkGroundingScorerSelection(t *testing.T) {
	candidates := []MergedWork{{IID: 1, Title: "Repair webhook retries", MergedAt: mergedWorkAnchor}}
	title := "Add semantic grounding"
	for _, tc := range []struct {
		name    string
		scorer  textsim.Scorer
		wantHit bool
	}{
		{name: "default Jaccard"},
		{name: "explicit Jaccard", scorer: textsim.JaccardScorer{}},
		{name: "configured embeddings", scorer: textsim.NewSemanticScorer(groundingEmbedder{vectors: [][]float64{{1, 0}, {1, 0}}}), wantHit: true},
		{name: "nil backend", scorer: textsim.NewSemanticScorer(nil)},
		{name: "backend error", scorer: textsim.NewSemanticScorer(groundingEmbedder{err: errors.New("offline")})},
		{name: "empty response", scorer: textsim.NewSemanticScorer(groundingEmbedder{})},
		{name: "empty vectors", scorer: textsim.NewSemanticScorer(groundingEmbedder{vectors: [][]float64{nil, nil}})},
		{name: "mismatched vectors", scorer: textsim.NewSemanticScorer(groundingEmbedder{vectors: [][]float64{{1}, {1, 0}}})},
		{name: "invalid vectors", scorer: textsim.NewSemanticScorer(groundingEmbedder{vectors: [][]float64{{math.NaN()}, {1}}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := findMergedWorkGrounded(context.Background(), tc.scorer, title, candidates, .7, mergedWorkAnchor)
			if (hit != nil) != tc.wantHit {
				t.Fatalf("hit = %+v, wantHit = %v", hit, tc.wantHit)
			}
			if tc.wantHit {
				if hit.score != 1 || hit.lexicalScore != 0 || !hit.semanticAvailable {
					t.Fatalf("expected semantic-only match, got %+v", hit)
				}
				return
			}
			// Fallback must also preserve positive matches and their exact scores.
			lexicalTitle := "Repair webhook retries safely"
			want := findMergedWork(lexicalTitle, candidates, .7, mergedWorkAnchor)
			got := findMergedWorkGrounded(context.Background(), tc.scorer, lexicalTitle, candidates, .7, mergedWorkAnchor)
			if want == nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("fallback hit = %+v, want exact lexical hit %+v", got, want)
			}
		})
	}
}

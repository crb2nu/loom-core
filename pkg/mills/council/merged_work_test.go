package council

import (
	"context"
	"errors"
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

package council

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/textsim"
)

// The factory-exhaust source only adds evidence to the brief. Nothing about a
// proposal records that it came from the exhaust section, and the mutator has
// no notion of proposal provenance — these tests pin that: an exhaust-derived
// proposal must ride the SAME dedup and merged-work guards as any other, with
// no privileged path in either direction.

// exhaustFlakeIssue is the fixture piece of exhaust these tests plan against:
// one open quarantined-test issue as scripts/flakereport files it.
func exhaustFlakeIssue() FactoryExhaustItem {
	return FactoryExhaustItem{
		Kind:      FactoryExhaustFlakyTest,
		IID:       612,
		Title:     "flake: TestReconcilerIdleBackoff",
		WebURL:    "https://gitlab.flexinfer.ai/services/loom-core/-/issues/612",
		UpdatedAt: mergedWorkAnchor.Add(-3 * time.Hour),
	}
}

// exhaustProposal is what the council would author after reading the exhaust
// section — an ordinary BacklogProposal, structurally indistinguishable from
// one sourced off a roadmap intent.
func exhaustProposal() *EditorOutput {
	return &EditorOutput{BacklogProposals: []BacklogProposal{{
		Title:    "Fix the flaky quarantined TestReconcilerIdleBackoff test",
		Priority: store.P2,
		Budget:   store.Budget{MaxCostUSD: 1},
	}}}
}

// exhaustFixMR is the merge request that already fixed that flake, wearing the
// decorations a mills-shipped MR carries.
func exhaustFixMR(age time.Duration) MergedWork {
	return MergedWork{
		IID:      1450,
		Title:    "fix(mills): fix flaky quarantined TestReconcilerIdleBackoff test in the reconciler — flake-quarantine",
		WebURL:   "https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/1450",
		MergedAt: mergedWorkAnchor.Add(-age),
	}
}

// TestExhaustProposal_HitsMergedWorkGrounding is the slice's cross-guard
// acceptance test: the exhaust section can surface a flake whose fix already
// merged (the brief lists OPEN issues, and an MR closing one lands before the
// issue state propagates). Such a proposal must be suppressed by the ordinary
// merged-work guard, under the ordinary merged_work_skip action.
func TestExhaustProposal_HitsMergedWorkGrounding(t *testing.T) {
	// The brief that seeded this proposal really did carry the issue.
	b := compileWithExhaust(t, &stubFactoryExhaust{items: []FactoryExhaustItem{exhaustFlakeIssue()}}, nil)
	if !strings.Contains(b.Markdown, "`#612`") {
		t.Fatalf("fixture brief does not carry the exhaust issue:\n%s", b.Markdown)
	}

	m, st, _ := newMutatorEnv(t)
	src := &stubMergedWork{merged: []MergedWork{exhaustFixMR(6 * time.Hour)}}
	m.MergedWork = src

	res, err := m.Apply(context.Background(), "COUNCIL-X", exhaustProposal(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.CreatedItems) != 0 {
		t.Fatalf("created=%d want 0 — exhaust provenance must not exempt a proposal from grounding", len(res.CreatedItems))
	}
	if len(res.MergedWorkSkipped) != 1 {
		t.Fatalf("merged_work_skipped=%d want 1", len(res.MergedWorkSkipped))
	}
	skip := res.MergedWorkSkipped[0]
	if skip.MergedIID != 1450 {
		t.Errorf("merged_iid = %d want 1450", skip.MergedIID)
	}
	if skip.Basis != mergedWorkBasisHard {
		t.Errorf("basis = %q want %q (score %v)", skip.Basis, mergedWorkBasisHard, skip.JaccardScore)
	}
	// A genuine near-duplicate, not a copy: if the fixture ever becomes an
	// exact-title match it stops proving the guard tolerates rewording.
	if skip.JaccardScore < 0.7 || skip.JaccardScore >= 1 {
		t.Errorf("score = %v, want a near-duplicate in [0.7, 1)", skip.JaccardScore)
	}
	if raw := textsim.TitleJaccard(skip.ProposalTitle, skip.MergedTitle); raw >= skip.JaccardScore {
		t.Errorf("fixture no longer exercises work-title normalization: raw %v >= normalized %v", raw, skip.JaccardScore)
	}
	if src.calls != 1 {
		t.Errorf("ListMergedWork calls = %d want 1 — grounding must run once per Apply regardless of source", src.calls)
	}
	all, _ := st.Backlog.List(context.Background())
	if len(all) != 0 {
		t.Errorf("backlog size = %d want 0", len(all))
	}
}

// TestExhaustProposal_HitsBacklogDedup: the same flake proposed on two
// consecutive ticks must collide with the item the first tick authored. The
// exhaust section is stateless — an open issue keeps appearing until it is
// closed — so backlog dedup is what stops it re-minting nightly.
func TestExhaustProposal_HitsBacklogDedup(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	ctx := context.Background()

	first, err := m.Apply(ctx, "COUNCIL-X", exhaustProposal(), MutationOptions{})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if len(first.CreatedItems) != 1 {
		t.Fatalf("first tick created=%d want 1 (nothing should suppress a novel exhaust proposal)", len(first.CreatedItems))
	}

	second, err := m.Apply(ctx, "COUNCIL-Y", exhaustProposal(), MutationOptions{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(second.CreatedItems) != 0 {
		t.Fatalf("second tick created=%d want 0", len(second.CreatedItems))
	}
	if len(second.DuplicatesSkipped) != 1 {
		t.Fatalf("duplicates_skipped=%d want 1", len(second.DuplicatesSkipped))
	}
	if got := second.DuplicatesSkipped[0].SimilarToID; got != first.CreatedItems[0].ID {
		t.Errorf("deduped against %q, want the item the first tick authored (%q)", got, first.CreatedItems[0].ID)
	}
	all, _ := st.Backlog.List(ctx)
	if len(all) != 1 {
		t.Errorf("backlog size = %d want 1", len(all))
	}
}

// TestExhaustProposal_LandsWhenNothingCollides is the other half of "no special
// casing": the guards must not treat exhaust-derived work as suspect either. An
// exhaust proposal with no backlog twin and no merged counterpart lands like
// any other proposal.
func TestExhaustProposal_LandsWhenNothingCollides(t *testing.T) {
	m, st, _ := newMutatorEnv(t)
	// Grounding wired and healthy, corpus simply holds unrelated work.
	m.MergedWork = &stubMergedWork{merged: []MergedWork{{
		IID:      1451,
		Title:    "feat(hud): add a mill-floor panel for spawn pool depth",
		MergedAt: mergedWorkAnchor.Add(-2 * time.Hour),
	}}}

	res, err := m.Apply(context.Background(), "COUNCIL-C", exhaustProposal(), MutationOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.CreatedItems) != 1 {
		t.Fatalf("created=%d want 1", len(res.CreatedItems))
	}
	if len(res.MergedWorkSkipped) != 0 || len(res.DuplicatesSkipped) != 0 {
		t.Errorf("unrelated corpus suppressed an exhaust proposal: merged=%d dedup=%d",
			len(res.MergedWorkSkipped), len(res.DuplicatesSkipped))
	}
	all, _ := st.Backlog.List(context.Background())
	if len(all) != 1 {
		t.Errorf("backlog size = %d want 1", len(all))
	}
}

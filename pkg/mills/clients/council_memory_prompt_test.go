package clients

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mills/council"
)

// fakeCouncilMemory is a MemoryLoader over an in-process journal, so the prompt
// tests can grow a memory across simulated ticks without a database.
type fakeCouncilMemory struct {
	j   *journalengine.Journal
	err error
}

func (f *fakeCouncilMemory) Get(context.Context) (*journalengine.Journal, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.j, nil
}

// tick appends one turn the way runner.recordCouncilMemory does: epoch and the
// displayed ordinal both come from the journal itself, never from a clock.
func (f *fakeCouncilMemory) tick(outcome string) {
	entries := f.j.Entries()
	turns := 0
	for _, e := range entries {
		if e.Kind == journalengine.KindSituation {
			turns++
		}
	}
	f.j.RecordTurn(len(entries), fmt.Sprintf("Council run %d completed.", turns+1), nil, outcome)
}

func newFakeCouncilMemory() *fakeCouncilMemory {
	return &fakeCouncilMemory{j: journalengine.New("council", nil)}
}

// repoLayoutMarker opens the first section BELOW the memory block. Everything
// before it — constant preamble, guardrails, memory render — is the region a
// prefix cache can still match after the memory grows.
const repoLayoutMarker = "\n## Repository layout"

// warmPrefix returns the part of a stable half at and above the memory block.
func warmPrefix(t *testing.T, stable string) string {
	t.Helper()
	i := strings.Index(stable, repoLayoutMarker)
	if i < 0 {
		t.Fatalf("stable half has no repo-layout section to bound the warm prefix:\n%s", stable)
	}
	return stable[:i]
}

// TestCouncilMemory_StableHalfIsStrictPrefixExtension is the kill-test the
// program spec demands before any downstream slice leans on the council lane
// being cache-warm.
//
// The load-bearing assumption is that an append-only memory block inserted into
// the editor's STABLE half extends a warm prefix rather than sitting behind
// volatile bytes. It renders the stable half across four simulated ticks with a
// growing memory and asserts journalengine.CheckPrefixExtension — the same
// consumer-side assertion pkg/journalengine/doc.go demands and
// pkg/mills/pipeline/item_memory_test.go makes for the item journal.
//
// What it asserts on is the WARM PREFIX (constant preamble + guardrails +
// memory render), not the whole stable half, and that distinction is the
// measured result of this slice rather than a weakening of the test. The memory
// is deliberately placed ABOVE repoTree/patterns, so the memory grows in the
// MIDDLE of the stable half: everything below the append point shifts, which
// means the whole stable half can never be a single strict prefix extension.
// What the ordering buys is that the ONLY thing below the append point is the
// small, fixed-size repo digest + pattern catalog, while the unbounded, growing
// block — the memory — sits entirely inside the matchable prefix and stays warm
// across ticks AND across commits (see TestCouncilMemory_SurvivesRepoTreeChurn).
// Placed after repoTree instead, the whole stable half would match within one
// commit and go fully cold on the next — and Mills council ticks are far enough
// apart that a commit between them is the normal case.
func TestCouncilMemory_StableHalfIsStrictPrefixExtension(t *testing.T) {
	t.Setenv(council.MemoryEnv, "1")
	mem := newFakeCouncilMemory()
	// Fixed repo tree: this is the same-commit case, which is what the cache
	// contract is actually about — consecutive ticks within one repo state.
	repoTree := "cmd/\ncmd/loom-mills-operator/\npkg/\npkg/mills/\npkg/mills/clients/"
	patterns := []council.PatternRef{{ID: "PAT-1", Name: "worktree-first"}}

	outcomes := []string{
		"Minted backlog items: none.\nQuality gate: score 0.91, partial=false.\nDisposition: 0 proposed, 0 minted.",
		"Minted backlog items:\n  - MILLS-1: Add council memory\nQuality gate: score 0.88, partial=false.\nDisposition: 1 proposed, 1 minted.",
		"Minted backlog items: none.\nRefused as duplicates of existing work:\n  - \"Add council memory\" ≈ MILLS-1 \"Add council memory\" (jaccard 1.00)\nQuality gate: score 0.80, partial=false.\nDisposition: 1 proposed, 0 minted.",
		"Minted backlog items:\n  - MILLS-2: Wire consolidation seam\nQuality gate: score 0.72, partial=true.\nDisposition: 2 proposed, 1 minted; mutations skipped (council run scored below eval threshold; mutations dropped).",
	}

	var stables []string
	for i, outcome := range outcomes {
		mem.tick(outcome)
		// The brief is deliberately different every tick: it is the volatile
		// half, and a leak of it into `stable` is one of the failures this
		// assertion exists to catch.
		brief := &council.Brief{Markdown: fmt.Sprintf("Tick %d brief, generated %s", i, time.Now().Format(time.RFC3339Nano))}
		stable, _ := buildCouncilEditorPromptParts(brief, nil, patterns, repoTree,
			council.MemoryBlock(context.Background(), mem))
		stables = append(stables, stable)
	}

	if len(stables) < 3 {
		t.Fatalf("need at least 3 renders to assert the contract, got %d", len(stables))
	}
	for i := 1; i < len(stables); i++ {
		if err := journalengine.CheckPrefixExtension(warmPrefix(t, stables[i-1]), warmPrefix(t, stables[i])); err != nil {
			t.Fatalf("prefix cache contract broken between council tick %d and %d: %v", i-1, i, err)
		}
		// Belt and braces: the engine matches on the raw prompt, not on the
		// slice this test carved out, so assert the real strings diverge no
		// earlier than the end of the warm region.
		if div := journalengine.FirstDivergence(stables[i-1], stables[i]); div >= 0 && div < len(warmPrefix(t, stables[i-1])) {
			t.Fatalf("stable half diverged at byte %d, inside the memory region (ends at %d) between ticks %d and %d",
				div, len(warmPrefix(t, stables[i-1])), i-1, i)
		}
	}

	final := stables[len(stables)-1]
	if !strings.Contains(final, council.MemoryPreface) {
		t.Errorf("stable half is missing the memory preface:\n%s", final)
	}
	for _, want := range []string{
		"Council run 1 completed.",
		"Council run 4 completed.",
		"MILLS-2: Wire consolidation seam",
	} {
		if !strings.Contains(final, want) {
			t.Errorf("stable half missing %q", want)
		}
	}
	// The memory must lead the churning sections, or its growth would sit
	// behind a byte that moves whenever the repo does.
	memIdx := strings.Index(final, council.MemoryPreface)
	treeIdx := strings.Index(final, "## Repository layout")
	patIdx := strings.Index(final, "PAT-1")
	if treeIdx < 0 || patIdx < 0 {
		t.Fatalf("stable half lost the repo-layout or pattern section:\n%s", final)
	}
	if memIdx > treeIdx || memIdx > patIdx {
		t.Errorf("memory block at %d must precede repo tree (%d) and patterns (%d)", memIdx, treeIdx, patIdx)
	}
	// A clock reading anywhere above the boundary is the exact silent failure
	// CheckPrefixExtension exists to catch.
	if strings.Contains(final, time.Now().UTC().Format("2006-01-02")) {
		t.Errorf("stable half carries a wall-clock date; the prefix must be time-free:\n%s", final)
	}
	// The volatile brief must never reach the stable half.
	if strings.Contains(final, "Tick 3 brief") {
		t.Errorf("volatile brief leaked into the stable half:\n%s", final)
	}
}

// TestCouncilMemory_SurvivesRepoTreeChurn is the other half of the placement
// claim. The repo digest churns per commit, so consecutive stable halves across
// a commit are NOT a prefix extension end-to-end — but because the memory sits
// ABOVE the digest, every memory byte stays inside the common prefix and stays
// warm. Placed after the digest it would be cold on every commit.
func TestCouncilMemory_SurvivesRepoTreeChurn(t *testing.T) {
	t.Setenv(council.MemoryEnv, "1")
	mem := newFakeCouncilMemory()
	brief := &council.Brief{Markdown: "same brief"}

	mem.tick("Minted backlog items: none.\nDisposition: 0 proposed, 0 minted.")
	before, _ := buildCouncilEditorPromptParts(brief, nil, nil,
		"pkg/\npkg/mills/", council.MemoryBlock(context.Background(), mem))

	mem.tick("Minted backlog items:\n  - MILLS-9: after the commit")
	// A new package appeared: the digest moves, which is the known threat.
	after, _ := buildCouncilEditorPromptParts(brief, nil, nil,
		"pkg/\npkg/mills/\npkg/mills/memory/", council.MemoryBlock(context.Background(), mem))

	div := journalengine.FirstDivergence(before, after)
	if div < 0 {
		t.Fatal("expected the churned repo digest to diverge; it did not")
	}
	end := len(warmPrefix(t, before))
	if div < end {
		t.Fatalf("repo-tree churn invalidated the memory block: first divergence at %d, memory region ends at %d",
			div, end)
	}
	if !strings.Contains(before[:div], "Council run 1 completed.") {
		t.Error("the first run's memory did not survive inside the common prefix across a commit")
	}
}

// Flag off must be byte-identical to the pre-feature prompt, even with a
// populated loader wired: that is the whole safety story for shipping this
// default-OFF.
func TestCouncilMemory_FlagOffIsByteIdentical(t *testing.T) {
	t.Setenv(council.MemoryEnv, "")
	mem := newFakeCouncilMemory()
	mem.tick("Minted backlog items:\n  - MILLS-1: should never render")

	brief := &council.Brief{Markdown: "Ship the thing."}
	repoTree := "pkg/\npkg/mills/"
	patterns := []council.PatternRef{{ID: "PAT-1", Name: "worktree-first"}}

	block := council.MemoryBlock(context.Background(), mem)
	if block != "" {
		t.Fatalf("memory rendered with the flag off: %q", block)
	}
	gotStable, gotVolatile := buildCouncilEditorPromptParts(brief, nil, patterns, repoTree, block)
	wantStable, wantVolatile := buildCouncilEditorPromptParts(brief, nil, patterns, repoTree, "")
	if gotStable != wantStable || gotVolatile != wantVolatile {
		t.Error("flag-off editor prompt is not byte-identical to the pre-feature prompt")
	}
	if strings.Contains(gotStable, council.MemoryPreface) {
		t.Error("memory preface leaked into the flag-off prompt")
	}
}

// A memory that fails to load, or has nothing in it yet, must render nothing —
// the prompt degrades to today's bytes rather than blocking the run.
func TestCouncilMemoryBlock_DegradesToEmpty(t *testing.T) {
	t.Setenv(council.MemoryEnv, "1")
	ctx := context.Background()

	if got := council.MemoryBlock(ctx, nil); got != "" {
		t.Errorf("nil loader rendered %q", got)
	}
	if got := council.MemoryBlock(ctx, newFakeCouncilMemory()); got != "" {
		t.Errorf("empty journal rendered %q, want the empty string", got)
	}
	failing := &fakeCouncilMemory{err: errors.New("db gone")}
	if got := council.MemoryBlock(ctx, failing); got != "" {
		t.Errorf("failing loader rendered %q", got)
	}
}

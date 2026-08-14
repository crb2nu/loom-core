package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// The record hook is the producer half of the kill-test: pkg/mills/clients
// proves the prompt assembly keeps the memory inside a matchable prefix, and
// this proves the journal the runner actually writes is append-only across
// consecutive real council runs — no clock reading, no run id, no map rendered
// unsorted smuggled into the render.
func TestCouncilMemory_RecordsAppendOnlyAcrossRuns(t *testing.T) {
	t.Setenv(council.MemoryEnv, "1")
	env := newRunnerEnv(t, sampleProposals(2))
	ctx := context.Background()

	var renders []string
	for i := 0; i < 3; i++ {
		if _, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerManual}); err != nil {
			t.Fatalf("council run %d: %v", i, err)
		}
		j, err := env.store.CouncilMemory.Get(ctx)
		if err != nil {
			t.Fatalf("reload council memory after run %d: %v", i, err)
		}
		renders = append(renders, j.Render())
	}

	if len(renders) < 3 {
		t.Fatalf("need at least 3 renders to assert the contract, got %d", len(renders))
	}
	for i := 1; i < len(renders); i++ {
		if err := journalengine.CheckPrefixExtension(renders[i-1], renders[i]); err != nil {
			t.Fatalf("prefix cache contract broken between council run %d and %d: %v", i-1, i, err)
		}
	}

	final := renders[len(renders)-1]
	for _, want := range []string{
		"Council run 1 completed.",
		"Council run 2 completed.",
		"Council run 3 completed.",
		"Quality gate: score",
	} {
		if !strings.Contains(final, want) {
			t.Errorf("council memory render missing %q:\n%s", want, final)
		}
	}
	// Run 2 and 3 re-propose run 1's titles, so the dedup refusal must be
	// recorded — that is the single most useful thing a later tick can read.
	if !strings.Contains(final, "Refused as duplicates of existing work:") {
		t.Errorf("dedup refusals were not journaled:\n%s", final)
	}
	// The epoch must be journal-derived, never a clock.
	if strings.Contains(final, time.Now().UTC().Format("2006-01-02")) {
		t.Errorf("council memory render carries a wall-clock date:\n%s", final)
	}
	// Epochs are strictly increasing per turn and identical within one.
	j, err := env.store.CouncilMemory.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := CouncilMemoryTurns(j); got != 3 {
		t.Errorf("recorded %d turns, want 3", got)
	}
	prevEpoch := -1
	for i, e := range j.Entries() {
		switch {
		case e.Kind == journalengine.KindSituation:
			if e.Epoch <= prevEpoch {
				t.Fatalf("entry %d epoch = %d, not greater than the previous turn's %d", i, e.Epoch, prevEpoch)
			}
			prevEpoch = e.Epoch
		case e.Epoch != prevEpoch:
			t.Fatalf("entry %d epoch = %d, want %d (same turn)", i, e.Epoch, prevEpoch)
		}
	}
}

func TestCouncilMemory_DisabledByDefault(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()

	if _, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerManual}); err != nil {
		t.Fatalf("council run: %v", err)
	}
	j, err := env.store.CouncilMemory.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := j.Render(); got != journalengine.EmptyJournal {
		t.Errorf("council memory recorded with the knob off: %q", got)
	}
}

// A dryrun is deliberately side-effect free; writing lane memory from one would
// let a rehearsal claim work the audit trail lacks.
func TestCouncilMemory_DryrunRecordsNothing(t *testing.T) {
	t.Setenv(council.MemoryEnv, "1")
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()

	if _, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerManual, Dryrun: true}); err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	j, err := env.store.CouncilMemory.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := j.Render(); got != journalengine.EmptyJournal {
		t.Errorf("dryrun wrote council memory: %q", got)
	}
}

// A DAO failure must be logged and swallowed: by the record point the artifacts
// are on disk and the run row is committed, so failing here would erase real
// work.
func TestCouncilMemory_RecordSurvivesStoreFailure(t *testing.T) {
	t.Setenv(council.MemoryEnv, "1")
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()

	if _, err := env.store.DB().ExecContext(ctx, `DROP TABLE council_memory`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerManual}); err != nil {
		t.Fatalf("council run must survive a broken memory table: %v", err)
	}
}

// The 256 KiB cap refuses rather than truncates, and the refusal must not fail
// the run that hit it.
func TestCouncilMemory_OversizedRecordIsSkipped(t *testing.T) {
	t.Setenv(council.MemoryEnv, "1")
	env := newRunnerEnv(t, sampleProposals(1))
	ctx := context.Background()

	// Seed the journal right up to the cap: grow it until Put refuses, then
	// persist the last snapshot that fit. The next recorded run is therefore
	// guaranteed to hit the refusal path rather than depending on arithmetic.
	j := journalengine.New(store.CouncilMemoryLane, nil)
	var lastGood *journalengine.Journal
	// Coarse fill to get near the cap, then a fine fill so the remaining
	// headroom is smaller than one real recorded outcome.
	for _, chunk := range []int{councilMemoryMaxOwnBytes, 16} {
		for i := 0; i < 4096; i++ {
			j.RecordTurn(len(j.Entries()), "Council run seed completed.", nil, strings.Repeat("x", chunk))
			if err := env.store.CouncilMemory.Put(ctx, j); err != nil {
				if !errors.Is(err, store.ErrCouncilMemoryTooLarge) {
					t.Fatalf("seed: %v", err)
				}
				j = journalengine.FromSnapshot(lastGood.Snapshot())
				break
			}
			lastGood = journalengine.FromSnapshot(j.Snapshot())
		}
	}
	if lastGood == nil {
		t.Fatal("could not seed a journal under the cap")
	}
	if err := env.store.CouncilMemory.Put(ctx, lastGood); err != nil {
		t.Fatalf("re-seed the last good snapshot: %v", err)
	}
	seededTurns := CouncilMemoryTurns(lastGood)

	if _, err := env.runner.Run(ctx, RunInput{Trigger: store.CouncilTriggerManual}); err != nil {
		t.Fatalf("council run must survive an over-cap memory: %v", err)
	}

	got, err := env.store.CouncilMemory.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if turns := CouncilMemoryTurns(got); turns != seededTurns {
		t.Errorf("recorded %d turns, want the seeded %d — the over-cap record was not refused", turns, seededTurns)
	}
	snap, err := got.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(snap) > store.CouncilMemoryMaxSnapshotBytes {
		t.Errorf("persisted snapshot = %d bytes, over the %d cap", len(snap), store.CouncilMemoryMaxSnapshotBytes)
	}
}

func TestCouncilMemoryOutcome_Composition(t *testing.T) {
	out := councilMemoryOutcome(
		&council.EditorOutput{},
		&eval.Verdict{Score: 0.72, Partial: true},
		&council.MutationResult{
			TotalProposed: 3,
			Truncated:     1,
			CreatedItems: []*store.BacklogItem{
				{ID: "MILLS-1", Title: "Add\ncouncil memory"},
			},
			RoutedPlanLane: []string{"plan-7"},
			DuplicatesSkipped: []council.DuplicateSkipped{
				{ProposalTitle: "Add council memory again", SimilarToID: "MILLS-1", SimilarTitle: "Add council memory", JaccardScore: 0.91},
			},
			PlanDuplicatesSkipped: []council.PlanDuplicateSkipped{
				{ProposalTitle: "Wire consolidation", SimilarPlanID: "plan-3", SimilarTitle: "Consolidation seam", JaccardScore: 0.77},
			},
			Skipped:    true,
			SkipReason: "council run scored below eval threshold; mutations dropped",
		},
	)
	for _, want := range []string{
		"MILLS-1: Add council memory",
		"Routed to the plan lane: plan-7",
		"Refused as duplicates of existing work:",
		`"Add council memory again" ≈ MILLS-1 "Add council memory" (jaccard 0.91)`,
		"Refused as duplicates of existing plans:",
		"plan plan-3",
		"Quality gate: score 0.72, partial=true.",
		"Disposition: 3 proposed, 1 minted, 1 dropped over the per-run cap; mutations skipped (council run scored below eval threshold; mutations dropped).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composed outcome missing %q:\n%s", want, out)
		}
	}
	// A multi-line title must not be able to impersonate the section structure.
	if strings.Contains(out, "Add\ncouncil memory") {
		t.Errorf("a multi-line title survived into the composition:\n%s", out)
	}
}

func TestCouncilMemoryOutcome_TruncatesFromTheTail(t *testing.T) {
	created := make([]*store.BacklogItem, 0, 4096)
	for i := 0; i < 4096; i++ {
		created = append(created, &store.BacklogItem{ID: "MILLS-XXXXXXXXXXXX", Title: strings.Repeat("t", 32)})
	}
	out := councilMemoryOutcome(nil, &eval.Verdict{Score: 1}, &council.MutationResult{
		TotalProposed: len(created),
		CreatedItems:  created,
	})
	if len(out) > councilMemoryMaxOwnBytes {
		t.Fatalf("composed outcome = %d bytes, want <= %d", len(out), councilMemoryMaxOwnBytes)
	}
	if !strings.HasSuffix(out, councilMemoryTruncationMarker) {
		t.Errorf("truncated outcome does not carry the marker:\n%s", out[len(out)-80:])
	}
	// Mints lead the composition, so truncation keeps them.
	if !strings.Contains(out, "Minted backlog items:") {
		t.Error("truncation dropped the mint list; it must outrank the trailing sections")
	}
}

func TestCouncilMemoryEnabled_ParsesTruthyForms(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(council.MemoryEnv, v)
		if !council.MemoryEnabled() {
			t.Errorf("%q should enable the council memory", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "no", "maybe"} {
		t.Setenv(council.MemoryEnv, v)
		if council.MemoryEnabled() {
			t.Errorf("%q should not enable the council memory", v)
		}
	}
}

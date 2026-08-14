package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// councilMemoryMaxOwnBytes caps one recorded council run outcome. The journal is
// re-sent in every subsequent editor prompt, so an uncapped composition would be
// paid for on every later tick.
const councilMemoryMaxOwnBytes = 8 << 10

// councilMemoryTruncationMarker terminates a tail-truncated outcome so a reader
// (human or model) can tell elision from a run that simply did little.
const councilMemoryTruncationMarker = "\n[... truncated]"

// recordCouncilMemory appends one turn to the council lane's durable journal,
// after the run's own outcome is durable: artifacts are on disk, the verdict is
// persisted, and the mutator has applied (or deliberately skipped) the backlog
// deltas. Same discipline as Runner.recordItemMemory being called after
// PutStage — the journal must never claim work the audit trail lacks.
//
// Best-effort by contract: every failure path logs and returns. A memory write
// must never fail a council run that already produced artifacts.
func (r *Runner) recordCouncilMemory(
	ctx context.Context,
	out *council.EditorOutput,
	verdict *eval.Verdict,
	mutation *council.MutationResult,
) {
	if !council.MemoryEnabled() {
		return
	}
	if r == nil || r.Store == nil || r.Store.CouncilMemory == nil {
		return
	}
	j, err := r.Store.CouncilMemory.Get(ctx)
	if err != nil {
		r.logf("council memory: load failed; run outcome not journaled", "error", err)
		return
	}
	// Epoch is the count of entries already in the journal, never a clock
	// reading: a timestamp anywhere in the render is the exact volatile-byte
	// failure CheckPrefixExtension exists to catch, and it would drop the
	// prefix-cache hit rate to zero silently. The displayed run ordinal is
	// derived from the journal too — the number of turns already recorded —
	// for the same reason (and because a run id would be volatile).
	epoch := len(j.Entries())
	j.RecordTurn(epoch, councilMemorySituation(CouncilMemoryTurns(j)+1), nil,
		councilMemoryOutcome(out, verdict, mutation))

	if err := r.Store.CouncilMemory.Put(ctx, j); err != nil {
		if errors.Is(err, store.ErrCouncilMemoryTooLarge) {
			r.logf("council memory: snapshot over cap; run outcome skipped",
				"cap_bytes", store.CouncilMemoryMaxSnapshotBytes)
			return
		}
		r.logf("council memory: persist failed; run outcome not journaled", "error", err)
	}
}

// CouncilMemoryTurns counts the turns already recorded in a council journal —
// one KindSituation entry per completed run. Exported so the operator and tests
// can report the lane's depth without reimplementing the count.
func CouncilMemoryTurns(j *journalengine.Journal) int {
	if j == nil {
		return 0
	}
	n := 0
	for _, e := range j.Entries() {
		if e.Kind == journalengine.KindSituation {
			n++
		}
	}
	return n
}

// councilMemorySituation is the "world" half of a recorded turn. The ordinal is
// derived from the journal's own turn count — reproducible on replay, and free
// of any clock reading or run id (a run id is volatile, and every byte here
// lands above the now-block boundary).
func councilMemorySituation(ordinal int) string {
	return fmt.Sprintf("Council run %d completed.", ordinal)
}

// councilMemoryOutcome composes the "own response" half: what this run minted,
// what dedup refused, how the quality gate ruled, and what the mutator finally
// did with it. Neutral third person, because the journal is replayed to the
// editor as fact rather than as its own recollection.
//
// Ordering is a truncation policy: proposals first (the thing a later tick most
// needs so it does not re-mint them), then the refusals that shape the next
// proposal set, then the gate outcome, then the dispositions. Overflow costs the
// tail, not the mints.
func councilMemoryOutcome(out *council.EditorOutput, verdict *eval.Verdict, mutation *council.MutationResult) string {
	var b strings.Builder

	created := 0
	if mutation != nil {
		created = len(mutation.CreatedItems)
	}
	if created > 0 {
		b.WriteString("Minted backlog items:")
		for _, item := range mutation.CreatedItems {
			if item == nil {
				continue
			}
			fmt.Fprintf(&b, "\n  - %s: %s", item.ID, oneLine(item.Title))
		}
	} else {
		b.WriteString("Minted backlog items: none.")
	}
	if mutation != nil && len(mutation.RoutedPlanLane) > 0 {
		fmt.Fprintf(&b, "\nRouted to the plan lane: %s", strings.Join(mutation.RoutedPlanLane, ", "))
	}

	// Dedup + gray-band refusals share one result field; both are "this theme
	// already exists", which is exactly what a later tick must not re-propose.
	if mutation != nil && len(mutation.DuplicatesSkipped) > 0 {
		b.WriteString("\nRefused as duplicates of existing work:")
		for _, d := range mutation.DuplicatesSkipped {
			fmt.Fprintf(&b, "\n  - %q ≈ %s %q (jaccard %.2f)",
				oneLine(d.ProposalTitle), d.SimilarToID, oneLine(d.SimilarTitle), d.JaccardScore)
		}
	}
	if mutation != nil && len(mutation.PlanDuplicatesSkipped) > 0 {
		b.WriteString("\nRefused as duplicates of existing plans:")
		for _, d := range mutation.PlanDuplicatesSkipped {
			fmt.Fprintf(&b, "\n  - %q ≈ plan %s %q (jaccard %.2f)",
				oneLine(d.ProposalTitle), d.SimilarPlanID, oneLine(d.SimilarTitle), d.JaccardScore)
		}
	}

	// The eval judge is the gate that decides whether this run's proposals were
	// allowed to become work at all; the mutator's skip is that gate's effect.
	if verdict != nil {
		fmt.Fprintf(&b, "\nQuality gate: score %.2f, partial=%t.", verdict.Score, verdict.Partial)
	}
	if mutation != nil {
		fmt.Fprintf(&b, "\nDisposition: %d proposed, %d minted", mutation.TotalProposed, created)
		if mutation.Truncated > 0 {
			fmt.Fprintf(&b, ", %d dropped over the per-run cap", mutation.Truncated)
		}
		if mutation.Skipped {
			reason := oneLine(mutation.SkipReason)
			if reason == "" {
				reason = "unspecified"
			}
			fmt.Fprintf(&b, "; mutations skipped (%s)", reason)
		}
		b.WriteString(".")
	}
	if out != nil && out.Empty {
		b.WriteString("\nEditor returned an empty response; this run produced no usable synthesis.")
	}

	return truncateCouncilTail(b.String(), councilMemoryMaxOwnBytes)
}

// oneLine collapses arbitrary text to a single space-separated line so a
// body-heavy title or skip reason cannot impersonate the section structure of
// the composed outcome.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateCouncilTail cuts s to at most maxBytes, replacing the removed tail
// with a marker. Cuts back to a rune boundary so the result is still valid
// UTF-8. Mirrors pipeline.truncateTailBytes, which cannot be shared: pkg/mills/
// pipeline imports pkg/mills/council, so the helper cannot live there without a
// cycle for the council-side caller.
func truncateCouncilTail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	keep := maxBytes - len(councilMemoryTruncationMarker)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !councilRuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + councilMemoryTruncationMarker
}

// councilRuneStart reports whether b can begin a UTF-8 rune (i.e. is not a
// continuation byte).
func councilRuneStart(b byte) bool { return b&0xC0 != 0x80 }

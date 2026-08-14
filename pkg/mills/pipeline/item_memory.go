package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// ItemJournalEnv gates the per-backlog-item memory journal: both the runner's
// record hook and the operator's prompt render hook read it, so one variable
// turns the whole feature on or off. Default OFF.
const ItemJournalEnv = "LOOM_MILLS_ITEM_JOURNAL"

// itemMemoryMaxOwnBytes caps one recorded stage outcome. The journal is
// re-sent in every subsequent stage prompt, so an uncapped log tail would be
// paid for repeatedly.
const itemMemoryMaxOwnBytes = 8 << 10

// itemMemoryTruncationMarker terminates a tail-truncated outcome so a reader
// (human or model) can tell elision from a stage that simply said little.
const itemMemoryTruncationMarker = "\n[... truncated]"

// itemMemorySoftThresholdBytes is half the hard row cap: the point at which a
// journal is still persisted normally but is on a trajectory toward the refusal
// path, and therefore the point worth telling an operator about.
//
// Derived from the store constant rather than restated, so raising the cap
// cannot silently leave the warning pinned to a stale fraction of it.
const itemMemorySoftThresholdBytes = store.ItemMemoryMaxSnapshotBytes / 2

// itemMemoryConsolidationKeepFraction is the share of entries a consolidation
// keeps verbatim; the rest are distilled into the identity passage and the
// episodic ledger. Half is the balance journalengine's own tests exercise:
// enough reclaimed to be worth an LLM call, enough recent detail kept that the
// next stage prompt still reads as a continuous history.
const itemMemoryConsolidationKeepFraction = 0.5

// itemMemoryConsolidationTimeout bounds the distillation call.
//
// It runs synchronously on the stage-completion path, and the shared FlexInfer
// client's own timeout is five minutes — sized for a research stage, not for a
// best-effort memory write. Journal work must never fail a stage that already
// succeeded, and stalling one for minutes is the same harm by a slower route:
// on expiry the wrapper persists the unconsolidated journal, exactly as it does
// for any other consolidator error.
const itemMemoryConsolidationTimeout = 90 * time.Second

// ItemJournalEnabled reports whether the per-backlog-item memory journal is on.
func ItemJournalEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ItemJournalEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// recordItemMemory appends one stage outcome to the item's durable journal,
// immediately after the stage result itself is durable.
//
// Best-effort by contract: every failure path logs and returns. A memory write
// must never fail a stage that already succeeded, and must never mask the error
// of one that did not.
func (r *Runner) recordItemMemory(
	ctx context.Context,
	item *store.BacklogItem,
	stage Stage,
	attempt int,
	out StageOutput,
	logTail string,
	stageErr error,
) {
	if !ItemJournalEnabled() || item == nil || item.ID == "" {
		return
	}
	if r.Store == nil || r.Store.ItemMemory == nil {
		return
	}
	j, err := r.Store.ItemMemory.Get(ctx, item.ID)
	if err != nil {
		r.logger().Warn("item memory: load failed; stage outcome not journaled",
			"item", item.ID, "stage", stage.ID, "error", err)
		return
	}
	// Epoch is the count of entries already in the journal, never a clock
	// reading: a timestamp anywhere in the render is the exact volatile-byte
	// failure CheckPrefixExtension exists to catch, and it would drop the
	// prefix-cache hit rate to zero silently.
	epoch := len(j.Entries())
	j.RecordTurn(epoch, itemMemorySituation(stage.ID, attempt), nil,
		itemMemoryOutcome(out, logTail, stageErr))

	// Growth check between RecordTurn and Put: the journal about to be written
	// is the thing the cap will judge, and a consolidation must land before the
	// write, not after a refusal.
	if r.observeItemMemoryGrowth(item.ID, stage.ID, j) {
		r.consolidateItemMemory(ctx, item.ID, stage.ID, j)
	}

	if err := r.Store.ItemMemory.Put(ctx, item.ID, j); err != nil {
		if errors.Is(err, store.ErrItemMemoryTooLarge) {
			r.logger().Warn("item memory: snapshot over cap; stage outcome skipped",
				"item", item.ID, "stage", stage.ID, "cap_bytes", store.ItemMemoryMaxSnapshotBytes)
			return
		}
		r.logger().Warn("item memory: persist failed; stage outcome not journaled",
			"item", item.ID, "stage", stage.ID, "error", err)
	}
}

// observeItemMemoryGrowth reports whether the journal about to be persisted is
// over the soft threshold, warning and counting once when it is.
//
// Unconditional: it runs with or without LOOM_MILLS_MEMORY_CONSOLIDATE, because
// its whole purpose is to answer the question dao_item_memory.go's v1 note left
// open — is the cap actually biting? Before this, the only signal was the Warn
// at the hard refusal, by which point the item's memory had already stopped
// growing silently.
//
// A marshal failure returns false rather than guessing: Put will hit the same
// failure a moment later and report it properly.
func (r *Runner) observeItemMemoryGrowth(itemID, stageID string, j *journalengine.Journal) bool {
	if j == nil {
		return false
	}
	// Byte-identical to what ItemMemoryDAO.Put encodes and measures
	// (json.Marshal of the same Snapshot), so the threshold and the cap are
	// counting the same bytes.
	snap, err := j.MarshalJSON()
	if err != nil {
		return false
	}
	if len(snap) <= itemMemorySoftThresholdBytes {
		return false
	}
	mills.ItemMemorySoftThresholdTotal.Inc()
	r.logger().Warn("item memory: snapshot over soft threshold",
		"item", itemID, "stage", stageID,
		"snapshot_bytes", len(snap),
		"soft_threshold_bytes", itemMemorySoftThresholdBytes,
		"cap_bytes", store.ItemMemoryMaxSnapshotBytes,
		"consolidate_enabled", MemoryConsolidateEnabled())
	return true
}

// consolidateItemMemory distils the journal's oldest entries in place, at most
// once per record call, when the flag is on and a consolidator is wired.
//
// Failure is deliberately quiet at the caller: journalengine.Consolidate leaves
// the journal completely untouched on any error and on an empty result, so the
// caller goes on to persist the grown-but-unconsolidated journal exactly as it
// would have with this feature absent. Running one turn over budget is
// recoverable; discarding history is not.
func (r *Runner) consolidateItemMemory(ctx context.Context, itemID, stageID string, j *journalengine.Journal) {
	if !MemoryConsolidateEnabled() || r.MemoryConsolidator == nil || j == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, itemMemoryConsolidationTimeout)
	defer cancel()
	before := len(j.Entries())
	_, dropped, err := journalengine.Consolidate(cctx, j, r.MemoryConsolidator, itemMemoryConsolidationKeepFraction)
	switch {
	case err != nil:
		mills.ItemMemoryConsolidationsTotal.WithLabelValues("error").Inc()
		r.logger().Warn("item memory: consolidation failed; persisting unconsolidated journal",
			"item", itemID, "stage", stageID, "entries", before, "error", err)
	case dropped == 0:
		// Nothing old enough to split — not a failure, and not worth a warn.
		mills.ItemMemoryConsolidationsTotal.WithLabelValues("noop").Inc()
	default:
		mills.ItemMemoryConsolidationsTotal.WithLabelValues("ok").Inc()
		r.logger().Info("item memory: consolidated oldest entries",
			"item", itemID, "stage", stageID,
			"entries_before", before, "entries_dropped", dropped,
			"entries_after", len(j.Entries()),
			"consolidations", j.Consolidations())
	}
}

// itemMemorySituation is the "world" half of a recorded turn: which stage ran,
// and which attempt of it.
func itemMemorySituation(stageID string, attempt int) string {
	return fmt.Sprintf("Pipeline stage %q ran (attempt %d).", stageID, attempt)
}

// itemMemoryOutcome composes the "own response" half: the verdict, then the
// diff STAT (never the patch — the full diff is capped at 32 KiB per attempt
// and re-sending it every stage would dwarf everything else), then commit
// messages, then the log tail.
//
// The log tail goes last on purpose: the composition is truncated from the
// tail, so overflow costs log noise rather than the structured summary above
// it.
func itemMemoryOutcome(out StageOutput, logTail string, stageErr error) string {
	var b strings.Builder
	if stageErr != nil {
		fmt.Fprintf(&b, "Outcome: FAILED — %s", stageErr.Error())
	} else {
		b.WriteString("Outcome: succeeded.")
	}
	if stat := itemMemoryDiffStat(out); stat != "" {
		b.WriteString("\n")
		b.WriteString(stat)
	}
	if len(out.CommitMessages) > 0 {
		b.WriteString("\nCommits:")
		for _, msg := range out.CommitMessages {
			msg = strings.TrimSpace(msg)
			if msg == "" {
				continue
			}
			// Collapse to one line per commit so a body-heavy message cannot
			// impersonate the section structure above.
			fmt.Fprintf(&b, "\n  - %s", strings.Join(strings.Fields(msg), " "))
		}
	}
	if tail := strings.TrimSpace(logTail); tail != "" {
		b.WriteString("\nLog tail:\n")
		b.WriteString(tail)
	}
	return truncateTailBytes(b.String(), itemMemoryMaxOwnBytes)
}

// itemMemoryDiffStat summarises what the stage changed without carrying the
// patch bytes.
func itemMemoryDiffStat(out StageOutput) string {
	if len(out.FilesChanged) == 0 && out.LinesAdded == 0 && out.LinesRemoved == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Diff: %d file(s), +%d/-%d", len(out.FilesChanged), out.LinesAdded, out.LinesRemoved)
	if len(out.FilesChanged) > 0 {
		fmt.Fprintf(&b, " — %s", strings.Join(out.FilesChanged, ", "))
	}
	return b.String()
}

// truncateTailBytes cuts s to at most maxBytes, replacing the removed tail with
// a marker. Cuts back to a rune boundary so the result is still valid UTF-8.
func truncateTailBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	keep := maxBytes - len(itemMemoryTruncationMarker)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + itemMemoryTruncationMarker
}

// utf8RuneStart reports whether b can begin a UTF-8 rune (i.e. is not a
// continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

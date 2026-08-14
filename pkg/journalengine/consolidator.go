package journalengine

import (
	"context"
	"fmt"
	"strings"
)

// Consolidation is what one distillation produces: two artifacts with
// deliberately different survival rules.
//
// Identity is in-voice prose describing who the agent has become, and is
// re-synthesized every consolidation — this part *should* drift.
//
// Ledger is neutral, third-person event lines ("[Epochs 4-9] the deploy was
// rolled back twice; the cause is still unknown"), and is append-only: a later
// consolidation may add lines but never rewrites earlier ones.
//
// The split exists because summarizing a summary repeatedly preserves *register*
// and destroys *events*. Each pass paraphrases the previous paraphrase, so
// whatever is most characteristic survives and whatever is merely what happened
// evaporates. Keeping the event lines out of the re-synthesis loop is the fix.
//
// A useful side effect: because ledger lines are neutral third-person summaries,
// they are also the right keys to embed if the caller archives dropped epochs in
// a vector store. In-character prose is a poor key — a corpus in a strong
// consistent register is register-saturated, so every record looks alike to a
// plain-language query. The Python libs/journal-engine carries that archive layer
// and a kill-test for the rule; this port covers the journal primitives only.
type Consolidation struct {
	Identity string
	Ledger   []string
}

// IsEmpty reports whether the consolidation carries nothing usable. Applying an
// empty consolidation would drop entries without recording anything about them.
func (c Consolidation) IsEmpty() bool {
	return strings.TrimSpace(c.Identity) == "" && len(c.Ledger) == 0
}

// ConsolidationRequest is everything a Consolidator needs to distil a span of a
// journal. The prompt, the model, and the transport all stay on the caller's
// side of this boundary — this package never performs I/O.
type ConsolidationRequest struct {
	// Owner names whose memory is being distilled.
	Owner string
	// PriorIdentity is the identity passage this consolidation replaces, or ""
	// on the first consolidation. Pass it to the model so the new passage
	// integrates the old rather than starting over — and compare against it if
	// you want to detect a consolidation that merely paraphrased.
	PriorIdentity string
	// Entries are the oldest entries being dropped, in order. RenderEntries
	// formats them for a prompt.
	Entries []Entry
	// EpochStart and EpochEnd bound the span, for stamping ledger lines.
	EpochStart int
	EpochEnd   int
}

// Consolidator turns a span of journal entries into a Consolidation.
//
// The LLM call lives here, on the caller's side: implementations own the prompt,
// the model choice, the lane, retries, and any similarity guard against
// PriorIdentity. This package deliberately knows nothing about any of that, so
// it stays testable without a service and reusable across runtimes.
type Consolidator interface {
	Consolidate(ctx context.Context, req ConsolidationRequest) (Consolidation, error)
}

// ConsolidatorFunc adapts a plain function to Consolidator.
type ConsolidatorFunc func(ctx context.Context, req ConsolidationRequest) (Consolidation, error)

// Consolidate implements Consolidator.
func (f ConsolidatorFunc) Consolidate(ctx context.Context, req ConsolidationRequest) (Consolidation, error) {
	return f(ctx, req)
}

// Consolidate distils a journal's oldest entries and applies the result,
// reclaiming context budget.
//
// It keeps roughly the newest keepFraction of entries. On any error from the
// Consolidator the journal is left completely untouched and the error is
// returned: running one turn over budget is recoverable, silently discarding
// history is not. Same for an empty result — dropping entries in exchange for
// nothing is worse than not consolidating.
//
// Returns the applied Consolidation and the number of entries dropped.
func Consolidate(
	ctx context.Context,
	j *Journal,
	c Consolidator,
	keepFraction float64,
) (Consolidation, int, error) {
	if j == nil {
		return Consolidation{}, 0, fmt.Errorf("journalengine: nil journal")
	}
	if c == nil {
		return Consolidation{}, 0, fmt.Errorf("journalengine: nil consolidator")
	}

	old, firstKept := j.SplitOldest(keepFraction)
	if len(old) == 0 {
		return Consolidation{}, 0, nil
	}

	req := ConsolidationRequest{
		Owner:         j.Owner(),
		PriorIdentity: latestIdentity(j.CoreMemories()),
		Entries:       old,
		EpochStart:    old[0].Epoch,
		EpochEnd:      old[len(old)-1].Epoch,
	}

	result, err := c.Consolidate(ctx, req)
	if err != nil {
		return Consolidation{}, 0, fmt.Errorf("journalengine: consolidate %s: %w", j.Owner(), err)
	}
	if result.IsEmpty() {
		return Consolidation{}, 0, fmt.Errorf(
			"journalengine: consolidate %s: empty result for epochs %d-%d; journal left intact",
			j.Owner(), req.EpochStart, req.EpochEnd,
		)
	}

	j.ApplyConsolidation(result, firstKept)
	return result, len(old), nil
}

func latestIdentity(coreMemories []string) string {
	if len(coreMemories) == 0 {
		return ""
	}
	return coreMemories[len(coreMemories)-1]
}

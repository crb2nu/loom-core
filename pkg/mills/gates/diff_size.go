package gates

import (
	"context"
	"fmt"
	"strings"
)

// defaultMaxDiffLines is the per-item ceiling when neither the policy's
// per-label override nor the item's budget supplies one. Tuned for "one
// reviewable change per backlog item"; larger PRs get auto-split during
// council planning, not waved through.
const defaultMaxDiffLines = 800

// DiffSize fails when the implement stage's diff is larger than the
// per-item ceiling. Caps the blast radius of any single auto-merged
// change and turns "the agent went on a tear" into an escalation rather
// than a 5,000-line surprise on main.
type DiffSize struct {
	// MaxLines overrides the per-instance ceiling for tests. Zero falls
	// back to the policy / item override / package default.
	MaxLines int
}

// Name identifies the gate in persistence + logs.
func (g *DiffSize) Name() string { return "diff_size" }

// Evaluate compares (LinesAdded + LinesRemoved) against the ceiling
// derived from (gate override → per-item policy → package default).
func (g *DiffSize) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	limit := g.MaxLines
	itemOverride := false
	if limit <= 0 {
		limit, itemOverride = effectiveDiffLimit(in)
	}
	added, removed := in.LinesAdded, in.LinesRemoved
	total := added + removed
	// Telemetry fallback: the Codex spawn parser reports 0/0 line counts
	// (internal/hud/spawn_codex_parser.go), so a real — possibly oversized —
	// Codex diff arrives with total==0 and sails past the cap, defeating the
	// blast-radius guard (the gate fails OPEN for every Codex implement). When
	// the line-count telemetry is absent but the raw patch is present
	// (attachGitContext populates DiffPatch from `git diff`), recover the real
	// counts from the patch. This only fires on the telemetry gap: a genuinely
	// empty diff carries no DiffPatch, so it still reads as 0 and passes.
	if total == 0 && len(in.DiffPatch) > 0 {
		added, removed = CountDiffLines(in.DiffPatch)
		total = added + removed
	}
	if total <= limit {
		return pass(), nil
	}
	capDescription := fmt.Sprintf("%d", limit)
	if itemOverride {
		capDescription += " (item override)"
	}
	return fail(fmt.Sprintf(
		"diff is %d lines (added %d, removed %d); cap is %s",
		total, added, removed, capDescription,
	)), nil
}

// countDiffLines counts added/removed content lines in a unified diff,
// excluding the +++/--- file headers (mirrors gates.addedLines). Used as a
// fallback for the diff-size gate when the spawn parser did not populate
// line-count telemetry.
// CountDiffLines counts added/removed content lines in a unified diff,
// excluding the +++/--- file headers. It is exported so read models can use
// the exact same fallback as the enforcement gate.
func CountDiffLines(patch []byte) (added, removed int) {
	for _, raw := range strings.Split(string(patch), "\n") {
		switch {
		case strings.HasPrefix(raw, "+++"), strings.HasPrefix(raw, "---"):
			continue
		case strings.HasPrefix(raw, "+"):
			added++
		case strings.HasPrefix(raw, "-"):
			removed++
		}
	}
	return added, removed
}

// effectiveDiffLimit returns the positive item override when one is present,
// otherwise the package default. The boolean marks an item override so failure
// reasons make the loosened (or tightened) ceiling auditable.
func effectiveDiffLimit(in StageInput) (limit int, itemOverride bool) {
	if in.MaxDiffLines > 0 {
		return in.MaxDiffLines, true
	}
	return defaultMaxDiffLines, false
}

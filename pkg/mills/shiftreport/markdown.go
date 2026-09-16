package shiftreport

import (
	"fmt"
	"strings"
	"time"
)

// RenderMarkdown renders the stable stand-up artifact from an already
// composed report. rows must be the eligible departures (Compose passes its
// filtered set); excluded outcomes never reach the tables.
func RenderMarkdown(r Report, rows []Departure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Mills shift report — %s UTC\n\n", r.GeneratedAt.UTC().Format("2006-01-02 15:04"))
	for _, line := range r.NarrativeLines {
		fmt.Fprintln(&b, line)
	}
	if h := r.MainRedExternalHold; h != nil {
		fmt.Fprintf(&b, "\n## Merge queue hold\n\n| project | branch | state | expires (UTC) | reason |\n|---|---|---|---|---|\n| %s | %s | %s | %s | main_red_external |\n", escape(h.Project), escape(h.Branch), h.state(r.GeneratedAt), h.ExpiresAt.UTC().Format(time.RFC3339))
	}
	status := "NOT BREACHED"
	if r.ThroughputGuardrail.Breached {
		status = "BREACHED"
	}
	fmt.Fprintf(&b, "\n## Throughput guardrail — %s\n", status)
	if len(r.ThroughputGuardrail.Reasons) == 0 {
		fmt.Fprintln(&b, "\nAll reported throughput signals are within policy thresholds.")
	} else {
		for _, reason := range r.ThroughputGuardrail.Reasons {
			fmt.Fprintf(&b, "\n- %s", reason)
		}
		fmt.Fprintln(&b)
	}
	if len(r.Bolts) > 0 {
		fmt.Fprintln(&b, "\n## Bolts\n| when (UTC) | item | change | eval | cost |\n|---|---|---:|---:|---:|")
		for _, row := range rows {
			if Kind(row.Card) != KindBolt {
				continue
			}
			c := row.Card
			item := escape(c.Title)
			if item == "" {
				item = escape(cardID(c))
			}
			if c.MR.IID != nil {
				item += fmt.Sprintf(" (!%d)", *c.MR.IID)
			}
			eval := "—"
			if c.Eval.MinScore != nil {
				eval = fmt.Sprintf("%.2f", *c.Eval.MinScore)
			}
			fmt.Fprintf(&b, "| %s | %s | %d files +%d/-%d | %s | $%.2f |\n", row.EndedAt.UTC().Format("15:04"), item, c.Diff.Files, c.Diff.Added, c.Diff.Removed, eval, c.CostUSD)
		}
	}
	if len(r.Sparks) > 0 {
		fmt.Fprintln(&b, "\n## Sparks\n| when (UTC) | item | class | signature | week occurrences |\n|---|---|---|---|---:|")
		for _, row := range rows {
			if Kind(row.Card) != KindSpark {
				continue
			}
			c := row.Card
			class, sig, occ := "—", "—", 0
			if c.Escalation != nil {
				class = escape(c.Escalation.Class)
				sig = escape(c.Escalation.Signature)
				occ = c.Escalation.WeekOccurrences
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %d |\n", row.EndedAt.UTC().Format("15:04"), escape(cardID(c)), class, sig, occ)
		}
	}
	fmt.Fprintf(&b, "\n## Taste\n| graded this shift | bolts this shift | coverage 14d | gate | ranker | last grade |\n|---:|---:|---:|---:|---|---|\n| %d | %d | %.1f%% | %.1f%% | %s | %s |\n", r.Taste.GradedThisShift, r.Taste.BoltsThisShift, r.Taste.Coverage14d*100, r.Taste.CoverageGate*100, map[bool]string{true: "armed", false: "disarmed"}[r.Taste.RankerArmed], formatTime(r.Taste.LastGradeAt))
	return b.String()
}

func escape(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|") }
func formatTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.UTC().Format(time.RFC3339)
}

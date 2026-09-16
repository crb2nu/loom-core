// Package shiftreport composes the canonical, deterministic Mills shift ledger.
package shiftreport

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/boltcard"
	"github.com/crb2nu/loom/pkg/mills/finishing"
	"github.com/crb2nu/loom/pkg/mills/overseer"
)

type Departure struct {
	Card    boltcard.Card
	EndedAt time.Time
}

type Movement struct {
	Now  float64 `json:"now"`
	Prev float64 `json:"prev"`
}

type KPIDelta struct {
	EscalationRate  Movement `json:"escalation_rate"`
	TestsErrorRate  Movement `json:"tests_error_rate"`
	CostPerMergeUSD Movement `json:"cost_per_merge_usd"`
	Merges          Movement `json:"merges"`
	Regressions     Movement `json:"regressions"`
}

type Taste struct {
	GradedThisShift int        `json:"graded_this_shift"`
	BoltsThisShift  int        `json:"bolts_this_shift"`
	Coverage14d     float64    `json:"coverage_14d"`
	CoverageGate    float64    `json:"coverage_gate"`
	RankerArmed     bool       `json:"ranker_armed"`
	LastGradeAt     *time.Time `json:"last_grade_at"`
}

type Input struct {
	MainRedExternalHold *MainRedExternalHold
	ThroughputGuardrail mills.ThroughputGuardrailVerdict
	GeneratedAt         time.Time
	WindowSeconds       int64
	Departures          []Departure
	KPIDelta            KPIDelta
	Taste               Taste
	SoakProgress        *overseer.SoakProgress
	DocsMirror          *finishing.DocsMirrorDrift
	DeployChecks        map[string]finishing.DeployCheck
	BuildSHA            string
}

type Finishing struct {
	BoltsLive          int                        `json:"bolts_live"`
	BoltsPending       int                        `json:"bolts_pending"`
	BoltsUnknown       int                        `json:"bolts_unknown"`
	BoltsNotApplicable int                        `json:"bolts_not_applicable"`
	BuildSHA           string                     `json:"build_sha"`
	DocsMirror         *finishing.DocsMirrorDrift `json:"docs_mirror,omitempty"`
}

type Report struct {
	ThroughputGuardrail mills.ThroughputGuardrailVerdict `json:"throughput_guardrail"`
	GeneratedAt         time.Time                        `json:"generated_at"`
	WindowSeconds       int64                            `json:"window_seconds"`
	Bolts               []boltcard.Card                  `json:"bolts"`
	Sparks              []boltcard.Card                  `json:"sparks"`
	KPIDelta            KPIDelta                         `json:"kpi_delta"`
	Taste               Taste                            `json:"taste"`
	SoakProgress        *overseer.SoakProgress           `json:"soak_progress,omitempty"`
	SoakComplete        bool                             `json:"soak_complete"`
	Finishing           Finishing                        `json:"finishing"`
	MainRedExternalHold *MainRedExternalHold             `json:"main_red_external_hold,omitempty"`
	NarrativeLines      []string                         `json:"narrative_lines"`
	Markdown            string                           `json:"markdown"`
}

type stamp struct {
	name          string
	bolts, sparks int
}

// Departure kinds under the weave rule shared with the HUD's shiftWindow
// (shiftHelpers.ts): done|merged is a bolt, escalated is a spark, and
// everything else (paused = held thread, preflight_failed, …) is not cloth.
const (
	KindBolt  = "bolt"
	KindSpark = "spark"
)

// Kind classifies one departure, or returns "" for outcomes the ledger
// excludes entirely — they must not reach counts, narrative, or Markdown.
func Kind(c boltcard.Card) string {
	switch strings.ToLower(strings.TrimSpace(c.Outcome)) {
	case "done", "merged":
		return KindBolt
	case "escalated":
		return KindSpark
	}
	return ""
}

// Compose is pure over its input. It drops excluded outcomes up front, then
// sorts the remaining departures oldest-first, so callers get byte-identical
// JSON content and Markdown for the same data.
func Compose(in Input) Report {
	rows := make([]Departure, 0, len(in.Departures))
	for _, d := range in.Departures {
		if Kind(d.Card) != "" {
			rows = append(rows, d)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].EndedAt.Equal(rows[j].EndedAt) {
			return cardID(rows[i].Card) < cardID(rows[j].Card)
		}
		return rows[i].EndedAt.Before(rows[j].EndedAt)
	})
	r := Report{MainRedExternalHold: in.MainRedExternalHold, GeneratedAt: in.GeneratedAt.UTC(), WindowSeconds: in.WindowSeconds, Bolts: []boltcard.Card{}, Sparks: []boltcard.Card{}, KPIDelta: in.KPIDelta, Taste: in.Taste, SoakProgress: in.SoakProgress, SoakComplete: in.SoakProgress.CompleteAt(in.GeneratedAt), ThroughputGuardrail: in.ThroughputGuardrail, Finishing: Finishing{BuildSHA: strings.TrimSpace(in.BuildSHA), DocsMirror: in.DocsMirror}}
	if r.ThroughputGuardrail.Reasons == nil {
		r.ThroughputGuardrail.Reasons = []string{}
	}
	for _, row := range rows {
		if Kind(row.Card) == KindBolt {
			check, ok := in.DeployChecks[deployKey(row.Card)]
			if !ok {
				check = finishing.DeployCheck{State: finishing.DeployNotApplicable, BuildSHA: strings.TrimSpace(in.BuildSHA)}
			}
			row.Card.Deploy = &boltcard.Deploy{State: string(check.State), MergeSHA: check.MergeSHA, BuildSHA: check.BuildSHA, Reason: check.Reason}
			switch check.State {
			case finishing.DeployLive:
				r.Finishing.BoltsLive++
			case finishing.DeployPending:
				r.Finishing.BoltsPending++
			case finishing.DeployUnknown:
				r.Finishing.BoltsUnknown++
			default:
				r.Finishing.BoltsNotApplicable++
			}
			r.Bolts = append(r.Bolts, row.Card)
		} else {
			r.Sparks = append(r.Sparks, row.Card)
		}
	}
	r.NarrativeLines = narrative(rows, r)
	r.Markdown = RenderMarkdown(r, rows)
	return r
}

// narrative renders the prose lines from the eligible departures only —
// Compose has already dropped excluded outcomes from rows.
func narrative(rows []Departure, r Report) []string {
	hours := r.WindowSeconds / 3600
	lines := []string{}
	if len(rows) == 0 {
		lines = append(lines, fmt.Sprintf("The loom sat quiet — no cloth came off the beam in the last %d hours.", hours))
	} else {
		lines = append(lines, fmt.Sprintf("The floor wove %s and struck %s over the last %d hours.", plural(len(r.Bolts), "bolt"), noPlural(len(r.Sparks), "spark"), hours))
		stamps := map[string]*stamp{}
		for _, row := range rows {
			if row.Card.Stamp == nil || strings.TrimSpace(*row.Card.Stamp) == "" {
				continue
			}
			name := strings.TrimSpace(*row.Card.Stamp)
			s := stamps[name]
			if s == nil {
				s = &stamp{name: name}
				stamps[name] = s
			}
			if Kind(row.Card) == KindBolt {
				s.bolts++
			} else {
				s.sparks++
			}
		}
		ordered := make([]*stamp, 0, len(stamps))
		for _, s := range stamps {
			ordered = append(ordered, s)
		}
		sort.Slice(ordered, func(i, j int) bool {
			ai, aj := ordered[i].bolts+ordered[i].sparks, ordered[j].bolts+ordered[j].sparks
			if ai == aj {
				return ordered[i].name < ordered[j].name
			}
			return ai > aj
		})
		for _, s := range ordered {
			n := s.bolts + s.sparks
			var times string
			switch n {
			case 1:
				times = "once"
			case 2:
				times = "twice"
			default:
				times = fmt.Sprintf("%d times", n)
			}
			outcome := ""
			if s.sparks == 0 {
				if n == 1 {
					outcome = "merged on green"
				} else {
					outcome = "all merged on green"
				}
			} else if s.bolts == 0 {
				if n == 1 {
					outcome = "escalated"
				} else {
					outcome = "all escalated"
				}
			} else {
				outcome = fmt.Sprintf("%s, %s", plural(s.bolts, "merge"), plural(s.sparks, "escalation"))
			}
			lines = append(lines, fmt.Sprintf("Pattern %s stamped %s — %s.", s.name, times, outcome))
		}
		retried := append([]Departure(nil), rows...)
		sort.SliceStable(retried, func(i, j int) bool { return retried[i].Card.Attempts > retried[j].Card.Attempts })
		n := 0
		for n < len(retried) && retried[n].Card.Attempts > 1 {
			n++
		}
		retried = retried[:n]
		if n > 0 {
			worst := retried[0].Card
			lines = append(lines, fmt.Sprintf("%s needed extra passes (worst: %s at %d attempts).", plural(n, "run"), cardID(worst), worst.Attempts))
		}
		counts := map[int]int{}
		busiestHour, busiestCount := -1, 0
		for _, row := range rows {
			h := row.EndedAt.UTC().Hour()
			counts[h]++
			if counts[h] > busiestCount {
				busiestHour, busiestCount = h, counts[h]
			}
		}
		if busiestCount > 1 {
			lines = append(lines, fmt.Sprintf("Busiest hour %02d:00–%02d:00 — %s.", busiestHour, (busiestHour+1)%24, plural(busiestCount, "departure")))
		}
		cost := 0.0
		for _, row := range rows {
			cost += row.Card.CostUSD
		}
		if cost > 0 {
			lines = append(lines, fmt.Sprintf("The shift burned $%.2f of pipeline fuel.", cost))
		}
	}
	if line := finishingLine(r.Finishing); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines, fmt.Sprintf("KPI movement: escalation %.1f%% → %.1f%%; test errors %.1f%% → %.1f%%; cost/merge $%.2f → $%.2f; merges %.0f → %.0f; regressions %.0f → %.0f.", r.KPIDelta.EscalationRate.Prev*100, r.KPIDelta.EscalationRate.Now*100, r.KPIDelta.TestsErrorRate.Prev*100, r.KPIDelta.TestsErrorRate.Now*100, r.KPIDelta.CostPerMergeUSD.Prev, r.KPIDelta.CostPerMergeUSD.Now, r.KPIDelta.Merges.Prev, r.KPIDelta.Merges.Now, r.KPIDelta.Regressions.Prev, r.KPIDelta.Regressions.Now))
	lines = append(lines, fmt.Sprintf("%d of %d bolts graded this shift; 14-day taste coverage %.1f%% (gate %.1f%%), ranker %s.", r.Taste.GradedThisShift, r.Taste.BoltsThisShift, r.Taste.Coverage14d*100, r.Taste.CoverageGate*100, map[bool]string{true: "armed", false: "disarmed"}[r.Taste.RankerArmed]))
	if h := r.MainRedExternalHold; h != nil {
		if h.state(r.GeneratedAt) == "active" {
			lines = append(lines, fmt.Sprintf("Merge queue hold: %s/%s is deferred as main_red_external for %s more.", h.Project, h.Branch, h.ExpiresAt.Sub(r.GeneratedAt).Round(time.Minute)))
		} else if h.Escalated {
			lines = append(lines, fmt.Sprintf("Merge queue hold: %s/%s expired and was escalated.", h.Project, h.Branch))
		} else {
			lines = append(lines, fmt.Sprintf("Merge queue hold: %s/%s expired; escalation pending reconciliation.", h.Project, h.Branch))
		}
	}
	if d := r.Finishing.DocsMirror; d != nil {
		last := "last sync unknown"
		if d.MirrorCommitAt != nil {
			last = "last sync " + d.MirrorCommitAt.UTC().Format("2006-01-02")
		}
		switch d.State {
		case finishing.StateFresh:
			lines = append(lines, fmt.Sprintf("Docs mirror: fresh (%s).", last))
		case finishing.StateDrifted:
			behind := d.Missing + d.Stale
			lines = append(lines, fmt.Sprintf("Docs mirror: %d files behind, %d stale (flexinfer-site %s, %s).", behind, d.Stale, d.MirrorRef, last))
		default:
			reason := strings.TrimSpace(d.Reason)
			if reason == "" {
				reason = "reason unavailable"
			}
			lines = append(lines, "Docs mirror: unknown ("+reason+").")
		}
	}
	return lines
}

func finishingLine(f Finishing) string {
	total := f.BoltsLive + f.BoltsPending + f.BoltsUnknown
	if total == 0 {
		return ""
	}
	line := fmt.Sprintf("Finishing: %d of %d bolts live in the running operator (build %s)", f.BoltsLive, total, f.BuildSHA)
	parts := []string{}
	if f.BoltsPending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending rollout", f.BoltsPending))
	}
	if f.BoltsUnknown > 0 {
		parts = append(parts, fmt.Sprintf("%d unknown", f.BoltsUnknown))
	}
	if len(parts) > 0 {
		line += "; " + strings.Join(parts, ", ")
	}
	return line + "."
}

func plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}
func noPlural(n int, one string) string {
	if n == 0 {
		return "no " + one + "s"
	}
	return plural(n, one)
}
func cardID(c boltcard.Card) string {
	if c.BacklogID != "" {
		return c.BacklogID
	}
	if c.RunID != nil {
		return *c.RunID
	}
	return "—"
}

func deployKey(c boltcard.Card) string {
	if c.RunID != nil && strings.TrimSpace(*c.RunID) != "" {
		return strings.TrimSpace(*c.RunID)
	}
	return c.BacklogID
}

// Package digest composes the deterministic daily finished-goods digest —
// the Finishing House's third seam (docs/FACTORY_MODEL.md, J5) — from one
// UTC day's 24-hour shift report. It is a leaf below shiftreport, which
// already imports finishing and the mills root, so nothing under pkg/mills
// may import it back; the operator's digest job is its only caller.
package digest

import (
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/finishing"
	"github.com/crb2nu/loom/pkg/mills/shiftreport"
)

// Input is the UTC day a digest describes and the shift report composed
// over that day's 24-hour window.
type Input struct {
	Day    time.Time
	Report shiftreport.Report
}

// Counts are the digest's headline numbers, stored beside the Markdown so
// readers never re-derive them from prose.
type Counts struct {
	Bolts          int `json:"bolts"`
	Live           int `json:"live"`
	Pending        int `json:"pending"`
	Unknown        int `json:"unknown"`
	Sparks         int `json:"sparks"`
	Graded         int `json:"graded"`
	DocsDriftFiles int `json:"docs_drift_files"`
}

// Digest is one day's finished-goods digest. The stored event payload and
// the operator's API response share this shape.
type Digest struct {
	Day      string `json:"day"`
	Markdown string `json:"markdown"`
	Summary  string `json:"summary"`
	Counts   Counts `json:"counts"`
}

// Compose is pure over its input: the same report yields byte-identical
// Markdown, which is what lets the operator append one digest per day and
// the goldens under testdata pin the rendering. No LLM is involved.
func Compose(in Input) Digest {
	day := in.Day.UTC().Format("2006-01-02")
	r := in.Report
	c := Counts{Bolts: len(r.Bolts), Live: r.Finishing.BoltsLive, Pending: r.Finishing.BoltsPending, Unknown: r.Finishing.BoltsUnknown, Sparks: len(r.Sparks), Graded: r.Taste.GradedThisShift}
	if d := r.Finishing.DocsMirror; d != nil && d.State == finishing.StateDrifted {
		c.DocsDriftFiles = d.Missing + d.Stale
	}
	if c.Bolts == 0 && c.Sparks == 0 {
		s := "No finished goods for " + day + "."
		return Digest{Day: day, Markdown: s, Summary: s, Counts: c}
	}
	docs := "docs mirror unknown"
	if d := r.Finishing.DocsMirror; d != nil {
		switch d.State {
		case finishing.StateFresh:
			docs = "docs mirror fresh"
		case finishing.StateDrifted:
			docs = fmt.Sprintf("docs mirror %d files drifted", c.DocsDriftFiles)
		}
	}
	summary := fmt.Sprintf("Finished goods for %s: %d bolts, %d live, %d pending rollout, %s", day, c.Bolts, c.Live, c.Pending, docs)
	var b strings.Builder
	fmt.Fprintln(&b, summary)
	fmt.Fprintln(&b, "\n## Bolts")
	for _, card := range r.Bolts {
		link := card.MR.URL
		if link == "" {
			link = "MR unavailable"
		}
		state := "unknown"
		if card.Deploy != nil && card.Deploy.State != "" {
			state = card.Deploy.State
		}
		grade := "ungraded"
		if card.Grade != nil {
			grade = card.Grade.Value
		}
		fmt.Fprintf(&b, "- %s — %s — deploy: %s — grade: %s\n", card.Title, link, state, grade)
	}
	if c.Sparks > 0 {
		fmt.Fprintln(&b, "\n## Sparks")
		for _, card := range r.Sparks {
			class := "unknown"
			if card.Escalation != nil && card.Escalation.Class != "" {
				class = card.Escalation.Class
			}
			fmt.Fprintf(&b, "- %s — escalation: %s\n", card.Title, class)
		}
	}
	fmt.Fprintln(&b, "\n## Finishing")
	fmt.Fprintf(&b, "- Deploy: %d live, %d pending rollout, %d unknown.\n", c.Live, c.Pending, c.Unknown)
	fmt.Fprintf(&b, "- Docs mirror: %s.\n", strings.TrimPrefix(docs, "docs mirror "))
	fmt.Fprintln(&b, "\n## Taste coverage")
	fmt.Fprintf(&b, "- %d of %d bolts graded; 14-day coverage %.1f%%.\n", c.Graded, c.Bolts, r.Taste.Coverage14d*100)
	return Digest{Day: day, Markdown: strings.TrimSpace(b.String()), Summary: summary, Counts: c}
}

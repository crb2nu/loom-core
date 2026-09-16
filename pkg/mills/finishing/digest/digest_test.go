package digest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/boltcard"
	"github.com/crb2nu/loom/pkg/mills/finishing"
	"github.com/crb2nu/loom/pkg/mills/shiftreport"
)

func TestComposeGoldens(t *testing.T) {
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	iid := int64(42)
	// One bolt in every deploy state, one spark, a drifted docs mirror, and
	// partial grading — the acceptance fixture for the mixed-day golden.
	report := shiftreport.Report{
		Bolts: []boltcard.Card{
			{Title: "Live bolt", MR: boltcard.MR{IID: &iid, URL: "https://git.example/mr/42"}, Deploy: &boltcard.Deploy{State: string(finishing.DeployLive)}, Grade: &boltcard.Grade{Value: "keep"}},
			{Title: "Rolling bolt", MR: boltcard.MR{URL: "https://git.example/mr/43"}, Deploy: &boltcard.Deploy{State: string(finishing.DeployPending)}},
			{Title: "Uncertain bolt", MR: boltcard.MR{URL: "https://git.example/mr/44"}, Deploy: &boltcard.Deploy{State: string(finishing.DeployUnknown)}},
			{Title: "Foreign bolt", MR: boltcard.MR{URL: "https://git.example/mr/45"}, Deploy: &boltcard.Deploy{State: string(finishing.DeployNotApplicable)}},
		},
		Sparks:    []boltcard.Card{{Title: "Broken shuttle", Escalation: &boltcard.Escalation{Class: "infrastructure"}}},
		Finishing: shiftreport.Finishing{BoltsLive: 1, BoltsPending: 1, BoltsUnknown: 1, BoltsNotApplicable: 1, DocsMirror: &finishing.DocsMirrorDrift{State: finishing.StateDrifted, Missing: 2, Stale: 1}},
		Taste:     shiftreport.Taste{GradedThisShift: 1, BoltsThisShift: 4, Coverage14d: .75},
	}
	tests := []struct {
		name, golden string
		report       shiftreport.Report
		counts       Counts
	}{
		{"mixed", "mixed_day.golden.md", report, Counts{Bolts: 4, Live: 1, Pending: 1, Unknown: 1, Sparks: 1, Graded: 1, DocsDriftFiles: 3}},
		{"empty", "empty_day.golden.md", shiftreport.Report{}, Counts{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Compose(Input{Day: day, Report: tc.report})
			want, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatal(err)
			}
			if got.Markdown != strings.TrimSuffix(string(want), "\n") {
				t.Fatalf("markdown:\n%s\nwant:\n%s", got.Markdown, want)
			}
			if got.Day != "2026-09-10" || got.Counts != tc.counts {
				t.Fatalf("day=%s counts=%+v want %+v", got.Day, got.Counts, tc.counts)
			}
			if !strings.HasPrefix(got.Markdown, got.Summary) {
				t.Fatalf("summary %q is not the digest's first line:\n%s", got.Summary, got.Markdown)
			}
			if again := Compose(Input{Day: day, Report: tc.report}); again != got {
				t.Fatal("compose is not deterministic")
			}
		})
	}
}

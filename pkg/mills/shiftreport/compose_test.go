package shiftreport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/boltcard"
	"github.com/crb2nu/loom/pkg/mills/finishing"
	"github.com/crb2nu/loom/pkg/mills/overseer"
)

func TestShiftComposeSoakCompleteProjection(t *testing.T) {
	started := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		progress *overseer.SoakProgress
		want     bool
	}{
		{name: "missing"},
		{name: "short", progress: &overseer.SoakProgress{StartedAt: started, ElapsedSeconds: 604799}},
		{name: "complete", progress: &overseer.SoakProgress{StartedAt: started, ElapsedSeconds: 604800}, want: true},
		{name: "diverged", progress: &overseer.SoakProgress{StartedAt: started, ElapsedSeconds: 604800, Divergences: 1}},
		{name: "malformed", progress: &overseer.SoakProgress{ElapsedSeconds: 604800}},
		{name: "future dated", progress: &overseer.SoakProgress{StartedAt: started.Add(time.Second), ElapsedSeconds: 604800}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Compose(Input{GeneratedAt: started, SoakProgress: tc.progress})
			if r.SoakComplete != tc.want {
				t.Fatalf("soak_complete = %v, want %v", r.SoakComplete, tc.want)
			}
			first, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			second, err := json.Marshal(Compose(Input{GeneratedAt: started, SoakProgress: tc.progress}))
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) {
				t.Fatalf("report JSON is nondeterministic:\n%s\n%s", first, second)
			}
		})
	}
}

func assertNarrative(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("narrative has %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i, line := range want {
		if got[i] != line {
			t.Fatalf("line %d = %q, want %q", i, got[i], line)
		}
	}
}

func TestShiftComposeMixedNarrativeAndBusiestHour(t *testing.T) {
	now := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	stamp := "go-rest-service"
	gradeAt := now.Add(-time.Hour)
	score := .91
	mr := int64(42)
	report := Compose(Input{GeneratedAt: now, WindowSeconds: 86400, Departures: []Departure{
		{EndedAt: now.Add(-3 * time.Hour), Card: boltcard.Card{BacklogID: "BL-5", Outcome: "merged", Attempts: 1}},
		{EndedAt: now.Add(-2 * time.Hour), Card: boltcard.Card{BacklogID: "BL-1", Title: "Add ledger", Outcome: "merged", MR: boltcard.MR{IID: &mr}, Diff: boltcard.Diff{Files: 3, Added: 20, Removed: 4}, Eval: boltcard.Eval{MinScore: &score}, Attempts: 1, CostUSD: 1.25, Stamp: &stamp, Grade: &boltcard.Grade{Value: "keep", At: gradeAt}}},
		{EndedAt: now.Add(-90 * time.Minute), Card: boltcard.Card{BacklogID: "BL-2", Outcome: "escalated", Attempts: 3, CostUSD: .75, Stamp: &stamp, Escalation: &boltcard.Escalation{Class: "code", Signature: "sig-1", WeekOccurrences: 2}}},
		// done departures are bolts, exactly like merged ones.
		{EndedAt: now.Add(-time.Hour), Card: boltcard.Card{BacklogID: "BL-3", Outcome: "done", Diff: boltcard.Diff{Files: 2, Added: 5, Removed: 1}, Attempts: 1, CostUSD: .5}},
		// A held thread: excluded outcomes must not reach counts, stamps,
		// retries, busiest hour, fuel, or the tables — poison values prove it.
		{EndedAt: now.Add(-30 * time.Minute), Card: boltcard.Card{BacklogID: "BL-4", Outcome: "paused", Attempts: 5, CostUSD: 99.99, Stamp: &stamp}},
	}, KPIDelta: KPIDelta{EscalationRate: Movement{Now: .1, Prev: .2}, TestsErrorRate: Movement{Now: .05, Prev: .1}, CostPerMergeUSD: Movement{Now: 2, Prev: 3}, Merges: Movement{Now: 4, Prev: 2}, Regressions: Movement{Now: 0, Prev: 1}}, Taste: Taste{GradedThisShift: 1, BoltsThisShift: 3, Coverage14d: .7, CoverageGate: .6, RankerArmed: true, LastGradeAt: &gradeAt}, BuildSHA: "2d5e44b5", DeployChecks: map[string]finishing.DeployCheck{
		"BL-1": {State: finishing.DeployLive, MergeSHA: "aaa", BuildSHA: "2d5e44b5"},
		"BL-3": {State: finishing.DeployPending, MergeSHA: "bbb", BuildSHA: "2d5e44b5"},
		"BL-5": {State: finishing.DeployUnknown, MergeSHA: "ccc", BuildSHA: "2d5e44b5", Reason: "git unavailable"},
	}})
	assertNarrative(t, report.NarrativeLines, []string{
		"The floor wove 3 bolts and struck 1 spark over the last 24 hours.",
		"Pattern go-rest-service stamped twice — 1 merge, 1 escalation.",
		"1 run needed extra passes (worst: BL-2 at 3 attempts).",
		"Busiest hour 14:00–15:00 — 2 departures.",
		"The shift burned $2.50 of pipeline fuel.",
		"Finishing: 1 of 3 bolts live in the running operator (build 2d5e44b5); 1 pending rollout, 1 unknown.",
		"KPI movement: escalation 20.0% → 10.0%; test errors 10.0% → 5.0%; cost/merge $3.00 → $2.00; merges 2 → 4; regressions 1 → 0.",
		"1 of 3 bolts graded this shift; 14-day taste coverage 70.0% (gate 60.0%), ranker armed.",
	})
	if len(report.Bolts) != 3 || len(report.Sparks) != 1 {
		t.Fatalf("bolts=%d sparks=%d, want 3/1", len(report.Bolts), len(report.Sparks))
	}
	if strings.Contains(report.Markdown, "BL-4") {
		t.Fatalf("excluded departure leaked into markdown:\n%s", report.Markdown)
	}
	assertGolden(t, "testdata/mixed_window.golden.md", report.Markdown)
}

func TestShiftComposeQuietGolden(t *testing.T) {
	report := Compose(Input{GeneratedAt: time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC), WindowSeconds: 86400, Taste: Taste{CoverageGate: .6}})
	if report.NarrativeLines[0] != "The loom sat quiet — no cloth came off the beam in the last 24 hours." {
		t.Fatalf("quiet line = %q", report.NarrativeLines[0])
	}
	assertGolden(t, "testdata/quiet_window.golden.md", report.Markdown)
}

func TestShiftComposeOmitsFinishingWhenAllBoltsNotApplicable(t *testing.T) {
	now := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	report := Compose(Input{GeneratedAt: now, WindowSeconds: 86400, BuildSHA: "build", Departures: []Departure{{EndedAt: now, Card: boltcard.Card{BacklogID: "foreign", Outcome: "merged"}}}})
	if report.Bolts[0].Deploy == nil || report.Bolts[0].Deploy.State != string(finishing.DeployNotApplicable) {
		t.Fatalf("deploy = %+v", report.Bolts[0].Deploy)
	}
	if strings.Contains(report.Markdown, "Finishing:") || strings.Contains(strings.Join(report.NarrativeLines, "\n"), "Finishing:") {
		t.Fatalf("not-applicable finishing line was rendered: %s", report.Markdown)
	}
}

func TestShiftComposeThroughputGuardrailJSONAndMarkdown(t *testing.T) {
	r := Compose(Input{GeneratedAt: time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC), ThroughputGuardrail: mills.ThroughputGuardrailVerdict{Breached: true, Reasons: []string{"escalation_rate 0.3000 exceeds 0.2500"}}})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"throughput_guardrail":{"breached":true,"reasons":[`) {
		t.Fatalf("JSON = %s", b)
	}
	if !strings.Contains(r.Markdown, "## Throughput guardrail — BREACHED") || !strings.Contains(r.Markdown, "- escalation_rate") {
		t.Fatalf("markdown:\n%s", r.Markdown)
	}
	healthy := Compose(Input{})
	if healthy.ThroughputGuardrail.Reasons == nil || !strings.Contains(healthy.Markdown, "NOT BREACHED") {
		t.Fatalf("healthy = %+v", healthy)
	}
}

func TestShiftComposeExcludesHeldThreads(t *testing.T) {
	now := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	report := Compose(Input{GeneratedAt: now, WindowSeconds: 86400, Taste: Taste{CoverageGate: .6}, Departures: []Departure{
		{EndedAt: now.Add(-2 * time.Hour), Card: boltcard.Card{BacklogID: "BL-PRE", Outcome: "preflight_failed", Attempts: 2, CostUSD: 1}},
		{EndedAt: now.Add(-time.Hour), Card: boltcard.Card{BacklogID: "BL-HELD", Outcome: "paused", Attempts: 4, CostUSD: 5}},
	}})
	if len(report.Bolts) != 0 || len(report.Sparks) != 0 {
		t.Fatalf("held threads classified as cloth: bolts=%d sparks=%d", len(report.Bolts), len(report.Sparks))
	}
	if report.NarrativeLines[0] != "The loom sat quiet — no cloth came off the beam in the last 24 hours." {
		t.Fatalf("held-threads-only window is not quiet: %q", report.NarrativeLines[0])
	}
	if strings.Contains(report.Markdown, "BL-HELD") || strings.Contains(report.Markdown, "## Bolts") {
		t.Fatalf("held thread leaked into markdown:\n%s", report.Markdown)
	}
}

func TestShiftComposeSuppressesSingleDepartureBusiestHour(t *testing.T) {
	now := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	report := Compose(Input{GeneratedAt: now, WindowSeconds: 86400, Taste: Taste{BoltsThisShift: 1, CoverageGate: .6}, Departures: []Departure{
		{EndedAt: now.Add(-175 * time.Minute), Card: boltcard.Card{BacklogID: "BL-1", Outcome: "merged", Diff: boltcard.Diff{Files: 1, Added: 2}, Attempts: 1, CostUSD: .4}},
		{EndedAt: now.Add(-50 * time.Minute), Card: boltcard.Card{BacklogID: "BL-2", Outcome: "escalated", Attempts: 1, CostUSD: .35, Escalation: &boltcard.Escalation{Class: "code", Signature: "sig-9", WeekOccurrences: 1}}},
	}})
	// Every hour holds a single departure, so no line may claim a peak.
	assertNarrative(t, report.NarrativeLines, []string{
		"The floor wove 1 bolt and struck 1 spark over the last 24 hours.",
		"The shift burned $0.75 of pipeline fuel.",
		"KPI movement: escalation 0.0% → 0.0%; test errors 0.0% → 0.0%; cost/merge $0.00 → $0.00; merges 0 → 0; regressions 0 → 0.",
		"0 of 1 bolts graded this shift; 14-day taste coverage 0.0% (gate 60.0%), ranker disarmed.",
	})
	assertGolden(t, "testdata/busiest_hour_suppressed.golden.md", report.Markdown)
}

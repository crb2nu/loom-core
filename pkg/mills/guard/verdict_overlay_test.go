package guard

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Trustworthy Verdicts S2: the reports partition runs by their current
// verdict. A superseded escalation (explicit run.verdict.* event or the
// legacy ghost-spark closure) must leave the escalated bucket — labeled,
// never silently folded into merged.

func TestJudgeCalibration_CorrectedEscalationJoinsMerged(t *testing.T) {
	f := &fakeFilteredEvents{}
	jv := ev("pipeline", store.JudgeVerdictEventKind, "pipeline_run", "run-x", 30*time.Minute)
	jv.Payload = map[string]any{"gate": "code_review", "score": 0.9, "pass": true}
	f.events = append(f.events, jv,
		ev("reconciler", mills.RunVerdictKindGhostSparkMerged, "pipeline_run", "run-x", 10*time.Minute))
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-x", store.PipelineEscalated, 1.0, nil),
	}}

	rep, err := BuildJudgeCalibrationReport(context.Background(), f, runs, reportNow.Add(-time.Hour), reportNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rep.PerGate) != 1 {
		t.Fatalf("want 1 gate, got %d", len(rep.PerGate))
	}
	g := rep.PerGate[0]
	if g.MergedVerdicts != 1 || g.EscalatedVerdicts != 0 {
		t.Fatalf("corrected run must join merged: %+v", g)
	}
	if g.CorrectedVerdicts != 1 || rep.CorrectedVerdicts != 1 || rep.CorrectedRuns != 1 {
		t.Fatalf("correction must be counted explicitly: gate=%+v report corrected=%d runs=%d",
			g, rep.CorrectedVerdicts, rep.CorrectedRuns)
	}
	if g.MeanScoreMerged != 0.9 {
		t.Fatalf("corrected verdict must feed the merged score mean: %+v", g)
	}
}

func TestJudgeCalibration_UncorrectedEscalationStaysEscalated(t *testing.T) {
	f := &fakeFilteredEvents{}
	jv := ev("pipeline", store.JudgeVerdictEventKind, "pipeline_run", "run-y", 30*time.Minute)
	jv.Payload = map[string]any{"gate": "code_review", "score": 0.4, "pass": false}
	f.events = append(f.events, jv)
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-y", store.PipelineEscalated, 1.0, nil),
	}}

	rep, err := BuildJudgeCalibrationReport(context.Background(), f, runs, reportNow.Add(-time.Hour), reportNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	g := rep.PerGate[0]
	if g.EscalatedVerdicts != 1 || g.MergedVerdicts != 0 || g.CorrectedVerdicts != 0 || rep.CorrectedRuns != 0 {
		t.Fatalf("uncorrected escalation must stay escalated: %+v", g)
	}
}

func TestConfigOutcomes_CorrectedEscalationPartitionsSeparately(t *testing.T) {
	f := &fakeFilteredEvents{}
	prov := ev("pipeline", mills.RunProvenanceEventKind, "pipeline_run", "run-x", 40*time.Minute)
	prov.Payload = map[string]any{"policy_checksum": "abc"}
	provDone := ev("pipeline", mills.RunProvenanceEventKind, "pipeline_run", "run-done", 40*time.Minute)
	provDone.Payload = map[string]any{"policy_checksum": "abc"}
	f.events = append(f.events, prov, provDone,
		// Legacy closure kind — the retroactive path.
		ev("reconciler", mills.GhostSparkClosedEventKind, "pipeline_run", "run-x", 10*time.Minute),
		// A stray closure event on a DONE run must not relabel it.
		ev("reconciler", mills.GhostSparkClosedEventKind, "pipeline_run", "run-done", 10*time.Minute))
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-x", store.PipelineEscalated, 1.0, nil),
		terminalRun("run-done", store.PipelineDone, 2.0, nil),
	}}

	rep, err := BuildConfigOutcomeReport(context.Background(), f, runs, reportNow.Add(-time.Hour), reportNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	tt := rep.Totals
	if tt.Runs != 2 || tt.MergedAfterEscalation != 1 || tt.Merged != 1 || tt.Escalated != 0 {
		t.Fatalf("partition wrong: %+v", tt)
	}
	if tt.MergeRate != 1.0 {
		t.Fatalf("merge rate must count corrected merges: %v", tt.MergeRate)
	}
	if len(rep.PerPolicyChecksum) != 1 || rep.PerPolicyChecksum[0].MergedAfterEscalation != 1 {
		t.Fatalf("per-policy group must carry the corrected column: %+v", rep.PerPolicyChecksum)
	}
}

func TestConfigOutcomes_UncorrectedEscalationStaysEscalated(t *testing.T) {
	f := &fakeFilteredEvents{}
	prov := ev("pipeline", mills.RunProvenanceEventKind, "pipeline_run", "run-z", 40*time.Minute)
	prov.Payload = map[string]any{"policy_checksum": "abc"}
	f.events = append(f.events, prov)
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-z", store.PipelineEscalated, 1.0, nil),
	}}

	rep, err := BuildConfigOutcomeReport(context.Background(), f, runs, reportNow.Add(-time.Hour), reportNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.Totals.Escalated != 1 || rep.Totals.MergedAfterEscalation != 0 || rep.Totals.MergeRate != 0 {
		t.Fatalf("uncorrected escalation must stay escalated: %+v", rep.Totals)
	}
}

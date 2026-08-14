package guard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func provenanceEvent(at time.Time, runID, checksum string, stageModels map[string]string) *store.Event {
	models := make(map[string]any, len(stageModels))
	for stage, model := range stageModels {
		models[stage] = model
	}
	return &store.Event{
		OccurredAt: at, Actor: mills.RunProvenanceActor, Kind: mills.RunProvenanceEventKind,
		SubjectKind: "pipeline_run", SubjectID: runID,
		Payload: map[string]any{
			"run_id": runID, "backlog_id": "BL-" + runID, "lane": "pipeline",
			"policy_checksum": checksum, "stage_models": models,
			"prompt_hashes": map[string]any{}, "outcome": "ok",
		},
	}
}

func regressionEvent(at time.Time, mrIID int64) *store.Event {
	return &store.Event{
		OccurredAt: at, Actor: mills.RegressionAttributionActor, Kind: mills.RegressionAttributedEventKind,
		SubjectKind: "merge_request", SubjectID: "42",
		Payload: map[string]any{
			// float64 is what the payload comes back as after the events
			// table's JSON round-trip.
			"regressed_mr_iid": float64(mrIID),
			"merged_sha":       "abcdef123456",
			"revert_sha":       "fedcba654321",
			"revert_title":     `Revert "feat: thing"`,
		},
	}
}

func terminalRun(runID string, state store.PipelineState, cost float64, mrIID *int64) *store.RunTerminalOutcome {
	return &store.RunTerminalOutcome{
		RunID: runID, BacklogID: "BL-" + runID, State: state, CostUSD: cost, MRIID: mrIID,
	}
}

func iid(v int64) *int64 { return &v }

func policyRow(t *testing.T, rep ConfigOutcomeReport, checksum string) PolicyOutcomeGroup {
	t.Helper()
	for _, g := range rep.PerPolicyChecksum {
		if g.PolicyChecksum == checksum {
			return g
		}
	}
	t.Fatalf("policy checksum %q missing from %+v", checksum, rep.PerPolicyChecksum)
	return PolicyOutcomeGroup{}
}

func stageModelRow(t *testing.T, rep ConfigOutcomeReport, stage, model string) StageModelOutcomeGroup {
	t.Helper()
	for _, g := range rep.PerStageModel {
		if g.Stage == stage && g.Model == model {
			return g
		}
	}
	t.Fatalf("(%s, %s) missing from %+v", stage, model, rep.PerStageModel)
	return StageModelOutcomeGroup{}
}

// The whole point of the report: a merged run, an escalated run and an
// unstamped run, each landing where it belongs rather than being averaged into
// one number.
func TestBuildConfigOutcomeReport_JoinsProvenanceToOutcomes(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	models := map[string]string{"implement": "sonnet", "judge": "gemma"}
	events := &fakeEventLister{events: []*store.Event{
		provenanceEvent(now.Add(-time.Hour), "run-merged", "checksum-a", models),
		verdictEvent(now.Add(-50*time.Minute), "run-merged", "spec_conformance", "gemma", 0.90, true),
		verdictEvent(now.Add(-49*time.Minute), "run-merged", "pr_self_review", "gemma", 0.80, true),
		provenanceEvent(now.Add(-2*time.Hour), "run-escalated", "checksum-a", models),
		verdictEvent(now.Add(-100*time.Minute), "run-escalated", "spec_conformance", "gemma", 0.40, false),
		// run-unstamped finished in the window but carries no provenance: the
		// blind spot the report has to name.
	}}
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-merged", store.PipelineDone, 3.00, iid(101)),
		terminalRun("run-escalated", store.PipelineEscalated, 1.00, nil),
		terminalRun("run-unstamped", store.PipelineDone, 9.00, nil),
	}}

	rep, err := BuildConfigOutcomeReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.StampedRuns != 2 || rep.UncoveredRuns != 1 {
		t.Fatalf("coverage = %d stamped / %d uncovered, want 2/1", rep.StampedRuns, rep.UncoveredRuns)
	}
	if rep.ZeroEvidence {
		t.Error("zero_evidence set on a window holding two stamps")
	}

	// The unstamped run's $9 must not reach any configuration's economics.
	if !nearly(rep.Totals.TotalCostUSD, 4.00) || !nearly(rep.Totals.MeanCostUSD, 2.00) {
		t.Errorf("totals cost = %+v, want 4.00 total / 2.00 mean over the two stamped runs", rep.Totals)
	}
	if rep.Totals.Runs != 2 || rep.Totals.Merged != 1 || rep.Totals.Escalated != 1 || rep.Totals.Other != 0 {
		t.Fatalf("totals split = %+v", rep.Totals)
	}
	if !nearly(rep.Totals.MergeRate, 0.5) {
		t.Errorf("merge_rate = %v, want 0.5", rep.Totals.MergeRate)
	}

	row := policyRow(t, rep, "checksum-a")
	if row.Runs != 2 || row.Merged != 1 || row.Escalated != 1 {
		t.Fatalf("checksum-a = %+v", row)
	}
	// Per-run averaging: run-merged's two verdicts collapse to one 0.85 data
	// point before the group averages it against run-escalated's 0.40.
	if row.JudgeGradedRuns != 2 || !nearly(row.MeanJudgeScore, 0.625) {
		t.Errorf("judge score = %+v, want 2 graded runs at mean 0.625", row)
	}
	if !nearly(row.JudgePassRate, 0.5) {
		t.Errorf("judge_pass_rate = %v, want 0.5 (one run all-pass, one all-fail)", row.JudgePassRate)
	}

	// A run pins several stages, so each (stage, model) row sees the same runs.
	for _, want := range [][2]string{{"implement", "sonnet"}, {"judge", "gemma"}} {
		got := stageModelRow(t, rep, want[0], want[1])
		if got.Runs != 2 || got.Merged != 1 || !nearly(got.TotalCostUSD, 4.00) {
			t.Errorf("(%s, %s) = %+v", want[0], want[1], got)
		}
	}
	if len(rep.PerStageModel) != 2 {
		t.Errorf("per_stage_model = %+v, want exactly the two pinned stages", rep.PerStageModel)
	}
}

// Two policy revisions in one window is the comparison the report exists for.
func TestBuildConfigOutcomeReport_SeparatesConfigurations(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	events := &fakeEventLister{events: []*store.Event{
		provenanceEvent(now.Add(-time.Hour), "run-a1", "checksum-a", map[string]string{"implement": "sonnet"}),
		provenanceEvent(now.Add(-2*time.Hour), "run-a2", "checksum-a", map[string]string{"implement": "sonnet"}),
		provenanceEvent(now.Add(-3*time.Hour), "run-b1", "checksum-b", map[string]string{"implement": "opus"}),
		// A stamp written before any PolicyManager was wired: recorded empty,
		// bucketed as unknown, never folded into a real revision.
		provenanceEvent(now.Add(-4*time.Hour), "run-c1", "", map[string]string{"implement": "opus"}),
	}}
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-a1", store.PipelineDone, 2.00, nil),
		terminalRun("run-a2", store.PipelineDone, 4.00, nil),
		terminalRun("run-b1", store.PipelineEscalated, 6.00, nil),
		// run-c1 is still in flight: terminal record absent.
	}}

	rep, err := BuildConfigOutcomeReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	a := policyRow(t, rep, "checksum-a")
	if a.Runs != 2 || a.Merged != 2 || !nearly(a.MergeRate, 1.0) || !nearly(a.MeanCostUSD, 3.00) {
		t.Errorf("checksum-a = %+v, want 2/2 merged at $3.00 mean", a)
	}
	b := policyRow(t, rep, "checksum-b")
	if b.Runs != 1 || b.Escalated != 1 || b.MergeRate != 0 {
		t.Errorf("checksum-b = %+v, want a single escalation", b)
	}
	unknown := policyRow(t, rep, ConfigChecksumUnknown)
	if unknown.Runs != 1 || unknown.Other != 1 {
		t.Fatalf("unknown = %+v, want the in-flight run counted as other", unknown)
	}
	// An in-flight run has no cost to attribute, so it must not drag the mean.
	if unknown.CostedRuns != 0 || unknown.TotalCostUSD != 0 || unknown.MeanCostUSD != 0 {
		t.Errorf("unknown cost = %+v, want no cost attributed to an unfinished run", unknown)
	}
	// The two opus runs share a stage-model row across different policies.
	if got := stageModelRow(t, rep, "implement", "opus"); got.Runs != 2 || got.Escalated != 1 || got.Other != 1 {
		t.Errorf("(implement, opus) = %+v", got)
	}
}

// A regression is attributed through the run's MR, and one that cannot be
// linked is reported as unlinked rather than assigned to a guess.
func TestBuildConfigOutcomeReport_AttributesRegressionsThroughMR(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	events := &fakeEventLister{events: []*store.Event{
		provenanceEvent(now.Add(-time.Hour), "run-a1", "checksum-a", map[string]string{"implement": "sonnet"}),
		provenanceEvent(now.Add(-2*time.Hour), "run-b1", "checksum-b", map[string]string{"implement": "opus"}),
		regressionEvent(now.Add(-30*time.Minute), 101),
		// MR 999 was produced by a run that started before the window.
		regressionEvent(now.Add(-31*time.Minute), 999),
	}}
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-a1", store.PipelineDone, 2.00, iid(101)),
		terminalRun("run-b1", store.PipelineDone, 2.00, iid(102)),
	}}

	rep, err := BuildConfigOutcomeReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.Regressions.Total != 2 || rep.Regressions.Linked != 1 || rep.Regressions.Unlinked != 1 {
		t.Fatalf("regressions = %+v, want 2 total / 1 linked / 1 unlinked", rep.Regressions)
	}
	if got := policyRow(t, rep, "checksum-a"); got.Regressions != 1 {
		t.Errorf("checksum-a regressions = %d, want 1", got.Regressions)
	}
	if got := policyRow(t, rep, "checksum-b"); got.Regressions != 0 {
		t.Errorf("checksum-b regressions = %d, want 0 — its MR was never reverted", got.Regressions)
	}
	if got := stageModelRow(t, rep, "implement", "sonnet"); got.Regressions != 1 {
		t.Errorf("(implement, sonnet) regressions = %d, want 1", got.Regressions)
	}
	if rep.Totals.Regressions != 1 {
		t.Errorf("totals regressions = %d, want only the linked one", rep.Totals.Regressions)
	}
}

// Events outside the window are dropped even when the lister hands them over —
// a clock-skewed future stamp cannot land in a closed window.
func TestBuildConfigOutcomeReport_WindowFiltering(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	events := &fakeEventLister{events: []*store.Event{
		provenanceEvent(now.Add(-30*time.Minute), "run-in", "checksum-a", map[string]string{"implement": "sonnet"}),
		provenanceEvent(now.Add(time.Hour), "run-future", "checksum-b", map[string]string{"implement": "opus"}),
	}}
	// fakeEventLister filters on `since`; the future stamp is the one the real
	// newest-first scan would also return.
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-in", store.PipelineDone, 1.00, nil),
		terminalRun("run-future", store.PipelineDone, 1.00, nil),
	}}

	rep, err := BuildConfigOutcomeReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.StampedRuns != 1 || len(rep.PerPolicyChecksum) != 1 {
		t.Fatalf("report = %+v, want only the in-window stamp", rep)
	}
	if rep.PerPolicyChecksum[0].PolicyChecksum != "checksum-a" {
		t.Errorf("policy = %q, want checksum-a", rep.PerPolicyChecksum[0].PolicyChecksum)
	}
	// The future run is still a terminal run the window saw without a stamp.
	if rep.UncoveredRuns != 1 {
		t.Errorf("uncovered_runs = %d, want the unattributable run counted", rep.UncoveredRuns)
	}
}

func TestBuildConfigOutcomeReport_ZeroEvidence(t *testing.T) {
	now := time.Now().UTC()
	rep, err := BuildConfigOutcomeReport(context.Background(),
		&fakeEventLister{}, &fakeRunLister{}, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !rep.ZeroEvidence || rep.StampedRuns != 0 {
		t.Fatalf("empty window = %+v, want a stated zero-evidence finding", rep)
	}
	if len(rep.PerPolicyChecksum) != 0 || len(rep.PerStageModel) != 0 {
		t.Errorf("empty window must produce no group rows: %+v", rep)
	}
}

// A stamp with no run id cannot be joined to anything and must not aggregate
// under an empty key, where it would pad a real configuration's row.
func TestBuildConfigOutcomeReport_DropsUnattributableStamps(t *testing.T) {
	now := time.Now().UTC()
	orphan := provenanceEvent(now.Add(-time.Hour), "", "checksum-a", map[string]string{"implement": "sonnet"})
	orphan.SubjectID = ""

	rep, err := BuildConfigOutcomeReport(context.Background(),
		&fakeEventLister{events: []*store.Event{orphan}}, &fakeRunLister{}, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.StampedRuns != 0 || !rep.ZeroEvidence {
		t.Fatalf("report = %+v, want the orphan stamp dropped", rep)
	}
}

func TestBuildConfigOutcomeReport_RejectsBadInput(t *testing.T) {
	now := time.Now().UTC()
	if _, err := BuildConfigOutcomeReport(context.Background(), nil, &fakeRunLister{}, now.Add(-time.Hour), now); err == nil {
		t.Error("nil events lister must be rejected")
	}
	if _, err := BuildConfigOutcomeReport(context.Background(), &fakeEventLister{}, nil, now.Add(-time.Hour), now); err == nil {
		t.Error("nil runs lister must be rejected")
	}
	if _, err := BuildConfigOutcomeReport(context.Background(), &fakeEventLister{}, &fakeRunLister{}, now, now); err == nil {
		t.Error("empty window must be rejected")
	}
	if _, err := BuildConfigOutcomeReport(context.Background(),
		&fakeEventLister{err: errors.New("boom")}, &fakeRunLister{}, now.Add(-time.Hour), now); err == nil {
		t.Error("events read error must surface")
	}
	if _, err := BuildConfigOutcomeReport(context.Background(),
		&fakeEventLister{}, &fakeRunLister{err: errors.New("boom")}, now.Add(-time.Hour), now); err == nil {
		t.Error("runs read error must surface")
	}
}

// A truncated scan ranks configurations against a reality the report never
// saw, so both saturations are errors rather than quiet under-counts.
func TestBuildConfigOutcomeReport_SaturationIsAnError(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	runs := make([]*store.RunTerminalOutcome, configOutcomeRunLimit)
	for i := range runs {
		runs[i] = terminalRun("run", store.PipelineDone, 1.00, nil)
	}
	if _, err := BuildConfigOutcomeReport(context.Background(), &fakeEventLister{}, &fakeRunLister{runs: runs}, since, now); err == nil {
		t.Error("saturated run scan must error rather than truncate")
	}

	events := make([]*store.Event, configOutcomeEventLimit)
	for i := range events {
		events[i] = provenanceEvent(now.Add(-time.Hour), "run", "checksum-a", nil)
	}
	if _, err := BuildConfigOutcomeReport(context.Background(), &fakeEventLister{events: events}, &fakeRunLister{}, since, now); err == nil {
		t.Error("saturated event scan must error rather than truncate")
	}
}

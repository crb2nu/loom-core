package store

import (
	"context"
	"testing"
	"time"
)

func putEvidenceRun(t *testing.T, st *Store, id string, startedAt time.Time, state PipelineState, mutate func(*PipelineRun)) *PipelineRun {
	t.Helper()
	ctx := context.Background()
	if err := st.Backlog.Put(ctx, &BacklogItem{
		ID: id, Title: "item " + id, State: BacklogEscalated, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("put backlog %s: %v", id, err)
	}
	run := &PipelineRun{
		ID: "RUN-" + id, BacklogID: id, Template: "t",
		State: state, CurrentStage: "ci_watch", Attempts: 1, StartedAt: startedAt,
	}
	if mutate != nil {
		mutate(run)
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("put run %s: %v", run.ID, err)
	}
	return run
}

func putEvidenceStage(t *testing.T, st *Store, runID, stage string, attempt int, startedAt time.Time, logTail string) {
	t.Helper()
	outcome := StageOutcomeError
	if err := st.Pipeline.PutStage(context.Background(), &StageResult{
		PipelineRunID: runID, Stage: stage, Attempt: attempt,
		StartedAt: startedAt, Outcome: &outcome, LogTail: logTail,
	}); err != nil {
		t.Fatalf("put stage %s/%s: %v", runID, stage, err)
	}
}

// TestPipeline_ListEscalationEvidence covers the four things the signature
// miner depends on: only escalated runs, the window bound, the LAST non-empty
// log tail as evidence, and the classified flag treating all escalation
// markers as one group.
func TestPipeline_ListEscalationEvidence(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	putEvidenceRun(t, st, "MILLS-UNCLASSIFIED", now.Add(-time.Hour), PipelineEscalated, nil)
	putEvidenceStage(t, st, "RUN-MILLS-UNCLASSIFIED", "implement", 1, now.Add(-2*time.Hour), "early noise")
	putEvidenceStage(t, st, "RUN-MILLS-UNCLASSIFIED", "ci_watch", 1, now.Add(-time.Hour), "fatal: knitter refused sync")

	// Legacy shape: escalation_class set, escalation_failure_class still NULL.
	// Treating the markers as one group is what keeps this run out of the
	// mining corpus.
	putEvidenceRun(t, st, "MILLS-LEGACY-CLASS", now.Add(-2*time.Hour), PipelineEscalated, func(run *PipelineRun) {
		run.EscalationClass = "infra"
	})
	putEvidenceStage(t, st, "RUN-MILLS-LEGACY-CLASS", "ci_watch", 1, now.Add(-2*time.Hour), "longhorn: no available disk")

	// Classified only by the retryable flag — still classified.
	retryable := true
	putEvidenceRun(t, st, "MILLS-RETRYABLE", now.Add(-3*time.Hour), PipelineEscalated, func(run *PipelineRun) {
		run.EscalationRetryable = &retryable
	})
	putEvidenceStage(t, st, "RUN-MILLS-RETRYABLE", "ci_watch", 1, now.Add(-3*time.Hour), "gitlab 502")

	// Not escalated: never evidence of an unexplained failure.
	putEvidenceRun(t, st, "MILLS-DONE", now.Add(-time.Hour), PipelineDone, nil)
	putEvidenceStage(t, st, "RUN-MILLS-DONE", "ci_watch", 1, now.Add(-time.Hour), "all green")

	// Outside the window.
	putEvidenceRun(t, st, "MILLS-OLD", now.Add(-48*time.Hour), PipelineEscalated, nil)
	putEvidenceStage(t, st, "RUN-MILLS-OLD", "ci_watch", 1, now.Add(-48*time.Hour), "ancient failure")

	got, err := st.Pipeline.ListEscalationEvidence(ctx, now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list escalation evidence: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}

	byRun := map[string]*EscalationEvidence{}
	for _, rec := range got {
		byRun[rec.RunID] = rec
	}
	unclassified, ok := byRun["RUN-MILLS-UNCLASSIFIED"]
	if !ok {
		t.Fatalf("unclassified run missing: %+v", got)
	}
	if unclassified.Classified {
		t.Error("run with no escalation markers reported classified")
	}
	if unclassified.Evidence != "fatal: knitter refused sync" {
		t.Errorf("evidence = %q, want the LAST stage's log tail", unclassified.Evidence)
	}
	if unclassified.Stage != "ci_watch" {
		t.Errorf("stage = %q, want ci_watch", unclassified.Stage)
	}
	for _, runID := range []string{"RUN-MILLS-LEGACY-CLASS", "RUN-MILLS-RETRYABLE"} {
		rec, ok := byRun[runID]
		if !ok {
			t.Fatalf("%s missing from window: %+v", runID, got)
		}
		if !rec.Classified {
			t.Errorf("%s reported unclassified — every escalation marker counts", runID)
		}
	}
	if got[0].StartedAt.Before(got[len(got)-1].StartedAt) {
		t.Errorf("rows are not newest-first: %s then %s", got[0].StartedAt, got[len(got)-1].StartedAt)
	}
}

// TestPipeline_ListEscalationEvidenceNoStages: an escalated run that never
// recorded a log tail comes back with empty evidence rather than being dropped,
// so a caller can tell "no evidence" apart from "no such run".
func TestPipeline_ListEscalationEvidenceNoStages(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	putEvidenceRun(t, st, "MILLS-BARE", now.Add(-time.Hour), PipelineEscalated, nil)

	got, err := st.Pipeline.ListEscalationEvidence(context.Background(), now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list escalation evidence: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].Evidence != "" || got[0].Stage != "" {
		t.Errorf("bare run = %+v, want empty evidence", got[0])
	}
}

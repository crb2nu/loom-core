package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestListStagesOrdersSameSecondAttemptsChronologically is the store-level
// regression for the TestHandlePipelineRunRetryAttempts flake: attempts
// written inside one second, where one started_at has a fraction that
// time.RFC3339Nano used to trim (.848100 → ".8481"), must come back in
// chronological order, and exact ties must come back in insertion order.
func TestListStagesOrdersSameSecondAttemptsChronologically(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.Backlog.Put(ctx, &BacklogItem{ID: "BL-ORDER", Title: "t", State: BacklogRunning, Priority: P2, CreatedBy: "test"}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	base := time.Date(2026, 9, 15, 23, 2, 26, 0, time.UTC)
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{ID: "RUN-ORDER", BacklogID: "BL-ORDER", Template: "t", State: PipelineTesting, Attempts: 1, StartedAt: base}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Insertion order mirrors the flaky test: the row whose fraction ends in
	// zeros (attempt 3, .848100) is the earliest but was written after the
	// two it should precede. Attempts 4 and 5 sit at the whole second and at
	// .5 to cover the no-fraction and short-fraction shapes.
	offsets := []time.Duration{848150000, 848191000, 848100000, 0, 500 * time.Millisecond}
	for i, off := range offsets {
		if err := st.Pipeline.PutStage(ctx, &StageResult{PipelineRunID: "RUN-ORDER", Stage: "tests", Attempt: i + 1, StartedAt: base.Add(off)}); err != nil {
			t.Fatalf("put stage %d: %v", i+1, err)
		}
	}
	// Exact ties: three gate stages at the same instant.
	for attempt := 1; attempt <= 3; attempt++ {
		if err := st.Pipeline.PutStage(ctx, &StageResult{PipelineRunID: "RUN-ORDER", Stage: "gate", Attempt: attempt, StartedAt: base.Add(time.Second)}); err != nil {
			t.Fatalf("put gate stage %d: %v", attempt, err)
		}
	}

	stages, err := st.Pipeline.ListStages(ctx, "RUN-ORDER")
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	var got []string
	for _, s := range stages {
		got = append(got, fmt.Sprintf("%s/%d", s.Stage, s.Attempt))
	}
	want := "[tests/4 tests/5 tests/3 tests/1 tests/2 gate/1 gate/2 gate/3]"
	if fmt.Sprint(got) != want {
		t.Fatalf("ListStages order = %v, want %v", got, want)
	}
	for i := 1; i < len(stages); i++ {
		if stages[i].StartedAt.Before(stages[i-1].StartedAt) {
			t.Fatalf("stage %d (%v) listed after %d (%v)", i, stages[i].StartedAt, i-1, stages[i-1].StartedAt)
		}
	}
}

// TestListGatesBreaksTiesByID: gates evaluated in the same instant list in
// insertion order rather than heap order.
func TestListGatesBreaksTiesByID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.Backlog.Put(ctx, &BacklogItem{ID: "BL-GATES", Title: "t", State: BacklogRunning, Priority: P2, CreatedBy: "test"}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	at := time.Date(2026, 9, 15, 23, 2, 26, 848100000, time.UTC)
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{ID: "RUN-GATES", BacklogID: "BL-GATES", Template: "t", State: PipelineTesting, Attempts: 1, StartedAt: at}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	names := []string{"lint", "tests", "review", "ci"}
	for i, name := range names {
		// The last gate is a hair earlier than the tied trio and must lead.
		evaluated := at
		if i == len(names)-1 {
			evaluated = at.Add(-50 * time.Nanosecond)
		}
		if err := st.Pipeline.PutGate(ctx, &GateOutcome{PipelineRunID: "RUN-GATES", AfterStage: "tests", GateName: name, Outcome: GateOutcomePass, JudgedBy: "test", EvaluatedAt: evaluated}); err != nil {
			t.Fatalf("put gate %s: %v", name, err)
		}
	}
	gates, err := st.Pipeline.ListGates(ctx, "RUN-GATES")
	if err != nil {
		t.Fatalf("list gates: %v", err)
	}
	var got []string
	for _, g := range gates {
		got = append(got, g.GateName)
	}
	if want := "[ci lint tests review]"; fmt.Sprint(got) != want {
		t.Fatalf("ListGates order = %v, want %v", got, want)
	}
}

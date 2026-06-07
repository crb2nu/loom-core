package monitor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func newWorkflowMonitorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "wf.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestMillsWorkflowMonitor_Collect verifies the monitor assembles a snapshot of
// active runs + step deltas + counters from the DAO.
func TestMillsWorkflowMonitor_Collect(t *testing.T) {
	st := newWorkflowMonitorStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// One running imperative run with two steps.
	// No BacklogID: avoids the workflow_runs.backlog_id FK to backlog_items
	// (the monitor doesn't care about backlog linkage).
	if err := st.Workflow.PutWorkflowRun(ctx, &store.WorkflowRun{
		ID: "WF-RUN", Engine: store.WorkflowEngineImperative,
		Template: "wf", TemplateVersion: "v1", InterpreterVersion: "h1",
		State: store.WorkflowRunRunning, StartedAt: &now, CostUSD: 0.5,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}
	if _, err := st.Workflow.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-RUN", StepKey: "s1", EventType: store.WorkflowEventSpawnResult,
		CallHash: "s1", Status: store.WorkflowStepSuccess, StartedAt: &now, EndedAt: &now,
		CostSource: store.WorkflowCostReal, EffectCount: 1,
	}); err != nil {
		t.Fatalf("append s1: %v", err)
	}
	// A quarantined run (not running, so excluded from ActiveRuns but counted).
	if err := st.Workflow.PutWorkflowRun(ctx, &store.WorkflowRun{
		ID: "WF-Q", Engine: store.WorkflowEngineImperative,
		Template: "wf", TemplateVersion: "v1", InterpreterVersion: "h1",
		State: store.WorkflowRunQuarantined, StartedAt: &now,
	}); err != nil {
		t.Fatalf("put quar run: %v", err)
	}

	m := NewMillsWorkflowMonitor(st.Workflow, nil)
	snap, err := m.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.ActiveRunCount != 1 || len(snap.ActiveRuns) != 1 || snap.ActiveRuns[0].ID != "WF-RUN" {
		t.Fatalf("active runs wrong: %+v", snap.ActiveRuns)
	}
	if snap.QuarantinedCount != 1 {
		t.Errorf("quarantined count: got %d want 1", snap.QuarantinedCount)
	}
	if len(snap.RecentSteps) != 1 || snap.RecentSteps[0].StepKey != "s1" {
		t.Errorf("recent steps wrong: %+v", snap.RecentSteps)
	}
}

// TestMillsWorkflowMonitor_NilDAOInert ensures a nil DAO makes the monitor a
// benign no-op (degraded boot safety).
func TestMillsWorkflowMonitor_NilDAOInert(t *testing.T) {
	m := NewMillsWorkflowMonitor(nil, nil)
	snap, err := m.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.ActiveRunCount != 0 || len(snap.ActiveRuns) != 0 {
		t.Errorf("nil dao should produce empty snapshot, got %+v", snap)
	}
}

// TestMillsWorkflowMonitor_RefreshFiresOnRefresh verifies Refresh updates the
// cached snapshot and fires the OnRefresh callback (the SSE broadcast hook).
func TestMillsWorkflowMonitor_RefreshFiresOnRefresh(t *testing.T) {
	st := newWorkflowMonitorStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.Workflow.PutWorkflowRun(ctx, &store.WorkflowRun{
		ID: "WF-R", Engine: store.WorkflowEngineImperative, Template: "wf",
		TemplateVersion: "v1", InterpreterVersion: "h1",
		State: store.WorkflowRunRunning, StartedAt: &now,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}

	m := NewMillsWorkflowMonitor(st.Workflow, nil)
	var fired int
	m.OnRefresh(func(MillsWorkflowSnapshot) { fired++ })
	if err := m.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if fired != 1 {
		t.Errorf("OnRefresh fired %d times, want 1", fired)
	}
	if m.Snapshot().ActiveRunCount != 1 {
		t.Errorf("cached snapshot not updated: %+v", m.Snapshot())
	}
}

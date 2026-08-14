package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPipeline_CountBudgetedSince guards the budget-accounting fix for the
// 2026-07-02 spawn-pool wedge: a run that escalated at its spawn call because
// the pool was saturated (class=transient_quota, cost $0, no worker acquired)
// must NOT consume the daily MaxRunsPerDay budget, while every run that did
// real work — or failed for a real (code/infra) reason — still counts so the
// cap stays protective against runaway loops.
func TestPipeline_CountBudgetedSince(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{
		ID: "MILLS-BUDGET", Title: "budget", State: BacklogQueued, Priority: P2, CreatedBy: "test",
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}

	now := time.Now().UTC()
	attempt := 0
	put := func(id string, state PipelineState, cost float64, startedAt time.Time, class string) {
		t.Helper()
		attempt++
		if err := st.Pipeline.PutRun(ctx, &PipelineRun{
			ID: id, BacklogID: item.ID, Template: "t", State: state,
			Attempts: attempt, StartedAt: startedAt, CostUSD: cost,
		}); err != nil {
			t.Fatalf("put run %s: %v", id, err)
		}
		if class != "" {
			if err := st.Pipeline.SetEscalationClass(ctx, id, class); err != nil {
				t.Fatalf("set escalation class %s: %v", id, err)
			}
		}
	}

	in := now.Add(-1 * time.Hour)
	// Discounted: escalated, $0, transient_quota (never got a worker).
	put("ESC-QUOTA-NOOP", PipelineEscalated, 0, in, escalationClassNoWorkQuota)
	// Discounted: terminal config escalations (first-sight verdict no retry
	// can change; the item parks escalated so there is no loop to bound).
	// The 2026-07-07 scope-gate storm: real implement spend, then the gate
	// discarded it — must not also burn a run slot.
	put("ESC-CONFIG-WORKED", PipelineEscalated, 0.67, in, escalationClassTerminalConfig)
	put("ESC-CONFIG-NOOP", PipelineEscalated, 0, in, escalationClassTerminalConfig)
	// Every one of these must still count.
	put("DONE", PipelineDone, 5, in, "")                                            // merged real work
	put("RUNNING", PipelineImplementing, 0, in, "")                                 // in flight
	put("ESC-CODE", PipelineEscalated, 0, in, "code")                               // real failure, $0
	put("ESC-INFRA", PipelineEscalated, 0, in, "infra")                             // real failure, $0
	put("ESC-NULL", PipelineEscalated, 0, in, "")                                   // gate-fail: no class marker
	put("ESC-QUOTA-WORKED", PipelineEscalated, 2.5, in, escalationClassNoWorkQuota) // did billable work first
	put("DONE-CONFIG-CLASS", PipelineDone, 1, in, escalationClassTerminalConfig)    // class without escalated state still counts

	since := now.Add(-24 * time.Hour)

	total, err := st.Pipeline.CountSince(ctx, since)
	if err != nil {
		t.Fatalf("count-since: %v", err)
	}
	if total != 10 {
		t.Fatalf("CountSince = %d, want 10 (every row in window)", total)
	}

	budgeted, err := st.Pipeline.CountBudgetedSince(ctx, since)
	if err != nil {
		t.Fatalf("count-budgeted-since: %v", err)
	}
	if budgeted != 7 {
		t.Fatalf("CountBudgetedSince = %d, want 7 (ESC-QUOTA-NOOP + both ESC-CONFIG-* discounted)", budgeted)
	}

	// Window bound holds: a no-op capacity escalation that started before the
	// window is out of both counts (nothing to discount there).
	put("OLD-NOOP", PipelineEscalated, 0, now.Add(-48*time.Hour), escalationClassNoWorkQuota)
	total2, err := st.Pipeline.CountSince(ctx, since)
	if err != nil {
		t.Fatalf("count-since 2: %v", err)
	}
	if total2 != 10 {
		t.Fatalf("CountSince after old row = %d, want 10 (old row outside window)", total2)
	}
	budgeted2, err := st.Pipeline.CountBudgetedSince(ctx, since)
	if err != nil {
		t.Fatalf("count-budgeted-since 2: %v", err)
	}
	if budgeted2 != 7 {
		t.Fatalf("CountBudgetedSince after old row = %d, want 7", budgeted2)
	}
}

// TestPipeline_CountEscalationsByClassSince covers the by-class aggregation
// that feeds the HUD's escalations breakdown: escalated runs are grouped by
// their stamped fault class, runs with no class marker fall into
// "unclassified", non-escalated runs are excluded, and out-of-window rows
// don't leak in. The returned map must sum to the escalated-run count.
func TestPipeline_CountEscalationsByClassSince(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{ID: "MILLS-ESC-CLASS", Title: "esc", State: BacklogQueued, Priority: P2, CreatedBy: "test"}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}

	now := time.Now().UTC()
	attempt := 0
	put := func(id string, state PipelineState, startedAt time.Time, class string) {
		t.Helper()
		attempt++
		if err := st.Pipeline.PutRun(ctx, &PipelineRun{
			ID: id, BacklogID: item.ID, Template: "t", State: state,
			Attempts: attempt, StartedAt: startedAt,
		}); err != nil {
			t.Fatalf("put run %s: %v", id, err)
		}
		if class != "" {
			if err := st.Pipeline.SetEscalationClass(ctx, id, class); err != nil {
				t.Fatalf("set escalation class %s: %v", id, err)
			}
		}
	}

	in := now.Add(-1 * time.Hour)
	put("EC-CODE-1", PipelineEscalated, in, "code")
	put("EC-CODE-2", PipelineEscalated, in, "code")
	put("EC-INFRA", PipelineEscalated, in, "infra")
	put("EC-QUOTA", PipelineEscalated, in, "transient_quota")
	put("EC-NULL", PipelineEscalated, in, "")                        // no class marker → unclassified
	put("EC-DONE", PipelineDone, in, "")                             // not escalated → excluded
	put("EC-RUNNING", PipelineImplementing, in, "")                  // not escalated → excluded
	put("EC-OLD", PipelineEscalated, now.Add(-48*time.Hour), "code") // out of window → excluded

	since := now.Add(-24 * time.Hour)
	got, err := st.Pipeline.CountEscalationsByClassSince(ctx, since)
	if err != nil {
		t.Fatalf("count-escalations-by-class: %v", err)
	}
	want := map[string]int{"code": 2, "infra": 1, "transient_quota": 1, "unclassified": 1}
	if len(got) != len(want) {
		t.Fatalf("class buckets = %v, want %v", got, want)
	}
	sum := 0
	for k, v := range want {
		if got[k] != v {
			t.Errorf("class %q = %d, want %d", k, got[k], v)
		}
		sum += want[k]
	}
	// The breakdown must account for exactly the escalated runs in window (5).
	escalated := 0
	for _, v := range got {
		escalated += v
	}
	if escalated != sum || escalated != 5 {
		t.Fatalf("sum of by-class = %d, want 5 (all in-window escalated runs)", escalated)
	}

	// Empty window → never-nil empty map, no error.
	empty, err := st.Pipeline.CountEscalationsByClassSince(ctx, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("count on empty window: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty-window result = %v, want non-nil empty map", empty)
	}
}

// TestPipeline_SetEscalationClass covers the writer's edge cases: an empty
// class is a no-op (column stays NULL → run keeps counting), and an unknown
// run id surfaces ErrNotFound rather than silently succeeding.
func TestPipeline_SetEscalationClass(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{ID: "MILLS-SEC", Title: "sec", State: BacklogQueued, Priority: P2, CreatedBy: "test"}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}
	run := &PipelineRun{
		ID: "PIPE-SEC-1", BacklogID: item.ID, Template: "t",
		State: PipelineEscalated, Attempts: 1, StartedAt: time.Now().UTC(),
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("put run: %v", err)
	}

	// Empty class → no-op, no error, still counts against the budget.
	if err := st.Pipeline.SetEscalationClass(ctx, run.ID, ""); err != nil {
		t.Fatalf("empty class should be a no-op, got: %v", err)
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	if n, err := st.Pipeline.CountBudgetedSince(ctx, since); err != nil || n != 1 {
		t.Fatalf("budgeted after empty class = %d (err %v), want 1", n, err)
	}

	// Unknown id → ErrNotFound.
	if err := st.Pipeline.SetEscalationClass(ctx, "PIPE-DOES-NOT-EXIST", "code"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetEscalationClass on missing run = %v, want ErrNotFound", err)
	}

	// Stamping the exempt class discounts the run.
	if err := st.Pipeline.SetEscalationClass(ctx, run.ID, escalationClassNoWorkQuota); err != nil {
		t.Fatalf("set exempt class: %v", err)
	}
	if n, err := st.Pipeline.CountBudgetedSince(ctx, since); err != nil || n != 0 {
		t.Fatalf("budgeted after exempt class = %d (err %v), want 0", n, err)
	}
}

func TestPipeline_SetEscalationMetadata_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{ID: "MILLS-META", Title: "meta", State: BacklogQueued, Priority: P2, CreatedBy: "test"}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}
	run := &PipelineRun{
		ID: "PIPE-META-1", BacklogID: item.ID, Template: "t",
		State: PipelineEscalated, Attempts: 1, StartedAt: time.Now().UTC(),
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("put run: %v", err)
	}

	retryable := false
	exhausted := true
	err := st.Pipeline.SetEscalationMetadata(ctx, run.ID, EscalationMetadata{
		EscalationClass:      escalationClassTerminalConfig,
		FailureClass:         "configuration",
		ExternalDependencyID: "external_dependency.gitlab.auth_failure",
		ExternalDependency:   "gitlab",
		Retryable:            &retryable,
		RetryExhausted:       &exhausted,
	})
	if err != nil {
		t.Fatalf("set metadata: %v", err)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.EscalationClass != escalationClassTerminalConfig ||
		got.FailureClass != "configuration" ||
		got.ExternalDependencyID != "external_dependency.gitlab.auth_failure" ||
		got.ExternalDependency != "gitlab" {
		t.Fatalf("metadata = %+v", got)
	}
	if got.EscalationRetryable == nil || *got.EscalationRetryable {
		t.Fatalf("EscalationRetryable = %v, want false", got.EscalationRetryable)
	}
	if got.RetryExhausted == nil || !*got.RetryExhausted {
		t.Fatalf("RetryExhausted = %v, want true", got.RetryExhausted)
	}

	runs, err := st.Pipeline.ListRecentTerminal(ctx, time.Now().Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list terminal: %v", err)
	}
	if len(runs) != 1 || runs[0].ExternalDependency != "gitlab" {
		t.Fatalf("terminal metadata = %+v", runs)
	}

	if err := st.Pipeline.SetEscalationMetadata(ctx, "PIPE-MISSING", EscalationMetadata{FailureClass: "code"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run err = %v, want ErrNotFound", err)
	}
}

func TestPipeline_SetEscalationMetadata_PartialUpdatePreservesExistingFields(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{ID: "MILLS-META-PARTIAL", Title: "meta partial", State: BacklogQueued, Priority: P2, CreatedBy: "test"}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}
	run := &PipelineRun{
		ID: "PIPE-META-PARTIAL-1", BacklogID: item.ID, Template: "t",
		State: PipelineEscalated, Attempts: 1, StartedAt: time.Now().UTC(),
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("put run: %v", err)
	}

	retryable := true
	if err := st.Pipeline.SetEscalationMetadata(ctx, run.ID, EscalationMetadata{
		EscalationClass:      "transient",
		FailureClass:         "transient",
		ExternalDependencyID: "external_dependency.gitlab.pipeline_unavailable",
		ExternalDependency:   "gitlab",
		Retryable:            &retryable,
	}); err != nil {
		t.Fatalf("set initial metadata: %v", err)
	}

	if err := st.Pipeline.SetEscalationMetadata(ctx, run.ID, EscalationMetadata{
		FailureClass: "infrastructure",
	}); err != nil {
		t.Fatalf("set partial metadata: %v", err)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.EscalationClass != "transient" {
		t.Fatalf("EscalationClass = %q, want preserved transient", got.EscalationClass)
	}
	if got.FailureClass != "infrastructure" {
		t.Fatalf("FailureClass = %q, want infrastructure", got.FailureClass)
	}
	if got.ExternalDependencyID != "external_dependency.gitlab.pipeline_unavailable" ||
		got.ExternalDependency != "gitlab" {
		t.Fatalf("external dependency metadata = %q/%q, want preserved gitlab incident",
			got.ExternalDependencyID, got.ExternalDependency)
	}
	if got.EscalationRetryable == nil || !*got.EscalationRetryable {
		t.Fatalf("EscalationRetryable = %v, want preserved true", got.EscalationRetryable)
	}
}

func TestPipeline_ListRecentClassifiedCIFailures(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, item := range []*BacklogItem{
		{ID: "MILLS-CI-1", Title: "GitLab auth outage", State: BacklogEscalated, Priority: P1, CreatedBy: "test"},
		{ID: "MILLS-CI-2", Title: "Unit test regression", State: BacklogEscalated, Priority: P2, CreatedBy: "test"},
		{ID: "MILLS-OLD", Title: "Old CI failure", State: BacklogEscalated, Priority: P2, CreatedBy: "test"},
		{ID: "MILLS-NONCI", Title: "Non-CI failure", State: BacklogEscalated, Priority: P2, CreatedBy: "test"},
	} {
		if err := st.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("put backlog %s: %v", item.ID, err)
		}
	}

	retryableFalse := false
	retryableTrue := true
	runs := []*PipelineRun{
		{
			ID: "PIPE-CI-NEW", BacklogID: "MILLS-CI-1", Template: "t",
			State: PipelineEscalated, CurrentStage: "ci_watch", Attempts: 1,
			StartedAt: now.Add(-time.Hour), EscalationClass: "config",
			FailureClass: "configuration", ExternalDependencyID: "external_dependency.gitlab.auth_failure",
			ExternalDependency: "gitlab", EscalationRetryable: &retryableFalse,
		},
		{
			ID: "PIPE-CI-STAGE", BacklogID: "MILLS-CI-2", Template: "t",
			State: PipelineEscalated, CurrentStage: "merge", Attempts: 1,
			StartedAt: now.Add(-2 * time.Hour), EscalationClass: "code",
			FailureClass: "code", EscalationRetryable: &retryableTrue,
		},
		{
			ID: "PIPE-CI-OLD", BacklogID: "MILLS-OLD", Template: "t",
			State: PipelineEscalated, CurrentStage: "ci_watch", Attempts: 1,
			StartedAt: now.Add(-48 * time.Hour), EscalationClass: "code",
			FailureClass: "code", EscalationRetryable: &retryableTrue,
		},
		{
			ID: "PIPE-NONCI", BacklogID: "MILLS-NONCI", Template: "t",
			State: PipelineEscalated, CurrentStage: "tests", Attempts: 1,
			StartedAt: now.Add(-time.Hour), EscalationClass: "code",
			FailureClass: "code", EscalationRetryable: &retryableTrue,
		},
	}
	for _, run := range runs {
		if err := st.Pipeline.PutRun(ctx, run); err != nil {
			t.Fatalf("put run %s: %v", run.ID, err)
		}
	}
	outcome := StageOutcomeError
	if err := st.Pipeline.PutStage(ctx, &StageResult{
		PipelineRunID: "PIPE-CI-STAGE",
		Stage:         "ci_watch",
		Attempt:       1,
		StartedAt:     now.Add(-2 * time.Hour),
		Outcome:       &outcome,
	}); err != nil {
		t.Fatalf("put ci stage: %v", err)
	}

	got, err := st.Pipeline.ListRecentClassifiedCIFailures(ctx, now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list recent classified ci failures: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].RunID != "PIPE-CI-NEW" || got[0].BacklogTitle != "GitLab auth outage" ||
		got[0].FailureClass != "configuration" || got[0].ExternalDependency != "gitlab" ||
		got[0].Retryable == nil || *got[0].Retryable {
		t.Fatalf("first summary = %+v", got[0])
	}
	if got[1].RunID != "PIPE-CI-STAGE" || got[1].FailureClass != "code" ||
		got[1].Retryable == nil || !*got[1].Retryable {
		t.Fatalf("second summary = %+v", got[1])
	}
	if got[1].Classifier != "mills-failure-classifier" ||
		got[1].FreeRetry == nil || *got[1].FreeRetry ||
		got[1].Terminal == nil || *got[1].Terminal {
		t.Fatalf("second summary classification semantics = %+v", got[1])
	}
	if err := st.Pipeline.SetEscalationMetadata(ctx, "PIPE-CI-STAGE", EscalationMetadata{
		FailureClass: "transient_quota",
	}); err != nil {
		t.Fatalf("update failure class: %v", err)
	}
	got, err = st.Pipeline.ListRecentClassifiedCIFailures(ctx, now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("list after update: %v", err)
	}
	if got[1].FailureClass != "transient_quota" ||
		got[1].FreeRetry == nil || !*got[1].FreeRetry ||
		got[1].Terminal == nil || *got[1].Terminal {
		t.Fatalf("transient quota summary semantics = %+v", got[1])
	}
}

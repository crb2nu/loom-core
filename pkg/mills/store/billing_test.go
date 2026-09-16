package store

import (
	"context"
	"testing"
	"time"
)

func TestBillingForBackend(t *testing.T) {
	cases := map[string]BillingClass{
		"spawn": BillingSubscription, "SPAWN ": BillingSubscription,
		"flexinfer": BillingLocal, "local": BillingLocal,
		"openrouter": BillingAPI, "anthropic": BillingAPI, "litellm": BillingAPI, "weaver": BillingAPI,
		"": "", "  ": "",
	}
	for backend, want := range cases {
		if got := BillingForBackend(backend); got != want {
			t.Errorf("BillingForBackend(%q) = %q, want %q", backend, got, want)
		}
	}
}

// TestBillingClass_RoundTripAndRollup pins the persistence contract behind
// cost attribution: stage rows keep their billing class, an idempotent
// re-write without one preserves the earlier attribution, and the run-level
// subscription roll-up survives PutRun/scan so the budget can subtract it.
func TestBillingClass_RoundTripAndRollup(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	item := &BacklogItem{ID: "B1", Title: "item", State: BacklogRunning, Priority: P2, CreatedBy: "test"}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatal(err)
	}
	run := &PipelineRun{ID: "R1", BacklogID: item.ID, Template: "t", State: PipelineImplementing, StartedAt: now, CostUSD: 3, SubscriptionCostUSD: 2}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	ok := StageOutcomeSuccess
	stages := []*StageResult{
		{PipelineRunID: run.ID, Stage: "implement", Attempt: 1, StartedAt: now, EndedAt: &now, Outcome: &ok, CostUSD: 2, Model: "codex", Backend: "spawn", Billing: BillingSubscription},
		{PipelineRunID: run.ID, Stage: "research", Attempt: 1, StartedAt: now.Add(time.Second), EndedAt: &now, Outcome: &ok, CostUSD: 1, Model: "kimi", Backend: "flexinfer", Billing: BillingLocal},
	}
	for _, sr := range stages {
		if err := st.Pipeline.PutStage(ctx, sr); err != nil {
			t.Fatal(err)
		}
	}
	// An idempotent re-write that carries no billing keeps the earlier class.
	if err := st.Pipeline.PutStage(ctx, &StageResult{PipelineRunID: run.ID, Stage: "implement", Attempt: 1, StartedAt: now, EndedAt: &now, Outcome: &ok, CostUSD: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Pipeline.ListStages(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	byStage := map[string]BillingClass{}
	for _, sr := range got {
		byStage[sr.Stage] = sr.Billing
	}
	if byStage["implement"] != BillingSubscription || byStage["research"] != BillingLocal {
		t.Fatalf("billing after round trip = %v", byStage)
	}

	runs, err := st.Pipeline.ListByBacklog(ctx, item.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListByBacklog = %v, %v", runs, err)
	}
	if runs[0].CostUSD != 3 || runs[0].SubscriptionCostUSD != 2 {
		t.Fatalf("run cost = (%v total, %v subscription), want (3, 2)", runs[0].CostUSD, runs[0].SubscriptionCostUSD)
	}
	since := now.Add(-time.Hour)
	total, err := st.Pipeline.SumCostSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := st.Pipeline.SumSubscriptionCostSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || subscription != 2 {
		t.Fatalf("SumCostSince = %v, SumSubscriptionCostSince = %v, want 3 and 2", total, subscription)
	}
}

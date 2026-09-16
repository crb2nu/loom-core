package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The daily USD cap governs metered spend only. On 2026-09-14 the start
// kernels still summed total cost, so $79 of subscription-billed harness
// time ($1.82 metered) refused every admission with "daily USD cap 75.00
// reached: spent 81.03" while four run slots sat empty. These tests pin the
// kernels to the same semantics as Budget.spentSince.

func TestClaimPipelineStart_DailyCapIgnoresSubscriptionSpend(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	first := seedClaimBacklog(t, st, "MILLS-METERED-A")
	req := claimTestRequest(first.ID)
	req.Limits = PipelineStartLimits{MaxUSDPerDay: 75}
	claim, err := st.ClaimPipelineStart(ctx, req)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// A day of harness time: $100 list-price equivalent, $99 of it billed to
	// a subscription. Metered spend is $1.
	claim.Run.State = PipelineImplementing
	claim.Run.CostUSD = 100
	claim.Run.SubscriptionCostUSD = 99
	if err := st.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("record actual cost: %v", err)
	}

	second := seedClaimBacklog(t, st, "MILLS-METERED-B")
	req = claimTestRequest(second.ID)
	req.EstimateUSD = 1
	req.Limits = PipelineStartLimits{MaxUSDPerDay: 75}
	if _, err := st.ClaimPipelineStart(ctx, req); err != nil {
		t.Fatalf("second claim with $1 metered spent: %v, want admission", err)
	}

	// The same total with nothing on a subscription is still over the cap.
	st2 := newTestStore(t)
	a := seedClaimBacklog(t, st2, "MILLS-METERED-C")
	req = claimTestRequest(a.ID)
	req.Limits = PipelineStartLimits{MaxUSDPerDay: 75}
	claim, err = st2.ClaimPipelineStart(ctx, req)
	if err != nil {
		t.Fatalf("metered claim: %v", err)
	}
	claim.Run.State = PipelineImplementing
	claim.Run.CostUSD = 100
	if err := st2.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("record metered cost: %v", err)
	}
	b := seedClaimBacklog(t, st2, "MILLS-METERED-D")
	req = claimTestRequest(b.ID)
	req.EstimateUSD = 1
	req.Limits = PipelineStartLimits{MaxUSDPerDay: 75}
	var exceeded *BudgetExceededError
	if _, err := st2.ClaimPipelineStart(ctx, req); !errors.As(err, &exceeded) {
		t.Fatalf("metered $100 claim error=%v, want BudgetExceededError", err)
	}
	if exceeded.SpentUSD != 100 {
		t.Fatalf("metered snapshot spent=%v, want 100", exceeded.SpentUSD)
	}
}

func TestClaimWorkflowStart_DailyCapIgnoresSubscriptionSpend(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pipeItem := seedClaimBacklog(t, st, "MILLS-WF-METERED-A")
	pipeReq := claimTestRequest(pipeItem.ID)
	pipeReq.Limits.MaxUSDPerDay = 10
	claim, err := st.ClaimPipelineStart(ctx, pipeReq)
	if err != nil {
		t.Fatalf("pipeline claim: %v", err)
	}
	claim.Run.State = PipelineImplementing
	claim.Run.CostUSD = 100
	claim.Run.SubscriptionCostUSD = 100
	if err := st.Pipeline.PutRun(ctx, claim.Run); err != nil {
		t.Fatalf("record subscription cost: %v", err)
	}
	wfItem := seedClaimBacklog(t, st, "MILLS-WF-METERED-B")
	wfReq := workflowClaimRequest(wfItem.ID)
	wfReq.EstimateUSD = 5
	wfReq.Limits.MaxUSDPerDay = 10
	if _, err := st.ClaimWorkflowStart(ctx, wfReq); err != nil {
		t.Fatalf("workflow claim behind $100 subscription spend: %v, want admission", err)
	}
}

func TestClaimCouncilStart_DailyCapIgnoresLocalSpend(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claim, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-METERED-1", 1, CouncilStartLimits{MaxUSDPerRun: 1, MaxUSDPerDay: 5},
	))
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	ended := councilClaimNow.Add(time.Minute)
	claim.Run.Outcome = CouncilOutcomeError
	claim.Run.EndedAt = &ended
	claim.Run.CostFrontierUSD = 0.4
	claim.Run.CostLocalUSD = 100 // flexinfer/local inference: unmetered
	if err := st.FinalizeCouncilRun(ctx, claim.Run); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := st.ClaimCouncilStart(ctx, councilClaimRequest(
		"COUNCIL-METERED-2", 1, CouncilStartLimits{MaxUSDPerRun: 1, MaxUSDPerDay: 5},
	)); err != nil {
		t.Fatalf("second claim behind $100 local spend: %v, want admission (metered 0.4 of 5)", err)
	}
}

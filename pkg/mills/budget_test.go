package mills

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeReader is a deterministic stand-in for the canonical store. Every
// budget knob is exercised by varying the field values directly.
type fakeReader struct {
	councilCost              float64
	councilLocalCost         float64
	pipelineCost             float64
	pipelineSubscriptionCost float64
	councilRuns              int
	pipelineRuns             int
	pipelineActv             int
	debateCost               float64
}

func (f *fakeReader) CouncilCostSince(_ context.Context, _ time.Time) (float64, error) {
	return f.councilCost, nil
}
func (f *fakeReader) CouncilLocalCostSince(_ context.Context, _ time.Time) (float64, error) {
	return f.councilLocalCost, nil
}
func (f *fakeReader) PipelineCostSince(_ context.Context, _ time.Time) (float64, error) {
	return f.pipelineCost, nil
}
func (f *fakeReader) PipelineSubscriptionCostSince(_ context.Context, _ time.Time) (float64, error) {
	return f.pipelineSubscriptionCost, nil
}
func (f *fakeReader) CouncilRunsSince(_ context.Context, _ time.Time) (int, error) {
	return f.councilRuns, nil
}
func (f *fakeReader) PipelineRunsSince(_ context.Context, _ time.Time) (int, error) {
	return f.pipelineRuns, nil
}
func (f *fakeReader) PipelineActiveRuns(_ context.Context) (int, error) {
	return f.pipelineActv, nil
}
func (f *fakeReader) DebateCostSince(_ context.Context, _ time.Time) (float64, error) {
	return f.debateCost, nil
}

// budgetForTest returns a Budget wired to a fixed *Policy and a fake reader.
func budgetForTest(p *Policy, r *fakeReader) *Budget {
	return &Budget{
		PolicyFunc: func() *Policy { return p },
		Reader:     r,
		Now:        func() time.Time { return time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC) },
	}
}

// TestBudget_Allow_SubscriptionSpendIsNotMetered pins the cost attribution
// contract: subscription turns (Claude Code / Codex under the cluster OAuth
// accounts) never consume the metered daily cap, are reported separately,
// and are bounded only by their own optional cap.
func TestBudget_Allow_SubscriptionSpendIsNotMetered(t *testing.T) {
	ctx := context.Background()
	p, err := ParsePolicy([]byte(fixtureV1))
	if err != nil {
		t.Fatal(err)
	}
	on := true
	p.Enabled = &on
	p.Budgets.Pipeline.MaxUSDPerDay = 10
	p.Budgets.Pipeline.MaxUSDPerRun = 5
	p.Budgets.Pipeline.MaxRunsPerDay = 0
	p.Budgets.Pipeline.MaxConcurrentRuns = 0
	// $50 attributed in the window, $45 of it subscription: metered spend is $5.
	r := &fakeReader{pipelineCost: 50, pipelineSubscriptionCost: 45}

	d, err := budgetForTest(p, r).Allow(ctx, TierPipeline, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed || d.SpentUSD != 5 || d.SubscriptionSpentUSD != 45 {
		t.Fatalf("Allow = %+v, want allowed with metered spent 5 and subscription 45", d)
	}
	if d.RemainingUSD != 5 {
		t.Fatalf("RemainingUSD = %v, want 5 (cap 10 - metered 5)", d.RemainingUSD)
	}

	// The optional subscription cap bounds the subscription slice on its own.
	p.Budgets.Pipeline.MaxSubscriptionUSDPerDay = 40
	d, err = budgetForTest(p, r).Allow(ctx, TierPipeline, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || len(d.Reasons) != 1 || !strings.Contains(d.Reasons[0], "subscription cap 40.00") {
		t.Fatalf("Allow with subscription cap = %+v, want denied by the subscription cap only", d)
	}

	// The fuel gauge reports all three views.
	u, err := budgetForTest(p, r).WindowUsage(ctx, TierPipeline)
	if err != nil {
		t.Fatal(err)
	}
	if u.SpentUSD != 5 || u.TotalSpentUSD != 50 || u.SubscriptionSpentUSD != 45 || u.SubscriptionCapUSD != 40 || u.CapUSD != 10 {
		t.Fatalf("WindowUsage = %+v", u)
	}

	// Council: local flexinfer spend is not metered either.
	p.Budgets.Council.MaxUSDPerDay = 10
	cr := &fakeReader{councilCost: 12, councilLocalCost: 4}
	d, err = budgetForTest(p, cr).Allow(ctx, TierCouncil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed || d.SpentUSD != 8 {
		t.Fatalf("council Allow = %+v, want allowed with metered spent 8", d)
	}
}

func TestBudget_Allow_KillSwitch(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	off := false
	p.Enabled = &off
	b := budgetForTest(p, &fakeReader{})

	d, err := b.Allow(context.Background(), TierPipeline, 1.0)
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if d.Allowed {
		t.Errorf("kill switch should reject")
	}
}

func TestBudget_Allow_PerRunCap(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{})

	// pipeline.max_usd_per_run = 5 in the fixture
	d, err := b.Allow(context.Background(), TierPipeline, 7.5)
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if d.Allowed {
		t.Errorf("expected reject")
	}
	if !containsReason(d, "exceeds pipeline.max_usd_per_run") {
		t.Errorf("missing per-run reason: %v", d.Reasons)
	}
}

func TestBudget_Allow_DailyCap(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{pipelineCost: 73.0}) // 75 cap

	d, err := b.Allow(context.Background(), TierPipeline, 4.0)
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if d.Allowed {
		t.Errorf("expected daily cap rejection: %+v", d)
	}
	if d.SpentUSD != 73.0 {
		t.Errorf("spent: %v", d.SpentUSD)
	}
	if d.RemainingUSD != 2.0 {
		t.Errorf("remaining: %v", d.RemainingUSD)
	}
	if !containsReason(d, "daily cap") {
		t.Errorf("missing daily reason: %v", d.Reasons)
	}
}

func TestBudget_Allow_RunCount(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{pipelineRuns: 20}) // == max_runs_per_day

	d, _ := b.Allow(context.Background(), TierPipeline, 0.5)
	if d.Allowed {
		t.Errorf("expected run-count rejection: %+v", d)
	}
	if !containsReason(d, "daily run count") {
		t.Errorf("missing run-count reason: %v", d.Reasons)
	}
}

func TestBudget_Allow_Concurrency(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{pipelineActv: 4}) // == max_concurrent_runs

	d, _ := b.Allow(context.Background(), TierPipeline, 0.5)
	if d.Allowed {
		t.Errorf("expected concurrency rejection: %+v", d)
	}
	if !containsReason(d, "max_concurrent_runs") {
		t.Errorf("missing concurrency reason: %v", d.Reasons)
	}
}

func TestBudget_Allow_HappyPath(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{
		pipelineCost: 10, pipelineRuns: 3, pipelineActv: 1,
	})

	d, err := b.Allow(context.Background(), TierPipeline, 2.0)
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if !d.Allowed {
		t.Errorf("expected allow: %+v", d)
	}
	if d.RemainingUSD != 65.0 { // 75 - 10
		t.Errorf("remaining: %v", d.RemainingUSD)
	}
}

func TestBudget_Allow_CouncilTier(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	// council has no concurrency or run-count cap configured; the spend
	// caps still apply.
	b := budgetForTest(p, &fakeReader{councilCost: 49.0})

	d, _ := b.Allow(context.Background(), TierCouncil, 5.0)
	if d.Allowed {
		t.Errorf("expected daily reject: %+v", d)
	}
	d2, _ := b.Allow(context.Background(), TierCouncil, 0.5)
	if !d2.Allowed {
		t.Errorf("expected allow under cap: %+v", d2)
	}
}

func TestBudget_Remaining(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{pipelineCost: 30.0})

	r, err := b.Remaining(context.Background(), TierPipeline)
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	if r != 45.0 { // 75 - 30
		t.Errorf("remaining: %v", r)
	}
}

func TestBudget_Allow_RejectsNegativeEstimate(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{})
	if _, err := b.Allow(context.Background(), TierPipeline, -1); err == nil {
		t.Errorf("expected error on negative estimate")
	}
}

// TestBudget_DebateSpentSince proves the Phase 5.2 informational
// helper passes through to BudgetReader.DebateCostSince. No cap
// enforcement here — the value is for HUD widgets and future
// per-debate-tier daily caps.
func TestBudget_DebateSpentSince(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	r := &fakeReader{debateCost: 4.25}
	b := budgetForTest(p, r)
	got, err := b.DebateSpentSince(context.Background())
	if err != nil {
		t.Fatalf("DebateSpentSince: %v", err)
	}
	if got != 4.25 {
		t.Errorf("DebateSpentSince: got %v want 4.25", got)
	}
}

// TestBudget_DebateSpentSince_NotConfigured surfaces a clean error
// when the reader is missing instead of returning a silent zero.
func TestBudget_DebateSpentSince_NotConfigured(t *testing.T) {
	b := &Budget{Now: func() time.Time { return time.Now() }}
	if _, err := b.DebateSpentSince(context.Background()); err == nil {
		t.Errorf("expected error when Reader is nil")
	}
}

func containsReason(d Decision, sub string) bool {
	for _, r := range d.Reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func TestBudget_WindowUsage(t *testing.T) {
	p, _ := ParsePolicy([]byte(fixtureV1))
	b := budgetForTest(p, &fakeReader{pipelineCost: 12.5, pipelineRuns: 7, councilCost: 3.25, councilRuns: 2})

	// pipeline: max_usd_per_day=75, max_runs_per_day=20 in the fixture
	u, err := b.WindowUsage(context.Background(), TierPipeline)
	if err != nil {
		t.Fatalf("window usage: %v", err)
	}
	// No subscription or local spend in this fixture, so metered == total.
	want := WindowUsage{SpentUSD: 12.5, CapUSD: 75, Runs: 7, RunsCap: 20, TotalSpentUSD: 12.5}
	if u != want {
		t.Errorf("pipeline usage = %+v, want %+v", u, want)
	}

	// council: max_usd_per_day=50, no run cap in the fixture → RunsCap 0
	u, err = b.WindowUsage(context.Background(), TierCouncil)
	if err != nil {
		t.Fatalf("window usage: %v", err)
	}
	want = WindowUsage{SpentUSD: 3.25, CapUSD: 50, Runs: 2, RunsCap: 0, TotalSpentUSD: 3.25}
	if u != want {
		t.Errorf("council usage = %+v, want %+v", u, want)
	}
}

func TestBudget_WindowUsage_Unconfigured(t *testing.T) {
	var b *Budget
	if _, err := b.WindowUsage(context.Background(), TierPipeline); err == nil {
		t.Fatalf("nil budget must error, not report an empty tank")
	}
	if _, err := budgetForTest(nil, &fakeReader{}).WindowUsage(context.Background(), "bogus"); err == nil {
		t.Fatalf("unknown tier must error")
	}
}

func TestEvaluateThroughputGuardrail(t *testing.T) {
	thresholds := ThroughputGuardrailThresholds{MaxEscalationRate: .2, MaxCostPerMergedPipelineUSD: 4, MaxScopeQueueAgeSeconds: 60, MaxStarvedQueues: 1}
	all := ThroughputSignals{EscalationRate: .2, CostPerMergedPipelineUSD: 4, ScopeMaxQueueAgeSeconds: 60, StarvedQueues: 1, HasEscalationRate: true, HasCostPerMergedPipelineUSD: true, HasScopeMaxQueueAgeSeconds: true, HasStarvedQueues: true}
	tests := []struct {
		name   string
		mutate func(*ThroughputSignals)
		want   int
	}{
		{name: "exact thresholds healthy", want: 0},
		{name: "escalation", mutate: func(s *ThroughputSignals) { s.EscalationRate = .21 }, want: 1},
		{name: "cost", mutate: func(s *ThroughputSignals) { s.CostPerMergedPipelineUSD = 4.01 }, want: 1},
		{name: "queue age", mutate: func(s *ThroughputSignals) { s.ScopeMaxQueueAgeSeconds = 61 }, want: 1},
		{name: "starvation", mutate: func(s *ThroughputSignals) { s.StarvedQueues = 2 }, want: 1},
		{name: "all", mutate: func(s *ThroughputSignals) {
			s.EscalationRate = 1
			s.CostPerMergedPipelineUSD = 9
			s.ScopeMaxQueueAgeSeconds = 99
			s.StarvedQueues = 9
		}, want: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := all
			if tc.mutate != nil {
				tc.mutate(&s)
			}
			got := EvaluateThroughputGuardrail(s, thresholds)
			if got.Breached != (tc.want > 0) || len(got.Reasons) != tc.want {
				t.Fatalf("verdict = %+v", got)
			}
		})
	}
	missing := EvaluateThroughputGuardrail(ThroughputSignals{EscalationRate: 99}, thresholds)
	if missing.Breached || missing.Reasons == nil {
		t.Fatalf("missing metrics = %+v", missing)
	}
}

func TestThroughputGuardrailReasonOrder(t *testing.T) {
	got := EvaluateThroughputGuardrail(ThroughputSignals{
		EscalationRate: .5, CostPerMergedPipelineUSD: 6,
		ScopeMaxQueueAgeSeconds: 21601, StarvedQueues: 1,
		HasEscalationRate: true, HasCostPerMergedPipelineUSD: true,
		HasScopeMaxQueueAgeSeconds: true, HasStarvedQueues: true,
	}, ThroughputGuardrailThresholds{})
	want := []string{
		"escalation_rate 0.5000 exceeds 0.2500",
		"cost_per_merged_pipeline_usd 6.00 exceeds 5.00",
		"scope_max_queue_age_seconds 21601 exceeds 21600",
		"scope_starvation_reservations 1 exceeds 0",
	}
	if !got.Breached || !reflect.DeepEqual(got.Reasons, want) {
		t.Fatalf("verdict = %+v, want ordered reasons %v", got, want)
	}
}

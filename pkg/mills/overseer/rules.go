package overseer

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Foreman anomaly-rule vocabulary. Each rule is a deterministic predicate over
// the store: it fires (returns a non-nil Anomaly) or it doesn't (nil, nil).
// No LLM involvement — the rules are the ground truth; the LLM only composes a
// human summary for the filed issue.
const (
	ruleStuckRuns          = "stuck_runs"
	ruleThroughputCollapse = "throughput_collapse"
	ruleEscalationStorm    = "escalation_storm"
	ruleBudgetBurn         = "budget_burn"

	severityWarning  = "warning"
	severityCritical = "critical"

	// foremanStuckRunEvidenceCap bounds how many run ids/ages the stuck_runs
	// evidence blob carries — enough to triage, not the whole in-flight set.
	foremanStuckRunEvidenceCap = 5
	// foremanEscalationWindow is the fixed 24h window for the escalation-storm
	// rule (the threshold is defined as "escalations in the last 24h").
	foremanEscalationWindow = 24 * time.Hour
	// foremanBudgetWindow is the fixed 1d window for the budget-burn rule.
	foremanBudgetWindow = 24 * time.Hour
)

// Anomaly is one firing KPI rule: the rule name, a coarse severity, and a
// deterministic evidence blob (used both for the anomaly_opened observation
// payload and, when an issue is filed, the LLM prompt / fallback template).
type Anomaly struct {
	Rule     string         `json:"rule"`
	Severity string         `json:"severity"`
	Evidence map[string]any `json:"evidence"`
}

// evalStuckRuns fires when any ACTIVE (non-terminal) pipeline run has been
// running longer than ForemanPolicy.StuckRunAge(). Evidence carries the count,
// the age threshold, and the oldest few run ids + ages (capped).
func evalStuckRuns(ctx context.Context, st *store.Store, fp mills.ForemanPolicy, now time.Time) (*Anomaly, error) {
	if st == nil || st.Pipeline == nil {
		return nil, nil
	}
	runs, err := st.Pipeline.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("foreman stuck_runs: list active: %w", err)
	}
	threshold := fp.StuckRunAge()
	type stuck struct {
		id  string
		age time.Duration
	}
	stuckRuns := make([]stuck, 0, len(runs))
	for _, r := range runs {
		if r == nil {
			continue
		}
		age := now.Sub(r.StartedAt)
		if age >= threshold {
			stuckRuns = append(stuckRuns, stuck{id: r.ID, age: age})
		}
	}
	if len(stuckRuns) == 0 {
		return nil, nil
	}
	// Oldest first so the evidence cap keeps the worst offenders.
	sort.Slice(stuckRuns, func(i, j int) bool { return stuckRuns[i].age > stuckRuns[j].age })
	ids := make([]string, 0, foremanStuckRunEvidenceCap)
	ages := make([]string, 0, foremanStuckRunEvidenceCap)
	for i, s := range stuckRuns {
		if i >= foremanStuckRunEvidenceCap {
			break
		}
		ids = append(ids, s.id)
		ages = append(ages, s.age.Round(time.Minute).String())
	}
	return &Anomaly{
		Rule:     ruleStuckRuns,
		Severity: severityWarning,
		Evidence: map[string]any{
			"count":     len(stuckRuns),
			"threshold": threshold.String(),
			"run_ids":   ids,
			"ages":      ages,
		},
	}, nil
}

// evalThroughputCollapse fires when the queue is non-empty AND zero pipeline
// runs reached `done` across the ZeroMergeWindow(). Both signals come straight
// from the store DAOs (queued backlog list + merged-run list), so the rule is
// window-accurate and testable by seeding rows — it does not depend on the KPI
// writer's snapshot cadence.
func evalThroughputCollapse(ctx context.Context, st *store.Store, fp mills.ForemanPolicy, now time.Time) (*Anomaly, error) {
	if st == nil || st.Pipeline == nil || st.Backlog == nil {
		return nil, nil
	}
	queued, err := st.Backlog.ListByState(ctx, store.BacklogQueued)
	if err != nil {
		return nil, fmt.Errorf("foreman throughput_collapse: list queued: %w", err)
	}
	if len(queued) == 0 {
		return nil, nil // nothing waiting ⇒ zero merges is not a collapse
	}
	window := fp.ZeroMergeWindow()
	merged, err := st.Pipeline.ListByStateSince(ctx, store.PipelineDone, now.Add(-window))
	if err != nil {
		return nil, fmt.Errorf("foreman throughput_collapse: list merged: %w", err)
	}
	if len(merged) > 0 {
		return nil, nil
	}
	return &Anomaly{
		Rule:     ruleThroughputCollapse,
		Severity: severityCritical,
		Evidence: map[string]any{
			"queued_depth":     len(queued),
			"window":           window.String(),
			"merged_in_window": 0,
		},
	}, nil
}

// evalEscalationStorm fires when escalations in the last 24h reach the storm
// threshold. Counts pipeline runs that STARTED in the window and are now in the
// escalated state, minus those whose verdict was superseded (their MRs merged
// — resolved incidents). The raw population matches the KPI writer's
// pipeline_escalated_runs; the discounted count matches its
// pipeline_escalated_active, so the operator sees consistent numbers.
func evalEscalationStorm(ctx context.Context, st *store.Store, fp mills.ForemanPolicy, now time.Time) (*Anomaly, error) {
	if st == nil || st.Pipeline == nil {
		return nil, nil
	}
	escalated, err := st.Pipeline.ListByStateSince(ctx, store.PipelineEscalated, now.Add(-foremanEscalationWindow))
	if err != nil {
		return nil, fmt.Errorf("foreman escalation_storm: list escalated: %w", err)
	}
	// Discount superseded escalations (Trustworthy Verdicts S3): a run whose
	// MR later merged is a resolved incident, and counting it kept storms
	// alive on corrected history. The raw count stays in the evidence so the
	// discount is visible, never silent.
	var superseded map[string]struct{}
	if st.Events != nil {
		superseded, err = mills.SupersededRunIDsSince(ctx, st.Events, now.Add(-foremanEscalationWindow))
		if err != nil {
			return nil, fmt.Errorf("foreman escalation_storm: superseded: %w", err)
		}
	}
	active := 0
	for _, run := range escalated {
		if run == nil {
			continue
		}
		if _, ok := superseded[run.ID]; ok {
			continue
		}
		active++
	}
	threshold := fp.StormThreshold()
	if active < threshold {
		return nil, nil
	}
	return &Anomaly{
		Rule:     ruleEscalationStorm,
		Severity: severityCritical,
		Evidence: map[string]any{
			"count":      active,
			"raw_count":  len(escalated),
			"superseded": len(escalated) - active,
			"threshold":  threshold,
			"window":     foremanEscalationWindow.String(),
		},
	}, nil
}

// evalBudgetBurn fires when 1d pipeline cost / maxUSDPerDay >= BurnRatio(). The
// rule is skipped entirely (nil, nil) when the daily pipeline budget is unset
// (0), since the ratio is undefined without a denominator.
func evalBudgetBurn(ctx context.Context, st *store.Store, fp mills.ForemanPolicy, maxUSDPerDay float64, now time.Time) (*Anomaly, error) {
	if st == nil || st.Pipeline == nil || maxUSDPerDay <= 0 {
		return nil, nil
	}
	cost, err := st.Pipeline.SumCostSince(ctx, now.Add(-foremanBudgetWindow))
	if err != nil {
		return nil, fmt.Errorf("foreman budget_burn: sum cost: %w", err)
	}
	ratio := cost / maxUSDPerDay
	burn := fp.BurnRatio()
	if ratio < burn {
		return nil, nil
	}
	return &Anomaly{
		Rule:     ruleBudgetBurn,
		Severity: severityWarning,
		Evidence: map[string]any{
			"cost_usd_1d":     cost,
			"max_usd_per_day": maxUSDPerDay,
			"ratio":           ratio,
			"threshold_ratio": burn,
		},
	}, nil
}

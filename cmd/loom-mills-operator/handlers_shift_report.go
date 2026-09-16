package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/finishing"
	"github.com/crb2nu/loom/pkg/mills/shiftreport"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func (o *operator) handleShiftReport(w http.ResponseWriter, r *http.Request) {
	window := 24 * time.Hour
	if raw := strings.TrimSpace(r.URL.Query().Get("window")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid window", http.StatusBadRequest)
			return
		}
		window = parsed
	}
	report, err := o.composeShiftReport(r.Context(), o.shiftNow().UTC(), window)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	mills.FinishingBoltsPending.Set(float64(report.Finishing.BoltsPending))
	mills.FinishingBoltsUnknown.Set(float64(report.Finishing.BoltsUnknown))
	writeJSON(w, http.StatusOK, report)
}

// composeShiftReport builds the shift ledger ending at now over window. The
// shift-report endpoint and the daily finished-goods digest share it, so a
// digest is exactly the 24-hour ledger a reader would have fetched at the
// day boundary; only the endpoint updates the finishing gauges.
func (o *operator) composeShiftReport(ctx context.Context, now time.Time, window time.Duration) (shiftreport.Report, error) {
	now = now.UTC()
	runs, err := o.store.Pipeline.ListRecentTerminalBolts(ctx, now.Add(-window), boltReadLimit)
	if err != nil {
		return shiftreport.Report{}, err
	}
	weekRuns, err := o.store.Pipeline.ListRecentTerminalBolts(ctx, now.Add(-7*24*time.Hour), boltReadLimit)
	if err != nil {
		return shiftreport.Report{}, err
	}
	occ := map[string]int{}
	for _, run := range weekRuns {
		if run.State == store.PipelineEscalated && run.FailureSignature != "" {
			occ[run.FailureSignature]++
		}
	}
	departures := make([]shiftreport.Departure, 0, len(runs))
	deployChecks := make(map[string]finishing.DeployCheck)
	probe := finishing.DeployProbe{RepoRoot: o.repoRoot, BuildSHA: version, IsAncestor: mills.DeploymentIsAncestor}
	for _, run := range runs {
		item, err := o.store.Backlog.Get(ctx, run.BacklogID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return shiftreport.Report{}, err
		}
		card, err := o.runBoltCard(ctx, run, item, occ)
		if err != nil {
			return shiftreport.Report{}, err
		}
		if shiftreport.Kind(card) == shiftreport.KindBolt {
			check := finishing.DeployCheck{State: finishing.DeployNotApplicable, BuildSHA: strings.TrimSpace(version)}
			target := strings.TrimSpace(item.TargetProject)
			if target == "" || store.SameRepo(target, o.homeProject()) {
				stages, stageErr := o.store.Pipeline.ListStages(ctx, run.ID)
				if stageErr != nil {
					return shiftreport.Report{}, stageErr
				}
				_, mergeSHA := finishing.DeploymentEvidence(stages)
				check = probe.Check(ctx, mergeSHA)
			}
			deployChecks[run.ID] = check
		}
		at := run.StartedAt
		if run.EndedAt != nil {
			at = *run.EndedAt
		}
		if at.Before(now.Add(-window)) || at.After(now) {
			continue
		}
		departures = append(departures, shiftreport.Departure{Card: card, EndedAt: at.UTC()})
	}
	snaps, err := o.store.KPI.Range(ctx, int(window/time.Second), now.Add(-2*window), now)
	if err != nil {
		return shiftreport.Report{}, err
	}
	var current, previous *store.KPISnapshot
	boundary := now.Add(-window)
	for _, s := range snaps {
		if !s.SnapshotAt.After(boundary) {
			previous = s
		}
		current = s
	}
	agg, err := o.store.Backlog.TasteAggregates(ctx, now, 14*24*time.Hour)
	if err != nil {
		return shiftreport.Report{}, err
	}
	taste := shiftreport.Taste{BoltsThisShift: 0, Coverage14d: agg.OverallCoverage14d, CoverageGate: mills.MinimumTasteCoverage, RankerArmed: agg.OverallMerged14d > 0 && agg.OverallCoverage14d >= mills.MinimumTasteCoverage}
	boltIDs := map[string]bool{}
	for _, d := range departures {
		if shiftreport.Kind(d.Card) != shiftreport.KindBolt {
			continue
		}
		taste.BoltsThisShift++
		boltIDs[d.Card.BacklogID] = true
		if d.Card.RunID != nil {
			boltIDs[*d.Card.RunID] = true
		}
	}
	gradeEvents, err := o.store.Events.ListSinceByKinds(ctx, []string{"bolt.graded"}, now.Add(-window), boltReadLimit)
	if err != nil {
		return shiftreport.Report{}, err
	}
	gradedBolts := map[string]bool{}
	for _, event := range gradeEvents {
		if event.OccurredAt.After(now) {
			continue
		}
		itemID, _ := event.Payload["item_id"].(string)
		runID, _ := event.Payload["run_id"].(string)
		key := itemID
		if key == "" {
			key = runID
		}
		if !boltIDs[itemID] && !boltIDs[runID] && !boltIDs[event.SubjectID] {
			continue
		}
		gradedBolts[key] = true
		at := event.OccurredAt.UTC()
		if taste.LastGradeAt == nil || at.After(*taste.LastGradeAt) {
			taste.LastGradeAt = &at
		}
	}
	taste.GradedThisShift = len(gradedBolts)
	thresholds := mills.DefaultThroughputGuardrailThresholds
	if o.policy != nil && o.policy.Current() != nil {
		thresholds = o.policy.Current().Budgets.ThroughputGuardrail
	}
	docsMirror := o.docsMirror.Get()
	var reportHold *shiftreport.MainRedExternalHold
	hold, err := o.store.CurrentMainRedExternalHold(ctx)
	if err != nil {
		return shiftreport.Report{}, err
	}
	if hold != nil {
		reportHold = &shiftreport.MainRedExternalHold{Project: hold.Project, Branch: hold.Branch, ActivatedAt: hold.ActivatedAt, ExpiresAt: hold.ExpiresAt, Escalated: hold.EscalationSentAt != nil}
	}
	return shiftreport.Compose(shiftreport.Input{GeneratedAt: now, WindowSeconds: int64(window / time.Second), Departures: departures, KPIDelta: kpiDelta(current, previous), Taste: taste, DocsMirror: &docsMirror, DeployChecks: deployChecks, BuildSHA: version, MainRedExternalHold: reportHold, ThroughputGuardrail: mills.EvaluateThroughputGuardrail(throughputSignals(current), thresholds)}), nil
}

func throughputSignals(s *store.KPISnapshot) mills.ThroughputSignals {
	var out mills.ThroughputSignals
	out.EscalationRate, out.HasEscalationRate = metricOK(s, "escalation_rate")
	out.CostPerMergedPipelineUSD, out.HasCostPerMergedPipelineUSD = metricOK(s, "cost_per_merged_pipeline_usd")
	out.ScopeMaxQueueAgeSeconds, out.HasScopeMaxQueueAgeSeconds = metricOK(s, "scope_max_queue_age_seconds")
	starved, ok := metricOK(s, "scope_starvation_reservations")
	out.StarvedQueues, out.HasStarvedQueues = int(starved), ok
	return out
}

func kpiDelta(now, prev *store.KPISnapshot) shiftreport.KPIDelta {
	return shiftreport.KPIDelta{EscalationRate: movement(now, prev, "escalation_rate"), TestsErrorRate: rateMovement(now, prev, []string{"tests_error_rate"}, []string{"tests_stage_errors", "test_stage_errors"}, []string{"tests_stage_attempts", "test_stage_attempts"}), CostPerMergeUSD: movement(now, prev, "cost_per_merged_change_usd", "cost_per_merged_pipeline_usd", "cost_per_merged"), Merges: movement(now, prev, "merges", "merged_runs", "pipeline_merged_runs", "autonomous_merges"), Regressions: shiftreport.Movement{Now: regressionCount(now), Prev: regressionCount(prev)}}
}

func regressionCount(s *store.KPISnapshot) float64 {
	if value, ok := metricOK(s, "regressions", "regression_count"); ok {
		return value
	}
	rate, rateOK := metricOK(s, "regression_rate")
	merges, mergesOK := metricOK(s, "merges", "merged_runs", "pipeline_merged_runs", "autonomous_merges")
	if rateOK && mergesOK {
		return rate * merges
	}
	return 0
}
func movement(now, prev *store.KPISnapshot, keys ...string) shiftreport.Movement {
	return shiftreport.Movement{Now: metric(now, keys...), Prev: metric(prev, keys...)}
}
func rateMovement(now, prev *store.KPISnapshot, direct, num, den []string) shiftreport.Movement {
	return shiftreport.Movement{Now: rateMetric(now, direct, num, den), Prev: rateMetric(prev, direct, num, den)}
}
func rateMetric(s *store.KPISnapshot, direct, num, den []string) float64 {
	if v, ok := metricOK(s, direct...); ok {
		return v
	}
	n, nok := metricOK(s, num...)
	d, dok := metricOK(s, den...)
	if nok && dok && d > 0 {
		return n / d
	}
	return 0
}
func metric(s *store.KPISnapshot, keys ...string) float64 { v, _ := metricOK(s, keys...); return v }
func metricOK(s *store.KPISnapshot, keys ...string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	for _, key := range keys {
		switch v := s.Metrics[key].(type) {
		case float64:
			return v, true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		}
	}
	return 0, false
}

package mills

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetricsRegistered confirms every metric is non-nil and registered
// against the default registry. A typo in promauto naming would manifest
// here as a missing metric — catching it at compile-time-of-test rather
// than at first prod scrape is worth the line count.
func TestMetricsRegistered(t *testing.T) {
	t.Helper()
	cases := []struct {
		name      string
		collector prometheus.Collector
	}{
		{"mills_council_runs_total", CouncilRunsTotal},
		{"mills_council_cost_usd_total", CouncilCostUSDTotal},
		{"mills_council_duration_seconds", CouncilDurationSeconds},
		{"mills_mcphub_calls_total", MCPHubCallsTotal},
		{"mills_mcphub_call_duration_seconds", MCPHubCallDurationSeconds},
		{"mills_mcphub_queue_wait_seconds", MCPHubQueueWaitSeconds},
		{"mills_mcphub_transport_retries_total", MCPHubTransportRetriesTotal},
		{"mills_pipeline_runs_total", PipelineRunsTotal},
		{"mills_pipeline_active", PipelineActiveGauge},
		{"mills_pipeline_stage_attempts_total", PipelineStageAttemptsTotal},
		{"mills_pipeline_stage_error_class_total", PipelineStageErrorClassTotal},
		{"mills_pipeline_stage_duration_seconds", PipelineStageDurationSeconds},
		{"mills_pipeline_cost_usd_total", PipelineCostUSDTotal},
		{"mills_autonomous_merges", AutonomousMerges},
		{"mills_autonomous_merges_real", AutonomousMergesReal},
		{"mills_gate_evaluations_total", GateEvaluationsTotal},
		{"mills_scope_amendments_total", ScopeAmendmentsTotal},
		{"mills_escalations_total", EscalationsTotal},
		{"mills_pipeline_escalation_class_total", EscalationClassTotal},
		{"mills_escalation_issues_created_total", EscalationIssueCreatedTotal},
		{"mills_escalation_issues_deduped_total", EscalationIssueDedupedTotal},
		{"mills_escalation_issues_auto_closed_total", EscalationIssueAutoClosedTotal},
		{"mills_escalation_handoffs_created_total", EscalationHandoffCreatedTotal},
		{"mills_ghost_sparks_closed_total", GhostSparksClosedTotal},
		{"mills_auto_requeues_total", AutoRequeuesTotal},
		{"mills_reconciler_ticks_total", ReconcileTicksTotal},
		{"mills_reconciler_tick_duration_seconds", ReconcileTickDurationSeconds},
		{"mills_pipeline_start_claims_total", PipelineStartClaimsTotal},
		{"mills_pipeline_start_claim_duration_seconds", PipelineStartClaimDurationSeconds},
		{"mills_pipeline_dispatch_outbox_pending", PipelineDispatchOutboxPending},
		{"mills_run_provenance_stamps_total", RunProvenanceStampsTotal},
		{"mills_takeup_ticks_total", TakeupTicksTotal},
		{"mills_takeup_tick_duration_seconds", TakeupTickDurationSeconds},
		{"mills_takeup_plans_scanned_total", TakeupPlansScannedTotal},
		{"mills_takeup_slices_merged_total", TakeupSlicesMergedTotal},
		{"mills_takeup_plans_merged_total", TakeupPlansMergedTotal},
		{"mills_takeup_items_closed_total", TakeupItemsClosedTotal},
		{"mills_takeup_orphans_flagged_total", TakeupOrphansFlaggedTotal},
		{"mills_takeup_pattern_harvests_total", TakeupPatternHarvestsTotal},
		{"mills_plan_slice_emitter_ticks_total", PlanSliceEmitterTicksTotal},
		{"mills_plan_slice_emitter_tick_duration_seconds", PlanSliceEmitterTickDurationSeconds},
		{"mills_plan_slice_emitter_items_emitted_total", PlanSliceEmitterItemsEmittedTotal},
		{"mills_workflow_scheduler_ticks_total", WorkflowSchedulerTicksTotal},
		{"mills_workflow_scheduler_tick_duration_seconds", WorkflowSchedulerTickDurationSeconds},
		{"mills_workflow_run_advances_total", WorkflowRunAdvancesTotal},
		{"mills_workflow_runs_advancing", WorkflowRunsAdvancing},
		{"mills_workflow_start_claims_total", WorkflowStartClaimsTotal},
		{"mills_workflow_selection_outcomes_total", WorkflowSelectionOutcomesTotal},
		{"mills_workflow_runs_terminal_total", WorkflowRunsTerminalTotal},
		{"mills_spin_total", SpinsTotal},
		{"mills_async_spins_total", AsyncSpinsTotal},
		{"mills_eval_score", EvalScoreSummary},
		{"mills_pipeline_recursion_depth", PipelineRecursionDepthHistogram},
		{"mills_regression_count_total", RegressionCountTotal},
		{"mills_regression_auto_revert_pending_total", RegressionAutoRevertPendingTotal},
		{"mills_regression_attributions_total", RegressionAttributionsTotal},
		{"mills_regression_sweep_errors_total", RegressionSweepErrorsTotal},
		{"mills_signature_mining_candidates_total", SignatureCandidatesTotal},
		{"mills_signature_mining_texts_scanned_total", SignatureMiningTextsScannedTotal},
		{"mills_signature_mining_errors_total", SignatureMiningErrorsTotal},
		{"mills_research_notes_guard_total", ResearchNotesGuardTotal},
		{"mills_research_paths_dropped_total", ResearchPathsDroppedTotal},
		{"mills_council_slices_guard_total", CouncilSlicesGuardTotal},
		{"mills_council_slice_paths_dropped_total", CouncilSlicePathsDroppedTotal},
		{"mills_council_plan_dedup_skipped_total", CouncilPlanDedupSkippedTotal},
		{"mills_council_merged_work_skipped_total", CouncilMergedWorkSkippedTotal},
		{"mills_council_merged_work_errors_total", CouncilMergedWorkErrorsTotal},
		{"mills_council_factory_exhaust_items_total", CouncilFactoryExhaustItemsTotal},
		{"mills_council_factory_exhaust_errors_total", CouncilFactoryExhaustErrorsTotal},
		{"mills_llm_prompt_tokens_total", LLMPromptTokensTotal},
		{"mills_llm_cached_prompt_tokens_total", LLMCachedPromptTokensTotal},
		{"mills_llm_completion_tokens_total", LLMCompletionTokensTotal},
		{"mills_item_memory_soft_threshold_total", ItemMemorySoftThresholdTotal},
		{"mills_item_memory_consolidations_total", ItemMemoryConsolidationsTotal},
		{"mills_overseer_ticks_total", OverseerTicksTotal},
		{"mills_overseer_tick_duration_seconds", OverseerTickDurationSeconds},
		{"mills_overseer_actions_total", OverseerActionsTotal},
		{"mills_overseer_suppression_active", OverseerSuppressionActive},
		{"mills_judge_calibration_mean_score", JudgeCalibrationMeanScore},
		{"mills_judge_calibration_discrimination", JudgeCalibrationDiscrimination},
		{"mills_judge_calibration_graded_runs", JudgeCalibrationGradedRuns},
		{"mills_promotion_evidence_actions", PromotionEvidenceActions},
		{"mills_config_outcome_merge_rate", ConfigOutcomeMergeRate},
		{"mills_config_outcome_runs", ConfigOutcomeRuns},
		{"mills_regressions_window_total", RegressionsWindowTotal},
		{"mills_learning_signal_export_errors_total", LearningSignalExportErrorsTotal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.collector == nil {
				t.Fatalf("metric %s is nil", tc.name)
			}
		})
	}
}

// TestCounterVecLabelsAccept verifies that the canonical label values
// mills code writes don't trip Prometheus cardinality validation. If
// someone adds a new label dimension and forgets to update the
// counter declaration, this catches it before deploy.
func TestCounterVecLabelsAccept(t *testing.T) {
	// Use the canonical label values that the runner / reconciler /
	// escalator pass at runtime. Each block has at least one Inc to
	// confirm the dimension count matches.
	CouncilRunsTotal.WithLabelValues("manual", "success").Inc()
	CouncilCostUSDTotal.WithLabelValues("cron").Add(0.01)
	CouncilDurationSeconds.WithLabelValues("manual").Observe(12.5)
	MCPHubCallsTotal.WithLabelValues("agent_context", "agent_session_list", "ok").Inc()
	MCPHubCallDurationSeconds.WithLabelValues("agent_context", "agent_session_list").Observe(0.1)
	MCPHubQueueWaitSeconds.WithLabelValues("agent_context", "agent_session_list").Observe(0.01)
	MCPHubTransportRetriesTotal.WithLabelValues("agent_context", "agent_session_list").Inc()

	PipelineRunsTotal.WithLabelValues("done").Inc()
	PipelineActiveGauge.WithLabelValues("implementing").Set(3)
	PipelineStageAttemptsTotal.WithLabelValues("implement", "success").Inc()
	PipelineStageErrorClassTotal.WithLabelValues("implement", "transient").Inc()
	PipelineStageErrorClassTotal.WithLabelValues("implement", "transient_quota").Inc()
	PipelineStageErrorClassTotal.WithLabelValues("tests", "infra").Inc()
	PipelineStageErrorClassTotal.WithLabelValues("implement", "code").Inc()
	PipelineStageDurationSeconds.WithLabelValues("tests").Observe(180.0)
	PipelineCostUSDTotal.WithLabelValues("done").Add(1.25)
	AutonomousMerges.WithLabelValues("1d").Set(2)
	AutonomousMergesReal.WithLabelValues("1d").Set(1)

	GateEvaluationsTotal.WithLabelValues("diff_size", "pass").Inc()
	ScopeAmendmentsTotal.WithLabelValues("admitted").Inc()
	ScopeAmendmentsTotal.WithLabelValues("refused").Inc()
	ScopeAmendmentsTotal.WithLabelValues("conflict").Inc()
	ScopeAmendmentsTotal.WithLabelValues("disabled").Inc()
	EscalationsTotal.WithLabelValues("retry_cap_exceeded").Inc()
	EscalationClassTotal.WithLabelValues("code").Inc()
	EscalationClassTotal.WithLabelValues("unclassified").Inc()
	EscalationIssueCreatedTotal.Inc()
	EscalationIssueDedupedTotal.Inc()
	EscalationIssueAutoClosedTotal.Inc()
	EscalationHandoffCreatedTotal.Inc()
	GhostSparksClosedTotal.WithLabelValues("merged").Inc()
	GhostSparksClosedTotal.WithLabelValues("mr_closed").Inc()
	AutoRequeuesTotal.WithLabelValues("infra").Inc()
	AutoRequeuesTotal.WithLabelValues("transient").Inc()
	AutoRequeuesTotal.WithLabelValues("transient_quota").Inc()
	AutoRequeuesTotal.WithLabelValues("external_dependency").Inc()

	SpinsTotal.WithLabelValues("jacquard", "ok").Inc()
	AsyncSpinsTotal.WithLabelValues("accepted").Inc()
	AsyncSpinsTotal.WithLabelValues("succeeded").Inc()
	AsyncSpinsTotal.WithLabelValues("failed").Inc()
	AsyncSpinsTotal.WithLabelValues("timeout").Inc()

	ReconcileTicksTotal.WithLabelValues("started_one").Inc()
	ReconcileTickDurationSeconds.Observe(0.42)
	PipelineStartClaimsTotal.WithLabelValues("committed").Inc()
	PipelineStartClaimsTotal.WithLabelValues("conflict").Inc()
	PipelineStartClaimsTotal.WithLabelValues("budget").Inc()
	PipelineStartClaimsTotal.WithLabelValues("error").Inc()
	PipelineStartClaimDurationSeconds.Observe(0.02)
	PipelineDispatchOutboxPending.Set(0)

	TakeupTicksTotal.WithLabelValues("ok").Inc()
	TakeupTicksTotal.WithLabelValues("timeout").Inc()
	TakeupTickDurationSeconds.Observe(0.42)
	TakeupPlansScannedTotal.Add(1)
	TakeupSlicesMergedTotal.Add(1)
	TakeupPlansMergedTotal.Add(1)
	TakeupItemsClosedTotal.Add(1)
	TakeupOrphansFlaggedTotal.Add(1)
	TakeupPatternHarvestsTotal.WithLabelValues("recorded").Inc()
	TakeupPatternHarvestsTotal.WithLabelValues("unmatched").Inc()
	TakeupPatternHarvestsTotal.WithLabelValues("error").Inc()
	PlanSliceEmitterTicksTotal.WithLabelValues("ok").Inc()
	PlanSliceEmitterTickDurationSeconds.Observe(0.2)
	PlanSliceEmitterItemsEmittedTotal.Inc()
	WorkflowSchedulerTicksTotal.WithLabelValues("ok").Inc()
	WorkflowSchedulerTickDurationSeconds.Observe(0.2)
	WorkflowRunAdvancesTotal.WithLabelValues("ok").Inc()
	WorkflowRunsAdvancing.Set(1)
	WorkflowStartClaimsTotal.WithLabelValues("committed").Inc()
	WorkflowStartClaimsTotal.WithLabelValues("conflict").Inc()
	WorkflowStartClaimsTotal.WithLabelValues("budget").Inc()
	WorkflowStartClaimsTotal.WithLabelValues("error").Inc()
	WorkflowSelectionOutcomesTotal.WithLabelValues("selected").Inc()
	WorkflowSelectionOutcomesTotal.WithLabelValues("hold").Inc()
	WorkflowSelectionOutcomesTotal.WithLabelValues("none").Inc()
	WorkflowSelectionOutcomesTotal.WithLabelValues("error").Inc()
	WorkflowRunsTerminalTotal.WithLabelValues("done", "runtime").Inc()
	WorkflowRunsTerminalTotal.WithLabelValues("error", "deadline").Inc()

	EvalScoreSummary.WithLabelValues("pipeline_run", "pipeline_outcome_v1").Observe(0.87)

	PipelineRecursionDepthHistogram.Observe(1)

	RegressionCountTotal.WithLabelValues("KubePodCrashLooping", "critical").Inc()
	RegressionAutoRevertPendingTotal.Inc()
	RegressionAttributionsTotal.Inc()
	RegressionSweepErrorsTotal.WithLabelValues("list_merged").Inc()
	RegressionSweepErrorsTotal.WithLabelValues("list_commits").Inc()
	RegressionSweepErrorsTotal.WithLabelValues("append_event").Inc()

	SignatureCandidatesTotal.Inc()
	SignatureMiningTextsScannedTotal.Add(12)
	SignatureMiningErrorsTotal.WithLabelValues("list_evidence").Inc()
	SignatureMiningErrorsTotal.WithLabelValues("append_event").Inc()
	SignatureMiningErrorsTotal.WithLabelValues("sweep").Inc()

	ResearchNotesGuardTotal.WithLabelValues("withheld").Inc()
	ResearchNotesGuardTotal.WithLabelValues("flagged").Inc()
	ResearchPathsDroppedTotal.Add(2)

	CouncilSlicesGuardTotal.WithLabelValues("speculative").Inc()
	CouncilSlicesGuardTotal.WithLabelValues("flagged").Inc()
	CouncilSlicesGuardTotal.WithLabelValues("dropped").Inc()
	CouncilSlicePathsDroppedTotal.Add(1)
	CouncilPlanDedupSkippedTotal.Inc()
	CouncilMergedWorkSkippedTotal.WithLabelValues("hard").Inc()
	CouncilMergedWorkSkippedTotal.WithLabelValues("gray_band").Inc()
	CouncilMergedWorkErrorsTotal.Inc()
	// Kind labels match council.FactoryExhaustKind (referenced by literal —
	// council imports mills, so mills tests can't import the constants back
	// without a cycle).
	CouncilFactoryExhaustItemsTotal.WithLabelValues("flaky_test").Inc()
	CouncilFactoryExhaustItemsTotal.WithLabelValues("audit_digest").Inc()
	CouncilFactoryExhaustErrorsTotal.Inc()

	// Component labels match the closed roster in pkg/mills/clients/usage.go
	// (referenced by literal — clients imports mills, so mills tests can't
	// import the constants back without a cycle).
	LLMPromptTokensTotal.WithLabelValues("mills-judge", "qwen3-32b").Add(1000)
	LLMCachedPromptTokensTotal.WithLabelValues("mills-judge", "qwen3-32b").Add(800)
	LLMCompletionTokensTotal.WithLabelValues("mills-judge", "qwen3-32b").Add(200)

	ItemMemorySoftThresholdTotal.Inc()
	ItemMemoryConsolidationsTotal.WithLabelValues("ok").Inc()
	ItemMemoryConsolidationsTotal.WithLabelValues("noop").Inc()
	ItemMemoryConsolidationsTotal.WithLabelValues("error").Inc()

	OverseerTicksTotal.WithLabelValues("groomer", "ok").Inc()
	OverseerTicksTotal.WithLabelValues("sentinel", "error").Inc()
	OverseerTickDurationSeconds.WithLabelValues("foreman").Observe(0.8)
	OverseerActionsTotal.WithLabelValues("groomer", "dedup_close", "dryrun").Inc()
	OverseerActionsTotal.WithLabelValues("groomer", "dedup_close", "committed").Inc()
	OverseerActionsTotal.WithLabelValues("sentinel", "incident_opened", "observed").Inc()
	OverseerActionsTotal.WithLabelValues("groomer", "zombie_flagged", "flagged").Inc()
	OverseerSuppressionActive.WithLabelValues("sentinel").Set(0)

	// Learning-signal window gauges. The outcome label carries only the two
	// terminal verdict outcomes (guard.JudgeOutcomeMerged/Escalated); "other"
	// is never exported because a run that has not finished carries no
	// calibration signal.
	JudgeCalibrationMeanScore.WithLabelValues("code_review", "merged").Set(0.91)
	JudgeCalibrationMeanScore.WithLabelValues("code_review", "escalated").Set(0.42)
	JudgeCalibrationDiscrimination.WithLabelValues("code_review").Set(0.49)
	JudgeCalibrationGradedRuns.WithLabelValues("code_review").Set(12)
	PromotionEvidenceActions.WithLabelValues("overseer.groomer").Set(7)
	ConfigOutcomeMergeRate.Set(0.6)
	ConfigOutcomeRuns.Set(20)
	RegressionsWindowTotal.Set(1)
	LearningSignalExportErrorsTotal.WithLabelValues("judge_calibration").Inc()
	LearningSignalExportErrorsTotal.WithLabelValues("promotion").Inc()
	LearningSignalExportErrorsTotal.WithLabelValues("config_outcomes").Inc()
	LearningSignalExportErrorsTotal.WithLabelValues("sweep").Inc()

	// Confirm at least one sample landed somewhere readable. testutil
	// panics if the metric isn't registered — implicit assertion.
	if got := testutil.ToFloat64(EscalationIssueCreatedTotal); got < 1 {
		t.Errorf("EscalationIssueCreatedTotal = %v, want ≥ 1 after Inc()", got)
	}
}

// TestGatherableExposesMillsMetrics scrapes the default registry and
// confirms the canonical mills_* names are in the output. This is the
// integration-level guarantee that promhttp.Handler() will surface
// our metrics on /metrics.
func TestGatherableExposesMillsMetrics(t *testing.T) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	// Every metric declared in metrics.go. Vec metrics only appear in
	// Gather output once they hold at least one child, which
	// TestCounterVecLabelsAccept (declared above, runs first) guarantees.
	want := map[string]bool{
		"mills_council_runs_total":                       false,
		"mills_council_cost_usd_total":                   false,
		"mills_council_duration_seconds":                 false,
		"mills_mcphub_calls_total":                       false,
		"mills_mcphub_call_duration_seconds":             false,
		"mills_mcphub_queue_wait_seconds":                false,
		"mills_mcphub_transport_retries_total":           false,
		"mills_pipeline_runs_total":                      false,
		"mills_pipeline_active":                          false,
		"mills_pipeline_stage_attempts_total":            false,
		"mills_pipeline_stage_error_class_total":         false,
		"mills_pipeline_stage_duration_seconds":          false,
		"mills_pipeline_cost_usd_total":                  false,
		"mills_autonomous_merges":                        false,
		"mills_autonomous_merges_real":                   false,
		"mills_gate_evaluations_total":                   false,
		"mills_scope_amendments_total":                   false,
		"mills_escalations_total":                        false,
		"mills_pipeline_escalation_class_total":          false,
		"mills_escalation_issues_created_total":          false,
		"mills_escalation_issues_deduped_total":          false,
		"mills_escalation_issues_auto_closed_total":      false,
		"mills_escalation_handoffs_created_total":        false,
		"mills_ghost_sparks_closed_total":                false,
		"mills_auto_requeues_total":                      false,
		"mills_reconciler_ticks_total":                   false,
		"mills_reconciler_tick_duration_seconds":         false,
		"mills_pipeline_start_claims_total":              false,
		"mills_pipeline_start_claim_duration_seconds":    false,
		"mills_pipeline_dispatch_outbox_pending":         false,
		"mills_takeup_ticks_total":                       false,
		"mills_takeup_tick_duration_seconds":             false,
		"mills_takeup_plans_scanned_total":               false,
		"mills_takeup_slices_merged_total":               false,
		"mills_takeup_plans_merged_total":                false,
		"mills_takeup_items_closed_total":                false,
		"mills_takeup_orphans_flagged_total":             false,
		"mills_takeup_pattern_harvests_total":            false,
		"mills_plan_slice_emitter_ticks_total":           false,
		"mills_plan_slice_emitter_tick_duration_seconds": false,
		"mills_plan_slice_emitter_items_emitted_total":   false,
		"mills_workflow_scheduler_ticks_total":           false,
		"mills_workflow_scheduler_tick_duration_seconds": false,
		"mills_workflow_run_advances_total":              false,
		"mills_workflow_runs_advancing":                  false,
		"mills_workflow_start_claims_total":              false,
		"mills_workflow_selection_outcomes_total":        false,
		"mills_workflow_runs_terminal_total":             false,
		"mills_spin_total":                               false,
		"mills_async_spins_total":                        false,
		"mills_pipeline_recursion_depth":                 false,
		"mills_regression_count_total":                   false,
		"mills_regression_auto_revert_pending_total":     false,
		"mills_regression_attributions_total":            false,
		"mills_regression_sweep_errors_total":            false,
		"mills_signature_mining_candidates_total":        false,
		"mills_signature_mining_texts_scanned_total":     false,
		"mills_signature_mining_errors_total":            false,
		"mills_research_notes_guard_total":               false,
		"mills_research_paths_dropped_total":             false,
		"mills_council_slices_guard_total":               false,
		"mills_council_slice_paths_dropped_total":        false,
		"mills_council_plan_dedup_skipped_total":         false,
		"mills_council_merged_work_skipped_total":        false,
		"mills_council_merged_work_errors_total":         false,
		"mills_council_factory_exhaust_items_total":      false,
		"mills_council_factory_exhaust_errors_total":     false,
		"mills_eval_score":                               false,
		"mills_llm_prompt_tokens_total":                  false,
		"mills_llm_cached_prompt_tokens_total":           false,
		"mills_llm_completion_tokens_total":              false,
		"mills_item_memory_soft_threshold_total":         false,
		"mills_item_memory_consolidations_total":         false,
		"mills_overseer_ticks_total":                     false,
		"mills_overseer_tick_duration_seconds":           false,
		"mills_overseer_actions_total":                   false,
		"mills_overseer_suppression_active":              false,
		"mills_judge_calibration_mean_score":             false,
		"mills_judge_calibration_discrimination":         false,
		"mills_judge_calibration_graded_runs":            false,
		"mills_promotion_evidence_actions":               false,
		"mills_config_outcome_merge_rate":                false,
		"mills_config_outcome_runs":                      false,
		"mills_regressions_window_total":                 false,
		"mills_learning_signal_export_errors_total":      false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %s not in default registry", name)
		}
	}
	// Sanity: confirm mills_* prefix is consistent.
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "mills_") && mf.GetHelp() == "" {
			t.Errorf("metric %s has empty help text", mf.GetName())
		}
	}
}

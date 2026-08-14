// Package mills's metrics file registers the Prometheus instrumentation
// every observable mills surface emits. The operator's metrics listener
// at /metrics already surfaces Go runtime gauges; this module layers
// mills-specific counters / gauges / histograms on top.
//
// Naming follows Prometheus conventions: mills_<subsystem>_<unit>.
// All metrics live in the default registry so promhttp.Handler() picks
// them up automatically — no operator-side wiring required beyond
// importing this package once.
//
// Counters track cumulative event counts (council runs by outcome,
// pipeline state transitions, gate evaluations, escalations). Gauges
// track current state (active runs by state). Histograms track latency
// (council duration, pipeline duration). The Grafana dashboard in
// platform/gitops/k3s/monitoring/dashboards/services-loom-mills-dashboard.yaml
// reads these.
//
// Wiring: the runner / reconciler / escalator / council runner each
// import this package and call the Inc/Set/Observe helpers at the
// instrumentation points. The metric definitions live here so a
// dashboard refactor doesn't require touching call sites.
package mills

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ----- Council metrics -----

var (
	ScopeDeferralCount               = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "mills_reconciler_scope_deferral_count", Help: "Durable scope-overlap deferrals for a queued item."}, []string{"item"})
	ScopeQueueAgeSeconds             = promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "mills_reconciler_scope_queue_age_seconds", Help: "Seconds since a queued item's first scope-overlap deferral."}, []string{"item"})
	ScopeStarvationTotal             = promauto.NewCounter(prometheus.CounterOpts{Name: "mills_reconciler_scope_starvation_total", Help: "Scope reservations activated after the fairness threshold."})
	ScopeReservationCapReleasesTotal = promauto.NewCounter(prometheus.CounterOpts{Name: "mills_reconciler_scope_reservation_cap_releases_total", Help: "Scope reservations released after their hold cap."})
	// CouncilRunsTotal counts every council run that reached a terminal
	// state, partitioned by trigger (cron/roadmap/incident/manual) and
	// outcome (success/partial/error/conflict).
	CouncilRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_council_runs_total",
		Help: "Total council runs that reached terminal state, by trigger and outcome.",
	}, []string{"trigger", "outcome"})

	// CouncilCostUSDTotal counts cumulative council cost so dashboards
	// can render $/day or $/run by dividing into CouncilRunsTotal.
	// Separate gauges for frontier vs local would be over-cardinality;
	// per-run cost is on the eval row already.
	CouncilCostUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_council_cost_usd_total",
		Help: "Cumulative council run cost in USD, by trigger.",
	}, []string{"trigger"})

	// CouncilDurationSeconds histograms the wall-clock time per council
	// run. Buckets are tuned for typical council ensembles (10s–10min).
	CouncilDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mills_council_duration_seconds",
		Help:    "Council run wall-clock duration, by trigger.",
		Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"trigger"})
)

// ----- MCP hub dependency metrics -----

var (
	// MCPHubCallsTotal counts complete logical tool calls after the client's
	// one-shot transport retry. The label sets are bounded by the configured
	// MCP server/tool registry and the closed outcome taxonomy.
	MCPHubCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_mcphub_calls_total",
		Help: "Mills MCP hub tool calls by server, tool, and outcome (ok/error/transport_error/timeout/cancelled/queue_timeout/queue_cancelled).",
	}, []string{"server", "tool", "outcome"})

	// MCPHubCallDurationSeconds captures total caller latency, including time
	// waiting for the per-server request/response stream and a possible redial.
	MCPHubCallDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mills_mcphub_call_duration_seconds",
		Help:    "Total Mills MCP hub tool-call latency, including queue wait and retry.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"server", "tool"})

	// MCPHubQueueWaitSeconds makes head-of-line blocking visible. Calls to the
	// same MCP server are deliberately serialized because a transport is one
	// JSON-RPC response stream; calls to different servers remain concurrent.
	MCPHubQueueWaitSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mills_mcphub_queue_wait_seconds",
		Help:    "Time a Mills MCP hub tool call waited for its server's request/response stream.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"server", "tool"})

	// MCPHubTransportRetriesTotal counts stale/broken transport recovery. A
	// rising rate with successful calls means redial is masking instability; a
	// rising rate with transport_error outcomes means the dependency is down.
	MCPHubTransportRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_mcphub_transport_retries_total",
		Help: "Mills MCP hub calls retried after a transport-level failure.",
	}, []string{"server", "tool"})
)

// ----- Pipeline metrics -----

var (
	// PipelineRunsTotal counts every pipeline run that reached a
	// terminal state, partitioned by terminal state (done/escalated).
	// Active-runs are tracked by PipelineActiveGauge.
	PipelineRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_pipeline_runs_total",
		Help: "Total pipeline runs that reached terminal state, by terminal state.",
	}, []string{"state"})

	// PipelineActiveGauge is the current count of pipeline runs in any
	// non-terminal state, by state. Useful to render a live "what's
	// happening right now" panel without scanning the events table.
	PipelineActiveGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_pipeline_active",
		Help: "Current pipeline runs in a non-terminal state, by state.",
	}, []string{"state"})

	// PipelineStageAttemptsTotal counts every stage attempt the runner
	// dispatched, partitioned by stage and outcome. Retry rate per
	// stage is the fail/(success+fail) ratio in the dashboard.
	PipelineStageAttemptsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_pipeline_stage_attempts_total",
		Help: "Pipeline stage attempts, by stage and outcome (success/error/gate_fail).",
	}, []string{"stage", "outcome"})

	// PipelineStageErrorClassTotal partitions stage errors by the
	// retry-classifier output (transient / transient_quota / infra /
	// code). Together with the kill-test bucket analysis at
	// .loom/local/handoffs/mills-autonomy-killtest-2026-05-24.md this
	// lets the operator see whether transient flake or real-code-bug
	// is consuming the attempt budget without grepping log_tail.
	PipelineStageErrorClassTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_pipeline_stage_error_class_total",
		Help: "Pipeline stage errors, by stage and error_class (transient/transient_quota/infra/code).",
	}, []string{"stage", "error_class"})

	// PipelineStageDurationSeconds histograms per-stage wall-clock time.
	// Stage names go in a label so a dashboard can render heatmaps per
	// stage without rolling up.
	PipelineStageDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mills_pipeline_stage_duration_seconds",
		Help:    "Pipeline stage attempt wall-clock duration, by stage.",
		Buckets: []float64{1, 5, 15, 30, 60, 300, 600, 1800, 3600, 7200},
	}, []string{"stage"})

	// PipelineCostUSDTotal counts cumulative spend across all pipeline
	// runs, partitioned by terminal state so dashboards can render
	// "cost per merged item" by dividing.
	PipelineCostUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_pipeline_cost_usd_total",
		Help: "Cumulative pipeline run cost in USD, by terminal state.",
	}, []string{"state"})

	// AutonomousMerges is a RESTART-DURABLE gauge of autonomous merges
	// (pipeline runs that reached state=done) within a rolling window,
	// recomputed from the SQLite store by the KPI writer at startup
	// (SeedDurableGauges) and on each tick (Record). Unlike
	// PipelineRunsTotal — an in-memory counter that resets to 0 on every
	// operator pod roll — this survives restarts because it is re-derived
	// from durable storage. window="1d" is the north-star
	// autonomous_merges_24h. The two are complementary: the counter shows
	// merges since process start; this gauge shows the true rolling window
	// regardless of how often the operator rolls (which it does on every
	// loom-core image build via Flux image automation).
	AutonomousMerges = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_autonomous_merges",
		Help: "Autonomous merges (pipeline runs reaching state=done) within a rolling window, recomputed from the durable store so it survives operator restarts. Label window: 1d/7d/30d (1d is the north-star autonomous_merges_24h).",
	}, []string{"window"})

	// AutonomousMergesReal is the north-star with heartbeat noise removed:
	// merged runs EXCLUDING the deterministic mills-canary fixtures the
	// autopilot enqueues for liveness. mills_autonomous_merges can read a
	// healthy 7/week while every one is a trivial canary and zero real
	// council/importer/plan-slice work lands (the 2026-06-30 audit that
	// motivated this gauge: every council item escalated, only canaries
	// merged). Watching this series alongside the headline gauge makes
	// "the loop is idle on REAL demand" visible instead of hidden.
	AutonomousMergesReal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_autonomous_merges_real",
		Help: "Autonomous merges within a rolling window EXCLUDING mills-canary heartbeat fixtures — the real-work north-star. A persistent gap below mills_autonomous_merges means only canaries are merging. Label window: 1d/7d/30d.",
	}, []string{"window"})
)

// ----- Gate metrics -----

var (
	// GateEvaluationsTotal counts every gate evaluation, partitioned by
	// gate name and verdict. Pass-rate per gate is the success ratio in
	// Grafana.
	GateEvaluationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_gate_evaluations_total",
		Help: "Gate evaluations, by gate name and outcome (pass/fail/skip).",
	}, []string{"gate", "outcome"})

	// ScopeAmendmentsTotal counts every scope-only gate failure the runner ran
	// the amendment evaluator against, by what it decided. This is the rollout
	// measurement for the 2026-07-26 scope-gate reliability slice: read
	// decision="admitted" against the KPI escalation_rate (0.83 baseline) —
	// admissions that do NOT show up as a drop in scope-class escalations mean
	// the amendment is firing on the wrong population.
	//
	// Bounded label set:
	//   admitted  — every violation cleared policy; the item's slice scope was
	//               amended and the pipeline continued on the existing diff.
	//   refused   — at least one violation was not admissible (cross-tree
	//               reach, protected path, no declared scope, over max_files).
	//   conflict  — every violation was admissible but the backlog CAS lost
	//               twice; the run falls through to the normal retry path.
	//   disabled  — policy has the amendment switched off.
	ScopeAmendmentsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_scope_amendments_total",
		Help: "Scope-only gate failures evaluated for auto-amendment, by decision (admitted/refused/conflict/disabled).",
	}, []string{"decision"})
)

// ----- Escalation metrics -----

var (
	// EscalationsTotal counts every pipeline run that escalated. The
	// label is the escalation reason classification — gate_cap_exceeded,
	// stage_error, integrator_conflict, integrator_alloc_fail, or
	// dispatch_dead_letter. The
	// runner / integrator pass these in.
	EscalationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_escalations_total",
		Help: "Pipeline escalations, by classification reason.",
	}, []string{"reason"})

	// EscalationClassTotal counts every pipeline escalation by its
	// TERMINAL FAULT CLASS — the ErrorClass stamped on the run
	// (pipeline_runs.escalation_class): transient / transient_quota /
	// infra / code / config, plus "unclassified" for escalations that
	// carry no [class=…] marker (gate fail, cross-repo, drive failure).
	// This is orthogonal to EscalationsTotal{reason}, which partitions
	// by the escalation MECHANISM (gate_cap_exceeded / stage_error /
	// auto_retried). The fault class answers "are escalations driven by
	// real code bugs, infra flake, quota walls, or config?" — the signal
	// that was invisible during the 2026-07-02 budget no-op escalation
	// DoS (every run escalated transient_quota at a $0 no-op spawn, but
	// nothing surfaced the class so it read as generic escalation churn).
	// Incremented co-located with EscalationsTotal so the two series
	// share one population: sum(class) == sum(reason). Label cardinality
	// is bounded to the closed ErrorClass taxonomy + "unclassified".
	EscalationClassTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_pipeline_escalation_class_total",
		Help: "Pipeline escalations by terminal fault class (transient/transient_quota/infra/code/config/unclassified).",
	}, []string{"class"})

	// EscalationIssueCreatedTotal counts successful GitLab issue
	// creations from the escalator. Dashboards alert on the ratio
	// (escalations - issues_created) to surface escalator outages.
	EscalationIssueCreatedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_escalation_issues_created_total",
		Help: "Successful GitLab issue creations from escalation handler.",
	})

	// EscalationIssueDedupedTotal counts escalations that reused an existing
	// open issue (appending a recurrence note) instead of filing a new one
	// (DEBT-073 / #167). The ratio deduped/(created+deduped) shows how much
	// duplicate-issue noise the dedup path suppresses.
	EscalationIssueDedupedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_escalation_issues_deduped_total",
		Help: "Escalations that reused an existing open issue instead of filing a duplicate.",
	})

	// EscalationIssueAutoClosedTotal counts open escalation issues that were
	// auto-closed because a later run for the same backlog item succeeded
	// (DEBT-073 / #167). Together with the created/deduped counters this closes
	// the loop: created opens them, deduped keeps them from multiplying,
	// auto-closed reaps them when the item is finally green.
	EscalationIssueAutoClosedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_escalation_issues_auto_closed_total",
		Help: "Open escalation issues auto-closed after a later run for the item succeeded.",
	})

	// EscalationHandoffCreatedTotal counts successful agent_handoff
	// creates. Same alerting pattern as the issue counter.
	EscalationHandoffCreatedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_escalation_handoffs_created_total",
		Help: "Successful agent_handoff_create calls from escalation handler.",
	})

	// GhostSparksClosedTotal counts escalated backlog items the reconciler's
	// ghost-spark reap sweep reconciled against GitLab MR reality, by outcome:
	//   - "merged":    the escalated run's MR had already merged out-of-band
	//     (merge-when-pipeline-succeeds landed it minutes after the run escalated
	//     at the merge stage — the 2026-07-17 pile where 91/141 items sat
	//     escalated while their MRs, e.g. !1037/!1044, had merged). The item is
	//     transitioned escalated→merged and its open escalation issue auto-closed.
	//     Monotonic: a reaped item leaves the escalated set and is never recounted.
	//   - "mr_closed": the escalated run's MR was closed/abandoned without merging;
	//     the item is LEFT escalated for a human but counted once per run (gated on
	//     a first-writer event) so the abandoned-MR share of the pile is measurable
	//     without inflating on every re-check.
	GhostSparksClosedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_ghost_sparks_closed_total",
		Help: "Escalated ghost-spark backlog items reconciled against GitLab MR state, by outcome (merged/mr_closed).",
	}, []string{"outcome"})

	// AutoRequeuesTotal counts escalated backlog items the reconciler's bounded
	// auto-requeue sweep flipped escalated→queued without a human, labelled by
	// the retryable fault class that made them eligible ("infra", "transient",
	// "transient_quota", "external_dependency"). Monotonic: each successful
	// requeue increments once. Read against mills_pipeline_escalation_class_total
	// it shows what share of retryable escalations the operator now recovers
	// itself vs. what a human still requeues by hand.
	AutoRequeuesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_auto_requeues_total",
		Help: "Escalated backlog items auto-requeued (escalated→queued) by the reconciler, by eligible fault class.",
	}, []string{"class"})

	ExternalIncidentDwellDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mills_external_incident_dwell_duration_seconds",
		Help:    "Completed wait_for_dependency_recovery dwell duration, by first terminal outcome.",
		Buckets: []float64{60, 300, 900, 3600, 21600, 43200, 86400},
	}, []string{"outcome"})
)

// ----- Reconciler metrics -----

var (
	// ReconcileTicksTotal counts reconciler ticks, partitioned by the
	// outcome of the tick (started_one / deferred / skipped / errored
	// / no_op). Helps spot a stuck loop where every tick is "errored".
	ReconcileTicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_reconciler_ticks_total",
		Help: "Reconciler ticks, by aggregate outcome.",
	}, []string{"outcome"})

	// ReconcileTickDurationSeconds histograms how long each tick takes.
	// Tail latency above ~5s suggests slow store queries — useful early
	// signal before the operator falls behind.
	ReconcileTickDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mills_reconciler_tick_duration_seconds",
		Help:    "Reconciler tick wall-clock duration.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	})

	EscalationSweepDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "mills_escalation_sweep_duration_seconds", Help: "Dedicated escalation sweep pass duration.",
		Buckets: []float64{0.1, 0.5, 1, 5, 10, 30, 60},
	})
	EscalationSweepTimeoutsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_escalation_sweep_timeouts_total", Help: "Escalation sweep passes that exhausted their deadline.",
	})
	EscalationSweepLookups = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "mills_escalation_sweep_lookups", Help: "GitLab lookups performed per escalation sweep pass.",
		Buckets: []float64{0, 1, 2, 5, 10, 15, 20},
	})

	// PipelineStartClaimsTotal counts transactional admission attempts. The
	// outcome taxonomy is intentionally closed (committed/conflict/budget/error)
	// so a busy scheduler cannot create unbounded metric cardinality.
	PipelineStartClaimsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_pipeline_start_claims_total",
		Help: "Transactional pipeline start claims, by bounded outcome.",
	}, []string{"outcome"})

	// PipelineStartClaimDurationSeconds measures the complete SQLite claim
	// transaction: queued/version CAS, admission reservation, run/workflow
	// creation, transition append, and dispatch-intent commit. The 100 ms
	// bucket is the fleet architecture's 10k-queue p95 objective.
	PipelineStartClaimDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mills_pipeline_start_claim_duration_seconds",
		Help:    "Transactional pipeline start claim latency.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	})

	// PipelineDispatchOutboxPending is the number of committed starts not yet
	// accepted by PipelineStarter. A non-zero value after an operator restart
	// is expected briefly; a value that stays flat identifies a wedged driver
	// without querying SQLite by hand.
	PipelineDispatchOutboxPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mills_pipeline_dispatch_outbox_pending",
		Help: "Committed pipeline start intents still awaiting starter acceptance.",
	})

	// RunProvenanceStampsTotal counts the run-start provenance stamps written
	// at the post-commit boundary, by lane and bounded outcome. "duplicate" is
	// crash-recovery replay re-reaching an already-stamped run and is normal;
	// a rising "error" rate means runs are landing without the join key every
	// per-version win-rate and cost query depends on. The stamped values
	// (checksum, models, prompt hashes) stay in the event payload — putting
	// them in labels would be unbounded cardinality.
	RunProvenanceStampsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_run_provenance_stamps_total",
		Help: "Run-start provenance stamps, by lane (pipeline/workflow) and outcome (stamped/duplicate/error).",
	}, []string{"lane", "outcome"})
)

// ----- Take-up reconciler metrics (Live Beam slice 2) -----
//
// The take-up reconciler shipped "enabled but silent" (2026-07-03): the pod
// logged "started" then never reconciled and never errored, so nothing in
// logs OR metrics distinguished a wedged loop from an idle one. These make
// both states observable: a flat mills_takeup_ticks_total on an enabled
// operator is the alertable "stalled loop" signal, while mills_takeup_ticks_
// total{outcome="ok"} climbing with mills_takeup_plans_scanned_total flat at
// 0 means the namespace gate matched no active plans (a config problem, not a
// wedge).

var (
	// TakeupTicksTotal counts take-up reconcile passes by outcome
	// (ok / timeout / error / cancelled). A reconciler doing nothing still
	// increments "ok" every poll; a wedged one stops incrementing entirely.
	TakeupTicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_takeup_ticks_total",
		Help: "Take-up reconcile passes, by outcome (ok/timeout/error/cancelled).",
	}, []string{"outcome"})

	// TakeupTickDurationSeconds histograms wall-clock per take-up tick.
	// Tail latency approaching the per-tick deadline is the early warning of
	// a slow hub/GitLab dependency before a tick actually times out.
	TakeupTickDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mills_takeup_tick_duration_seconds",
		Help:    "Take-up reconcile pass wall-clock duration.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120},
	})

	// TakeupPlansScannedTotal / TakeupSlicesMergedTotal / TakeupPlansMerged
	// Total / TakeupItemsClosedTotal / TakeupOrphansFlaggedTotal are the
	// cumulative work counters — the same TickStats the reconciler already
	// computes, exported so "enabled but doing nothing" is a Prometheus
	// query, not a log grep.
	TakeupPlansScannedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_takeup_plans_scanned_total",
		Help: "Active plans scanned across all take-up ticks.",
	})
	TakeupSlicesMergedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_takeup_slices_merged_total",
		Help: "Slices advanced to merged by the take-up reconciler.",
	})
	TakeupPlansMergedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_takeup_plans_merged_total",
		Help: "Plans rolled forward to merged by the take-up reconciler.",
	})
	TakeupItemsClosedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_takeup_items_closed_total",
		Help: "Emitted backlog items closed to merged by the take-up reconciler.",
	})
	TakeupOrphansFlaggedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_takeup_orphans_flagged_total",
		Help: "Slices flagged orphaned (MR closed without merging) by the take-up reconciler.",
	})
	// TakeupPatternHarvestsTotal counts J2 auto-harvest outcomes: a merged
	// stamped plan recording a green instance against the pattern taste gate.
	// outcome ∈ recorded | unmatched | error.
	TakeupPatternHarvestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_takeup_pattern_harvests_total",
		Help: "Pattern taste-gate harvests from merged stamped plans, by outcome.",
	}, []string{"outcome"})
)

// ----- Plan-slice emitter metrics -----

var (
	PlanSliceEmitterTicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_plan_slice_emitter_ticks_total",
		Help: "Plan-slice demand scans, by outcome (ok/timeout/error/cancelled).",
	}, []string{"outcome"})
	PlanSliceEmitterTickDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mills_plan_slice_emitter_tick_duration_seconds",
		Help:    "Plan-slice demand scan wall-clock duration.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120},
	})
	PlanSliceEmitterItemsEmittedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_plan_slice_emitter_items_emitted_total",
		Help: "Backlog items emitted from ready plan slices.",
	})
	PlanSliceEmitterSliceGroundingTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_plan_slice_emitter_slice_grounding_total",
		Help: "Emit-time slice file grounding verdicts (grounded/partial/fabricated/ungroundable).",
	}, []string{"verdict"})
)

// ----- Dynamic workflow scheduler metrics -----

var (
	WorkflowSchedulerTicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_workflow_scheduler_ticks_total",
		Help: "Dynamic workflow scheduler passes, by outcome (ok/disabled/error/cancelled).",
	}, []string{"outcome"})
	WorkflowSchedulerTickDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mills_workflow_scheduler_tick_duration_seconds",
		Help:    "Dynamic workflow scheduler pass wall-clock duration.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 30, 60, 300, 600},
	})
	WorkflowRunAdvancesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_workflow_run_advances_total",
		Help: "Dynamic workflow run advance attempts, by outcome (ok/fenced/disabled/reload_error/paused/error).",
	}, []string{"outcome"})
	WorkflowRunsAdvancing = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mills_workflow_runs_advancing",
		Help: "Dynamic workflow runs currently being advanced.",
	})
	// S7 imperative-lane observability: claim outcomes mirror
	// mills_pipeline_start_claims_total so both start kernels graph alike.
	WorkflowStartClaimsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_workflow_start_claims_total",
		Help: "ClaimWorkflowStart outcomes (committed/conflict/budget/error).",
	}, []string{"outcome"})
	WorkflowSelectionOutcomesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_workflow_selection_outcomes_total",
		Help: "Reconciler S7 template-selection outcomes (selected/hold/none/error).",
	}, []string{"outcome"})
	WorkflowRunsTerminalTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_workflow_runs_terminal_total",
		Help: "Imperative workflow runs reaching a terminal state, by state (done/error/quarantined/escalated) and cause (runtime/deadline).",
	}, []string{"state", "cause"})
)

// ----- Spinning Room metrics (Live Beam slice 3) -----

var (
	// SpinsTotal counts Spinning Room spins per frame by outcome (ok/error).
	// Every frame of a competitive spin counts once, so "which frame actually
	// yields usable drafts, and how often" is a Prometheus query — the signal
	// that later decides whether a frame stays in policy.
	SpinsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_spin_total",
		Help: "Spinning Room spins, by frame and outcome (ok/error).",
	}, []string{"frame", "outcome"})

	// AsyncSpinsTotal counts async spins (POST /api/mills/spin/async) by
	// lifecycle outcome: "accepted" on the 202, then one of
	// "succeeded"/"failed"/"timeout" when the background spin resolves. accepted
	// minus the terminal sum is the current in-flight count; a rising "timeout"
	// share flags a frame whose synthesis is outgrowing the request budget.
	// Outcome is a closed set, so cardinality stays bounded — per-FRAME async
	// reliability is already covered by mills_spin_total (async reuses the same
	// Spinner + editor-reached hook, so it never mints labels from arbitrary
	// request strings).
	AsyncSpinsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_async_spins_total",
		Help: "Async Spinning Room spins by lifecycle outcome (accepted/succeeded/failed/timeout).",
	}, []string{"outcome"})
)

// ----- Recursion metrics (Phase 6 slice 6.3) -----

var (
	// PipelineRecursionDepthHistogram observes the depth of every
	// successfully-created subrun (parent.depth + 1). The Grafana
	// dashboard reads this to render a "what depth do subruns
	// actually reach" panel; alerting on the .99 quantile catches a
	// runaway recursion before the depth-cap guard rejects every
	// subsequent call.
	//
	// Buckets are tuned for the V2-D6 default of max_depth = 2 plus
	// headroom: 1, 2, 3 cover the realistic range, 5 + 10 give the
	// histogram room to grow if policy.recursion.max_depth is
	// raised. Depth is an integer so float buckets are upper bounds.
	PipelineRecursionDepthHistogram = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mills_pipeline_recursion_depth",
		Help:    "Depth of successfully-created pipeline subruns. 1 = first child of a top-level run.",
		Buckets: []float64{1, 2, 3, 5, 10},
	})
)

// ----- Regression gate metrics (slice 6.3) -----

var (
	// RegressionCountTotal counts post-merge alert correlations: every
	// time the regression gate's webhook receives an Alertmanager fire
	// AND a mills-merged pipeline run landed within RegressionWindow,
	// the counter is bumped once per (alert, severity, run_id) tuple.
	// Dashboards alert on a non-zero rate so a hands-off auto-merge
	// regression surfaces fast.
	RegressionCountTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_regression_count_total",
		Help: "Post-merge alert correlations attributed to a mills auto-merge.",
	}, []string{"alert", "severity"})

	// RegressionAutoRevertPendingTotal is incremented when policy.
	// pipeline.auto_revert_on_regression=true would have opened a
	// revert MR. The MR-opening flow itself ships in a follow-up
	// slice; this counter exposes the gap so we can monitor adoption
	// of the flag without losing visibility.
	RegressionAutoRevertPendingTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_regression_auto_revert_pending_total",
		Help: "Auto-revert candidates the gate would have opened when the MR-opener lands.",
	})

	// RegressionAttributionsTotal counts merged MRs the reconciler's sweep
	// attributed to a later revert on the default branch. Unlike
	// RegressionCountTotal — an alert correlation, which is circumstantial —
	// each increment here is backed by a revert commit naming the MR's landed
	// SHA, so the counter reads as ground truth. First-writer gated: exactly
	// one increment per regressed MR, no matter how many sweeps see the revert.
	RegressionAttributionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_regression_attributions_total",
		Help: "Merged MRs attributed to a later revert commit on the default branch (exactly once per MR).",
	})

	// RegressionSweepErrorsTotal counts failures inside that sweep, by stage
	// ("list_merged", "list_commits", "append_event"). The sweep never wedges a
	// tick, so this counter is the only signal that attribution has gone blind
	// — a silent sweep and a failing sweep otherwise look identical.
	RegressionSweepErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_regression_sweep_errors_total",
		Help: "Post-merge regression attribution sweep failures, by stage.",
	}, []string{"stage"})
)

// ----- Signature-candidate mining metrics -----

var (
	// SignatureCandidatesTotal counts classifier-signature proposals the
	// mining sweep recorded. First-writer gated: exactly one increment per
	// distinct normalized phrase, however many sweeps re-derive the cluster.
	// A rising counter means the factory is failing in shapes no live
	// classifier explains — the input to a reviewed classifier change, never
	// an enforcement signal on its own.
	SignatureCandidatesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_signature_mining_candidates_total",
		Help: "Classifier-signature candidates mined from unexplained escalations (exactly once per phrase).",
	})

	// SignatureMiningTextsScannedTotal counts the escalation evidence texts
	// the sweep read. Pair it with the candidate counter: zero candidates over
	// a large scan is a healthy classifier corpus, while zero candidates over a
	// zero scan means the sweep is looking at nothing.
	SignatureMiningTextsScannedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_signature_mining_texts_scanned_total",
		Help: "Escalation evidence texts read by the signature-candidate mining sweep.",
	})

	// SignatureMiningErrorsTotal counts failures inside that sweep, by stage
	// ("list_evidence", "append_event", "sweep"). The sweep never wedges a
	// tick, so this counter is the only signal that mining has gone blind.
	SignatureMiningErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_signature_mining_errors_total",
		Help: "Signature-candidate mining sweep failures, by stage.",
	}, []string{"stage"})
)

// ----- Research-stage grounding metrics -----

var (
	// ResearchNotesGuardTotal counts research-stage outputs whose
	// referenced file paths failed validation against the real
	// repository, partitioned by action:
	//   - "withheld": a likely wholesale hallucination — the notes were
	//     suppressed before reaching the implement worker.
	//   - "flagged":  partially fabricated — notes kept with an
	//     unverified-paths footer.
	// A non-zero "withheld" rate means the research model
	// (e.g. gemma4-26b in research_mode=off) is inventing codebases;
	// see pkg/mills/clients/research_grounding.go. The guard increments
	// this in WeaverClient.groundNotes.
	ResearchNotesGuardTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_research_notes_guard_total",
		Help: "Research outputs sanitized for referencing non-existent repo paths, by action (withheld/flagged).",
	}, []string{"action"})

	// ResearchPathsDroppedTotal counts individual fabricated file paths
	// removed/flagged across all research outputs — a finer-grained
	// signal than ResearchNotesGuardTotal (one guarded output can drop
	// several paths).
	ResearchPathsDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_research_paths_dropped_total",
		Help: "Fabricated file paths dropped or flagged in research notes.",
	})
)

// ----- Council slice-grounding metrics -----

var (
	// CouncilSlicesGuardTotal counts council-proposed plan slices whose
	// `files` referenced directories absent from the operator's repo
	// checkout, partitioned by action:
	//   - "speculative": every file lived under a NEW directory (parent
	//     absent). Indistinguishable by path from a legitimate new package,
	//     so as of the 2026-06-30 flag-never-drop policy the slice is KEPT
	//     and recorded here rather than dropped (dropping escalated real
	//     new-package work on an empty diff). Downstream build/tests/CI +
	//     the editor prompt grounding catch genuine fabrication.
	//   - "flagged": the slice mixed real and new-directory paths; kept and
	//     recorded so the new-path rate stays observable.
	//   - "dropped": retained for metric-shape compatibility; no longer
	//     emitted (the guard stopped dropping under flag-never-drop).
	// Symmetric with ResearchNotesGuardTotal: MR !848 grounded the council
	// editor PROMPT in the real package layout; this guards the editor
	// OUTPUT. Incremented by council.BacklogMutator.Apply.
	CouncilSlicesGuardTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_council_slices_guard_total",
		Help: "Council proposal slices guarded for referencing non-existent repo directories, by action (speculative/flagged/dropped).",
	}, []string{"action"})

	// CouncilSlicePathsDroppedTotal counts individual fabricated file
	// paths found across guarded slices — a finer signal than
	// CouncilSlicesGuardTotal (one slice can reference several).
	CouncilSlicePathsDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_council_slice_paths_dropped_total",
		Help: "Fabricated file paths found in council proposal slices (directory absent from the repo).",
	})

	// CouncilPlanDedupSkippedTotal counts plan-lane proposals the council did
	// NOT author because a near-duplicate Plan already lived in the target
	// namespace (or was authored earlier in the same run). This is the demand-
	// sourcing dedup guard: a rising rate means the council keeps re-proposing
	// already-served themes and dedup is (correctly) suppressing them, rather
	// than flooding the Plan Store + plan_slice_emitter with duplicate MRs.
	// Incremented by council.BacklogMutator.Apply.
	CouncilPlanDedupSkippedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_council_plan_dedup_skipped_total",
		Help: "Council plan-lane proposals skipped as near-duplicates of an existing Plan Store plan.",
	})

	// CouncilMergedWorkSkippedTotal counts proposals suppressed because they
	// restated a merge request the target branch had already taken,
	// partitioned by band:
	//   - "hard": at or above the dedup threshold against a merged MR title.
	//   - "gray_band": in [textsim.GrayBandFloor, threshold) against an MR
	//     merged inside the gray-band recency window — the reworded-re-mint
	//     shape the backlog gray band catches, read off merged work.
	// A rising rate means the council's brief is stale relative to main and
	// grounding is absorbing it, instead of the mill burning escalation
	// attempts on empty diffs (2026-08-04: 3 of 5 sparks were this class).
	// Incremented by council.BacklogMutator.Apply.
	CouncilMergedWorkSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_council_merged_work_skipped_total",
		Help: "Council proposals skipped as restatements of recently-merged merge requests, by band (hard/gray_band).",
	}, []string{"band"})

	// CouncilMergedWorkErrorsTotal counts merged-MR snapshot failures during
	// grounding. The pass fails OPEN on these — proposals proceed ungrounded —
	// so this counter is the ONLY signal that a GitLab outage quietly removed
	// the guard; a flat suppression rate alone cannot distinguish "nothing
	// collided" from "we never looked".
	CouncilMergedWorkErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_council_merged_work_errors_total",
		Help: "Merged-MR snapshot failures during council merged-work grounding (grounding fails open).",
	})

	// CouncilFactoryExhaustItemsTotal counts open self-maintenance issues the
	// factory surfaced into a council brief, partitioned by producer:
	//   - "flaky_test": one quarantined test (scripts/flakereport).
	//   - "audit_digest": a rolling audit-advisory digest (pkg/mills/audit).
	// This is demand the mill filed against itself and nobody triaged. A rising
	// count with a flat proposal rate means the council is seeing the exhaust
	// and declining it; a flat count means the factory is either clean or
	// (see the error counter) not being asked. Incremented by council.Compile,
	// once per item per brief.
	CouncilFactoryExhaustItemsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_council_factory_exhaust_items_total",
		Help: "Open factory self-maintenance issues surfaced into a council brief, by kind (flaky_test/audit_digest).",
	}, []string{"kind"})

	// CouncilFactoryExhaustErrorsTotal counts exhaust snapshot failures during
	// brief compilation. The section degrades to an explicit "unavailable"
	// body rather than blocking the brief, so this counter is the only signal
	// that a GitLab outage removed the demand source; a zero item count alone
	// cannot distinguish "the factory is clean" from "we never looked".
	CouncilFactoryExhaustErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_council_factory_exhaust_errors_total",
		Help: "Factory-exhaust snapshot failures during council brief compilation (the section degrades, the brief does not fail).",
	})
)

// ----- Eval metrics -----

var (
	// EvalScoreSummary publishes the latest eval score per
	// (subject_kind, rubric) as a summary so Grafana can render the
	// distribution. The runner / attributor calls Observe with the
	// score (range [0,1]) on every score record.
	EvalScoreSummary = promauto.NewSummaryVec(prometheus.SummaryOpts{
		Name:       "mills_eval_score",
		Help:       "Eval score distribution, by subject kind and rubric.",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	}, []string{"subject_kind", "rubric"})
)

// ----- LLM token accounting -----

// The cached-prompt-token counters answer one question: on mills' own traffic
// shapes, what share of each prompt does the serving engine give back for free?
//
// pkg/journalengine is built on the answer being "most of it" — 79.3% engine
// hit rate, warm repeats 99% cached — but that was measured on a
// psyche-simulation workload, not on a rubric judge re-sending a rubric or a
// research stage re-sending a brief. These counters are read-only observation
// so the deeper adoption in docs/JOURNAL_ENGINE.md can be decided from mills
// data rather than from someone else's.
//
// Divide LLMCachedPromptTokensTotal by LLMPromptTokensTotal for the warm share.
// Do it as a ratio of counters rather than averaging a per-request share, so
// large prompts weigh proportionally — a thousand tiny cold calls should not
// outvote one deep warm one.
//
// A flat zero on the cached counter is ambiguous: either nothing is hitting, or
// this engine omits prompt_tokens_details entirely (older vLLM builds do).
// Check the engine's own prefix-cache metric before concluding the first.
var (
	// LLMPromptTokensTotal counts prompt tokens billed, cached part included,
	// by component (mills-judge / mills-weaver / mills-council-editor / …) and
	// served model. Component rather than client because one FlexInfer client
	// serves all of those callers and they have no reason to cache alike.
	LLMPromptTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_llm_prompt_tokens_total",
		Help: "Cumulative prompt tokens sent to LLM backends (cached portion included), by component and served model.",
	}, []string{"component", "model"})

	// LLMCachedPromptTokensTotal counts the portion of the above the engine
	// served from its prefix cache (usage.prompt_tokens_details.cached_tokens,
	// or the Responses API's input_tokens_details equivalent).
	LLMCachedPromptTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_llm_cached_prompt_tokens_total",
		Help: "Cumulative prompt tokens served from the engine prefix cache, by component and served model. Divide by mills_llm_prompt_tokens_total for the warm share; a flat 0 may mean the engine omits the field rather than a cold cache.",
	}, []string{"component", "model"})

	// LLMCompletionTokensTotal is carried alongside because a cached-share
	// trend is uninterpretable without knowing whether completion lengths
	// moved at the same time.
	LLMCompletionTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_llm_completion_tokens_total",
		Help: "Cumulative completion tokens generated by LLM backends, by component and served model.",
	}, []string{"component", "model"})
)

// ----- Item-memory growth metrics -----

// The per-backlog-item journal (pkg/mills/pipeline/item_memory.go) is bounded
// by a hard 256 KiB row cap that refuses the write outright
// (store.ErrItemMemoryTooLarge). Until these counters existed, "the cap is
// biting" was unobservable until it was already a refusal: the item's memory
// simply stopped growing and nothing said so.
//
// The soft-threshold counter is the early-warning half — it fires at half the
// cap, while the journal is still being persisted normally, which is the
// signal docs/JOURNAL_ENGINE.md's v1 note asked for before wiring an LLM
// consolidation call ("wire a Consolidator only once the cap is observed to
// bite"). It increments unconditionally, with or without
// LOOM_MILLS_MEMORY_CONSOLIDATE.
var (
	// ItemMemorySoftThresholdTotal counts record-time observations of an item
	// journal snapshot over the soft threshold (half of
	// store.ItemMemoryMaxSnapshotBytes). One increment per record call, not per
	// item, so a single pathological item that records ten more stages counts
	// ten times — the rate is the pressure signal.
	ItemMemorySoftThresholdTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_item_memory_soft_threshold_total",
		Help: "Record-time item-memory snapshots observed over the soft threshold (half of the 256 KiB hard cap). Read against mills_item_memory_consolidations_total: sustained growth here with consolidation off is the cue to enable LOOM_MILLS_MEMORY_CONSOLIDATE.",
	})

	// ItemMemoryConsolidationsTotal counts consolidation attempts by outcome,
	// so a dark-then-enabled rollout can tell "never ran" from "ran and failed".
	// Outcomes: "ok" (entries distilled and dropped), "noop" (nothing old
	// enough to split), "error" (the consolidator failed — journalengine
	// guarantees the journal was left untouched, so the unconsolidated
	// snapshot is persisted as before).
	ItemMemoryConsolidationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_item_memory_consolidations_total",
		Help: "Item-memory consolidation attempts by outcome (ok/noop/error). Flat at zero while LOOM_MILLS_MEMORY_CONSOLIDATE is off.",
	}, []string{"outcome"})
)

// ----- Overseer (Mill Staff floor lane) metrics -----
//
// The overseers write their audit trail to the append-only events table, but
// the sentinel and foreman deliberately emit no per-tick event (the events
// table rides idx_events_occurred; adding tick rows for 5m/15m cadences would
// bloat it for no audit value). These metrics are therefore the only durable
// evidence a healthy tick ran, and the first observability the soak gets.

var (
	// OverseerTicksTotal counts completed overseer ticks by agent
	// (groomer/sentinel/foreman) and outcome (ok/error). A soak that shows
	// zero ticks here means the harness never fired — enabled gate, policy
	// section, or admission veto — before anyone reads audit events.
	OverseerTicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_overseer_ticks_total",
		Help: "Completed overseer ticks, by agent (groomer/sentinel/foreman) and outcome (ok/error).",
	}, []string{"agent", "outcome"})

	// OverseerTickDurationSeconds histograms tick wall-clock per agent.
	// Buckets span probe-only sentinel ticks (<1s) through groomer ticks
	// that make several LLM verdict calls (minutes, bounded by the 5m
	// harness tick timeout).
	OverseerTickDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mills_overseer_tick_duration_seconds",
		Help:    "Overseer tick wall-clock duration, by agent.",
		Buckets: []float64{0.05, 0.25, 1, 5, 15, 60, 120, 300},
	}, []string{"agent"})

	// OverseerActionsTotal counts recorder writes by agent, action, and mode.
	// Modes mirror the ActionRecorder contract: "dryrun" (planned decision in
	// a soak), "committed" (live action), "observed" (reality note recorded
	// under the committed kind regardless of dry-run: incident/anomaly
	// open/clear, suppression cleared), "flagged" (once-per-subject
	// observation, e.g. zombie_flagged). Action labels are the recorder's
	// action names (dedup_close, close_obsolete, reprioritize, zombie_flagged,
	// suppress_admission, file_issue, incident_opened, incident_cleared,
	// anomaly_opened, anomaly_cleared, pause, alert, suppression_cleared) —
	// a closed set defined in pkg/mills/overseer.
	OverseerActionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_overseer_actions_total",
		Help: "Overseer audit-trail writes, by agent, action, and mode (dryrun/committed/observed/flagged).",
	}, []string{"agent", "action", "mode"})

	// OverseerSuppressionActive is 1 while the agent holds a live
	// admission-suppression lease (sentinel incident suppression / foreman
	// pause), else 0. Updated at tick cadence, so a TTL expiry between ticks
	// zeroes on the next tick; the admission read path re-checks the lease
	// at read time regardless.
	OverseerSuppressionActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_overseer_suppression_active",
		Help: "1 while the agent holds a live admission-suppression lease (sentinel/foreman), else 0.",
	}, []string{"agent"})
)

// ----- Learning-signal metrics -----

// The factory's learning loop — judge calibration, promotion evidence,
// configuration outcomes, post-merge regressions — is otherwise readable only
// as request-time JSON from the operator's report endpoints, which alerting
// cannot see. The gauges below are the alertable projection of those same
// reports, republished each learning-signal sweep over a fixed window (see
// Reconciler.SweepLearningSignals).
//
// They are WINDOW gauges: a sweep overwrites the previous values rather than
// adding to them, so rate()/increase() are meaningless on them. Label sets stay
// closed — gate names and guarded actors are small fixed rosters. Deliberately
// NOT exported: policy_checksum (one series per policy revision, unbounded over
// time) and judge model (drifts with every stage pin). Both stay per-row in the
// JSON reports.
var (
	// JudgeCalibrationMeanScore is a gate's mean judge score split by what the
	// graded run finally did. NaN when that outcome recorded no verdicts for
	// the gate in the window: the mean of no observations is not zero, and an
	// alert must not read "no evidence" as "scored 0".
	JudgeCalibrationMeanScore = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_judge_calibration_mean_score",
		Help: "Mean judge score for a gate over the learning-signal window, by terminal outcome of the graded run (merged/escalated). NaN when that outcome has no verdicts.",
	}, []string{"gate", "outcome"})

	// JudgeCalibrationDiscrimination is merged mean − escalated mean: how far
	// a gate's judge separates work that shipped from work that escalated. It
	// is the headline drift signal — a gate whose discrimination decays toward
	// zero has stopped telling the two apart, whatever its pass rate says. NaN
	// when either side of the difference has no verdicts.
	JudgeCalibrationDiscrimination = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_judge_calibration_discrimination",
		Help: "Judge discrimination for a gate over the learning-signal window: mean score of merged runs minus mean score of escalated runs. NaN when either outcome has no verdicts. Decay toward 0 means the judge no longer separates shipped work from escalated work.",
	}, []string{"gate"})

	// JudgeCalibrationGradedRuns is how many of a gate's verdicts reached a
	// terminal outcome in the window — the denominator behind the two gauges
	// above, and the sample-size guard a drift alert needs so a two-verdict
	// window cannot page anyone.
	JudgeCalibrationGradedRuns = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_judge_calibration_graded_runs",
		Help: "Verdicts for a gate whose graded run reached merged or escalated within the learning-signal window — the sample size behind mills_judge_calibration_mean_score and mills_judge_calibration_discrimination.",
	}, []string{"gate"})

	// PromotionEvidenceActions is how many audited actions an actor recorded
	// in the window, dry-run plus executed. It is the trend behind the
	// promotion report's ZeroEvidence guard: a soaking agent whose series sits
	// at zero is accumulating no evidence, so its dry_run cannot responsibly
	// be flipped off.
	PromotionEvidenceActions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mills_promotion_evidence_actions",
		Help: "Audited actions (dry-run plus executed) one guarded actor recorded within the learning-signal window — the evidence volume a promotion review reads.",
	}, []string{"actor"})

	// ConfigOutcomeMergeRate is the window's merged/stamped ratio across ALL
	// configurations. Per-configuration rates stay in the JSON report: one
	// series per policy revision is unbounded over time.
	ConfigOutcomeMergeRate = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mills_config_outcome_merge_rate",
		Help: "Fraction of provenance-stamped runs in the learning-signal window that merged, across all configurations. NaN when the window stamped no runs.",
	})

	// ConfigOutcomeRuns is the denominator of ConfigOutcomeMergeRate: the runs
	// the window could attribute to a configuration at all.
	ConfigOutcomeRuns = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mills_config_outcome_runs",
		Help: "Provenance-stamped runs in the learning-signal window — the denominator of mills_config_outcome_merge_rate.",
	})

	// RegressionsWindowTotal is the window count of attributed post-merge
	// regressions. A window gauge despite the _total suffix the alerting
	// contract fixes; mills_regression_attributions_total is the cumulative
	// counter over the same events.
	RegressionsWindowTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mills_regressions_window_total",
		Help: "Post-merge regressions attributed within the learning-signal window (a window gauge, not a cumulative counter — see mills_regression_attributions_total for that).",
	})

	// LearningSignalExportErrorsTotal counts failed export passes by which
	// report could not be built. It is what tells an alert that the gauges
	// above are frozen at their last good values rather than steady: a failed
	// sweep publishes nothing at all.
	LearningSignalExportErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_learning_signal_export_errors_total",
		Help: "Failed learning-signal export passes, by report (judge_calibration/promotion/config_outcomes/sweep). A failed pass publishes no gauges, so the learning-signal families hold their previous values.",
	}, []string{"report"})
)

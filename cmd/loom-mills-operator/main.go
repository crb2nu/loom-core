// loom-mills-operator is the always-on cluster control plane for Loom Mills.
// It owns the canonical SQLite store, runs the council scheduler + pipeline
// reconciler, evaluates policy gates, and exposes the REST + MCP surface that
// the Mac-side `loom mills` CLI and the HUD consume. See .loom/91-… Phase 1.
//
// This binary is the home of the slow lights-on processes the operator's
// laptop cannot host: scheduled council runs, the per-backlog-item DAG
// reconciler, OAuth refresh integration, and the budget enforcer. Mac
// clients are read-mostly callers over HTTPS.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/crb2nu/loom/internal/hud/monitor"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/audit"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/intake"
	"github.com/crb2nu/loom/pkg/mills/notify"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/runner"
	"github.com/crb2nu/loom/pkg/mills/squads"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/worker"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "loom-mills-operator: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cfg := DefaultConfig()
	cfg.ApplyEnv()

	cmd := &cobra.Command{
		Use:           "loom-mills-operator",
		Short:         "Loom Mills cluster operator (council + pipeline)",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "Path to the canonical SQLite database")
	flags.StringVar(&cfg.PolicyPath, "policy-path", cfg.PolicyPath, "Path to the YAML policy file")
	flags.StringVar(&cfg.SquadsPath, "squads-path", cfg.SquadsPath, "Directory containing squad manifest YAMLs (missing dir is non-fatal)")
	flags.StringVar(&cfg.HTTPAddr, "listen", cfg.HTTPAddr, "Bind address for the REST + MCP listener (empty disables)")
	flags.StringVar(&cfg.MetricsAddr, "metrics-addr", cfg.MetricsAddr, "Bind address for /healthz, /readyz, /metrics (empty disables)")
	flags.StringVar(&cfg.RepoRoot, "repo-root", cfg.RepoRoot, "Path to the loom-core checkout the council writes into")
	flags.BoolVar(&cfg.Debug, "debug", cfg.Debug, "Enable verbose logging")
	return cmd
}

// run is the top-level lifecycle: prepare deps → start listeners → block on
// signal → graceful shutdown. Lifecycle is ordered so the readyz probe only
// flips to 200 after migrations + initial policy load complete.
func run(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger := newLogger(cfg.Debug)
	logger.Info("loom-mills-operator booting",
		"version", version,
		"db_path", cfg.DBPath,
		"policy_path", cfg.PolicyPath,
		"http_addr", cfg.HTTPAddr,
		"metrics_addr", cfg.MetricsAddr,
	)

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return fmt.Errorf("ensure db dir: %w", err)
	}

	// Read the admin token from env once. setAdminToken is atomic so a
	// future K8s Secret rotation path can swap it in without a restart.
	loadAdminTokenFromEnv()

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := ensureRepoRoot(rootCtx, cfg, logger); err != nil {
		logger.Warn("repo root bootstrap failed; autonomy readiness will report repo_root red", "repo_root", cfg.RepoRoot, "error", err)
	}

	st, err := store.Open(rootCtx, store.Options{Path: cfg.DBPath})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Warn("store close", "error", cerr)
		}
	}()
	logger.Info("store opened, migrations applied")

	pm, err := mills.NewPolicyManager(rootCtx, cfg.PolicyPath, mills.PolicyManagerOptions{
		OnError: func(e error) { logger.Warn("policy reload failed", "error", e) },
	})
	if err != nil {
		return fmt.Errorf("policy manager: %w", err)
	}
	defer func() {
		if cerr := pm.Close(); cerr != nil {
			logger.Warn("policy manager close", "error", cerr)
		}
	}()
	pm.Subscribe(func(_, n *mills.Policy) {
		logger.Info("policy reloaded",
			"version", n.Version,
			"enabled", n.IsEnabled(),
			"council_max_usd_per_day", n.Budgets.Council.MaxUSDPerDay,
			"pipeline_max_concurrent_runs", n.Budgets.Pipeline.MaxConcurrentRuns,
		)
	})

	budget := mills.NewBudget(pm, mills.NewStoreBudgetReader(st))

	// Squad loader (Phase 2 slice 2.4). Reflects squad manifests from
	// cfg.SquadsPath into the canonical store and watches the dir via
	// fsnotify. A missing dir is non-fatal: the squads endpoints return
	// empty results until manifests are mounted.
	squadsLoader := buildSquadsLoader(rootCtx, cfg, st, logger)
	if squadsLoader != nil {
		defer func() {
			if cerr := squadsLoader.Close(); cerr != nil {
				logger.Warn("squads loader close", "error", cerr)
			}
		}()
	}

	// Gate registry: deterministic gates always; LLM-judged gates only
	// when FlexInfer is configured.
	gateRegistry := gates.Default()
	flexClient := buildFlexInferClient(cfg, logger)
	capabilities := newCapabilityWiring(cfg)
	capabilities.FlexInferConfigured = strings.TrimSpace(cfg.FlexInferProxyURL) != ""
	capabilities.FlexInferReady = flexClient != nil
	if flexClient != nil {
		gates.RegisterLLMGates(gateRegistry, clients.NewRubricJudge(flexClient))
		logger.Info("LLM-judged gates enabled (FlexInfer)")
	} else {
		logger.Warn("LLM-judged gates disabled; spec_conformance + pr_self_review skipped (set FLEXINFER_PROXY_URL)")
	}

	// Council runner. In production it uses FlexInfer-backed reviewers,
	// editor, and artifact judge. Local/degraded runs keep the deterministic
	// fakes so handlers can still be exercised, but autonomy readiness reports
	// the fake fallback as a blocker.
	councilRunner, councilUsesFakeAgents := buildCouncilRunner(st, pm, budget, cfg.RepoRoot, flexClient, logger)
	capabilities.CouncilConfigured = councilRunner != nil
	capabilities.CouncilUsesFakeAgents = councilUsesFakeAgents

	// Workspace-signals council brief (W3.1, .loom/126 Next waves). When a
	// Loki endpoint is configured, feed recent error clusters into the
	// council brief so it proposes grounded work over synthetic canaries.
	// Plain HTTP read (no hub); a nil client just omits the brief section.
	if lokiClient := clients.NewLokiClient(cfg.LokiURL, logger); lokiClient != nil && councilRunner != nil {
		councilRunner.Signals = lokiClient
		logger.Info("council brief workspace-signals enabled (Loki)", "url", cfg.LokiURL)
	}

	// GitOps-scoped GitLab client for the autonomy kill-switch auto-PR
	// (plan 42 Slice 1b). Separate from the pipeline GitLab client: the
	// pipeline token is walled off from platform/gitops by policy, so the
	// kill-switch needs its own credential. Returns a nil interface (not a
	// typed nil) when unconfigured so the endpoint reports 503.
	gitopsKillSwitch := buildGitOpsGitLabClient(cfg, logger)

	op := newOperator(st, pm, budget, logger).
		withRunner(councilRunner).
		withSquadsLoader(squadsLoader).
		withKillSwitch(gitopsKillSwitch, cfg.GitOpsPolicyPath, cfg.GitOpsDefaultBranch)
	// Audit subsystem is attached below after the pipeline runner +
	// FlexInfer client are ready; handlers read the fields at request
	// time so late attachment is fine.

	httpSrv := httpServer(cfg.HTTPAddr, op.httpMux())
	metricsSrv := httpServer(cfg.MetricsAddr, op.metricsMux())

	// MCP hub client: shared by Devbox, Handoff, and Worktree wrappers.
	// Nil hub means stage workers + escalator handoff fall back to
	// stubs. The operator establishes a persistent agent-context session
	// so handoff + worktree-allocate calls have a stable source session
	// id; defer cleanup so a clean shutdown ends the session row.
	hubClient, sessionID := establishHubAndSession(rootCtx, cfg, logger)
	operatorSession := &operatorSessionRef{}
	operatorSession.Set(sessionID)
	capabilities.MCPHubConfigured = strings.TrimSpace(os.Getenv("LOOM_MCP_HUB_URL")) != ""
	capabilities.MCPHubSessionReady = hubClient != nil && sessionID != ""
	if hubClient != nil {
		defer func() { endOperatorSession(hubClient, operatorSession.SessionID(), logger) }()
	}

	// HUD spawn client: built ONCE here so both the DAG pipeline dispatcher
	// AND the S6-min imperative workflow runtime share the exact same client
	// (zero new pods/services). Nil when LOOM_HUD_URL/TOKEN are unset.
	hudSpawn := buildHUDSpawnClient(cfg, logger)

	// Worker dispatcher: real clients where configured, NoOpDispatcher
	// for stages whose backing service isn't wired yet. The operator
	// logs each gap so it's obvious which surfaces are stub vs production.
	dispatcher, realStages := buildDispatcher(cfg, flexClient, hubClient, st, logger, autoMergeFor(pm), substrateForStage(pm), hudSpawn)
	capabilities.DispatcherRealStages = realStages
	capabilities.BranchContractReady = true
	capabilities.BranchContractSource = "pkg/mills/pipeline/branch_contract.go"
	capabilities.HUDSpawnConfigured = strings.TrimSpace(cfg.HUDBaseURL) != "" && strings.TrimSpace(cfg.HUDToken) != ""
	capabilities.HUDSpawnReady = realStages["plan_slice"] && realStages["implement"] && realStages["pr_self_review"]

	pipelineRunner := pipeline.New(st, gateRegistry, dispatcher, pm)
	pipelineRunner.Logger = logger
	attributor := eval.NewOutcomeAttributor(st)

	// Squad outcome recorder (Phase 2 v2.0 reconciler integration).
	// Reads the squad attribution event the reconciler emits at routing
	// time and writes a squad_outcomes row when a run merges. Wired
	// alongside attributor.OnMerged via a small composite hook so both
	// fire on every successful merge.
	squadRecorder := squads.NewOutcomeRecorder(st)
	squadRecorder.Logger = logger
	mergedHooks := []pipelineMergedHook{attributor.OnMerged, squadRecorder.OnMerged}

	// Webhook notifier (Slice 3a of plan 43). When policy.notify.webhook_url
	// is set, a Slack/Discord-compatible JSON POST fires on every merge.
	// Disabled by default; hot-reloads via PolicyManager fsnotify.
	if notifyHook := buildNotifyHook(pm, st, logger); notifyHook != nil && notifyHook.Enabled() {
		mergedHooks = append(mergedHooks, notifyHook.OnMerged)
		logger.Info("notify webhook hook enabled")
	}

	// Handoff-inbox notifier (.loom/126 Next waves W2.1). When
	// policy.notify.handoff_inbox is set, every merge posts a "Mills merged
	// X" record to the agent_context handoff inbox over the MCP hub — the
	// in-cluster alternative to the webhook. Late-bound like tick-on-merge
	// below: the handoff client is constructed further down (with the
	// escalator), so we append a closure now and assign the hook once the
	// client exists. The closure is nil-safe (HandoffHook.OnMerged guards a
	// nil receiver), so an unset/disabled hook is a no-op.
	var handoffNotify *notify.HandoffHook
	mergedHooks = append(mergedHooks, func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
		return handoffNotify.OnMerged(ctx, run, item)
	})

	// Tick-on-merge (Slice 3b of plan 43). After a merge lands the
	// scheduler picks up the next backlog item within ~1s instead of
	// waiting up to 60s for the regularly-scheduled tick. The wire-up
	// happens here as a closure because the *mills.Scheduler is
	// created later in this function; the closure captures the local
	// var by reference and the var is assigned after NewScheduler.
	var schedulerRef *mills.Scheduler
	mergedHooks = append(mergedHooks, func(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem) error {
		schedulerRef.KickNow()
		return nil
	})

	// Audit subsystem (Phase 3). Activates only when FlexInfer is
	// configured AND the operator can reach the canonical store +
	// council runner. Without it the audit endpoints serve canonical
	// rows but the dispatcher / trigger fire-paths short-circuit.
	auditDispatcher, auditWorker, auditTriggers, auditPolicy := buildAuditSubsystem(
		flexClient, councilRunner, st, cfg.RepoRoot, logger,
	)
	if auditTriggers != nil {
		mergedHooks = append(mergedHooks, auditTriggers.OnPipelineMerged)
		logger.Info("audit triggers enabled (council + pipeline)")
	} else {
		logger.Info("audit triggers disabled (FLEXINFER_PROXY_URL or council runner missing)")
	}
	pipelineRunner.OnMerged = chainPipelineMerged(mergedHooks...)
	// Slice 3d: when maybeAutoRetry converts a transient-cap
	// escalation into a re-queue, kick the scheduler so the new run
	// starts within ~1s instead of waiting for the next tick.
	pipelineRunner.OnAutoRetry = func(_ context.Context, _ *store.PipelineRun, _ *store.BacklogItem) error {
		if schedulerRef != nil {
			schedulerRef.KickNow()
		}
		return nil
	}
	op.withAudit(auditDispatcher, auditWorker, auditTriggers, auditPolicy)

	// Escalator: GitLab for issues, MCP hub for handoff. Either may be
	// disabled independently; the escalator runs whichever it has.
	gitlabClient := buildGitLabClient(cfg, logger)
	capabilities.GitLabConfigured = strings.TrimSpace(cfg.GitLabAPIURL) != "" && strings.TrimSpace(cfg.GitLabToken) != "" && strings.TrimSpace(cfg.GitLabProject) != ""
	capabilities.GitLabReady = gitlabClient != nil
	var handoffClient pipeline.HandoffClient
	if hubClient != nil {
		handoff := clients.NewHandoffClient(hubClient, sessionID)
		handoff.SourceSessionIDFunc = operatorSession.SessionID
		handoffClient = handoff
		logger.Info("escalator handoff configured (mcp-agent-context)")
	} else {
		logger.Warn("escalator handoff disabled (no MCP hub or operator session)")
	}
	// Assign the late-bound handoff-inbox merge notifier now that the handoff
	// client exists. Gated on policy.notify.handoff_inbox + a reachable hub;
	// reuses the same handoffClient the escalator uses.
	if pol := pm.Current(); pol != nil && pol.Notify.HandoffInbox && handoffClient != nil {
		handoffNotify = notify.NewHandoffHook(handoffClient, pol.Notify.HandoffTarget, pol.Notify.MRBaseURL, logger)
		logger.Info("notify handoff-inbox hook enabled", "target", handoffNotify.Target())
	}
	if gitlabClient != nil || handoffClient != nil {
		escalator := pipeline.NewEscalator(st, gitlabClient, handoffClient)
		escalator.Logger = logger
		pipelineRunner.Escalator = escalator
		logger.Info("escalator enabled", "issue", gitlabClient != nil, "handoff", handoffClient != nil)
	} else {
		logger.Warn("escalator disabled; failures will transition to escalated state without issue/handoff publication")
	}

	// Plan backfill (plan store S7b): one-shot, default-OFF. When
	// LOOM_MILLS_PLAN_BACKFILL is set and the MCP hub is reachable, author
	// a first-class Plan for every backlog item that has no plan_id yet
	// and stamp the returned id back onto the item, so a spawned agent
	// resolves the live plan via agent_plan_get instead of a stale .loom
	// SpecDoc (S7 added the link + read-through; this populates it).
	// Best-effort: failures are logged, never fatal; the reconciler runs
	// regardless. Re-running is safe (already-linked items are skipped and
	// the authored plan id is deterministic, so a retry upserts).
	if os.Getenv("LOOM_MILLS_PLAN_BACKFILL") != "" {
		if hubClient != nil {
			backfiller := &intake.PlanBackfiller{
				Store:   st.Backlog,
				Author:  clients.NewPlanClient(hubClient, "loom-mills-operator"),
				Project: cfg.GitLabProject,
				Logger:  logger,
			}
			if n, err := backfiller.Run(rootCtx); err != nil {
				logger.Warn("plan backfill failed", "error", err)
			} else {
				logger.Info("plan backfill pass complete", "linked", n)
			}
		} else {
			logger.Warn("plan backfill requested (LOOM_MILLS_PLAN_BACKFILL) but MCP hub unavailable; skipping")
		}
	}

	// Council inline plan authoring (plan store S7b-γ): when enabled and
	// the MCP hub is reachable, the council's backlog mutator authors a
	// Plan for each newly created item so it is born linked (vs. waiting
	// for the boot backfill), matching the GitLab importer (S7b-β).
	// Default-off; best-effort, never blocks council item creation. Reached
	// in via councilRunner.Mutator because buildCouncilRunner runs before
	// the hub is established.
	if os.Getenv("LOOM_MILLS_PLAN_AUTHORING") != "" && hubClient != nil &&
		councilRunner != nil && councilRunner.Mutator != nil {
		councilRunner.Mutator.PlanAuthor = clients.NewPlanClient(hubClient, "loom-mills-operator")
		councilRunner.Mutator.Project = cfg.GitLabProject
		councilRunner.Mutator.Logger = logger
		logger.Info("council inline plan authoring enabled")
	}

	// Audit follow-up writer (Phase 3 slice 3.6). When the audit
	// subsystem and a GitLab client are both wired, low-survival
	// findings auto-open advisory issues. Without GitLab, audits still
	// land in the canonical store + HUD; the follow-up step is a no-op.
	if auditWorker != nil && gitlabClient != nil {
		followup := audit.NewFollowup(gitlabClient)
		followup.Logger = logger
		auditWorker.OnRecorded = followup.OnRecorded
		logger.Info("audit follow-up writer enabled",
			"threshold", followup.Threshold)
	} else if auditWorker != nil {
		logger.Info("audit follow-up writer disabled (no GitLab client)")
	}

	// Pipeline starter routes fan-out items through the integrator when
	// the worktree allocator + branch merger are both available.
	var integrator *pipeline.Integrator
	if hubClient != nil && cfg.RepoRoot != "" {
		alloc := clients.NewWorktreeAllocator(hubClient, "loom-mills-operator", sessionID, cfg.RepoRoot)
		alloc.SourceSessionIDFunc = operatorSession.SessionID
		merger := clients.NewGitBranchMerger(cfg.RepoRoot)
		integrator = pipeline.NewIntegrator(st, pipelineRunner, alloc, merger)
		integrator.Logger = logger
		// Inherit the pipeline runner's MaxConcurrentRuns budget for the
		// integrator's parallel fan-out cap so a single backlog item
		// can't blow through the daily run budget.
		if max := pm.Current().Budgets.Pipeline.MaxConcurrentRuns; max > 0 {
			integrator.MaxParallel = max
		}
		// Hook the same Escalator the runner uses so a fan-out parent
		// that escalates publishes a failure record + handoff.
		integrator.Escalator = pipelineRunner.Escalator
		logger.Info("integrator enabled (worktree allocator + git branch merger)")
	} else {
		logger.Warn("integrator disabled; multi-slice items will run via Runner only (no fan-out)")
	}
	starter := pipeline.NewRunnerStarter(pipelineRunner, integrator)
	starter.Logger = logger
	kpiWriter := mills.NewKPIWriter(st, pm)
	kpiWriter.Logger = logger
	capabilities.KPIWriterReady = true
	capabilities.KPIWriterSource = "pkg/mills/kpi_writer.go"
	op.setCapabilities(capabilities)
	op.markReady()
	logger.Info("operator ready",
		"policy_enabled", pm.Current().IsEnabled(),
		"autonomy_ready", op.capabilityReport(rootCtx).AutonomyReady,
	)

	// Reconciler / scheduler. The reconciler hands queued items to the
	// pipeline starter (which spawns goroutines that drive the DAG and
	// fire OnMerged → eval Loop B per merge).
	reconciler := mills.NewReconciler(st, pm, budget, starter)
	reconciler.Logger = logger
	reconciler.AutonomyGate = func(ctx context.Context) (bool, []string) {
		report := op.capabilityReport(ctx)
		return report.AutonomyReady, report.AutonomyBlockers
	}
	if squadsLoader != nil {
		// Wire the squad router into the reconciler so each tick attributes
		// the chosen squad via a "reconciler.squad_routed" event keyed on
		// the new run id. squadRecorder.OnMerged then reads it back at merge
		// time. Adapter glues squads.Decision → mills.SquadDecision without
		// pulling pkg/mills/squads into pkg/mills (no import cycle).
		router := squads.NewRouter(squadsLoader, st)
		reconciler.SquadRouter = &squadRouterAdapter{router: router}
		logger.Info("squad routing enabled", "min_confidence", router.MinConfidence)
	} else {
		logger.Info("squad routing disabled (no squads loader)")
	}
	op.withReconciler(reconciler)
	terminalSync, err := reconciler.SyncTerminalBacklogs(rootCtx)
	if err != nil {
		logger.Warn("pipeline startup backlog terminal sync failed", "error", err)
	} else if terminalSync.Inspected > 0 {
		logger.Info("pipeline startup backlog terminal sync complete",
			"inspected", terminalSync.Inspected, "updated", terminalSync.Updated,
			"skipped", terminalSync.Skipped, "errored", terminalSync.Errored)
	}
	resumed, err := reconciler.ResumeInFlightRuns(rootCtx)
	if err != nil {
		logger.Warn("pipeline startup resume failed", "error", err)
	} else if resumed.Inspected > 0 {
		logger.Info("pipeline startup resume complete",
			"inspected", resumed.Inspected, "resumed", resumed.Resumed, "errored", resumed.Errored)
	}
	// Seed the restart-durable autonomous-merge gauge from the store so the
	// north-star (mills_autonomous_merges{window="1d"}) is correct the instant
	// /metrics is first scraped after this roll — not 0 until the first
	// scheduler tick. The in-memory mills_pipeline_runs_total counter reset to
	// 0 on this restart; this gauge does not. (W1.1, .loom/126 Next waves.)
	if err := kpiWriter.SeedDurableGauges(rootCtx); err != nil {
		logger.Warn("durable KPI gauge seeding failed", "error", err)
	} else {
		logger.Info("durable KPI gauges seeded from store")
	}
	scheduler := mills.NewScheduler(reconciler)
	scheduler.Logger = logger
	scheduler.KPIRecorder = kpiWriter
	// Bind the tick-on-merge closure now that the scheduler exists.
	schedulerRef = scheduler

	// Eval Loop C — weekly cross-run consistency check (default Sunday
	// 06:00 UTC). Runs alongside the reconciler scheduler in the same
	// errgroup so a panic in either takes the whole operator down for a
	// supervised restart, not a silent stuck loop.
	crossRunChecker := &eval.CrossRunChecker{Store: st, Logger: logger}
	crossRunSched := eval.NewCrossRunScheduler(crossRunChecker)
	crossRunSched.Logger = logger
	logger.Info("eval Loop C scheduler armed",
		"weekday", crossRunSched.Weekday.String(), "hour_utc", crossRunSched.Hour)

	// Council scheduler — wakes every minute and fires runner.Run when
	// the current UTC time matches policy.council.schedule_cron. The
	// cron field has lived in policy since v1 but had no reader until
	// this slice (the deferred slice-3.7 wiring), so prior to this
	// change the operator was perfectly deployed and perfectly idle.
	// Skipped silently when councilRunner is nil (degraded / fake
	// agents mode).
	var councilRunFn mills.CouncilRunFn
	if councilRunner != nil {
		councilRunFn = func(ctx context.Context, trigger store.CouncilTrigger, reason string) error {
			_, err := councilRunner.Run(ctx, runner.RunInput{Trigger: trigger, Reason: reason})
			return err
		}
	}
	councilSched := mills.NewCouncilScheduler(councilRunFn, pm)
	councilSched.Logger = logger

	// S6-min imperative workflow runtime (plan .loom/134 §S6-min). Always
	// wired into the errgroup but DEFAULT-OFF: the scheduler self-gates on
	// policy.workflows.enabled inside every tick, so it is inert until the
	// S1c canary window flips the flag. It reuses the SAME HUD spawn client
	// the DAG pipeline uses (wrapped as a worker.WorkerRunner) — zero new
	// pods/services. When the spawn client is unconfigured the scheduler
	// idles (nil runtime), keeping g.Go balanced.
	workflowSched := buildWorkflowScheduler(st, pm, hudSpawn, cfg.GitLabProject, logger)

	// Workflow step-log monitor (plan .loom/134 §S4a). Polls the durable
	// workflow journal in-process and broadcasts a `hud.workflows`-shaped
	// snapshot via OnRefresh. The operator has no browser SSE hub, so the
	// callback emits a structured log line — the live SSE delivery lands when
	// the Mac-side HUD grows a workflows channel (S4b). Wired into the errgroup
	// like the schedulers; a nil store/DAO makes its refresh a benign no-op.
	workflowMonitor := monitor.NewMillsWorkflowMonitor(st.Workflow, logger)
	workflowMonitor.OnRefresh(func(snap monitor.MillsWorkflowSnapshot) {
		logger.Debug("workflow snapshot refreshed",
			"active_runs", snap.ActiveRunCount,
			"quarantined_runs", snap.QuarantinedCount,
			"recent_steps", len(snap.RecentSteps))
	})

	g, gctx := errgroup.WithContext(rootCtx)
	g.Go(func() error { return runListener(gctx, "http", httpSrv, logger) })
	g.Go(func() error { return runListener(gctx, "metrics", metricsSrv, logger) })
	g.Go(func() error { return scheduler.Run(gctx) })
	g.Go(func() error { return crossRunSched.Run(gctx) })
	g.Go(func() error { return councilSched.Run(gctx) })
	g.Go(func() error { return workflowSched.Run(gctx) })

	// Drive the workflow monitor's poll loop and stop it on shutdown. Start
	// kicks off an immediate refresh + a ticker; the g.Go blocks until ctx
	// cancels, then Stop unblocks the loop's select.
	workflowMonitor.Start(workflowMonitorInterval)
	g.Go(func() error {
		<-gctx.Done()
		workflowMonitor.Stop()
		return nil
	})

	// GitLab issue importer (Slice 1a of plan 43). Opt-in via
	// policy.intake.gitlab.enabled: true. No-op without a configured
	// GitLab client.
	if gitlabImporter := buildGitLabImporter(pm, gitlabClient, st, logger); gitlabImporter != nil {
		// Inline plan authoring (plan store S7b-β): when enabled and the
		// MCP hub is reachable, the importer authors a Plan for each newly
		// imported item so it is born linked (vs. waiting for the boot
		// backfill). Default-off; best-effort, off the reconciler path.
		if os.Getenv("LOOM_MILLS_PLAN_AUTHORING") != "" && hubClient != nil {
			gitlabImporter.PlanAuthor = clients.NewPlanClient(hubClient, "loom-mills-operator")
			gitlabImporter.Project = cfg.GitLabProject
			logger.Info("gitlab importer inline plan authoring enabled")
		}
		g.Go(func() error { return gitlabImporter.Run(gctx) })
	}

	// Stale-canary GC (plan 43 follow-up to Slice 3d). Sweeps escalated
	// canary backlog items older than StaleAfterHours so they stop
	// blocking new mills-canary enqueues. Opt-in via
	// policy.intake.canary_gc.enabled.
	if canaryGC := buildCanaryGC(pm, st, logger); canaryGC != nil {
		g.Go(func() error { return canaryGC.Run(gctx) })
	}
	if hubClient != nil {
		g.Go(func() error {
			runOperatorSessionMaintainer(gctx, hubClient, operatorSession, op, logger, 30*time.Second)
			return nil
		})
	}
	if auditWorker != nil {
		g.Go(func() error {
			auditWorker.Run(gctx)
			return nil
		})
		// Stop the worker on shutdown so Run() unblocks before Wait()
		// returns. defer Stop here (after errgroup setup) so a panic
		// in any sibling goroutine still triggers a clean drain.
		defer auditWorker.Stop()
	}

	err = g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("loom-mills-operator stopped")
	return nil
}

type operatorSessionRef struct {
	mu        sync.RWMutex
	sessionID string
}

func (r *operatorSessionRef) SessionID() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessionID
}

func (r *operatorSessionRef) Set(sessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionID = sessionID
}

// newLogger returns a slog.Logger writing JSON to stderr — the format Loki
// expects from cluster pods.
func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// buildCouncilRunner wires the configured council ensemble into a runner. When
// FlexInfer is configured, reviewers + editor + judge all use real local-tier
// model calls. Without FlexInfer, local/degraded deployments keep the
// deterministic fake fallback so dry-run/handler smoke tests still work, while
// autonomy readiness reports the fake participants as a blocker.
//
// Returns nil + a structured log line if the policy doesn't configure any
// reviewer ensemble. The operator continues to serve read-only endpoints
// in that case; council POSTs respond 503.
func buildCouncilRunner(
	st *store.Store,
	pm *mills.PolicyManager,
	budget *mills.Budget,
	repoRoot string,
	flexClient *clients.FlexInferClient,
	logger *slog.Logger,
) (*runner.Runner, bool) {
	policy := pm.Current()
	lenses := council.LensesFromPolicy(policy)
	if len(lenses) == 0 {
		logger.Warn("no council reviewers configured; council POST endpoints will return 503")
		return nil, false
	}

	usesFakeAgents := flexClient == nil
	reviewers := map[string]council.Reviewer{}
	var editor council.Editor
	var judge *eval.Judge
	if flexClient != nil {
		for _, l := range lenses {
			reviewers[l.Name] = &clients.FlexInferCouncilReviewer{
				Client: flexClient,
			}
		}
		editor = &clients.FlexInferCouncilEditor{
			Client:  flexClient,
			Backend: "flexinfer",
			Model:   policy.Council.Ensemble.Editor.Model,
		}
		judge = &eval.Judge{Criteria: eval.DefaultRubric(&clients.FlexInferEvalJudge{Client: flexClient})}
		logger.Info("council participants wired to FlexInfer-backed reviewers/editor/judge")
	} else {
		for _, l := range lenses {
			reviewers[l.Name] = &council.FakeReviewer{
				Notes:   "fake reviewer fallback; set FLEXINFER_PROXY_URL for production council participants",
				CostUSD: 0.05,
			}
		}
		editor = &council.FakeEditor{
			Backend: policy.Council.Ensemble.Editor.Backend,
			Model:   policy.Council.Ensemble.Editor.Model,
			CostUSD: 0.42,
			Notes:   "FakeEditor fallback; set FLEXINFER_PROXY_URL for production council participants",
		}
		judge = &eval.Judge{Criteria: eval.DefaultRubric(&eval.FakeLLMJudge{Score: 1.0})}
		logger.Warn("council participants using fake fallback; autonomy readiness will fail closed")
	}
	dispatcher := &council.Dispatcher{Reviewers: reviewers}
	writer := &council.ArtifactWriter{RepoRoot: repoRoot}
	mutator := &council.BacklogMutator{Store: st}

	return &runner.Runner{
		Store:     st,
		Policy:    pm,
		Budget:    budget,
		Reviewers: dispatcher,
		Editor:    editor,
		Writer:    writer,
		Mutator:   mutator,
		Judge:     judge,
		RepoRoot:  repoRoot,
		Logger:    logger,
	}, usesFakeAgents
}

// buildSquadsLoader instantiates the squads manifest loader pointing at
// cfg.SquadsPath. Missing dir is non-fatal: a warn log fires and the
// operator boots without a loader (squad endpoints return empty results
// until manifests are mounted). Other errors (fsnotify failure) also
// log + return nil so a busted watcher doesn't block boot.
func buildSquadsLoader(ctx context.Context, cfg Config, st *store.Store, logger *slog.Logger) *squads.Loader {
	if strings.TrimSpace(cfg.SquadsPath) == "" {
		logger.Warn("squads loader disabled (squads-path empty)")
		return nil
	}
	if _, err := os.Stat(cfg.SquadsPath); err != nil {
		logger.Warn("squads loader skipped: path not present",
			"squads_path", cfg.SquadsPath, "error", err)
		return nil
	}
	loader, err := squads.NewLoader(ctx, cfg.SquadsPath, st, squads.LoaderOptions{
		OnError: func(e error) { logger.Warn("squads reload error", "error", e) },
		Logger:  logger,
	})
	if err != nil {
		logger.Warn("squads loader init failed; squad endpoints will return empty",
			"error", err)
		return nil
	}
	logger.Info("squads loader running", "squads_path", cfg.SquadsPath,
		"loaded", len(loader.Current()))
	return loader
}

// buildFlexInferClient returns a configured FlexInfer client when
// FLEXINFER_PROXY_URL is set, or nil + a warn log otherwise. The nil
// path lets the operator boot in "policy disabled" mode without LLM
// dependencies for local dev.
func buildFlexInferClient(cfg Config, logger *slog.Logger) *clients.FlexInferClient {
	if cfg.FlexInferProxyURL == "" {
		return nil
	}
	c, err := clients.NewFlexInferClient(clients.FlexInferConfig{
		ProxyURL:    cfg.FlexInferProxyURL,
		Token:       cfg.FlexInferToken,
		JudgeModel:  cfg.FlexInferJudgeModel,
		WeaverModel: cfg.FlexInferWeaverModel,
		Timeout:     cfg.FlexInferTimeout,
	})
	if err != nil {
		logger.Error("flexinfer client init failed; LLM gates + research stage will skip", "error", err)
		return nil
	}
	return c
}

// buildGitLabClient returns a configured GitLab client when
// GITLAB_API_URL + GITLAB_TOKEN + GITLAB_PROJECT are all set, otherwise
// nil + a warn log so the operator boots without it.
func buildGitLabClient(cfg Config, logger *slog.Logger) *clients.GitLabClient {
	if cfg.GitLabAPIURL == "" || cfg.GitLabToken == "" || cfg.GitLabProject == "" {
		return nil
	}
	c, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL:  cfg.GitLabAPIURL,
		Token:   cfg.GitLabToken,
		Project: cfg.GitLabProject,
	})
	if err != nil {
		logger.Error("gitlab client init failed; mr/ci/merge/cleanup stages will stub", "error", err)
		return nil
	}
	return c
}

// buildGitOpsGitLabClient returns a GitLab client scoped to the GitOps
// repo for the autonomy kill-switch auto-PR (plan 42 Slice 1b), or a nil
// gitopsCommitter interface when GITOPS_GITLAB_TOKEN + GITOPS_GITLAB_PROJECT
// are unset (the endpoint then reports 503). The return type is the
// interface — not *clients.GitLabClient — so an unconfigured build yields a
// true nil interface rather than a typed-nil that would slip past the
// handler's `== nil` guard and panic. An explicit User-Agent is set because
// this client may transit the public GitLab edge (Cloudflare 403s the
// default Go UA), unlike the in-cluster pipeline client.
func buildGitOpsGitLabClient(cfg Config, logger *slog.Logger) gitopsCommitter {
	if cfg.GitOpsGitLabToken == "" || cfg.GitOpsGitLabProject == "" {
		return nil
	}
	apiURL := cfg.GitOpsGitLabAPIURL
	if apiURL == "" {
		apiURL = cfg.GitLabAPIURL
	}
	if apiURL == "" {
		logger.Warn("gitops kill-switch disabled: no GITOPS_GITLAB_API_URL and no GITLAB_API_URL fallback")
		return nil
	}
	c, err := clients.NewGitLabClient(clients.GitLabConfig{
		APIURL:    apiURL,
		Token:     cfg.GitOpsGitLabToken,
		Project:   cfg.GitOpsGitLabProject,
		UserAgent: "loom-mills-operator/kill-switch",
	})
	if err != nil {
		logger.Error("gitops gitlab client init failed; kill-switch endpoint will 503", "error", err)
		return nil
	}
	logger.Info("gitops kill-switch client configured", "project", cfg.GitOpsGitLabProject)
	return c
}

// buildNotifyHook returns a configured webhook hook when policy
// has notify.webhook_url set. Returns nil otherwise (the hook chain
// silently skips a nil hook). PolicyManager fsnotify makes the URL
// hot-reloadable; the hook reads cfg by value at construction time so
// a URL change requires an operator restart for now — flagged in plan
// 43 as a follow-up if it bites.
func buildNotifyHook(pm *mills.PolicyManager, st *store.Store, logger *slog.Logger) *notify.WebhookHook {
	pol := pm.Current()
	if pol == nil {
		return nil
	}
	if strings.TrimSpace(pol.Notify.WebhookURL) == "" {
		return nil
	}
	cfg := notify.Config{
		URL:       pol.Notify.WebhookURL,
		MRBaseURL: pol.Notify.MRBaseURL,
	}
	if pol.Notify.WebhookTimeoutSec > 0 {
		cfg.Timeout = time.Duration(pol.Notify.WebhookTimeoutSec) * time.Second
	}
	return notify.New(cfg, st.Pipeline, nil, logger)
}

// autoMergeFor returns the callback the GitLabWorker uses to decide
// whether to set merge_when_pipeline_succeeds on a new MR. Precedence:
// item.Policy.AutoMerge (explicit per-item opt-in, set by importer or
// council) OR policy.LabelOverrideFor(item.Labels).AutoMerge (per-label
// override from the policy YAML). Live policy is read on every call so
// the hot-reloadable YAML takes effect without an operator restart.
func autoMergeFor(pm *mills.PolicyManager) func(pipeline.JobContext) bool {
	return func(jc pipeline.JobContext) bool {
		if jc.Item == nil {
			return false
		}
		if jc.Item.Policy.AutoMerge {
			return true
		}
		pol := pm.Current()
		if pol == nil {
			return false
		}
		if ov, ok := pol.LabelOverrideFor(jc.Item.Labels); ok && ov.AutoMerge {
			return true
		}
		return false
	}
}

// substrateForStage returns a closure that consults the active policy on
// every invocation so hot-reloaded `pipeline.stage_substrate` changes
// take effect on the next stage attempt. Mirrors autoMergeFor's pattern.
// Nil-safe via Policy.SubstrateForStage which returns SubstrateDefault
// on a nil receiver, so an unwired PolicyManager.Current() never blocks
// a spawn — it just falls back to the default backend.
func substrateForStage(pm *mills.PolicyManager) func(stage string) string {
	return func(stage string) string {
		return pm.Current().SubstrateForStage(stage)
	}
}

// buildCanaryGC returns a configured stale-canary GC when policy
// has intake.canary_gc.enabled = true. Returns nil otherwise.
func buildCanaryGC(pm *mills.PolicyManager, st *store.Store, logger *slog.Logger) *intake.CanaryGC {
	pol := pm.Current()
	if pol == nil || !pol.Intake.CanaryGC.Enabled {
		return nil
	}
	cfg := intake.CanaryGCConfig{DryRun: pol.Intake.CanaryGC.DryRun}
	if h := pol.Intake.CanaryGC.StaleAfterHours; h > 0 {
		cfg.StaleAfter = time.Duration(h) * time.Hour
	}
	if m := pol.Intake.CanaryGC.IntervalMinutes; m > 0 {
		cfg.Interval = time.Duration(m) * time.Minute
	}
	logger.Info("canary GC enabled",
		"stale_after_hours", pol.Intake.CanaryGC.StaleAfterHours,
		"interval_minutes", pol.Intake.CanaryGC.IntervalMinutes,
		"dry_run", cfg.DryRun,
	)
	return intake.NewCanaryGC(st.Backlog, cfg, logger)
}

// buildGitLabImporter returns a configured importer when both a GitLab
// client exists AND policy.intake.gitlab.enabled is true. Returns nil
// (no error) when either is missing so the operator boots without it.
// Logs at info on enable + warn on the disabled-but-could-be-on case so
// the gating decision is visible in the pod logs.
func buildGitLabImporter(pm *mills.PolicyManager, gitlab *clients.GitLabClient, st *store.Store, logger *slog.Logger) *intake.GitLabImporter {
	pol := pm.Current()
	if !pol.Intake.GitLab.Enabled {
		return nil
	}
	if gitlab == nil {
		logger.Warn("gitlab intake enabled in policy but GitLab client unconfigured; importer disabled")
		return nil
	}
	cfg := intake.GitLabImporterConfig{
		EligibleLabel: pol.Intake.GitLab.EligibleLabel,
	}
	if secs := pol.Intake.GitLab.PollIntervalSeconds; secs > 0 {
		cfg.PollInterval = time.Duration(secs) * time.Second
	}
	if p := pol.Intake.GitLab.DefaultPriority; p != "" {
		cfg.DefaultPriority = store.Priority(p)
	}
	logger.Info("gitlab importer enabled",
		"eligible_label", cfg.EligibleLabel,
		"poll_interval_seconds", pol.Intake.GitLab.PollIntervalSeconds,
		"default_priority", pol.Intake.GitLab.DefaultPriority,
	)
	return intake.NewGitLabImporter(gitlab, st.Backlog, cfg, logger)
}

// buildDispatcher wires the per-stage worker dispatcher. Real clients
// are used where configured; stages whose backing service isn't bridged
// fall back to the NoOp output so the runner still drives the DAG to
// done in a smoke-test sense. Each gap is logged at startup so
// production deployments can see exactly what's still stub.
//
// Wired stages (when env-configured):
//   - WeaverWorker (research): FlexInfer proxy. When
//     MILLS_RESEARCH_VIA_WEAVER=shadow|on AND a weaver URL is
//     configured, the worker also calls the routed multi-domain
//     dispatch via WeaverHTTPDelegator and (in shadow mode) records
//     the diff to pipeline_runs.research_diff via PipelineDAO.
//   - GitLabWorker (mr/ci_watch/merge/cleanup): GitLab REST API
//   - DevboxWorker (tests): mcp-devbox via MCP hub
//   - SpawnWorker (plan_slice/implement/pr_self_review): HUD mobile API
func buildDispatcher(cfg Config, flex *clients.FlexInferClient, hub *clients.MCPHubClient, st *store.Store, logger *slog.Logger, autoMerge func(pipeline.JobContext) bool, substrateFor func(stage string) string, spawn *clients.HUDSpawnClient) (pipeline.WorkerDispatcher, map[string]bool) {
	noop := &pipeline.NoOpDispatcher{}
	gitlab := buildGitLabClient(cfg, logger)

	routes := map[string]pipeline.Worker{}
	realStages := newCapabilityWiring(cfg).DispatcherRealStages
	if flex != nil {
		wc := clients.NewWeaverClient(flex)
		// Mode is read at construction time from MILLS_RESEARCH_VIA_
		// WEAVER. When non-default, attach the delegator + recorder
		// so shadow/on can actually do something. Mode==off ignores
		// both, so wiring them unconditionally would be wasteful.
		if wc.Mode != clients.ResearchModeOff {
			attachWeaverDelegation(wc, cfg, st, logger)
		}
		routes["research"] = &pipeline.WeaverWorker{Client: wc, PromptFor: stagePromptFor("research")}
		realStages["research"] = true
		logger.Info("research stage wired to FlexInfer (WeaverClient)",
			"research_mode", string(wc.Mode))
	} else {
		logger.Warn("research stage stub: NoOpDispatcher (FLEXINFER_PROXY_URL unset)")
	}
	if gitlab != nil {
		gw := &pipeline.GitLabWorker{
			Client:       gitlab,
			AutoMergeFor: autoMerge,
			BranchPusher: clients.NewGitBranchPusher(),
		}
		routes["mr"] = gw
		routes["ci_watch"] = gw
		routes["merge"] = gw
		routes["cleanup"] = gw
		realStages["mr"] = true
		realStages["ci_watch"] = true
		realStages["merge"] = true
		realStages["cleanup"] = true
		logger.Info("mr/ci_watch/merge/cleanup stages wired to GitLab")
	} else {
		logger.Warn("mr/ci_watch/merge/cleanup stages stub: NoOpDispatcher (GITLAB_API_URL/TOKEN/PROJECT unset)")
	}

	project := cfg.GitLabProject
	if project == "" {
		project = "loom-core"
	}
	if hub != nil {
		routes["tests"] = &pipeline.DevboxWorker{
			Client:  clients.NewDevboxClient(hub),
			Project: project,
			AgentID: "loom-mills-operator",
		}
		realStages["tests"] = true
		logger.Info("tests stage wired to devbox via MCP hub")
	} else {
		logger.Warn("tests stage stub: NoOpDispatcher (LOOM_MCP_HUB_URL unset)")
	}

	if spawn != nil {
		// All three Claude/Codex-backed stages share the spawn client.
		// Per-stage Model + PromptFor closures select agent type and
		// prompt body; production deployments register richer prompt
		// builders here once spec doc loaders ship.
		//
		// LOOM_MILLS_SPAWN_AGENT overrides the agent type the dispatcher
		// asks the HUD to spawn for plan_slice/implement/pr_self_review.
		// Default "claude-code"; set to "codex" or "gemini" when the
		// preferred agent's auth is unavailable.
		spawnAgent := os.Getenv("LOOM_MILLS_SPAWN_AGENT")
		if spawnAgent == "" {
			spawnAgent = "claude-code"
		}
		routes["plan_slice"] = &pipeline.SpawnWorker{
			Client:       spawn,
			Model:        spawnAgent,
			Project:      project,
			Namespace:    "loom-mills",
			PromptFor:    stagePromptFor("plan_slice"),
			SubstrateFor: substrateFor,
		}
		routes["implement"] = &pipeline.SpawnWorker{
			Client:        spawn,
			Model:         spawnAgent,
			Project:       project,
			Namespace:     "loom-mills",
			PromptFor:     stagePromptFor("implement"),
			NeedsWorktree: true,
			SubstrateFor:  substrateFor,
		}
		routes["pr_self_review"] = &pipeline.SpawnWorker{
			Client:       spawn,
			Model:        spawnAgent,
			Project:      project,
			Namespace:    "loom-mills",
			PromptFor:    stagePromptFor("pr_self_review"),
			SubstrateFor: substrateFor,
		}
		realStages["plan_slice"] = true
		realStages["implement"] = true
		realStages["pr_self_review"] = true
		logger.Info("plan_slice/implement/pr_self_review stages wired to HUD spawn API", "agent_type", spawnAgent)
	} else {
		logger.Warn("plan_slice/implement/pr_self_review stages stub: NoOpDispatcher (LOOM_HUD_URL+LOOM_HUD_TOKEN unset)")
	}

	return &fallbackDispatcher{routes: routes, fallback: noop}, realStages
}

// stagePromptFor returns a default per-stage prompt builder. Production
// deployments override this with spec-doc-aware closures; the default
// gives each stage a terse but pointed prompt that the runner's
// JobContext fills with item title + slice scope.
func stagePromptFor(stage string) func(jc pipeline.JobContext) string {
	templates := map[string]string{
		"plan_slice":     "Plan implementation slices for backlog item %s (%q). Output a numbered list of independent slices with files touched and test strategy per slice.",
		"research":       "Research backlog item %s (%q). Summarize relevant code paths, prior decisions, test constraints, and rollout risks for the implementation worker.",
		"implement":      "Implement backlog item %s (%q). Write code + tests in the allocated worktree. Commit with conventional commit format. As your FINAL step, push the branch with `git push -u origin HEAD` so the downstream `mr` stage can open a real merge request against your commits — without the push, GitLab sees an empty branch and the pipeline hangs.",
		"pr_self_review": "Review your own diff for backlog item %s (%q) before opening a merge request. Score on the pr_self_review_v1 rubric and fix anything below 0.8.",
	}
	tmpl := templates[stage]
	if tmpl == "" {
		tmpl = "Run stage %s for item %s (%q)."
	}
	return func(jc pipeline.JobContext) string {
		title := ""
		id := ""
		if jc.Item != nil {
			id = jc.Item.ID
			title = jc.Item.Title
		}
		if stage == "" {
			return fmt.Sprintf("%s\n\n%s", fmt.Sprintf(tmpl, jc.Stage.ID, id, title), backlogPromptContext(jc.Item))
		}
		return fmt.Sprintf("%s\n\n%s", fmt.Sprintf(tmpl, id, title), backlogPromptContext(jc.Item))
	}
}

func backlogPromptContext(item *store.BacklogItem) string {
	if item == nil {
		return "Backlog context: unavailable."
	}
	var b strings.Builder
	b.WriteString("Backlog context:\n")
	if item.PlanID != "" {
		fmt.Fprintf(&b, "- Plan: %s — resolve the live plan + slices with agent_plan_get{plan_id:\"%s\"} (the store is canonical; do NOT rely on stale .loom files).\n", item.PlanID, item.PlanID)
	}
	if len(item.Labels) > 0 {
		fmt.Fprintf(&b, "- Labels: %s\n", strings.Join(item.Labels, ", "))
	}
	if item.SpecDoc != "" {
		fmt.Fprintf(&b, "- Spec: %s\n", item.SpecDoc)
	}
	if item.SpecAnchor != "" {
		fmt.Fprintf(&b, "- Spec anchor: %s\n", item.SpecAnchor)
	}
	if len(item.Success.Tests) > 0 {
		fmt.Fprintf(&b, "- Required tests: %s\n", strings.Join(item.Success.Tests, "; "))
	}
	if len(item.Success.Metrics) > 0 {
		fmt.Fprintf(&b, "- Required metrics: %s\n", strings.Join(item.Success.Metrics, "; "))
	}
	if item.Success.ManualCheck != "" {
		fmt.Fprintf(&b, "- Manual check: %s\n", item.Success.ManualCheck)
	}
	if len(item.Slices) > 0 {
		b.WriteString("- Slice scope:\n")
		for _, s := range item.Slices {
			fmt.Fprintf(&b, "  - %s", s.Name)
			if len(s.Files) > 0 {
				fmt.Fprintf(&b, " files=%s", strings.Join(s.Files, ", "))
			}
			if len(s.Tests) > 0 {
				fmt.Fprintf(&b, " tests=%s", strings.Join(s.Tests, "; "))
			}
			b.WriteByte('\n')
		}
	}
	if len(item.Policy.ProtectedPathsTouched) > 0 {
		fmt.Fprintf(&b, "- Predeclared protected paths: %s\n", strings.Join(item.Policy.ProtectedPathsTouched, ", "))
	}
	return strings.TrimSpace(b.String())
}

// attachWeaverDelegation wires the routed weaver delegator + research
// diff recorder onto wc when MILLS_RESEARCH_VIA_WEAVER is "shadow" or
// "on". Falls back gracefully — every missing piece is a warn log, not
// a startup failure, so the operator can still serve the legacy
// FlexInfer chat path.
//
// Resolution order for the weaver URL:
//  1. LOOM_WEAVER_URL (cfg.WeaverURL)
//  2. LOOM_HUD_URL    (cfg.HUDBaseURL) — same loomd hosts both today
//
// Without a URL, the WeaverClient remains in shadow/on mode but
// without a delegator; flexinfer.go falls back to legacy + records a
// "delegator not configured" diff entry in shadow mode. That's
// intentional: the env knob is the source of truth for "we want the
// shadow signal," and the operator log surfaces the missing URL so
// operators can fix it without flipping the knob back.
func attachWeaverDelegation(wc *clients.WeaverClient, cfg Config, st *store.Store, logger *slog.Logger) {
	weaverURL := strings.TrimSpace(cfg.WeaverURL)
	if weaverURL == "" {
		weaverURL = strings.TrimSpace(cfg.HUDBaseURL)
	}
	if weaverURL == "" {
		logger.Warn("weaver delegation requested but no URL configured",
			"mode", string(wc.Mode),
			"hint", "set LOOM_WEAVER_URL or LOOM_HUD_URL")
		return
	}
	delegator, err := clients.NewWeaverHTTPDelegator(clients.WeaverHTTPConfig{
		BaseURL: weaverURL,
		Token:   cfg.WeaverToken,
		AgentID: "loom-mills-operator",
	})
	if err != nil {
		logger.Warn("weaver delegator init failed; falling back to legacy chat",
			"error", err, "weaver_url", weaverURL)
		return
	}
	wc.Delegator = delegator

	// The recorder is only useful in shadow mode (the diff comparison).
	// On mode delegates fully so there's no diff to record. Wiring it
	// for both modes is harmless but the log noise is cleaner this way.
	if wc.Mode == clients.ResearchModeShadow {
		if st == nil || st.Pipeline == nil {
			logger.Warn("research diff recorder disabled: store unavailable")
		} else {
			wc.DiffRecorder = clients.NewPipelineDAOResearchDiffRecorder(st.Pipeline, logger)
			logger.Info("research diff recorder wired (shadow mode → pipeline_runs.research_diff)")
		}
	}

	logger.Info("weaver delegation wired",
		"mode", string(wc.Mode),
		"weaver_url", weaverURL,
		"recorder_enabled", wc.DiffRecorder != nil,
		"token_set", cfg.WeaverToken != "")
}

// buildHUDSpawnClient returns a configured SpawnClient when LOOM_HUD_URL
// and LOOM_HUD_TOKEN are both set. Nil otherwise + warn log so the
// operator boots without it.
func buildHUDSpawnClient(cfg Config, logger *slog.Logger) *clients.HUDSpawnClient {
	if cfg.HUDBaseURL == "" || cfg.HUDToken == "" {
		return nil
	}
	c, err := clients.NewHUDSpawnClient(clients.HUDSpawnConfig{
		BaseURL: cfg.HUDBaseURL,
		Token:   cfg.HUDToken,
	})
	if err != nil {
		logger.Error("HUD spawn client init failed; spawn-driven stages disabled", "error", err)
		return nil
	}
	return c
}

// workflowMonitorInterval is the poll cadence for the workflow step-log monitor
// (plan .loom/134 §S4a). 15s matches the HUD MillsMonitor cadence — frequent
// enough to feel live during the S1c crash window, cheap because each poll is an
// in-process DAO read of a small journal.
const workflowMonitorInterval = 15 * time.Second

// buildWorkflowScheduler constructs the S6-min imperative workflow scheduler.
// It wraps the SAME HUD spawn client the DAG pipeline uses (worker.NewSpawnRunner)
// as the runtime's WorkerRunner/WorkerResumer — no new pods or services. The
// scheduler self-gates on policy.workflows.enabled (default OFF) inside every
// tick, so it is always safe to wire even when the flag is off.
//
// When the spawn client is unconfigured (LOOM_HUD_URL/TOKEN unset) the runner is
// nil; NewWorkflowScheduler then idles (block-until-cancel) so the operator's
// g.Go stays balanced and degraded local boots don't error.
func buildWorkflowScheduler(st *store.Store, pm *mills.PolicyManager, spawn *clients.HUDSpawnClient, spawnProject string, logger *slog.Logger) *workflow.WorkflowScheduler {
	gate := workflow.PolicyGateFunc(func() bool { return pm.Current().WorkflowsEnabled() })
	if spawn == nil {
		logger.Warn("imperative workflow runtime disabled (LOOM_HUD_URL/TOKEN unset); scheduler idles")
		return workflow.NewWorkflowScheduler(nil, nil, gate, logger)
	}
	runner := worker.NewSpawnRunner(spawn)
	interp := workflow.NewWorkflowInterpreter(st.Workflow, runner, logger)
	// Give agent() spawns the same git-routing project the DAG pipeline uses
	// (SpawnWorker.Project = cfg.GitLabProject, "loom-core" fallback). Without
	// this every spawn fails the HUD spawn API's required-Project validation.
	// BaseBranch left empty defers to the spawn service default ("main").
	if spawnProject == "" {
		spawnProject = "loom-core"
	}
	interp.SetSpawnDefaults(spawnProject, "")
	logger.Info("imperative workflow runtime wired (default-OFF; flips via policy.workflows.enabled)", "spawn_project", spawnProject)
	return workflow.NewWorkflowScheduler(st.Workflow, interp, gate, logger)
}

// fallbackDispatcher routes stages with a real worker through that
// worker, and stages without one through the NoOp fallback. It's a
// thin variant of pipeline.Dispatcher with a guaranteed fallback so
// unmapped stages never error during the bring-up window.
type fallbackDispatcher struct {
	routes   map[string]pipeline.Worker
	fallback pipeline.WorkerDispatcher
}

func (d *fallbackDispatcher) Dispatch(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stage pipeline.Stage, prior map[string]pipeline.StageOutput) (pipeline.StageOutput, error) {
	if w, ok := d.routes[stage.ID]; ok {
		jc := pipeline.JobContext{
			Run:           run,
			Item:          item,
			Stage:         stage,
			Prior:         prior,
			ResumeSpawnID: pipeline.ResumeSpawnIDFromContext(ctx),
			Budget:        item.Budget,
			Env:           pipeline.BuildMillsEnv(run, item, stage),
		}
		return w.Run(ctx, jc)
	}
	return d.fallback.Dispatch(ctx, run, item, stage, prior)
}

type agentContextCaller interface {
	CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (string, error)
}

// establishHubAndSession constructs the MCP hub client (when
// LOOM_MCP_HUB_URL is set) and tries to register a long-lived
// agent-context session for the operator. The session id is the
// SourceSessionID passed to HandoffClient + WorktreeAllocator so handoff
// packages and worktree-allocate calls have a consistent source.
//
// Returns (nil, "") and a warn log when the hub is unconfigured or the
// session-start call fails — the operator still boots, just without
// hub-backed clients.
func establishHubAndSession(ctx context.Context, cfg Config, logger *slog.Logger) (*clients.MCPHubClient, string) {
	hubCfg, ok := clients.MCPHubConfigFromEnv(os.Getenv)
	if !ok {
		logger.Warn("MCP hub disabled (set LOOM_MCP_HUB_URL); devbox/handoff/worktree clients fall back to stubs")
		return nil, ""
	}
	hub, err := clients.NewMCPHubClient(hubCfg)
	if err != nil {
		logger.Error("MCP hub init failed; devbox/handoff/worktree clients disabled", "error", err)
		return nil, ""
	}
	logger.Info("MCP hub configured", "url", hubCfg.HubURL, "profile", hubCfg.Profile)

	sessionID, err := startOperatorSession(ctx, hub)
	if err != nil {
		logger.Error("agent_session_start failed; handoff + worktree clients will retry", "error", err)
		return hub, ""
	}
	logger.Info("operator session established", "session_id", sessionID)
	return hub, sessionID
}

func startOperatorSession(ctx context.Context, caller agentContextCaller) (string, error) {
	if caller == nil {
		return "", errors.New("agent_context caller not configured")
	}
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := caller.CallTool(startCtx, clients.AgentContextServerName, "agent_session_start", map[string]any{
		"namespace":   "loom-mills",
		"agent_id":    "loom-mills-operator",
		"agent_type":  "operator",
		"description": "loom-mills-operator persistent session (boot " + time.Now().UTC().Format(time.RFC3339) + ")",
	})
	if err != nil {
		return "", err
	}
	sessionID := extractSessionID(body)
	if sessionID == "" {
		return "", fmt.Errorf("agent_session_start returned empty session_id; body_tail=%s", truncateForLog(body, 200))
	}
	return sessionID, nil
}

func runOperatorSessionMaintainer(ctx context.Context, caller agentContextCaller, ref *operatorSessionRef, op *operator, logger *slog.Logger, retryEvery time.Duration) {
	if caller == nil || ref == nil {
		return
	}
	if retryEvery <= 0 {
		retryEvery = 30 * time.Second
	}
	try := func() {
		if ref.SessionID() != "" {
			return
		}
		sessionID, err := startOperatorSession(ctx, caller)
		if err != nil {
			logger.Warn("agent_session_start retry failed", "error", err)
			return
		}
		ref.Set(sessionID)
		if op != nil {
			op.setMCPHubSessionReady(true)
		}
		logger.Info("operator session established after retry", "session_id", sessionID)
	}

	try()
	ticker := time.NewTicker(retryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			try()
		}
	}
}

// endOperatorSession is the deferred cleanup that ends the operator's
// agent-context session on shutdown. Best-effort: errors are logged.
func endOperatorSession(hub *clients.MCPHubClient, sessionID string, logger *slog.Logger) {
	if hub == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := hub.CallTool(ctx, clients.AgentContextServerName, "agent_session_end", map[string]any{
		"session_id": sessionID,
		"summarize":  false,
	}); err != nil {
		logger.Warn("agent_session_end on shutdown failed", "error", err)
		return
	}
	logger.Info("operator session ended", "session_id", sessionID)
}

// extractSessionID pulls session_id out of the agent_session_start
// response body. The mcp-agent-context tool emits its result via the
// active LOOM_MCP_OUTPUT_FORMAT — usually TOON (yaml-like) in
// production, JSON when override env is set. Try JSON first; fall back
// to a line-by-line scan for "session_id: <value>" which works for
// both TOON and JSON-pretty formats.
func extractSessionID(body string) string {
	if body == "" {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if v, ok := parsed["session_id"].(string); ok {
			return v
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "session_id:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
		val = strings.Trim(val, "\"' ")
		if val != "" && val != "null" {
			return val
		}
	}
	return ""
}

// truncateForLog clips a string to n characters for safe slog output.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// squadRouterAdapter glues the squads-package Router (which returns the
// rich squads.Decision) onto the slimmer mills.SquadRouter contract. The
// indirection keeps pkg/mills free of an import on pkg/mills/squads — the
// operator owns the type translation here.
type squadRouterAdapter struct {
	router *squads.Router
}

func (a *squadRouterAdapter) Pick(ctx context.Context, item *store.BacklogItem) (mills.SquadDecision, error) {
	if a == nil || a.router == nil {
		return mills.SquadDecision{SquadName: squads.FallbackName}, nil
	}
	d, err := a.router.Pick(ctx, item)
	if err != nil {
		return mills.SquadDecision{}, err
	}
	return mills.SquadDecision{
		SquadName:  d.SquadName,
		PathClass:  d.PathClass,
		Confidence: d.Confidence,
		SampleSize: d.SampleSize,
		Reason:     d.Reason,
	}, nil
}

// pipelineMergedHook is the on-merge callback shape pipeline.Runner
// fires after a successful merge. Aliased so the operator can build a
// chain of N hooks from a slice without a wall of identical signatures.
type pipelineMergedHook = func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error

// chainPipelineMerged composes N hooks. Each hook fires even if a
// previous one errored; the returned error is the FIRST non-nil error
// so the runner's logging surfaces the upstream cause without losing
// downstream signals.
func chainPipelineMerged(hooks ...pipelineMergedHook) pipelineMergedHook {
	return func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
		var firstErr error
		for _, h := range hooks {
			if h == nil {
				continue
			}
			if err := h(ctx, run, item); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
}

// buildAuditSubsystem stands up the Phase 3 audit dispatcher, queue
// worker, and trigger plumbing when FlexInfer is configured. Returns
// (nil, nil, nil, nil) when any required dependency is missing — the
// operator boots with the audit endpoints in degraded mode.
//
// PoolPolicy defaults are conservative: bulk = Llama 4 70B + Qwen 3 32B
// (both on FlexInfer), escalation = Claude Opus + Codex GPT-5 backed by
// the same flexinfer reviewer (proxy routes by model id). v2.1 will
// load the pool from policy.yaml so operators can rotate without a
// restart.
func buildAuditSubsystem(
	flex *clients.FlexInferClient,
	councilRunner *runner.Runner,
	st *store.Store,
	repoRoot string,
	logger *slog.Logger,
) (*audit.Dispatcher, *audit.QueueWorker, *audit.Triggers, *audit.PoolPolicy) {
	if flex == nil {
		return nil, nil, nil, nil
	}
	reviewer := clients.NewFlexInferAuditReviewer(flex)
	if reviewer == nil {
		return nil, nil, nil, nil
	}
	rubric, err := audit.LoadRubric()
	if err != nil {
		logger.Warn("audit: rubric load failed; subsystem disabled", "error", err)
		return nil, nil, nil, nil
	}
	dispatcher := audit.New(map[string]audit.Reviewer{reviewer.Backend(): reviewer}, rubric)
	dispatcher.Logger = logger

	// Pool defaults align with FlexInfer models actually deployed on the
	// canonical cluster. History (most recent first):
	//   - 2026-05-23: qwen3-8b / qwen3-14b-abliterated dropped from
	//     flexinfer-system; every audit run 404'd with HTML, surfacing
	//     as "Unrecognized token '<'" on the Council tab. Migrated to
	//     gemma4-26b-a4b-gptq (two GPU variants for parallelism);
	//     gitops configmap-policy.yaml is updated in lockstep (gitops
	//     MR !170 above).
	//   - 2026-05-09 (MW-004): llama-4-70b-instruct / qwen-3-32b
	//     dropped → qwen3-8b / qwen3-14b-abliterated.
	// Escalation entries retain the `flexinfer` backend tag because
	// audit.PoolMember has no driver field today; the policy.AuditPool
	// YAML mirror keeps the per-driver split for the eventual
	// spawn-backend wiring (v2.1).
	policy := &audit.PoolPolicy{
		Bulk: []audit.PoolMember{
			{Backend: "flexinfer", Model: "gemma4-26b-a4b-gptq"},
			{Backend: "flexinfer", Model: "gemma4-26b-a4b-gptq-5930k"},
		},
		Escalation: []audit.PoolMember{
			{Backend: "flexinfer", Model: "claude-opus-4-7"},
			{Backend: "flexinfer", Model: "codex-gpt5"},
		},
	}
	worker := audit.NewQueueWorker(dispatcher, st.Audit, *policy, audit.QueueOptions{
		Capacity: 64,
		Logger:   logger,
	})
	triggers := &audit.Triggers{
		Worker:              worker,
		LoadCouncilArtifact: audit.LoadCouncilArtifactFromFS(repoRoot),
		LoadMergedDiff:      stubMergedDiffLoader(),
		Logger:              logger,
	}
	if councilRunner != nil {
		// Wire the council runner's post-commit hook so successful
		// council artifacts auto-enqueue an audit job. Pipeline merges
		// are wired separately via the pipelineRunner.OnMerged chain.
		councilRunner.OnArtifactsCommitted = triggers.OnArtifactsCommitted
		logger.Info("audit: council post-commit trigger wired")
	}
	logger.Info("audit subsystem enabled",
		"bulk", len(policy.Bulk),
		"escalation", len(policy.Escalation),
	)
	return dispatcher, worker, triggers, policy
}

// stubMergedDiffLoader is a v2.0 placeholder: returns a brief metadata
// summary derived from the run + item state. v2.1 will fetch the real
// unified diff via mcp-gitlab so the rubric scores actual code rather
// than commit metadata. The audit row still produces today; the rubric
// just has less to work with.
func stubMergedDiffLoader() func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) (string, error) {
	return func(_ context.Context, run *store.PipelineRun, item *store.BacklogItem) (string, error) {
		if run == nil {
			return "", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# Pipeline merge %s\n", run.ID)
		if item != nil {
			fmt.Fprintf(&b, "Backlog: %s — %s\n", item.ID, item.Title)
			if len(item.Slices) > 0 {
				b.WriteString("\n## Slices\n")
				for _, sl := range item.Slices {
					fmt.Fprintf(&b, "- %s — files: %v\n", sl.Name, sl.Files)
				}
			}
		}
		if run.MRIID != nil {
			fmt.Fprintf(&b, "\nMR iid: %d\n", *run.MRIID)
		}
		fmt.Fprintf(&b, "\nCost: $%.2f, attempts: %d\n", run.CostUSD, run.Attempts)
		return b.String(), nil
	}
}

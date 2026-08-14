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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/crb2nu/loom/internal/hud/monitor"
	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/audit"
	"github.com/crb2nu/loom/pkg/mills/bootstrap"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/intake"
	"github.com/crb2nu/loom/pkg/mills/mergequeue"
	"github.com/crb2nu/loom/pkg/mills/notify"
	"github.com/crb2nu/loom/pkg/mills/overseer"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/runner"
	"github.com/crb2nu/loom/pkg/mills/spin"
	"github.com/crb2nu/loom/pkg/mills/squads"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/takeup"
	"github.com/crb2nu/loom/pkg/mills/worker"
	"github.com/crb2nu/loom/pkg/mills/workflow"
	"github.com/crb2nu/loom/pkg/openairesponses"
	"github.com/crb2nu/loom/pkg/telemetry"
)

var version = "dev"

// Compile-time guard that the production GitLab client satisfies the audit
// digest capability. If a method signature drifts, the audit follow-up writer
// would silently fall back to one-issue-per-finding (the capability assertion
// in Followup.OnRecorded fails at runtime, not build time) — this catches that
// at build. See pkg/mills/audit.DigestIssuer.
var _ audit.DigestIssuer = (*clients.GitLabClient)(nil)

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
	if err := installRepoGitAuth(rootCtx, cfg); err != nil {
		logger.Warn("repo git auth install failed; cumulative diff capture will report empty diffs", "repo_root", cfg.RepoRoot, "error", err)
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
	// when a judge client is configured.
	gateRegistry := gates.Default()
	flexClient := buildFlexInferClient(cfg, logger)
	litellmClient := buildLiteLLMClient(cfg, logger)
	// Backend selection (MILLS_JUDGE_BACKEND / MILLS_WEAVER_BACKEND). Default
	// keeps the judge + council contradiction judge + research weaver on the
	// FlexInfer proxy (judgeClient == flexClient, councilJudgeModel == "",
	// weaverClient == flexClient — byte-identical wiring). "litellm" binds the
	// gateway client so the judge/weaver run on a frontier OpenRouter model;
	// the judge hardening rides through unchanged because it lives in the
	// shared client + RubricJudge, not the proxy. A misconfigured litellm
	// backend fails loud here and degrades to FlexInfer.
	judgeClient, councilJudgeModel := resolveMillsJudgeClient(cfg, flexClient, logger)
	weaverClient := resolveMillsWeaverClient(cfg, flexClient, logger)
	capabilities := newCapabilityWiring(cfg)
	capabilities.FlexInferConfigured = strings.TrimSpace(cfg.FlexInferProxyURL) != ""
	capabilities.FlexInferReady = flexClient != nil
	// gateTiebreaker feeds the /wiring snapshot: "anthropic" when the dissent
	// tiebreaker is wired, else "none". Captured here (not re-derived) so it
	// tracks exactly what RegisterLLMGates* bound.
	gateTiebreaker := "none"
	if judgeClient != nil {
		judgeBackend := "flexinfer"
		if judgeClient != flexClient {
			judgeBackend = "litellm"
		}
		tiebreaker := buildGateTiebreaker(logger)
		if tiebreaker != nil {
			gateTiebreaker = "anthropic"
			gates.RegisterLLMGatesWithTiebreaker(gateRegistry, clients.NewRubricJudge(judgeClient), tiebreaker, "anthropic")
		} else {
			gates.RegisterLLMGates(gateRegistry, clients.NewRubricJudge(judgeClient))
		}
		logger.Info("LLM-judged gates enabled", "judge_backend", judgeBackend, "judge_model", judgeClient.JudgeModel(), "tiebreaker", tiebreaker != nil)
	} else {
		logger.Warn("LLM-judged gates disabled; spec_conformance + pr_self_review skipped (set FLEXINFER_PROXY_URL, or MILLS_JUDGE_BACKEND=litellm with LITELLM_PROXY_URL + FLEXINFER_JUDGE_MODEL)")
	}

	// Council runner. In production it uses FlexInfer-backed reviewers,
	// editor, and artifact judge. Local/degraded runs keep the deterministic
	// fakes so handlers can still be exercised, but autonomy readiness reports
	// the fake fallback as a blocker.
	councilRunner, councilUsesFakeAgents := buildCouncilRunner(st, pm, budget, cfg.RepoRoot, flexClient, litellmClient, judgeClient, councilJudgeModel, cfg.CouncilStages, logger)
	capabilities.CouncilConfigured = councilRunner != nil
	capabilities.CouncilUsesFakeAgents = councilUsesFakeAgents

	// Workspace-signals council brief (W3.1, .loom/126 Next waves). Feed real
	// failures into the council brief so it proposes grounded work over
	// synthetic canaries: Loki error clusters + recent FAILED GitLab CI
	// pipelines, merged via a composite source. Both are plain HTTP reads (no
	// hub); each is best-effort, so a nil/unconfigured source just drops out.
	// The GitLab client here is a dedicated read instance (cheap to construct;
	// the pipeline-stage client is built separately below).
	if councilRunner != nil {
		var sigSources []council.WorkspaceSignalSource
		if loki := clients.NewLokiClient(cfg.LokiURL, logger); loki != nil {
			sigSources = append(sigSources, loki)
		}
		if ci := buildGitLabClient(cfg, logger); ci != nil {
			sigSources = append(sigSources, ci)
		}
		if signals := council.NewCompositeSignals(sigSources...); signals != nil {
			councilRunner.Signals = signals
			logger.Info("council brief workspace-signals enabled", "sources", len(sigSources))
		}
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
		withGitLabBaseURL(cfg.GitLabAPIURL).
		withKillSwitch(gitopsKillSwitch, cfg.GitOpsPolicyPath, cfg.GitOpsDefaultBranch)
	op.beginActivitySourceWiring()
	// Audit subsystem is attached below after the pipeline runner +
	// FlexInfer client are ready; handlers read the fields at request
	// time so late attachment is fine.

	// Async spins (plan .loom/166). Root background spins at the operator
	// lifetime (rootCtx) so a client disconnect can't cancel an in-flight spin;
	// size the concurrency gate from the environment (default 2 — frontier
	// frames are slow + costly).
	op.spinBaseCtx = rootCtx
	if n := spinConcurrencyFromEnv(logger); n > 0 {
		op.spinSem = make(chan struct{}, n)
	}
	// Reconcile async-spin status rows orphaned by the previous process: the
	// operator is a singleton, so any pending/running spin_runs row is a spin
	// whose goroutine died with the old pod. Mark them failed(orphaned); the
	// draft plan, if authored before the crash, is still durable in the Plan
	// Store. Best-effort — a sweep error must not block startup.
	if swept, err := st.Spin.MarkOrphaned(rootCtx); err != nil {
		logger.Warn("async spin startup orphan-sweep failed", "error", err)
	} else if swept > 0 {
		logger.Info("async spin startup orphan-sweep complete", "orphaned", swept)
	}

	// Async council runs (#334), same shape as the spins above: root them at
	// the operator lifetime so a client disconnect can't cancel a pass, size
	// the worker gate from config, and reconcile council_runs rows whose worker
	// died with the old pod. An orphaned row also holds an active budget
	// reservation, so the sweep is what keeps the daily cap honest across a
	// restart instead of waiting out the 6-hour admission lease.
	op.councilBaseCtx = rootCtx
	if n := cfg.CouncilAsyncConcurrency; n > 0 {
		op.councilSem = make(chan struct{}, n)
	}
	if swept, err := op.sweepOrphanedCouncilRuns(rootCtx); err != nil {
		logger.Warn("council startup orphan-sweep failed", "error", err)
	} else if swept > 0 {
		logger.Info("council startup orphan-sweep complete", "orphaned", swept)
	}

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
		capabilities.MCPHubLiveHealth = func() (bool, string) {
			health := hubClient.ServerHealth(clients.AgentContextServerName)
			switch {
			case !health.Known:
				return false, "MCP hub agent_context health is not yet known"
			case health.Healthy:
				return true, "MCP hub and operator agent-context session are live"
			default:
				return false, fmt.Sprintf("MCP hub agent_context unavailable after %d consecutive failure(s): %s",
					health.ConsecutiveFailures, health.LastError)
			}
		}
		defer func() { endOperatorSession(hubClient, operatorSession.SessionID(), logger) }()
		// Backlog intake reads the plan store to hydrate slice scope onto
		// plan-linked items (handlers read the field at request time, so
		// late attachment is fine — same pattern as the audit subsystem).
		op.planReader = clients.NewPlanClient(hubClient, "loom-mills-operator")

		// Spinning Room (Live Beam slice 3 / F2): the operator picks a model
		// frame from the HUD and spins a draft plan into the Plan Store over the
		// hub. Frames + enablement are GitOps policy (read via pm.Current so a
		// ConfigMap hot-reload re-shapes the frame list live); the chosen frame
		// is run-scoped and recorded on the draft plan for audit. Wired only
		// when the hub is reachable — draft plans are authored over it.
		op.withSpinner(&spin.Spinner{
			Enabled: func() bool { return pm.Current().SpinningRoomEnabled() },
			Frame: func(name string) (spin.Frame, bool) {
				a, ok := pm.Current().SpinningRoomFrame(name)
				if !ok {
					return spin.Frame{}, false
				}
				return spin.Frame{Name: a.Name, Model: a.Model, Backend: a.Backend}, true
			},
			NewEditor: func(f spin.Frame) (council.Editor, error) {
				ed := buildEditorForAgent(
					mills.CouncilAgent{Name: f.Name, Model: f.Model, Backend: f.Backend},
					"", flexClient, cfg.RepoRoot, nil, logger)
				if ed == nil {
					return nil, fmt.Errorf("no inference backend for frame %q (set FLEXINFER_PROXY_URL, an OpenAI key, or an Anthropic key)", f.Name)
				}
				return ed, nil
			},
			Author:          clients.NewPlanClient(hubClient, spinningRoomAgentID),
			DefaultPriority: func() string { return pm.Current().SpinningRoomDefaultPriority() },
			// Only attempts that reached the editor are counted, so the frame
			// label space stays bounded by policy (never request strings).
			OnSpinDone: func(frame string, ok bool) {
				outcome := "ok"
				if !ok {
					outcome = "error"
				}
				mills.SpinsTotal.WithLabelValues(frame, outcome).Inc()
			},
			Logger: logger,
		})
	}

	// HUD spawn client: built ONCE here so both the DAG pipeline dispatcher
	// AND the S6-min imperative workflow runtime share the exact same client
	// (zero new pods/services). Nil when LOOM_HUD_URL/TOKEN are unset.
	hudSpawn := buildHUDSpawnClient(cfg, logger)
	op.withSpawnClient(hudSpawn)

	// Worker dispatcher: real clients where configured, NoOpDispatcher
	// for stages whose backing service isn't wired yet. The operator
	// logs each gap so it's obvious which surfaces are stub vs production.
	// agentFor/modelFor feed the /wiring snapshot below with the ITEM-LESS
	// baseline (env > stage_agents/stage_models > default). Dispatch itself
	// goes through spawnRouteFor, which layers per-item routing on top of the
	// same rungs — see its doc comment for the full precedence.
	agentFor := agentForStage(pm)
	modelFor := modelForStage(pm)

	// Serial merge queue (policy merge_queue.enabled, default OFF). The
	// gateway routes the merge stage's candidate into the durable queue; the
	// processor (started in the errgroup below) serializes merges per
	// (project, target_branch). Both consult the hot-reloaded policy so a
	// ConfigMap flip activates/halts the queue without a restart.
	mergeQueueEnabled := func() bool { return pm.Current().MergeQueueEnabled() }
	var mergeQueueGateway pipeline.MergeQueue
	if st != nil && st.MergeQueue != nil {
		mergeQueueGateway = &mergequeue.StageGateway{
			DAO:      st.MergeQueue,
			MaxDepth: func() int { return pm.Current().MergeQueueMaxDepth() },
		}
	}
	dispatcher, realStages := buildDispatcher(cfg, weaverClient, hubClient, st, logger, autoMergeFor(pm), flakyCIJobsFor(pm), substrateForStage(pm), spawnRouteFor(pm, st, logger), hudSpawn, mergeQueueGateway, mergeQueueEnabled)
	capabilities.DispatcherRealStages = realStages
	capabilities.BranchContractReady = true
	capabilities.BranchContractSource = "pkg/mills/pipeline/branch_contract.go"
	capabilities.HUDSpawnConfigured = strings.TrimSpace(cfg.HUDBaseURL) != "" && strings.TrimSpace(cfg.HUDToken) != ""
	capabilities.HUDSpawnReady = realStages["plan_slice"] && realStages["implement"] && realStages["pr_self_review"]

	// Resolved model-wiring snapshot for GET /api/mills/wiring. Populated HERE,
	// adjacent to the wiring log lines, from the SAME resolved values (judge /
	// weaver clients, councilJudgeModel, agentFor/modelFor, gateTiebreaker) so
	// it can never drift from what the operator logged at startup. Static — the
	// routing changes only on restart.
	spawnDefaultAgent := strings.TrimSpace(os.Getenv("LOOM_MILLS_SPAWN_AGENT"))
	spawnEnvAgent := spawnDefaultAgent != ""
	if spawnDefaultAgent == "" {
		spawnDefaultAgent = mills.AgentDefault
	}
	op.withWiringSnapshot(buildWiringSnapshot(wiringInputs{
		cfg:               cfg,
		policy:            pm.Current(),
		flexClient:        flexClient,
		judgeClient:       judgeClient,
		weaverClient:      weaverClient,
		litellmConfigured: litellmClient != nil,
		councilJudgeModel: councilJudgeModel,
		agentFor:          agentFor,
		modelFor:          modelFor,
		spawnDefaultAgent: spawnDefaultAgent,
		spawnEnvAgent:     spawnEnvAgent,
		spawnEnvModel:     strings.TrimSpace(os.Getenv("LOOM_MILLS_SPAWN_MODEL")) != "",
		gateTiebreaker:    gateTiebreaker,
		now:               time.Now(),
	}))

	// Infrastructure admission gates (storage health + local config). One
	// evaluator is shared by the pipeline preflight, the council planning
	// policy, and the status endpoint so all three agree on one round of
	// probes. Defaults to observe mode — see healthGateModeObserve.
	healthGates := buildHealthGates(cfg, st, capabilities.MCPHubLiveHealth, logger)
	op.withHealthGates(healthGates)

	pipelineRunner := pipeline.New(st, gateRegistry, dispatcher, pm)
	pipelineRunner.Logger = logger
	pipelineRunner.HealthGates = healthGates.runnerPreflight()
	if councilRunner != nil {
		councilRunner.HealthGates = healthGates.runnerPreflight()
	}
	// Item-memory consolidation. Wired whenever a weaver-capable client
	// exists, but INERT until LOOM_MILLS_MEMORY_CONSOLIDATE is set: the record
	// hook checks the flag itself, so an unset operator makes no LLM call ever.
	// Constructing it unconditionally means a bad FLEXINFER_MEMORY_MODEL shows
	// up in this startup log rather than in the first oversized journal.
	if weaverClient != nil {
		memoryModel := strings.TrimSpace(cfg.FlexInferMemoryModel)
		if memoryModel == "" {
			memoryModel = weaverClient.WeaverModel()
		}
		pipelineRunner.MemoryConsolidator = pipeline.NewMemoryConsolidator(
			weaverClient, memoryModel, logger)
		logger.Info("pipeline item-memory consolidation wired",
			"model", memoryModel,
			"enabled", pipeline.MemoryConsolidateEnabled(),
			"env", pipeline.MemoryConsolidateEnv)
	}
	// Post-plan_slice scope hydration: after the plan_slice stage authors a
	// decomposition into the plan store, the runner stamps file-bearing
	// slices back onto a slice-less item so post_implement_gate enforces a
	// real envelope instead of recording an advisory skip (escalations
	// #332/#338: importer/emitter items reached the gate slice-less despite
	// a successful plan_slice stage).
	if hubClient != nil {
		pipelineRunner.SliceHydrator = clients.NewPlanClient(hubClient, "loom-mills-operator")
		logger.Info("pipeline plan-slice scope hydration wired")
	}
	pipelineRunner.AutonomyGate = pipeline.AutonomyGateFromCouncil(council.AutonomyGateFunc(func(ctx context.Context) council.AutonomyGateDecision {
		report := op.capabilityReport(ctx)
		return council.AutonomyGateDecision{
			Allowed:  report.AutonomyReady,
			Blockers: report.AutonomyBlockers,
		}
	}))
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
	var auditCfg mills.AuditPolicy
	if pol := pm.Current(); pol != nil {
		auditCfg = pol.Audit
	}
	auditDispatcher, auditWorker, auditTriggers, auditPolicy := buildAuditSubsystem(
		flexClient, councilRunner, st, cfg.RepoRoot, auditCfg, logger,
	)
	if auditTriggers != nil {
		mergedHooks = append(mergedHooks, auditTriggers.OnPipelineMerged)
		logger.Info("audit triggers enabled (council + pipeline)")
	} else {
		logger.Info("audit triggers disabled (FLEXINFER_PROXY_URL or council runner missing)")
	}
	pipelineRunner.OnMerged = chainPipelineMerged(mergedHooks...)
	// Record `failed` squad outcomes on real escalations so the router's
	// confidence signal reflects failures, not just merges.
	pipelineRunner.OnEscalated = squadRecorder.OnEscalated
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
	if gitlabClient != nil {
		op.withVerdictMRVerification(func(project string) mills.MRStateClient {
			return gitlabClient.ForProject(project)
		}, st.Pipeline, cfg.GitLabProject)
		backfillCtx, backfillCancel := context.WithTimeout(rootCtx, legacyMRProjectBackfillTimeout)
		result, backfillErr := backfillLegacyMRProjects(
			backfillCtx,
			st.Pipeline,
			gitLabLegacyMRProjectVerifier{client: gitlabClient},
			cfg.GitLabAPIURL,
			logger,
		)
		backfillCancel()
		if backfillErr != nil {
			logger.Warn("legacy MR project backfill pass failed", "error", backfillErr)
		} else if result.Scanned > 0 {
			logger.Info("legacy MR project backfill pass complete",
				"scanned", result.Scanned,
				"patched", result.Patched,
				"already_present", result.AlreadyPresent,
				"rejected", result.Rejected,
				"verification_errors", result.VerificationError,
				"patch_errors", result.PatchError)
		}
	}
	var handoffClient pipeline.HandoffClient
	// contextRecorder writes the operator's decisions/findings into the same
	// long-lived agent-context session its handoffs are packaged from, so those
	// handoffs stop shipping with entry_count: 0.
	var contextRecorder *clients.ContextRecorder
	if hubClient != nil {
		handoff := clients.NewHandoffClient(hubClient, sessionID)
		handoff.SourceSessionIDFunc = operatorSession.SessionID
		handoffClient = handoff
		contextRecorder = clients.NewContextRecorder(hubClient, sessionID)
		contextRecorder.SessionIDFunc = operatorSession.SessionID
		logger.Info("escalator handoff configured (mcp-agent-context)")
		logger.Info("operator context recorder configured (agent_context_add)")
	} else {
		logger.Warn("escalator handoff disabled (no MCP hub or operator session)")
		logger.Warn("operator context recorder disabled (no MCP hub); handoffs will carry no entries")
	}
	// Assign the late-bound handoff-inbox merge notifier now that the handoff
	// client exists. Gated on policy.notify.handoff_inbox + a reachable hub;
	// reuses the same handoffClient the escalator uses.
	if pol := pm.Current(); pol != nil && pol.Notify.HandoffInbox && handoffClient != nil {
		handoffNotify = notify.NewHandoffHook(handoffClient, pol.Notify.HandoffTarget, pol.Notify.MRBaseURL, logger)
		if contextRecorder != nil {
			handoffNotify.SetContextRecorder(contextRecorder)
		}
		logger.Info("notify handoff-inbox hook enabled", "target", handoffNotify.Target())
	}
	// Plan→repo bootstrap (POST /api/mills/projects/bootstrap): mint a new
	// GitLab project from a Spinning Room plan. Needs BOTH the GitLab client
	// (repo create + seed commit; the token must be group-scoped for
	// POST /projects) and the MCP hub (plan read + re-scope). The plan is
	// re-scoped into the emitter's namespace gate so its slices are demand-
	// visible once the operator advances it. Policy-gated at request time
	// (cross_repo.enabled + cross_repo.allow_bootstrapped), so wiring it
	// unconditionally here is safe — it ships inert.
	if gitlabClient != nil && hubClient != nil {
		op.bootstrapper = &bootstrap.Service{
			GitLab:    bootstrap.GitLabAdapter{Client: gitlabClient},
			Plans:     clients.NewPlanClient(hubClient, "loom-mills-operator"),
			Store:     st.Bootstrap,
			Namespace: pm.Current().PlanSliceEmitterNamespace(),
			// GroupAllowed bounds where a repo may be minted (manual endpoint AND
			// the reconciler pre-flight) to the policy allow-list. Read per call
			// so a hot policy reload is honored — no restart to change the set.
			GroupAllowed: func(group string) bool {
				return pm.Current().CrossRepoBootstrapGroupAllowed(group)
			},
			Logger: logger,
		}
		logger.Info("project bootstrap wired",
			"namespace", pm.Current().PlanSliceEmitterNamespace(),
			"policy_enabled", pm.Current().CrossRepoBootstrapEnabled(),
			"allowed_groups", pm.Current().CrossRepoBootstrapAllowedGroups())
	}

	if gitlabClient != nil || handoffClient != nil {
		escalator := pipeline.NewEscalator(st, gitlabClient, handoffClient)
		escalator.Logger = logger
		if contextRecorder != nil {
			escalator.Recorder = contextRecorder
		}
		pipelineRunner.Escalator = escalator
		logger.Info("escalator enabled",
			"issue", gitlabClient != nil,
			"handoff", handoffClient != nil,
			"context_recorder", contextRecorder != nil)
	} else {
		logger.Warn("escalator disabled; failures will transition to escalated state without issue/handoff publication")
	}

	// Scope-escalation rescue MR (S2 of the 2026-07-26 scope-gate reliability
	// plan). A run that dies at post_implement_gate never reaches the `mr`
	// stage, so its complete diff sits on an un-MR'd branch until a human goes
	// looking. This opens a Draft MR over that branch — routed per item so a
	// cross-repo item's rescue lands in ITS project, and never auto-merge-armed
	// (CreateMR ignores req.AutoMerge; the merge stage is the only merge path).
	if gitlabClient != nil {
		pipelineRunner.RescueMR = func(ctx context.Context, _ *store.PipelineRun, item *store.BacklogItem, req pipeline.CreateMRRequest) (pipeline.CreateMRResponse, error) {
			client := gitlabClient
			if item != nil {
				if forProject := gitlabClient.ForProject(item.TargetProject); forProject != nil {
					client = forProject
				}
			}
			return client.CreateMR(ctx, req)
		}
		logger.Info("scope-escalation rescue MR enabled")
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
		// S3 (.loom/163): when the plan-slice emitter is configured, route any
		// proposal the editor decomposed into slices to a sliced Plan in that
		// namespace (one MR per slice via the emitter) instead of a flat
		// fan-out backlog item. Empty namespace ⇒ off (flat-item behavior).
		councilRunner.Mutator.PlanSliceNamespace = pm.Current().PlanSliceEmitterNamespace()
		logger.Info("council inline plan authoring enabled",
			"plan_slice_namespace", pm.Current().PlanSliceEmitterNamespace())
	}

	// Merged-work grounding: let the mutator compare each proposal against the
	// merge requests main already took, so a brief assembled before the tick's
	// merges stops re-proposing shipped work. Reached in here because
	// buildCouncilRunner runs before the GitLab client exists — the same
	// late-injection the plan authoring above uses. Left nil when GitLab is
	// unconfigured, which makes the pass inert rather than fail-closed; the
	// policy gate (council.dedup.merged_work.enabled, default ON) is read
	// per-run by the runner so a hot reload takes effect without a restart.
	if gitlabClient != nil && councilRunner != nil && councilRunner.Mutator != nil {
		councilRunner.Mutator.MergedWork = gitlabClient
		if councilRunner.Mutator.Logger == nil {
			councilRunner.Mutator.Logger = logger
		}
		logger.Info("council merged-work grounding wired",
			"enabled", pm.Current().CouncilMergedWorkGroundingEnabled(),
			"lookback", pm.Current().CouncilMergedWorkLookback().String())
	}

	// Factory-exhaust demand: let the brief surface the open `flaky-test` and
	// `audit-digest` issues this mill filed against itself, so an unattended
	// shift has machine-filed maintenance demand to draw on instead of idling.
	// Same late injection as merged-work grounding above, for the same reason
	// (buildCouncilRunner runs before the GitLab client exists). Left nil when
	// GitLab is unconfigured, which omits the section rather than failing the
	// brief; the policy gate (council.sources.factory_exhaust.enabled, default
	// ON) is read per-run by the runner.
	if gitlabClient != nil && councilRunner != nil {
		councilRunner.FactoryExhaust = gitlabClient
		logger.Info("council factory-exhaust demand source wired",
			"enabled", pm.Current().CouncilFactoryExhaustEnabled(),
			"lookback", pm.Current().CouncilFactoryExhaustLookback().String(),
			"max_items", pm.Current().CouncilFactoryExhaustMaxItems())
	}

	// Pattern Loom A1: inject the approved-pattern catalog into the council
	// editor prompt so each proposal conforms to / cites a vetted archetype
	// and carries a pattern_id. Reached in here (not buildCouncilRunner)
	// because the hub is established after the runner is built — same shape
	// as the inline plan-authoring wiring above. Best-effort: the editor's
	// fetch is non-blocking, so an unreachable catalog just drops the prompt
	// section. Only the concrete FlexInfer/OpenAI editors carry the catalog;
	// the FakeEditor fallback ignores it.
	if hubClient != nil && councilRunner != nil {
		patternLister := clients.NewPatternClient(hubClient)
		switch ed := councilRunner.Editor.(type) {
		case *clients.FlexInferCouncilEditor:
			ed.Patterns = patternLister
			logger.Info("council editor wired to approved-pattern catalog", "editor", "flexinfer")
		case *clients.OpenAIResponsesCouncilEditor:
			ed.Patterns = patternLister
			logger.Info("council editor wired to approved-pattern catalog", "editor", "openai-responses")
		}
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
	if auditTriggers != nil && gitlabClient != nil {
		auditTriggers.LoadMergedDiff = gitlabMergedDiffLoader(gitlabClient, st.Pipeline)
		logger.Info("audit merged-diff loader wired (GitLab MR diffs)")
	} else if auditTriggers != nil {
		logger.Info("audit merged-diff loader disabled (no GitLab client); pipeline_merge audits skip")
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
	escalationSweeper := mills.NewEscalationSweeper(reconciler, pm)
	escalationSweeper.Logger = logger
	escalationSweeper.Enabled = op.workAdmissionOpen
	retryMetrics := telemetry.NewRetryMetrics(prometheus.DefaultRegisterer)
	reconciler.ExternalIncidentRetryDecision = func(ctx context.Context, runID string) (bool, string, error) {
		decision, err := (pipeline.RetryPolicy{
			Store:                        st.Pipeline,
			VerdictStore:                 st.ClassificationVerdicts,
			ExternalIncidentPaidRetryCap: pm.Current().Pipeline.Retry.ExternalIncidentPaidRetryCap,
			Metrics:                      retryMetrics,
		}).Decide(ctx, runID, true)
		return decision.Allowed, decision.Disposition, err
	}
	// HomeProject is the reference for the cross-repo gate: an item whose
	// TargetProject names a different repo only runs when cross_repo is enabled.
	reconciler.HomeProject = cfg.GitLabProject
	// Session continuity: stamp the operator's CURRENT agent-context session on
	// every backlog-driven run so stage spawns inherit it as
	// LOOM_PARENT_SESSION_ID and can recall what the operator recorded. Read
	// through the getter (not a frozen copy) — the session maintainer replaces
	// the id after a hub outage.
	reconciler.OperatorSessionID = operatorSession.SessionID
	reconciler.AutonomyGate = func(ctx context.Context) (bool, []string) {
		report := op.capabilityReport(ctx)
		return report.AutonomyReady, report.AutonomyBlockers
	}
	// Run provenance: stamp the configuration each run starts under so a merged
	// run can be joined back to the policy revision, stage-model pins, and
	// prompt templates that produced it. Stage models resolve through the same
	// precedence chain the SpawnWorker uses (env break-glass included); prompt
	// hashes are over compile-time template bytes, so they are computed once.
	reconciler.ProvenanceStageModels = provenanceStageModels(resolveSpawnRoute(pm))
	promptHashes := provenancePromptHashes()
	reconciler.ProvenancePromptHashes = func() map[string]string { return promptHashes }
	// Plan→repo bootstrap pre-flight: before a cross-repo item whose
	// TargetProject has no repo yet dispatches, mint the repo (when policy
	// allow-lists its group) so the spawn's git-clone succeeds instead of
	// escalating on a 404. Shares the same gated create-repo helper as the
	// manual POST /api/mills/projects/bootstrap endpoint. Guarded on a non-nil
	// bootstrapper so the interface field never holds a typed-nil pointer.
	if op.bootstrapper != nil {
		reconciler.RepoEnsurer = op.bootstrapper
		logger.Info("reconciler plan→repo bootstrap pre-flight wired")
	}
	if squadsLoader != nil {
		// Wire the squad router into the reconciler so each tick attributes
		// the chosen squad via a "reconciler.squad_routed" event keyed on
		// the new run id. squadRecorder.OnMerged then reads it back at merge
		// time. Adapter glues squads.Decision → mills.SquadDecision without
		// pulling pkg/mills/squads into pkg/mills (no import cycle).
		router := squads.NewRouter(squadsLoader, st)
		// Gate routing on policy.squads.enabled via the live PolicyManager
		// so a hot-reload flips routing without a restart. Previously the
		// gate was designed but never wired, so the flag had no effect.
		router.Policy = squadsPolicyGate{pm: pm}
		reconciler.SquadRouter = &squadRouterAdapter{router: router}
		logger.Info("squad routing enabled", "min_confidence", router.MinConfidence)
	} else {
		logger.Info("squad routing disabled (no squads loader)")
	}
	// S7 imperative template selection: always wired; inert for items with no
	// selection, and explicit selections stay held (skip + reason) until
	// policy workflows.enabled=true.
	reconciler.WorkflowSelector = &workflowSelectorAdapter{registry: workflow.NewDefaultRegistry()}
	// Ghost-spark reap sweep: reconcile escalated items against GitLab MR
	// reality so merge-when-pipeline-succeeds merges that landed after a run
	// escalated at the merge stage drain the escalated pile instead of sitting
	// forever. Needs the GitLab client for MR-state lookups; the Escalator (when
	// wired) auto-closes the reaped item's open escalation issue.
	if gitlabClient != nil {
		reconciler.GhostSparkMRState = gitlabClient
		reconciler.GhostSparkMRStateForProject = func(project string) mills.MRStateClient {
			return gitlabClient.ForProject(project)
		}
		if resolver, ok := pipelineRunner.Escalator.(mills.GhostSparkResolver); ok && resolver != nil {
			reconciler.GhostSparkResolver = resolver
		}
		// Green-MR adoption: when a run escalated because CI infrastructure
		// killed its pipeline and a retry later went green, the MR is left open
		// and mergeable with no live stage to merge it. The client refuses
		// anything that is not open + conflict-free + mergeable + green.
		reconciler.GhostSparkGreenMRAdopter = gitlabClient
		// Second pass: items that escalated before the mr stage have no IID to
		// look up, so the pass above cannot see them even after their branch is
		// merged by hand. Resolve their deterministic branches here, where
		// pkg/mills/pipeline is importable — pkg/mills cannot import it (the
		// dependency runs the other way), and duplicating the label-prefix
		// mapping would let the two drift.
		reconciler.GhostSparkMergedBranch = gitlabClient
		reconciler.GhostSparkBranchesFor = func(item *store.BacklogItem) []string {
			if item == nil {
				return nil
			}
			seen := make(map[string]bool)
			var out []string
			add := func(branch string) {
				if branch == "" || seen[branch] {
					return
				}
				seen[branch] = true
				out = append(out, branch)
			}
			// Slice branches first: a council-authored item lands its work on
			// the slice branch, so that is the likeliest hit.
			for _, slice := range item.Slices {
				add(pipeline.BranchContractFor(nil, item, pipeline.Stage{}, slice.Name).SliceBranch)
			}
			add(pipeline.BranchContractFor(nil, item, pipeline.Stage{}, "").SourceBranch)
			return out
		}
		// Cross-repo half of the merged-branch pass: the lookup project is
		// authorized by the run's immutable escalation-time target binding
		// (never mutable target_project); this only supplies the client for
		// the project the reconciler already authorized. Same shared-token
		// contract as GhostSparkMRStateForProject above.
		reconciler.GhostSparkMergedBranchForProject = func(project string) mills.MergedBranchMRClient {
			return gitlabClient.ForProject(project)
		}
		logger.Info("ghost-spark reap sweep enabled",
			"issue_autoclose", reconciler.GhostSparkResolver != nil,
			"merged_branch_pass", true)
		// Post-merge regression attribution: join merged MRs to later revert
		// commits on the default branch, revert-trailer only. Both halves come
		// from the same read-only client, so they arm together or not at all.
		regressionSource := gitlabRegressionSource{client: gitlabClient}
		reconciler.RegressionMergedMRs = regressionSource
		reconciler.RegressionCommits = regressionSource
		reconciler.RegressionSweepInterval = cfg.RegressionSweepInterval
		effectiveInterval := cfg.RegressionSweepInterval
		if effectiveInterval <= 0 {
			effectiveInterval = mills.DefaultRegressionSweepInterval
		}
		logger.Info("regression attribution sweep enabled", "interval", effectiveInterval.String())
	} else {
		logger.Info("ghost-spark reap sweep disabled (no GitLab client)")
		logger.Info("regression attribution sweep disabled (no GitLab client)")
	}
	// Signature-candidate mining reads the store and the in-process classifier
	// corpus only, so it arms regardless of the GitLab client. pkg/mills cannot
	// import pkg/mills/pipeline (the dependency runs the other way), so the
	// predicate is injected here from the one real classifier corpus rather
	// than reimplemented in the reconciler.
	reconciler.SignatureEvidenceClassified = pipeline.KnownFailureSignature
	reconciler.SignatureMiningInterval = cfg.SignatureMiningInterval
	signatureInterval := cfg.SignatureMiningInterval
	if signatureInterval <= 0 {
		signatureInterval = mills.DefaultSignatureMiningInterval
	}
	logger.Info("signature-candidate mining sweep enabled", "interval", signatureInterval.String())
	// Learning-signal export: republish the same reports handlePromotionReport,
	// handleJudgeCalibration and handleConfigOutcomes serve as gauges, so a
	// judge-drift alert can watch them. Wired here because pkg/mills cannot
	// import pkg/mills/guard (the dependency runs the other way), and the
	// exporter reuses those builders rather than re-aggregating.
	if cfg.LearningSignalExport == nil || *cfg.LearningSignalExport {
		reconciler.LearningSignals = &guard.LearningSignalExporter{
			Events:               st.Events,
			Runs:                 st.Pipeline,
			PromotionActorPrefix: promotionReportDefaultActor,
		}
		reconciler.LearningSignalInterval = cfg.LearningSignalInterval
		reconciler.LearningSignalWindow = cfg.LearningSignalWindow
		learningInterval := cfg.LearningSignalInterval
		if learningInterval <= 0 {
			learningInterval = mills.DefaultLearningSignalInterval
		}
		logger.Info("learning-signal export sweep enabled",
			"interval", learningInterval.String(), "actor_prefix", promotionReportDefaultActor)
	} else {
		logger.Info("learning-signal export sweep disabled (LOOM_MILLS_LEARNING_SIGNAL_EXPORT)")
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
	// workAdmissionEnabled gates every automated work-admission loop. Beyond
	// the durable kill-switch/crash-lease (workAdmissionOpen), overseer vetoes
	// (the sentinel's TTL suppression lease) registered later in wiring are
	// consulted at CALL time via op.admissionSuppressed, so a dependency
	// incident pauses new work without touching the kill-switch machinery.
	// HTTP `admit` endpoints deliberately ignore overseer vetoes — a human
	// may still act during an incident.
	workAdmissionEnabled := func() bool {
		return op.workAdmissionOpen() && !op.admissionSuppressed()
	}
	scheduler := mills.NewScheduler(reconciler)
	scheduler.Logger = logger
	scheduler.Enabled = workAdmissionEnabled
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
	crossRunSched.Enabled = workAdmissionEnabled
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
	councilSched.Enabled = workAdmissionEnabled

	// Canary-autopilot scheduler (.loom/126 Wave 1 / A3-sustain). Wakes every
	// minute and, when policy.intake.canary_autopilot.enabled, enqueues + starts
	// one deterministic heartbeat canary per schedule match — the automation
	// that lets autonomous_merges_24h tick ≥1/day without a human running
	// `loom mills pipelines canary` (the gap that dropped the loop to 0 merges
	// on 2026-06-26). Default-OFF: the scheduler self-gates on the policy flag,
	// so it is inert until the ConfigMap opts in. The run fn applies the SAME
	// 24h canary dedupe the manual path uses, so a still-in-flight or escalated
	// canary suppresses the autopilot enqueue rather than piling on.
	canaryRunFn := func(ctx context.Context, reason string) error {
		pol := pm.Current()
		if pol == nil || !pol.IsEnabled() {
			return nil
		}
		id := "MILLS-CANARY-AUTOPILOT-" + time.Now().UTC().Format("20060102-150405")
		item := store.CanaryHeartbeatItem(id, "", pol.CanaryAutopilotPriority(), pol.CanaryAutopilotFixturePath(), "mills canary autopilot")
		existing, derr := findRecentCanary(ctx, st, canaryDedupeWindow, item.ID)
		if derr != nil {
			return fmt.Errorf("canary autopilot dedupe check: %w", derr)
		}
		if existing != nil {
			logger.Info("canary autopilot skipped: prior canary still in flight",
				"existing_id", existing.ID, "existing_state", existing.State, "trigger", reason)
			return nil
		}
		if err := st.Backlog.Put(ctx, &item); err != nil {
			return fmt.Errorf("canary autopilot enqueue %s: %w", item.ID, err)
		}
		res, err := reconciler.StartQueuedItem(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("canary autopilot start %s: %w", item.ID, err)
		}
		runID := ""
		runState := ""
		if res.Run != nil {
			runID = res.Run.ID
			runState = string(res.Run.State)
		}
		logger.Info("canary autopilot enqueued + started",
			"backlog_id", item.ID, "decision", res.Decision, "run_id", runID, "state", runState, "trigger", reason)
		return nil
	}
	canarySched := mills.NewCanaryScheduler(canaryRunFn, pm)
	canarySched.Logger = logger
	canarySched.Enabled = workAdmissionEnabled

	// Serial merge queue processor. Self-gates on the hot-reloaded policy
	// fence every tick (merge_queue.enabled AND the mills kill switch), so
	// wiring it into the errgroup activates nothing until the policy flips.
	// A nil ForProject (GitLab unwired) leaves it a benign no-op.
	mergeQueueProc := &mergequeue.Processor{
		Store:   st,
		Enabled: mergeQueueEnabled,
		Logger:  logger,
	}
	if gitlabClient != nil {
		mergeQueueProc.ForProject = func(project string) mergequeue.Forge {
			return gitlabClient.ForProject(project)
		}
	}
	op.withActivitySources(
		namedActivitySource{name: activitySourceReconciler, source: scheduler},
		namedActivitySource{name: activitySourcePipeline, source: pipelineRunner},
		namedActivitySource{name: activitySourceCrossRun, source: crossRunSched},
		namedActivitySource{name: activitySourceCouncil, source: councilSched},
		namedActivitySource{name: activitySourceCanary, source: canarySched},
	)

	// Mills overseers — the supervisory agents that groom the mill itself:
	// the backlog groomer here, then the deployment-health sentinel and
	// the KPI foreman below, all on the same harness. Guard
	// rails: default-OFF policy section, dry-run default ON, per-tick/day
	// caps, and every action audited in the events table. The groomer's LLM
	// verdicts reuse the ALREADY-RESOLVED judge client so its backend
	// selection (and litellm exclusions) stay single-sourced; a nil judge
	// degrades the groomer to deterministic-only, never blocks it.
	var groomerTriage *overseer.Triage
	if judgeClient != nil {
		groomerTriage = &overseer.Triage{Client: judgeClient, Logger: logger}
	}
	groomer := &overseer.Groomer{
		Store:  st,
		Policy: pm.Current,
		Triage: groomerTriage,
		Recorder: &overseer.ActionRecorder{
			Events: st.Events,
			Actor:  "overseer.groomer",
			DryRun: func() bool {
				pol := pm.Current()
				return pol == nil || mills.DryRunOn(pol.Overseers.Groomer.DryRun)
			},
		},
		Logger: logger,
	}
	groomerHarness := &overseer.Harness{
		Agent: groomer,
		Enabled: func() bool {
			pol := pm.Current()
			return workAdmissionEnabled() && pol != nil && pol.GroomerEnabled()
		},
		Interval: func() time.Duration {
			pol := pm.Current()
			if pol == nil {
				return time.Hour
			}
			return pol.Overseers.Groomer.Interval()
		},
		// First tick shortly after boot: Recreate rollouts reset the 60m
		// interval clock, so churn-heavy days otherwise starve the groomer.
		BootTick: 2 * time.Minute,
		Logger:   logger,
	}
	// Optional activity source (NOT in requiredActivitySourceNames): the
	// safety quiescence snapshot counts an in-flight groomer tick without
	// requiring the source on operators built before this slice.
	op.addActivitySource("overseer_groomer", groomerHarness)
	logger.Info("overseer groomer wired",
		"enabled", pm.Current().GroomerEnabled(),
		"dry_run", mills.DryRunOn(pm.Current().Overseers.Groomer.DryRun),
		"llm_triage", groomerTriage.Available())

	// Deployment-health sentinel (overseers slice 2). Probes the operator's
	// hard dependencies; after trips_to_open consecutive failures it opens
	// an incident, optionally files a dedup-marked GitLab issue, and — when
	// allow.suppress_admission — asserts a TTL suppression lease that the
	// workAdmissionEnabled closure above consults. Probes are built only for
	// dependencies this deployment actually configures.
	var sentinelProbes []overseer.Probe
	if cfg.FlexInferProxyURL != "" {
		sentinelProbes = append(sentinelProbes, overseer.NewHTTPProbe("flexinfer",
			strings.TrimRight(cfg.FlexInferProxyURL, "/")+"/v1/models",
			map[string]string{"Authorization": bearerOrEmpty(cfg.FlexInferToken)}))
	}
	if cfg.GitLabAPIURL != "" {
		sentinelProbes = append(sentinelProbes, overseer.NewHTTPProbe("gitlab",
			strings.TrimRight(cfg.GitLabAPIURL, "/")+"/version",
			map[string]string{"PRIVATE-TOKEN": cfg.GitLabToken}))
	}
	if cfg.HUDBaseURL != "" {
		sentinelProbes = append(sentinelProbes, overseer.NewHTTPProbe("hud",
			strings.TrimRight(cfg.HUDBaseURL, "/")+"/api/health",
			map[string]string{"Authorization": bearerOrEmpty(cfg.HUDToken)}))
	}
	if cfg.LokiURL != "" {
		sentinelProbes = append(sentinelProbes, overseer.NewHTTPProbe("loki",
			strings.TrimRight(cfg.LokiURL, "/")+"/ready", nil))
	}
	sentinel := &overseer.Sentinel{
		Probes: sentinelProbes,
		Policy: pm.Current,
		Recorder: &overseer.ActionRecorder{
			Events: st.Events,
			Actor:  "overseer.sentinel",
			DryRun: func() bool {
				pol := pm.Current()
				return pol == nil || mills.DryRunOn(pol.Overseers.Sentinel.DryRun)
			},
		},
		Logger: logger,
	}
	if gitlabClient != nil {
		sentinel.Issues = gitlabClient
	}
	op.addAdmissionSuppressor(sentinel.SuppressAdmission)
	sentinelHarness := &overseer.Harness{
		Agent: sentinel,
		// Gated on workAdmissionOpen (kill-switch/crash-lease) but NOT on the
		// composed workAdmissionEnabled: the sentinel must keep ticking while
		// its own suppression lease is live, or it could never clear it.
		Enabled: func() bool {
			pol := pm.Current()
			return op.workAdmissionOpen() && pol != nil && pol.SentinelEnabled()
		},
		Interval: func() time.Duration {
			pol := pm.Current()
			if pol == nil {
				return time.Hour
			}
			return pol.Overseers.Sentinel.Interval()
		},
		BootTick: 2 * time.Minute,
		Logger:   logger,
	}
	op.addActivitySource("overseer_sentinel", sentinelHarness)
	op.overseers = map[string]overseerEntry{
		"groomer": {
			Harness: groomerHarness,
			Enabled: func() bool { pol := pm.Current(); return pol != nil && pol.GroomerEnabled() },
			DryRun: func() bool {
				pol := pm.Current()
				return pol == nil || mills.DryRunOn(pol.Overseers.Groomer.DryRun)
			},
		},
		"sentinel": {
			Harness: sentinelHarness,
			Enabled: func() bool { pol := pm.Current(); return pol != nil && pol.SentinelEnabled() },
			DryRun: func() bool {
				pol := pm.Current()
				return pol == nil || mills.DryRunOn(pol.Overseers.Sentinel.DryRun)
			},
			Suppression: sentinel.CurrentSuppression,
		},
	}
	logger.Info("overseer sentinel wired",
		"enabled", pm.Current().SentinelEnabled(),
		"dry_run", mills.DryRunOn(pm.Current().Overseers.Sentinel.DryRun),
		"probes", len(sentinelProbes))

	// Mill foreman (overseers slice 3). Deterministic KPI-anomaly rules over the
	// store (stuck runs, throughput collapse, escalation storm, budget burn);
	// optional LLM-composed issue bodies; guarded actions (file dedup-marked
	// issue, alert to the notify webhook, TTL admission pause hard-capped once
	// per 24h). Reuses the ALREADY-RESOLVED judge client + the notify webhook so
	// backend/config stay single-sourced; a nil judge degrades issue bodies to a
	// template and a nil/disabled webhook skips alerts — neither blocks the
	// deterministic rules.
	var foremanTriage *overseer.Triage
	if judgeClient != nil {
		foremanTriage = &overseer.Triage{Client: judgeClient, Logger: logger}
	}
	foreman := &overseer.Foreman{
		Store:  st,
		Policy: pm.Current,
		Triage: foremanTriage,
		Recorder: &overseer.ActionRecorder{
			Events: st.Events,
			Actor:  "overseer.foreman",
			DryRun: func() bool {
				pol := pm.Current()
				return pol == nil || mills.DryRunOn(pol.Overseers.Foreman.DryRun)
			},
		},
		Logger: logger,
	}
	if gitlabClient != nil {
		foreman.Issues = gitlabClient
	}
	// Reuse policy.notify.webhook_url verbatim for the alert action. Only wire a
	// non-nil, enabled hook so the foreman's `f.Webhook == nil` skip path stays
	// correct (a disabled hook would otherwise error every alert).
	if foremanNotify := buildNotifyHook(pm, st, logger); foremanNotify != nil && foremanNotify.Enabled() {
		foreman.Webhook = foremanNotify
	}
	op.addAdmissionSuppressor(foreman.SuppressAdmission)
	foremanHarness := &overseer.Harness{
		Agent: foreman,
		// Gated on workAdmissionOpen (kill-switch/crash-lease) but NOT on the
		// composed workAdmissionEnabled: like the sentinel, the foreman must keep
		// ticking while its own pause lease is live, or it could never clear it.
		Enabled: func() bool {
			pol := pm.Current()
			return op.workAdmissionOpen() && pol != nil && pol.ForemanEnabled()
		},
		Interval: func() time.Duration {
			pol := pm.Current()
			if pol == nil {
				return time.Hour
			}
			return pol.Overseers.Foreman.Interval()
		},
		BootTick: 2 * time.Minute,
		Logger:   logger,
	}
	op.addActivitySource("overseer_foreman", foremanHarness)
	op.overseers["foreman"] = overseerEntry{
		Harness: foremanHarness,
		Enabled: func() bool { pol := pm.Current(); return pol != nil && pol.ForemanEnabled() },
		DryRun: func() bool {
			pol := pm.Current()
			return pol == nil || mills.DryRunOn(pol.Overseers.Foreman.DryRun)
		},
		Suppression: foreman.CurrentSuppression,
	}
	logger.Info("overseer foreman wired",
		"enabled", pm.Current().ForemanEnabled(),
		"dry_run", mills.DryRunOn(pm.Current().Overseers.Foreman.DryRun),
		"llm_triage", foremanTriage.Available(),
		"webhook", foreman.Webhook != nil)

	// S6-min imperative workflow runtime (plan .loom/134 §S6-min). Always
	// wired into the errgroup but DEFAULT-OFF: the scheduler self-gates on
	// policy.workflows.enabled inside every tick, so it is inert until the
	// S1c canary window flips the flag. It reuses the SAME HUD spawn client
	// the DAG pipeline uses (wrapped as a worker.WorkerRunner) — zero new
	// pods/services. When the spawn client is unconfigured the scheduler
	// idles (nil runtime), keeping g.Go balanced.
	workflowSched := buildWorkflowScheduler(st, pm, hudSpawn, gitlabClient, cfg.GitLabProject, logger)
	workflowSched.Enabled = func() bool {
		pol := pm.Current()
		if pol == nil {
			return false
		}
		autonomyReady := true
		if pol.IsEnabled() {
			autonomyReady = op.capabilityReport(rootCtx).AutonomyReady
		}
		return workflowAdmissionAllowed(
			op.canaryAdmissions.Load() != 0,
			pol.IsEnabled(),
			pol.WorkflowsEnabled(),
			autonomyReady,
		)
	}
	op.addActivitySource(activitySourceWorkflow, workflowSched)

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
	g.Go(func() error { return escalationSweeper.Run(gctx) })
	g.Go(func() error { return crossRunSched.Run(gctx) })
	g.Go(func() error { return councilSched.Run(gctx) })
	g.Go(func() error { return canarySched.Run(gctx) })
	g.Go(func() error { return mergeQueueProc.Run(gctx) })
	g.Go(func() error { return workflowSched.Run(gctx) })
	g.Go(func() error { return groomerHarness.Run(gctx) })
	g.Go(func() error { return sentinelHarness.Run(gctx) })
	g.Go(func() error { return foremanHarness.Run(gctx) })

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
		gitlabImporter.Enabled = workAdmissionEnabled
		op.addActivitySource("gitlab_importer", gitlabImporter)
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

	// Plan-slice emitter (.loom/163 S2): the Plan Store → backlog bridge.
	// Polls the agent-context Plan Store for pending slices in the configured
	// namespace and emits one plan-linked BacklogItem per slice, so planning
	// feeds the autonomous loop with real work. Opt-in via
	// policy.intake.plan_slice_emitter.enabled; needs the MCP hub for reads;
	// fail-closed on an empty namespace gate (see PlanSliceEmitterPolicy).
	// Snapshot at startup like the GitLab importer — flip alongside a
	// deployment pod-checksum bump.
	if hubClient != nil && pm.Current().PlanSliceEmitterEnabled() {
		project := pm.Current().PlanSliceEmitterProject()
		if project == "" {
			project = cfg.GitLabProject
		}
		emitter := intake.NewPlanSliceEmitter(
			clients.NewPlanClient(hubClient, "loom-mills-operator"),
			st.Backlog,
			intake.PlanSliceEmitterConfig{
				Project:   project,
				Namespace: pm.Current().PlanSliceEmitterNamespace(),
				// S6 multi-repo demand: the allowlist is non-nil only when
				// cross_repo execution is enabled (two-key activation lives in
				// the accessor), so foreign demand is inert until the operator
				// opts in AND lists repos. Snapshotted at startup like the rest
				// of the emitter config — flip alongside a pod-checksum bump.
				DemandProjects: pm.Current().CrossRepoDemandProjects(),
				// Runtime-bootstrapped repos (POST /api/mills/projects/
				// bootstrap) join demand per tick, NOT per deploy: the
				// provider re-reads policy AND the registry on every call, so
				// a mint or a policy flip takes effect without a restart.
				// Fails closed on either policy key or a store error.
				DynamicDemandProjects: func(ctx context.Context) []string {
					if !pm.Current().CrossRepoBootstrapEnabled() {
						return nil
					}
					rows, err := st.Bootstrap.List(ctx)
					if err != nil {
						logger.Warn("bootstrapped-project demand read failed", "err", err)
						return nil
					}
					out := make([]string, 0, len(rows))
					for _, r := range rows {
						out = append(out, r.Project)
					}
					return out
				},
				ReadyPhase:   pm.Current().PlanSliceEmitterReadyPhase(),
				Label:        pm.Current().PlanSliceEmitterLabel(),
				Priority:     store.Priority(pm.Current().PlanSliceEmitterPriority()),
				PollInterval: pm.Current().PlanSliceEmitterPollInterval(),
				TickTimeout:  pm.Current().PlanSliceEmitterTickTimeout(),
			},
			logger,
		)
		emitter.Enabled = workAdmissionEnabled
		op.addActivitySource("plan_slice_emitter", emitter)
		// Pre-declare protected-path touches a slice's declared files carry, so
		// the post-implement path_policy gate treats a plan-declared touch (e.g.
		// **/*auth*.go) as intended instead of escalating the item; an
		// undeclared touch the implement stage introduces still fails the gate.
		// pm.Current() is read per call so a hot policy reload is honored.
		emitter.SetProtectedPathHitter(func(paths []string) []string {
			return pm.Current().ProtectedPathsHit(paths)
		})
		// Ground each emitted slice's declared files against a revision-pinned
		// origin/main tree read from the operator-local clone. A slice whose
		// EVERY declared file is absent carries the fabricated-slice signature
		// (17 psl merges landed 27 dead Go files this way, 2026-08 sweep) and
		// is stamped Fabricated for the fabricated_slice gate to escalate
		// terminally. Reads the tree OBJECT via fetch + ls-tree, never the
		// working tree, so a boot-stale checkout cannot poison the verdict
		// (the 2026-08-01 research-grounding failure mode). Fail-open: no
		// clone or a foreign demand repo emits ungrounded, prior behavior.
		if strings.TrimSpace(cfg.RepoRoot) != "" {
			grounder := &clients.RepoTreeGrounder{
				RepoRoot: cfg.RepoRoot,
				Project:  cfg.GitLabProject,
				Logger:   logger,
			}
			emitter.SetSliceGrounder(grounder.Ground)
		}
		g.Go(func() error { return emitter.Run(gctx) })
		logger.Info("plan-slice emitter enabled",
			"project", project,
			"namespace", pm.Current().PlanSliceEmitterNamespace(),
			"demand_projects", pm.Current().CrossRepoDemandProjects(),
			"ready_phase", pm.Current().PlanSliceEmitterReadyPhase(),
			"tick_timeout", pm.Current().PlanSliceEmitterTickTimeout())
	}

	// Take-up reconciler (Live Beam slice 2): trues Plan Store lifecycle state
	// to GitLab MR reality — merged MRs advance slices/plans and close their
	// emitted backlog items; closed-without-merge MRs flag orphaned slices.
	// Opt-in via policy.intake.takeup.enabled; needs the MCP hub (plan writes)
	// AND a GitLab client (MR state reads); fail-closed on an empty namespace
	// gate. Snapshot at startup like the emitter.
	if hubClient != nil && gitlabClient != nil && pm.Current().TakeupEnabled() {
		project := pm.Current().TakeupProject()
		if project == "" {
			project = cfg.GitLabProject
		}
		takeupRec := takeup.New(
			clients.NewPlanClient(hubClient, "loom-mills-operator"),
			takeupMRStateClient(gitlabClient, project),
			st.Backlog,
			takeup.Config{
				Project:       project,
				GitLabBaseURL: cfg.GitLabAPIURL,
				Namespace:     pm.Current().TakeupNamespace(),
				PollInterval:  pm.Current().TakeupPollInterval(),
				TickTimeout:   pm.Current().TakeupTickTimeout(),
			},
			logger,
		)
		takeupRec.Enabled = workAdmissionEnabled
		// J2 auto-harvest (docs/FACTORY_MODEL.md): merged stamped plans feed
		// the pattern taste gate. Same hub the plan writes ride; nil-safe off.
		takeupRec.Patterns = clients.NewPatternClient(hubClient)
		op.addActivitySource("takeup", takeupRec)
		g.Go(func() error { return takeupRec.Run(gctx) })
		logger.Info("take-up reconciler enabled",
			"project", project,
			"namespace", pm.Current().TakeupNamespace(),
			"poll_interval", pm.Current().TakeupPollInterval())
	}

	// Stale-canary GC (plan 43 follow-up to Slice 3d). Sweeps escalated
	// canary backlog items older than StaleAfterHours so they stop
	// blocking new mills-canary enqueues. Opt-in via
	// policy.intake.canary_gc.enabled.
	if canaryGC := buildCanaryGC(pm, st, logger); canaryGC != nil {
		canaryGC.Enabled = workAdmissionEnabled
		op.addActivitySource("canary_gc", canaryGC)
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
	op.markActivitySourcesReady()

	err = g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("loom-mills-operator stopped")
	return nil
}

func takeupMRStateClient(gl *clients.GitLabClient, project string) takeup.MRStater {
	if gl == nil {
		return nil
	}
	return gl.ForProject(project)
}

// workflowAdmissionAllowed keeps ordinary dynamic workflows behind the same
// live capability gate as DAG runs while preserving the deliberately isolated
// S1c crash-canary window (global admission off, workflows on). The canary
// creation transaction itself fences the scheduler via canaryAdmissions.
func workflowAdmissionAllowed(canaryAdmissionActive, policyEnabled, workflowsEnabled, autonomyReady bool) bool {
	if canaryAdmissionActive || !workflowsEnabled {
		return false
	}
	if !policyEnabled {
		return true
	}
	return autonomyReady
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

// buildCouncilEditor selects the council editor. When the policy's editor
// backend is "openai"/"openai-responses" AND an OpenAI key is configured
// (OPENAI_API_KEY / LOOM_RESPONSES_API_KEY), it drives a frontier model (e.g.
// gpt-5.4) via the Responses API for higher-quality roadmap decomposition
// (.loom/163 S3). Otherwise — or if the OpenAI client can't be built — it
// falls back to the local flexinfer editor, so a missing key never breaks
// council runs.
// openAICouncilEditorTimeout bounds one OpenAI Responses editor call. gpt-5.4
// is a reasoning model and a full council synthesis (3 docs + a decomposition
// block) routinely exceeds the 90s openairesponses default — the first prod
// run on gpt-5.4 (2026-06-28T12:00) timed out with that default. 5 min gives
// the reasoning + large output room; the FallbackCouncilEditor still catches
// a genuine stall so a council run never hard-fails on it.
const openAICouncilEditorTimeout = 5 * time.Minute

func buildCouncilEditor(policy *mills.Policy, flexClient *clients.FlexInferClient, repoRoot string, mem council.MemoryLoader, logger *slog.Logger) council.Editor {
	return buildEditorForAgent(policy.Council.Ensemble.Editor, policy.Council.Ensemble.EditorFallbackModel, flexClient, repoRoot, mem, logger)
}

// buildEditorForAgent builds a council.Editor for an arbitrary {name, model,
// backend} agent — the council ensemble editor (buildCouncilEditor) OR a
// Spinning Room frame (Live Beam slice 3). An OpenAI/Responses backend with a
// key configured drives the frontier model with a flexinfer fallback; any other
// backend (or a missing key) uses the local flexinfer editor. Returns nil only
// when no backend is available at all (flexClient nil AND no usable OpenAI
// client) so a caller can fail loudly rather than spin on a dead editor.
//
// mem is the council lane's durable cross-run memory (nil = none). Only the
// council ensemble editor passes it: a Spinning Room frame is a different lane
// that never records into that journal, so rendering it there would put another
// lane's history above the frame's own boundary for no gain.
func buildEditorForAgent(agent mills.CouncilAgent, localFallbackModel string, flexClient *clients.FlexInferClient, repoRoot string, mem council.MemoryLoader, logger *slog.Logger) council.Editor {
	model := agent.Model
	backend := strings.ToLower(strings.TrimSpace(agent.Backend))
	// A remote frontier model id is never deployable on the local flexinfer
	// tier, so the per-run fallback / no-key degrade editor must not inherit
	// it (COUNCIL-2026-08-03-060011/-120011 hard-failed on a guaranteed 404
	// when a DNS blip pushed the Anthropic editor onto the fallback). The
	// explicit policy pin wins; empty resolves to the client's weaver chain.
	flexModel := model
	switch backend {
	case "openai", "openai-responses", "anthropic", "claude":
		flexModel = strings.TrimSpace(localFallbackModel)
	}
	var flex *clients.FlexInferCouncilEditor
	if flexClient != nil {
		flex = &clients.FlexInferCouncilEditor{Client: flexClient, Backend: "flexinfer", Model: flexModel, RepoRoot: repoRoot, Memory: mem}
	}
	if backend == "openai" || backend == "openai-responses" {
		if apiKey := openairesponses.APIKeyFromEnv(); apiKey != "" {
			apiClient, err := openairesponses.NewAPIClient(openairesponses.APIClientConfig{
				APIKey:  apiKey,
				BaseURL: openairesponses.BaseURLFromEnv(),
				Timeout: openAICouncilEditorTimeout,
				// Opt this client into per-completion usage logging. The
				// Responses API is the one backend that has always reported
				// cached_tokens, so it is the cleanest read on whether the
				// council's stable-first prompt is actually earning a warm
				// prefix.
				Logger:    logger,
				Component: clients.ComponentCouncilEditor,
			})
			if err == nil {
				logger.Info("council editor wired to OpenAI Responses",
					"model", model, "timeout", openAICouncilEditorTimeout)
				primary := &clients.OpenAIResponsesCouncilEditor{Client: apiClient, Model: model, RepoRoot: repoRoot, Memory: mem}
				if flex == nil {
					// No local fallback available; drive OpenAI directly.
					return primary
				}
				// Run-time resilience: if the OpenAI call errors/times out, fall
				// back to the local flexinfer editor for THAT run so a transient
				// API hiccup never hard-fails a scheduled council run.
				return &clients.FallbackCouncilEditor{
					Primary:  primary,
					Fallback: flex,
					Logger:   logger,
				}
			}
			logger.Warn("openai council editor requested but client init failed; falling back to flexinfer", "err", err)
		} else {
			logger.Warn("openai council editor requested but no OPENAI_API_KEY/LOOM_RESPONSES_API_KEY; falling back to flexinfer")
		}
	}
	if backend == "anthropic" || backend == "claude" {
		if apiKey := clients.AnthropicAPIKeyFromEnv(); apiKey != "" {
			anthropicClient, err := clients.NewAnthropicClient(clients.AnthropicClientConfig{
				APIKey:  apiKey,
				BaseURL: clients.AnthropicBaseURLFromEnv(),
				Timeout: openAICouncilEditorTimeout,
			})
			if err == nil {
				logger.Info("council editor wired to Anthropic Messages API",
					"model", model, "timeout", openAICouncilEditorTimeout)
				primary := &clients.AnthropicCouncilEditor{Client: anthropicClient, Model: model, RepoRoot: repoRoot, Memory: mem}
				if flex == nil {
					// No local fallback available; drive Anthropic directly.
					return primary
				}
				// Same run-time resilience as the OpenAI path: a transient
				// Anthropic error/timeout/refusal falls back to flexinfer for
				// THAT run rather than hard-failing.
				return &clients.FallbackCouncilEditor{
					Primary:  primary,
					Fallback: flex,
					Logger:   logger,
				}
			}
			logger.Warn("anthropic council editor requested but client init failed; falling back to flexinfer", "err", err)
		} else {
			logger.Warn("anthropic council editor requested but no ANTHROPIC_API_KEY/LOOM_ANTHROPIC_API_KEY; falling back to flexinfer")
		}
	}
	if flex == nil {
		return nil
	}
	return flex
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
	litellmClient *clients.FlexInferClient,
	judgeClient *clients.FlexInferClient,
	councilJudgeModel string,
	stages runner.StageBudgets,
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
		litellmLenses := 0
		for _, l := range lenses {
			// A "litellm" lens routes through the cluster LiteLLM gateway
			// (OpenRouter-fronted frontier models, e.g. or/kimi-k3). Same
			// wire protocol, different base URL + key — only the client
			// binding differs; the lens model rides through unchanged. A
			// litellm lens with no gateway configured gets the fake reviewer
			// so the misconfiguration is visible in council notes instead of
			// silently 404ing against the FlexInfer proxy.
			if l.Backend == "litellm" {
				if litellmClient == nil {
					reviewers[l.Name] = &council.FakeReviewer{
						Notes:   "litellm lens configured but LITELLM_PROXY_URL unset; reviewer skipped",
						CostUSD: 0,
					}
					logger.Warn("council lens backend litellm without LITELLM_PROXY_URL; using fake reviewer", "lens", l.Name, "model", l.Model)
					continue
				}
				litellmLenses++
				reviewers[l.Name] = &clients.FlexInferCouncilReviewer{
					Client: litellmClient,
				}
				continue
			}
			reviewers[l.Name] = &clients.FlexInferCouncilReviewer{
				Client: flexClient,
			}
		}
		editor = buildCouncilEditor(policy, flexClient, repoRoot, st.CouncilMemory, logger)
		// The council contradiction judge shares the rubric-judge backend
		// selection (MILLS_JUDGE_BACKEND). judgeClient is flexClient by
		// default; when the judge is on litellm it's the gateway client and
		// councilJudgeModel pins the explicit gateway-routable model so the
		// judge dials or/kimi-k3 (not the flexinfer weaver model the empty-
		// model resolution would otherwise pick). A nil judgeClient (defensive)
		// falls back to flexClient with the legacy empty-model behavior.
		judgeBackendClient := judgeClient
		councilJudgeModelID := councilJudgeModel
		if judgeBackendClient == nil {
			judgeBackendClient = flexClient
			councilJudgeModelID = ""
		}
		judgeBackendLabel := "flexinfer"
		if judgeBackendClient != flexClient {
			judgeBackendLabel = "litellm"
		}
		judge = &eval.Judge{Criteria: eval.DefaultRubric(&clients.FlexInferEvalJudge{
			Client: judgeBackendClient, Model: councilJudgeModelID, Backend: judgeBackendLabel,
		})}
		logger.Info("council participants wired to FlexInfer-backed reviewers/editor",
			"litellm_lenses", litellmLenses,
			"judge_backend", judgeBackendLabel,
			"judge_model", councilJudgeModelID)
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
	// Mill Staff audit: mutator actions land under actor "council.mutator"
	// alongside the overseer.* trail. DryRun=false is deliberate — the
	// council's artifact-dryrun path never reaches Apply, so any mutation
	// the recorder sees is real.
	mutator.Recorder = &guard.ActionRecorder{
		Events: st.Events,
		Actor:  "council.mutator",
		DryRun: func() bool { return false },
	}

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
		// Per-stage + overall bounds. Applied inside Execute, so the cron
		// scheduler's uncapped root context inherits the overall cap too.
		StageBudgets: stages,
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

// buildLiteLLMClient returns an OpenAI-compatible client for the cluster
// LiteLLM gateway when LITELLM_PROXY_URL is set, or nil otherwise. LiteLLM
// fronts remote providers (OpenRouter → Moonshot etc.), so this is how council
// reviewer lenses with backend "litellm" reach frontier models like or/kimi-k3
// without the FlexInfer proxy federating them. Reuses the FlexInfer client
// (both speak /v1/chat/completions with bearer auth); the judge/weaver model
// resolution the constructor performs is inert here — reviewer calls always
// pass an explicit lens model.
func buildLiteLLMClient(cfg Config, logger *slog.Logger) *clients.FlexInferClient {
	if cfg.LiteLLMProxyURL == "" {
		return nil
	}
	c, err := clients.NewFlexInferClient(clients.FlexInferConfig{
		ProxyURL: cfg.LiteLLMProxyURL,
		Token:    cfg.LiteLLMToken,
		Timeout:  cfg.FlexInferTimeout,
	})
	if err != nil {
		logger.Error("litellm client init failed; litellm-backed council lenses will use the fake reviewer", "error", err)
		return nil
	}
	return c
}

// buildLiteLLMModelClient builds a LiteLLM-gateway-backed FlexInferClient that
// carries EXPLICIT, gateway-routable judge/weaver model ids (e.g. or/kimi-k3)
// and suppresses the aimodels-registry fallback resolution
// (DisableRegistryFallbacks) so a frontier outage degrades backend-locally —
// it walks only the env-listed litellm ids (FLEXINFER_*_MODEL_FALLBACKS) or
// nothing, never a FlexInfer-proxy id the gateway can't route. Same
// /v1/chat/completions + bearer protocol as the FlexInfer client, so all judge
// hardening (truncation recovery, echo-stripping, max-tokens env, length-retry)
// and provider-cost passthrough (usage.cost) ride through unchanged. Returns
// nil on init error (callers degrade to the FlexInfer client).
func buildLiteLLMModelClient(cfg Config, judgeModel, weaverModel string, logger *slog.Logger) *clients.FlexInferClient {
	c, err := clients.NewFlexInferClient(clients.FlexInferConfig{
		ProxyURL:                 cfg.LiteLLMProxyURL,
		Token:                    cfg.LiteLLMToken,
		JudgeModel:               judgeModel,
		WeaverModel:              weaverModel,
		Timeout:                  cfg.FlexInferTimeout,
		DisableRegistryFallbacks: true,
	})
	if err != nil {
		logger.Error("litellm model client init failed", "error", err,
			"judge_model", judgeModel, "weaver_model", weaverModel)
		return nil
	}
	return c
}

// resolveMillsJudgeClient selects the LLM client backing the rubric judge (the
// LLM-judged gates) and the council contradiction judge, per
// MILLS_JUDGE_BACKEND. Default (unset / "flexinfer") returns the FlexInfer
// client unchanged with an empty council-judge model — zero behavior change.
//
// "litellm" binds the cluster LiteLLM gateway so the judge runs on a frontier
// OpenRouter model (FLEXINFER_JUDGE_MODEL, e.g. or/kimi-k3). Misconfiguration —
// no LITELLM_PROXY_URL, no explicit judge model, or a client init error — fails
// loud at startup wiring and DEGRADES to the FlexInfer judge instead of 404ing
// every gate call (mirrors the council-lens visible-misconfiguration contract).
// The second return is the explicit model id the council contradiction judge
// must dial (empty for the flexinfer default so its legacy weaver-model
// resolution is preserved; the gateway model when on litellm).
func resolveMillsJudgeClient(cfg Config, flexClient *clients.FlexInferClient, logger *slog.Logger) (*clients.FlexInferClient, string) {
	if !strings.EqualFold(strings.TrimSpace(cfg.JudgeBackend), "litellm") {
		return flexClient, ""
	}
	if strings.TrimSpace(cfg.LiteLLMProxyURL) == "" {
		logger.Error("MILLS_JUDGE_BACKEND=litellm but LITELLM_PROXY_URL unset; falling back to FlexInfer judge")
		return flexClient, ""
	}
	if strings.TrimSpace(cfg.FlexInferJudgeModel) == "" {
		logger.Error("MILLS_JUDGE_BACKEND=litellm but FLEXINFER_JUDGE_MODEL unset; a litellm judge needs an explicit gateway-routable model (e.g. or/kimi-k3); falling back to FlexInfer judge")
		return flexClient, ""
	}
	c := buildLiteLLMModelClient(cfg, cfg.FlexInferJudgeModel, "", logger)
	if c == nil {
		logger.Error("litellm judge client init failed; falling back to FlexInfer judge")
		return flexClient, ""
	}
	logger.Info("mills judge backend: litellm",
		"model", cfg.FlexInferJudgeModel, "proxy", cfg.LiteLLMProxyURL,
		"fallbacks", "FLEXINFER_JUDGE_MODEL_FALLBACKS (backend-local: litellm-routable ids only)")
	return c, cfg.FlexInferJudgeModel
}

// resolveMillsWeaverClient selects the LLM client backing the research/weaver
// stage, per MILLS_WEAVER_BACKEND (independent of the judge backend). Same
// contract as resolveMillsJudgeClient: default keeps the FlexInfer client;
// "litellm" binds the gateway on FLEXINFER_WEAVER_MODEL and degrades loud to
// FlexInfer on any misconfiguration.
func resolveMillsWeaverClient(cfg Config, flexClient *clients.FlexInferClient, logger *slog.Logger) *clients.FlexInferClient {
	if !strings.EqualFold(strings.TrimSpace(cfg.WeaverBackend), "litellm") {
		return flexClient
	}
	if strings.TrimSpace(cfg.LiteLLMProxyURL) == "" {
		logger.Error("MILLS_WEAVER_BACKEND=litellm but LITELLM_PROXY_URL unset; falling back to FlexInfer weaver")
		return flexClient
	}
	if strings.TrimSpace(cfg.FlexInferWeaverModel) == "" {
		logger.Error("MILLS_WEAVER_BACKEND=litellm but FLEXINFER_WEAVER_MODEL unset; a litellm weaver needs an explicit gateway-routable model; falling back to FlexInfer weaver")
		return flexClient
	}
	c := buildLiteLLMModelClient(cfg, "", cfg.FlexInferWeaverModel, logger)
	if c == nil {
		logger.Error("litellm weaver client init failed; falling back to FlexInfer weaver")
		return flexClient
	}
	logger.Info("mills weaver backend: litellm",
		"model", cfg.FlexInferWeaverModel, "proxy", cfg.LiteLLMProxyURL,
		"fallbacks", "FLEXINFER_WEAVER_MODEL_FALLBACKS (backend-local: litellm-routable ids only)")
	return c
}

// weaverBackendLabel reports the effective research/weaver backend for logging.
// "litellm" only when the backend is selected AND its gateway + model are
// present (the conditions resolveMillsWeaverClient needs to bind the gateway);
// otherwise "flexinfer" (default or degraded-to-flexinfer).
func weaverBackendLabel(cfg Config) string {
	if strings.EqualFold(strings.TrimSpace(cfg.WeaverBackend), "litellm") &&
		strings.TrimSpace(cfg.LiteLLMProxyURL) != "" &&
		strings.TrimSpace(cfg.FlexInferWeaverModel) != "" {
		return "litellm"
	}
	return "flexinfer"
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
		// Zero leaves the client's own defaults (5m / 10m) in place.
		HeadSHADeadline:        cfg.GitLabHeadSHADeadline,
		BranchPipelineDeadline: cfg.GitLabBranchPipelineDeadline,
	})
	if err != nil {
		logger.Error("gitlab client init failed; mr/ci/merge/cleanup stages will stub", "error", err)
		return nil
	}
	logger.Info("gitlab client ready",
		"head_sha_deadline", c.HeadSHADeadline(),
		"branch_pipeline_deadline", c.BranchPipelineDeadline())
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

func flakyCIJobsFor(pm *mills.PolicyManager) func() []string {
	return func() []string {
		jobs := pm.Current().Pipeline.CIWatch.FlakyJobs
		if len(jobs) == 0 {
			return []string{"test:reliability", "test:unit"}
		}
		return append([]string(nil), jobs...)
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

// agentForStage returns a closure that resolves the ITEM-LESS effective spawn
// agent for a stage on every invocation, so a hot-reloaded
// `pipeline.stage_agents` change takes effect immediately.
//
// This is the /wiring snapshot's view, NOT the dispatch path: dispatch goes
// through spawnRouteFor, which layers the per-item agent/* label and
// pipeline.agent_routing rules above these rungs. Keep the two in step — this
// closure's answer is what an operator sees when no per-item rule claims the
// work. Precedence (highest first):
//
//  1. LOOM_MILLS_SPAWN_AGENT env — the global break-glass override. Read once
//     at startup (pod env vars don't hot-reload) and, when set, wins for EVERY
//     stage regardless of policy — the auth-outage failover knob.
//  2. policy `pipeline.stage_agents[stage]` — the per-stage override.
//  3. mills.AgentDefault ("claude-code") — the built-in default.
//
// Never returns empty (it always resolves to at least AgentDefault), so the
// reported baseline is always a concrete harness.
func agentForStage(pm *mills.PolicyManager) func(stage string) string {
	envOverride := strings.TrimSpace(os.Getenv("LOOM_MILLS_SPAWN_AGENT"))
	return func(stage string) string {
		if envOverride != "" {
			return envOverride
		}
		if a := pm.Current().AgentForStage(stage); a != "" {
			return a
		}
		return mills.AgentDefault
	}
}

// modelForStage returns a closure that resolves the ITEM-LESS effective
// vendor-native LLM model for a stage on every invocation, so a hot-reloaded
// `pipeline.stage_models` change takes effect immediately. Like agentForStage
// this feeds the /wiring snapshot, not the dispatch path — spawnRouteFor owns
// the effective model, and a routing rule may pin its own or drop this one when
// it re-targets the vendor. Precedence (highest first):
//
//  1. LOOM_MILLS_SPAWN_MODEL env — the global break-glass override. Read once
//     at startup (pod env vars don't hot-reload) and, when set, wins for EVERY
//     stage regardless of policy — the model failover knob that mirrors
//     LOOM_MILLS_SPAWN_AGENT.
//  2. policy `pipeline.stage_models[stage]` — the per-stage override.
//  3. empty string — "no per-spawn override". The SpawnWorker leaves
//     SpawnRequest.AgentModel empty and the HUD spawn server applies its own
//     vendor default (SPAWN_CODEX_MODEL env / resolveCodexModel for codex).
//
// Unlike agentForStage this closure DOES return empty on the fallback path:
// there is no operator-side default model to fall back to (each vendor's CLI
// owns its default). The empty return is the "keep vendor default" signal.
func modelForStage(pm *mills.PolicyManager) func(stage string) string {
	envOverride := strings.TrimSpace(os.Getenv("LOOM_MILLS_SPAWN_MODEL"))
	return func(stage string) string {
		if envOverride != "" {
			return envOverride
		}
		return pm.Current().ModelForStage(stage)
	}
}

// agentRoutedEventKind is the dispatch-context event recording WHY a stage ran
// on the harness it ran on. It lives in the runner's `pipeline.stage.*`
// namespace because it is emitted once per stage dispatch, alongside
// pipeline.stage.start.
const agentRoutedEventKind = "pipeline.stage.agent_routed"

// spawnRouteFor returns the closure the SpawnWorker consults to resolve the
// harness + vendor model for one stage dispatch of one backlog item. This is
// the single source of truth for effective routing; resolution runs on every
// invocation so a hot-reloaded pipeline.agent_routing / stage_agents /
// stage_models change takes effect on the next stage attempt.
//
// Precedence (highest first):
//
//  1. LOOM_MILLS_SPAWN_AGENT / LOOM_MILLS_SPAWN_MODEL env — the global
//     break-glass overrides. Read once at startup (pod env vars don't
//     hot-reload) and, when set, win for EVERY item and stage. They stay the
//     auth-outage / vendor-outage failover knobs and routing must never be
//     able to route around them.
//  2. the item's `agent/<id>` label — the explicit per-item override.
//  3. the first matching policy `pipeline.agent_routing` rule.
//  4. policy `pipeline.stage_agents[stage]` (model: `stage_models[stage]`).
//  5. mills.AgentDefault ("claude-code").
//
// Steps 2-5 are Policy.ResolveAgentRoute; this closure adds only the env layer,
// the dispatch event, and the malformed-label warning. The returned Agent is
// never empty, so the SpawnWorker's static Model stays a nil-closure fallback.
//
// The env agent rung carries the same model-follows-agent rule as
// ResolveAgentRoute: when the break-glass re-targets the VENDOR it drops the
// resolved model, because a `stage_models` / route pin names a vendor-native id
// that the substituted CLI cannot honour (`codex exec --model claude-opus-5`
// fails every implement stage — during an outage failover, the worst possible
// moment). An env agent that matches the already-resolved agent leaves the pin
// untouched, so a deployment whose break-glass names the harness its models were
// authored for keeps byte-identical behavior. Set LOOM_MILLS_SPAWN_MODEL
// alongside the agent to pin a model explicitly across a vendor switch.
//
// The event append is best-effort: an events-table failure must never block a
// dispatch, so it is logged and swallowed.
func spawnRouteFor(pm *mills.PolicyManager, st *store.Store, logger *slog.Logger) func(context.Context, string, *store.BacklogItem) mills.AgentDecision {
	resolve := resolveSpawnRoute(pm)
	return func(ctx context.Context, stage string, item *store.BacklogItem) mills.AgentDecision {
		d := resolve(stage, item)
		itemID := ""
		if item != nil {
			itemID = item.ID
		}
		if len(d.IgnoredLabels) > 0 && logger != nil {
			logger.Warn("agent routing: ignoring unrecognized agent/* labels",
				"item", itemID, "stage", stage, "labels", d.IgnoredLabels,
				"allowed", "claude-code, codex, gemini")
		}
		recordAgentRoute(ctx, st, logger, itemID, stage, d)
		return d
	}
}

// resolveSpawnRoute is the side-effect-free core of spawnRouteFor: the full
// precedence chain with no dispatch event and no logging. Run provenance reads
// routing through it at run start, where appending a
// pipeline.stage.agent_routed row would attribute a dispatch that has not
// happened. The env break-glass is read once here, as before — pod env does
// not hot-reload.
func resolveSpawnRoute(pm *mills.PolicyManager) func(string, *store.BacklogItem) mills.AgentDecision {
	envAgent := strings.TrimSpace(os.Getenv("LOOM_MILLS_SPAWN_AGENT"))
	envModel := strings.TrimSpace(os.Getenv("LOOM_MILLS_SPAWN_MODEL"))
	return func(stage string, item *store.BacklogItem) mills.AgentDecision {
		d := pm.Current().ResolveAgentRoute(stage, item)
		if envAgent != "" {
			if envAgent != d.Agent {
				d.Model = ""
			}
			d.Agent = envAgent
			d.DecidedBy = mills.AgentDecidedByEnv
		}
		if envModel != "" {
			d.Model = envModel
		}
		return d
	}
}

// recordAgentRoute appends the dispatch-context routing event so an operator can
// answer "why did this item go to codex?" straight off the event stream.
//
// Only ROUTED dispatches are recorded — those an agent/* label or an
// agent_routing rule claimed. The stage_agents / default rungs are the
// pre-routing behavior and writing an event for them would put a per-dispatch
// row into the (unpruned) events table of every deployment that never opted in.
// An empty itemID means the caller asked for the item-less baseline (the startup
// wiring log), not a real dispatch, so there is nothing to attribute.
func recordAgentRoute(ctx context.Context, st *store.Store, logger *slog.Logger, itemID, stage string, d mills.AgentDecision) {
	if st == nil || st.Events == nil || itemID == "" || !mills.AgentRouted(d) {
		return
	}
	err := st.Events.Append(ctx, &store.Event{
		Actor:       "pipeline",
		Kind:        agentRoutedEventKind,
		SubjectKind: "backlog_item",
		SubjectID:   itemID,
		Payload: map[string]any{
			"item":  itemID,
			"stage": stage,
			"agent": d.Agent,
			"model": d.Model,
			// decided_by ∈ {label, rule:<idx>} here; the env / stage_agents /
			// default rungs do not reach this append.
			"decided_by": d.DecidedBy,
			// outcome matches the convention Runner.event stamps on every
			// other pipeline.* event, so consumers can filter uniformly.
			"outcome": "ok",
		},
	})
	if err != nil && logger != nil {
		logger.Warn("agent routing: append dispatch event failed",
			"error", err, "item", itemID, "stage", stage)
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
func buildDispatcher(cfg Config, weaver *clients.FlexInferClient, hub *clients.MCPHubClient, st *store.Store, logger *slog.Logger, autoMerge func(pipeline.JobContext) bool, flakyJobs func() []string, substrateFor func(stage string) string, routeFor func(context.Context, string, *store.BacklogItem) mills.AgentDecision, spawn *clients.HUDSpawnClient, mergeQueue pipeline.MergeQueue, mergeQueueEnabled func() bool) (pipeline.WorkerDispatcher, map[string]bool) {
	gitlab := buildGitLabClient(cfg, logger)

	// Per-item cross-stage memory. Nil (store-less test wiring) or a disabled
	// LOOM_MILLS_ITEM_JOURNAL both yield the stateless prompts.
	var itemMemory *store.ItemMemoryDAO
	if st != nil {
		itemMemory = st.ItemMemory
	}
	if itemMemory != nil && pipeline.ItemJournalEnabled() {
		logger.Info("per-item memory journal enabled; stage prompts carry prior stage outcomes",
			"env", pipeline.ItemJournalEnv)
	}

	routes := map[string]pipeline.Worker{}
	realStages := newCapabilityWiring(cfg).DispatcherRealStages
	// weaver is the research-stage LLM client, resolved via MILLS_WEAVER_
	// BACKEND (FlexInfer proxy by default, LiteLLM gateway when selected).
	if weaver != nil {
		wc := clients.NewWeaverClient(weaver)
		// RepoRoot grounds the output guard: any path the research
		// model cites that doesn't exist in the checkout is treated as
		// a hallucination and sanitized/withheld before it reaches the
		// implement worker (PIPE-MILLS-2026-06-29-001).
		wc.RepoRoot = cfg.RepoRoot
		// Mode is read at construction time from MILLS_RESEARCH_VIA_
		// WEAVER. When non-default, attach the delegator + recorder
		// so shadow/on can actually do something. Mode==off ignores
		// both, so wiring them unconditionally would be wasteful.
		if wc.Mode != clients.ResearchModeOff {
			attachWeaverDelegation(wc, cfg, st, logger)
		}
		routes["research"] = &pipeline.WeaverWorker{Client: wc, PromptFor: researchPromptFor(cfg.RepoRoot, itemMemory)}
		realStages["research"] = true
		logger.Info("research stage wired to WeaverClient",
			"weaver_backend", weaverBackendLabel(cfg),
			"weaver_model", weaver.WeaverModel(),
			"weaver_max_tokens", wc.MaxTokens,
			"research_mode", string(wc.Mode), "grounding_repo_root", cfg.RepoRoot)
	} else {
		logger.Warn("research stage stub: NoOpDispatcher (set FLEXINFER_PROXY_URL, or MILLS_WEAVER_BACKEND=litellm with LITELLM_PROXY_URL + FLEXINFER_WEAVER_MODEL)")
	}
	if gitlab != nil {
		gw := &pipeline.GitLabWorker{
			Client:       gitlab,
			AutoMergeFor: autoMerge,
			BranchPusher: clients.NewGitBranchPusher(),
			Logger:       logger,
			FlakyJobs:    flakyJobs,
			// Per-item cross-repo routing: scope mr/ci_watch/merge/cleanup to
			// an item's TargetProject. ForProject shares the home client's token
			// (which, for cross-repo, must be the services group token — a
			// deployment concern gated with cross_repo.enabled). Returns the home
			// client for an empty/home target, so this is inert until an item
			// carries a non-home TargetProject and the reconciler gate opens.
			ForProject: func(project string) pipeline.GitLabClient {
				return gitlab.ForProject(project)
			},
			// Serial merge queue: nil gateway or a false policy fence keeps
			// the merge stage on the direct path (pre-queue behaviour).
			MergeQueue:        mergeQueue,
			MergeQueueEnabled: mergeQueueEnabled,
		}
		// Close the plan↔MR link at creation time: a plan-linked item's slice
		// gets its mr_ref stamped by the mr stage instead of depending on the
		// spawned agent to remember agent_plan_slice_update. Without it the
		// take-up reconciler has nothing to poll, the plan never walks to
		// merged, and the J2 pattern harvest never fires. Needs the MCP hub
		// (plan writes ride it); best-effort, so an unavailable hub only
		// costs the linkage, never the MR.
		if hub != nil {
			gw.PlanMRRecorder = takeup.NewMRRefRecorder(
				clients.NewPlanClient(hub, "loom-mills-operator"), logger)
			logger.Info("mr stage records mr_ref onto plan slices (take-up write path)")
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
		// PromptFor closures select the prompt body; the RouteFor closure
		// (spawnRouteFor) selects the harness + vendor model per stage AND per
		// item. Production deployments register richer prompt builders here
		// once spec doc loaders ship.
		//
		// Effective-routing precedence lives in spawnRouteFor: the
		// LOOM_MILLS_SPAWN_AGENT env break-glass wins for everything, else the
		// item's agent/* label, else a pipeline.agent_routing rule, else policy
		// pipeline.stage_agents[stage], else AgentDefault. spawnAgent below
		// mirrors only the env/default tail for the worker's static Model (the
		// nil-RouteFor fallback); the wired RouteFor overrides Model per
		// dispatch, which is what lets claude-code and codex implementers run
		// simultaneously across the queue.
		spawnAgent := strings.TrimSpace(os.Getenv("LOOM_MILLS_SPAWN_AGENT"))
		if spawnAgent == "" {
			spawnAgent = mills.AgentDefault
		}
		// BaseBranch + RepoRoot feed the spawn client's post-terminal git
		// capture (clients.attachGitContext): fetch origin/<branch> into
		// the operator-local clone at cfg.RepoRoot (bootstrapped by
		// ensureRepoRoot; the runtime image ships git) and record the
		// cumulative branch-vs-base diff + commit messages for gate input.
		// Before this wiring both fields were empty on the standard path,
		// so the capture silently no-op'd and gates judged per-attempt
		// spawn telemetry only — the attempt-1-errored-after-push
		// escalation shape (issue #224) that runner.carryForwardDiff
		// cannot cover. "main" matches the HUD spawn server's own
		// base_branch default (internal/hud/spawn.go), so the spawn wire
		// behavior is unchanged.
		routes["plan_slice"] = &pipeline.SpawnWorker{
			Client:       spawn,
			Model:        spawnAgent,
			Project:      project,
			Namespace:    "loom-mills",
			BaseBranch:   "main",
			RepoRoot:     cfg.RepoRoot,
			PromptFor:    planSlicePromptFor(itemMemory),
			SubstrateFor: substrateFor,
			RouteFor:     routeFor,
		}
		routes["implement"] = &pipeline.SpawnWorker{
			Client:        spawn,
			Model:         spawnAgent,
			Project:       project,
			Namespace:     "loom-mills",
			BaseBranch:    "main",
			RepoRoot:      cfg.RepoRoot,
			PromptFor:     implementPromptFor(itemMemory),
			NeedsWorktree: true,
			SubstrateFor:  substrateFor,
			RouteFor:      routeFor,
		}
		routes["pr_self_review"] = &pipeline.SpawnWorker{
			Client:       spawn,
			Model:        spawnAgent,
			Project:      project,
			Namespace:    "loom-mills",
			BaseBranch:   "main",
			RepoRoot:     cfg.RepoRoot,
			PromptFor:    prSelfReviewPromptFor(itemMemory),
			SubstrateFor: substrateFor,
			RouteFor:     routeFor,
		}
		realStages["plan_slice"] = true
		realStages["implement"] = true
		realStages["pr_self_review"] = true
		// Log the per-stage EFFECTIVE baseline, not just the env/default
		// resolution — a policy stage_agents override (e.g. pr_self_review on
		// a cheaper agent) would otherwise be invisible at wiring time and
		// mislead operator triage. This is the ITEM-LESS baseline: a nil item
		// resolves env > stage_agents > default and emits no routing event, so
		// per-item agent/* labels and agent_routing rules can still move an
		// individual dispatch off these values. The values reflect the policy
		// loaded at startup; hot reloads change them later.
		eff := func(stage string) mills.AgentDecision {
			if routeFor == nil {
				return mills.AgentDecision{Agent: spawnAgent}
			}
			return routeFor(context.Background(), stage, nil)
		}
		planRoute, implRoute, reviewRoute := eff("plan_slice"), eff("implement"), eff("pr_self_review")
		logger.Info("plan_slice/implement/pr_self_review stages wired to HUD spawn API",
			"agent_plan_slice", planRoute.Agent,
			"agent_implement", implRoute.Agent,
			"agent_pr_self_review", reviewRoute.Agent,
			// Empty model = "vendor default" (SPAWN_CODEX_MODEL / resolveCodexModel
			// for codex); a policy stage_models entry surfaces here at startup.
			"model_plan_slice", planRoute.Model,
			"model_implement", implRoute.Model,
			"model_pr_self_review", reviewRoute.Model,
			"git_capture_root", cfg.RepoRoot)
	} else {
		logger.Warn("plan_slice/implement/pr_self_review stages stub: NoOpDispatcher (LOOM_HUD_URL+LOOM_HUD_TOKEN unset)")
	}

	return newOperatorDispatcher(routes), realStages
}

// stagePromptTemplates is the per-stage prompt body each dispatch renders the
// item into. Package-level so run provenance can hash the exact template bytes
// a run started under; the empty-stage fallback lives in
// stagePromptWithPreamble because it takes a different format arity.
var stagePromptTemplates = map[string]string{
	"plan_slice":     "Plan implementation slices for backlog item %s (%q). Output a numbered list of independent slices with files touched and test strategy per slice.",
	"research":       "Research backlog item %s (%q). Summarize relevant code paths, prior decisions, test constraints, and rollout risks for the implementation worker.",
	"implement":      "Implement backlog item %s (%q). Write code + tests in the allocated worktree. Commit with conventional commit format. As your FINAL step, push the branch with `git push -u origin HEAD` so the downstream `mr` stage can open a real merge request against your commits — without the push, GitLab sees an empty branch and the pipeline hangs.",
	"pr_self_review": "Review your own diff for backlog item %s (%q) before opening a merge request. Score on the pr_self_review_v1 rubric and fix anything below 0.8.",
}

// stagePromptFor returns a default per-stage prompt builder. Production
// deployments override this with spec-doc-aware closures; the default
// gives each stage a terse but pointed prompt that the runner's
// JobContext fills with item title + slice scope.
//
// mem, when non-nil AND LOOM_MILLS_ITEM_JOURNAL is on, prepends the item's
// cross-stage memory journal. Pass nil to get the stateless prompt.
func stagePromptFor(stage string, mem *store.ItemMemoryDAO) func(jc pipeline.JobContext) string {
	return stagePromptWithPreamble(stage, mem, "")
}

// stagePromptWithPreamble is stagePromptFor plus an INVARIANT block that must
// sit above the journal render.
//
// The order is the cache contract, not cosmetics: everything byte-stable across
// a run comes first (preamble, then journal render), everything volatile comes
// after (stage template, item context, disciplines, retry context). One
// volatile byte above the journal truncates every prefix match behind it — see
// pkg/journalengine/doc.go.
func stagePromptWithPreamble(stage string, mem *store.ItemMemoryDAO, preamble string) func(jc pipeline.JobContext) string {
	tmpl := stagePromptTemplates[stage]
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
		body := fmt.Sprintf(tmpl, id, title)
		if stage == "" {
			body = fmt.Sprintf(tmpl, jc.Stage.ID, id, title)
		}
		var b strings.Builder
		if preamble != "" {
			b.WriteString(preamble)
			b.WriteString("\n\n")
		}
		if journal := itemJournalBlock(mem, jc.Item); journal != "" {
			b.WriteString(journal)
			b.WriteString("\n\n")
		}
		b.WriteString(body)
		b.WriteString("\n\n")
		b.WriteString(backlogPromptContext(jc.Item))
		return b.String()
	}
}

// itemJournalPreface labels the journal block. It is a constant: any per-run
// text here would sit above the now-block boundary and void the warm prefix.
const itemJournalPreface = "WHAT THIS PIPELINE HAS ALREADY DONE FOR THIS ITEM (recorded by the pipeline, in order — treat it as fact, not as your own recollection):"

// itemJournalBlock renders the item's durable cross-stage memory as the leading
// stable block of a stage prompt. Empty string when the feature is off, no
// store is wired, the item has no memory yet, or the load fails — a memory read
// must never block a dispatch.
func itemJournalBlock(mem *store.ItemMemoryDAO, item *store.BacklogItem) string {
	if mem == nil || item == nil || item.ID == "" || !pipeline.ItemJournalEnabled() {
		return ""
	}
	j, err := mem.Get(context.Background(), item.ID)
	if err != nil {
		return ""
	}
	rendered := j.Render()
	if rendered == "" || rendered == journalengine.EmptyJournal {
		return ""
	}
	return itemJournalPreface + "\n" + rendered
}

// planSliceSpecDiscipline is appended to the plan_slice prompt so the spawned
// agent PERSISTS its decomposition instead of only printing it. The base
// prompt asks for "a numbered list of independent slices" as chat output,
// which the pipeline discards — so an item that arrived slice-less (a GitLab-
// issue import, or a council plan slice that declared no files) stayed
// slice-less through post_implement_gate and the scope gate had no envelope
// to enforce (escalations #332/#338). The runner re-reads the plan store
// right after this stage (Runner.SliceHydrator) and stamps file-bearing
// slices onto the item, so the persisted decomposition becomes the enforced
// scope envelope for the rest of the run.
const planSliceSpecDiscipline = "PERSIST YOUR DECOMPOSITION (pipeline-enforced): after planning, resolve the plan named in the backlog context with agent_plan_get. " +
	"If the plan has no slices, add each planned slice with agent_plan_slice_add{plan_id, name, goal, files} — `files` MUST list the real repo-relative paths the slice will touch (existing files, or the exact paths new files will be created at). " +
	"If the plan already has slices but they declare no files, backfill each with agent_plan_slice_update{slice_id, files}. " +
	"The pipeline reads these slices right after this stage to enforce the implementation's file scope; a decomposition that exists only in your chat output is discarded. " +
	"Do NOT invent paths — ground every path in the real repository tree. " +
	"A slice whose `files` are ALL new paths is invalid: new code that nothing existing imports merges dead, and the fabricated_slice gate escalates it. " +
	"Every slice that creates a file must also list the EXISTING file that will import/call it (the wiring edit), in the same slice."

// planSlicePromptFor wraps the default plan_slice prompt with the persist
// discipline above. Mirrors researchPromptFor/implementPromptFor: the base
// stagePromptFor stays generic and the stage-specific guardrail layers here.
func planSlicePromptFor(mem *store.ItemMemoryDAO) func(jc pipeline.JobContext) string {
	base := stagePromptFor("plan_slice", mem)
	return func(jc pipeline.JobContext) string {
		return base(jc) + "\n\n" + planSliceSpecDiscipline
	}
}

// researchPathDiscipline is appended to the research prompt to forbid
// path invention — the root cause of the gemma4-26b hallucination in
// PIPE-MILLS-2026-06-29-001 was a prompt that asked for "relevant code
// paths" with nothing real to anchor on.
const researchPathDiscipline = "IMPORTANT: The directory layout above is authoritative — a directory not shown there (within its depth) does not exist. " +
	"Reference only file paths that live inside directories shown above or that appear in the backlog context (e.g. the plan slices' file lists). " +
	"File paths the plan slices declare for creation are legitimate references even though the files do not exist yet. " +
	"Do NOT invent any other file paths. If you are unsure whether a file exists, describe the area in prose (package / responsibility) instead of guessing a path."

// researchLayoutMaxEntries caps the research grounding digest. Mirrors the
// council's councilLayoutMaxEntries: loom-core's real directory layout sits
// well under this; the cap is a safety valve, not an expected limit.
const researchLayoutMaxEntries = 250

// researchPromptFor wraps the default research prompt with a grounding
// preamble: the real directory layout (read once from repoRoot) plus an
// explicit no-invented-paths instruction. When repoRoot is empty or
// unreadable the digest is omitted but the path-discipline instruction
// still ships, so an ungrounded operator degrades rather than breaks.
//
// The layout is clients.RepoPackageLayout — top-level directories plus the
// package dirs beneath pkg/, internal/, cmd/ — NOT clients.RepoTreeDigest:
// that helper sorts the whole tree and truncates at a flat entry cap, so on
// loom-core the alphabetically-early changelog.d/ fragments exhausted the
// whole budget and the "authoritative layout" never showed cmd/, internal/,
// or pkg/ at all. The model was then told any path outside that list does
// not exist — grounding that actively taught it a false repo shape.
//
// The digest LEADS the prompt. It is read once at construction and is
// byte-identical for every item, so it belongs above the journal render in the
// cacheable prefix; trailing the volatile per-item text (where it used to sit)
// put an invariant block behind a volatile one and wasted it.
func researchPromptFor(repoRoot string, mem *store.ItemMemoryDAO) func(jc pipeline.JobContext) string {
	preamble := ""
	if digest := clients.RepoPackageLayout(repoRoot, researchLayoutMaxEntries); digest != "" {
		preamble = "Repository directory layout (real, authoritative):\n" + digest
	}
	base := stagePromptWithPreamble("research", mem, preamble)
	return func(jc pipeline.JobContext) string {
		return base(jc) + "\n\n" + researchPathDiscipline
	}
}

// implementDocsDiscipline is appended to the implement prompt so the spawned
// agent satisfies the repo's CI docs guardrail. scripts/ci/check_docs_guardrails.sh
// fails any MR with code-facing changes that does not also touch one of
// README.md / CHANGELOG.md / ROADMAP.md / AGENTS.md / docs/ / changelog.d/. The
// default implement prompt only asks for "code + tests", so every code-changing
// run produced a docs-less diff that failed the guardrails:docs-cli job at the
// ci_watch stage and escalated — the reason real council work never merged
// while doc-only heartbeat canaries did (e.g. MILLS-2026-06-30-001 → MR !847 →
// pipeline 16005 failed solely on guardrails:docs-cli).
//
// The satisfying artifact is now a per-MR changelog FRAGMENT (one new file
// under changelog.d/), not an edit to the shared CHANGELOG.md — direct edits to
// CHANGELOG.md's [Unreleased] section collide across concurrent MRs (GitLab
// flags server-side conflicts and drops auto-merge), which is exactly the
// failure mode this fragment workflow removes. A fragment is one isolated file,
// so parallel Mills runs never conflict.
const implementDocsDiscipline = "DOCS GUARDRAIL (CI-enforced): this repository's `guardrails:docs-cli` job " +
	"(scripts/ci/check_docs_guardrails.sh) FAILS any merge request with code-facing changes (e.g. *.go) " +
	"that does not also add documentation. A code-only diff WILL fail CI at ci_watch and waste the whole run. " +
	"Satisfy it by adding ONE new changelog fragment file: `changelog.d/<slug>.<category>.md` where " +
	"<category> is one of added|changed|deprecated|removed|fixed|security and <slug> is unique to this MR " +
	"(use the branch name). The file body is the Keep a Changelog bullet exactly as it should appear " +
	"(start with `- `, name the change and the touched files). Do NOT edit CHANGELOG.md directly — that " +
	"collides with other MRs; the fragment is folded into CHANGELOG.md at release time. Do NOT bypass the " +
	"check with [skip-docs-check]; add the real fragment as part of your diff."

// canaryImplementDiscipline keeps deterministic fixture canaries inside the
// implementation-stage time budget. The pipeline has a dedicated tests stage;
// repeating a cold `go test ./cmd/loom` inside the agent turn caused three
// consecutive 20-minute timeouts even though the requested edit was complete.
// Fixture canaries also do not need the code-change docs guardrail.
const canaryImplementDiscipline = "CANARY FAST PATH: this is a deterministic fixture-only canary. " +
	"Edit only the paths allowed by the slice, commit, and push the branch. Do NOT run the required tests " +
	"inside this implement turn; the pipeline's dedicated tests stage runs them next. Do not update CHANGELOG.md " +
	"or any other file outside the canary slice."

// canarySelfReviewDiscipline prevents the review spawn from repeating the
// dedicated tests stage. A cold Go compile can consume the entire spawn budget
// even though the pipeline already proved the branch green before review.
const canarySelfReviewDiscipline = "CANARY REVIEW FAST PATH: the pipeline's dedicated tests stage has already passed. " +
	"Do NOT run tests, builds, linters, or other compiles in this self-review turn. Review only the existing " +
	"cumulative branch diff against pr_self_review_v1, report the score and any findings, and finish promptly."

// prSelfReviewScopeDiscipline keeps self-review focused on reviewing evidence
// already produced by the run. Build and suite verification belong to the
// dedicated tests stage and CI, not to the review spawn.
const prSelfReviewScopeDiscipline = "SELF-REVIEW SCOPE: inspect the cumulative branch diff, the backlog specification, " +
	"and the tests-stage and CI outcomes already recorded for this run. Do NOT re-run builds or full test suites; " +
	"verification belongs to the dedicated tests stage and CI. Run a cheap, targeted static check (for example, go vet) " +
	"or a single-package test only when the diff clearly makes it necessary, and state the reason before running it."

// implementPromptFor wraps the default implement prompt with the docs-guardrail
// discipline above and, on a gate-fail retry, the retry discipline below.
// Mirrors researchPromptFor: stagePromptFor stays generic and the
// stage-specific guardrails are layered here, so the live operator's
// implement spawns produce MRs that pass the docs gate instead of escalating.
func implementPromptFor(mem *store.ItemMemoryDAO) func(jc pipeline.JobContext) string {
	base := stagePromptFor("implement", mem)
	return func(jc pipeline.JobContext) string {
		p := base(jc)
		if isFixtureCanary(jc.Item) {
			p += "\n\n" + canaryImplementDiscipline
		} else {
			p += "\n\n" + implementDocsDiscipline
		}
		if notes := researchNotesBlock(jc); notes != "" {
			p += "\n\n" + notes
		}
		if jc.RetryContext != nil {
			p += "\n\n" + implementRetryDiscipline(jc.RetryContext)
		}
		return p
	}
}

// researchNotesEnv disables piping the research stage's output into the
// implement prompt. Default ON — set to a falsy value to restore the previous
// behavior, where the research worker's notes were written to
// Artifacts["research_notes"] and read by nothing at all.
const researchNotesEnv = "LOOM_MILLS_RESEARCH_NOTES_IN_IMPLEMENT"

// maxResearchNotesBytes caps the notes block. The research model is not budget-
// bounded on output length, and the implement spawn pays for every byte.
const maxResearchNotesBytes = 8 << 10

const researchNotesHeader = "## Research findings (from the research stage)"

// researchNotesBlock renders the research stage's notes for the implement
// prompt, or "" when there are none.
//
// jc.Prior is populated by the dispatcher on a fresh drive and rehydrated by
// Runner.loadPriorOutputs on resume; in both cases research_notes arrives as a
// plain string (a rehydrated artifact is a decoded JSON value), so the type
// assertion is the guard against a worker that stashes something else there.
func researchNotesBlock(jc pipeline.JobContext) string {
	if !researchNotesInImplementEnabled() {
		return ""
	}
	prior, ok := jc.Prior["research"]
	if !ok || prior.Artifacts == nil {
		return ""
	}
	notes, _ := prior.Artifacts["research_notes"].(string)
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return ""
	}
	if len(notes) > maxResearchNotesBytes {
		notes = truncateResearchNotes(notes, maxResearchNotesBytes)
	}
	return researchNotesHeader + "\n\n" + notes
}

func researchNotesInImplementEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(researchNotesEnv))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// truncateResearchNotes cuts from the tail at a rune boundary and marks the
// elision, so the implement agent can tell a short note from a clipped one.
func truncateResearchNotes(s string, maxBytes int) string {
	const marker = "\n[... research notes truncated]"
	keep := maxBytes - len(marker)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && s[keep]&0xC0 == 0x80 {
		keep--
	}
	return s[:keep] + marker
}

func prSelfReviewPromptFor(mem *store.ItemMemoryDAO) func(jc pipeline.JobContext) string {
	base := stagePromptFor("pr_self_review", mem)
	return func(jc pipeline.JobContext) string {
		p := base(jc) + "\n\n" + prSelfReviewScopeDiscipline
		if isFixtureCanary(jc.Item) {
			p += "\n\n" + canarySelfReviewDiscipline
		}
		return p
	}
}

func isFixtureCanary(item *store.BacklogItem) bool {
	if item == nil {
		return false
	}
	var canary, safeFixture bool
	for _, label := range item.Labels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "mills-canary":
			canary = true
		case "safe-fixture":
			safeFixture = true
		}
	}
	return canary && safeFixture
}

// implementRetryDiscipline tells a gate-fail retry spawn what actually
// happened, because it cannot see it any other way: the retry runs as a
// FRESH agent in a fresh clone, while the plan store still shows the slice
// claimed/advanced by the discarded attempt. Without this block the retry
// agent resolves the plan via agent_plan_get, concludes the slice is already
// implemented, does nothing, and fails nonempty_diff — masking the original
// gate failure and burning the remaining attempts (observed live 2026-07-01
// on PIPE-pattern-stamp-go-rest-service-{widget,gadget}-…).
func implementRetryDiscipline(rc *pipeline.StageRetryContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "RETRY CONTEXT (implement attempt %d): a previous implement attempt for this item already ran and FAILED the %s gate", rc.Attempt, rc.GateStage)
	if rc.FirstFailure != "" {
		fmt.Fprintf(&b, " — %s", rc.FirstFailure)
	}
	if rc.LastFailure != "" && rc.LastFailure != rc.FirstFailure {
		fmt.Fprintf(&b, " (most recent failure: %s)", rc.LastFailure)
	}
	b.WriteString(". That attempt's commits were DISCARDED — none of its work exists in this workspace. ")
	b.WriteString("If the backlog context references a Plan, the plan store may still show the slice as claimed/in_progress/completed by the FAILED attempt; that status is STALE. ")
	b.WriteString("Do NOT conclude the work is already done and do NOT finish without changes: re-do the full implementation in THIS worktree, fix the gate failure named above, update the slice via agent_plan_slice_update once your redo is committed, and finish with a non-empty committed diff pushed via `git push -u origin HEAD`. ")
	b.WriteString("Finishing with an empty diff will fail the nonempty_diff gate and escalate the run.")
	return b.String()
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
		// The operator's logger, not slog.Default(), so the cumulative
		// git-capture warn events land on the same structured stream as
		// every other Mills log line (issue #224 triage).
		Logger: logger,
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
func buildWorkflowScheduler(st *store.Store, pm *mills.PolicyManager, spawn *clients.HUDSpawnClient, gitlab *clients.GitLabClient, spawnProject string, logger *slog.Logger) *workflow.WorkflowScheduler {
	gate := &workflowPolicyGate{pm: pm}
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
	// S7: registry-template runs derive spec-aware work prompts from the
	// claimed backlog item (canary prompts are untouched replay contracts).
	interp.SetBacklogItemLookup(func(ctx context.Context, id string) (string, string, error) {
		item, err := st.Backlog.Get(ctx, id)
		if err != nil {
			return "", "", err
		}
		return item.Title, item.SpecDoc, nil
	})
	// S6-full: the merging canary's single merge effect rides the pipeline
	// lane's idempotent GitLab client. Left unwired (nil client), merge()
	// fails closed instead of silently passing.
	if gitlab != nil {
		interp.SetMergeExecutor(newCanaryMerger(gitlab, spawnProject, logger))
		logger.Info("workflow merge executor wired", "project", spawnProject)
	} else {
		logger.Warn("workflow merge executor NOT wired (no GitLab client); merging canaries will fail closed")
	}
	logger.Info("imperative workflow runtime wired (default-OFF; flips via policy.workflows.enabled)", "spawn_project", spawnProject)
	return workflow.NewWorkflowScheduler(st.Workflow, interp, gate, logger)
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
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := caller.CallTool(probeCtx, clients.AgentContextServerName, "agent_session_list", map[string]any{
				"status": "active",
				"light":  true,
				"limit":  1,
			}); err != nil {
				logger.Warn("operator MCP hub health probe failed", "error", err)
			}
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

// squadsPolicyGate adapts the live PolicyManager to squads.PolicyGate so
// the router re-reads policy.squads.enabled on every Pick — a policy
// hot-reload flips routing on the next reconciler tick.
type squadsPolicyGate struct {
	pm *mills.PolicyManager
}

func (g squadsPolicyGate) SquadsEnabled() bool {
	if g.pm == nil {
		return false
	}
	return g.pm.Current().SquadsEnabled()
}

// SquadsMinConfidence satisfies squads.RoutingPolicy so the router honors
// the live policy.squads.routing.min_confidence value per Pick.
func (g squadsPolicyGate) SquadsMinConfidence() float64 {
	if g.pm == nil {
		return 0
	}
	return g.pm.Current().SquadsMinConfidence()
}

// workflowPolicyGate serves the imperative scheduler live policy reads: the
// enable flag (workflow.PolicyGate) and the run wall-clock bound
// (workflow.RunDeadlineGate). Hot-reloads apply on the next tick.
type workflowPolicyGate struct {
	pm *mills.PolicyManager
}

func (g *workflowPolicyGate) WorkflowsEnabled() bool {
	return g.pm != nil && g.pm.Current().WorkflowsEnabled()
}

func (g *workflowPolicyGate) MaxRunAge() time.Duration {
	if g.pm == nil {
		return 0
	}
	return g.pm.Current().WorkflowMaxRunAge()
}

// workflowSelectorAdapter glues the S7 registry-backed resolver
// (pkg/mills/workflow.ResolveItemSelection) onto the mills.WorkflowSelector
// contract. The indirection keeps pkg/mills free of an import on
// pkg/mills/workflow (which sits downstream of worker/pipeline/council). The
// error taxonomy maps onto the contract's outcomes: disabled/invalid
// selections become hold reasons (the reconciler skips fail-closed), only
// infrastructure errors propagate as errors (defer + retry).
type workflowSelectorAdapter struct {
	registry *workflow.Registry
}

func (a *workflowSelectorAdapter) Resolve(_ context.Context, item *store.BacklogItem, workflowsEnabled bool) (*store.WorkflowSelection, string, error) {
	if a == nil || a.registry == nil {
		return nil, "", nil
	}
	sel, err := workflow.ResolveItemSelection(a.registry, workflowsEnabled, item)
	if err != nil {
		// Resolution errors are authoring/policy conditions, not
		// infrastructure: hold the item with the reason rather than
		// retry-looping on a deterministic rejection.
		return nil, err.Error(), nil
	}
	return sel, "", nil
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
// The reviewer pool is loaded from policy.yaml's audit.pool_default /
// audit.pool_escalation (auditCfg) at boot, so operators rotate models
// by editing the configmap and rolling the pod — no recompile. When the
// configmap omits pool_default the built-in fallback in auditPoolPolicy
// applies. This closes the long-standing "v2.1 will load the pool from
// policy.yaml" TODO; per-run hot-reload without a restart remains a
// follow-up — the QueueWorker still snapshots the pool at construction.
func buildAuditSubsystem(
	flex *clients.FlexInferClient,
	councilRunner *runner.Runner,
	st *store.Store,
	repoRoot string,
	auditCfg mills.AuditPolicy,
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

	// The reviewer pool comes from policy.yaml (audit.pool_default /
	// audit.pool_escalation) so model drift is a configmap edit, not a
	// recompile. auditPoolPolicy falls back to the built-in Ready-model
	// defaults when the configmap omits pool_default.
	policy := auditPoolPolicy(auditCfg, logger)
	worker := audit.NewQueueWorker(dispatcher, st.Audit, *policy, audit.QueueOptions{
		Capacity: 64,
		Logger:   logger,
	})
	triggers := &audit.Triggers{
		Worker:              worker,
		LoadCouncilArtifact: audit.LoadCouncilArtifactFromFS(repoRoot),
		// LoadMergedDiff is wired AFTER the GitLab client is built (see
		// the audit follow-up block in run) — gitlabMergedDiffLoader
		// needs it. Left nil here, the trigger skips pipeline_merge
		// enqueues with a warn instead of auditing metadata-only stubs.
		Logger: logger,
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

// auditPoolPolicy builds the dispatcher pool from the policy.yaml audit
// section. When pool_default is empty it falls back to the two FlexInfer
// chat models verified Ready on the canonical cluster (proxy /v1/models,
// 2026-06-27): gemma4-26b-a4b-gptq + qwen36-35b-mtp-uncensored-5930k.
//
// The fallback intentionally configures NO escalation pool. FlexInfer
// serves no model stronger than those two, and the spawn-driver frontier
// (claude-code / codex) is not registered in the dispatcher's reviewer
// map — a "spawn" member would be skipped, while a flexinfer-tagged
// frontier model id (the historical claude-opus-4-7 / codex-gpt5 entries)
// 404s. With an empty escalation pool the dispatcher records borderline
// bulk medians as-is instead of escalating into a dead model. Re-add an
// escalation pool once a real frontier reviewer is wired.
func auditPoolPolicy(cfg mills.AuditPolicy, logger *slog.Logger) *audit.PoolPolicy {
	bulk := auditPoolMembers(cfg.PoolDefault)
	escalation := auditPoolMembers(cfg.PoolEscalation)
	if len(bulk) == 0 {
		bulk = []audit.PoolMember{
			{Backend: "flexinfer", Model: "gemma4-26b-a4b-gptq"},
			{Backend: "flexinfer", Model: "qwen36-35b-mtp-uncensored-5930k"},
		}
		escalation = nil
		if logger != nil {
			logger.Info("audit: policy audit.pool_default empty; using built-in Ready-model fallback",
				"bulk", len(bulk), "escalation", len(escalation))
		}
		return &audit.PoolPolicy{Bulk: bulk, Escalation: escalation}
	}
	if logger != nil {
		logger.Info("audit: reviewer pool loaded from policy",
			"bulk", len(bulk), "escalation", len(escalation))
	}
	return &audit.PoolPolicy{Bulk: bulk, Escalation: escalation}
}

// auditPoolMembers converts policy.yaml AuditPool entries into dispatcher
// PoolMembers, dropping entries missing a backend or model. The Driver
// field is policy-only metadata (it documents which spawn CLI a "spawn"
// backend would use); the dispatcher routes purely on Backend, so it is
// not copied.
func auditPoolMembers(in []mills.AuditPool) []audit.PoolMember {
	if len(in) == 0 {
		return nil
	}
	out := make([]audit.PoolMember, 0, len(in))
	for _, m := range in {
		if strings.TrimSpace(m.Backend) == "" || strings.TrimSpace(m.Model) == "" {
			continue
		}
		out = append(out, audit.PoolMember{Backend: m.Backend, Model: m.Model})
	}
	return out
}

// auditDiffMaxBytes caps the unified diff fed to the audit rubric. The
// artifact is substituted verbatim into the LLM prompt (no downstream
// truncation — see pkg/mills/audit/rubric.go), and the bulk auditors are
// ~26B quantized models with limited context, so the cap protects the
// prompt rather than the network.
const auditDiffMaxBytes = 64 * 1024

// gitlabMergedDiffLoader fetches the merged MR's real unified diff for
// the pipeline_merge audit (the v2.1 the old stub promised). The v2.0
// stub returned only run/item metadata — no diff at all — so EVERY
// pipeline_merge audit scored 0.00 with a critical "diff unreadable"
// finding and opened an advisory issue (#225/#227/#233/#234): pure noise
// that buried real audit signal. A metadata header keeps the auditor
// grounded on what the run intended; the diff body is what it scores.
// Returns "" (trigger skips the enqueue with a warn) when the run has no
// MR iid or GitLab returns an empty diff — no audit beats a junk audit.
// buildGateTiebreaker constructs the dissent-tiebreaker judge for the
// LLM-judged gates when an Anthropic key is configured (same env resolution
// as the council editor). Nil when the key is absent or the client fails to
// init — the gates then run primary-only, exactly as before. Model comes
// from LOOM_MILLS_GATE_TIEBREAKER_MODEL (default claude-sonnet-5: the gate
// envelope is a ~200-token verdict, so the mid-tier model is plenty and the
// call fires only on primary-vs-tests dissent). Disable outright by setting
// the env var to "off".
func buildGateTiebreaker(logger *slog.Logger) gates.RubricJudge {
	model := strings.TrimSpace(os.Getenv("LOOM_MILLS_GATE_TIEBREAKER_MODEL"))
	if strings.EqualFold(model, "off") {
		logger.Info("gate tiebreaker disabled via LOOM_MILLS_GATE_TIEBREAKER_MODEL=off")
		return nil
	}
	if model == "" {
		model = "claude-sonnet-5"
	}
	key := clients.AnthropicAPIKeyFromEnv()
	if key == "" {
		return nil
	}
	ac, err := clients.NewAnthropicClient(clients.AnthropicClientConfig{
		APIKey:  key,
		BaseURL: clients.AnthropicBaseURLFromEnv(),
		Timeout: 2 * time.Minute,
	})
	if err != nil {
		logger.Warn("gate tiebreaker requested but anthropic client init failed; gates run primary-only", "err", err)
		return nil
	}
	logger.Info("gate dissent tiebreaker enabled", "backend", "anthropic", "model", model)
	return &clients.AnthropicRubricJudge{Client: ac, Model: model}
}

type pipelineProjectResolver interface {
	AuthorizedProject(ctx context.Context, pipelineRunID string) (string, error)
}

func gitlabMergedDiffLoader(gl *clients.GitLabClient, projects pipelineProjectResolver) func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) (string, error) {
	return func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) (string, error) {
		if gl == nil || run == nil || run.MRIID == nil || *run.MRIID == 0 {
			return "", nil
		}
		if projects == nil {
			return "", fmt.Errorf("load merged diff for run %s: durable project resolver unavailable", run.ID)
		}
		project, err := projects.AuthorizedProject(ctx, run.ID)
		if err != nil {
			return "", fmt.Errorf("load merged diff for run %s: %w", run.ID, err)
		}
		// MR IIDs are per-project. Resolve the project from immutable successful
		// stage artifacts, never the refreshed backlog item's mutable target.
		diffClient := gl.ForProject(project)
		diff, err := diffClient.MRDiff(ctx, *run.MRIID, auditDiffMaxBytes)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(diff) == "" {
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
		fmt.Fprintf(&b, "\nProject: %s\nMR iid: %d\nCost: $%.2f, attempts: %d\n", project, *run.MRIID, run.CostUSD, run.Attempts)
		b.WriteString("\n## Unified diff\n\n")
		b.WriteString(diff)
		return b.String(), nil
	}
}

// spinConcurrencyFromEnv reads LOOM_MILLS_SPIN_MAX_CONCURRENT as the async-spin
// concurrency cap. Returns 0 (keep the newOperator default) when unset or
// invalid.
func spinConcurrencyFromEnv(logger *slog.Logger) int {
	raw := strings.TrimSpace(os.Getenv("LOOM_MILLS_SPIN_MAX_CONCURRENT"))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logger.Warn("ignoring invalid LOOM_MILLS_SPIN_MAX_CONCURRENT", "value", raw)
		return 0
	}
	return n
}

// bearerOrEmpty formats a bearer Authorization value, or "" when the token
// is unset so NewHTTPProbe drops the header entirely.
func bearerOrEmpty(token string) string {
	if strings.TrimSpace(token) == "" {
		return ""
	}
	return "Bearer " + token
}

// This file holds the embedding surface used when the HUD is co-hosted in
// the same process as the loom daemon. The constructors here (NewApp,
// StartMonitors, RegisterRoutes, RefreshMonitors, StopMonitors) form the
// public library API that downstream Go consumers — including the loom
// daemon's own startEmbeddedHUD path — depend on. Background, lifecycle
// rules, and a worked example live in docs/HUD_EMBEDDING.md.
//
// Embedded mode pairs hud.NewApp with bridge.NewLocalCaller so JSON-RPC
// calls dispatch directly to the daemon's handleMessage in-process; no
// Unix socket, no transport layer, no circuit breaker. The standalone
// "loom hud" CLI by contrast uses bridge.NewDaemonClient over a socket.
//
// Stability: this surface is pre-1.0 and may change in minor versions.
// Pin to a tagged Loom Core release if downstream stability matters.

package hud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	loomcache "github.com/crb2nu/loom/internal/cache"
	"github.com/crb2nu/loom/internal/devbox/backend"
	"github.com/crb2nu/loom/internal/hud/alerting"
	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/hud/cfaccess"
	"github.com/crb2nu/loom/internal/hud/coordinator"
	"github.com/crb2nu/loom/internal/hud/domain/memory"
	"github.com/crb2nu/loom/internal/hud/mirror"
	"github.com/crb2nu/loom/internal/hud/monitor"
	"github.com/crb2nu/loom/internal/hud/mrwatch"
	"github.com/crb2nu/loom/internal/hud/shuttle"
	"github.com/crb2nu/loom/pkg/codebase"
	"github.com/crb2nu/loom/pkg/mcpotel"
	"github.com/crb2nu/loom/pkg/poll"
)

const (
	embeddedFleetSnapshotCacheKey    = "hud:embedded:fleet_snapshot"
	embeddedPipelineSnapshotCacheKey = "hud:embedded:pipeline_snapshot"
	embeddedSnapshotCacheTTL         = 10 * time.Minute
)

// NewApp constructs a HUD App with the given caller and configuration.
// It does NOT start any background work — caller responsibilities are:
//
//  1. Pass a bridge.Caller. Two implementations ship with Loom Core:
//     bridge.NewDaemonClient(socketPath, logger) for standalone mode, and
//     bridge.NewLocalCaller(dispatch) for in-process / embedded mode.
//     Both satisfy the Caller interface; embedded mode skips the socket
//     and the circuit breaker by dispatching directly to the daemon's
//     MCP message handler.
//  2. Call (*App).StartMonitors(ctx) once to begin background polling.
//     StartMonitors is NOT idempotent — calling it twice double-starts
//     each monitor goroutine.
//  3. Call (*App).RegisterRoutes(mux) to mount HTTP handlers, OR rely on
//     the standalone Run() function which builds its own mux and listener.
//  4. Defer (*App).StopMonitors() so monitors, the coordinator, the cache,
//     and the OTel tracer get torn down on shutdown.
//
// If logger is nil, slog.Default() with component="hud" is used. The
// returned App is safe to use from multiple goroutines after StartMonitors
// has returned; concurrent calls to NewApp itself are not supported.
//
// See docs/HUD_EMBEDDING.md for embedding patterns, lifecycle rules, and
// a worked in-process example.
func NewApp(cfg Config, caller bridge.Caller, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default().With("component", "hud")
	}

	agent := bridge.NewAgentBridge(caller)

	cacheCfg := loomcache.LoadConfigFromEnv()
	appCache := loomcache.New(cacheCfg, logger)

	app := &App{
		config:               cfg,
		client:               caller,
		agent:                agent,
		cache:                appCache,
		cacheBackend:         cacheCfg.Backend,
		logger:               logger,
		nudgeQueue:           NewNudgeQueue(),
		mobileRevocationList: NewMobileTokenRevocationList(),
		deviceTokenStore:     NewDeviceTokenStore(),
		mobileRateLimiter: NewMobileRateLimiter(MobileRateLimitConfig{
			MutationPerMinute: cfg.MobileRateLimitMutation,
			ReadPerMinute:     cfg.MobileRateLimitRead,
		}),
	}

	// Initialize OTel tracer.
	tp, otelShutdown, err := mcpotel.InitTracer(context.Background(), "loom-hud", logger)
	if err != nil {
		logger.Warn("otel tracer init failed, continuing without tracing", "error", err)
	} else if otelShutdown != nil {
		// Store shutdown for StopMonitors to call.
		app.otelShutdown = otelShutdown
	}
	app.tracer = mcpotel.Tracer(tp, "loom-hud")
	app.metrics = NewHUDMetrics()
	app.agentContextMetrics = NewAgentContextMetrics()
	app.agentContextLatest = NewAgentContextLatestStore()
	app.blocked = newBlockedStore()

	// Cloudflare Access SSO → HUD admin (optional). Enabled only when a team
	// domain + admin allowlist are configured; the JWKS refresh runs for the
	// life of the process, so use a background context (not a request one).
	app.accessVerifier = cfaccess.New(context.Background(), cfaccess.Config{
		TeamDomain:  cfg.CFAccessTeamDomain,
		AUD:         cfg.CFAccessAUD,
		AdminEmails: cfaccess.ParseEmails(cfg.CFAccessAdminEmails),
	})
	if app.accessVerifier.Enabled() {
		logger.Info("cloudflare access admin enabled",
			"team_domain", cfg.CFAccessTeamDomain,
			"aud_checked", app.accessVerifier.AUDChecked(),
			"admin_emails", len(cfaccess.ParseEmails(cfg.CFAccessAdminEmails)))
		if !app.accessVerifier.AUDChecked() {
			logger.Warn("cloudflare access admin: HUD_CF_ACCESS_AUD unset — aud check disabled; set it to scope tokens to this app")
		}
	}

	// LAN trusted-network admin (optional) — for the internal-ingress path that
	// bypasses Cloudflare. See internal/hud/trusted_network.go.
	app.adminTrustedNets = parseCIDRs(cfg.AdminTrustedCIDRs)
	if len(app.adminTrustedNets) > 0 {
		nets := make([]string, len(app.adminTrustedNets))
		for i, n := range app.adminTrustedNets {
			nets[i] = n.String()
		}
		logger.Info("trusted-network admin enabled", "cidrs", strings.Join(nets, ","))
	}

	return app, nil
}

// StartMonitors initializes and starts all background monitors and the
// optional components (SSE hub, event log, coordinator, spawn orchestrator,
// alert engine, push bridge, session reaper). Each monitor runs on its own
// fixed cadence:
//
//	fleet         15s   memory     10s   sandbox       10s
//	health         5s   workflow    5s   cost          10s
//	stream         5s   pipeline   10s   context-health 5s
//	codebase      30s   shuttle     3s
//
// The supplied ctx governs lifecycle for the spawn orchestrator's reconcile
// loop, the session reaper, and the push token reaper. Cancelling ctx does
// NOT stop the monitor polling loops — call (*App).StopMonitors() for that.
//
// StartMonitors is NOT idempotent. Call it exactly once per App.
//
// In embedded mode the daemon's external SSE event consumer is bypassed,
// so monitors will only observe daemon-side state changes via their
// polling cadence. Use (*App).RefreshMonitors() to force an immediate
// refresh after startup or after the daemon reloads.
func (a *App) StartMonitors(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			a.releaseSpawnControllerOwnerLock()
		}
	}()

	// Shared runtime wiring must exist before the first monitor refresh so the
	// embedded HUD can broadcast the initial snapshots instead of missing them.
	a.sseHub = NewSSEHub(a.logger)
	a.eventLog = NewEventLog(1000)

	// Background monitors.
	a.fleetMonitor = monitor.NewFleetMonitor(a.client, a.agent, a.logger)

	a.healthMonitor = monitor.NewHealthMonitor(a.client, a.logger)

	a.memoryMonitor = monitor.NewMemoryMonitor(a.agent, a.logger)

	a.workflowMonitor = monitor.NewWorkflowMonitor(a.agent, a.logger)

	a.streamMonitor = monitor.NewStreamMonitor(a.agent, a.logger)

	a.sandboxMonitor = monitor.NewSandboxMonitor(a.client, a.logger)

	a.costMonitor = monitor.NewCostMonitor(a.client, a.logger)

	a.otelMonitor = monitor.NewOTelMonitor(a.client, a.logger)

	// Mills status monitor — only when the operator URL is configured (unset
	// on developer laptops, where every /api/mills route 503s anyway).
	if a.config.MillsOperatorURL != "" {
		a.millsMonitor = monitor.NewMillsMonitor(a.config.MillsOperatorURL, a.logger)
	}

	// Branch→MR status registry (mrwatch). Nil when GitLab is unconfigured;
	// a build error is logged and skipped so an unreachable/misconfigured
	// GitLab never aborts HUD init (degraded-mode contract).
	if poller, shepherd, err := mrwatch.NewPollerFromEnv(a.logger); err != nil {
		a.logger.Error("mrwatch: poller init failed; MR awareness disabled", "error", err)
	} else {
		a.mrwatchPoller = poller
		a.mrwatchShepherd = shepherd
		// M5 notifier: durable inbox nudges + attention-lane items on unhealthy
		// transitions. Registered additively so it runs alongside the shepherd
		// without either clobbering the other. Enabled by default (read-only
		// attention lanes); LOOM_MRWATCH_NOTIFY=off silences both channels.
		if poller != nil {
			a.mrwatchNotifier = mrwatch.NewNotifier(
				&mrwatchMessageSender{agent: a.agent},
				a.mrwatchOwnerResolver,
				mrwatch.NotifierOptions{
					Enabled: mrwatch.NotifyEnabledFromEnv(),
					Logger:  a.logger,
				},
			)
			poller.AddPostPoll(a.mrwatchNotifier.Notify)
			a.logger.Info("mrwatch: notifier configured", "enabled", a.mrwatchNotifier.Enabled())
		}
	}

	pipelineProjects := a.config.PipelineProjects
	if pipelineProjects == "" {
		if detected := codebase.DetectPipelineProject(ctx, "."); detected != "" {
			pipelineProjects = detected
			a.logger.Info("auto-detected pipeline project", "project", detected)
		}
	}
	var projects []string
	if pipelineProjects != "" {
		for _, p := range strings.Split(pipelineProjects, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				projects = append(projects, trimmed)
			}
		}
	} else {
		// Fallback: ask the gitlab MCP server what projects the authenticated
		// user has access to. This makes "pipelines show up" work out of the
		// box in the cluster/hub deployment without requiring an exhaustive
		// hardcoded `HUD_PIPELINE_PROJECTS` env var to enumerate every repo.
		if discovered, err := a.agent.ListPipelineProjects(ctx, 5); err != nil {
			a.logger.Info(
				"pipeline project auto-discovery unavailable, skipping pipeline monitor",
				"reason", err.Error(),
			)
		} else if len(discovered) > 0 {
			projects = discovered
			a.logger.Info(
				"auto-discovered pipeline projects via gitlab MCP",
				"count", len(discovered),
			)
		}
	}
	if len(projects) > 0 {
		a.pipelineMonitor = monitor.NewPipelineMonitor(a.agent, projects, a.cache, a.logger)
	}

	a.contextHealthMonitor = monitor.NewContextHealthMonitor(a.agent, nil, a.logger)

	a.codebaseMonitor = monitor.NewCodebaseMonitor(a.agent, a.logger)

	// Shuttle engine + monitor.
	a.shuttleEngine = shuttle.NewEngine(a.logger)
	a.shuttleMonitor = shuttle.NewShuttleMonitor(a.shuttleEngine, a.agent, a.logger)

	// Alert engine + auto-fix.
	alertDispatcher := alerting.NewDispatcher(a.sseHub, nil, nil, nil, a.logger)
	a.alertEngine = alerting.NewAlertEngine(alertDispatcher, a.logger)

	// Spawn orchestrator. A broken spawn backend must not take the whole HUD
	// down with it: returning the error here aborts embedded-HUD init in the
	// daemon, which then serves 404 on every /api/* route for the process
	// lifetime while the listener stays up (observed live 2026-07-14 when a
	// transient k3s API outage at boot failed spawn-state recovery). Serve
	// the HUD without the spawn orchestrator instead.
	if a.config.SpawnEnabled {
		if err := a.initSpawnOrchestrator(ctx); err != nil {
			a.logger.Error("spawn backend init failed; continuing without spawn orchestrator",
				"error", err)
		}
	}

	// Session reaper.
	go a.sessionReaper(ctx)

	// Push token reaper.
	if a.config.MobilePushEnabled {
		go a.pushTokenReaper(ctx)
	}

	// APNs push bridge.
	if a.config.MobilePushEnabled && a.config.APNsKeyPath != "" {
		a.initPushBridge()
	}

	// Wire monitor → SSE broadcast callbacks.
	a.wireMonitorCallbacks()

	// Start the polling loops only after callbacks are wired so the initial
	// refreshes are immediately visible to embedded/mobile clients.
	a.fleetMonitor.Start(15 * time.Second)
	a.healthMonitor.Start(5 * time.Second)
	a.memoryMonitor.Start(10 * time.Second)
	a.workflowMonitor.Start(5 * time.Second)
	a.streamMonitor.Start(5 * time.Second)
	a.sandboxMonitor.Start(10 * time.Second)
	a.costMonitor.Start(10 * time.Second)
	a.otelMonitor.Start(30 * time.Second)
	if a.millsMonitor != nil {
		a.millsMonitor.Start(15 * time.Second)
	}
	if a.mrwatchPoller != nil {
		// mrwatch owns its own jittered cadence (LOOM_MRWATCH_INTERVAL,
		// default 90s) and stops on ctx cancellation.
		a.mrwatchPoller.Start(ctx)
	}
	if a.pipelineMonitor != nil {
		a.pipelineMonitor.Start(10 * time.Second)
	}
	a.contextHealthMonitor.Start(5 * time.Second)
	a.codebaseMonitor.Start(30 * time.Second)
	a.shuttleMonitor.Start(3 * time.Second)

	// HUD presence mirror: federate this daemon's active presence to a
	// remote HUD when LOOM_HUD_MIRROR_URL is set. No-op otherwise.
	if cfg := mirror.NewConfigFromEnv(); cfg.Enabled() {
		a.hudMirror = mirror.New(cfg, a.agent, nil, a.logger)
		// Forward this daemon's per-session tool-call activity (captured in the
		// EventLog via the embedded event bridge) to the remote HUD, so a
		// distributed agent's calls surface in the central HUD's session trace.
		a.hudMirror.SetToolCalls(a)
		a.hudMirror.Start(ctx)
		a.logger.Info("hud presence mirror enabled",
			"url", cfg.URL,
			"interval", cfg.Interval,
			"timeout", cfg.Timeout,
		)
	}

	// Wire pipeline monitor → alert engine callback.
	if a.pipelineMonitor != nil && a.alertEngine != nil {
		a.pipelineMonitor.OnRefresh(func(pipelines []bridge.PipelineInfo) {
			a.alertEngine.Evaluate(pipelines)
		})
	}

	a.logger.Info("background monitors started",
		"fleet", "15s", "health", "5s", "memory", "10s",
		"workflow", "5s", "stream", "5s", "sandbox", "10s", "cost", "10s",
		"context-health", "5s", "codebase", "30s", "shuttle", "3s")

	// Coordinator.
	if a.config.FlexInferURL != "" {
		a.initCoordinator()
	}

	// Auto-fix engine — after the coordinator (shares its LLM client) and
	// the spawn orchestrator (agent_fix strategy). No-op unless
	// config.AutofixEnabled; routes stay honest-empty while disabled.
	a.initAutofixEngine()

	// Domain registry.
	a.initDomainRegistry()

	return nil
}

// RefreshMonitors forces a best-effort one-shot refresh of every embedded
// HUD snapshot. Standalone HUD subscribes to the daemon's SSE event endpoint
// and refreshes monitors on event arrival; embedded mode bypasses that
// endpoint (both sides share a process) so explicit refreshes are needed
// to avoid serving stale or empty state.
//
// Behaviour:
//   - Each monitor's Refresh() is called sequentially. Failures are logged
//     and skipped; one bad monitor does not block the others.
//   - The fleet refresh retries once after a 500ms backoff if the first
//     refresh produced an empty snapshot, to absorb daemon startup races.
//   - When a refresh produces an empty snapshot but a cached snapshot
//     exists in loomcache (key embeddedFleetSnapshotCacheKey or
//     embeddedPipelineSnapshotCacheKey, TTL embeddedSnapshotCacheTTL = 10m)
//     the cached value is restored to the monitor.
//
// Call sites:
//   - Once shortly after StartMonitors returns. The daemon's
//     startEmbeddedHUD launches it via "go app.RefreshMonitors()" so the
//     refresh does not block route registration.
//   - After the daemon hot-reloads its config or rebuilds its tool registry.
//
// Safe to call concurrently with monitor polling; safe to call repeatedly.
func (a *App) RefreshMonitors() {
	if a.fleetMonitor != nil && a.fleetMonitor.Ready() {
		if err := a.fleetMonitor.RefreshForce(); err != nil {
			a.logger.Warn("embedded refresh: fleet refresh failed", "error", err)
		} else if fleetSnapshotLooksEmpty(a.fleetMonitor.Snapshot()) {
			// If the first refresh raced startup/reload, give the daemon a brief
			// moment to settle and try once more before we fall back to polling.
			time.Sleep(500 * time.Millisecond)
			if err := a.fleetMonitor.RefreshForce(); err != nil {
				a.logger.Warn("embedded refresh retry: fleet refresh failed", "error", err)
			}
		}
		if snap := a.fleetMonitor.Snapshot(); !fleetSnapshotLooksEmpty(snap) {
			a.storeCachedSnapshot(embeddedFleetSnapshotCacheKey, snap, embeddedSnapshotCacheTTL)
		} else if cached, ok := a.loadCachedSnapshot(embeddedFleetSnapshotCacheKey); ok {
			a.logger.Info("embedded refresh: restored cached fleet snapshot")
			a.fleetMonitor.Update(cached)
		}
	}
	if a.healthMonitor != nil {
		if err := a.healthMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: health refresh failed", "error", err)
		}
	}
	if a.memoryMonitor != nil {
		if err := a.memoryMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: memory refresh failed", "error", err)
		}
	}
	if a.workflowMonitor != nil {
		if err := a.workflowMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: workflow refresh failed", "error", err)
		}
	}
	if a.streamMonitor != nil {
		if err := a.streamMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: stream refresh failed", "error", err)
		}
	}
	if a.sandboxMonitor != nil {
		if err := a.sandboxMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: sandbox refresh failed", "error", err)
		}
	}
	if a.costMonitor != nil {
		if err := a.costMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: cost refresh failed", "error", err)
		}
	}
	if a.otelMonitor != nil {
		if err := a.otelMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: otel refresh failed", "error", err)
		}
	}
	if a.millsMonitor != nil {
		if err := a.millsMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: mills refresh failed", "error", err)
		}
	}
	if a.pipelineMonitor != nil && a.pipelineMonitor.Ready() {
		if refreshPipelineMonitor(a.pipelineMonitor, a.logger) {
			pipelines := a.pipelineMonitor.Pipelines()
			a.storeCachedSnapshot(embeddedPipelineSnapshotCacheKey, pipelines, embeddedSnapshotCacheTTL)
		} else if cached, ok := a.loadCachedPipelineSnapshot(); ok {
			a.logger.Info("embedded refresh: restored cached pipeline snapshot")
			a.pipelineMonitor.Update(cached)
		}
	}
	if a.contextHealthMonitor != nil {
		if err := a.contextHealthMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: context-health refresh failed", "error", err)
		}
	}
	if a.codebaseMonitor != nil {
		if err := a.codebaseMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: codebase refresh failed", "error", err)
		}
	}
	if a.shuttleMonitor != nil {
		if err := a.shuttleMonitor.Refresh(); err != nil {
			a.logger.Warn("embedded refresh: shuttle refresh failed", "error", err)
		}
	}
}

func fleetSnapshotLooksEmpty(s monitor.FleetSnapshot) bool {
	return len(s.Agents) == 0 &&
		len(s.Tasks) == 0 &&
		len(s.Sessions) == 0 &&
		len(s.FileClaims) == 0 &&
		len(s.Worktrees) == 0 &&
		len(s.Spawns) == 0 &&
		s.ActiveSessions == 0 &&
		s.TotalSessions == 0 &&
		s.TotalTasks == 0
}

type pipelineMonitorRefresher interface {
	Ready() bool
	Refresh() error
	Pipelines() []bridge.PipelineInfo
	Projects() []string
}

func refreshPipelineMonitor(mon pipelineMonitorRefresher, logger *slog.Logger) bool {
	if mon == nil || !mon.Ready() {
		return false
	}
	if err := mon.Refresh(); err != nil {
		logger.Warn("embedded refresh: pipeline refresh failed", "error", err)
		return false
	}
	if len(mon.Pipelines()) == 0 && len(mon.Projects()) > 0 {
		time.Sleep(500 * time.Millisecond)
		if err := mon.Refresh(); err != nil {
			logger.Warn("embedded refresh retry: pipeline refresh failed", "error", err)
		}
	}
	return len(mon.Pipelines()) > 0
}

func (a *App) loadCachedSnapshot(key string) (monitor.FleetSnapshot, bool) {
	var snap monitor.FleetSnapshot
	if a.cache == nil {
		return snap, false
	}
	cached, ok := a.cache.Get(key)
	if !ok || cached == nil {
		return snap, false
	}
	raw, err := json.Marshal(cached)
	if err != nil {
		return snap, false
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return snap, false
	}
	return snap, true
}

func (a *App) loadCachedPipelineSnapshot() ([]bridge.PipelineInfo, bool) {
	if a.cache == nil {
		return nil, false
	}
	cached, ok := a.cache.Get(embeddedPipelineSnapshotCacheKey)
	if !ok || cached == nil {
		return nil, false
	}
	raw, err := json.Marshal(cached)
	if err != nil {
		return nil, false
	}
	var pipelines []bridge.PipelineInfo
	if err := json.Unmarshal(raw, &pipelines); err != nil {
		return nil, false
	}
	return pipelines, true
}

func (a *App) storeCachedSnapshot(key string, value any, ttl time.Duration) {
	if a.cache == nil {
		return
	}
	a.cache.Set(key, value, ttl)
}

// StopMonitors stops every background monitor, the optional coordinator,
// closes the loomcache backend, and shuts down the OTel tracer. It is
// safe to call when StartMonitors has not been called (each component
// is nil-checked) and safe to call multiple times.
//
// Embedders should defer StopMonitors immediately after a successful
// StartMonitors so cleanup runs on shutdown regardless of the exit path:
//
//	if err := app.StartMonitors(ctx); err != nil {
//	    return err
//	}
//	defer app.StopMonitors()
//
// StopMonitors does NOT shut down any HTTP server the embedder mounted
// the HUD on — that lifecycle belongs to the host process.
func (a *App) StopMonitors() {
	if a.fleetMonitor != nil {
		a.fleetMonitor.Stop()
	}
	if a.healthMonitor != nil {
		a.healthMonitor.Stop()
	}
	if a.memoryMonitor != nil {
		a.memoryMonitor.Stop()
	}
	if a.workflowMonitor != nil {
		a.workflowMonitor.Stop()
	}
	if a.streamMonitor != nil {
		a.streamMonitor.Stop()
	}
	if a.sandboxMonitor != nil {
		a.sandboxMonitor.Stop()
	}
	if a.costMonitor != nil {
		a.costMonitor.Stop()
	}
	if a.pipelineMonitor != nil {
		a.pipelineMonitor.Stop()
	}
	if a.contextHealthMonitor != nil {
		a.contextHealthMonitor.Stop()
	}
	if a.codebaseMonitor != nil {
		a.codebaseMonitor.Stop()
	}
	if a.shuttleMonitor != nil {
		a.shuttleMonitor.Stop()
	}
	if a.hudMirror != nil {
		a.hudMirror.Stop()
	}
	if a.coordinator != nil {
		a.coordinator.Stop()
	}
	if a.cache != nil {
		a.cache.Close()
	}
	if a.otelShutdown != nil {
		a.otelShutdown(context.Background())
	}
	a.releaseSpawnControllerOwnerLock()
}

// RegisterRoutes mounts all HUD HTTP routes on the supplied ServeMux.
// This is the same route set the standalone "loom hud" command installs:
// the static frontend (served from the embedded frontend/dist FS),
// the JSON API under /api/, the SSE event hub under /api/events,
// pprof under /debug/pprof/ when enabled, and the mobile operator API
// under /api/mobile/v1 when MobileOperatorToken is configured.
//
// Call this AFTER StartMonitors so the SSE hub and event log are ready
// to receive broadcasts from monitor refresh callbacks.
//
// The embedder owns the ServeMux and the http.Server / listener; the
// HUD only registers handlers. The standalone runtime at
// internal/hud/runtime.go shows the full server-construction pattern
// (TLS, port file, signal handling, browser-open) for cases where the
// embedder wants the same defaults.
func (a *App) RegisterRoutes(mux *http.ServeMux) {
	a.registerRoutes(mux)
}

func resolveSpawnControllerIdentity(cfg Config) (controllerID string, generated bool) {
	if configured := strings.TrimSpace(cfg.SpawnControllerID); configured != "" {
		return configured, false
	}
	// Config normally carries the environment value, but direct embedders may
	// construct Config themselves. Treat the environment as explicit in both
	// paths so the cluster singleton never takes a host-local lock.
	if configured := strings.TrimSpace(os.Getenv("SPAWN_CONTROLLER_ID")); configured != "" {
		return configured, false
	}
	return scopedDefaultSpawnControllerID(
		cfg.SocketPath,
		fmt.Sprintf("%s:%d", cfg.BindAddress, cfg.Port),
	), true
}

// initSpawnOrchestrator sets up the spawn backend and orchestrator.
func (a *App) initSpawnOrchestrator(ctx context.Context) (err error) {
	cfg := a.config
	spawnCfg := DefaultSpawnConfig()
	spawnCfg.SyncMode = cfg.SpawnSyncMode
	controllerID, generatedControllerID := resolveSpawnControllerIdentity(cfg)
	spawnCfg.ControllerID = controllerID
	if generatedControllerID {
		ownerLock, lockErr := acquireSpawnControllerOwnerLock(controllerID)
		if lockErr != nil {
			return fmt.Errorf("claim generated spawn controller identity: %w", lockErr)
		}
		a.spawnControllerOwnerLock = ownerLock
		defer func() {
			if err != nil {
				a.releaseSpawnControllerOwnerLock()
			}
		}()
		if a.logger != nil {
			a.logger.Info("claimed generated local spawn controller identity",
				"controller_id", controllerID,
				"lock", ownerLock.path)
		}
	}

	spawnBackend, err := backend.NewK8sBackend(backend.K8sBackendConfig{
		Kubeconfig:                   cfg.SpawnKubeconfig,
		Namespace:                    cfg.SpawnNamespace,
		Registry:                     cfg.SpawnRegistry,
		SyncMode:                     cfg.SpawnSyncMode,
		GitBaseURL:                   cfg.SpawnGitBaseURL,
		GitSecret:                    cfg.SpawnGitSecret,
		GitCloneImage:                cfg.SpawnGitCloneImage,
		BuildCPURequest:              cfg.SpawnBuildCPURequest,
		BuildCPULimit:                cfg.SpawnBuildCPULimit,
		BuildMemoryRequest:           cfg.SpawnBuildMemoryRequest,
		BuildMemoryLimit:             cfg.SpawnBuildMemoryLimit,
		BuildEphemeralStorageRequest: cfg.SpawnBuildEphemeralStorageRequest,
		BuildEphemeralStorageLimit:   cfg.SpawnBuildEphemeralStorageLimit,
		BuildAvoidNodes:              cfg.SpawnBuildAvoidNodes,
		MaxConcurrentBuilds:          cfg.SpawnMaxConcurrentBuilds,
	})
	if err != nil {
		return err
	}

	if cfg.SpawnRecoveryAuthority {
		spawnCfg.RecoveryAuthority = true
	}
	if cfg.SpawnProjects != "" {
		spawnCfg.Projects = strings.Split(cfg.SpawnProjects, ",")
	}
	if cfg.SpawnDefaultCPU > 0 {
		spawnCfg.DefaultCPUs = cfg.SpawnDefaultCPU
	}
	if cfg.SpawnDefaultMemory > 0 {
		spawnCfg.DefaultMemory = cfg.SpawnDefaultMemory
	}
	if cfg.SpawnMaxConcurrent > 0 {
		spawnCfg.MaxConcurrent = cfg.SpawnMaxConcurrent
	}
	if cfg.SpawnMaxConcurrentBuilds > 0 {
		spawnCfg.MaxConcurrentBuilds = cfg.SpawnMaxConcurrentBuilds
	}
	// Slice 2d: assemble the substrate backend map. K8s is the default and
	// always registered; the harvester-vm backend is optional and gated on
	// SpawnHarvesterKubeconfig being set. Mills opts in per-stage via
	// pipeline.stage_substrate (Slice 2a) → SpawnRequest.Substrate
	// (Slice 2b/2c) → o.substrateBackend(req.Substrate).
	backends := map[string]backend.Backend{
		DefaultSubstrate: spawnBackend,
	}
	if cfg.SpawnHarvesterKubeconfig != "" {
		hvm, herr := backend.NewHarvesterVMBackend(backend.HarvesterVMBackendConfig{
			KubeconfigPath:       cfg.SpawnHarvesterKubeconfig,
			BaseImageName:        cfg.SpawnHarvesterBaseImage,
			Namespace:            cfg.SpawnHarvesterNamespace,
			StorageClassName:     cfg.SpawnHarvesterStorageClass,
			NetworkAttachmentDef: cfg.SpawnHarvesterNetworkAttachDef,
			DefaultVCPUs:         cfg.SpawnHarvesterDefaultVCPUs,
			DefaultMemMi:         cfg.SpawnHarvesterDefaultMemMi,
			DefaultDiskGi:        cfg.SpawnHarvesterDefaultDiskGi,
			SSHUser:              cfg.SpawnHarvesterSSHUser,
			// Git clone config so Start can hydrate the VM workspace (the VM
			// disk IS the worktree). Reuses the same base URL + token Secret
			// the K8s git-clone init container uses. Without this the VM boots
			// with an empty /workspace and the agent fails with
			// `cd: <workdir>: No such file or directory` (Mills A2 kill-test).
			GitBaseURL: cfg.SpawnGitBaseURL,
			GitSecret:  cfg.SpawnGitSecret,
			// K8sBackend implements SecretResolver natively. Without it,
			// SecretEnv refs (CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY,
			// GEMINI_API_KEY) would be silently dropped on harvester-vm
			// spawns and the agent CLI would run without credentials.
			// Slice 2d.5.
			SecretResolver: spawnBackend,
		})
		if herr != nil {
			// Don't fail HUD startup over an optional substrate; log loud
			// and continue with k8s only. Mills runs that target
			// harvester-vm will fall back via substrateBackend's warn-log
			// path until the config is fixed.
			a.logger.Warn("harvester-vm substrate disabled (config error)", "error", herr)
		} else {
			backends["harvester-vm"] = hvm
			a.logger.Info("harvester-vm substrate enabled",
				"namespace", cfg.SpawnHarvesterNamespace,
				"base_image", cfg.SpawnHarvesterBaseImage)
		}
	}

	spawner := NewSpawnOrchestrator(
		backends, DefaultSubstrate, a.agent, a.sseHub, a.tracer, a.metrics, a.logger,
		spawnCfg,
	)

	ctrl := spawner.Controller()
	ctrl.SetK8sClient(spawnBackend.Clientset(), spawnBackend.Namespace())
	a.spawner = spawner
	a.fleetMonitor.SetSpawnLister(spawnAdapter{a.spawner})
	a.finishSpawnInit(ctx, spawner)
	return nil
}

// Spawn-state recovery pacing. Vars, not consts, so the regression tests can
// compress the schedule; production never mutates them.
var (
	spawnRecoverySyncAttempts = 5
	spawnRecoverySyncInitial  = 250 * time.Millisecond
	spawnRecoverySyncMax      = 5 * time.Second
	spawnRecoveryRetryInitial = 1 * time.Second
	spawnRecoveryRetryMax     = 1 * time.Minute
)

// finishSpawnInit runs the mandatory startup spawn-state recovery pass and
// starts the reconcile/prune loops once it succeeds.
//
// Restart recovery MUST complete before the reconcile loop starts: (a)
// RecoverSpawns previously had no caller at all, so resumePreRuntimeSpawns
// and recoverInterruptedSpawns (the loom-core#300 re-drive) were dead code on
// this path — an interrupted turn was never re-driven; and (b) the reconcile
// loop's discovered-untracked-pod path persists a LOSSY label-rebuilt record
// (empty request, no idempotency key) which, when it runs before recovery,
// clobbers the durable full record in the store — observed live on the S1c
// re-run: the keyed canary spawn's record lost its request and became
// un-re-drivable.
//
// An unreachable store, however, must NOT abort HUD init: observed live
// 2026-07-14, a transient "no route to host" to the k3s API at daemon boot
// failed recovery and 404'd every /api/* route for the process lifetime.
// Instead the orchestrator is marked degraded — Spawn refuses new work while
// read routes keep serving — and recovery retries in the background with
// capped exponential backoff until the store is reachable or ctx ends.
func (a *App) finishSpawnInit(ctx context.Context, spawner *SpawnOrchestrator) {
	err := poll.RetryWithBackoff(ctx, spawnRecoverySyncAttempts,
		spawnRecoverySyncInitial, spawnRecoverySyncMax, spawner.RecoverSpawnsContext)
	if err == nil {
		a.startSpawnLoops(ctx, spawner)
		return
	}
	spawner.SetDegraded(true)
	a.logger.Error("spawn state recovery failed; spawn backend degraded, retrying in background",
		"error", err)
	go a.retrySpawnRecovery(ctx, spawner)
}

// retrySpawnRecovery keeps retrying the startup recovery pass until it
// succeeds or ctx is cancelled, then clears the degraded flag and starts the
// reconcile/prune loops. Runs as a goroutine spawned by finishSpawnInit.
func (a *App) retrySpawnRecovery(ctx context.Context, spawner *SpawnOrchestrator) {
	delay := spawnRecoveryRetryInitial
	for {
		if err := poll.WaitWithContext(ctx, delay); err != nil {
			return
		}
		if err := spawner.RecoverSpawnsContext(ctx); err != nil {
			a.logger.Warn("spawn state recovery retry failed; spawn backend still degraded",
				"error", err)
			delay *= 2
			if delay > spawnRecoveryRetryMax {
				delay = spawnRecoveryRetryMax
			}
			continue
		}
		spawner.SetDegraded(false)
		a.logger.Info("spawn state recovered; spawn backend healthy")
		a.startSpawnLoops(ctx, spawner)
		return
	}
}

// startSpawnLoops starts the controller's reconcile and prune loops. Only
// call after RecoverSpawnsContext has succeeded (see finishSpawnInit).
func (a *App) startSpawnLoops(ctx context.Context, spawner *SpawnOrchestrator) {
	cfg := a.config
	ctrl := spawner.Controller()
	ctrl.StartReconcileLoop(ctx, 30*time.Second)
	// Periodic prune of terminal spawn records. Reconcile reaps the live
	// pod + presence + session via TerminalHook, but the State entry stays
	// in-memory and on-disk so the operator can still drill into a recent
	// failure. Without this loop the in-memory map and `~/.config/loom/spawns`
	// (or the cluster ConfigMap) accumulate indefinitely; the HUD spawn list
	// then surfaces "old orphan spawns" from days ago. 24h retention keeps a
	// useful triage window without unbounded growth; 10min cadence amortises
	// the disk I/O against the existing 30s reconcile tick.
	ctrl.StartPruneLoop(ctx, 10*time.Minute, 24*time.Hour)

	a.logger.Info("spawn orchestrator enabled",
		"namespace", cfg.SpawnNamespace, "registry", cfg.SpawnRegistry,
		"sync_mode", cfg.SpawnSyncMode, "projects", len(spawner.projects))
}

// initPushBridge sets up the APNs push notification bridge.
func (a *App) initPushBridge() {
	cfg := a.config
	apnsSender := NewAPNsSender(APNsSenderConfig{
		KeyPath: cfg.APNsKeyPath,
		KeyID:   cfg.APNsKeyID,
		TeamID:  cfg.APNsTeamID,
		Topic:   cfg.APNsTopic,
		Sandbox: cfg.APNsSandbox,
	}, a.tracer, a.metrics, a.logger).WithTokenStore(a.deviceTokenStore)

	a.pushBridge = NewPushEventBridge(
		apnsSender, a.deviceTokenStore, a.tracer, a.metrics, a.logger,
	)
	a.logger.Info("APNs push bridge enabled", "topic", cfg.APNsTopic, "sandbox", cfg.APNsSandbox)
}

// wireMonitorCallbacks registers OnRefresh callbacks that broadcast monitor
// snapshots to browser clients via the SSE hub.
func (a *App) wireMonitorCallbacks() {
	// Optional webhook pusher.
	var fleetWebhook *FleetWebhook
	if a.config.WebhookURL != "" {
		fleetWebhook = NewFleetWebhook(a.config.WebhookURL, a.config.WebhookToken, a.config.WebhookResolve, a.logger)
		a.logger.Info("fleet webhook enabled", "url", a.config.WebhookURL)
	}

	a.fleetMonitor.OnRefresh(func(snap monitor.FleetSnapshot) {
		data, err := json.Marshal(snap)
		if err == nil {
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-fleet-%d", time.Now().UnixMilli()),
				Type:      "hud.fleet",
				Timestamp: time.Now(),
				Data:      data,
			})
		}
		if fleetWebhook != nil {
			go fleetWebhook.Push(snap)
		}
	})
	a.healthMonitor.OnRefresh(func(servers []monitor.ServerHealthEntry) {
		data, err := json.Marshal(map[string]any{"servers": servers})
		if err != nil {
			return
		}
		a.sseHub.Broadcast(bridge.SSEEvent{
			ID:        fmt.Sprintf("hud-health-%d", time.Now().UnixMilli()),
			Type:      "hud.health",
			Timestamp: time.Now(),
			Data:      data,
		})
	})
	a.memoryMonitor.OnRefresh(func(stats *bridge.MemoryStatsResult) {
		data, err := json.Marshal(memory.StatsPayload(stats))
		if err != nil {
			return
		}
		a.sseHub.Broadcast(bridge.SSEEvent{
			ID:        fmt.Sprintf("hud-memory-%d", time.Now().UnixMilli()),
			Type:      "hud.memory",
			Timestamp: time.Now(),
			Data:      data,
		})
	})
	a.workflowMonitor.OnRefresh(func(workflows []bridge.WorkflowInfo) {
		data, err := json.Marshal(map[string]any{"workflows": workflows})
		if err != nil {
			return
		}
		a.sseHub.Broadcast(bridge.SSEEvent{
			ID:        fmt.Sprintf("hud-workflows-%d", time.Now().UnixMilli()),
			Type:      "hud.workflows",
			Timestamp: time.Now(),
			Data:      data,
		})
	})
	a.workflowMonitor.OnNewApproval(func(workflows []bridge.WorkflowInfo) {
		now := time.Now()
		for _, w := range workflows {
			data, err := json.Marshal(map[string]any{
				"workflow_id":  w.ID,
				"name":         w.Name,
				"current_step": w.CurrentStep,
			})
			if err != nil {
				continue
			}
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-workflow-approval-%s-%d", w.ID, now.UnixMilli()),
				Type:      "hud.workflow.waiting_approval",
				Timestamp: now,
				Data:      data,
			})
		}
	})
	a.streamMonitor.OnRefresh(func(entries []monitor.StreamEntry) {
		data, err := json.Marshal(map[string]any{"entries": entries})
		if err != nil {
			return
		}
		a.sseHub.Broadcast(bridge.SSEEvent{
			ID:        fmt.Sprintf("hud-stream-%d", time.Now().UnixMilli()),
			Type:      "hud.stream",
			Timestamp: time.Now(),
			Data:      data,
		})
	})
	a.sandboxMonitor.OnRefresh(func(snap map[string]any) {
		snap["available"] = true
		data, err := json.Marshal(snap)
		if err != nil {
			return
		}
		a.sseHub.Broadcast(bridge.SSEEvent{
			ID:        fmt.Sprintf("hud-sandbox-%d", time.Now().UnixMilli()),
			Type:      "hud.sandbox",
			Timestamp: time.Now(),
			Data:      data,
		})
	})
	a.costMonitor.OnRefresh(func(snap monitor.CostSnapshot) {
		data, err := json.Marshal(snap)
		if err != nil {
			return
		}
		a.sseHub.Broadcast(bridge.SSEEvent{
			ID:        fmt.Sprintf("hud-cost-%d", time.Now().UnixMilli()),
			Type:      "hud.cost",
			Timestamp: time.Now(),
			Data:      data,
		})
	})
	if a.otelMonitor != nil {
		a.otelMonitor.OnRefresh(func(snap bridge.OTelStatusResult) {
			data, err := json.Marshal(snap)
			if err != nil {
				return
			}
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-otel-%d", time.Now().UnixMilli()),
				Type:      "hud.otel",
				Timestamp: time.Now(),
				Data:      data,
			})
		})
	}
	if a.millsMonitor != nil {
		a.millsMonitor.OnRefresh(func(snap monitor.MillsSnapshot) {
			data, err := json.Marshal(snap)
			if err != nil {
				return
			}
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-mills-%d", time.Now().UnixMilli()),
				Type:      "hud.mills",
				Timestamp: time.Now(),
				Data:      data,
			})
		})
	}
	if a.pipelineMonitor != nil {
		a.pipelineMonitor.OnRefresh(func(pipelines []bridge.PipelineInfo) {
			data, err := json.Marshal(map[string]any{"pipelines": pipelines})
			if err != nil {
				return
			}
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-pipeline-%d", time.Now().UnixMilli()),
				Type:      "hud.pipeline",
				Timestamp: time.Now(),
				Data:      data,
			})
		})
	}
	if a.contextHealthMonitor != nil {
		a.contextHealthMonitor.OnRefresh(func(snap monitor.ContextHealthSnapshot) {
			data, err := json.Marshal(snap)
			if err != nil {
				return
			}
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-context-health-%d", time.Now().UnixMilli()),
				Type:      "hud.context_health",
				Timestamp: time.Now(),
				Data:      data,
			})
		})
	}
	if a.codebaseMonitor != nil {
		a.codebaseMonitor.OnRefresh(func(snap monitor.CodebaseSnapshot) {
			data, err := json.Marshal(snap)
			if err != nil {
				return
			}
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-codebase-%d", time.Now().UnixMilli()),
				Type:      "hud.codebase",
				Timestamp: time.Now(),
				Data:      data,
			})
		})
	}
	if a.shuttleMonitor != nil {
		a.shuttleMonitor.OnRefresh(func(snap shuttle.ShuttleSnapshot) {
			data, err := json.Marshal(snap)
			if err != nil {
				return
			}
			a.sseHub.Broadcast(bridge.SSEEvent{
				ID:        fmt.Sprintf("hud-shuttle-%d", time.Now().UnixMilli()),
				Type:      "hud.shuttle",
				Timestamp: time.Now(),
				Data:      data,
			})
		})
	}
}

// initCoordinator sets up the LLM-powered coordinator.
func (a *App) initCoordinator() {
	cfg := a.config
	coordCfg := coordinator.ConfigFromEnv()
	coordCfg.FlexInferURL = cfg.FlexInferURL
	if cfg.FlexInferKey != "" {
		coordCfg.FlexInferKey = cfg.FlexInferKey
	}
	if cfg.CoordinatorModel != "" {
		coordCfg.DefaultModel = cfg.CoordinatorModel
	}

	if err := coordCfg.Validate(); err != nil {
		a.logger.Error("coordinator config invalid", "error", err)
		return
	}

	c := coordinator.NewCoordinator(coordCfg, a.agent, a.sseHub, a.logger)
	if c == nil {
		return
	}
	m := coordinator.NewMetrics()
	c.SetMetrics(m)
	if err := c.Start(); err != nil {
		a.logger.Warn("coordinator: failed to start, continuing without it", "error", err)
		return
	}
	a.coordinator = c
	a.coordinatorMetrics = m
	a.logger.Info("coordinator started", "url", cfg.FlexInferURL, "model", coordCfg.DefaultModel)
}

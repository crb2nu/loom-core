// daemon_new.go contains the DefaultConfig and New constructor for the Daemon.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/internal/hubproto"
	"github.com/crb2nu/loom/internal/pool"
	"github.com/crb2nu/loom/internal/process"
	"github.com/crb2nu/loom/internal/router"
	"github.com/crb2nu/loom/pkg/mcpotel"
	"github.com/crb2nu/loom/pkg/netmode"
	"github.com/crb2nu/loom/pkg/profiles"
	"github.com/crb2nu/loom/pkg/registry"
	"github.com/crb2nu/loom/pkg/sync"
	loomtransport "github.com/crb2nu/loom/pkg/transport"
)

// DefaultConfig returns the default daemon configuration.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	cfg := Config{
		SocketPath:   filepath.Join(home, ".config", "loom", "loom.sock"),
		RegistryPath: "",
		Target:       "codex",
		HubURL:       "wss://mcp.flexinfer.ai/ws",
		HubFallback:  true,
		HubPrefer:    false,
		WarmOnStart:  nil,
		Debug:        false,
	}
	if home != "" {
		cfg.EnvFilePath = filepath.Join(home, ".config", "loom", "hud.env")
	}
	return cfg
}

func dialHubTransport(
	ctx context.Context,
	wsConfig mcp.WebSocketConfig,
	serverName string,
	registry *hubTransportRegistry,
	onNotification func(string, *mcp.Message),
) (mcp.Transport, error) {
	rawTransport, err := mcp.NewWebSocketTransport(ctx, wsConfig, serverName)
	if err != nil {
		return nil, fmt.Errorf("connect hub transport: %w", err)
	}
	transport, err := registry.Track(serverName, rawTransport, onNotification)
	if err != nil {
		return nil, fmt.Errorf("track hub transport: %w", err)
	}
	if err := initializeMCPTransportWithTimeout(ctx, transport, hubInitTimeout); err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("initialize hub transport: %w", err)
	}
	return transport, nil
}

// New creates a new daemon instance.
func New(cfg Config) (*Daemon, error) {
	// Load config file and merge with CLI config (CLI takes precedence)
	fileCfg, err := LoadConfigFile()
	if err != nil {
		// Log but don't fail - use CLI config
		fmt.Fprintf(os.Stderr, "Warning: failed to load config file: %v\n", err)
	} else {
		// Apply file config where CLI config is not set
		if cfg.HubURL == "" || cfg.HubURL == DefaultConfig().HubURL {
			if fileCfg.Hub.URL != "" {
				cfg.HubURL = fileCfg.Hub.URL
			}
		}
		if !cfg.HubFallback && fileCfg.Hub.Enabled {
			cfg.HubFallback = fileCfg.Hub.Enabled
		}
		if !cfg.HubPrefer && fileCfg.Hub.PreferHub {
			cfg.HubPrefer = fileCfg.Hub.PreferHub
		}
		if cfg.Target == "" || cfg.Target == DefaultConfig().Target {
			if fileCfg.Hub.Profile != "" {
				cfg.Target = fileCfg.Hub.Profile
			}
		}
		if !cfg.Debug && fileCfg.Debug {
			cfg.Debug = fileCfg.Debug
		}
	}

	// Set up logger
	var handler slog.Handler
	if cfg.Debug {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	logger := slog.New(handler)

	otelTP, otelShutdown, otelRuntimeState, otelErr := initDaemonOTel(context.Background(), fileCfg.OTel, logger)
	if otelErr != nil {
		logger.Warn("failed to initialize daemon OTel tracing", "error", otelErr)
	}
	runtimeTracer := mcpotel.Tracer(otelTP, otelRuntimeState.ServiceName)

	// Load registry
	var reg *registry.Registry
	var repoRoot string

	// Use repo_root from config if set
	if fileCfg.RepoRoot != "" {
		repoRoot = fileCfg.RepoRoot
		logger.Debug("using configured repo root", "path", repoRoot)
	}

	// Determine registry path: explicit > auto-discover
	registryPath := cfg.RegistryPath
	if registryPath == "" {
		if path, found := registry.FindRegistry(); found {
			registryPath = path
		}
	}
	cfg.RegistryPath = registryPath

	if registryPath != "" {
		var err error
		reg, err = registry.LoadWithDefaults(registryPath)
		if err != nil {
			return nil, fmt.Errorf("load registry: %w", err)
		}
		reg, err = runtimeRegistryForTarget(reg, cfg.Target)
		if err != nil {
			return nil, fmt.Errorf("normalize runtime registry: %w", err)
		}
		reg = applyCatalogState(reg, logger)
		logger.Info("loaded registry", "path", registryPath, "servers", len(reg.Servers))

		// If repo_root not set in config, derive from registry path
		if repoRoot == "" {
			repoRoot = registry.GetRepoRoot(registryPath)
			logger.Debug("derived repo root", "path", repoRoot)
		}
	}

	// d will be set once the Daemon struct is created (below). Closures below
	// capture this pointer so runtime expansion and process/event behavior can
	// follow reloaded daemon state.
	var d *Daemon

	// LOOM_MUX_STDIO controls the per-id muxing wrapper around local stdio
	// transports (pkg/transport/muxstdio). When enabled, every pool.Conn for
	// the same serverName shares one *muxstdio.Transport and the per-server
	// callLock is skipped for TargetLocal — see callpipeline_routing.go and
	// .loom/implementation-plan-stdio-mux-2026-05-20.md slice 3.
	//
	// Defaults to ON as of the S3-followup flip (2026-05-20). The S3 MR
	// (!460) shipped the path default-off; this flip makes it the active
	// behavior. Operators who hit a regression can opt out by setting
	// LOOM_MUX_STDIO=0 (also accepts "false" / "off") — the daemon then
	// falls back to the pre-S3 callLock-based path with no other changes
	// needed. The plan's R3 risk-register entry covers this rollback.
	muxStdioRaw := strings.TrimSpace(os.Getenv("LOOM_MUX_STDIO"))
	muxStdioEnabled := !strings.EqualFold(muxStdioRaw, "0") &&
		!strings.EqualFold(muxStdioRaw, "false") &&
		!strings.EqualFold(muxStdioRaw, "off")
	if muxStdioEnabled {
		logger.Info("local stdio per-id muxing enabled (LOOM_MUX_STDIO default on; set =0 to disable)")
	} else {
		logger.Info("local stdio per-id muxing disabled (LOOM_MUX_STDIO=0); using legacy callLock path")
	}

	// Create process manager with variable expansion (using the daemon's current
	// registry so reloads immediately affect env/template expansion).
	procMgr := process.NewManager(reg, cfg.Target)
	procMgr.SetExpandFunc(func(s string) string {
		if d != nil {
			return expandVarsWithRegistry(s, d.repoRoot, d.currentRegistry())
		}
		return expandVarsWithRegistry(s, repoRoot, reg)
	})
	// Production generations use one dependency Manager shard per server key.
	// Keep procMgr as the compatibility path for focused tests and legacy daemon
	// construction, but do not put fleet traffic through its global lifecycle
	// mutex.
	localProcController := newShardedProcessController(reg, cfg.Target, repoRoot)

	// Create connection pool for local servers
	poolMaxIdle, poolMaxOpen, poolIdleTimeout, poolWaitTimeout := fileCfg.Resources.GetPoolConfig()
	connPool := pool.New(pool.Config{
		MaxIdle:     poolMaxIdle,
		MaxOpen:     poolMaxOpen,
		IdleTimeout: poolIdleTimeout,
		WaitTimeout: poolWaitTimeout,
		DialFunc: func(ctx context.Context, serverName string) (mcp.Transport, error) {
			tracer := runtimeTracer
			if d != nil {
				tracer = d.daemonTracer()
			}
			_, span := tracer.Start(ctx, "daemon.server.connect",
				trace.WithAttributes(attribute.String("server.name", serverName)),
			)
			defer span.End()

			if d == nil || d.serverSupervisor == nil {
				err := fmt.Errorf("server supervisor unavailable")
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return nil, err
			}
			transport, generationID, started, err := d.serverSupervisor.connectionRegistered(ctx, serverName, func(readyGeneration uint64) {
				d.runningServers.Store(serverName, readyGeneration)
				if d.eventBus != nil {
					d.eventBus.Publish(EventProcessStart, map[string]any{
						"server":     serverName,
						"generation": readyGeneration,
					})
				}
			})
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return nil, err
			}
			span.SetAttributes(
				attribute.Bool("server.was_running", !started),
				attribute.Bool("server.started", started),
				attribute.Int64("server.generation", int64(generationID)),
			)
			return transport, nil
		},
	})

	// Create hub client if hub fallback is enabled
	var hubClient *mcp.WebSocketClient
	var hubPool *pool.Pool
	var hubTransports *hubTransportRegistry
	if cfg.HubFallback && cfg.HubURL != "" {
		hubWSConfig := mcp.WebSocketConfig{
			URL:                  cfg.HubURL,
			Profile:              cfg.Target,
			CFAccessClientID:     fileCfg.Hub.CFAccessClientID,
			CFAccessClientSecret: fileCfg.Hub.CFAccessClientSecret,
			ConnectTimeout:       10 * time.Second,
		}
		hubClient = mcp.NewWebSocketClient(hubWSConfig)
		hubMaxIdle, hubMaxOpen, hubIdleTimeout, hubWaitTimeout := fileCfg.Resources.GetHubPoolConfig()
		hubTransports = newHubTransportRegistry()
		reconnectPolicy := loomtransport.KeepaliveConfig{
			PingInterval:   time.Duration(fileCfg.Hub.PingIntervalSeconds) * time.Second,
			MaxMissedPongs: fileCfg.Hub.MaxMissedPongs,
			BackoffInitial: time.Duration(fileCfg.Hub.ReconnectIntervalSeconds) * time.Second,
			BackoffMax:     time.Duration(fileCfg.Hub.MaxReconnectBackoffSeconds) * time.Second,
			Jitter:         0.2,
		}
		reconnectBackoff := loomtransport.NewBackoff(reconnectPolicy, rand.NewSource(time.Now().UnixNano())) // #nosec G404 -- reconnect jitter is non-cryptographic
		hubPool = pool.New(pool.Config{
			MaxIdle:     hubMaxIdle,
			MaxOpen:     hubMaxOpen,
			IdleTimeout: hubIdleTimeout,
			WaitTimeout: hubWaitTimeout,
			DialFunc: func(ctx context.Context, serverName string) (mcp.Transport, error) {
				var lastErr error
				for attempt := 0; attempt < fileCfg.Hub.MaxRetries; attempt++ {
					transport, err := dialHubTransport(ctx, hubWSConfig, serverName, hubTransports, d.handleHubNotification)
					if err == nil {
						return transport, nil
					}
					lastErr = err
					if attempt+1 >= fileCfg.Hub.MaxRetries {
						break
					}
					delay := reconnectBackoff.Delay(attempt)
					timer := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						timer.Stop()
						return nil, ctx.Err()
					case <-timer.C:
					}
				}
				return nil, lastErr
			},
		})
		logger.Info("hub fallback enabled", "url", cfg.HubURL)
	}

	// Create router
	rtr := router.New(router.Config{
		Registry:         reg,
		HubEnabled:       cfg.HubFallback && hubClient != nil,
		HubURL:           cfg.HubURL,
		FailureThreshold: 3,
		RecoveryTime:     30 * time.Second,
	})

	// Build per-server routing preferences.
	// Sources: explicit routing.preferences config, and the legacy hub.prefer flag.
	routingPrefs := make(map[string]RoutingPreference)

	// Load explicit per-server preferences from config file
	if fileCfg.Routing.Preferences != nil {
		if err := ValidateRoutingPreferences(fileCfg.Routing.Preferences); err != nil {
			logger.Warn("invalid routing preferences in config", "error", err)
		} else {
			for server, pref := range fileCfg.Routing.Preferences {
				p, _ := ParseRoutingPreference(pref)
				routingPrefs[server] = p
			}
		}
	}

	// Legacy: hub.prefer flag -> apply prefer-hub to all hub-capable servers
	// that don't already have an explicit preference.
	if cfg.HubPrefer && cfg.HubFallback && hubClient != nil {
		for _, srv := range reg.Servers {
			if srv == nil || srv.IsLocalOnly() {
				continue
			}
			if _, exists := routingPrefs[srv.Name]; !exists {
				routingPrefs[srv.Name] = RoutingPreferHub
			}
		}
		logger.Info("hub prefer enabled via legacy flag, converted to per-server prefer-hub")
	}

	// Off-LAN routing: when the laptop is off the home LAN, local backends that
	// depend on LAN-only services (qdrant, *.lan, k8s) are unreachable, so treat
	// local-only / prefer-local pins as prefer-hub for hub-capable servers. The
	// hub (wss://mcp.flexinfer.ai/ws) runs those backends cluster-side and is
	// reachable through Cloudflare Access from anywhere. Gated on a configured hub.
	//
	// The configured pins are never mutated: the upgrade is applied per call via
	// effectiveRoutingPreference from the offLAN flag, seeded here and kept
	// current by netmodeWatchLoop. (A startup-only mutation once flipped
	// agent_context's prefer-local pin for the daemon's whole lifetime because
	// the single 600ms sentinel probe raced a transient network outage.)
	var offLANHubCapable map[string]bool
	if hubClient != nil && cfg.HubFallback {
		offLANHubCapable = make(map[string]bool, len(reg.Servers))
		for _, srv := range reg.Servers {
			if srv == nil {
				continue
			}
			offLANHubCapable[srv.Name] = !srv.IsLocalOnly()
		}
	}

	if len(routingPrefs) > 0 {
		logger.Info("routing preferences loaded", "count", len(routingPrefs))
	}

	// Create manifest manager for persistent tool cache
	manifest := NewManifestManager()

	// Create profiles manager for tool filtering
	profileMgr := profiles.NewManager()
	if fileCfg.Context.CustomProfilePath != "" {
		if err := profileMgr.LoadFromFile(fileCfg.Context.CustomProfilePath); err != nil {
			logger.Warn("failed to load custom profiles", "path", fileCfg.Context.CustomProfilePath, "error", err)
		}
	}

	// Create sync manager for profile operations
	var syncMgr *sync.Manager
	if repoRoot != "" {
		var err error
		syncMgr, err = sync.NewManager(repoRoot)
		if err != nil {
			logger.Warn("failed to create sync manager", "error", err)
		}
	}

	// Load tool metadata for enhanced descriptions
	toolMetadata, err := registry.LoadEmbeddedMetadata()
	if err != nil {
		logger.Warn("failed to load tool metadata", "error", err)
	} else {
		logger.Debug("loaded tool metadata", "servers", len(toolMetadata.Servers))
	}

	// Determine cache TTL from config
	cacheTTL := fileCfg.Resources.GetManifestTTL()

	d = &Daemon{
		cfg:                 cfg,
		fileCfg:             fileCfg,
		daemonEpoch:         1,
		registry:            reg,
		repoRoot:            repoRoot,
		procMgr:             procMgr,
		localProcController: localProcController,
		pool:                connPool,
		hubPool:             hubPool,
		router:              rtr,
		hubRouter:           hubproto.NewRouter(),
		hubClient:           hubClient,
		logger:              logger,
		toolCache: &ToolCache{
			ttl: cacheTTL,
		},
		resourceCache: &ResourceCache{
			ttl: cacheTTL,
		},
		manifest:           manifest,
		profiles:           profileMgr,
		metadata:           toolMetadata,
		syncManager:        syncMgr,
		metrics:            NewMetrics(),
		respCache:          NewResponseCache(fileCfg.Cache),
		routingPreferences: routingPrefs,
		offLANHubCapable:   offLANHubCapable,
		done:               make(chan struct{}),
		tracer:             runtimeTracer,
		otelShutdown:       otelShutdown,
		otelRuntimeState:   otelRuntimeState,
		muxStdio:           muxStdioEnabled,
		hubTransports:      hubTransports,
	}

	// Seed the off-LAN flag with one startup probe. netmodeWatchLoop (started
	// in Start) re-probes and self-heals a wrong result within one interval.
	if offLANHubCapable != nil && netmode.Resolve() == netmode.Tunnel {
		d.offLAN.Store(true)
		logger.Info("off-LAN detected at startup: routing LAN-pinned servers via hub",
			"servers", d.offLANPinnedServers())
	}

	// Initialize daemon-wide call concurrency semaphore
	if maxCalls := fileCfg.Resources.GetMaxConcurrentCalls(); maxCalls > 0 {
		d.callSem = make(chan struct{}, maxCalls)
		logger.Info("daemon-wide call concurrency limit enabled", "max_concurrent_calls", maxCalls)
	}

	// Initialize event bus for SSE streaming
	d.eventBus = NewEventBus(logger)
	d.registrySnapshot.Store(reg)
	d.registryEpoch.Store(1)
	d.serverSupervisor = newServerSupervisor(localProcController, muxStdioEnabled, logger, func(serverName string, generationID uint64) {
		value, ok := d.runningServers.Load(serverName)
		if !ok {
			return
		}
		observed, typed := value.(uint64)
		if !typed || observed == generationID {
			d.runningServers.CompareAndDelete(serverName, value)
		}
	})
	d.serverSupervisor.registryEpoch = d.registryEpoch.Load

	// Initialize RBAC enforcer (nil when disabled)
	d.rbac = NewRBACEnforcer(fileCfg.RBAC, logger)
	if d.rbac != nil {
		logger.Info("RBAC enabled",
			"default_policy", fileCfg.RBAC.DefaultPolicy,
			"roles", len(fileCfg.RBAC.Roles),
			"bindings", len(fileCfg.RBAC.Bindings),
			"global_deny", len(fileCfg.RBAC.GlobalDeny),
			"rate_limits", len(fileCfg.RBAC.RateLimits))
	}
	d.policy = NewGatewayPolicyEnforcer(fileCfg.Policy, logger)
	if d.policy != nil {
		logger.Info("gateway policy enabled",
			"request_rules", len(fileCfg.Policy.Request))
	}

	// Initialize audit logger (nil when disabled)
	auditLogger, err := NewAuditLogger(fileCfg.Audit, logger)
	if err != nil {
		logger.Warn("failed to initialize audit logger", "error", err)
	}
	d.audit = auditLogger

	// Initialize cost tracker (nil when disabled)
	d.cost = NewCostTracker(fileCfg.Cost, logger)

	// Initialize OTel metric instruments (noop when provider not configured)
	d.otelMetrics = NewDaemonOTelMetrics()

	// Initialize OAuth 2.1 authorization server (nil when disabled)
	if fileCfg.HTTP.OAuth.Enabled && cfg.HTTPAddr != "" {
		issuer := fileCfg.HTTP.OAuth.Issuer
		if issuer == "" {
			scheme := "http"
			if fileCfg.HTTP.TLSCertFile != "" {
				scheme = "https"
			}
			issuer = scheme + "://localhost" + cfg.HTTPAddr
		}
		d.oauth = NewOAuthServer(fileCfg.HTTP.OAuth, issuer, logger)
	}

	// Initialize health monitor
	d.healthMonitor = NewHealthMonitor(d, fileCfg.Health.ToHealthMonitorConfig())

	// Initialize tunnel manager
	d.tunnelMgr = NewTunnelManager(DefaultTunnelManagerConfig(), logger)

	return d, nil
}

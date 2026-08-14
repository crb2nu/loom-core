// Package daemon provides the main Loom daemon orchestrator.
package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/crb2nu/loom/internal/daemon/generation"
	"github.com/crb2nu/loom/pkg/registry"
)

// ServerHealthStatus represents the health state of a server.
type ServerHealthStatus struct {
	Name              string    `json:"name"`
	Healthy           bool      `json:"healthy"`
	LastCheck         time.Time `json:"last_check"`
	LastHealthy       time.Time `json:"last_healthy,omitempty"`
	ConsecutiveFails  int       `json:"consecutive_fails"`
	TotalChecks       int       `json:"total_checks"`
	TotalFailures     int       `json:"total_failures"`
	AvgLatencyMs      float64   `json:"avg_latency_ms"`
	LastError         string    `json:"last_error,omitempty"`
	RestartCount      int       `json:"restart_count"`
	LastRestart       time.Time `json:"last_restart,omitempty"`
	AutoRestartFailed bool      `json:"auto_restart_failed,omitempty"`
	LastDeepProbe     time.Time `json:"last_deep_probe,omitempty"`
	LastFailure       time.Time `json:"last_failure,omitempty"`
}

// HealthMonitor monitors server health and handles auto-restarts.
type HealthMonitor struct {
	daemon   *Daemon
	logger   *slog.Logger
	statuses map[string]*ServerHealthStatus
	mu       sync.RWMutex

	// Configuration
	checkInterval      time.Duration
	deepProbeInterval  time.Duration // interval between full process-spawning probes
	deepProbeTimeout   time.Duration // timeout for a single deep (process-spawning) probe
	healthyThreshold   int           // consecutive successes to mark healthy
	unhealthyThreshold int           // consecutive failures to mark unhealthy
	restartThreshold   int           // failures before auto-restart
	maxRestarts        int           // max restarts before giving up
	restartCooldown    time.Duration

	// Restart hysteresis: when many distinct servers fail probes within a short
	// window, the failure is systemic (host/hub under load) rather than a single
	// broken process, so restarting healthy local servers is counterproductive.
	restartPressureThreshold int           // distinct servers failing in window to suppress restarts (0 = disabled)
	restartPressureWindow    time.Duration // rolling window for the systemic-pressure signal

	// Control
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// defaultDeepProbeTimeout bounds a single deep (process-spawning) health probe.
// Matches defaultDaemonControlRPCTimeout so a slow subprocess start under load is
// not clipped at the old hardcoded 10s and mistaken for an unhealthy server.
const defaultDeepProbeTimeout = 30 * time.Second

// defaultRestartPressureThreshold is the number of distinct servers that must
// fail a probe within defaultRestartPressureWindow before auto-restart is
// suppressed as systemic. Three distinct servers failing together rarely
// coincides outside a real host/hub-wide event, so normal single-server
// failures are unaffected.
const defaultRestartPressureThreshold = 3

// defaultRestartPressureWindow bounds the rolling window for the systemic
// failure-pressure signal.
const defaultRestartPressureWindow = 60 * time.Second

// HealthMonitorConfig holds configuration for the health monitor.
type HealthMonitorConfig struct {
	CheckInterval      time.Duration
	DeepProbeInterval  time.Duration // how often to run a full process-spawning probe (0 = every check)
	DeepProbeTimeout   time.Duration // timeout for a single deep probe (0 = default 30s)
	HealthyThreshold   int
	UnhealthyThreshold int
	RestartThreshold   int
	MaxRestarts        int
	RestartCooldown    time.Duration
	// RestartPressureThreshold is the number of distinct servers failing within
	// RestartPressureWindow that suppresses auto-restart (0 = disabled).
	RestartPressureThreshold int
	// RestartPressureWindow is the rolling window for the systemic-pressure
	// signal (0 = default 60s).
	RestartPressureWindow time.Duration
}

// DefaultHealthMonitorConfig returns sensible defaults.
func DefaultHealthMonitorConfig() HealthMonitorConfig {
	return HealthMonitorConfig{
		CheckInterval:            30 * time.Second,
		DeepProbeInterval:        5 * time.Minute,
		DeepProbeTimeout:         defaultDeepProbeTimeout,
		HealthyThreshold:         2,
		UnhealthyThreshold:       3,
		RestartThreshold:         3,
		MaxRestarts:              3,
		RestartCooldown:          5 * time.Minute,
		RestartPressureThreshold: defaultRestartPressureThreshold,
		RestartPressureWindow:    defaultRestartPressureWindow,
	}
}

// NewHealthMonitor creates a new health monitor.
func NewHealthMonitor(daemon *Daemon, cfg HealthMonitorConfig) *HealthMonitor {
	deepProbeTimeout := cfg.DeepProbeTimeout
	if deepProbeTimeout <= 0 {
		deepProbeTimeout = defaultDeepProbeTimeout
	}
	restartPressureWindow := cfg.RestartPressureWindow
	if restartPressureWindow <= 0 {
		restartPressureWindow = defaultRestartPressureWindow
	}
	monitorCtx, monitorCancel := context.WithCancel(context.Background())
	return &HealthMonitor{
		daemon:                   daemon,
		logger:                   daemon.logger.With("component", "health-monitor"),
		statuses:                 make(map[string]*ServerHealthStatus),
		checkInterval:            cfg.CheckInterval,
		deepProbeInterval:        cfg.DeepProbeInterval,
		deepProbeTimeout:         deepProbeTimeout,
		healthyThreshold:         cfg.HealthyThreshold,
		unhealthyThreshold:       cfg.UnhealthyThreshold,
		restartThreshold:         cfg.RestartThreshold,
		maxRestarts:              cfg.MaxRestarts,
		restartCooldown:          cfg.RestartCooldown,
		restartPressureThreshold: cfg.RestartPressureThreshold,
		restartPressureWindow:    restartPressureWindow,
		done:                     make(chan struct{}),
		ctx:                      monitorCtx,
		cancel:                   monitorCancel,
	}
}

// Start begins the health monitoring loop.
func (h *HealthMonitor) Start() {
	h.wg.Add(1)
	go h.monitorLoop()
}

// Stop gracefully stops the health monitor.
func (h *HealthMonitor) Stop() {
	h.stopOnce.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		if h.done != nil {
			close(h.done)
		}
	})
	h.wg.Wait()
}

// GetStatus returns the health status for a server.
func (h *HealthMonitor) GetStatus(serverName string) *ServerHealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if status, ok := h.statuses[serverName]; ok {
		// Return a copy
		copy := *status
		return &copy
	}
	return nil
}

// GetAllStatuses returns health status for all monitored servers.
func (h *HealthMonitor) GetAllStatuses() map[string]*ServerHealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make(map[string]*ServerHealthStatus, len(h.statuses))
	for name, status := range h.statuses {
		copy := *status
		result[name] = &copy
	}
	return result
}

// monitorLoop runs the health check loop.
func (h *HealthMonitor) monitorLoop() {
	defer h.wg.Done()

	ticker := time.NewTicker(h.checkInterval)
	defer ticker.Stop()

	// Initial check
	h.checkAllServers()

	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			h.checkAllServers()
		}
	}
}

// checkAllServers performs health checks on all registered servers.
func (h *HealthMonitor) checkAllServers() {
	reg := h.daemon.currentRegistry()
	if reg == nil {
		h.reconcileRegistry(nil)
		return
	}
	h.reconcileRegistry(reg)

	// Budget the sweep for the deep probe (the long pole). Pool probes return
	// fast; only the interval-gated, running-server-only deep probe needs the
	// full timeout, so a slow subprocess start isn't clipped into a false
	// unhealthy verdict. Floor at 10s for the legacy minimum.
	budget := h.deepProbeTimeout
	if budget < 10*time.Second {
		budget = 10 * time.Second
	}
	baseCtx := h.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, budget)
	defer cancel()

	var wg sync.WaitGroup

	for _, server := range reg.Servers {
		if server == nil {
			continue
		}
		wg.Add(1)
		go func(serverName string) {
			defer wg.Done()
			h.checkServer(ctx, serverName)
		}(server.Name)
	}

	wg.Wait()
}

// reconcileRegistry removes health rows for servers that no longer exist in
// the currently published registry. Without this, a reload can leave removed
// servers permanently counted in /health while Summary.Total reflects only
// the new registry.
func (h *HealthMonitor) reconcileRegistry(reg *registry.Registry) {
	if h == nil {
		return
	}
	current := make(map[string]struct{})
	if reg != nil {
		current = make(map[string]struct{}, len(reg.Servers))
		for _, server := range reg.Servers {
			if server != nil {
				current[server.Name] = struct{}{}
			}
		}
	}

	h.mu.Lock()
	for serverName := range h.statuses {
		if _, ok := current[serverName]; !ok {
			delete(h.statuses, serverName)
		}
	}
	h.mu.Unlock()
}

// needsDeepProbe returns true when the server requires a full process-spawning
// health probe (either no previous deep probe, or the interval has elapsed).
func (h *HealthMonitor) needsDeepProbe(status *ServerHealthStatus) bool {
	if h.deepProbeInterval <= 0 {
		return true // deep probes disabled → always deep
	}
	return status == nil || status.LastDeepProbe.IsZero() || time.Since(status.LastDeepProbe) >= h.deepProbeInterval
}

// checkServer performs a health check on a single server.
// It uses a lightweight pool-based probe when an idle connection exists and
// falls back to a full process-spawning deep probe on the configured interval
// or when the pool probe fails.
func (h *HealthMonitor) checkServer(ctx context.Context, serverName string) {
	start := time.Now()
	var observedGeneration uint64
	if h.daemon != nil && h.daemon.serverSupervisor != nil {
		if snapshot, ok := h.daemon.serverSupervisor.current(serverName); ok {
			observedGeneration = snapshot.Generation
		}
	}

	h.mu.RLock()
	existing := h.statuses[serverName]
	h.mu.RUnlock()

	// Health monitoring observes resident generations; it must not create a
	// process that idle retirement (or an explicit stop) intentionally removed.
	// The next real call owns cold-start publication.
	_, running := h.daemon.runningServers.Load(serverName)
	if !running {
		return
	}
	if h.daemon.serverSupervisor != nil {
		snapshot, ready := h.daemon.serverSupervisor.current(serverName)
		if !ready || snapshot.State != generation.StateReady {
			return
		}
	}

	deep := h.needsDeepProbe(existing)
	var err error
	if !deep {
		_, err = h.daemon.fetchServerToolsViaPool(ctx, serverName)
		if err != nil {
			// Pool probe failed. Only escalate to a deep (process-spawning) probe
			// if the server process is currently running. Servers that have been
			// reaped for idleness are not "unhealthy" — they just aren't running.
			// Starting them speculatively to health-check wastes resources and
			// triggers false-unhealthy alerts for slow-starting servers (e.g. devbox).
			if running {
				h.logger.Debug("pool probe failed, escalating to deep probe",
					"server", serverName, "error", err)
				deep = true
				_, err = h.daemon.fetchServerToolsWithTimeout(ctx, serverName, h.deepProbeTimeout)
			} else {
				h.logger.Debug("pool probe failed, server not running, skipping deep probe",
					"server", serverName)
				return // Not running → not unhealthy, just idle.
			}
		}
	} else {
		// For scheduled deep probes, also skip if the server isn't running.
		if !running {
			return
		}
		_, err = h.daemon.fetchServerToolsWithTimeout(ctx, serverName, h.deepProbeTimeout)
	}

	latencyMs := float64(time.Since(start).Milliseconds())
	now := time.Now()
	if !registryContainsServer(h.daemon.currentRegistry(), serverName) {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	status, ok := h.statuses[serverName]
	if !ok {
		status = &ServerHealthStatus{
			Name:    serverName,
			Healthy: true,
		}
		h.statuses[serverName] = status
	}

	status.LastCheck = now
	status.TotalChecks++

	// Update rolling average latency
	if status.TotalChecks == 1 {
		status.AvgLatencyMs = latencyMs
	} else {
		// Exponential moving average
		alpha := 0.2
		status.AvgLatencyMs = alpha*latencyMs + (1-alpha)*status.AvgLatencyMs
	}

	if err != nil {
		status.ConsecutiveFails++
		status.TotalFailures++
		status.LastError = err.Error()
		status.LastFailure = now

		// Update Prometheus metrics
		if h.daemon.metrics != nil {
			h.daemon.metrics.ServerHealth.WithLabelValues(serverName, "local").Set(0)
			h.daemon.metrics.ServerFailures.WithLabelValues(serverName, "local", "health_check").Inc()
		}

		// Check if we should mark as unhealthy
		if status.ConsecutiveFails >= h.unhealthyThreshold && status.Healthy {
			status.Healthy = false
			h.logger.Warn("server marked unhealthy",
				"server", serverName,
				"consecutive_failures", status.ConsecutiveFails,
				"error", err)

			// Emit health event
			if h.daemon.eventBus != nil {
				h.daemon.eventBus.Publish(EventServerHealth, map[string]any{
					"server":  serverName,
					"healthy": false,
					"error":   err.Error(),
				})
			}
		}

		// Check if we should auto-restart. Under systemic failure pressure
		// (many distinct servers failing at once → host/hub overload, not a
		// single broken process), suppress the restart: the server stays marked
		// unhealthy for visibility and the next sweep re-evaluates once pressure
		// subsides. Restarting healthy local servers during a transport storm is
		// the documented collateral-restart failure mode (.loom/149 Slice 4).
		if status.ConsecutiveFails >= h.restartThreshold && !status.AutoRestartFailed {
			if suppress, pressure := h.shouldSuppressRestartLocked(now); suppress {
				h.logger.Warn("auto-restart suppressed under systemic failure pressure",
					"server", serverName,
					"failing_servers", pressure,
					"pressure_threshold", h.restartPressureThreshold,
					"window", h.restartPressureWindow)
				if h.daemon.eventBus != nil {
					h.daemon.eventBus.Publish(EventServerHealth, map[string]any{
						"server":             serverName,
						"healthy":            false,
						"restart_suppressed": true,
						"failing_servers":    pressure,
					})
				}
				return
			}
			h.handleRestart(serverName, status, observedGeneration)
		}
	} else {
		// Success
		wasUnhealthy := !status.Healthy
		status.ConsecutiveFails = 0
		status.LastHealthy = now
		status.LastError = ""
		if deep {
			status.LastDeepProbe = now
		}

		// Update Prometheus metrics
		if h.daemon.metrics != nil {
			h.daemon.metrics.ServerHealth.WithLabelValues(serverName, "local").Set(1)
			h.daemon.metrics.ServerSuccesses.WithLabelValues(serverName, "local").Inc()
			h.daemon.metrics.ServerLatency.WithLabelValues(serverName, "local").Set(latencyMs)
		}

		// Mark as healthy after threshold successes
		if !status.Healthy {
			// Count as one success toward recovery
			if wasUnhealthy {
				status.Healthy = true
				status.AutoRestartFailed = false
				h.logger.Info("server recovered", "server", serverName)

				// Emit recovery event
				if h.daemon.eventBus != nil {
					h.daemon.eventBus.Publish(EventServerHealth, map[string]any{
						"server":  serverName,
						"healthy": true,
					})
				}
			}
		}
	}
}

func registryContainsServer(reg *registry.Registry, serverName string) bool {
	if reg == nil {
		return false
	}
	for _, server := range reg.Servers {
		if server != nil && server.Name == serverName {
			return true
		}
	}
	return false
}

// countRecentlyFailedServersLocked returns the number of distinct servers whose
// most recent probe failed within restartPressureWindow of now. It is the
// systemic failure-pressure signal used to suppress collateral restarts during
// a host/hub-wide event. Callers must hold h.mu.
func (h *HealthMonitor) countRecentlyFailedServersLocked(now time.Time) int {
	count := 0
	for _, status := range h.statuses {
		if status.LastFailure.IsZero() {
			continue
		}
		if now.Sub(status.LastFailure) <= h.restartPressureWindow {
			count++
		}
	}
	return count
}

// shouldSuppressRestartLocked reports whether auto-restart should be skipped
// because the failure is systemic (>= restartPressureThreshold distinct servers
// failing within the window) rather than a single broken process. Returns the
// current pressure count for logging. Hysteresis is disabled when the threshold
// is <= 0. Callers must hold h.mu.
func (h *HealthMonitor) shouldSuppressRestartLocked(now time.Time) (bool, int) {
	if h.restartPressureThreshold <= 0 {
		return false, 0
	}
	pressure := h.countRecentlyFailedServersLocked(now)
	return pressure >= h.restartPressureThreshold, pressure
}

// handleRestart attempts to restart an unhealthy server.
func (h *HealthMonitor) handleRestart(serverName string, status *ServerHealthStatus, observed ...uint64) {
	tracer := otel.GetTracerProvider().Tracer("loomd")
	if h.daemon != nil {
		tracer = h.daemon.daemonTracer()
	}
	_, span := tracer.Start(context.Background(), "daemon.server.restart",
		trace.WithAttributes(
			attribute.String("server.name", serverName),
			attribute.Int("server.restart_count", status.RestartCount),
			attribute.Int("server.max_restarts", h.maxRestarts),
			attribute.Bool("daemon.proc_manager_available", h.daemon != nil && h.daemon.procMgr != nil),
		),
	)
	defer span.End()

	// Check cooldown
	if !status.LastRestart.IsZero() && time.Since(status.LastRestart) < h.restartCooldown {
		span.AddEvent("daemon.server.restart.skipped_cooldown")
		return
	}

	// Check max restarts
	if status.RestartCount >= h.maxRestarts {
		h.logger.Error("max restarts exceeded, giving up",
			"server", serverName,
			"restarts", status.RestartCount)
		status.AutoRestartFailed = true
		span.SetStatus(codes.Error, "max restarts exceeded")
		span.AddEvent("daemon.server.restart.exhausted")
		return
	}

	if h.daemon == nil || h.daemon.procMgr == nil {
		span.AddEvent("daemon.server.restart.skipped_no_proc_manager")
		return
	}

	h.logger.Info("attempting auto-restart",
		"server", serverName,
		"restart_count", status.RestartCount+1)
	span.AddEvent("daemon.server.restart.attempt",
		trace.WithAttributes(
			attribute.Int("server.next_restart_count", status.RestartCount+1),
		),
	)

	var observedGeneration uint64
	observedGenerationProvided := len(observed) > 0
	if len(observed) > 0 {
		observedGeneration = observed[0]
	}
	if !observedGenerationProvided && h.daemon.serverSupervisor != nil {
		if snapshot, ok := h.daemon.serverSupervisor.current(serverName); ok {
			observedGeneration = snapshot.Generation
		}
	}

	// Retire only the generation observed by the probe. A delayed health
	// failure from an older process must not stop its healthy replacement.
	retired, stopErr := h.stopServerGenerationBounded(serverName, observedGeneration)
	if stopErr != nil {
		h.logger.Warn("failed to stop server during restart", "server", serverName, "error", stopErr)
		span.RecordError(stopErr)
		span.AddEvent("daemon.server.restart.stop_failed",
			trace.WithAttributes(attribute.String("error", stopErr.Error())),
		)
	}
	if !retired {
		span.AddEvent("daemon.server.restart.skipped_stale_generation",
			trace.WithAttributes(attribute.Int64("server.generation", int64(observedGeneration))),
		)
		return
	}

	// Give it a moment to clean up, but never delay daemon shutdown.
	cleanupDelay := time.NewTimer(time.Second)
	defer cleanupDelay.Stop()
	cleanupCtx := h.ctx
	if cleanupCtx == nil {
		cleanupCtx = context.Background()
	}
	select {
	case <-cleanupDelay.C:
	case <-cleanupCtx.Done():
		return
	}

	// Start it again - it will be started on next request
	status.RestartCount++
	status.LastRestart = time.Now()
	span.SetAttributes(attribute.Int("server.restart_count", status.RestartCount))
	span.AddEvent("daemon.server.restart.completed",
		trace.WithAttributes(attribute.Int("server.restart_count", status.RestartCount)),
	)

	// Update Prometheus metrics
	if h.daemon.metrics != nil {
		h.daemon.metrics.RecordProcessRestart(serverName)
	}
}

type healthStopResult struct {
	retired bool
	err     error
}

func (h *HealthMonitor) stopServerGenerationBounded(serverName string, generationID uint64) (bool, error) {
	result := make(chan healthStopResult, 1)
	go func() {
		retired, err := h.daemon.stopServerGeneration(serverName, generationID)
		result <- healthStopResult{retired: retired, err: err}
	}()

	baseCtx := h.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	timer := time.NewTimer(reaperStopTimeout)
	defer timer.Stop()
	select {
	case completed := <-result:
		return completed.retired, completed.err
	case <-baseCtx.Done():
		return false, baseCtx.Err()
	case <-timer.C:
		return false, context.DeadlineExceeded
	}
}

// HealthDivergence represents a disagreement between the health monitor and the router.
type HealthDivergence struct {
	Monitor         bool   `json:"monitor_healthy"`
	RouterAvailable bool   `json:"router_available"`
	Reason          string `json:"reason"`
}

// computeHealthDivergence returns non-nil when the monitor and router disagree on server health.
func computeHealthDivergence(monitor *ServerHealthStatus, routerAvailable bool) *HealthDivergence {
	if monitor == nil {
		return nil
	}
	if monitor.Healthy == routerAvailable {
		return nil
	}
	reason := "monitor_healthy_router_unavailable"
	if !monitor.Healthy {
		reason = "monitor_unhealthy_router_available"
	}
	return &HealthDivergence{
		Monitor:         monitor.Healthy,
		RouterAvailable: routerAvailable,
		Reason:          reason,
	}
}

// ResetRestartCount resets the restart count for a server (e.g., after manual intervention).
func (h *HealthMonitor) ResetRestartCount(serverName string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if status, ok := h.statuses[serverName]; ok {
		status.RestartCount = 0
		status.AutoRestartFailed = false
	}
}

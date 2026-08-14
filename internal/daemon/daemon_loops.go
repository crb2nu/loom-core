package daemon

import (
	"runtime"
	"time"
)

// idleReaperLoop periodically terminates idle server processes.
func (d *Daemon) idleReaperLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	idleTimeout := d.fileCfg.Resources.GetIdleTimeout()

	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			reaped := d.reapIdleServers(idleTimeout)
			if len(reaped) > 0 {
				d.logger.Info("reaped idle servers", "servers", reaped, "count", len(reaped))
				for _, name := range reaped {
					if d.serverSupervisor == nil {
						d.runningServers.Delete(name)
					}
					if d.eventBus != nil {
						d.eventBus.Publish(EventProcessStop, map[string]any{
							"server": name,
							"reason": "idle_reaped",
						})
					}
				}
			}
		}
	}
}

// shouldReapIdleServer reports whether an idle server is eligible for reaping.
// A server is reaped only when it is past the idle timeout AND is not a
// resident (stateful) server. Resident servers hold long-lived in-memory
// working state or run continuous background reconciliation, so idle-reaping
// them only churns reload/respawn (and, before durable-watch persistence, lost
// in-flight work). Genuine wedges are still recovered by the health monitor.
func shouldReapIdleServer(name string, idle, timeout time.Duration, resident map[string]bool) bool {
	if resident[name] {
		return false
	}
	return idle > timeout
}

// reaperStopTimeout caps how long the reaper waits for stopServerProc to
// return before giving up and releasing the per-server call lock. A hung
// procMgr.Stop (e.g. Cmd.Wait blocked on an inherited pipe after SIGKILL)
// would otherwise hold the lock indefinitely and brick every future call to
// that server with "acquire call lock for X after 0s: context deadline
// exceeded".
const reaperStopTimeout = 5 * time.Second

// reapIdleServers reaps idle processes while respecting per-server call locks.
// This prevents races where the reaper closes a process mid tools/call.
func (d *Daemon) reapIdleServers(idleTimeout time.Duration) []string {
	if d.serverSupervisor != nil {
		return d.reapIdleGenerations(idleTimeout)
	}
	if d.procMgr == nil {
		return nil
	}

	idleInfo := d.procMgr.GetIdleInfo()
	if len(idleInfo) == 0 {
		return nil
	}

	resident := d.fileCfg.Resources.GetResidentServers()
	reaped := make([]string, 0)
	for _, info := range idleInfo {
		if !shouldReapIdleServer(info.Name, info.IdleDuration, idleTimeout, resident) {
			continue
		}

		callMu := d.callLock(info.Name)
		if !callMu.TryLock() {
			// Server has an in-flight call; skip this reaper cycle.
			continue
		}

		// Re-check idleness while holding the call lock to avoid stale snapshot races.
		stillIdle := false
		for _, current := range d.procMgr.GetIdleInfo() {
			if current.Name == info.Name {
				stillIdle = current.IdleDuration > idleTimeout
				break
			}
		}

		if stillIdle {
			if err := d.stopServerProcBounded(info.Name, reaperStopTimeout); err != nil {
				d.logger.Warn("failed to reap idle server", "server", info.Name, "error", err)
			} else {
				reaped = append(reaped, info.Name)
			}
		}

		callMu.Unlock()
	}

	return reaped
}

func (d *Daemon) reapIdleGenerations(idleTimeout time.Duration) []string {
	resident := d.fileCfg.Resources.GetResidentServers()
	cutoff := time.Now().Add(-idleTimeout)
	reaped := make([]string, 0)
	for _, snapshot := range d.serverSupervisor.snapshots() {
		if resident[snapshot.Key] || snapshot.LastActivity.After(cutoff) {
			continue
		}

		var retired bool
		err, completed := runWithTimeout(func() error {
			var retireErr error
			retired, retireErr = d.serverSupervisor.retireIfIdle(snapshot.Key, snapshot.Generation, cutoff)
			return retireErr
		}, reaperStopTimeout)
		if !completed {
			if d.logger != nil {
				d.logger.Warn("generation retirement timed out; teardown remains tracked",
					"server", snapshot.Key, "generation", snapshot.Generation, "timeout", reaperStopTimeout)
			}
			continue
		}
		if err != nil {
			if d.logger != nil {
				d.logger.Warn("failed to retire idle generation",
					"server", snapshot.Key, "generation", snapshot.Generation, "error", err)
			}
			continue
		}
		if retired {
			if d.pool != nil {
				// Logical pool views are non-owning, but retaining wrappers for a
				// retired generation forces the next checkout through a known-stale
				// rejection. Evict them after the generation-fenced retirement wins.
				d.pool.ClearServer(snapshot.Key)
			}
			reaped = append(reaped, snapshot.Key)
		}
	}
	return reaped
}

// stopServerProcBounded runs stopServerProc in a goroutine with a timeout so a
// hung Stop cannot pin the caller (and the per-server call lock it holds). On
// timeout the goroutine is left running — procMgr.Stop will eventually
// complete or be reaped at daemon shutdown — but the caller returns so the
// call lock is released and future requests for the server are not blocked.
func (d *Daemon) stopServerProcBounded(serverName string, timeout time.Duration) error {
	err, completed := runWithTimeout(func() error { return d.stopServerProc(serverName) }, timeout)
	if !completed && d.logger != nil {
		d.logger.Warn("stopServerProc timed out; releasing call lock so the server is not bricked",
			"server", serverName, "timeout", timeout)
	}
	return err
}

// runWithTimeout runs fn in a goroutine and waits up to timeout for it to
// return. The second return value reports whether fn completed before the
// timeout. On timeout the goroutine is intentionally leaked — it will
// complete on its own schedule. A non-positive timeout disables the bound.
func runWithTimeout(fn func() error, timeout time.Duration) (error, bool) {
	if timeout <= 0 {
		return fn(), true
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

// sessionReaperLoop periodically reaps expired proxy sessions.
func (d *Daemon) sessionReaperLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			if d.sessions != nil {
				if reaped := d.sessions.ReapExpired(); reaped > 0 {
					d.logger.Info("reaped expired proxy sessions", "count", reaped)
					if d.metrics != nil {
						d.metrics.RecordSessionReaped(reaped)
					}
				}
			}
		}
	}
}

// metricsCollectorLoop periodically updates metrics that require polling.
func (d *Daemon) metricsCollectorLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.collectMetrics()
		}
	}
}

// collectMetrics gathers current state and updates metrics.
func (d *Daemon) collectMetrics() {
	// Pool stats
	stats := d.pool.Stats()
	d.metrics.UpdatePoolStats("local", stats.IdleConns, stats.ActiveConns)

	if d.hubPool != nil {
		hubStats := d.hubPool.Stats()
		d.metrics.UpdatePoolStats("hub", hubStats.IdleConns, hubStats.ActiveConns)
	}

	// Process count
	processes := d.runningLocalServerNames()
	d.metrics.UpdateProcessCount(len(processes))

	// Tool cache
	d.toolCache.mu.RLock()
	cacheSize := len(d.toolCache.tools)
	cacheAge := time.Since(d.toolCache.updatedAt)
	d.toolCache.mu.RUnlock()
	d.metrics.UpdateToolCache(cacheSize, cacheAge)

	// Server health from router
	allHealth := d.router.GetAllHealth()
	for name, h := range allHealth {
		if h.Local != nil {
			d.metrics.UpdateServerHealth(name, "local", h.Local.Healthy, h.Local.AvgLatencyMs)
		}
		if h.Hub != nil {
			d.metrics.UpdateServerHealth(name, "hub", h.Hub.Healthy, h.Hub.AvgLatencyMs)
		}
	}

	// Hub connection status
	if d.hubClient != nil {
		// Simple check - if hubPool exists and has connections, we're connected
		connected := false
		var latency float64
		if d.hubPool != nil {
			hubStats := d.hubPool.Stats()
			if hubStats.IdleConns > 0 || hubStats.ActiveConns > 0 {
				connected = true
			}
		}
		d.metrics.UpdateHubConnection(connected, latency)
	}

	// Concurrent call gauge (from activeRPCs atomic counter)
	d.metrics.ConcurrentCalls.Set(float64(d.activeRPCs.Load()))

	// Proxy session gauges (active count + current daemon epoch)
	if d.sessions != nil {
		d.metrics.UpdateSessionGauges(d.sessions.ActiveCount(), d.sessions.Epoch())
	}

	// Runtime stats
	d.metrics.GoroutineCount.Set(float64(runtime.NumGoroutine()))
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	d.metrics.MemAllocBytes.Set(float64(memStats.Alloc))
	d.metrics.MemSysBytes.Set(float64(memStats.Sys))
	if memStats.NumGC > 0 {
		d.metrics.GCPauseNs.Set(float64(memStats.PauseNs[(memStats.NumGC+255)%256]))
	}

	// EventBus dropped events
	if d.eventBus != nil {
		d.metrics.EventsDropped.Add(0) // Ensure metric exists
		// We read the cumulative count; Prometheus counter must only increase.
		// Since DroppedCount() is cumulative and the counter is too, we track
		// the delta from the eventBus.
	}
}

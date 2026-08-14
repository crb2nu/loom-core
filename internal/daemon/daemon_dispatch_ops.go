package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/registry"
)

// handleReload reloads the registry and refreshes the tool cache.
func (d *Daemon) handleReload(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if err := d.Reload(ctx); err != nil {
		return mcp.NewErrorResponse(msg.ID, mcp.InternalError, err.Error()), nil
	}

	serverCount := 0
	if reg := d.currentRegistry(); reg != nil {
		serverCount = len(reg.Servers)
	}
	return mcp.NewResponse(msg.ID, map[string]any{
		"reloaded": true,
		"servers":  serverCount,
	})
}

// handleCacheStats returns response cache statistics.
func (d *Daemon) handleCacheStats(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.respCache == nil {
		return mcp.NewResponse(msg.ID, map[string]any{
			"enabled": false,
		})
	}

	stats := d.respCache.Stats()
	return mcp.NewResponse(msg.ID, map[string]any{
		"enabled":    true,
		"entries":    stats.Entries,
		"size_bytes": stats.SizeBytes,
		"max_bytes":  stats.MaxBytes,
		"total_hits": stats.TotalHits,
	})
}

// handleCacheClear clears the response cache.
func (d *Daemon) handleCacheClear(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.respCache == nil {
		return mcp.NewResponse(msg.ID, map[string]any{
			"cleared": false,
			"reason":  "cache not enabled",
		})
	}

	// Parse optional server parameter
	var params struct {
		Server string `json:"server,omitempty"`
	}
	if len(msg.Params) > 0 {
		json.Unmarshal(msg.Params, &params)
	}

	if params.Server != "" {
		d.respCache.ClearServer(params.Server)
		d.logger.Info("response cache cleared for server", "server", params.Server)
	} else {
		d.respCache.Clear()
		d.logger.Info("response cache cleared")
	}

	stats := d.respCache.Stats()
	d.metrics.UpdateResponseCacheStats(stats.Entries, stats.SizeBytes)

	return mcp.NewResponse(msg.ID, map[string]any{
		"cleared": true,
		"server":  params.Server,
	})
}

// handleCostStats returns cost tracking usage data.
func (d *Daemon) handleCostStats(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.cost == nil {
		return mcp.NewResponse(msg.ID, map[string]any{
			"enabled": false,
			"reason":  "cost tracking not enabled",
		})
	}
	snap := d.cost.Snapshot()
	return mcp.NewResponse(msg.ID, snap)
}

// Reload reloads the registry and refreshes servers.
func (d *Daemon) Reload(ctx context.Context) error {
	if err := d.acquireReload(ctx); err != nil {
		return err
	}
	defer d.releaseReload()
	d.logger.Info("reloading configuration")

	// Refresh runtime-mutable env settings (HUD admin token, etc.)
	// before touching the registry — keeps an out-of-band
	// X-Admin-Token rotation effective even if registry reload fails.
	if err := d.reloadEnvFile(); err != nil {
		d.logger.Warn("env file reload failed", "path", d.cfg.EnvFilePath, "error", err)
	}

	// Reload registry
	if d.cfg.RegistryPath != "" {
		oldReg := d.currentRegistry()
		newReg, err := registry.LoadWithDefaults(d.cfg.RegistryPath)
		if err != nil {
			return fmt.Errorf("load registry: %w", err)
		}
		newReg, err = runtimeRegistryForTarget(newReg, d.cfg.Target)
		if err != nil {
			return fmt.Errorf("normalize runtime registry: %w", err)
		}
		newReg = applyCatalogState(newReg, d.logger)

		// Find servers that were removed
		oldServers := make(map[string]bool)
		if oldReg != nil {
			for _, s := range oldReg.Servers {
				oldServers[s.Name] = true
			}
		}
		newServers := make(map[string]bool)
		for _, s := range newReg.Servers {
			newServers[s.Name] = true
		}

		// Publish the new launch/routing registry before retiring removed or
		// changed generations. A concurrent checkout after this point can no
		// longer recreate a removed server from the old registry.
		oldEpoch := d.registryEpoch.Load()
		if oldEpoch == 0 {
			oldEpoch = 1
			d.registryEpoch.Store(oldEpoch)
		}
		// Publish future per-key shard config before advancing registryEpoch. A
		// generation racing this window is conservatively stamped with oldEpoch
		// and retired below; no generation can observe a new epoch with old launch
		// config. Existing shards retain their revision until generation Close.
		if d.localProcController != nil {
			d.localProcController.UpdateRegistry(newReg)
		}
		// procMgr is retained only for daemon literals/legacy fallback. Updating it
		// while the sharded production controller is active would reintroduce a
		// reload wait on fi-mcp-kit's fleet-global Manager mutex.
		if d.localProcController == nil && d.procMgr != nil {
			d.procMgr.SetRegistry(newReg)
		}
		d.router.SetRegistry(newReg)
		d.storeRegistryForCacheRevision(newReg)
		d.registryEpoch.Add(1)
		if d.healthMonitor != nil {
			d.healthMonitor.reconcileRegistry(newReg)
		}
		observedGenerations := d.currentLocalGenerations()
		if d.serverSupervisor != nil {
			observedGenerations = d.serverSupervisor.generationsAtOrBefore(oldEpoch)
		}
		d.logger.Info("registry reloaded", "servers", len(newReg.Servers))

		// Stop removed servers
		for name := range oldServers {
			if !newServers[name] {
				d.logger.Info("stopping removed server", "server", name)
				retired := true
				var stopErr error
				if d.serverSupervisor != nil {
					generationID, observed := observedGenerations[name]
					if observed {
						retired, stopErr = d.serverSupervisor.retireGenerationAsync(name, generationID)
					} else {
						retired = false
					}
				} else {
					stopErr = d.stopServerProc(name)
					d.runningServers.Delete(name)
				}
				if stopErr != nil && d.logger != nil {
					d.logger.Warn("failed to stop removed server", "server", name, "error", stopErr)
				}
				if retired && d.pool != nil {
					d.pool.ClearServer(name)
				}
				d.manifest.RemoveServer(name)
				if d.eventBus != nil {
					d.eventBus.Publish(EventProcessStop, map[string]any{
						"server":     name,
						"reason":     "removed_from_config",
						"generation": observedGenerations[name],
					})
				}
			}
		}

		invalidated := d.invalidateServersForReload(oldReg, newReg, observedGenerations)
		if len(invalidated) > 0 {
			d.logger.Info("invalidated running servers after reload", "count", len(invalidated), "servers", invalidated)
		}
	}

	// Refresh tool cache
	refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if _, err := d.refreshToolCacheDeduplicated(refreshCtx); err != nil {
		d.logger.Warn("tool cache refresh failed after reload", "error", err)
	}

	// Emit config.reload event
	if d.eventBus != nil {
		serverCount := 0
		if reg := d.currentRegistry(); reg != nil {
			serverCount = len(reg.Servers)
		}
		d.eventBus.Publish(EventConfigReload, map[string]any{
			"servers": serverCount,
		})
	}
	if d.hudApp != nil {
		d.hudApp.RefreshMonitors()
	}

	return nil
}

func (d *Daemon) acquireReload(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.reloadGateOnce.Do(func() {
		d.reloadGate = make(chan struct{}, 1)
	})
	select {
	case d.reloadGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Daemon) releaseReload() {
	if d != nil && d.reloadGate != nil {
		<-d.reloadGate
	}
}

type tunnelsResult struct {
	Tunnels   map[string]*TunnelStatus `json:"tunnels"`
	Total     int                      `json:"total"`
	Connected int                      `json:"connected"`
}

// handleTunnels returns the status of all SSH tunnels.
func (d *Daemon) handleTunnels(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	if d.tunnelMgr == nil {
		return mcp.NewResponse(msg.ID, tunnelsResult{
			Tunnels: make(map[string]*TunnelStatus),
		})
	}

	result := tunnelsResult{
		Tunnels:   d.tunnelMgr.GetAllStatuses(),
		Total:     d.tunnelMgr.TunnelCount(),
		Connected: d.tunnelMgr.ConnectedCount(),
	}

	return mcp.NewResponse(msg.ID, result)
}

// startTunnelsForServers scans the registry and starts tunnels for servers with SSH config.
func (d *Daemon) startTunnelsForServers() {
	reg := d.currentRegistry()
	if d.tunnelMgr == nil || reg == nil {
		return
	}

	// Port allocation starts at 16443 for K8s API tunnels
	nextPort := 16443

	for _, server := range reg.Servers {
		if server == nil {
			continue
		}

		// Get target spec for current target profile
		spec, err := reg.GetServerSpec(server.Name, d.cfg.Target)
		if err != nil || spec == nil {
			continue
		}

		// Check if server has SSH configuration
		if spec.SSH == nil {
			continue
		}

		// Determine the remote address from server config
		// Common pattern: K8s API server on 6443, or use env var
		remoteAddr := "localhost:6443"
		if envHost, ok := spec.Env["KUBECONFIG_REMOTE_HOST"]; ok {
			remoteAddr = d.expandVars(envHost)
		}

		d.logger.Info("starting tunnel for server",
			"server", server.Name,
			"ssh_host", spec.SSH.Host,
			"local_port", nextPort,
			"remote_addr", remoteAddr)

		if err := d.tunnelMgr.AddTunnel(server.Name, spec.SSH, nextPort, remoteAddr); err != nil {
			d.logger.Warn("failed to start tunnel", "server", server.Name, "error", err)
			continue
		}

		nextPort++
	}

	count := d.tunnelMgr.TunnelCount()
	if count > 0 {
		d.logger.Info("tunnels started", "count", count)
	}
}

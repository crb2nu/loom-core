package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/router"
	"github.com/crb2nu/loom/pkg/registry"
)

const cacheRefreshLeaderTimeout = 60 * time.Second

var errCacheRefreshStopped = errors.New("cache refresh coordinator stopped")

// refreshToolCacheDeduplicated wraps refreshToolCache via singleflight to prevent
// redundant concurrent refreshes. Multiple callers get the same result.
func (d *Daemon) refreshToolCacheDeduplicated(ctx context.Context) ([]mcp.Tool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := d.cacheSnapshot()
	result := d.refreshGroup.DoChan(toolRefreshSingleflightKey(snapshot.revision), func() (any, error) {
		leaderRoot, done, err := d.claimCacheRefreshWork()
		if err != nil {
			return nil, err
		}
		defer done()
		leaderCtx, cancel := context.WithTimeout(leaderRoot, cacheRefreshLeaderTimeout)
		defer cancel()
		return d.refreshToolCacheForSnapshot(leaderCtx, snapshot)
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case completed := <-result:
		if completed.Err != nil {
			return nil, completed.Err
		}
		return completed.Val.([]mcp.Tool), nil
	}
}

// claimToolRefreshWork joins a daemon-owned cancellation domain. The stopped
// flag and WaitGroup.Add share one lock, so shutdown can forbid new work before
// waiting without an Add/Wait race.
func (d *Daemon) claimCacheRefreshWork() (context.Context, func(), error) {
	d.cacheRefreshWorkMu.Lock()
	defer d.cacheRefreshWorkMu.Unlock()
	if d.cacheRefreshWorkStopped {
		return nil, nil, errCacheRefreshStopped
	}
	if d.cacheRefreshWorkCtx == nil {
		d.cacheRefreshWorkCtx, d.cacheRefreshWorkCancel = context.WithCancel(context.Background())
	}
	d.cacheRefreshWorkWG.Add(1)
	return d.cacheRefreshWorkCtx, d.cacheRefreshWorkWG.Done, nil
}

func (d *Daemon) startBackgroundToolRefresh(reason string) {
	root, done, err := d.claimCacheRefreshWork()
	if err != nil {
		return
	}
	go func() {
		defer done()
		ctx, cancel := context.WithTimeout(root, cacheRefreshLeaderTimeout)
		defer cancel()
		if _, refreshErr := d.refreshToolCacheDeduplicated(ctx); refreshErr != nil &&
			!errors.Is(refreshErr, context.Canceled) && !errors.Is(refreshErr, errCacheRefreshStopped) && d.logger != nil {
			d.logger.Debug("background tool cache refresh failed", "reason", reason, "error", refreshErr)
		}
	}()
}

func (d *Daemon) stopCacheRefreshWork() {
	d.cacheRefreshWorkMu.Lock()
	d.cacheRefreshWorkStopped = true
	if d.cacheRefreshWorkCancel != nil {
		d.cacheRefreshWorkCancel()
	}
	d.cacheRefreshWorkMu.Unlock()
	d.cacheRefreshWorkWG.Wait()
}

// refreshToolCacheForSnapshot fetches tools using one immutable
// registry/profile snapshot. Publication is fenced by snapshot.revision.
func (d *Daemon) refreshToolCacheForSnapshot(ctx context.Context, snapshot cacheRefreshSnapshot) ([]mcp.Tool, error) {
	refreshConcurrency := d.fileCfg.Resources.GetRefreshConcurrency()
	reg := snapshot.registry
	if reg == nil {
		return nil, fmt.Errorf("registry not loaded")
	}
	localServerCount := len(reg.Servers)

	// Build a unified list of sources (local + hub) and fetch them with bounded concurrency.
	sources := make([]toolSource, 0, localServerCount+20)
	for _, server := range reg.Servers {
		if server == nil {
			continue
		}
		sources = append(sources, toolSource{name: server.Name, kind: toolSourceLocal})
	}

	var hubClient *router.HubClient
	if d.cfg.HubFallback && d.hubClient != nil {
		now := time.Now()
		hubAuthDisabled, hubAuthBackoffUntil := d.currentHubAuthState()
		if hubAuthDisabled {
			d.logger.Debug("hub fallback disabled after auth-gated discovery failure")
		} else if !hubAuthBackoffUntil.IsZero() && now.Before(hubAuthBackoffUntil) {
			d.logger.Debug("skipping hub discovery during auth backoff", "until", hubAuthBackoffUntil)
		} else {
			token, err := resolveHubToken(ctx, d.repoRoot, reg)
			if err != nil {
				return nil, err
			}

			hubClient = router.NewHubClientWithCFAccess(
				d.cfg.HubURL, token,
				d.fileCfg.Hub.CFAccessClientID,
				d.fileCfg.Hub.CFAccessClientSecret,
			)
			hostNames, err := hubClient.DiscoverHosts(ctx)
			if err != nil {
				if isHubAuthError(err) {
					hint := "check MCP_HUB_TOKEN or Cloudflare Access credentials, or set hub.disable_on_auth_failure"
					if d.fileCfg.Hub.DisableOnAuthFailure {
						if d.withCurrentCacheRevision(snapshot, func(state *cacheRuntimeState) {
							state.hubAuthDisabled = true
						}) {
							d.logger.Warn("hub discovery auth required; disabling hub fallback", "error", err, "hint", hint)
						}
					} else {
						backoffUntil := now.Add(hubAuthBackoff)
						if d.withCurrentCacheRevision(snapshot, func(state *cacheRuntimeState) {
							state.hubAuthBackoffUntil = backoffUntil
						}) {
							d.logger.Warn("hub discovery auth required; backing off", "until", backoffUntil, "error", err, "hint", hint)
						}
					}
				} else {
					d.logger.Warn("failed to discover hub hosts", "error", err)
				}
				hubClient = nil
			} else {
				d.withCurrentCacheRevision(snapshot, func(state *cacheRuntimeState) {
					state.hubAuthBackoffUntil = time.Time{}
				})
				for _, host := range hostNames {
					// Avoid shadowing local servers if they have the same name.
					isLocal := false
					for _, s := range reg.Servers {
						if s == nil {
							continue
						}
						if s.Name == host {
							isLocal = true
							break
						}
					}
					if isLocal {
						continue
					}
					sources = append(sources, toolSource{name: host, kind: toolSourceHub})
				}
			}
		}
	}

	d.logger.Info("refreshing tool cache", "sources", len(sources), "local", localServerCount)

	results := fetchToolsBounded(ctx, sources, refreshConcurrency, func(ctx context.Context, src toolSource) ([]mcp.Tool, error) {
		switch src.kind {
		case toolSourceHub:
			if hubClient == nil {
				return nil, fmt.Errorf("hub client unavailable")
			}
			return hubClient.FetchTools(ctx, src.name)
		default:
			return d.fetchServerToolsFromRegistry(ctx, src.name, fetchServerToolsTimeout, reg)
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Aggregate results
	var allTools []mcp.Tool
	successCount := 0
	manifestUpdates := make(map[string][]mcp.Tool)
	dynamicRoutes := make(map[string][]string)

	// Helper to sanitize tool names
	sanitize := func(s string) string {
		// Replace dots with underscores
		s = strings.ReplaceAll(s, ".", "_")
		// Remove any other invalid characters (keep alphanumeric, _, -)
		var b strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				b.WriteRune(r)
			}
		}
		res := b.String()
		// Truncate to 64 chars
		if len(res) > 64 {
			res = res[:64]
		}
		return res
	}

	for _, result := range results {
		if result.err != nil {
			d.logger.Debug("failed to get tools from server", "server", result.name, "error", result.err)
			continue
		}
		successCount++

		// Namespace tools with server prefix and enhance descriptions
		var namespacedTools []mcp.Tool
		for _, tool := range result.tools {
			originalToolName := tool.Name

			// Build a replaceable live route snapshot for prefix-less calls.
			dynamicRoutes[originalToolName] = append(dynamicRoutes[originalToolName], result.name)

			// Sanitize the original tool name first
			safeToolName := sanitize(tool.Name)
			// Create namespaced name
			namespacedName := result.name + "__" + safeToolName
			// Sanitize again just in case server name had issues (though registry should be clean)
			tool.Name = sanitize(namespacedName)

			// Enhance description with metadata if available
			if d.metadata != nil && d.fileCfg.Context.EnrichDescriptions {
				tool.Description = d.metadata.EnhanceDescription(result.name, originalToolName, tool.Description)
			}

			namespacedTools = append(namespacedTools, tool)
			allTools = append(allTools, tool)
		}

		manifestUpdates[result.name] = namespacedTools
	}

	// Apply the profile captured with this refresh revision.
	activeProfile := snapshot.profile

	filterResult := d.profiles.Filter(allTools, activeProfile)
	if filterResult.Truncated {
		d.logger.Warn("tools truncated by profile",
			"profile", activeProfile,
			"before", filterResult.TotalBefore,
			"after", filterResult.TotalAfter)
	}
	allTools = filterResult.Tools

	published := d.publishToolCache(snapshot, allTools, manifestUpdates, dynamicRoutes)
	if !published {
		return nil, errCacheRefreshSuperseded
	}
	// Filesystem I/O stays outside the runtime publication mutex. Save snapshots
	// the manifest and rejects/retries a superseded in-memory generation.
	if d.manifest != nil {
		if err := d.manifest.Save(); err != nil {
			d.logger.Warn("failed to save manifest", "error", err)
		}
	}

	d.logger.Info("tool cache refreshed",
		"profile", activeProfile,
		"revision", snapshot.revision,
		"total_tools", len(allTools),
		"servers_succeeded", successCount,
		"servers_total", localServerCount)

	return allTools, nil
}

func resolveHubToken(ctx context.Context, repoRoot string, reg *registry.Registry) (string, error) {
	// Resolve the cheap registry/env path first. Only initialize external
	// secret stores when no environment-backed value exists.
	var token string
	if reg != nil {
		token = reg.GetEnvWithFallback("MCP_HUB_TOKEN")
	}
	if token == "" {
		token = os.Getenv("MCP_HUB_TOKEN")
	}
	if token == "" {
		token = expandVarsWithRegistryContext(ctx, "${secret:MCP_HUB_TOKEN}", repoRoot, reg)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return token, nil
}

func (d *Daemon) publishToolCache(
	snapshot cacheRefreshSnapshot,
	tools []mcp.Tool,
	manifestUpdates map[string][]mcp.Tool,
	dynamicRoutes map[string][]string,
) bool {
	return d.withCurrentCacheRevision(snapshot, func(state *cacheRuntimeState) {
		// Replace live prefix-less routes wholesale. No removed tool survives a
		// successful refresh merely because an earlier leader discovered it.
		state.routes.Store(buildDynamicToolRouteIndex(dynamicRoutes))

		if d.manifest != nil {
			d.manifest.ReplaceServerTools(manifestUpdates)
		}

		d.toolCache.mu.Lock()
		oldTools := d.toolCache.tools
		d.toolCache.tools = tools
		d.toolCache.updatedAt = time.Now()
		d.toolCache.mu.Unlock()

		if toolNamesChanged(oldTools, tools) && d.eventBus != nil {
			d.eventBus.Publish(EventToolsChanged, map[string]any{
				"old_count": len(oldTools),
				"new_count": len(tools),
			})
		}

		if d.metrics != nil {
			d.metrics.RecordToolCacheRefresh()
			d.metrics.UpdateToolCache(len(tools), 0)
			d.metrics.UpdateProcessCount(len(d.runningLocalServerNames()))
		}
	})
}

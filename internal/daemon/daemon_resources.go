package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	gosync "sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// resourcesResult holds the aggregated resources response.
type resourcesResult struct {
	Resources          []mcp.Resource `json:"resources"`
	CachedAt           time.Time      `json:"cachedAt"`
	ServerCount        int            `json:"serverCount"`
	RunningServerCount int            `json:"runningServerCount"`
}

func daemonBuiltInResources() []mcp.Resource {
	return []mcp.Resource{
		{
			URI:         "loom://servers",
			Name:        "Loom servers",
			Description: "List MCP servers managed by the loom daemon",
			MimeType:    "application/json",
		},
		{
			URI:         "loom://tools",
			Name:        "Loom tools",
			Description: "Cached aggregated tools from loom daemon",
			MimeType:    "application/json",
		},
		{
			URI:         "loom://health",
			Name:        "Loom health",
			Description: "Health summary for all servers (local/hub) managed by loom",
			MimeType:    "application/json",
		},
		{
			URI:         "loom://config",
			Name:        "Loom config",
			Description: "Active profile and daemon configuration summary",
			MimeType:    "application/json",
		},
	}
}

func (d *Daemon) handleResources(ctx context.Context, msg *mcp.Message) (*mcp.Message, error) {
	serverCount := 0
	if reg := d.currentRegistry(); reg != nil {
		serverCount = len(reg.Servers)
	}

	runningServers := d.readyLocalServerNames()
	runningSet := make(map[string]struct{}, len(runningServers))
	for _, serverName := range runningServers {
		runningSet[serverName] = struct{}{}
	}

	// Always return cached resources immediately if available (even if stale).
	d.resourceCache.mu.RLock()
	hasCache := len(d.resourceCache.resources) > 0
	cacheStale := time.Since(d.resourceCache.updatedAt) >= d.resourceCache.ttl
	cachedResources := d.resourceCache.resources
	cachedAt := d.resourceCache.updatedAt
	d.resourceCache.mu.RUnlock()

	if hasCache {
		if cacheStale {
			d.startBackgroundResourceRefresh()
		}
		return mcp.NewResponse(msg.ID, resourcesResult{
			Resources:          cachedResources,
			CachedAt:           cachedAt,
			ServerCount:        serverCount,
			RunningServerCount: len(runningSet),
		})
	}

	// No cache yet: return built-ins immediately and refresh asynchronously.
	builtins := daemonBuiltInResources()
	now := time.Now()
	d.resourceCache.mu.Lock()
	d.resourceCache.resources = builtins
	d.resourceCache.updatedAt = now
	d.resourceCache.mu.Unlock()

	d.startBackgroundResourceRefresh()

	return mcp.NewResponse(msg.ID, resourcesResult{
		Resources:          builtins,
		CachedAt:           now,
		ServerCount:        serverCount,
		RunningServerCount: len(runningSet),
	})
}

// refreshResourcesCacheDeduplicated wraps refreshResourcesCache via singleflight to
// prevent redundant concurrent refreshes.
func (d *Daemon) refreshResourcesCacheDeduplicated(ctx context.Context) ([]mcp.Resource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := d.cacheSnapshot()
	result := d.refreshGroup.DoChan(resourceRefreshSingleflightKey(snapshot.revision), func() (any, error) {
		leaderRoot, done, err := d.claimCacheRefreshWork()
		if err != nil {
			return nil, err
		}
		defer done()
		leaderCtx, cancel := context.WithTimeout(leaderRoot, cacheRefreshLeaderTimeout)
		defer cancel()
		return d.refreshResourcesCacheForSnapshot(leaderCtx, snapshot)
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case completed := <-result:
		if completed.Err != nil {
			return nil, completed.Err
		}
		return completed.Val.([]mcp.Resource), nil
	}
}

func (d *Daemon) startBackgroundResourceRefresh() {
	root, done, err := d.claimCacheRefreshWork()
	if err != nil {
		return
	}
	go func() {
		defer done()
		ctx, cancel := context.WithTimeout(root, cacheRefreshLeaderTimeout)
		defer cancel()
		_, _ = d.refreshResourcesCacheDeduplicated(ctx)
	}()
}

// refreshResourcesCacheForSnapshot uses one immutable registry/profile
// revision and fences its cache/event publication against later changes.
func (d *Daemon) refreshResourcesCacheForSnapshot(ctx context.Context, snapshot cacheRefreshSnapshot) ([]mcp.Resource, error) {
	refreshConcurrency := d.fileCfg.Resources.GetRefreshConcurrency()
	if refreshConcurrency <= 0 {
		refreshConcurrency = 1
	}

	base := daemonBuiltInResources()
	running := d.readyLocalServerNames()
	runningSet := make(map[string]struct{}, len(running))
	for _, serverName := range running {
		runningSet[serverName] = struct{}{}
	}

	if len(runningSet) == 0 {
		if !d.publishResourceCache(snapshot, base) {
			return nil, errCacheRefreshSuperseded
		}
		return base, nil
	}

	reg := snapshot.registry
	if reg == nil {
		if !d.publishResourceCache(snapshot, base) {
			return nil, errCacheRefreshSuperseded
		}
		return base, nil
	}

	serverResources := make(map[string][]mcp.Resource, len(runningSet))
	var (
		mu  gosync.Mutex
		wg  gosync.WaitGroup
		sem = make(chan struct{}, refreshConcurrency)
	)

	for _, server := range reg.Servers {
		if server == nil {
			continue
		}
		if _, ok := runningSet[server.Name]; !ok {
			continue
		}

		wg.Add(1)
		go func(serverName string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			resources, err := d.fetchServerResources(callCtx, serverName)
			if err != nil {
				d.logger.Debug("resources probe failed", "server", serverName, "error", err)
				return
			}
			if len(resources) == 0 {
				return
			}
			mu.Lock()
			serverResources[serverName] = resources
			mu.Unlock()
		}(server.Name)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Keep output deterministic by walking servers in registry order.
	allResources := make([]mcp.Resource, 0, len(base)+len(serverResources)*4)
	allResources = append(allResources, base...)
	for _, server := range reg.Servers {
		if server == nil {
			continue
		}
		resources, ok := serverResources[server.Name]
		if !ok {
			continue
		}
		for _, r := range resources {
			r.URI = server.Name + "__" + r.URI
			allResources = append(allResources, r)
		}
	}

	if !d.publishResourceCache(snapshot, allResources) {
		return nil, errCacheRefreshSuperseded
	}

	return allResources, nil
}

func (d *Daemon) publishResourceCache(snapshot cacheRefreshSnapshot, resources []mcp.Resource) bool {
	return d.withCurrentCacheRevision(snapshot, func(_ *cacheRuntimeState) {
		d.resourceCache.mu.Lock()
		oldResources := d.resourceCache.resources
		d.resourceCache.resources = resources
		d.resourceCache.updatedAt = time.Now()
		d.resourceCache.mu.Unlock()

		if resourceNamesChanged(oldResources, resources) && d.eventBus != nil {
			d.eventBus.Publish(EventResourcesChanged, map[string]any{
				"old_count": len(oldResources),
				"new_count": len(resources),
			})
		}
	})
}

func (d *Daemon) fetchServerResources(ctx context.Context, serverName string) ([]mcp.Resource, error) {
	if d.pool == nil {
		return nil, fmt.Errorf("local pool not configured")
	}

	// Acquire callLock BEFORE pool.Get to match callPipeline.routeAndConnect
	// ordering. Reversed ordering (pool->lock) can deadlock against the
	// callPipeline path (lock->pool) when the pool is at capacity.
	mu, _, err := d.acquireCallLock(ctx, serverName)
	if err != nil {
		return nil, fmt.Errorf("acquire call lock: %w", err)
	}
	defer mu.Unlock()

	checkout, err := d.checkoutLocalConnection(ctx, serverName)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer checkout.close()
	conn := checkout.conn

	req, _ := mcp.NewRequest(1, "resources/list", nil)
	if err := conn.Transport.Send(ctx, req); err != nil {
		checkout.failObservedGeneration(err)
		return nil, fmt.Errorf("send resources/list: %w", err)
	}

	resp, err := conn.Transport.Recv(ctx)
	if err != nil {
		checkout.failObservedGeneration(err)
		return nil, fmt.Errorf("recv resources/list: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("server error: %s", resp.Error.Message)
	}

	var result struct {
		Resources []mcp.Resource `json:"resources"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse resources/list: %w", err)
	}

	return result.Resources, nil
}

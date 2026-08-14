package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/registry"
)

var errCacheRefreshSuperseded = errors.New("cache refresh superseded by a newer runtime revision")

// cacheRuntimeState serializes the small publication boundary shared by
// registry reloads, profile switches, and cache refresh commits. Fetching stays
// outside this lock; only revision changes and their visible side effects are
// committed while it is held.
type cacheRuntimeState struct {
	mu                  sync.Mutex
	revision            uint64
	activeProfile       string
	hubAuthDisabled     bool
	hubAuthBackoffUntil time.Time
	routes              atomic.Pointer[dynamicToolRouteIndex]
}

// cacheRefreshSnapshot is immutable input for one tool/resource refresh. A
// leader may do slow I/O with this snapshot, but it can publish only while its
// revision remains current.
type cacheRefreshSnapshot struct {
	revision uint64
	registry *registry.Registry
	profile  string
}

// dynamicToolRouteIndex is replaced as one immutable snapshot after a
// successful refresh. This daemon-owned layer complements the dependency
// router's static registry index and, unlike AddToolToIndex, can remove routes.
type dynamicToolRouteIndex struct {
	servers map[string][]string
}

func normalizeActiveProfile(profile string) string {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return "full"
	}
	return profile
}

func (d *Daemon) runtimeCacheState() *cacheRuntimeState {
	if d == nil {
		return nil
	}
	if current := d.cacheRuntime.Load(); current != nil {
		return current
	}
	candidate := &cacheRuntimeState{
		revision:            1,
		activeProfile:       normalizeActiveProfile(d.fileCfg.Context.ActiveProfile),
		hubAuthDisabled:     d.hubAuthDisabled,
		hubAuthBackoffUntil: d.hubAuthBackoffUntil,
	}
	if d.cacheRuntime.CompareAndSwap(nil, candidate) {
		return candidate
	}
	return d.cacheRuntime.Load()
}

func (d *Daemon) currentHubAuthState() (bool, time.Time) {
	state := d.runtimeCacheState()
	if state == nil {
		return false, time.Time{}
	}
	state.mu.Lock()
	disabled := state.hubAuthDisabled
	backoffUntil := state.hubAuthBackoffUntil
	state.mu.Unlock()
	return disabled, backoffUntil
}

func (d *Daemon) cacheSnapshot() cacheRefreshSnapshot {
	state := d.runtimeCacheState()
	if state == nil {
		return cacheRefreshSnapshot{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return cacheRefreshSnapshot{
		revision: state.revision,
		registry: d.currentRegistry(),
		profile:  state.activeProfile,
	}
}

func (d *Daemon) currentActiveProfile() string {
	state := d.runtimeCacheState()
	if state == nil {
		return "full"
	}
	state.mu.Lock()
	profile := state.activeProfile
	state.mu.Unlock()
	return normalizeActiveProfile(profile)
}

// setActiveProfile publishes the profile and advances the cache revision as
// one operation. Runtime code intentionally does not mutate fileCfg: it is the
// immutable startup configuration and remains safe for concurrent readers.
func (d *Daemon) setActiveProfile(profile string) uint64 {
	state := d.runtimeCacheState()
	if state == nil {
		return 0
	}
	state.mu.Lock()
	state.activeProfile = normalizeActiveProfile(profile)
	state.revision++
	d.narrowPublishedToolsLocked(state.activeProfile)
	state.routes.Store(buildDynamicToolRouteIndex(nil))
	revision := state.revision
	state.mu.Unlock()
	return revision
}

// storeRegistryForCacheRevision makes a reloaded registry visible to refresh
// snapshots and advances the cache revision under the same publication lock.
// A refresh from the previous registry therefore cannot commit afterward.
func (d *Daemon) storeRegistryForCacheRevision(reg *registry.Registry) uint64 {
	state := d.runtimeCacheState()
	if state == nil {
		return 0
	}
	state.mu.Lock()
	d.registrySnapshot.Store(reg)
	state.revision++
	d.clearPublishedCachesLocked()
	state.routes.Store(buildDynamicToolRouteIndex(nil))
	revision := state.revision
	state.mu.Unlock()
	return revision
}

// narrowPublishedToolsLocked immediately prevents a profile switch from
// serving tools admitted only by the previous (possibly broader) profile.
// Expanding to a broader profile remains safely incomplete until refresh.
func (d *Daemon) narrowPublishedToolsLocked(profile string) {
	if d == nil || d.toolCache == nil {
		return
	}
	d.toolCache.mu.Lock()
	oldTools := d.toolCache.tools
	var tools []mcp.Tool
	if d.profiles != nil {
		tools = d.profiles.Filter(oldTools, profile).Tools
	}
	d.toolCache.tools = tools
	// Force callers to schedule the new revision even when a safe narrowed
	// subset remains available immediately.
	d.toolCache.updatedAt = time.Time{}
	d.toolCache.mu.Unlock()
	if toolNamesChanged(oldTools, tools) && d.eventBus != nil {
		d.eventBus.Publish(EventToolsChanged, map[string]any{
			"old_count": len(oldTools),
			"new_count": len(tools),
		})
	}
}

// clearPublishedCachesLocked removes old-registry data at the same boundary
// that makes the new registry revision visible. Static tools from the new
// registry are served through the profile-filtered fallback while refresh runs.
func (d *Daemon) clearPublishedCachesLocked() {
	if d == nil {
		return
	}
	if d.toolCache != nil {
		d.toolCache.mu.Lock()
		oldTools := d.toolCache.tools
		d.toolCache.tools = nil
		d.toolCache.updatedAt = time.Time{}
		d.toolCache.mu.Unlock()
		if len(oldTools) > 0 && d.eventBus != nil {
			d.eventBus.Publish(EventToolsChanged, map[string]any{
				"old_count": len(oldTools),
				"new_count": 0,
			})
		}
	}
	if d.resourceCache != nil {
		d.resourceCache.mu.Lock()
		d.resourceCache.resources = nil
		d.resourceCache.updatedAt = time.Time{}
		d.resourceCache.mu.Unlock()
	}
	if d.manifest != nil {
		d.manifest.ReplaceServerTools(nil)
	}
}

func (d *Daemon) withCurrentCacheRevision(snapshot cacheRefreshSnapshot, publish func(*cacheRuntimeState)) bool {
	state := d.runtimeCacheState()
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.revision != snapshot.revision ||
		state.activeProfile != snapshot.profile ||
		d.currentRegistry() != snapshot.registry {
		return false
	}
	publish(state)
	return true
}

func toolRefreshSingleflightKey(revision uint64) string {
	return fmt.Sprintf("refresh:%d", revision)
}

func resourceRefreshSingleflightKey(revision uint64) string {
	return fmt.Sprintf("resources-refresh:%d", revision)
}

func buildDynamicToolRouteIndex(routes map[string][]string) *dynamicToolRouteIndex {
	index := &dynamicToolRouteIndex{servers: make(map[string][]string, len(routes))}
	for toolName, candidates := range routes {
		seen := make(map[string]struct{}, len(candidates))
		for _, serverName := range candidates {
			if _, exists := seen[serverName]; exists {
				continue
			}
			seen[serverName] = struct{}{}
			index.servers[toolName] = append(index.servers[toolName], serverName)
		}
	}
	return index
}

// resolveToolServer preserves registry rules/static routes in the dependency
// router, then consults the replaceable live-discovery index. Refresh commits
// replace this index wholesale, so a removed tool cannot remain routable.
func (d *Daemon) resolveToolServer(profile, toolName string, args map[string]any) (string, error) {
	if d == nil || d.router == nil {
		return "", nil
	}
	resolved, err := d.router.ResolveServer(profile, toolName, args)
	if err != nil || resolved != "" {
		return resolved, err
	}
	state := d.runtimeCacheState()
	if state == nil {
		return "", nil
	}
	index := state.routes.Load()
	if index == nil {
		return "", nil
	}
	servers := index.servers[toolName]
	switch len(servers) {
	case 0:
		return "", nil
	case 1:
		return servers[0], nil
	default:
		return "", fmt.Errorf("ambiguous tool %q provided by multiple servers: %v", toolName, servers)
	}
}

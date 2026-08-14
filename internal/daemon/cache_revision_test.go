package daemon

import (
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/internal/router"
	"github.com/crb2nu/loom/pkg/profiles"
	"github.com/crb2nu/loom/pkg/registry"
)

func newCacheRevisionTestDaemon() *Daemon {
	reg := &registry.Registry{Servers: []*registry.Server{{Name: "mcp-git", Common: &registry.TargetSpec{}}}}
	d := newCallPipelineTestDaemon()
	d.registry = reg
	d.router = router.New(router.Config{Registry: reg})
	d.profiles = profiles.NewManager()
	d.fileCfg.Context.ActiveProfile = "full"
	d.toolCache = &ToolCache{ttl: time.Minute}
	d.resourceCache = &ResourceCache{ttl: time.Minute}
	d.manifest = NewManifestManager()
	return d
}

func TestToolCacheOldRevisionCannotOverwriteProfileRefreshOrRoutes(t *testing.T) {
	d := newCacheRevisionTestDaemon()
	d.toolCache.tools = []mcp.Tool{
		{Name: "mcp-git__x"},
		{Name: "mcp-k8s__admin"},
	}
	oldSnapshot := d.cacheSnapshot()

	oldEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	oldPublished := make(chan bool, 1)
	go func() {
		close(oldEntered)
		<-releaseOld
		oldPublished <- d.publishToolCache(
			oldSnapshot,
			[]mcp.Tool{{Name: "mcp-git__x"}},
			map[string][]mcp.Tool{"mcp-git": {{Name: "mcp-git__x"}}},
			map[string][]string{"x": {"mcp-git"}},
		)
	}()
	<-oldEntered

	d.setActiveProfile("dev")
	newSnapshot := d.cacheSnapshot()
	if toolRefreshSingleflightKey(oldSnapshot.revision) == toolRefreshSingleflightKey(newSnapshot.revision) {
		t.Fatalf("tool refresh keys did not change across profile revision %d", oldSnapshot.revision)
	}
	for _, tool := range d.toolCache.tools {
		if tool.Name == "mcp-k8s__admin" {
			t.Fatal("broader-profile tool remained visible while new refresh was pending")
		}
	}

	if !d.publishToolCache(
		newSnapshot,
		[]mcp.Tool{{Name: "mcp-git__y"}},
		map[string][]mcp.Tool{"mcp-git": {{Name: "mcp-git__y"}}},
		map[string][]string{"y": {"mcp-git"}},
	) {
		t.Fatal("new profile revision failed to publish")
	}
	close(releaseOld)
	if <-oldPublished {
		t.Fatal("old profile refresh published after the new revision")
	}

	d.toolCache.mu.RLock()
	tools := append([]mcp.Tool(nil), d.toolCache.tools...)
	d.toolCache.mu.RUnlock()
	if len(tools) != 1 || tools[0].Name != "mcp-git__y" {
		t.Fatalf("tool cache after stale leader release = %v, want [mcp-git__y]", cacheRevisionTestToolNames(tools))
	}
	if resolved, err := d.resolveToolServer("codex", "x", nil); err != nil || resolved != "" {
		t.Fatalf("removed dynamic route x resolved to %q, err=%v", resolved, err)
	}
	if resolved, err := d.resolveToolServer("codex", "y", nil); err != nil || resolved != "mcp-git" {
		t.Fatalf("replacement dynamic route y resolved to %q, err=%v", resolved, err)
	}
	manifestTools, ok := d.manifest.GetServerTools("mcp-git")
	if !ok || len(manifestTools) != 1 || manifestTools[0].Name != "mcp-git__y" {
		t.Fatalf("manifest after stale leader release = %v, found=%v", cacheRevisionTestToolNames(manifestTools), ok)
	}
}

func cacheRevisionTestToolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestResourceCacheOldRegistryRevisionCannotOverwriteNewRefresh(t *testing.T) {
	d := newCacheRevisionTestDaemon()
	oldSnapshot := d.cacheSnapshot()

	oldEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	oldPublished := make(chan bool, 1)
	go func() {
		close(oldEntered)
		<-releaseOld
		oldPublished <- d.publishResourceCache(oldSnapshot, []mcp.Resource{{URI: "old://resource"}})
	}()
	<-oldEntered

	newReg := &registry.Registry{Servers: []*registry.Server{{Name: "mcp-git", Common: &registry.TargetSpec{}}}}
	d.storeRegistryForCacheRevision(newReg)
	newSnapshot := d.cacheSnapshot()
	if resourceRefreshSingleflightKey(oldSnapshot.revision) == resourceRefreshSingleflightKey(newSnapshot.revision) {
		t.Fatalf("resource refresh keys did not change across registry revision %d", oldSnapshot.revision)
	}
	if !d.publishResourceCache(newSnapshot, []mcp.Resource{{URI: "new://resource"}}) {
		t.Fatal("new registry resource refresh failed to publish")
	}
	close(releaseOld)
	if <-oldPublished {
		t.Fatal("old registry resource refresh published after the new revision")
	}

	d.resourceCache.mu.RLock()
	resources := append([]mcp.Resource(nil), d.resourceCache.resources...)
	d.resourceCache.mu.RUnlock()
	if len(resources) != 1 || resources[0].URI != "new://resource" {
		t.Fatalf("resource cache after stale leader release = %+v, want new://resource", resources)
	}
}

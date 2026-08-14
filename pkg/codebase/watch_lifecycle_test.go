package codebase

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/codebase/embed"
)

// Regression tests for the watch lifecycle: duplicate immortal watches were
// the root cause of runaway embedding-token spend. Every codebase_watch_start
// minted a fresh durable watch with no dedup, no TTL, and no root validation,
// so abandoned watches accumulated forever and each one re-embedded every
// file change through the paid embedding API.

func newLifecycleService(t *testing.T, stateDir string, ttl time.Duration) *Service {
	t.Helper()
	svc, err := NewServiceWithEmbedder(Config{
		MaxFileBytes:  2 << 20,
		WatchStateDir: stateDir,
		WatchTTL:      ttl,
	}, embed.NewDummyEmbedder(1))
	if err != nil {
		t.Fatalf("NewServiceWithEmbedder: %v", err)
	}
	return svc
}

func resultJSON(t *testing.T, res *mcp.CallToolResult, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	var m map[string]any
	if uerr := json.Unmarshal([]byte(res.Content[0].Text), &m); uerr != nil {
		t.Fatalf("unmarshal result %q: %v", res.Content[0].Text, uerr)
	}
	return m
}

func startWatch(t *testing.T, svc *Service, root, repoID string) map[string]any {
	t.Helper()
	res, err := svc.HandleWatchStart(context.Background(), map[string]any{
		"root":    root,
		"repo_id": repoID,
	})
	return resultJSON(t, res, err)
}

func stopWatch(t *testing.T, svc *Service, watchID string) {
	t.Helper()
	if _, err := svc.HandleWatchStop(context.Background(), map[string]any{"watch_id": watchID}); err != nil {
		t.Fatalf("HandleWatchStop(%s): %v", watchID, err)
	}
}

func TestWatchStartIsIdempotentPerRoot(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	stateDir := filepath.Join(t.TempDir(), "watches")
	root := t.TempDir()
	svc := newLifecycleService(t, stateDir, 0)

	first := startWatch(t, svc, root, "repo-x")
	id, _ := first["watch_id"].(string)
	if id == "" {
		t.Fatalf("first start returned no watch_id: %v", first)
	}
	defer stopWatch(t, svc, id)
	if reused, _ := first["reused"].(bool); reused {
		t.Fatalf("first start must not be marked reused: %v", first)
	}

	second := startWatch(t, svc, root, "repo-x")
	if got, _ := second["watch_id"].(string); got != id {
		t.Fatalf("second start on the same root created a duplicate watch: %s != %s", got, id)
	}
	if reused, _ := second["reused"].(bool); !reused {
		t.Fatalf("second start must be marked reused: %v", second)
	}

	descriptors, err := svc.watchStore.load()
	if err != nil {
		t.Fatalf("load descriptors: %v", err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("want exactly 1 persisted descriptor after duplicate start, got %d", len(descriptors))
	}
}

func TestWatchStartRejectsMissingRoot(t *testing.T) {
	svc := newLifecycleService(t, filepath.Join(t.TempDir(), "watches"), 0)
	_, err := svc.HandleWatchStart(context.Background(), map[string]any{
		"root":    filepath.Join(t.TempDir(), "does-not-exist"),
		"repo_id": "repo-x",
	})
	if err == nil {
		t.Fatal("expected error starting a watch on a missing root")
	}
}

func TestResumeWatchesDropsMissingRoot(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "watches")
	store := newWatchStore(stateDir)
	if err := store.save(watchDescriptor{
		WatchID:    "aaaa1111",
		RepoID:     "repo-x",
		Root:       filepath.Join(t.TempDir(), "deleted-worktree"),
		DebounceMs: 100,
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed descriptor: %v", err)
	}

	svc := newLifecycleService(t, stateDir, 0)
	if n := svc.ResumeWatches(context.Background()); n != 0 {
		t.Fatalf("ResumeWatches resumed %d watches with a missing root, want 0", n)
	}
	descriptors, _ := store.load()
	if len(descriptors) != 0 {
		t.Fatalf("descriptor with missing root must be removed, %d remain", len(descriptors))
	}
}

func TestResumeWatchesExpiresIdleDescriptor(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "watches")
	store := newWatchStore(stateDir)
	if err := store.save(watchDescriptor{
		WatchID:      "bbbb2222",
		RepoID:       "repo-x",
		Root:         t.TempDir(),
		DebounceMs:   100,
		StartedAt:    time.Now().Add(-3 * time.Hour),
		LastActiveAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("seed descriptor: %v", err)
	}

	svc := newLifecycleService(t, stateDir, time.Hour)
	if n := svc.ResumeWatches(context.Background()); n != 0 {
		t.Fatalf("ResumeWatches resumed %d expired watches, want 0", n)
	}
	descriptors, _ := store.load()
	if len(descriptors) != 0 {
		t.Fatalf("expired descriptor must be removed, %d remain", len(descriptors))
	}
}

func TestResumeWatchesSkipsWatchClaimedByAnotherProcess(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "watches")
	store := newWatchStore(stateDir)
	root := t.TempDir()
	if err := store.save(watchDescriptor{
		WatchID:    "cccc3333",
		RepoID:     "repo-x",
		Root:       root,
		DebounceMs: 100,
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed descriptor: %v", err)
	}

	// Simulate the owning process: flock conflicts across separate opens even
	// within one test process, so a second store cannot claim.
	otherProcess := newWatchStore(stateDir)
	claim, ok := otherProcess.tryClaim("cccc3333")
	if !ok {
		t.Fatal("seed claim should succeed")
	}
	defer releaseClaim(claim)

	svc := newLifecycleService(t, stateDir, 0)
	if n := svc.ResumeWatches(context.Background()); n != 0 {
		t.Fatalf("ResumeWatches resumed %d watches owned by another process, want 0", n)
	}
	if svc.hasWatchJob("cccc3333") {
		t.Fatal("claimed watch must not be tracked in this process")
	}
	descriptors, _ := store.load()
	if len(descriptors) != 1 {
		t.Fatalf("claimed descriptor must be left in place, got %d", len(descriptors))
	}
}

func TestExpireIdleWatchesStopsAbandonedWatch(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	stateDir := filepath.Join(t.TempDir(), "watches")
	svc := newLifecycleService(t, stateDir, time.Hour)

	stale := startWatch(t, svc, t.TempDir(), "repo-stale")
	staleID, _ := stale["watch_id"].(string)
	fresh := startWatch(t, svc, t.TempDir(), "repo-fresh")
	freshID, _ := fresh["watch_id"].(string)
	defer stopWatch(t, svc, freshID)

	now := time.Now()
	svc.watchMu.Lock()
	svc.watchJobs[staleID].lastActive = now.Add(-2 * time.Hour)
	svc.watchMu.Unlock()

	if n := svc.expireIdleWatches(now); n != 1 {
		t.Fatalf("expireIdleWatches = %d, want 1", n)
	}

	svc.watchMu.RLock()
	staleStatus := svc.watchJobs[staleID].status
	freshStatus := svc.watchJobs[freshID].status
	svc.watchMu.RUnlock()
	if staleStatus != "expired" {
		t.Fatalf("stale watch status = %q, want expired", staleStatus)
	}
	if freshStatus != "running" {
		t.Fatalf("fresh watch status = %q, want running", freshStatus)
	}

	descriptors, _ := svc.watchStore.load()
	if len(descriptors) != 1 || descriptors[0].WatchID != freshID {
		t.Fatalf("only the fresh descriptor should remain, got %+v", descriptors)
	}
}

func TestWatchPollAdoptsOrphanedWatch(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	stateDir := filepath.Join(t.TempDir(), "watches")
	root := t.TempDir()
	if err := newWatchStore(stateDir).save(watchDescriptor{
		WatchID:    "dddd4444",
		RepoID:     "repo-x",
		Root:       root,
		DebounceMs: 100,
		Embeddings: true,
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed descriptor: %v", err)
	}

	svc := newLifecycleService(t, stateDir, 0)
	res, err := svc.HandleWatchPoll(context.Background(), map[string]any{"watch_id": "dddd4444"})
	got := resultJSON(t, res, err)
	if found, _ := got["found"].(bool); !found {
		t.Fatalf("poll must adopt the orphaned watch, got %v", got)
	}
	if resumed, _ := got["resumed"].(bool); !resumed {
		t.Fatalf("adopted watch must be marked resumed, got %v", got)
	}
	if !svc.hasWatchJob("dddd4444") {
		t.Fatal("adopted watch must be tracked in memory")
	}
	stopWatch(t, svc, "dddd4444")
}

func TestWatchPollUnknownIDIncludesHint(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	svc := newLifecycleService(t, filepath.Join(t.TempDir(), "watches"), 0)
	res, err := svc.HandleWatchPoll(context.Background(), map[string]any{"watch_id": "eeee5555"})
	got := resultJSON(t, res, err)
	if found, _ := got["found"].(bool); found {
		t.Fatalf("unknown watch must report found:false, got %v", got)
	}
	if hint, _ := got["hint"].(string); hint == "" {
		t.Fatalf("found:false must carry a restart hint, got %v", got)
	}
}

func TestWatchListReportsWatches(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	stateDir := filepath.Join(t.TempDir(), "watches")
	svc := newLifecycleService(t, stateDir, 0)

	started := startWatch(t, svc, t.TempDir(), "repo-x")
	id, _ := started["watch_id"].(string)
	defer stopWatch(t, svc, id)

	res, err := svc.HandleWatchList(context.Background(), nil)
	got := resultJSON(t, res, err)
	if count, _ := got["count"].(float64); count != 1 {
		t.Fatalf("watch list count = %v, want 1", got["count"])
	}
	if running, _ := got["running"].(float64); running != 1 {
		t.Fatalf("watch list running = %v, want 1", got["running"])
	}
}

func TestWatchClaimIsExclusiveAndReleasable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "watches")
	store1 := newWatchStore(dir)
	store2 := newWatchStore(dir)

	claim, ok := store1.tryClaim("ffff6666")
	if !ok {
		t.Fatal("first claim should succeed")
	}
	if _, ok := store2.tryClaim("ffff6666"); ok {
		t.Fatal("second claim must fail while the first is held")
	}
	releaseClaim(claim)
	claim2, ok := store2.tryClaim("ffff6666")
	if !ok {
		t.Fatal("claim must be takeable after release")
	}
	releaseClaim(claim2)
}

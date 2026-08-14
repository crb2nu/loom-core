package codebase

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/codebase/embed"
)

func newTestService(t *testing.T, stateDir string) *Service {
	t.Helper()
	svc, err := NewServiceWithEmbedder(Config{
		MaxFileBytes:  2 << 20,
		WatchStateDir: stateDir,
	}, embed.NewDummyEmbedder(1))
	if err != nil {
		t.Fatalf("NewServiceWithEmbedder: %v", err)
	}
	return svc
}

func TestWatchStoreRoundTrip(t *testing.T) {
	store := newWatchStore(filepath.Join(t.TempDir(), "watches"))

	d1 := watchDescriptor{
		WatchID:     "id1",
		RepoID:      "r1",
		Root:        "/abs/root1",
		Languages:   []string{"go"},
		Exclude:     []string{"vendor"},
		DebounceMs:  750,
		GitMetadata: true,
		Embeddings:  true,
		StartedAt:   time.Now().Truncate(time.Second),
	}
	d2 := watchDescriptor{WatchID: "id2", RepoID: "r2", Root: "/abs/root2", DebounceMs: 100}

	if err := store.save(d1); err != nil {
		t.Fatalf("save d1: %v", err)
	}
	if err := store.save(d2); err != nil {
		t.Fatalf("save d2: %v", err)
	}

	got, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("load returned %d descriptors, want 2", len(got))
	}

	byID := map[string]watchDescriptor{}
	for _, d := range got {
		byID[d.WatchID] = d
	}
	if g := byID["id1"]; g.Root != "/abs/root1" || !g.GitMetadata || len(g.Languages) != 1 || g.Languages[0] != "go" || g.DebounceMs != 750 {
		t.Errorf("id1 round-trip mismatch: %+v", g)
	}

	// remove deletes and is idempotent.
	if err := store.remove("id1"); err != nil {
		t.Fatalf("remove id1: %v", err)
	}
	if err := store.remove("id1"); err != nil {
		t.Fatalf("second remove should be a no-op: %v", err)
	}
	got, _ = store.load()
	if len(got) != 1 || got[0].WatchID != "id2" {
		t.Fatalf("after remove want only id2, got %+v", got)
	}
}

func TestWatchStoreDisabledIsNoop(t *testing.T) {
	store := newWatchStore("")
	if store.enabled() {
		t.Fatal("empty dir should be disabled")
	}
	if err := store.save(watchDescriptor{WatchID: "x", Root: "/r"}); err != nil {
		t.Fatalf("disabled save should be no-op: %v", err)
	}
	got, err := store.load()
	if err != nil || len(got) != 0 {
		t.Fatalf("disabled load = (%v, %v), want ([], nil)", got, err)
	}
	if err := store.remove("x"); err != nil {
		t.Fatalf("disabled remove should be no-op: %v", err)
	}
}

func TestWatchStoreRejectsUnsafeID(t *testing.T) {
	store := newWatchStore(filepath.Join(t.TempDir(), "watches"))
	if err := store.save(watchDescriptor{WatchID: "../escape", Root: "/r"}); err == nil {
		t.Fatal("expected error persisting a path-traversal watch id")
	}
}

// TestResumeWatchesReattachesPersistedWatch is the core regression: a watch
// persisted by one process must be re-launched when a fresh process
// (idle-reaper kill, transport-storm respawn, crash, or deploy) starts over the
// same state dir — instead of being silently lost.
func TestResumeWatchesReattachesPersistedWatch(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "watches")

	// First process: persist a running-watch descriptor.
	desc := watchDescriptor{
		WatchID:    "deadbeefcafe",
		RepoID:     "repo-1",
		Root:       t.TempDir(),
		DebounceMs: 100,
		Embeddings: true, // skip the qdrant startup path; keep the test offline
		StartedAt:  time.Now(),
	}
	if err := newWatchStore(stateDir).save(desc); err != nil {
		t.Fatalf("persist descriptor: %v", err)
	}

	// Second process (simulated restart): fresh service over the same dir.
	svc := newTestService(t, stateDir)
	if svc.hasWatchJob(desc.WatchID) {
		t.Fatal("fresh service should start with no in-memory watches")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop the resumed watch goroutine when the test ends

	if n := svc.ResumeWatches(ctx); n != 1 {
		t.Fatalf("ResumeWatches = %d, want 1", n)
	}
	if !svc.hasWatchJob(desc.WatchID) {
		t.Fatalf("watch %s was not re-attached after resume", desc.WatchID)
	}
}

// TestWatchStopRemovesPersistedDescriptor proves stop is authoritative cleanup:
// a stopped watch must not resume after a restart, even when it is no longer
// tracked in memory.
func TestWatchStopRemovesPersistedDescriptor(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "watches")
	svc := newTestService(t, stateDir)

	if err := svc.watchStore.save(watchDescriptor{WatchID: "abc", RepoID: "r", Root: t.TempDir()}); err != nil {
		t.Fatalf("seed descriptor: %v", err)
	}

	if _, err := svc.HandleWatchStop(context.Background(), map[string]any{"watch_id": "abc"}); err != nil {
		t.Fatalf("HandleWatchStop: %v", err)
	}

	got, err := svc.watchStore.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("descriptor not removed after stop: %d remain", len(got))
	}
}

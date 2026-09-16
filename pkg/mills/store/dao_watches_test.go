package store

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchLifecycleFiltersAndConcurrentResolution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mills.db")
	st, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	w := &Watch{SubjectKind: WatchSubjectBacklogItem, SubjectID: "bl-1", TerminalCondition: "merged", Note: "land it", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.Watches.Register(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := st.Watches.List(ctx, WatchFilter{State: WatchActive, SubjectKind: WatchSubjectBacklogItem, SubjectID: "bl-1"})
	if err != nil || len(got) != 1 {
		t.Fatalf("filtered list=%v err=%v", got, err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, e := st.Watches.Resolve(ctx, w.ID, WatchMet, "merged", now.Add(time.Minute))
			if e != nil {
				t.Errorf("resolve: %v", e)
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("resolution wins=%d want 1", wins.Load())
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	persisted, err := st.Watches.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != WatchMet || persisted.ResolvedAt == nil || persisted.Resolution != "merged" {
		t.Fatalf("persisted=%+v", persisted)
	}
}

func TestWatchRejectsInvalidTTL(t *testing.T) {
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	err = st.Watches.Register(context.Background(), &Watch{SubjectKind: WatchSubjectPipelineRun, SubjectID: "run", TerminalCondition: "done", CreatedAt: now, ExpiresAt: now})
	if err == nil {
		t.Fatal("expected invalid TTL")
	}
}

package mills

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestSweepWatchesExpiresOnceWithAttentionEvent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	w := &store.Watch{SubjectKind: store.WatchSubjectBacklogItem, SubjectID: "missing-is-not-read-after-expiry", TerminalCondition: "merged", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	if err := st.Watches.Register(ctx, w); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Store: st, Clock: func() time.Time { return now }}
	for i := 0; i < 2; i++ {
		if _, err := r.SweepWatches(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Watches.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.WatchExpired {
		t.Fatalf("state=%s", got.State)
	}
	n, err := st.Events.CountBySubjectKind(ctx, "watch", w.ID, "attention.watch.expired")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("events=%d want 1", n)
	}
}

func TestFactoryHealthPagesOnceUntilRecovery(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var pushed []string
	r := &Reconciler{
		Clock: func() time.Time { return now },
		MobilePushEvent: func(_ context.Context, event store.Event) error {
			mu.Lock()
			defer mu.Unlock()
			pushed = append(pushed, event.Kind)
			return nil
		},
		DigestEvent: func(context.Context, store.Event) error {
			t.Fatal("page event reached digest sink")
			return nil
		},
	}
	zero, one := 0, 1
	observe := func(delta time.Duration, observation FactoryHealthObservation) {
		now = now.Add(delta)
		if err := r.ObserveFactoryHealth(context.Background(), observation); err != nil {
			t.Fatal(err)
		}
	}

	observe(0, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	observe(MainRedPageThreshold-time.Second, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	if len(pushed) != 0 {
		t.Fatalf("early pages=%v", pushed)
	}
	observe(time.Second, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	observe(time.Hour, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	if len(pushed) != 1 || pushed[0] != PageEventMainRed {
		t.Fatalf("red pages=%v", pushed)
	}
	observe(AutonomousMergePageThreshold-MainRedPageThreshold-time.Hour, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	if len(pushed) != 2 || pushed[1] != PageEventAutonomousMergeStarved {
		t.Fatalf("starvation pages=%v", pushed)
	}
	observe(time.Hour, FactoryHealthObservation{}) // unknown resets continuity, not fired state
	observe(time.Hour, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	observe(AutonomousMergePageThreshold, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	if len(pushed) != 2 {
		t.Fatalf("unknown re-paged fired window: %v", pushed)
	}
	observe(0, FactoryHealthObservation{MainKnown: true, MainGreen: true, AutonomousMerges24h: &one})
	observe(0, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	observe(AutonomousMergePageThreshold, FactoryHealthObservation{MainKnown: true, AutonomousMerges24h: &zero})
	if len(pushed) != 4 || pushed[2] != PageEventMainRed || pushed[3] != PageEventAutonomousMergeStarved {
		t.Fatalf("re-armed pages=%v", pushed)
	}
}

func TestFactoryHealthUsesRecoveredMainRedDuration(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	pages := 0
	r := &Reconciler{Clock: func() time.Time { return now }, MobilePushEvent: func(_ context.Context, event store.Event) error {
		pages++
		if event.Kind != PageEventMainRed {
			t.Fatalf("kind=%s", event.Kind)
		}
		return nil
	}}
	if err := r.ObserveFactoryHealth(context.Background(), FactoryHealthObservation{MainKnown: true, MainRedDuration: MainRedPageThreshold}); err != nil {
		t.Fatal(err)
	}
	if pages != 1 {
		t.Fatalf("pages=%d want 1", pages)
	}
}

func TestSweepWatchesTerminalDigestNeverPushes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	if err := st.Backlog.Put(ctx, &store.BacklogItem{ID: "BL-WATCH", Title: "watched", State: store.BacklogMerged, Priority: store.P2, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	w := &store.Watch{SubjectKind: store.WatchSubjectBacklogItem, SubjectID: "BL-WATCH", TerminalCondition: "merged", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	if err := st.Watches.Register(ctx, w); err != nil {
		t.Fatal(err)
	}
	pushes, digests := 0, 0
	r := &Reconciler{Store: st, Clock: func() time.Time { return now }, MobilePushEvent: func(context.Context, store.Event) error { pushes++; return nil }, DigestEvent: func(_ context.Context, event store.Event) error { digests++; return nil }}
	for i := 0; i < 2; i++ {
		if _, err := r.SweepWatches(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if pushes != 0 || digests != 1 {
		t.Fatalf("pushes=%d digests=%d", pushes, digests)
	}
}

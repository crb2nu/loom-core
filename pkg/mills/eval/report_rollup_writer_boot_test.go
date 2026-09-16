package eval

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestReportRollupWriterRunFiresOnFirstRefreshOnce pins the boot-ordering
// hook: OnFirstRefresh fires exactly once, only after the boot refresh has
// returned (every spec built), and even when a spec fails — the operator's
// reconciler waits on it and must never be held by a broken report.
func TestReportRollupWriterRunFiresOnFirstRefreshOnce(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var builds, fires atomic.Int32
	released := make(chan struct{})
	w := &ReportRollupWriter{
		Reports:  st.Reports,
		Interval: time.Hour,
		Specs: []ReportRollupSpec{
			{Name: "ok", Window: time.Hour, Build: func(context.Context, time.Time, time.Time) (any, error) {
				builds.Add(1)
				return map[string]any{"ok": true}, nil
			}},
			{Name: "broken", Window: time.Hour, Build: func(context.Context, time.Time, time.Time) (any, error) {
				builds.Add(1)
				return nil, errors.New("boom")
			}},
		},
		OnFirstRefresh: func() {
			if fires.Add(1) == 1 {
				close(released)
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("OnFirstRefresh did not fire after the boot refresh")
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("specs built before OnFirstRefresh = %d, want 2 (hook fires after the whole refresh)", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fires.Load(); got != 1 {
		t.Fatalf("OnFirstRefresh fired %d times, want 1", got)
	}
}

package eval

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestReportRollupWriterFailureRetainsLastValidSnapshot(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	fail := false
	w := &ReportRollupWriter{Reports: st.Reports, Now: func() time.Time { return now }, Specs: []ReportRollupSpec{{Name: "x", Window: time.Hour, Build: func(context.Context, time.Time, time.Time) (any, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return map[string]any{"ok": true}, nil
	}}}}
	if err := w.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail = true
	now = now.Add(time.Hour)
	if err := w.Refresh(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	snap, err := st.Reports.Latest(context.Background(), "x", 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.SnapshotAt.Equal(now) {
		t.Fatal("failed refresh replaced snapshot")
	}
}

func TestReportRollupWriterHonorsCancellation(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &ReportRollupWriter{Reports: st.Reports, Specs: []ReportRollupSpec{{Name: "x", Window: time.Hour, Build: func(ctx context.Context, _ time.Time, _ time.Time) (any, error) { return nil, ctx.Err() }}}}
	if err := w.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestReportRollupWriterBoundsBuildTimeout(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := &ReportRollupWriter{Reports: st.Reports, Timeout: time.Millisecond, Specs: []ReportRollupSpec{{Name: "slow", Window: time.Hour, Build: func(ctx context.Context, _ time.Time, _ time.Time) (any, error) { <-ctx.Done(); return nil, ctx.Err() }}}}
	if err := w.Refresh(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if _, err := st.Reports.Latest(context.Background(), "slow", 3600, ""); err != store.ErrNotFound {
		t.Fatalf("snapshot after timeout: %v", err)
	}
}

package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// ReportRollupSpec describes one independently materialized report tuple.
type ReportRollupSpec struct {
	Name   string
	Window time.Duration
	Key    string
	Build  func(context.Context, time.Time, time.Time) (any, error)
}

// ReportRollupWriter moves expensive report aggregation off request paths.
type ReportRollupWriter struct {
	Reports  *store.ReportRollupDAO
	Specs    []ReportRollupSpec
	Interval time.Duration
	Timeout  time.Duration
	Now      func() time.Time
	Logger   *slog.Logger
	// OnFirstRefresh, when set, is called exactly once after Run's immediate
	// boot refresh returns, success or failure. The boot refresh walks the
	// widest event windows the operator reads (~30s on a cold page cache,
	// 2026-09-08), so it doubles as the store warm-up: the operator releases
	// the boot phase that lets the reconciler's first tick and the intake
	// loops start behind it rather than in the middle of it.
	OnFirstRefresh func()
}

func (w *ReportRollupWriter) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

// Refresh builds each report before writing it, so failures retain the prior
// snapshot. Specs are isolated: one broken report does not starve the others.
func (w *ReportRollupWriter) Refresh(ctx context.Context) error {
	if w == nil || w.Reports == nil {
		return fmt.Errorf("report rollup writer: store required")
	}
	var first error
	for _, spec := range w.Specs {
		started, now := time.Now(), w.now()
		buildCtx := ctx
		cancel := func() {}
		if w.Timeout > 0 {
			buildCtx, cancel = context.WithTimeout(ctx, w.Timeout)
		}
		payload, err := spec.Build(buildCtx, now.Add(-spec.Window), now)
		cancel()
		// Kept separate from building to guarantee no invalid/partial payload is written.
		if err == nil {
			raw, marshalErr := json.Marshal(payload)
			if marshalErr != nil {
				err = marshalErr
			} else {
				err = w.Reports.Put(ctx, &store.ReportRollupSnapshot{ReportName: spec.Name, WindowSeconds: int(spec.Window.Seconds()), ReportKey: spec.Key, SnapshotAt: now, Payload: raw})
			}
		}
		if w.Logger != nil {
			args := []any{"report", spec.Name, "window", spec.Window.String(), "duration", time.Since(started).String()}
			if err != nil {
				w.Logger.Warn("report rollup refresh failed", append(args, "error", err)...)
			} else {
				w.Logger.Info("report rollup refreshed", append(args, "freshness", "0s")...)
			}
		}
		if err != nil && first == nil {
			first = fmt.Errorf("%s: %w", spec.Name, err)
		}
	}
	return first
}

func (w *ReportRollupWriter) Run(ctx context.Context) error {
	_ = w.Refresh(ctx)
	if w.OnFirstRefresh != nil {
		w.OnFirstRefresh()
	}
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = w.Refresh(ctx)
		}
	}
}

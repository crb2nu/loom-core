package intake

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// CanaryGC sweeps stale escalated mills-canary backlog items so they
// stop blocking new canary enqueues forever.
//
// Context: commit 2fcc705a (2026-05-24) made escalated canaries block
// new canary enqueues *regardless of age* to stop dupe accumulation.
// That fix paired with the kill-test discovery that ~62% of escalations
// were transient infrastructure flakes, not real bugs — so the original
// "human must investigate before next canary" assumption no longer
// holds. Slice 3d auto-retries transient-cap escalations; this sweep
// cleans up the legacy escalated canaries that pre-date 3d so the
// operator queue can flow.
//
// Sweep logic: delete every backlog item that
//   - carries the canary label,
//   - is in state Escalated, and
//   - was created more than StaleAfter ago.
//
// Other states (queued, running, merged) are left alone — running
// canaries are still in-flight, merged ones are historical.
type CanaryGC struct {
	store  CanaryGCStore
	cfg    CanaryGCConfig
	logger *slog.Logger
	// Enabled is the live global admission barrier. Nil preserves standalone
	// behavior; production wires policy.enabled.
	Enabled func() bool
	active  atomic.Int64
}

// CanaryGCConfig captures the operator-tunable knobs.
type CanaryGCConfig struct {
	StaleAfter time.Duration // default 48h
	Interval   time.Duration // default 1h
	Label      string        // default "mills-canary"
	DryRun     bool          // when true, log candidates but do not delete
}

// CanaryGCStore is the slim Store surface the GC uses. *store.Store
// satisfies via embedding of *store.BacklogDAO.
type CanaryGCStore interface {
	ListByState(ctx context.Context, state store.BacklogState) ([]*store.BacklogItem, error)
	Delete(ctx context.Context, id string) error
}

const (
	defaultStaleAfter  = 48 * time.Hour
	defaultGCInterval  = 1 * time.Hour
	defaultCanaryLabel = "mills-canary"
)

func (c *CanaryGCConfig) applyDefaults() {
	if c.StaleAfter <= 0 {
		c.StaleAfter = defaultStaleAfter
	}
	if c.Interval <= 0 {
		c.Interval = defaultGCInterval
	}
	if c.Label == "" {
		c.Label = defaultCanaryLabel
	}
}

// NewCanaryGC wires config + store + logger. Defaults applied to zero
// fields. Logger nil → slog.Default.
func NewCanaryGC(st CanaryGCStore, cfg CanaryGCConfig, logger *slog.Logger) *CanaryGC {
	cfg.applyDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &CanaryGC{store: st, cfg: cfg, logger: logger}
}

// Run drives Tick on Interval until ctx is canceled. Errors from a
// single tick are logged and the loop continues — a transient store
// failure should not stop the GC permanently.
func (g *CanaryGC) Run(ctx context.Context) error {
	g.logger.Info("canary GC started",
		"stale_after", g.cfg.StaleAfter,
		"interval", g.cfg.Interval,
		"label", g.cfg.Label,
		"dry_run", g.cfg.DryRun,
	)
	if n, err := g.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		g.logger.Warn("canary GC initial tick failed", "err", err)
	} else if n > 0 {
		g.logger.Info("canary GC initial sweep deleted items", "count", n)
	}
	t := time.NewTicker(g.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n, err := g.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Warn("canary GC tick failed", "err", err)
			} else if n > 0 {
				g.logger.Info("canary GC swept stale canaries", "count", n)
			}
		}
	}
}

// Tick performs one GC pass and returns the count of items deleted (or
// counted in dry-run mode). Safe to call from tests / admin endpoints.
func (g *CanaryGC) Tick(ctx context.Context) (int, error) {
	g.active.Add(1)
	defer g.active.Add(-1)
	if g.Enabled != nil && !g.Enabled() {
		return 0, nil
	}
	items, err := g.store.ListByState(ctx, store.BacklogEscalated)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().UTC().Add(-g.cfg.StaleAfter)
	deleted := 0
	for _, it := range items {
		if it == nil || !hasLabel(it, g.cfg.Label) {
			continue
		}
		if !it.CreatedAt.Before(cutoff) {
			continue
		}
		if g.cfg.DryRun {
			g.logger.Info("canary GC would delete",
				"id", it.ID, "created_at", it.CreatedAt, "dry_run", true)
			deleted++
			continue
		}
		if err := g.store.Delete(ctx, it.ID); err != nil {
			g.logger.Warn("canary GC delete failed",
				"id", it.ID, "err", err)
			continue
		}
		g.logger.Info("canary GC deleted stale escalated canary",
			"id", it.ID, "created_at", it.CreatedAt)
		deleted++
	}
	return deleted, nil
}

// ActiveOperations reports stale-canary GC passes currently executing.
func (g *CanaryGC) ActiveOperations() int64 {
	if g == nil {
		return 0
	}
	return g.active.Load()
}

func hasLabel(it *store.BacklogItem, label string) bool {
	for _, l := range it.Labels {
		if l == label {
			return true
		}
	}
	return false
}

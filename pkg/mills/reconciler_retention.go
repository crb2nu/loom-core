package mills

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store retention + event-noise gating for the reconciler.
//
// Measured on the production store copy of 2026-09-02 (450MB, 285k events):
// 92% of the last week's 75k event rows were the reconciler's own per-tick
// bookkeeping — 22k `reconciler.deferred`, 22k `reconciler.ghost_spark_skipped`,
// 9.4k `reconciler.tick`, 6.5k `inflight_redriven`, 3.7k `skipped` — most of
// them the SAME item re-stating the SAME reason every minute. kpi_snapshots
// (one row per window per minute since May) was 214k rows / 196MB, the
// largest object in the file. Every window read (KPI writer, promotion
// report, judge calibration, HUD panels) paid for that growth, and the
// operator logged "kpi snapshot failed: event scan: context deadline
// exceeded" ~10×/h all day.
//
// Two complementary controls:
//   - noiseGate: a per-process cooldown that suppresses an identical
//     (kind, subject, outcome, reason) row inside eventNoiseCooldown. The
//     first occurrence and every change still land, so the deferral /
//     ghost-spark evidence operators read stays truthful; only the
//     minute-by-minute restatement is dropped.
//   - SweepRetention: a daily bounded prune of those bookkeeping kinds
//     older than eventNoiseRetention and of kpi_snapshots older than
//     kpiSnapshotRetention. Audit kinds (council, overseer, pipeline,
//     merge-queue, verdict corrections) are never pruned.
const (
	// eventNoiseCooldown bounds how often an identical bookkeeping row may
	// repeat for the same subject. 30m keeps a long deferral visible on the
	// timeline without a row per tick.
	eventNoiseCooldown = 30 * time.Minute
	// eventNoiseRetention is how long bookkeeping rows stay queryable. The
	// longest reader of these kinds is the 14-day scope/deferral evidence in
	// escalation triage.
	eventNoiseRetention = 14 * 24 * time.Hour
	// kpiSnapshotRetention keeps a quarter of minute-granularity snapshots;
	// the deepest reader (shift report) looks back 2×720h.
	kpiSnapshotRetention = 90 * 24 * time.Hour
	// DefaultRetentionInterval is how often the retention sweep runs once the
	// store is clean (a sweep completed inside its budget).
	DefaultRetentionInterval = 24 * time.Hour
	// retentionCatchUpInterval is the retry cadence after a sweep that did
	// NOT finish inside retentionSweepTimeout. Backing off a whole day after a
	// timed-out pass is a starvation loop: the bloat that makes the prune slow
	// is exactly what the prune never gets to remove. Live 2026-09-08 the
	// production store (497MB) still held 82k bookkeeping events past the
	// 14-day retention and ~130k KPI snapshots past 90 days because every
	// daily pass timed out on a cold cache and was not retried until the next
	// day. A timed-out pass now comes back after this interval, and keeps
	// coming back, until one pass runs to completion.
	retentionCatchUpInterval = 10 * time.Minute
	retentionSweepTimeout    = 2 * time.Minute
	// retentionPruneBatch bounds one DELETE. Each batch holds the SQLite write
	// lock for its duration; at 5000 rows of ~1KB KPI JSON on the Longhorn
	// volume that exceeded busy_timeout (5s) and surfaced as
	// `database is locked (SQLITE_BUSY)` on the ledger writer's append-once
	// rows (merge-queue verdict corrections, 2026-09-08 17:29Z, exactly while
	// the retention sweep ran). 1000 keeps a batch well under a second so
	// concurrent writers serialise through busy_timeout instead of failing.
	retentionPruneBatch = 1000
	// noiseGateMaxEntries bounds the cooldown map; beyond it the oldest
	// entries are evicted so a long-running operator cannot grow it forever.
	noiseGateMaxEntries = 20000
)

// reconcilerNoiseKinds are the per-tick bookkeeping kinds subject to both
// the cooldown and retention. Adding a kind here is a statement that the
// row is operational exhaust, not audit evidence.
var reconcilerNoiseKinds = []string{
	"reconciler.tick",
	"reconciler.deferred",
	"reconciler.skipped",
	"reconciler.ghost_spark_skipped",
	"reconciler.ghost_spark_sweep",
	"reconciler.inflight_redriven",
	"reconciler.escalation_sweep",
	"reconciler.auto_requeue_sweep",
}

var reconcilerNoiseKindSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(reconcilerNoiseKinds))
	for _, k := range reconcilerNoiseKinds {
		m[k] = struct{}{}
	}
	return m
}()

// noiseGate is the per-process cooldown for repeated bookkeeping rows.
type noiseGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether a row with this identity may be appended now, and
// records it as the latest occurrence when it may.
func (g *noiseGate) allow(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = make(map[string]time.Time)
	}
	if at, ok := g.last[key]; ok && now.Sub(at) < eventNoiseCooldown {
		return false
	}
	if len(g.last) >= noiseGateMaxEntries {
		g.evictOldest(len(g.last) / 4)
	}
	g.last[key] = now
	return true
}

func (g *noiseGate) evictOldest(n int) {
	type kv struct {
		k string
		t time.Time
	}
	entries := make([]kv, 0, len(g.last))
	for k, t := range g.last {
		entries = append(entries, kv{k, t})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].t.Before(entries[j].t) })
	for i := 0; i < n && i < len(entries); i++ {
		delete(g.last, entries[i].k)
	}
}

// noiseKey derives the identity of a bookkeeping row: the kind, outcome, and
// whichever subject/reason fields the payload carries. Two rows with the
// same key say the same thing about the same subject.
func noiseKey(kind, outcome string, payload map[string]any) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteByte('|')
	b.WriteString(outcome)
	for _, field := range []string{"item", "backlog", "backlog_id", "run", "run_id", "reason", "blocked_by", "precondition", "skip_reason", "policy"} {
		if v, ok := payload[field]; ok && v != nil {
			b.WriteByte('|')
			b.WriteString(field)
			b.WriteByte('=')
			b.WriteString(fmt.Sprint(v))
		}
	}
	return b.String()
}

// allowNoise decides whether a bookkeeping row may be appended. Non-noise
// kinds always pass; the gate is nil-safe so direct constructions in tests
// keep the old always-append behaviour until they opt in.
func (r *Reconciler) allowNoise(kind, outcome string, payload map[string]any) bool {
	if r == nil {
		return true
	}
	if _, noisy := reconcilerNoiseKindSet[kind]; !noisy {
		return true
	}
	// reconciler.tick is the scheduler heartbeat: one row per tick is its
	// whole purpose, so it is retained-only, never cooled down.
	if kind == "reconciler.tick" {
		return true
	}
	if r.noise == nil {
		r.noiseOnce.Do(func() { r.noise = &noiseGate{} })
	}
	allowed := r.noise.allow(noiseKey(kind, outcome, payload), r.now())
	if !allowed {
		EventNoiseSuppressedTotal.WithLabelValues(kind).Inc()
	}
	return allowed
}

// RetentionSweepResult reports what one retention pass removed.
type RetentionSweepResult struct {
	EventsPruned       int64
	KPISnapshotsPruned int64
}

func (r *Reconciler) retentionInterval() time.Duration {
	if r != nil && r.RetentionInterval > 0 {
		return r.RetentionInterval
	}
	return DefaultRetentionInterval
}

// retentionDue reports whether the daily sweep should run at this tick.
func (r *Reconciler) retentionDue(now time.Time) bool {
	if r == nil || r.Store == nil || r.Store.Events == nil || r.Store.KPI == nil || r.RetentionDisabled {
		return false
	}
	return !now.Before(r.nextRetention)
}

// sweepRetentionDue runs the retention sweep when it is due and schedules the
// next pass: DefaultRetentionInterval after a pass that ran to completion,
// retentionCatchUpInterval after one that hit its budget (or whose parent tick
// context expired underneath it). The pessimistic catch-up slot is armed
// BEFORE the sweep so a parent-context failure that returns early still
// retries soon rather than immediately on every tick. Housekeeping never marks
// reconcile health errored: only a spent parent context is returned as an
// error, mirroring the other sweeps in Tick.
func (r *Reconciler) sweepRetentionDue(ctx context.Context, now time.Time) error {
	if !r.retentionDue(now) {
		return nil
	}
	r.nextRetention = now.Add(retentionCatchUpInterval)
	rtCtx, rtCancel := context.WithTimeout(ctx, retentionSweepTimeout)
	pruned, rtErr := r.SweepRetention(rtCtx)
	rtCancel()
	if parentErr := ctx.Err(); parentErr != nil {
		return fmt.Errorf("retention sweep: %w", parentErr)
	}
	if rtErr != nil {
		if r.Logger != nil {
			r.Logger.Warn("reconciler: retention sweep did not finish; retrying on the catch-up cadence",
				"error", rtErr, "retry_in", retentionCatchUpInterval,
				"events_pruned", pruned.EventsPruned, "kpi_pruned", pruned.KPISnapshotsPruned)
		}
		return nil
	}
	r.nextRetention = now.Add(r.retentionInterval())
	if r.Logger != nil && (pruned.EventsPruned > 0 || pruned.KPISnapshotsPruned > 0) {
		r.Logger.Info("reconciler: retention sweep",
			"events_pruned", pruned.EventsPruned, "kpi_pruned", pruned.KPISnapshotsPruned)
	}
	return nil
}

// SweepRetention prunes bookkeeping events past eventNoiseRetention and KPI
// snapshots past kpiSnapshotRetention in bounded batches. Read the
// constants above before widening either set: audit kinds are evidence.
func (r *Reconciler) SweepRetention(ctx context.Context) (RetentionSweepResult, error) {
	res := RetentionSweepResult{}
	if r == nil || r.Store == nil || r.Store.Events == nil || r.Store.KPI == nil {
		return res, nil
	}
	now := r.now()
	pruned, err := r.Store.Events.PruneKindsBefore(ctx, reconcilerNoiseKinds, now.Add(-eventNoiseRetention), retentionPruneBatch)
	res.EventsPruned = pruned
	if err != nil {
		return res, fmt.Errorf("prune bookkeeping events: %w", err)
	}
	kpi, err := r.Store.KPI.PruneBefore(ctx, now.Add(-kpiSnapshotRetention), retentionPruneBatch)
	res.KPISnapshotsPruned = kpi
	if err != nil {
		return res, fmt.Errorf("prune kpi snapshots: %w", err)
	}
	if res.EventsPruned > 0 {
		RetentionPrunedTotal.WithLabelValues("events").Add(float64(res.EventsPruned))
	}
	if res.KPISnapshotsPruned > 0 {
		RetentionPrunedTotal.WithLabelValues("kpi_snapshots").Add(float64(res.KPISnapshotsPruned))
	}
	return res, nil
}

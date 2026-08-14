// Package takeup implements the take-up motion (Live Beam slice 2,
// .loom/brainstorm-mills-steering-preparation-line-2026-07-03.md): on a loom
// the take-up roller winds finished cloth and advances the warp automatically
// — nobody moves the beam by hand. Here, plan/slice lifecycle phases advance
// only when an agent remembers to call agent_plan_lifecycle_advance, and
// mr_refs are append-only strings with no back-pressure from GitLab — so the
// Plan Store drifts stale the moment work merges outside the recorded flow.
//
// The reconciler polls active plans in a configured project+namespace and
// trues them to MR reality:
//   - a slice whose MR merged advances to phase "merged";
//   - the slice's emitted Mills backlog item (psl-…) is marked merged when it
//     was still queued/paused/escalated (a running item is the pipeline's to
//     finish; it is never touched);
//   - a slice whose MR closed WITHOUT merging gets a deduped orphan decision
//     note (state says in-flight, reality says dead — a human or planner must
//     re-plan or manually close it);
//   - a plan whose slices are ALL merged is advanced hop-by-hop along the
//     phase DAG toward "merged", each hop recorded in phase_history with
//     "mills:take-up" attribution.
//
// FAIL-CLOSED like the plan-slice emitter: an empty Namespace makes every
// tick a no-op, so the reconciler never rewrites arbitrary planning-scaffold
// state.
package takeup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/intake"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// PlanStore is the reconciler's view of the Plan Store, satisfied by
// *clients.PlanClient. Narrow so tests fake it without the MCP hub.
type PlanStore interface {
	ListPlans(ctx context.Context, project, namespace, phase string) ([]clients.PlanSummary, error)
	ListSlices(ctx context.Context, planID string) ([]clients.PlanSliceSummary, error)
	GetSlice(ctx context.Context, sliceID string) (clients.PlanSliceSummary, error)
	UpdateSlicePhase(ctx context.Context, sliceID, phase string) error
	AppendSliceDecision(ctx context.Context, sliceID, note string) error
	AdvancePlan(ctx context.Context, planID, toPhase, note string) error
}

// MRStater resolves a merge request's lifecycle state by IID, satisfied by
// *clients.GitLabClient.
type MRStater interface {
	MRState(ctx context.Context, mrIID int64) (string, error)
}

// PatternHarvester feeds the Pattern Loom's taste gate from merged stamped
// plans (factory model junction J2, docs/FACTORY_MODEL.md). Satisfied by
// *clients.PatternClient. OPTIONAL on the reconciler: nil disables the
// harvest entirely, preserving pre-J2 behavior.
type PatternHarvester interface {
	ListApprovedPatterns(ctx context.Context) ([]council.PatternRef, error)
	RecordInstance(ctx context.Context, patternID, mrRef, repo string) (clients.PatternHarvest, error)
}

// BacklogStore is the Get+Put slice of the Mills backlog the reconciler needs
// (same shape the intake package uses).
type BacklogStore interface {
	Get(ctx context.Context, id string) (*store.BacklogItem, error)
	Put(ctx context.Context, item *store.BacklogItem) error
}

// Config captures the operator-tunable knobs. Defaults apply on zero fields.
type Config struct {
	Project       string
	GitLabBaseURL string // REQUIRED to authorize absolute MR URL hosts
	Namespace     string // REQUIRED: empty = inert (fail-closed)
	PollInterval  time.Duration
	// TickTimeout bounds a single reconcile pass. A stalled hub or GitLab
	// call inside a tick can otherwise block forever — before this bound the
	// initial synchronous tick could wedge the whole goroutine so the ticker
	// never started (the 2026-07-03 "enabled but silent" incident). Default
	// 2min.
	TickTimeout time.Duration
}

const (
	defaultPollInterval = 5 * time.Minute
	defaultTickTimeout  = 2 * time.Minute
)

// activePlanPhases are the plan phases the reconciler scans, in deterministic
// query order. Draft plans are still being shaped; merged/deployed/done/
// abandoned are already terminal for take-up purposes.
var activePlanPhases = []string{
	"planned",
	"in_progress",
	"in_review",
	"merging",
}

// nextTowardMerged is the forward chain of the plan phase DAG the reconciler
// walks when every slice of a plan has merged. Each hop is validated again by
// the store, so a concurrent transition loses cleanly rather than corrupting.
var nextTowardMerged = map[string]string{
	"planned":     "in_progress",
	"in_progress": "in_review",
	"in_review":   "merging",
	"merging":     "merged",
}

// TickStats summarizes one reconcile pass for logs/tests.
type TickStats struct {
	PlansScanned      int
	SlicesMerged      int
	PlansMerged       int
	ItemsClosed       int
	OrphansFlagged    int
	PatternsHarvested int
	Errors            int
}

// Reconciler is the take-up loop.
type Reconciler struct {
	plans   PlanStore
	mrs     MRStater
	backlog BacklogStore
	cfg     Config
	logger  *slog.Logger
	// Enabled is the live global admission barrier. Nil preserves standalone
	// behavior; production wires policy.enabled.
	Enabled func() bool
	// Patterns, when set, closes the Pattern Loop (factory junction J2): a
	// plan the reconciler itself advances to merged whose id carries stamp
	// provenance (plan-stamp-<slug>-…) records a green instance against the
	// taste gate. Nil = harvest disabled (pre-J2 behavior).
	Patterns PatternHarvester
	active   atomic.Int64
}

// New wires a reconciler. Defaults are applied to zero config fields.
func New(plans PlanStore, mrs MRStater, backlog BacklogStore, cfg Config, logger *slog.Logger) *Reconciler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.TickTimeout <= 0 {
		cfg.TickTimeout = defaultTickTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{plans: plans, mrs: mrs, backlog: backlog, cfg: cfg, logger: logger}
}

// Run drives Tick on the configured PollInterval until ctx is done. Every
// pass — the immediate startup pass AND each periodic one — goes through
// runTick, which bounds it with a per-tick deadline and records the outcome
// unconditionally. The initial pass used to be a bare, unbounded r.Tick(ctx)
// call: a single hub/GitLab call stalling there wedged the goroutine before
// the ticker started, so the reconciler logged "started" then went silent
// forever (2026-07-03). Bounding it means a stall is a logged timeout and the
// loop keeps going.
func (r *Reconciler) Run(ctx context.Context) error {
	r.logger.Info("take-up reconciler started",
		"project", r.cfg.Project, "namespace", r.cfg.Namespace,
		"poll_interval", r.cfg.PollInterval, "tick_timeout", r.cfg.TickTimeout)
	r.runTick(ctx, "initial")
	t := time.NewTicker(r.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.runTick(ctx, "periodic")
		}
	}
}

// runTick executes one reconcile pass under a per-tick deadline and records
// its outcome to logs + metrics UNCONDITIONALLY. This is what makes "enabled
// but doing nothing" observable: an ok tick with plans_scanned=0 says the
// namespace gate matched no active plans, a timeout says a hub/GitLab call
// stalled, and a flat mills_takeup_ticks_total says the loop wedged. The
// bounded tickCtx is what a stalled dependency can no longer outlive.
func (r *Reconciler) runTick(ctx context.Context, kind string) TickStats {
	r.active.Add(1)
	defer r.active.Add(-1)
	if r.Enabled != nil && !r.Enabled() {
		return TickStats{}
	}
	tickCtx, cancel := context.WithTimeout(ctx, r.cfg.TickTimeout)
	defer cancel()

	start := time.Now()
	stats, err := r.Tick(tickCtx)
	elapsed := time.Since(start)

	mills.TakeupTickDurationSeconds.Observe(elapsed.Seconds())
	mills.TakeupPlansScannedTotal.Add(float64(stats.PlansScanned))
	mills.TakeupSlicesMergedTotal.Add(float64(stats.SlicesMerged))
	mills.TakeupPlansMergedTotal.Add(float64(stats.PlansMerged))
	mills.TakeupItemsClosedTotal.Add(float64(stats.ItemsClosed))
	mills.TakeupOrphansFlaggedTotal.Add(float64(stats.OrphansFlagged))

	// Classify off the contexts, not just the returned error: a deadline that
	// fires MID-tick leaves later per-slice calls to fail into stats.Errors
	// while Tick still returns nil, so tickCtx.Err() is the reliable signal.
	var outcome string
	switch {
	case tickCtx.Err() == context.DeadlineExceeded:
		outcome = "timeout"
		r.logger.Warn("take-up tick timed out",
			"kind", kind, "timeout", r.cfg.TickTimeout,
			"elapsed", elapsed.Round(time.Millisecond),
			"plans_scanned", stats.PlansScanned, "err", err)
	case ctx.Err() != nil:
		outcome = "cancelled"
		r.logger.Info("take-up tick cancelled (shutdown)",
			"kind", kind, "elapsed", elapsed.Round(time.Millisecond))
	case err != nil:
		outcome = "error"
		r.logger.Warn("take-up tick failed",
			"kind", kind, "elapsed", elapsed.Round(time.Millisecond), "err", err)
	default:
		outcome = "ok"
		r.logger.Info("take-up tick complete",
			"kind", kind, "elapsed", elapsed.Round(time.Millisecond),
			"plans_scanned", stats.PlansScanned, "slices_merged", stats.SlicesMerged,
			"plans_merged", stats.PlansMerged, "items_closed", stats.ItemsClosed,
			"orphans_flagged", stats.OrphansFlagged, "errors", stats.Errors)
	}
	mills.TakeupTicksTotal.WithLabelValues(outcome).Inc()
	return stats
}

// ActiveOperations reports take-up reconciliation passes currently executing.
func (r *Reconciler) ActiveOperations() int64 {
	if r == nil {
		return 0
	}
	return r.active.Load()
}

// Tick performs one reconcile pass. Safe to call from tests. The per-pass
// summary log + metrics live in runTick (the production driver) so a direct
// Tick call stays a pure state transition returning the stats; per-event Info
// logs (slice merged, orphan flagged, …) fire from the reconcile helpers.
func (r *Reconciler) Tick(ctx context.Context) (TickStats, error) {
	var stats TickStats
	// FAIL-CLOSED: never scan without an explicit namespace gate.
	if strings.TrimSpace(r.cfg.Namespace) == "" {
		return stats, nil
	}
	seenPlanIDs := make(map[string]struct{})
	plans := make([]clients.PlanSummary, 0)
	for _, phase := range activePlanPhases {
		phasePlans, err := r.plans.ListPlans(ctx, r.cfg.Project, r.cfg.Namespace, phase)
		if err != nil {
			return stats, fmt.Errorf("list active plans for phase %q: %w", phase, err)
		}
		for _, pl := range phasePlans {
			if _, seen := seenPlanIDs[pl.ID]; seen {
				continue
			}
			// Preserve the existing phase guard even though the query is scoped:
			// a malformed or stale response must not reconcile terminal plans.
			if !isActivePlanPhase(pl.Phase) {
				continue
			}
			seenPlanIDs[pl.ID] = struct{}{}
			plans = append(plans, pl)
		}
	}
	for _, pl := range plans {
		stats.PlansScanned++
		r.reconcilePlan(ctx, pl, &stats)
	}
	return stats, nil
}

func isActivePlanPhase(phase string) bool {
	phase = strings.ToLower(strings.TrimSpace(phase))
	for _, activePhase := range activePlanPhases {
		if phase == activePhase {
			return true
		}
	}
	return false
}

// reconcilePlan trues one plan's slices to MR reality and rolls the plan
// forward when everything has merged.
func (r *Reconciler) reconcilePlan(ctx context.Context, pl clients.PlanSummary, stats *TickStats) {
	slices, err := r.plans.ListSlices(ctx, pl.ID)
	if err != nil {
		r.logger.Warn("take-up list slices failed", "plan_id", pl.ID, "err", err)
		stats.Errors++
		return
	}
	if len(slices) == 0 {
		// Nothing to true; a plan with no slices is advanced by humans/agents.
		return
	}
	allMerged := true
	for _, sl := range slices {
		if strings.EqualFold(strings.TrimSpace(sl.Phase), "merged") {
			continue
		}
		if !r.reconcileSlice(ctx, sl, stats) {
			allMerged = false
		}
	}
	if allMerged {
		r.advancePlanToMerged(ctx, pl, slices, stats)
	}
}

// reconcileSlice trues one non-merged slice; returns true when the slice is
// now merged (so the caller can decide plan roll-up).
func (r *Reconciler) reconcileSlice(ctx context.Context, sl clients.PlanSliceSummary, stats *TickStats) bool {
	ref := strings.TrimSpace(sl.MRRef)
	if ref == "" {
		return false // no MR recorded; nothing to true against
	}
	parsed, ok := clients.ParseGitLabMRReference(ref)
	if !ok {
		r.logger.Warn("take-up unparseable mr_ref", "slice_id", sl.ID, "mr_ref", ref)
		return false
	}
	if parsed.ProjectBound && (!clients.SameGitLabProject(parsed.Project, r.cfg.Project) ||
		(parsed.Authority != "" && !clients.SameGitLabAuthority(parsed.Authority, r.cfg.GitLabBaseURL))) {
		r.logger.Warn("take-up mr_ref identity mismatch",
			"slice_id", sl.ID,
			"mr_ref", ref,
			"mr_project", parsed.Project,
			"mr_authority", parsed.Authority,
			"configured_project", r.cfg.Project)
		stats.Errors++
		return false
	}
	state, err := r.mrs.MRState(ctx, parsed.IID)
	if err != nil {
		r.logger.Warn("take-up MR state fetch failed", "slice_id", sl.ID, "mr_iid", parsed.IID, "err", err)
		stats.Errors++
		return false
	}
	switch state {
	case "merged":
		if err := r.plans.UpdateSlicePhase(ctx, sl.ID, "merged"); err != nil {
			r.logger.Warn("take-up slice phase update failed", "slice_id", sl.ID, "err", err)
			stats.Errors++
			return false
		}
		stats.SlicesMerged++
		r.logger.Info("take-up slice merged", "slice_id", sl.ID, "mr_iid", parsed.IID)
		r.closeBacklogItem(ctx, sl.ID, stats)
		return true
	case "closed":
		r.flagOrphan(ctx, sl, ref, stats)
		return false
	default: // opened, locked — still in flight
		return false
	}
}

// closeBacklogItem marks the slice's emitted Mills backlog item merged when
// the item is not actively being worked. A running item belongs to the
// pipeline (its own merge path updates state); queued/paused items would
// otherwise dispatch work that is already merged, and an escalated item whose
// MR merged externally is resolved by reality.
func (r *Reconciler) closeBacklogItem(ctx context.Context, sliceID string, stats *TickStats) {
	id := intake.BacklogIDForSlice(sliceID)
	item, err := r.backlog.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			r.logger.Warn("take-up backlog get failed", "id", id, "err", err)
			stats.Errors++
		}
		return
	}
	switch item.State {
	case store.BacklogQueued, store.BacklogPaused, store.BacklogEscalated:
		prev := item.State
		item.State = store.BacklogMerged
		if err := r.backlog.Put(ctx, item); err != nil {
			r.logger.Warn("take-up backlog close failed", "id", id, "err", err)
			stats.Errors++
			return
		}
		stats.ItemsClosed++
		r.logger.Info("take-up backlog item closed", "id", id, "from", prev)
	default:
		// running (pipeline owns it) or already terminal — leave untouched.
	}
}

// flagOrphan records a deduped decision note on a slice whose MR closed
// without merging. The LIST view omits the decisions array (TOON tabular), so
// dedupe reads the slice detail first; on detail failure it skips rather than
// risk appending the same note every tick.
func (r *Reconciler) flagOrphan(ctx context.Context, sl clients.PlanSliceSummary, ref string, stats *TickStats) {
	marker := fmt.Sprintf("take-up: MR %s closed without merging", ref)
	detail, err := r.plans.GetSlice(ctx, sl.ID)
	if err != nil {
		r.logger.Warn("take-up orphan detail fetch failed; skipping flag", "slice_id", sl.ID, "err", err)
		stats.Errors++
		return
	}
	for _, d := range detail.Decisions {
		if strings.HasPrefix(d, marker) {
			return // already flagged
		}
	}
	note := marker + " — slice needs a re-plan or a manual phase update"
	if err := r.plans.AppendSliceDecision(ctx, sl.ID, note); err != nil {
		r.logger.Warn("take-up orphan flag failed", "slice_id", sl.ID, "err", err)
		stats.Errors++
		return
	}
	stats.OrphansFlagged++
	r.logger.Info("take-up orphaned slice flagged", "slice_id", sl.ID, "mr_ref", ref)
}

// advancePlanToMerged walks the plan hop-by-hop along the phase DAG to
// "merged", recording each hop. Stops (and logs) on the first rejected hop —
// e.g. a concurrent transition — leaving the plan wherever the store says it
// legitimately is. On a completed walk the pattern harvest fires: this is the
// one-shot observation point (a merged plan leaves activePlanPhases and is
// never scanned again), which is what makes the green-instance record
// exactly-once per plan without any dedupe store. Best-effort across a crash:
// an operator dying between the final advance and the harvest loses that
// plan's record — acceptable for a taste signal.
func (r *Reconciler) advancePlanToMerged(ctx context.Context, pl clients.PlanSummary, slices []clients.PlanSliceSummary, stats *TickStats) {
	cur := strings.ToLower(strings.TrimSpace(pl.Phase))
	for cur != "merged" {
		next := nextTowardMerged[cur]
		if next == "" {
			return
		}
		note := fmt.Sprintf("take-up: all %s slices merged", pl.ID)
		if err := r.plans.AdvancePlan(ctx, pl.ID, next, note); err != nil {
			r.logger.Warn("take-up plan advance failed", "plan_id", pl.ID, "to_phase", next, "err", err)
			stats.Errors++
			return
		}
		cur = next
	}
	stats.PlansMerged++
	r.logger.Info("take-up plan merged", "plan_id", pl.ID)
	r.harvestPattern(ctx, pl, slices, stats)
}

// stampPlanIDPrefix marks a plan minted by the pattern stamp
// (svc_pattern_stamp.go: `plan-stamp-<pattern-slug>-<primary>`).
const stampPlanIDPrefix = "plan-stamp-"

// harvestPattern records a green instance against the taste gate for the
// pattern that stamped a just-merged plan (factory junction J2). Attribution
// mirrors the Factory shelf's rule (patternBooks.ts stampedPatternSlug):
// longest-slug prefix match, so `go-rest` can never swallow
// `go-rest-service`, and an unknown slug attributes to NOTHING rather than
// the wrong pattern. Wholly best-effort: no failure here may disturb the
// completed plan advance.
func (r *Reconciler) harvestPattern(ctx context.Context, pl clients.PlanSummary, slices []clients.PlanSliceSummary, stats *TickStats) {
	if r.Patterns == nil || !strings.HasPrefix(pl.ID, stampPlanIDPrefix) {
		return
	}
	patterns, err := r.Patterns.ListApprovedPatterns(ctx)
	if err != nil {
		r.logger.Warn("take-up pattern harvest: catalog fetch failed", "plan_id", pl.ID, "err", err)
		mills.TakeupPatternHarvestsTotal.WithLabelValues("error").Inc()
		stats.Errors++
		return
	}
	patternID := matchStampedPattern(pl.ID, patterns)
	if patternID == "" {
		// Not an error: the stamping pattern may have been deprecated or the
		// slug renamed since the stamp. Attribute to nothing, loudly.
		r.logger.Warn("take-up pattern harvest: no approved pattern matches stamp",
			"plan_id", pl.ID, "approved_patterns", len(patterns))
		mills.TakeupPatternHarvestsTotal.WithLabelValues("unmatched").Inc()
		return
	}
	harvest, err := r.Patterns.RecordInstance(ctx, patternID, mergedMRRefs(slices), store.RepoBase(r.cfg.Project))
	if err != nil {
		r.logger.Warn("take-up pattern harvest: record instance failed",
			"plan_id", pl.ID, "pattern_id", patternID, "err", err)
		mills.TakeupPatternHarvestsTotal.WithLabelValues("error").Inc()
		stats.Errors++
		return
	}
	stats.PatternsHarvested++
	mills.TakeupPatternHarvestsTotal.WithLabelValues("recorded").Inc()
	r.logger.Info("take-up pattern instance harvested",
		"plan_id", pl.ID, "pattern_id", patternID,
		"instances_shipped_green", harvest.InstancesShippedGreen,
		"status", harvest.Status, "promoted", harvest.Promoted)
}

// matchStampedPattern resolves the approved pattern whose slug the stamped
// plan id embeds, by longest-slug prefix match. Empty when nothing matches.
func matchStampedPattern(planID string, patterns []council.PatternRef) string {
	rest := strings.TrimPrefix(planID, stampPlanIDPrefix)
	bestID, bestLen := "", 0
	for _, p := range patterns {
		slug := strings.TrimSpace(p.Slug)
		if slug == "" {
			continue
		}
		if rest != slug && !strings.HasPrefix(rest, slug+"-") {
			continue
		}
		if len(slug) > bestLen {
			bestID, bestLen = p.ID, len(slug)
		}
	}
	return bestID
}

// mergedMRRefs joins the slices' recorded MR refs (capped at 3) for the
// pattern's provenance note. Empty when no slice recorded one.
func mergedMRRefs(slices []clients.PlanSliceSummary) string {
	var refs []string
	for _, sl := range slices {
		if ref := strings.TrimSpace(sl.MRRef); ref != "" {
			refs = append(refs, ref)
			if len(refs) == 3 {
				break
			}
		}
	}
	return strings.Join(refs, ", ")
}

// ParseMRIID extracts a merge-request IID from the free-form refs agents
// record: "!912", "912", "https://gitlab…/-/merge_requests/912",
// ".../merge_requests/912/diffs". Returns false when no IID is recoverable.
func ParseMRIID(ref string) (int64, bool) {
	parsed, ok := clients.ParseGitLabMRReference(ref)
	return parsed.IID, ok
}

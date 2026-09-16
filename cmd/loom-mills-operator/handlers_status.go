package main

import (
	"context"
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// handleStatusFull replaces the slice-1.2 stub with a fully populated
// snapshot. Fields the mills doesn't yet have data for (queue depth,
// active pipeline runs, last council) are sourced from the canonical
// store, so as soon as the reconciler starts producing rows the values
// become non-nil automatically — no further handler changes needed.
func (o *operator) handleStatusFull(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	policy := o.policy.Current()

	// queue_held_human is the subset of the queue the reconciler will never
	// start on its own: items whose effective policy (item flag or per-repo
	// override) requires human review. Surfaced beside queue_depth so a
	// queue_depth=1 that sits for hours reads as "waiting on a hand-off"
	// rather than as a dispatch outage.
	queueDepth, queueHeldHuman := 0, 0
	if items, err := o.store.Backlog.ListByState(ctx, "queued"); err == nil {
		queueDepth = len(items)
		queueHeldHuman = countHeldForHuman(policy, items, o.homeProject())
	}
	active, _ := o.store.Pipeline.CountActive(ctx)

	// One newest-first council read serves both last_council_at and the
	// council_yield block (how many finished runs since one produced a
	// backlog delta, and what they cost).
	var lastCouncil *time.Time
	var yield *councilYield
	if runs, err := o.store.Council.List(ctx, councilYieldSample); err == nil {
		if len(runs) > 0 {
			t := runs[0].StartedAt
			lastCouncil = &t
		}
		y := computeCouncilYield(runs)
		yield = &y
	}
	// last_merge_at is the all-time most-recent autonomous merge. The HUD
	// health banner cannot derive this from its active-only pipeline-run
	// list (terminal `done` runs are excluded), so the operator surfaces
	// it here. nil until the first merge ever lands.
	lastMerge, _ := o.store.Pipeline.LatestMergedAt(ctx)
	capabilities := o.capabilityReport(ctx)

	// Rolling-24h budget fuel for the HUD gauge. Informational — a
	// failed read omits the tier rather than failing the whole status
	// (the gauge renders an em dash; enforcement lives in Budget.Allow).
	budget := map[string]any{}
	if o.budget != nil {
		if u, err := o.budget.WindowUsage(ctx, mills.TierPipeline); err == nil {
			budget["pipeline"] = u
		}
		if u, err := o.budget.WindowUsage(ctx, mills.TierCouncil); err == nil {
			budget["council"] = u
		}
	}

	payload := map[string]any{
		"build_sha":            version,
		"budget":               budget,
		"db_ok":                o.dbOK(ctx),
		"policy_enabled":       policy.IsEnabled(),
		"policy_version":       policy.Version,
		"autonomy_ready":       capabilities.AutonomyReady,
		"autonomy_blockers":    capabilities.AutonomyBlockers,
		"capabilities":         capabilities.Capabilities,
		"queue_depth":          queueDepth,
		"queue_held_human":     queueHeldHuman,
		"active_pipeline_runs": active,
		"last_council_at":      lastCouncil,
		"last_merge_at":        lastMerge,
		// council_yield is nil only when the council table could not be read;
		// a healthy operator with zero runs reports an all-zero block.
		"council_yield": yield,
		// GitLab instance web base (mill-floor B1). Lets the HUD build a
		// clickable MR link from a run's MR iid + the item's TargetProject
		// without a per-run schema change. Empty string when no GitLab API URL
		// is configured — the HUD then renders an iid chip only.
		"gitlab_base_url": o.gitlabBaseURL,
		"slice":           "2.4-rest-surface",
	}

	// health_gates is the infrastructure admission verdict the HUD tile
	// renders. The key is present only when the gates are wired: the HUD
	// treats a missing key as fail-closed, which is the correct reading of
	// "this operator has no admission gates".
	if o.healthGates != nil {
		decision, err := o.healthGates.decide(ctx)
		if err != nil {
			decision = gates.HealthDecision{
				Allowed:    false,
				FailClosed: true,
				Status:     "block",
				CheckedAt:  time.Now().UTC(),
				Reasons:    []string{"health gates unavailable: " + err.Error()},
			}
		}
		payload["health_gates"] = gates.NewHealthGateReport(decision)
		payload["health_gates_mode"] = o.healthGates.Mode
	}

	writeJSON(w, http.StatusOK, payload)
}

// countHeldForHuman counts queued items the reconciler's tryStart would hold
// for a human hand-off, using the same effective-policy predicate the gate
// itself uses (mills.Policy.RequiresHumanReview) so the two never drift.
// homeProject is o.homeProject() — the per-repo override key an empty
// TargetProject resolves to.
func countHeldForHuman(policy *mills.Policy, items []*store.BacklogItem, homeProject string) int {
	held := 0
	for _, item := range items {
		if ok, _ := policy.RequiresHumanReview(item, homeProject); ok {
			held++
		}
	}
	return held
}

func (o *operator) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, o.capabilityReport(r.Context()))
}

// handlePolicy returns the current effective policy. Read-only — the
// policy is mutated by ConfigMap writes + the operator's fsnotify
// hot-reload, never via this endpoint.
func (o *operator) handlePolicy(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, o.policy.Current())
}

// handleKPIs returns the most recent KPI snapshot for the requested
// rolling window. The scheduler's KPIWriter (pkg/mills/kpi_writer.go)
// records a snapshot per window after every successful reconciler tick,
// so this returns data once the operator has ticked at least once; the
// 404 is reserved for the brief pre-first-tick window (the HUD renders a
// placeholder card rather than an error in that case).
func (o *operator) handleKPIs(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	seconds := windowSeconds(window)
	if seconds == 0 {
		http.Error(w, "window must be one of 1d, 7d, 30d", http.StatusBadRequest)
		return
	}
	snap, err := o.store.KPI.Latest(r.Context(), seconds)
	if err != nil {
		// ErrNotFound surfaces as 404 with a clear "no snapshot yet" body
		// so HUD can render a placeholder card rather than an error toast.
		http.Error(w, "no kpi snapshot for window "+window, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// windowSeconds maps the user-friendly window names to seconds. Keep the
// set tight; arbitrary windows would let a caller cardinality-bomb the
// kpi_snapshots table.
func windowSeconds(window string) int {
	switch window {
	case "1d", "":
		return 86400
	case "7d":
		return 7 * 86400
	case "30d":
		return 30 * 86400
	}
	return 0
}

func (o *operator) dbOK(ctx context.Context) bool {
	if o.store == nil {
		return false
	}
	if db := o.store.DB(); db != nil {
		return db.PingContext(ctx) == nil
	}
	return false
}

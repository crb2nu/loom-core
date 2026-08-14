// plan_reconciler.go -- plan-store truth sweep: a periodic reconciler that
// advances plan/slice phases once their merge requests land.
//
// The drift this kills is human, not technical. An implementer merges a slice's
// MR and never comes back to move the slice — and its plan — off in_progress,
// so the plan store reads weeks stale and every consumer (HUD board, Mills
// emitter, a fresh agent recalling the plan) inherits the lie. The sweep
// re-derives phase from the one externally observable fact — whether the
// slice's MR is merged — and only ever moves forward.
//
// MERGED-SIGNAL CONTRACT: the sweep advances only on an explicit merged marker
// in the HUD mr-status answer (a merge_requests[] entry whose state is "merged"
// or whose merged flag is true). Absence is never merged: the mrwatch registry
// tracks OPEN MRs, so "no record for this branch" is indistinguishable from "MR
// never existed" and from a HUD outage. Every uncertain answer fails open — the
// slice is left exactly as it was.
package agentcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/projectmeta"
)

// PlanReconcilerConfig configures the plan-store truth sweep.
type PlanReconcilerConfig struct {
	Enabled       bool          `json:"enabled"`
	CheckInterval time.Duration `json:"check_interval"`

	// PassTimeout bounds one sweep, so a slow HUD can never wedge the loop or
	// overlap the next tick.
	PassTimeout time.Duration `json:"pass_timeout"`

	// MaxPlansPerPhase caps the plan scroll issued per swept phase.
	MaxPlansPerPhase int `json:"max_plans_per_phase"`
}

// DefaultPlanReconcilerConfig returns sensible defaults.
func DefaultPlanReconcilerConfig() PlanReconcilerConfig {
	return PlanReconcilerConfig{
		Enabled:          true,
		CheckInterval:    15 * time.Minute,
		PassTimeout:      2 * time.Minute,
		MaxPlansPerPhase: 200,
	}
}

// planSweepPhases are the plan phases whose slices may still be waiting on a
// merge. draft plans have no MRs yet; merged/deployed/done/abandoned are at or
// past the sweep's target.
var planSweepPhases = []string{
	PlanPhasePlanned, PlanPhaseInProgress, PlanPhaseInReview, PlanPhaseMerging,
}

const (
	// planSweepActor stamps phase_history so an operator can tell an automated
	// advance from a human one.
	planSweepActor = "plan-truth-sweep"

	// planSweepPlanNote is the plan-level phase-history note.
	planSweepPlanNote = "auto-advanced: all slice MRs merged [plan-truth-sweep]"

	// mrStateMerged is the merged marker the sweep accepts from the HUD.
	mrStateMerged = "merged"
)

// PlanReconcileStats contains statistics from a single sweep.
type PlanReconcileStats struct {
	StartTime      time.Time     `json:"start_time"`
	Duration       time.Duration `json:"duration"`
	PlansScanned   int           `json:"plans_scanned"`
	SlicesAdvanced int           `json:"slices_advanced"`
	PlansAdvanced  int           `json:"plans_advanced"`
	Errors         int           `json:"errors"`
}

// PlanReconciler periodically reconciles plan/slice phases against merged MRs.
type PlanReconciler struct {
	mu sync.RWMutex

	config     PlanReconcilerConfig
	ps         *PlanSvc
	hudBaseURL string
	client     *http.Client
	metrics    *Metrics
	logger     *slog.Logger

	running   bool
	stopCh    chan struct{}
	lastRun   time.Time
	runCount  int64
	lastStats PlanReconcileStats
}

// NewPlanReconciler creates a plan-store truth sweep. hudBaseURL is the same
// cfg.HUDBaseURL the agent_mr_status tool resolves (AGENT_CONTEXT_HUD_URL →
// LOOM_HUD_URL → default); empty disables the sweep.
func NewPlanReconciler(config PlanReconcilerConfig, ps *PlanSvc, hudBaseURL string, metrics *Metrics, logger *slog.Logger) *PlanReconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &PlanReconciler{
		config:     config,
		ps:         ps,
		hudBaseURL: strings.TrimRight(strings.TrimSpace(hudBaseURL), "/"),
		client:     &http.Client{Timeout: mrStatusHTTPTimeout},
		metrics:    metrics,
		logger:     logger,
		stopCh:     make(chan struct{}),
	}
}

// Enabled reports whether the sweep can run. Without a HUD base URL there is no
// merged signal to reconcile against, so the sweep stays off rather than
// treating every slice as unmerged.
func (r *PlanReconciler) Enabled() bool {
	return r != nil && r.config.Enabled && r.ps != nil && r.hudBaseURL != ""
}

// Start begins the sweep loop.
func (r *PlanReconciler) Start(ctx context.Context) {
	if r == nil {
		return
	}
	if !r.Enabled() {
		r.logger.Info("plan truth sweep not started",
			"enabled", r.config.Enabled, "hud_configured", r.hudBaseURL != "")
		return
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.stopCh = make(chan struct{})
	r.mu.Unlock()

	go r.runLoop(ctx)
}

// Stop stops the sweep loop.
func (r *PlanReconciler) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	close(r.stopCh)
	r.running = false
}

// TriggerReconcile runs one sweep on demand. Returns nil stats when disabled.
func (r *PlanReconciler) TriggerReconcile(ctx context.Context) (*PlanReconcileStats, error) {
	return r.reconcile(ctx)
}

// LastStats returns stats from the most recent sweep.
func (r *PlanReconciler) LastStats() PlanReconcileStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastStats
}

func (r *PlanReconciler) runLoop(ctx context.Context) {
	ticker := time.NewTicker(r.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			stats, err := r.reconcile(ctx)
			if err != nil {
				r.logger.Warn("plan truth sweep failed", "error", err)
			} else if stats != nil && (stats.SlicesAdvanced+stats.PlansAdvanced) > 0 {
				r.logger.Info("plan truth sweep advanced phases",
					"plans_scanned", stats.PlansScanned,
					"slices_advanced", stats.SlicesAdvanced,
					"plans_advanced", stats.PlansAdvanced,
					"errors", stats.Errors,
					"duration", stats.Duration,
				)
			}
		}
	}
}

// reconcile runs one bounded, best-effort sweep. A failure on one plan is
// counted and skipped; it never aborts the pass.
func (r *PlanReconciler) reconcile(ctx context.Context) (*PlanReconcileStats, error) {
	if !r.Enabled() {
		return nil, nil
	}

	start := time.Now()
	stats := PlanReconcileStats{StartTime: start}

	passCtx, cancel := context.WithTimeout(ctx, r.config.PassTimeout)
	defer cancel()

	seen := make(map[string]struct{})
	for _, phase := range planSweepPhases {
		plans, err := r.ps.list(passCtx, "", "", phase, r.config.MaxPlansPerPhase)
		if err != nil {
			r.logger.Warn("plan truth sweep: list failed", "phase", phase, "error", err)
			r.countError(&stats)
			continue
		}
		for _, plan := range plans {
			if plan == nil {
				continue
			}
			if _, dup := seen[plan.ID]; dup {
				continue
			}
			seen[plan.ID] = struct{}{}
			stats.PlansScanned++
			r.sweepPlan(passCtx, plan, &stats)
		}
	}

	stats.Duration = time.Since(start)

	r.mu.Lock()
	r.lastRun = start
	r.runCount++
	r.lastStats = stats
	r.mu.Unlock()

	return &stats, nil
}

// sweepPlan advances every merged slice of one plan, then the plan itself when
// all of its slices are merged.
func (r *PlanReconciler) sweepPlan(ctx context.Context, plan *Plan, stats *PlanReconcileStats) {
	slices := r.ps.slicesForPlan(ctx, plan.ID)
	if len(slices) == 0 {
		return
	}

	// The HUD keys MRs by GitLab path_with_namespace. A bare project name
	// ("loom-core") would filter every MR out, so query on branch alone.
	repo := plan.Project
	if projectmeta.LooksLikeBareRepo(repo) {
		repo = ""
	}

	allMerged := true
	for i := range slices {
		s := &slices[i]
		if s.Phase == SlicePhaseMerged {
			continue
		}
		branch := sliceMergeBranch(s)
		if branch == "" {
			// No branch/MR provenance — nothing to check against.
			allMerged = false
			continue
		}
		mr, known := r.branchMerged(ctx, repo, branch)
		if !known || !mr.Merged {
			allMerged = false
			continue
		}
		advanced, err := r.advanceSlice(ctx, s.ID, mergedNoteRef(s, mr, branch))
		if err != nil {
			r.logger.Warn("plan truth sweep: slice advance failed",
				"plan_id", plan.ID, "slice_id", s.ID, "error", err)
			r.countError(stats)
			allMerged = false
			continue
		}
		if advanced {
			stats.SlicesAdvanced++
			if r.metrics != nil {
				r.metrics.PlanSweepSlicesAdvanced.Add(1)
			}
		}
	}

	if !allMerged {
		return
	}
	advanced, err := r.advancePlan(ctx, plan.ID)
	if err != nil {
		r.logger.Warn("plan truth sweep: plan advance failed", "plan_id", plan.ID, "error", err)
		r.countError(stats)
		return
	}
	if advanced {
		stats.PlansAdvanced++
		if r.metrics != nil {
			r.metrics.PlanSweepPlansAdvanced.Add(1)
		}
	}
}

// advanceSlice re-reads the slice and moves it to merged with a decision note.
// The re-read matters: slice writes are last-writer-wins full-record upserts,
// so persisting the copy this pass scrolled would clobber whatever a concurrent
// implementer wrote in between.
func (r *PlanReconciler) advanceSlice(ctx context.Context, sliceID, ref string) (bool, error) {
	fresh, err := r.ps.fetchSlice(ctx, sliceID)
	if err != nil {
		return false, err
	}
	if fresh == nil || fresh.Phase == SlicePhaseMerged {
		return false, nil
	}
	fresh.Phase = SlicePhaseMerged
	fresh.Decisions = append(fresh.Decisions,
		fmt.Sprintf("auto-advanced: MR merged (%s) [plan-truth-sweep]", ref))
	fresh.UpdatedAt = time.Now().UTC()
	if err := r.ps.persistSlice(ctx, fresh); err != nil {
		return false, err
	}
	return true, nil
}

// advancePlan walks a plan forward to merged one legal hop at a time (the DAG
// forbids in_review→merged directly). Returns false when the plan is already at
// or past merged.
func (r *PlanReconciler) advancePlan(ctx context.Context, planID string) (bool, error) {
	fresh, err := r.ps.fetch(ctx, planID)
	if err != nil {
		return false, err
	}
	if fresh == nil {
		return false, nil
	}
	path := planPhasePathTo(fresh.Phase, PlanPhaseMerged)
	if len(path) == 0 {
		return false, nil
	}
	for _, to := range path {
		if err := r.ps.advancePhase(ctx, fresh, to, planSweepActor, planSweepPlanNote); err != nil {
			return false, err
		}
	}
	return true, nil
}

// countError records one best-effort failure on both the run stats and the
// process counter.
func (r *PlanReconciler) countError(stats *PlanReconcileStats) {
	stats.Errors++
	if r.metrics != nil {
		r.metrics.PlanSweepErrors.Add(1)
	}
}

// ---- HUD mr-status lookup --------------------------------------------------

// mergedMR is the sweep's read of the HUD answer for one branch.
type mergedMR struct {
	Merged bool
	// Ref identifies the merged MR ("<repo>!<iid>") for the decision note.
	Ref string
}

// mrStatusBranchResponse is the subset of the HUD BranchStatusResponse the
// sweep reads. `merged` is not in today's wire contract — the registry classes
// only open MRs — so it is decoded as an optional flag alongside state.
type mrStatusBranchResponse struct {
	Stale         bool `json:"stale"`
	MergeRequests []struct {
		Repo   string `json:"repo"`
		IID    int64  `json:"iid"`
		State  string `json:"state"`
		Merged *bool  `json:"merged"`
	} `json:"merge_requests"`
}

// branchMerged asks the HUD mr-status registry whether branch has a merged MR.
// The second return is false when the answer cannot be trusted — transport
// failure, non-2xx, malformed body, or a stale snapshot — and the caller must
// then leave the slice alone.
func (r *PlanReconciler) branchMerged(ctx context.Context, repo, branch string) (mergedMR, bool) {
	endpoint := r.hudBaseURL + "/api/agent/mr-status?branch=" + url.QueryEscape(branch)
	if repo != "" {
		endpoint += "&repo=" + url.QueryEscape(repo)
	}

	reqCtx, cancel := context.WithTimeout(ctx, mrStatusHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return mergedMR{}, false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.logger.Debug("plan truth sweep: mr-status unreachable", "branch", branch, "error", err)
		return mergedMR{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.logger.Debug("plan truth sweep: mr-status non-2xx", "branch", branch, "status", resp.StatusCode)
		return mergedMR{}, false
	}

	var body mrStatusBranchResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		r.logger.Debug("plan truth sweep: malformed mr-status body", "branch", branch, "error", err)
		return mergedMR{}, false
	}
	if body.Stale {
		return mergedMR{}, false
	}

	for _, mr := range body.MergeRequests {
		if !strings.EqualFold(mr.State, mrStateMerged) && (mr.Merged == nil || !*mr.Merged) {
			continue
		}
		return mergedMR{Merged: true, Ref: formatMRRef(mr.Repo, mr.IID)}, true
	}
	return mergedMR{}, true
}

// sliceMergeBranch returns the branch a slice's MR can be looked up by. The
// mr-status endpoint is keyed by branch, so an mr_ref only helps when it IS a
// branch path: iid forms ("!1234", "services/loom-core!1234") and MR URLs carry
// no branch. Empty means no provenance — the slice is skipped.
func sliceMergeBranch(s *PlanSlice) string {
	if b := strings.TrimSpace(s.BranchName); b != "" {
		return b
	}
	ref := strings.TrimSpace(s.MRRef)
	if ref == "" || strings.ContainsAny(ref, "!#") || strings.Contains(ref, "://") {
		return ""
	}
	if !strings.Contains(ref, "/") {
		return ""
	}
	return ref
}

// mergedNoteRef picks the most specific identifier for the decision note: the
// slice's own recorded mr_ref, then the MR the HUD reported, then the branch.
func mergedNoteRef(s *PlanSlice, mr mergedMR, branch string) string {
	if ref := strings.TrimSpace(s.MRRef); ref != "" {
		return ref
	}
	if mr.Ref != "" {
		return mr.Ref
	}
	return branch
}

// formatMRRef renders a "<repo>!<iid>" reference, degrading when either half is
// missing from the HUD record.
func formatMRRef(repo string, iid int64) string {
	switch {
	case iid <= 0:
		return repo
	case repo == "":
		return fmt.Sprintf("!%d", iid)
	default:
		return fmt.Sprintf("%s!%d", repo, iid)
	}
}

// ---- phase pathing ---------------------------------------------------------

// planPhaseRank orders the forward lifecycle so the sweep can prove structurally
// that it never moves a plan backwards. abandoned and unknown phases rank -1 and
// are never entered or left by the sweep.
func planPhaseRank(phase string) int {
	switch phase {
	case PlanPhaseDraft:
		return 0
	case PlanPhasePlanned:
		return 1
	case PlanPhaseInProgress:
		return 2
	case PlanPhaseInReview:
		return 3
	case PlanPhaseMerging:
		return 4
	case PlanPhaseMerged:
		return 5
	case PlanPhaseDeployed:
		return 6
	case PlanPhaseDone:
		return 7
	default:
		return -1
	}
}

// planPhasePathTo returns the shortest chain of legal hops from → to, excluding
// from and including to. Every hop strictly increases the phase rank, so a
// returned path can never regress a plan. nil when to is not forward-reachable
// (already at or past it, abandoned, or an unknown phase).
func planPhasePathTo(from, to string) []string {
	fromRank, toRank := planPhaseRank(from), planPhaseRank(to)
	if fromRank < 0 || toRank < 0 || fromRank >= toRank {
		return nil
	}

	prev := map[string]string{from: ""}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == to {
			break
		}
		for _, next := range planPhaseTransitions[cur] {
			if planPhaseRank(next) <= planPhaseRank(cur) {
				continue
			}
			if _, visited := prev[next]; visited {
				continue
			}
			prev[next] = cur
			queue = append(queue, next)
		}
	}
	if _, reached := prev[to]; !reached {
		return nil
	}

	var path []string
	for cur := to; cur != from; cur = prev[cur] {
		path = append(path, cur)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

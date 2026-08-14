package pipeline

import (
	"context"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// PlanSliceHydrator resolves a plan's file-bearing slice scope so the runner
// can stamp it onto a slice-less backlog item once the plan_slice stage has
// authored (or refreshed) the decomposition in the plan store. Implemented by
// *clients.PlanClient (SliceScopeForPlan); nil disables hydration and the
// scope gate's slice-less advisory skip applies downstream.
type PlanSliceHydrator interface {
	SliceScopeForPlan(ctx context.Context, planID string) ([]store.Slice, []string, error)
}

// hydrateSliceScope stamps file-bearing slices from the plan store onto a
// backlog item that reached the pipeline without an enforceable scope. It runs
// right after the plan_slice stage completes: that stage is the pipeline's
// designated decomposition step, but its output previously lived only in chat
// text, so a GitLab-issue-sourced item (slice-less by construction) or a
// plan-slice-emitter item whose council slice declared no files carried
// Slices=[] all the way to post_implement_gate, where the scope gate had
// nothing to enforce (escalations #332/#338 — both ran a SUCCESSFUL plan_slice
// stage and still hit "backlog item has no slices; no scope to enforce").
//
// Best-effort by design: a nil hydrator, an unlinked item, a fetch error, or a
// plan with no file-bearing slices leaves the item as-is and the scope gate
// records an advisory skip downstream. The in-memory item is stamped first so
// this run's gates see the scope even when the store write loses a CAS race;
// the persisted copy is what a resumed Drive and the HUD read.
func (r *Runner) hydrateSliceScope(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) {
	if r.SliceHydrator == nil || item == nil || strings.TrimSpace(item.PlanID) == "" {
		return
	}
	for _, s := range item.Slices {
		if len(s.Files) > 0 {
			return // enforceable scope already materialized at intake
		}
	}
	slices, files, err := r.SliceHydrator.SliceScopeForPlan(ctx, item.PlanID)
	if err != nil {
		r.logger().Warn("pipeline plan-slice scope hydration failed; scope gate will skip",
			"run", run.ID, "item", item.ID, "plan_id", item.PlanID, "error", err)
		return
	}
	if len(slices) == 0 {
		// The plan carries no file-bearing slices (e.g. a docs-only council
		// slice that declared no paths). Nothing to enforce; the scope gate
		// skips with its slice-less reason.
		r.logger().Info("pipeline plan-slice scope hydration found no file-bearing slices",
			"run", run.ID, "item", item.ID, "plan_id", item.PlanID)
		return
	}
	item.Slices = slices
	// Pre-declare protected paths the hydrated files touch so path_policy
	// treats the plan-declared touch as intended — mirrors the backlog-intake
	// handler and the plan-slice emitter. An UNDECLARED protected touch the
	// implement stage introduces still fails the gate.
	if len(item.Policy.ProtectedPathsTouched) == 0 {
		if hit := r.policy().ProtectedPathsHit(files); len(hit) > 0 {
			item.Policy.ProtectedPathsTouched = append([]string(nil), hit...)
		}
	}
	// Persist against a fresh row: the Drive-held copy's revision may be
	// stale (HUD edits, reconciler touches), and Put is CAS on row_version.
	fresh, gerr := r.Store.Backlog.Get(ctx, item.ID)
	if gerr != nil || fresh == nil {
		r.logger().Warn("pipeline plan-slice scope hydration persist skipped; item reload failed",
			"run", run.ID, "item", item.ID, "error", gerr)
	} else {
		fresh.Slices = item.Slices
		fresh.Policy.ProtectedPathsTouched = item.Policy.ProtectedPathsTouched
		if perr := r.Store.Backlog.Put(ctx, fresh); perr != nil {
			r.logger().Warn("pipeline plan-slice scope hydration persist failed; scope enforced in-memory only",
				"run", run.ID, "item", item.ID, "error", perr)
		} else {
			item.Revision = fresh.Revision
			item.ClaimVersion = fresh.ClaimVersion
		}
	}
	fileCount := 0
	for _, s := range slices {
		fileCount += len(s.Files)
	}
	r.event(ctx, "pipeline.plan_slice.scope_hydrated", "ok", map[string]any{
		"run": run.ID, "item": item.ID, "plan_id": item.PlanID,
		"slices": len(slices), "files": fileCount,
	})
	r.logger().Info("pipeline hydrated plan-slice scope onto slice-less item",
		"run", run.ID, "item", item.ID, "plan_id", item.PlanID,
		"slices", len(slices), "files", fileCount)
}

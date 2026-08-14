package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PlanMRRecorder records a created merge request onto the plan slice(s) a
// plan-linked backlog item came from, so the take-up reconciler has an mr_ref
// to poll. Implemented by *takeup.MRRefRecorder (which owns the attribution
// rules); nil disables the write entirely, restoring pre-fix behavior where
// the plan only learned about the MR if the spawned agent remembered to call
// agent_plan_slice_update itself.
//
// It returns the slice id it recorded against, or "" when the plan offered no
// store point to record (a slice-less plan) — that case is benign and already
// logged by the implementation. Errors are ADVISORY: runMR logs them and
// reports the MR as created regardless.
type PlanMRRecorder interface {
	RecordMRRef(ctx context.Context, planID, backlogID, mrRef string, files []string) (string, error)
}

// planMRRecordTimeout bounds the plan-store round trip. The MR already exists
// by the time this runs, so a wedged MCP hub must cost the mr stage seconds,
// not the stage.
const planMRRecordTimeout = 30 * time.Second

// recordPlanMRRef writes the freshly created MR onto the item's plan slice.
//
// Best-effort by construction: an unwired recorder, an item with no PlanID, a
// plan with no slices, or a failed write all leave the stage successful. The
// take-up reconciler is the safety net for a lost write only in the sense that
// it will keep finding the slice un-trued — which is exactly the state this
// call exists to prevent, so failures are logged loudly enough to notice.
func (w *GitLabWorker) recordPlanMRRef(ctx context.Context, jc JobContext, mrIID int64) {
	if w.PlanMRRecorder == nil || jc.Item == nil || mrIID == 0 {
		return
	}
	planID := strings.TrimSpace(jc.Item.PlanID)
	if planID == "" {
		return // not plan-linked; nothing to true
	}
	ref := fmt.Sprintf("!%d", mrIID)
	ctx, cancel := context.WithTimeout(ctx, planMRRecordTimeout)
	defer cancel()
	sliceID, err := w.PlanMRRecorder.RecordMRRef(ctx, planID, jc.Item.ID, ref, priorFilesChanged(jc.Prior))
	switch {
	case err != nil:
		w.logger().Warn("mr: recording mr_ref on plan slice failed; take-up cannot true this slice",
			"item", jc.Item.ID, "plan_id", planID, "mr_ref", ref, "err", err)
	case sliceID != "":
		w.logger().Info("mr: recorded mr_ref on plan slice",
			"item", jc.Item.ID, "plan_id", planID, "slice_id", sliceID, "mr_ref", ref)
	}
}

// priorFilesChanged resolves the paths this run actually touched, for
// attributing the MR to one slice of a multi-slice plan. The implement stage
// is the authoritative source; when it recorded nothing (a docs-only or
// resumed run), fall back to the union of every prior stage's capture in
// deterministic stage order.
func priorFilesChanged(prior map[string]StageOutput) []string {
	if impl, ok := prior["implement"]; ok && len(impl.FilesChanged) > 0 {
		return impl.FilesChanged
	}
	stages := make([]string, 0, len(prior))
	for id := range prior {
		stages = append(stages, id)
	}
	sort.Strings(stages)
	var out []string
	seen := make(map[string]struct{})
	for _, id := range stages {
		for _, f := range prior[id].FilesChanged {
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

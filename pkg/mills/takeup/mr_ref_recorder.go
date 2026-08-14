package takeup

// mr_ref_recorder.go -- the take-up motion's WRITE-SIDE half, at MR creation
// time.
//
// The reconciler in this package trues plan state to MR reality by polling
// each slice's mr_ref. That only works if something records the ref. Nothing
// did: the mr stage created the GitLab MR and moved on, so for a plan-linked
// item (a psl-* emitter item, a stamped pattern plan) the slice's mr_ref
// stayed empty unless the spawned agent remembered to call
// agent_plan_slice_update itself — and agents forget. With mr_ref empty the
// reconciler has nothing to poll, the slice never advances to merged, the
// plan never walks to merged, and the J2 pattern harvest (which fires exactly
// once on that final plan hop, docs/FACTORY_MODEL.md) never fires at all.
//
// Observed live 2026-08-01: plan-stamp-loom-runbook-loom-runbook slice #1 had
// mr_ref=None after MR !1380 merged; the loop needed a manual
// agent_plan_slice_update to unstick.
//
// The recorder closes that gap from the mr stage. It is BEST-EFFORT by
// contract: every failure path returns an error for the caller to log and
// never fails the stage — the MR exists either way, and a missing ref is a
// stale-plan problem, not a broken merge.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/intake"
)

// PlanSliceMRStore is the Plan Store surface the recorder needs, satisfied by
// *clients.PlanClient. Narrow so tests fake it without the MCP hub.
type PlanSliceMRStore interface {
	ListSlices(ctx context.Context, planID string) ([]clients.PlanSliceSummary, error)
	GetSlice(ctx context.Context, sliceID string) (clients.PlanSliceSummary, error)
	UpdateSliceMRRef(ctx context.Context, sliceID, mrRef string) error
	AppendSliceDecision(ctx context.Context, sliceID, note string) error
}

// ErrSliceAmbiguous reports that the MR could not be attributed to a single
// slice. The recorder leaves a decision note on each candidate instead of
// guessing: a wrong mr_ref makes the reconciler true the WRONG slice to
// merged, which is worse than leaving it empty.
var ErrSliceAmbiguous = errors.New("take-up: mr ref not attributable to a single slice")

// ambiguousNoteMaxSlices bounds the ambiguous-attribution fan-out so a
// pathological plan can't turn one MR into dozens of decision writes.
const ambiguousNoteMaxSlices = 8

// MRRefRecorder records a freshly created merge request onto the plan slice
// an emitted backlog item came from.
type MRRefRecorder struct {
	plans  PlanSliceMRStore
	logger *slog.Logger
}

// NewMRRefRecorder returns a recorder bound to plans. A nil logger defaults to
// slog.Default().
func NewMRRefRecorder(plans PlanSliceMRStore, logger *slog.Logger) *MRRefRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &MRRefRecorder{plans: plans, logger: logger}
}

// RecordMRRef writes mrRef onto the plan slice backlogID came from and returns
// the slice id it wrote to. Attribution, in order:
//
//  1. The emitted-item linkage: a psl-* item's id is derived from its slice id
//     (intake.BacklogIDForSlice), so the match is exact and needs no guessing.
//     This is the dominant case and it wins even over a slice that already
//     carries a different ref — a retried run's newer MR is the live one.
//  2. A single-slice plan: there is exactly one place the MR can belong.
//  3. Elimination: exactly one slice has no ref recorded yet.
//  4. File overlap: the slice whose declared files best match the run's
//     captured paths, uniquely.
//
// When none of those resolve it returns ErrSliceAmbiguous after leaving a
// decision note on each candidate. A plan with no slices returns ("", nil):
// there is nothing to record and that is not a fault. Re-running against a
// slice that already carries this exact MR is a no-op (the mr stage retries,
// and the operator adopts existing MRs).
func (r *MRRefRecorder) RecordMRRef(ctx context.Context, planID, backlogID, mrRef string, files []string) (string, error) {
	if r == nil || r.plans == nil {
		return "", errors.New("take-up: mr-ref recorder not configured")
	}
	planID = strings.TrimSpace(planID)
	mrRef = strings.TrimSpace(mrRef)
	if planID == "" {
		return "", errors.New("take-up: plan_id required")
	}
	if mrRef == "" {
		return "", errors.New("take-up: mr_ref required")
	}
	slices, err := r.plans.ListSlices(ctx, planID)
	if err != nil {
		return "", fmt.Errorf("take-up: list slices for plan %s: %w", planID, err)
	}
	if len(slices) == 0 {
		// A plan authored from a slice-less backlog item (the plan backfiller
		// mints these) offers no store point to record against. Benign, and
		// common enough that it must not read as a fault to the caller.
		r.logger.Info("take-up plan carries no slices; mr_ref not recorded",
			"plan_id", planID, "mr_ref", mrRef, "backlog_id", backlogID)
		return "", nil
	}

	// (1) exact emitted-item linkage.
	if sl, ok := sliceForBacklogID(slices, backlogID); ok {
		if sameMRRef(sl.MRRef, mrRef) {
			return sl.ID, nil
		}
		return r.write(ctx, sl.ID, mrRef, "backlog_id")
	}

	// Already recorded somewhere on this plan: a re-dispatched mr stage
	// re-adopting the same MR must not append a second attribution.
	for _, sl := range slices {
		if sameMRRef(sl.MRRef, mrRef) {
			return sl.ID, nil
		}
	}

	// (2) A plan with exactly one slice has exactly one place the MR can
	// belong, so a non-emitter item (a backfilled plan-mills-<id> plan, a
	// stamped pattern) still resolves — and a stale ref from an earlier
	// attempt is replaced by the live MR.
	if len(slices) == 1 && recordable(slices[0]) {
		return r.write(ctx, slices[0].ID, mrRef, "single_slice_plan")
	}

	candidates := unrecordedSlices(slices)
	switch len(candidates) {
	case 0:
		// Every slice already points at some other MR. Recording here would
		// overwrite a live ref with an unattributable one.
		return "", fmt.Errorf("%w: all %d slice(s) already carry an mr_ref", ErrSliceAmbiguous, len(slices))
	case 1:
		return r.write(ctx, candidates[0].ID, mrRef, "single_candidate")
	}

	// (4) file overlap. The LIST view's TOON tabular encoding drops array
	// columns, so declared files come back empty and must be re-fetched from
	// the slice detail — the same hydration the emitter and the scope
	// hydrator do.
	candidates = r.hydrate(ctx, candidates)
	if sl, ok := sliceByFileOverlap(candidates, files); ok {
		return r.write(ctx, sl.ID, mrRef, "file_overlap")
	}
	return "", r.noteAmbiguous(ctx, candidates, backlogID, mrRef)
}

// write records the ref and logs the attribution reason so a wrong mapping is
// diagnosable from operator logs alone.
func (r *MRRefRecorder) write(ctx context.Context, sliceID, mrRef, reason string) (string, error) {
	if err := r.plans.UpdateSliceMRRef(ctx, sliceID, mrRef); err != nil {
		return "", fmt.Errorf("take-up: record mr_ref %s on slice %s: %w", mrRef, sliceID, err)
	}
	r.logger.Info("take-up recorded mr_ref on plan slice",
		"slice_id", sliceID, "mr_ref", mrRef, "attribution", reason)
	return sliceID, nil
}

// hydrate re-fetches slice details so Files and Decisions are populated. A
// detail fetch that fails leaves that slice as the list projection returned
// it — it simply scores zero and can't be deduped, which is the safe side.
func (r *MRRefRecorder) hydrate(ctx context.Context, slices []clients.PlanSliceSummary) []clients.PlanSliceSummary {
	out := make([]clients.PlanSliceSummary, 0, len(slices))
	for _, sl := range slices {
		if len(sl.Files) > 0 && len(sl.Decisions) > 0 {
			out = append(out, sl)
			continue
		}
		detail, err := r.plans.GetSlice(ctx, sl.ID)
		if err != nil {
			r.logger.Warn("take-up slice detail fetch failed during mr-ref attribution",
				"slice_id", sl.ID, "err", err)
			out = append(out, sl)
			continue
		}
		if len(sl.Files) > 0 {
			detail.Files = sl.Files
		}
		if strings.TrimSpace(detail.ID) == "" {
			detail.ID = sl.ID
		}
		out = append(out, detail)
	}
	return out
}

// noteAmbiguous leaves the linkage on the record where a human or a planner
// agent will see it. Every candidate gets the note (whichever slice someone
// opens tells the truth), deduped against prior decisions so a retried mr
// stage doesn't re-append.
func (r *MRRefRecorder) noteAmbiguous(ctx context.Context, candidates []clients.PlanSliceSummary, backlogID, mrRef string) error {
	marker := fmt.Sprintf("take-up: MR %s opened for backlog item %s", mrRef, strings.TrimSpace(backlogID))
	note := marker + " — could not be attributed to a single slice; set mr_ref manually on the slice it implements"
	notified := 0
	var errs []error
	for _, sl := range candidates {
		if notified >= ambiguousNoteMaxSlices {
			break
		}
		if hasDecisionPrefix(sl.Decisions, marker) {
			notified++
			continue
		}
		if err := r.plans.AppendSliceDecision(ctx, sl.ID, note); err != nil {
			errs = append(errs, fmt.Errorf("slice %s: %w", sl.ID, err))
			continue
		}
		notified++
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrSliceAmbiguous, errors.Join(errs...))
	}
	r.logger.Warn("take-up could not attribute mr_ref to a single slice; flagged candidates",
		"mr_ref", mrRef, "backlog_id", backlogID, "candidates", len(candidates))
	return fmt.Errorf("%w: %d candidate slice(s) flagged", ErrSliceAmbiguous, notified)
}

// sliceForBacklogID finds the slice whose emitted backlog item is backlogID.
// The derivation is the emitter's own (lossy, deterministic) sanitization, so
// this is the exact inverse of how the item was minted.
func sliceForBacklogID(slices []clients.PlanSliceSummary, backlogID string) (clients.PlanSliceSummary, bool) {
	id := strings.TrimSpace(backlogID)
	if id == "" {
		return clients.PlanSliceSummary{}, false
	}
	for _, sl := range slices {
		if strings.TrimSpace(sl.ID) == "" {
			continue
		}
		if intake.BacklogIDForSlice(sl.ID) == id {
			return sl, true
		}
	}
	return clients.PlanSliceSummary{}, false
}

// recordable reports whether a slice is a legitimate target for a new ref: it
// must be addressable, and it must not already have merged against a DIFFERENT
// MR. Take-up ignores merged slices, so overwriting one's ref would destroy
// provenance and gain nothing.
func recordable(sl clients.PlanSliceSummary) bool {
	if strings.TrimSpace(sl.ID) == "" {
		return false
	}
	return strings.TrimSpace(sl.MRRef) == "" || !strings.EqualFold(strings.TrimSpace(sl.Phase), "merged")
}

// unrecordedSlices returns the slices with no mr_ref yet, preserving plan
// order.
func unrecordedSlices(slices []clients.PlanSliceSummary) []clients.PlanSliceSummary {
	out := make([]clients.PlanSliceSummary, 0, len(slices))
	for _, sl := range slices {
		if strings.TrimSpace(sl.ID) == "" {
			continue
		}
		if strings.TrimSpace(sl.MRRef) != "" {
			continue
		}
		out = append(out, sl)
	}
	return out
}

// sliceByFileOverlap picks the slice whose declared files best cover the run's
// captured paths. A tie is NOT a match: two slices scoring equally means we
// don't know, and a wrong ref is worse than none.
func sliceByFileOverlap(slices []clients.PlanSliceSummary, files []string) (clients.PlanSliceSummary, bool) {
	captured := normalizePaths(files)
	if len(captured) == 0 {
		return clients.PlanSliceSummary{}, false
	}
	best, bestScore, tied := clients.PlanSliceSummary{}, 0, false
	for _, sl := range slices {
		score := overlapScore(sl.Files, captured)
		switch {
		case score > bestScore:
			best, bestScore, tied = sl, score, false
		case score == bestScore && score > 0:
			tied = true
		}
	}
	if bestScore == 0 || tied {
		return clients.PlanSliceSummary{}, false
	}
	return best, true
}

// overlapScore counts how many of a slice's declared entries the captured set
// touches. A declared entry matches a captured path when they are the same
// file, when one is a path-boundary suffix of the other (worktree-relative vs
// repo-relative spellings), or when the entry is a directory envelope
// containing the captured path.
func overlapScore(declared []string, captured map[string]struct{}) int {
	score := 0
	for _, raw := range declared {
		entry := normalizePath(raw)
		if entry == "" {
			continue
		}
		for c := range captured {
			if pathsMatch(entry, c) {
				score++
				break
			}
		}
	}
	return score
}

func pathsMatch(declared, captured string) bool {
	if declared == captured {
		return true
	}
	if strings.HasSuffix(captured, "/"+declared) || strings.HasSuffix(declared, "/"+captured) {
		return true
	}
	// Directory envelope: slices routinely declare a package directory while
	// the run captures the files inside it.
	return strings.HasPrefix(captured, declared+"/")
}

// normalizePaths cleans a captured path list into a lookup set.
func normalizePaths(files []string) map[string]struct{} {
	out := make(map[string]struct{}, len(files))
	for _, f := range files {
		if p := normalizePath(f); p != "" {
			out[p] = struct{}{}
		}
	}
	return out
}

// normalizePath trims a path to its comparable form: no surrounding space, no
// leading "./" or "/", no trailing "/".
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "." {
		return ""
	}
	return p
}

// sameMRRef reports whether two references name the same merge request.
// Refs arrive in three spellings (bare IID, !IID, canonical URL); comparing
// the parsed identity keeps a re-run from re-recording an equivalent ref.
func sameMRRef(a, b string) bool {
	pa, oka := clients.ParseGitLabMRReference(a)
	pb, okb := clients.ParseGitLabMRReference(b)
	if !oka || !okb || pa.IID != pb.IID {
		return false
	}
	if pa.ProjectBound && pb.ProjectBound {
		return clients.SameGitLabProject(pa.Project, pb.Project)
	}
	return true
}

// hasDecisionPrefix reports whether any decision already opens with marker.
func hasDecisionPrefix(decisions []string, marker string) bool {
	for _, d := range decisions {
		if strings.HasPrefix(d, marker) {
			return true
		}
	}
	return false
}

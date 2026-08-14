package mrwatch

import (
	"net/http"
	"strings"
	"time"

	registry "github.com/crb2nu/loom/internal/hud/mrwatch"
)

// BranchStatusResponse is the payload for GET /api/agent/mr-status.
type BranchStatusResponse struct {
	Branch        string                  `json:"branch"`
	Repo          string                  `json:"repo,omitempty"`
	MergeRequests []registry.MergeRequest `json:"merge_requests"`
	Count         int                     `json:"count"`
	LastPollAt    time.Time               `json:"last_poll_at"`
	Stale         bool                    `json:"stale"`
	// Merged is true when at least one matched MR has merged. It is the
	// branch-level form of the per-MR marker (state "merged" / merged true) and
	// exists so a consumer can answer "did this branch land?" without walking
	// the array. False means "no merged MR in the registry right now", which
	// covers not-merged, merged longer ago than the retention window, and a
	// registry that never saw the MR — so it is a signal to keep waiting, never
	// a signal that the branch was closed unmerged.
	Merged bool `json:"merged"`
}

// handleBranchStatus returns the classified status of every MR whose source
// branch matches the `branch` query param — open MRs plus any that merged
// within the registry's merged-retention window (state "merged"). An optional
// `repo` param narrows to a single project path. `branch` is required (400
// otherwise).
func (d *Domain) handleBranchStatus(w http.ResponseWriter, r *http.Request) {
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if branch == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "branch query parameter is required", nil)
		return
	}

	snap := d.deps.MRWatchSnapshot()
	matches := make([]registry.MergeRequest, 0)
	merged := false
	for _, mr := range snap.MergeRequests {
		if mr.SourceBranch != branch {
			continue
		}
		if repo != "" && mr.Repo != repo {
			continue
		}
		if mr.State == registry.StateMerged {
			merged = true
		}
		matches = append(matches, mr)
	}

	d.deps.WriteJSON(w, http.StatusOK, BranchStatusResponse{
		Branch:        branch,
		Repo:          repo,
		MergeRequests: matches,
		Count:         len(matches),
		LastPollAt:    snap.LastPollAt,
		Stale:         snap.Stale,
		Merged:        merged,
	})
}

// ActionsResponse is the payload for GET /api/mrwatch/actions.
type ActionsResponse struct {
	Actions         []registry.ActionRecord `json:"actions"`
	Count           int                     `json:"count"`
	ShepherdEnabled bool                    `json:"shepherd_enabled"`
}

// handleActions returns the shepherd's bounded audit log: the most recent
// bounded auto-actions (retry pipeline / create pipeline / arm auto-merge)
// with their outcomes. Empty (and always a JSON array) when the shepherd is
// disabled or has taken no actions.
func (d *Domain) handleActions(w http.ResponseWriter, _ *http.Request) {
	actions := d.deps.MRWatchActions()
	if actions == nil {
		actions = []registry.ActionRecord{}
	}
	d.deps.WriteJSON(w, http.StatusOK, ActionsResponse{
		Actions:         actions,
		Count:           len(actions),
		ShepherdEnabled: d.deps.MRWatchShepherdEnabled(),
	})
}

// handleSummary returns the full registry snapshot: every classified open MR,
// counts by class, watched projects, and the last successful poll time.
func (d *Domain) handleSummary(w http.ResponseWriter, r *http.Request) {
	snap := d.deps.MRWatchSnapshot()
	// Snapshot() already guarantees non-nil slices/maps, but defend the
	// contract at the wire boundary so a hand-constructed snapshot can't emit
	// null arrays.
	if snap.MergeRequests == nil {
		snap.MergeRequests = []registry.MergeRequest{}
	}
	if snap.Counts == nil {
		snap.Counts = map[string]int{}
	}
	if snap.Projects == nil {
		snap.Projects = []string{}
	}
	d.deps.WriteJSON(w, http.StatusOK, snap)
}

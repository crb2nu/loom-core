package merge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/hud/coordination"
	"github.com/crb2nu/loom/internal/hud/mrwatch"
)

// gitRemoteURLEnv is the env var consulted by the merge domain to build
// per-candidate deep links into the upstream forge. When set, it overrides
// auto-detection. When unset and auto-detection fails, deep-link fields
// are omitted from the JSON payload and the HUD renders nothing.
const gitRemoteURLEnv = "LOOM_HUD_GIT_REMOTE_URL"

// gitRemoteDetector is overridable so tests can swap in a deterministic
// implementation. Production code uses detectGitRemoteURL, which shells out
// to `git remote get-url origin` in the daemon's current working directory.
var gitRemoteDetector = detectGitRemoteURL

// MergeCandidate represents an agent branch eligible for merge consideration.
type MergeCandidate struct {
	AgentID            string        `json:"agent_id"`
	Branch             string        `json:"branch"`
	Namespace          string        `json:"namespace,omitempty"`
	Status             string        `json:"status"`
	MergeReady         bool          `json:"merge_ready"`
	MergeBlockers      []string      `json:"merge_blockers,omitempty"`
	ConflictFiles      int           `json:"conflict_files"`
	BlockedTasks       int           `json:"blocked_tasks"`
	TaskCount          int           `json:"task_count"`
	BranchURL          string        `json:"branch_url,omitempty"`
	MergeRequestNewURL string        `json:"merge_request_new_url,omitempty"`
	MRIID              int64         `json:"mr_iid,omitempty"`
	MRState            mrwatch.State `json:"mr_state,omitempty"`
	MRWebURL           string        `json:"mr_web_url,omitempty"`
}

// MergeQueueResponse is the payload for GET /api/merge-queue.
type MergeQueueResponse struct {
	Ready   []MergeCandidate  `json:"ready"`
	Blocked []MergeCandidate  `json:"blocked"`
	Summary MergeQueueSummary `json:"summary"`
}

// MergeQueueSummary provides top-level merge queue metrics.
type MergeQueueSummary struct {
	TotalBranches int `json:"total_branches"`
	ReadyToMerge  int `json:"ready_to_merge"`
	Blocked       int `json:"blocked"`
	ConflictPairs int `json:"conflict_pairs"`
}

// MergeConflictPair describes a predicted merge conflict between two agents.
type MergeConflictPair struct {
	LeftAgent    string   `json:"left_agent"`
	LeftBranch   string   `json:"left_branch"`
	RightAgent   string   `json:"right_agent"`
	RightBranch  string   `json:"right_branch"`
	ConflictType string   `json:"conflict_type"`
	Files        []string `json:"files,omitempty"`
	Detail       string   `json:"detail,omitempty"`
}

// MergeConflictsResponse is the payload for GET /api/merge-queue/conflicts.
type MergeConflictsResponse struct {
	Conflicts []MergeConflictPair `json:"conflicts"`
	Count     int                 `json:"count"`
}

// handleMergeQueue returns the ordered merge queue derived from the coordination snapshot.
func (d *MergeDomain) handleMergeQueue(w http.ResponseWriter, r *http.Request) {
	snap := d.deps.CoordinationSnapshot()
	mrSnap := d.deps.MRWatchSnapshot()
	remoteURL := resolveRemoteURL()
	mrsByBranch := make(map[string]mrwatch.MergeRequest, len(mrSnap.MergeRequests))
	for _, mr := range mrSnap.MergeRequests {
		mrsByBranch[mr.SourceBranch] = mr
	}

	var ready, blocked []MergeCandidate
	for _, agent := range snap.Agents {
		if agent.Branch == "" || agent.Branch == "main" || agent.Branch == "master" {
			continue
		}
		branchURL, mrURL := candidateURLs(remoteURL, agent.Branch)
		candidate := MergeCandidate{
			AgentID:            agent.AgentID,
			Branch:             agent.Branch,
			Namespace:          agent.Namespace,
			Status:             agent.Status,
			MergeReady:         agent.MergeReady,
			MergeBlockers:      agent.MergeBlockers,
			ConflictFiles:      agent.ConflictFiles,
			BlockedTasks:       agent.BlockedTasks,
			TaskCount:          agent.TaskCount,
			BranchURL:          branchURL,
			MergeRequestNewURL: mrURL,
		}
		if mr, ok := mrsByBranch[agent.Branch]; ok {
			candidate.MRIID = mr.IID
			candidate.MRState = mr.State
			candidate.MRWebURL = mr.WebURL
			candidate.MergeRequestNewURL = ""
			candidate.MergeReady = candidate.MergeReady && (mr.State == mrwatch.StateOK || mr.State == mrwatch.StateAutomergeUnarmed)
		}
		if candidate.MergeReady {
			ready = append(ready, candidate)
		} else {
			blocked = append(blocked, candidate)
		}
	}

	sort.Slice(ready, func(i, j int) bool {
		if ready[i].ConflictFiles != ready[j].ConflictFiles {
			return ready[i].ConflictFiles < ready[j].ConflictFiles
		}
		return ready[i].AgentID < ready[j].AgentID
	})

	sort.Slice(blocked, func(i, j int) bool {
		if len(blocked[i].MergeBlockers) != len(blocked[j].MergeBlockers) {
			return len(blocked[i].MergeBlockers) < len(blocked[j].MergeBlockers)
		}
		return blocked[i].AgentID < blocked[j].AgentID
	})

	conflictPairs := countConflictPairs(snap)

	if ready == nil {
		ready = []MergeCandidate{}
	}
	if blocked == nil {
		blocked = []MergeCandidate{}
	}

	d.deps.WriteJSON(w, http.StatusOK, MergeQueueResponse{
		Ready:   ready,
		Blocked: blocked,
		Summary: MergeQueueSummary{
			TotalBranches: len(ready) + len(blocked),
			ReadyToMerge:  len(ready),
			Blocked:       len(blocked),
			ConflictPairs: conflictPairs,
		},
	})
}

// handleMergeConflicts returns predicted merge conflicts based on file claims
// and shared branches in the coordination snapshot.
func (d *MergeDomain) handleMergeConflicts(w http.ResponseWriter, r *http.Request) {
	snap := d.deps.CoordinationSnapshot()
	conflicts := buildConflictPairs(snap)

	d.deps.WriteJSON(w, http.StatusOK, MergeConflictsResponse{
		Conflicts: conflicts,
		Count:     len(conflicts),
	})
}

// buildConflictPairs extracts file_conflict and shared_branch relations
// that would create merge conflicts, enriched with branch info. Multiple
// file_conflict relations between the same (source, target) are aggregated
// into a single pair whose Files slice lists every conflicting path.
func buildConflictPairs(snap coordination.Snapshot) []MergeConflictPair {
	agentBranch := make(map[string]string, len(snap.Agents))
	for _, agent := range snap.Agents {
		if agent.Branch != "" {
			agentBranch[agent.AgentID] = agent.Branch
		}
	}

	var conflicts []MergeConflictPair
	indexByKey := make(map[string]int)

	for _, rel := range snap.Relations {
		if rel.Kind != "file_conflict" && rel.Kind != "shared_branch" {
			continue
		}
		key := rel.Source + "|" + rel.Target + "|" + rel.Kind
		if idx, ok := indexByKey[key]; ok {
			// Aggregate additional files into the existing pair.
			if rel.Kind == "file_conflict" && rel.Detail != "" {
				conflicts[idx].Files = append(conflicts[idx].Files, rel.Detail)
			}
			continue
		}

		pair := MergeConflictPair{
			LeftAgent:    rel.Source,
			LeftBranch:   agentBranch[rel.Source],
			RightAgent:   rel.Target,
			RightBranch:  agentBranch[rel.Target],
			ConflictType: rel.Kind,
			Detail:       rel.Detail,
		}
		if rel.Kind == "file_conflict" && rel.Detail != "" {
			pair.Files = []string{rel.Detail}
		}
		conflicts = append(conflicts, pair)
		indexByKey[key] = len(conflicts) - 1
	}

	// Sort files inside each pair so output is stable regardless of relation order.
	for i := range conflicts {
		if len(conflicts[i].Files) > 1 {
			sort.Strings(conflicts[i].Files)
		}
	}

	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].ConflictType != conflicts[j].ConflictType {
			return conflicts[i].ConflictType < conflicts[j].ConflictType
		}
		return conflicts[i].LeftAgent < conflicts[j].LeftAgent
	})

	if conflicts == nil {
		conflicts = []MergeConflictPair{}
	}
	return conflicts
}

// resolveRemoteURL returns the upstream forge URL used for merge-queue deep
// links, in priority order: the LOOM_HUD_GIT_REMOTE_URL env var (operator
// opt-in, e.g., for daemons running in k8s where the working tree is not a
// git checkout), then auto-detection via `git remote get-url origin` from
// the daemon's current working directory. The returned URL is always passed
// through sanitizeRemoteURL so embedded credentials (e.g., the oauth2:token
// userinfo that GitLab adds for some clones) cannot leak into the HUD's
// browser-readable JSON payload.
func resolveRemoteURL() string {
	if env := strings.TrimSpace(os.Getenv(gitRemoteURLEnv)); env != "" {
		return sanitizeRemoteURL(env)
	}
	return sanitizeRemoteURL(gitRemoteDetector())
}

// sanitizeRemoteURL removes any userinfo segment (user:password@host) from
// the URL and refuses any scheme other than http/https, since deep links are
// rendered as <a href> in the HUD frontend and must be browser-clickable.
// Returns "" for any input that can't be safely emitted; callers treat ""
// as "no deep link" and the omitempty JSON tags suppress the field.
func sanitizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Reject scp-style SSH URLs like `git@host:owner/repo.git` — they cannot
	// be opened from a browser anyway, and url.Parse misinterprets them.
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	if u.Host == "" {
		return ""
	}
	// Strip any embedded credentials before emitting to the frontend.
	u.User = nil
	return u.String()
}

// detectGitRemoteURL invokes `git remote get-url origin` in the daemon's
// current working directory with a short timeout. Any failure (git missing,
// not a repo, no origin, timeout) yields "" so the link surface stays
// graceful. The 2s timeout protects against hung git invocations without
// noticeably delaying a HUD poll.
func detectGitRemoteURL() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// candidateURLs builds GitLab-style deep links for a merge candidate. It
// returns ("", "") when either the remote base or the branch is empty so
// the JSON payload omits the fields (via omitempty) and the HUD can render
// nothing. The remote base tolerates trailing slashes and a `.git` suffix
// so the env var can hold either the clone URL or the project URL.
//
// GitLab patterns (the workspace's primary forge):
//   - branch: {base}/-/tree/{branch}
//   - new MR: {base}/-/merge_requests/new?merge_request[source_branch]={branch}
func candidateURLs(remoteURL, branch string) (string, string) {
	if remoteURL == "" || branch == "" {
		return "", ""
	}
	base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(remoteURL), "/"), ".git")
	if base == "" {
		return "", ""
	}
	branchURL := fmt.Sprintf("%s/-/tree/%s", base, url.PathEscape(branch))
	mrURL := fmt.Sprintf("%s/-/merge_requests/new?merge_request[source_branch]=%s", base, url.QueryEscape(branch))
	return branchURL, mrURL
}

// countConflictPairs counts the number of unique (source, target) agent pairs
// that have at least one file_conflict relation. The "conflict_pairs" summary
// metric is rendered as "N conflict pair(s)" in the HUD merge queue view, so
// this counts agent pairs, not individual conflicting files.
func countConflictPairs(snap coordination.Snapshot) int {
	seen := make(map[string]struct{})
	for _, rel := range snap.Relations {
		if rel.Kind != "file_conflict" {
			continue
		}
		key := rel.Source + "|" + rel.Target
		seen[key] = struct{}{}
	}
	return len(seen)
}

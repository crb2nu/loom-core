package merge

import (
	"net/http"
	"sort"

	"github.com/crb2nu/loom/internal/hud/coordination"
)

// MergeCandidate represents an agent branch eligible for merge consideration.
type MergeCandidate struct {
	AgentID       string   `json:"agent_id"`
	Branch        string   `json:"branch"`
	Namespace     string   `json:"namespace,omitempty"`
	Status        string   `json:"status"`
	MergeReady    bool     `json:"merge_ready"`
	MergeBlockers []string `json:"merge_blockers,omitempty"`
	ConflictFiles int      `json:"conflict_files"`
	BlockedTasks  int      `json:"blocked_tasks"`
	TaskCount     int      `json:"task_count"`
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

	var ready, blocked []MergeCandidate
	for _, agent := range snap.Agents {
		if agent.Branch == "" || agent.Branch == "main" || agent.Branch == "master" {
			continue
		}
		candidate := MergeCandidate{
			AgentID:       agent.AgentID,
			Branch:        agent.Branch,
			Namespace:     agent.Namespace,
			Status:        agent.Status,
			MergeReady:    agent.MergeReady,
			MergeBlockers: agent.MergeBlockers,
			ConflictFiles: agent.ConflictFiles,
			BlockedTasks:  agent.BlockedTasks,
			TaskCount:     agent.TaskCount,
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

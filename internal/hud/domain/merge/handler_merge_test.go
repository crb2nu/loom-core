package merge

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/internal/hud/coordination"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockDeps struct {
	snap coordination.Snapshot
}

func (m *mockDeps) WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *mockDeps) WriteError(w http.ResponseWriter, status int, msg string, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (m *mockDeps) Logger() *slog.Logger { return slog.Default() }

func (m *mockDeps) CoordinationSnapshot() coordination.Snapshot { return m.snap }

func TestHandleMergeQueue_Empty(t *testing.T) {
	d := New(&mockDeps{snap: coordination.Snapshot{}})
	req := httptest.NewRequest("GET", "/api/merge-queue", nil)
	rec := httptest.NewRecorder()

	d.handleMergeQueue(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp MergeQueueResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Summary.TotalBranches)
	assert.Equal(t, 0, resp.Summary.ReadyToMerge)
	assert.Empty(t, resp.Ready)
	assert.Empty(t, resp.Blocked)
}

func TestHandleMergeQueue_ReadyAndBlocked(t *testing.T) {
	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{AgentID: "ready-1", Branch: "feat/a", Status: "active", MergeReady: true},
			{AgentID: "ready-2", Branch: "feat/b", Status: "active", MergeReady: true},
			{AgentID: "blocked-1", Branch: "feat/c", Status: "active", MergeReady: false, MergeBlockers: []string{"blocked_tasks"}},
			{AgentID: "on-main", Branch: "main", Status: "active"},
			{AgentID: "no-branch", Status: "active"},
		},
	}
	d := New(&mockDeps{snap: snap})
	req := httptest.NewRequest("GET", "/api/merge-queue", nil)
	rec := httptest.NewRecorder()

	d.handleMergeQueue(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp MergeQueueResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.Summary.TotalBranches)
	assert.Equal(t, 2, resp.Summary.ReadyToMerge)
	assert.Equal(t, 1, resp.Summary.Blocked)
	assert.Len(t, resp.Ready, 2)
	assert.Len(t, resp.Blocked, 1)
	assert.Equal(t, "blocked-1", resp.Blocked[0].AgentID)
}

func TestHandleMergeConflicts_Empty(t *testing.T) {
	d := New(&mockDeps{snap: coordination.Snapshot{}})
	req := httptest.NewRequest("GET", "/api/merge-queue/conflicts", nil)
	rec := httptest.NewRecorder()

	d.handleMergeConflicts(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp MergeConflictsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.Count)
	assert.Empty(t, resp.Conflicts)
}

func TestHandleMergeConflicts_FileConflicts(t *testing.T) {
	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{AgentID: "agent-a", Branch: "feat/a"},
			{AgentID: "agent-b", Branch: "feat/b"},
		},
		Relations: []coordination.RelationEdge{
			{Kind: "file_conflict", Source: "agent-a", Target: "agent-b", Detail: "shared.go", Severity: "critical"},
			{Kind: "shared_branch", Source: "agent-c", Target: "agent-d", Detail: "feat/shared"},
			{Kind: "task_blocker", Source: "task-1", Target: "task-2"},
		},
	}
	d := New(&mockDeps{snap: snap})
	req := httptest.NewRequest("GET", "/api/merge-queue/conflicts", nil)
	rec := httptest.NewRecorder()

	d.handleMergeConflicts(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp MergeConflictsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Count)

	// file_conflict comes first alphabetically
	assert.Equal(t, "file_conflict", resp.Conflicts[0].ConflictType)
	assert.Equal(t, "agent-a", resp.Conflicts[0].LeftAgent)
	assert.Equal(t, "feat/a", resp.Conflicts[0].LeftBranch)
	assert.Equal(t, []string{"shared.go"}, resp.Conflicts[0].Files)

	assert.Equal(t, "shared_branch", resp.Conflicts[1].ConflictType)
}

func TestHandleMergeConflicts_MultipleFilesAggregated(t *testing.T) {
	// Three file_conflict relations between the same (agent-a, agent-b) pair
	// should collapse into a single MergeConflictPair whose Files slice lists
	// every conflicting path in sorted order, and the summary should report
	// one conflict pair, not three.
	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{AgentID: "agent-a", Branch: "feat/a", MergeReady: true},
			{AgentID: "agent-b", Branch: "feat/b", MergeReady: true},
		},
		Relations: []coordination.RelationEdge{
			{Kind: "file_conflict", Source: "agent-a", Target: "agent-b", Detail: "pkg/zeta.go"},
			{Kind: "file_conflict", Source: "agent-a", Target: "agent-b", Detail: "pkg/alpha.go"},
			{Kind: "file_conflict", Source: "agent-a", Target: "agent-b", Detail: "pkg/mid.go"},
		},
	}
	d := New(&mockDeps{snap: snap})

	conflictsReq := httptest.NewRequest("GET", "/api/merge-queue/conflicts", nil)
	conflictsRec := httptest.NewRecorder()
	d.handleMergeConflicts(conflictsRec, conflictsReq)
	require.Equal(t, http.StatusOK, conflictsRec.Code)

	var conflictsResp MergeConflictsResponse
	require.NoError(t, json.Unmarshal(conflictsRec.Body.Bytes(), &conflictsResp))
	require.Equal(t, 1, conflictsResp.Count, "three file_conflicts between one pair must collapse to one pair")
	require.Len(t, conflictsResp.Conflicts, 1)

	pair := conflictsResp.Conflicts[0]
	assert.Equal(t, "file_conflict", pair.ConflictType)
	assert.Equal(t, "agent-a", pair.LeftAgent)
	assert.Equal(t, "agent-b", pair.RightAgent)
	assert.Equal(t, "feat/a", pair.LeftBranch)
	assert.Equal(t, "feat/b", pair.RightBranch)
	assert.Equal(t, []string{"pkg/alpha.go", "pkg/mid.go", "pkg/zeta.go"}, pair.Files,
		"aggregated files must be sorted for stable output")

	// The merge-queue summary should report a single conflict pair, matching the
	// rendered "1 conflict pair" label in the HUD.
	queueReq := httptest.NewRequest("GET", "/api/merge-queue", nil)
	queueRec := httptest.NewRecorder()
	d.handleMergeQueue(queueRec, queueReq)
	require.Equal(t, http.StatusOK, queueRec.Code)

	var queueResp MergeQueueResponse
	require.NoError(t, json.Unmarshal(queueRec.Body.Bytes(), &queueResp))
	assert.Equal(t, 1, queueResp.Summary.ConflictPairs,
		"conflict_pairs metric counts distinct agent pairs, not individual files")
}

func TestCandidateURLs(t *testing.T) {
	cases := []struct {
		name       string
		remote     string
		branch     string
		wantBranch string
		wantMR     string
	}{
		{
			name:       "unset_remote_returns_empty",
			remote:     "",
			branch:     "feat/x",
			wantBranch: "",
			wantMR:     "",
		},
		{
			name:       "empty_branch_returns_empty",
			remote:     "https://gitlab.example.com/team/repo",
			branch:     "",
			wantBranch: "",
			wantMR:     "",
		},
		{
			name:       "trailing_slash_tolerated",
			remote:     "https://gitlab.example.com/team/repo/",
			branch:     "feat/x",
			wantBranch: "https://gitlab.example.com/team/repo/-/tree/feat%2Fx",
			wantMR:     "https://gitlab.example.com/team/repo/-/merge_requests/new?merge_request[source_branch]=feat%2Fx",
		},
		{
			name:       "git_suffix_stripped",
			remote:     "https://gitlab.example.com/team/repo.git",
			branch:     "fix/y",
			wantBranch: "https://gitlab.example.com/team/repo/-/tree/fix%2Fy",
			wantMR:     "https://gitlab.example.com/team/repo/-/merge_requests/new?merge_request[source_branch]=fix%2Fy",
		},
		{
			name:       "branch_with_special_chars_escaped",
			remote:     "https://gitlab.example.com/team/repo",
			branch:     "feat/a b&c",
			wantBranch: "https://gitlab.example.com/team/repo/-/tree/feat%2Fa%20b&c",
			wantMR:     "https://gitlab.example.com/team/repo/-/merge_requests/new?merge_request[source_branch]=feat%2Fa+b%26c",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBranch, gotMR := candidateURLs(tc.remote, tc.branch)
			assert.Equal(t, tc.wantBranch, gotBranch, "branch URL")
			assert.Equal(t, tc.wantMR, gotMR, "merge request URL")
		})
	}
}

func TestHandleMergeQueue_PopulatesDeepLinksWhenEnvSet(t *testing.T) {
	t.Setenv("LOOM_HUD_GIT_REMOTE_URL", "https://gitlab.example.com/team/repo")

	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{AgentID: "a", Branch: "feat/one", Status: "active", MergeReady: true},
			{AgentID: "b", Branch: "feat/two", Status: "active", MergeReady: false, MergeBlockers: []string{"blocked_tasks"}},
		},
	}
	d := New(&mockDeps{snap: snap})

	req := httptest.NewRequest("GET", "/api/merge-queue", nil)
	rec := httptest.NewRecorder()
	d.handleMergeQueue(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp MergeQueueResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Ready, 1)
	require.Len(t, resp.Blocked, 1)
	assert.Equal(t, "https://gitlab.example.com/team/repo/-/tree/feat%2Fone", resp.Ready[0].BranchURL)
	assert.Equal(t, "https://gitlab.example.com/team/repo/-/merge_requests/new?merge_request[source_branch]=feat%2Fone", resp.Ready[0].MergeRequestNewURL)
	// Blocked candidates also get URLs so the HUD can still link to the branch
	// before it becomes merge-ready.
	assert.Equal(t, "https://gitlab.example.com/team/repo/-/tree/feat%2Ftwo", resp.Blocked[0].BranchURL)
	assert.Equal(t, "https://gitlab.example.com/team/repo/-/merge_requests/new?merge_request[source_branch]=feat%2Ftwo", resp.Blocked[0].MergeRequestNewURL)
}

func TestHandleMergeQueue_OmitsDeepLinksWhenEnvUnset(t *testing.T) {
	t.Setenv("LOOM_HUD_GIT_REMOTE_URL", "")

	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{AgentID: "a", Branch: "feat/one", Status: "active", MergeReady: true},
		},
	}
	d := New(&mockDeps{snap: snap})

	req := httptest.NewRequest("GET", "/api/merge-queue", nil)
	rec := httptest.NewRecorder()
	d.handleMergeQueue(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Body must not include the deep-link keys at all (omitempty contract).
	body := rec.Body.String()
	assert.NotContains(t, body, "branch_url")
	assert.NotContains(t, body, "merge_request_new_url")
}

func TestHandleMergeConflicts_BidirectionalPairsAreSeparate(t *testing.T) {
	// a→b and b→a are directional relations: surfacing them as a single pair
	// would require normalization upstream. Preserve current behavior: each
	// direction yields its own MergeConflictPair so the HUD can show the
	// asymmetry. This test pins the contract so future refactors are deliberate.
	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{AgentID: "agent-a", Branch: "feat/a"},
			{AgentID: "agent-b", Branch: "feat/b"},
		},
		Relations: []coordination.RelationEdge{
			{Kind: "file_conflict", Source: "agent-a", Target: "agent-b", Detail: "x.go"},
			{Kind: "file_conflict", Source: "agent-b", Target: "agent-a", Detail: "y.go"},
		},
	}
	d := New(&mockDeps{snap: snap})

	req := httptest.NewRequest("GET", "/api/merge-queue/conflicts", nil)
	rec := httptest.NewRecorder()
	d.handleMergeConflicts(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp MergeConflictsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Count)
	assert.Len(t, resp.Conflicts, 2)
}

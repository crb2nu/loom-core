package mrwatch

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	registry "github.com/crb2nu/loom/internal/hud/mrwatch"
)

// mockDeps is a fake host App for the mrwatch domain handlers.
type mockDeps struct {
	snap            registry.Snapshot
	actions         []registry.ActionRecord
	shepherdEnabled bool
}

func (m *mockDeps) WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *mockDeps) WriteError(w http.ResponseWriter, status int, msg string, _ error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (m *mockDeps) Logger() *slog.Logger { return slog.Default() }

func (m *mockDeps) MRWatchSnapshot() registry.Snapshot { return m.snap }

func (m *mockDeps) MRWatchActions() []registry.ActionRecord { return m.actions }

func (m *mockDeps) MRWatchShepherdEnabled() bool { return m.shepherdEnabled }

func fixtureSnapshot() registry.Snapshot {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	return registry.Snapshot{
		MergeRequests: []registry.MergeRequest{
			{Repo: "services/loom-core", IID: 1, SourceBranch: "feat/a", State: registry.StateOK},
			{Repo: "services/loom-core", IID: 2, SourceBranch: "feat/b", State: registry.StateConflict},
			{Repo: "services/other", IID: 5, SourceBranch: "feat/a", State: registry.StateCIRunning},
		},
		Counts: map[string]int{
			string(registry.StateOK):        1,
			string(registry.StateConflict):  1,
			string(registry.StateCIRunning): 1,
		},
		Projects:   []string{"services/loom-core", "services/other"},
		LastPollAt: now,
	}
}

func TestHandleBranchStatus_MatchesBranch(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot()})
	req := httptest.NewRequest("GET", "/api/agent/mr-status?branch=feat/a", nil)
	rec := httptest.NewRecorder()
	d.handleBranchStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp BranchStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 2 {
		t.Errorf("count = %d, want 2 (feat/a across both repos)", resp.Count)
	}
	if resp.Branch != "feat/a" {
		t.Errorf("branch = %q", resp.Branch)
	}
}

func TestHandleBranchStatus_RepoFilter(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot()})
	req := httptest.NewRequest("GET", "/api/agent/mr-status?branch=feat/a&repo=services/other", nil)
	rec := httptest.NewRecorder()
	d.handleBranchStatus(rec, req)

	var resp BranchStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 {
		t.Fatalf("count = %d, want 1", resp.Count)
	}
	if resp.MergeRequests[0].Repo != "services/other" {
		t.Errorf("repo = %q, want services/other", resp.MergeRequests[0].Repo)
	}
}

// TestHandleBranchStatus_CarriesMergedMarker: the mr-status payload is what the
// plan-store truth sweep reads, and it advances only on an explicit merged
// marker. A retained merged MR must therefore surface BOTH state "merged" and
// merged true on the entry, plus the branch-level merged flag.
func TestHandleBranchStatus_CarriesMergedMarker(t *testing.T) {
	mergedAt := time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC)
	snap := fixtureSnapshot()
	snap.MergeRequests = append(snap.MergeRequests, registry.MergeRequest{
		Repo: "services/loom-core", IID: 9, SourceBranch: "feat/landed",
		State: registry.StateMerged, Merged: true, MergedAt: mergedAt,
	})
	d := New(&mockDeps{snap: snap})
	req := httptest.NewRequest("GET", "/api/agent/mr-status?branch=feat/landed", nil)
	rec := httptest.NewRecorder()
	d.handleBranchStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp BranchStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 {
		t.Fatalf("count = %d, want 1", resp.Count)
	}
	if !resp.Merged {
		t.Error("branch-level merged = false, want true")
	}
	got := resp.MergeRequests[0]
	if got.State != registry.StateMerged {
		t.Errorf("state = %q, want %q", got.State, registry.StateMerged)
	}
	if !got.Merged {
		t.Error("entry merged = false, want true")
	}
	if !got.MergedAt.Equal(mergedAt) {
		t.Errorf("merged_at = %v, want %v", got.MergedAt, mergedAt)
	}

	// Assert on the raw wire shape too: the sweep matches on the JSON keys.
	var raw struct {
		Merged        bool `json:"merged"`
		MergeRequests []struct {
			State  string `json:"state"`
			Merged bool   `json:"merged"`
		} `json:"merge_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if !raw.Merged || len(raw.MergeRequests) != 1 ||
		raw.MergeRequests[0].State != "merged" || !raw.MergeRequests[0].Merged {
		t.Errorf("wire payload missing the merged marker: %s", rec.Body.Bytes())
	}
}

// TestHandleBranchStatus_ClosedIsNotMerged: a closed-unmerged MR must never set
// the merged marker. (The registry drops closed MRs today; this pins the
// handler so a future decision to surface them cannot conflate the two.)
func TestHandleBranchStatus_ClosedIsNotMerged(t *testing.T) {
	snap := fixtureSnapshot()
	snap.MergeRequests = append(snap.MergeRequests, registry.MergeRequest{
		Repo: "services/loom-core", IID: 10, SourceBranch: "feat/abandoned",
		State: registry.StateClosed,
	})
	d := New(&mockDeps{snap: snap})
	req := httptest.NewRequest("GET", "/api/agent/mr-status?branch=feat/abandoned", nil)
	rec := httptest.NewRecorder()
	d.handleBranchStatus(rec, req)

	var resp BranchStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Merged {
		t.Error("closed MR reported as merged")
	}
	if resp.MergeRequests[0].Merged {
		t.Error("closed entry carries the merged flag")
	}
}

// TestHandleBranchStatus_NoMRIsNotMerged: an unknown branch must not read as
// merged — absence is "keep waiting", never "it landed".
func TestHandleBranchStatus_NoMRIsNotMerged(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot()})
	req := httptest.NewRequest("GET", "/api/agent/mr-status?branch=never-existed", nil)
	rec := httptest.NewRecorder()
	d.handleBranchStatus(rec, req)

	var resp BranchStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Merged || resp.Count != 0 {
		t.Errorf("merged = %v count = %d, want false/0", resp.Merged, resp.Count)
	}
}

func TestHandleBranchStatus_MissingBranchIs400(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot()})
	req := httptest.NewRequest("GET", "/api/agent/mr-status", nil)
	rec := httptest.NewRecorder()
	d.handleBranchStatus(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleBranchStatus_NoMatchEncodesEmptyArray(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot()})
	req := httptest.NewRequest("GET", "/api/agent/mr-status?branch=does-not-exist", nil)
	rec := httptest.NewRecorder()
	d.handleBranchStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// merge_requests must be [] not null.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["merge_requests"]) != "[]" {
		t.Errorf("merge_requests = %s, want []", raw["merge_requests"])
	}
}

func TestHandleSummary(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot()})
	req := httptest.NewRequest("GET", "/api/mrwatch/summary", nil)
	rec := httptest.NewRecorder()
	d.handleSummary(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp registry.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.MergeRequests) != 3 {
		t.Errorf("merge_requests = %d, want 3", len(resp.MergeRequests))
	}
	if resp.Counts[string(registry.StateConflict)] != 1 {
		t.Errorf("conflict count = %d, want 1", resp.Counts[string(registry.StateConflict)])
	}
}

func TestHandleSummary_EmptySnapshotEncodesArraysNotNull(t *testing.T) {
	// Zero-value snapshot (nil slices/maps): the handler must defend the wire
	// contract and emit [] / {} rather than null.
	d := New(&mockDeps{snap: registry.Snapshot{}})
	req := httptest.NewRequest("GET", "/api/mrwatch/summary", nil)
	rec := httptest.NewRecorder()
	d.handleSummary(rec, req)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"merge_requests", "counts", "projects"} {
		if string(raw[key]) == "null" {
			t.Errorf("key %q encoded as null: %s", key, rec.Body.Bytes())
		}
	}
}

func TestHandleActions(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	d := New(&mockDeps{
		snap: fixtureSnapshot(),
		actions: []registry.ActionRecord{
			{Time: now, Repo: "services/loom-core", MRIID: 7, Branch: "feat/x",
				State: string(registry.StateAutomergeUnarmed), Action: string(registry.ActionArmAutoMerge), Outcome: string(registry.OutcomeOK)},
		},
	})
	req := httptest.NewRequest("GET", "/api/mrwatch/actions", nil)
	rec := httptest.NewRecorder()
	d.handleActions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp ActionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || len(resp.Actions) != 1 {
		t.Fatalf("count = %d, actions = %d, want 1/1", resp.Count, len(resp.Actions))
	}
	if resp.ShepherdEnabled {
		t.Error("shepherd_enabled = true, want false")
	}
	if resp.Actions[0].Action != string(registry.ActionArmAutoMerge) {
		t.Errorf("action = %q, want arm_auto_merge", resp.Actions[0].Action)
	}
}

func TestHandleActions_NilEncodesEmptyArray(t *testing.T) {
	// Shepherd disabled → nil actions. The endpoint must emit [] not null.
	d := New(&mockDeps{snap: fixtureSnapshot(), actions: nil})
	req := httptest.NewRequest("GET", "/api/mrwatch/actions", nil)
	rec := httptest.NewRecorder()
	d.handleActions(rec, req)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["actions"]) != "[]" {
		t.Errorf("actions = %s, want []", raw["actions"])
	}
}

func TestHandleActions_ShepherdEnabled(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot(), shepherdEnabled: true})
	req := httptest.NewRequest("GET", "/api/mrwatch/actions", nil)
	rec := httptest.NewRecorder()
	d.handleActions(rec, req)

	var resp ActionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.ShepherdEnabled {
		t.Error("shepherd_enabled = false, want true")
	}
}

// TestRegisterRoutes ensures all endpoints are wired and reachable through a
// real mux (exercises the RegisterRoutes path, not just handlers directly).
func TestRegisterRoutes(t *testing.T) {
	d := New(&mockDeps{snap: fixtureSnapshot()})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	for _, path := range []string{"/api/agent/mr-status?branch=feat/a", "/api/mrwatch/summary", "/api/mrwatch/actions"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

package mrwatch

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSource is a deterministic in-memory Source for tests. Per-project results
// or errors are configured up front; ListOpenMRs never touches the network.
type fakeSource struct {
	mu      sync.Mutex
	results map[string][]MRInfo
	errs    map[string]error
	calls   int
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		results: map[string][]MRInfo{},
		errs:    map[string]error{},
	}
}

func (f *fakeSource) set(project string, mrs []MRInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[project] = mrs
}

func (f *fakeSource) fail(project string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[project] = err
}

func (f *fakeSource) ListOpenMRs(_ context.Context, project string) ([]MRInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.errs[project]; err != nil {
		return nil, err
	}
	return f.results[project], nil
}

func newTestPoller(src Source, now time.Time, projects ...string) *Poller {
	return NewPoller(src, Options{
		Projects: projects,
		Interval: time.Minute,
		Now:      func() time.Time { return now },
	})
}

func TestPoller_ClassifiesAndCounts(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	src := newFakeSource()
	src.set("services/loom-core", []MRInfo{
		{IID: 1, State: "opened", SourceBranch: "feat/a", UpdatedAt: recent,
			MergeWhenPipelineSucceeds: true, Pipeline: &PipelineInfo{Status: "success"}},
		{IID: 2, State: "opened", SourceBranch: "feat/b", UpdatedAt: recent,
			Pipeline: &PipelineInfo{Status: "success"}}, // unarmed
		{IID: 3, State: "opened", SourceBranch: "feat/c", UpdatedAt: recent,
			Pipeline: &PipelineInfo{Status: "failed"}},
		{IID: 4, State: "merged", SourceBranch: "feat/d", MergedAt: recent},  // retained
		{IID: 5, State: "closed", SourceBranch: "feat/e", UpdatedAt: recent}, // dropped
	})

	p := newTestPoller(src, now, "services/loom-core")
	p.pollOnce(context.Background())
	snap := p.Snapshot()

	if len(snap.MergeRequests) != 4 {
		t.Fatalf("want 3 open MRs + 1 retained merged, got %d", len(snap.MergeRequests))
	}
	if snap.Counts[string(StateMerged)] != 1 {
		t.Errorf("merged count = %d, want 1", snap.Counts[string(StateMerged)])
	}
	if snap.Counts[string(StateClosed)] != 0 {
		t.Errorf("closed count = %d, want 0 (closed-unmerged is dropped)", snap.Counts[string(StateClosed)])
	}
	if snap.Counts[string(StateOK)] != 1 {
		t.Errorf("ok count = %d, want 1", snap.Counts[string(StateOK)])
	}
	if snap.Counts[string(StateAutomergeUnarmed)] != 1 {
		t.Errorf("automerge_unarmed count = %d, want 1", snap.Counts[string(StateAutomergeUnarmed)])
	}
	if snap.Counts[string(StateCIFailedDeterministic)] != 1 {
		t.Errorf("ci_failed_deterministic count = %d, want 1", snap.Counts[string(StateCIFailedDeterministic)])
	}
	if snap.Stale {
		t.Error("snapshot should not be stale after a clean poll")
	}
	if !snap.LastPollAt.Equal(now) {
		t.Errorf("LastPollAt = %v, want %v", snap.LastPollAt, now)
	}
	// Repo defaulted to the project path.
	if snap.MergeRequests[0].Repo != "services/loom-core" {
		t.Errorf("repo = %q, want services/loom-core", snap.MergeRequests[0].Repo)
	}
}

// TestPoller_CarriesObservedHeadSHA: the head sha observed by the poll must
// reach the classified snapshot, since that is what the shepherd pins its
// auto-merge arm to. An MR whose source reported no sha carries none, which the
// shepherd treats as "refuse to arm".
func TestPoller_CarriesObservedHeadSHA(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	const sha = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	src := newFakeSource()
	src.set("r", []MRInfo{
		{IID: 1, State: "opened", SourceBranch: "feat/a", SHA: sha, UpdatedAt: now.Add(-time.Hour),
			Pipeline: &PipelineInfo{Status: "success"}},
		{IID: 2, State: "opened", SourceBranch: "feat/b", UpdatedAt: now.Add(-time.Hour),
			Pipeline: &PipelineInfo{Status: "success"}},
	})

	p := newTestPoller(src, now, "r")
	p.pollOnce(context.Background())

	byIID := map[int64]MergeRequest{}
	for _, mr := range p.Snapshot().MergeRequests {
		byIID[mr.IID] = mr
	}
	if got := byIID[1].SHA; got != sha {
		t.Errorf("MR 1 sha = %q, want %q", got, sha)
	}
	if got := byIID[2].SHA; got != "" {
		t.Errorf("MR 2 sha = %q, want empty (source reported none)", got)
	}
}

func TestPoller_DegradedModeRetainsLastSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	src := newFakeSource()
	src.set("services/loom-core", []MRInfo{
		{IID: 1, State: "opened", SourceBranch: "feat/a", UpdatedAt: now.Add(-time.Hour),
			MergeWhenPipelineSucceeds: true, Pipeline: &PipelineInfo{Status: "success"}},
	})

	p := newTestPoller(src, now, "services/loom-core")
	p.pollOnce(context.Background())
	good := p.Snapshot()
	if len(good.MergeRequests) != 1 || good.Stale {
		t.Fatalf("precondition failed: %+v", good)
	}

	// GitLab becomes unreachable.
	src.fail("services/loom-core", errors.New("dial tcp: connection refused"))
	p.pollOnce(context.Background())
	degraded := p.Snapshot()

	if len(degraded.MergeRequests) != 1 {
		t.Errorf("degraded poll must retain last MRs, got %d", len(degraded.MergeRequests))
	}
	if !degraded.Stale {
		t.Error("degraded poll must mark snapshot stale")
	}
	if !degraded.LastPollAt.Equal(now) {
		t.Errorf("degraded LastPollAt should stay at last good poll %v, got %v", now, degraded.LastPollAt)
	}
}

func TestPoller_TransitionTimestampMovesOnlyOnChange(t *testing.T) {
	t1 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	src := newFakeSource()
	src.set("r", []MRInfo{
		{Repo: "r", IID: 1, State: "opened", SourceBranch: "b", UpdatedAt: t1,
			MergeWhenPipelineSucceeds: true, Pipeline: &PipelineInfo{Status: "success"}},
	})

	nowVal := t1
	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute, Now: func() time.Time { return nowVal }})

	p.pollOnce(context.Background())
	first := p.Snapshot().MergeRequests[0].LastTransitionAt
	if !first.Equal(t1) {
		t.Fatalf("first transition = %v, want %v", first, t1)
	}

	// Poll again later with the SAME state — transition time must not move.
	nowVal = t1.Add(10 * time.Minute)
	p.pollOnce(context.Background())
	same := p.Snapshot().MergeRequests[0].LastTransitionAt
	if !same.Equal(t1) {
		t.Errorf("unchanged state moved transition time to %v, want %v", same, t1)
	}

	// Now the pipeline fails — state changes, transition time advances.
	src.set("r", []MRInfo{
		{Repo: "r", IID: 1, State: "opened", SourceBranch: "b", UpdatedAt: t1,
			MergeWhenPipelineSucceeds: true, Pipeline: &PipelineInfo{Status: "failed"}},
	})
	nowVal = t1.Add(20 * time.Minute)
	p.pollOnce(context.Background())
	changed := p.Snapshot().MergeRequests[0].LastTransitionAt
	if !changed.Equal(nowVal) {
		t.Errorf("changed state transition = %v, want %v", changed, nowVal)
	}
}

func TestPoller_MultiProjectPartialFailure(t *testing.T) {
	now := time.Now()
	src := newFakeSource()
	src.set("repo/a", []MRInfo{
		{IID: 1, State: "opened", SourceBranch: "x", UpdatedAt: now, Pipeline: &PipelineInfo{Status: "success"}},
	})
	src.fail("repo/b", errors.New("boom"))

	p := newTestPoller(src, now, "repo/a", "repo/b")
	p.pollOnce(context.Background())
	snap := p.Snapshot()

	if len(snap.MergeRequests) != 1 {
		t.Errorf("want 1 MR from healthy project, got %d", len(snap.MergeRequests))
	}
	if !snap.Stale {
		t.Error("partial failure must mark snapshot stale")
	}
	if len(snap.Projects) != 2 {
		t.Errorf("want 2 watched projects, got %d", len(snap.Projects))
	}
}

func TestPoller_NilSafetyEmptySnapshotEncodesArrays(t *testing.T) {
	// A nil poller must still yield a valid, non-null snapshot.
	var p *Poller
	snap := p.Snapshot()
	assertEncodesArrays(t, snap)

	// A configured-but-never-polled poller too.
	p2 := newTestPoller(newFakeSource(), time.Now(), "services/loom-core")
	assertEncodesArrays(t, p2.Snapshot())
}

func TestPoller_EmptyPollEncodesArraysNotNull(t *testing.T) {
	now := time.Now()
	src := newFakeSource()
	src.set("services/loom-core", nil) // polled fine, zero MRs
	p := newTestPoller(src, now, "services/loom-core")
	p.pollOnce(context.Background())
	assertEncodesArrays(t, p.Snapshot())
}

func assertEncodesArrays(t *testing.T, snap Snapshot) {
	t.Helper()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"merge_requests", "counts", "projects"} {
		v, ok := raw[key]
		if !ok {
			t.Errorf("missing key %q in %s", key, b)
			continue
		}
		if string(v) == "null" {
			t.Errorf("key %q encoded as null (want [] or {}): %s", key, b)
		}
	}
}

func TestNormalizeProjects(t *testing.T) {
	got := normalizeProjects([]string{" b ", "a", "b", "", "a"})
	want := []string{"a", "b"}
	if len(got) != len(want) || got[0] != "a" || got[1] != "b" {
		t.Fatalf("normalizeProjects = %v, want %v", got, want)
	}
}

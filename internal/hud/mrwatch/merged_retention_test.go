package mrwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// mergedFakeSource is a fakeSource that also implements MergedLister, so a test
// can drive the merged-marker path without a GitLab client. Merged results are
// returned verbatim; the `since` bound each poll passes is recorded so the
// window handed to the API can be asserted.
type mergedFakeSource struct {
	mu     sync.Mutex
	open   map[string][]MRInfo
	merged map[string][]MRInfo
	err    error
	since  []time.Time
	calls  int
}

func newMergedFakeSource() *mergedFakeSource {
	return &mergedFakeSource{
		open:   map[string][]MRInfo{},
		merged: map[string][]MRInfo{},
	}
}

func (f *mergedFakeSource) setOpen(project string, mrs []MRInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open[project] = mrs
}

func (f *mergedFakeSource) setMerged(project string, mrs []MRInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged[project] = mrs
}

func (f *mergedFakeSource) failMerged(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *mergedFakeSource) ListOpenMRs(_ context.Context, project string) ([]MRInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[project], nil
}

func (f *mergedFakeSource) ListMergedMRs(_ context.Context, project string, since time.Time) ([]MRInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.since = append(f.since, since)
	if f.err != nil {
		return nil, f.err
	}
	return f.merged[project], nil
}

func mergedSnapshotEntry(t *testing.T, snap Snapshot, iid int64) (MergeRequest, bool) {
	t.Helper()
	for _, mr := range snap.MergeRequests {
		if mr.IID == iid {
			return mr, true
		}
	}
	return MergeRequest{}, false
}

// TestPoller_MergedMarkerSurvivesOpenListDisappearance is the whole point of the
// slice: once an MR merges it must stay in the registry with an explicit merged
// state instead of silently vanishing, which is indistinguishable from "never
// existed" and from a degraded registry.
func TestPoller_MergedMarkerSurvivesOpenListDisappearance(t *testing.T) {
	t0 := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	nowVal := t0
	src := newMergedFakeSource()
	src.setOpen("r", []MRInfo{
		{IID: 7, State: "opened", SourceBranch: "feat/ship", UpdatedAt: t0,
			MergeWhenPipelineSucceeds: true, Pipeline: &PipelineInfo{Status: "success"}},
	})

	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute,
		Now: func() time.Time { return nowVal }})
	p.pollOnce(context.Background())
	if mr, ok := mergedSnapshotEntry(t, p.Snapshot(), 7); !ok || mr.State != StateOK {
		t.Fatalf("precondition: MR 7 = %+v (ok=%v), want state ok", mr, ok)
	}

	// The MR merges: it leaves the open list and appears on the merged list.
	mergedAt := t0.Add(5 * time.Minute)
	nowVal = t0.Add(10 * time.Minute)
	src.setOpen("r", nil)
	src.setMerged("r", []MRInfo{
		{IID: 7, State: "merged", SourceBranch: "feat/ship", MergedAt: mergedAt, UpdatedAt: mergedAt},
	})
	p.pollOnce(context.Background())

	mr, ok := mergedSnapshotEntry(t, p.Snapshot(), 7)
	if !ok {
		t.Fatal("merged MR dropped from snapshot; the truth sweep would see nothing")
	}
	if mr.State != StateMerged {
		t.Errorf("state = %q, want %q", mr.State, StateMerged)
	}
	if !mr.Merged {
		t.Error("merged flag = false, want true (the explicit positive marker)")
	}
	if !mr.MergedAt.Equal(mergedAt) {
		t.Errorf("merged_at = %v, want %v", mr.MergedAt, mergedAt)
	}
	if mr.Stale {
		t.Error("a merged MR must never be flagged stale")
	}

	// GitLab stops returning it (e.g. the merged page moved on) — retention
	// keeps the marker anyway, which is what makes it restart/outage tolerant.
	nowVal = t0.Add(2 * time.Hour)
	src.setMerged("r", nil)
	p.pollOnce(context.Background())
	if mr, ok := mergedSnapshotEntry(t, p.Snapshot(), 7); !ok || mr.State != StateMerged {
		t.Fatalf("retained merged MR = %+v (ok=%v), want state merged", mr, ok)
	}
	if got := p.Snapshot().Counts[string(StateMerged)]; got != 1 {
		t.Errorf("merged count = %d, want 1", got)
	}
}

// TestPoller_MergedRetentionExpires: the window is bounded — past it, the merged
// entry leaves the snapshot and its retention record is released.
func TestPoller_MergedRetentionExpires(t *testing.T) {
	t0 := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	nowVal := t0
	src := newMergedFakeSource()
	src.setMerged("r", []MRInfo{
		{IID: 7, State: "merged", SourceBranch: "feat/ship", MergedAt: t0},
	})

	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute,
		MergedRetention: time.Hour, Now: func() time.Time { return nowVal }})
	p.pollOnce(context.Background())
	if _, ok := mergedSnapshotEntry(t, p.Snapshot(), 7); !ok {
		t.Fatal("precondition: merged MR should be retained inside the window")
	}

	// Still inside the window.
	nowVal = t0.Add(59 * time.Minute)
	src.setMerged("r", nil)
	p.pollOnce(context.Background())
	if _, ok := mergedSnapshotEntry(t, p.Snapshot(), 7); !ok {
		t.Error("merged MR expired early (59m into a 1h window)")
	}

	// Past the window.
	nowVal = t0.Add(time.Hour + time.Minute)
	p.pollOnce(context.Background())
	if _, ok := mergedSnapshotEntry(t, p.Snapshot(), 7); ok {
		t.Error("merged MR outlived its retention window")
	}
	p.mu.RLock()
	retained := len(p.merged)
	p.mu.RUnlock()
	if retained != 0 {
		t.Errorf("retention map holds %d expired entries, want 0", retained)
	}
	if got := p.Snapshot().Counts[string(StateMerged)]; got != 0 {
		t.Errorf("merged count = %d, want 0", got)
	}
}

// TestPoller_MergedRetentionDisabled: a negative retention restores the original
// behavior — merged MRs are dropped on sight and never listed.
func TestPoller_MergedRetentionDisabled(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	src := newMergedFakeSource()
	src.setMerged("r", []MRInfo{
		{IID: 7, State: "merged", SourceBranch: "feat/ship", MergedAt: now},
	})
	src.setOpen("r", []MRInfo{
		{IID: 8, State: "merged", SourceBranch: "feat/other", MergedAt: now},
	})

	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute,
		MergedRetention: -1, Now: func() time.Time { return now }})
	p.pollOnce(context.Background())

	if got := len(p.Snapshot().MergeRequests); got != 0 {
		t.Errorf("snapshot holds %d MRs, want 0 with retention disabled", got)
	}
	if src.calls != 0 {
		t.Errorf("merged lister called %d times, want 0 with retention disabled", src.calls)
	}
}

// TestPoller_MergedRetentionCountCapped: the retained set is bounded by count as
// well as age, so a repo merging faster than the window cannot grow the
// registry without limit. The newest merges win.
func TestPoller_MergedRetentionCountCapped(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	src := newMergedFakeSource()

	total := maxRetainedMerged + 25
	infos := make([]MRInfo, 0, total)
	for i := range total {
		infos = append(infos, MRInfo{
			IID:          int64(i + 1),
			State:        "merged",
			SourceBranch: "feat/x",
			// Oldest first: IID 1 merged longest ago, so the low IIDs evict.
			MergedAt: now.Add(-time.Duration(total-i) * time.Minute),
		})
	}
	src.setMerged("r", infos)

	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute,
		MergedRetention: 30 * 24 * time.Hour, Now: func() time.Time { return now }})
	p.pollOnce(context.Background())

	snap := p.Snapshot()
	if len(snap.MergeRequests) != maxRetainedMerged {
		t.Fatalf("snapshot holds %d merged MRs, want the cap %d", len(snap.MergeRequests), maxRetainedMerged)
	}
	p.mu.RLock()
	retained := len(p.merged)
	p.mu.RUnlock()
	if retained != maxRetainedMerged {
		t.Errorf("retention map holds %d entries, want the cap %d", retained, maxRetainedMerged)
	}
	// The oldest merge must be the one evicted, the newest must survive.
	if _, ok := mergedSnapshotEntry(t, snap, 1); ok {
		t.Error("oldest merge retained past the cap")
	}
	if _, ok := mergedSnapshotEntry(t, snap, int64(total)); !ok {
		t.Error("newest merge evicted by the cap")
	}
}

// TestPoller_MergedListerFailureDoesNotDegrade: a failed merged listing means a
// missing marker, which is fail-closed for consumers. It must not flip the
// snapshot to stale — that would misreport freshly-polled open MRs as retained.
func TestPoller_MergedListerFailureDoesNotDegrade(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	src := newMergedFakeSource()
	src.setOpen("r", []MRInfo{
		{IID: 1, State: "opened", SourceBranch: "feat/a", UpdatedAt: now,
			MergeWhenPipelineSucceeds: true, Pipeline: &PipelineInfo{Status: "success"}},
	})
	src.failMerged(errors.New("500 internal server error"))

	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute,
		Now: func() time.Time { return now }})
	p.pollOnce(context.Background())

	snap := p.Snapshot()
	if snap.Stale {
		t.Error("merged-list failure marked the whole snapshot stale")
	}
	if !snap.LastPollAt.Equal(now) {
		t.Errorf("LastPollAt = %v, want %v (the open poll succeeded)", snap.LastPollAt, now)
	}
	if len(snap.MergeRequests) != 1 {
		t.Errorf("open MRs = %d, want 1", len(snap.MergeRequests))
	}
}

// TestPoller_MergedListerWindowBound: the poller asks GitLab only for merges
// inside the retention window, so the API page cannot be dominated by history
// the registry would immediately expire.
func TestPoller_MergedListerWindowBound(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	src := newMergedFakeSource()
	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute,
		MergedRetention: 6 * time.Hour, Now: func() time.Time { return now }})
	p.pollOnce(context.Background())

	if len(src.since) != 1 {
		t.Fatalf("merged lister called %d times, want 1", len(src.since))
	}
	if want := now.Add(-6 * time.Hour); !src.since[0].Equal(want) {
		t.Errorf("since = %v, want %v", src.since[0], want)
	}
}

// TestPoller_MergedWinsOverStaleOpenView: if a poll sees the same MR as both
// open and merged, the terminal state wins and the MR is reported once.
func TestPoller_MergedWinsOverStaleOpenView(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	src := newMergedFakeSource()
	src.setOpen("r", []MRInfo{
		{IID: 7, State: "opened", SourceBranch: "feat/ship", UpdatedAt: now,
			Pipeline: &PipelineInfo{Status: "success"}}, // would be automerge_unarmed
	})
	src.setMerged("r", []MRInfo{
		{IID: 7, State: "merged", SourceBranch: "feat/ship", MergedAt: now},
	})

	p := NewPoller(src, Options{Projects: []string{"r"}, Interval: time.Minute,
		Now: func() time.Time { return now }})
	p.pollOnce(context.Background())

	snap := p.Snapshot()
	if len(snap.MergeRequests) != 1 {
		t.Fatalf("snapshot holds %d entries for one MR, want 1", len(snap.MergeRequests))
	}
	if snap.MergeRequests[0].State != StateMerged {
		t.Errorf("state = %q, want %q", snap.MergeRequests[0].State, StateMerged)
	}
	if snap.Counts[string(StateAutomergeUnarmed)] != 0 {
		t.Error("stale open view still counted after the merge")
	}
}

// TestMergedRetentionFromEnv covers the operator surface: default, override, and
// the explicit "off" values.
func TestMergedRetentionFromEnv(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		wantOff bool
	}{
		{raw: "", want: DefaultMergedRetention},
		{raw: "24h", want: 24 * time.Hour},
		{raw: "not-a-duration", want: DefaultMergedRetention},
		{raw: "0", wantOff: true},
		{raw: "-5m", wantOff: true},
	} {
		t.Setenv(EnvMergedRetention, tc.raw)
		got := MergedRetentionFromEnv(nil)
		if tc.wantOff {
			if got > 0 {
				t.Errorf("%q → %v, want a disabling (non-positive) duration", tc.raw, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%q → %v, want %v", tc.raw, got, tc.want)
		}
	}
}

package mills

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeRegressionSource serves a fixed merged-MR list and commit list, counting
// calls so a disabled sweep is provable (zero calls) rather than merely quiet.
type fakeRegressionSource struct {
	mu           sync.Mutex
	merged       []MergedMRRecord
	commits      []BranchCommitRecord
	mergedErr    error
	commitsErr   error
	mergedCalls  int
	commitCalls  int
	lastRef      string
	lastSince    time.Time
	lastMRsSince time.Time
}

func (f *fakeRegressionSource) ListMergedMRs(_ context.Context, since time.Time, _ int) ([]MergedMRRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergedCalls++
	f.lastMRsSince = since
	if f.mergedErr != nil {
		return nil, f.mergedErr
	}
	return append([]MergedMRRecord(nil), f.merged...), nil
}

func (f *fakeRegressionSource) ListBranchCommits(_ context.Context, ref string, since time.Time, _ int) ([]BranchCommitRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCalls++
	f.lastRef = ref
	f.lastSince = since
	if f.commitsErr != nil {
		return nil, f.commitsErr
	}
	return append([]BranchCommitRecord(nil), f.commits...), nil
}

func (f *fakeRegressionSource) calls() (merged, commits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mergedCalls, f.commitCalls
}

func wireRegressionSource(rec *Reconciler, src *fakeRegressionSource) {
	rec.RegressionMergedMRs = src
	rec.RegressionCommits = src
}

const (
	regressionTestSHA      = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	regressionTestOtherSHA = "a1b2c3d4e5f6ffffffffffffffffffffffffffff"
)

func revertCommit(sha, trailerSHA string) BranchCommitRecord {
	return BranchCommitRecord{
		SHA:     sha,
		Title:   `Revert "feat(mills): thing"`,
		Message: "Revert \"feat(mills): thing\"\n\nThis reverts commit " + trailerSHA + ".\n",
	}
}

// TestRegressionSweepRevertTrailerMatching pins the matcher's contract: a full
// SHA and an unambiguous >=12-character prefix attribute; prose that merely
// mentions the SHA, a too-short prefix, and an ambiguous prefix do not.
func TestRegressionSweepRevertTrailerMatching(t *testing.T) {
	merged := []MergedMRRecord{{IID: 1421, Title: "feat(mills): thing", LandedSHA: regressionTestSHA}}

	cases := []struct {
		name       string
		merged     []MergedMRRecord
		commit     BranchCommitRecord
		wantAttrib int
		wantAmbig  int
	}{
		{
			name:       "full sha trailer",
			merged:     merged,
			commit:     revertCommit("ff00000000000000000000000000000000000001", regressionTestSHA),
			wantAttrib: 1,
		},
		{
			name:       "twelve character prefix",
			merged:     merged,
			commit:     revertCommit("ff00000000000000000000000000000000000002", regressionTestSHA[:12]),
			wantAttrib: 1,
		},
		{
			name:   "prose mention is not a revert",
			merged: merged,
			commit: BranchCommitRecord{
				SHA:     "ff00000000000000000000000000000000000003",
				Title:   "docs: note the rollback discussion",
				Message: "docs: note the rollback discussion\n\nWe may want to revert commit " + regressionTestSHA + " if the alert recurs.\n",
			},
		},
		{
			name:      "short prefix is refused",
			merged:    merged,
			commit:    revertCommit("ff00000000000000000000000000000000000004", regressionTestSHA[:8]),
			wantAmbig: 1,
		},
		{
			name:      "ambiguous prefix is refused",
			merged:    append([]MergedMRRecord{{IID: 1422, Title: "feat: other", LandedSHA: regressionTestOtherSHA}}, merged...),
			commit:    revertCommit("ff00000000000000000000000000000000000005", regressionTestSHA[:12]),
			wantAmbig: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecEnv(t, nil)
			src := &fakeRegressionSource{merged: tc.merged, commits: []BranchCommitRecord{tc.commit}}
			wireRegressionSource(env.rec, src)

			res, err := env.rec.SweepRegressionAttribution(context.Background())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if res.Attributed != tc.wantAttrib {
				t.Errorf("attributed = %d, want %d (%+v)", res.Attributed, tc.wantAttrib, res)
			}
			if res.Ambiguous != tc.wantAmbig {
				t.Errorf("ambiguous = %d, want %d (%+v)", res.Ambiguous, tc.wantAmbig, res)
			}

			ev, err := env.store.Events.FirstBySubjectKind(context.Background(),
				"merge_request", "1421", RegressionAttributedEventKind)
			if tc.wantAttrib == 0 {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("want no attribution event, got %+v (err %v)", ev, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read attribution event: %v", err)
			}
			if ev.Actor != RegressionAttributionActor {
				t.Errorf("actor = %q, want %q", ev.Actor, RegressionAttributionActor)
			}
			if got, want := ev.Payload["merged_sha"], regressionTestSHA; got != want {
				t.Errorf("merged_sha = %v, want %v", got, want)
			}
			if got, want := ev.Payload["revert_sha"], tc.commit.SHA; got != want {
				t.Errorf("revert_sha = %v, want %v", got, want)
			}
			if got, want := ev.Payload["revert_title"], tc.commit.Title; got != want {
				t.Errorf("revert_title = %v, want %v", got, want)
			}
		})
	}
}

// TestRegressionSweepAttributesExactlyOnce proves a second pass over the same
// window neither writes a duplicate event nor re-increments the counter — the
// window is bounded by wall-clock time, so re-observation is the normal case.
func TestRegressionSweepAttributesExactlyOnce(t *testing.T) {
	env := newRecEnv(t, nil)
	src := &fakeRegressionSource{
		merged:  []MergedMRRecord{{IID: 1421, LandedSHA: regressionTestSHA}},
		commits: []BranchCommitRecord{revertCommit("ff00000000000000000000000000000000000001", regressionTestSHA)},
	}
	wireRegressionSource(env.rec, src)

	before := testutil.ToFloat64(RegressionAttributionsTotal)
	first, err := env.rec.SweepRegressionAttribution(context.Background())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if first.Attributed != 1 {
		t.Fatalf("first sweep attributed = %d, want 1", first.Attributed)
	}
	second, err := env.rec.SweepRegressionAttribution(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if second.Attributed != 0 {
		t.Errorf("second sweep attributed = %d, want 0 (%+v)", second.Attributed, second)
	}
	if second.Reverts != 1 {
		t.Errorf("second sweep reverts = %d, want 1 — the revert is still observed", second.Reverts)
	}
	if got := testutil.ToFloat64(RegressionAttributionsTotal) - before; got != 1 {
		t.Errorf("attribution counter delta = %v, want 1", got)
	}

	events, err := env.store.Events.ListByActorSince(context.Background(),
		RegressionAttributionActor, time.Now().UTC().Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("attribution events = %d, want exactly 1", len(events))
	}
}

// TestRegressionSweepDisabledWithoutClient: the sweep is off unless BOTH list
// clients are wired, and a disabled sweep makes no calls and no error.
func TestRegressionSweepDisabledWithoutClient(t *testing.T) {
	t.Run("no clients", func(t *testing.T) {
		env := newRecEnv(t, nil)
		res, err := env.rec.SweepRegressionAttribution(context.Background())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res != (RegressionSweepResult{}) {
			t.Errorf("result = %+v, want zero value", res)
		}
		if env.rec.regressionSweepDue(env.now) {
			t.Error("sweep reported due with no clients wired")
		}
	})

	t.Run("commit lister only", func(t *testing.T) {
		env := newRecEnv(t, nil)
		src := &fakeRegressionSource{
			merged:  []MergedMRRecord{{IID: 1421, LandedSHA: regressionTestSHA}},
			commits: []BranchCommitRecord{revertCommit("ff00000000000000000000000000000000000001", regressionTestSHA)},
		}
		env.rec.RegressionCommits = src

		res, err := env.rec.SweepRegressionAttribution(context.Background())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Attributed != 0 {
			t.Errorf("attributed = %d, want 0", res.Attributed)
		}
		if merged, commits := src.calls(); merged != 0 || commits != 0 {
			t.Errorf("calls = (merged %d, commits %d), want (0, 0)", merged, commits)
		}
	})
}

// TestRegressionSweepWindowsAndBranch pins the two list calls to the same
// lookback window and to the configured branch.
func TestRegressionSweepWindowsAndBranch(t *testing.T) {
	env := newRecEnv(t, nil)
	src := &fakeRegressionSource{
		merged:  []MergedMRRecord{{IID: 1421, LandedSHA: regressionTestSHA}},
		commits: []BranchCommitRecord{revertCommit("ff00000000000000000000000000000000000001", regressionTestSHA)},
	}
	wireRegressionSource(env.rec, src)
	env.rec.RegressionBranch = "trunk"
	env.rec.RegressionLookback = 48 * time.Hour

	if _, err := env.rec.SweepRegressionAttribution(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	wantSince := env.now.Add(-48 * time.Hour)
	if !src.lastSince.Equal(wantSince) || !src.lastMRsSince.Equal(wantSince) {
		t.Errorf("since = (mrs %s, commits %s), want %s", src.lastMRsSince, src.lastSince, wantSince)
	}
	if src.lastRef != "trunk" {
		t.Errorf("ref = %q, want %q", src.lastRef, "trunk")
	}
}

// TestRegressionSweepListFailureIsBestEffort: a GitLab list failure surfaces as
// an error for the caller to log, never as a partial attribution.
func TestRegressionSweepListFailureIsBestEffort(t *testing.T) {
	env := newRecEnv(t, nil)
	src := &fakeRegressionSource{mergedErr: errors.New("gitlab 502")}
	wireRegressionSource(env.rec, src)

	res, err := env.rec.SweepRegressionAttribution(context.Background())
	if err == nil {
		t.Fatal("want an error from a failing merged-MR list")
	}
	if res.Attributed != 0 {
		t.Errorf("attributed = %d, want 0", res.Attributed)
	}
	if _, commits := src.calls(); commits != 0 {
		t.Errorf("commit list called %d times after the merged list failed", commits)
	}
}

// TestRegressionSweepDueRespectsInterval: the reconciler rate-limits the sweep
// rather than running it on every tick.
func TestRegressionSweepDueRespectsInterval(t *testing.T) {
	env := newRecEnv(t, nil)
	wireRegressionSource(env.rec, &fakeRegressionSource{})
	env.rec.RegressionSweepInterval = 30 * time.Minute

	if !env.rec.regressionSweepDue(env.now) {
		t.Fatal("first sweep must be due immediately")
	}
	env.rec.nextRegressionSweep = env.now.Add(env.rec.regressionSweepInterval())
	if env.rec.regressionSweepDue(env.now.Add(29 * time.Minute)) {
		t.Error("sweep due before the interval elapsed")
	}
	if !env.rec.regressionSweepDue(env.now.Add(30 * time.Minute)) {
		t.Error("sweep not due after the interval elapsed")
	}
}

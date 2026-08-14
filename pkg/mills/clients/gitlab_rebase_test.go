package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// ----- fake GitLab observation surface -----
//
// The transport routes on a path PREFIX, so the MR detail, versions, and
// activity-feed endpoints must be dispatched from one handler; matching them
// as separate prefix routes would race Go's random map iteration order.

const (
	obsReviewedSHA  = "aa2274784bd1269d79830aeaf2ae081e7508c4a4"
	obsSuccessorSHA = "bb13c0ffee0000000000000000000000deadbeef"
	obsThirdSHA     = "cc99c0ffee0000000000000000000000feedface"
	obsSourceBranch = "feat/head-transitions"
	obsMRPath       = "GET /api/v4/projects/services%2Floom-core/merge_requests/1214"
	obsEventsPath   = "GET /api/v4/projects/services%2Floom-core/events"
)

// obsMR is the shape GET .../merge_requests/:iid?include_rebase_in_progress=true
// returns (field names verified live against GitLab 18.4.3 CE, 2026-07-25).
func obsMR(sha string, rebaseInProgress bool, mergeError string) map[string]any {
	return map[string]any{
		"iid":                1214,
		"state":              "opened",
		"sha":                sha,
		"source_branch":      obsSourceBranch,
		"target_branch":      testTargetBranch,
		"rebase_in_progress": rebaseInProgress,
		"merge_error":        mergeError,
		"diff_refs": map[string]any{
			"base_sha":  "69b31838013cdfe6f3f054843055f7a49f5912d2",
			"head_sha":  sha,
			"start_sha": "6cbcf534f10ac06a971cfec8bbc49aee534717fc",
		},
	}
}

func obsVersion(id int64, head string) map[string]any {
	return map[string]any{
		"id":               id,
		"head_commit_sha":  head,
		"base_commit_sha":  "69b31838013cdfe6f3f054843055f7a49f5912d2",
		"start_commit_sha": "6cbcf534f10ac06a971cfec8bbc49aee534717fc",
		"created_at":       "2026-07-25T14:54:52.422Z",
		"state":            "collected",
	}
}

func obsPush(id int64, from, to, ref string) map[string]any {
	return map[string]any{
		"id":              id,
		"action_name":     "pushed to",
		"created_at":      "2026-07-25T14:54:50.779Z",
		"author_username": "root",
		"push_data": map[string]any{
			"commit_count": 1,
			"action":       "pushed",
			"ref_type":     "branch",
			"ref":          ref,
			"commit_from":  from,
			"commit_to":    to,
		},
	}
}

// obsFixture scripts one observation scenario.
type obsFixture struct {
	// mrs is the sequence of MR bodies returned by successive GETs; the last
	// entry repeats once exhausted.
	mrs      []map[string]any
	versions []map[string]any
	pushes   []map[string]any
	mrCalls  int
}

func newObserveStub(t *testing.T, f *obsFixture) *GitLabClient {
	t.Helper()
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		obsMRPath: func(r *http.Request) (int, any) {
			path := r.URL.Path
			if r.URL.RawPath != "" {
				path = r.URL.RawPath
			}
			switch {
			case strings.HasSuffix(path, "/versions"):
				if f.versions == nil {
					return 200, []map[string]any{}
				}
				return 200, f.versions
			default:
				// The client MUST ask for include_rebase_in_progress: the field
				// is absent from the body without it, and a missing field
				// would decode as "no rebase running" on every poll.
				if r.URL.Query().Get("include_rebase_in_progress") != "true" {
					t.Errorf("MR GET missing include_rebase_in_progress=true: %s", r.URL.String())
				}
				i := f.mrCalls
				f.mrCalls++
				if i >= len(f.mrs) {
					i = len(f.mrs) - 1
				}
				return 200, f.mrs[i]
			}
		},
		obsEventsPath: func(r *http.Request) (int, any) {
			if r.URL.Query().Get("action") != "pushed" {
				t.Errorf("events GET must filter action=pushed: %s", r.URL.String())
			}
			if f.pushes == nil {
				return 200, []map[string]any{}
			}
			return 200, f.pushes
		},
	})
	_ = rt
	cli.headObserveInterval = 5 * time.Millisecond
	return cli
}

func observeRequest() pipeline.HeadObservationRequest {
	return pipeline.HeadObservationRequest{
		Project:        testGitLabProject,
		MRIID:          1214,
		SourceBranch:   obsSourceBranch,
		TargetBranch:   testTargetBranch,
		ReviewedSHA:    obsReviewedSHA,
		VersionsCursor: 15318,
		EventsCursor:   34901,
		SettleDeadline: 200 * time.Millisecond,
	}
}

func mustObserve(t *testing.T, cli *GitLabClient) pipeline.HeadObservation {
	t.Helper()
	obs, err := cli.ObserveHead(context.Background(), observeRequest())
	if err != nil {
		t.Fatalf("observe head: %v", err)
	}
	return obs
}

// ----- §5.3 classifier cases -----

// (a) One version row and one corroborating push off the reviewed SHA is the
// only shape that can be attributed.
func TestObserveHead_OneMovementAttributed(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsSuccessorSHA, false, "")},
		versions: []map[string]any{obsVersion(15327, obsSuccessorSHA), obsVersion(15318, obsReviewedSHA)},
		pushes:   []map[string]any{obsPush(34948, obsReviewedSHA, obsSuccessorSHA, obsSourceBranch)},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAttributed {
		t.Fatalf("verdict = %q (%s), want attributed", obs.Verdict, obs.Reason)
	}
	if obs.SuccessorSHA != obsSuccessorSHA {
		t.Errorf("successor = %q, want %q", obs.SuccessorSHA, obsSuccessorSHA)
	}
	if len(obs.Versions) != 1 || obs.Versions[0].ID != 15327 {
		t.Errorf("only rows past the cursor count as evidence: %+v", obs.Versions)
	}
	if len(obs.Pushes) != 1 || obs.Pushes[0].Author != "root" {
		t.Errorf("push evidence = %+v", obs.Pushes)
	}
	prov := obs.Provenance()
	if prov["classifier"] != "attributed" {
		t.Errorf("provenance classifier = %v", prov["classifier"])
	}
	if prov["versions_cursor_before"] != int64(15318) {
		t.Errorf("provenance must record the cursor it measured from: %v", prov["versions_cursor_before"])
	}
}

// (b) Two version rows means something moved the branch besides the one
// replay Mills would have asked for.
func TestObserveHead_TwoMovementsAmbiguous(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs: []map[string]any{obsMR(obsThirdSHA, false, "")},
		versions: []map[string]any{
			obsVersion(15329, obsThirdSHA),
			obsVersion(15327, obsSuccessorSHA),
			obsVersion(15318, obsReviewedSHA),
		},
		pushes: []map[string]any{obsPush(34948, obsReviewedSHA, obsThirdSHA, obsSourceBranch)},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAmbiguous {
		t.Fatalf("verdict = %q, want ambiguous", obs.Verdict)
	}
	if !strings.Contains(obs.Reason, "2 movements") {
		t.Errorf("reason should name the movement count: %q", obs.Reason)
	}
	if len(obs.Versions) != 2 {
		t.Errorf("both new versions must be recorded verbatim: %+v", obs.Versions)
	}
}

// (c) A single push that does not describe the reviewed→successor edge means
// the chain did not start where CI left it.
func TestObserveHead_PushFromUnreviewedSHAAmbiguous(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsSuccessorSHA, false, "")},
		versions: []map[string]any{obsVersion(15327, obsSuccessorSHA)},
		pushes:   []map[string]any{obsPush(34948, obsThirdSHA, obsSuccessorSHA, obsSourceBranch)},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAmbiguous {
		t.Fatalf("verdict = %q (%s), want ambiguous", obs.Verdict, obs.Reason)
	}
	if !strings.Contains(obs.Reason, "reviewed->successor") {
		t.Errorf("reason = %q", obs.Reason)
	}
}

// (d) The activity feed is asynchronous. A version row with no push event yet
// is still sufficient positive evidence — absence of corroboration must not
// downgrade the primary witness.
func TestObserveHead_VersionWithoutPushEventAttributed(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsSuccessorSHA, false, "")},
		versions: []map[string]any{obsVersion(15327, obsSuccessorSHA)},
		pushes:   []map[string]any{},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAttributed {
		t.Fatalf("verdict = %q (%s), want attributed", obs.Verdict, obs.Reason)
	}
	if !strings.Contains(obs.Reason, "activity feed") {
		t.Errorf("reason should name the async feed: %q", obs.Reason)
	}
}

// Pushes on an unrelated branch are not evidence about THIS source branch.
func TestObserveHead_IgnoresPushesOnOtherRefs(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsSuccessorSHA, false, "")},
		versions: []map[string]any{obsVersion(15327, obsSuccessorSHA)},
		pushes: []map[string]any{
			obsPush(34950, "1111111", "2222222", "main"),
			obsPush(34949, "3333333", "4444444", "feat/somebody-else"),
		},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAttributed {
		t.Fatalf("verdict = %q (%s), want attributed", obs.Verdict, obs.Reason)
	}
	if len(obs.Pushes) != 0 {
		t.Errorf("other refs must not enter the evidence bundle: %+v", obs.Pushes)
	}
}

// Rows at or before the cursor are pre-existing history, not new evidence.
func TestObserveHead_CursorExcludesPriorRows(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsSuccessorSHA, false, "")},
		versions: []map[string]any{obsVersion(15318, obsSuccessorSHA)},
		pushes:   []map[string]any{obsPush(34901, obsReviewedSHA, obsSuccessorSHA, obsSourceBranch)},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAmbiguous {
		t.Fatalf("verdict = %q, want ambiguous (all evidence is at or before the cursor)", obs.Verdict)
	}
	if !strings.Contains(obs.Reason, "no version row") {
		t.Errorf("reason = %q", obs.Reason)
	}
}

// (e) noop is decided ONLY by SHA equality on the MR itself, never by an empty
// ledger — a dropped activity feed must not read as "nothing happened".
func TestObserveHead_UnchangedHeadIsNoop(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsReviewedSHA, false, "")},
		versions: []map[string]any{},
		pushes:   []map[string]any{},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictNoop {
		t.Fatalf("verdict = %q (%s), want noop", obs.Verdict, obs.Reason)
	}
	if obs.SuccessorSHA != obsReviewedSHA {
		t.Errorf("successor = %q, want the reviewed sha", obs.SuccessorSHA)
	}
	if obs.Verdict.State() != "noop" {
		t.Errorf("verdict state = %q", obs.Verdict.State())
	}
}

// (f) A head that moved with no witness at all is the dangerous case: the
// design pre-empts a dropped feed being read as a clean rebase.
func TestObserveHead_MovedHeadWithoutWitnessIsAmbiguous(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsSuccessorSHA, false, "")},
		versions: []map[string]any{},
		pushes:   []map[string]any{},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAmbiguous {
		t.Fatalf("verdict = %q, want ambiguous", obs.Verdict)
	}
	if !strings.Contains(obs.Reason, "no version row to witness it") {
		t.Errorf("reason = %q", obs.Reason)
	}
}

// (g) A rebase that never reports done settles ambiguous at the cap. Absence
// of a verdict is never a clean movement.
func TestObserveHead_SettleDeadlineIsAmbiguous(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsReviewedSHA, true, "")},
		versions: []map[string]any{obsVersion(15327, obsSuccessorSHA)},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAmbiguous {
		t.Fatalf("verdict = %q, want ambiguous", obs.Verdict)
	}
	if !strings.Contains(obs.Reason, "still in progress") {
		t.Errorf("reason = %q", obs.Reason)
	}
	if !obs.RebaseInProgress {
		t.Error("observation must record that the rebase was still running")
	}
	if obs.Attempts < 2 {
		t.Errorf("attempts = %d; the settle loop should have polled repeatedly", obs.Attempts)
	}
}

// A rebase that settles with merge_error set is a failure, not an ambiguity.
func TestObserveHead_MergeErrorIsFailed(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs: []map[string]any{
			obsMR(obsReviewedSHA, true, ""),
			obsMR(obsReviewedSHA, false, "Rebase failed. Please rebase locally"),
		},
		versions: []map[string]any{},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictFailed {
		t.Fatalf("verdict = %q (%s), want failed", obs.Verdict, obs.Reason)
	}
	if !strings.Contains(obs.Reason, "Rebase failed") {
		t.Errorf("reason must carry GitLab's merge_error verbatim: %q", obs.Reason)
	}
	if obs.Verdict.State() != "failed" {
		t.Errorf("verdict state = %q", obs.Verdict.State())
	}
}

// A rebase that is still running on the first poll and settles cleanly on a
// later one is the ordinary success path.
func TestObserveHead_PollsUntilRebaseSettles(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs: []map[string]any{
			obsMR(obsReviewedSHA, true, ""),
			obsMR(obsReviewedSHA, true, ""),
			obsMR(obsSuccessorSHA, false, ""),
		},
		versions: []map[string]any{obsVersion(15327, obsSuccessorSHA)},
	})
	obs := mustObserve(t, cli)
	if obs.Verdict != pipeline.HeadVerdictAttributed {
		t.Fatalf("verdict = %q (%s), want attributed", obs.Verdict, obs.Reason)
	}
	if obs.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", obs.Attempts)
	}
	if obs.RebaseInProgress {
		t.Error("settled observation must report rebase_in_progress=false")
	}
}

// ----- guards -----

// An observation must never silently follow a rerouted item into another
// project that happens to hold the same IID.
func TestObserveHead_RefusesForeignProject(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{mrs: []map[string]any{obsMR(obsSuccessorSHA, false, "")}})
	req := observeRequest()
	req.Project = "services/somebody-else"
	_, err := cli.ObserveHead(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "does not match client project") {
		t.Fatalf("err = %v, want a project-binding refusal", err)
	}
}

func TestObserveHead_RequiresReviewedSHA(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{mrs: []map[string]any{obsMR(obsSuccessorSHA, false, "")}})
	req := observeRequest()
	req.ReviewedSHA = ""
	if _, err := cli.ObserveHead(context.Background(), req); err == nil {
		t.Fatal("expected a missing reviewed sha to be refused")
	}
}

// ReadHeadCursors snapshots the positions a later observation measures from.
func TestReadHeadCursors_SnapshotsNewestIDs(t *testing.T) {
	cli := newObserveStub(t, &obsFixture{
		mrs:      []map[string]any{obsMR(obsReviewedSHA, false, "")},
		versions: []map[string]any{obsVersion(15327, obsReviewedSHA), obsVersion(15318, "older")},
		pushes: []map[string]any{
			obsPush(34948, "older", obsReviewedSHA, obsSourceBranch),
			obsPush(34947, "oldest", "older", obsSourceBranch),
			obsPush(34999, "x", "y", "main"), // other ref: not a cursor for us
		},
	})
	cursors, err := cli.ReadHeadCursors(context.Background(), pipeline.HeadCursorRequest{
		Project:      testGitLabProject,
		MRIID:        1214,
		SourceBranch: obsSourceBranch,
	})
	if err != nil {
		t.Fatalf("read cursors: %v", err)
	}
	if cursors.SHA != obsReviewedSHA {
		t.Errorf("sha = %q", cursors.SHA)
	}
	if cursors.VersionsCursor != 15327 {
		t.Errorf("versions cursor = %d, want 15327", cursors.VersionsCursor)
	}
	if cursors.EventsCursor != 34948 {
		t.Errorf("events cursor = %d, want 34948 (other refs excluded)", cursors.EventsCursor)
	}
}

// The observation path is READ-ONLY: #374 slice 1 ships no GitLab mutation at
// all, so a full settle must never issue a PUT/POST/DELETE.
func TestObserveHead_IssuesNoMutations(t *testing.T) {
	f := &obsFixture{
		mrs:      []map[string]any{obsMR(obsSuccessorSHA, false, "")},
		versions: []map[string]any{obsVersion(15327, obsSuccessorSHA)},
		pushes:   []map[string]any{obsPush(34948, obsReviewedSHA, obsSuccessorSHA, obsSourceBranch)},
	}
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		obsMRPath: func(r *http.Request) (int, any) {
			path := r.URL.Path
			if r.URL.RawPath != "" {
				path = r.URL.RawPath
			}
			if strings.HasSuffix(path, "/versions") {
				return 200, f.versions
			}
			return 200, f.mrs[0]
		},
		obsEventsPath: func(*http.Request) (int, any) { return 200, f.pushes },
	})
	cli.headObserveInterval = 5 * time.Millisecond
	if _, err := cli.ObserveHead(context.Background(), observeRequest()); err != nil {
		t.Fatalf("observe: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.requests) == 0 {
		t.Fatal("expected the observation to issue reads")
	}
	for _, req := range rt.requests {
		if req.Method != http.MethodGet {
			t.Errorf("observation issued a %s to %s; the read path must never mutate", req.Method, req.Path)
		}
	}
}

// ----- the typed head-movement error (#374 slice 1 -> runner seam) -----

// A moved head must surface a MergeSourceSHAMismatchError carrying both SHAs
// so the runner can mint a ledger row without scraping the message. Everything
// else about the error must be unchanged: it still wraps
// ErrMergeAuthorizationStale, and its text is byte-identical to the historical
// form that error-class needles and operator escalations read.
func TestMerge_MovedHeadReturnsTypedMismatchWithBothSHAs(t *testing.T) {
	putCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/30": func(_ *http.Request) (int, any) {
			return 200, openedMR(30, obsSuccessorSHA)
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/30/merge": func(_ *http.Request) (int, any) {
			putCalls++
			return 200, mergedMR(30, obsSuccessorSHA, "cab005e")
		},
	})

	_, err := cli.Merge(context.Background(), testMergeArgs(30, obsReviewedSHA))
	if err == nil {
		t.Fatal("expected a moved head to refuse the merge")
	}
	if putCalls != 0 {
		t.Errorf("merge PUTs = %d, want 0 - the identity check must refuse before any mutation", putCalls)
	}
	if !errors.Is(err, pipeline.ErrMergeAuthorizationStale) {
		t.Errorf("err must still wrap ErrMergeAuthorizationStale: %v", err)
	}
	var mismatch *pipeline.MergeSourceSHAMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %T, want *pipeline.MergeSourceSHAMismatchError", err)
	}
	if mismatch.ReviewedSHA != obsReviewedSHA {
		t.Errorf("reviewed sha = %q, want %q", mismatch.ReviewedSHA, obsReviewedSHA)
	}
	if mismatch.ObservedSHA != obsSuccessorSHA {
		t.Errorf("observed sha = %q, want %q", mismatch.ObservedSHA, obsSuccessorSHA)
	}
	if mismatch.MRIID != 30 || mismatch.Project != testGitLabProject {
		t.Errorf("identity = %+v", mismatch)
	}
	if mismatch.SourceBranch != testSourceBranch || mismatch.TargetBranch != testTargetBranch {
		t.Errorf("branch identity = %+v", mismatch)
	}

	want := fmt.Sprintf("mr 30 source sha %q no longer matches CI-authorized source sha %q: %s",
		obsSuccessorSHA, obsReviewedSHA, pipeline.ErrMergeAuthorizationStale)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error text drifted from the historical form:\n got %q\nwant %q contained", err.Error(), want)
	}
}

// A BRANCH mismatch is a routing defect, not a head movement, and must NOT
// present as one - otherwise a reroute would mint a ledger row, burn the
// transition budget, and rewind a run that has nothing to re-gate.
func TestMerge_BranchMismatchIsNotAHeadMovement(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/31": func(_ *http.Request) (int, any) {
			return 200, openedMR(31, obsReviewedSHA, "feat/somewhere-else")
		},
	})
	_, err := cli.Merge(context.Background(), testMergeArgs(31, obsReviewedSHA))
	if err == nil {
		t.Fatal("expected a source-branch mismatch to refuse the merge")
	}
	if !errors.Is(err, pipeline.ErrMergeAuthorizationStale) {
		t.Errorf("err must wrap ErrMergeAuthorizationStale: %v", err)
	}
	var mismatch *pipeline.MergeSourceSHAMismatchError
	if errors.As(err, &mismatch) {
		t.Errorf("a branch mismatch must not present as a head movement: %+v", mismatch)
	}
}

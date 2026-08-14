package clients

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// The stale-405 close+reopen recovery clears the one merge failure nothing else
// can: GitLab refusing the merge PUT while its own head_pipeline / mergeability
// state is stale. Live shape (loom-core!1249, 2026-07-26): head_pipeline source
// "push" and status "success", merge_status can_be_merged, and PUT .../merge
// still 405s. The superseding-pipeline path declines that shape by design (it
// only owns merge_request_event/api heads), so before this recovery the stage
// burned its settle window on identical PUTs and escalated as terminal config.

// stale405MR is the live !1249 shape: an open MR whose head pipeline came from
// an ordinary branch push and is already green.
func stale405MR(iid int64, sha, sourceBranch string, headPipelineID int64, detailed string) mrResponse {
	mr := openedMR(iid, sha, sourceBranch)
	mr.HeadPipeline = mrHeadPipe{ID: headPipelineID, Status: "success", Source: "push"}
	mr.MergeStatus = "can_be_merged"
	mr.DetailedMergeStatus = detailed
	return mr
}

func greenPipelineList(sha, ref string, id int64) []shaPipeline {
	return []shaPipeline{{ID: id, SHA: sha, Ref: ref, Status: "success", Source: "push"}}
}

// inFlightStateEvent reports the state_event of the request a stub route is
// currently serving. recordingTransport drains req.Body to record it, so a
// route handler cannot decode the body itself; it reads the recorded copy
// instead. Safe to call from a handler: the transport already holds its lock
// and has appended the in-flight request.
func inFlightStateEvent(rt *recordingTransport) string {
	if rt == nil || len(rt.requests) == 0 {
		return ""
	}
	var body struct {
		StateEvent string `json:"state_event"`
	}
	_ = json.Unmarshal([]byte(rt.requests[len(rt.requests)-1].Body), &body)
	return body.StateEvent
}

// mrStateEvents extracts the state_event values the client PUT at the MR
// endpoint, in order. Merge PUTs are excluded — they carry no state_event.
func mrStateEvents(t *testing.T, rt *recordingTransport) []string {
	t.Helper()
	var events []string
	for _, req := range rt.requests {
		if req.Method != http.MethodPut || strings.HasSuffix(req.Path, "/merge") {
			continue
		}
		var body struct {
			StateEvent string `json:"state_event"`
		}
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			t.Fatalf("decode MR mutation body %q: %v", req.Body, err)
		}
		events = append(events, body.StateEvent)
	}
	return events
}

func assertNoMRMutation(t *testing.T, rt *recordingTransport) {
	t.Helper()
	if events := mrStateEvents(t, rt); len(events) != 0 {
		t.Fatalf("MR was mutated with state_event %v, want none", events)
	}
	for _, req := range rt.requests {
		if req.Method == http.MethodDelete {
			t.Fatalf("unexpected delete: %s %s", req.Method, req.Path)
		}
	}
}

// Happy path: one close, one reopen, then the merge lands. The cycle is issued
// exactly once even though the loop revisits the failure state.
func TestMerge_Stale405ClosesAndReopensOnceThenMerges(t *testing.T) {
	const (
		iid    = 1249
		sha    = "1b2210b24029a29a59d83df5497e418f1f4ef024"
		branch = "feat/bl-hud-mills-token-sweep"
	)
	mergeCalls, reopened := 0, false
	var rt *recordingTransport
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		// One PUT route for the MR: the stub matches on prefix, so a separate
		// ".../merge" route would be ambiguous with the state_event route.
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/1249": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/merge") {
				mergeCalls++
				if reopened {
					return 200, mergedMR(iid, sha, "merge-commit-sha", branch)
				}
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			if inFlightStateEvent(rt) == "reopen" {
				reopened = true
			}
			return 200, map[string]any{"iid": iid}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/1249": func(_ *http.Request) (int, any) {
			mr := stale405MR(iid, sha, branch, 21012, "mergeable")
			if reopened {
				// GitLab recomputed head_pipeline onto the green push pipeline.
				mr.HeadPipeline = mrHeadPipe{ID: 21012, Status: "success", Source: "push"}
			}
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, greenPipelineList(sha, branch, 21012)
		},
	})
	cli.mergeRetryTimeout = 500 * time.Millisecond

	resp, err := cli.Merge(context.Background(), testMergeArgs(iid, sha, branch))
	if err != nil {
		t.Fatalf("merge after 405 recovery: %v", err)
	}
	if resp.MergedSHA != "merge-commit-sha" {
		t.Fatalf("merged sha = %q, want merge-commit-sha", resp.MergedSHA)
	}
	if got := mrStateEvents(t, rt); len(got) != 2 || got[0] != "close" || got[1] != "reopen" {
		t.Fatalf("state events = %v, want exactly [close reopen]", got)
	}
	if mergeCalls != 2 {
		t.Fatalf("merge PUTs = %d, want 2 (the 405 and the post-recovery retry)", mergeCalls)
	}
}

// A 405 whose MR is no longer open must never be "recovered" — checking live MR
// state first is the 422 lesson. A merged MR returns its authoritative identity.
func TestMerge_Stale405NoRecoveryWhenMRAlreadyMerged(t *testing.T) {
	mergeCalls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/31": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/merge") {
				mergeCalls++
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			return 200, map[string]any{"iid": 31}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/31": func(_ *http.Request) (int, any) {
			mr := mergedMR(31, "tested-head", "already-merged-sha")
			mr.DetailedMergeStatus = "not_open"
			return 200, mr
		},
	})
	cli.mergeRetryTimeout = 200 * time.Millisecond

	resp, err := cli.Merge(context.Background(), testMergeArgs(31, "tested-head"))
	if err != nil {
		t.Fatalf("already-merged reconcile: %v", err)
	}
	if resp.MergedSHA != "already-merged-sha" {
		t.Fatalf("merged sha = %q, want already-merged-sha", resp.MergedSHA)
	}
	if mergeCalls != 0 {
		t.Fatalf("merge PUTs = %d, want 0 against an already-merged MR", mergeCalls)
	}
	assertNoMRMutation(t, rt)
}

// A closed MR stays closed: the recovery may only transition an OPEN MR, so the
// pre-existing "refusing automatic reopen" contract is untouched.
func TestMerge_Stale405NoRecoveryWhenMRClosed(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/32": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/merge") {
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			return 200, map[string]any{"iid": 32}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/32": func(_ *http.Request) (int, any) {
			mr := stale405MR(32, "tested-head", testSourceBranch, 3201, "mergeable")
			mr.State = "closed"
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, greenPipelineList("tested-head", testSourceBranch, 3201)
		},
	})
	cli.mergeRetryTimeout = 200 * time.Millisecond

	_, err := cli.Merge(context.Background(), testMergeArgs(32, "tested-head"))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "closed") {
		t.Fatalf("closed MR error = %v, want terminal closed error", err)
	}
	if !errorIsMergeRequestClosed(err) {
		t.Fatalf("closed MR error %v does not wrap ErrMergeRequestClosed", err)
	}
	assertNoMRMutation(t, rt)
}

func errorIsMergeRequestClosed(err error) bool {
	return err != nil && strings.Contains(err.Error(), pipeline.ErrMergeRequestClosed.Error())
}

// One cycle per stage attempt: a 405 that survives the close+reopen escalates
// with the original 405 (still ClassConfig) plus what the recovery observed.
func TestMerge_Stale405SecondFailureEscalatesWithRecoveryNote(t *testing.T) {
	const (
		iid    = 33
		sha    = "tested-head"
		branch = "feat/persistent-405"
	)
	mergeCalls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/33": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/merge") {
				mergeCalls++
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			return 200, map[string]any{"iid": iid}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/33": func(_ *http.Request) (int, any) {
			return 200, stale405MR(iid, sha, branch, 3301, "mergeable")
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, greenPipelineList(sha, branch, 3301)
		},
	})
	cli.mergeRetryTimeout = 300 * time.Millisecond

	_, err := cli.Merge(context.Background(), testMergeArgs(iid, sha, branch))
	if err == nil {
		t.Fatal("persistent 405 after recovery merged, want error")
	}
	if !strings.Contains(err.Error(), "status 405") {
		t.Fatalf("error %v lost the original 405", err)
	}
	if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify = %s, want config (escalates as today)", got)
	}
	if !strings.Contains(err.Error(), "405 recovery: closed+reopened mr 33") {
		t.Fatalf("error %v does not report what the recovery observed", err)
	}
	if got := mrStateEvents(t, rt); len(got) != 2 || got[0] != "close" || got[1] != "reopen" {
		t.Fatalf("state events = %v, want exactly one [close reopen] cycle", got)
	}
	if mergeCalls < 2 {
		t.Fatalf("merge PUTs = %d, want the 405 plus at least one post-recovery retry", mergeCalls)
	}
}

// Only detailed_merge_status values an MR state transition can actually fix may
// trigger a mutation. Everything else (and an absent value) fails closed.
func TestMerge_Stale405SkippedForUnrecoverableDetailedStatus(t *testing.T) {
	for _, detailed := range []string{
		"not_approved",
		"draft_status",
		"conflict",
		"need_rebase",
		"discussions_not_resolved",
		"requested_changes",
		"blocked_status",
		"ci_still_running",
		"checking",
		"", // GitLab too old to report the field
	} {
		t.Run("detailed="+detailed, func(t *testing.T) {
			cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/34": func(req *http.Request) (int, any) {
					if strings.HasSuffix(req.URL.Path, "/merge") {
						return 405, map[string]any{"message": "405 Method Not Allowed"}
					}
					return 200, map[string]any{"iid": 34}
				},
				"GET /api/v4/projects/services%2Floom-core/merge_requests/34": func(_ *http.Request) (int, any) {
					return 200, stale405MR(34, "tested-head", testSourceBranch, 3401, detailed)
				},
				"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
					return 200, greenPipelineList("tested-head", testSourceBranch, 3401)
				},
			})
			cli.mergeRetryTimeout = 120 * time.Millisecond

			if _, err := cli.Merge(context.Background(), testMergeArgs(34, "tested-head")); err == nil {
				t.Fatal("merge succeeded, want the 405 to surface")
			}
			assertNoMRMutation(t, rt)
		})
	}
}

// Without a green pipeline at the CI-authorized SHA a recompute has nothing
// better to point at, so the MR is left alone and the escalation says why.
func TestMerge_Stale405SkippedWithoutGreenPipelineForAuthorizedSHA(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/35": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/merge") {
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			return 200, map[string]any{"iid": 35}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/35": func(_ *http.Request) (int, any) {
			return 200, stale405MR(35, "tested-head", testSourceBranch, 3501, "ci_must_pass")
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			// A green pipeline exists, but for a different SHA.
			return 200, greenPipelineList("some-other-sha", testSourceBranch, 3599)
		},
	})
	cli.mergeRetryTimeout = 200 * time.Millisecond

	_, err := cli.Merge(context.Background(), testMergeArgs(35, "tested-head"))
	if err == nil {
		t.Fatal("merge succeeded, want the 405 to surface")
	}
	if !strings.Contains(err.Error(), "status 405") {
		t.Fatalf("error %v lost the original 405", err)
	}
	if !strings.Contains(err.Error(), "no successful pipeline") {
		t.Fatalf("error %v does not explain why recovery declined", err)
	}
	if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify = %s, want config", got)
	}
	assertNoMRMutation(t, rt)
}

// A close+reopen re-points head_pipeline at whatever is NEWEST for the SHA. If
// that is a skipped/failed merge_request_event placeholder sitting on top of an
// older green push pipeline, the cycle would pin the blocker as head — that
// wedge needs the placeholder deleted, not an MR state transition.
func TestMerge_Stale405SkippedWhenNewestPipelineForSHAIsNotGreen(t *testing.T) {
	for _, newest := range []shaPipeline{
		{ID: 3899, Status: "skipped", Source: "merge_request_event"},
		{ID: 3899, Status: "failed", Source: "merge_request_event"},
		{ID: 3899, Status: "failed", Source: "push"},
	} {
		t.Run(newest.Status+"/"+newest.Source, func(t *testing.T) {
			newest := newest
			newest.SHA, newest.Ref = "tested-head", testSourceBranch
			cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/38": func(req *http.Request) (int, any) {
					if strings.HasSuffix(req.URL.Path, "/merge") {
						return 405, map[string]any{"message": "405 Method Not Allowed"}
					}
					return 200, map[string]any{"iid": 38}
				},
				"GET /api/v4/projects/services%2Floom-core/merge_requests/38": func(_ *http.Request) (int, any) {
					// Head is the older green push pipeline, so the superseder
					// path declines and this recovery is the only candidate.
					return 200, stale405MR(38, "tested-head", testSourceBranch, 3801, "ci_must_pass")
				},
				"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
					return 200, []shaPipeline{
						newest,
						{ID: 3801, SHA: "tested-head", Ref: testSourceBranch, Status: "success", Source: "push"},
					}
				},
			})
			cli.mergeRetryTimeout = 200 * time.Millisecond

			_, err := cli.Merge(context.Background(), testMergeArgs(38, "tested-head"))
			if err == nil {
				t.Fatal("merge succeeded, want the 405 to surface")
			}
			if !strings.Contains(err.Error(), "would pin it as head") {
				t.Fatalf("error %v does not explain why recovery declined", err)
			}
			assertNoMRMutation(t, rt)
		})
	}
}

// Non-405 merge failures keep their existing behavior and never mutate the MR.
func TestMerge_Non405ErrorsNeverTriggerRecovery(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		payload map[string]any
	}{
		{"422 branch cannot be merged", 422, map[string]any{"message": "Branch cannot be merged"}},
		{"500 server error", 500, map[string]any{"message": "500 Internal Server Error"}},
		{"403 forbidden", 403, map[string]any{"message": "403 Forbidden"}},
		{"401 unauthorized", 401, map[string]any{"message": "401 Unauthorized"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/36": func(req *http.Request) (int, any) {
					if strings.HasSuffix(req.URL.Path, "/merge") {
						return tc.status, tc.payload
					}
					return 200, map[string]any{"iid": 36}
				},
				"GET /api/v4/projects/services%2Floom-core/merge_requests/36": func(_ *http.Request) (int, any) {
					return 200, stale405MR(36, "tested-head", testSourceBranch, 3601, "mergeable")
				},
				"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
					return 200, greenPipelineList("tested-head", testSourceBranch, 3601)
				},
			})
			cli.mergeRetryTimeout = 150 * time.Millisecond

			if _, err := cli.Merge(context.Background(), testMergeArgs(36, "tested-head")); err == nil {
				t.Fatal("merge succeeded, want the original error")
			}
			assertNoMRMutation(t, rt)
		})
	}
}

// If the close lands but every reopen fails, the MR is now closed and the
// escalation has to say so in as many words — that is worse than the 405.
func TestMerge_Stale405ReopenFailureReportsMRLeftClosed(t *testing.T) {
	reopenAttempts := 0
	var rt *recordingTransport
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/39": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/merge") {
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			if inFlightStateEvent(rt) == "reopen" {
				reopenAttempts++
				return 500, map[string]any{"message": "500 Internal Server Error"}
			}
			return 200, map[string]any{"iid": 39}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/39": func(_ *http.Request) (int, any) {
			return 200, stale405MR(39, "tested-head", testSourceBranch, 3901, "mergeable")
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, greenPipelineList("tested-head", testSourceBranch, 3901)
		},
	})
	cli.mergeRetryTimeout = 300 * time.Millisecond

	_, err := cli.Merge(context.Background(), testMergeArgs(39, "tested-head"))
	if err == nil {
		t.Fatal("merge succeeded, want the recovery failure to surface")
	}
	if !strings.Contains(err.Error(), "LEFT CLOSED") {
		t.Fatalf("error %v does not warn that the MR was left closed", err)
	}
	if !strings.Contains(err.Error(), "status 405") {
		t.Fatalf("error %v lost the original 405", err)
	}
	if reopenAttempts != stale405ReopenAttempts {
		t.Fatalf("reopen attempts = %d, want %d", reopenAttempts, stale405ReopenAttempts)
	}
	// Exactly one close: the cycle is never retried after it fails.
	closes := 0
	for _, ev := range mrStateEvents(t, rt) {
		if ev == "close" {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("close events = %d, want exactly 1", closes)
	}
}

// A close+reopen must not launder a head that moved: the post-recovery retry
// re-reconciles the CI-authorized identity, so a new source SHA still fails the
// head-movement fence (#374) instead of merging untested code.
func TestMerge_Stale405ReopenDoesNotBypassHeadSHAFence(t *testing.T) {
	const (
		iid    = 37
		sha    = "tested-head"
		branch = "feat/head-moved"
	)
	mergeCalls, reopened := 0, false
	var rt *recordingTransport
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/37": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/merge") {
				mergeCalls++
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			if inFlightStateEvent(rt) == "reopen" {
				reopened = true
			}
			return 200, map[string]any{"iid": iid}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/37": func(_ *http.Request) (int, any) {
			mr := stale405MR(iid, sha, branch, 3701, "mergeable")
			if reopened {
				// Somebody pushed while the recovery was mid-cycle.
				mr.SHA = "untested-head"
			}
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, greenPipelineList(sha, branch, 3701)
		},
	})
	cli.mergeRetryTimeout = 300 * time.Millisecond

	_, err := cli.Merge(context.Background(), testMergeArgs(iid, sha, branch))
	if err == nil {
		t.Fatal("merge succeeded after the head moved, want the fence to hold")
	}
	if !strings.Contains(err.Error(), "no longer matches CI-authorized") {
		t.Fatalf("error %v is not the head-movement fence", err)
	}
	if mergeCalls != 1 {
		t.Fatalf("merge PUTs = %d, want 1 — no PUT may follow the head move", mergeCalls)
	}
}

// The gate is the whole safety story, so pin it directly.
func TestStale405ReopenApplies(t *testing.T) {
	err405 := &GitLabHTTPError{Method: http.MethodPut, Path: "/merge", StatusCode: 405, Body: `{"message":"405 Method Not Allowed"}`}
	base := stale405MR(1, "sha", "branch", 10, "mergeable")

	cases := []struct {
		name string
		err  error
		mr   mrResponse
		want bool
	}{
		{"405 on open mergeable push head", err405, base, true},
		{"405 blocked only on ci", err405, stale405MR(1, "sha", "branch", 10, "ci_must_pass"), true},
		{"405 with null head pipeline", err405, func() mrResponse {
			mr := base
			mr.HeadPipeline = mrHeadPipe{}
			return mr
		}(), true},
		{"nil error", nil, base, false},
		{"non-http error", context.DeadlineExceeded, base, false},
		{"422", &GitLabHTTPError{StatusCode: 422, Body: "Branch cannot be merged"}, base, false},
		{"500", &GitLabHTTPError{StatusCode: 500}, base, false},
		{"closed mr", err405, func() mrResponse {
			mr := base
			mr.State = "closed"
			return mr
		}(), false},
		{"merged mr", err405, func() mrResponse {
			mr := base
			mr.State = "merged"
			return mr
		}(), false},
		{"locked mr", err405, func() mrResponse {
			mr := base
			mr.State = "locked"
			return mr
		}(), false},
		{"merge_request_event head belongs to superseder", err405, func() mrResponse {
			mr := base
			mr.HeadPipeline.Source = "merge_request_event"
			return mr
		}(), false},
		{"api head belongs to superseder", err405, func() mrResponse {
			mr := base
			mr.HeadPipeline.Source = "api"
			return mr
		}(), false},
		{"unmet approvals", err405, stale405MR(1, "sha", "branch", 10, "not_approved"), false},
		{"draft", err405, stale405MR(1, "sha", "branch", 10, "draft_status"), false},
		{"missing detailed status", err405, stale405MR(1, "sha", "branch", 10, ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stale405ReopenApplies(tc.err, tc.mr); got != tc.want {
				t.Fatalf("stale405ReopenApplies = %v, want %v", got, tc.want)
			}
		})
	}
}

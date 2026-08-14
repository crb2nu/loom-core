package clients

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// recordingTransport captures every request the client makes and serves
// canned responses keyed on (method, path-prefix). It's the test
// substrate for every GitLab REST verb we exercise.
type recordingTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
	routes   map[string]func(*http.Request) (int, any)
}

type recordedRequest struct {
	Method string
	Path   string
	Body   string
	Token  string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	body := ""
	if req.Body != nil {
		buf, _ := io.ReadAll(req.Body)
		body = string(buf)
	}
	// Use RawPath when the URL contains percent-encoded segments
	// (Go's URL.Path decodes %2F → /, which loses information GitLab
	// project paths depend on). Tests assert on the encoded form.
	matchPath := req.URL.Path
	if req.URL.RawPath != "" {
		matchPath = req.URL.RawPath
	}
	rt.requests = append(rt.requests, recordedRequest{
		Method: req.Method,
		Path:   matchPath,
		Body:   body,
		Token:  req.Header.Get("PRIVATE-TOKEN"),
	})
	// Longest matching prefix wins. Map iteration order is randomized, so a
	// generic route (".../pipelines") would otherwise shadow a more specific
	// one (".../pipelines/99/jobs") non-deterministically — the generic
	// handler's payload then decodes into the wrong struct and surfaces as a
	// baffling empty/garbage result rather than a routing error.
	var (
		bestHandler func(*http.Request) (int, any)
		bestLen     = -1
	)
	for prefix, handler := range rt.routes {
		method, path, _ := strings.Cut(prefix, " ")
		if req.Method == method && strings.HasPrefix(matchPath, path) && len(path) > bestLen {
			bestHandler, bestLen = handler, len(path)
		}
	}
	if bestHandler != nil {
		status, payload := bestHandler(req)
		buf, _ := json.Marshal(payload)
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(bytes.NewReader(buf)),
			Header:     make(http.Header),
		}, nil
	}
	return &http.Response{
		StatusCode: 404,
		Body:       io.NopCloser(strings.NewReader(`{"message":"not found"}`)),
		Header:     make(http.Header),
	}, nil
}

func newGitLabStub(t *testing.T, routes map[string]func(*http.Request) (int, any)) (*GitLabClient, *recordingTransport) {
	t.Helper()
	cli, err := NewGitLabClient(GitLabConfig{
		APIURL:       "https://gitlab.example/api/v4",
		Token:        "tok-123",
		Project:      "services/loom-core",
		PollInterval: 10 * time.Millisecond,
		PollDeadline: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	rt := &recordingTransport{routes: routes}
	cli.SetTransport(rt)
	return cli, rt
}

const (
	testGitLabProject = "services/loom-core"
	testSourceBranch  = "feat/test"
	testTargetBranch  = "main"
)

func testPollRequest(iid int64, sourceBranch string) pipeline.PollPipelineRequest {
	return pipeline.PollPipelineRequest{
		MRIID:        iid,
		Project:      testGitLabProject,
		SourceBranch: sourceBranch,
		TargetBranch: testTargetBranch,
	}
}

func testMergeArgs(iid int64, sha string, sourceBranch ...string) pipeline.MergeRequestArgs {
	source := testSourceBranch
	if len(sourceBranch) > 0 {
		source = sourceBranch[0]
	}
	return pipeline.MergeRequestArgs{
		MRIID:        iid,
		Project:      testGitLabProject,
		SourceBranch: source,
		TargetBranch: testTargetBranch,
		ExpectedSHA:  sha,
	}
}

func testMergeAuthorization(sha string, sourceBranch ...string) mergeAuthorization {
	req := testMergeArgs(1, sha, sourceBranch...)
	return mergeAuthorization{project: req.Project, sourceBranch: req.SourceBranch, targetBranch: req.TargetBranch, sha: req.ExpectedSHA}
}

func openedMR(iid int64, sha string, sourceBranch ...string) mrResponse {
	req := testMergeArgs(iid, sha, sourceBranch...)
	return mrResponse{IID: iid, State: "opened", SHA: sha, SourceBranch: req.SourceBranch, TargetBranch: req.TargetBranch}
}

func mergedMR(iid int64, sourceSHA, mergeCommitSHA string, sourceBranch ...string) mrResponse {
	mr := openedMR(iid, sourceSHA, sourceBranch...)
	mr.State = "merged"
	mr.MergeCommitSHA = mergeCommitSHA
	return mr
}

// ----- Config validation -----

func TestNewGitLabClient_RequiresFields(t *testing.T) {
	cases := []GitLabConfig{
		{},
		{APIURL: "x"},
		{APIURL: "x", Token: "y"},
	}
	for i, c := range cases {
		if _, err := NewGitLabClient(c); err == nil {
			t.Errorf("case %d: expected error for %+v", i, c)
		}
	}
}

func TestNewGitLabClient_AppliesDefaults(t *testing.T) {
	c, err := NewGitLabClient(GitLabConfig{
		APIURL: "x", Token: "y", Project: "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.PollInterval < 2*time.Second {
		t.Errorf("PollInterval = %v, want >= 2s default", c.cfg.PollInterval)
	}
	if c.cfg.MergeMethod != "merge" {
		t.Errorf("MergeMethod = %q, want merge", c.cfg.MergeMethod)
	}
}

// ----- CreateMR -----

func TestCreateMR_PostsAndPropagatesIID(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 200, []mrResponse{}
		},
		"POST /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 201, mrResponse{IID: 99, WebURL: "https://gitlab/services/loom-core/-/merge_requests/99"}
		},
	})
	resp, err := cli.CreateMR(context.Background(), pipeline.CreateMRRequest{
		BacklogID:    "BL-X",
		SourceBranch: "feat/x",
		TargetBranch: "main",
		Title:        "feat: x",
		Description:  "details",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.MRIID != 99 {
		t.Errorf("MRIID = %d", resp.MRIID)
	}
	if !strings.Contains(resp.URL, "/merge_requests/99") {
		t.Errorf("URL = %q", resp.URL)
	}
	if resp.Project != testGitLabProject || resp.SourceBranch != "feat/x" || resp.TargetBranch != "main" {
		t.Errorf("MR provenance = %q:%q→%q", resp.Project, resp.SourceBranch, resp.TargetBranch)
	}
	if len(rt.requests) != 2 {
		t.Fatalf("expected list then POST, got %d requests", len(rt.requests))
	}
	got := rt.requests[1]
	if got.Token != "tok-123" {
		t.Errorf("token header = %q", got.Token)
	}
	var body createMRBody
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.SourceBranch != "feat/x" || body.TargetBranch != "main" {
		t.Errorf("body branches wrong: %+v", body)
	}
	if !body.RemoveSourceBranch {
		t.Error("remove_source_branch should default true")
	}
}

// TestCreateMR_AutoMergeDoesNotArmMWPSOnCreate guards the fix for the
// long-standing Mills autonomous-merge 405 (#147/#148/#150): arming
// merge_when_pipeline_succeeds at MR-create time triggers a detached
// merge-request pipeline that this repo's `workflow.rules` empty into a
// `failed` head pipeline, blocking the merge. Even with AutoMerge=true, the
// create body must NOT carry merge_when_pipeline_succeeds; the operator's
// explicit `merge` stage performs the autonomous merge after `ci_watch`.
func TestCreateMR_AutoMergeDoesNotArmMWPSOnCreate(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) { return 200, []mrResponse{} },
		"POST /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 201, mrResponse{IID: 100, WebURL: "https://gitlab/-/merge_requests/100"}
		},
	})
	if _, err := cli.CreateMR(context.Background(), pipeline.CreateMRRequest{
		BacklogID: "BL-Y", SourceBranch: "feat/y", TargetBranch: "main",
		Title: "feat: y", AutoMerge: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(rt.requests) != 2 {
		t.Fatalf("requests = %d, want list + POST", len(rt.requests))
	}
	var body createMRBody
	if err := json.Unmarshal([]byte(rt.requests[1].Body), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.MergeWhenPipelineSucceeds {
		t.Errorf("merge_when_pipeline_succeeds = true on create; must be false to avoid the empty MR pipeline (AutoMerge is honored by the merge stage)")
	}
	if strings.Contains(rt.requests[1].Body, "merge_when_pipeline_succeeds") {
		t.Errorf("create body must omit merge_when_pipeline_succeeds even when AutoMerge=true: %s", rt.requests[1].Body)
	}
}

func TestCreateMR_AutoMergeFalseOmitsFlag(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) { return 200, []mrResponse{} },
		"POST /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 201, mrResponse{IID: 101, WebURL: "https://gitlab/-/merge_requests/101"}
		},
	})
	if _, err := cli.CreateMR(context.Background(), pipeline.CreateMRRequest{
		BacklogID: "BL-Z", SourceBranch: "feat/z", TargetBranch: "main",
		Title: "feat: z", AutoMerge: false,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// `omitempty` on the JSON field means the key should be absent when
	// AutoMerge=false — verifies we don't accidentally enable auto-merge
	// for an item the policy disallowed.
	if strings.Contains(rt.requests[1].Body, "merge_when_pipeline_succeeds") {
		t.Errorf("body unexpectedly contained merge_when_pipeline_succeeds: %s", rt.requests[1].Body)
	}
}

func TestCreateMR_ServerError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects": func(_ *http.Request) (int, any) {
			return 422, map[string]string{"message": "branch already has open MR"}
		},
	})
	if _, err := cli.CreateMR(context.Background(), pipeline.CreateMRRequest{SourceBranch: "x", TargetBranch: "main", Title: "x"}); err == nil {
		t.Error("expected error on 422")
	}
}

// TestCreateMR_AdoptsExistingOn409 guards the mr-stage 409 fix (live 2026-07-16:
// "Another open merge request already exists for this source branch: !1031"
// escalated ×3). CreateMR must look the existing open MR up by source branch
// and adopt its IID instead of surfacing the 409 as a stage error.
func TestCreateMR_AdoptsExistingOn409(t *testing.T) {
	postCalls := 0
	listCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			postCalls++
			return 409, map[string]string{"message": "Another open merge request already exists for this source branch: !1031"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(req *http.Request) (int, any) {
			listCalls++
			q := req.URL.Query()
			if q.Get("source_branch") != "feat/telemetry" {
				t.Errorf("list source_branch = %q, want feat/telemetry", q.Get("source_branch"))
			}
			if q.Get("state") != "opened" {
				t.Errorf("list state = %q, want opened", q.Get("state"))
			}
			if listCalls == 1 {
				return 200, []mrResponse{}
			}
			return 200, []mrResponse{{IID: 1031, WebURL: "https://gitlab/services/loom-core/-/merge_requests/1031", State: "opened", SourceBranch: "feat/telemetry", TargetBranch: "main"}}
		},
	})
	resp, err := cli.CreateMR(context.Background(), pipeline.CreateMRRequest{
		BacklogID:    "BL-Y",
		SourceBranch: "feat/telemetry",
		TargetBranch: "main",
		Title:        "feat: telemetry",
	})
	if err != nil {
		t.Fatalf("create should adopt existing MR on 409, got err: %v", err)
	}
	if resp.MRIID != 1031 {
		t.Errorf("adopted MRIID = %d, want 1031", resp.MRIID)
	}
	if !resp.Adopted {
		t.Errorf("resp.Adopted = false, want true")
	}
	if postCalls != 1 || listCalls != 2 {
		t.Errorf("expected initial GET + POST + 409 GET, got post=%d list=%d", postCalls, listCalls)
	}
}

func TestCreateMR_AdoptsExactOpenMRBeforePost(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(req *http.Request) (int, any) {
			q := req.URL.Query()
			if q.Get("state") != "opened" || q.Get("source_branch") != "feat/item/slice" || q.Get("target_branch") != "main" {
				t.Fatalf("exact MR filters = %q", req.URL.RawQuery)
			}
			return 200, []mrResponse{
				{IID: 7, State: "opened", SourceBranch: "feat/item/other", TargetBranch: "main"},
				{IID: 8, State: "opened", SourceBranch: "feat/item/slice", TargetBranch: "main", WebURL: "https://gitlab/mr/8"},
			}
		},
	})
	resp, err := cli.CreateMR(context.Background(), pipeline.CreateMRRequest{SourceBranch: "feat/item/slice", TargetBranch: "main", Title: "item"})
	if err != nil || !resp.Adopted || resp.MRIID != 8 {
		t.Fatalf("adoption = %+v, %v", resp, err)
	}
	if len(rt.requests) != 1 || rt.requests[0].Method != http.MethodGet {
		t.Fatalf("expected GET-only adoption, got %+v", rt.requests)
	}
}

// TestCreateMR_409ButNoOpenMRSurfacesError: if the 409 fires but the follow-up
// list finds no open MR (a race where it just closed/merged), CreateMR must not
// silently succeed — it returns the original error so the stage is auditable.
func TestCreateMR_409ButNoOpenMRSurfacesError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 409, map[string]string{"message": "Another open merge request already exists for this source branch: !77"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests": func(_ *http.Request) (int, any) {
			return 200, []mrResponse{}
		},
	})
	if _, err := cli.CreateMR(context.Background(), pipeline.CreateMRRequest{SourceBranch: "feat/z", TargetBranch: "main", Title: "z"}); err == nil {
		t.Error("expected error when 409 fires but no open MR is found to adopt")
	}
}

func TestAdoptGreenMR_SendsObservedSHA(t *testing.T) {
	const observedSHA = "ci-validated-head"
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/82": func(_ *http.Request) (int, any) {
			mr := openedMR(82, observedSHA)
			mr.DetailedMergeStatus = "mergeable"
			mr.HeadPipeline.Status = "success"
			return http.StatusOK, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/82/merge": func(req *http.Request) (int, any) {
			if got := req.URL.Query().Get("should_remove_source_branch"); got != "true" {
				t.Errorf("should_remove_source_branch = %q, want true", got)
			}
			return http.StatusOK, mergedMR(82, observedSHA, "merged-commit")
		},
	})

	adopted, reason, err := cli.AdoptGreenMR(context.Background(), 82)
	if err != nil || !adopted || reason != "merged open green mr" {
		t.Fatalf("AdoptGreenMR() = %t, %q, %v", adopted, reason, err)
	}
	if len(rt.requests) != 2 {
		t.Fatalf("requests = %d, want readiness GET + merge PUT", len(rt.requests))
	}
	var body mergeBody
	if err := json.Unmarshal([]byte(rt.requests[1].Body), &body); err != nil {
		t.Fatalf("decode merge body: %v", err)
	}
	if body.SHA != observedSHA {
		t.Errorf("merge sha = %q, want observed head %q", body.SHA, observedSHA)
	}
}

func TestAdoptGreenMR_RefusesMissingObservedSHA(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/84": func(_ *http.Request) (int, any) {
			mr := openedMR(84, "")
			mr.DetailedMergeStatus = "mergeable"
			mr.HeadPipeline.Status = "success"
			return http.StatusOK, mr
		},
	})

	adopted, reason, err := cli.AdoptGreenMR(context.Background(), 84)
	if err != nil || adopted || reason != "mr has no head sha" {
		t.Fatalf("AdoptGreenMR() = %t, %q, %v; want local missing-sha refusal", adopted, reason, err)
	}
	if len(rt.requests) != 1 {
		t.Fatalf("requests = %d, want readiness GET only", len(rt.requests))
	}
}

func TestAdoptGreenMR_MovedHeadRefusesWithoutRetry(t *testing.T) {
	const observedSHA = "ci-validated-head"
	putCalls := 0
	getCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/83": func(_ *http.Request) (int, any) {
			getCalls++
			mr := openedMR(83, observedSHA)
			mr.DetailedMergeStatus = "mergeable"
			mr.HeadPipeline.Status = "success"
			return http.StatusOK, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/83/merge": func(_ *http.Request) (int, any) {
			putCalls++
			return http.StatusConflict, map[string]any{"message": "SHA does not match HEAD of source branch"}
		},
	})

	adopted, _, err := cli.AdoptGreenMR(context.Background(), 83)
	if err == nil || adopted {
		t.Fatalf("AdoptGreenMR() = %t, err %v; want refused conflict", adopted, err)
	}
	if status, ok := GitLabHTTPStatus(err); !ok || status != http.StatusConflict {
		t.Fatalf("GitLabHTTPStatus(error) = %d, %t; want 409, true", status, ok)
	}
	if putCalls != 1 {
		t.Fatalf("merge PUT calls = %d, want exactly 1", putCalls)
	}
	if getCalls != 1 {
		t.Fatalf("readiness GET calls = %d, want 1 (next sweep must re-evaluate)", getCalls)
	}
}

// ----- PollPipeline -----

func TestPollPipeline_TerminatesOnSuccess(t *testing.T) {
	var pollCount int32
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/42": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 42, SHA: "abc123", SourceBranch: "feat/pipeline-auth", TargetBranch: testTargetBranch}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			q := req.URL.Query()
			if q.Get("sha") != "abc123" || q.Get("ref") != "feat/pipeline-auth" || q.Get("source") != "push" {
				t.Fatalf("pipeline filters = sha:%q ref:%q source:%q", q.Get("sha"), q.Get("ref"), q.Get("source"))
			}
			n := atomic.AddInt32(&pollCount, 1)
			status := "running"
			if n >= 2 {
				status = "success"
			}
			return 200, []map[string]any{{"id": 1234, "sha": "abc123", "ref": "feat/pipeline-auth", "status": status, "source": "push"}}
		},
	})
	resp, err := cli.PollPipeline(context.Background(), testPollRequest(42, "feat/pipeline-auth"))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("status = %q, want success (must ignore the failed merge_request_event placeholder)", resp.Status)
	}
	if resp.SHA != "abc123" {
		t.Errorf("sha = %q, want exact terminal MR head abc123", resp.SHA)
	}
	if resp.Project != testGitLabProject || resp.SourceBranch != "feat/pipeline-auth" || resp.TargetBranch != testTargetBranch {
		t.Errorf("terminal identity = %q:%q→%q, want %q:%q→%q", resp.Project, resp.SourceBranch, resp.TargetBranch, testGitLabProject, "feat/pipeline-auth", testTargetBranch)
	}
	if !strings.Contains(resp.LogTail, "pipeline 1234 (push) status=success") {
		t.Errorf("log tail should track the push pipeline, not the placeholder: %q", resp.LogTail)
	}
	if atomic.LoadInt32(&pollCount) < 2 {
		t.Errorf("expected at least 2 poll calls, got %d", pollCount)
	}
}

func TestPollPipeline_FailedTerminal(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/77": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 77, SHA: "deadbeef", SourceBranch: "feat/failing", TargetBranch: testTargetBranch}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, []map[string]any{{"id": 99, "sha": "deadbeef", "ref": "feat/failing", "status": "failed", "source": "push"}}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines/99/jobs": func(req *http.Request) (int, any) {
			q := req.URL.Query()
			if got := q["scope[]"]; !reflect.DeepEqual(got, []string{"failed"}) {
				t.Fatalf("job scope = %v, want [failed]", got)
			}
			if q.Get("include_retried") != "false" {
				t.Fatalf("include_retried = %q, want false", q.Get("include_retried"))
			}
			return 200, []map[string]any{
				{"id": 88, "name": "test:reliability", "status": "failed", "failure_reason": "runner_system_failure", "retried": false},
				{"status": "failed", "failure_reason": "script_failure", "retried": true},
			}
		},
	})
	resp, err := cli.PollPipeline(context.Background(), testPollRequest(77, "feat/failing"))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Status != "failed" {
		t.Errorf("status = %q", resp.Status)
	}
	if resp.SHA != "deadbeef" {
		t.Errorf("sha = %q, want exact terminal MR head deadbeef", resp.SHA)
	}
	if !reflect.DeepEqual(resp.FailedJobReasons, []string{"runner_system_failure"}) {
		t.Errorf("failed job reasons = %v, want current non-retried runner failure", resp.FailedJobReasons)
	}
	if !reflect.DeepEqual(resp.FailedJobs, []pipeline.FailedJob{{ID: 88, Name: "test:reliability", FailureReason: "runner_system_failure"}}) {
		t.Errorf("failed jobs = %#v", resp.FailedJobs)
	}
}

func TestRetryJob_PostsExpectedEndpoint(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/jobs/321/retry": func(req *http.Request) (int, any) {
			if req.Method != http.MethodPost {
				t.Fatalf("method=%s", req.Method)
			}
			return http.StatusCreated, map[string]any{"id": 654}
		},
	})
	if err := cli.RetryJob(context.Background(), 321); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestRetryJob_APIError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/jobs/321/retry": func(*http.Request) (int, any) { return http.StatusConflict, map[string]any{"message": "cannot retry"} },
	})
	if err := cli.RetryJob(context.Background(), 321); err == nil {
		t.Fatal("expected API error")
	}
}

func TestFailedJobReasons_PaginatesCompleteResult(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/pipelines/99/jobs": func(req *http.Request) (int, any) {
			if req.URL.Query().Get("page") == "1" {
				jobs := make([]pipelineJob, 100)
				for i := range jobs {
					jobs[i] = pipelineJob{Status: "failed", FailureReason: "runner_system_failure"}
				}
				return 200, jobs
			}
			return 200, []pipelineJob{{Status: "failed", FailureReason: "script_failure"}}
		},
	})

	reasons, err := cli.failedJobReasons(context.Background(), 99)
	if err != nil {
		t.Fatalf("failedJobReasons: %v", err)
	}
	if len(reasons) != 101 {
		t.Fatalf("reasons len = %d, want 101", len(reasons))
	}
	if reasons[100] != "script_failure" {
		t.Fatalf("last reason = %q, want script_failure", reasons[100])
	}
}

// TestPollPipeline_IgnoresMergeRequestEventPlaceholder is the core regression
// guard for the autonomous-merge blocker: when ONLY the spurious
// merge_request_event pipeline exists for the SHA, the poll must NOT report it
// as a terminal `failed` — it keeps waiting for the branch pipeline (here it
// never appears, so it times out rather than falsely failing).
func TestPollPipeline_IgnoresMergeRequestEventPlaceholder(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/88": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 88, SHA: "cafef00d", SourceBranch: "feat/placeholder", TargetBranch: testTargetBranch}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			// source=push excludes the project-wide merge_request_event placeholder.
			return 200, []map[string]any{}
		},
	})
	cli.cfg.PollDeadline = 50 * time.Millisecond
	cli.cfg.PollInterval = 10 * time.Millisecond
	resp, err := cli.PollPipeline(context.Background(), testPollRequest(88, "feat/placeholder"))
	if err == nil {
		t.Error("expected timeout, not a terminal failed from the placeholder")
	}
	if resp.Status != "timeout" {
		t.Errorf("status = %q, want timeout", resp.Status)
	}
}

// TestPollPipeline_TimeoutWhenNoSHA: when the overall poll deadline is the
// shorter bound it still wins, and the response keeps the "timeout" shape the
// ci_watch extension path reads.
func TestPollPipeline_TimeoutWhenNoSHA(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/55": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 55, SourceBranch: "feat/no-sha", TargetBranch: testTargetBranch} // never gets a head sha
		},
	})
	cli.cfg.PollDeadline = 50 * time.Millisecond
	cli.cfg.PollInterval = 10 * time.Millisecond
	cli.cfg.HeadSHADeadline = time.Hour // poll deadline is the binding bound here
	resp, err := cli.PollPipeline(context.Background(), testPollRequest(55, "feat/no-sha"))
	if err == nil {
		t.Error("expected timeout error")
	}
	if resp.Status != "timeout" {
		t.Errorf("status = %q, want timeout", resp.Status)
	}
}

// TestPollPipeline_HeadSHAUnavailableAfterBoundedWait is the 2026-07-26 wedge:
// an MR whose head SHA never materializes used to burn the whole 30m poll
// deadline (then two ci_watch extensions) emitting nothing but "head sha
// pending". It must now fail on its own bounded deadline, wrapping a distinct
// sentinel and carrying the MR state that explains the missing head.
func TestPollPipeline_HeadSHAUnavailableAfterBoundedWait(t *testing.T) {
	polls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/55": func(_ *http.Request) (int, any) {
			polls++
			return 200, mrResponse{
				IID:          55,
				SourceBranch: "feat/no-sha",
				TargetBranch: testTargetBranch,
				State:        "opened",
				MergeStatus:  "checking",
				WebURL:       "https://gitlab.example/services/loom-core/-/merge_requests/55",
			}
		},
	})
	cli.cfg.PollDeadline = 10 * time.Second
	cli.cfg.PollInterval = 5 * time.Millisecond
	cli.cfg.HeadSHADeadline = 20 * time.Millisecond

	resp, err := cli.PollPipeline(context.Background(), testPollRequest(55, "feat/no-sha"))
	if err == nil {
		t.Fatal("expected a head-sha error, not an indefinite wait")
	}
	if !errors.Is(err, pipeline.ErrMRHeadSHAUnavailable) {
		t.Fatalf("error = %v, want ErrMRHeadSHAUnavailable", err)
	}
	if errors.Is(err, pipeline.ErrPipelinePollTimeout) {
		t.Fatal("a headless MR must not be laundered into the retryable poll-timeout class")
	}
	if !strings.Contains(err.Error(), "merge_status=checking") ||
		!strings.Contains(err.Error(), "merge_requests/55") {
		t.Errorf("error should name the MR state and url, got %q", err)
	}
	if !strings.Contains(resp.LogTail, "head sha pending") {
		t.Errorf("log tail should retain the pending history, got %q", resp.LogTail)
	}
	if polls < 2 {
		t.Errorf("polls = %d, want at least 2 (the first observation starts the clock)", polls)
	}
}

// TestPollPipeline_HeadSHAUnresolvableFailsFast: GitLab already published the
// reason (conflicted diff / closed MR), so the bounded wait is pointless —
// !1239 sat at sha=null + has_conflicts=true for 15 hours.
func TestPollPipeline_HeadSHAUnresolvableFailsFast(t *testing.T) {
	cases := []struct {
		name string
		mr   mrResponse
		want string
	}{
		{
			name: "conflicts",
			mr:   mrResponse{IID: 55, State: "opened", MergeStatus: "cannot_be_merged", HasConflicts: true},
			want: "has_conflicts=true",
		},
		{
			name: "closed",
			mr:   mrResponse{IID: 55, State: "closed", MergeStatus: "cannot_be_merged"},
			want: "state=closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			polls := 0
			mr := tc.mr
			mr.SourceBranch = "feat/no-sha"
			mr.TargetBranch = testTargetBranch
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/55": func(_ *http.Request) (int, any) {
					polls++
					return 200, mr
				},
			})
			cli.cfg.PollDeadline = 10 * time.Second
			cli.cfg.PollInterval = 5 * time.Millisecond
			cli.cfg.HeadSHADeadline = time.Hour // never reached: the state is decisive

			_, err := cli.PollPipeline(context.Background(), testPollRequest(55, "feat/no-sha"))
			if !errors.Is(err, pipeline.ErrMRHeadSHAUnavailable) {
				t.Fatalf("error = %v, want ErrMRHeadSHAUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %q, got %q", tc.want, err)
			}
			if polls != 1 {
				t.Errorf("polls = %d, want 1 (a decisive MR state must not be re-polled)", polls)
			}
		})
	}
}

// TestPollPipeline_HeadSHAPendingClockResetsWhenHeadAppears: only an UNBROKEN
// run of headless observations counts. A blank sha that resolves on a later
// poll is a slow prepare and must not trip the bound.
func TestPollPipeline_HeadSHAPendingClockResetsWhenHeadAppears(t *testing.T) {
	polls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/55": func(_ *http.Request) (int, any) {
			polls++
			mr := mrResponse{IID: 55, SourceBranch: "feat/slow", TargetBranch: testTargetBranch, State: "opened"}
			// headless, head, headless, head — never two headless in a row.
			if polls%2 == 1 {
				return 200, mr
			}
			mr.SHA = "beadfeed"
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			if polls < 4 {
				return 200, []map[string]any{}
			}
			return 200, []map[string]any{{"id": 7, "sha": "beadfeed", "ref": "feat/slow", "status": "success", "source": "push"}}
		},
	})
	cli.cfg.PollDeadline = 10 * time.Second
	cli.cfg.PollInterval = 5 * time.Millisecond
	cli.cfg.HeadSHADeadline = time.Nanosecond // any second consecutive blank would trip

	resp, err := cli.PollPipeline(context.Background(), testPollRequest(55, "feat/slow"))
	if err != nil {
		t.Fatalf("interleaved blank shas must not trip the head-sha bound: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("status = %q, want success", resp.Status)
	}
}

func TestNewGitLabClientClampsHeadSHADeadline(t *testing.T) {
	cli, err := NewGitLabClient(GitLabConfig{
		APIURL: "https://gitlab.example/api/v4", Token: "t", Project: "p",
		PollDeadline: time.Minute,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if cli.cfg.HeadSHADeadline != time.Minute {
		t.Errorf("HeadSHADeadline = %s, want it clamped to the poll deadline (1m)", cli.cfg.HeadSHADeadline)
	}
	def, err := NewGitLabClient(GitLabConfig{APIURL: "https://gitlab.example/api/v4", Token: "t", Project: "p"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if def.cfg.HeadSHADeadline != defaultHeadSHADeadline {
		t.Errorf("HeadSHADeadline = %s, want %s", def.cfg.HeadSHADeadline, defaultHeadSHADeadline)
	}
	if cli.cfg.BranchPipelineDeadline != time.Minute {
		t.Errorf("BranchPipelineDeadline = %s, want it clamped to the poll deadline (1m)", cli.cfg.BranchPipelineDeadline)
	}
	if def.cfg.BranchPipelineDeadline != defaultBranchPipelineDeadline {
		t.Errorf("BranchPipelineDeadline = %s, want %s", def.cfg.BranchPipelineDeadline, defaultBranchPipelineDeadline)
	}
}

// TestPollPipeline_BranchPipelineUnavailableAfterBoundedWait is the round-2 twin
// of the head-SHA wedge: the MR HAS a head, but no push pipeline ever appears
// for it (workflow rules that admit only merge_request_event, CI disabled, a
// deleted pipeline). That used to log "branch pipeline pending" until the poll
// deadline and return the generic ErrPipelinePollTimeout, which ci_watch reads
// as "still running" — so it burned both watch extensions and escalated a
// pipeline that never existed. It must now fail on its own bounded deadline
// with a distinct sentinel, and name the pipelines that DO exist for the SHA.
func TestPollPipeline_BranchPipelineUnavailableAfterBoundedWait(t *testing.T) {
	pushLookups := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/91": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 91, SHA: "cafef00dcafef00d", SourceBranch: "feat/no-push-pipeline",
				TargetBranch: testTargetBranch, State: "opened", MergeStatus: "can_be_merged",
				WebURL: "https://gitlab.example/services/loom-core/-/merge_requests/91",
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(r *http.Request) (int, any) {
			// The bounded poll filters source=push and finds nothing. The single
			// unfiltered diagnostic lookup on the failure path finds the
			// merge_request_event pipeline that explains why.
			if r.URL.Query().Get("source") == "push" {
				pushLookups++
				return 200, []map[string]any{}
			}
			return 200, []map[string]any{
				{"id": 9, "sha": "cafef00dcafef00d", "ref": "feat/no-push-pipeline", "status": "success", "source": "merge_request_event"},
			}
		},
	})
	cli.cfg.PollDeadline = 10 * time.Second
	cli.cfg.PollInterval = 5 * time.Millisecond
	cli.cfg.BranchPipelineDeadline = 20 * time.Millisecond

	resp, err := cli.PollPipeline(context.Background(), testPollRequest(91, "feat/no-push-pipeline"))
	if err == nil {
		t.Fatal("expected a branch-pipeline error, not an indefinite wait")
	}
	if !errors.Is(err, pipeline.ErrBranchPipelineUnavailable) {
		t.Fatalf("error = %v, want ErrBranchPipelineUnavailable", err)
	}
	if errors.Is(err, pipeline.ErrPipelinePollTimeout) {
		t.Fatal("a head with no push pipeline must not be laundered into the retryable poll-timeout class")
	}
	if !strings.Contains(err.Error(), "merge_request_event") {
		t.Errorf("error should name the pipelines that DO exist for the sha, got %q", err)
	}
	if !strings.Contains(err.Error(), "merge_requests/91") {
		t.Errorf("error should name the MR url, got %q", err)
	}
	if !strings.Contains(resp.LogTail, "branch pipeline pending") {
		t.Errorf("log tail should retain the pending history, got %q", resp.LogTail)
	}
	if pushLookups < 2 {
		t.Errorf("push lookups = %d, want at least 2 (the first observation starts the clock)", pushLookups)
	}
}

// TestPollPipeline_BranchPipelineClockResetsOnRepush: the bound counts an
// UNBROKEN run of observations for ONE head. A repush mints a new head SHA, and
// its pipeline gets the full window again rather than inheriting the dead head's
// elapsed clock.
func TestPollPipeline_BranchPipelineClockResetsOnRepush(t *testing.T) {
	polls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/92": func(_ *http.Request) (int, any) {
			polls++
			mr := mrResponse{
				IID: 92, SourceBranch: "feat/repush", TargetBranch: testTargetBranch, State: "opened",
			}
			// Every observation is a fresh head, so no head ever accumulates two
			// consecutive pipeline-less polls.
			mr.SHA = fmt.Sprintf("head%04d", polls)
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(r *http.Request) (int, any) {
			if polls < 4 {
				return 200, []map[string]any{}
			}
			return 200, []map[string]any{{
				"id": 11, "sha": r.URL.Query().Get("sha"), "ref": "feat/repush",
				"status": "success", "source": "push",
			}}
		},
	})
	cli.cfg.PollDeadline = 10 * time.Second
	cli.cfg.PollInterval = 5 * time.Millisecond
	cli.cfg.BranchPipelineDeadline = time.Nanosecond // any second poll on ONE head would trip

	resp, err := cli.PollPipeline(context.Background(), testPollRequest(92, "feat/repush"))
	if err != nil {
		t.Fatalf("a moving head must not trip the branch-pipeline bound: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("status = %q, want success", resp.Status)
	}
}

// TestPollPipeline_ClosedMidWatchStopsPolling: an operator closing the MR while
// ci_watch runs ends its CI story. Previously only the headless path noticed a
// non-open state; with a head SHA the poll ran to the deadline and reported a
// generic pipeline timeout, so the run spent both watch extensions on a dead MR.
func TestPollPipeline_ClosedMidWatchStopsPolling(t *testing.T) {
	polls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/93": func(_ *http.Request) (int, any) {
			polls++
			mr := mrResponse{
				IID: 93, SHA: "deadbeef", SourceBranch: "feat/closed-mid-watch",
				TargetBranch: testTargetBranch, State: "opened",
				WebURL: "https://gitlab.example/services/loom-core/-/merge_requests/93",
			}
			if polls > 1 {
				mr.State = "closed"
			}
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, []map[string]any{{"id": 12, "sha": "deadbeef", "ref": "feat/closed-mid-watch", "status": "running", "source": "push"}}
		},
	})
	cli.cfg.PollDeadline = 10 * time.Second
	cli.cfg.PollInterval = 5 * time.Millisecond

	resp, err := cli.PollPipeline(context.Background(), testPollRequest(93, "feat/closed-mid-watch"))
	if !errors.Is(err, pipeline.ErrMergeRequestClosed) {
		t.Fatalf("error = %v, want ErrMergeRequestClosed", err)
	}
	if errors.Is(err, pipeline.ErrPipelinePollTimeout) {
		t.Fatal("a closed MR must not be reported as a pipeline poll timeout")
	}
	if polls != 2 {
		t.Errorf("polls = %d, want 2 (stop on the observation that saw the close)", polls)
	}
	if !strings.Contains(resp.LogTail, "abandoning ci watch") {
		t.Errorf("log tail should record why the watch stopped, got %q", resp.LogTail)
	}
	// The running pipeline seen before the close is still the operator's best
	// lead, so it must survive onto the error response.
	if resp.LastStatus != "running" {
		t.Errorf("LastStatus = %q, want the pre-close pipeline status", resp.LastStatus)
	}
}

// TestPollPipeline_MergedMidWatchKeepsPolling pins the deliberate carve-out: a
// merged MR is NOT terminal for ci_watch. Its head pipeline still exists and
// normally resolves on the next poll, and failing it would escalate a run whose
// work actually landed.
func TestPollPipeline_MergedMidWatchKeepsPolling(t *testing.T) {
	polls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/94": func(_ *http.Request) (int, any) {
			polls++
			mr := mrResponse{
				IID: 94, SHA: "deadbeef", SourceBranch: "feat/merged-mid-watch",
				TargetBranch: testTargetBranch, State: "opened",
			}
			if polls > 1 {
				mr.State = "merged"
			}
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			status := "running"
			if polls > 1 {
				status = "success"
			}
			return 200, []map[string]any{{"id": 13, "sha": "deadbeef", "ref": "feat/merged-mid-watch", "status": status, "source": "push"}}
		},
	})
	cli.cfg.PollDeadline = 10 * time.Second
	cli.cfg.PollInterval = 5 * time.Millisecond

	resp, err := cli.PollPipeline(context.Background(), testPollRequest(94, "feat/merged-mid-watch"))
	if err != nil {
		t.Fatalf("a merged MR must keep resolving its head pipeline: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("status = %q, want success", resp.Status)
	}
}

// TestPollPipeline_ErrorReturnsKeepLogTail: every non-timeout error return used
// to hand back a zero-valued response, so ci_watch appended an empty tail and
// the escalation lost the poll history that explains the failure.
func TestPollPipeline_ErrorReturnsKeepLogTail(t *testing.T) {
	polls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/95": func(_ *http.Request) (int, any) {
			polls++
			mr := mrResponse{
				IID: 95, SHA: "deadbeef", SourceBranch: "feat/log-tail",
				TargetBranch: testTargetBranch, State: "opened",
			}
			if polls > 1 {
				// A branch rename mid-watch invalidates the CI authorization.
				mr.SourceBranch = "feat/renamed"
			}
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, []map[string]any{{"id": 14, "sha": "deadbeef", "ref": "feat/log-tail", "status": "running", "source": "push"}}
		},
	})
	cli.cfg.PollDeadline = 10 * time.Second
	cli.cfg.PollInterval = 5 * time.Millisecond

	resp, err := cli.PollPipeline(context.Background(), testPollRequest(95, "feat/log-tail"))
	if !errors.Is(err, pipeline.ErrMergeAuthorizationStale) {
		t.Fatalf("error = %v, want ErrMergeAuthorizationStale", err)
	}
	if !strings.Contains(resp.LogTail, "pipeline 14") {
		t.Errorf("log tail should carry the poll history that preceded the failure, got %q", resp.LogTail)
	}
	if resp.PipelineURL == "" && resp.LastStatus == "" {
		t.Error("the last observed pipeline should survive onto the error response")
	}
}

// TestPollPipeline_TimeoutWrapsSentinelAndSurfacesURL guards DEBT-073 (#167)
// class a: when a branch pipeline exists but never reaches a terminal state
// within PollDeadline, the timeout error must (1) wrap ErrPipelinePollTimeout so
// the runner's Classify tags it ClassInfra rather than the default ClassCode,
// and (2) embed the pipeline web_url so the escalation the runner builds from
// this error is directly actionable (escalations #149/#153).
func TestPollPipeline_TimeoutWrapsSentinelAndSurfacesURL(t *testing.T) {
	const pipeURL = "https://gitlab.example/services/loom-core/-/pipelines/4242"
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/61": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 61, SHA: "beadfeed", SourceBranch: "feat/stuck", TargetBranch: testTargetBranch}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			// A real branch pipeline that never terminates within the deadline.
			return 200, []map[string]any{{"id": 4242, "sha": "beadfeed", "ref": "feat/stuck", "status": "running", "source": "push", "web_url": pipeURL}}
		},
	})
	cli.cfg.PollDeadline = 50 * time.Millisecond
	cli.cfg.PollInterval = 10 * time.Millisecond
	resp, err := cli.PollPipeline(context.Background(), testPollRequest(61, "feat/stuck"))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, pipeline.ErrPipelinePollTimeout) {
		t.Errorf("timeout error must wrap ErrPipelinePollTimeout so Classify tags it infra; got %v", err)
	}
	if !strings.Contains(err.Error(), pipeURL) {
		t.Errorf("timeout error must embed the pipeline web_url for an actionable escalation; got %q", err.Error())
	}
	if resp.Status != "timeout" {
		t.Errorf("status = %q, want timeout", resp.Status)
	}
	// S3: the ci_watch stage extends the watch on a still-running pipeline, so the
	// timeout response must surface the stuck pipeline URL and its last status as
	// structured fields (not just embedded in the error text).
	if resp.PipelineURL != pipeURL {
		t.Errorf("PipelineURL = %q, want %q", resp.PipelineURL, pipeURL)
	}
	if resp.LastStatus != "running" {
		t.Errorf("LastStatus = %q, want running", resp.LastStatus)
	}
}

func TestPollPipeline_ParentCancellationIsNotReportedAsPollTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/62": func(_ *http.Request) (int, any) {
			cancel()
			return 200, mrResponse{IID: 62, SHA: "tested-head", SourceBranch: "feat/cancel", TargetBranch: testTargetBranch}
		},
	})
	resp, err := cli.PollPipeline(ctx, testPollRequest(62, "feat/cancel"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation error = %v, want context.Canceled", err)
	}
	if errors.Is(err, pipeline.ErrPipelinePollTimeout) || resp.Status == "timeout" {
		t.Fatalf("parent cancellation was converted to pipeline timeout: %+v, %v", resp, err)
	}
}

func TestPollPipeline_RequiresMRIID(t *testing.T) {
	cli, _ := newGitLabStub(t, nil)
	if _, err := cli.PollPipeline(context.Background(), pipeline.PollPipelineRequest{}); err == nil {
		t.Error("expected error for missing MRIID")
	}
}

func TestPollPipeline_IncompleteOrReroutedProvenanceBlocksBeforeHTTP(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*pipeline.PollPipelineRequest)
	}{
		{"missing project", func(req *pipeline.PollPipelineRequest) { req.Project = "" }},
		{"missing source", func(req *pipeline.PollPipelineRequest) { req.SourceBranch = "" }},
		{"missing target", func(req *pipeline.PollPipelineRequest) { req.TargetBranch = "" }},
		{"rerouted project", func(req *pipeline.PollPipelineRequest) { req.Project = "services/other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, rt := newGitLabStub(t, nil)
			req := testPollRequest(90, "feat/provenance")
			tt.mutate(&req)
			if _, err := cli.PollPipeline(context.Background(), req); err == nil {
				t.Fatal("expected incomplete or rerouted provenance to fail closed")
			} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
				t.Fatalf("Classify(%v) = %s, want config", err, got)
			}
			if len(rt.requests) != 0 {
				t.Fatalf("provenance failure issued HTTP requests: %+v", rt.requests)
			}
		})
	}
}

func TestPollPipeline_RejectsLiveSourceOrTargetChangeBeforePipelineLookup(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mrResponse)
	}{
		{"source changed", func(mr *mrResponse) { mr.SourceBranch = "feat/reassigned" }},
		{"target changed", func(mr *mrResponse) { mr.TargetBranch = "release" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pipelineCalls := 0
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/91": func(_ *http.Request) (int, any) {
					mr := openedMR(91, "tested-head", "feat/provenance")
					tt.mutate(&mr)
					return 200, mr
				},
				"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
					pipelineCalls++
					return 200, []shaPipeline{}
				},
			})
			if _, err := cli.PollPipeline(context.Background(), testPollRequest(91, "feat/provenance")); err == nil {
				t.Fatal("expected live MR provenance change to fail closed")
			} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
				t.Fatalf("Classify(%v) = %s, want config", err, got)
			}
			if pipelineCalls != 0 {
				t.Fatalf("pipeline lookup calls = %d, want 0", pipelineCalls)
			}
		})
	}
}

func TestPollPipeline_MissingSourceBranchFailsClosed(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/89": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 89, SHA: "tested-head", TargetBranch: testTargetBranch}
		},
	})
	req := testPollRequest(89, "feat/expected")
	if _, err := cli.PollPipeline(context.Background(), req); err == nil {
		t.Fatal("expected missing MR source branch to block pipeline authorization")
	} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify(missing source branch) = %s, want config", got)
	}
}

func TestBranchPipelineForSHA_RejectsRefAndSourceConfusion(t *testing.T) {
	tests := []struct {
		name string
		got  shaPipeline
	}{
		{
			name: "same sha different ref",
			got:  shaPipeline{ID: 71, SHA: "shared-commit", Ref: "other-branch", Source: "push", Status: "success"},
		},
		{
			name: "same ref wrong source",
			got:  shaPipeline{ID: 72, SHA: "shared-commit", Ref: "feat/wanted", Source: "web", Status: "success"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
					q := req.URL.Query()
					if q.Get("sha") != "shared-commit" || q.Get("ref") != "feat/wanted" || q.Get("source") != "push" {
						t.Fatalf("pipeline filters = sha:%q ref:%q source:%q", q.Get("sha"), q.Get("ref"), q.Get("source"))
					}
					return 200, []shaPipeline{tt.got}
				},
			})
			if _, _, err := cli.branchPipelineForSHA(context.Background(), "shared-commit", "feat/wanted", "push"); err == nil {
				t.Fatal("expected mismatched pipeline identity to fail closed")
			} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
				t.Fatalf("Classify(pipeline identity mismatch) = %s, want config", got)
			}
		})
	}
}

// ----- Merge -----

func TestMerge_UsesExpectedSHAAndReturnsMergeCommitSHA(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/3": func(_ *http.Request) (int, any) {
			return 200, openedMR(3, "tested-head-sha")
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/3/merge": func(_ *http.Request) (int, any) {
			return 200, mergedMR(3, "tested-head-sha", "merge-commit-sha")
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(3, "tested-head-sha"))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if resp.MergedSHA != "merge-commit-sha" {
		t.Errorf("sha = %q", resp.MergedSHA)
	}
	var body mergeBody
	if len(rt.requests) != 2 || rt.requests[0].Method != http.MethodGet || rt.requests[1].Method != http.MethodPut {
		t.Fatalf("request sequence = %+v, want GET immediately followed by PUT", rt.requests)
	}
	if err := json.Unmarshal([]byte(rt.requests[1].Body), &body); err != nil {
		t.Fatalf("decode merge body: %v", err)
	}
	if body.SHA != "tested-head-sha" {
		t.Errorf("merge body sha = %q, want tested-head-sha", body.SHA)
	}
}

func TestMerge_GitLabReportsMergeError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/4/merge": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 4, MergeError: "branch cannot be merged"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/4": func(_ *http.Request) (int, any) {
			return 200, openedMR(4, "tested-head")
		},
	})
	if _, err := cli.Merge(context.Background(), testMergeArgs(4, "tested-head")); err == nil {
		t.Error("expected error from merge_error field")
	}
}

func TestMergedResponse_IdentityPrecedence(t *testing.T) {
	tests := []struct {
		name string
		mr   mrResponse
		want string
	}{
		{"merged commit", mrResponse{State: "merged", SHA: "tested", SourceBranch: testSourceBranch, TargetBranch: testTargetBranch, MergedCommitSHA: "preferred", MergeCommitSHA: "merge", SquashCommitSHA: "squash"}, "preferred"},
		{"merge commit", mrResponse{State: "merged", SHA: "tested", SourceBranch: testSourceBranch, TargetBranch: testTargetBranch, MergeCommitSHA: "merge"}, "merge"},
		{"squash commit", mrResponse{State: "merged", SHA: "tested", SourceBranch: testSourceBranch, TargetBranch: testTargetBranch, SquashCommitSHA: "squash"}, "squash"},
		{"fast forward", mrResponse{State: "merged", SHA: "tested", SourceBranch: testSourceBranch, TargetBranch: testTargetBranch}, "tested"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := mergedResponse(1, tt.mr, testMergeAuthorization("tested"))
			if err != nil || resp.MergedSHA != tt.want {
				t.Fatalf("merged response = %+v, %v; want sha %q", resp, err, tt.want)
			}
		})
	}
}

func TestMergedResponse_RequiresStateAndTestedSourceIdentity(t *testing.T) {
	for _, mr := range []mrResponse{
		{State: "opened", SHA: "tested", SourceBranch: testSourceBranch, TargetBranch: testTargetBranch, MergeCommitSHA: "merge"},
		{State: "merged", SourceBranch: testSourceBranch, TargetBranch: testTargetBranch, MergeCommitSHA: "merge"},
		{State: "merged", SHA: "different", SourceBranch: testSourceBranch, TargetBranch: testTargetBranch, MergeCommitSHA: "merge"},
	} {
		if _, err := mergedResponse(1, mr, testMergeAuthorization("tested")); err == nil {
			t.Fatalf("expected incomplete or mismatched merged identity to fail: %+v", mr)
		}
	}
}

func TestMerge_RequiresMRIID(t *testing.T) {
	cli, _ := newGitLabStub(t, nil)
	if _, err := cli.Merge(context.Background(), pipeline.MergeRequestArgs{}); err == nil {
		t.Error("expected error for missing MRIID")
	}
	if _, err := cli.Merge(context.Background(), pipeline.MergeRequestArgs{MRIID: 3}); err == nil {
		t.Error("expected error for missing expected SHA")
	}
}

func TestMerge_IncompleteOrReroutedAuthorizationBlocksBeforeHTTP(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*pipeline.MergeRequestArgs)
	}{
		{"missing project", func(req *pipeline.MergeRequestArgs) { req.Project = "" }},
		{"missing source", func(req *pipeline.MergeRequestArgs) { req.SourceBranch = "" }},
		{"missing target", func(req *pipeline.MergeRequestArgs) { req.TargetBranch = "" }},
		{"missing sha", func(req *pipeline.MergeRequestArgs) { req.ExpectedSHA = "" }},
		{"rerouted project", func(req *pipeline.MergeRequestArgs) { req.Project = "services/other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, rt := newGitLabStub(t, nil)
			req := testMergeArgs(31, "tested-head")
			tt.mutate(&req)
			if _, err := cli.Merge(context.Background(), req); err == nil {
				t.Fatal("expected incomplete or rerouted authorization to fail closed")
			} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
				t.Fatalf("Classify(%v) = %s, want config", err, got)
			}
			if len(rt.requests) != 0 {
				t.Fatalf("authorization failure issued HTTP requests: %+v", rt.requests)
			}
		})
	}
}

func TestMerge_RejectsLiveSourceOrTargetChangeBeforePUT(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mrResponse)
	}{
		{"source changed", func(mr *mrResponse) { mr.SourceBranch = "feat/reassigned" }},
		{"target changed", func(mr *mrResponse) { mr.TargetBranch = "release" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergeCalls := 0
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/32": func(_ *http.Request) (int, any) {
					mr := openedMR(32, "tested-head")
					tt.mutate(&mr)
					return 200, mr
				},
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/32/merge": func(_ *http.Request) (int, any) {
					mergeCalls++
					return 200, mergedMR(32, "tested-head", "must-not-merge")
				},
			})
			if _, err := cli.Merge(context.Background(), testMergeArgs(32, "tested-head")); err == nil {
				t.Fatal("expected live MR identity change to fail closed")
			} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
				t.Fatalf("Classify(%v) = %s, want config", err, got)
			}
			if mergeCalls != 0 {
				t.Fatalf("merge PUT calls = %d, want 0", mergeCalls)
			}
		})
	}
}

func TestMerge_RevalidatesSourceAndTargetAfterWaitBeforeRetryPUT(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mrResponse)
	}{
		{"source changed", func(mr *mrResponse) { mr.SourceBranch = "feat/reassigned" }},
		{"target changed", func(mr *mrResponse) { mr.TargetBranch = "release" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getCalls := 0
			mergeCalls := 0
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/33": func(_ *http.Request) (int, any) {
					getCalls++
					mr := openedMR(33, "tested-head")
					mr.HeadPipeline = mrHeadPipe{Source: "push"}
					if getCalls >= 3 {
						tt.mutate(&mr)
					}
					return 200, mr
				},
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/33/merge": func(_ *http.Request) (int, any) {
					mergeCalls++
					return 405, map[string]any{"message": "405 Method Not Allowed"}
				},
			})
			if _, err := cli.Merge(context.Background(), testMergeArgs(33, "tested-head")); err == nil {
				t.Fatal("expected identity change during merge wait to fail closed")
			} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
				t.Fatalf("Classify(%v) = %s, want config", err, got)
			}
			if mergeCalls != 1 {
				t.Fatalf("merge PUT calls = %d, want exactly the pre-change attempt", mergeCalls)
			}
		})
	}
}

func TestMerge_DirectMergedResponseRequiresFullIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*mrResponse)
	}{
		{"source changed", func(mr *mrResponse) { mr.SourceBranch = "feat/reassigned" }},
		{"target changed", func(mr *mrResponse) { mr.TargetBranch = "release" }},
		{"sha changed", func(mr *mrResponse) { mr.SHA = "unreviewed-head" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/34": func(_ *http.Request) (int, any) {
					return 200, openedMR(34, "tested-head")
				},
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/34/merge": func(_ *http.Request) (int, any) {
					mr := mergedMR(34, "tested-head", "merged-commit")
					tt.mutate(&mr)
					return 200, mr
				},
			})
			if _, err := cli.Merge(context.Background(), testMergeArgs(34, "tested-head")); err == nil {
				t.Fatal("expected mismatched merged response identity to fail closed")
			} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
				t.Fatalf("Classify(%v) = %s, want config", err, got)
			}
		})
	}
}

func TestMerge_LockedTransitionsToMergedWithoutPUT(t *testing.T) {
	getCalls := 0
	mergeCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/35": func(_ *http.Request) (int, any) {
			getCalls++
			if getCalls == 1 {
				mr := openedMR(35, "tested-head")
				mr.State = "locked"
				return 200, mr
			}
			return 200, mergedMR(35, "tested-head", "authoritative-merge")
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/35/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 500, map[string]any{"message": "must not PUT while locked"}
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(35, "tested-head"))
	if err != nil || resp.MergedSHA != "authoritative-merge" {
		t.Fatalf("locked-to-merged reconciliation = %+v, %v", resp, err)
	}
	if mergeCalls != 0 {
		t.Fatalf("merge PUT calls = %d, want 0", mergeCalls)
	}
}

func TestMerge_LockedTransitionsToOpenedBeforePUT(t *testing.T) {
	getCalls := 0
	mergeCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/36": func(_ *http.Request) (int, any) {
			getCalls++
			mr := openedMR(36, "tested-head")
			if getCalls == 1 {
				mr.State = "locked"
			}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/36/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 200, mergedMR(36, "tested-head", "merged-after-unlock")
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(36, "tested-head"))
	if err != nil || resp.MergedSHA != "merged-after-unlock" {
		t.Fatalf("locked-to-opened merge = %+v, %v", resp, err)
	}
	if mergeCalls != 1 || getCalls < 2 {
		t.Fatalf("GET/PUT calls = %d/%d, want >=2/1", getCalls, mergeCalls)
	}
}

func TestMerge_PersistentLockedReturnsBoundedTransientWithoutPUT(t *testing.T) {
	mergeCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/37": func(_ *http.Request) (int, any) {
			mr := openedMR(37, "tested-head")
			mr.State = "locked"
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/37/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 500, map[string]any{"message": "must not PUT while locked"}
		},
	})
	cli.mergeRetryTimeout = 35 * time.Millisecond
	cli.cfg.PollInterval = 5 * time.Millisecond
	started := time.Now()
	_, err := cli.Merge(context.Background(), testMergeArgs(37, "tested-head"))
	if !errors.Is(err, pipeline.ErrMergeRequestLocked) {
		t.Fatalf("persistent locked error = %v, want ErrMergeRequestLocked", err)
	}
	if got := pipeline.Classify(err); got != pipeline.ClassTransient {
		t.Fatalf("Classify(%v) = %s, want transient", err, got)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("locked reconciliation exceeded bounded test deadline: %s", elapsed)
	}
	if mergeCalls != 0 {
		t.Fatalf("merge PUT calls = %d, want 0", mergeCalls)
	}
}

func TestMerge_DirectLockedResponseReconcilesAuthoritativeState(t *testing.T) {
	tests := []struct {
		name      string
		afterLock func(getCalls int) mrResponse
		wantPUTs  int
		wantSHA   string
	}{
		{
			name: "already merged",
			afterLock: func(_ int) mrResponse {
				return mergedMR(38, "tested-head", "merged-after-lost-locked-response")
			},
			wantPUTs: 1,
			wantSHA:  "merged-after-lost-locked-response",
		},
		{
			name: "opened then merged",
			afterLock: func(_ int) mrResponse {
				return openedMR(38, "tested-head")
			},
			wantPUTs: 2,
			wantSHA:  "merged-after-unlock",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getCalls := 0
			putCalls := 0
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/38": func(_ *http.Request) (int, any) {
					getCalls++
					if getCalls == 1 {
						return 200, openedMR(38, "tested-head")
					}
					return 200, tt.afterLock(getCalls)
				},
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/38/merge": func(_ *http.Request) (int, any) {
					putCalls++
					if putCalls == 1 {
						return 200, mrResponse{IID: 38, State: "locked"}
					}
					return 200, mergedMR(38, "tested-head", "merged-after-unlock")
				},
			})
			resp, err := cli.Merge(context.Background(), testMergeArgs(38, "tested-head"))
			if err != nil || resp.MergedSHA != tt.wantSHA {
				t.Fatalf("direct locked reconciliation = %+v, %v", resp, err)
			}
			if putCalls != tt.wantPUTs {
				t.Fatalf("merge PUT calls = %d, want %d", putCalls, tt.wantPUTs)
			}
		})
	}
}

func TestMerge_LockedAnd405ShareOneDeadline(t *testing.T) {
	getCalls := 0
	putCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/39": func(_ *http.Request) (int, any) {
			getCalls++
			mr := openedMR(39, "tested-head")
			mr.HeadPipeline = mrHeadPipe{Source: "push"}
			if getCalls >= 3 {
				mr.State = "locked"
			}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/39/merge": func(_ *http.Request) (int, any) {
			putCalls++
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
	})
	cli.mergeRetryTimeout = 45 * time.Millisecond
	cli.cfg.PollInterval = 15 * time.Millisecond
	started := time.Now()
	_, err := cli.Merge(context.Background(), testMergeArgs(39, "tested-head"))
	if !errors.Is(err, pipeline.ErrMergeRequestLocked) {
		t.Fatalf("mixed 405/locked error = %v, want ErrMergeRequestLocked", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("mixed states minted a fresh deadline: %s", elapsed)
	}
	if putCalls != 1 {
		t.Fatalf("merge PUT calls = %d, want 1 before lock became authoritative", putCalls)
	}
}

func TestMerge_HistoricalLockDoesNotMaskCurrent405(t *testing.T) {
	getCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/45": func(_ *http.Request) (int, any) {
			getCalls++
			mr := openedMR(45, "tested-head")
			mr.HeadPipeline = mrHeadPipe{Source: "push"}
			if getCalls == 1 {
				mr.State = "locked"
			}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/45/merge": func(_ *http.Request) (int, any) {
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
	})
	cli.mergeRetryTimeout = 45 * time.Millisecond
	cli.cfg.PollInterval = 10 * time.Millisecond
	_, err := cli.Merge(context.Background(), testMergeArgs(45, "tested-head"))
	if err == nil || !isMergeNotReady(err) {
		t.Fatalf("historical lock/current 405 error = %v, want current 405", err)
	}
	if errors.Is(err, pipeline.ErrMergeRequestLocked) {
		t.Fatalf("historical lock masked current 405: %v", err)
	}
}

func TestMerge_SharedDeadlineCancelsBlockedGET(t *testing.T) {
	cli, _ := newGitLabStub(t, nil)
	var calls int32
	cli.SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	cli.mergeRetryTimeout = 35 * time.Millisecond
	started := time.Now()
	_, err := cli.Merge(context.Background(), testMergeArgs(43, "tested-head"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked GET error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("blocked GET outlived shared merge deadline: %s", elapsed)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("blocked GET transport was not called")
	}
}

func TestMerge_SharedDeadlineCancelsBlockedPUT(t *testing.T) {
	cli, _ := newGitLabStub(t, nil)
	var getCalls int32
	var putCalls int32
	cli.SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			atomic.AddInt32(&getCalls, 1)
			buf, err := json.Marshal(openedMR(44, "tested-head"))
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(buf)),
				Header:     make(http.Header),
			}, nil
		}
		atomic.AddInt32(&putCalls, 1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	cli.mergeRetryTimeout = 35 * time.Millisecond
	started := time.Now()
	_, err := cli.Merge(context.Background(), testMergeArgs(44, "tested-head"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked PUT error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("blocked PUT outlived shared merge deadline: %s", elapsed)
	}
	if atomic.LoadInt32(&getCalls) != 1 || atomic.LoadInt32(&putCalls) == 0 {
		t.Fatalf("blocked GET/PUT calls = %d/%d, want 1/>=1", getCalls, putCalls)
	}
}

func TestMerge_BranchMovedAfterCIRejectsWithPrecondition(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/5/merge": func(_ *http.Request) (int, any) {
			return 409, map[string]any{"message": "SHA does not match HEAD of source branch"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/5": func(_ *http.Request) (int, any) {
			return 200, openedMR(5, "moved-head")
		},
	})
	_, err := cli.Merge(context.Background(), testMergeArgs(5, "ci-tested-head"))
	if err == nil {
		t.Fatal("expected moved branch to be rejected")
	}
	if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify(moved branch) = %s, want config", got)
	}
	if len(rt.requests) != 1 || rt.requests[0].Method != http.MethodGet {
		t.Fatalf("requests = %+v, want one read-only reconciliation and no merge PUT", rt.requests)
	}
}

func TestMerge_BranchMovedAfterCICannotEnter422Settle(t *testing.T) {
	mergeCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/16/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 422, map[string]any{"message": "Branch cannot be merged"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/16": func(_ *http.Request) (int, any) {
			return 200, openedMR(16, "moved-unreviewed-head")
		},
	})

	_, err := cli.Merge(context.Background(), testMergeArgs(16, "ci-tested-head"))
	if err == nil {
		t.Fatal("expected moved branch to fail before 422 settle retries")
	}
	if mergeCalls != 0 {
		t.Fatalf("merge called %d times after head moved, want 0", mergeCalls)
	}
}

// TestMerge_RetriesNotMergeableYet405 guards the Mills A2 north-star regression
// (escalations #147/#148/#150): GitLab returns 405 on PUT .../merge while the
// MR's merge_status is still settling after CI turned green — a timing race.
// Merge must poll past the transient 405 and succeed, not fail on the first hit.
func TestMerge_RetriesNotMergeableYet405(t *testing.T) {
	calls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/7/merge": func(_ *http.Request) (int, any) {
			calls++
			if calls < 3 {
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			return 200, mergedMR(7, "tested-head", "deadbeef")
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/7": func(_ *http.Request) (int, any) {
			mr := openedMR(7, "tested-head")
			mr.HeadPipeline = mrHeadPipe{Source: "push"}
			return 200, mr
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(7, "tested-head"))
	if err != nil {
		t.Fatalf("merge should succeed after transient 405s: %v", err)
	}
	if resp.MergedSHA != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", resp.MergedSHA)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts (2x405 then 200), got %d", calls)
	}
	for _, req := range rt.requests {
		if req.Method != http.MethodPut || !strings.HasSuffix(req.Path, "/merge") {
			continue
		}
		var body mergeBody
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			t.Fatalf("decode merge body: %v", err)
		}
		if body.SHA != "tested-head" {
			t.Errorf("retry merge sha = %q, want tested-head", body.SHA)
		}
	}
}

// GitLab can also return 422 "Branch cannot be merged" during the same short
// mergeability-settling window. Live Mills runs !1037 and !1044 reached green
// CI, hit this response once, and were incorrectly escalated.
func TestMerge_RetriesNotMergeableYet422(t *testing.T) {
	calls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/14/merge": func(_ *http.Request) (int, any) {
			calls++
			if calls < 3 {
				return 422, map[string]any{"message": "Branch cannot be merged"}
			}
			return 200, mergedMR(14, "tested-head", "cab005e")
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/14": func(_ *http.Request) (int, any) {
			return 200, openedMR(14, "tested-head")
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(14, "tested-head"))
	if err != nil {
		t.Fatalf("merge should succeed after transient 422s: %v", err)
	}
	if resp.MergedSHA != "cab005e" {
		t.Errorf("sha = %q, want cab005e", resp.MergedSHA)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts (2x422 then 200), got %d", calls)
	}
}

func TestMerge_DoesNotRetryUnrelated422(t *testing.T) {
	calls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/15/merge": func(_ *http.Request) (int, any) {
			calls++
			return 422, map[string]any{"message": "sha does not match HEAD"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/15": func(_ *http.Request) (int, any) {
			return 200, openedMR(15, "tested-head")
		},
	})
	if _, err := cli.Merge(context.Background(), testMergeArgs(15, "tested-head")); err == nil {
		t.Fatal("expected unrelated 422 to return an error")
	}
	if calls != 1 {
		t.Errorf("unrelated 422 should not retry; got %d attempts", calls)
	}
}

// A persistent 422 means the branch is behind target. Mills must return the
// error after bounded settle retries; an external rebase must restart the full
// review/gate/CI cycle rather than changing the authorized SHA inside merge.
func TestMerge_Persistent422ReturnsWithoutRebase(t *testing.T) {
	mergeCalls := 0
	rebaseCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/20/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 422, map[string]string{"message": "Branch cannot be merged"}
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/20/rebase": func(_ *http.Request) (int, any) {
			rebaseCalls++
			return 202, map[string]any{"rebase_in_progress": true}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/20": func(_ *http.Request) (int, any) {
			return 200, openedMR(20, "tested-head")
		},
	})
	_, err := cli.Merge(context.Background(), testMergeArgs(20, "tested-head"))
	if err == nil {
		t.Fatal("expected persistent 422 to remain terminal")
	}
	if rebaseCalls != 0 {
		t.Errorf("rebase calls = %d, want 0", rebaseCalls)
	}
	if mergeCalls != branchMergeSettleAttempts {
		t.Errorf("merge calls = %d, want %d", mergeCalls, branchMergeSettleAttempts)
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "422") || !strings.Contains(low, "cannot be merged") {
		t.Errorf("terminal 422 error must carry the 422 signal for classification: %v", err)
	}
}

func TestMerge_Alternating405And422UsesOneBoundedBudget(t *testing.T) {
	mergeCalls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/21/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if mergeCalls%2 == 1 {
				return 405, map[string]string{"message": "405 Method Not Allowed"}
			}
			return 422, map[string]string{"message": "Branch cannot be merged"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/21": func(_ *http.Request) (int, any) {
			mr := openedMR(21, "tested-head")
			mr.HeadPipeline = mrHeadPipe{Source: "push"}
			return 200, mr
		},
	})
	if _, err := cli.Merge(context.Background(), testMergeArgs(21, "tested-head")); err == nil {
		t.Fatal("expected alternating not-ready responses to exhaust the 422 budget")
	}
	if want := branchMergeSettleAttempts * 2; mergeCalls != want {
		t.Fatalf("merge calls = %d, want %d", mergeCalls, want)
	}
	for _, req := range rt.requests {
		if req.Method == http.MethodPut && strings.HasSuffix(req.Path, "/merge") && !strings.Contains(req.Body, `"sha":"tested-head"`) {
			t.Fatalf("unbound merge retry: %s", req.Body)
		}
	}
}

// TestMerge_NonRetryableErrorReturnsImmediately verifies a non-405 error
// (e.g. 409 conflict) is NOT swallowed by the merge-readiness poll.
func TestMerge_NonRetryableErrorReturnsImmediately(t *testing.T) {
	calls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/8/merge": func(_ *http.Request) (int, any) {
			calls++
			return 409, map[string]any{"message": "409 Conflict"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/8": func(_ *http.Request) (int, any) {
			return 200, openedMR(8, "tested-head")
		},
	})
	if _, err := cli.Merge(context.Background(), testMergeArgs(8, "tested-head")); err == nil {
		t.Error("expected 409 conflict to return an error")
	}
	if calls != 1 {
		t.Errorf("non-405 error should not retry; got %d attempts", calls)
	}
}

func TestMerge_Recovery5xxPreservesTransientClassification(t *testing.T) {
	for _, failAt := range []string{"reconcile", "pipeline_list"} {
		t.Run(failAt, func(t *testing.T) {
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/26/merge": func(_ *http.Request) (int, any) {
					return 405, map[string]any{"message": "405 Method Not Allowed"}
				},
				"GET /api/v4/projects/services%2Floom-core/merge_requests/26": func(_ *http.Request) (int, any) {
					if failAt == "reconcile" {
						return 500, map[string]any{"message": "temporary GitLab failure"}
					}
					return 200, mrResponse{
						IID: 26, State: "opened", SHA: "tested-head", SourceBranch: "feat/recovery-500", TargetBranch: testTargetBranch,
						HeadPipeline: mrHeadPipe{ID: 2601, Status: "failed", Source: "merge_request_event"},
					}
				},
				"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
					return 500, map[string]any{"message": "temporary pipeline-list failure"}
				},
			})
			_, err := cli.Merge(context.Background(), testMergeArgs(26, "tested-head", "feat/recovery-500"))
			if err == nil {
				t.Fatal("expected recovery 500")
			}
			lower := strings.ToLower(err.Error())
			if strings.Contains(lower, "status 405") || strings.Contains(lower, "method not allowed") {
				t.Fatalf("recovery error retained stale merge signal: %v", err)
			}
			if got := pipeline.Classify(err); got != pipeline.ClassTransient {
				t.Fatalf("Classify(%v) = %s, want transient", err, got)
			}
		})
	}
}

func TestMerge_ReconcilesAlreadyMergedAfterLostResponse(t *testing.T) {
	// A global retry setting must not make the shared HTTP transport replay the
	// merge PUT behind the identity-aware state machine's back.
	t.Setenv("HTTP_RETRIES", "1")
	for _, status := range []int{405, 500} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			mergeCalls := 0
			getCalls := 0
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"PUT /api/v4/projects/services%2Floom-core/merge_requests/23/merge": func(_ *http.Request) (int, any) {
					mergeCalls++
					return status, map[string]any{"message": "merge response lost"}
				},
				"GET /api/v4/projects/services%2Floom-core/merge_requests/23": func(_ *http.Request) (int, any) {
					getCalls++
					if getCalls == 1 {
						return 200, openedMR(23, "tested-head")
					}
					return 200, mergedMR(23, "tested-head", "authoritative-merge")
				},
			})
			resp, err := cli.Merge(context.Background(), testMergeArgs(23, "tested-head"))
			if err != nil || resp.MergedSHA != "authoritative-merge" {
				t.Fatalf("merge reconciliation = %+v, %v", resp, err)
			}
			if mergeCalls != 1 {
				t.Fatalf("merge calls = %d, want 1", mergeCalls)
			}
		})
	}
}

func TestMerge_ClosedMRIsNeverReopened(t *testing.T) {
	mergeCalls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/24/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 405, map[string]any{"message": "MR is closed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/24": func(_ *http.Request) (int, any) {
			mr := openedMR(24, "tested-head")
			mr.State = "closed"
			return 200, mr
		},
	})
	_, err := cli.Merge(context.Background(), testMergeArgs(24, "tested-head"))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "closed") {
		t.Fatalf("closed MR error = %v, want terminal closed error", err)
	}
	if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify(closed MR) = %s, want config", got)
	}
	if mergeCalls != 0 {
		t.Fatalf("merge calls = %d, want 0 while MR is already closed", mergeCalls)
	}
	for _, req := range rt.requests {
		if req.Method == http.MethodPut && !strings.HasSuffix(req.Path, "/merge") {
			t.Fatalf("closed MR was mutated: %s %s %s", req.Method, req.Path, req.Body)
		}
	}
}

// A detached merge_request_event head may only be recovered by creating and
// polling a same-SHA API pipeline. The recovery never deletes a pipeline or
// closes/reopens the MR.
func TestMerge_RecoversDetachedHeadWithSameSHASuperseder(t *testing.T) {
	mergeCalls := 0
	pollCalls := 0
	superseded := false
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/9/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if superseded {
				return 200, mergedMR(9, "feedface", "feedface", "feat/safe-recovery")
			}
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/9": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 9, State: "opened", SHA: "feedface", SourceBranch: "feat/safe-recovery", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 9001, Status: "failed", Source: "merge_request_event"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/9002") {
				pollCalls++
				return 200, shaPipeline{ID: 9002, SHA: "feedface", Ref: "feat/safe-recovery", Status: "success", Source: "api"}
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			superseded = true
			// GitLab's create response omits source; the by-ID GET supplies it.
			return 201, shaPipeline{ID: 9002, SHA: "feedface", Ref: "feat/safe-recovery", Status: "created"}
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(9, "feedface", "feat/safe-recovery"))
	if err != nil || resp.MergedSHA != "feedface" {
		t.Fatalf("detached-head recovery = %+v, %v", resp, err)
	}
	if mergeCalls != 2 || pollCalls == 0 {
		t.Fatalf("merge/poll calls = %d/%d, want 2/>0", mergeCalls, pollCalls)
	}
	var createdRef string
	for _, req := range rt.requests {
		if req.Method == http.MethodDelete || (req.Method == http.MethodPut && !strings.HasSuffix(req.Path, "/merge")) {
			t.Fatalf("unsafe detached-head mutation: %s %s", req.Method, req.Path)
		}
		if req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/pipeline") {
			var body struct {
				Ref string `json:"ref"`
			}
			if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
				t.Fatalf("decode create-pipeline body: %v", err)
			}
			createdRef = body.Ref
		}
	}
	if createdRef != "feat/safe-recovery" {
		t.Fatalf("superseding ref = %q, want feat/safe-recovery", createdRef)
	}
}

func TestMerge_DetachedHeadRecoveryCanOutliveOrdinarySettleDeadline(t *testing.T) {
	mergeCalls := 0
	pollCalls := 0
	postCalls := 0
	pipelineGreen := false
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/49/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if pipelineGreen {
				return 200, mergedMR(49, "tested-head", "merged-after-long-pipeline", "feat/long-recovery")
			}
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/49": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 49, State: "opened", SHA: "tested-head", SourceBranch: "feat/long-recovery", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 4901, Status: "failed", Source: "merge_request_event"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/4902") {
				pollCalls++
				status := "running"
				if pollCalls >= 8 {
					status = "success"
					pipelineGreen = true
				}
				return 200, shaPipeline{ID: 4902, SHA: "tested-head", Ref: "feat/long-recovery", Status: status, Source: "api"}
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			postCalls++
			return 201, shaPipeline{ID: 4902, SHA: "tested-head", Ref: "feat/long-recovery", Status: "created"}
		},
	})
	cli.mergeSettleTimeout = 25 * time.Millisecond
	cli.cfg.PollDeadline = 200 * time.Millisecond
	cli.cfg.PollInterval = 5 * time.Millisecond

	started := time.Now()
	resp, err := cli.Merge(context.Background(), testMergeArgs(49, "tested-head", "feat/long-recovery"))
	if err != nil || resp.MergedSHA != "merged-after-long-pipeline" {
		t.Fatalf("long detached-head recovery = %+v, %v", resp, err)
	}
	if elapsed := time.Since(started); elapsed <= cli.mergeSettleTimeout {
		t.Fatalf("recovery elapsed %s, want it to outlive ordinary settle deadline %s", elapsed, cli.mergeSettleTimeout)
	}
	if mergeCalls != 2 || pollCalls < 8 || postCalls != 1 {
		t.Fatalf("merge/poll/post calls = %d/%d/%d, want 2/>=8/1", mergeCalls, pollCalls, postCalls)
	}
}

func TestMerge_RunningSupersederTimeoutIsRetryableAndFencedRestartReattaches(t *testing.T) {
	mergeCalls := 0
	postCalls := 0
	created := false
	allowPipelineSuccess := false
	pipelineGreen := false
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/50/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if pipelineGreen {
				return 200, mergedMR(50, "tested-head", "merged-after-fenced-reattach", "feat/poll-timeout")
			}
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/50": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 50, State: "opened", SHA: "tested-head", SourceBranch: "feat/poll-timeout", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 5001, Status: "failed", Source: "merge_request_event"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/5002") {
				status := "running"
				if allowPipelineSuccess {
					status = "success"
					pipelineGreen = true
				}
				return 200, shaPipeline{ID: 5002, SHA: "tested-head", Ref: "feat/poll-timeout", Status: status, Source: "api"}
			}
			if created {
				return 200, []shaPipeline{{ID: 5002, SHA: "tested-head", Ref: "feat/poll-timeout", Status: "running", Source: "api"}}
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			postCalls++
			created = true
			return 201, shaPipeline{ID: 5002, SHA: "tested-head", Ref: "feat/poll-timeout", Status: "created"}
		},
	})
	cli.mergeSettleTimeout = 40 * time.Millisecond
	cli.cfg.PollDeadline = 30 * time.Millisecond
	cli.cfg.PollInterval = 5 * time.Millisecond

	_, err := cli.Merge(context.Background(), testMergeArgs(50, "tested-head", "feat/poll-timeout"))
	if !errors.Is(err, pipeline.ErrPipelinePollTimeout) {
		t.Fatalf("first recovery error = %v, want ErrPipelinePollTimeout", err)
	}
	if got := pipeline.Classify(err); got != pipeline.ClassInfra {
		t.Fatalf("Classify(first recovery) = %s, want infra", got)
	}
	if postCalls != 1 {
		t.Fatalf("first recovery POSTs = %d, want 1", postCalls)
	}

	allowPipelineSuccess = true
	args := testMergeArgs(50, "tested-head", "feat/poll-timeout")
	args.RecoveryPipelineCreateAttempted = true
	resp, err := cli.Merge(context.Background(), args)
	if err != nil || resp.MergedSHA != "merged-after-fenced-reattach" {
		t.Fatalf("fenced reattach = %+v, %v", resp, err)
	}
	if postCalls != 1 {
		t.Fatalf("pipeline create POSTs after reattach = %d, want exactly 1 total", postCalls)
	}
	if mergeCalls < 3 {
		t.Fatalf("merge calls = %d, want first failure plus retry merge", mergeCalls)
	}
}

func TestMerge_OrdinaryNetworkCallsUseSettleDeadline(t *testing.T) {
	for _, blockedMethod := range []string{http.MethodGet, http.MethodPut} {
		t.Run(blockedMethod, func(t *testing.T) {
			cli, _ := newGitLabStub(t, nil)
			cli.mergeSettleTimeout = 25 * time.Millisecond
			cli.cfg.PollDeadline = 250 * time.Millisecond
			cli.SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == blockedMethod {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				body, err := json.Marshal(openedMR(51, "tested-head", "feat/blocked-call"))
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(body)),
					Header:     make(http.Header),
				}, nil
			}))

			started := time.Now()
			_, err := cli.Merge(context.Background(), testMergeArgs(51, "tested-head", "feat/blocked-call"))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked %s error = %v, want deadline exceeded", blockedMethod, err)
			}
			if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
				t.Fatalf("blocked %s used pipeline recovery budget: elapsed=%s settle=%s poll=%s", blockedMethod, elapsed, cli.mergeSettleTimeout, cli.cfg.PollDeadline)
			}
		})
	}
}

// An API pipeline older than the pinned detached head cannot supersede it,
// even when ref/SHA/source all match and that old pipeline is green. Recovery
// must create and verify a newer API pipeline instead of repeatedly adopting
// the stale one across retries or restarts.
func TestMerge_OlderAPIPipelineDoesNotSupersedeNewerDetachedHead(t *testing.T) {
	mergeCalls := 0
	createCalls := 0
	oldPollCalls := 0
	freshGreen := false
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/26/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if freshGreen {
				return 200, mergedMR(26, "tested-head", "merged-after-fresh-api", "feat/exact-head")
			}
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/26": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 26, State: "opened", SHA: "tested-head", SourceBranch: "feat/exact-head", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 2605, Status: "failed", Source: "merge_request_event"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/2606") {
				freshGreen = true
				return 200, shaPipeline{ID: 2606, SHA: "tested-head", Ref: "feat/exact-head", Status: "success", Source: "api"}
			}
			if strings.HasSuffix(req.URL.Path, "/pipelines/2604") {
				oldPollCalls++
				return 200, shaPipeline{ID: 2604, SHA: "tested-head", Ref: "feat/exact-head", Status: "success", Source: "api"}
			}
			return 200, []shaPipeline{{ID: 2604, SHA: "tested-head", Ref: "feat/exact-head", Status: "success", Source: "api"}}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			createCalls++
			return 201, shaPipeline{ID: 2606, SHA: "tested-head", Ref: "feat/exact-head", Status: "created"}
		},
	})

	resp, err := cli.Merge(context.Background(), testMergeArgs(26, "tested-head", "feat/exact-head"))
	if err != nil || resp.MergedSHA != "merged-after-fresh-api" {
		t.Fatalf("detached recovery = %+v, %v", resp, err)
	}
	if mergeCalls != 2 || createCalls != 1 || oldPollCalls != 0 {
		t.Fatalf("merge/create/old-poll calls = %d/%d/%d, want 2/1/0", mergeCalls, createCalls, oldPollCalls)
	}
}

// A restarted worker can lose the created pipeline ID from memory. GitLab's
// same-SHA list is the durable recovery record: rediscover the running API
// pipeline, poll it under PollDeadline, then retry the exact-SHA merge.
func TestMerge_RestartResumesRunningSameSHASuperseder(t *testing.T) {
	mergeCalls := 0
	pollCalls := 0
	createCalls := 0
	pipelineGreen := false
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/12/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if pipelineGreen {
				return 200, mergedMR(12, "beadf00d", "beadf00d", "feat/mills-x")
			}
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/12": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 12, State: "opened", SHA: "beadf00d", SourceBranch: "feat/mills-x", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 1202, Status: "running", Source: "api"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/1202") {
				pollCalls++
				status := "running"
				if pollCalls >= 2 {
					status = "success"
					pipelineGreen = true
				}
				return 200, shaPipeline{ID: 1202, SHA: "beadf00d", Ref: "feat/mills-x", Status: status, Source: "api"}
			}
			return 200, []shaPipeline{{ID: 1202, SHA: "beadf00d", Ref: "feat/mills-x", Status: "running", Source: "api"}}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			createCalls++
			return 500, map[string]any{"message": "must not create another pipeline"}
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(12, "beadf00d", "feat/mills-x"))
	if err != nil || resp.MergedSHA != "beadf00d" {
		t.Fatalf("restart recovery = %+v, %v", resp, err)
	}
	if mergeCalls != 2 || pollCalls < 2 || createCalls != 0 {
		t.Fatalf("merge/poll/create calls = %d/%d/%d, want 2/>=2/0", mergeCalls, pollCalls, createCalls)
	}
}

func TestMerge_APIHeadListVisibilitySettlesWithinSharedBudget(t *testing.T) {
	mergeCalls := 0
	listCalls := 0
	createCalls := 0
	pipelineGreen := false
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/27/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if pipelineGreen {
				return 200, mergedMR(27, "tested-head", "merged-after-list-settle", "feat/list-settle")
			}
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/27": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 27, State: "opened", SHA: "tested-head", SourceBranch: "feat/list-settle", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 2702, Status: "running", Source: "api"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/2702") {
				pipelineGreen = true
				return 200, shaPipeline{ID: 2702, SHA: "tested-head", Ref: "feat/list-settle", Status: "success", Source: "api"}
			}
			listCalls++
			if listCalls == 1 {
				return 200, []shaPipeline{}
			}
			return 200, []shaPipeline{{ID: 2702, SHA: "tested-head", Ref: "feat/list-settle", Status: "running", Source: "api"}}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			createCalls++
			return 500, map[string]any{"message": "must not duplicate superseder"}
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(27, "tested-head", "feat/list-settle"))
	if err != nil || resp.MergedSHA != "merged-after-list-settle" {
		t.Fatalf("API head settle = %+v, %v", resp, err)
	}
	if mergeCalls != 3 || listCalls != 2 || createCalls != 0 {
		t.Fatalf("merge/list/create calls = %d/%d/%d, want 3/2/0", mergeCalls, listCalls, createCalls)
	}
}

func TestMerge_SupersedeCreateFailureFailsClosedWithoutMRMutation(t *testing.T) {
	mergeCalls := 0
	createCalls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/13/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/13": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 13, State: "opened", SHA: "deadbea7", SourceBranch: "feat/psl-runbook-1", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 1301, Status: "failed", Source: "merge_request_event"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			createCalls++
			return 400, map[string]any{"message": "Project include not found"}
		},
	})
	if _, err := cli.Merge(context.Background(), testMergeArgs(13, "deadbea7", "feat/psl-runbook-1")); err == nil {
		t.Fatal("expected superseder creation failure to remain terminal")
	} else if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify(create pipeline 400) = %s, want config", got)
	}
	if mergeCalls != 1 || createCalls != 1 {
		t.Fatalf("merge/create calls = %d/%d, want 1/1", mergeCalls, createCalls)
	}
	for _, req := range rt.requests {
		if req.Method == http.MethodDelete || (req.Method == http.MethodPut && !strings.HasSuffix(req.Path, "/merge")) {
			t.Fatalf("failed recovery mutated MR or pipeline: %s %s", req.Method, req.Path)
		}
	}
}

func TestMerge_ConfirmedPipelineCreate429IsTerminalWithDurableFence(t *testing.T) {
	postCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/52": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 52, State: "opened", SHA: "tested-head", SourceBranch: "feat/rate-limited-create", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 5201, Status: "failed", Source: "merge_request_event"},
			}
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/52/merge": func(_ *http.Request) (int, any) {
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			postCalls++
			return http.StatusTooManyRequests, map[string]any{"message": "rate limit"}
		},
	})

	_, err := cli.Merge(context.Background(), testMergeArgs(52, "tested-head", "feat/rate-limited-create"))
	if err == nil {
		t.Fatal("expected confirmed create rejection")
	}
	if got := pipeline.Classify(err); got != pipeline.ClassConfig || !pipeline.IsTerminal(got) {
		t.Fatalf("Classify(confirmed 429) = %s terminal=%v, want terminal config: %v", got, pipeline.IsTerminal(got), err)
	}
	if postCalls != 1 {
		t.Fatalf("pipeline create POSTs = %d, want exactly 1", postCalls)
	}
}

func TestMerge_SourceMovesDuringSupersederCreationBlocks(t *testing.T) {
	mergeCalls := 0
	pollCalls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/25/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/25": func(_ *http.Request) (int, any) {
			return 200, mrResponse{
				IID: 25, State: "opened", SHA: "tested-head", SourceBranch: "feat/moving", TargetBranch: testTargetBranch,
				HeadPipeline: mrHeadPipe{ID: 2501, Status: "failed", Source: "merge_request_event"},
			}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.Contains(req.URL.Path, "/pipelines/") {
				pollCalls++
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			return 201, shaPipeline{ID: 2502, SHA: "moved-head", Ref: "feat/moving", Status: "created"}
		},
	})
	_, err := cli.Merge(context.Background(), testMergeArgs(25, "tested-head", "feat/moving"))
	if err == nil || !strings.Contains(err.Error(), "moved-head") {
		t.Fatalf("source-movement error = %v, want SHA mismatch", err)
	}
	if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify(source movement) = %s, want config", got)
	}
	if mergeCalls != 1 || pollCalls != 0 {
		t.Fatalf("merge/poll calls = %d/%d, want 1/0", mergeCalls, pollCalls)
	}
	for _, req := range rt.requests {
		if req.Method == http.MethodPut && strings.HasSuffix(req.Path, "/merge") && !strings.Contains(req.Body, `"sha":"tested-head"`) {
			t.Fatalf("unbound merge request: %s", req.Body)
		}
	}
}

func TestMerge_RetargetDuringSupersederPollBlocksRetryPUT(t *testing.T) {
	getCalls := 0
	mergeCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/40": func(_ *http.Request) (int, any) {
			getCalls++
			mr := openedMR(40, "tested-head", "feat/superseder-retarget")
			mr.HeadPipeline = mrHeadPipe{ID: 4001, Status: "failed", Source: "merge_request_event"}
			if getCalls >= 3 {
				mr.TargetBranch = "release"
			}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/40/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/4002") {
				return 200, shaPipeline{ID: 4002, SHA: "tested-head", Ref: "feat/superseder-retarget", Status: "success", Source: "api"}
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			return 201, shaPipeline{ID: 4002, SHA: "tested-head", Ref: "feat/superseder-retarget", Status: "created"}
		},
	})
	_, err := cli.Merge(context.Background(), testMergeArgs(40, "tested-head", "feat/superseder-retarget"))
	if err == nil || !strings.Contains(err.Error(), "target branch") {
		t.Fatalf("retarget during superseder poll error = %v", err)
	}
	if got := pipeline.Classify(err); got != pipeline.ClassConfig {
		t.Fatalf("Classify(%v) = %s, want config", err, got)
	}
	if mergeCalls != 1 {
		t.Fatalf("merge PUT calls = %d, want only the pre-retarget attempt", mergeCalls)
	}
}

func TestMerge_SupersederPollUsesSharedDeadlineAndNeverPUTsAfterExpiry(t *testing.T) {
	mergeCalls := 0
	pollCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/41": func(_ *http.Request) (int, any) {
			mr := openedMR(41, "tested-head", "feat/shared-deadline")
			mr.HeadPipeline = mrHeadPipe{ID: 4101, Status: "failed", Source: "merge_request_event"}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/41/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/4102") {
				pollCalls++
				return 200, shaPipeline{ID: 4102, SHA: "tested-head", Ref: "feat/shared-deadline", Status: "running", Source: "api"}
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			return 201, shaPipeline{ID: 4102, SHA: "tested-head", Ref: "feat/shared-deadline", Status: "created"}
		},
	})
	cli.mergeRetryTimeout = 45 * time.Millisecond
	cli.cfg.PollInterval = 5 * time.Millisecond
	cli.cfg.PollDeadline = time.Second
	started := time.Now()
	_, err := cli.Merge(context.Background(), testMergeArgs(41, "tested-head", "feat/shared-deadline"))
	if err == nil || !isMergeNotReady(err) {
		t.Fatalf("shared-deadline superseder error = %v, want original not-ready error", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("superseder polling exceeded merge deadline: %s", elapsed)
	}
	if mergeCalls != 1 || pollCalls < 1 {
		t.Fatalf("merge/poll calls = %d/%d, want 1/>=1", mergeCalls, pollCalls)
	}
}

func TestMerge_AmbiguousPipelineCreateReconcilesBeforeAnotherPOST(t *testing.T) {
	mergeCalls := 0
	listCalls := 0
	postCalls := 0
	created := false
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/46": func(_ *http.Request) (int, any) {
			mr := openedMR(46, "tested-head", "feat/ambiguous-create")
			mr.HeadPipeline = mrHeadPipe{ID: 4601, Status: "failed", Source: "merge_request_event"}
			if created {
				mr.HeadPipeline = mrHeadPipe{ID: 4602, Status: "success", Source: "api"}
			}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/46/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if mergeCalls == 1 {
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			return 200, mergedMR(46, "tested-head", "merged-after-ambiguous-create", "feat/ambiguous-create")
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/4602") {
				return 200, shaPipeline{ID: 4602, SHA: "tested-head", Ref: "feat/ambiguous-create", Status: "success", Source: "api"}
			}
			listCalls++
			if created && listCalls >= 3 {
				return 200, []shaPipeline{{ID: 4602, SHA: "tested-head", Ref: "feat/ambiguous-create", Status: "success", Source: "api"}}
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			postCalls++
			created = true
			return 500, map[string]any{"message": "response lost after create"}
		},
	})
	cli.mergeRetryTimeout = 250 * time.Millisecond
	cli.cfg.PollInterval = 5 * time.Millisecond
	resp, err := cli.Merge(context.Background(), testMergeArgs(46, "tested-head", "feat/ambiguous-create"))
	if err != nil || resp.MergedSHA != "merged-after-ambiguous-create" {
		t.Fatalf("ambiguous create reconciliation = %+v, %v", resp, err)
	}
	if postCalls != 1 {
		t.Fatalf("create pipeline POSTs = %d, want exactly 1", postCalls)
	}
}

func TestMerge_PersistedPipelineCreateFenceBlocksDuplicatePOST(t *testing.T) {
	postCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/47": func(_ *http.Request) (int, any) {
			mr := openedMR(47, "tested-head", "feat/fenced-create")
			mr.HeadPipeline = mrHeadPipe{ID: 4701, Status: "failed", Source: "merge_request_event"}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/47/merge": func(_ *http.Request) (int, any) {
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(_ *http.Request) (int, any) {
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			postCalls++
			return 201, shaPipeline{ID: 4702, SHA: "tested-head", Ref: "feat/fenced-create"}
		},
	})
	args := testMergeArgs(47, "tested-head", "feat/fenced-create")
	args.RecoveryPipelineCreateAttempted = true
	cli.mergeRetryTimeout = 35 * time.Millisecond
	cli.cfg.PollInterval = 5 * time.Millisecond
	started := time.Now()
	_, err := cli.Merge(context.Background(), args)
	if err == nil || pipeline.Classify(err) != pipeline.ClassConfig {
		t.Fatalf("persisted create fence error = %v, class=%s; want bounded config error", err, pipeline.Classify(err))
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("persisted create fence did not respect merge deadline: %s", elapsed)
	}
	if postCalls != 0 {
		t.Fatalf("pipeline create POSTs = %d, want 0 after durable fence", postCalls)
	}
}

func TestMerge_PersistedPipelineCreateFenceWaitsForListVisibility(t *testing.T) {
	listCalls := 0
	postCalls := 0
	mergeCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/48": func(_ *http.Request) (int, any) {
			mr := openedMR(48, "tested-head", "feat/fenced-lag")
			mr.HeadPipeline = mrHeadPipe{ID: 4801, Status: "failed", Source: "merge_request_event"}
			return 200, mr
		},
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/48/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if mergeCalls == 1 {
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			mr := mergedMR(48, "tested-head", "merged-after-fenced-lag")
			mr.SourceBranch = "feat/fenced-lag"
			return 200, mr
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines": func(req *http.Request) (int, any) {
			if strings.HasSuffix(req.URL.Path, "/pipelines/4802") {
				return 200, shaPipeline{ID: 4802, SHA: "tested-head", Ref: "feat/fenced-lag", Status: "success", Source: "api"}
			}
			listCalls++
			if listCalls >= 2 {
				return 200, []shaPipeline{{ID: 4802, SHA: "tested-head", Ref: "feat/fenced-lag", Status: "success", Source: "api"}}
			}
			return 200, []shaPipeline{}
		},
		"POST /api/v4/projects/services%2Floom-core/pipeline": func(_ *http.Request) (int, any) {
			postCalls++
			return 201, shaPipeline{ID: 4803, SHA: "tested-head", Ref: "feat/fenced-lag", Source: "api"}
		},
	})
	cli.mergeRetryTimeout = 250 * time.Millisecond
	cli.cfg.PollInterval = 5 * time.Millisecond
	args := testMergeArgs(48, "tested-head", "feat/fenced-lag")
	args.RecoveryPipelineCreateAttempted = true
	resp, err := cli.Merge(context.Background(), args)
	if err != nil || resp.MergedSHA != "merged-after-fenced-lag" {
		t.Fatalf("restart list-lag reconciliation = %+v, %v", resp, err)
	}
	if listCalls < 2 {
		t.Fatalf("pipeline list calls = %d, want delayed rediscovery", listCalls)
	}
	if postCalls != 0 {
		t.Fatalf("pipeline create POSTs = %d, want 0 with persisted fence", postCalls)
	}
}

func TestPollSupersedingPipeline_ParentCancellationPreserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/pipelines/4202": func(_ *http.Request) (int, any) {
			cancel()
			return 200, shaPipeline{ID: 4202, SHA: "tested-head", Ref: "feat/cancel", Status: "running", Source: "api"}
		},
	})
	_, err := cli.pollPipelineToSuccess(ctx, 4202, "tested-head", "feat/cancel", time.Now().Add(time.Second))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("superseder parent cancellation = %v, want context.Canceled", err)
	}
	if errors.Is(err, pipeline.ErrPipelinePollTimeout) {
		t.Fatalf("parent cancellation was converted to pipeline timeout: %v", err)
	}
}

func TestMerge_BenignTiming405DoesNotMutateMR(t *testing.T) {
	mergeCalls := 0
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/4/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			if mergeCalls >= 2 {
				return 200, mergedMR(4, "cafebabe", "cafebabe")
			}
			return 405, map[string]any{"message": "405 Method Not Allowed"}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/4": func(_ *http.Request) (int, any) {
			mr := openedMR(4, "cafebabe")
			mr.HeadPipeline = mrHeadPipe{ID: 4000, Status: "success", Source: "push"}
			return 200, mr
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(4, "cafebabe"))
	if err != nil || resp.MergedSHA != "cafebabe" {
		t.Fatalf("timing recovery = %+v, %v", resp, err)
	}
	for _, req := range rt.requests {
		if req.Method == http.MethodPut && !strings.HasSuffix(req.Path, "/merge") {
			t.Fatalf("timing recovery mutated MR: %s %s", req.Method, req.Path)
		}
	}
}

// ----- Cleanup -----

func TestCleanup_DeletesSourceBranch(t *testing.T) {
	called := false
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"DELETE /api/v4/projects/services%2Floom-core/repository/branches/feat%2Fx": func(_ *http.Request) (int, any) {
			called = true
			return 204, nil
		},
	})
	resp, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{Project: testGitLabProject, BranchName: "feat/x"})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !called {
		t.Error("DELETE not called")
	}
	if !strings.Contains(resp.LogTail, "deleted") {
		t.Errorf("log tail = %q", resp.LogTail)
	}
}

func TestCleanup_404IsSuccess(t *testing.T) {
	cli, _ := newGitLabStub(t, nil) // default 404 for everything
	resp, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{Project: testGitLabProject, BranchName: "feat/gone"})
	if err != nil {
		t.Errorf("cleanup should swallow 404: %v", err)
	}
	if !strings.Contains(resp.LogTail, "already removed") {
		t.Errorf("log tail = %q", resp.LogTail)
	}
}

func TestCleanup_DeletesOnlyVerifiedClosedHusk(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mr         mrResponse
		wantDelete bool
	}{
		{"closed unmerged", mrResponse{IID: 71, State: "closed", SourceBranch: "feat/husk", TargetBranch: "main"}, true},
		{"opened", mrResponse{IID: 71, State: "opened", SourceBranch: "feat/husk", TargetBranch: "main"}, false},
		{"merged", mrResponse{IID: 71, State: "merged", SourceBranch: "feat/husk", TargetBranch: "main"}, false},
		{"source mismatch", mrResponse{IID: 71, State: "closed", SourceBranch: "feat/other", TargetBranch: "main"}, false},
		{"target mismatch", mrResponse{IID: 71, State: "closed", SourceBranch: "feat/husk", TargetBranch: "release"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted := false
			cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
				"GET /api/v4/projects/services%2Floom-core/merge_requests/71": func(_ *http.Request) (int, any) { return 200, tc.mr },
				"DELETE /api/v4/projects/services%2Floom-core/repository/branches/feat%2Fhusk": func(_ *http.Request) (int, any) {
					deleted = true
					return 204, nil
				},
			})
			_, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{
				Project: testGitLabProject, MRIID: 71, BranchName: "feat/husk", TargetBranch: "main",
			})
			if err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			if deleted != tc.wantDelete {
				t.Fatalf("deleted = %v, want %v", deleted, tc.wantDelete)
			}
		})
	}
}

func TestCleanup_ClosedHuskDelete404IsSuccess(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/71": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 71, State: "closed", SourceBranch: "feat/gone", TargetBranch: "main"}
		},
	})
	resp, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{Project: testGitLabProject, MRIID: 71, BranchName: "feat/gone", TargetBranch: "main"})
	if err != nil || !strings.Contains(resp.LogTail, "already removed") {
		t.Fatalf("closed-husk 404 cleanup = %+v, %v", resp, err)
	}
}

func TestCleanup_NoBranchIsNoOp(t *testing.T) {
	cli, rt := newGitLabStub(t, nil)
	if _, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(rt.requests) != 0 {
		t.Errorf("no branch should mean no HTTP call, got %d", len(rt.requests))
	}
}

func TestCleanup_ProjectMismatchMakesNoHTTPRequest(t *testing.T) {
	cli, rt := newGitLabStub(t, nil)
	_, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{
		Project:    "services/other",
		BranchName: "feat/same-name",
	})
	if err == nil || !errors.Is(err, pipeline.ErrMergeAuthorizationStale) {
		t.Fatalf("cleanup project mismatch error = %v", err)
	}
	if len(rt.requests) != 0 {
		t.Fatalf("cleanup project mismatch made %d HTTP requests", len(rt.requests))
	}
}

// TestCleanup_400ReferenceUpdateFailedIsSuccess guards the cleanup-stage fix
// (live 2026-07-16: DELETE branch 400 "reference update failed" escalated a
// merged run ×6). Cleanup runs after a successful merge and is best-effort, so
// a 400 delete must be logged and swallowed, never fail the run's terminal
// stage.
func TestCleanup_400ReferenceUpdateFailedIsSuccess(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"DELETE /api/v4/projects/services%2Floom-core/repository/branches/feat%2Fgone": func(_ *http.Request) (int, any) {
			return 400, map[string]string{"message": "reference update failed"}
		},
	})
	resp, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{Project: testGitLabProject, BranchName: "feat/gone"})
	if err != nil {
		t.Fatalf("cleanup should swallow branch-delete 400: %v", err)
	}
	if !strings.Contains(resp.LogTail, "best-effort") {
		t.Errorf("log tail should note the best-effort skip: %q", resp.LogTail)
	}
}

// TestCleanup_500SurfacesError: the best-effort carve-out is scoped to 400/404;
// a genuinely broken cleanup (auth, 5xx) still surfaces so it's visible.
func TestCleanup_500SurfacesError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"DELETE /api/v4/projects/services%2Floom-core/repository/branches/feat%2Fx": func(_ *http.Request) (int, any) {
			return 500, map[string]string{"message": "internal error"}
		},
	})
	if _, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{Project: testGitLabProject, BranchName: "feat/x"}); err == nil {
		t.Error("cleanup should surface non-400/404 delete errors")
	}
}

// ----- CreateIssue -----

func TestCreateIssue_PostsAndJoinsLabels(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/issues": func(_ *http.Request) (int, any) {
			return 201, issueResponse{IID: 7, WebURL: "https://gitlab/services/loom-core/-/issues/7"}
		},
	})
	resp, err := cli.CreateIssue(context.Background(), pipeline.IssueRequest{
		Title:       "mills escalation",
		Description: "boom",
		Labels:      []string{"mills-escalation", "priority/P1"},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if resp.IID != 7 {
		t.Errorf("iid = %d", resp.IID)
	}
	if len(rt.requests) != 1 {
		t.Fatalf("requests = %d", len(rt.requests))
	}
	var body createIssueBody
	_ = json.Unmarshal([]byte(rt.requests[0].Body), &body)
	if body.Labels != "mills-escalation,priority/P1" {
		t.Errorf("labels CSV wrong: %q", body.Labels)
	}
}

// ----- Numeric project id is passed through -----

func TestProjectPath_NumericIDPassthrough(t *testing.T) {
	c, err := NewGitLabClient(GitLabConfig{APIURL: "x", Token: "y", Project: "47"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.projectPath(); got != "47" {
		t.Errorf("projectPath = %q, want 47", got)
	}
}

func TestProjectPath_SlugPathEncoded(t *testing.T) {
	c, err := NewGitLabClient(GitLabConfig{APIURL: "x", Token: "y", Project: "services/loom-core"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.projectPath(); got != "services%2Floom-core" {
		t.Errorf("projectPath = %q", got)
	}
}

// ----- ListIssues -----

func TestListIssues_BuildsQueryAndDecodes(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(req *http.Request) (int, any) {
			// Round-trip the query so the assertions below see what
			// the client sent.
			q := req.URL.Query()
			if got := q.Get("labels"); got != "mills-eligible" {
				return 400, map[string]string{"message": "wrong labels: " + got}
			}
			if got := q.Get("state"); got != "opened" {
				return 400, map[string]string{"message": "wrong state: " + got}
			}
			if got := q.Get("per_page"); got != "100" {
				return 400, map[string]string{"message": "wrong per_page: " + got}
			}
			return 200, []IssueListItem{
				{IID: 12, ProjectID: 47, Title: "fix flake", State: "opened",
					Labels:      []string{"mills-eligible", "priority:P1"},
					Description: "repro"},
			}
		},
	})
	got, err := cli.ListIssues(context.Background(), ListIssuesOpts{
		Labels:  []string{"mills-eligible"},
		State:   "opened",
		PerPage: 100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got))
	}
	if got[0].IID != 12 || got[0].ProjectID != 47 {
		t.Errorf("unexpected issue: %+v", got[0])
	}
	if len(rt.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(rt.requests))
	}
	if rt.requests[0].Token != "tok-123" {
		t.Errorf("token = %q", rt.requests[0].Token)
	}
}

func TestListIssues_EmptyOptsOmitsQuery(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(req *http.Request) (int, any) {
			if req.URL.RawQuery != "" {
				return 400, map[string]string{"message": "expected no query, got " + req.URL.RawQuery}
			}
			return 200, []IssueListItem{}
		},
	})
	if _, err := cli.ListIssues(context.Background(), ListIssuesOpts{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rt.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(rt.requests))
	}
}

func TestListIssues_PropagatesServerError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects": func(_ *http.Request) (int, any) {
			return 500, map[string]string{"message": "boom"}
		},
	})
	_, err := cli.ListIssues(context.Background(), ListIssuesOpts{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("want status in error, got %v", err)
	}
}

// ----- FindOpenEscalation / CommentIssue (DEBT-073 #167 escalation dedup) -----

func TestFindOpenEscalation_MatchesMarker(t *testing.T) {
	const backlog = "BL-DEDUP-1"
	marker := pipeline.EscalationClassDedupMarker(backlog, string(pipeline.FailureCode))
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(req *http.Request) (int, any) {
			q := req.URL.Query()
			if got := q.Get("labels"); got != "mills-escalation" {
				return 400, map[string]string{"message": "wrong labels: " + got}
			}
			if got := q.Get("state"); got != "opened" {
				return 400, map[string]string{"message": "wrong state: " + got}
			}
			// Newest-first: a different-item escalation plus the target one.
			return 200, []IssueListItem{
				{IID: 5, State: "opened", Description: "unrelated\n" + pipeline.EscalationDedupMarker("BL-OTHER")},
				{IID: 8, State: "opened", WebURL: "https://gl/issues/8", Description: "## Pipeline Escalation\n" + marker + "\n"},
			}
		},
	})
	ref, found, err := cli.FindOpenEscalationByClass(context.Background(), backlog, string(pipeline.FailureCode))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found {
		t.Fatal("expected to find the marker-bearing open issue")
	}
	if ref.IID != 8 || ref.URL != "https://gl/issues/8" {
		t.Errorf("matched wrong issue: %+v", ref)
	}
}

func TestFindOpenEscalation_LegacyContractFindsClassSpecificIssue(t *testing.T) {
	const backlog = "BL-LEGACY-CALLER"
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(_ *http.Request) (int, any) {
			return 200, []IssueListItem{{
				IID: 9, WebURL: "https://gl/issues/9",
				Description: pipeline.EscalationClassDedupMarker(backlog, string(pipeline.FailureCode)),
			}}
		},
	})
	ref, found, err := cli.FindOpenEscalation(context.Background(), backlog)
	if err != nil || !found || ref.IID != 9 {
		t.Fatalf("legacy lookup = (%+v, %v, %v), want class-specific issue 9", ref, found, err)
	}
}

func TestFindOpenEscalation_DoesNotConflateFailureClasses(t *testing.T) {
	const backlog = "BL-DEDUP-CLASS"
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(_ *http.Request) (int, any) {
			return 200, []IssueListItem{
				{IID: 7, WebURL: "https://gl/issues/7", Description: pipeline.EscalationClassDedupMarker(backlog, string(pipeline.FailureConfiguration))},
				{IID: 8, WebURL: "https://gl/issues/8", Description: pipeline.EscalationClassDedupMarker(backlog, string(pipeline.FailureCode))},
			}
		},
	})
	ref, found, err := cli.FindOpenEscalationByClass(context.Background(), backlog, string(pipeline.FailureCode))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found || ref.IID != 8 {
		t.Fatalf("class-aware lookup = (%+v, %v), want code issue 8", ref, found)
	}
}

func TestFindOpenEscalation_EmptyClassMatchesLegacyOnly(t *testing.T) {
	const backlog = "BL-DEDUP-LEGACY"
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(_ *http.Request) (int, any) {
			return 200, []IssueListItem{
				{IID: 7, WebURL: "https://gl/issues/7", Description: pipeline.EscalationClassDedupMarker(backlog, string(pipeline.FailureCode))},
				{IID: 8, WebURL: "https://gl/issues/8", Description: pipeline.EscalationDedupMarker(backlog)},
			}
		},
	})
	ref, found, err := cli.FindOpenEscalationByClass(context.Background(), backlog, "")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found || ref.IID != 8 {
		t.Fatalf("legacy lookup = (%+v, %v), want legacy issue 8", ref, found)
	}
}

func TestListOpenEscalations_MatchesLegacyAndEveryClass(t *testing.T) {
	const backlog = "BL-RESOLVE-ALL"
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(_ *http.Request) (int, any) {
			return 200, []IssueListItem{
				{IID: 5, Description: pipeline.EscalationDedupMarker("BL-OTHER")},
				{IID: 6, WebURL: "https://gl/issues/6", Description: pipeline.EscalationDedupMarker(backlog)},
				{IID: 7, WebURL: "https://gl/issues/7", Description: pipeline.EscalationClassDedupMarker(backlog, string(pipeline.FailureCode))},
				{IID: 8, WebURL: "https://gl/issues/8", Description: pipeline.EscalationClassDedupMarker(backlog, string(pipeline.FailureConfiguration))},
			}
		},
	})
	refs, err := cli.ListOpenEscalations(context.Background(), backlog)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := refs, []pipeline.IssueRef{{IID: 6, URL: "https://gl/issues/6"}, {IID: 7, URL: "https://gl/issues/7"}, {IID: 8, URL: "https://gl/issues/8"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
}

func TestListOpenEscalations_EmptyBacklogShortCircuits(t *testing.T) {
	cli, rt := newGitLabStub(t, nil)
	refs, err := cli.ListOpenEscalations(context.Background(), "  ")
	if err != nil || len(refs) != 0 {
		t.Fatalf("empty backlog list = (%v, %v), want empty nil", refs, err)
	}
	if len(rt.requests) != 0 {
		t.Fatalf("empty backlog must not hit the API; got %d requests", len(rt.requests))
	}
}

func TestFindOpenEscalation_NoMatchReturnsNotFound(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(_ *http.Request) (int, any) {
			return 200, []IssueListItem{{IID: 1, State: "opened", Description: "no marker here"}}
		},
	})
	ref, found, err := cli.FindOpenEscalationByClass(context.Background(), "BL-NONE", string(pipeline.FailureCode))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Errorf("expected not found, got %+v", ref)
	}
}

func TestFindOpenEscalation_EmptyBacklogShortCircuits(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/issues": func(_ *http.Request) (int, any) {
			return 200, []IssueListItem{}
		},
	})
	if _, found, err := cli.FindOpenEscalationByClass(context.Background(), "  ", string(pipeline.FailureCode)); err != nil || found {
		t.Errorf("empty backlog must return (_, false, nil); got found=%v err=%v", found, err)
	}
	if len(rt.requests) != 0 {
		t.Errorf("empty backlog must not hit the API; got %d requests", len(rt.requests))
	}
}

func TestFindOpenEscalation_PropagatesListErrorSoEscalatorFailsOpen(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects": func(_ *http.Request) (int, any) {
			return 503, map[string]string{"message": "down"}
		},
	})
	if _, _, err := cli.FindOpenEscalationByClass(context.Background(), "BL-X", string(pipeline.FailureCode)); err == nil {
		t.Fatal("expected the list error to propagate so the escalator can fail open")
	}
}

func TestCommentIssue_PostsNote(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/issues/42/notes": func(_ *http.Request) (int, any) {
			return 201, map[string]any{"id": 1001}
		},
	})
	if err := cli.CommentIssue(context.Background(), 42, "recurred again"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if len(rt.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(rt.requests))
	}
	got := rt.requests[0]
	if got.Method != http.MethodPost || !strings.HasSuffix(got.Path, "/issues/42/notes") {
		t.Errorf("unexpected request: %s %s", got.Method, got.Path)
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Body != "recurred again" {
		t.Errorf("note body = %q", body.Body)
	}
}

func TestCommentIssue_RejectsZeroIID(t *testing.T) {
	cli, rt := newGitLabStub(t, nil)
	if err := cli.CommentIssue(context.Background(), 0, "x"); err == nil {
		t.Error("expected error for zero iid")
	}
	if len(rt.requests) != 0 {
		t.Errorf("must not hit the API on a zero iid; got %d", len(rt.requests))
	}
}

func TestCloseIssue_PutsStateEventClose(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/issues/88": func(_ *http.Request) (int, any) {
			return 200, map[string]any{"iid": 88, "state": "closed"}
		},
	})
	if err := cli.CloseIssue(context.Background(), 88); err != nil {
		t.Fatalf("close: %v", err)
	}
	if len(rt.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(rt.requests))
	}
	got := rt.requests[0]
	if got.Method != http.MethodPut || !strings.HasSuffix(got.Path, "/issues/88") {
		t.Errorf("unexpected request: %s %s", got.Method, got.Path)
	}
	var body struct {
		StateEvent string `json:"state_event"`
	}
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.StateEvent != "close" {
		t.Errorf("state_event = %q, want close", body.StateEvent)
	}
}

func TestCloseIssue_RejectsZeroIID(t *testing.T) {
	cli, rt := newGitLabStub(t, nil)
	if err := cli.CloseIssue(context.Background(), 0); err == nil {
		t.Error("expected error for zero iid")
	}
	if len(rt.requests) != 0 {
		t.Errorf("must not hit the API on a zero iid; got %d", len(rt.requests))
	}
}

// ----- compile-time interface assertions -----

var _ pipeline.GitLabClient = (*GitLabClient)(nil)
var _ pipeline.IssueClient = (*GitLabClient)(nil)
var _ pipeline.DedupIssueClient = (*GitLabClient)(nil)
var _ pipeline.ClassAwareDedupIssueClient = (*GitLabClient)(nil)
var _ pipeline.ClosableIssueClient = (*GitLabClient)(nil)
var _ pipeline.OpenEscalationLister = (*GitLabClient)(nil)

// ----- GetRawFile + CreateCommit (kill-switch GitOps auto-PR) -----

// rawRoundTripper serves a fixed status + raw (non-JSON) body and records
// the last request, for exercising the raw file-read path.
type rawRoundTripper struct {
	status  int
	body    string
	lastReq *http.Request
	ua      string
}

func (rt *rawRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.lastReq = req
	rt.ua = req.Header.Get("User-Agent")
	return &http.Response{
		StatusCode: rt.status,
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Header:     make(http.Header),
	}, nil
}

func TestGitLabClient_GetRawFile(t *testing.T) {
	cli, err := NewGitLabClient(GitLabConfig{
		APIURL:    "https://gitlab.example/api/v4",
		Token:     "tok-123",
		Project:   "platform/gitops",
		UserAgent: "loom-mills-operator/kill-switch",
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	rt := &rawRoundTripper{status: 200, body: "version: 2\nenabled: true\n"}
	cli.SetTransport(rt)

	got, err := cli.GetRawFile(context.Background(), "k3s/mills/configmap-policy.yaml", "main")
	if err != nil {
		t.Fatalf("GetRawFile: %v", err)
	}
	if !strings.Contains(got, "enabled: true") {
		t.Fatalf("raw content mismatch: %q", got)
	}
	// Path must be percent-encoded + carry the raw segment + ref query.
	wantPath := "/projects/platform%2Fgitops/repository/files/k3s%2Fmills%2Fconfigmap-policy.yaml/raw"
	if !strings.Contains(rt.lastReq.URL.RawPath, wantPath) {
		t.Errorf("raw path = %q, want it to contain %q", rt.lastReq.URL.RawPath, wantPath)
	}
	if rt.lastReq.URL.Query().Get("ref") != "main" {
		t.Errorf("ref = %q, want main", rt.lastReq.URL.Query().Get("ref"))
	}
	if rt.ua != "loom-mills-operator/kill-switch" {
		t.Errorf("User-Agent = %q, want the configured UA", rt.ua)
	}
}

func TestGitLabClient_GetRawFile_NotFound(t *testing.T) {
	cli, _ := NewGitLabClient(GitLabConfig{
		APIURL: "https://gitlab.example/api/v4", Token: "t", Project: "platform/gitops",
	})
	cli.SetTransport(&rawRoundTripper{status: 404, body: `{"message":"404 File Not Found"}`})
	if _, err := cli.GetRawFile(context.Background(), "missing.yaml", "main"); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestGitLabClient_CreateCommit(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects/services%2Floom-core/repository/commits": func(_ *http.Request) (int, any) {
			return 201, map[string]any{"id": "abc123", "web_url": "https://gitlab.example/commit/abc123"}
		},
	})
	got, err := cli.CreateCommit(context.Background(), CreateCommitRequest{
		Branch:        "mills/kill-switch-pause-x",
		StartBranch:   "main",
		CommitMessage: "chore(mills): pause",
		Actions: []CommitAction{{
			Action: "update", FilePath: "k3s/mills/configmap-policy.yaml", Content: "enabled: false\n",
		}},
	})
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if got.ID != "abc123" || got.WebURL == "" {
		t.Fatalf("unexpected response: %+v", got)
	}
	last := rt.requests[len(rt.requests)-1]
	if !strings.Contains(last.Body, `"start_branch":"main"`) ||
		!strings.Contains(last.Body, `"branch":"mills/kill-switch-pause-x"`) ||
		!strings.Contains(last.Body, `"action":"update"`) {
		t.Errorf("commit body missing expected fields: %s", last.Body)
	}
}

func TestGitLabClient_CreateCommit_Validation(t *testing.T) {
	cli, _ := newGitLabStub(t, nil)
	if _, err := cli.CreateCommit(context.Background(), CreateCommitRequest{Branch: ""}); err == nil {
		t.Error("expected error for empty branch")
	}
	if _, err := cli.CreateCommit(context.Background(), CreateCommitRequest{Branch: "b"}); err == nil {
		t.Error("expected error for no actions")
	}
}

// ----- MRDiff -----

func TestMRDiff_AssemblesUnifiedDiff(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/42/diffs": func(*http.Request) (int, any) {
			return 200, []map[string]any{
				{"old_path": "a.go", "new_path": "a.go", "diff": "@@ -1 +1 @@\n-x\n+y\n"},
				// No trailing newline — MRDiff must add one so files don't fuse.
				{"old_path": "b.go", "new_path": "b.go", "diff": "@@ -2 +2 @@\n-p\n+q"},
				// Empty diff entries (e.g. mode-only changes) are skipped.
				{"old_path": "empty.go", "new_path": "empty.go", "diff": ""},
			}
		},
	})
	out, err := cli.MRDiff(context.Background(), 42, 0)
	if err != nil {
		t.Fatalf("MRDiff: %v", err)
	}
	for _, want := range []string{
		"diff --git a/a.go b/a.go",
		"--- a/a.go",
		"+++ b/a.go",
		"@@ -1 +1 @@",
		"diff --git a/b.go b/b.go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("assembled diff missing %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "+q\n") {
		t.Errorf("missing added trailing newline on last entry, tail %q", out[max(0, len(out)-10):])
	}
	if strings.Contains(out, "empty.go") {
		t.Errorf("empty-diff entry should be skipped:\n%s", out)
	}
	// Short page (< per_page) stops pagination after one request.
	if n := len(rt.requests); n != 1 {
		t.Errorf("requests = %d, want 1 (short page ends pagination)", n)
	}
}

func TestMRDiff_TruncatesAtMaxBytes(t *testing.T) {
	big := strings.Repeat("+line\n", 200)
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/7/diffs": func(*http.Request) (int, any) {
			return 200, []map[string]any{
				{"old_path": "big.go", "new_path": "big.go", "diff": "@@ -1 +1 @@\n" + big},
			}
		},
	})
	out, err := cli.MRDiff(context.Background(), 7, 256)
	if err != nil {
		t.Fatalf("MRDiff: %v", err)
	}
	if !strings.HasSuffix(out, "… [diff truncated]\n") {
		t.Errorf("expected truncation marker, got tail %q", out[max(0, len(out)-40):])
	}
	if len(out) > 256+len("\n… [diff truncated]\n") {
		t.Errorf("truncated output too large: %d bytes", len(out))
	}
}

func TestMRDiff_Paginates(t *testing.T) {
	fullPage := make([]map[string]any, 100)
	for i := range fullPage {
		fullPage[i] = map[string]any{"old_path": "f.go", "new_path": "f.go", "diff": "@@ -1 +1 @@\n-a\n+b\n"}
	}
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/9/diffs": func(r *http.Request) (int, any) {
			if r.URL.Query().Get("page") == "1" {
				return 200, fullPage
			}
			return 200, []map[string]any{{"old_path": "last.go", "new_path": "last.go", "diff": "@@ -1 +1 @@\n-z\n+w\n"}}
		},
	})
	out, err := cli.MRDiff(context.Background(), 9, 1<<20)
	if err != nil {
		t.Fatalf("MRDiff: %v", err)
	}
	if !strings.Contains(out, "last.go") {
		t.Error("second page not fetched")
	}
	if n := len(rt.requests); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestMRDiff_Errors(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/13/diffs": func(*http.Request) (int, any) {
			return 500, map[string]any{"message": "boom"}
		},
	})
	if _, err := cli.MRDiff(context.Background(), 13, 0); err == nil {
		t.Error("expected error on 500")
	}
	if _, err := cli.MRDiff(context.Background(), 0, 0); err == nil {
		t.Error("expected error on zero mrIID")
	}
}

// TestMerge_405MidSettleRoutesToRecovery guards the mixed-sequence regression
// found in review of the 2026-07-16 hardening: a first-probe 422 followed by a
// timing 405 in the same settling window must ride the standard not-ready
// recovery (detached-head check + timing poll) instead of surfacing the 405
// raw as a stage error.
func TestMerge_405MidSettleRoutesToRecovery(t *testing.T) {
	mergeCalls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/30/merge": func(_ *http.Request) (int, any) {
			mergeCalls++
			switch mergeCalls {
			case 1:
				return 422, map[string]string{"message": "Branch cannot be merged"}
			case 2:
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			default:
				return 200, mergedMR(30, "tested-head", "settled405")
			}
		},
		"GET /api/v4/projects/services%2Floom-core/merge_requests/30": func(_ *http.Request) (int, any) {
			// Healthy branch head — not the merge_request_event placeholder.
			mr := openedMR(30, "tested-head")
			mr.HeadPipeline = mrHeadPipe{ID: 900, Status: "success", Source: "push"}
			return 200, mr
		},
	})
	resp, err := cli.Merge(context.Background(), testMergeArgs(30, "tested-head"))
	if err != nil {
		t.Fatalf("merge should succeed via not-ready recovery: %v", err)
	}
	if resp.MergedSHA != "settled405" {
		t.Errorf("sha = %q, want settled405", resp.MergedSHA)
	}
	if mergeCalls != 3 {
		t.Errorf("merge calls = %d, want 3 (422, 405, 200)", mergeCalls)
	}
}

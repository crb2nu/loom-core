package clients

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
	for prefix, handler := range rt.routes {
		method, path, _ := strings.Cut(prefix, " ")
		if req.Method == method && strings.HasPrefix(matchPath, path) {
			status, payload := handler(req)
			buf, _ := json.Marshal(payload)
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(bytes.NewReader(buf)),
				Header:     make(http.Header),
			}, nil
		}
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
	if len(rt.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(rt.requests))
	}
	got := rt.requests[0]
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
	if len(rt.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(rt.requests))
	}
	var body createMRBody
	if err := json.Unmarshal([]byte(rt.requests[0].Body), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.MergeWhenPipelineSucceeds {
		t.Errorf("merge_when_pipeline_succeeds = true on create; must be false to avoid the empty MR pipeline (AutoMerge is honored by the merge stage)")
	}
	if strings.Contains(rt.requests[0].Body, "merge_when_pipeline_succeeds") {
		t.Errorf("create body must omit merge_when_pipeline_succeeds even when AutoMerge=true: %s", rt.requests[0].Body)
	}
}

func TestCreateMR_AutoMergeFalseOmitsFlag(t *testing.T) {
	cli, rt := newGitLabStub(t, map[string]func(*http.Request) (int, any){
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
	if strings.Contains(rt.requests[0].Body, "merge_when_pipeline_succeeds") {
		t.Errorf("body unexpectedly contained merge_when_pipeline_succeeds: %s", rt.requests[0].Body)
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

// ----- PollPipeline -----

func TestPollPipeline_TerminatesOnSuccess(t *testing.T) {
	var pollCount int32
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/42": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 42, HeadPipeline: mrHeadPipe{ID: 1234, Status: "running"}}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines/1234": func(_ *http.Request) (int, any) {
			n := atomic.AddInt32(&pollCount, 1)
			status := "running"
			if n >= 2 {
				status = "success"
			}
			return 200, map[string]any{"id": 1234, "status": status}
		},
	})
	resp, err := cli.PollPipeline(context.Background(), pipeline.PollPipelineRequest{MRIID: 42})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("status = %q, want success", resp.Status)
	}
	if !strings.Contains(resp.LogTail, "status=success") {
		t.Errorf("log tail missing terminal status: %q", resp.LogTail)
	}
	if atomic.LoadInt32(&pollCount) < 2 {
		t.Errorf("expected at least 2 poll calls, got %d", pollCount)
	}
}

func TestPollPipeline_FailedTerminal(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/77": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 77, HeadPipeline: mrHeadPipe{ID: 99, Status: "failed"}}
		},
		"GET /api/v4/projects/services%2Floom-core/pipelines/99": func(_ *http.Request) (int, any) {
			return 200, map[string]any{"id": 99, "status": "failed"}
		},
	})
	resp, err := cli.PollPipeline(context.Background(), pipeline.PollPipelineRequest{MRIID: 77})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if resp.Status != "failed" {
		t.Errorf("status = %q", resp.Status)
	}
}

func TestPollPipeline_TimeoutWhenNoHeadPipeline(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects/services%2Floom-core/merge_requests/55": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 55} // never gets a head_pipeline.id
		},
	})
	cli.cfg.PollDeadline = 50 * time.Millisecond
	cli.cfg.PollInterval = 10 * time.Millisecond
	resp, err := cli.PollPipeline(context.Background(), pipeline.PollPipelineRequest{MRIID: 55})
	if err == nil {
		t.Error("expected timeout error")
	}
	if resp.Status != "timeout" {
		t.Errorf("status = %q, want timeout", resp.Status)
	}
}

func TestPollPipeline_RequiresMRIID(t *testing.T) {
	cli, _ := newGitLabStub(t, nil)
	if _, err := cli.PollPipeline(context.Background(), pipeline.PollPipelineRequest{}); err == nil {
		t.Error("expected error for missing MRIID")
	}
}

// ----- Merge -----

func TestMerge_PropagatesSHA(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/3/merge": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 3, SHA: "abc123def"}
		},
	})
	resp, err := cli.Merge(context.Background(), pipeline.MergeRequestArgs{MRIID: 3})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if resp.MergedSHA != "abc123def" {
		t.Errorf("sha = %q", resp.MergedSHA)
	}
}

func TestMerge_GitLabReportsMergeError(t *testing.T) {
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/4/merge": func(_ *http.Request) (int, any) {
			return 200, mrResponse{IID: 4, MergeError: "branch cannot be merged"}
		},
	})
	if _, err := cli.Merge(context.Background(), pipeline.MergeRequestArgs{MRIID: 4}); err == nil {
		t.Error("expected error from merge_error field")
	}
}

func TestMerge_RequiresMRIID(t *testing.T) {
	cli, _ := newGitLabStub(t, nil)
	if _, err := cli.Merge(context.Background(), pipeline.MergeRequestArgs{}); err == nil {
		t.Error("expected error for missing MRIID")
	}
}

// TestMerge_RetriesNotMergeableYet405 guards the Mills A2 north-star regression
// (escalations #147/#148/#150): GitLab returns 405 on PUT .../merge while the
// MR's merge_status is still settling after CI turned green — a timing race.
// Merge must poll past the transient 405 and succeed, not fail on the first hit.
func TestMerge_RetriesNotMergeableYet405(t *testing.T) {
	calls := 0
	cli, _ := newGitLabStub(t, map[string]func(*http.Request) (int, any){
		"PUT /api/v4/projects/services%2Floom-core/merge_requests/7/merge": func(_ *http.Request) (int, any) {
			calls++
			if calls < 3 {
				return 405, map[string]any{"message": "405 Method Not Allowed"}
			}
			return 200, mrResponse{IID: 7, SHA: "deadbeef"}
		},
	})
	resp, err := cli.Merge(context.Background(), pipeline.MergeRequestArgs{MRIID: 7})
	if err != nil {
		t.Fatalf("merge should succeed after transient 405s: %v", err)
	}
	if resp.MergedSHA != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", resp.MergedSHA)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts (2x405 then 200), got %d", calls)
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
	})
	if _, err := cli.Merge(context.Background(), pipeline.MergeRequestArgs{MRIID: 8}); err == nil {
		t.Error("expected 409 conflict to return an error")
	}
	if calls != 1 {
		t.Errorf("non-405 error should not retry; got %d attempts", calls)
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
	resp, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{BranchName: "feat/x"})
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
	resp, err := cli.Cleanup(context.Background(), pipeline.CleanupRequest{BranchName: "feat/gone"})
	if err != nil {
		t.Errorf("cleanup should swallow 404: %v", err)
	}
	if !strings.Contains(resp.LogTail, "already removed") {
		t.Errorf("log tail = %q", resp.LogTail)
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

// ----- compile-time interface assertions -----

var _ pipeline.GitLabClient = (*GitLabClient)(nil)
var _ pipeline.IssueClient = (*GitLabClient)(nil)

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

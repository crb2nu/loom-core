package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// GitLabConfig captures the connection settings for the GitLab REST API.
// The operator reads these from env at startup; the same token is used
// by both the merge-request flow and the issue-creation escalation path.
type GitLabConfig struct {
	// APIURL is the GitLab REST API base, e.g. "https://gitlab.flexinfer.ai/api/v4".
	APIURL string
	// Token is a personal access token or project access token with
	// api scope. Sent as the GitLab PRIVATE-TOKEN header.
	Token string
	// Project is the URL-encoded project path or numeric id (e.g.
	// "services%2Floom-core" or "47"). All MR/pipeline calls scope to
	// this project.
	Project string
	// PollInterval is how often PollPipeline checks the pipeline state.
	// Default 5s. Capped to 2s minimum to avoid hammering the API.
	PollInterval time.Duration
	// PollDeadline caps the total wait for a pipeline to terminate.
	// Default 30 minutes.
	PollDeadline time.Duration
	// HeadSHADeadline caps how long PollPipeline waits for the MR to report a
	// head SHA at all. Default 5 minutes. GitLab computes the diff (and with
	// it `sha`) during MR preparation, which normally finishes in seconds
	// (!1239: created 01:21:20Z, prepared_at 01:21:25Z), so a head SHA still
	// missing minutes later is a durable MR state, not a slow one. Bounding it
	// separately from PollDeadline keeps a stuck-but-real pipeline on the full
	// 30m watch while a never-materializing head fails fast and distinctly.
	HeadSHADeadline time.Duration
	// BranchPipelineDeadline caps how long PollPipeline waits for a *push*
	// pipeline to appear for an MR head SHA that GitLab has already published.
	// Default 10 minutes, clamped to PollDeadline. GitLab enqueues the pipeline
	// for a push as part of accepting the push, so a head SHA that still has no
	// push pipeline minutes later usually never will: `workflow:rules` that only
	// admit `merge_request_event`, CI disabled on the project, or an operator
	// deleting the pipeline. Like HeadSHADeadline this is bounded separately
	// from PollDeadline so a slow-but-real pipeline still gets the full 30m
	// watch while a pipeline that cannot exist fails fast and distinctly.
	BranchPipelineDeadline time.Duration
	// MergeMethod is "merge", "rebase", or "ff" — defaults to "merge".
	MergeMethod string
	// Timeout caps any individual HTTP call. Default 30s.
	Timeout time.Duration
	// UserAgent, when non-empty, is sent as the User-Agent header on
	// every request. The in-cluster operator reaches gitlab via internal
	// DNS so Go's default UA is fine for the pipeline token, but the
	// gitops kill-switch client may transit the public edge (Cloudflare
	// 403s the default urllib/Go UA with error 1010) — set this to a
	// browser-acceptable identifier there. Empty preserves prior behavior
	// (no explicit UA header).
	UserAgent string
}

// GitLabClient implements pipeline.GitLabClient + pipeline.IssueClient
// against the GitLab REST API.
type GitLabClient struct {
	cfg                GitLabConfig
	http               *httpclient.Client
	mergeSettleTimeout time.Duration
	mergeRetryTimeout  time.Duration
	// headObserveInterval is the poll cadence ObserveHead uses while a rebase
	// is in flight. Zero resolves to headObserveDefaultInterval (2s); tests
	// shorten it so a settle-deadline case stays sub-second.
	headObserveInterval time.Duration
}

// GitLabHTTPError preserves status while retaining the historical error text.
type GitLabHTTPError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *GitLabHTTPError) Error() string {
	if e == nil {
		return "gitlab: http error"
	}
	return fmt.Sprintf("gitlab: %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// GitLabHTTPStatus finds a preserved status through wrapped errors.
func GitLabHTTPStatus(err error) (int, bool) {
	var httpErr *GitLabHTTPError
	if !errors.As(err, &httpErr) || httpErr == nil {
		return 0, false
	}
	return httpErr.StatusCode, true
}

// NewGitLabClient validates config and returns a ready client.
func NewGitLabClient(cfg GitLabConfig) (*GitLabClient, error) {
	if cfg.APIURL == "" {
		return nil, errors.New("gitlab: APIURL required")
	}
	if cfg.Token == "" {
		return nil, errors.New("gitlab: Token required")
	}
	if cfg.Project == "" {
		return nil, errors.New("gitlab: Project required")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.PollDeadline == 0 {
		cfg.PollDeadline = 30 * time.Minute
	}
	if cfg.HeadSHADeadline == 0 {
		cfg.HeadSHADeadline = defaultHeadSHADeadline
	}
	if cfg.HeadSHADeadline > cfg.PollDeadline {
		cfg.HeadSHADeadline = cfg.PollDeadline
	}
	if cfg.BranchPipelineDeadline == 0 {
		cfg.BranchPipelineDeadline = defaultBranchPipelineDeadline
	}
	if cfg.BranchPipelineDeadline > cfg.PollDeadline {
		cfg.BranchPipelineDeadline = cfg.PollDeadline
	}
	if cfg.MergeMethod == "" {
		cfg.MergeMethod = "merge"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	hcfg := httpclient.DefaultConfig()
	hcfg.Timeout = cfg.Timeout
	// GitLab mutations must never be retried invisibly by the shared HTTP
	// transport. The merge state machine owns retries so every merge PUT is
	// preceded by a fresh identity GET and lost responses are reconciled before
	// another mutation. This also keeps POST/DELETE behavior explicit.
	hcfg.MaxRetries = 0
	c := httpclient.New(hcfg)
	return &GitLabClient{cfg: cfg, http: c}, nil
}

// HeadSHADeadline and BranchPipelineDeadline expose the two ci_watch bounds
// AFTER defaulting and clamping, so the operator can log what a
// LOOM_MILLS_GITLAB_*_DEADLINE override actually resolved to. Both silently
// clamp to PollDeadline, which is precisely the case an operator tuning them
// needs to see.
func (c *GitLabClient) HeadSHADeadline() time.Duration { return c.cfg.HeadSHADeadline }

func (c *GitLabClient) BranchPipelineDeadline() time.Duration { return c.cfg.BranchPipelineDeadline }

// SetTransport is for tests.
func (c *GitLabClient) SetTransport(rt http.RoundTripper) {
	c.http.HTTP().Transport = rt
}

// requestJSON is the shared call helper. It marshals body when non-nil,
// decodes the response into out, and surfaces non-2xx as an error with
// a truncated body for debugging.
func (c *GitLabClient) requestJSON(ctx context.Context, method, path string, body any, out any) error {
	full := strings.TrimRight(c.cfg.APIURL, "/") + path
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gitlab: marshal: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reqBody)
	if err != nil {
		return fmt.Errorf("gitlab: new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &GitLabHTTPError{
			Method: method, Path: path, StatusCode: resp.StatusCode,
			Body: strings.TrimSpace(string(buf)),
		}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("gitlab: decode %s: %w", path, err)
		}
	}
	return nil
}

// ForProject returns a client scoped to a different GitLab project path, for
// per-item cross-repo routing (a backlog item's TargetProject). An empty
// project or the current project returns the receiver unchanged. The HTTP
// client and token are shared — only the project segment changes — so the
// receiver's token must be authorized on the target project (the services
// group token, when cross-repo execution is enabled). Every request method
// keys off cfg.Project via projectPath(), so overriding it is sufficient.
func (c *GitLabClient) ForProject(project string) *GitLabClient {
	project = strings.TrimSpace(project)
	if project == "" || project == c.cfg.Project {
		return c
	}
	cp := *c
	cp.cfg.Project = project
	return &cp
}

// projectPath returns the URL-encoded project segment.
func (c *GitLabClient) projectPath() string {
	// Numeric IDs are passed through; slug paths are URL-encoded.
	if _, err := strconv.Atoi(c.cfg.Project); err == nil {
		return c.cfg.Project
	}
	return url.PathEscape(c.cfg.Project)
}

// MRState returns a merge request's lifecycle state ("opened", "merged",
// "closed", "locked") by IID. Read-only; the take-up reconciler polls this to
// true plan/slice phases to MR reality.
func (c *GitLabClient) MRState(ctx context.Context, mrIID int64) (string, error) {
	var mr mrResponse
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", c.projectPath(), mrIID)
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &mr); err != nil {
		return "", err
	}
	return mr.State, nil
}

// VerifyMR confirms that an MR GET in this client's exact project returns the
// requested IID and a recognized lifecycle state. It is intentionally
// read-only and is used before upgrading legacy URL provenance into a durable
// project artifact.
func (c *GitLabClient) VerifyMR(ctx context.Context, mrIID int64) error {
	if mrIID <= 0 {
		return errors.New("gitlab: positive MR IID required")
	}
	mr, err := c.getMR(ctx, mrIID)
	if err != nil {
		return err
	}
	if mr.IID != mrIID {
		return fmt.Errorf("gitlab: requested MR IID %d but response carried %d", mrIID, mr.IID)
	}
	ref, ok := ParseGitLabMRReference(mr.WebURL)
	if !ok || !ref.ProjectBound || ref.Authority == "" {
		return fmt.Errorf("gitlab: MR %d response has ambiguous web URL identity", mrIID)
	}
	if ref.IID != mrIID || !SameGitLabProject(ref.Project, c.cfg.Project) || !SameGitLabAuthority(ref.Authority, c.cfg.APIURL) {
		return fmt.Errorf("gitlab: MR %d response web URL does not match client identity", mrIID)
	}
	switch mr.State {
	case "opened", "merged", "closed", "locked":
		return nil
	default:
		return fmt.Errorf("gitlab: MR %d returned unrecognized state %q", mrIID, mr.State)
	}
}

// ----- CreateMR -----

type createMRBody struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description,omitempty"`
	RemoveSourceBranch bool   `json:"remove_source_branch"`
	Squash             bool   `json:"squash"`
	// MergeWhenPipelineSucceeds is DELIBERATELY NOT SET on create — see CreateMR.
	// Opening an MR with merge_when_pipeline_succeeds=true makes GitLab spin up a
	// detached merge-request pipeline; on a repo whose `workflow.rules` block
	// merge_request_event pipelines (loom-core runs branch pipelines), that
	// pipeline is emptied into a `failed`, 0-job placeholder which becomes the
	// MR head_pipeline and blocks merge with HTTP 405 under
	// only_allow_merge_if_pipeline_succeeds — the long-standing Mills
	// autonomous-merge blocker (#147/#148/#150). Plain MRs (no MWPS) keep the
	// branch push pipeline as head; the operator's explicit `merge` stage then
	// merges once `ci_watch` confirms it green.
	MergeWhenPipelineSucceeds bool `json:"merge_when_pipeline_succeeds,omitempty"`
}

type mrResponse struct {
	IID          int64      `json:"iid"`
	WebURL       string     `json:"web_url"`
	State        string     `json:"state"`
	HeadPipeline mrHeadPipe `json:"head_pipeline"`
	// MergeStatus is GitLab's legacy mergeability summary and
	// DetailedMergeStatus its authoritative successor (GitLab 15.6+). Only the
	// detailed value gates the stale-405 close+reopen recovery; the legacy one
	// is carried for escalation diagnostics. HasConflicts joins them on the
	// head-SHA-never-materialized path: together they name WHY GitLab has no
	// head to build (conflicted diff, unprepared MR, deleted source branch) so
	// that escalation is actionable instead of an opaque poll timeout.
	MergeStatus         string `json:"merge_status"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	HasConflicts        bool   `json:"has_conflicts"`
	SHA                 string `json:"sha"`
	MergedCommitSHA     string `json:"merged_commit_sha"`
	MergeCommitSHA      string `json:"merge_commit_sha"`
	SquashCommitSHA     string `json:"squash_commit_sha"`
	SourceBranch        string `json:"source_branch"`
	TargetBranch        string `json:"target_branch"`
	MergeError          string `json:"merge_error"`
	// MergedAt is set only on merged MRs. The ghost-spark branch sweep uses it
	// to prove a merge belongs to the escalation it is closing rather than to
	// an earlier attempt on the same branch.
	MergedAt *time.Time `json:"merged_at"`
}

type mrHeadPipe struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Source string `json:"source"`
}

// CreateMR implements pipeline.GitLabClient.
func (c *GitLabClient) CreateMR(ctx context.Context, req pipeline.CreateMRRequest) (pipeline.CreateMRResponse, error) {
	// NB: req.AutoMerge is intentionally NOT mapped to MergeWhenPipelineSucceeds
	// here. Arming MWPS at create time triggers a detached merge-request pipeline
	// that this repo's workflow rules empty into a `failed` head pipeline,
	// blocking the merge (see createMRBody.MergeWhenPipelineSucceeds). The
	// autonomous merge is performed by the operator's explicit `merge` stage
	// (runMerge → Merge) after `ci_watch` confirms the branch pipeline is green.
	// The worker pushes before this call, so an open MR on this exact ref is
	// already up to date. Adopt it before POSTing: retries then reuse one MR
	// instead of relying on GitLab's duplicate-MR 409 as normal control flow.
	existing, ok, err := c.findOpenMRForBranch(ctx, req.SourceBranch, req.TargetBranch)
	if err != nil {
		return pipeline.CreateMRResponse{}, fmt.Errorf("mr: list open mrs for %q: %w", req.SourceBranch, err)
	}
	if ok {
		slog.Default().Info("mr: adopted existing", "mr_iid", existing.IID, "source_branch", req.SourceBranch)
		return pipeline.CreateMRResponse{
			MRIID: existing.IID, URL: existing.WebURL, Project: c.cfg.Project,
			SourceBranch: req.SourceBranch, TargetBranch: req.TargetBranch, Adopted: true,
		}, nil
	}

	body := createMRBody{
		SourceBranch:       req.SourceBranch,
		TargetBranch:       req.TargetBranch,
		Title:              req.Title,
		Description:        req.Description,
		RemoveSourceBranch: true,
		Squash:             false,
	}
	var got mrResponse
	path := fmt.Sprintf("/projects/%s/merge_requests", c.projectPath())
	if err := c.requestJSON(ctx, http.MethodPost, path, body, &got); err != nil {
		// 409 "Another open merge request already exists for this source branch":
		// a prior attempt (or a crashed run) already opened the MR. This is not a
		// failure — adopt the existing MR so the pipeline continues on it instead
		// of escalating (live 2026-07-16: mr 409 escalated ×3 on
		// "Another open merge request already exists for this source branch:
		// !1031"). Look the MR up by source branch rather than parsing the IID
		// out of the message so a malformed message can't misroute the merge.
		if isMRAlreadyExists(err) {
			existing, ok, aerr := c.findOpenMRForBranch(ctx, req.SourceBranch, req.TargetBranch)
			if aerr != nil {
				return pipeline.CreateMRResponse{}, fmt.Errorf("mr 409 adopt: list open mrs for %q: %w (original: %v)", req.SourceBranch, aerr, err)
			}
			if ok {
				slog.Default().Info("mr: adopted existing", "mr_iid", existing.IID, "source_branch", req.SourceBranch)
				return pipeline.CreateMRResponse{
					MRIID:        existing.IID,
					URL:          existing.WebURL,
					Project:      c.cfg.Project,
					SourceBranch: req.SourceBranch,
					TargetBranch: req.TargetBranch,
					Adopted:      true,
				}, nil
			}
		}
		return pipeline.CreateMRResponse{}, err
	}
	return pipeline.CreateMRResponse{
		MRIID:        got.IID,
		URL:          got.WebURL,
		Project:      c.cfg.Project,
		SourceBranch: req.SourceBranch,
		TargetBranch: req.TargetBranch,
	}, nil
}

// isMRAlreadyExists reports whether a CreateMR error is GitLab's 409 rejection
// of a duplicate open MR for the source branch ("Another open merge request
// already exists for this source branch: !N"). Requires both the 409 status
// and the phrase so an unrelated 409 (or a same-phrase 4xx from another verb)
// can't trip the adoption path.
func isMRAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "409") &&
		strings.Contains(s, "another open merge request already exists")
}

// findOpenMRForBranch returns the open merge request whose source branch is
// sourceBranch (and, when non-empty, whose target branch is targetBranch), if
// one exists. Used by CreateMR to adopt the MR a prior attempt already opened
// after a 409. Filters state + source_branch + target_branch server-side so two
// open MRs sharing a source branch with different targets can't misroute the
// adoption; the first opened row with a non-zero IID wins.
func (c *GitLabClient) findOpenMRForBranch(ctx context.Context, sourceBranch, targetBranch string) (mrResponse, bool, error) {
	path := fmt.Sprintf("/projects/%s/merge_requests?state=opened&source_branch=%s&per_page=20",
		c.projectPath(), url.QueryEscape(sourceBranch))
	if targetBranch != "" {
		path += "&target_branch=" + url.QueryEscape(targetBranch)
	}
	var mrs []mrResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &mrs); err != nil {
		return mrResponse{}, false, err
	}
	for _, mr := range mrs {
		// Do not trust server-side filtering alone: a loose/stale response must
		// never adopt an MR for a different ref or target.
		if mr.IID != 0 && mr.State == "opened" && mr.SourceBranch == sourceBranch && mr.TargetBranch == targetBranch {
			return mr, true, nil
		}
	}
	return mrResponse{}, false, nil
}

// AdoptGreenMR merges merge request mrIID when — and only when — GitLab reports
// it open, conflict-free and carrying a SUCCESSFUL head pipeline. It is the
// reconciler's GreenMRAdopter: the escalated-run population whose MR is green
// and mergeable but which no live stage owns any more (see the interface doc in
// pkg/mills for the 2026-08-02 CI-storm shape it exists for).
//
// Deliberately conservative — this merges without a human, so every ambiguous
// answer refuses rather than guesses:
//   - state must be exactly "opened" (a merged MR is already someone else's
//     success; a closed one is abandoned).
//   - has_conflicts must be false and detailed_merge_status must be "mergeable".
//     Anything else (ci_still_running, discussions_not_resolved, blocked_status,
//     draft_status, …) is a refusal, so a still-running pipeline or an unresolved
//     review thread can never be merged out from under its reviewer.
//   - the head pipeline must exist and be "success". A missing head pipeline is
//     the husk shape (branch/MR split) and must never be treated as green.
//   - the observed head SHA must be non-empty so the merge can be pinned to the
//     exact head whose readiness and pipeline were checked.
//
// Returns adopted=false with a human-readable reason for every refusal.
// A concurrent merge landing between the check and the PUT is success, not an
// error: the post-merge re-read confirms state=merged.
func (c *GitLabClient) AdoptGreenMR(ctx context.Context, mrIID int64) (bool, string, error) {
	mr, err := c.getMR(ctx, mrIID)
	if err != nil {
		return false, "", err
	}
	if state := strings.ToLower(strings.TrimSpace(mr.State)); state != "opened" {
		return false, fmt.Sprintf("mr state %q is not open", state), nil
	}
	if mr.HasConflicts {
		return false, "mr has conflicts", nil
	}
	if status := strings.ToLower(strings.TrimSpace(mr.DetailedMergeStatus)); status != "mergeable" {
		return false, fmt.Sprintf("detailed_merge_status %q is not mergeable", status), nil
	}
	pipeStatus := strings.ToLower(strings.TrimSpace(mr.HeadPipeline.Status))
	if pipeStatus == "" {
		return false, "mr has no head pipeline", nil
	}
	if pipeStatus != "success" {
		return false, fmt.Sprintf("head pipeline %q is not green", pipeStatus), nil
	}
	sha := strings.TrimSpace(mr.SHA)
	if sha == "" {
		return false, "mr has no head sha", nil
	}

	path := fmt.Sprintf("/projects/%s/merge_requests/%d/merge?should_remove_source_branch=true",
		c.projectPath(), mrIID)
	var merged mrResponse
	if mergeErr := c.requestJSON(ctx, http.MethodPut, path, mergeBody{SHA: sha}, &merged); mergeErr != nil {
		// A 409 means GitLab refused the SHA precondition because the head
		// changed after the readiness read. Surface the typed refusal and let
		// the next reconciler sweep validate the new head; never retry it here.
		if status, ok := GitLabHTTPStatus(mergeErr); ok && status == http.StatusConflict {
			return false, "", mergeErr
		}
		// Someone (or GitLab's own auto-merge) may have landed it in the gap
		// between the check and this call; that is the outcome we wanted.
		current, getErr := c.getMR(ctx, mrIID)
		if getErr == nil && strings.EqualFold(strings.TrimSpace(current.State), "merged") {
			return true, "merged concurrently", nil
		}
		return false, "", mergeErr
	}
	if state := strings.ToLower(strings.TrimSpace(merged.State)); state != "" && state != "merged" {
		return false, fmt.Sprintf("merge returned state %q", state), nil
	}
	return true, "merged open green mr", nil
}

// MergedMRForBranch returns the most recently merged merge request whose source
// branch is exactly sourceBranch. Used by the reconciler's ghost-spark sweep to
// close an escalated item whose run never recorded an MR IID — it escalated
// before the mr stage — but whose branch was pushed and merged by hand
// afterwards.
//
// Filters state=merged server-side, then re-checks SourceBranch on every row:
// GitLab's source_branch filter has matched loosely in the past, and a mismatch
// here would close the wrong backlog item. Picks the LATEST merge when a branch
// was somehow merged more than once, so the caller's "merge is newer than the
// escalated attempt" guard sees the most recent evidence.
func (c *GitLabClient) MergedMRForBranch(ctx context.Context, sourceBranch string) (int64, time.Time, bool, error) {
	sourceBranch = strings.TrimSpace(sourceBranch)
	if sourceBranch == "" {
		return 0, time.Time{}, false, nil
	}
	path := fmt.Sprintf("/projects/%s/merge_requests?state=merged&source_branch=%s&order_by=updated_at&sort=desc&per_page=20",
		c.projectPath(), url.QueryEscape(sourceBranch))
	var mrs []mrResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &mrs); err != nil {
		return 0, time.Time{}, false, err
	}
	var (
		bestIID  int64
		bestTime time.Time
	)
	for _, mr := range mrs {
		if mr.IID == 0 || mr.State != "merged" || mr.SourceBranch != sourceBranch {
			continue
		}
		// A merged MR with no merged_at is unusable: the caller cannot prove the
		// merge is newer than the escalation, and closing on an unprovable merge
		// is exactly the mis-attribution this guard exists to prevent.
		if mr.MergedAt == nil {
			continue
		}
		if bestIID == 0 || mr.MergedAt.After(bestTime) {
			bestIID, bestTime = mr.IID, *mr.MergedAt
		}
	}
	if bestIID == 0 {
		return 0, time.Time{}, false, nil
	}
	return bestIID, bestTime, true, nil
}

// ----- PollPipeline -----

// shaPipeline is one entry of GET /projects/:id/pipelines?sha=.
type shaPipeline struct {
	ID     int64  `json:"id"`
	SHA    string `json:"sha"`
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Source string `json:"source"`
	WebURL string `json:"web_url"`
}

type pipelineJob struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason"`
	Retried       bool   `json:"retried"`
}

// failedJobReasons returns a complete, current failed-job reason set. GitLab
// retains retried attempts in job listings, so both the API query and the
// defensive client-side filter exclude them. Any page failure makes the result
// inconclusive; callers then preserve the existing code classification.
func (c *GitLabClient) failedJobs(ctx context.Context, pipelineID int64) ([]pipeline.FailedJob, error) {
	const perPage = 100
	var failed []pipeline.FailedJob
	for page := 1; ; page++ {
		path := fmt.Sprintf("/projects/%s/pipelines/%d/jobs?scope%%5B%%5D=failed&include_retried=false&per_page=%d&page=%d",
			c.projectPath(), pipelineID, perPage, page)
		var jobs []pipelineJob
		if err := c.requestJSON(ctx, http.MethodGet, path, nil, &jobs); err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if job.Status == "failed" && !job.Retried {
				failed = append(failed, pipeline.FailedJob{ID: job.ID, Name: job.Name, FailureReason: job.FailureReason})
			}
		}
		if len(jobs) < perPage {
			return failed, nil
		}
	}
}

func (c *GitLabClient) failedJobReasons(ctx context.Context, pipelineID int64) ([]string, error) {
	jobs, err := c.failedJobs(ctx, pipelineID)
	if err != nil {
		return nil, err
	}
	reasons := make([]string, 0, len(jobs))
	for _, job := range jobs {
		reasons = append(reasons, job.FailureReason)
	}
	return reasons, nil
}

// RetryJob retries one GitLab CI job. Eligibility remains an operator concern.
func (c *GitLabClient) RetryJob(ctx context.Context, jobID int64) error {
	if jobID <= 0 {
		return errors.New("gitlab: positive job ID required")
	}
	path := fmt.Sprintf("/projects/%s/jobs/%d/retry", c.projectPath(), jobID)
	return c.requestJSON(ctx, http.MethodPost, path, nil, nil)
}

// PipelinePollDeadline lets ci_watch preserve this client's original polling
// budget across its one eligible terminal-job rescue.
func (c *GitLabClient) PipelinePollDeadline() time.Duration { return c.cfg.PollDeadline }

// PollPipeline implements pipeline.GitLabClient. It resolves the *branch*
// pipeline for the MR's head SHA and polls its state until terminal.
//
// It deliberately does NOT poll mr.head_pipeline: when an MR is opened on a
// repo whose `workflow.rules` block merge_request_event pipelines (loom-core
// runs branch pipelines), GitLab briefly attaches a spurious, 0-job
// `merge_request_event` pipeline as the head and immediately fails it (then
// usually deletes it). The previous head-pipeline poll raced that transient
// and reported `failed` — the long-standing autonomous-merge blocker
// (#147/#148/#150). Polling the push/branch pipeline for the SHA ignores the
// merge_request_event placeholder entirely. The contract stays blocking.
func (c *GitLabClient) PollPipeline(ctx context.Context, req pipeline.PollPipelineRequest) (pipeline.PollPipelineResponse, error) {
	if req.MRIID == 0 {
		return pipeline.PollPipelineResponse{}, errors.New("gitlab: MRIID required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"project", req.Project},
		{"source branch", req.SourceBranch},
		{"target branch", req.TargetBranch},
	} {
		if strings.TrimSpace(field.value) == "" {
			return pipeline.PollPipelineResponse{}, fmt.Errorf("gitlab: expected MR %s required for pipeline authorization: %w", field.name, pipeline.ErrMergeAuthorizationStale)
		}
	}
	if req.Project != c.cfg.Project {
		return pipeline.PollPipelineResponse{}, fmt.Errorf("gitlab: CI authorization project %q does not match client project %q: %w", req.Project, c.cfg.Project, pipeline.ErrMergeAuthorizationStale)
	}
	pollCtx, cancel := context.WithTimeout(ctx, c.cfg.PollDeadline)
	defer cancel()
	logTail := strings.Builder{}
	terminal := map[string]bool{
		"success": true, "failed": true, "canceled": true, "skipped": true,
	}
	// lastPipelineURL is the web_url of the most recent branch pipeline seen
	// for the MR head SHA. Embedded in the timeout error so the escalation is
	// directly actionable (a human can open the stuck pipeline) instead of
	// leaving the operator to hunt for it (escalations #149/#153).
	var lastPipelineURL string
	// lastStatus is the most recent NON-terminal branch pipeline status observed
	// (running/pending/created). Surfaced on the timeout response so the ci_watch
	// stage can log the actual state while extending the watch. (S3)
	var lastStatus string
	// headSHAPendingSince is when the MR first reported no head SHA in an
	// UNBROKEN run of empty observations. Reset the moment a head appears, so
	// only a continuously headless MR trips the bounded deadline.
	var headSHAPendingSince time.Time
	// branchPipelinePendingSince / …SHA track an UNBROKEN run of observations in
	// which a KNOWN head SHA had no push pipeline. Keyed on the SHA so a repush
	// (new head) restarts the wait rather than inheriting the old head's clock.
	var (
		branchPipelinePendingSince time.Time
		branchPipelinePendingSHA   string
	)
	// partialResp carries whatever poll history exists onto a non-timeout error
	// return (caller interrupt, MR fetch failure, branch mismatch, pipeline
	// lookup failure). These all used to return a zero-valued response, so the
	// ci_watch stage appended an empty tail and the escalation lost the very
	// poll history that explains the failure.
	partialResp := func() pipeline.PollPipelineResponse {
		return pipeline.PollPipelineResponse{LogTail: logTail.String(), PipelineURL: lastPipelineURL, LastStatus: lastStatus}
	}
	timeoutResp := func() (pipeline.PollPipelineResponse, error) {
		urlNote := ""
		if lastPipelineURL != "" {
			urlNote = " (pipeline: " + lastPipelineURL + ")"
		}
		// Wrap ErrPipelinePollTimeout so the runner's Classify tags this
		// ClassInfra (a stuck CI pipeline is not a code bug) rather than the
		// default ClassCode. See pipeline.ErrPipelinePollTimeout. PipelineURL /
		// LastStatus let the ci_watch stage extend the watch and, at its hard
		// cap, key the external-dependency stall on the stuck pipeline. (S3)
		return pipeline.PollPipelineResponse{Status: "timeout", LogTail: logTail.String(), PipelineURL: lastPipelineURL, LastStatus: lastStatus},
			fmt.Errorf("gitlab: pipeline poll timed out after %s%s: %w", c.cfg.PollDeadline, urlNote, pipeline.ErrPipelinePollTimeout)
	}
	for {
		if ctx.Err() != nil {
			return partialResp(), ctx.Err()
		}
		if pollCtx.Err() != nil {
			return timeoutResp()
		}

		mrPath := fmt.Sprintf("/projects/%s/merge_requests/%d", c.projectPath(), req.MRIID)
		var mr mrResponse
		if err := c.requestJSON(pollCtx, http.MethodGet, mrPath, nil, &mr); err != nil {
			if ctx.Err() != nil {
				return partialResp(), ctx.Err()
			}
			if pollCtx.Err() != nil {
				return timeoutResp()
			}
			return partialResp(), err
		}
		if mr.SourceBranch != req.SourceBranch {
			return partialResp(), fmt.Errorf("gitlab: mr %d source branch %q does not match MR-stage source branch %q: %w", req.MRIID, mr.SourceBranch, req.SourceBranch, pipeline.ErrMergeAuthorizationStale)
		}
		if mr.TargetBranch != req.TargetBranch {
			return partialResp(), fmt.Errorf("gitlab: mr %d target branch %q does not match MR-stage target branch %q: %w", req.MRIID, mr.TargetBranch, req.TargetBranch, pipeline.ErrMergeAuthorizationStale)
		}
		// An operator who closes (or locks) the MR mid-watch has ended the run's
		// CI story: GitLab creates no further pipelines for it and the merge
		// stage would refuse it anyway. Without this the with-SHA path kept
		// polling a dead MR to the full deadline and then reported a generic
		// pipeline timeout.
		//
		// Scoped to a KNOWN head on purpose. The headless path reaches the same
		// conclusion one branch down through headSHAUnresolvable, whose error
		// already names the state and offers reopening; preempting it here would
		// only swap one terminal config verdict for another (!1270).
		//
		// "merged" is deliberately NOT terminal either way — the head pipeline
		// still exists and normally resolves on the very next poll, and failing a
		// merged MR would escalate a run whose work actually landed.
		if mr.SHA != "" && (mr.State == "closed" || mr.State == "locked") {
			fmt.Fprintf(&logTail, "[%s] MR %d is %s; abandoning ci watch\n", time.Now().Format(time.RFC3339), req.MRIID, mr.State)
			return partialResp(), fmt.Errorf("gitlab: mr %d is %s during ci watch%s: %w",
				req.MRIID, mr.State, mrURLNote(mr), pipeline.ErrMergeRequestClosed)
		}
		if mr.SHA != "" {
			// Only an UNBROKEN run of headless observations counts against the
			// head-SHA deadline; a head that appears late is a normal slow
			// prepare, not a stuck MR.
			headSHAPendingSince = time.Time{}
		}

		if mr.SHA == "" {
			// A vanished head also breaks the branch-pipeline run: whatever head
			// comes back gets a fresh wait.
			branchPipelinePendingSHA = ""
			now := time.Now()
			if headSHAPendingSince.IsZero() {
				headSHAPendingSince = now
			}
			waited := now.Sub(headSHAPendingSince)
			fmt.Fprintf(&logTail, "[%s] MR %d head sha pending (%s, state=%s merge_status=%s)\n",
				now.Format(time.RFC3339), req.MRIID, waited.Round(time.Second), mr.State, mr.MergeStatus)
			// Bounded: an MR that never reports a head SHA has no pipeline to
			// wait for, so burning the poll deadline (and then two ci_watch
			// extensions, 90m total) produced only an endless "head sha pending"
			// log and a stall escalation that blamed a pipeline that never
			// existed (!1239, 2026-07-26: sha=null, has_conflicts=true,
			// merge_status=cannot_be_merged, unchanged for 15 hours).
			if headSHAUnresolvable(mr) || waited >= c.cfg.HeadSHADeadline {
				return partialResp(),
					headSHAUnavailableError(mr, req.MRIID, waited, c.cfg.HeadSHADeadline)
			}
		} else if pipe, ok, err := c.branchPipelineForSHA(pollCtx, mr.SHA, mr.SourceBranch, "push"); err != nil {
			if ctx.Err() != nil {
				return partialResp(), ctx.Err()
			}
			if pollCtx.Err() != nil {
				return timeoutResp()
			}
			return partialResp(), err
		} else if !ok {
			// Only the spurious merge_request_event pipeline (or none) exists so
			// far — keep polling until the branch pipeline appears, but BOUNDED.
			// GitLab enqueues a push pipeline as part of accepting the push, so a
			// head that still has none minutes later is a project-configuration
			// state no re-poll can change (workflow:rules admitting only
			// merge_request_event, CI disabled, the pipeline deleted). Unbounded,
			// this replayed "branch pipeline pending" for the full poll deadline
			// and then handed ci_watch a generic ErrPipelinePollTimeout, which it
			// reads as "still running" — so it spent both watch extensions (90m)
			// before escalating a phantom stall, the same burn !1270 removed for a
			// head SHA that never materializes.
			now := time.Now()
			if branchPipelinePendingSHA != mr.SHA {
				branchPipelinePendingSHA = mr.SHA
				branchPipelinePendingSince = now
			}
			waited := now.Sub(branchPipelinePendingSince)
			fmt.Fprintf(&logTail, "[%s] MR %d branch pipeline pending for %s (%s)\n",
				now.Format(time.RFC3339), req.MRIID, shortSHA(mr.SHA), waited.Round(time.Second))
			if waited >= c.cfg.BranchPipelineDeadline {
				// One extra unfiltered lookup, only on the failure path: naming the
				// pipelines that DO exist for this SHA is what separates "workflow
				// rules only build merge_request_event" from "CI never ran at all",
				// and the operator needs that distinction to fix it.
				return partialResp(), branchPipelineUnavailableError(
					mr, req.MRIID, waited, c.cfg.BranchPipelineDeadline,
					c.pipelineDigestForSHA(pollCtx, mr.SHA))
			}
		} else {
			branchPipelinePendingSHA = ""
			lastPipelineURL = pipe.WebURL
			lastStatus = pipe.Status
			fmt.Fprintf(&logTail, "[%s] pipeline %d (%s) status=%s %s\n", time.Now().Format(time.RFC3339), pipe.ID, pipe.Source, pipe.Status, pipe.WebURL)
			if terminal[pipe.Status] {
				var failedJobReasons []string
				var failedJobs []pipeline.FailedJob
				if pipe.Status != "success" {
					var jobErr error
					failedJobs, jobErr = c.failedJobs(pollCtx, pipe.ID)
					if jobErr != nil {
						fmt.Fprintf(&logTail, "[%s] failed-job inspection unavailable: %v; retaining code classification\n", time.Now().Format(time.RFC3339), jobErr)
						failedJobReasons = nil
						failedJobs = nil
					} else {
						for _, job := range failedJobs {
							failedJobReasons = append(failedJobReasons, job.FailureReason)
						}
					}
				}
				return pipeline.PollPipelineResponse{
					Status:           pipe.Status,
					Project:          c.cfg.Project,
					SourceBranch:     mr.SourceBranch,
					TargetBranch:     mr.TargetBranch,
					SHA:              mr.SHA,
					LogTail:          logTail.String(),
					PipelineURL:      pipe.WebURL,
					FailedJobReasons: failedJobReasons,
					FailedJobs:       failedJobs,
				}, nil
			}
		}

		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return partialResp(), ctx.Err()
			}
			return timeoutResp()
		case <-time.After(c.cfg.PollInterval):
		}
	}
}

// defaultHeadSHADeadline bounds the wait for an MR to report any head SHA.
// GitLab sets `sha` when it finishes preparing the MR diff — seconds in
// practice — so five minutes is ~60× the observed prepare time while staying
// far inside the 30m poll deadline it replaces for this failure mode.
const defaultHeadSHADeadline = 5 * time.Minute

// defaultBranchPipelineDeadline bounds the wait for a push pipeline to appear
// for a head SHA GitLab has already published. Pipeline creation is part of
// accepting the push, so ten minutes is a wide margin over the seconds it takes
// in practice while still leaving the remaining poll deadline for a pipeline
// that exists but is slow. Longer than defaultHeadSHADeadline on purpose:
// runner backlog can delay a pipeline becoming visible, whereas an MR's `sha`
// is computed by GitLab itself.
const defaultBranchPipelineDeadline = 10 * time.Minute

// headSHAUnresolvable reports whether GitLab has already published a reason the
// MR can never report a head SHA, so the bounded wait is pointless. Deliberately
// narrow: transient prepare states ("checking", "unchecked", "preparing") are
// NOT listed and still get the full head-SHA deadline.
func headSHAUnresolvable(mr mrResponse) bool {
	if mr.State != "" && mr.State != "opened" {
		// A closed/merged/locked MR will never grow a head pipeline; ci_watch
		// polling it can only time out.
		return true
	}
	// A conflicted MR has no computable diff, so GitLab publishes no head SHA
	// until the source branch is rebased — an external act, not something a
	// re-poll can produce.
	return mr.HasConflicts || mr.MergeStatus == "cannot_be_merged"
}

// headSHAUnavailableError builds the operator-facing head-SHA failure. It names
// the MR state that blocks the head so the escalation says what to do (rebase,
// repush, reopen) instead of pointing at a pipeline that was never created.
func headSHAUnavailableError(mr mrResponse, mrIID int64, waited, deadline time.Duration) error {
	detail := fmt.Sprintf("state=%s merge_status=%s", orUnknownMRField(mr.State), orUnknownMRField(mr.MergeStatus))
	if mr.DetailedMergeStatus != "" {
		detail += " detailed_merge_status=" + mr.DetailedMergeStatus
	}
	if mr.HasConflicts {
		detail += " has_conflicts=true"
	}
	if mr.WebURL != "" {
		detail += " (" + mr.WebURL + ")"
	}
	return fmt.Errorf(
		"gitlab: mr %d reported no head sha after %s (bound %s): %s: %w",
		mrIID, waited.Round(time.Second), deadline, detail, pipeline.ErrMRHeadSHAUnavailable)
}

func orUnknownMRField(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// mrURLNote renders " (<web_url>)" for an MR, or "" when GitLab returned none.
func mrURLNote(mr mrResponse) string {
	if mr.WebURL == "" {
		return ""
	}
	return " (" + mr.WebURL + ")"
}

// branchPipelineUnavailableError builds the operator-facing failure for a head
// SHA that never grew a push pipeline. digest names whatever pipelines DO exist
// for the SHA, which is the whole diagnosis: "merge_request_event/success" means
// the project's workflow rules never build pushes, while "none" means CI never
// ran for the commit at all.
func branchPipelineUnavailableError(mr mrResponse, mrIID int64, waited, deadline time.Duration, digest string) error {
	detail := fmt.Sprintf("state=%s merge_status=%s", orUnknownMRField(mr.State), orUnknownMRField(mr.MergeStatus))
	if digest != "" {
		detail += "; pipelines for this sha: " + digest
	}
	return fmt.Errorf(
		"gitlab: mr %d head %s has no push pipeline after %s (bound %s): %s%s: %w",
		mrIID, shortSHA(mr.SHA), waited.Round(time.Second), deadline, detail, mrURLNote(mr),
		pipeline.ErrBranchPipelineUnavailable)
}

// pipelineDigestForSHA summarizes every pipeline GitLab has for a SHA,
// regardless of source or ref — the deliberately UNfiltered counterpart to
// branchPipelineForSHA. Best-effort and only called once, on the
// branch-pipeline-deadline failure path: it costs an API call and its only job
// is to make the escalation say why no push pipeline exists. Returns "none"
// when GitLab has no pipelines at all and "" when the lookup itself failed
// (nothing useful to claim).
func (c *GitLabClient) pipelineDigestForSHA(ctx context.Context, sha string) string {
	path := fmt.Sprintf("/projects/%s/pipelines?sha=%s&order_by=id&sort=desc&per_page=5",
		c.projectPath(), url.QueryEscape(sha))
	var pipes []shaPipeline
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipes); err != nil {
		return ""
	}
	if len(pipes) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(pipes))
	for _, p := range pipes {
		parts = append(parts, fmt.Sprintf("%s/%s@%s", orUnknownMRField(p.Source), orUnknownMRField(p.Status), orUnknownMRField(p.Ref)))
	}
	return strings.Join(parts, ", ")
}

// branchPipelineForSHA returns the most recent pipeline for the exact source,
// SHA, and branch tuple. All three filters are load-bearing: project-wide lists
// can contain the same commit on branches or pipeline sources with different CI
// rules and variables. ok=false when no matching pipeline exists yet.
func (c *GitLabClient) branchPipelineForSHA(ctx context.Context, sha, ref, source string) (shaPipeline, bool, error) {
	if strings.TrimSpace(ref) == "" {
		return shaPipeline{}, false, fmt.Errorf("gitlab: source branch required to resolve branch pipeline: %w", pipeline.ErrMergeAuthorizationStale)
	}
	if strings.TrimSpace(source) == "" {
		return shaPipeline{}, false, fmt.Errorf("gitlab: pipeline source required to resolve branch pipeline: %w", pipeline.ErrMergeAuthorizationStale)
	}
	path := fmt.Sprintf("/projects/%s/pipelines?sha=%s&ref=%s&source=%s&order_by=id&sort=desc&per_page=20",
		c.projectPath(), url.QueryEscape(sha), url.QueryEscape(ref), url.QueryEscape(source))
	var pipes []shaPipeline
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipes); err != nil {
		return shaPipeline{}, false, err
	}
	if len(pipes) == 0 {
		return shaPipeline{}, false, nil
	}
	p := pipes[0]
	if p.SHA != sha || p.Ref != ref || p.Source != source {
		return shaPipeline{}, false, fmt.Errorf("gitlab: pipeline %d identity %s:%s@%s does not match requested %s:%s@%s: %w", p.ID, p.Source, p.Ref, p.SHA, source, ref, sha, pipeline.ErrMergeAuthorizationStale)
	}
	return p, true, nil
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ----- Merge -----

type mergeBody struct {
	MergeWhenPipelineSucceeds bool   `json:"merge_when_pipeline_succeeds,omitempty"`
	Squash                    bool   `json:"squash,omitempty"`
	SHA                       string `json:"sha,omitempty"`
}

const (
	// mergeReadyTimeout bounds the single retry window shared by GitLab's
	// transient 405 and 422 mergeability responses. Transitions between those
	// states never reset the deadline.
	mergeReadyTimeout = 2 * time.Minute
	// branchMergeSettleAttempts caps total 422 responses within that shared
	// window. A persistent 422 requires a new external rebase + gate/CI cycle.
	branchMergeSettleAttempts = 3
)

// Merge implements pipeline.GitLabClient.
//
// The merge faces two distinct 405s:
//
//  1. A benign TIMING 405 while GitLab settles merge_status right after the
//     branch pipeline turns green. retryMerge polls this away.
//  2. A PERSISTENT 405 from the spurious failed `merge_request_event`
//     placeholder pipeline. GitLab attaches it as the MR head_pipeline on
//     MR-open EVEN with `workflow.rules: merge_request_event → never` and no
//     merge_when_pipeline_succeeds — the rule suppresses the jobs but GitLab
//     still records a 0-job `failed` pipeline and pins it as head. Under
//     only_allow_merge_if_pipeline_succeeds that failed head blocks merge
//     forever (detailed_merge_status=ci_must_pass). Verified live on !770
//     (2026-06-23): the no-MWPS + branch-poll fixes reached a green ci_watch
//     yet merge still 405'd. prepareMergeNotReady starts or resumes a same-SHA
//     API pipeline to supersede it without mutating MR state.
func (c *GitLabClient) Merge(ctx context.Context, req pipeline.MergeRequestArgs) (pipeline.MergeResponse, error) {
	if req.MRIID == 0 {
		return pipeline.MergeResponse{}, errors.New("gitlab: MRIID required")
	}
	auth, err := c.mergeAuthorization(req)
	if err != nil {
		return pipeline.MergeResponse{}, err
	}
	return c.retryMerge(ctx, req.MRIID, auth)
}

type mergeAuthorization struct {
	project                         string
	sourceBranch                    string
	targetBranch                    string
	sha                             string
	recoveryPipelineCreateAttempted bool
}

// mergeAuthorization validates the durable CI tuple before any GitLab
// request. In particular, a backlog reroute cannot point a resumed stage at a
// different project that happens to contain the same MR IID.
func (c *GitLabClient) mergeAuthorization(req pipeline.MergeRequestArgs) (mergeAuthorization, error) {
	auth := mergeAuthorization{
		project:                         strings.TrimSpace(req.Project),
		sourceBranch:                    strings.TrimSpace(req.SourceBranch),
		targetBranch:                    strings.TrimSpace(req.TargetBranch),
		sha:                             strings.TrimSpace(req.ExpectedSHA),
		recoveryPipelineCreateAttempted: req.RecoveryPipelineCreateAttempted,
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"project", auth.project},
		{"source branch", auth.sourceBranch},
		{"target branch", auth.targetBranch},
		{"SHA", auth.sha},
	} {
		if field.value == "" {
			return mergeAuthorization{}, fmt.Errorf("gitlab: expected %s required for merge authorization: %w", field.name, pipeline.ErrMergeAuthorizationStale)
		}
	}
	if auth.project != c.cfg.Project {
		return mergeAuthorization{}, fmt.Errorf("gitlab: CI-authorized project %q does not match client project %q: %w", auth.project, c.cfg.Project, pipeline.ErrMergeAuthorizationStale)
	}
	return auth, nil
}

type mergeMRState uint8

const (
	mergeMROpened mergeMRState = iota
	mergeMRLocked
	mergeMRAlreadyMerged
)

// reconcileMergeState requires the live MR source, target, and SHA to match the
// durable CI authorization. Merged MRs return their authoritative identity;
// closed MRs are terminal; locked MRs are observed without issuing a merge PUT.
func (c *GitLabClient) reconcileMergeState(ctx context.Context, mrIID int64, auth mergeAuthorization) (pipeline.MergeResponse, mrResponse, mergeMRState, error) {
	mr, err := c.getMR(ctx, mrIID)
	if err != nil {
		return pipeline.MergeResponse{}, mrResponse{}, mergeMROpened, err
	}
	if err := validateMRMergeIdentity(mrIID, mr, auth); err != nil {
		return pipeline.MergeResponse{}, mr, mergeMROpened, err
	}
	switch mr.State {
	case "merged":
		resp, err := mergedResponse(mrIID, mr, auth)
		return resp, mr, mergeMRAlreadyMerged, err
	case "closed":
		return pipeline.MergeResponse{}, mr, mergeMROpened, fmt.Errorf("mr %d is closed; refusing automatic reopen: %w", mrIID, pipeline.ErrMergeRequestClosed)
	case "locked":
		return pipeline.MergeResponse{}, mr, mergeMRLocked, nil
	case "opened":
		return pipeline.MergeResponse{}, mr, mergeMROpened, nil
	default:
		return pipeline.MergeResponse{}, mr, mergeMROpened, fmt.Errorf("mr %d has unsupported state %q: %w", mrIID, mr.State, pipeline.ErrMergeAuthorizationStale)
	}
}

func validateMRMergeIdentity(mrIID int64, mr mrResponse, auth mergeAuthorization) error {
	for _, identity := range []struct {
		name string
		got  string
		want string
	}{
		{"source branch", mr.SourceBranch, auth.sourceBranch},
		{"target branch", mr.TargetBranch, auth.targetBranch},
		{"source sha", mr.SHA, auth.sha},
	} {
		if identity.got == identity.want {
			continue
		}
		if identity.name != "source sha" {
			// A branch mismatch is a routing defect, not a head movement.
			return fmt.Errorf("mr %d %s %q no longer matches CI-authorized %s %q: %w", mrIID, identity.name, identity.got, identity.name, identity.want, pipeline.ErrMergeAuthorizationStale)
		}
		// The MR is still the authorized one but its head moved. Surface both
		// SHAs structurally so the runner can mint a durable external
		// head-transition row (#374) rather than parse them out of a string.
		// The message text stays byte-identical to the historical form, so
		// error-class needles and operator-facing strings are unchanged.
		return &pipeline.MergeSourceSHAMismatchError{
			MRIID:        mrIID,
			Project:      auth.project,
			SourceBranch: auth.sourceBranch,
			TargetBranch: auth.targetBranch,
			ReviewedSHA:  auth.sha,
			ObservedSHA:  mr.SHA,
			Message: fmt.Sprintf("mr %d %s %q no longer matches CI-authorized %s %q: %s",
				mrIID, identity.name, identity.got, identity.name, identity.want,
				pipeline.ErrMergeAuthorizationStale),
		}
	}
	return nil
}

func (c *GitLabClient) getMR(ctx context.Context, mrIID int64) (mrResponse, error) {
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", c.projectPath(), mrIID)
	var mr mrResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &mr); err != nil {
		return mrResponse{}, err
	}
	return mr, nil
}

// retryMerge handles validation, merge attempts, and recovery in one
// non-recursive loop. Ordinary 405/422/locked settling uses the short merge
// readiness deadline. Once an exact MR read proves that a 405 requires an API
// superseder, the same operation may consume the longer pipeline deadline;
// both clocks start here, so changing state never mints a fresh retry budget.
// Every merge PUT is immediately preceded by an exact-identity MR GET; this is
// the strongest available target-branch check because GitLab's merge endpoint
// only offers SHA as an atomic precondition.
func (c *GitLabClient) retryMerge(ctx context.Context, mrIID int64, auth mergeAuthorization) (mergeResp pipeline.MergeResponse, mergeRetErr error) {
	// A completed close+reopen cycle is appended to whatever error this call
	// finally surfaces, so the escalation carries the original 405 plus what the
	// recovery observed. %w keeps the errors.Is chain and the literal
	// "status 405" substring, so pipeline.Classify still returns ClassConfig.
	recovery405Note := ""
	defer func() {
		if mergeRetErr != nil && recovery405Note != "" {
			mergeRetErr = fmt.Errorf("%w (405 recovery: %s)", mergeRetErr, recovery405Note)
		}
	}()
	settleTimeout := c.mergeSettleTimeout
	if settleTimeout <= 0 {
		settleTimeout = mergeReadyTimeout
	}
	overallTimeout := c.mergeRetryTimeout
	if overallTimeout > 0 && c.mergeSettleTimeout <= 0 {
		// Existing focused tests use mergeRetryTimeout as a complete operation
		// override. Keep their ordinary settle path on the same bounded clock.
		settleTimeout = overallTimeout
	}
	if overallTimeout <= 0 {
		// Reserve the original settle window beyond the pipeline poll session.
		// Both clocks still start above: this is not a reset. It lets a known,
		// still-running superseder hit its own PollDeadline first and surface the
		// retryable ErrPipelinePollTimeout classification; a subsequent Runner
		// attempt can then reattach through the durable create fence.
		overallTimeout = c.cfg.PollDeadline + settleTimeout
		if overallTimeout < settleTimeout {
			overallTimeout = settleTimeout
		}
	}
	startedAt := time.Now()
	settleDeadline := startedAt.Add(settleTimeout)
	overallDeadline := startedAt.Add(overallTimeout)
	mergeCtx, cancel := context.WithDeadline(ctx, overallDeadline)
	defer cancel()
	branch422s := 0
	supersederHandled := false
	// reopenAttempted caps the stale-405 close+reopen at exactly one cycle per
	// merge call (one stage attempt). A second 405 after the cycle escalates.
	reopenAttempted := false
	recoveryInProgress := false
	sawLocked := false
	var mergeErr error
	activeDeadline := func() time.Time {
		if recoveryInProgress {
			return overallDeadline
		}
		return settleDeadline
	}
	activeContext := func() (context.Context, context.CancelFunc, time.Time) {
		deadline := activeDeadline()
		opCtx, opCancel := context.WithDeadline(mergeCtx, deadline)
		return opCtx, opCancel, deadline
	}
	deadlineResult := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if mergeCtx.Err() != nil || !time.Now().Before(activeDeadline()) {
			return mergeDeadlineError(mrIID, mergeErr, sawLocked)
		}
		return nil
	}
	for {
		if errors.Is(mergeErr, pipeline.ErrMergeRequestLocked) {
			sawLocked = true
		}
		if err := deadlineResult(); err != nil {
			return pipeline.MergeResponse{}, err
		}
		opCtx, opCancel, opDeadline := activeContext()
		reconciled, mr, state, stateErr := c.reconcileMergeState(opCtx, mrIID, auth)
		opCompletedWithinDeadline := time.Now().Before(opDeadline)
		opCancel()
		if !opCompletedWithinDeadline {
			return pipeline.MergeResponse{}, mergeDeadlineError(mrIID, mergeErr, sawLocked)
		}
		if stateErr != nil {
			if err := deadlineResult(); err != nil {
				return pipeline.MergeResponse{}, err
			}
			return pipeline.MergeResponse{}, fmt.Errorf("gitlab: reconcile mr %d before merge: %w", mrIID, stateErr)
		}
		if state == mergeMRAlreadyMerged {
			return reconciled, nil
		}
		if mergeRecoveryPipelineRequired(mergeErr, mr) {
			recoveryInProgress = true
		}
		if state == mergeMRLocked {
			// A lock supersedes any prior 405/422 signal. Wait without issuing a
			// PUT, then re-read authoritative state and identity.
			mergeErr = nil
			sawLocked = true
			if err := c.waitForMergeRetry(mergeCtx, activeDeadline()); err != nil {
				if deadlineErr := deadlineResult(); deadlineErr != nil {
					return pipeline.MergeResponse{}, deadlineErr
				}
				if errors.Is(err, errMergeRetryDeadline) {
					return pipeline.MergeResponse{}, fmt.Errorf("gitlab: mr %d remained locked until merge readiness deadline: %w", mrIID, pipeline.ErrMergeRequestLocked)
				}
				return pipeline.MergeResponse{}, err
			}
			continue
		}
		if errors.Is(mergeErr, pipeline.ErrMergeRequestLocked) {
			// A merge response can report locked just before the MR GET flips back
			// to opened. Consume the same deadline and revalidate once more instead
			// of hammering PUT or surfacing a transient before the in-stage budget.
			sawLocked = true
			if err := c.waitForMergeRetry(mergeCtx, activeDeadline()); err != nil {
				if deadlineErr := deadlineResult(); deadlineErr != nil {
					return pipeline.MergeResponse{}, deadlineErr
				}
				if errors.Is(err, errMergeRetryDeadline) {
					return pipeline.MergeResponse{}, fmt.Errorf("gitlab: mr %d remained locked until merge readiness deadline: %w", mrIID, pipeline.ErrMergeRequestLocked)
				}
				return pipeline.MergeResponse{}, err
			}
			opCtx, opCancel, opDeadline = activeContext()
			reconciled, _, state, stateErr = c.reconcileMergeState(opCtx, mrIID, auth)
			opCompletedWithinDeadline = time.Now().Before(opDeadline)
			opCancel()
			if !opCompletedWithinDeadline {
				return pipeline.MergeResponse{}, mergeDeadlineError(mrIID, mergeErr, sawLocked)
			}
			if stateErr != nil {
				if err := deadlineResult(); err != nil {
					return pipeline.MergeResponse{}, err
				}
				return pipeline.MergeResponse{}, fmt.Errorf("gitlab: reconcile mr %d after locked merge response: %w", mrIID, stateErr)
			}
			if state == mergeMRAlreadyMerged {
				return reconciled, nil
			}
			if state == mergeMRLocked {
				mergeErr = nil
				continue
			}
			mergeErr = nil
		}
		if err := deadlineResult(); err != nil {
			return pipeline.MergeResponse{}, err
		}

		if mergeErr != nil {
			if !isMergeNotReady(mergeErr) {
				return pipeline.MergeResponse{}, mergeErr
			}

			waitBeforeRetry := true
			if isBranchCannotBeMerged(mergeErr) {
				branch422s++
				if branch422s >= branchMergeSettleAttempts {
					return pipeline.MergeResponse{}, mergeErr
				}
			} else {
				handled, err := c.prepareMergeNotReady(mergeCtx, mr, auth, supersederHandled, activeDeadline())
				if err != nil {
					if deadlineErr := deadlineResult(); deadlineErr != nil {
						return pipeline.MergeResponse{}, deadlineErr
					}
					if errors.Is(err, errMergeRetryDeadline) {
						return pipeline.MergeResponse{}, mergeErr
					}
					return pipeline.MergeResponse{}, fmt.Errorf("gitlab: merge readiness recovery failed: %w", err)
				}
				supersederHandled = supersederHandled || handled
				waitBeforeRetry = !handled

				// The superseding-pipeline path owns merge_request_event/api
				// heads. Everything else that 405s while GitLab itself reports
				// the MR ready is a stale head_pipeline / mergeability cache,
				// whose only remedy is an MR state transition (!768).
				if !handled && !reopenAttempted && stale405ReopenApplies(mergeErr, mr) {
					reopenAttempted = true
					note, rerr := c.reopenStaleMergeHead(mergeCtx, mr, auth, activeDeadline())
					if rerr != nil {
						// Fall through to today's escalation carrying the
						// original 405 plus what the recovery observed.
						return pipeline.MergeResponse{}, fmt.Errorf("%w (405 recovery failed: %v)", mergeErr, rerr)
					}
					recovery405Note = note
					waitBeforeRetry = false
				}
			}

			// A successful same-identity superseder skips the ordinary settle sleep,
			// but it does not extend the original shared deadline.
			if waitBeforeRetry {
				if err := c.waitForMergeRetry(mergeCtx, activeDeadline()); err != nil {
					if deadlineErr := deadlineResult(); deadlineErr != nil {
						return pipeline.MergeResponse{}, deadlineErr
					}
					if errors.Is(err, errMergeRetryDeadline) {
						return pipeline.MergeResponse{}, mergeErr
					}
					return pipeline.MergeResponse{}, err
				}
			}

			// Waiting and superseder polling create a retarget/source-change window;
			// close it with a fresh GET immediately before the next merge PUT.
			opCtx, opCancel, opDeadline = activeContext()
			reconciled, mr, state, stateErr = c.reconcileMergeState(opCtx, mrIID, auth)
			opCompletedWithinDeadline = time.Now().Before(opDeadline)
			opCancel()
			if !opCompletedWithinDeadline {
				return pipeline.MergeResponse{}, mergeDeadlineError(mrIID, mergeErr, sawLocked)
			}
			if stateErr != nil {
				if err := deadlineResult(); err != nil {
					return pipeline.MergeResponse{}, err
				}
				return pipeline.MergeResponse{}, fmt.Errorf("gitlab: reconcile mr %d after merge wait: %w", mrIID, stateErr)
			}
			if state == mergeMRAlreadyMerged {
				return reconciled, nil
			}
			if mergeRecoveryPipelineRequired(mergeErr, mr) {
				recoveryInProgress = true
			}
			if state == mergeMRLocked {
				mergeErr = nil
				sawLocked = true
				continue
			}
		}

		if err := deadlineResult(); err != nil {
			return pipeline.MergeResponse{}, err
		}

		// No network call occurs between the exact MR reconciliation above and
		// this PUT. GitLab atomically enforces auth.sha in the request body.
		opCtx, opCancel, opDeadline = activeContext()
		resp, err := c.mergeOnce(opCtx, mrIID, auth)
		opCompletedWithinDeadline = time.Now().Before(opDeadline)
		opCancel()
		if err == nil {
			return resp, nil
		}
		mergeErr = err
		if opCompletedWithinDeadline && mergeRecoveryPipelineRequired(mergeErr, mr) {
			recoveryInProgress = true
		}
		if errors.Is(err, pipeline.ErrMergeRequestLocked) {
			sawLocked = true
		}
		if deadlineErr := deadlineResult(); deadlineErr != nil {
			return pipeline.MergeResponse{}, deadlineErr
		}
	}
}

var errMergeRetryDeadline = errors.New("gitlab: merge readiness deadline reached")

func mergeDeadlineError(mrIID int64, mergeErr error, sawLocked bool) error {
	if mergeErr != nil {
		return mergeErr
	}
	if sawLocked {
		return fmt.Errorf("gitlab: mr %d remained locked until merge readiness deadline: %w", mrIID, pipeline.ErrMergeRequestLocked)
	}
	return fmt.Errorf("gitlab: merge readiness deadline reached before PUT: %w", context.DeadlineExceeded)
}

func (c *GitLabClient) waitForMergeRetry(ctx context.Context, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errMergeRetryDeadline
	}
	wait := c.cfg.PollInterval
	if wait > remaining {
		wait = remaining
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// mergeOnce PUTs .../merge a single time and maps the response.
func (c *GitLabClient) mergeOnce(ctx context.Context, mrIID int64, auth mergeAuthorization) (pipeline.MergeResponse, error) {
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/merge", c.projectPath(), mrIID)
	var got mrResponse
	if err := c.requestJSON(ctx, http.MethodPut, path, mergeBody{SHA: auth.sha}, &got); err != nil {
		return pipeline.MergeResponse{}, err
	}
	if got.State == "merged" {
		return mergedResponse(mrIID, got, auth)
	}
	if got.State == "locked" {
		return pipeline.MergeResponse{}, fmt.Errorf("gitlab: merge response left mr %d locked: %w", mrIID, pipeline.ErrMergeRequestLocked)
	}
	if got.MergeError != "" {
		return pipeline.MergeResponse{}, fmt.Errorf("gitlab: merge failed: %s", got.MergeError)
	}
	return pipeline.MergeResponse{}, fmt.Errorf("gitlab: merge response state %q, want merged", got.State)
}

func mergedResponse(mrIID int64, mr mrResponse, auth mergeAuthorization) (pipeline.MergeResponse, error) {
	if mr.State != "merged" {
		return pipeline.MergeResponse{}, fmt.Errorf("gitlab: merge response state %q, want merged", mr.State)
	}
	if err := validateMRMergeIdentity(mrIID, mr, auth); err != nil {
		return pipeline.MergeResponse{}, fmt.Errorf("gitlab: merged response identity mismatch: %w", err)
	}
	for _, sha := range []string{mr.MergedCommitSHA, mr.MergeCommitSHA, mr.SquashCommitSHA, mr.SHA} {
		if sha = strings.TrimSpace(sha); sha != "" {
			return pipeline.MergeResponse{MergedSHA: sha}, nil
		}
	}
	return pipeline.MergeResponse{}, errors.New("gitlab: merged response has no authoritative commit identity")
}

// prepareMergeNotReady handles a persistent 405 without closing, reopening, or
// deleting anything. If GitLab still pins the failed merge_request_event head,
// it resumes the newest same-SHA API pipeline or creates exactly one. A restart
// rediscovers that pipeline through the SHA-filtered list endpoint and resumes
// polling it under PollDeadline.
func (c *GitLabClient) prepareMergeNotReady(ctx context.Context, mr mrResponse, auth mergeAuthorization, supersederHandled bool, deadline time.Time) (bool, error) {
	if !time.Now().Before(deadline) {
		return false, errMergeRetryDeadline
	}
	opCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if mr.State != "opened" {
		return false, fmt.Errorf("mr %d state %q is not open for merge recovery: %w", mr.IID, mr.State, pipeline.ErrMergeAuthorizationStale)
	}
	if err := validateMRMergeIdentity(mr.IID, mr, auth); err != nil {
		return false, err
	}
	if mr.HeadPipeline.Source != "merge_request_event" && mr.HeadPipeline.Source != "api" {
		return false, nil
	}
	if supersederHandled {
		return false, nil
	}

	pipe, ok, err := c.branchPipelineForSHA(opCtx, auth.sha, auth.sourceBranch, "api")
	if err != nil {
		return false, fmt.Errorf("find same-SHA superseding pipeline: %w", mergeOperationError(ctx, deadline, err))
	}
	// A same-identity API pipeline only supersedes the pinned head when it is
	// at least as new as that head. In particular, an older successful API
	// pipeline cannot displace a newer failed merge_request_event pipeline.
	if ok && pipelineCanSupersede(mr, pipe) {
		if err := c.awaitSupersedingPipeline(ctx, mr.IID, pipe, auth.sha, auth.sourceBranch, deadline); err != nil {
			return false, err
		}
		return true, nil
	}
	if mr.HeadPipeline.Source == "api" {
		// GitLab may expose the new API pipeline as head before it is visible in
		// the filtered project list. Let the shared retry deadline absorb that
		// eventual-consistency window without creating a duplicate pipeline.
		return false, nil
	}
	if !time.Now().Before(deadline) {
		return false, errMergeRetryDeadline
	}
	if auth.recoveryPipelineCreateAttempted {
		// A prior process persisted the fence before POST and may have died after
		// GitLab accepted it but before the response arrived. The exact API
		// pipeline can lag both this filtered list and mr.head_pipeline, so keep
		// reconciling under the existing shared deadline. This path never POSTs:
		// if the process died before sending the request, it simply times out.
		recovered, err := c.waitForAmbiguousPipelineCreate(opCtx, mr, auth, deadline)
		if err != nil {
			return false, err
		}
		if err := c.awaitSupersedingPipeline(opCtx, mr.IID, recovered, auth.sha, auth.sourceBranch, deadline); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := pipeline.RecordMergeRecoveryPipelineCreate(ctx); err != nil {
		return false, fmt.Errorf("persist recovery pipeline creation fence for mr %d: %w", mr.IID, err)
	}
	newPipe, err := c.createBranchPipeline(opCtx, auth.sourceBranch)
	if err != nil {
		err = mergeOperationError(ctx, deadline, err)
		if errors.Is(err, errMergeRetryDeadline) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		if status, ok := GitLabHTTPStatus(err); ok && status < http.StatusInternalServerError {
			// The durable fence is monotonic because ambiguous POST failures must
			// never permit a duplicate create after restart. A definitive 4xx means
			// GitLab did not accept this create, but the same fence cannot safely be
			// reused as a transient retry signal. Fail terminal on the first attempt
			// (including 429) instead of advertising a retry that could only wait for
			// a pipeline known not to exist.
			return false, fmt.Errorf("create superseding branch pipeline for %s rejected with status %d: %w (%v)", auth.sourceBranch, status, pipeline.ErrMergeRecoveryConfig, err)
		}
		// A network failure, 5xx, or decode error is ambiguous: GitLab may have
		// created the pipeline even though the response was lost. Reconcile the
		// exact same-SHA/ref API pipeline under the existing deadline before any
		// later Runner attempt is allowed to consider another POST.
		recovered, rerr := c.waitForAmbiguousPipelineCreate(opCtx, mr, auth, deadline)
		if rerr != nil {
			return false, rerr
		}
		if err := c.awaitSupersedingPipeline(opCtx, mr.IID, recovered, auth.sha, auth.sourceBranch, deadline); err != nil {
			return false, err
		}
		return true, nil
	}
	if newPipe.ID <= mr.HeadPipeline.ID {
		return false, fmt.Errorf("created superseding pipeline %d is not newer than pinned head pipeline %d: %w", newPipe.ID, mr.HeadPipeline.ID, pipeline.ErrMergeAuthorizationStale)
	}
	if err := c.awaitSupersedingPipeline(ctx, mr.IID, newPipe, auth.sha, auth.sourceBranch, deadline); err != nil {
		return false, err
	}
	return true, nil
}

const (
	// stale405ReopenAttempts bounds the reopen leg of the single close+reopen
	// cycle. The close has already landed by then, so a failed reopen leaves
	// the MR strictly worse off than the 405 it was clearing — retry hard.
	stale405ReopenAttempts = 3
	// stale405ReopenGrace is the reopen leg's own budget. It is deliberately
	// detached from the merge deadlines: a merge clock expiring between the
	// close and the reopen must never strand the MR closed.
	stale405ReopenGrace = 60 * time.Second
)

// stale405ReopenApplies gates the close+reopen recovery to exactly one shape:
// a genuine HTTP 405 from the merge PUT, on a still-open MR, whose head
// pipeline does not belong to the superseding-pipeline path, and which GitLab
// itself reports as either ready to merge or blocked only on a pipeline
// (head_pipeline missing or stale — the !768 recipe). Every other 405 cause
// (draft, unmet approvals, unresolved discussions, conflicts, needs rebase,
// not open, still checking) is unfixable by an MR state transition and must
// never trigger one, so anything outside the allowlist fails closed.
func stale405ReopenApplies(err error, mr mrResponse) bool {
	status, ok := GitLabHTTPStatus(err)
	if !ok || status != http.StatusMethodNotAllowed {
		return false
	}
	if mr.State != "opened" {
		return false
	}
	if mr.HeadPipeline.Source == "merge_request_event" || mr.HeadPipeline.Source == "api" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(mr.DetailedMergeStatus)) {
	case "mergeable", "ci_must_pass":
		// "mergeable": GitLab contradicts itself — it reports the MR ready and
		// still refuses the PUT. "ci_must_pass": the pinned head pipeline is
		// missing or stale while the branch has a green one. Both recompute.
		return true
	default:
		// Includes the empty string, so a GitLab too old to report
		// detailed_merge_status never has its MRs mutated.
		return false
	}
}

// reopenStaleMergeHead performs one bounded close+reopen cycle so GitLab
// recomputes head_pipeline and mergeability against the branch's existing
// green pipeline, and returns a human-readable note describing what it
// observed. It never merges: control returns to retryMerge, whose loop
// re-reconciles the exact CI-authorized identity before the next PUT, so the
// cycle cannot bypass the head-SHA authorization fence (#374). It also never
// touches the merge-recovery pipeline create fence — nothing here POSTs a
// pipeline.
func (c *GitLabClient) reopenStaleMergeHead(ctx context.Context, mr mrResponse, auth mergeAuthorization, deadline time.Time) (string, error) {
	if !time.Now().Before(deadline) {
		return "", errMergeRetryDeadline
	}
	opCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// Re-assert both halves of the authorization on the caller's freshly read
	// MR before mutating anything.
	if mr.State != "opened" {
		return "", fmt.Errorf("mr %d state %q is not open for 405 reopen recovery: %w", mr.IID, mr.State, pipeline.ErrMergeAuthorizationStale)
	}
	if err := validateMRMergeIdentity(mr.IID, mr, auth); err != nil {
		return "", err
	}
	// Only mutate MR state when the pipeline a recompute would actually land on
	// — the newest one for the exact CI-authorized SHA — is already green.
	// Without that the cycle is at best churn on somebody's MR and at worst
	// swaps a stale head for a blocking one.
	green, ok, err := c.newestGreenPipelineForSHA(opCtx, auth.sha, auth.sourceBranch)
	if err != nil {
		return "", fmt.Errorf("find green pipeline for %s@%s: %w", auth.sourceBranch, shortSHA(auth.sha), mergeOperationError(ctx, deadline, err))
	}
	if !ok {
		if green.ID != 0 {
			return "", fmt.Errorf("newest pipeline %d for %s@%s is %s (source %s), not success; a close+reopen would pin it as head: %w",
				green.ID, auth.sourceBranch, shortSHA(auth.sha), green.Status, green.Source, pipeline.ErrMergeRecoveryConfig)
		}
		return "", fmt.Errorf("no successful pipeline for %s@%s to recompute head_pipeline against: %w", auth.sourceBranch, shortSHA(auth.sha), pipeline.ErrMergeRecoveryConfig)
	}

	beforeHead := mr.HeadPipeline
	beforeDetailed := mr.DetailedMergeStatus
	pipeline.RecordMergeRecovery405(ctx, map[string]any{
		"mr":                       mr.IID,
		"sha":                      auth.sha,
		"source_branch":            auth.sourceBranch,
		"green_pipeline":           green.ID,
		"head_pipeline":            beforeHead.ID,
		"head_pipeline_source":     beforeHead.Source,
		"head_pipeline_status":     beforeHead.Status,
		"merge_status":             mr.MergeStatus,
		"detailed_merge_status":    beforeDetailed,
		"recovery":                 "close_reopen",
		"merge_request_web_url":    mr.WebURL,
		"green_pipeline_web_url":   green.WebURL,
		"green_pipeline_status":    green.Status,
		"green_pipeline_source":    green.Source,
		"authorized_target_branch": auth.targetBranch,
	})

	// Everything from here through the post-reopen verification runs on a
	// context detached from every merge clock, with its own budget. Once the
	// gate has decided to mutate, a merge deadline expiring mid-cycle must not
	// be able to abandon the MR closed or leave the outcome unobserved.
	mutateCtx, cancelMutate := context.WithTimeout(context.WithoutCancel(ctx), stale405ReopenGrace)
	defer cancelMutate()

	if err := c.closeReopenMR(mutateCtx, mr.IID); err != nil {
		return "", err
	}

	// Confirm the MR came back open with an unchanged authorized identity. A
	// reopen that did not stick is a loud failure: the MR is worse off closed
	// than it was 405ing.
	after, err := c.getMR(mutateCtx, mr.IID)
	if err != nil {
		return "", fmt.Errorf("re-read mr %d after close+reopen: %w", mr.IID, err)
	}
	if after.State != "opened" {
		return "", fmt.Errorf("mr %d is %q after close+reopen, not reopened: %w", mr.IID, after.State, pipeline.ErrMergeRecoveryConfig)
	}
	if err := validateMRMergeIdentity(mr.IID, after, auth); err != nil {
		return "", err
	}
	// Give GitLab a bounded beat to re-point head_pipeline before the caller
	// retries the PUT. Best-effort: the MR is already reopened, so a recompute
	// that has not landed yet must not be reported as a recovery failure.
	after = c.awaitRecomputedMergeHead(opCtx, after, auth, green, deadline)
	return fmt.Sprintf("closed+reopened mr %d against green pipeline %d (%s@%s); head_pipeline %d(%s/%s)→%d(%s/%s), detailed_merge_status %q→%q",
		mr.IID, green.ID, auth.sourceBranch, shortSHA(auth.sha),
		beforeHead.ID, beforeHead.Source, beforeHead.Status,
		after.HeadPipeline.ID, after.HeadPipeline.Source, after.HeadPipeline.Status,
		beforeDetailed, after.DetailedMergeStatus), nil
}

// awaitRecomputedMergeHead polls until GitLab re-points head_pipeline at the
// green pipeline or declares the MR mergeable, bounded by the merge deadline.
// It returns the last MR it read; a poll that never converges is not an error.
func (c *GitLabClient) awaitRecomputedMergeHead(ctx context.Context, mr mrResponse, auth mergeAuthorization, green shaPipeline, deadline time.Time) mrResponse {
	latest := mr
	for {
		if latest.HeadPipeline.ID == green.ID ||
			strings.EqualFold(strings.TrimSpace(latest.DetailedMergeStatus), "mergeable") {
			return latest
		}
		if err := c.waitForMergeRetry(ctx, deadline); err != nil {
			return latest
		}
		next, err := c.getMR(ctx, mr.IID)
		if err != nil {
			return latest
		}
		if validateMRMergeIdentity(mr.IID, next, auth) != nil {
			// The head moved under us. Return the last good read and let the
			// caller's reconcile surface the mismatch through the normal fence.
			return latest
		}
		latest = next
	}
}

// closeReopenMR issues the close then the reopen. ctx is expected to already be
// detached from the merge clocks; the reopen is retried, because once the close
// has landed a failed reopen leaves the MR strictly worse off than the 405.
func (c *GitLabClient) closeReopenMR(ctx context.Context, mrIID int64) error {
	if err := c.setMRState(ctx, mrIID, "close"); err != nil {
		return fmt.Errorf("close mr %d for 405 recovery: %w", mrIID, err)
	}
	var lastErr error
	for attempt := 0; attempt < stale405ReopenAttempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(c.cfg.PollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return mrLeftClosedError(mrIID, lastErr)
			case <-timer.C:
			}
		}
		if err := c.setMRState(ctx, mrIID, "reopen"); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return mrLeftClosedError(mrIID, lastErr)
}

// mrLeftClosedError is the loudest failure this recovery can produce: the close
// landed and every reopen failed, so an MR that was merely refusing to merge is
// now closed and needs a human.
func mrLeftClosedError(mrIID int64, lastErr error) error {
	return fmt.Errorf("mr %d LEFT CLOSED after 405 recovery — reopen it manually; every reopen attempt failed (last: %v): %w",
		mrIID, lastErr, pipeline.ErrMergeRecoveryConfig)
}

// setMRState drives GitLab's MR state machine via state_event.
func (c *GitLabClient) setMRState(ctx context.Context, mrIID int64, event string) error {
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", c.projectPath(), mrIID)
	body := struct {
		StateEvent string `json:"state_event"`
	}{StateEvent: event}
	return c.requestJSON(ctx, http.MethodPut, path, body, nil)
}

// newestGreenPipelineForSHA returns the newest pipeline for an exact SHA on an
// exact ref, and reports whether it is successful. Unlike branchPipelineForSHA
// (which pins source=api for the superseder path) it accepts any source,
// because the ordinary push pipeline is what a recomputed head_pipeline lands
// on.
//
// It deliberately reports the NEWEST pipeline rather than the newest SUCCESSFUL
// one. A close+reopen re-points head_pipeline at whatever is newest, so when
// the newest is a skipped or failed merge_request_event placeholder sitting on
// top of an older green push pipeline, the cycle would swap a merely-stale head
// for a definitively blocking one. That wedge needs the placeholder deleted, not
// an MR state transition, so the caller must decline it.
func (c *GitLabClient) newestGreenPipelineForSHA(ctx context.Context, sha, ref string) (shaPipeline, bool, error) {
	if strings.TrimSpace(sha) == "" || strings.TrimSpace(ref) == "" {
		return shaPipeline{}, false, fmt.Errorf("gitlab: sha and ref required to resolve a green pipeline: %w", pipeline.ErrMergeAuthorizationStale)
	}
	path := fmt.Sprintf("/projects/%s/pipelines?sha=%s&ref=%s&order_by=id&sort=desc&per_page=20",
		c.projectPath(), url.QueryEscape(sha), url.QueryEscape(ref))
	var pipes []shaPipeline
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipes); err != nil {
		return shaPipeline{}, false, err
	}
	newest := shaPipeline{}
	for _, p := range pipes {
		// Re-check identity locally: a filter GitLab silently ignored must not
		// promote an unrelated pipeline into the merge authorization.
		if p.SHA != sha || p.Ref != ref {
			continue
		}
		if p.ID > newest.ID {
			newest = p
		}
	}
	if newest.ID == 0 {
		return shaPipeline{}, false, nil
	}
	return newest, newest.Status == "success", nil
}

func pipelineCanSupersede(mr mrResponse, pipe shaPipeline) bool {
	if pipe.Source != "api" {
		return false
	}
	if mr.HeadPipeline.Source == "merge_request_event" {
		return pipe.ID > mr.HeadPipeline.ID
	}
	return pipe.ID >= mr.HeadPipeline.ID
}

func (c *GitLabClient) waitForAmbiguousPipelineCreate(ctx context.Context, mr mrResponse, auth mergeAuthorization, deadline time.Time) (shaPipeline, error) {
	for {
		if !time.Now().Before(deadline) {
			return shaPipeline{}, errMergeRetryDeadline
		}
		pipe, ok, err := c.branchPipelineForSHA(ctx, auth.sha, auth.sourceBranch, "api")
		if err == nil && ok && pipelineCanSupersede(mr, pipe) {
			return pipe, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return shaPipeline{}, ctx.Err()
			}
			if errors.Is(err, pipeline.ErrMergeAuthorizationStale) {
				return shaPipeline{}, err
			}
			if status, ok := GitLabHTTPStatus(err); ok && status < http.StatusInternalServerError {
				return shaPipeline{}, err
			}
		}
		if err := c.waitForMergeRetry(ctx, deadline); err != nil {
			return shaPipeline{}, err
		}
	}
}

func mergeOperationError(parent context.Context, deadline time.Time, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if !time.Now().Before(deadline) {
		return errMergeRetryDeadline
	}
	return err
}

func (c *GitLabClient) awaitSupersedingPipeline(ctx context.Context, mrIID int64, pipe shaPipeline, expectedSHA, expectedRef string, deadline time.Time) error {
	if !time.Now().Before(deadline) {
		return errMergeRetryDeadline
	}
	if pipe.ID == 0 {
		return fmt.Errorf("superseding pipeline for mr %d has no id: %w", mrIID, pipeline.ErrMergeAuthorizationStale)
	}
	// GitLab's create-pipeline response omits source; list/get responses include
	// it. Reject a contradictory source here and require "api" on every poll.
	if pipe.Source != "" && pipe.Source != "api" {
		return fmt.Errorf("pipeline %d for mr %d has source %q, want api: %w", pipe.ID, mrIID, pipe.Source, pipeline.ErrMergeAuthorizationStale)
	}
	if pipe.SHA != expectedSHA {
		return fmt.Errorf("superseding pipeline %d sha %q does not match CI-tested sha %q: %w", pipe.ID, pipe.SHA, expectedSHA, pipeline.ErrMergeAuthorizationStale)
	}
	if pipe.Ref != expectedRef {
		return fmt.Errorf("superseding pipeline %d ref %q does not match MR source branch %q: %w", pipe.ID, pipe.Ref, expectedRef, pipeline.ErrMergeAuthorizationStale)
	}
	status, err := c.pollPipelineToSuccess(ctx, pipe.ID, expectedSHA, expectedRef, deadline)
	if err != nil {
		return fmt.Errorf("poll superseding pipeline %d: %w", pipe.ID, err)
	}
	if status != "success" {
		return fmt.Errorf("superseding pipeline %d for mr %d ended %s, not success", pipe.ID, mrIID, status)
	}
	return nil
}

// createBranchPipeline POSTs a fresh pipeline on ref (the MR source branch).
// The new pipeline supersedes the failed merge_request_event placeholder as the
// MR head_pipeline without mutating MR state. Returns the created pipeline so
// the caller can verify its SHA and poll it to success.
func (c *GitLabClient) createBranchPipeline(ctx context.Context, ref string) (shaPipeline, error) {
	path := fmt.Sprintf("/projects/%s/pipeline", c.projectPath())
	body := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	var got shaPipeline
	if err := c.requestJSON(ctx, http.MethodPost, path, body, &got); err != nil {
		return shaPipeline{}, err
	}
	return got, nil
}

// pollPipelineToSuccess polls one pipeline by id until it reaches a terminal
// state, bounded by both PollDeadline and the enclosing merge deadline. It
// preserves parent cancellation and wraps ErrPipelinePollTimeout only when the
// pipeline-specific deadline is the one that expires.
func (c *GitLabClient) pollPipelineToSuccess(ctx context.Context, id int64, expectedSHA, expectedRef string, mergeDeadline time.Time) (string, error) {
	pollDeadline := time.Now().Add(c.cfg.PollDeadline)
	if mergeDeadline.Before(pollDeadline) {
		pollDeadline = mergeDeadline
	}
	pollCtx, cancel := context.WithDeadline(ctx, pollDeadline)
	defer cancel()
	terminal := map[string]bool{"success": true, "failed": true, "canceled": true, "skipped": true}
	path := fmt.Sprintf("/projects/%s/pipelines/%d", c.projectPath(), id)
	for {
		var p shaPipeline
		if err := c.requestJSON(pollCtx, http.MethodGet, path, nil, &p); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if pollCtx.Err() != nil {
				if !time.Now().Before(mergeDeadline) {
					return "", errMergeRetryDeadline
				}
				return "", fmt.Errorf("pipeline %d poll timed out after %s: %w", id, c.cfg.PollDeadline, pipeline.ErrPipelinePollTimeout)
			}
			return "", err
		}
		if p.SHA != expectedSHA {
			return "", fmt.Errorf("pipeline %d sha %q does not match CI-tested sha %q: %w", id, p.SHA, expectedSHA, pipeline.ErrMergeAuthorizationStale)
		}
		if p.Ref != expectedRef {
			return "", fmt.Errorf("pipeline %d ref %q does not match MR source branch %q: %w", id, p.Ref, expectedRef, pipeline.ErrMergeAuthorizationStale)
		}
		if p.Source != "api" {
			return "", fmt.Errorf("pipeline %d source %q, want api: %w", id, p.Source, pipeline.ErrMergeAuthorizationStale)
		}
		if terminal[p.Status] {
			return p.Status, nil
		}
		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if !time.Now().Before(mergeDeadline) {
				return "", errMergeRetryDeadline
			}
			return "", fmt.Errorf("pipeline %d poll timed out after %s: %w", id, c.cfg.PollDeadline, pipeline.ErrPipelinePollTimeout)
		case <-time.After(c.cfg.PollInterval):
		}
	}
}

// isMergeNotReady reports whether a merge error is GitLab's transient "not
// mergeable yet" response. Depending on which internal mergeability check is
// still settling, GitLab returns either 405 Method Not Allowed or 422 Branch
// cannot be merged immediately after the branch pipeline turns green. Match
// the 422 narrowly so real validation errors remain terminal.
func isMergeNotReady(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "status 405") ||
		strings.Contains(s, "method not allowed") ||
		(strings.Contains(s, "status 422") && strings.Contains(s, "branch cannot be merged"))
}

// mergeRecoveryPipelineRequired distinguishes an ordinary mergeability settle
// from the detached-head 405 that requires polling or creating an exact API
// pipeline. The caller has already validated the live MR identity. A 422 never
// expands to the pipeline budget; it remains attempt-bounded and requires a
// fresh external rebase plus CI cycle when persistent.
func mergeRecoveryPipelineRequired(err error, mr mrResponse) bool {
	if !isMergeNotReady(err) || isBranchCannotBeMerged(err) {
		return false
	}
	return mr.HeadPipeline.Source == "merge_request_event" || mr.HeadPipeline.Source == "api"
}

// isBranchCannotBeMerged reports GitLab's 422 "Branch cannot be merged"
// specifically. Narrow matching keeps unrelated 422s out of the bounded
// mergeability-settle retries. Persistent 422s return without rebasing.
func isBranchCannotBeMerged(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "status 422") && strings.Contains(s, "branch cannot be merged")
}

// ----- Cleanup -----

// Cleanup removes a stranded source branch only when its live MR is verified
// closed without merge. Normal merged-MR source removal remains GitLab's
// RemoveSourceBranch=true responsibility; this sweep is deliberately narrow
// so it cannot delete an opened, merged, or mismatched ref.
//
// Cleanup is best-effort. A 404 (branch already gone) and a 400 ("reference
// update failed": protected branch, concurrent delete, or an already-updated
// ref) are logged and swallowed. Other statuses (auth, 5xx) still surface so
// a genuinely misconfigured cleanup is visible.
func (c *GitLabClient) Cleanup(ctx context.Context, req pipeline.CleanupRequest) (pipeline.CleanupResponse, error) {
	logTail := strings.Builder{}
	if req.BranchName != "" {
		project := strings.TrimSpace(req.Project)
		if project == "" {
			return pipeline.CleanupResponse{}, fmt.Errorf("gitlab: expected project required for cleanup authorization: %w", pipeline.ErrMergeAuthorizationStale)
		}
		if project != c.cfg.Project {
			return pipeline.CleanupResponse{}, fmt.Errorf("gitlab: cleanup-authorized project %q does not match client project %q: %w", project, c.cfg.Project, pipeline.ErrMergeAuthorizationStale)
		}
		if req.MRIID != 0 {
			var mr mrResponse
			if err := c.requestJSON(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/merge_requests/%d", c.projectPath(), req.MRIID), nil, &mr); err != nil {
				return pipeline.CleanupResponse{}, fmt.Errorf("gitlab: inspect MR %d before husk cleanup: %w", req.MRIID, err)
			}
			if mr.IID != req.MRIID || mr.SourceBranch != req.BranchName || mr.TargetBranch != req.TargetBranch {
				fmt.Fprintf(&logTail, "cleanup: skipped unverified MR %d branch %s\n", req.MRIID, req.BranchName)
				return pipeline.CleanupResponse{LogTail: logTail.String()}, nil
			}
			if mr.State != "closed" {
				fmt.Fprintf(&logTail, "cleanup: skipped MR %d in state %s\n", req.MRIID, mr.State)
				return pipeline.CleanupResponse{LogTail: logTail.String()}, nil
			}
		}
		path := fmt.Sprintf("/projects/%s/repository/branches/%s", c.projectPath(), url.PathEscape(req.BranchName))
		if err := c.requestJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
			switch {
			case strings.Contains(err.Error(), "status 404"):
				// Branch already gone (the RemoveSourceBranch=true path).
				fmt.Fprintf(&logTail, "branch %s already removed\n", req.BranchName)
			case strings.Contains(err.Error(), "status 400"):
				slog.Default().Warn("cleanup: branch delete failed (best-effort, ignoring)",
					"branch", req.BranchName, "error", err)
				fmt.Fprintf(&logTail, "branch %s delete skipped (best-effort): %v\n", req.BranchName, err)
			default:
				return pipeline.CleanupResponse{LogTail: logTail.String()}, err
			}
		} else {
			fmt.Fprintf(&logTail, "deleted branch %s\n", req.BranchName)
		}
	}
	return pipeline.CleanupResponse{LogTail: logTail.String()}, nil
}

// ----- Issue (escalation path) -----

type createIssueBody struct {
	Title        string `json:"title"`
	Description  string `json:"description,omitempty"`
	Labels       string `json:"labels,omitempty"` // GitLab takes a CSV
	Confidential bool   `json:"confidential,omitempty"`
}

type issueResponse struct {
	IID    int64  `json:"iid"`
	WebURL string `json:"web_url"`
}

// CreateIssue implements pipeline.IssueClient.
func (c *GitLabClient) CreateIssue(ctx context.Context, req pipeline.IssueRequest) (pipeline.IssueResponse, error) {
	body := createIssueBody{
		Title:       req.Title,
		Description: req.Description,
		Labels:      strings.Join(req.Labels, ","),
	}
	var got issueResponse
	path := fmt.Sprintf("/projects/%s/issues", c.projectPath())
	if err := c.requestJSON(ctx, http.MethodPost, path, body, &got); err != nil {
		return pipeline.IssueResponse{}, err
	}
	return pipeline.IssueResponse{IID: got.IID, URL: got.WebURL}, nil
}

// ListIssuesOpts filters a ListIssues call. Empty fields are omitted; an
// empty Labels slice does NOT filter (matches all).
type ListIssuesOpts struct {
	Labels  []string // ANDed (GitLab requires comma-separated)
	State   string   // "opened" | "closed" | "" (all)
	PerPage int      // 1..100; defaults to 20 at the GitLab side
}

// IssueListItem is the subset of GitLab's issue response the importer needs.
// Fields not consumed downstream are omitted from the struct (JSON decoder
// ignores unknown keys); add fields explicitly as future intake needs grow.
type IssueListItem struct {
	IID         int64    `json:"iid"`
	ProjectID   int64    `json:"project_id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Labels      []string `json:"labels"`
	State       string   `json:"state"`
	WebURL      string   `json:"web_url"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// ListIssues returns issues for the configured project, filtered by opts.
// Scope is intentionally single-project (matches GitLabConfig.Project);
// multi-project intake is a future-Slice concern.
func (c *GitLabClient) ListIssues(ctx context.Context, opts ListIssuesOpts) ([]IssueListItem, error) {
	q := url.Values{}
	if len(opts.Labels) > 0 {
		q.Set("labels", strings.Join(opts.Labels, ","))
	}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(opts.PerPage))
	}
	path := fmt.Sprintf("/projects/%s/issues", c.projectPath())
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var got []IssueListItem
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &got); err != nil {
		return nil, err
	}
	return got, nil
}

// FindOpenEscalation implements the legacy pipeline.DedupIssueClient contract.
// It returns the newest open escalation for a backlog item, across both legacy
// and class-specific markers. New publication uses FindOpenEscalationByClass.
func (c *GitLabClient) FindOpenEscalation(ctx context.Context, backlogID string) (pipeline.IssueRef, bool, error) {
	refs, err := c.ListOpenEscalations(ctx, backlogID)
	if err != nil {
		return pipeline.IssueRef{}, false, err
	}
	if len(refs) == 0 {
		return pipeline.IssueRef{}, false, nil
	}
	return refs[0], true, nil
}

// FindOpenEscalationByClass implements pipeline.ClassAwareDedupIssueClient. It
// returns the existing OPEN issue that carries the exact escalation dedup
// marker for backlogID and failureClass, so distinct failure classes do not
// share an incident thread. An empty failureClass matches only the legacy
// backlog-only marker. found=false with a nil error means none exists.
//
// Scope is the newest ≤100 open `mills-escalation` issues (GitLab returns them
// created_at-desc), which is sufficient because the marker-bearing issues are
// created by this same path and the most recent one for an item sorts first;
// the older markerless backlog of escalations is handled by the one-time bulk
// triage (the other half of #167), not by this lookup.
func (c *GitLabClient) FindOpenEscalationByClass(ctx context.Context, backlogID, failureClass string) (pipeline.IssueRef, bool, error) {
	backlogID = strings.TrimSpace(backlogID)
	if backlogID == "" {
		return pipeline.IssueRef{}, false, nil
	}
	items, err := c.listOpenEscalationIssues(ctx)
	if err != nil {
		return pipeline.IssueRef{}, false, err
	}
	marker := pipeline.EscalationClassDedupMarker(backlogID, failureClass)
	for _, it := range items {
		if strings.Contains(it.Description, marker) {
			return pipeline.IssueRef{IID: it.IID, URL: it.WebURL}, true, nil
		}
	}
	return pipeline.IssueRef{}, false, nil
}

// ListOpenEscalations implements pipeline.ClosableIssueClient. It returns all
// open escalation issues for a backlog item, including both class-aware and
// legacy backlog-only markers, so a later successful run can resolve every
// incident thread for the item.
func (c *GitLabClient) ListOpenEscalations(ctx context.Context, backlogID string) ([]pipeline.IssueRef, error) {
	backlogID = strings.TrimSpace(backlogID)
	if backlogID == "" {
		return nil, nil
	}
	items, err := c.listOpenEscalationIssues(ctx)
	if err != nil {
		return nil, err
	}
	legacyMarker := pipeline.EscalationDedupMarker(backlogID)
	classPrefix := pipeline.EscalationClassDedupMarkerPrefix(backlogID)
	refs := make([]pipeline.IssueRef, 0, len(items))
	for _, it := range items {
		if strings.Contains(it.Description, legacyMarker) || strings.Contains(it.Description, classPrefix) {
			refs = append(refs, pipeline.IssueRef{IID: it.IID, URL: it.WebURL})
		}
	}
	return refs, nil
}

func (c *GitLabClient) listOpenEscalationIssues(ctx context.Context) ([]IssueListItem, error) {
	return c.ListIssues(ctx, ListIssuesOpts{
		Labels:  []string{"mills-escalation"},
		State:   "opened",
		PerPage: 100,
	})
}

// FindOpenAuditDigest implements audit.DigestIssuer. It returns the open audit
// advisory digest issue for the given UTC day (period "YYYY-MM-DD"), matched by
// the pipeline.AuditDigestMarker embedded in the issue body. found=false with a
// nil error means no digest exists for that day yet.
//
// Scope is the newest ≤100 open `audit-digest` issues (GitLab returns them
// created_at-desc). At one digest per day this window spans months, so the
// marker-bearing issue for any recent period is always present; the writer
// only ever looks up the current day.
func (c *GitLabClient) FindOpenAuditDigest(ctx context.Context, period string) (pipeline.IssueRef, bool, error) {
	period = strings.TrimSpace(period)
	if period == "" {
		return pipeline.IssueRef{}, false, nil
	}
	items, err := c.ListIssues(ctx, ListIssuesOpts{
		Labels:  []string{pipeline.AuditDigestLabel},
		State:   "opened",
		PerPage: 100,
	})
	if err != nil {
		return pipeline.IssueRef{}, false, err
	}
	marker := pipeline.AuditDigestMarker(period)
	for _, it := range items {
		if strings.Contains(it.Description, marker) {
			return pipeline.IssueRef{IID: it.IID, URL: it.WebURL}, true, nil
		}
	}
	return pipeline.IssueRef{}, false, nil
}

// CommentIssue implements pipeline.DedupIssueClient. It appends a note to an
// existing issue via POST /projects/:id/issues/:iid/notes.
func (c *GitLabClient) CommentIssue(ctx context.Context, iid int64, body string) error {
	if iid == 0 {
		return errors.New("gitlab: CommentIssue requires a non-zero iid")
	}
	path := fmt.Sprintf("/projects/%s/issues/%d/notes", c.projectPath(), iid)
	reqBody := struct {
		Body string `json:"body"`
	}{Body: body}
	return c.requestJSON(ctx, http.MethodPost, path, reqBody, nil)
}

// CloseIssue implements pipeline.ClosableIssueClient. It transitions an issue to
// the closed state via PUT /projects/:id/issues/:iid with state_event=close.
func (c *GitLabClient) CloseIssue(ctx context.Context, iid int64) error {
	if iid == 0 {
		return errors.New("gitlab: CloseIssue requires a non-zero iid")
	}
	path := fmt.Sprintf("/projects/%s/issues/%d", c.projectPath(), iid)
	reqBody := struct {
		StateEvent string `json:"state_event"`
	}{StateEvent: "close"}
	return c.requestJSON(ctx, http.MethodPut, path, reqBody, nil)
}

// ----- Repository file read + commit (GitOps auto-PR for the kill-switch) -----
//
// These two methods back the operator's POST /api/mills/policy/kill-switch
// endpoint, which flips `enabled:` in platform/gitops' mills policy
// ConfigMap via a branch+commit+MR rather than fighting Flux with a live
// write-through. They are general-purpose (any caller can read a file or
// stage a commit) but exist for the gitops-scoped client instance.

// GetRawFile fetches the raw contents of a repository file at ref using
// GET /projects/:id/repository/files/:path/raw. Returns the body verbatim
// (the file is not JSON-decoded). A 404 surfaces as an error containing
// the GitLab message so callers can distinguish "no such file/ref".
func (c *GitLabClient) GetRawFile(ctx context.Context, filePath, ref string) (string, error) {
	if strings.TrimSpace(filePath) == "" {
		return "", errors.New("gitlab: GetRawFile: filePath required")
	}
	if ref == "" {
		ref = "main"
	}
	path := fmt.Sprintf("/projects/%s/repository/files/%s/raw?ref=%s",
		c.projectPath(), url.PathEscape(filePath), url.QueryEscape(ref))
	full := strings.TrimRight(c.cfg.APIURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", fmt.Errorf("gitlab: new request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gitlab: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB ceiling
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gitlab: GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	return string(buf), nil
}

// ----- MR diffs (audit merged-diff loader) -----

// mrDiffEntry is one file's diff from GET /merge_requests/:iid/diffs.
type mrDiffEntry struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

// MRDiff assembles a unified diff for a merge request from
// GET /projects/:id/merge_requests/:iid/diffs. GitLab returns per-file
// entries whose `diff` field holds the @@ hunks; this re-adds the
// per-file `diff --git` / `---` / `+++` headers so the result reads as
// one standard unified diff. Pagination is capped at a few pages and the
// assembled text is truncated at maxBytes (with a marker) — the caller
// feeds this verbatim into an LLM rubric prompt, so unbounded output is
// a context blowout, not extra fidelity.
func (c *GitLabClient) MRDiff(ctx context.Context, mrIID int64, maxBytes int) (string, error) {
	if mrIID <= 0 {
		return "", errors.New("gitlab: MRDiff: mrIID required")
	}
	if maxBytes <= 0 {
		maxBytes = 64 * 1024
	}
	const perPage = 100
	const maxPages = 5
	var b strings.Builder
	for page := 1; page <= maxPages; page++ {
		var entries []mrDiffEntry
		path := fmt.Sprintf("/projects/%s/merge_requests/%d/diffs?per_page=%d&page=%d",
			c.projectPath(), mrIID, perPage, page)
		if err := c.requestJSON(ctx, http.MethodGet, path, nil, &entries); err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.Diff == "" {
				continue
			}
			fmt.Fprintf(&b, "diff --git a/%s b/%s\n", e.OldPath, e.NewPath)
			fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", e.OldPath, e.NewPath)
			b.WriteString(e.Diff)
			if !strings.HasSuffix(e.Diff, "\n") {
				b.WriteByte('\n')
			}
			if b.Len() >= maxBytes {
				return b.String()[:maxBytes] + "\n… [diff truncated]\n", nil
			}
		}
		if len(entries) < perPage {
			break
		}
	}
	return b.String(), nil
}

// CommitAction is one file action in a commits-API request. Action is
// "create" | "update" | "delete" | "move"; Content is required for
// create/update.
type CommitAction struct {
	Action   string `json:"action"`
	FilePath string `json:"file_path"`
	Content  string `json:"content,omitempty"`
}

// CreateCommitRequest creates a commit on Branch, optionally branching it
// off StartBranch when Branch does not yet exist (GitLab creates the
// branch as part of the commit when start_branch is set).
type CreateCommitRequest struct {
	Branch        string
	StartBranch   string
	CommitMessage string
	Actions       []CommitAction
}

// CreateCommitResponse is the subset of the commit object the kill-switch
// flow needs.
type CreateCommitResponse struct {
	ID     string
	WebURL string
}

type createCommitBody struct {
	Branch        string         `json:"branch"`
	StartBranch   string         `json:"start_branch,omitempty"`
	CommitMessage string         `json:"commit_message"`
	Actions       []CommitAction `json:"actions"`
}

type commitResponse struct {
	ID     string `json:"id"`
	WebURL string `json:"web_url"`
}

// CreateCommit posts to POST /projects/:id/repository/commits, creating
// Branch (off StartBranch) and applying Actions atomically.
func (c *GitLabClient) CreateCommit(ctx context.Context, req CreateCommitRequest) (CreateCommitResponse, error) {
	if req.Branch == "" {
		return CreateCommitResponse{}, errors.New("gitlab: CreateCommit: Branch required")
	}
	if len(req.Actions) == 0 {
		return CreateCommitResponse{}, errors.New("gitlab: CreateCommit: at least one action required")
	}
	// createCommitBody is field-identical to CreateCommitRequest (it only
	// adds JSON tags), so a direct conversion is both correct and what
	// staticcheck prefers over a field-by-field literal.
	body := createCommitBody(req)
	var got commitResponse
	path := fmt.Sprintf("/projects/%s/repository/commits", c.projectPath())
	if err := c.requestJSON(ctx, http.MethodPost, path, body, &got); err != nil {
		return CreateCommitResponse{}, err
	}
	return CreateCommitResponse(got), nil
}

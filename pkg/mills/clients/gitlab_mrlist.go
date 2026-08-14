package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MergeRequestPipeline is a bounded view of an MR's head pipeline as returned
// inline by the merge-requests list endpoint. Nil on a list item means GitLab
// returned no head pipeline for that MR.
type MergeRequestPipeline struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Source string `json:"source"`
	WebURL string `json:"web_url"`
}

// MergeRequestListItem is a bounded, read-only view of one open merge request
// used by the HUD mrwatch registry. It intentionally omits the write-path
// fields on the internal mrResponse so this can evolve independently of the
// merge/pipeline flow. HeadPipeline is populated from whichever of
// `head_pipeline` / `pipeline` the list endpoint returns.
type MergeRequestListItem struct {
	IID          int64  `json:"iid"`
	Title        string `json:"title"`
	State        string `json:"state"`
	WebURL       string `json:"web_url"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	// SHA is the head commit of the source branch at the time GitLab answered.
	// Present on both the list items and the single-MR GET. Callers that write
	// against the MR (e.g. the mrwatch shepherd arming auto-merge) must carry it
	// back as the merge `sha` precondition so the write cannot land on a head
	// they never observed.
	SHA string `json:"sha"`
	// MergedCommitSHA / MergeCommitSHA / SquashCommitSHA identify the commit a
	// merged MR's work became on the target branch. GitLab populates whichever
	// one matches the project's merge method, so all three are carried and
	// LandedSHA resolves them in the same preference order the merge path
	// records — a landed identity read from a list item must be the identity
	// mergedResponse would have written.
	MergedCommitSHA           string                `json:"merged_commit_sha"`
	MergeCommitSHA            string                `json:"merge_commit_sha"`
	SquashCommitSHA           string                `json:"squash_commit_sha"`
	Draft                     bool                  `json:"draft"`
	WorkInProgress            bool                  `json:"work_in_progress"`
	HasConflicts              bool                  `json:"has_conflicts"`
	DetailedMergeStatus       string                `json:"detailed_merge_status"`
	MergeStatus               string                `json:"merge_status"`
	MergeWhenPipelineSucceeds bool                  `json:"merge_when_pipeline_succeeds"`
	CreatedAt                 time.Time             `json:"created_at"`
	UpdatedAt                 time.Time             `json:"updated_at"`
	MergedAt                  time.Time             `json:"merged_at"`
	HeadPipeline              *MergeRequestPipeline `json:"head_pipeline"`
	Pipeline                  *MergeRequestPipeline `json:"pipeline"`
}

// IsDraft reports whether the MR is a draft, tolerating either the modern
// `draft` field or the legacy `work_in_progress` alias.
func (m MergeRequestListItem) IsDraft() bool { return m.Draft || m.WorkInProgress }

// LandedSHA returns the commit a merged MR's work landed as, preferring
// merged_commit_sha → merge_commit_sha → squash_commit_sha → sha (the order
// mergedResponse uses at merge time). Empty when GitLab reported none; on an
// OPEN MR only `sha` is set, which is the source-branch head and not a commit
// on the target branch — callers must only read this for merged MRs.
func (m MergeRequestListItem) LandedSHA() string {
	for _, sha := range []string{m.MergedCommitSHA, m.MergeCommitSHA, m.SquashCommitSHA, m.SHA} {
		if sha = strings.TrimSpace(sha); sha != "" {
			return sha
		}
	}
	return ""
}

// EffectiveHeadPipeline prefers head_pipeline and falls back to pipeline so a
// GitLab instance that only populates one of them still yields a status.
func (m MergeRequestListItem) EffectiveHeadPipeline() *MergeRequestPipeline {
	if m.HeadPipeline != nil {
		return m.HeadPipeline
	}
	return m.Pipeline
}

// ListOpenMergeRequests returns the open merge requests for the client's
// project (read-only). It scopes to state=opened, newest-updated first.
// perPage bounds the page size (default 50, capped at 100); only the first
// page is fetched — the registry watches active repos, not deep MR history.
//
// NOTE (verified against the live instance 2026-07-18): GitLab's *list*
// endpoint returns NO pipeline info on list items — neither `head_pipeline`
// nor `pipeline` is present. Only the single-MR GET includes `head_pipeline`.
// Callers that need CI state must follow up with GetMergeRequest per MR
// (bounded); HeadPipeline/Pipeline on a list item will be nil.
//
// This reuses the client's token and requestJSON helper, so it inherits the
// same auth wiring as the mills merge/pipeline flow (do not invent new auth).
func (c *GitLabClient) ListOpenMergeRequests(ctx context.Context, perPage int) ([]MergeRequestListItem, error) {
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 100 {
		perPage = 100
	}
	path := fmt.Sprintf(
		"/projects/%s/merge_requests?state=opened&scope=all&order_by=updated_at&sort=desc&per_page=%d",
		c.projectPath(), perPage)
	var items []MergeRequestListItem
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// ListMergedMergeRequests returns the merge requests of the client's project
// that GitLab has merged, newest-updated first, bounded to those updated at or
// after updatedAfter (zero = unbounded). Read-only, single page, same auth and
// requestJSON wiring as ListOpenMergeRequests.
//
// The HUD mrwatch registry uses this to publish an explicit `merged` marker for
// a bounded window. Asking GitLab server-side — rather than inferring a merge
// from an MR disappearing off the open list — is what keeps the marker correct
// across a daemon restart and distinguishable from a closed-unmerged MR.
// As with the open list, list items carry NO pipeline info; merged MRs need
// none, so this path performs no enrichment.
func (c *GitLabClient) ListMergedMergeRequests(ctx context.Context, perPage int, updatedAfter time.Time) ([]MergeRequestListItem, error) {
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 100 {
		perPage = 100
	}
	path := fmt.Sprintf(
		"/projects/%s/merge_requests?state=merged&scope=all&order_by=updated_at&sort=desc&per_page=%d",
		c.projectPath(), perPage)
	if !updatedAfter.IsZero() {
		path += "&updated_after=" + url.QueryEscape(updatedAfter.UTC().Format(time.RFC3339))
	}
	var items []MergeRequestListItem
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// GetMergeRequest fetches one merge request by IID (read-only). Unlike the
// list endpoint, the single-MR GET includes `head_pipeline` plus fresher
// `detailed_merge_status` / `merge_when_pipeline_succeeds`, so it is the
// enrichment call for classifying CI state. Same auth/requestJSON wiring.
func (c *GitLabClient) GetMergeRequest(ctx context.Context, iid int64) (MergeRequestListItem, error) {
	var item MergeRequestListItem
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", c.projectPath(), iid)
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &item); err != nil {
		return MergeRequestListItem{}, err
	}
	return item, nil
}

// ----- mrwatch shepherd write actions (slice M4) -----
//
// These three bounded write actions back the HUD mrwatch shepherd. They reuse
// the client's token + requestJSON wiring (do not invent new auth) and are the
// ONLY writes the shepherd performs — it never rebases, force-pushes, or merges
// directly. Each is scoped to a single project via ForProject.

// RetryPipeline retries a pipeline by id (POST /projects/:id/pipelines/:id/retry).
// The shepherd calls this once per flaky-classified failed head pipeline: GitLab
// re-runs the failed/canceled jobs of that pipeline in place. Read of the result
// is intentionally skipped — the next mrwatch poll observes the fresh status.
func (c *GitLabClient) RetryPipeline(ctx context.Context, pipelineID int64) error {
	if pipelineID <= 0 {
		return errors.New("gitlab: RetryPipeline requires a non-zero pipeline id")
	}
	path := fmt.Sprintf("/projects/%s/pipelines/%d/retry", c.projectPath(), pipelineID)
	return c.requestJSON(ctx, http.MethodPost, path, nil, nil)
}

// CreatePipelineForRef creates a fresh branch pipeline on ref
// (POST /projects/:id/pipeline). The shepherd uses this to re-point a merge
// request whose head pipeline was skipped (or never attached): loom-core's
// workflow.rules run any pipeline with CI_COMMIT_BRANCH set, so the new pipeline
// runs the real jobs and — being newest for the head SHA — GitLab makes it the
// MR head_pipeline. It deliberately does NOT delete the stale pipeline (Owner-only
// and out of scope for v1). Reuses the same POST as the merge-recovery path.
func (c *GitLabClient) CreatePipelineForRef(ctx context.Context, ref string) (int64, error) {
	if ref == "" {
		return 0, errors.New("gitlab: CreatePipelineForRef requires a ref")
	}
	p, err := c.createBranchPipeline(ctx, ref)
	if err != nil {
		return 0, err
	}
	return p.ID, nil
}

// ArmAutoMerge arms auto-merge (merge-when-pipeline-succeeds) on an open MR via
// PUT /projects/:id/merge_requests/:iid/merge?auto_merge=true&sha=<headSHA>.
//
// It uses the `auto_merge=true` query param rather than the legacy
// `merge_when_pipeline_succeeds=true` body field: on this GitLab instance the
// legacy param 409s, while auto_merge=true arms cleanly (institutional note
// reference_glab_automerge_405). GitLab returns 405 "Method Not Allowed" while
// it is still (re)computing mergeability right after a push — the caller
// (shepherd) treats that as retry-next-poll, not a failure, so callers can
// distinguish it via GitLabHTTPStatus.
//
// headSHA is REQUIRED and is sent as GitLab's `sha` merge precondition: GitLab
// rejects the request with 409 Conflict when it does not match the current head
// of the source branch. That is what keeps an arm bound to the head the caller
// actually observed — without it, a push landing between observation and arm
// would silently auto-merge an unreviewed head. An empty headSHA is a
// programming error and is refused locally (fail closed, no request issued).
func (c *GitLabClient) ArmAutoMerge(ctx context.Context, mrIID int64, headSHA string) error {
	if mrIID <= 0 {
		return errors.New("gitlab: ArmAutoMerge requires a non-zero MR iid")
	}
	sha := strings.TrimSpace(headSHA)
	if sha == "" {
		return errors.New("gitlab: ArmAutoMerge requires the observed head sha (refusing to arm an unpinned head)")
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/merge?auto_merge=true&sha=%s",
		c.projectPath(), mrIID, url.QueryEscape(sha))
	return c.requestJSON(ctx, http.MethodPut, path, nil, nil)
}

// Merge request operation handlers for mcp-gitlab
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
	"github.com/crb2nu/loom/pkg/poll"
	"github.com/crb2nu/loom/pkg/validate"
)

// GitLab rejects merge/auto-merge requests with 405 (not ready: no head
// pipeline yet, draft, blocked) or 406 (mergeability check still running, or a
// real conflict) when they arrive too early after a push. For auto_merge we
// wait briefly for the head pipeline and retry a bounded number of times
// instead of surfacing the raw status, which sends agents into blind
// merge-retry loops. Vars (not consts) so tests can shrink the delays.
var (
	autoMergeRetryAttempts    = 2
	autoMergeHeadPipelineWait = 8 * time.Second
	autoMergePollInterval     = 2 * time.Second
	autoMergeRetryBackoffMax  = 4 * time.Second
)

func registerMergeRequestTools(srv *mcpscaffold.Server, gl *gitlabServer) {
	// create_merge_request
	srv.AddTracedTool(mcp.Tool{
		Name:        "create_merge_request",
		Description: "Create a new merge request",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project": map[string]any{
					"type":        "string",
					"description": "Project ID or URL-encoded path",
				},
				"source_branch": map[string]any{
					"type":        "string",
					"description": "Source branch",
				},
				"target_branch": map[string]any{
					"type":        "string",
					"description": "Target branch",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "MR title",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "MR description",
				},
				"remove_source_branch": map[string]any{
					"type":        "boolean",
					"description": "Remove source branch after merge",
				},
			},
			Required: []string{"project", "source_branch", "target_branch", "title"},
		},
	}, gl.handleCreateMergeRequest)
	// get_merge_request
	srv.AddTracedTool(mcp.Tool{
		Name:        "get_merge_request",
		Description: "Get a merge request by IID",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project": map[string]any{
					"type":        "string",
					"description": "Project ID or URL-encoded path",
				},
				"merge_request_iid": map[string]any{
					"type":        "integer",
					"description": "Merge request IID",
				},
			},
			Required: []string{"project", "merge_request_iid"},
		},
	}, gl.handleGetMergeRequest)
	// list_merge_requests
	srv.AddTracedTool(mcp.Tool{
		Name:        "list_merge_requests",
		Description: "List merge requests for a project",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project": map[string]any{
					"type":        "string",
					"description": "Project ID or URL-encoded path",
				},
				"state": map[string]any{
					"type":        "string",
					"description": "State: opened, closed, merged, all. Defaults to 'opened'.",
				},
				"source_branch": map[string]any{
					"type":        "string",
					"description": "Optional source branch filter",
				},
				"target_branch": map[string]any{
					"type":        "string",
					"description": "Optional target branch filter",
				},
				"per_page": map[string]any{
					"type":        "integer",
					"description": "Results per page (max 100)",
				},
				"page": map[string]any{
					"type":        "integer",
					"description": "Page number (default 1).",
				},
			},
			Required: []string{"project"},
		},
	}, gl.handleListMergeRequests)
	// merge_merge_request
	srv.AddTracedTool(mcp.Tool{
		Name:        "merge_merge_request",
		Description: "Merge a merge request immediately or request GitLab auto-merge",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"project": map[string]any{
					"type":        "string",
					"description": "Project ID or URL-encoded path",
				},
				"merge_request_iid": map[string]any{
					"type":        "integer",
					"description": "Merge request IID",
				},
				"auto_merge": map[string]any{
					"type":        "boolean",
					"description": "Request GitLab auto-merge instead of an immediate merge",
				},
				"squash": map[string]any{
					"type":        "boolean",
					"description": "Squash commits when merging",
				},
				"should_remove_source_branch": map[string]any{
					"type":        "boolean",
					"description": "Remove the source branch after merge",
				},
				"sha": map[string]any{
					"type":        "string",
					"description": "Optional expected HEAD SHA to avoid merging unexpected commits",
				},
				"merge_commit_message": map[string]any{
					"type":        "string",
					"description": "Optional custom merge commit message",
				},
				"squash_commit_message": map[string]any{
					"type":        "string",
					"description": "Optional custom squash commit message",
				},
			},
			Required: []string{"project", "merge_request_iid"},
		},
	}, gl.handleMergeMergeRequest)
}
func (g *gitlabServer) handleCreateMergeRequest(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	sourceBranch := v.Required("source_branch")
	targetBranch := v.Required("target_branch")
	title := v.Required("title")
	description := v.String("description", "")
	removeSourceBranch := v.Bool("remove_source_branch", false)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	payload := map[string]any{
		"source_branch":        sourceBranch,
		"target_branch":        targetBranch,
		"title":                title,
		"remove_source_branch": removeSourceBranch,
	}
	if description != "" {
		payload["description"] = description
	}
	path := fmt.Sprintf("/projects/%s/merge_requests", encodeProject(project))
	result, err := g.request(ctx, "POST", path, payload)
	if err != nil {
		return nil, err
	}
	return gitlabJSONResult(result)
}
func (g *gitlabServer) handleGetMergeRequest(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	mergeRequestIID := v.RequiredInt("merge_request_iid")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if errResult := validatePositiveIntParam("merge_request_iid", mergeRequestIID); errResult != nil {
		return errResult, nil
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", encodeProject(project), mergeRequestIID)
	result, err := g.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	return gitlabJSONResult(result)
}
func (g *gitlabServer) handleListMergeRequests(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	state := v.Enum("state", "opened", "opened", "closed", "merged", "all")
	sourceBranch := v.String("source_branch", "")
	targetBranch := v.String("target_branch", "")
	perPage := normalizePerPage(v.Int("per_page", 20), 20)
	page := normalizePage(v.Int("page", 1))
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	q := url.Values{}
	q.Set("state", state)
	q.Set("per_page", fmt.Sprintf("%d", perPage))
	q.Set("page", fmt.Sprintf("%d", page))
	if sourceBranch != "" {
		q.Set("source_branch", sourceBranch)
	}
	if targetBranch != "" {
		q.Set("target_branch", targetBranch)
	}
	path := fmt.Sprintf("/projects/%s/merge_requests?%s", encodeProject(project), q.Encode())
	result, meta, err := g.requestListWithMeta(ctx, path)
	if err != nil {
		return nil, err
	}
	return gitlabJSONResult(map[string]any{"merge_requests": result, "count": len(result), "pagination": meta})
}
func (g *gitlabServer) handleMergeMergeRequest(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	mergeRequestIID := v.RequiredInt("merge_request_iid")
	autoMerge := v.Bool("auto_merge", false)
	squash := v.Bool("squash", false)
	shouldRemoveSourceBranch := v.Bool("should_remove_source_branch", false)
	sha := v.String("sha", "")
	mergeCommitMessage := v.String("merge_commit_message", "")
	squashCommitMessage := v.String("squash_commit_message", "")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if errResult := validatePositiveIntParam("merge_request_iid", mergeRequestIID); errResult != nil {
		return errResult, nil
	}
	if g.mergeQueueURL != "" {
		result, fallback, err := g.enqueueMergeRequest(ctx, project, mergeRequestIID, sha)
		if err != nil || !fallback {
			return result, err
		}
		// The queue reported policy-disabled: fall through to the direct
		// merge, mirroring the cluster merge stage's own disable semantics
		// (serialization is a policy choice, not an availability constraint).
	}
	payload := map[string]any{}
	if autoMerge {
		payload["auto_merge"] = true
	}
	if squash {
		payload["squash"] = true
	}
	if shouldRemoveSourceBranch {
		payload["should_remove_source_branch"] = true
	}
	if sha != "" {
		payload["sha"] = sha
	}
	if mergeCommitMessage != "" {
		payload["merge_commit_message"] = mergeCommitMessage
	}
	if squashCommitMessage != "" {
		payload["squash_commit_message"] = squashCommitMessage
	}
	var body any
	if len(payload) > 0 {
		body = payload
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/merge", encodeProject(project), mergeRequestIID)
	result, err := g.request(ctx, "PUT", path, body)
	if err == nil {
		return gitlabJSONResult(result)
	}
	if !autoMerge || !isMergeNotAcceptedError(err) {
		return nil, err
	}
	return g.retryAutoMerge(ctx, project, mergeRequestIID, path, body, err)
}

// enqueueMergeRequest routes the merge through the Mills serial merge queue.
// fallbackToDirect is true ONLY for the queue's policy-disabled outcome, in
// which case the caller performs the pre-queue direct merge; every other
// non-success is surfaced to the agent (a full lane or an unreachable queue
// must NOT silently degrade into an unserialized merge).
func (g *gitlabServer) enqueueMergeRequest(ctx context.Context, project string, iid int, requestedSHA string) (result *mcp.CallToolResult, fallbackToDirect bool, err error) {
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", encodeProject(project), iid)
	mr, err := g.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, false, err
	}
	observed, _ := mr["sha"].(string)
	if observed == "" {
		return mcp.ErrorResult(mcperror.New("MERGE_QUEUE_INVALID_CANDIDATE", "GitLab did not return the merge request head SHA")), false, nil
	}
	if requestedSHA != "" && requestedSHA != observed {
		return mcp.ErrorResult(mcperror.New("MERGE_QUEUE_STALE_SHA", "requested SHA no longer matches the merge request head")), false, nil
	}
	source, _ := mr["source_branch"].(string)
	target, _ := mr["target_branch"].(string)
	if source == "" || target == "" {
		return mcp.ErrorResult(mcperror.New("MERGE_QUEUE_INVALID_CANDIDATE", "GitLab did not return source and target branches")), false, nil
	}
	payload := map[string]any{"producer": "mcp_gitlab", "idempotency_key": fmt.Sprintf("mcp_gitlab:%s:%d:%s", project, iid, observed), "project": project, "mr_iid": iid, "source_branch": source, "target_branch": target, "observed_sha": observed}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.mergeQueueURL+"/api/mills/merge-queue/enqueue", bytes.NewReader(b))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.mergeQueueToken != "" {
		req.Header.Set("Authorization", "Bearer "+g.mergeQueueToken)
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("merge queue unavailable: %w", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false, fmt.Errorf("decode merge queue response: %w", err)
	}
	if outcome, _ := body["outcome"].(string); outcome == "disabled" {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return mcp.ErrorResult(mcperror.New("MERGE_QUEUE_REJECTED", fmt.Sprintf("merge queue rejected candidate with HTTP %d", resp.StatusCode)).WithDetails(body)), false, nil
	}
	jr, err := gitlabJSONResult(map[string]any{"merge_request_iid": iid, "project": project, "merge_queue": body, "message": "merge request queued; poll GitLab for completion"})
	return jr, false, err
}

// isMergeNotAcceptedError reports whether err is GitLab's "merge not accepted
// (yet)" rejection: 405 Method Not Allowed or 406 Not Acceptable.
func isMergeNotAcceptedError(err error) bool {
	sc := apiStatusCode(err)
	return sc == http.StatusMethodNotAllowed || sc == http.StatusNotAcceptable
}

// retryAutoMerge handles a 405/406 on an auto_merge request: wait for the head
// pipeline to exist, retry the merge request a bounded number of times, and on
// persistent rejection return an actionable error instead of the raw status.
func (g *gitlabServer) retryAutoMerge(ctx context.Context, project string, mergeRequestIID int, path string, body any, firstErr error) (*mcp.CallToolResult, error) {
	lastErr := firstErr
	var headPipeline map[string]any
	for attempt := 0; attempt < autoMergeRetryAttempts; attempt++ {
		if err := poll.WaitWithContext(ctx, backoffDelay(attempt, autoMergeRetryBackoffMax)); err != nil {
			return nil, err
		}
		headPipeline = g.waitForHeadPipeline(ctx, project, mergeRequestIID, autoMergeHeadPipelineWait)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		result, err := g.request(ctx, "PUT", path, body)
		if err == nil {
			return gitlabJSONResult(result)
		}
		if !isMergeNotAcceptedError(err) {
			return nil, err
		}
		lastErr = err
	}
	return mcp.ErrorResult(autoMergeRejectedError(project, mergeRequestIID, headPipeline, lastErr)), nil
}

// waitForHeadPipeline polls the merge request until GitLab reports a head
// pipeline or maxWait elapses. Returns the head pipeline object, or nil.
func (g *gitlabServer) waitForHeadPipeline(ctx context.Context, project string, mergeRequestIID int, maxWait time.Duration) map[string]any {
	deadline := time.Now().Add(maxWait)
	path := fmt.Sprintf("/projects/%s/merge_requests/%d", encodeProject(project), mergeRequestIID)
	for {
		mr, err := g.request(ctx, "GET", path, nil)
		if err == nil {
			if hp, ok := mr["head_pipeline"].(map[string]any); ok && len(hp) > 0 {
				return hp
			}
		}
		if time.Now().Add(autoMergePollInterval).After(deadline) {
			return nil
		}
		if poll.WaitWithContext(ctx, autoMergePollInterval) != nil {
			return nil
		}
	}
}

// autoMergeRejectedError turns a persistent 405/406 into guidance the calling
// agent can act on, instead of an opaque status it will retry in a loop.
func autoMergeRejectedError(project string, mergeRequestIID int, headPipeline map[string]any, lastErr error) error {
	sc := apiStatusCode(lastErr)
	pipelineHint := "no head pipeline exists yet (pipeline creation can lag the push)"
	details := map[string]any{
		"project":           project,
		"merge_request_iid": mergeRequestIID,
		"status_code":       sc,
		"retries_exhausted": true,
	}
	if headPipeline != nil {
		status, _ := headPipeline["status"].(string)
		if id, ok := toInt(headPipeline["id"]); ok {
			details["head_pipeline_id"] = id
			details["head_pipeline_status"] = status
			pipelineHint = fmt.Sprintf("head pipeline %d is %q", id, status)
		}
	}
	msg := fmt.Sprintf(
		"GitLab rejected auto-merge for %s!%d after retries (HTTP %d): %s. "+
			"Do not call merge_merge_request again in a loop. "+
			"Poll the head pipeline with poll_pipeline (or pipeline_summary) until it succeeds, then request the merge once. "+
			"A persistent HTTP 406 usually means the source branch conflicts with the target and needs a rebase. "+
			"Also check the merge request for draft status or unresolved blocking discussions.",
		project, mergeRequestIID, sc, pipelineHint)
	return mcperror.New("AUTO_MERGE_NOT_READY", msg).WithDetails(details)
}

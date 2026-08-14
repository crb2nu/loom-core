package main

import (
	"context"
	"fmt"
	"net/url"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mcpscaffold"
	"github.com/crb2nu/loom/pkg/strutil"
	"github.com/crb2nu/loom/pkg/validate"
)

const (
	defaultMergeRequestDiffBudget = 40_000
	maxMergeRequestDiffBudget     = 1_000_000
)

func registerMergeRequestReviewTools(srv *mcpscaffold.Server, gl *gitlabServer) {
	srv.AddTracedTool(mcp.Tool{
		Name:        "list_merge_request_diffs",
		Description: "List a merge request's file diffs with pagination and a bounded total diff-text budget",
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
				"page": map[string]any{
					"type":        "integer",
					"description": "Page number (default 1)",
				},
				"per_page": map[string]any{
					"type":        "integer",
					"description": "Diff files per page (default 20, max 100)",
				},
				"unidiff": map[string]any{
					"type":        "boolean",
					"description": "Return diff text in unified diff format",
				},
				"max_total_diff_bytes": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     maxMergeRequestDiffBudget,
					"description": "Maximum combined diff-text bytes returned across this page (default 40000, max 1000000)",
				},
			},
			Required: []string{"project", "merge_request_iid"},
		},
	}, gl.handleListMergeRequestDiffs)

	srv.AddTracedTool(mcp.Tool{
		Name:        "list_merge_request_discussions",
		Description: "List merge request discussions, optionally filtering the fetched page by resolution state",
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
				"discussion_state": map[string]any{
					"type":        "string",
					"enum":        []string{"all", "unresolved", "resolved"},
					"description": "Filter the fetched page by resolution state (default all)",
				},
				"page": map[string]any{
					"type":        "integer",
					"description": "Page number (default 1)",
				},
				"per_page": map[string]any{
					"type":        "integer",
					"description": "Discussions per page (default 20, max 100)",
				},
			},
			Required: []string{"project", "merge_request_iid"},
		},
	}, gl.handleListMergeRequestDiscussions)

	srv.AddTracedTool(mcp.Tool{
		Name:        "resolve_merge_request_discussion",
		Description: "Resolve or reopen a merge request discussion",
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
				"discussion_id": map[string]any{
					"type":        "string",
					"description": "Discussion ID returned by list_merge_request_discussions",
				},
				"resolved": map[string]any{
					"type":        "boolean",
					"description": "True to resolve the discussion; false to reopen it",
				},
			},
			Required: []string{"project", "merge_request_iid", "discussion_id", "resolved"},
		},
	}, gl.handleResolveMergeRequestDiscussion)
}

func (g *gitlabServer) handleListMergeRequestDiffs(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	mergeRequestIID := v.RequiredInt("merge_request_iid")
	page := normalizePage(v.Int("page", 1))
	perPage := normalizePerPage(v.Int("per_page", 20), 20)
	unidiff := v.Bool("unidiff", false)
	diffBudget := v.IntRange("max_total_diff_bytes", defaultMergeRequestDiffBudget, 1, maxMergeRequestDiffBudget)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if errResult := validatePositiveIntParam("merge_request_iid", mergeRequestIID); errResult != nil {
		return errResult, nil
	}

	q := url.Values{}
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("per_page", fmt.Sprintf("%d", perPage))
	if unidiff {
		q.Set("unidiff", "true")
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/diffs?%s", encodeProject(project), mergeRequestIID, q.Encode())
	diffs, pagination, err := g.requestListWithMeta(ctx, path)
	if err != nil {
		return nil, err
	}

	diffBytesReturned, truncatedFiles := truncateMergeRequestDiffs(diffs, diffBudget)
	return gitlabJSONResult(map[string]any{
		"diffs":               diffs,
		"count":               len(diffs),
		"pagination":          pagination,
		"diff_budget_bytes":   diffBudget,
		"diff_bytes_returned": diffBytesReturned,
		"truncated_files":     truncatedFiles,
	})
}

func truncateMergeRequestDiffs(diffs []any, budget int) (int, int) {
	remaining := budget
	returned := 0
	truncated := 0

	for i, raw := range diffs {
		diff, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, ok := diff["diff"].(string)
		if !ok {
			continue
		}

		filesRemaining := len(diffs) - i
		allowance := 0
		if filesRemaining > 0 {
			allowance = remaining / filesRemaining
		}
		originalBytes := len(text)
		if originalBytes > allowance {
			text = truncateMergeRequestDiffText(text, allowance)
			diff["diff"] = text
			diff["diff_truncated"] = true
			diff["original_diff_bytes"] = originalBytes
			truncated++
		}
		used := len(text)
		returned += used
		remaining -= used
	}

	return returned, truncated
}

func truncateMergeRequestDiffText(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	// Truncation with an ellipsis requires enough room for at least one byte of
	// content. Returning an empty string for smaller budgets also avoids cutting
	// a multi-byte UTF-8 rune in the middle.
	if maxBytes < 4 {
		return ""
	}
	return strutil.TruncateBytes(text, maxBytes)
}

func (g *gitlabServer) handleListMergeRequestDiscussions(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	mergeRequestIID := v.RequiredInt("merge_request_iid")
	discussionState := v.Enum("discussion_state", "all", "all", "unresolved", "resolved")
	page := normalizePage(v.Int("page", 1))
	perPage := normalizePerPage(v.Int("per_page", 20), 20)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if errResult := validatePositiveIntParam("merge_request_iid", mergeRequestIID); errResult != nil {
		return errResult, nil
	}

	q := url.Values{}
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("per_page", fmt.Sprintf("%d", perPage))
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/discussions?%s", encodeProject(project), mergeRequestIID, q.Encode())
	discussions, pagination, err := g.requestListWithMeta(ctx, path)
	if err != nil {
		return nil, err
	}

	filtered, summary := filterMergeRequestDiscussions(discussions, discussionState)
	return gitlabJSONResult(map[string]any{
		"discussions":      filtered,
		"count":            len(filtered),
		"discussion_state": discussionState,
		"page_summary":     summary,
		"pagination":       pagination,
	})
}

func filterMergeRequestDiscussions(discussions []any, requestedState string) ([]any, map[string]any) {
	filtered := make([]any, 0, len(discussions))
	counts := map[string]int{
		"unresolved":   0,
		"resolved":     0,
		"unresolvable": 0,
	}

	for _, discussion := range discussions {
		state := mergeRequestDiscussionState(discussion)
		counts[state]++
		if requestedState == "all" || requestedState == state {
			filtered = append(filtered, discussion)
		}
	}

	return filtered, map[string]any{
		"fetched":      len(discussions),
		"unresolved":   counts["unresolved"],
		"resolved":     counts["resolved"],
		"unresolvable": counts["unresolvable"],
	}
}

func mergeRequestDiscussionState(raw any) string {
	discussion, ok := raw.(map[string]any)
	if !ok {
		return "unresolvable"
	}
	notes, ok := discussion["notes"].([]any)
	if !ok {
		return "unresolvable"
	}

	resolvable := false
	for _, rawNote := range notes {
		note, ok := rawNote.(map[string]any)
		if !ok {
			continue
		}
		canResolve, _ := note["resolvable"].(bool)
		if !canResolve {
			continue
		}
		resolvable = true
		resolved, _ := note["resolved"].(bool)
		if !resolved {
			return "unresolved"
		}
	}
	if resolvable {
		return "resolved"
	}
	return "unresolvable"
}

func (g *gitlabServer) handleResolveMergeRequestDiscussion(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	project := v.Required("project")
	mergeRequestIID := v.RequiredInt("merge_request_iid")
	discussionID := v.Required("discussion_id")
	resolved := v.RequiredBool("resolved")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	if errResult := validatePositiveIntParam("merge_request_iid", mergeRequestIID); errResult != nil {
		return errResult, nil
	}

	path := fmt.Sprintf("/projects/%s/merge_requests/%d/discussions/%s", encodeProject(project), mergeRequestIID, url.PathEscape(discussionID))
	result, err := g.request(ctx, "PUT", path, map[string]any{"resolved": resolved})
	if err != nil {
		return nil, err
	}
	return gitlabJSONResult(result)
}

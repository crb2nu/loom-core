package clients

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type projectHook struct {
	ID                 int64  `json:"id"`
	URL                string `json:"url"`
	PipelineEvents     bool   `json:"pipeline_events"`
	MergeRequestEvents bool   `json:"merge_requests_events"`
}

type projectHookRequest struct {
	URL                string `json:"url"`
	Token              string `json:"token"`
	PipelineEvents     bool   `json:"pipeline_events"`
	MergeRequestEvents bool   `json:"merge_requests_events"`
}

// EnsureMillsWebhook idempotently creates or repairs this client's project
// hook. GitLab never returns hook tokens, so a matching hook is updated on
// every reconciliation to ensure secret rotation is applied.
func (c *GitLabClient) EnsureMillsWebhook(ctx context.Context, publicURL, secret string) error {
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	secret = strings.TrimSpace(secret)
	if publicURL == "" || secret == "" {
		return nil
	}
	wantURL := publicURL + "/api/mills/hooks/gitlab"
	path := fmt.Sprintf("/projects/%s/hooks", c.projectPath())
	var hooks []projectHook
	if err := c.requestJSON(ctx, http.MethodGet, path+"?per_page=100", nil, &hooks); err != nil {
		return err
	}
	body := projectHookRequest{URL: wantURL, Token: secret, PipelineEvents: true, MergeRequestEvents: true}
	for _, hook := range hooks {
		if strings.TrimRight(hook.URL, "/") != wantURL {
			continue
		}
		return c.requestJSON(ctx, http.MethodPut, fmt.Sprintf("%s/%d", path, hook.ID), body, nil)
	}
	return c.requestJSON(ctx, http.MethodPost, path, body, nil)
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Issue is the subset of GitLab's issue payload this tool needs.
type Issue struct {
	IID         int    `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	WebURL      string `json:"web_url"`
	State       string `json:"state"`
}

// GitLab is a minimal GitLab issues client.
//
// Deliberately hand-rolled rather than pulled from a client library: this runs
// in a job's after_script, where the smallest possible dependency surface is
// the point. Every method is context-bounded so a hung GitLab cannot extend a
// CI job past its timeout.
type GitLab struct {
	BaseURL string // e.g. https://gitlab.example.com/api/v4
	Project string // numeric project ID or URL-encoded path
	Token   string
	HTTP    *http.Client
}

// NewGitLab builds a client with a bounded HTTP timeout.
func NewGitLab(baseURL, project, token string) *GitLab {
	return &GitLab{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Project: project,
		Token:   token,
		HTTP:    &http.Client{Timeout: 20 * time.Second},
	}
}

func (g *GitLab) projectPath(suffix string) string {
	return fmt.Sprintf("%s/projects/%s%s", g.BaseURL, url.PathEscape(g.Project), suffix)
}

func (g *GitLab) do(ctx context.Context, method, endpoint string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, redact(endpoint), err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s",
			method, redact(endpoint), resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// redact strips query strings from an endpoint before it reaches a log line.
func redact(endpoint string) string {
	if i := strings.Index(endpoint, "?"); i >= 0 {
		return endpoint[:i]
	}
	return endpoint
}

// ListOpenByLabel returns every open issue carrying the label, newest first.
func (g *GitLab) ListOpenByLabel(ctx context.Context, label string) ([]Issue, error) {
	q := url.Values{}
	q.Set("labels", label)
	q.Set("state", "opened")
	q.Set("per_page", "100")
	var issues []Issue
	if err := g.do(ctx, http.MethodGet, g.projectPath("/issues?"+q.Encode()), nil, &issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// FindOpenByTitle returns the open labelled issue whose title matches exactly.
//
// GitLab's `search` is fuzzy, so the exact match is re-checked client-side:
// "flake: TestFoo" must not adopt the issue for "flake: TestFooBar".
func (g *GitLab) FindOpenByTitle(ctx context.Context, label, title string) (*Issue, error) {
	q := url.Values{}
	q.Set("labels", label)
	q.Set("state", "opened")
	q.Set("search", title)
	q.Set("in", "title")
	q.Set("per_page", "100")
	var issues []Issue
	if err := g.do(ctx, http.MethodGet, g.projectPath("/issues?"+q.Encode()), nil, &issues); err != nil {
		return nil, err
	}
	for i := range issues {
		if issues[i].Title == title {
			return &issues[i], nil
		}
	}
	return nil, nil
}

// CreateIssue files a new issue.
func (g *GitLab) CreateIssue(ctx context.Context, title, description string, labels []string) (*Issue, error) {
	body := map[string]any{
		"title":       title,
		"description": description,
		"labels":      strings.Join(labels, ","),
	}
	var issue Issue
	if err := g.do(ctx, http.MethodPost, g.projectPath("/issues"), body, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

// UpdateDescription rewrites an issue's description.
func (g *GitLab) UpdateDescription(ctx context.Context, iid int, description string) error {
	body := map[string]any{"description": description}
	return g.do(ctx, http.MethodPut, g.projectPath("/issues/"+strconv.Itoa(iid)), body, nil)
}

// Comment appends a note to an issue.
func (g *GitLab) Comment(ctx context.Context, iid int, note string) error {
	body := map[string]any{"body": note}
	return g.do(ctx, http.MethodPost, g.projectPath("/issues/"+strconv.Itoa(iid)+"/notes"), body, nil)
}

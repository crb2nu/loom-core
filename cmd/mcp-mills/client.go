package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/mcperror"
)

// operatorClient speaks the mills operator's PascalCase REST wire contract.
// Reads are open; writes carry the bearer token when configured.
type operatorClient struct {
	base   string
	token  string
	client *httpclient.Client
	// cfID / cfSecret are the Cloudflare Access service-token pair for the
	// public operator edge (mills.flexinfer.ai). The operator itself leaves
	// reads tokenless, but the edge in front of it does not, so without these
	// every call from a workstation answers with the Access login page. Never
	// sent to loopback targets (an in-cluster port-forward needs no edge).
	cfID     string
	cfSecret string
}

// decorate adds the Cloudflare Access headers when the target is not loopback
// and a service-token pair is configured.
func (c *operatorClient) decorate(req *http.Request) {
	if c.cfID == "" || c.cfSecret == "" {
		return
	}
	host := req.URL.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return
	}
	req.Header.Set("CF-Access-Client-Id", c.cfID)
	req.Header.Set("CF-Access-Client-Secret", c.cfSecret)
}

func (c *operatorClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *operatorClient) post(ctx context.Context, path string, body, out any) error {
	if c.token == "" {
		return mcperror.NotConfigured("LOOM_MILLS_TOKEN", "mutations need the mills operator admin token (k8s secret loom-mills/loom-mills-admin, key admin-token) in LOOM_MILLS_TOKEN, LOOM_MILLS_OPERATOR_TOKEN, or LOOM_MILLS_ADMIN_TOKEN; the HUD admin token is a different credential the write path rejects")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.do(req, out)
}

// errStaleRevision marks a 409 so callers can re-read and retry exactly once.
type errStaleRevision struct{ body string }

func (e errStaleRevision) Error() string { return "revision conflict (409): " + e.body }

func (c *operatorClient) do(req *http.Request, out any) error {
	c.decorate(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return mcperror.WrapAPI("mills-operator", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return mcperror.WrapAPI("mills-operator", err)
	}
	if resp.StatusCode == http.StatusConflict {
		return errStaleRevision{body: truncate(string(raw), 200)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return mcperror.APIError("mills-operator", resp.StatusCode, truncate(string(raw), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mills-operator: unparseable response: %w", err)
	}
	return nil
}

// branchProber reports remote implement branches for a backlog item. The
// pipeline pushes branches named feat/<backlog-id>/<slice>; any survivor means
// a requeue would collide with the prior attempt (FailureClass=configuration,
// no useful logs) instead of adopting it.
type branchProber interface {
	ImplementBranches(ctx context.Context, project, backlogID string) ([]string, error)
}

type gitLsRemoteProber struct{ gitlabBase string }

func (g *gitLsRemoteProber) ImplementBranches(ctx context.Context, project, backlogID string) ([]string, error) {
	if project == "" {
		project = "services/loom-core"
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "ls-remote", "--heads",
		g.gitlabBase+"/"+project+".git", "refs/heads/feat/"+backlogID+"/*")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("branch probe (%s): %w", project, err)
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			branches = append(branches, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	return branches, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ScopeRescueBranch verifies the MR belongs to this exact surviving branch.
// Query failures fail closed; the operator token is never sent to GitLab.
func (g *gitLsRemoteProber) ScopeRescueBranch(ctx context.Context, project, branch string) (bool, error) {
	if project == "" {
		project = "services/loom-core"
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, g.gitlabBase+"/api/v4/projects/"+url.PathEscape(project)+"/merge_requests?state=opened&source_branch="+url.QueryEscape(branch)+"&per_page=100", nil)
	if err != nil {
		return false, err
	}
	if token := env.StringWithFallbacks("GITLAB_PERSONAL_ACCESS_TOKEN", "GITLAB_TOKEN"); token != "" {
		req.Header.Set("PRIVATE-TOKEN", token)
	}
	resp, err := httpclient.NewDefault().Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("scope-rescue MR probe: HTTP %d", resp.StatusCode)
	}
	var mrs []struct {
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		State        string `json:"state"`
		Draft        bool   `json:"draft"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&mrs); err != nil {
		return false, err
	}
	for _, mr := range mrs {
		if mr.SourceBranch == branch && mr.State == "opened" && mr.Draft && strings.HasPrefix(mr.Title, "Draft: [scope-escalated]") {
			return true, nil
		}
	}
	return false, nil
}

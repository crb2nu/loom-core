// Package forge gives the HUD a thin, credential-aware view of the two code
// forges the workspace uses: GitLab (canonical, where Mills weaves) and
// GitHub (the public mirror). It answers "who am I on each forge", lists
// repos and groups for the intake picker, creates a new GitLab project with
// a seed commit and a GitHub push mirror, and imports a GitHub repo into
// GitLab so Mills can drive it.
//
// Credentials are never stored by the HUD. Each request resolves a token
// from, in order, the environment (GITLAB_TOKEN / GITHUB_TOKEN, the same
// names the operator and mrwatch use) or the operator's own CLI login where
// the daemon runs (`glab config get token`, `gh auth token`). That is what
// "auth via glab or gh" means here: the CLIs are the credential source, the
// HUD talks to the REST APIs itself. In-cluster there are no CLIs and only
// env tokens apply; the status endpoint says so with the exact command to run.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Provider identifies a forge.
type Provider string

const (
	ProviderGitLab Provider = "gitlab"
	ProviderGitHub Provider = "github"
)

// Credential is a resolved token plus where it came from.
type Credential struct {
	Provider Provider
	Token    string
	Source   string // "env" | "glab" | "gh"
	Host     string // e.g. gitlab.flexinfer.ai / github.com
	APIURL   string // REST base
}

// Status is the non-secret view of a provider for GET /api/forge/auth.
type Status struct {
	Provider    Provider `json:"provider"`
	Configured  bool     `json:"configured"`
	Source      string   `json:"source,omitempty"`
	Host        string   `json:"host"`
	APIURL      string   `json:"api_url"`
	User        string   `json:"user,omitempty"`
	ConnectHint string   `json:"connect_hint,omitempty"`
	Error       string   `json:"error,omitempty"`
	CLIPresent  bool     `json:"cli_present"`
}

const (
	defaultGitLabAPIURL = "https://gitlab.flexinfer.ai/api/v4"
	defaultGitHubAPIURL = "https://api.github.com"
	cliTimeout          = 4 * time.Second
	credentialTTL       = 60 * time.Second
	requestTimeout      = 20 * time.Second
)

// ErrNotConfigured is returned when no credential resolves for a provider.
var ErrNotConfigured = errors.New("forge: provider not configured")

// Resolver resolves and caches credentials.
type Resolver struct {
	// Env/exec seams for tests.
	Getenv   func(string) string
	LookPath func(string) (string, error)
	RunCLI   func(ctx context.Context, name string, args ...string) (string, error)
	HTTP     *http.Client

	mu    sync.Mutex
	cache map[Provider]cachedCred
}

type cachedCred struct {
	cred Credential
	err  error
	at   time.Time
}

// NewResolver builds a Resolver with real env, PATH, and exec.
func NewResolver() *Resolver {
	return &Resolver{
		Getenv:   os.Getenv,
		LookPath: exec.LookPath,
		RunCLI:   runCLI,
		HTTP:     &http.Client{Timeout: requestTimeout},
		cache:    map[Provider]cachedCred{},
	}
}

func runCLI(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s: %s", name, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func firstEnv(get func(string) string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(get(k)); v != "" {
			return v
		}
	}
	return ""
}

func (r *Resolver) gitlabAPIURL() string {
	if v := firstEnv(r.Getenv, "GITLAB_API_URL", "LOOM_GITLAB_API_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultGitLabAPIURL
}

func (r *Resolver) githubAPIURL() string {
	if v := firstEnv(r.Getenv, "GITHUB_API_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultGitHubAPIURL
}

func hostOf(apiURL string) string {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return apiURL
	}
	h := u.Host
	if h == "api.github.com" {
		return "github.com"
	}
	return h
}

// CLIPresent reports whether the provider's CLI is on PATH where the daemon runs.
func (r *Resolver) CLIPresent(p Provider) bool {
	name := "glab"
	if p == ProviderGitHub {
		name = "gh"
	}
	_, err := r.LookPath(name)
	return err == nil
}

// ConnectHint is the command an operator runs to make a provider available.
func (r *Resolver) ConnectHint(p Provider) string {
	switch p {
	case ProviderGitLab:
		return fmt.Sprintf("glab auth login --hostname %s   (or set GITLAB_TOKEN where loomd runs)", hostOf(r.gitlabAPIURL()))
	default:
		return "gh auth login   (or set GITHUB_TOKEN where loomd runs)"
	}
}

// Credential resolves (with a short cache) the token for a provider.
func (r *Resolver) Credential(ctx context.Context, p Provider) (Credential, error) {
	r.mu.Lock()
	if c, ok := r.cache[p]; ok && time.Since(c.at) < credentialTTL {
		r.mu.Unlock()
		return c.cred, c.err
	}
	r.mu.Unlock()

	cred, err := r.resolve(ctx, p)
	r.mu.Lock()
	r.cache[p] = cachedCred{cred: cred, err: err, at: time.Now()}
	r.mu.Unlock()
	return cred, err
}

// Forget drops the cached credential (after an auth failure).
func (r *Resolver) Forget(p Provider) {
	r.mu.Lock()
	delete(r.cache, p)
	r.mu.Unlock()
}

func (r *Resolver) resolve(ctx context.Context, p Provider) (Credential, error) {
	switch p {
	case ProviderGitLab:
		api := r.gitlabAPIURL()
		host := hostOf(api)
		if tok := firstEnv(r.Getenv, "GITLAB_TOKEN", "GITLAB_PERSONAL_ACCESS_TOKEN", "GITLAB_PAT"); tok != "" {
			return Credential{Provider: p, Token: tok, Source: "env", Host: host, APIURL: api}, nil
		}
		if r.CLIPresent(p) {
			tok, err := r.RunCLI(ctx, "glab", "config", "get", "token", "--host", host)
			if err == nil && strings.TrimSpace(tok) != "" {
				return Credential{Provider: p, Token: strings.TrimSpace(tok), Source: "glab", Host: host, APIURL: api}, nil
			}
		}
		return Credential{Provider: p, Host: host, APIURL: api}, ErrNotConfigured
	case ProviderGitHub:
		api := r.githubAPIURL()
		host := hostOf(api)
		if tok := firstEnv(r.Getenv, "GITHUB_TOKEN", "GITHUB_PERSONAL_ACCESS_TOKEN", "GH_TOKEN"); tok != "" {
			return Credential{Provider: p, Token: tok, Source: "env", Host: host, APIURL: api}, nil
		}
		if r.CLIPresent(p) {
			tok, err := r.RunCLI(ctx, "gh", "auth", "token")
			if err == nil && strings.TrimSpace(tok) != "" {
				return Credential{Provider: p, Token: strings.TrimSpace(tok), Source: "gh", Host: host, APIURL: api}, nil
			}
		}
		return Credential{Provider: p, Host: host, APIURL: api}, ErrNotConfigured
	}
	return Credential{}, fmt.Errorf("forge: unknown provider %q", p)
}

// APIError is a non-2xx forge response.
type APIError struct {
	Provider   Provider
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300] + "…"
	}
	return fmt.Sprintf("%s %s %s: HTTP %d %s", e.Provider, e.Method, e.Path, e.StatusCode, b)
}

// do performs one REST call against the credential's API.
func (r *Resolver) do(ctx context.Context, cred Credential, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	full := cred.APIURL + path
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "loom-hud-forge/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch cred.Provider {
	case ProviderGitLab:
		req.Header.Set("PRIVATE-TOKEN", cred.Token)
	case ProviderGitHub:
		req.Header.Set("Authorization", "Bearer "+cred.Token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s %s: %w", cred.Provider, method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Provider: cred.Provider, Method: method, Path: path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(buf))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func isNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound
}

func isUnauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden)
}

// --- identity -------------------------------------------------------------

// Whoami returns the login for a credential.
func (r *Resolver) Whoami(ctx context.Context, cred Credential) (string, error) {
	var u struct {
		Username string `json:"username"` // GitLab
		Login    string `json:"login"`    // GitHub
	}
	if err := r.do(ctx, cred, http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Username != "" {
		return u.Username, nil
	}
	return u.Login, nil
}

// StatusFor composes the non-secret status for a provider.
func (r *Resolver) StatusFor(ctx context.Context, p Provider) Status {
	st := Status{Provider: p, CLIPresent: r.CLIPresent(p), ConnectHint: r.ConnectHint(p)}
	cred, err := r.Credential(ctx, p)
	st.Host, st.APIURL = cred.Host, cred.APIURL
	if err != nil {
		st.Error = "no credential: set the token in the environment or log in with the CLI where loomd runs"
		return st
	}
	st.Configured = true
	st.Source = cred.Source
	user, err := r.Whoami(ctx, cred)
	if err != nil {
		st.Configured = false
		st.Error = "credential rejected: " + err.Error()
		r.Forget(p)
		return st
	}
	st.User = user
	st.ConnectHint = ""
	return st
}

// --- listing --------------------------------------------------------------

// Repo is one repository on either forge, normalised for the picker.
type Repo struct {
	Provider      Provider `json:"provider"`
	Path          string   `json:"path"` // "group/name" or "owner/name"
	Name          string   `json:"name"`
	WebURL        string   `json:"web_url"`
	Visibility    string   `json:"visibility"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	Description   string   `json:"description,omitempty"`
	LastActivity  string   `json:"last_activity,omitempty"`
	Archived      bool     `json:"archived"`
	ProjectID     int64    `json:"project_id,omitempty"`
}

// Group is a GitLab namespace a project can be created under.
type Group struct {
	ID       int64  `json:"id"`
	FullPath string `json:"full_path"`
	Name     string `json:"name"`
	WebURL   string `json:"web_url,omitempty"`
}

const listPageSize = 30

// ListRepos lists repos the credential can see, most recently active first.
func (r *Resolver) ListRepos(ctx context.Context, p Provider, query string, page int) ([]Repo, bool, error) {
	cred, err := r.Credential(ctx, p)
	if err != nil {
		return nil, false, err
	}
	if page < 1 {
		page = 1
	}
	switch p {
	case ProviderGitLab:
		var rows []struct {
			ID                int64  `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
			Name              string `json:"name"`
			WebURL            string `json:"web_url"`
			Visibility        string `json:"visibility"`
			DefaultBranch     string `json:"default_branch"`
			Description       string `json:"description"`
			LastActivityAt    string `json:"last_activity_at"`
			Archived          bool   `json:"archived"`
		}
		q := url.Values{}
		q.Set("membership", "true")
		q.Set("order_by", "last_activity_at")
		q.Set("sort", "desc")
		q.Set("per_page", fmt.Sprint(listPageSize))
		q.Set("page", fmt.Sprint(page))
		if s := strings.TrimSpace(query); s != "" {
			q.Set("search", s)
		}
		if err := r.do(ctx, cred, http.MethodGet, "/projects?"+q.Encode(), nil, &rows); err != nil {
			return nil, false, err
		}
		out := make([]Repo, 0, len(rows))
		for _, x := range rows {
			out = append(out, Repo{
				Provider: p, ProjectID: x.ID, Path: x.PathWithNamespace, Name: x.Name, WebURL: x.WebURL,
				Visibility: x.Visibility, DefaultBranch: x.DefaultBranch, Description: x.Description,
				LastActivity: x.LastActivityAt, Archived: x.Archived,
			})
		}
		return out, len(rows) == listPageSize, nil
	case ProviderGitHub:
		var rows []struct {
			ID            int64  `json:"id"`
			FullName      string `json:"full_name"`
			Name          string `json:"name"`
			HTMLURL       string `json:"html_url"`
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
			Description   string `json:"description"`
			PushedAt      string `json:"pushed_at"`
			Archived      bool   `json:"archived"`
		}
		q := url.Values{}
		q.Set("sort", "pushed")
		q.Set("direction", "desc")
		q.Set("per_page", fmt.Sprint(listPageSize))
		q.Set("page", fmt.Sprint(page))
		q.Set("affiliation", "owner,collaborator,organization_member")
		if err := r.do(ctx, cred, http.MethodGet, "/user/repos?"+q.Encode(), nil, &rows); err != nil {
			return nil, false, err
		}
		needle := strings.ToLower(strings.TrimSpace(query))
		out := make([]Repo, 0, len(rows))
		for _, x := range rows {
			if needle != "" && !strings.Contains(strings.ToLower(x.FullName), needle) {
				continue
			}
			vis := "public"
			if x.Private {
				vis = "private"
			}
			out = append(out, Repo{
				Provider: p, ProjectID: x.ID, Path: x.FullName, Name: x.Name, WebURL: x.HTMLURL,
				Visibility: vis, DefaultBranch: x.DefaultBranch, Description: x.Description,
				LastActivity: x.PushedAt, Archived: x.Archived,
			})
		}
		return out, len(rows) == listPageSize, nil
	}
	return nil, false, fmt.Errorf("forge: unknown provider %q", p)
}

// ListGroups lists GitLab groups the credential can create projects in.
func (r *Resolver) ListGroups(ctx context.Context) ([]Group, error) {
	cred, err := r.Credential(ctx, ProviderGitLab)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID       int64  `json:"id"`
		FullPath string `json:"full_path"`
		Name     string `json:"name"`
		WebURL   string `json:"web_url"`
	}
	// min_access_level 30 (Developer) is the floor for creating projects in a
	// group on this instance; the operator's group token is Maintainer.
	if err := r.do(ctx, cred, http.MethodGet, "/groups?min_access_level=30&per_page=100&order_by=path&sort=asc", nil, &rows); err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(rows))
	for _, g := range rows {
		out = append(out, Group{ID: g.ID, FullPath: g.FullPath, Name: g.Name, WebURL: g.WebURL})
	}
	return out, nil
}

// gitlabProject is the subset of a GitLab project the flows need.
type gitlabProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch"`
	ImportStatus      string `json:"import_status"`
	ImportError       string `json:"import_error"`
	Visibility        string `json:"visibility"`
}

func (r *Resolver) gitlabProjectByPath(ctx context.Context, cred Credential, path string) (gitlabProject, bool, error) {
	var p gitlabProject
	err := r.do(ctx, cred, http.MethodGet, "/projects/"+url.PathEscape(path), nil, &p)
	if err != nil {
		if isNotFound(err) {
			return gitlabProject{}, false, nil
		}
		return gitlabProject{}, false, err
	}
	return p, true, nil
}

func (r *Resolver) gitlabNamespaceID(ctx context.Context, cred Credential, fullPath string) (int64, error) {
	var rows []struct {
		ID       int64  `json:"id"`
		FullPath string `json:"full_path"`
	}
	if err := r.do(ctx, cred, http.MethodGet, "/namespaces?search="+url.QueryEscape(fullPath)+"&per_page=100", nil, &rows); err != nil {
		return 0, err
	}
	for _, n := range rows {
		if n.FullPath == fullPath {
			return n.ID, nil
		}
	}
	return 0, fmt.Errorf("gitlab namespace %q not found (or the token cannot see it)", fullPath)
}

// githubRepo is the subset of a GitHub repo the flows need.
type githubRepo struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
}

func (r *Resolver) githubRepoByPath(ctx context.Context, cred Credential, ownerRepo string) (githubRepo, bool, error) {
	var g githubRepo
	err := r.do(ctx, cred, http.MethodGet, "/repos/"+ownerRepo, nil, &g)
	if err != nil {
		if isNotFound(err) {
			return githubRepo{}, false, nil
		}
		return githubRepo{}, false, err
	}
	return g, true, nil
}

// mirrorURLWithoutCreds strips userinfo for comparison and display.
func mirrorURLWithoutCreds(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

package clients

// gitlab_projects.go -- project-creation surface for the plan→repo bootstrap
// flow (POST /api/mills/projects/bootstrap). The operator's token must be
// group-scoped (the same services-group token that authorizes cross-repo
// execution) for these calls to succeed; a project-scoped pipeline token
// gets a 403 from POST /projects, which surfaces as a normal request error.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrNamespaceNotFound is returned by LookupNamespaceID when no accessible
// GitLab namespace matches the requested full path.
var ErrNamespaceNotFound = errors.New("gitlab: namespace not found")

// ProjectExists reports whether a GitLab project exists and is visible to the
// operator's token via GET /projects/:id. A 404 is a clean negative (exists
// false, nil error) — the caller uses this for the plan→repo bootstrap
// pre-flight, where "no repo yet" is the expected trigger, not an error. Any
// other non-2xx (403, 5xx, transport) surfaces as an error so the caller can
// defer rather than mistake a permissions problem for a missing repo. The
// returned web URL lets the caller record the registry row for a repo that
// already exists out-of-band (e.g. created by hand).
func (c *GitLabClient) ProjectExists(ctx context.Context, project string) (bool, string, error) {
	project = strings.Trim(strings.TrimSpace(project), "/")
	if project == "" {
		return false, "", errors.New("gitlab: ProjectExists: project required")
	}
	path := "/projects/" + url.PathEscape(project)
	full := strings.TrimRight(c.cfg.APIURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return false, "", fmt.Errorf("gitlab: ProjectExists new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("gitlab: ProjectExists GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, "", nil
	case resp.StatusCode >= 400:
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, "", &GitLabHTTPError{
			Method: http.MethodGet, Path: path, StatusCode: resp.StatusCode,
			Body: strings.TrimSpace(string(buf)),
		}
	}
	var got struct {
		WebURL string `json:"web_url"`
	}
	// A decode failure on a 2xx still means the project exists; the web URL is
	// a best-effort convenience, so swallow the decode error.
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return true, got.WebURL, nil
}

// LookupNamespaceID resolves a group full path (e.g. "services") to its
// numeric namespace id via GET /namespaces?search=. GitLab's search matches
// substrings, so the result set is filtered to an exact full_path match.
func (c *GitLabClient) LookupNamespaceID(ctx context.Context, fullPath string) (int64, error) {
	fullPath = strings.Trim(strings.TrimSpace(fullPath), "/")
	if fullPath == "" {
		return 0, errors.New("gitlab: LookupNamespaceID: fullPath required")
	}
	var namespaces []struct {
		ID       int64  `json:"id"`
		FullPath string `json:"full_path"`
	}
	path := fmt.Sprintf("/namespaces?search=%s&per_page=100", url.QueryEscape(fullPath))
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &namespaces); err != nil {
		return 0, err
	}
	for _, ns := range namespaces {
		if ns.FullPath == fullPath {
			return ns.ID, nil
		}
	}
	return 0, fmt.Errorf("%w: %q", ErrNamespaceNotFound, fullPath)
}

// CreateProjectRequest shapes POST /projects for the bootstrap flow. Path is
// the project slug WITHIN the namespace ("procmodel"); NamespaceID targets
// the group resolved by LookupNamespaceID. The repo is created EMPTY
// (initialize_with_readme false) so the seed commit is the single authored
// root commit.
type CreateProjectRequest struct {
	Name          string
	Path          string
	NamespaceID   int64
	Description   string
	Visibility    string // "private" | "internal" | "public"; empty = private
	DefaultBranch string // empty = "main"
}

// CreateProjectResponse is the subset of the GitLab project object the
// bootstrap flow needs.
type CreateProjectResponse struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch"`
}

// CreateProject creates a new GitLab project via POST /projects. The receiver
// is NOT re-scoped to the new project — use ForProject(resp.PathWithNamespace)
// for follow-up calls (e.g. the seed commit).
func (c *GitLabClient) CreateProject(ctx context.Context, req CreateProjectRequest) (CreateProjectResponse, error) {
	if strings.TrimSpace(req.Path) == "" {
		return CreateProjectResponse{}, errors.New("gitlab: CreateProject: Path required")
	}
	if req.NamespaceID <= 0 {
		return CreateProjectResponse{}, errors.New("gitlab: CreateProject: NamespaceID required")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.Path
	}
	visibility := strings.TrimSpace(req.Visibility)
	if visibility == "" {
		visibility = "private"
	}
	branch := strings.TrimSpace(req.DefaultBranch)
	if branch == "" {
		branch = "main"
	}
	payload := map[string]any{
		"name":                   name,
		"path":                   req.Path,
		"namespace_id":           req.NamespaceID,
		"visibility":             visibility,
		"default_branch":         branch,
		"initialize_with_readme": false,
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		payload["description"] = d
	}
	var got CreateProjectResponse
	if err := c.requestJSON(ctx, http.MethodPost, "/projects", payload, &got); err != nil {
		return CreateProjectResponse{}, err
	}
	if got.DefaultBranch == "" {
		got.DefaultBranch = branch
	}
	return got, nil
}

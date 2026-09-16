package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Deps is the subset of App the forge domain needs.
type Deps interface {
	WriteJSON(w http.ResponseWriter, status int, v any)
	WriteError(w http.ResponseWriter, status int, msg string, err error)
	RequireAdminToken(w http.ResponseWriter, r *http.Request) bool
	Logger() *slog.Logger
}

// Domain registers /api/forge/*.
type Domain struct {
	deps     Deps
	resolver *Resolver
}

// New creates the domain with a real resolver.
func New(deps Deps) *Domain {
	return &Domain{deps: deps, resolver: NewResolver()}
}

// NewWithResolver is the test seam.
func NewWithResolver(deps Deps, r *Resolver) *Domain {
	return &Domain{deps: deps, resolver: r}
}

// Name satisfies domain.Domain.
func (d *Domain) Name() string { return "forge" }

// RegisterRoutes satisfies domain.Domain.
func (d *Domain) RegisterRoutes(mux *http.ServeMux, mw func(http.HandlerFunc) http.HandlerFunc) {
	// Reads: identity and listings. No admin gate — nothing here mutates and
	// no token ever leaves the daemon.
	mux.HandleFunc("GET /api/forge/auth", mw(d.handleAuth))
	mux.HandleFunc("GET /api/forge/groups", mw(d.handleGroups))
	mux.HandleFunc("GET /api/forge/repos", mw(d.handleRepos))
	// Mutations: create a project (GitLab + seed + optional GitHub mirror) or
	// import a GitHub repo into GitLab. Admin-gated; both honour dry_run.
	mux.HandleFunc("POST /api/forge/projects", mw(d.handleCreateProject))
	mux.HandleFunc("POST /api/forge/projects/import", mw(d.handleImportProject))
}

// --- reads ----------------------------------------------------------------

func (d *Domain) handleAuth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	out := map[string]Status{
		string(ProviderGitLab): d.resolver.StatusFor(ctx, ProviderGitLab),
		string(ProviderGitHub): d.resolver.StatusFor(ctx, ProviderGitHub),
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (d *Domain) handleGroups(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	groups, err := d.resolver.ListGroups(ctx)
	if err != nil {
		d.writeProviderError(w, ProviderGitLab, err)
		return
	}
	if groups == nil {
		groups = []Group{}
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{"groups": groups, "count": len(groups)})
}

func (d *Domain) handleRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	provider := Provider(strings.ToLower(strings.TrimSpace(q.Get("provider"))))
	if provider == "" {
		provider = ProviderGitLab
	}
	if provider != ProviderGitLab && provider != ProviderGitHub {
		d.deps.WriteError(w, http.StatusBadRequest, "provider must be gitlab or github", nil)
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	repos, more, err := d.resolver.ListRepos(ctx, provider, q.Get("q"), page)
	if err != nil {
		d.writeProviderError(w, provider, err)
		return
	}
	if repos == nil {
		repos = []Repo{}
	}
	if page < 1 {
		page = 1
	}
	d.deps.WriteJSON(w, http.StatusOK, map[string]any{
		"provider": provider, "repos": repos, "count": len(repos), "page": page, "has_more": more,
	})
}

func (d *Domain) writeProviderError(w http.ResponseWriter, p Provider, err error) {
	switch {
	case errors.Is(err, ErrNotConfigured):
		d.deps.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":        fmt.Sprintf("%s is not connected", p),
			"provider":     p,
			"connect_hint": d.resolver.ConnectHint(p),
		})
	case isUnauthorized(err):
		d.resolver.Forget(p)
		d.deps.WriteError(w, http.StatusBadGateway, fmt.Sprintf("%s rejected the credential", p), err)
	default:
		d.deps.WriteError(w, http.StatusBadGateway, fmt.Sprintf("%s request failed", p), err)
	}
}

// --- mutations ------------------------------------------------------------

var pathSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// StepResult is one line of the per-request ledger every mutation returns.
type StepResult struct {
	Step   string `json:"step"`
	Status string `json:"status"` // planned | done | exists | skipped | failed
	Detail string `json:"detail,omitempty"`
}

// GitHubMirror describes the mirror half of a project.
type GitHubMirror struct {
	Repo          string `json:"repo"`
	WebURL        string `json:"web_url"`
	MirrorID      int64  `json:"mirror_id,omitempty"`
	MirrorEnabled bool   `json:"mirror_enabled"`
}

// CreateProjectRequest is POST /api/forge/projects.
type CreateProjectRequest struct {
	Name             string `json:"name"`
	Group            string `json:"group"`
	Description      string `json:"description,omitempty"`
	Visibility       string `json:"visibility,omitempty"` // private | internal | public
	Template         string `json:"template,omitempty"`   // generic | go
	MirrorToGitHub   bool   `json:"mirror_to_github"`
	GitHubOwner      string `json:"github_owner,omitempty"`
	GitHubVisibility string `json:"github_visibility,omitempty"` // private | public
	DryRun           bool   `json:"dry_run,omitempty"`
}

// ProjectResponse is the result of create or import.
type ProjectResponse struct {
	Project       string        `json:"project"`
	WebURL        string        `json:"web_url,omitempty"`
	DefaultBranch string        `json:"default_branch,omitempty"`
	SeedCommit    string        `json:"seed_commit,omitempty"`
	SeedPaths     []string      `json:"seed_paths,omitempty"`
	GitHub        *GitHubMirror `json:"github,omitempty"`
	Steps         []StepResult  `json:"steps"`
	DryRun        bool          `json:"dry_run"`
}

func normVisibility(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "":
		return "private", nil
	case "private", "internal", "public":
		return v, nil
	}
	return "", fmt.Errorf("visibility must be private, internal or public (got %q)", v)
}

func validGroupPath(g string) (string, error) {
	g = strings.Trim(strings.TrimSpace(g), "/")
	if g == "" {
		return "", errors.New("group required (e.g. \"services\")")
	}
	for _, s := range strings.Split(g, "/") {
		if !pathSegment.MatchString(s) {
			return "", fmt.Errorf("group segment %q must match %s", s, pathSegment.String())
		}
	}
	return g, nil
}

func validSlug(n string) (string, error) {
	n = strings.TrimSpace(n)
	if !pathSegment.MatchString(n) || strings.Contains(n, "/") {
		return "", fmt.Errorf("name %q must be a lowercase slug matching %s", n, pathSegment.String())
	}
	return n, nil
}

func (d *Domain) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !d.deps.RequireAdminToken(w, r) {
		return
	}
	var req CreateProjectRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, "invalid JSON body", err)
		return
	}
	group, err := validGroupPath(req.Group)
	if err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	name, err := validSlug(req.Name)
	if err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	visibility, err := normVisibility(req.Visibility)
	if err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	template := strings.ToLower(strings.TrimSpace(req.Template))
	if template == "" {
		template = "generic"
	}
	if !validTemplate(template) {
		d.deps.WriteError(w, http.StatusBadRequest, "template must be one of "+strings.Join(SeedTemplates, ", "), nil)
		return
	}
	ghVis := strings.ToLower(strings.TrimSpace(req.GitHubVisibility))
	if ghVis == "" {
		ghVis = "private"
		if visibility == "public" {
			ghVis = "public"
		}
	}
	if ghVis != "private" && ghVis != "public" {
		d.deps.WriteError(w, http.StatusBadRequest, "github_visibility must be private or public", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	resp := ProjectResponse{Project: group + "/" + name, DryRun: req.DryRun, Steps: []StepResult{}}
	step := func(s, status, detail string) {
		resp.Steps = append(resp.Steps, StepResult{Step: s, Status: status, Detail: detail})
	}
	fail := func(status int, s string, err error) {
		step(s, "failed", err.Error())
		d.deps.Logger().Warn("forge: create project failed", "project", resp.Project, "step", s, "err", err)
		d.deps.WriteJSON(w, status, map[string]any{"error": err.Error(), "result": resp})
	}

	gl, err := d.resolver.Credential(ctx, ProviderGitLab)
	if err != nil {
		d.writeProviderError(w, ProviderGitLab, err)
		return
	}
	var gh Credential
	var ghUser string
	if req.MirrorToGitHub {
		gh, err = d.resolver.Credential(ctx, ProviderGitHub)
		if err != nil {
			d.writeProviderError(w, ProviderGitHub, err)
			return
		}
		ghUser, err = d.resolver.Whoami(ctx, gh)
		if err != nil {
			fail(http.StatusBadGateway, "github_identity", err)
			return
		}
	}
	owner := strings.TrimSpace(req.GitHubOwner)
	if owner == "" {
		owner = ghUser
	}

	// Pre-flight: namespace + existence, both sides.
	nsID, err := d.resolver.gitlabNamespaceID(ctx, gl, group)
	if err != nil {
		fail(http.StatusUnprocessableEntity, "gitlab_namespace", err)
		return
	}
	step("gitlab_namespace", "done", fmt.Sprintf("%s (id %d)", group, nsID))
	if existing, ok, err := d.resolver.gitlabProjectByPath(ctx, gl, resp.Project); err != nil {
		fail(http.StatusBadGateway, "gitlab_exists", err)
		return
	} else if ok {
		resp.WebURL = existing.WebURL
		step("gitlab_project", "exists", existing.WebURL)
		d.deps.WriteJSON(w, http.StatusConflict, map[string]any{"error": "project already exists on GitLab", "result": resp})
		return
	}
	var ghExisting *githubRepo
	if req.MirrorToGitHub {
		if g, ok, err := d.resolver.githubRepoByPath(ctx, gh, owner+"/"+name); err != nil {
			fail(http.StatusBadGateway, "github_exists", err)
			return
		} else if ok {
			ghExisting = &g
		}
	}

	if req.DryRun {
		step("gitlab_project", "planned", fmt.Sprintf("create %s (%s, default branch main)", resp.Project, visibility))
		step("seed_commit", "planned", strings.Join(SeedPaths(template, name), ", "))
		if req.MirrorToGitHub {
			if ghExisting != nil {
				step("github_repo", "exists", ghExisting.HTMLURL)
			} else {
				step("github_repo", "planned", fmt.Sprintf("create %s/%s (%s)", owner, name, ghVis))
			}
			step("push_mirror", "planned", fmt.Sprintf("GitLab → https://github.com/%s/%s.git", owner, name))
		}
		resp.SeedPaths = SeedPaths(template, name)
		d.deps.WriteJSON(w, http.StatusOK, resp)
		return
	}

	// 1. GitLab project.
	var created gitlabProject
	if err := d.resolver.do(ctx, gl, http.MethodPost, "/projects", map[string]any{
		"name": name, "path": name, "namespace_id": nsID, "visibility": visibility,
		"default_branch": "main", "initialize_with_readme": false, "description": strings.TrimSpace(req.Description),
	}, &created); err != nil {
		fail(http.StatusBadGateway, "gitlab_project", err)
		return
	}
	if created.PathWithNamespace != "" {
		resp.Project = created.PathWithNamespace
	}
	resp.WebURL = created.WebURL
	resp.DefaultBranch = created.DefaultBranch
	if resp.DefaultBranch == "" {
		resp.DefaultBranch = "main"
	}
	step("gitlab_project", "done", created.WebURL)

	// 2. Seed commit.
	mirrorName := ""
	if req.MirrorToGitHub {
		mirrorName = owner + "/" + name
	}
	var commit struct {
		ID string `json:"id"`
	}
	if err := d.resolver.do(ctx, gl, http.MethodPost, fmt.Sprintf("/projects/%d/repository/commits", created.ID), map[string]any{
		"branch":         resp.DefaultBranch,
		"commit_message": fmt.Sprintf("chore: seed %s from the Loom HUD (Mills intake)", resp.Project),
		"actions":        SeedActions(group, name, req.Description, template, mirrorName),
	}, &commit); err != nil {
		fail(http.StatusBadGateway, "seed_commit", fmt.Errorf("repo was created but is empty; seed it by hand or delete and retry: %w", err))
		return
	}
	resp.SeedCommit = commit.ID
	resp.SeedPaths = SeedPaths(template, name)
	step("seed_commit", "done", commit.ID)

	// 3. GitHub mirror.
	if req.MirrorToGitHub {
		d.ensureGitHubMirror(ctx, gh, gl, created.ID, owner, name, ghVis, strings.TrimSpace(req.Description), created.WebURL, ghExisting, &resp)
	}
	d.deps.Logger().Info("forge: project created", "project", resp.Project, "web_url", resp.WebURL, "mirror", req.MirrorToGitHub)
	d.deps.WriteJSON(w, http.StatusCreated, resp)
}

// ensureGitHubMirror creates the GitHub repo if needed and the GitLab push
// mirror if absent, recording each as a step. Failures here never undo the
// GitLab side — the canonical repo is the durable artifact.
func (d *Domain) ensureGitHubMirror(ctx context.Context, gh, gl Credential, projectID int64, owner, name, ghVis, description, homepage string, existing *githubRepo, resp *ProjectResponse) {
	step := func(s, status, detail string) {
		resp.Steps = append(resp.Steps, StepResult{Step: s, Status: status, Detail: detail})
	}
	full := owner + "/" + name
	mirror := &GitHubMirror{Repo: full}
	if existing != nil {
		mirror.WebURL = existing.HTMLURL
		step("github_repo", "exists", existing.HTMLURL)
	} else {
		me, err := d.resolver.Whoami(ctx, gh)
		if err != nil {
			step("github_repo", "failed", err.Error())
			resp.GitHub = mirror
			return
		}
		path := "/user/repos"
		if !strings.EqualFold(me, owner) {
			path = "/orgs/" + owner + "/repos"
		}
		var g githubRepo
		if err := d.resolver.do(ctx, gh, http.MethodPost, path, map[string]any{
			"name": name, "private": ghVis != "public", "description": description, "homepage": homepage,
			"has_wiki": false, "auto_init": false,
		}, &g); err != nil {
			step("github_repo", "failed", err.Error())
			resp.GitHub = mirror
			return
		}
		mirror.WebURL = g.HTMLURL
		step("github_repo", "done", g.HTMLURL)
	}

	target := fmt.Sprintf("https://github.com/%s.git", full)
	var mirrors []struct {
		ID      int64  `json:"id"`
		URL     string `json:"url"`
		Enabled bool   `json:"enabled"`
	}
	if err := d.resolver.do(ctx, gl, http.MethodGet, fmt.Sprintf("/projects/%d/remote_mirrors", projectID), nil, &mirrors); err != nil {
		step("push_mirror", "failed", err.Error())
		resp.GitHub = mirror
		return
	}
	for _, m := range mirrors {
		if mirrorURLWithoutCreds(m.URL) == target {
			mirror.MirrorID, mirror.MirrorEnabled = m.ID, m.Enabled
			step("push_mirror", "exists", target)
			resp.GitHub = mirror
			return
		}
	}
	// The push mirror authenticates with the GitHub token as the password;
	// GitHub ignores the username for token auth. The credentialed URL is
	// sent to GitLab only and never echoed back (GitLab stores it masked).
	credURL := &url.URL{Scheme: "https", User: url.UserPassword(owner, gh.Token), Host: "github.com", Path: "/" + full + ".git"}
	var m struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	if err := d.resolver.do(ctx, gl, http.MethodPost, fmt.Sprintf("/projects/%d/remote_mirrors", projectID), map[string]any{
		"url": credURL.String(), "enabled": true, "only_protected_branches": false, "keep_divergent_refs": false,
	}, &m); err != nil {
		step("push_mirror", "failed", err.Error())
		resp.GitHub = mirror
		return
	}
	mirror.MirrorID, mirror.MirrorEnabled = m.ID, m.Enabled
	step("push_mirror", "done", target)
	resp.GitHub = mirror
}

// ImportProjectRequest is POST /api/forge/projects/import: bring a GitHub repo
// into GitLab (the canonical forge) and keep GitHub as the push mirror.
type ImportProjectRequest struct {
	GitHubRepo  string `json:"github_repo"` // owner/name
	Group       string `json:"group"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
	MirrorBack  bool   `json:"mirror_back"`
	DryRun      bool   `json:"dry_run,omitempty"`
}

const importPollBudget = 75 * time.Second

func (d *Domain) handleImportProject(w http.ResponseWriter, r *http.Request) {
	if !d.deps.RequireAdminToken(w, r) {
		return
	}
	var req ImportProjectRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, "invalid JSON body", err)
		return
	}
	ownerRepo := strings.Trim(strings.TrimSpace(req.GitHubRepo), "/")
	parts := strings.Split(ownerRepo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		d.deps.WriteError(w, http.StatusBadRequest, "github_repo must be owner/name", nil)
		return
	}
	group, err := validGroupPath(req.Group)
	if err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.ToLower(parts[1])
	}
	if name, err = validSlug(name); err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), importPollBudget+30*time.Second)
	defer cancel()
	resp := ProjectResponse{Project: group + "/" + name, DryRun: req.DryRun, Steps: []StepResult{}}
	step := func(s, status, detail string) {
		resp.Steps = append(resp.Steps, StepResult{Step: s, Status: status, Detail: detail})
	}
	fail := func(status int, s string, err error) {
		step(s, "failed", err.Error())
		d.deps.Logger().Warn("forge: import failed", "project", resp.Project, "step", s, "err", err)
		d.deps.WriteJSON(w, status, map[string]any{"error": err.Error(), "result": resp})
	}

	gl, err := d.resolver.Credential(ctx, ProviderGitLab)
	if err != nil {
		d.writeProviderError(w, ProviderGitLab, err)
		return
	}
	gh, err := d.resolver.Credential(ctx, ProviderGitHub)
	if err != nil {
		d.writeProviderError(w, ProviderGitHub, err)
		return
	}
	src, ok, err := d.resolver.githubRepoByPath(ctx, gh, ownerRepo)
	if err != nil {
		fail(http.StatusBadGateway, "github_source", err)
		return
	}
	if !ok {
		fail(http.StatusNotFound, "github_source", fmt.Errorf("github repo %q not found (or the token cannot see it)", ownerRepo))
		return
	}
	step("github_source", "done", src.HTMLURL)
	visibility, err := normVisibility(req.Visibility)
	if err != nil {
		d.deps.WriteError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if req.Visibility == "" && !src.Private {
		visibility = "public"
	}
	nsID, err := d.resolver.gitlabNamespaceID(ctx, gl, group)
	if err != nil {
		fail(http.StatusUnprocessableEntity, "gitlab_namespace", err)
		return
	}
	step("gitlab_namespace", "done", fmt.Sprintf("%s (id %d)", group, nsID))
	if existing, ok, err := d.resolver.gitlabProjectByPath(ctx, gl, resp.Project); err != nil {
		fail(http.StatusBadGateway, "gitlab_exists", err)
		return
	} else if ok {
		resp.WebURL = existing.WebURL
		step("gitlab_project", "exists", existing.WebURL)
		d.deps.WriteJSON(w, http.StatusConflict, map[string]any{"error": "project already exists on GitLab", "result": resp})
		return
	}
	if req.DryRun {
		step("gitlab_import", "planned", fmt.Sprintf("import %s into %s (%s)", src.HTMLURL, resp.Project, visibility))
		if req.MirrorBack {
			step("push_mirror", "planned", fmt.Sprintf("GitLab → %s", src.HTMLURL))
		}
		d.deps.WriteJSON(w, http.StatusOK, resp)
		return
	}

	// Import: GitLab pulls from GitHub with the token as the password.
	me, err := d.resolver.Whoami(ctx, gh)
	if err != nil {
		fail(http.StatusBadGateway, "github_identity", err)
		return
	}
	importURL := &url.URL{Scheme: "https", User: url.UserPassword(me, gh.Token), Host: "github.com", Path: "/" + ownerRepo + ".git"}
	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		desc = src.Description
	}
	var created gitlabProject
	if err := d.resolver.do(ctx, gl, http.MethodPost, "/projects", map[string]any{
		"name": name, "path": name, "namespace_id": nsID, "visibility": visibility,
		"import_url": importURL.String(), "description": desc,
	}, &created); err != nil {
		fail(http.StatusBadGateway, "gitlab_import", err)
		return
	}
	if created.PathWithNamespace != "" {
		resp.Project = created.PathWithNamespace
	}
	resp.WebURL = created.WebURL
	step("gitlab_project", "done", created.WebURL)

	// Poll the import to a terminal state within budget.
	deadline := time.Now().Add(importPollBudget)
	status := created.ImportStatus
	for status != "finished" && status != "failed" && status != "none" && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			status = "timeout"
		case <-time.After(3 * time.Second):
		}
		if status == "timeout" {
			break
		}
		var p gitlabProject
		if err := d.resolver.do(ctx, gl, http.MethodGet, fmt.Sprintf("/projects/%d", created.ID), nil, &p); err != nil {
			continue
		}
		status, created.DefaultBranch, created.ImportError = p.ImportStatus, p.DefaultBranch, p.ImportError
	}
	resp.DefaultBranch = created.DefaultBranch
	switch status {
	case "finished", "none":
		step("gitlab_import", "done", "import finished")
	case "failed":
		step("gitlab_import", "failed", created.ImportError)
	default:
		step("gitlab_import", "skipped", "import still running on GitLab; the mirror is set up once it finishes — check the project page")
	}

	if req.MirrorBack {
		vis := "private"
		if !src.Private {
			vis = "public"
		}
		d.ensureGitHubMirror(ctx, gh, gl, created.ID, parts[0], parts[1], vis, desc, created.WebURL, &src, &resp)
	}
	d.deps.Logger().Info("forge: project imported", "project", resp.Project, "from", ownerRepo, "status", status)
	d.deps.WriteJSON(w, http.StatusCreated, resp)
}

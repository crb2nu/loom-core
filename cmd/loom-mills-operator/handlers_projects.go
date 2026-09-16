package main

// handlers_projects.go -- the Mills project registry and repo onboarding.
//
//   GET  /api/mills/projects            every project Mills knows, with a
//                                       readiness verdict and named blockers
//   POST /api/mills/projects/onboard    register an existing GitLab repo at
//                                       runtime (bootstrapped_projects) — the
//                                       "live now" half of onboarding
//   POST /api/mills/projects/policy-mr  open the gitops MR that writes the
//                                       Git-policy half (demand list, issue
//                                       intake, protected paths, budget caps,
//                                       checksum bump) — "after restart"
//
// Readiness is computed from the SAME policy accessors the reconciler and
// the gates use (CrossRepo.*, ResolveProtectedPaths, PerRepoOverrides), so
// the HUD's verdict can never disagree with what a run would experience.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Project source tags. A project may carry several.
const (
	projectSourceHome         = "home"
	projectSourceDemand       = "demand_projects"
	projectSourceIntake       = "intake_issues"
	projectSourceBootstrapped = "bootstrapped"
)

// Readiness blockers — the reasons a foreign repo cannot be woven right now.
const (
	blockerCrossRepoDisabled     = "cross_repo_disabled"
	blockerNotInDemand           = "not_in_demand"
	blockerAllowBootstrappedOff  = "allow_bootstrapped_off"
	blockerProtectedPathsUnknown = "protected_paths_unknown"
	pendingGitPolicyMissing      = "git_policy_missing"
	onboardCreatedBy             = "hud:intake"
	projectsPolicyRequestBudget  = 30 * time.Second
	projectsOnboardRequestBudget = 20 * time.Second
	defaultGitOpsDeploymentPath  = "k3s/mills/deployment.yaml"
	onboardMRBranchPrefix        = "mills/onboard-"
)

// projectEntry is one row of the registry.
type projectEntry struct {
	Project string   `json:"project"`
	Sources []string `json:"sources"`
	WebURL  string   `json:"web_url,omitempty"`
	// ProtectedPaths is "per_repo" (an explicit overlay), "global" (the
	// repo is known and inherits the global list) or "unknown" (fail-closed
	// match-all: every touched file counts as protected).
	ProtectedPaths     string                     `json:"protected_paths"`
	ProtectedPathCount int                        `json:"protected_path_count"`
	MaxUSDPerRun       float64                    `json:"max_usd_per_run,omitempty"`
	MaxRunsPerDay      int                        `json:"max_runs_per_day,omitempty"`
	Registered         *store.BootstrappedProject `json:"registered,omitempty"`
	Ready              bool                       `json:"ready"`
	Blockers           []string                   `json:"blockers"`
	// Pending names things that work at runtime but are not yet in Git
	// policy (e.g. a registered repo missing from demand_projects).
	Pending []string `json:"pending"`
}

// projectsRegistryResponse is GET /api/mills/projects.
type projectsRegistryResponse struct {
	HomeProject            string         `json:"home_project"`
	CrossRepoEnabled       bool           `json:"cross_repo_enabled"`
	AllowBootstrapped      bool           `json:"allow_bootstrapped"`
	BootstrapAllowedGroups []string       `json:"bootstrap_allowed_groups"`
	Onboardable            bool           `json:"onboardable"`
	PolicyMRAvailable      bool           `json:"policy_mr_available"`
	Projects               []projectEntry `json:"projects"`
	Count                  int            `json:"count"`
}

var projectPathSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// validProjectPath normalizes "group[/subgroup]/slug" and rejects anything
// that is not at least group/slug of safe segments.
func validProjectPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", errors.New("project required (e.g. \"services/procmodel\")")
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("project %q must include a group (e.g. \"services/%s\")", p, p)
	}
	for _, s := range segs {
		if !projectPathSegment.MatchString(s) {
			return "", fmt.Errorf("project segment %q must match %s", s, projectPathSegment.String())
		}
	}
	return strings.Join(segs, "/"), nil
}

func (o *operator) homeProject() string {
	if o.reconciler != nil {
		return o.reconciler.HomeProject
	}
	return ""
}

// projectRegistry assembles the registry + readiness from policy and store.
func (o *operator) projectRegistry(ctx context.Context) (projectsRegistryResponse, error) {
	pol := o.policy.Current()
	home := o.homeProject()
	entries := map[string]*projectEntry{}
	order := []string{}
	add := func(project, source string) *projectEntry {
		key := strings.Trim(strings.TrimSpace(project), "/")
		if key == "" {
			return nil
		}
		e := entries[key]
		if e == nil {
			e = &projectEntry{Project: key, Blockers: []string{}, Pending: []string{}}
			entries[key] = e
			order = append(order, key)
		}
		for _, s := range e.Sources {
			if s == source {
				return e
			}
		}
		e.Sources = append(e.Sources, source)
		return e
	}
	if home != "" {
		add(home, projectSourceHome)
	}
	for _, p := range pol.CrossRepo.DemandProjects {
		add(p, projectSourceDemand)
	}
	for _, p := range pol.Intake.GitLab.Projects {
		add(p, projectSourceIntake)
	}
	var rows []*store.BootstrappedProject
	if o.store != nil && o.store.Bootstrap != nil {
		var err error
		rows, err = o.store.Bootstrap.List(ctx)
		if err != nil {
			return projectsRegistryResponse{}, fmt.Errorf("list registry: %w", err)
		}
	}
	for _, r := range rows {
		if e := add(r.Project, projectSourceBootstrapped); e != nil {
			e.Registered = r
			if e.WebURL == "" {
				e.WebURL = r.WebURL
			}
		}
	}

	inDemand := func(p string) bool {
		for _, d := range pol.CrossRepo.DemandProjects {
			if store.SameRepo(d, p) {
				return true
			}
		}
		return false
	}

	for _, key := range order {
		e := entries[key]
		isHome := home != "" && store.SameRepo(home, key)

		// Protected-path posture, from the same resolver the gates use.
		e.ProtectedPaths = "unknown"
		for overlayKey, overlay := range pol.Pipeline.ProtectedPathsPerRepo {
			if store.SameRepo(overlayKey, key) {
				e.ProtectedPaths = "per_repo"
				e.ProtectedPathCount = len(overlay)
				break
			}
		}
		if e.ProtectedPaths == "unknown" {
			target := key
			if isHome {
				target = ""
			}
			if paths, err := pol.ResolveProtectedPaths(target); err == nil {
				e.ProtectedPaths = "global"
				e.ProtectedPathCount = len(paths)
			}
		}
		for ovKey, ov := range pol.Pipeline.PerRepoOverrides {
			if store.SameRepo(ovKey, key) {
				e.MaxUSDPerRun = ov.MaxUSDPerRun
				e.MaxRunsPerDay = ov.MaxRunsPerDay
				break
			}
		}

		if !isHome {
			if !pol.CrossRepo.Enabled {
				e.Blockers = append(e.Blockers, blockerCrossRepoDisabled)
			}
			switch {
			case inDemand(key):
			case e.Registered != nil && pol.CrossRepo.AllowBootstrapped:
				e.Pending = append(e.Pending, pendingGitPolicyMissing)
			case e.Registered != nil:
				e.Blockers = append(e.Blockers, blockerAllowBootstrappedOff)
			default:
				e.Blockers = append(e.Blockers, blockerNotInDemand)
			}
			if e.ProtectedPaths == "unknown" {
				e.Blockers = append(e.Blockers, blockerProtectedPathsUnknown)
			}
		}
		e.Ready = len(e.Blockers) == 0
	}

	out := make([]projectEntry, 0, len(order))
	for _, key := range order {
		out = append(out, *entries[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		hi := home != "" && store.SameRepo(home, out[i].Project)
		hj := home != "" && store.SameRepo(home, out[j].Project)
		if hi != hj {
			return hi
		}
		if out[i].Ready != out[j].Ready {
			return out[i].Ready
		}
		return out[i].Project < out[j].Project
	})
	groups := pol.CrossRepoBootstrapAllowedGroups()
	if groups == nil {
		groups = []string{}
	}
	return projectsRegistryResponse{
		HomeProject:            home,
		CrossRepoEnabled:       pol.CrossRepo.Enabled,
		AllowBootstrapped:      pol.CrossRepo.AllowBootstrapped,
		BootstrapAllowedGroups: groups,
		Onboardable:            o.store != nil && o.store.Bootstrap != nil,
		PolicyMRAvailable:      o.gitopsClient != nil,
		Projects:               out,
		Count:                  len(out),
	}, nil
}

// handleProjectsList is GET /api/mills/projects (open read).
func (o *operator) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	reg, err := o.projectRegistry(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, reg)
}

// onboardRequest is POST /api/mills/projects/onboard.
type onboardRequest struct {
	Project string `json:"project"`
	WebURL  string `json:"web_url,omitempty"`
	// Note is provenance recorded on the registry row (where a mint records
	// its plan id), e.g. "onboarded by cody via HUD".
	Note string `json:"note,omitempty"`
}

// handleProjectOnboard registers an existing GitLab repo at runtime. When the
// GitLab client is wired the repo must exist (404 otherwise); the registry
// row is insert-once (409 on a repeat, with the current entry).
func (o *operator) handleProjectOnboard(w http.ResponseWriter, r *http.Request) {
	if o.store == nil || o.store.Bootstrap == nil {
		http.Error(w, "project registry not configured", http.StatusServiceUnavailable)
		return
	}
	var req onboardRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	project, err := validProjectPath(req.Project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), projectsOnboardRequestBudget)
	defer cancel()

	webURL := strings.TrimSpace(req.WebURL)
	if o.bootstrapper != nil && o.bootstrapper.GitLab != nil {
		exists, url, err := o.bootstrapper.GitLab.ProjectExists(ctx, project)
		if err != nil {
			http.Error(w, "gitlab lookup: "+err.Error(), http.StatusBadGateway)
			return
		}
		if !exists {
			http.Error(w, fmt.Sprintf("project %q not found on GitLab (or the operator token cannot see it)", project), http.StatusNotFound)
			return
		}
		if url != "" {
			webURL = url
		}
	}
	note := strings.TrimSpace(req.Note)
	if note == "" {
		note = onboardCreatedBy
	}
	insertErr := o.store.Bootstrap.Insert(ctx, &store.BootstrappedProject{
		Project:   project,
		PlanID:    note,
		WebURL:    webURL,
		CreatedBy: onboardCreatedBy,
		CreatedAt: time.Now().UTC(),
	})
	if insertErr != nil && !errors.Is(insertErr, store.ErrAlreadyBootstrapped) {
		http.Error(w, "register project: "+insertErr.Error(), http.StatusInternalServerError)
		return
	}
	reg, err := o.projectRegistry(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusCreated
	if errors.Is(insertErr, store.ErrAlreadyBootstrapped) {
		status = http.StatusConflict
	}
	for _, e := range reg.Projects {
		if store.SameRepo(e.Project, project) {
			if status == http.StatusCreated {
				o.logger.Info("project onboarded (runtime registry)", "project", project, "ready", e.Ready, "blockers", e.Blockers)
			}
			writeJSON(w, status, e)
			return
		}
	}
	http.Error(w, "registered but not visible in registry", http.StatusInternalServerError)
}

// onboardPolicyRequest is POST /api/mills/projects/policy-mr.
type onboardPolicyRequest struct {
	Project        string   `json:"project"`
	IntakeIssues   bool     `json:"intake_issues,omitempty"`
	ProtectedPaths []string `json:"protected_paths,omitempty"`
	MaxUSDPerRun   float64  `json:"max_usd_per_run,omitempty"`
	MaxRunsPerDay  int      `json:"max_runs_per_day,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

// onboardPolicyResponse links the MR the HUD shows.
type onboardPolicyResponse struct {
	Changed         bool     `json:"changed"`
	Edited          []string `json:"edited"`
	Skipped         []string `json:"skipped"`
	MRURL           string   `json:"mr_url,omitempty"`
	MRIID           int64    `json:"mr_iid,omitempty"`
	Branch          string   `json:"branch,omitempty"`
	Checksum        string   `json:"checksum,omitempty"`
	RestartRequired bool     `json:"restart_required"`
	Message         string   `json:"message"`
}

// handleProjectPolicyMR opens the gitops MR that writes a repo's Git-policy
// admission. Never writes the live ConfigMap — Flux owns it.
func (o *operator) handleProjectPolicyMR(w http.ResponseWriter, r *http.Request) {
	if o.gitopsClient == nil {
		http.Error(w, "gitops committer not configured (GITOPS_GITLAB_TOKEN/GITOPS_GITLAB_PROJECT unset)", http.StatusServiceUnavailable)
		return
	}
	var req onboardPolicyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	project, err := validProjectPath(req.Project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.MaxUSDPerRun < 0 || req.MaxRunsPerDay < 0 {
		http.Error(w, "budget caps must be >= 0", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), projectsPolicyRequestBudget)
	defer cancel()

	branch := o.gitopsDefaultBranch
	policyPath := o.gitopsPolicyPath
	deploymentPath := o.gitopsDeploymentPath
	if deploymentPath == "" {
		deploymentPath = defaultGitOpsDeploymentPath
	}

	policyContent, err := o.gitopsClient.GetRawFile(ctx, policyPath, branch)
	if err != nil {
		http.Error(w, "read gitops policy file: "+err.Error(), http.StatusBadGateway)
		return
	}
	note := fmt.Sprintf("%s: %s onboarded from the HUD (Mills intake)", time.Now().UTC().Format("2006-01-02"), project)
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		note += " — " + reason
	}
	edit, err := applyOnboardPolicyEdit(policyContent, onboardPolicyEdit{
		Project:        project,
		IntakeIssues:   req.IntakeIssues,
		ProtectedPaths: req.ProtectedPaths,
		MaxUSDPerRun:   req.MaxUSDPerRun,
		MaxRunsPerDay:  req.MaxRunsPerDay,
		Note:           note,
	})
	if err != nil {
		if errors.Is(err, errPolicyAnchor) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(edit.Changed) == 0 {
		writeJSON(w, http.StatusOK, onboardPolicyResponse{
			Changed: false, Edited: []string{}, Skipped: edit.Skipped,
			Message: fmt.Sprintf("%s is already in every requested policy block; no MR opened", project),
		})
		return
	}

	deployment, err := o.gitopsClient.GetRawFile(ctx, deploymentPath, branch)
	if err != nil {
		http.Error(w, "read gitops deployment file: "+err.Error(), http.StatusBadGateway)
		return
	}
	newDeployment, checksum, err := bumpPolicyChecksum(deployment, edit.Content)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	mrBranch := onboardMRBranchPrefix + strings.ReplaceAll(project, "/", "-") + "-" + stamp
	commitMsg := fmt.Sprintf("mills: onboard %s (%s) [HUD]", project, strings.Join(edit.Changed, ", "))
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		commitMsg += "\n\n" + reason
	}
	actions := []clients.CommitAction{{Action: "update", FilePath: policyPath, Content: edit.Content}}
	if newDeployment != deployment {
		actions = append(actions, clients.CommitAction{Action: "update", FilePath: deploymentPath, Content: newDeployment})
	}
	if _, err := o.gitopsClient.CreateCommit(ctx, clients.CreateCommitRequest{
		Branch: mrBranch, StartBranch: branch, CommitMessage: commitMsg, Actions: actions,
	}); err != nil {
		http.Error(w, "gitops commit: "+err.Error(), http.StatusBadGateway)
		return
	}
	mr, err := o.gitopsClient.CreateMR(ctx, pipeline.CreateMRRequest{
		SourceBranch: mrBranch,
		TargetBranch: branch,
		Title:        fmt.Sprintf("Mills intake: onboard %s", project),
		Description:  onboardMRDescription(project, edit, checksum, req),
	})
	if err != nil {
		http.Error(w, "gitops merge request: "+err.Error(), http.StatusBadGateway)
		return
	}
	o.logger.Info("project onboarding MR opened", "project", project, "mr", mr.URL, "edited", edit.Changed)
	writeJSON(w, http.StatusOK, onboardPolicyResponse{
		Changed:         true,
		Edited:          edit.Changed,
		Skipped:         edit.Skipped,
		MRURL:           mr.URL,
		MRIID:           mr.MRIID,
		Branch:          mrBranch,
		Checksum:        checksum,
		RestartRequired: true,
		Message:         fmt.Sprintf("opened MR to onboard %s; merge + Flux reconcile applies it, the operator restarts on the checksum bump", project),
	})
}

func onboardMRDescription(project string, edit policyEditResult, checksum string, req onboardPolicyRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Automated GitOps MR from the Loom HUD (Mills intake) to onboard **%s**.\n\n", project)
	b.WriteString("Edits:\n")
	for _, c := range edit.Changed {
		fmt.Fprintf(&b, "- `%s`\n", c)
	}
	for _, s := range edit.Skipped {
		fmt.Fprintf(&b, "- `%s` — already present, untouched\n", s)
	}
	if len(req.ProtectedPaths) > 0 {
		fmt.Fprintf(&b, "\nProtected paths: `%s`\n", strings.Join(req.ProtectedPaths, "`, `"))
	}
	if req.MaxUSDPerRun > 0 || req.MaxRunsPerDay > 0 {
		fmt.Fprintf(&b, "Budget caps: max_usd_per_run=%s, max_runs_per_day=%d\n", trimFloat(req.MaxUSDPerRun), req.MaxRunsPerDay)
	}
	fmt.Fprintf(&b, "\nDeployment `policy-checksum` bumped to `%s` so the operator Recreates and re-reads the snapshot-at-start keys (`cross_repo.demand_projects`, `intake.gitlab.projects`).\n", checksum)
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		fmt.Fprintf(&b, "\n**Reason:** %s\n", reason)
	}
	b.WriteString("\n_Origin: POST /api/mills/projects/policy-mr (HUD admin)._\n")
	return b.String()
}

// cachedRuntimeProjects adapts the registry to mills.SetRuntimeKnownProjects
// with a short TTL so ResolveProtectedPaths never hits SQLite per call.
type cachedRuntimeProjects struct {
	list func(context.Context) ([]*store.BootstrappedProject, error)
	ttl  time.Duration

	mu     sync.Mutex
	at     time.Time
	cached []string
}

func newCachedRuntimeProjects(list func(context.Context) ([]*store.BootstrappedProject, error), ttl time.Duration) *cachedRuntimeProjects {
	return &cachedRuntimeProjects{list: list, ttl: ttl}
}

// Projects returns the registered project paths, refreshed at most every ttl.
// A store error keeps the last-good list (never widens or narrows a gate on a
// transient failure).
func (c *cachedRuntimeProjects) Projects() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < c.ttl && c.cached != nil {
		return c.cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.list(ctx)
	if err != nil {
		if c.cached == nil {
			return []string{}
		}
		return c.cached
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Project)
	}
	c.cached = out
	c.at = time.Now()
	return out
}

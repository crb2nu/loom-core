// Package bootstrap mints a new GitLab project from a Spinning Room plan.
//
// A spin (or the plans compare/merge editor) can author a draft plan for a
// product that has no repo yet — the plan lands in the Plan Store with
// project="" and the plan-slice emitter can never source it. Bootstrap closes
// that gap in one admin action: create the GitLab project with the operator's
// group token, seed an initial commit (README from the plan's spec_doc plus a
// self-contained green CI skeleton), record the project in the store's
// bootstrapped_projects registry, and re-scope the plan onto the new path so
// demand can see it. The plan's lifecycle is untouched: the operator still
// reviews the draft and advances it to "planned" before the emitter dispatches
// its slices.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// createdBy is the attribution stamped on registry rows and the seed commit.
const createdBy = "mills:project-bootstrap"

// planStore is the Plan Store surface Bootstrap needs, satisfied by
// *clients.PlanClient. Narrow so tests fake it without an MCP hub.
type planStore interface {
	GetPlan(ctx context.Context, planID string) (clients.PlanDetail, error)
	RescopePlan(ctx context.Context, planID, project, namespace string) error
}

// gitlabAPI is the GitLab surface Bootstrap needs, satisfied by
// *clients.GitLabClient (CreateCommitIn is provided by the adapter below so
// the ForProject re-scope stays out of the service logic).
type gitlabAPI interface {
	LookupNamespaceID(ctx context.Context, fullPath string) (int64, error)
	CreateProject(ctx context.Context, req clients.CreateProjectRequest) (clients.CreateProjectResponse, error)
	CreateCommitIn(ctx context.Context, project string, req clients.CreateCommitRequest) (clients.CreateCommitResponse, error)
	// ProjectExists reports whether a project already exists (404 → false).
	// EnsureRepo uses it as the pre-flight check that turns a missing-repo
	// handoff into a mint instead of a clone-time escalation.
	ProjectExists(ctx context.Context, project string) (bool, string, error)
}

// GitLabAdapter adapts *clients.GitLabClient to gitlabAPI: CreateCommitIn
// re-scopes the client to the freshly-minted project for the seed commit.
type GitLabAdapter struct{ Client *clients.GitLabClient }

func (a GitLabAdapter) LookupNamespaceID(ctx context.Context, fullPath string) (int64, error) {
	return a.Client.LookupNamespaceID(ctx, fullPath)
}

func (a GitLabAdapter) ProjectExists(ctx context.Context, project string) (bool, string, error) {
	return a.Client.ProjectExists(ctx, project)
}

func (a GitLabAdapter) CreateProject(ctx context.Context, req clients.CreateProjectRequest) (clients.CreateProjectResponse, error) {
	return a.Client.CreateProject(ctx, req)
}

func (a GitLabAdapter) CreateCommitIn(ctx context.Context, project string, req clients.CreateCommitRequest) (clients.CreateCommitResponse, error) {
	return a.Client.ForProject(project).CreateCommit(ctx, req)
}

// Service wires the bootstrap flow's dependencies.
type Service struct {
	GitLab gitlabAPI
	Plans  planStore
	Store  *store.BootstrapDAO
	// Namespace is the plan-slice emitter's namespace gate; the plan is
	// re-scoped into it so the emitter's foreign-project pass (which filters
	// by the HOME namespace, the S6 gotcha) can see the plan.
	Namespace string
	// GroupAllowed, when non-nil, gates *where* a repo may be minted: both
	// Bootstrap and EnsureRepo refuse (ErrGroupNotAllowed) to create a repo
	// under a group it rejects. The operator wires it to the policy's
	// bootstrap allow-list (Policy.CrossRepoBootstrapGroupAllowed) so autonomous
	// repo creation is bounded to an explicit set of groups. Nil imposes no
	// group restriction — used only by tests that gate elsewhere; production
	// wiring always sets it.
	GroupAllowed func(group string) bool
	Logger       *slog.Logger
}

// Request is one bootstrap ask.
type Request struct {
	// PlanID is the Spinning Room plan the new repo hosts.
	PlanID string
	// Path is the full GitLab project path to mint, e.g. "services/procmodel".
	// The group part must already exist; the leaf is the new project slug.
	Path string
	// Description overrides the GitLab project description (default: the
	// plan's title).
	Description string
	// Visibility is the GitLab visibility ("private" default).
	Visibility string
}

// Result reports the minted project. PlanRescoped=false with a non-empty
// Warning means the repo exists but the plan write failed — the operator can
// re-scope by hand (agent_plan_update) instead of re-minting.
type Result struct {
	Project      string `json:"project"`
	WebURL       string `json:"web_url"`
	PlanID       string `json:"plan_id"`
	Namespace    string `json:"namespace"`
	SeedCommit   string `json:"seed_commit"`
	PlanRescoped bool   `json:"plan_rescoped"`
	Warning      string `json:"warning,omitempty"`
}

// Typed failures the HTTP handler maps onto status codes.
var (
	// ErrInvalidRequest → 400.
	ErrInvalidRequest = errors.New("bootstrap: invalid request")
	// ErrPlanNotBootstrappable → 409: the plan is already scoped to another
	// project, or its phase is past planning.
	ErrPlanNotBootstrappable = errors.New("bootstrap: plan not bootstrappable")
	// ErrAlreadyBootstrapped → 409: the path was minted before.
	ErrAlreadyBootstrapped = store.ErrAlreadyBootstrapped
	// ErrGroupNotAllowed → 403: the target's group is not in the bootstrap
	// allow-list (Policy.CrossRepoBootstrapGroupAllowed). Bounds where
	// autonomous repo creation can land.
	ErrGroupNotAllowed = errors.New("bootstrap: group not allow-listed")
	// errCreateStep wraps failures from mintRepo's namespace-resolve and
	// project-create steps (before the repo has a seed commit). EnsureRepo uses
	// it to distinguish a recoverable create race from a fatal seed failure;
	// unexported because it is an internal control-flow signal, not an API.
	errCreateStep = errors.New("bootstrap: project create step")
)

// ensureRepoFailure exposes classification structurally, avoiding a
// mills→bootstrap import cycle.
type ensureRepoFailure struct {
	code      string
	retryable bool
	cause     error
}

func (e *ensureRepoFailure) Error() string {
	if e == nil || e.cause == nil {
		return "bootstrap: repo ensure failed"
	}
	return e.cause.Error()
}

func (e *ensureRepoFailure) Unwrap() error { return e.cause }

func (e *ensureRepoFailure) FailureCode() string { return e.code }

func (e *ensureRepoFailure) Retryable() bool { return e.retryable }

func newEnsureRepoFailure(code string, err error) error {
	retryable := true
	switch {
	case code == "seed_commit",
		errors.Is(err, ErrInvalidRequest),
		errors.Is(err, ErrGroupNotAllowed),
		errors.Is(err, clients.ErrNamespaceNotFound):
		retryable = false
	default:
		if status, ok := clients.GitLabHTTPStatus(err); ok {
			// Request timeout, rate limiting, and provider 5xx responses may
			// recover without operator action. Other 4xx responses describe a
			// target/auth/configuration problem and are terminal after the
			// create-race reconciliation check has already missed.
			retryable = status == http.StatusRequestTimeout ||
				status == http.StatusTooManyRequests || status >= 500
		}
	}
	return &ensureRepoFailure{code: code, retryable: retryable, cause: err}
}

// pathSegment validates one GitLab path segment (group or project slug).
var pathSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// bootstrappablePhases are the plan lifecycle phases a repo may be minted
// for. Later phases mean work already landed somewhere — minting a repo
// under them re-homes history and is refused.
var bootstrappablePhases = map[string]bool{"draft": true, "planned": true}

// Bootstrap mints the project. Order is deliberate: every fallible read runs
// before the externally-visible create, and the store row lands before the
// plan re-scope so a crash between the two leaves a registry record pointing
// at the repo rather than an unaccounted-for GitLab project.
func (s *Service) Bootstrap(ctx context.Context, req Request) (Result, error) {
	group, slug, err := splitPath(req.Path)
	if err != nil {
		return Result{}, err
	}
	if err := s.checkGroupAllowed(group); err != nil {
		return Result{}, err
	}
	planID := strings.TrimSpace(req.PlanID)
	if planID == "" {
		return Result{}, fmt.Errorf("%w: plan_id required", ErrInvalidRequest)
	}
	fullPath := group + "/" + slug

	// Registry pre-check: a re-mint of the same path is a conflict, not an
	// upsert — the repo already exists on GitLab. ErrNotFound is the happy
	// path; any other store error fails closed before the external create.
	if existing, err := s.Store.Get(ctx, fullPath); err == nil && existing != nil {
		return Result{}, fmt.Errorf("%w: %s (plan %s)", ErrAlreadyBootstrapped, fullPath, existing.PlanID)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Result{}, fmt.Errorf("bootstrap: registry check %s: %w", fullPath, err)
	}

	plan, err := s.Plans.GetPlan(ctx, planID)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrPlanNotBootstrappable, err)
	}
	if p := strings.TrimSpace(plan.Project); p != "" && p != fullPath {
		return Result{}, fmt.Errorf("%w: plan %s is already scoped to %q", ErrPlanNotBootstrappable, planID, p)
	}
	if phase := strings.ToLower(strings.TrimSpace(plan.Phase)); !bootstrappablePhases[phase] {
		return Result{}, fmt.Errorf("%w: plan %s is in phase %q (want draft or planned)", ErrPlanNotBootstrappable, planID, plan.Phase)
	}

	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		desc = strings.TrimSpace(plan.Title)
	}
	minted, webURL, commitID, err := s.mintRepo(ctx, group, slug, desc, req.Visibility, planID,
		func(branch string) clients.CreateCommitRequest { return seedCommit(branch, plan) })
	if err != nil {
		return Result{}, err
	}

	res := Result{
		Project:      minted,
		WebURL:       webURL,
		PlanID:       planID,
		Namespace:    s.Namespace,
		SeedCommit:   commitID,
		PlanRescoped: true,
	}
	if err := s.Plans.RescopePlan(ctx, planID, minted, s.Namespace); err != nil {
		res.PlanRescoped = false
		res.Warning = fmt.Sprintf("repo created but plan re-scope failed (%v); set the plan's project to %q and namespace to %q via agent_plan_update", err, minted, s.Namespace)
		s.logger().Error("bootstrap: plan re-scope failed", "plan_id", planID, "project", minted, "err", err)
	}
	s.logger().Info("bootstrapped project from plan",
		"project", minted, "plan_id", planID, "web_url", webURL, "rescoped", res.PlanRescoped)
	return res, nil
}

// EnsureRepo makes the GitLab repo at project exist so a cross-repo backlog
// item's clone step succeeds. It is the planless twin of Bootstrap: no Plan
// Store read, no plan re-scope — just "create the repo if it is missing". The
// reconciler's admission pre-flight calls it before dispatching a cross-repo
// item whose TargetProject has no repo yet (the new-project handoff case that
// otherwise escalates on a git-clone 404).
//
// Idempotent and safe to call every reconcile tick:
//   - a registry hit or a live GitLab project ⇒ created=false, no external write;
//   - a create race (another reconciler or the endpoint minted it between our
//     checks and our create) ⇒ reconciled to created=false, not a hard error.
//
// reason is recorded in the registry row's plan_id column for traceability —
// typically the backlog item id that triggered the mint. Never deletes or
// overwrites an existing repo.
// SeedPaths satisfies mills.RepoEnsurer: the repo-root files EnsureRepo's root
// commit creates, which the reconciler declares in the minted item's scope.
func (s *Service) SeedPaths() []string { return SeedPaths() }

func (s *Service) EnsureRepo(ctx context.Context, project, reason string) (created bool, webURL string, err error) {
	group, slug, err := splitPath(project)
	if err != nil {
		return false, "", newEnsureRepoFailure("invalid_target", err)
	}
	if err := s.checkGroupAllowed(group); err != nil {
		return false, "", newEnsureRepoFailure("group_not_allowed", err)
	}
	fullPath := group + "/" + slug

	// Registry hit: this path was minted before — nothing to do.
	if existing, err := s.Store.Get(ctx, fullPath); err == nil && existing != nil {
		return false, existing.WebURL, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, "", newEnsureRepoFailure("registry_check",
			fmt.Errorf("bootstrap: registry check %s: %w", fullPath, err))
	}

	// GitLab existence check: the repo may exist out-of-band (created by hand,
	// or by a prior run whose registry insert failed). Treat as success and
	// backfill the registry so the emitter's demand union sources it.
	exists, existingURL, err := s.GitLab.ProjectExists(ctx, fullPath)
	if err != nil {
		return false, "", newEnsureRepoFailure("existence_check",
			fmt.Errorf("bootstrap: existence check %s: %w", fullPath, err))
	}
	if exists {
		s.recordExisting(ctx, fullPath, existingURL, reason)
		return false, existingURL, nil
	}

	minted, mintedURL, _, err := s.mintRepo(ctx, group, slug, "", "", reason,
		func(branch string) clients.CreateCommitRequest { return seedCommitGeneric(branch, slug, reason) })
	if err != nil {
		// A create-step conflict means another writer minted it in the race
		// window: reconcile to idempotent success. A seed-step failure (empty
		// repo) is NOT masked — return it so the item defers and retries.
		if errors.Is(err, errCreateStep) {
			if ok, url2, e2 := s.GitLab.ProjectExists(ctx, fullPath); e2 == nil && ok {
				s.recordExisting(ctx, fullPath, url2, reason)
				return false, url2, nil
			} else if e2 != nil {
				return false, "", newEnsureRepoFailure("project_reconcile",
					fmt.Errorf("bootstrap: reconcile create failure %s: %w", fullPath, e2))
			}
			code := "project_create"
			if errors.Is(err, clients.ErrNamespaceNotFound) {
				code = "namespace_lookup"
			}
			return false, "", newEnsureRepoFailure(code, err)
		}
		return false, "", newEnsureRepoFailure("seed_commit", err)
	}
	s.logger().Info("bootstrap: minted repo for handoff",
		"project", minted, "reason", reason, "web_url", mintedURL)
	return true, mintedURL, nil
}

// mintRepo is the shared create path for both Bootstrap (plan-seeded) and
// EnsureRepo (generic-seeded): resolve the group namespace, create the empty
// project, author the single seed commit, and record the registry row. seed
// receives the created project's default branch so the commit targets the
// right ref. A registry-insert failure after the repo is created is logged,
// not fatal — the repo is the durable artifact; the demand row can be
// backfilled. Returns errCreateStep-wrapped errors from the create step so
// EnsureRepo can distinguish a create race from a fatal seed failure.
func (s *Service) mintRepo(
	ctx context.Context,
	group, slug, desc, visibility, registryPlanID string,
	seed func(defaultBranch string) clients.CreateCommitRequest,
) (minted, webURL, commitID string, err error) {
	fullPath := group + "/" + slug
	nsID, err := s.GitLab.LookupNamespaceID(ctx, group)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: resolve group %q: %w", errCreateStep, group, err)
	}
	proj, err := s.GitLab.CreateProject(ctx, clients.CreateProjectRequest{
		Name:        slug,
		Path:        slug,
		NamespaceID: nsID,
		Description: desc,
		Visibility:  visibility,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("%w: create project %s: %w", errCreateStep, fullPath, err)
	}
	minted = proj.PathWithNamespace
	if minted == "" {
		minted = fullPath
	}

	commit, err := s.GitLab.CreateCommitIn(ctx, minted, seed(proj.DefaultBranch))
	if err != nil {
		// The repo exists but is empty. Surface the failure — an empty repo
		// breaks the pipeline's clone step — rather than pretending success.
		return "", "", "", fmt.Errorf("bootstrap: seed commit in %s (repo was created; seed it manually or delete and retry): %w", minted, err)
	}

	if err := s.Store.Insert(ctx, &store.BootstrappedProject{
		Project:   minted,
		PlanID:    registryPlanID,
		WebURL:    proj.WebURL,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
	}); err != nil && !errors.Is(err, store.ErrAlreadyBootstrapped) {
		// Registry insert failed after the repo was created: log loudly but
		// keep going — the operator can re-run the insert path, and EnsureRepo
		// backfills the row on its next tick via the existence check.
		s.logger().Error("bootstrap: registry insert failed; demand union will miss this project",
			"project", minted, "err", err)
	}
	return minted, proj.WebURL, commit.ID, nil
}

// recordExisting backfills a registry row for a repo that already exists on
// GitLab (found via the existence check) so the emitter's demand union can
// source it. Best-effort: an existing row (ErrAlreadyBootstrapped) or a
// transient store error is logged, not fatal.
func (s *Service) recordExisting(ctx context.Context, project, webURL, reason string) {
	err := s.Store.Insert(ctx, &store.BootstrappedProject{
		Project:   project,
		PlanID:    reason,
		WebURL:    webURL,
		CreatedBy: createdBy,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyBootstrapped) {
		s.logger().Warn("bootstrap: backfill registry row failed", "project", project, "err", err)
	}
}

// checkGroupAllowed enforces the bootstrap group allow-list when a GroupAllowed
// gate is wired. Nil gate imposes no restriction (tests that gate elsewhere);
// production always wires it to Policy.CrossRepoBootstrapGroupAllowed.
func (s *Service) checkGroupAllowed(group string) error {
	if s.GroupAllowed != nil && !s.GroupAllowed(group) {
		return fmt.Errorf("%w: %q", ErrGroupNotAllowed, group)
	}
	return nil
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// splitPath validates and splits "group[/subgroup]/slug" into (group-path,
// slug). At least one group level is required — bootstrap never mints into a
// user's root namespace.
func splitPath(p string) (group, slug string, err error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", "", fmt.Errorf("%w: path required (e.g. \"services/procmodel\")", ErrInvalidRequest)
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", "", fmt.Errorf("%w: path %q must include a group (e.g. \"services/%s\")", ErrInvalidRequest, p, p)
	}
	for _, seg := range segs {
		if !pathSegment.MatchString(seg) {
			return "", "", fmt.Errorf("%w: path segment %q must match %s", ErrInvalidRequest, seg, pathSegment.String())
		}
	}
	return strings.Join(segs[:len(segs)-1], "/"), segs[len(segs)-1], nil
}

package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type classifiedEnsureRepoError interface {
	error
	FailureCode() string
	Retryable() bool
}

func requireEnsureRepoFailure(t *testing.T, err error, code string, retryable bool) {
	t.Helper()
	var classified classifiedEnsureRepoError
	if !errors.As(err, &classified) {
		t.Fatalf("err=%T %v, want classified EnsureRepo failure", err, err)
	}
	if classified.FailureCode() != code || classified.Retryable() != retryable {
		t.Fatalf("classification=(%q,%v), want (%q,%v)",
			classified.FailureCode(), classified.Retryable(), code, retryable)
	}
}

// ensureGitLab returns a fakeGitLab whose create/seed path succeeds for a
// "services/familyforge" mint. Existence defaults to "missing" so EnsureRepo
// takes the create path unless a test overrides it.
func ensureGitLab() *fakeGitLab {
	return &fakeGitLab{
		nsID: 3,
		createResp: clients.CreateProjectResponse{
			ID:                101,
			PathWithNamespace: "services/familyforge",
			WebURL:            "https://gitlab.example/services/familyforge",
			DefaultBranch:     "main",
		},
		commitResp: clients.CreateCommitResponse{ID: "seedsha"},
	}
}

// TestEnsureRepo_MissingCreates: the happy pre-flight — a missing repo in an
// allow-listed group is minted with a generic README + main branch, and the
// registry row lands stamped with the reason (the backlog item id).
func TestEnsureRepo_MissingCreates(t *testing.T) {
	gl := ensureGitLab()
	svc, st := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(g string) bool { return g == "services" }

	created, url, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if !created {
		t.Errorf("created = false, want true")
	}
	if url != "https://gitlab.example/services/familyforge" {
		t.Errorf("url = %q", url)
	}
	if gl.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", gl.createCalls)
	}
	// Seed commit went to main with the four seed files.
	if gl.commitReq.Branch != "main" {
		t.Errorf("seed branch = %q, want main", gl.commitReq.Branch)
	}
	files := map[string]bool{}
	for _, a := range gl.commitReq.Actions {
		files[a.FilePath] = true
	}
	for _, want := range []string{"README.md", ".gitlab-ci.yml", ".gitignore", "AGENTS.md"} {
		if !files[want] {
			t.Errorf("seed missing %s", want)
		}
	}
	// Registry row landed for the demand union, stamped with the reason.
	got, err := st.Bootstrap.Get(context.Background(), "services/familyforge")
	if err != nil || got.PlanID != "item-42" {
		t.Errorf("registry row = %+v err=%v", got, err)
	}
}

// TestEnsureRepo_RegistryHitNoOp: a path already in the registry is an
// idempotent no-op — no GitLab existence check, no create.
func TestEnsureRepo_RegistryHitNoOp(t *testing.T) {
	gl := ensureGitLab()
	svc, st := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(string) bool { return true }
	if err := st.Bootstrap.Insert(context.Background(), &store.BootstrappedProject{
		Project: "services/familyforge", PlanID: "prior", WebURL: "https://gl/prior",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	created, url, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if created {
		t.Errorf("created = true, want false (registry hit)")
	}
	if url != "https://gl/prior" {
		t.Errorf("url = %q, want the registry row's web url", url)
	}
	if gl.createCalls != 0 || len(gl.existsQueried) != 0 {
		t.Errorf("registry hit must not touch GitLab: createCalls=%d existsQueried=%v", gl.createCalls, gl.existsQueried)
	}
}

// TestEnsureRepo_GitLabExistsBackfills: a repo that exists on GitLab but has no
// registry row (created out-of-band, e.g. by hand) is a no-op success that
// backfills the registry so the emitter's demand union can source it.
func TestEnsureRepo_GitLabExistsBackfills(t *testing.T) {
	gl := ensureGitLab()
	gl.existsResp = true
	gl.existsURL = "https://gitlab.example/services/familyforge"
	svc, st := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(string) bool { return true }

	created, url, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if created {
		t.Errorf("created = true, want false (repo already exists)")
	}
	if url != gl.existsURL {
		t.Errorf("url = %q", url)
	}
	if gl.createCalls != 0 {
		t.Errorf("must not create when the repo already exists: createCalls=%d", gl.createCalls)
	}
	// Registry backfilled with the existing repo's URL.
	got, err := st.Bootstrap.Get(context.Background(), "services/familyforge")
	if err != nil || got.WebURL != gl.existsURL {
		t.Errorf("registry backfill = %+v err=%v", got, err)
	}
}

// TestEnsureRepo_GroupNotAllowed: a target whose group is not allow-listed is
// refused before any GitLab or store work.
func TestEnsureRepo_GroupNotAllowed(t *testing.T) {
	gl := ensureGitLab()
	svc, _ := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(g string) bool { return g == "services" }

	_, _, err := svc.EnsureRepo(context.Background(), "rogue/evil", "item-42")
	if !errors.Is(err, ErrGroupNotAllowed) {
		t.Fatalf("err = %v, want ErrGroupNotAllowed", err)
	}
	if gl.createCalls != 0 || gl.lookupCalled || len(gl.existsQueried) != 0 {
		t.Errorf("disallowed group must not touch GitLab")
	}
}

// TestEnsureRepo_CreateRaceIdempotent: another writer mints the repo in the
// window between our existence check (missing) and our create (409-ish). The
// create-step failure is reconciled to idempotent success once a re-check finds
// the repo, and the registry is backfilled.
func TestEnsureRepo_CreateRaceIdempotent(t *testing.T) {
	gl := ensureGitLab()
	gl.createErr = errors.New("path has already been taken")
	// First existence check (pre-flight) → missing; second (post-create
	// recovery) → present.
	calls := 0
	gl.existsFn = func(string) (bool, string, error) {
		calls++
		if calls == 1 {
			return false, "", nil
		}
		return true, "https://gitlab.example/services/familyforge", nil
	}
	svc, st := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(string) bool { return true }

	created, url, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	if err != nil {
		t.Fatalf("EnsureRepo should reconcile a create race, got: %v", err)
	}
	if created {
		t.Errorf("created = true, want false (lost the create race)")
	}
	if url != "https://gitlab.example/services/familyforge" {
		t.Errorf("url = %q", url)
	}
	if _, err := st.Bootstrap.Get(context.Background(), "services/familyforge"); err != nil {
		t.Errorf("registry backfill missing after race: %v", err)
	}
}

// TestEnsureRepo_HiddenProjectConflictIsTerminal covers GitLab's ambiguous
// 404 behavior for private projects: the pre-check cannot see the project, the
// create reports a conflict, and the post-create check still cannot see it.
// Retrying cannot repair token visibility, so the reconciler must park the
// item immediately instead of spinning forever.
func TestEnsureRepo_HiddenProjectConflictIsTerminal(t *testing.T) {
	gl := ensureGitLab()
	gl.createErr = &clients.GitLabHTTPError{
		Method: http.MethodPost, Path: "/projects", StatusCode: http.StatusConflict,
		Body: `{"message":{"path":["has already been taken"]}}`,
	}
	svc, _ := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(string) bool { return true }

	_, _, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	requireEnsureRepoFailure(t, err, "project_create", false)
	if len(gl.existsQueried) != 2 {
		t.Fatalf("existence checks=%d, want pre-create + conflict reconciliation", len(gl.existsQueried))
	}
}

func TestEnsureRepo_CreateProviderFailureIsRetryable(t *testing.T) {
	gl := ensureGitLab()
	gl.createErr = &clients.GitLabHTTPError{
		Method: http.MethodPost, Path: "/projects", StatusCode: http.StatusBadGateway,
		Body: "upstream unavailable",
	}
	svc, _ := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(string) bool { return true }

	_, _, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	requireEnsureRepoFailure(t, err, "project_create", true)
}

func TestEnsureRepo_NamespaceNotFoundIsTerminal(t *testing.T) {
	gl := ensureGitLab()
	gl.nsErr = clients.ErrNamespaceNotFound
	svc, _ := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(string) bool { return true }

	_, _, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	requireEnsureRepoFailure(t, err, "namespace_lookup", false)
}

// TestEnsureRepo_SeedFailureFatal: a create that succeeds but whose seed commit
// fails leaves an EMPTY repo — that must surface as an error (so the item
// defers and retries), NOT be masked as success by the race-recovery path.
func TestEnsureRepo_SeedFailureFatal(t *testing.T) {
	gl := ensureGitLab()
	gl.commitErr = errors.New("boom")
	svc, _ := newSvc(t, gl, draftPlan())
	svc.GroupAllowed = func(string) bool { return true }

	created, _, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	if err == nil || !strings.Contains(err.Error(), "seed commit") {
		t.Fatalf("err = %v, want a fatal seed-commit failure", err)
	}
	requireEnsureRepoFailure(t, err, "seed_commit", false)
	if created {
		t.Errorf("created = true on seed failure; empty repo must not report success")
	}
}

// TestEnsureRepo_NilGateAllows: a nil GroupAllowed imposes no restriction (the
// test-only path; production always wires the policy predicate).
func TestEnsureRepo_NilGateAllows(t *testing.T) {
	gl := ensureGitLab()
	svc, _ := newSvc(t, gl, draftPlan())
	// GroupAllowed left nil.
	created, _, err := svc.EnsureRepo(context.Background(), "services/familyforge", "item-42")
	if err != nil || !created {
		t.Fatalf("nil gate should allow the mint: created=%v err=%v", created, err)
	}
}

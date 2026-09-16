package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// intakePolicy: cross-repo on with one Git-admitted demand repo (flexdeck,
// with an overlay) and the runtime registry allowed.
const intakePolicy = `
version: 2
budgets:
  council:  { max_usd_per_run: 1, max_usd_per_day: 5 }
  pipeline: { max_usd_per_run: 1, max_usd_per_day: 5, max_concurrent_runs: 2 }
council:
  schedule_cron: "0 5 * * *"
  ensemble:
    editor: { name: editor, model: qwen3-8b, backend: flexinfer }
    reviewers:
      - { name: architecture, model: qwen3-8b, backend: flexinfer, lens: architecture }
  artifacts_branch: "council/{date}"
  artifacts_merge_strategy: "fast-merge-loom-only"
pipeline:
  default_template: mills-default-pipeline
  retry: { max_attempts: 3, cooldown_seconds: 60 }
  protected_paths: ["**/*auth*.go"]
  protected_paths_per_repo:
    "services/flexdeck": ["internal/auth/**"]
  per_repo_overrides:
    "services/flexdeck": { max_usd_per_run: 3, max_runs_per_day: 3 }
cross_repo:
  enabled: true
  demand_projects: ["services/flexdeck", "libs/orphan"]
  allow_bootstrapped: true
  bootstrap_allowed_groups: [services]
intake:
  gitlab:
    enabled: true
    projects: ["services/flexdeck"]
`

// newOperatorWithPolicy builds an operator over a temp store with the given
// policy text (mirrors newBootstrapOperator without the bootstrapper).
func newOperatorWithPolicy(t *testing.T, policyText string) (*operator, func()) {
	t.Helper()
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(policyText), 0o644); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(dir, "mills.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	pm, err := mills.NewPolicyManager(context.Background(), policyPath, mills.PolicyManagerOptions{SkipWatch: true})
	if err != nil {
		_ = st.Close()
		t.Fatalf("policy manager: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	op := newOperator(st, pm, mills.NewBudget(pm, mills.NewStoreBudgetReader(st)), logger)
	return op, func() { _ = pm.Close(); _ = st.Close() }
}

func newIntakeOperator(t *testing.T) (*operator, func()) {
	t.Helper()
	op, cleanup := newOperatorWithPolicy(t, intakePolicy)
	// Wire the runtime registry into the resolver exactly as main.go does.
	mills.SetRuntimeKnownProjects(newCachedRuntimeProjects(op.store.Bootstrap.List, 0).Projects)
	return op, func() {
		mills.SetRuntimeKnownProjects(nil)
		cleanup()
	}
}

func projectsList(t *testing.T, op *operator) projectsRegistryResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	op.handleProjectsList(rec, httptest.NewRequest(http.MethodGet, "/api/mills/projects", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var out projectsRegistryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func entryFor(reg projectsRegistryResponse, project string) *projectEntry {
	for i := range reg.Projects {
		if reg.Projects[i].Project == project {
			return &reg.Projects[i]
		}
	}
	return nil
}

func TestProjectsRegistry_ReadinessFromPolicy(t *testing.T) {
	op, cleanup := newIntakeOperator(t)
	defer cleanup()
	reg := projectsList(t, op)
	if !reg.CrossRepoEnabled || !reg.AllowBootstrapped || !reg.Onboardable || reg.PolicyMRAvailable {
		t.Fatalf("flags = %+v", reg)
	}
	fd := entryFor(reg, "services/flexdeck")
	if fd == nil || !fd.Ready || fd.ProtectedPaths != "per_repo" || fd.ProtectedPathCount != 1 || fd.MaxUSDPerRun != 3 {
		t.Fatalf("flexdeck = %+v", fd)
	}
	if strings.Join(fd.Sources, ",") != "demand_projects,intake_issues" {
		t.Fatalf("flexdeck sources = %v", fd.Sources)
	}
	// In the demand list without an overlay: known, inherits the global list.
	orphan := entryFor(reg, "libs/orphan")
	if orphan == nil || !orphan.Ready || orphan.ProtectedPaths != "global" || orphan.ProtectedPathCount != 1 {
		t.Fatalf("orphan = %+v", orphan)
	}
}

func TestProjectOnboard_RegistersAndBecomesReadyAtRuntime(t *testing.T) {
	op, cleanup := newIntakeOperator(t)
	defer cleanup()

	body := `{"project":"labs/newthing","web_url":"https://gl/labs/newthing","note":"onboarded in test"}`
	rec := httptest.NewRecorder()
	op.handleProjectOnboard(rec, httptest.NewRequest(http.MethodPost, "/api/mills/projects/onboard", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("onboard: %d %s", rec.Code, rec.Body.String())
	}
	var e projectEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	// The kill-test: registered at runtime → ready, protected paths GLOBAL
	// (not the fail-closed unknown), pending only the Git-policy half.
	if !e.Ready || e.ProtectedPaths != "global" || len(e.Blockers) != 0 {
		t.Fatalf("entry = %+v", e)
	}
	if strings.Join(e.Pending, ",") != pendingGitPolicyMissing || e.Registered == nil || e.Registered.CreatedBy != onboardCreatedBy || e.Registered.PlanID != "onboarded in test" {
		t.Fatalf("entry pending/registered = %+v", e)
	}
	if e.WebURL != "https://gl/labs/newthing" {
		t.Fatalf("web_url = %q", e.WebURL)
	}
	// The resolver the gates use agrees.
	if paths, err := op.policy.Current().ResolveProtectedPaths("labs/newthing"); err != nil || len(paths) != 1 {
		t.Fatalf("ResolveProtectedPaths = %v, %v", paths, err)
	}

	// Repeat → 409 with the current entry, no duplicate row.
	rec = httptest.NewRecorder()
	op.handleProjectOnboard(rec, httptest.NewRequest(http.MethodPost, "/api/mills/projects/onboard", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("repeat onboard: %d %s", rec.Code, rec.Body.String())
	}
	rows, _ := op.store.Bootstrap.List(context.Background())
	if len(rows) != 1 {
		t.Fatalf("registry rows = %d", len(rows))
	}
}

func TestProjectOnboard_RejectsBadPathsAndUnknownFields(t *testing.T) {
	op, cleanup := newIntakeOperator(t)
	defer cleanup()
	for _, body := range []string{`{"project":"nogroup"}`, `{"project":"Bad/Case"}`, `{"project":"a/b","bogus":1}`, `{}`} {
		rec := httptest.NewRecorder()
		op.handleProjectOnboard(rec, httptest.NewRequest(http.MethodPost, "/api/mills/projects/onboard", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestProjectOnboard_BlockedWhenAllowBootstrappedOff(t *testing.T) {
	op, cleanup := newOperatorWithPolicy(t, strings.Replace(intakePolicy, "allow_bootstrapped: true", "allow_bootstrapped: false", 1))
	defer cleanup()
	mills.SetRuntimeKnownProjects(newCachedRuntimeProjects(op.store.Bootstrap.List, 0).Projects)
	defer mills.SetRuntimeKnownProjects(nil)
	rec := httptest.NewRecorder()
	op.handleProjectOnboard(rec, httptest.NewRequest(http.MethodPost, "/api/mills/projects/onboard", strings.NewReader(`{"project":"labs/x"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("onboard: %d %s", rec.Code, rec.Body.String())
	}
	var e projectEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Ready || strings.Join(e.Blockers, ",") != blockerAllowBootstrappedOff+","+blockerProtectedPathsUnknown {
		t.Fatalf("expected blocked entry, got %+v", e)
	}
}

// fakeGitOpsFiles serves per-path content, unlike the kill-switch fake.
type fakeGitOpsFiles struct {
	files     map[string]string
	commitReq clients.CreateCommitRequest
	mrReq     pipeline.CreateMRRequest
}

func (f *fakeGitOpsFiles) GetRawFile(_ context.Context, path, _ string) (string, error) {
	c, ok := f.files[path]
	if !ok {
		return "", &clients.GitLabHTTPError{Method: "GET", Path: path, StatusCode: 404}
	}
	return c, nil
}
func (f *fakeGitOpsFiles) CreateCommit(_ context.Context, req clients.CreateCommitRequest) (clients.CreateCommitResponse, error) {
	f.commitReq = req
	return clients.CreateCommitResponse{ID: "c0ffee"}, nil
}
func (f *fakeGitOpsFiles) CreateMR(_ context.Context, req pipeline.CreateMRRequest) (pipeline.CreateMRResponse, error) {
	f.mrReq = req
	return pipeline.CreateMRResponse{MRIID: 9, URL: "https://gl/mr/9"}, nil
}

func TestProjectPolicyMR_EditsBothFilesAndOpensMR(t *testing.T) {
	op, cleanup := newIntakeOperator(t)
	defer cleanup()
	dep := "spec:\n  template:\n    metadata:\n      annotations:\n        loom.flexinfer.ai/policy-checksum: " + strings.Repeat("a", 64) + "\n"
	fake := &fakeGitOpsFiles{files: map[string]string{
		"k3s/mills/configmap-policy.yaml": onboardPolicyFixture,
		"k3s/mills/deployment.yaml":       dep,
	}}
	op.withKillSwitch(fake, "", "")

	body := `{"project":"labs/newthing","intake_issues":true,"protected_paths":[".gitlab-ci.yml"],"max_usd_per_run":2,"max_runs_per_day":3,"reason":"pilot"}`
	rec := httptest.NewRecorder()
	op.handleProjectPolicyMR(rec, httptest.NewRequest(http.MethodPost, "/api/mills/projects/policy-mr", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("policy-mr: %d %s", rec.Code, rec.Body.String())
	}
	var out onboardPolicyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Changed || out.MRIID != 9 || !out.RestartRequired || len(out.Edited) != 4 || out.Checksum == strings.Repeat("a", 64) {
		t.Fatalf("response = %+v", out)
	}
	if len(fake.commitReq.Actions) != 2 || fake.commitReq.StartBranch != "main" || !strings.HasPrefix(fake.commitReq.Branch, "mills/onboard-labs-newthing-") {
		t.Fatalf("commit = %+v", fake.commitReq)
	}
	policy := fake.commitReq.Actions[0].Content
	if !strings.Contains(policy, `- "labs/newthing"`) || !strings.Contains(policy, `"labs/newthing":`) || !strings.Contains(policy, "# kill switch") {
		t.Fatalf("policy edit wrong:\n%s", policy)
	}
	if !strings.Contains(fake.commitReq.Actions[1].Content, "policy-checksum: "+out.Checksum) {
		t.Fatalf("deployment checksum not bumped:\n%s", fake.commitReq.Actions[1].Content)
	}
	if fake.mrReq.TargetBranch != "main" || !strings.Contains(fake.mrReq.Title, "labs/newthing") || !strings.Contains(fake.mrReq.Description, "pilot") {
		t.Fatalf("mr = %+v", fake.mrReq)
	}

	// Already-admitted repo → no MR.
	rec = httptest.NewRecorder()
	op.handleProjectPolicyMR(rec, httptest.NewRequest(http.MethodPost, "/api/mills/projects/policy-mr", strings.NewReader(`{"project":"libs/edilint"}`)))
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Changed {
		t.Fatalf("noop expected: %d %+v", rec.Code, out)
	}
}

func TestProjectPolicyMR_503WithoutGitOps(t *testing.T) {
	op, cleanup := newIntakeOperator(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	op.handleProjectPolicyMR(rec, httptest.NewRequest(http.MethodPost, "/api/mills/projects/policy-mr", strings.NewReader(`{"project":"a/b"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestCachedRuntimeProjects_KeepsLastGoodOnError(t *testing.T) {
	calls := 0
	c := newCachedRuntimeProjects(func(context.Context) ([]*store.BootstrappedProject, error) {
		calls++
		if calls == 2 {
			return nil, context.DeadlineExceeded
		}
		return []*store.BootstrappedProject{{Project: "a/b"}}, nil
	}, 0)
	if got := c.Projects(); len(got) != 1 {
		t.Fatalf("first = %v", got)
	}
	if got := c.Projects(); len(got) != 1 || got[0] != "a/b" {
		t.Fatalf("error must keep last-good, got %v", got)
	}
}

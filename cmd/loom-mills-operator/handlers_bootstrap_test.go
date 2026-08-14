package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/bootstrap"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// bootstrapPolicy enables the bootstrap two-key gate and sets the emitter
// namespace the service re-scopes plans into.
const bootstrapPolicy = `
version: 1
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
cross_repo:
  enabled: true
  allow_bootstrapped: true
  bootstrap_allowed_groups: [services]
`

// newBootstrapOperator builds an operator whose policy enables the bootstrap
// gate, optionally wiring a bootstrapper. Mirrors newTestOperator but with a
// custom policy so the two-key gate is live.
func newBootstrapOperator(t *testing.T, wire bool) (*operator, func()) {
	t.Helper()
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(bootstrapPolicy), 0o644); err != nil {
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
	if wire {
		op.bootstrapper = &bootstrap.Service{
			GitLab:    &stubGitLab{},
			Plans:     &stubPlans{plan: clients.PlanDetail{ID: "plan-abc", Title: "Proc", Phase: "draft"}},
			Store:     st.Bootstrap,
			Namespace: "loom-core/mills",
			GroupAllowed: func(group string) bool {
				return pm.Current().CrossRepoBootstrapGroupAllowed(group)
			},
			Logger: logger,
		}
	}
	return op, func() { _ = pm.Close(); _ = st.Close() }
}

// stubGitLab / stubPlans structurally satisfy the bootstrap service's
// unexported dependency interfaces (exported method sets match).
type stubGitLab struct{}

func (stubGitLab) LookupNamespaceID(context.Context, string) (int64, error) { return 3, nil }
func (stubGitLab) CreateProject(_ context.Context, req clients.CreateProjectRequest) (clients.CreateProjectResponse, error) {
	return clients.CreateProjectResponse{
		ID: 99, PathWithNamespace: "services/" + req.Path, WebURL: "https://gl/services/" + req.Path, DefaultBranch: "main",
	}, nil
}
func (stubGitLab) CreateCommitIn(context.Context, string, clients.CreateCommitRequest) (clients.CreateCommitResponse, error) {
	return clients.CreateCommitResponse{ID: "seed1"}, nil
}
func (stubGitLab) ProjectExists(context.Context, string) (bool, string, error) {
	return false, "", nil
}

type stubPlans struct{ plan clients.PlanDetail }

func (s *stubPlans) GetPlan(context.Context, string) (clients.PlanDetail, error) {
	return s.plan, nil
}
func (s *stubPlans) RescopePlan(context.Context, string, string, string) error { return nil }

func TestHandleProjectBootstrap_RequiresAdmin(t *testing.T) {
	op, cleanup := newBootstrapOperator(t, true)
	defer cleanup()
	withAdminToken(t, "secret")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/projects/bootstrap", bytes.NewBufferString("{}"))
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token: got %d want 401", rec.Code)
	}
}

func TestHandleProjectBootstrap_503WhenNotWired(t *testing.T) {
	op, cleanup := newBootstrapOperator(t, false) // policy on, no bootstrapper
	defer cleanup()
	withAdminToken(t, "secret")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/projects/bootstrap", bytes.NewBufferString(`{"plan_id":"p","path":"services/x"}`))
	req.Header.Set("Authorization", "Bearer secret")
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not-wired: got %d want 503; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleProjectBootstrap_503WhenPolicyOff(t *testing.T) {
	op, cleanup := newTestOperator(t) // default policy: no cross_repo keys
	defer cleanup()
	withAdminToken(t, "secret")
	// Wire a bootstrapper so the gate — not the wiring — is what trips.
	op.bootstrapper = &bootstrap.Service{
		GitLab: &stubGitLab{}, Plans: &stubPlans{plan: clients.PlanDetail{ID: "p", Phase: "draft"}},
		Store: op.store.Bootstrap, Namespace: "ns",
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/projects/bootstrap", bytes.NewBufferString(`{"plan_id":"p","path":"services/x"}`))
	req.Header.Set("Authorization", "Bearer secret")
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("policy-off: got %d want 503; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleProjectBootstrap_Success(t *testing.T) {
	op, cleanup := newBootstrapOperator(t, true)
	defer cleanup()
	withAdminToken(t, "secret")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/projects/bootstrap",
		bytes.NewBufferString(`{"plan_id":"plan-abc","path":"services/procmodel"}`))
	req.Header.Set("Authorization", "Bearer secret")
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var res bootstrap.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Project != "services/procmodel" || !res.PlanRescoped {
		t.Errorf("result = %+v", res)
	}
	// The GET list now surfaces the minted project + enabled=true.
	rec = httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/projects/bootstrapped", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: got %d want 200", rec.Code)
	}
	var list struct {
		Projects []store.BootstrappedProject `json:"projects"`
		Count    int                         `json:"count"`
		Enabled  bool                        `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Count != 1 || !list.Enabled || list.Projects[0].Project != "services/procmodel" {
		t.Errorf("list = %+v", list)
	}
}

func TestHandleProjectBootstrap_ForbiddenWhenGroupNotAllowed(t *testing.T) {
	op, cleanup := newBootstrapOperator(t, true) // allow-list is [services]
	defer cleanup()
	withAdminToken(t, "secret")

	rec := httptest.NewRecorder()
	// Target a group that is NOT allow-listed.
	req := httptest.NewRequest(http.MethodPost, "/api/mills/projects/bootstrap",
		bytes.NewBufferString(`{"plan_id":"plan-abc","path":"rogue/evil"}`))
	req.Header.Set("Authorization", "Bearer secret")
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-allow-listed group: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleProjectBootstrap_ConflictOnRemint(t *testing.T) {
	op, cleanup := newBootstrapOperator(t, true)
	defer cleanup()
	withAdminToken(t, "secret")
	if err := op.store.Bootstrap.Insert(context.Background(), &store.BootstrappedProject{
		Project: "services/procmodel", PlanID: "old",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/projects/bootstrap",
		bytes.NewBufferString(`{"plan_id":"plan-abc","path":"services/procmodel"}`))
	req.Header.Set("Authorization", "Bearer secret")
	op.httpMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("remint: got %d want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBootstrappedList_EmptyOpenRead(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/projects/bootstrapped", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200", rec.Code)
	}
	var list struct {
		Projects []store.BootstrappedProject `json:"projects"`
		Count    int                         `json:"count"`
		Enabled  bool                        `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Count != 0 || list.Enabled {
		t.Errorf("empty list = %+v (enabled should be false: not wired + policy off)", list)
	}
}

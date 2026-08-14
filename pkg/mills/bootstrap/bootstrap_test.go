package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// fakeGitLab records calls and serves canned results.
type fakeGitLab struct {
	nsID          int64
	nsErr         error
	createResp    clients.CreateProjectResponse
	createErr     error
	commitResp    clients.CreateCommitResponse
	commitErr     error
	createReq     clients.CreateProjectRequest
	commitProj    string
	commitReq     clients.CreateCommitRequest
	lookupCalled  bool
	createCalls   int
	existsResp    bool
	existsURL     string
	existsErr     error
	existsQueried []string
	existsFn      func(project string) (bool, string, error)
}

func (f *fakeGitLab) LookupNamespaceID(_ context.Context, _ string) (int64, error) {
	f.lookupCalled = true
	return f.nsID, f.nsErr
}

func (f *fakeGitLab) CreateProject(_ context.Context, req clients.CreateProjectRequest) (clients.CreateProjectResponse, error) {
	f.createReq = req
	f.createCalls++
	return f.createResp, f.createErr
}

func (f *fakeGitLab) ProjectExists(_ context.Context, project string) (bool, string, error) {
	f.existsQueried = append(f.existsQueried, project)
	if f.existsFn != nil {
		return f.existsFn(project)
	}
	return f.existsResp, f.existsURL, f.existsErr
}

func (f *fakeGitLab) CreateCommitIn(_ context.Context, project string, req clients.CreateCommitRequest) (clients.CreateCommitResponse, error) {
	f.commitProj = project
	f.commitReq = req
	return f.commitResp, f.commitErr
}

// fakePlans serves a canned plan detail and records the re-scope.
type fakePlans struct {
	plan        clients.PlanDetail
	getErr      error
	rescopeErr  error
	rescopedTo  string
	rescopedNS  string
	rescopeSeen bool
}

func (f *fakePlans) GetPlan(_ context.Context, _ string) (clients.PlanDetail, error) {
	return f.plan, f.getErr
}

func (f *fakePlans) RescopePlan(_ context.Context, _, project, namespace string) error {
	f.rescopeSeen = true
	f.rescopedTo = project
	f.rescopedNS = namespace
	return f.rescopeErr
}

func newSvc(t *testing.T, gl *fakeGitLab, pl *fakePlans) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "mills.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Service{
		GitLab:    gl,
		Plans:     pl,
		Store:     st.Bootstrap,
		Namespace: "loom-core/mills",
	}, st
}

func happyGitLab() *fakeGitLab {
	return &fakeGitLab{
		nsID: 3,
		createResp: clients.CreateProjectResponse{
			ID:                99,
			PathWithNamespace: "services/procmodel",
			WebURL:            "https://gitlab.example/services/procmodel",
			DefaultBranch:     "main",
		},
		commitResp: clients.CreateCommitResponse{ID: "deadbeef"},
	}
}

func draftPlan() *fakePlans {
	return &fakePlans{plan: clients.PlanDetail{
		ID:      "plan-abc",
		Title:   "Business process analyzer",
		Phase:   "draft",
		SpecDoc: "## Merged slices\n- procmodel types",
	}}
}

func TestBootstrap_HappyPath(t *testing.T) {
	gl, pl := happyGitLab(), draftPlan()
	svc, st := newSvc(t, gl, pl)

	res, err := svc.Bootstrap(context.Background(), Request{PlanID: "plan-abc", Path: "services/procmodel"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.Project != "services/procmodel" || !res.PlanRescoped || res.SeedCommit != "deadbeef" {
		t.Errorf("result = %+v", res)
	}
	if res.Namespace != "loom-core/mills" {
		t.Errorf("namespace = %q", res.Namespace)
	}
	// Plan re-scoped to the minted path + emitter namespace.
	if pl.rescopedTo != "services/procmodel" || pl.rescopedNS != "loom-core/mills" {
		t.Errorf("rescope to %q ns %q", pl.rescopedTo, pl.rescopedNS)
	}
	// Seed commit went to the minted project with the four seed files.
	if gl.commitProj != "services/procmodel" {
		t.Errorf("commit project = %q", gl.commitProj)
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
	// Registry row landed for the demand union.
	if got, err := st.Bootstrap.Get(context.Background(), "services/procmodel"); err != nil || got.PlanID != "plan-abc" {
		t.Errorf("registry row = %+v err=%v", got, err)
	}
}

func TestBootstrap_RejectsScopedPlan(t *testing.T) {
	gl := happyGitLab()
	pl := &fakePlans{plan: clients.PlanDetail{ID: "plan-abc", Phase: "draft", Project: "services/existing"}}
	svc, _ := newSvc(t, gl, pl)

	_, err := svc.Bootstrap(context.Background(), Request{PlanID: "plan-abc", Path: "services/procmodel"})
	if !errors.Is(err, ErrPlanNotBootstrappable) {
		t.Fatalf("err = %v, want ErrPlanNotBootstrappable", err)
	}
	// No external create when the plan is already scoped elsewhere.
	if gl.lookupCalled {
		t.Errorf("must not touch GitLab for an already-scoped plan")
	}
}

func TestBootstrap_RejectsAdvancedPhase(t *testing.T) {
	pl := &fakePlans{plan: clients.PlanDetail{ID: "plan-abc", Phase: "in_progress"}}
	svc, _ := newSvc(t, happyGitLab(), pl)

	_, err := svc.Bootstrap(context.Background(), Request{PlanID: "plan-abc", Path: "services/procmodel"})
	if !errors.Is(err, ErrPlanNotBootstrappable) {
		t.Fatalf("err = %v, want ErrPlanNotBootstrappable", err)
	}
}

func TestBootstrap_ConflictOnRemint(t *testing.T) {
	svc, st := newSvc(t, happyGitLab(), draftPlan())
	if err := st.Bootstrap.Insert(context.Background(), &store.BootstrappedProject{
		Project: "services/procmodel", PlanID: "plan-old",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := svc.Bootstrap(context.Background(), Request{PlanID: "plan-abc", Path: "services/procmodel"})
	if !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("err = %v, want ErrAlreadyBootstrapped", err)
	}
}

func TestBootstrap_InvalidPath(t *testing.T) {
	svc, _ := newSvc(t, happyGitLab(), draftPlan())
	for _, bad := range []string{"", "noslash", "services/", "/procmodel", "Services/Proc Model"} {
		if _, err := svc.Bootstrap(context.Background(), Request{PlanID: "plan-abc", Path: bad}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("path %q err = %v, want ErrInvalidRequest", bad, err)
		}
	}
}

func TestBootstrap_SeedCommitFailureIsFatal(t *testing.T) {
	gl := happyGitLab()
	gl.commitErr = errors.New("boom")
	svc, _ := newSvc(t, gl, draftPlan())

	_, err := svc.Bootstrap(context.Background(), Request{PlanID: "plan-abc", Path: "services/procmodel"})
	if err == nil || !strings.Contains(err.Error(), "seed commit") {
		t.Fatalf("err = %v, want seed-commit failure", err)
	}
}

func TestBootstrap_RescopeFailureSurfacesWarning(t *testing.T) {
	gl := happyGitLab()
	pl := draftPlan()
	pl.rescopeErr = errors.New("hub down")
	svc, st := newSvc(t, gl, pl)

	res, err := svc.Bootstrap(context.Background(), Request{PlanID: "plan-abc", Path: "services/procmodel"})
	if err != nil {
		t.Fatalf("bootstrap should not hard-fail on re-scope error: %v", err)
	}
	if res.PlanRescoped || res.Warning == "" {
		t.Errorf("expected PlanRescoped=false + warning, got %+v", res)
	}
	// The registry row still landed — demand can find it once the plan is
	// re-scoped by hand.
	if _, err := st.Bootstrap.Get(context.Background(), "services/procmodel"); err != nil {
		t.Errorf("registry row missing: %v", err)
	}
}

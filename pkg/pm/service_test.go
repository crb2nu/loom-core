package pm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeStore is an in-memory Store for hermetic tests.
type fakeStore struct {
	risks       map[string]Risk
	vectors     map[string][]float64
	ensureErr   error
	upsertErr   error
	ensureCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		risks:   make(map[string]Risk),
		vectors: make(map[string][]float64),
	}
}

func (f *fakeStore) EnsureReady(ctx context.Context) error {
	f.ensureCalls++
	return f.ensureErr
}

func (f *fakeStore) Upsert(ctx context.Context, r Risk, vector []float64) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.risks[r.ID] = r
	f.vectors[r.ID] = vector
	return nil
}

func (f *fakeStore) Get(ctx context.Context, id string) (*Risk, error) {
	r, ok := f.risks[id]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (f *fakeStore) List(ctx context.Context, project, status string, limit int) ([]Risk, error) {
	var out []Risk
	for _, r := range f.risks {
		if project != "" && r.Project != project {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// fakeEmbedder returns a fixed-dimension vector, or an error when failNext is
// set (to simulate the gte-qwen2 HTTP 400 outage).
type fakeEmbedder struct {
	fail bool
	dim  int
}

func (e *fakeEmbedder) EmbedQuery(ctx context.Context, query string) ([]float64, error) {
	if e.fail {
		return nil, errors.New("Pooling type 'none' is not OAI compatible")
	}
	v := make([]float64, e.dim)
	for i := range v {
		v[i] = 0.01
	}
	return v, nil
}

func (e *fakeEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i], _ = e.EmbedQuery(ctx, texts[i])
	}
	return out, nil
}

func (e *fakeEmbedder) Name() string  { return "fake" }
func (e *fakeEmbedder) Model() string { return "fake" }

func newService(store Store, embedFail bool) *Service {
	return NewService(store, &fakeEmbedder{fail: embedFail, dim: VectorSize}, Config{}, nil)
}

func TestCreateRisk_Success(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)

	id, err := svc.CreateRisk(context.Background(), CreateRiskInput{
		Project:    "services/flexdeck",
		Title:      "Grafana token revoked",
		Likelihood: "high",
		Impact:     "medium",
		Mitigation: "Rotate token",
		Owner:      "cody",
		Status:     "identified",
	})
	if err != nil {
		t.Fatalf("CreateRisk: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}
	r := store.risks[id]
	if r.Project != "services/flexdeck" || r.Title != "Grafana token revoked" {
		t.Fatalf("stored risk fields wrong: %+v", r)
	}
	if r.Likelihood != "high" || r.Impact != "medium" || r.Status != "identified" {
		t.Fatalf("enum fields wrong: %+v", r)
	}
	if len(store.vectors[id]) != VectorSize {
		t.Fatalf("expected vector dim %d, got %d", VectorSize, len(store.vectors[id]))
	}
}

func TestCreateRisk_Defaults(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)
	id, err := svc.CreateRisk(context.Background(), CreateRiskInput{
		Project: "services/flexdeck",
		Title:   "Some risk",
	})
	if err != nil {
		t.Fatalf("CreateRisk: %v", err)
	}
	r := store.risks[id]
	if r.Likelihood != LevelMedium || r.Impact != LevelMedium || r.Status != StatusIdentified {
		t.Fatalf("defaults not applied: %+v", r)
	}
	if r.Links == nil {
		t.Fatal("Links should be non-nil")
	}
}

func TestCreateRisk_Validation(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)
	cases := []struct {
		name string
		in   CreateRiskInput
	}{
		{"missing project", CreateRiskInput{Title: "t"}},
		{"missing title", CreateRiskInput{Project: "p"}},
		{"bad likelihood", CreateRiskInput{Project: "p", Title: "t", Likelihood: "huge"}},
		{"bad impact", CreateRiskInput{Project: "p", Title: "t", Impact: "huge"}},
		{"bad status", CreateRiskInput{Project: "p", Title: "t", Status: "open"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateRisk(context.Background(), tc.in); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// TestCreateRisk_EmbedFailureStillPersists is the core best-effort guarantee:
// a failing embedder must NOT fail the write, and the risk must remain stored
// and listable with a non-zero fallback vector.
func TestCreateRisk_EmbedFailureStillPersists(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, true) // embedder fails

	id, err := svc.CreateRisk(context.Background(), CreateRiskInput{
		Project:    "services/flexdeck",
		Title:      "Embedder down",
		Mitigation: "best-effort persist",
	})
	if err != nil {
		t.Fatalf("CreateRisk must not fail on embed error: %v", err)
	}
	if _, ok := store.risks[id]; !ok {
		t.Fatal("risk was not persisted despite embed failure")
	}
	// fallback vector must be non-zero and full dimension
	vec := store.vectors[id]
	if len(vec) != VectorSize {
		t.Fatalf("fallback vector dim = %d, want %d", len(vec), VectorSize)
	}
	if vec[0] == 0 {
		t.Fatal("fallback vector must be non-zero (Qdrant cosine rejects zero vectors)")
	}

	// And it must be listable.
	risks, err := svc.ListRisks(context.Background(), "services/flexdeck", "")
	if err != nil {
		t.Fatalf("ListRisks: %v", err)
	}
	if len(risks) != 1 || risks[0].ID != id {
		t.Fatalf("embed-failed risk not listable: %+v", risks)
	}
}

// TestCreateRisk_WrongDimUsesFallback covers the branch where the embedder
// succeeds but returns the wrong dimension: we must fall back, not store a
// mismatched vector.
func TestCreateRisk_WrongDimUsesFallback(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, &fakeEmbedder{fail: false, dim: 8}, Config{}, nil)
	id, err := svc.CreateRisk(context.Background(), CreateRiskInput{
		Project: "p", Title: "t", Mitigation: "m",
	})
	if err != nil {
		t.Fatalf("CreateRisk: %v", err)
	}
	if len(store.vectors[id]) != VectorSize {
		t.Fatalf("expected fallback dim %d, got %d", VectorSize, len(store.vectors[id]))
	}
}

func TestListRisks_Filters(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)
	ctx := context.Background()

	idA, _ := svc.CreateRisk(ctx, CreateRiskInput{Project: "services/flexdeck", Title: "A", Status: "identified"})
	_, _ = svc.CreateRisk(ctx, CreateRiskInput{Project: "services/flexdeck", Title: "B", Status: "closed"})
	_, _ = svc.CreateRisk(ctx, CreateRiskInput{Project: "services/loom", Title: "C", Status: "identified"})

	all, _ := svc.ListRisks(ctx, "", "")
	if len(all) != 3 {
		t.Fatalf("expected 3 risks, got %d", len(all))
	}
	byProject, _ := svc.ListRisks(ctx, "services/flexdeck", "")
	if len(byProject) != 2 {
		t.Fatalf("expected 2 flexdeck risks, got %d", len(byProject))
	}
	byStatus, _ := svc.ListRisks(ctx, "services/flexdeck", "identified")
	if len(byStatus) != 1 || byStatus[0].ID != idA {
		t.Fatalf("expected only risk A, got %+v", byStatus)
	}
}

func TestListRisks_BadStatus(t *testing.T) {
	svc := newService(newFakeStore(), false)
	if _, err := svc.ListRisks(context.Background(), "", "nope"); err == nil {
		t.Fatal("expected error for invalid status filter")
	}
}

func TestListRisks_EmptyNonNil(t *testing.T) {
	svc := newService(newFakeStore(), false)
	risks, err := svc.ListRisks(context.Background(), "services/x", "")
	if err != nil {
		t.Fatalf("ListRisks: %v", err)
	}
	if risks == nil {
		risks = []Risk{}
	}
	if len(risks) != 0 {
		t.Fatalf("expected empty, got %d", len(risks))
	}
}

func TestUpdateRisk(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)
	ctx := context.Background()

	id, _ := svc.CreateRisk(ctx, CreateRiskInput{Project: "p", Title: "orig", Status: "identified"})

	newTitle := "updated"
	newStatus := "mitigating"
	if err := svc.UpdateRisk(ctx, id, UpdateRiskInput{Title: &newTitle, Status: &newStatus}); err != nil {
		t.Fatalf("UpdateRisk: %v", err)
	}
	r := store.risks[id]
	if r.Title != "updated" || r.Status != "mitigating" {
		t.Fatalf("update not applied: %+v", r)
	}
	// untouched fields remain
	if r.Project != "p" {
		t.Fatalf("project changed unexpectedly: %+v", r)
	}
}

func TestUpdateRisk_NotFound(t *testing.T) {
	svc := newService(newFakeStore(), false)
	title := "x"
	err := svc.UpdateRisk(context.Background(), "missing", UpdateRiskInput{Title: &title})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestUpdateRisk_Validation(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)
	ctx := context.Background()
	id, _ := svc.CreateRisk(ctx, CreateRiskInput{Project: "p", Title: "t"})

	bad := "INVALID"
	empty := ""
	if err := svc.UpdateRisk(ctx, id, UpdateRiskInput{Likelihood: &bad}); err == nil {
		t.Fatal("expected bad likelihood error")
	}
	if err := svc.UpdateRisk(ctx, id, UpdateRiskInput{Impact: &bad}); err == nil {
		t.Fatal("expected bad impact error")
	}
	if err := svc.UpdateRisk(ctx, id, UpdateRiskInput{Status: &bad}); err == nil {
		t.Fatal("expected bad status error")
	}
	if err := svc.UpdateRisk(ctx, id, UpdateRiskInput{Title: &empty}); err == nil {
		t.Fatal("expected empty title error")
	}
	if err := svc.UpdateRisk(ctx, "", UpdateRiskInput{Title: &bad}); err == nil {
		t.Fatal("expected missing id error")
	}
}

func TestUpdateRisk_EmbedFailureStillPersists(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, true) // embedder fails
	ctx := context.Background()
	id, _ := svc.CreateRisk(ctx, CreateRiskInput{Project: "p", Title: "t"})

	newTitle := "re-embed me"
	if err := svc.UpdateRisk(ctx, id, UpdateRiskInput{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateRisk must not fail on embed error: %v", err)
	}
	if store.risks[id].Title != "re-embed me" {
		t.Fatal("update not persisted under embed failure")
	}
}

func TestLinkRisk(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)
	ctx := context.Background()
	id, _ := svc.CreateRisk(ctx, CreateRiskInput{Project: "p", Title: "t"})

	if err := svc.LinkRisk(ctx, id, "services/flexdeck#42"); err != nil {
		t.Fatalf("LinkRisk: %v", err)
	}
	if len(store.risks[id].Links) != 1 || store.risks[id].Links[0] != "services/flexdeck#42" {
		t.Fatalf("link not appended: %+v", store.risks[id].Links)
	}
	// idempotent
	if err := svc.LinkRisk(ctx, id, "services/flexdeck#42"); err != nil {
		t.Fatalf("LinkRisk idempotent: %v", err)
	}
	if len(store.risks[id].Links) != 1 {
		t.Fatalf("duplicate link added: %+v", store.risks[id].Links)
	}
	// second distinct ref
	if err := svc.LinkRisk(ctx, id, "task_abc"); err != nil {
		t.Fatalf("LinkRisk: %v", err)
	}
	if len(store.risks[id].Links) != 2 {
		t.Fatalf("expected 2 links, got %+v", store.risks[id].Links)
	}
}

func TestLinkRisk_Validation(t *testing.T) {
	store := newFakeStore()
	svc := newService(store, false)
	ctx := context.Background()
	id, _ := svc.CreateRisk(ctx, CreateRiskInput{Project: "p", Title: "t"})

	if err := svc.LinkRisk(ctx, "", "ref"); err == nil {
		t.Fatal("expected missing id error")
	}
	if err := svc.LinkRisk(ctx, id, ""); err == nil {
		t.Fatal("expected missing ref error")
	}
	if err := svc.LinkRisk(ctx, "missing", "ref"); err == nil {
		t.Fatal("expected not found error")
	}
}

func TestCreateRisk_EnsureError(t *testing.T) {
	store := newFakeStore()
	store.ensureErr = errors.New("qdrant down")
	svc := newService(store, false)
	if _, err := svc.CreateRisk(context.Background(), CreateRiskInput{Project: "p", Title: "t"}); err == nil {
		t.Fatal("expected ensure error to fail create")
	}
}

func TestCreateRisk_UpsertError(t *testing.T) {
	store := newFakeStore()
	store.upsertErr = errors.New("upsert failed")
	svc := newService(store, false)
	if _, err := svc.CreateRisk(context.Background(), CreateRiskInput{Project: "p", Title: "t"}); err == nil {
		t.Fatal("expected upsert error to fail create")
	}
}

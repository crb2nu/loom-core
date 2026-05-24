package intake

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubClient is an in-memory GitLabIssuesClient for the importer tests.
type stubClient struct {
	issues []clients.IssueListItem
	calls  int
	err    error
}

func (s *stubClient) ListIssues(_ context.Context, opts clients.ListIssuesOpts) ([]clients.IssueListItem, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	// The importer always passes EligibleLabel in opts.Labels; tests
	// rely on this stub returning everything, regardless of label, so a
	// missing label assertion stays the test's responsibility.
	_ = opts
	return append([]clients.IssueListItem(nil), s.issues...), nil
}

// stubStore is an in-memory BacklogStore so tests don't need SQLite.
type stubStore struct {
	mu    sync.Mutex
	items map[string]*store.BacklogItem
}

func newStubStore() *stubStore {
	return &stubStore{items: map[string]*store.BacklogItem{}}
}

func (s *stubStore) Get(_ context.Context, id string) (*store.BacklogItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	// return a shallow copy so callers can't mutate our state
	clone := *it
	return &clone, nil
}

func (s *stubStore) Put(_ context.Context, item *store.BacklogItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *item
	s.items[item.ID] = &clone
	return nil
}

func TestExtractPriority_TableDriven(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		dflt   store.Priority
		want   store.Priority
	}{
		{"no labels → default", nil, store.P2, store.P2},
		{"only mills-eligible → default", []string{"mills-eligible"}, store.P2, store.P2},
		{"priority:P0 wins", []string{"mills-eligible", "priority:P0"}, store.P2, store.P0},
		{"priority:P3 lowest", []string{"priority:P3"}, store.P2, store.P3},
		{"P0 beats P2 same issue", []string{"priority:P2", "priority:P0"}, store.P3, store.P0},
		{"case-insensitive prefix", []string{"PRIORITY:p1"}, store.P2, store.P1},
		{"malformed → default", []string{"priority:URGENT"}, store.P3, store.P3},
		{"whitespace tolerated", []string{"  priority:P1  "}, store.P2, store.P1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPriority(tc.labels, tc.dflt)
			if got != tc.want {
				t.Errorf("extractPriority(%v, %s) = %s, want %s",
					tc.labels, tc.dflt, got, tc.want)
			}
		})
	}
}

func TestIssueToBacklog_MapsFields(t *testing.T) {
	issue := clients.IssueListItem{
		IID:         42,
		ProjectID:   47,
		Title:       "Fix flaky test in pkg/foo",
		Description: "## Repro\n...\n",
		Labels:      []string{"mills-eligible", "priority:P1", "area:foo"},
		State:       "opened",
		WebURL:      "https://gitlab/services/loom-core/-/issues/42",
	}
	item := issueToBacklog(issue, store.P2)

	if item.ID != "gl-47-42" {
		t.Errorf("ID = %q, want gl-47-42", item.ID)
	}
	if item.GitLabIssueIID == nil || *item.GitLabIssueIID != 42 {
		t.Errorf("GitLabIssueIID = %v, want 42", item.GitLabIssueIID)
	}
	if item.Title != issue.Title {
		t.Errorf("Title = %q, want %q", item.Title, issue.Title)
	}
	if item.Priority != store.P1 {
		t.Errorf("Priority = %s, want P1", item.Priority)
	}
	if item.State != store.BacklogQueued {
		t.Errorf("State = %s, want queued", item.State)
	}
	if item.SpecDoc != issue.Description {
		t.Errorf("SpecDoc mismatch")
	}
	if item.CreatedBy != importerCreatedBy {
		t.Errorf("CreatedBy = %q, want %q", item.CreatedBy, importerCreatedBy)
	}
	if len(item.Labels) != 3 {
		t.Errorf("Labels len = %d, want 3", len(item.Labels))
	}
}

func TestTick_CreatesNewBacklogItems(t *testing.T) {
	client := &stubClient{issues: []clients.IssueListItem{
		{IID: 1, ProjectID: 47, Title: "A", State: "opened",
			Labels: []string{"mills-eligible", "priority:P0"}},
		{IID: 2, ProjectID: 47, Title: "B", State: "opened",
			Labels: []string{"mills-eligible"}},
	}}
	st := newStubStore()
	im := NewGitLabImporter(client, st, GitLabImporterConfig{}, nil)

	imported, err := im.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if imported != 2 {
		t.Errorf("imported = %d, want 2", imported)
	}
	a, err := st.Get(context.Background(), "gl-47-1")
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	if a.Priority != store.P0 {
		t.Errorf("A priority = %s, want P0", a.Priority)
	}
	b, err := st.Get(context.Background(), "gl-47-2")
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if b.Priority != store.P2 {
		t.Errorf("B priority = %s, want P2 (default)", b.Priority)
	}
}

func TestTick_IsIdempotent(t *testing.T) {
	client := &stubClient{issues: []clients.IssueListItem{
		{IID: 1, ProjectID: 47, Title: "A", State: "opened",
			Labels: []string{"mills-eligible"}},
	}}
	st := newStubStore()
	im := NewGitLabImporter(client, st, GitLabImporterConfig{}, nil)

	if _, err := im.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	imported, err := im.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if imported != 0 {
		t.Errorf("second tick imported = %d, want 0", imported)
	}
}

func TestTick_PreservesLocalStateChanges(t *testing.T) {
	// Simulate: import → reconciler flips state to running → re-import
	// MUST NOT clobber the running state back to queued.
	client := &stubClient{issues: []clients.IssueListItem{
		{IID: 7, ProjectID: 47, Title: "Live", State: "opened",
			Labels: []string{"mills-eligible"}},
	}}
	st := newStubStore()
	im := NewGitLabImporter(client, st, GitLabImporterConfig{}, nil)

	if _, err := im.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	existing, _ := st.Get(context.Background(), "gl-47-7")
	existing.State = store.BacklogRunning
	if err := st.Put(context.Background(), existing); err != nil {
		t.Fatal(err)
	}

	if _, err := im.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	after, _ := st.Get(context.Background(), "gl-47-7")
	if after.State != store.BacklogRunning {
		t.Errorf("State = %s, want running (importer should not stomp)", after.State)
	}
}

func TestTick_SkipsNonOpenedIssues(t *testing.T) {
	client := &stubClient{issues: []clients.IssueListItem{
		{IID: 1, ProjectID: 47, Title: "open", State: "opened",
			Labels: []string{"mills-eligible"}},
		{IID: 2, ProjectID: 47, Title: "closed-but-leaked", State: "closed",
			Labels: []string{"mills-eligible"}},
	}}
	st := newStubStore()
	im := NewGitLabImporter(client, st, GitLabImporterConfig{}, nil)

	imported, err := im.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if imported != 1 {
		t.Errorf("imported = %d, want 1 (closed issue skipped)", imported)
	}
	if _, err := st.Get(context.Background(), "gl-47-2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("closed issue 2 was imported: %v", err)
	}
}

func TestTick_PropagatesListError(t *testing.T) {
	client := &stubClient{err: errors.New("boom")}
	im := NewGitLabImporter(client, newStubStore(), GitLabImporterConfig{}, nil)

	_, err := im.Tick(context.Background())
	if err == nil {
		t.Fatal("expected error from failed list")
	}
}

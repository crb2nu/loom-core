package intake

import (
	"context"
	"errors"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubPlanAuthor records calls and returns a scripted id/err per backlog id.
type stubPlanAuthor struct {
	ids    map[string]string
	errs   map[string]error
	called []string
}

func (s *stubPlanAuthor) AuthorPlan(_ context.Context, item *store.BacklogItem, _ string) (string, error) {
	s.called = append(s.called, item.ID)
	if err := s.errs[item.ID]; err != nil {
		return "", err
	}
	if id, ok := s.ids[item.ID]; ok {
		return id, nil
	}
	return "plan-mills-" + item.ID, nil
}

// stubBackfillStore serves a fixed list and records Put calls.
type stubBackfillStore struct {
	items  []*store.BacklogItem
	puts   map[string]string // backlog id -> stamped plan id
	putErr map[string]error
}

func (s *stubBackfillStore) List(_ context.Context) ([]*store.BacklogItem, error) {
	return s.items, nil
}

func (s *stubBackfillStore) Put(_ context.Context, item *store.BacklogItem) error {
	if err := s.putErr[item.ID]; err != nil {
		return err
	}
	if s.puts == nil {
		s.puts = map[string]string{}
	}
	s.puts[item.ID] = item.PlanID
	return nil
}

func TestPlanBackfiller_LinksUnlinkedSkipsLinked(t *testing.T) {
	store0 := &stubBackfillStore{
		items: []*store.BacklogItem{
			{ID: "a", Title: "A"},                          // unlinked → authored
			{ID: "b", Title: "B", PlanID: "plan-existing"}, // linked → skipped
		},
	}
	author := &stubPlanAuthor{}
	bf := &PlanBackfiller{Store: store0, Author: author, Project: "services/loom-core"}

	linked, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if linked != 1 {
		t.Errorf("linked = %d, want 1", linked)
	}
	if len(author.called) != 1 || author.called[0] != "a" {
		t.Errorf("author called for %v, want only [a]", author.called)
	}
	if store0.puts["a"] != "plan-mills-a" {
		t.Errorf("item a stamped with %q, want plan-mills-a", store0.puts["a"])
	}
	if _, ok := store0.puts["b"]; ok {
		t.Errorf("already-linked item b should not be Put")
	}
}

func TestPlanBackfiller_BestEffortOnAuthorError(t *testing.T) {
	store0 := &stubBackfillStore{
		items: []*store.BacklogItem{
			{ID: "x", Title: "X"},
			{ID: "y", Title: "Y"},
		},
	}
	author := &stubPlanAuthor{errs: map[string]error{"x": errors.New("hub down")}}
	bf := &PlanBackfiller{Store: store0, Author: author}

	linked, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if linked != 1 {
		t.Errorf("linked = %d, want 1 (x failed, y linked)", linked)
	}
	if _, ok := store0.puts["x"]; ok {
		t.Errorf("failed item x should not be stamped")
	}
	if store0.puts["y"] != "plan-mills-y" {
		t.Errorf("item y stamped with %q", store0.puts["y"])
	}
}

func TestPlanBackfiller_PutFailureDoesNotCount(t *testing.T) {
	store0 := &stubBackfillStore{
		items:  []*store.BacklogItem{{ID: "z", Title: "Z"}},
		putErr: map[string]error{"z": errors.New("sqlite busy")},
	}
	bf := &PlanBackfiller{Store: store0, Author: &stubPlanAuthor{}}
	linked, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if linked != 0 {
		t.Errorf("linked = %d, want 0 when Put fails", linked)
	}
}

func TestPlanBackfiller_NilDepsNoOp(t *testing.T) {
	var bf *PlanBackfiller
	if n, err := bf.Run(context.Background()); n != 0 || err != nil {
		t.Errorf("nil backfiller: n=%d err=%v, want 0/nil", n, err)
	}
	bf2 := &PlanBackfiller{}
	if n, err := bf2.Run(context.Background()); n != 0 || err != nil {
		t.Errorf("empty backfiller: n=%d err=%v, want 0/nil", n, err)
	}
}

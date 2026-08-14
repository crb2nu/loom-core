package intake

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type gcStubStore struct {
	mu        sync.Mutex
	items     []*store.BacklogItem
	deleted   []string
	listErr   error
	deleteErr error
}

func (s *gcStubStore) ListByState(_ context.Context, st store.BacklogState) ([]*store.BacklogItem, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := []*store.BacklogItem{}
	for _, it := range s.items {
		if it.State == st {
			out = append(out, it)
		}
	}
	return out, nil
}

func (s *gcStubStore) Delete(_ context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.mu.Lock()
	s.deleted = append(s.deleted, id)
	// Remove from items so subsequent List doesn't see it.
	kept := s.items[:0]
	for _, it := range s.items {
		if it.ID != id {
			kept = append(kept, it)
		}
	}
	s.items = kept
	s.mu.Unlock()
	return nil
}

func canaryEsc(id string, ageHours int) *store.BacklogItem {
	return &store.BacklogItem{
		ID:        id,
		Title:     id,
		State:     store.BacklogEscalated,
		Labels:    []string{"mills-canary"},
		CreatedAt: time.Now().UTC().Add(-time.Duration(ageHours) * time.Hour),
	}
}

func TestCanaryGC_DeletesStaleEscalatedCanaries(t *testing.T) {
	st := &gcStubStore{items: []*store.BacklogItem{
		canaryEsc("OLD-1", 100), // older than 48h → delete
		canaryEsc("OLD-2", 49),  // older than 48h → delete
		canaryEsc("YOUNG", 10),  // younger than 48h → keep
	}}
	g := NewCanaryGC(st, CanaryGCConfig{StaleAfter: 48 * time.Hour}, nil)
	n, err := g.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}
	if got := len(st.deleted); got != 2 {
		t.Errorf("store delete count = %d, want 2", got)
	}
}

func TestCanaryGC_GlobalAdmissionBarrier(t *testing.T) {
	st := &gcStubStore{items: []*store.BacklogItem{canaryEsc("OLD", 100)}}
	gc := NewCanaryGC(st, CanaryGCConfig{StaleAfter: 48 * time.Hour}, nil)
	gc.Enabled = func() bool { return false }
	if n, err := gc.Tick(context.Background()); err != nil || n != 0 || len(st.deleted) != 0 || gc.ActiveOperations() != 0 {
		t.Fatalf("disabled Tick() n=%d deleted=%v active=%d err=%v", n, st.deleted, gc.ActiveOperations(), err)
	}
}

func TestCanaryGC_SkipsNonCanaryEscalated(t *testing.T) {
	st := &gcStubStore{items: []*store.BacklogItem{
		canaryEsc("CANARY-OLD", 100),
		{
			ID: "REAL-OLD", State: store.BacklogEscalated,
			Labels:    []string{"feature", "priority:P1"},
			CreatedAt: time.Now().UTC().Add(-200 * time.Hour),
		},
	}}
	g := NewCanaryGC(st, CanaryGCConfig{}, nil)
	n, err := g.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (only the canary)", n)
	}
	if len(st.deleted) != 1 || st.deleted[0] != "CANARY-OLD" {
		t.Errorf("deleted = %v, want [CANARY-OLD]", st.deleted)
	}
}

func TestCanaryGC_SkipsNonEscalated(t *testing.T) {
	// ListByState(Escalated) inside Tick filters automatically — but
	// guard against future regressions by feeding mixed states.
	st := &gcStubStore{items: []*store.BacklogItem{
		canaryEsc("CANARY-ESC", 100),
		{
			ID: "CANARY-QUEUED", State: store.BacklogQueued,
			Labels:    []string{"mills-canary"},
			CreatedAt: time.Now().UTC().Add(-200 * time.Hour),
		},
		{
			ID: "CANARY-MERGED", State: store.BacklogMerged,
			Labels:    []string{"mills-canary"},
			CreatedAt: time.Now().UTC().Add(-200 * time.Hour),
		},
	}}
	g := NewCanaryGC(st, CanaryGCConfig{}, nil)
	n, _ := g.Tick(context.Background())
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (only the escalated canary)", n)
	}
}

func TestCanaryGC_DryRunCountsButDoesNotDelete(t *testing.T) {
	st := &gcStubStore{items: []*store.BacklogItem{
		canaryEsc("OLD-1", 100),
		canaryEsc("OLD-2", 100),
	}}
	g := NewCanaryGC(st, CanaryGCConfig{DryRun: true}, nil)
	n, _ := g.Tick(context.Background())
	if n != 2 {
		t.Errorf("dry-run count = %d, want 2", n)
	}
	if len(st.deleted) != 0 {
		t.Errorf("dry-run still deleted: %v", st.deleted)
	}
}

func TestCanaryGC_PropagatesListError(t *testing.T) {
	st := &gcStubStore{listErr: errors.New("db down")}
	g := NewCanaryGC(st, CanaryGCConfig{}, nil)
	if _, err := g.Tick(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCanaryGC_SkipsDeleteFailures(t *testing.T) {
	// Two stale canaries; delete fails for both. Tick must return 0
	// deleted + nil error (the per-item failures are logged + skipped).
	st := &gcStubStore{
		items: []*store.BacklogItem{
			canaryEsc("OLD-1", 100), canaryEsc("OLD-2", 100),
		},
		deleteErr: errors.New("delete forbidden"),
	}
	g := NewCanaryGC(st, CanaryGCConfig{}, nil)
	n, err := g.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0 (all failed)", n)
	}
}

func TestCanaryGC_AppliesDefaults(t *testing.T) {
	g := NewCanaryGC(&gcStubStore{}, CanaryGCConfig{}, nil)
	if g.cfg.StaleAfter != 48*time.Hour {
		t.Errorf("StaleAfter = %v, want 48h", g.cfg.StaleAfter)
	}
	if g.cfg.Label != "mills-canary" {
		t.Errorf("Label = %q, want mills-canary", g.cfg.Label)
	}
}

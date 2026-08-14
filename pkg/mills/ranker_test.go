package mills

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func bi(id string, p store.Priority, ageHours float64, now time.Time) *store.BacklogItem {
	return &store.BacklogItem{
		ID:        id,
		Priority:  p,
		CreatedAt: now.Add(-time.Duration(ageHours * float64(time.Hour))),
	}
}

func ids(items []*store.BacklogItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestRank_PriorityDominates(t *testing.T) {
	now := time.Now()
	// A fresh, clean P0 must outrank a very old P1.
	items := []*store.BacklogItem{
		bi("p1-old", store.P1, 200, now),
		bi("p0-fresh", store.P0, 0, now),
	}
	got := ids(Rank(items, nil, now))
	if got[0] != "p0-fresh" {
		t.Errorf("order = %v, want p0-fresh first", got)
	}
}

func TestRank_EscalationDeprioritizesWithinBand(t *testing.T) {
	now := time.Now()
	items := []*store.BacklogItem{
		bi("a", store.P2, 1, now),
		bi("b", store.P2, 1, now),
	}
	esc := map[string]int{"a": 3} // a keeps escalating -> yields its slot to b
	got := ids(Rank(items, esc, now))
	if got[0] != "b" || got[1] != "a" {
		t.Errorf("order = %v, want [b a] (escalated a deprioritized)", got)
	}
}

func TestRank_EscalationNeverCrossesPriorityBand(t *testing.T) {
	now := time.Now()
	// A P1 with a huge escalation count must STILL outrank a clean P2 — the
	// ranker only reorders within a priority band, never across one.
	items := []*store.BacklogItem{
		bi("p2-clean", store.P2, 0, now),
		bi("p1-bad", store.P1, 0, now),
	}
	esc := map[string]int{"p1-bad": 100}
	got := ids(Rank(items, esc, now))
	if got[0] != "p1-bad" {
		t.Errorf("order = %v, want p1-bad first (priority band preserved)", got)
	}
}

func TestRank_AgeBreaksTiesOldestFirst(t *testing.T) {
	now := time.Now()
	items := []*store.BacklogItem{
		bi("young", store.P2, 1, now),
		bi("old", store.P2, 50, now),
	}
	got := ids(Rank(items, nil, now))
	if got[0] != "old" {
		t.Errorf("order = %v, want old first (anti-starvation)", got)
	}
}

func TestRank_ZeroEscalationsReproducesFIFOWithinPriority(t *testing.T) {
	now := time.Now()
	// Input already in the store's priority,created_at ASC order. With no
	// escalations the ranker must preserve it exactly (strict refinement —
	// the age bonus reinforces oldest-first, matching created_at ASC).
	items := []*store.BacklogItem{
		bi("p0", store.P0, 10, now),
		bi("p1a", store.P1, 20, now),
		bi("p1b", store.P1, 5, now),
		bi("p2", store.P2, 100, now),
	}
	got := ids(Rank(items, nil, now))
	want := []string{"p0", "p1a", "p1b", "p2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (FIFO-within-priority preserved)", got, want)
		}
	}
}

func TestRank_EmptyAndNilSafe(t *testing.T) {
	now := time.Now()
	if got := Rank(nil, nil, now); len(got) != 0 {
		t.Errorf("Rank(nil) = %v, want empty", got)
	}
	// A nil item in the slice must not panic.
	items := []*store.BacklogItem{nil, bi("x", store.P1, 1, now)}
	got := Rank(items, nil, now)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

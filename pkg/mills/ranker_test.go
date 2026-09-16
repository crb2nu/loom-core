package mills

import (
	"context"
	"errors"
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

func TestDispatchRanker_TasteAt856PercentCoverageInfluencesOrder(t *testing.T) {
	now := time.Now()
	clean := bi("clean", store.P2, 1, now)
	regretted := bi("regretted", store.P2, 1, now)
	result, err := (DispatchRanker{}).Rank(context.Background(), []RankingInput{
		{Item: regretted, Taste: &store.PlanTasteAggregate{RegretRate: 1, GradeCoverage: .856}},
		{Item: clean, Taste: &store.PlanTasteAggregate{RegretRate: 0, GradeCoverage: .856}},
	}, RankingBudget{MaxCandidates: 2, MaxCost: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Items); got[0] != "clean" {
		t.Fatalf("order = %v, want clean first", got)
	}
	if result.CandidatesEvaluated != 2 || result.Cost != 2 {
		t.Fatalf("accounting = %+v", result)
	}
}

func TestDispatchRanker_MissingTasteUsesOutcomeOnly(t *testing.T) {
	now := time.Now()
	items := []RankingInput{
		{Item: bi("fifo-a", store.P2, 1, now)},
		{Item: bi("fifo-b", store.P2, 1, now)},
	}
	result, err := (DispatchRanker{}).Rank(context.Background(), items, RankingBudget{MaxCandidates: 2, MaxCost: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Items); got[0] != "fifo-a" || got[1] != "fifo-b" {
		t.Fatalf("equal outcome scores must retain FIFO: %v", got)
	}
}

func TestRankerTasteFeaturesCoverageKillGate(t *testing.T) {
	plan := store.PlanTasteAggregate{PlanID: "plan-a", RegretRate: 1}
	for _, tc := range []struct {
		name string
		agg  store.TasteAggregates
		want bool
	}{
		{"exactly sixty percent", store.TasteAggregates{Plans: []store.PlanTasteAggregate{plan}, OverallMerged14d: 5, OverallGraded14d: 3, OverallCoverage14d: .6}, true},
		{"below sixty percent", store.TasteAggregates{Plans: []store.PlanTasteAggregate{plan}, OverallMerged14d: 5, OverallGraded14d: 2, OverallCoverage14d: .4}, false},
		{"missing aggregate", store.TasteAggregates{Plans: []store.PlanTasteAggregate{plan}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := rankerTasteFeatures(tc.agg)["plan-a"]
			if ok != tc.want {
				t.Fatalf("taste present=%v want=%v", ok, tc.want)
			}
		})
	}
}

func TestDispatchRanker_OutcomeFeaturesInfluenceOrder(t *testing.T) {
	now := time.Now()
	result, err := (DispatchRanker{}).Rank(context.Background(), []RankingInput{
		{Item: bi("escalates", store.P2, 1, now), Outcome: &store.OutcomeFeatures{Samples: 4, MergeRate: .1}},
		{Item: bi("merges", store.P2, 1, now), Outcome: &store.OutcomeFeatures{Samples: 4, MergeRate: .9}},
	}, RankingBudget{MaxCandidates: 2, MaxCost: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Items); got[0] != "merges" {
		t.Fatalf("outcome order=%v", got)
	}
}

func TestOutcomeDispatchScoreIsCalibratableAndPreservesOrder(t *testing.T) {
	now := time.Now()
	high := OutcomeDispatchScore(bi("high", store.P0, 24, now), 0, now)
	low := OutcomeDispatchScore(bi("low", store.P3, 0, now), 3, now)
	if high <= low {
		t.Fatalf("score order high=%v low=%v", high, low)
	}
	if high < 0 || high > 1 || low < 0 || low > 1 {
		t.Fatalf("scores outside calibration domain: high=%v low=%v", high, low)
	}
}

func TestDispatchRanker_BoundsAndCancellation(t *testing.T) {
	now := time.Now()
	in := []RankingInput{{Item: bi("a", store.P2, 0, now)}, {Item: bi("b", store.P2, 0, now)}}
	if _, err := (DispatchRanker{}).Rank(context.Background(), in, RankingBudget{MaxCandidates: 2, MaxCost: 1}); !errors.Is(err, ErrRankingBudgetExceeded) {
		t.Fatalf("cost error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (DispatchRanker{}).Rank(ctx, in, RankingBudget{MaxCandidates: 2, MaxCost: 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

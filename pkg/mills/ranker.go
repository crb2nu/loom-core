package mills

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

var ErrRankingBudgetExceeded = errors.New("ranking budget exceeded")

const (
	DefaultRankerTimeout       = 250 * time.Millisecond
	DefaultRankerMaxCandidates = 100
	DefaultRankerMaxCost       = 100.0
	// MinimumTasteCoverage is the grade coverage required before taste can
	// influence dispatch ranking. Read models should expose this value rather
	// than copying the threshold.
	MinimumTasteCoverage = 0.6
	minimumTasteCoverage = MinimumTasteCoverage
	tasteRegretPenalty   = 500.0
	outcomeMergeBonus    = 200.0
)

// RankingInput is the bounded, immutable feature set presented to a Ranker.
// Taste is nil when a plan has no sufficiently-covered grade history.
type RankingInput struct {
	Item        *store.BacklogItem
	Escalations int
	Outcome     *store.OutcomeFeatures
	Taste       *store.PlanTasteAggregate
}

type RankingBudget struct {
	MaxCandidates int
	MaxCost       float64
}

type RankingResult struct {
	Items               []*store.BacklogItem
	CandidatesEvaluated int
	Cost                float64
}

// Ranker orders candidates within priority bands. Implementations must return
// a new slice and preserve input order for equal scores.
type Ranker interface {
	Rank(context.Context, []RankingInput, RankingBudget) (RankingResult, error)
}

// DispatchRanker is the default deterministic outcome+taste heuristic.
type DispatchRanker struct{}

func (DispatchRanker) Rank(ctx context.Context, in []RankingInput, budget RankingBudget) (RankingResult, error) {
	limit := len(in)
	if budget.MaxCandidates > 0 && limit > budget.MaxCandidates {
		limit = budget.MaxCandidates
	}
	if budget.MaxCost > 0 && float64(limit) > budget.MaxCost {
		return RankingResult{}, ErrRankingBudgetExceeded
	}
	type scored struct {
		item  *store.BacklogItem
		score float64
	}
	rows := make([]scored, 0, limit)
	now := time.Now()
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return RankingResult{}, err
		}
		row := in[i]
		score := scoreItem(row.Item, row.Escalations, now)
		if row.Outcome != nil {
			score += clamp01(row.Outcome.MergeRate) * outcomeMergeBonus
		}
		if row.Taste != nil {
			score -= clamp01(row.Taste.RegretRate) * clamp01(row.Taste.GradeCoverage) * tasteRegretPenalty
		}
		rows = append(rows, scored{item: row.Item, score: score})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].item == nil || rows[j].item == nil {
			return rows[i].score > rows[j].score
		}
		if rows[i].item.Priority != rows[j].item.Priority {
			return priorityBase(rows[i].item.Priority) > priorityBase(rows[j].item.Priority)
		}
		return rows[i].score > rows[j].score
	})
	out := make([]*store.BacklogItem, 0, len(in))
	for _, row := range rows {
		out = append(out, row.item)
	}
	for i := limit; i < len(in); i++ {
		out = append(out, in[i].Item)
	}
	return RankingResult{Items: out, CandidatesEvaluated: limit, Cost: float64(limit)}, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Dispatch ranker (W3.2 of .loom/126). Replaces the store's
// FIFO-within-priority queue order with a deterministic estimate of expected
// merge probability so the limited per-tick dispatch slots go to the work most
// likely to merge — without an LLM in the hot path. Pure + deterministic: no
// I/O, no model. Policy-gated (pipeline.ranker_enabled); default-off falls back
// to the store's priority,created_at order.
//
// Score = priority base (dominant) − escalation penalty (chronically-failing
// items yield slots) + a small age bonus (anti-starvation tie-break). The
// penalty and bonus are both capped strictly below the priority gap, so the
// ranker never promotes a lower-priority item above a higher-priority one — it
// only reorders WITHIN a priority band. With zero escalations it reproduces
// FIFO-within-priority exactly (a strict refinement, safe to flip on).

const (
	// priorityGap is the score distance between adjacent priority buckets.
	priorityGap = 1000.0

	// escalationPenaltyPer docks score for each recent escalation of the SAME
	// backlog item; capped so it cannot cross a priority band.
	escalationPenaltyPer = 150.0
	maxEscalationPenalty = priorityGap - 200.0 // 800: stays within the band

	// ageBonusPerHour nudges older items up to avoid starvation; capped small
	// so it only breaks ties within a priority+risk band.
	ageBonusPerHour = 1.5
	maxAgeBonus     = priorityGap - 400.0 // 600: never crosses a band
)

// rankerEscalationWindow is how far back the reconciler counts a backlog item's
// escalations when scoring its recent merge probability for the dispatch ranker.
const rankerEscalationWindow = 7 * 24 * time.Hour

// priorityBase maps a bucket to a dominant additive base (P0 highest).
func priorityBase(p store.Priority) float64 {
	switch p {
	case store.P0:
		return 4 * priorityGap
	case store.P1:
		return 3 * priorityGap
	case store.P2:
		return 2 * priorityGap
	case store.P3:
		return 1 * priorityGap
	default:
		// Unknown priority sits between P2 and P3 so a mislabeled item is
		// neither starved nor jumped to the front.
		return 1.5 * priorityGap
	}
}

// scoreItem computes the dispatch score for one item given its recent
// escalation count and the current time. Higher dispatches sooner.
func scoreItem(item *store.BacklogItem, escalations int, now time.Time) float64 {
	if item == nil {
		return 0
	}
	s := priorityBase(item.Priority)

	if escalations > 0 {
		pen := float64(escalations) * escalationPenaltyPer
		if pen > maxEscalationPenalty {
			pen = maxEscalationPenalty
		}
		s -= pen
	}

	if age := now.Sub(item.CreatedAt).Hours(); age > 0 {
		bonus := age * ageBonusPerHour
		if bonus > maxAgeBonus {
			bonus = maxAgeBonus
		}
		s += bonus
	}
	return s
}

// OutcomeDispatchScore maps the deterministic base dispatch score to a
// probability-like 0..1 value for calibration. The positive constant divisor
// preserves ordering while keeping terminal writebacks in the domain consumed
// by OutcomeWritebackDAO.Calibration. startedAt fixes the age component to the
// point the run was dispatched.
func OutcomeDispatchScore(item *store.BacklogItem, escalations int, startedAt time.Time) float64 {
	const maximumBaseScore = 4*priorityGap + maxAgeBonus
	return clamp01(scoreItem(item, escalations, startedAt) / maximumBaseScore)
}

// Rank returns a new slice ordered by descending dispatch score. Equal scores
// keep the input order (the store's priority,created_at FIFO), so the ranker is
// a stable refinement of the existing order. `escalations` maps backlog id →
// recent escalation count; a missing id scores as zero escalations.
func Rank(items []*store.BacklogItem, escalations map[string]int, now time.Time) []*store.BacklogItem {
	out := make([]*store.BacklogItem, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool {
		var ei, ej int
		if out[i] != nil {
			ei = escalations[out[i].ID]
		}
		if out[j] != nil {
			ej = escalations[out[j].ID]
		}
		return scoreItem(out[i], ei, now) > scoreItem(out[j], ej, now)
	})
	return out
}

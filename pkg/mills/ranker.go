package mills

import (
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

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

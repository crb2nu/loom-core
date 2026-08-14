package council

import (
	"sort"

	basepolicy "github.com/crb2nu/loom/pkg/policy"
)

// PlanningCandidate is the council-facing subset needed for stability-first
// ordering. It intentionally stays decoupled from store.BacklogItem so tests
// and future intake sources can reuse it without database fixtures.
type PlanningCandidate struct {
	ID       string
	Title    string
	Labels   []string
	Files    []string
	Priority int
}

// StabilityFirstOrder returns a copy of candidates with remediation work first.
func StabilityFirstOrder(candidates []PlanningCandidate) []PlanningCandidate {
	out := append([]PlanningCandidate(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		di := basepolicy.PrioritizeStabilityFirst(stabilityItem(out[i]))
		dj := basepolicy.PrioritizeStabilityFirst(stabilityItem(out[j]))
		if di.Remediation != dj.Remediation {
			return di.Remediation
		}
		if di.Score != dj.Score {
			return di.Score > dj.Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func stabilityItem(c PlanningCandidate) basepolicy.StabilityItem {
	return basepolicy.StabilityItem{
		ID: c.ID, Title: c.Title, Labels: c.Labels, Files: c.Files, Priority: c.Priority,
	}
}

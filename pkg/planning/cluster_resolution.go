package planning

import (
	"sort"
	"time"
)

const defaultRequiredZeroWindows = 3

// ClusterWindow is one observation window for a failure cluster.
type ClusterWindow struct {
	ObservedAt time.Time
	Count      int
}

// ClusterResolutionResult states whether a failure cluster is resolved.
type ClusterResolutionResult struct {
	Resolved            bool
	ConsecutiveZeroRuns int
	RequiredZeroRuns    int
	LastObservedAt      time.Time
	Reason              string
}

// EvaluateClusterResolution requires the last N windows to be zero before a
// cluster is considered resolved. If requiredZeroWindows is zero, the default
// is three windows. Observations are sorted by ObservedAt so callers do not
// have to pre-normalize their query results.
func EvaluateClusterResolution(windows []ClusterWindow, requiredZeroWindows int) ClusterResolutionResult {
	if requiredZeroWindows <= 0 {
		requiredZeroWindows = defaultRequiredZeroWindows
	}
	res := ClusterResolutionResult{RequiredZeroRuns: requiredZeroWindows}
	if len(windows) == 0 {
		res.Reason = "no observation windows"
		return res
	}

	ordered := append([]ClusterWindow(nil), windows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
	})
	res.LastObservedAt = ordered[len(ordered)-1].ObservedAt

	for i := len(ordered) - 1; i >= 0; i-- {
		if ordered[i].Count != 0 {
			break
		}
		res.ConsecutiveZeroRuns++
	}
	if res.ConsecutiveZeroRuns < requiredZeroWindows {
		res.Reason = "insufficient consecutive zero windows"
		return res
	}
	res.Resolved = true
	res.Reason = "required consecutive zero windows observed"
	return res
}

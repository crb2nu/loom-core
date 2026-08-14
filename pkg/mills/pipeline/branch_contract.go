package pipeline

import (
	"strings"
	"unicode"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const (
	defaultSourceBranchPrefix      = "feat/"
	defaultSliceBranchPrefix       = "feat/"
	defaultIntegrationBranchPrefix = "integrate/"
)

// RunBranchContract names the git branches tied to one Mills run.
type RunBranchContract struct {
	SourceBranch      string
	SliceBranch       string
	IntegrationBranch string
}

// BranchContractFor returns the canonical branch names for a run/item/slice.
//
// A source or slice ref is a pure function of the item-owned ID, labels,
// priority, and (for a slice) its name. In particular, run ID, retry attempt,
// stage, agent, and escalation state are deliberately not inputs. Prefix
// precedence is: an explicit branch/type label (hotfix, fix, feat), then a
// P0 priority (hotfix), then feat. The item/slice separator is always '/'.
func BranchContractFor(run *store.PipelineRun, item *store.BacklogItem, stage Stage, sliceName string) RunBranchContract {
	_ = stage
	if item == nil || item.ID == "" {
		return RunBranchContract{}
	}
	backlog := sanitizeBranchComponent(item.ID)
	if backlog == "" {
		return RunBranchContract{}
	}

	contract := RunBranchContract{
		SourceBranch:      sourceBranchName(item),
		IntegrationBranch: IntegrationBranchName(item.ID),
	}
	if sliceName == "" && run != nil && run.ParentSessionID != "" && len(item.Slices) == 1 {
		sliceName = item.Slices[0].Name
	}
	if sliceName != "" {
		contract.SliceBranch = sliceBranchName(item, sliceName)
		contract.SourceBranch = contract.SliceBranch
	}
	if ShouldFanOut(item) {
		contract.SourceBranch = contract.IntegrationBranch
	}
	return contract
}

func sourceBranchName(item *store.BacklogItem) string {
	if item == nil {
		return ""
	}
	backlog := sanitizeBranchComponent(item.ID)
	if backlog == "" {
		return ""
	}
	return branchPrefixFor(item) + backlog
}

// SourceBranchName is the default straight-through branch for a backlog item.
// Prefer BranchContractFor when item metadata is available so the documented
// label/priority prefix mapping is honored.
func SourceBranchName(backlogID string) string {
	backlog := sanitizeBranchComponent(backlogID)
	if backlog == "" {
		return ""
	}
	return defaultSourceBranchPrefix + backlog
}

func sliceBranchName(item *store.BacklogItem, sliceName string) string {
	if item == nil {
		return ""
	}
	backlog := sanitizeBranchComponent(item.ID)
	slice := sanitizeBranchComponent(sliceName)
	if backlog == "" || slice == "" {
		return ""
	}
	return branchPrefixFor(item) + backlog + "/" + slice
}

// SliceBranchName is the default deterministic branch for one fan-out slice.
// Prefer BranchContractFor when item metadata is available so the documented
// label/priority prefix mapping is honored.
func SliceBranchName(backlogID, sliceName string) string {
	backlog := sanitizeBranchComponent(backlogID)
	slice := sanitizeBranchComponent(sliceName)
	if backlog == "" || slice == "" {
		return ""
	}
	return defaultSliceBranchPrefix + backlog + "/" + slice
}

// branchPrefixFor derives a stable type prefix solely from immutable item
// metadata. Labels are compared case-insensitively; callers must never feed
// retry or escalation classifications into this decision.
func branchPrefixFor(item *store.BacklogItem) string {
	has := func(want string) bool {
		for _, label := range item.Labels {
			label = strings.ToLower(strings.TrimSpace(label))
			if label == want || label == "branch/"+want || label == "type/"+want || label == "kind/"+want {
				return true
			}
		}
		return false
	}
	switch {
	case has("hotfix") || has("security"):
		return "hotfix/"
	case has("fix") || has("bug"):
		return "fix/"
	case has("feat") || has("feature"):
		return "feat/"
	case item.Priority == store.P0:
		return "hotfix/"
	default:
		return "feat/"
	}
}

// IntegrationBranchName is the fan-out parent branch.
func IntegrationBranchName(backlogID string) string {
	backlog := sanitizeBranchComponent(backlogID)
	if backlog == "" {
		return ""
	}
	return defaultIntegrationBranchPrefix + backlog
}

func sanitizeBranchComponent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r), r == '.', r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '/':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), ".-_/")
	if out == "" {
		return "x"
	}
	return out
}

package council

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/textsim"
)

// MergedWork is the minimal projection of a merge request the target branch has
// already taken that the council needs to ground a proposal against it: what it
// was called, when it landed, and how to name it in an audit note. Declared
// locally rather than imported from clients — clients imports council, so the
// reverse would be a cycle; *clients.GitLabClient projects its merged-MR list
// into this (the same shape PlanAuthor/PlanLister use).
type MergedWork struct {
	IID      int64
	Title    string
	WebURL   string
	MergedAt time.Time
}

// Ref is the audit subject for one merged MR: the GitLab shorthand when we have
// an iid, the web url otherwise, so an operator reading the events table can
// jump straight to the work that suppressed the proposal.
func (w MergedWork) Ref() string {
	if w.IID > 0 {
		return "!" + strconv.FormatInt(w.IID, 10)
	}
	return strings.TrimSpace(w.WebURL)
}

// MergedWorkSource lists the merge requests landed since a cutoff. Consumer-side
// interface (see MergedWork) so the mutator can be wired to GitLab by the
// operator and left nil everywhere else; nil disables grounding rather than
// blocking it.
type MergedWorkSource interface {
	ListMergedWork(ctx context.Context, since time.Time) ([]MergedWork, error)
}

// defaultMergedWorkLookback bounds the merged-MR corpus. Two weeks covers the
// window in which the council's brief is stale relative to main — a council tick
// runs against a brief assembled before the last few merges landed, so the
// proposals it authors collide with work that shipped hours earlier. Beyond that
// a similar title is legitimate follow-up rather than a restatement, the same
// judgement grayBandRecentWindow makes for backlog items.
const defaultMergedWorkLookback = 14 * 24 * time.Hour

// Merged-work grounding bases, recorded on the skip record + the audit payload
// and used as the metric's band label.
const (
	mergedWorkBasisHard = "hard"
	mergedWorkBasisGray = "gray_band"
)

// MergedWorkSkipped records one proposal the mutator suppressed because it
// restated work that already merged. Carries the MR identity so the council run
// summary and the promotion report name the collision rather than just counting
// it.
type MergedWorkSkipped struct {
	ProposalIndex int     `json:"proposal_index"`
	ProposalTitle string  `json:"proposal_title"`
	MergedIID     int64   `json:"merged_iid"`
	MergedTitle   string  `json:"merged_title"`
	MergedURL     string  `json:"merged_url,omitempty"`
	JaccardScore  float64 `json:"jaccard_score"`
	// Basis is "hard" (at or above the dedup threshold) or "gray_band"
	// ([textsim.GrayBandFloor, threshold) against an MR merged inside the
	// gray-band recency window).
	Basis string `json:"basis"`
}

// mergedWorkHit pairs a matched merged MR with the score and band that produced
// it, mirroring dedupHit/planDedupHit.
type mergedWorkHit struct {
	work  MergedWork
	score float64
	basis string
}

// findMergedWork returns the merged MR that best explains title as a
// restatement of already-shipped work, or nil when nothing crosses the bar.
//
// It applies the SAME two bands the backlog dedup uses, against the merged-MR
// corpus instead of the backlog:
//
//   - hard: score >= threshold, over the whole lookback window.
//   - gray band: score in [textsim.GrayBandFloor, threshold), and ONLY against
//     an MR merged within grayBandRecentWindow of now. A reworded restatement
//     of last week's merge is a re-mint; a loose lookalike of older work is
//     legitimate follow-up. This is the same recency gate
//     findGrayBandDuplicate applies to backlog items, read off MergedAt.
//
// Titles are compared through textsim.NormalizeWorkTitle so a proposal
// ("Wire config-gated OTel trace export into the daemon") matches the MR that
// shipped it ("feat(daemon): wire config-gated OTel trace export into the
// daemon — daemon-otel-export") instead of being diluted by the conventional-
// commit prefix and the plan-slice decoration.
//
// A hard hit always wins over a gray-band one; ties within a band go to the
// higher score, so the recorded collision is the most explanatory one.
func findMergedWork(title string, candidates []MergedWork, threshold float64, now time.Time) *mergedWorkHit {
	// threshold > 1 is the documented "dedup disabled" escape hatch (see
	// MutationOptions.DedupSimilarityThreshold); grounding respects it.
	if title == "" || threshold <= 0 || threshold > 1 {
		return nil
	}
	tokens := textsim.NormalizeWorkTitleTokens(title)
	if len(tokens) == 0 {
		return nil
	}
	var hard, gray *mergedWorkHit
	for _, c := range candidates {
		score := textsim.Jaccard(tokens, textsim.NormalizeWorkTitleTokens(c.Title))
		switch {
		case score >= threshold:
			if hard == nil || score > hard.score {
				hard = &mergedWorkHit{work: c, score: score, basis: mergedWorkBasisHard}
			}
		case score >= textsim.GrayBandFloor && threshold > textsim.GrayBandFloor:
			if c.MergedAt.IsZero() || now.Sub(c.MergedAt) > grayBandRecentWindow {
				continue
			}
			if gray == nil || score > gray.score {
				gray = &mergedWorkHit{work: c, score: score, basis: mergedWorkBasisGray}
			}
		}
	}
	if hard != nil {
		return hard
	}
	return gray
}

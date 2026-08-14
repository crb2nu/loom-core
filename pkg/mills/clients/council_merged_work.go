package clients

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// councilMergedWorkPerPage bounds one grounding snapshot. Grounding asks "what
// shipped lately", not for deep history — the council's own lookback bounds the
// window, and a single page at GitLab's maximum covers every realistic tick.
const councilMergedWorkPerPage = 100

// ListMergedWork makes *GitLabClient a council.MergedWorkSource: the merge
// requests this project took since `since`, projected to the title-only view
// the council's grounding pass compares proposals against. Reuses
// ListMergedMergeRequests so the grounding corpus and the HUD's merged marker
// read the same endpoint with the same auth wiring.
//
// Titles are carried verbatim; the decoration stripping (conventional-commit
// prefix, plan-slice slug) belongs to textsim so both sides of the comparison
// normalize identically.
func (c *GitLabClient) ListMergedWork(ctx context.Context, since time.Time) ([]council.MergedWork, error) {
	if c == nil {
		return nil, nil
	}
	items, err := c.ListMergedMergeRequests(ctx, councilMergedWorkPerPage, since)
	if err != nil {
		return nil, fmt.Errorf("gitlab: list merged work: %w", err)
	}
	out := make([]council.MergedWork, 0, len(items))
	for _, it := range items {
		title := strings.TrimSpace(it.Title)
		if title == "" {
			continue
		}
		// merged_at is the field that matters for the gray band's recency gate;
		// fall back to updated_at, which the list endpoint always populates and
		// which for a merged MR is at or after the merge.
		mergedAt := it.MergedAt
		if mergedAt.IsZero() {
			mergedAt = it.UpdatedAt
		}
		out = append(out, council.MergedWork{
			IID:      it.IID,
			Title:    title,
			WebURL:   it.WebURL,
			MergedAt: mergedAt,
		})
	}
	return out, nil
}

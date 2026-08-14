package clients

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// councilFactoryExhaustPerPage bounds one exhaust snapshot per label. The
// council only ever renders the newest handful, but the API returns issues
// created_at-desc while the brief ranks on last activity, so we take a full
// page and re-rank locally rather than trusting the server's order.
const councilFactoryExhaustPerPage = 100

// ListFactoryExhaust makes *GitLabClient a council.FactoryExhaustSource: the
// open issues this factory filed about its own health, newest first, capped at
// limit. Two label queries rather than one because GitLab ANDs the `labels`
// parameter and no issue carries both — a flake issue is `flaky-test`, an audit
// digest is `audit-digest` (the flake digest deliberately carries neither so it
// does not list itself).
//
// Any sub-query failure fails the whole call. Partial exhaust would render as a
// complete list, and a council reading "1 open flake" when the audit digests
// were simply unreachable draws exactly the wrong conclusion; the brief's
// unavailable-section path states the uncertainty instead.
func (c *GitLabClient) ListFactoryExhaust(ctx context.Context, since time.Time, limit int) ([]council.FactoryExhaustItem, error) {
	if c == nil {
		return nil, nil
	}
	if limit <= 0 {
		return nil, nil
	}
	kinds := []struct {
		label string
		kind  council.FactoryExhaustKind
	}{
		{council.FactoryExhaustFlakyTestLabel, council.FactoryExhaustFlakyTest},
		{council.FactoryExhaustAuditDigestLabel, council.FactoryExhaustAuditDigest},
	}
	var out []council.FactoryExhaustItem
	for _, k := range kinds {
		items, err := c.ListIssues(ctx, ListIssuesOpts{
			Labels:  []string{k.label},
			State:   "opened",
			PerPage: councilFactoryExhaustPerPage,
		})
		if err != nil {
			return nil, fmt.Errorf("gitlab: list factory exhaust (%s): %w", k.label, err)
		}
		for _, it := range items {
			title := strings.TrimSpace(it.Title)
			if title == "" {
				continue
			}
			created := parseGitLabIssueTime(it.CreatedAt)
			updated := parseGitLabIssueTime(it.UpdatedAt)
			// Bound on last activity, falling back to creation: a flake issue
			// opened months ago that flaked again yesterday is live demand,
			// while one untouched for the whole window is not what this tick
			// is about.
			recency := updated
			if recency.IsZero() {
				recency = created
			}
			if !since.IsZero() && !recency.IsZero() && recency.Before(since) {
				continue
			}
			out = append(out, council.FactoryExhaustItem{
				Kind:      k.kind,
				IID:       it.IID,
				Title:     title,
				WebURL:    it.WebURL,
				CreatedAt: created,
				UpdatedAt: updated,
			})
		}
	}
	council.SortFactoryExhaust(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// parseGitLabIssueTime decodes the RFC3339 timestamps IssueListItem carries as
// strings. An unparseable value yields the zero time, which the callers treat
// as "unknown" rather than "epoch" — the brief's ordering falls through to the
// other timestamp and the window filter declines to exclude on it.
func parseGitLabIssueTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

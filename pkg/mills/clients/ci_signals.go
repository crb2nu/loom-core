package clients

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
)

// ciPipeline is the subset of GitLab's pipeline-list payload we read for the
// CI-failure workspace signal (W3.1 GitLab half).
type ciPipeline struct {
	ID     int64  `json:"id"`
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Source string `json:"source"`
	WebURL string `json:"web_url"`
}

// ciSignalMaxClusters caps how many branch clusters the CI signal surfaces.
const ciSignalMaxClusters = 6

// RecentErrorClusters makes *GitLabClient a council.WorkspaceSignalSource: it
// surfaces recent FAILED CI pipelines clustered by branch ref so the council
// brief sees real CI pain alongside the Loki error clusters. Best-effort — a
// query error is returned so the brief assembler can skip the source. Implements
// the GitLab-CI half of W3.1 (.loom/126), reusing the same requestJSON +
// projectPath plumbing as the MR/poll/merge stages.
func (c *GitLabClient) RecentErrorClusters(ctx context.Context, since time.Time) ([]council.WorkspaceSignal, error) {
	if c == nil {
		return nil, nil
	}
	q := url.Values{}
	q.Set("status", "failed")
	q.Set("updated_after", since.UTC().Format(time.RFC3339))
	q.Set("order_by", "updated_at")
	q.Set("sort", "desc")
	q.Set("per_page", "50")
	path := "/projects/" + c.projectPath() + "/pipelines?" + q.Encode()

	var pipes []ciPipeline
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipes); err != nil {
		return nil, fmt.Errorf("gitlab: list failed pipelines: %w", err)
	}
	return clusterFailedPipelines(pipes, ciSignalMaxClusters), nil
}

// clusterFailedPipelines groups failed pipelines by branch ref and returns the
// top groups by count. Pure + deterministic so it is unit-testable without a
// live GitLab.
func clusterFailedPipelines(pipes []ciPipeline, maxClusters int) []council.WorkspaceSignal {
	if maxClusters <= 0 {
		maxClusters = 6
	}
	type agg struct {
		count  int
		sample string
		source string
	}
	groups := map[string]*agg{}
	order := []string{}
	for _, p := range pipes {
		if p.Status != "" && p.Status != "failed" {
			continue
		}
		ref := strings.TrimSpace(p.Ref)
		if ref == "" {
			ref = "(unknown)"
		}
		g, ok := groups[ref]
		if !ok {
			g = &agg{sample: p.WebURL, source: p.Source}
			groups[ref] = g
			order = append(order, ref)
		}
		g.count++
	}
	sort.SliceStable(order, func(i, j int) bool {
		if groups[order[i]].count != groups[order[j]].count {
			return groups[order[i]].count > groups[order[j]].count
		}
		return order[i] < order[j]
	})
	if len(order) > maxClusters {
		order = order[:maxClusters]
	}
	out := make([]council.WorkspaceSignal, 0, len(order))
	for _, ref := range order {
		g := groups[ref]
		sample := fmt.Sprintf("%d failed pipeline(s)", g.count)
		if g.source != "" {
			sample += " (source=" + g.source + ")"
		}
		if g.sample != "" {
			sample += "; latest: " + g.sample
		}
		out = append(out, council.WorkspaceSignal{
			Source:  "gitlab-ci",
			Service: "ci/" + ref,
			Count:   g.count,
			Sample:  sample,
		})
	}
	return out
}

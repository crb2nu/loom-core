package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/finishing"
)

// NewestPathCommit returns the newest commit touching path on ref.
func (c *GitLabClient) NewestPathCommit(ctx context.Context, project, ref, repoPath string) (*time.Time, error) {
	endpoint := fmt.Sprintf("/projects/%s/repository/commits?path=%s&ref_name=%s&per_page=1",
		url.PathEscape(project), url.QueryEscape(repoPath), url.QueryEscape(ref))
	var rows []struct {
		CommittedDate time.Time `json:"committed_date"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	t := rows[0].CommittedDate.UTC()
	return &t, nil
}

var _ finishing.TreeLister = (*GitLabClient)(nil)
var _ finishing.CommitLister = (*GitLabClient)(nil)

// CommitListItem is a bounded, read-only view of one repository commit as
// returned by the commits list endpoint.
//
// Message carries the FULL commit message (title plus body). Git writes its
// revert trailer — "This reverts commit <sha>." — into the body, so a
// title-only view structurally cannot see a revert; the regression-attribution
// sweep reads Message for exactly that reason.
type CommitListItem struct {
	ID         string    `json:"id"`
	ShortID    string    `json:"short_id"`
	Title      string    `json:"title"`
	Message    string    `json:"message"`
	AuthorName string    `json:"author_name"`
	CreatedAt  time.Time `json:"created_at"`
	WebURL     string    `json:"web_url"`
}

// ListBranchCommits returns the commits reachable from ref, newest-first,
// bounded to those created at or after since (zero = unbounded). perPage bounds
// the page size (default 50, capped at 100); only the first page is fetched —
// callers watch a recent window, not deep history.
//
// Read-only, same auth and requestJSON wiring as the merge-request list calls.
func (c *GitLabClient) ListBranchCommits(ctx context.Context, ref string, since time.Time, perPage int) ([]CommitListItem, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("gitlab: list commits: ref required")
	}
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 100 {
		perPage = 100
	}
	path := fmt.Sprintf(
		"/projects/%s/repository/commits?ref_name=%s&per_page=%d",
		c.projectPath(), url.QueryEscape(ref), perPage)
	if !since.IsZero() {
		path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	var items []CommitListItem
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

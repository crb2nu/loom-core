// Package health implements the policy-gated, read-only factory health poller.
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Pipeline struct {
	ID         int64      `json:"id"`
	SHA        string     `json:"sha"`
	Ref        string     `json:"ref"`
	Status     string     `json:"status"`
	FinishedAt *time.Time `json:"finished_at"`
	// MillsCommitAt is populated only when this successful pipeline's commit
	// changed the Mills operator surface.
	MillsCommitAt *time.Time `json:"-"`
}

type Client interface {
	Pipelines(context.Context, string) ([]Pipeline, error)
}

type Observation struct {
	Green            bool
	Known            bool
	RedDuration      time.Duration
	OperatorImageLag time.Duration
}

type Observer func(Observation)

type Poller struct {
	Client          Client
	Ref             string
	Interval        time.Duration
	OperatorBuiltAt time.Time
	Observe         Observer
	Now             func() time.Time
	last            Observation
	redSince        time.Time
}

func (p *Poller) Poll(ctx context.Context) (Observation, error) {
	pipes, err := p.Client.Pipelines(ctx, p.Ref)
	if err != nil {
		return p.current(), err
	}
	sort.SliceStable(pipes, func(i, j int) bool {
		if pipes[i].FinishedAt == nil {
			return false
		}
		if pipes[j].FinishedAt == nil {
			return true
		}
		return pipes[i].FinishedAt.After(*pipes[j].FinishedAt)
	})
	for i, pipe := range pipes {
		if pipe.Ref != "" && pipe.Ref != p.Ref {
			continue
		}
		if pipe.FinishedAt == nil || (pipe.Status != "success" && pipe.Status != "failed") {
			continue
		}
		p.last.Known = true
		p.last.Green = pipe.Status == "success"
		if p.last.Green {
			p.redSince = time.Time{}
			p.last.RedDuration = 0
		} else {
			if p.redSince.IsZero() {
				p.redSince = *pipe.FinishedAt
				// Recover the beginning of the current red streak after an
				// operator restart. Non-terminal pipelines do not interrupt it;
				// the first older success does.
				for _, older := range pipes[i+1:] {
					if older.Ref != "" && older.Ref != p.Ref {
						continue
					}
					if older.FinishedAt == nil || (older.Status != "success" && older.Status != "failed") {
						continue
					}
					if older.Status == "success" {
						break
					}
					p.redSince = *older.FinishedAt
				}
			}
			p.last.RedDuration = p.now().Sub(p.redSince)
		}
		break
	}
	// Image freshness is independent of the latest health transition: a newer
	// docs-only green or a red pipeline must not erase the last successful
	// Mills-touching main commit. Pipelines are ordered by finished_at, which
	// is NOT commit order (a rerun of an old commit finishes latest), so take
	// the maximum commit time across every green Mills-touching pipeline in
	// the window rather than the first hit. A window with none keeps the
	// previous observation instead of erasing it.
	var newestMills *time.Time
	for _, pipe := range pipes {
		if pipe.Ref != "" && pipe.Ref != p.Ref {
			continue
		}
		if pipe.Status != "success" || pipe.MillsCommitAt == nil {
			continue
		}
		if newestMills == nil || pipe.MillsCommitAt.After(*newestMills) {
			newestMills = pipe.MillsCommitAt
		}
	}
	if newestMills != nil {
		if !p.OperatorBuiltAt.IsZero() && newestMills.After(p.OperatorBuiltAt) {
			p.last.OperatorImageLag = newestMills.Sub(p.OperatorBuiltAt)
		} else {
			p.last.OperatorImageLag = 0
		}
	}
	if p.Observe != nil {
		p.Observe(p.last)
	}
	return p.last, nil
}

func (p *Poller) Run(ctx context.Context) error {
	if _, err := p.Poll(ctx); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	interval := p.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_, _ = p.Poll(ctx)
		}
	}
}
func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
func (p *Poller) current() Observation {
	if p.last.Known && !p.last.Green && !p.redSince.IsZero() {
		p.last.RedDuration = p.now().Sub(p.redSince)
	}
	return p.last
}

// GitLabClient reads pipeline details because list-level updated_at is not a
// valid health ordering signal.
type GitLabClient struct {
	BaseURL, Token, Project string
	HTTP                    *http.Client
}

// Pagination bounds. One page is the steady-state cost per poll; deeper pages
// are fetched only while the window has not yet produced both a terminal
// pipeline (health) and a green Mills-touching pipeline (image lag), so a
// stretch of 20+ docs-only or non-terminal pipelines cannot blind either
// signal after an operator restart. The caps keep the worst-case poll bounded
// rather than scanning history without limit.
const (
	pipelinesPerPage  = 20
	maxPipelinePages  = 3
	diffPerPage       = 100
	maxDiffPages      = 3
	commitCachePerRun = 64
)

func (c *GitLabClient) Pipelines(ctx context.Context, ref string) ([]Pipeline, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	project := url.PathEscape(c.Project)
	// Retried pipelines share a SHA; resolve each commit's Mills-touching
	// timestamp once per poll.
	millsCommitBySHA := make(map[string]*time.Time, commitCachePerRun)
	var result []Pipeline
	sawTerminal, sawMillsGreen := false, false
	for page := 1; page <= maxPipelinePages; page++ {
		var listed []Pipeline
		listURL := fmt.Sprintf("%s/projects/%s/pipelines?ref=%s&per_page=%d&page=%d",
			base, project, url.QueryEscape(ref), pipelinesPerPage, page)
		if err := c.get(ctx, listURL, &listed); err != nil {
			return nil, err
		}
		for _, item := range listed {
			var detail Pipeline
			if err := c.get(ctx, fmt.Sprintf("%s/projects/%s/pipelines/%d", base, project, item.ID), &detail); err != nil {
				return nil, err
			}
			if detail.Status == "success" && detail.SHA != "" {
				millsAt, cached := millsCommitBySHA[detail.SHA]
				if !cached {
					var err error
					millsAt, err = c.millsCommitAt(ctx, base, project, detail.SHA)
					if err != nil {
						return nil, err
					}
					if len(millsCommitBySHA) < commitCachePerRun {
						millsCommitBySHA[detail.SHA] = millsAt
					}
				}
				detail.MillsCommitAt = millsAt
				if millsAt != nil {
					sawMillsGreen = true
				}
			}
			if detail.FinishedAt != nil && (detail.Status == "success" || detail.Status == "failed") {
				sawTerminal = true
			}
			result = append(result, detail)
		}
		if len(listed) < pipelinesPerPage || (sawTerminal && sawMillsGreen) {
			break
		}
	}
	return result, nil
}

// LatestAdvisoryRed returns the govulncheck trace only when the latest
// terminal main pipeline is failed by that job. The trace is the job artifact
// emitted by the repository's security job and uses govulncheck's JSON stream.
func (c *GitLabClient) LatestAdvisoryRed(ctx context.Context) (*RemediationIncident, error) {
	pipes, err := c.Pipelines(ctx, "main")
	if err != nil {
		return nil, err
	}
	var failed *Pipeline
	for i := range pipes {
		if pipes[i].Status == "success" {
			return nil, nil
		}
		if pipes[i].Status == "failed" {
			failed = &pipes[i]
			break
		}
	}
	if failed == nil {
		return nil, nil
	}
	var jobs []struct {
		ID           int64 `json:"id"`
		Name, Status string
	}
	base := strings.TrimRight(c.BaseURL, "/")
	project := url.PathEscape(c.Project)
	if err := c.get(ctx, fmt.Sprintf("%s/projects/%s/pipelines/%d/jobs?scope[]=failed&per_page=100", base, project, failed.ID), &jobs); err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if job.Name != "security:govulncheck" || job.Status != "failed" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%s/jobs/%d/trace", base, project, job.ID), nil)
		if err != nil {
			return nil, err
		}
		if c.Token != "" {
			req.Header.Set("PRIVATE-TOKEN", c.Token)
		}
		hc := c.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: 20 * time.Second}
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("gitlab job trace: %s", resp.Status)
		}
		var report json.RawMessage
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		// CI logs may prefix the JSON artifact path; retain only JSON lines.
		var lines [][]byte
		for _, line := range bytes.Split(body, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) > 0 && line[0] == '{' && json.Unmarshal(line, &report) == nil {
				lines = append(lines, line)
			}
		}
		return &RemediationIncident{PipelineID: failed.ID, JobName: job.Name, Report: bytes.Join(lines, []byte("\n"))}, nil
	}
	return nil, nil
}

func (c *GitLabClient) OpenRenovateMRs(ctx context.Context) ([]RemediationMR, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	project := url.PathEscape(c.Project)
	var items []struct {
		IID          int64  `json:"iid"`
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		SHA          string `json:"sha"`
		AutoMerge    bool   `json:"merge_when_pipeline_succeeds"`
		Author       struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	if err := c.get(ctx, fmt.Sprintf("%s/projects/%s/merge_requests?state=opened&scope=all&per_page=100", base, project), &items); err != nil {
		return nil, err
	}
	var out []RemediationMR
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Author.Username+" "+item.SourceBranch), "renovate") {
			out = append(out, RemediationMR{IID: item.IID, Title: item.Title, SourceBranch: item.SourceBranch, HeadSHA: item.SHA, AutoMerge: item.AutoMerge})
		}
	}
	return out, nil
}

// millsCommitAt returns the commit's committed_date when the commit touches
// the Mills operator surface, nil otherwise. The diff listing is followed
// across pages (bounded) so a large commit whose Mills-touching paths sort
// past the first hundred entries is not misread as Mills-free.
func (c *GitLabClient) millsCommitAt(ctx context.Context, base, project, sha string) (*time.Time, error) {
	var commit struct {
		CommittedDate time.Time `json:"committed_date"`
	}
	commitURL := fmt.Sprintf("%s/projects/%s/repository/commits/%s", base, project, url.PathEscape(sha))
	if err := c.get(ctx, commitURL, &commit); err != nil {
		return nil, err
	}
	for page := 1; page <= maxDiffPages; page++ {
		var diff []struct {
			NewPath string `json:"new_path"`
			OldPath string `json:"old_path"`
		}
		if err := c.get(ctx, fmt.Sprintf("%s/diff?per_page=%d&page=%d", commitURL, diffPerPage, page), &diff); err != nil {
			return nil, err
		}
		for _, changed := range diff {
			if millsPath(changed.NewPath) || millsPath(changed.OldPath) {
				at := commit.CommittedDate
				return &at, nil
			}
		}
		if len(diff) < diffPerPage {
			break
		}
	}
	return nil, nil
}

func millsPath(path string) bool {
	return strings.HasPrefix(path, "pkg/mills/") || strings.HasPrefix(path, "cmd/loom-mills-operator/") || path == "docs/MILLS.md"
}
func (c *GitLabClient) get(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("gitlab health request: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

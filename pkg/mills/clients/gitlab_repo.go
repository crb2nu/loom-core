package clients

// gitlab_repo.go holds the small repository-content surface the S6-full
// merging canary needs: idempotent branch creation and an idempotent
// single-file commit. Both are lookup-first so a workflow replay after any
// crash converges on the same branch head instead of failing on "already
// exists" — the same discipline CreateMR (adopt-first) and Merge
// (merged-state reconciliation) already follow.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/finishing"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// ListTree returns every entry below path. GitLab caps a page at 100 entries.
func (c *GitLabClient) ListTree(ctx context.Context, project, ref, treePath string, recursive bool) ([]finishing.TreeEntry, error) {
	var all []finishing.TreeEntry
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("/projects/%s/repository/tree?path=%s&recursive=%t&ref=%s&per_page=100&page=%d",
			url.PathEscape(project), url.QueryEscape(treePath), recursive, url.QueryEscape(ref), page)
		var raw []struct {
			Path, Type string
			ID         string `json:"id"`
		}
		if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &raw); err != nil {
			return nil, err
		}
		for _, e := range raw {
			all = append(all, finishing.TreeEntry{Path: e.Path, Type: e.Type, BlobSHA: e.ID})
		}
		if len(raw) < 100 {
			return all, nil
		}
	}
}

type branchResponse struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// GetBranch returns the branch head SHA, with ok=false on 404.
func (c *GitLabClient) GetBranch(ctx context.Context, branch string) (string, bool, error) {
	path := fmt.Sprintf("/projects/%s/repository/branches/%s", c.projectPath(), url.PathEscape(branch))
	var got branchResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &got); err != nil {
		var httpErr *GitLabHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return got.Commit.ID, true, nil
}

type repositoryCompareResponse struct {
	Commits []struct {
		Title string `json:"title"`
	} `json:"commits"`
	Diffs []struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
		Diff    string `json:"diff"`
	} `json:"diffs"`
}

// CompareBranch returns GitLab's authoritative branch-vs-base diff. The
// source branch lookup is explicit so a missing branch remains distinguishable
// from a valid comparison whose diff is empty.
func (c *GitLabClient) CompareBranch(ctx context.Context, baseBranch, branch string) (pipeline.StageOutput, bool, error) {
	if _, ok, err := c.GetBranch(ctx, branch); err != nil || !ok {
		return pipeline.StageOutput{}, ok, err
	}
	path := fmt.Sprintf("/projects/%s/repository/compare?from=%s&to=%s",
		c.projectPath(), url.QueryEscape(baseBranch), url.QueryEscape(branch))
	var got repositoryCompareResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &got); err != nil {
		return pipeline.StageOutput{}, true, err
	}
	out := pipeline.StageOutput{}
	seen := map[string]struct{}{}
	for _, d := range got.Diffs {
		name := d.NewPath
		if name == "" {
			name = d.OldPath
		}
		if _, exists := seen[name]; name != "" && !exists {
			seen[name] = struct{}{}
			out.FilesChanged = append(out.FilesChanged, name)
		}
		if d.Diff != "" {
			out.DiffPatch = append(out.DiffPatch, d.Diff...)
			if d.Diff[len(d.Diff)-1] != '\n' {
				out.DiffPatch = append(out.DiffPatch, '\n')
			}
			for _, line := range strings.Split(d.Diff, "\n") {
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
					out.LinesAdded++
				}
				if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					out.LinesRemoved++
				}
			}
		}
	}
	for _, commit := range got.Commits {
		if commit.Title != "" {
			out.CommitMessages = append(out.CommitMessages, commit.Title)
		}
	}
	return out, true, nil
}

// EnsureBranch creates branch from ref if it does not exist and returns the
// branch head SHA. A concurrent or prior creation is adopted, not an error.
func (c *GitLabClient) EnsureBranch(ctx context.Context, branch, ref string) (string, error) {
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(ref) == "" {
		return "", errors.New("gitlab: branch and ref required")
	}
	if sha, ok, err := c.GetBranch(ctx, branch); err != nil {
		return "", fmt.Errorf("gitlab: get branch %q: %w", branch, err)
	} else if ok {
		return sha, nil
	}
	path := fmt.Sprintf("/projects/%s/repository/branches?branch=%s&ref=%s",
		c.projectPath(), url.QueryEscape(branch), url.QueryEscape(ref))
	var got branchResponse
	if err := c.requestJSON(ctx, http.MethodPost, path, nil, &got); err != nil {
		if isBranchAlreadyExists(err) {
			sha, ok, gerr := c.GetBranch(ctx, branch)
			if gerr != nil {
				return "", fmt.Errorf("gitlab: adopt existing branch %q: %w (original: %v)", branch, gerr, err)
			}
			if ok {
				return sha, nil
			}
		}
		return "", fmt.Errorf("gitlab: create branch %q from %q: %w", branch, ref, err)
	}
	return got.Commit.ID, nil
}

func isBranchAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "branch already exists")
}

// fileExistsOnRef reports whether path exists on ref (HEAD-equivalent GET).
func (c *GitLabClient) fileExistsOnRef(ctx context.Context, filePath, ref string) (bool, error) {
	path := fmt.Sprintf("/projects/%s/repository/files/%s?ref=%s",
		c.projectPath(), url.PathEscape(filePath), url.QueryEscape(ref))
	var got struct {
		FilePath string `json:"file_path"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &got); err != nil {
		var httpErr *GitLabHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

type commitAction struct {
	Action   string `json:"action"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type commitBody struct {
	Branch        string         `json:"branch"`
	CommitMessage string         `json:"commit_message"`
	Actions       []commitAction `json:"actions"`
}

type repoCommitResponse struct {
	ID string `json:"id"`
}

// EnsureFileOnBranch commits a single created file to branch if it is not
// already there and returns the resulting branch head SHA. A file that
// already exists (a prior interrupted attempt landed it) is adopted.
func (c *GitLabClient) EnsureFileOnBranch(ctx context.Context, branch, filePath, content, message string) (string, error) {
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(filePath) == "" {
		return "", errors.New("gitlab: branch and file path required")
	}
	exists, err := c.fileExistsOnRef(ctx, filePath, branch)
	if err != nil {
		return "", fmt.Errorf("gitlab: check %q on %q: %w", filePath, branch, err)
	}
	if !exists {
		body := commitBody{
			Branch:        branch,
			CommitMessage: message,
			Actions:       []commitAction{{Action: "create", FilePath: filePath, Content: content}},
		}
		path := fmt.Sprintf("/projects/%s/repository/commits", c.projectPath())
		var got repoCommitResponse
		if err := c.requestJSON(ctx, http.MethodPost, path, body, &got); err != nil {
			// "A file with this name already exists": a concurrent or prior
			// attempt landed it between our check and this commit. Adopt.
			if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
				return "", fmt.Errorf("gitlab: commit %q to %q: %w", filePath, branch, err)
			}
		} else if got.ID != "" {
			return got.ID, nil
		}
	}
	sha, ok, err := c.GetBranch(ctx, branch)
	if err != nil || !ok {
		return "", fmt.Errorf("gitlab: resolve head of %q after ensure: ok=%t err=%w", branch, ok, err)
	}
	return sha, nil
}

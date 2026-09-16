package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/mergequeue"
)

// This file is the serial merge queue's Forge surface on the GitLab client
// (mergequeue.Forge). It adds the ONE mutation the read-only observation file
// (gitlab_rebase.go) deliberately excludes: the rebase PUT. The #374 contract
// still holds — every rebase the queue requests is snapshotted (cursors
// before) and settled (ObserveHead after) into the mr_head_transitions
// ledger by the processor, so the movement is durable and never trusted.

var _ mergequeue.Forge = (*GitLabClient)(nil)

var adoptablePipelineSources = map[string]bool{"push": true, "api": true, "web": true, "trigger": true}
var adoptablePipelineStatuses = map[string]bool{"created": true, "waiting_for_resource": true, "preparing": true, "pending": true, "running": true, "success": true}

// FindActivePipeline returns the newest pipeline that can supply a verdict for
// the exact branch head. GitLab's list filters are advisory, so identity is
// checked again client-side before adoption.
func (c *GitLabClient) FindActivePipeline(ctx context.Context, ref, sha string) (mergequeue.PipelineStatus, error) {
	return c.findActivePipelineSources(ctx, ref, sha, []string{"push", "api", "web", "trigger"})
}

func (c *GitLabClient) findActivePipelineSources(ctx context.Context, ref, sha string, sources []string) (mergequeue.PipelineStatus, error) {
	ref, sha = strings.TrimSpace(ref), strings.TrimSpace(sha)
	if ref == "" || sha == "" {
		return mergequeue.PipelineStatus{}, errors.New("gitlab: ref and sha required to find active pipeline")
	}
	var best shaPipeline
	for _, source := range sources {
		path := fmt.Sprintf("/projects/%s/pipelines?sha=%s&ref=%s&source=%s&order_by=id&sort=desc&per_page=100", c.projectPath(), url.QueryEscape(sha), url.QueryEscape(ref), source)
		var pipes []shaPipeline
		if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipes); err != nil {
			return mergequeue.PipelineStatus{}, err
		}
		for _, p := range pipes {
			if p.Ref == ref && p.SHA == sha && p.Source == source && adoptablePipelineSources[p.Source] && adoptablePipelineStatuses[p.Status] && p.ID > best.ID {
				best = p
			}
		}
	}
	if best.ID == 0 {
		return mergequeue.PipelineStatus{}, nil
	}
	return mergequeue.PipelineStatus{ID: best.ID, SHA: best.SHA, Ref: best.Ref, Source: best.Source, Status: best.Status, WebURL: best.WebURL, Found: true}, nil
}

// MRSnapshot reads the queue's view of an MR: live head, lifecycle state, and
// the diff base the MR is currently computed against.
func (c *GitLabClient) MRSnapshot(ctx context.Context, mrIID int64) (mergequeue.MRSnapshot, error) {
	if mrIID <= 0 {
		return mergequeue.MRSnapshot{}, errors.New("gitlab: positive MRIID required")
	}
	mr, err := c.getMRWithRebaseState(ctx, mrIID)
	if err != nil {
		return mergequeue.MRSnapshot{}, err
	}
	merged := mr.MergedCommitSHA
	if merged == "" {
		merged = mr.MergeCommitSHA
	}
	if merged == "" {
		merged = mr.SquashCommitSHA
	}
	return mergequeue.MRSnapshot{
		SHA:              mr.SHA,
		State:            mr.State,
		BaseSHA:          mr.DiffRefs.BaseSHA,
		MergedSHA:        merged,
		RebaseInProgress: mr.RebaseInProgress,
		HasConflicts:     mr.HasConflicts,
		MergeError:       strings.TrimSpace(mr.MergeError),
	}, nil
}

// mergeQueueCommit is the slice of the commits API the queue reads (MR commit
// lists and cherry-pick results).
type mergeQueueCommit struct {
	ID string `json:"id"`
}

type mergeQueueCompare struct {
	Diffs          []json.RawMessage `json:"diffs"`
	CompareSameRef bool              `json:"compare_same_ref"`
}

// treesEqual reports whether two commits have byte-identical trees, using the
// compare API (`straight=true` diffs the two commits directly): an empty diff
// means equal trees. The single-commit API on this GitLab (18.x CE) returns no
// `tree_id`, which is why the proof cannot read tree ids directly. Results are
// memoized per project — commit trees are immutable.
func (c *GitLabClient) treesEqual(ctx context.Context, a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	key := c.cfg.Project + "\x00" + a + "\x00" + b
	if c.commitTrees != nil {
		c.commitTrees.mu.Lock()
		v, ok := c.commitTrees.trees[key]
		c.commitTrees.mu.Unlock()
		if ok {
			return v == "equal", nil
		}
	}
	path := fmt.Sprintf("/projects/%s/repository/compare?from=%s&to=%s&straight=true", c.projectPath(), url.QueryEscape(a), url.QueryEscape(b))
	var cmp mergeQueueCompare
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &cmp); err != nil {
		return false, err
	}
	equal := cmp.CompareSameRef || len(cmp.Diffs) == 0
	if c.commitTrees != nil {
		v := "differ"
		if equal {
			v = "equal"
		}
		c.commitTrees.mu.Lock()
		c.commitTrees.trees[key] = v
		c.commitTrees.mu.Unlock()
	}
	return equal, nil
}

// pipelineProofScanLimit bounds how many candidate pipelines one proof lookup
// compares against the head; memoization makes repeat lookups free.
const pipelineProofScanLimit = 40

// PipelineProof prefers a pipeline on the exact commit (which needs no tree
// evidence at all), then searches the newest project pipelines for a commit
// whose tree equals headSHA's. A failed comparison never authorizes a merge.
func (c *GitLabClient) PipelineProof(ctx context.Context, headSHA string) (mergequeue.PipelineProof, error) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return mergequeue.PipelineProof{}, errors.New("gitlab: head sha required")
	}
	path := fmt.Sprintf("/projects/%s/pipelines?sha=%s&order_by=id&sort=desc&per_page=100", c.projectPath(), url.QueryEscape(headSHA))
	var exact []shaPipeline
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &exact); err != nil {
		return mergequeue.PipelineProof{}, err
	}
	if len(exact) > 0 && exact[0].SHA == headSHA {
		p := exact[0]
		return mergequeue.PipelineProof{PipelineStatus: mergequeue.PipelineStatus{ID: p.ID, SHA: p.SHA, Status: p.Status, WebURL: p.WebURL, Found: true}, Tree: headSHA, Source: "sha"}, nil
	}
	path = fmt.Sprintf("/projects/%s/pipelines?order_by=id&sort=desc&per_page=100", c.projectPath())
	var pipes []shaPipeline
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipes); err != nil {
		return mergequeue.PipelineProof{}, err
	}
	compared := 0
	for _, p := range pipes {
		if strings.TrimSpace(p.SHA) == "" || p.SHA == headSHA {
			continue
		}
		if compared >= pipelineProofScanLimit {
			break
		}
		compared++
		equal, err := c.treesEqual(ctx, headSHA, p.SHA)
		if err != nil || !equal {
			continue
		}
		source := "tree"
		if strings.HasPrefix(p.Ref, "mills-mq/spec-") {
			source = "speculative"
		}
		return mergequeue.PipelineProof{PipelineStatus: mergequeue.PipelineStatus{ID: p.ID, SHA: p.SHA, Status: p.Status, WebURL: p.WebURL, Found: true}, Tree: headSHA, Source: source}, nil
	}
	return mergequeue.PipelineProof{}, nil
}

// BranchTip returns the current tip SHA of a branch in this client's project.
func (c *GitLabClient) BranchTip(ctx context.Context, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", errors.New("gitlab: branch required")
	}
	return c.branchTip(ctx, branch)
}

// RequestRebase asks GitLab to rebase the MR onto its target branch. The
// operation is async: GitLab returns 202 and sets rebase_in_progress; the
// caller settles the outcome via ObserveHead. A 409 means a rebase is already
// in flight and maps to mergequeue.ErrRebaseInProgress — the observation
// should simply proceed.
func (c *GitLabClient) RequestRebase(ctx context.Context, mrIID int64) error {
	if mrIID <= 0 {
		return errors.New("gitlab: positive MRIID required")
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/rebase", c.projectPath(), mrIID)
	err := c.requestJSON(ctx, http.MethodPut, path, nil, nil)
	if err == nil {
		return nil
	}
	var httpErr *GitLabHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %v", mergequeue.ErrRebaseInProgress, err)
	}
	return err
}

// BranchPipelineStatus resolves the newest branch pipeline for (sha, ref).
// Push pipelines (the rebase writes a push) are preferred; api-created
// recovery pipelines are the fallback. Not-found is not an error.
func (c *GitLabClient) BranchPipelineStatus(ctx context.Context, sha, ref string) (mergequeue.PipelineStatus, error) {
	for _, source := range []string{"push", "api"} {
		pipe, found, err := c.branchPipelineForSHA(ctx, sha, ref, source)
		if err != nil {
			return mergequeue.PipelineStatus{}, err
		}
		if found {
			return mergequeue.PipelineStatus{
				ID: pipe.ID, SHA: pipe.SHA, Status: pipe.Status, WebURL: pipe.WebURL, Found: true,
			}, nil
		}
	}
	return mergequeue.PipelineStatus{}, nil
}

// MRPipelineStatus resolves the newest merge-request pipeline (source
// merge_request_event) attached to mrIID whose built SHA matches sha. The
// MR-pipelines endpoint also lists source-branch BRANCH pipelines, so the
// source filter is load-bearing: without it a manual-parked branch pipeline
// minted after the MR pipeline would shadow the green proof. Not-found is not
// an error; a pipeline record missing the source field simply never matches,
// which degrades to the branch-pipeline-only behavior.
func (c *GitLabClient) MRPipelineStatus(ctx context.Context, mrIID int64, sha string) (mergequeue.PipelineStatus, error) {
	if mrIID <= 0 {
		return mergequeue.PipelineStatus{}, errors.New("gitlab: positive MRIID required")
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return mergequeue.PipelineStatus{}, errors.New("gitlab: sha required")
	}
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/pipelines?per_page=50", c.projectPath(), mrIID)
	var pipes []shaPipeline
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipes); err != nil {
		return mergequeue.PipelineStatus{}, err
	}
	var best *shaPipeline
	for i := range pipes {
		p := &pipes[i]
		if p.SHA != sha || p.Source != "merge_request_event" {
			continue
		}
		if best == nil || p.ID > best.ID {
			best = p
		}
	}
	if best == nil {
		return mergequeue.PipelineStatus{}, nil
	}
	return mergequeue.PipelineStatus{
		ID: best.ID, SHA: best.SHA, Status: best.Status, WebURL: best.WebURL, Found: true,
	}, nil
}

// CreateQueuePipeline creates a fresh branch pipeline on ref — the merge
// queue's bounded recovery when the rebase push minted none. Same POST as the
// shepherd's CreatePipelineForRef but returns the full pipeline identity so
// the queue can verify the built SHA matches its head.
func (c *GitLabClient) CreateQueuePipeline(ctx context.Context, ref string, mrIID int64) (mergequeue.PipelineStatus, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || mrIID <= 0 {
		return mergequeue.PipelineStatus{}, errors.New("gitlab: ref and positive MR IID required")
	}
	pipe, err := c.createBranchPipeline(ctx, ref, map[string]string{
		"MILLS_MERGE_QUEUE":    "1",
		"MILLS_MERGE_QUEUE_MR": fmt.Sprintf("%d", mrIID),
	})
	if err != nil {
		return mergequeue.PipelineStatus{}, err
	}
	return mergequeue.PipelineStatus{
		ID: pipe.ID, SHA: pipe.SHA, Status: pipe.Status, WebURL: pipe.WebURL, Found: true,
	}, nil
}

func (c *GitLabClient) PipelineTiming(ctx context.Context, pipelineID int64) (mergequeue.PipelineTiming, error) {
	if pipelineID <= 0 {
		return mergequeue.PipelineTiming{}, errors.New("gitlab: positive pipeline ID required")
	}
	var pipe shaPipeline
	path := fmt.Sprintf("/projects/%s/pipelines/%d", c.projectPath(), pipelineID)
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &pipe); err != nil {
		return mergequeue.PipelineTiming{}, err
	}
	return mergequeue.PipelineTiming{Duration: pipe.Duration, QueuedDuration: pipe.QueuedDuration}, nil
}

// PrepareSpeculativeHead creates a queue-owned branch at ontoSHA, replays the
// MR commits oldest-first, then adopts or creates its explicit API pipeline.
func (c *GitLabClient) PrepareSpeculativeHead(ctx context.Context, mrIID int64, ontoSHA, ref string) (mergequeue.SpeculativeHead, error) {
	if mrIID <= 0 || strings.TrimSpace(ontoSHA) == "" || !strings.HasPrefix(ref, "mills-mq/spec-") {
		return mergequeue.SpeculativeHead{}, errors.New("gitlab: invalid speculative head request")
	}
	var commits []mergeQueueCommit
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/commits?per_page=100", c.projectPath(), mrIID)
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &commits); err != nil {
		return mergequeue.SpeculativeHead{}, err
	}
	createPath := fmt.Sprintf("/projects/%s/repository/branches?branch=%s&ref=%s", c.projectPath(), url.QueryEscape(ref), url.QueryEscape(ontoSHA))
	if err := c.requestJSON(ctx, http.MethodPost, createPath, nil, nil); err != nil {
		return mergequeue.SpeculativeHead{}, fmt.Errorf("create speculative ref: %w", err)
	}
	sha := ontoSHA
	for i := len(commits) - 1; i >= 0; i-- {
		var picked mergeQueueCommit
		path = fmt.Sprintf("/projects/%s/repository/commits/%s/cherry_pick", c.projectPath(), url.PathEscape(commits[i].ID))
		if err := c.requestJSON(ctx, http.MethodPost, path, map[string]any{"branch": ref}, &picked); err != nil {
			_ = c.DeleteQueueRef(context.WithoutCancel(ctx), ref)
			return mergequeue.SpeculativeHead{}, fmt.Errorf("cherry-pick speculative commit %s: %w", commits[i].ID, err)
		}
		if picked.ID != "" {
			sha = picked.ID
		}
	}
	ps, err := c.BranchPipelineStatus(ctx, sha, ref)
	if err != nil {
		return mergequeue.SpeculativeHead{}, err
	}
	adopted := ps.Found
	if !adopted {
		ps, err = c.CreateQueuePipeline(ctx, ref, mrIID)
		if err != nil {
			return mergequeue.SpeculativeHead{}, err
		}
	}
	return mergequeue.SpeculativeHead{Ref: ref, SHA: sha, Pipeline: ps, Adopted: adopted}, nil
}

func (c *GitLabClient) CancelQueuePipeline(ctx context.Context, pipelineID int64) error {
	if pipelineID <= 0 {
		return nil
	}
	path := fmt.Sprintf("/projects/%s/pipelines/%d/cancel", c.projectPath(), pipelineID)
	return c.requestJSON(ctx, http.MethodPost, path, nil, nil)
}

func (c *GitLabClient) DeleteQueueRef(ctx context.Context, ref string) error {
	if !strings.HasPrefix(ref, "mills-mq/spec-") {
		return errors.New("gitlab: refusing to delete non-speculative ref")
	}
	path := fmt.Sprintf("/projects/%s/repository/branches/%s", c.projectPath(), url.PathEscape(ref))
	err := c.requestJSON(ctx, http.MethodDelete, path, nil, nil)
	var httpErr *GitLabHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

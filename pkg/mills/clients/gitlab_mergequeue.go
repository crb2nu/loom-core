package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// CreateQueuePipeline creates a fresh branch pipeline on ref — the merge
// queue's bounded recovery when the rebase push minted none. Same POST as the
// shepherd's CreatePipelineForRef but returns the full pipeline identity so
// the queue can verify the built SHA matches its head.
func (c *GitLabClient) CreateQueuePipeline(ctx context.Context, ref string) (mergequeue.PipelineStatus, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return mergequeue.PipelineStatus{}, errors.New("gitlab: ref required")
	}
	pipe, err := c.createBranchPipeline(ctx, ref)
	if err != nil {
		return mergequeue.PipelineStatus{}, err
	}
	return mergequeue.PipelineStatus{
		ID: pipe.ID, SHA: pipe.SHA, Status: pipe.Status, WebURL: pipe.WebURL, Found: true,
	}, nil
}

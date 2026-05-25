package clients

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// GitBranchPusher satisfies pipeline.BranchPusher by shelling out to
// `git push` from a worktree. Used by the mr stage to publish the
// spawn agent's commits to origin before CreateMR opens the MR row.
//
// Pre-2026-05-25 Mills had no push step: the spawn agent committed in
// the NFS-shared worktree, the operator created the MR, and the
// branch never made it to GitLab — every canary MR was an empty stub
// with no head_sha. This client closes that gap.
type GitBranchPusher struct {
	Runner CommandRunner
}

// NewGitBranchPusher returns a pusher backed by the production
// CommandRunner. Pass a custom Runner via the struct literal for
// tests.
func NewGitBranchPusher() *GitBranchPusher {
	return &GitBranchPusher{Runner: execCommandRunner{}}
}

// Push satisfies pipeline.BranchPusher.
//
// We use `git push --force-with-lease origin HEAD:<branch>` so that:
//   - HEAD pushes whatever the spawn just committed, regardless of
//     which local branch ref the spawn used. The mr stage only knows
//     the contract-defined branch name; it doesn't necessarily know
//     which local ref the spawn checked out.
//   - --force-with-lease lets a retry overwrite a stale prior push
//     (e.g. earlier failed pipeline attempt) without clobbering
//     unrelated upstream work. Plain --force would be unsafe.
//   - Subsequent retries become idempotent: if HEAD already matches
//     origin/<branch>, git exits 0 ("Everything up-to-date").
func (p *GitBranchPusher) Push(ctx context.Context, workingDir, branch string) error {
	if p == nil || p.Runner == nil {
		return errors.New("branch pusher: not configured")
	}
	if workingDir == "" {
		return errors.New("branch pusher: workingDir required")
	}
	if branch == "" {
		return errors.New("branch pusher: branch required")
	}
	stdout, stderr, code, err := p.Runner.Run(ctx, workingDir,
		"git", "push", "--force-with-lease", "-u", "origin", "HEAD:"+branch)
	if err != nil {
		return fmt.Errorf("git push: %w (stderr=%q)", err, strings.TrimSpace(stderr))
	}
	if code != 0 {
		return fmt.Errorf("git push exit %d: stdout=%q stderr=%q",
			code, strings.TrimSpace(stdout), strings.TrimSpace(stderr))
	}
	return nil
}

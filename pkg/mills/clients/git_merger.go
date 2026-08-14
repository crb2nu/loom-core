package clients

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// CommandRunner is the seam between GitBranchMerger and `os/exec`. It
// lets tests inject a fake that records invocations and returns canned
// stdout/stderr/exit. Production uses execCommandRunner which shells
// out to the real `git` binary.
type CommandRunner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (stdout, stderr string, exitCode int, err error)
}

// execCommandRunner is the production CommandRunner backed by os/exec.
type execCommandRunner struct{}

// gitBranchMergeMu protects the process-wide checkout used by every
// GitBranchMerger. A field mutex would not protect two merger instances
// pointed at the same RepoRoot, and Git checkout mutates shared HEAD and
// index state across the entire merge/push transaction.
var gitBranchMergeMu sync.Mutex

func (execCommandRunner) Run(ctx context.Context, dir, name string, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			// Non-zero exit isn't necessarily an err for us — git merge
			// returns 1 on conflict. Surface stdout/stderr to caller and
			// return nil err so the caller can decide.
			err = nil
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

// GitBranchMerger satisfies pipeline.BranchMerger by shelling out to
// git. It's the production implementation of the integrator's branch-
// combination step: each parallel slice produced its own branch in its
// own worktree; we fast-forward-or-merge them onto a fresh integration
// branch off main, detecting conflicts via `git status --porcelain`.
//
// Conflict policy: any unresolved hunk in the working tree after the
// final merge is a conflict, regardless of whether earlier branches
// merged cleanly. We don't attempt clever resolution — the integrator
// escalates so a human picks up.
type GitBranchMerger struct {
	RepoRoot string
	Runner   CommandRunner
	// IntegrationPrefix is prepended to the integration branch name.
	// Defaults to "integrate/".
	IntegrationPrefix string
}

// NewGitBranchMerger returns a merger bound to repoRoot. RepoRoot must
// be the absolute path to the operator's loom-core checkout. The
// integration branch is created INSIDE this worktree, so the operator
// pod must have write access to refs/heads.
func NewGitBranchMerger(repoRoot string) *GitBranchMerger {
	return &GitBranchMerger{
		RepoRoot:          repoRoot,
		Runner:            execCommandRunner{},
		IntegrationPrefix: "integrate/",
	}
}

// Merge satisfies pipeline.BranchMerger.
//
// Sequence:
//  1. fetch origin to make sure local refs are current.
//  2. snapshot the integration branch's current remote SHA for the push lease.
//  3. create or reset the local integration branch from base.
//  4. for each slice branch, run `git merge --no-ff <slice>`.
//     - clean merge → continue to next slice.
//     - conflict → abort merge, collect conflicted files, return Conflict=true.
//  5. publish the completed integration branch to origin.
//  6. on success, return the integration HEAD sha as IntegratedSHA.
func (m *GitBranchMerger) Merge(ctx context.Context, req pipeline.MergeBranchesRequest) (pipeline.MergeBranchesResponse, error) {
	if m == nil || m.Runner == nil {
		return pipeline.MergeBranchesResponse{}, errors.New("git_merger: not configured")
	}
	if m.RepoRoot == "" {
		return pipeline.MergeBranchesResponse{}, errors.New("git_merger: RepoRoot required")
	}
	if req.IntegrationBranch == "" {
		req.IntegrationBranch = m.defaultIntegrationBranch(req.BacklogID)
	}
	if req.BaseBranch == "" {
		req.BaseBranch = "main"
	}
	if len(req.SliceBranches) == 0 {
		return pipeline.MergeBranchesResponse{}, errors.New("git_merger: no SliceBranches to merge")
	}

	gitBranchMergeMu.Lock()
	defer gitBranchMergeMu.Unlock()

	logTail := strings.Builder{}
	cmd := func(args ...string) (string, string, int, error) {
		stdout, stderr, code, err := m.Runner.Run(ctx, m.RepoRoot, "git", args...)
		fmt.Fprintf(&logTail, "$ git %s (exit %d)\n", strings.Join(args, " "), code)
		if stdout != "" {
			fmt.Fprintf(&logTail, "%s\n", strings.TrimRight(stdout, "\n"))
		}
		if stderr != "" {
			fmt.Fprintf(&logTail, "stderr: %s\n", strings.TrimRight(stderr, "\n"))
		}
		if err != nil {
			return stdout, stderr, code, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return stdout, stderr, code, nil
	}

	// 1. Fetch.
	if _, stderr, code, err := cmd("fetch", "--prune", "origin"); err != nil || code != 0 {
		return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
			gitOperationError("fetch origin failed", code, stderr, err)
	}

	// 2. Snapshot the actual remote ref immediately after this invocation's
	// fetch. The production clone is single-branch, so origin/<integration>
	// may not exist locally; ls-remote reads the authoritative ref directly.
	// Holding this SHA in a value also prevents a later background fetch from
	// weakening --force-with-lease.
	remoteHeadRef := "refs/heads/" + req.IntegrationBranch
	remoteOutput, stderr, code, err := cmd("ls-remote", "--refs", "origin", remoteHeadRef)
	if err != nil || code != 0 {
		return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
			gitOperationError("read remote integration branch failed", code, stderr, err)
	}
	remoteSHA, err := remoteRefSHA(remoteOutput, remoteHeadRef)
	if err != nil {
		return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
			fmt.Errorf("read remote integration branch failed: %w", err)
	}

	// 3. Create or reset the integration branch off the base. checkout -B is
	// idempotent when a retry starts with this same branch still checked out.
	if _, stderr, code, err := cmd(
		"checkout", "-B", req.IntegrationBranch, "origin/"+req.BaseBranch,
	); err != nil || code != 0 {
		return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
			gitOperationError("create integration branch failed", code, stderr, err)
	}

	// 4. Merge each slice branch in order.
	for _, branch := range req.SliceBranches {
		_, stderr, code, err := cmd("merge", "--no-ff", "--no-edit", branch)
		if err != nil {
			return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
				gitOperationError(fmt.Sprintf("merge slice branch %q failed", branch), code, stderr, err)
		}
		if code != 0 {
			conflicts := m.detectConflicts(ctx, &logTail)
			// Abort the failed merge so the working tree is clean for
			// caller cleanup. Best-effort.
			_, _, _, _ = cmd("merge", "--abort")
			if len(conflicts) == 0 {
				return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
					gitOperationError(fmt.Sprintf("merge slice branch %q failed", branch), code, stderr, nil)
			}
			return pipeline.MergeBranchesResponse{
				Conflict:        true,
				ConflictedFiles: conflicts,
				LogTail:         logTail.String(),
			}, nil
		}
	}

	// 5. Publish the completed branch from the real checkout. The parent
	// integration run intentionally has no WorktreePath, so the MR worker
	// cannot use its worktree-based BranchPusher fallback. An explicit lease
	// pins the expected remote value captured above; an empty expected SHA
	// means the remote ref must not exist.
	lease := "--force-with-lease=" + remoteHeadRef + ":" + remoteSHA
	pushRef := "HEAD:" + remoteHeadRef
	if _, stderr, code, err := cmd("push", lease, "origin", pushRef); err != nil || code != 0 {
		return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
			gitOperationError("publish integration branch failed", code, stderr, err)
	}

	// 6. Success: read HEAD sha as the integrated sha.
	sha, stderr, code, err := cmd("rev-parse", "HEAD")
	if err != nil || code != 0 {
		return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
			gitOperationError("resolve integration HEAD failed", code, stderr, err)
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return pipeline.MergeBranchesResponse{LogTail: logTail.String()},
			errors.New("resolve integration HEAD failed: empty SHA")
	}
	return pipeline.MergeBranchesResponse{
		IntegratedSHA: sha,
		LogTail:       logTail.String(),
	}, nil
}

func remoteRefSHA(output, wantRef string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || fields[1] != wantRef {
		return "", fmt.Errorf("unexpected ls-remote output %q", strings.TrimSpace(output))
	}
	return fields[0], nil
}

func gitOperationError(action string, code int, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if err != nil {
		if detail != "" {
			return fmt.Errorf("%s: %w (stderr=%q)", action, err, detail)
		}
		return fmt.Errorf("%s: %w", action, err)
	}
	if detail != "" {
		return fmt.Errorf("%s (exit %d): %s", action, code, detail)
	}
	return fmt.Errorf("%s (exit %d)", action, code)
}

// detectConflicts parses `git status --porcelain` for files in unmerged
// state. Each conflicted file shows up with status code "UU", "AA",
// "DD", or "U?"/"?U" depending on conflict type. We treat any of those
// as a conflict.
func (m *GitBranchMerger) detectConflicts(ctx context.Context, logTail *strings.Builder) []string {
	stdout, stderr, _, err := m.Runner.Run(ctx, m.RepoRoot, "git", "status", "--porcelain")
	if logTail != nil {
		fmt.Fprintf(logTail, "$ git status --porcelain\n%s%s\n",
			strings.TrimRight(stdout, "\n"),
			tagStderr(stderr))
	}
	if err != nil || stdout == "" {
		return nil
	}
	var conflicts []string
	for _, line := range strings.Split(stdout, "\n") {
		if len(line) < 3 {
			continue
		}
		code := line[:2]
		if isConflictCode(code) {
			conflicts = append(conflicts, strings.TrimSpace(line[3:]))
		}
	}
	return conflicts
}

func isConflictCode(c string) bool {
	switch c {
	case "UU", "AA", "DD", "AU", "UA", "UD", "DU":
		return true
	}
	return false
}

func tagStderr(s string) string {
	if s == "" {
		return ""
	}
	return "\nstderr: " + strings.TrimRight(s, "\n")
}

func (m *GitBranchMerger) defaultIntegrationBranch(backlogID string) string {
	prefix := m.IntegrationPrefix
	if prefix == "" {
		prefix = "integrate/"
	}
	return prefix + backlogID
}

// Compile-time interface assertion.
var _ pipeline.BranchMerger = (*GitBranchMerger)(nil)

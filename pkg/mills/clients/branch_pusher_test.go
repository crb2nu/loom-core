package clients

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// captureRunner is a CommandRunner that records every call and returns
// canned stdout/stderr/exitCode. Used to pin the push command shape
// without shelling out to a real `git` binary.
type captureRunner struct {
	mu     sync.Mutex
	calls  []captureCall
	stdout string
	stderr string
	exit   int
	err    error
}

type captureCall struct {
	Dir  string
	Name string
	Args []string
}

func (r *captureRunner) Run(_ context.Context, dir, name string, args ...string) (string, string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, captureCall{Dir: dir, Name: name, Args: append([]string{}, args...)})
	return r.stdout, r.stderr, r.exit, r.err
}

func TestGitBranchPusher_PushesHEADWithForceWithLease(t *testing.T) {
	r := &captureRunner{}
	p := &GitBranchPusher{Runner: r}

	err := p.Push(context.Background(), "/worktrees/canary-x", "feat/MILLS-CANARY-123")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	c := r.calls[0]
	if c.Dir != "/worktrees/canary-x" {
		t.Errorf("dir = %q", c.Dir)
	}
	if c.Name != "git" {
		t.Errorf("name = %q, want git", c.Name)
	}
	want := []string{"push", "--force-with-lease", "-u", "origin", "HEAD:feat/MILLS-CANARY-123"}
	if strings.Join(c.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", c.Args, want)
	}
}

func TestGitBranchPusher_PropagatesNonZeroExit(t *testing.T) {
	r := &captureRunner{exit: 128, stderr: "fatal: invalid refspec\n"}
	p := &GitBranchPusher{Runner: r}

	err := p.Push(context.Background(), "/worktrees/x", "feat/y")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit 128") {
		t.Errorf("error missing exit code: %v", err)
	}
	if !strings.Contains(err.Error(), "fatal: invalid refspec") {
		t.Errorf("error missing stderr: %v", err)
	}
}

func TestGitBranchPusher_PropagatesProcessError(t *testing.T) {
	r := &captureRunner{err: errors.New("exec: \"git\": not found")}
	p := &GitBranchPusher{Runner: r}

	err := p.Push(context.Background(), "/worktrees/x", "feat/y")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error missing underlying: %v", err)
	}
}

func TestGitBranchPusher_RequiresInputs(t *testing.T) {
	cases := []struct {
		name       string
		pusher     *GitBranchPusher
		workingDir string
		branch     string
		wantSub    string
	}{
		{"nil receiver", nil, "/x", "feat/x", "not configured"},
		{"nil runner", &GitBranchPusher{Runner: nil}, "/x", "feat/x", "not configured"},
		{"empty workingDir", &GitBranchPusher{Runner: &captureRunner{}}, "", "feat/x", "workingDir"},
		{"empty branch", &GitBranchPusher{Runner: &captureRunner{}}, "/x", "", "branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pusher.Push(context.Background(), tc.workingDir, tc.branch)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q missing %q", err, tc.wantSub)
			}
		})
	}
}

package clients

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// fakeGitRunner records every git command and returns canned stdout/
// stderr/exit code keyed on the args. Tests register expectations
// before calling Merge.
type fakeGitRunner struct {
	mu       sync.Mutex
	calls    [][]string
	callDirs []string
	// scripted maps "subcommand" or full args joined by space → response
	scripted map[string]gitResponse
	// fallback applies when no key matches.
	fallback gitResponse
}

type gitResponse struct {
	stdout string
	stderr string
	code   int
	err    error
}

func (r *fakeGitRunner) Run(_ context.Context, dir string, name string, args ...string) (string, string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.callDirs = append(r.callDirs, dir)
	for prefix, resp := range r.scripted {
		if strings.HasPrefix(strings.Join(args, " "), prefix) {
			return resp.stdout, resp.stderr, resp.code, resp.err
		}
	}
	return r.fallback.stdout, r.fallback.stderr, r.fallback.code, r.fallback.err
}

func (r *fakeGitRunner) gitCallDirs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.callDirs...)
}

func (r *fakeGitRunner) gitCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// callsContain returns true when one of the recorded calls starts with
// the given args[1:] (i.e. matches `git <args...>`).
func (r *fakeGitRunner) callsContain(prefix ...string) bool {
	for _, call := range r.gitCalls() {
		if len(call) < 1+len(prefix) || call[0] != "git" {
			continue
		}
		ok := true
		for i, a := range prefix {
			if call[1+i] != a {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func newGitMerger(runner CommandRunner) *GitBranchMerger {
	return &GitBranchMerger{
		RepoRoot:          "/workspace/loom-core",
		Runner:            runner,
		IntegrationPrefix: "integrate/",
	}
}

// ----- Config validation -----

func TestGitBranchMerger_RejectsMissingConfig(t *testing.T) {
	m := &GitBranchMerger{}
	if _, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{}); err == nil {
		t.Error("expected error when Runner nil")
	}
	m2 := &GitBranchMerger{Runner: &fakeGitRunner{}}
	if _, err := m2.Merge(context.Background(), pipeline.MergeBranchesRequest{}); err == nil {
		t.Error("expected error when RepoRoot empty")
	}
	m3 := newGitMerger(&fakeGitRunner{})
	if _, err := m3.Merge(context.Background(), pipeline.MergeBranchesRequest{BacklogID: "BL"}); err == nil {
		t.Error("expected error when SliceBranches empty")
	}
}

// ----- Happy path -----

func TestGitBranchMerger_HappyPathProducesIntegratedSHA(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			"ls-remote --refs origin refs/heads/integrate/BL-X": {
				stdout: "0123456789012345678901234567890123456789\trefs/heads/integrate/BL-X\n",
			},
			"rev-parse HEAD": {stdout: "deadbeef1234\n"},
		},
	}
	m := newGitMerger(runner)
	resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:     "BL-X",
		BaseBranch:    "main",
		SliceBranches: []string{"feat/BL-X/alpha", "feat/BL-X/beta"},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if resp.Conflict {
		t.Errorf("expected clean merge")
	}
	if resp.IntegratedSHA != "deadbeef1234" {
		t.Errorf("IntegratedSHA = %q", resp.IntegratedSHA)
	}
	// Pin the ordering: publishing before all slices are merged could
	// expose a partial integration branch to the MR stage.
	wantCalls := [][]string{
		{"git", "fetch", "--prune", "origin"},
		{"git", "ls-remote", "--refs", "origin", "refs/heads/integrate/BL-X"},
		{"git", "checkout", "-B", "integrate/BL-X", "origin/main"},
		{"git", "merge", "--no-ff", "--no-edit", "feat/BL-X/alpha"},
		{"git", "merge", "--no-ff", "--no-edit", "feat/BL-X/beta"},
		{
			"git", "push",
			"--force-with-lease=refs/heads/integrate/BL-X:0123456789012345678901234567890123456789",
			"origin", "HEAD:refs/heads/integrate/BL-X",
		},
		{"git", "rev-parse", "HEAD"},
	}
	if got := runner.gitCalls(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("git calls = %#v, want %#v", got, wantCalls)
	}
	callDirs := runner.gitCallDirs()
	if len(callDirs) != len(wantCalls) {
		t.Fatalf("git call dirs = %d, want %d", len(callDirs), len(wantCalls))
	}
	if got := callDirs[5]; got != m.RepoRoot {
		t.Errorf("push dir = %q, want RepoRoot %q", got, m.RepoRoot)
	}
	if !strings.Contains(resp.LogTail, "git fetch") {
		t.Error("LogTail should record commands")
	}
}

func TestGitBranchMerger_DefaultsBaseBranchToMain(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			"rev-parse HEAD": {stdout: "abc\n"},
		},
	}
	m := newGitMerger(runner)
	if _, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:     "BL-Y",
		SliceBranches: []string{"feat/BL-Y/x"},
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !runner.callsContain("checkout", "-B", "integrate/BL-Y", "origin/main") {
		t.Error("expected default base branch=main")
	}
}

func TestGitBranchMerger_HonoursExplicitIntegrationBranch(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			"rev-parse HEAD": {stdout: "x\n"},
		},
	}
	m := newGitMerger(runner)
	if _, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:         "BL-Z",
		IntegrationBranch: "merge/special-branch",
		SliceBranches:     []string{"feat/BL-Z/a"},
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !runner.callsContain("checkout", "-B", "merge/special-branch", "origin/main") {
		t.Error("explicit IntegrationBranch override not honoured")
	}
}

// ----- Conflict path -----

func TestGitBranchMerger_DetectsConflictAndAborts(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			// First slice merges fine; second conflicts.
			"merge --no-ff --no-edit feat/BL/alpha": {stdout: "Merge made\n"},
			"merge --no-ff --no-edit feat/BL/beta":  {stdout: "CONFLICT in foo.go\n", code: 1},
			"status --porcelain":                    {stdout: "UU foo.go\nUU bar.go\nM  baz.go\n"},
		},
	}
	m := newGitMerger(runner)
	resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:     "BL",
		SliceBranches: []string{"feat/BL/alpha", "feat/BL/beta"},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !resp.Conflict {
		t.Errorf("expected Conflict=true")
	}
	if len(resp.ConflictedFiles) != 2 {
		t.Errorf("conflicted files = %v, want [foo.go, bar.go]", resp.ConflictedFiles)
	}
	if resp.ConflictedFiles[0] != "foo.go" || resp.ConflictedFiles[1] != "bar.go" {
		t.Errorf("conflicted files wrong: %v", resp.ConflictedFiles)
	}
	if !runner.callsContain("merge", "--abort") {
		t.Error("expected `git merge --abort` after conflict")
	}
}

func TestGitBranchMerger_ResetsIntegrationBranchForRetry(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			"rev-parse HEAD": {stdout: "ok\n"},
		},
	}
	m := newGitMerger(runner)
	resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:     "BL-X",
		SliceBranches: []string{"feat/BL-X/x"},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if resp.Conflict {
		t.Error("expected clean merge")
	}
	if !runner.callsContain("checkout", "-B", "integrate/BL-X", "origin/main") {
		t.Error("expected idempotent branch reset via checkout -B")
	}
	if runner.callsContain("branch", "-D", "integrate/BL-X") {
		t.Error("must not delete a branch that may already be checked out")
	}
}

func TestGitBranchMerger_FetchFailureSurfacesError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response gitResponse
		wantErr  string
	}{
		{
			name:     "non-zero exit",
			response: gitResponse{stderr: "fatal: authentication failed", code: 128},
			wantErr:  "fetch origin failed (exit 128): fatal: authentication failed",
		},
		{
			name:     "process error",
			response: gitResponse{stderr: "ssh transport failed", err: errFakeNetwork},
			wantErr:  "fetch origin failed: git fetch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeGitRunner{
				scripted: map[string]gitResponse{
					"fetch --prune origin": tc.response,
				},
			}
			m := newGitMerger(runner)
			resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
				BacklogID:     "BL",
				SliceBranches: []string{"x"},
			})
			if err == nil {
				t.Fatal("expected error from fetch failure")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.response.stderr) {
				t.Errorf("error missing actionable stderr %q: %v", tc.response.stderr, err)
			}
			if !strings.Contains(resp.LogTail, tc.response.stderr) {
				t.Errorf("LogTail missing fetch failure: %q", resp.LogTail)
			}
			if runner.callsContain("ls-remote") {
				t.Error("remote ref must not be read after fetch failure")
			}
		})
	}
}

func TestGitBranchMerger_RemoteRefLookupFailureSurfacesError(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			"ls-remote --refs origin refs/heads/integrate/BL": {
				stderr: "fatal: remote unavailable",
				code:   128,
			},
		},
	}
	m := newGitMerger(runner)
	resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:     "BL",
		SliceBranches: []string{"x"},
	})
	if err == nil || !strings.Contains(err.Error(), "read remote integration branch failed (exit 128): fatal: remote unavailable") {
		t.Fatalf("remote ref error = %v", err)
	}
	if !strings.Contains(resp.LogTail, "fatal: remote unavailable") {
		t.Errorf("LogTail missing remote lookup failure: %q", resp.LogTail)
	}
	if runner.callsContain("checkout") {
		t.Error("checkout must not run after remote ref lookup failure")
	}
}

func TestGitBranchMerger_CheckoutFailureSurfacesError(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			"checkout -B integrate/BL origin/main": {stderr: "boom", code: 128},
		},
	}
	m := newGitMerger(runner)
	_, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:     "BL",
		SliceBranches: []string{"x"},
	})
	if err == nil {
		t.Error("expected error when checkout fails")
	}
}

func TestGitBranchMerger_NonConflictMergeFailureSurfacesError(t *testing.T) {
	runner := &fakeGitRunner{
		scripted: map[string]gitResponse{
			"merge --no-ff --no-edit missing": {
				stderr: "merge: missing - not something we can merge",
				code:   1,
			},
		},
	}
	m := newGitMerger(runner)
	resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
		BacklogID:     "BL",
		SliceBranches: []string{"missing"},
	})
	if err == nil || !strings.Contains(err.Error(), "merge slice branch \"missing\" failed (exit 1)") {
		t.Fatalf("merge error = %v", err)
	}
	if resp.Conflict {
		t.Error("a missing branch is not a merge conflict")
	}
	if !runner.callsContain("merge", "--abort") {
		t.Error("failed merge should be aborted before returning")
	}
}

func TestGitBranchMerger_PushFailureSurfacesError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		response    gitResponse
		wantErr     string
		wantLogTail string
	}{
		{
			name:        "non-zero exit",
			response:    gitResponse{stderr: "remote rejected integration branch", code: 1},
			wantErr:     "publish integration branch failed (exit 1): remote rejected integration branch",
			wantLogTail: "remote rejected integration branch",
		},
		{
			name:        "process error",
			response:    gitResponse{stderr: "ssh transport failed", err: errFakeNetwork},
			wantErr:     "publish integration branch failed: git push",
			wantLogTail: "ssh transport failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeGitRunner{
				scripted: map[string]gitResponse{
					"push --force-with-lease=refs/heads/integrate/BL: origin HEAD:refs/heads/integrate/BL": tc.response,
					"rev-parse HEAD": {stdout: "must-not-be-read\n"},
				},
			}
			m := newGitMerger(runner)
			resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
				BacklogID:     "BL",
				SliceBranches: []string{"feat/BL/x"},
			})
			if err == nil {
				t.Fatal("expected error when publishing the integration branch fails")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("unexpected error: %v", err)
			}
			if !strings.Contains(err.Error(), tc.response.stderr) {
				t.Errorf("error missing actionable stderr %q: %v", tc.response.stderr, err)
			}
			if !strings.Contains(resp.LogTail, tc.wantLogTail) {
				t.Errorf("LogTail missing push failure: %q", resp.LogTail)
			}
			if runner.callsContain("rev-parse", "HEAD") {
				t.Error("rev-parse must not run after a failed push")
			}
		})
	}
}

func TestGitBranchMerger_HeadResolutionFailureSurfacesError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response gitResponse
		wantErr  string
	}{
		{
			name:     "non-zero exit",
			response: gitResponse{stderr: "fatal: bad revision", code: 128},
			wantErr:  "resolve integration HEAD failed (exit 128): fatal: bad revision",
		},
		{
			name:     "empty SHA",
			response: gitResponse{stdout: "\n"},
			wantErr:  "resolve integration HEAD failed: empty SHA",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeGitRunner{
				scripted: map[string]gitResponse{
					"rev-parse HEAD": tc.response,
				},
			}
			m := newGitMerger(runner)
			resp, err := m.Merge(context.Background(), pipeline.MergeBranchesRequest{
				BacklogID:     "BL",
				SliceBranches: []string{"feat/BL/x"},
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("HEAD resolution error = %v", err)
			}
			if tc.response.stderr != "" && !strings.Contains(resp.LogTail, tc.response.stderr) {
				t.Errorf("LogTail missing HEAD resolution failure: %q", resp.LogTail)
			}
		})
	}
}

func TestGitBranchMerger_RealRepoSequentialRetries(t *testing.T) {
	repoRoot, remote, req := newRealGitMergerRepo(t)
	m := NewGitBranchMerger(repoRoot)

	first, err := m.Merge(context.Background(), req)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if first.IntegratedSHA == "" {
		t.Fatal("first merge returned an empty SHA")
	}
	emptyLease := "--force-with-lease=refs/heads/" + req.IntegrationBranch + ": origin"
	if !strings.Contains(first.LogTail, emptyLease) {
		t.Fatalf("first merge did not require a missing remote ref:\n%s", first.LogTail)
	}

	// Retry while the integration branch is still checked out. checkout -B
	// must reset it cleanly, and the lease must use the SHA fetched by this
	// invocation rather than mutable remote-tracking state.
	second, err := m.Merge(context.Background(), req)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	wantLease := "--force-with-lease=refs/heads/" + req.IntegrationBranch + ":" + first.IntegratedSHA
	if !strings.Contains(second.LogTail, wantLease) {
		t.Fatalf("second merge lease missing fetched SHA %q:\n%s", first.IntegratedSHA, second.LogTail)
	}
	remoteSHA := runGitMergerTestGit(t, repoRoot,
		"--git-dir="+remote, "rev-parse", "refs/heads/"+req.IntegrationBranch)
	if remoteSHA != second.IntegratedSHA {
		t.Errorf("remote integration SHA = %q, want %q", remoteSHA, second.IntegratedSHA)
	}
	if branch := runGitMergerTestGit(t, repoRoot, "branch", "--show-current"); branch != req.IntegrationBranch {
		t.Errorf("current branch = %q, want %q", branch, req.IntegrationBranch)
	}
}

func TestGitBranchMerger_RealRepoConcurrentCallsAreSerialized(t *testing.T) {
	repoRoot, remote, firstReq := newRealGitMergerRepo(t)
	secondReq := firstReq
	secondReq.BacklogID = "BL-REAL-B"
	secondReq.IntegrationBranch = "integrate/BL-REAL-B"
	secondReq.SliceBranches = []string{"feat/BL-REAL/gamma", "feat/BL-REAL/delta"}
	probe := newSerializationProbeRunner()
	t.Cleanup(probe.releaseFirst)

	firstMerger := NewGitBranchMerger(repoRoot)
	firstMerger.Runner = probe
	secondMerger := NewGitBranchMerger(repoRoot)
	secondMerger.Runner = probe

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type mergeResult struct {
		name     string
		response pipeline.MergeBranchesResponse
		err      error
	}
	results := make(chan mergeResult, 2)
	go func() {
		response, err := firstMerger.Merge(ctx, firstReq)
		results <- mergeResult{name: "first", response: response, err: err}
	}()
	select {
	case <-probe.firstBlocked:
	case <-ctx.Done():
		t.Fatalf("first merge did not reach git: %v", ctx.Err())
	}

	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		response, err := secondMerger.Merge(ctx, secondReq)
		results <- mergeResult{name: "second", response: response, err: err}
	}()
	<-secondStarted

	overlapped := false
	select {
	case <-probe.overlap:
		overlapped = true
	case <-time.After(250 * time.Millisecond):
	}
	probe.releaseFirst()

	got := make(map[string]pipeline.MergeBranchesResponse, 2)
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Errorf("concurrent merge %q: %v", result.name, result.err)
				continue
			}
			got[result.name] = result.response
		case <-ctx.Done():
			t.Fatalf("concurrent merges did not finish: %v", ctx.Err())
		}
	}
	if overlapped {
		t.Fatal("two Merge calls entered the shared RepoRoot git sequence concurrently")
	}

	assertRemoteIntegrationTree(t, repoRoot, remote, firstReq, got["first"],
		[]string{"alpha.txt", "beta.txt"}, []string{"gamma.txt", "delta.txt"})
	assertRemoteIntegrationTree(t, repoRoot, remote, secondReq, got["second"],
		[]string{"gamma.txt", "delta.txt"}, []string{"alpha.txt", "beta.txt"})
}

type serializationProbeRunner struct {
	delegate CommandRunner

	mu          sync.Mutex
	active      int
	firstOnce   sync.Once
	overlapOnce sync.Once
	releaseOnce sync.Once

	firstBlocked chan struct{}
	release      chan struct{}
	overlap      chan struct{}
}

func newSerializationProbeRunner() *serializationProbeRunner {
	return &serializationProbeRunner{
		delegate:     execCommandRunner{},
		firstBlocked: make(chan struct{}),
		release:      make(chan struct{}),
		overlap:      make(chan struct{}),
	}
}

func (r *serializationProbeRunner) Run(
	ctx context.Context,
	dir string,
	name string,
	args ...string,
) (string, string, int, error) {
	r.mu.Lock()
	r.active++
	if r.active > 1 {
		r.overlapOnce.Do(func() { close(r.overlap) })
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active--
		r.mu.Unlock()
	}()

	block := false
	r.firstOnce.Do(func() { block = true })
	if block {
		close(r.firstBlocked)
		select {
		case <-r.release:
		case <-ctx.Done():
			return "", "", 0, ctx.Err()
		}
	}
	return r.delegate.Run(ctx, dir, name, args...)
}

func (r *serializationProbeRunner) releaseFirst() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func assertRemoteIntegrationTree(
	t *testing.T,
	repoRoot string,
	remote string,
	req pipeline.MergeBranchesRequest,
	response pipeline.MergeBranchesResponse,
	wantFiles []string,
	unwantedFiles []string,
) {
	t.Helper()
	if response.IntegratedSHA == "" {
		t.Errorf("%s returned an empty integration SHA", req.IntegrationBranch)
		return
	}
	remoteRef := "refs/heads/" + req.IntegrationBranch
	remoteSHA := runGitMergerTestGit(t, repoRoot, "--git-dir="+remote, "rev-parse", remoteRef)
	if remoteSHA != response.IntegratedSHA {
		t.Errorf("%s remote SHA = %q, want %q", req.IntegrationBranch, remoteSHA, response.IntegratedSHA)
	}
	tree := runGitMergerTestGit(t, repoRoot, "--git-dir="+remote, "ls-tree", "-r", "--name-only", remoteRef)
	for _, name := range wantFiles {
		if !linePresent(tree, name) {
			t.Errorf("%s tree missing %q:\n%s", req.IntegrationBranch, name, tree)
		}
	}
	for _, name := range unwantedFiles {
		if linePresent(tree, name) {
			t.Errorf("%s tree unexpectedly contains %q:\n%s", req.IntegrationBranch, name, tree)
		}
	}
}

func linePresent(lines, want string) bool {
	for _, line := range strings.Split(lines, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func newRealGitMergerRepo(t *testing.T) (repoRoot, remote string, req pipeline.MergeBranchesRequest) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "origin.git")
	repoRoot = filepath.Join(root, "repo")

	runGitMergerTestGit(t, root, "init", "--bare", remote)
	runGitMergerTestGit(t, root, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitMergerTestGit(t, root, "init", repoRoot)
	runGitMergerTestGit(t, repoRoot, "config", "user.name", "Mills Test")
	runGitMergerTestGit(t, repoRoot, "config", "user.email", "mills-test@example.invalid")
	runGitMergerTestGit(t, repoRoot, "config", "commit.gpgsign", "false")
	hooksDir := filepath.Join(root, "hooks")
	if err := os.Mkdir(hooksDir, 0o755); err != nil {
		t.Fatalf("create empty hooks dir: %v", err)
	}
	runGitMergerTestGit(t, repoRoot, "config", "core.hooksPath", hooksDir)
	runGitMergerTestGit(t, repoRoot, "checkout", "-b", "main")
	writeGitMergerTestFile(t, repoRoot, "base.txt", "base\n")
	runGitMergerTestGit(t, repoRoot, "add", "base.txt")
	runGitMergerTestGit(t, repoRoot, "commit", "-m", "seed main")
	runGitMergerTestGit(t, repoRoot, "remote", "add", "origin", remote)
	runGitMergerTestGit(t, repoRoot, "push", "-u", "origin", "main")

	slices := []struct {
		branch  string
		file    string
		content string
	}{
		{"feat/BL-REAL/alpha", "alpha.txt", "alpha\n"},
		{"feat/BL-REAL/beta", "beta.txt", "beta\n"},
		{"feat/BL-REAL/gamma", "gamma.txt", "gamma\n"},
		{"feat/BL-REAL/delta", "delta.txt", "delta\n"},
	}
	for _, slice := range slices {
		runGitMergerTestGit(t, repoRoot, "checkout", "-b", slice.branch, "main")
		writeGitMergerTestFile(t, repoRoot, slice.file, slice.content)
		runGitMergerTestGit(t, repoRoot, "add", slice.file)
		runGitMergerTestGit(t, repoRoot, "commit", "-m", "add "+slice.file)
	}
	runGitMergerTestGit(t, repoRoot, "checkout", "main")

	return repoRoot, remote, pipeline.MergeBranchesRequest{
		BacklogID:         "BL-REAL",
		IntegrationBranch: "integrate/BL-REAL",
		SliceBranches:     []string{slices[0].branch, slices[1].branch},
		BaseBranch:        "main",
	}
}

func runGitMergerTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(cleanGitMergerTestEnv(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func cleanGitMergerTestEnv() []string {
	env := os.Environ()
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_DIR=") ||
			strings.HasPrefix(entry, "GIT_WORK_TREE=") ||
			strings.HasPrefix(entry, "GIT_INDEX_FILE=") {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

func writeGitMergerTestFile(t *testing.T, repoRoot, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoRoot, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestRemoteRefSHA(t *testing.T) {
	const ref = "refs/heads/integrate/BL"
	for _, tc := range []struct {
		name    string
		output  string
		wantSHA string
		wantErr bool
	}{
		{name: "absent", output: "", wantSHA: ""},
		{
			name:    "present",
			output:  "0123456789012345678901234567890123456789\t" + ref + "\n",
			wantSHA: "0123456789012345678901234567890123456789",
		},
		{name: "wrong ref", output: "deadbeef\trefs/heads/other\n", wantErr: true},
		{name: "malformed", output: "deadbeef\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := remoteRefSHA(tc.output, ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("remoteRefSHA() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.wantSHA {
				t.Errorf("remoteRefSHA() = %q, want %q", got, tc.wantSHA)
			}
		})
	}
}

// ----- Conflict-code parser -----

func TestIsConflictCode(t *testing.T) {
	for _, c := range []string{"UU", "AA", "DD", "AU", "UA", "UD", "DU"} {
		if !isConflictCode(c) {
			t.Errorf("expected %q to be a conflict code", c)
		}
	}
	for _, c := range []string{"M ", " M", "??", "A ", "MM"} {
		if isConflictCode(c) {
			t.Errorf("expected %q NOT to be a conflict code", c)
		}
	}
}

// errFakeNetwork is a sentinel test error used to drive transient
// failures through the runner.
var errFakeNetwork = &fakeNetworkErr{}

type fakeNetworkErr struct{}

func (e *fakeNetworkErr) Error() string { return "fake network error" }

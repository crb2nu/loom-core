// proxy_mrtrailer.go — MR-status trailer injection on git/gitlab tool results
// (slice M3b of the "no MR left behind" spec).
//
// When a proxied session runs a git push/status/commit or a gitlab
// merge-request tool, the proxy consults the HUD mrwatch registry (M1,
// GET /api/agent/mr-status?branch=<current>) for the working directory's
// current branch and appends a short trailer to the tool result text:
//
//	[loom] MR !<IID> (<branch>): <state> — <one-line action hint>
//
// This is the same result-mutation mechanism as the existing truncation
// trailer (see proxy_truncate.go) and works across every proxied client.
//
// Design rules (from the spec):
//   - Fail-open: HUD unreachable / branch unknown / any error → no trailer,
//     no error, no added latency beyond the 2s HUD timeout.
//   - Delta-gated per proxy process: append only when the branch has an
//     unhealthy MR, or when the state changed since it was last shown.
//   - Cheap: branch is read from .git/HEAD and the HUD response is cached,
//     each with a ~30s TTL, so a burst of git calls does not re-hit the HUD.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

const (
	// mrTrailerBranchTTL bounds how often the proxy re-reads .git/HEAD.
	mrTrailerBranchTTL = 30 * time.Second
	// mrTrailerHUDTTL bounds how often the proxy re-hits the HUD per branch.
	mrTrailerHUDTTL = 30 * time.Second
	// mrTrailerHUDTimeout is the short per-request budget for the HUD call.
	mrTrailerHUDTimeout = 2 * time.Second
)

// mrTrailerTools is the set of proxied (server → tool) pairs whose results
// get an MR-status trailer. Nothing else is touched.
var mrTrailerTools = map[string]map[string]struct{}{
	"git": {
		"git_push":   {},
		"git_status": {},
		"git_commit": {},
	},
	"gitlab": {
		"create_merge_request": {},
		"get_merge_request":    {},
		"merge_merge_request":  {},
		"list_merge_requests":  {},
	},
}

// mrTrailerUnhealthy is the set of states that always warrant a trailer (they
// are actionable stalls). Mirrors the spec's unhealthy taxonomy; kept as a
// local set so the proxy does not import the HUD registry package.
var mrTrailerUnhealthy = map[string]struct{}{
	"conflict":                {},
	"ci_failed_flaky":         {},
	"ci_failed_deterministic": {},
	"automerge_unarmed":       {},
	"pipeline_skipped":        {},
	"stale_branch":            {},
}

// mrTrailerMR is the minimal view of one classified MR the proxy needs. It
// mirrors the JSON emitted by GET /api/agent/mr-status (M1
// internal/hud/domain/mrwatch.BranchStatusResponse.MergeRequests[*]).
type mrTrailerMR struct {
	IID          int64  `json:"iid"`
	SourceBranch string `json:"source_branch"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	WebURL       string `json:"web_url"`
}

// mrTrailerResponse is the minimal decode target for the HUD branch-status
// endpoint.
type mrTrailerResponse struct {
	Branch        string        `json:"branch"`
	MergeRequests []mrTrailerMR `json:"merge_requests"`
}

// mrTrailerActionHint maps a classified state to a one-line action hint.
func mrTrailerActionHint(state string) string {
	switch state {
	case "conflict":
		return "rebase onto target and resolve conflicts"
	case "ci_failed_flaky":
		return "retry the pipeline (transient failure)"
	case "ci_failed_deterministic":
		return "fix the failing CI job"
	case "automerge_unarmed":
		return "arm auto-merge (MWPS)"
	case "pipeline_skipped":
		return "create a pipeline for the head ref"
	case "stale_branch":
		return "rebase; branch is behind target"
	case "draft_idle":
		return "mark ready for review when done"
	case "awaiting_pipeline":
		return "waiting for the head pipeline to start"
	case "ci_running":
		return "CI is running"
	case "ok":
		return "healthy — auto-merge armed"
	case "merged":
		return "merged — nothing to do"
	case "closed":
		return "closed without merging"
	default:
		return "check MR status"
	}
}

// mrTrailerIsTriggerTool reports whether a (server, tool) pair should receive
// an MR-status trailer.
func mrTrailerIsTriggerTool(server, tool string) bool {
	tools, ok := mrTrailerTools[server]
	if !ok {
		return false
	}
	_, ok = tools[tool]
	return ok
}

// mrTrailer holds the per-process delta-gating state and the branch/HUD
// caches. All fields are guarded by mu. The *Fn seams are nil in production
// (defaults used) and injected in tests.
type mrTrailer struct {
	mu sync.Mutex

	// lastShown maps a branch to the hash of the MR state last displayed for
	// it, so a healthy/unchanged state is shown at most once (on transition).
	lastShown map[string]string

	// branch detection cache
	branchVal     string
	branchExpires time.Time

	// per-branch HUD response cache
	hudCache map[string]mrTrailerCacheEntry

	// injectable seams (nil → production behavior)
	nowFn    func() time.Time
	branchFn func() string
	fetchFn  func(branch string) ([]mrTrailerMR, bool)
}

type mrTrailerCacheEntry struct {
	mrs     []mrTrailerMR
	ok      bool
	expires time.Time
}

func newMRTrailer() *mrTrailer {
	return &mrTrailer{
		lastShown: map[string]string{},
		hudCache:  map[string]mrTrailerCacheEntry{},
	}
}

// defaultMRTrailer is the process-wide instance used by the proxy handler.
var defaultMRTrailer = newMRTrailer()

func (t *mrTrailer) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

func (t *mrTrailer) branch() string {
	if t.branchFn != nil {
		return t.branchFn()
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return readGitBranch(wd)
}

func (t *mrTrailer) fetch(branch string) ([]mrTrailerMR, bool) {
	if t.fetchFn != nil {
		return t.fetchFn(branch)
	}
	return fetchMRStatusFromHUD(branch)
}

// currentBranch returns the working directory's branch, cached for ~30s.
func (t *mrTrailer) currentBranch() string {
	now := t.now()

	t.mu.Lock()
	if now.Before(t.branchExpires) {
		v := t.branchVal
		t.mu.Unlock()
		return v
	}
	t.mu.Unlock()

	v := t.branch()

	t.mu.Lock()
	t.branchVal = v
	t.branchExpires = now.Add(mrTrailerBranchTTL)
	t.mu.Unlock()
	return v
}

// fetchStatus returns the classified MRs for a branch, cached for ~30s. The
// cache stores failures too, so a down HUD is not re-hit on every git call.
func (t *mrTrailer) fetchStatus(branch string) ([]mrTrailerMR, bool) {
	now := t.now()

	t.mu.Lock()
	if e, ok := t.hudCache[branch]; ok && now.Before(e.expires) {
		t.mu.Unlock()
		return e.mrs, e.ok
	}
	t.mu.Unlock()

	// Network call happens outside the lock.
	mrs, ok := t.fetch(branch)

	t.mu.Lock()
	t.hudCache[branch] = mrTrailerCacheEntry{mrs: mrs, ok: ok, expires: now.Add(mrTrailerHUDTTL)}
	t.mu.Unlock()
	return mrs, ok
}

// maybeAppend consults the HUD and, subject to delta-gating, appends an
// MR-status trailer to result. It returns true iff the result was mutated.
// Fail-open: every unknown/error path returns false without mutating.
func (t *mrTrailer) maybeAppend(result *mcp.CallToolResult, server, tool string) bool {
	if result == nil || !mrTrailerIsTriggerTool(server, tool) {
		return false
	}

	branch := t.currentBranch()
	if branch == "" {
		return false // detached HEAD or non-repo cwd → fail-open
	}

	mrs, ok := t.fetchStatus(branch)
	if !ok {
		return false // HUD error → fail-open
	}

	// Keep only MRs whose source branch matches (the endpoint already filters,
	// but stay defensive) and sort by IID for a deterministic trailer + hash.
	matches := make([]mrTrailerMR, 0, len(mrs))
	for _, mr := range mrs {
		if mr.SourceBranch == branch {
			matches = append(matches, mr)
		}
	}
	if len(matches) == 0 {
		return false // no MR on this branch → nothing to say
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].IID < matches[j].IID })

	hash := mrTrailerHash(matches)
	unhealthy := false
	lines := make([]string, 0, len(matches))
	for _, mr := range matches {
		if _, bad := mrTrailerUnhealthy[mr.State]; bad {
			unhealthy = true
		}
		lines = append(lines, fmt.Sprintf("[loom] MR !%d (%s): %s — %s",
			mr.IID, mr.SourceBranch, mr.State, mrTrailerActionHint(mr.State)))
	}

	// Delta-gating: show when unhealthy, or when the state changed since the
	// last trailer shown for this branch.
	t.mu.Lock()
	last := t.lastShown[branch]
	show := unhealthy || hash != last
	if show {
		t.lastShown[branch] = hash
	}
	t.mu.Unlock()

	if !show {
		return false
	}

	mrTrailerAppendText(result, "\n"+strings.Join(lines, "\n"))
	return true
}

// mrTrailerHash is a stable digest of the branch's MR states, used to detect
// transitions. Input must be sorted by IID.
func mrTrailerHash(mrs []mrTrailerMR) string {
	h := sha1.New()
	for _, mr := range mrs {
		fmt.Fprintf(h, "%d:%s\n", mr.IID, mr.State)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// mrTrailerAppendText appends text to the last text content item, or adds a
// new text content item when the result has no text content (e.g. an
// image-only result). The leading newline is dropped for a fresh item.
func mrTrailerAppendText(result *mcp.CallToolResult, text string) {
	for i := len(result.Content) - 1; i >= 0; i-- {
		if result.Content[i].Type == "text" {
			result.Content[i].Text += text
			return
		}
	}
	result.Content = append(result.Content, mcp.Content{
		Type: "text",
		Text: strings.TrimPrefix(text, "\n"),
	})
}

// fetchMRStatusFromHUD calls the M1 branch-status endpoint using the shared
// HUD base-URL resolution (same as the CLI/heartbeat path). Returns ok=false
// on any transport or decode error so the caller fails open.
func fetchMRStatusFromHUD(branch string) ([]mrTrailerMR, bool) {
	raw, err := hudGetFast("", "/api/agent/mr-status?branch="+url.QueryEscape(branch), mrTrailerHUDTimeout)
	if err != nil {
		return nil, false
	}
	var resp mrTrailerResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false
	}
	return resp.MergeRequests, true
}

// readGitBranch resolves the current branch by reading HEAD, walking up from
// startDir to find the git dir. Handles both a normal repo (.git directory)
// and a linked worktree (.git file with a `gitdir:` pointer). Returns "" on a
// detached HEAD or when no repo is found — both of which fail open.
func readGitBranch(startDir string) string {
	dir := startDir
	for i := 0; i < 40; i++ {
		gitPath := filepath.Join(dir, ".git")
		info, err := os.Stat(gitPath)
		if err == nil {
			var headPath string
			if info.IsDir() {
				headPath = filepath.Join(gitPath, "HEAD")
			} else {
				// .git is a file: `gitdir: <path>` (linked worktree).
				if gitDir := readGitdirPointer(gitPath, dir); gitDir != "" {
					headPath = filepath.Join(gitDir, "HEAD")
				}
			}
			if headPath != "" {
				if b := branchFromHEAD(headPath); b != "" {
					return b
				}
			}
			return "" // found the repo but HEAD is detached/unreadable
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// readGitdirPointer parses a `.git` file's `gitdir: <path>` line and resolves
// it relative to baseDir when the pointer is relative.
func readGitdirPointer(gitFile, baseDir string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	p := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	return p
}

// branchFromHEAD reads a HEAD file and returns the branch name for a symbolic
// ref (`ref: refs/heads/<branch>`), or "" for a detached HEAD.
func branchFromHEAD(headPath string) string {
	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const refPrefix = "ref: refs/heads/"
	if strings.HasPrefix(line, refPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(line, refPrefix))
	}
	return ""
}

// appendMRStatusTrailer is the proxy handler entry point: it appends an
// MR-status trailer (subject to delta-gating and fail-open rules) to the
// result of a git/gitlab tool call. Returns true iff result was mutated.
func appendMRStatusTrailer(result *mcp.CallToolResult, server, tool string) bool {
	return defaultMRTrailer.maybeAppend(result, server, tool)
}

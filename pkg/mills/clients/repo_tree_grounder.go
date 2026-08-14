package clients

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// repoTreeGrounderDefaultTTL bounds how stale a cached tree listing may be
// before the next Ground call re-fetches. Matched to the plan-slice emitter's
// default poll interval so one emit pass reuses one listing.
const repoTreeGrounderDefaultTTL = 5 * time.Minute

// RepoTreeGrounder answers "which of these repo-relative paths exist in the
// target repo?" against a revision-PINNED git tree, for the plan-slice
// emitter's fabrication grounding.
//
// Two prior grounding failures shape the design:
//
//   - The working tree is a trap. ensureRepoRoot hard-aligns the operator's
//     PVC clone to origin/main once, at boot; a pod that has been up for weeks
//     stats a frozen tree (the 2026-08-01 research-grounding incident: a clone
//     stale since May made every recent path read as a hallucination). Ground
//     therefore never stats the filesystem — it fetches origin/main and reads
//     the tree OBJECT (`ls-tree -r origin/main`), which works even when the
//     fetched tip shares no ancestry with the shallow checkout, so a
//     failed-to-advance working tree cannot poison the verdict.
//
//   - "Exists on main right now" is the wrong question to answer LATER: once
//     an implement run creates a declared file, current-main existence proves
//     nothing about fabrication. Ground therefore reports the revision it
//     consulted, and the emitter stamps it on the slice so post-merge audits
//     re-resolve the exact tree the verdict was based on.
//
// Failure posture is fail-open by returning ok=false — the emitter emits
// ungrounded rather than blocking demand on git availability — but a fetch
// failure alone does NOT fail Ground: the previously-fetched origin/main ref
// still answers, tagged with its actual (older) revision.
type RepoTreeGrounder struct {
	// RepoRoot is the operator-local clone (cfg.RepoRoot). Empty disables.
	RepoRoot string
	// Project is the repo the clone tracks (the operator's GitLab project
	// path). Ground refuses foreign projects — the clone cannot answer for a
	// repo it does not hold.
	Project string
	// Runner executes git. Nil gets the production exec runner.
	Runner CommandRunner
	Logger *slog.Logger
	// TTL bounds tree-listing reuse; zero gets repoTreeGrounderDefaultTTL.
	TTL time.Duration

	mu       sync.Mutex
	expires  time.Time
	revision string
	paths    map[string]struct{}
}

// Ground reports the subset of files absent from the target repo's tree at
// the returned revision. ok=false means the check could not run at all
// (foreign project, no clone, git unavailable) and the caller must treat the
// slice as ungrounded, not as grounded-clean. Glob-carrying entries are never
// reported missing — only concrete paths ground.
func (g *RepoTreeGrounder) Ground(ctx context.Context, project string, files []string) (missing []string, revision string, ok bool) {
	if g == nil || strings.TrimSpace(g.RepoRoot) == "" || len(files) == 0 {
		return nil, "", false
	}
	if !store.SameRepo(project, g.Project) {
		return nil, "", false
	}
	paths, revision, ok := g.treePaths(ctx)
	if !ok {
		return nil, "", false
	}
	for _, f := range files {
		clean := strings.TrimPrefix(strings.TrimSpace(f), "./")
		if clean == "" || strings.ContainsAny(clean, "*?[") {
			continue
		}
		if _, exists := paths[clean]; !exists {
			missing = append(missing, clean)
		}
	}
	return missing, revision, true
}

// treePaths returns the cached origin/main tree listing, refreshing it when
// the TTL has lapsed. The fetch is best-effort; ls-tree against the last
// successfully fetched ref is the authority.
func (g *RepoTreeGrounder) treePaths(ctx context.Context) (map[string]struct{}, string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paths != nil && time.Now().Before(g.expires) {
		return g.paths, g.revision, true
	}
	runner := g.Runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	// The clone is minted `--depth=1 --branch main`; the explicit refspec
	// mirrors refreshRepoRoot's for the same reason (remote.origin.fetch
	// covers only main). A failure here is logged and tolerated: the ref
	// from the last successful fetch still answers, just older.
	if _, stderr, code, err := runner.Run(ctx, g.RepoRoot, "git",
		"fetch", "--depth=1", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil || code != 0 {
		g.logger().Warn("slice grounding fetch failed; grounding against last fetched origin/main",
			"repo_root", g.RepoRoot, "exit", code, "stderr", strings.TrimSpace(stderr), "err", err)
	}
	rev, stderr, code, err := runner.Run(ctx, g.RepoRoot, "git", "rev-parse", "refs/remotes/origin/main")
	if err != nil || code != 0 {
		g.logger().Warn("slice grounding rev-parse failed; slices emit ungrounded",
			"repo_root", g.RepoRoot, "exit", code, "stderr", strings.TrimSpace(stderr), "err", err)
		return nil, "", false
	}
	list, stderr, code, err := runner.Run(ctx, g.RepoRoot, "git", "ls-tree", "-r", "--name-only", "refs/remotes/origin/main")
	if err != nil || code != 0 {
		g.logger().Warn("slice grounding ls-tree failed; slices emit ungrounded",
			"repo_root", g.RepoRoot, "exit", code, "stderr", strings.TrimSpace(stderr), "err", err)
		return nil, "", false
	}
	paths := make(map[string]struct{})
	for _, line := range strings.Split(list, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths[p] = struct{}{}
		}
	}
	if len(paths) == 0 {
		// An empty tree listing on a repo we know is populated means the read
		// is broken, not that every path is fabricated. Fail open.
		g.logger().Warn("slice grounding ls-tree returned an empty tree; slices emit ungrounded",
			"repo_root", g.RepoRoot)
		return nil, "", false
	}
	ttl := g.TTL
	if ttl <= 0 {
		ttl = repoTreeGrounderDefaultTTL
	}
	g.paths, g.revision, g.expires = paths, strings.TrimSpace(rev), time.Now().Add(ttl)
	return g.paths, g.revision, true
}

func (g *RepoTreeGrounder) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

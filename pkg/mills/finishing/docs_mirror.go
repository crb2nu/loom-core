// Package finishing contains deterministic finishing-house checks.
package finishing

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"time"
)

const (
	StateFresh   = "fresh"
	StateDrifted = "drifted"
	StateUnknown = "unknown"
)

type DocsMirrorDrift struct {
	Missing        int        `json:"missing"`
	Stale          int        `json:"stale"`
	Extra          int        `json:"extra"`
	Files          int        `json:"files"`
	MirrorRef      string     `json:"mirror_ref"`
	MirrorCommitAt *time.Time `json:"mirror_commit_at,omitempty"`
	CheckedAt      time.Time  `json:"checked_at"`
	State          string     `json:"state"`
	Reason         string     `json:"reason,omitempty"`
}

type TreeEntry struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	BlobSHA string `json:"blob_sha"`
}

type TreeLister interface {
	ListTree(context.Context, string, string, string, bool) ([]TreeEntry, error)
}

type CommitLister interface {
	NewestPathCommit(context.Context, string, string, string) (*time.Time, error)
}

type SourceLister func(context.Context) ([]TreeEntry, error)

func GitSourceLister(repoRoot, buildSHA string) SourceLister {
	return func(ctx context.Context) ([]TreeEntry, error) {
		cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "ls-tree", "-r", buildSHA, "--", "docs/")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-tree: %w", err)
		}
		return ParseLSTree(string(out))
	}
}

func ParseLSTree(raw string) ([]TreeEntry, error) {
	var entries []TreeEntry
	s := bufio.NewScanner(strings.NewReader(raw))
	for s.Scan() {
		meta, name, ok := strings.Cut(s.Text(), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid git ls-tree line %q", s.Text())
		}
		entries = append(entries, TreeEntry{Path: name, Type: fields[1], BlobSHA: fields[2]})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func CheckDocsMirror(ctx context.Context, source SourceLister, trees TreeLister, commits CommitLister, project, ref, mirrorPath string, now time.Time) DocsMirrorDrift {
	r := DocsMirrorDrift{MirrorRef: ref, CheckedAt: now.UTC(), State: StateUnknown}
	sourceEntries, err := source(ctx)
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	mirrorEntries, err := trees.ListTree(ctx, project, ref, mirrorPath, true)
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	commitAt, err := commits.NewestPathCommit(ctx, project, ref, mirrorPath)
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	r.MirrorCommitAt = commitAt
	src := normalize(sourceEntries, "docs")
	dst := normalize(mirrorEntries, mirrorPath)
	r.Files = len(src)
	for name, sha := range src {
		other, ok := dst[name]
		if !ok {
			r.Missing++
		} else if other != sha {
			r.Stale++
		}
	}
	for name := range dst {
		if _, ok := src[name]; !ok {
			r.Extra++
		}
	}
	r.State = StateFresh
	if r.Missing+r.Stale+r.Extra > 0 {
		r.State = StateDrifted
	}
	return r
}

func normalize(entries []TreeEntry, prefix string) map[string]string {
	out := map[string]string{}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		name := strings.TrimPrefix(strings.Trim(e.Path, "/"), prefix+"/")
		if ignored(name) {
			continue
		}
		out[name] = e.BlobSHA
	}
	return out
}

func ignored(name string) bool {
	base := path.Base(name)
	if base == "nav.yaml" {
		return true
	}
	return strings.HasPrefix(base, "roadmap-reconciliation-") && strings.HasSuffix(base, ".md") ||
		strings.HasPrefix(base, "ROADMAP_RECONCILIATION_") && strings.HasSuffix(base, ".md")
}

package clients

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// research_grounding.go hardens the research stage against a failure
// mode observed in pipeline run PIPE-MILLS-2026-06-29-001 (backlog
// MILLS-2026-06-29-001, a Go repo): the FlexInfer research model
// (gemma4-26b-a4b-gptq), asked to "summarize relevant code paths" with
// no grounding, invented a plausible-but-fictional codebase — Python
// files such as `mills/core/council/orchestrator.py` and
// `mills/state/transition_manager.py` for a repository that is actually
// Go (`pkg/mills/...`, `cmd/loom-mills-operator/...`). Those fabricated
// paths flowed into the implement worker's context as `research_notes`
// and poisoned it.
//
// Two complementary defenses live here:
//
//  1. RepoTreeDigest grounds the research PROMPT with the real
//     top-of-tree layout so the model has true anchors instead of
//     inventing them.
//  2. SanitizeResearchNotes validates the model OUTPUT: file paths it
//     references are checked against the real repository. Wholesale
//     hallucinations (the observed case: every referenced path absent)
//     are withheld so they never reach implement; partial ones are
//     flagged with a footer so the downstream agent distrusts them.
//
// Both are pure/FS-only and unit-tested in research_grounding_test.go.

// pathRefPattern matches file-path-like tokens: one or more
// slash-separated segments ending in a short file extension. Requiring
// at least one slash and a trailing extension filters out prose words,
// version strings ("0.85"), and bare identifiers.
var pathRefPattern = regexp.MustCompile(`(?:[A-Za-z0-9_.\-]+/)+[A-Za-z0-9_.\-]+\.[A-Za-z0-9]{1,8}`)

// urlPattern strips URLs before path extraction so a link's path
// component (e.g. https://host/a/b.html) isn't mistaken for a repo
// file reference.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// domainFirstSegment flags tokens whose first segment looks like a
// hostname (foo.com/...) rather than a repo-relative path.
var domainFirstSegment = regexp.MustCompile(`^[A-Za-z0-9\-]+\.(com|org|io|net|dev|ai|gov|edu|co)$`)

// withholdFabricationRate is the share of referenced paths that must be
// fabricated before the whole notes are withheld. Paired with a
// minimum count so a single stray token in otherwise-grounded notes
// only triggers the lighter footer path.
const (
	withholdFabricationRate = 0.5
	withholdMinFabricated   = 2
)

// ExtractReferencedPaths returns the unique, normalized file-path-like
// tokens referenced in free text, in first-seen order. URLs and
// hostname-rooted tokens are excluded.
func ExtractReferencedPaths(text string) []string {
	cleaned := urlPattern.ReplaceAllString(text, " ")
	matches := pathRefPattern.FindAllString(cleaned, -1)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		p := normalizeRefPath(m)
		if p == "" {
			continue
		}
		first := p
		if i := strings.IndexByte(p, '/'); i >= 0 {
			first = p[:i]
		}
		if domainFirstSegment.MatchString(first) {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// normalizeRefPath trims punctuation/markdown noise and leading
// ./ or / from a raw matched token.
func normalizeRefPath(raw string) string {
	p := strings.Trim(raw, "`'\"(),.;:[]{}<>*")
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}

// ResearchSanitizeOutcome is the result of validating research notes.
//
//	Withheld == true  → the notes were a likely wholesale hallucination
//	                    and Notes is a short grounded notice instead of
//	                    the model output.
//	Withheld == false &&
//	  len(Dropped) > 0 → the notes were kept but flagged with an
//	                    "Unverified paths" footer.
//	len(Dropped) == 0 → Notes is the original, untouched.
type ResearchSanitizeOutcome struct {
	Notes    string
	Dropped  []string
	Withheld bool
}

// SanitizeResearchNotes validates file paths referenced in research
// notes against exists(). declared lists paths the backlog item's plan
// slices claim — the enforced scope envelope, including the exact paths
// NEW files will be created at. Those are legitimate references even
// though they do not exist on disk yet: the research prompt explicitly
// directs the model to ground itself in the backlog context, so
// counting a slice-declared path as a fabrication punished the model
// for following instructions (observed live: a 4/4 "hallucination"
// withhold where every path came from the item's own slices). Behavior:
//
//   - no referenced paths              → notes unchanged
//   - all referenced paths real
//     or slice-declared               → notes unchanged
//   - mostly fabricated (>= rate and
//     >= min count)                    → notes WITHHELD, replaced with a
//     short grounded notice, since the output is a likely wholesale
//     hallucination that would poison the implement worker
//   - some fabricated (below the bar)  → notes kept, a footer is
//     appended listing the unverified paths
//
// Dropped is the sorted list of fabricated paths (empty when nothing
// was dropped).
func SanitizeResearchNotes(notes string, exists func(string) bool, declared []string) ResearchSanitizeOutcome {
	if exists == nil || strings.TrimSpace(notes) == "" {
		return ResearchSanitizeOutcome{Notes: notes}
	}
	refs := ExtractReferencedPaths(notes)
	if len(refs) == 0 {
		return ResearchSanitizeOutcome{Notes: notes}
	}
	declaredSet := make(map[string]struct{}, len(declared))
	for _, d := range declared {
		if d = normalizeRefPath(filepath.ToSlash(d)); d != "" {
			declaredSet[d] = struct{}{}
		}
	}
	var fabricated []string
	for _, p := range refs {
		if _, ok := declaredSet[p]; ok {
			continue
		}
		if !exists(p) {
			fabricated = append(fabricated, p)
		}
	}
	if len(fabricated) == 0 {
		return ResearchSanitizeOutcome{Notes: notes}
	}
	sort.Strings(fabricated)

	rate := float64(len(fabricated)) / float64(len(refs))
	if len(fabricated) >= withholdMinFabricated && rate >= withholdFabricationRate {
		notice := fmt.Sprintf(
			"Research notes withheld: the research model referenced %d file path(s), %d of which do not exist in the target repository. "+
				"The output is an unreliable (likely hallucinated) description of a non-existent codebase and was suppressed to avoid poisoning implementation. "+
				"Rely on the plan slices and inspect the actual repository directly.",
			len(refs), len(fabricated),
		)
		return ResearchSanitizeOutcome{Notes: notice, Dropped: fabricated, Withheld: true}
	}

	footer := "\n\n---\n⚠ Unverified paths (not found in the repository — likely hallucinated, do not rely on them): " +
		strings.Join(fabricated, ", ")
	return ResearchSanitizeOutcome{
		Notes:   strings.TrimRight(notes, "\n") + footer,
		Dropped: fabricated,
	}
}

// repoPathChecker returns an existence predicate that resolves a
// repo-relative path under root. It refuses paths that escape root via
// "..". An empty root yields a predicate that reports nothing exists,
// so callers can keep the same code path while grounding is disabled.
func repoPathChecker(root string) func(string) bool {
	root = strings.TrimSpace(root)
	return func(rel string) bool {
		if root == "" {
			return false
		}
		rel = strings.TrimSpace(rel)
		rel = strings.TrimPrefix(rel, "./")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return false
		}
		clean := filepath.Clean(rel)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return false
		}
		_, err := os.Stat(filepath.Join(root, clean))
		return err == nil
	}
}

// researchTreeSkipDirs are directories pruned from the prompt digest:
// VCS metadata, dependency caches, build output, and local-only Loom
// artifacts that don't help the model anchor on real source paths.
var researchTreeSkipDirs = map[string]struct{}{
	".git":         {},
	".worktrees":   {},
	".loom":        {},
	"node_modules": {},
	"vendor":       {},
	"dist":         {},
	"build":        {},
	".cache":       {},
	"testdata":     {},
}

// RepoTreeDigest returns a compact, deterministic listing of the
// repository's real top-of-tree structure to ground the research
// prompt. It walks up to maxDepth levels below root (depth 1 == the
// immediate children), prunes dotfiles and known noise directories,
// sorts entries, and caps the output at maxEntries lines. Directories
// are suffixed with "/".
//
// It returns "" when root is empty, unreadable, or yields no entries,
// so callers can fall back to an ungrounded prompt.
func RepoTreeDigest(root string, maxDepth, maxEntries int) string {
	root = strings.TrimSpace(root)
	if root == "" || maxDepth <= 0 || maxEntries <= 0 {
		return ""
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return ""
	}
	var entries []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		depth := strings.Count(rel, "/") + 1
		if d.IsDir() {
			if _, skip := researchTreeSkipDirs[name]; skip || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if depth >= maxDepth {
				// Record the directory but don't descend further.
				entries = append(entries, rel+"/")
				return fs.SkipDir
			}
			entries = append(entries, rel+"/")
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if depth > maxDepth {
			return nil
		}
		entries = append(entries, rel)
		return nil
	})
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	truncated := false
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
		truncated = true
	}
	out := strings.Join(entries, "\n")
	if truncated {
		out += "\n… (truncated)"
	}
	return out
}

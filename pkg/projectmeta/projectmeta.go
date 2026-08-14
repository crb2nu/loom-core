package projectmeta

import "strings"

// workspaceNamespaceRoots are the top-level buckets a canonical project key is
// rooted at (matching the workspace layout in AGENTS.md: services/<repo>,
// libs/<repo>, labs/<repo>, platform/<repo>, private/<repo>; apps/<repo> is
// retained for back-compat). The lifecycle-hook namespace minting validates
// against the SAME set (pkg/generator/configs_hooks.go hookNamespaceVars) so a
// namespace never canonicalizes to a non-workspace path segment like "Users"
// or "agents".
var workspaceNamespaceRoots = map[string]struct{}{
	"apps":     {},
	"labs":     {},
	"libs":     {},
	"platform": {},
	"private":  {},
	"services": {},
}

// Normalize trims an explicit project identifier.
func Normalize(project string) string {
	return strings.TrimSpace(project)
}

// FromNamespace derives a project identifier from a namespace like
// "loom-core/feature-x". It returns an empty string when the namespace is
// blank or does not contain a usable project segment.
func FromNamespace(namespace string) string {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		return ""
	}
	parts := strings.Split(ns, "/")
	if len(parts) >= 2 {
		if _, ok := workspaceNamespaceRoots[parts[0]]; ok && strings.TrimSpace(parts[1]) != "" {
			return parts[0] + "/" + parts[1]
		}
		if strings.TrimSpace(parts[0]) != "" {
			return parts[0]
		}
	}
	if strings.HasPrefix(ns, "/") {
		return ""
	}
	return ns
}

// FromPath derives a project identifier from a workspace-relative or absolute
// path that contains one of the workspace namespace roots.
func FromPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}

	segments := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	for i := 0; i+1 < len(segments); i++ {
		if _, ok := workspaceNamespaceRoots[segments[i]]; !ok {
			continue
		}
		next := strings.TrimSpace(segments[i+1])
		if next == "" || next == "." || next == ".." {
			continue
		}
		return segments[i] + "/" + next
	}

	return ""
}

// Canonical returns the preferred project identifier from the available link
// metadata. Explicit values win; namespace-derived values are the fallback.
func Canonical(explicitProject, namespace string) string {
	if project := Normalize(explicitProject); project != "" {
		return project
	}
	return FromNamespace(namespace)
}

// LooksLikeBareRepo reports whether project is a non-empty identifier that lacks
// any path segment — e.g. "loom-core" rather than the workspace-bucketed
// "services/loom-core" or a GitLab path_with_namespace like "group/loom-core".
// HUD project grouping keys on the exact project string, so a bare id fragments
// a project into its own ungrouped card; callers should warn or canonicalize.
func LooksLikeBareRepo(project string) bool {
	p := strings.TrimSpace(project)
	return p != "" && !strings.Contains(p, "/")
}

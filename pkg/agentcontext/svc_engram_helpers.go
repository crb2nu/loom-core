package agentcontext

import (
	"github.com/crb2nu/loom/pkg/validate"
)

// Shared argument helpers for the engram tools (svc_engrams.go).
//
// These were originally the recipe layer. The agent_recipe_add/recall/list
// tools were removed: they forwarded verbatim to agent_engram_* (tier=1, no
// prerequisites) and added no capability of their own. The tag-shaping helpers
// below are still the engram tools' own filter builders, so they stayed.

// buildRecipeTagFilters collects language, scope, and explicit tags into a
// combined tag filter slice for the memory recall system.
func buildRecipeTagFilters(v *validate.Args) []string {
	tags := v.StringSlice("tags")

	if lang := v.String("language", ""); lang != "" {
		tags = append(tags, "lang:"+lang)
	}
	if scope := v.String("scope", ""); scope != "" {
		tags = append(tags, "scope:"+scope)
	}

	return tags
}

// toAnySlice converts []string to []any so it can be embedded in map[string]any
// for the memory args interface.
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

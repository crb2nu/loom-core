package agentcontext

import (
	"testing"

	"github.com/crb2nu/loom/pkg/validate"
)

// ---------------------------------------------------------------------------
// toAnySlice
// ---------------------------------------------------------------------------

func TestToAnySlice_Empty(t *testing.T) {
	t.Parallel()
	out := toAnySlice(nil)
	if len(out) != 0 {
		t.Fatalf("expected empty slice, got %d elements", len(out))
	}
}

func TestToAnySlice_MultipleItems(t *testing.T) {
	t.Parallel()
	in := []string{"recipe", "lang:go", "scope:project"}
	out := toAnySlice(in)

	if len(out) != len(in) {
		t.Fatalf("expected %d elements, got %d", len(in), len(out))
	}
	for i, v := range out {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("element %d: expected string, got %T", i, v)
		}
		if s != in[i] {
			t.Errorf("element %d: got %q, want %q", i, s, in[i])
		}
	}
}

// ---------------------------------------------------------------------------
// buildRecipeTagFilters
// ---------------------------------------------------------------------------

func TestBuildRecipeTagFilters_Empty(t *testing.T) {
	t.Parallel()
	v := validate.NewArgs(map[string]any{})
	tags := buildRecipeTagFilters(v)
	if len(tags) != 0 {
		t.Errorf("expected no tags, got %v", tags)
	}
}

func TestBuildRecipeTagFilters_LanguageOnly(t *testing.T) {
	t.Parallel()
	v := validate.NewArgs(map[string]any{
		"language": "go",
	})
	tags := buildRecipeTagFilters(v)
	if len(tags) != 1 || tags[0] != "lang:go" {
		t.Errorf("expected [lang:go], got %v", tags)
	}
}

func TestBuildRecipeTagFilters_ScopeOnly(t *testing.T) {
	t.Parallel()
	v := validate.NewArgs(map[string]any{
		"scope": "universal",
	})
	tags := buildRecipeTagFilters(v)
	if len(tags) != 1 || tags[0] != "scope:universal" {
		t.Errorf("expected [scope:universal], got %v", tags)
	}
}

func TestBuildRecipeTagFilters_AllFields(t *testing.T) {
	t.Parallel()
	v := validate.NewArgs(map[string]any{
		"tags":     []any{"database", "pool"},
		"language": "python",
		"scope":    "workspace",
	})
	tags := buildRecipeTagFilters(v)

	expected := []string{"database", "pool", "lang:python", "scope:workspace"}
	if len(tags) != len(expected) {
		t.Fatalf("expected %d tags, got %d: %v", len(expected), len(tags), tags)
	}
	for i, want := range expected {
		if tags[i] != want {
			t.Errorf("tag %d: got %q, want %q", i, tags[i], want)
		}
	}
}

func TestBuildRecipeTagFilters_ExplicitTagsWithoutLanguageOrScope(t *testing.T) {
	t.Parallel()
	v := validate.NewArgs(map[string]any{
		"tags": []any{"reliability", "networking"},
	})
	tags := buildRecipeTagFilters(v)

	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d: %v", len(tags), tags)
	}
	if tags[0] != "reliability" || tags[1] != "networking" {
		t.Errorf("got %v, want [reliability networking]", tags)
	}
}

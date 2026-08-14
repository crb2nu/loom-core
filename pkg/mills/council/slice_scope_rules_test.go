package council

import (
	"strings"
	"testing"
)

// The scope-authoring contract is only useful if the editor actually reads it,
// and only cheap if it never varies per item. These tests pin both properties:
// the guidance is present with its concrete anchors, and it is reachable
// through EditorGuardrailsPromptSection — the seam the prompt builder writes
// into the STABLE (cached) prefix, ahead of the volatile per-run brief.

func TestSliceScopePromptSection_TeachesSharedComponentScope(t *testing.T) {
	section := SliceScopePromptSection()
	for _, want := range []string{
		// The rule the 2026-07-26 escalations violated: name every directory,
		// not just the one the slice is titled after.
		"list every directory the work touches",
		"ENFORCED allowlist",
		"the shell or layout it renders inside",
		// Directory-over-guessed-basename, because the gate enforces the
		// directory of each declared path.
		"the DIRECTORY of each declared path",
		"basename of a file that does not exist yet is systematically wrong",
		// The real incident, inline, so the model has a concrete anchor.
		"internal/hud/frontend/src/lib/components/mills/",
		"internal/hud/frontend/src/lib/components/shared/PanelShell.svelte",
		// Counterweight: widening is not a licence to pad the allowlist.
		"Do NOT pad the list with unrelated packages",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("slice-scope prompt section missing %q:\n%s", want, section)
		}
	}
}

// TestEditorGuardrailsPromptSection_CarriesSliceScopeContract proves the
// guidance reaches the prompt. EditorGuardrailsPromptSection is written into
// the stable prefix by clients.buildCouncilEditorPromptParts (before the memory
// block, repo layout, and brief), so presence here means presence in the cached
// half for every backend — flexinfer, OpenAI, and Anthropic alike.
func TestEditorGuardrailsPromptSection_CarriesSliceScopeContract(t *testing.T) {
	section := EditorGuardrailsPromptSection()
	if !strings.Contains(section, SliceScopePromptSection()) {
		t.Fatalf("editor guardrails prompt dropped the slice-scope contract:\n%s", section)
	}
	// The pre-existing external-incident contract must survive the join.
	if !strings.Contains(section, "Backlog proposals MUST be actionable in this repository") {
		t.Fatalf("editor guardrails prompt lost the external-incident contract:\n%s", section)
	}
}

// TestSliceScopePromptSection_IsConstant guards the cache contract: a section
// that varied per call would cold-start the Anthropic backend's cached prefix
// on every council run.
func TestSliceScopePromptSection_IsConstant(t *testing.T) {
	if a, b := SliceScopePromptSection(), SliceScopePromptSection(); a != b {
		t.Fatal("slice-scope prompt section is not constant; the stable prefix would not cache")
	}
}

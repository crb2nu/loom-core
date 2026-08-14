package registry

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestTemplateContentHashesPinned enforces the registry's immutability
// convention: a SHIPPED template version is never edited — its content hash is
// pinned here, and any change must ship as a NEW version instead. Editing a
// shipped version would terminalize every in-flight run frozen to it (the
// ScriptFromRun content-hash guard fails closed by design); the golden turns
// that runtime abruptness into a CI-time decision.
//
// Adding a template/version: append it and regenerate.
// Regenerate: UPDATE_TEMPLATE_HASHES=1 go test ./pkg/mills/workflow/registry -run TestTemplateContentHashesPinned
const templateHashGoldenPath = "testdata/templates.golden"

func renderTemplateHashes() string {
	r := NewDefault()
	var lines []string
	for _, name := range r.Names() {
		for version, t := range r.templates[name] {
			lines = append(lines, fmt.Sprintf("%s@%s %s", name, version, t.ContentHash()))
		}
	}
	sort.Strings(lines)
	var b strings.Builder
	b.WriteString("# Shipped workflow-template content hashes. NEVER edit a shipped version —\n")
	b.WriteString("# add a new version instead; in-flight runs frozen to an edited version\n")
	b.WriteString("# terminalize on the content-hash guard. Regenerate (new versions only):\n")
	b.WriteString("# UPDATE_TEMPLATE_HASHES=1 go test ./pkg/mills/workflow/registry -run TestTemplateContentHashesPinned\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func TestTemplateContentHashesPinned(t *testing.T) {
	got := renderTemplateHashes()
	if os.Getenv("UPDATE_TEMPLATE_HASHES") != "" {
		if err := os.WriteFile(templateHashGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", templateHashGoldenPath)
		return
	}
	want, err := os.ReadFile(templateHashGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_TEMPLATE_HASHES=1)", templateHashGoldenPath, err)
	}
	if string(want) != got {
		t.Fatalf("a SHIPPED template version changed content hash.\n"+
			"Shipped versions are immutable: ship the change as a NEW version and pin it here.\n"+
			"(In-flight runs frozen to an edited version terminalize fail-closed.)\n--- golden ---\n%s--- current ---\n%s", want, got)
	}
}

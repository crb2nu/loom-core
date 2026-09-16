package skills

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadConformanceYAML(t *testing.T, content string) (*Registry, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoadRejectsInvalidAuthoring(t *testing.T) {
	const valid = "version: 1\nskills:\n  - name: example\n    common:\n      description: Example workflow\n"
	tests := []struct {
		name, content, diagnostic string
	}{
		{"unknown registry field", "verison: 1\nskills: []\n", "field verison not found"},
		{"unknown skill field", valid + "    categorie: []\n", "field categorie not found"},
		{"unknown common field", valid + "      instrutions: run\n", "field instrutions not found"},
		{"unknown script field", valid + "      scripts:\n        - name: test\n          path: scripts/test.sh\n          executable: true\n", "field executable not found"},
		{"unknown target field", valid + "    targets:\n      claude:\n        enabld: true\n", "field enabld not found"},
		{"unknown target", valid + "    targets:\n      claud: {}\n", "skills[0].targets.claud: unknown target"},
		{"all is not an authoring target", valid + "    targets:\n      all: {}\n", "skills[0].targets.all: unknown target"},
		{"duplicate name", valid + "  - name: example\n    common:\n      description: Another workflow\n", "duplicate name \"example\" (first defined at skills[0].name)"},
		{"null skill", "version: 1\nskills: [null]\n", "skills[0]: must be a skill object"},
		{"null target", valid + "    targets:\n      codex: null\n", "skills[0].targets.codex: must be a target object"},
		{"missing common", "version: 1\nskills:\n  - name: example\n", "skills[0].common: must include a non-empty description"},
		{"empty description", strings.Replace(valid, "Example workflow", "''", 1), "common.description: must not be empty"},
		{"whitespace description", strings.Replace(valid, "Example workflow", "'   '", 1), "common.description: must not be empty"},
		{"long description", strings.Replace(valid, "Example workflow", strings.Repeat("é", 1025), 1), "is 1025 characters; maximum is 1024"},
		{"second YAML document", valid + "---\nskills: []\n", "expected a single YAML document"},
	}
	for _, name := range []string{"", "../escape", "Uppercase", "a_b", "-leading", "trailing-", "two--hyphens", strings.Repeat("a", 65)} {
		tests = append(tests, struct{ name, content, diagnostic string }{
			"invalid name " + name, strings.Replace(valid, "name: example", "name: '"+name+"'", 1), "skills[0].name: must be 1-64",
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := loadConformanceYAML(t, tt.content)
			if err == nil || !strings.Contains(err.Error(), tt.diagnostic) {
				t.Fatalf("Load() = %v, %v; want diagnostic %q", reg, err, tt.diagnostic)
			}
			if reg != nil {
				t.Fatal("invalid registry must not be available for generation")
			}
		})
	}
}

func TestLoadPortableLimitsAndSupportedTargets(t *testing.T) {
	content := "version: 1\nskills:\n  - name: " + strings.Repeat("a", 64) + "\n    common:\n      description: " + strings.Repeat("é", 1024) + "\n      instructions: |\n        Retired example: old_tool(obsolete_argument=true). Do not call it.\n    targets:\n"
	for _, target := range []string{"codex", "claude", "kilocode", "gemini", "antigravity", "zed", "opencode"} {
		content += "      " + target + ": {}\n"
	}
	reg, err := loadConformanceYAML(t, content)
	if err != nil {
		t.Fatal(err)
	}
	if errs := (&Generator{Registry: reg, Target: "all"}).Validate(); len(errs) != 0 {
		t.Fatalf("valid limits, prose and targets rejected: %v", errs)
	}
}

func TestCanonicalRegistryConforms(t *testing.T) {
	reg, err := Load(filepath.Join("..", "..", "mcp", "context", "skills-registry.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Skills) == 0 {
		t.Fatal("canonical registry contains no skills")
	}
}

func TestValidateRejectsInMemoryAuthoringErrors(t *testing.T) {
	for _, reg := range []*Registry{
		nil,
		{Skills: []*Skill{nil}},
		{Skills: []*Skill{{Name: "../unsafe", Common: &SkillSpec{Description: "Example"}}}},
		{Skills: []*Skill{{Name: "example", Common: &SkillSpec{Description: " "}}}},
		{Skills: []*Skill{{Name: "example", Common: &SkillSpec{Description: "Example"}, Targets: map[string]*TargetSpec{"codex": nil}}}},
	} {
		if errs := (&Generator{Registry: reg, Target: "codex"}).Validate(); len(errs) == 0 {
			t.Errorf("Validate(%#v) accepted invalid registry", reg)
		}
	}
	if errs := (&Generator{Registry: &Registry{}, Target: "claud"}).Validate(); len(errs) == 0 {
		t.Fatal("unknown generation target accepted")
	}
}

func TestValidateClaudeListingCountsCharacters(t *testing.T) {
	skill := newTestSkill("unicode-listing", strings.Repeat("é", 1000))
	skill.Common.WhenToUse = strings.Repeat("界", 536)
	g := &Generator{Registry: &Registry{Skills: []*Skill{skill}}, Target: "claude"}
	if errs := g.Validate(); len(errs) != 0 {
		t.Fatalf("1536 Unicode characters rejected: %v", errs)
	}
	skill.Common.WhenToUse += "界"
	if errs := g.Validate(); len(errs) != 1 || errs[0].ResourceType != "when_to_use" {
		t.Fatalf("1537 Unicode characters: got %v, want listing cap error", errs)
	}
}

func TestScaffoldProducesClaudeSkillBundle(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for scaffold integration test")
	}
	dir := t.TempDir()
	registry := filepath.Join(dir, "mcp", "context", "skills-registry.yaml")
	if err := os.MkdirAll(filepath.Dir(registry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registry, []byte("version: 1\nskills:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	script := filepath.Join("..", "..", "mcp", "skills", "loom-skill-builder", "scripts", "skill_scaffold.py")
	cmd := exec.CommandContext(ctx, python, "-B", script, "--root", dir, "--name", "Example Skill", "--apply")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scaffold: %v\n%s", err, output)
	}
	g, err := NewGenerator(GeneratorOptions{RegistryPath: registry, Target: "claude", RepoRoot: dir, WorkspaceRoot: dir, CodexHome: filepath.Join(dir, ".codex")})
	if err != nil {
		t.Fatal(err)
	}
	if errs := g.Validate(); len(errs) != 0 {
		t.Fatalf("scaffold does not conform: %v", errs)
	}
	if err := g.Generate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "skills", "example-skill", "SKILL.md")); err != nil {
		t.Fatalf("Claude skill bundle missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "commands", "example-skill.md")); !os.IsNotExist(err) {
		t.Fatalf("scaffold generated a legacy command: %v", err)
	}
}

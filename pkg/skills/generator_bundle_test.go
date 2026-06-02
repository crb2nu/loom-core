package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateBundleSkill_ZedAndOpencode verifies the SKILL.md-format targets
// (zed, opencode) write a bundle to <OutputDir>/skills/<name>/SKILL.md with
// valid frontmatter. Regression guard for the "unknown target" gap where these
// declared-skill-capable profiles produced no skills.
func TestGenerateBundleSkill_ZedAndOpencode(t *testing.T) {
	for _, target := range []string{"zed", "opencode"} {
		t.Run(target, func(t *testing.T) {
			tmp := t.TempDir()
			g := &Generator{
				SourceDir: filepath.Join(tmp, "src"),
				OutputDir: tmp, // generator appends skills/<name>
				Target:    target,
			}
			skill := newTestSkill("decision-journal", "Track decisions with rationale")

			if err := g.generateBundleSkill(skill, target); err != nil {
				t.Fatalf("generateBundleSkill(%s) error = %v", target, err)
			}

			skillMD := filepath.Join(tmp, "skills", "decision-journal", "SKILL.md")
			data, err := os.ReadFile(skillMD)
			if err != nil {
				t.Fatalf("expected SKILL.md at %s: %v", skillMD, err)
			}
			content := string(data)
			if !strings.HasPrefix(content, "---\n") {
				t.Errorf("%s SKILL.md missing YAML frontmatter, got:\n%s", target, content)
			}
			if !strings.Contains(content, "name: decision-journal\n") {
				t.Errorf("%s SKILL.md missing name field, got:\n%s", target, content)
			}
		})
	}
}

// TestGenerateForTarget_BundleTargetsEnabledByDefault verifies that skills with
// no explicit targets map are generated for zed/opencode (IsEnabled defaults to
// true), so the full registry reaches these profiles without per-skill edits.
func TestGenerateForTarget_BundleTargetsEnabledByDefault(t *testing.T) {
	tmp := t.TempDir()
	reg := &Registry{
		Skills: []*Skill{
			newTestSkill("alpha", "first skill"),
			newTestSkill("beta", "second skill"),
		},
	}
	for _, target := range []string{"zed", "opencode"} {
		t.Run(target, func(t *testing.T) {
			out := filepath.Join(tmp, target)
			g := &Generator{
				Registry:  reg,
				SourceDir: filepath.Join(tmp, "src"),
				OutputDir: out,
				Target:    target,
			}
			if err := g.generateForTarget(target); err != nil {
				t.Fatalf("generateForTarget(%s) error = %v", target, err)
			}
			for _, name := range []string{"alpha", "beta"} {
				p := filepath.Join(out, "skills", name, "SKILL.md")
				if _, err := os.Stat(p); err != nil {
					t.Errorf("expected generated skill %s for %s: %v", name, target, err)
				}
			}
		})
	}
}

// TestResolveInstructions_BundleSkillPath verifies ${SKILL_PATH} resolves to the
// target's own skills home for zed and opencode.
func TestResolveInstructions_BundleSkillPath(t *testing.T) {
	cases := map[string]string{
		"zed":      "$HOME/.config/zed/skills/my-skill",
		"opencode": "$HOME/.config/opencode/skills/my-skill",
	}
	for target, want := range cases {
		t.Run(target, func(t *testing.T) {
			s := &Skill{
				Name: "my-skill",
				Common: &SkillSpec{
					Description:  "desc",
					Instructions: "See ${SKILL_PATH}/references for details.",
				},
			}
			got := s.ResolveInstructions(target, "/tmp/codex", "/tmp/src/my-skill")
			if !strings.Contains(got, want) {
				t.Errorf("%s: expected SKILL_PATH %q in resolved instructions, got:\n%s", target, want, got)
			}
		})
	}
}

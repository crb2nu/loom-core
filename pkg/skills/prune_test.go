package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func writePruneFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestPruneStaleGenerated_RemovesStaleKeepsCurrent(t *testing.T) {
	dir := t.TempDir()
	writePruneFile(t, filepath.Join(dir, "commands", "kept.md"), "kept")
	writePruneFile(t, filepath.Join(dir, "commands", "stale.md"), "stale")

	old := []string{filepath.Join("commands", "kept.md"), filepath.Join("commands", "stale.md")}
	current := []string{filepath.Join("commands", "kept.md")}

	removed := PruneStaleGenerated(dir, old, current)

	if len(removed) != 1 || removed[0] != filepath.Join("commands", "stale.md") {
		t.Fatalf("expected only stale.md removed, got %#v", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "commands", "kept.md")); err != nil {
		t.Fatalf("expected kept.md to survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "commands", "stale.md")); !os.IsNotExist(err) {
		t.Fatalf("expected stale.md removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "commands")); err != nil {
		t.Fatalf("expected commands/ to survive while kept.md remains: %v", err)
	}
}

func TestPruneStaleGenerated_RemovesEmptiedBundleDir(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "skills", "old-skill")
	writePruneFile(t, filepath.Join(bundle, "SKILL.md"), "old")
	writePruneFile(t, filepath.Join(bundle, "scripts", "run.sh"), "echo")
	// Generation scaffolds empty subdirs that never appear in the manifest.
	for _, sub := range []string{"references", filepath.Join("assets", "templates")} {
		if err := os.MkdirAll(filepath.Join(bundle, sub), 0755); err != nil {
			t.Fatalf("mkdir scaffold: %v", err)
		}
	}
	writePruneFile(t, filepath.Join(dir, "skills", "new-skill", "SKILL.md"), "new")

	old := []string{
		filepath.Join("skills", "old-skill", "SKILL.md"),
		filepath.Join("skills", "old-skill", "scripts", "run.sh"),
		filepath.Join("skills", "new-skill", "SKILL.md"),
	}
	current := []string{filepath.Join("skills", "new-skill", "SKILL.md")}

	PruneStaleGenerated(dir, old, current)

	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Fatalf("expected old-skill bundle dir removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "new-skill", "SKILL.md")); err != nil {
		t.Fatalf("expected new-skill bundle to survive: %v", err)
	}
}

func TestPruneStaleGenerated_LeavesUnmanagedNeighborsUntouched(t *testing.T) {
	dir := t.TempDir()
	writePruneFile(t, filepath.Join(dir, "skills", "old-skill", "SKILL.md"), "old")
	// Hosted-import / hand-authored neighbors are never in the manifest.
	writePruneFile(t, filepath.Join(dir, "skills", "hosted-skill", "SKILL.md"), "hosted")
	writePruneFile(t, filepath.Join(dir, "skills", "hosted-skill", ".loom-hosted-skill.json"), "{}")

	old := []string{filepath.Join("skills", "old-skill", "SKILL.md")}

	PruneStaleGenerated(dir, old, nil)

	if _, err := os.Stat(filepath.Join(dir, "skills", "old-skill")); !os.IsNotExist(err) {
		t.Fatalf("expected old-skill removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "hosted-skill", "SKILL.md")); err != nil {
		t.Fatalf("expected hosted neighbor untouched: %v", err)
	}
}

func TestPruneStaleGenerated_IgnoresUnsafeManifestPaths(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.md")
	writePruneFile(t, outside, "do not delete")

	old := []string{
		outside, // absolute
		filepath.Join("..", filepath.Base(filepath.Dir(outside)), "victim.md"), // escapes dir
		".",
	}

	removed := PruneStaleGenerated(dir, old, nil)

	if len(removed) != 0 {
		t.Fatalf("expected nothing removed, got %#v", removed)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("expected file outside dir untouched: %v", err)
	}
}

func TestPruneStaleGenerated_MissingFilesAreNotReported(t *testing.T) {
	dir := t.TempDir()

	removed := PruneStaleGenerated(dir, []string{filepath.Join("commands", "gone.md")}, nil)
	if len(removed) != 0 {
		t.Fatalf("expected no removals for already-missing files, got %#v", removed)
	}
}

// TestGenerateForTarget_PrunesRemovedClaudeSkill covers the registry-deletion
// path end to end: a skill deleted from skills-registry.yaml must have its
// previously generated command file pruned on the next run.
func TestGenerateForTarget_PrunesRemovedClaudeSkill(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "mcp", "skills")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}

	enabled := true
	mkSkill := func(name string) *Skill {
		return &Skill{
			Name: name,
			Common: &SkillSpec{
				Description:  name + " description",
				Instructions: "# " + name + "\n\nDo work.",
			},
			Targets: map[string]*TargetSpec{
				"claude": {Enabled: &enabled, Type: "command"},
			},
		}
	}

	outputDir := filepath.Join(tmpDir, "home", ".claude")
	newGen := func(reg *Registry) *Generator {
		return &Generator{
			Registry:  reg,
			SourceDir: sourceDir,
			Target:    "claude",
			OutputDir: outputDir,
			RepoRoot:  tmpDir,
			CodexHome: "/tmp/codex",
		}
	}

	g := newGen(&Registry{Skills: []*Skill{mkSkill("keep-me"), mkSkill("remove-me")}})
	if err := g.generateForTarget("claude"); err != nil {
		t.Fatalf("generateForTarget(claude) first run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "commands", "remove-me.md")); err != nil {
		t.Fatalf("expected remove-me.md generated: %v", err)
	}

	// A hand-authored skill bundle in the same home dir must survive pruning.
	hostedSkill := filepath.Join(outputDir, "skills", "hosted-skill", "SKILL.md")
	writePruneFile(t, hostedSkill, "user-imported")

	g = newGen(&Registry{Skills: []*Skill{mkSkill("keep-me")}})
	if err := g.generateForTarget("claude"); err != nil {
		t.Fatalf("generateForTarget(claude) second run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outputDir, "commands", "remove-me.md")); !os.IsNotExist(err) {
		t.Fatalf("expected remove-me.md pruned after registry deletion, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "commands", "keep-me.md")); err != nil {
		t.Fatalf("expected keep-me.md to survive: %v", err)
	}
	if _, err := os.Stat(hostedSkill); err != nil {
		t.Fatalf("expected hosted skill untouched: %v", err)
	}

	manifest, err := ReadManifest(outputDir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if containsString(manifest.Generated, filepath.Join("commands", "remove-me.md")) {
		t.Fatalf("expected remove-me.md gone from manifest, got %#v", manifest.Generated)
	}
}

// TestGenerateForTarget_PrunesRemovedGeminiBundle checks that a deleted
// bundle-type skill has its whole skills/<name>/ directory (including empty
// scaffolding subdirs) pruned, while sibling bundles survive.
func TestGenerateForTarget_PrunesRemovedGeminiBundle(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "mcp", "skills")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}

	enabled := true
	mkSkill := func(name string) *Skill {
		return &Skill{
			Name: name,
			Common: &SkillSpec{
				Description:  name + " description",
				Instructions: "# " + name + "\n\nDo work.",
			},
			Targets: map[string]*TargetSpec{
				"gemini": {Enabled: &enabled, Type: "skill"},
			},
		}
	}

	newGen := func(reg *Registry) *Generator {
		return &Generator{
			Registry:  reg,
			SourceDir: sourceDir,
			Target:    "gemini",
			RepoRoot:  tmpDir,
			CodexHome: "/tmp/codex",
		}
	}

	g := newGen(&Registry{Skills: []*Skill{mkSkill("keep-me"), mkSkill("remove-me")}})
	if err := g.generateForTarget("gemini"); err != nil {
		t.Fatalf("generateForTarget(gemini) first run: %v", err)
	}

	geminiDir := filepath.Join(tmpDir, ".gemini")
	if _, err := os.Stat(filepath.Join(geminiDir, "skills", "remove-me", "SKILL.md")); err != nil {
		t.Fatalf("expected remove-me bundle generated: %v", err)
	}

	g = newGen(&Registry{Skills: []*Skill{mkSkill("keep-me")}})
	if err := g.generateForTarget("gemini"); err != nil {
		t.Fatalf("generateForTarget(gemini) second run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(geminiDir, "skills", "remove-me")); !os.IsNotExist(err) {
		t.Fatalf("expected remove-me bundle dir pruned, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(geminiDir, "skills", "keep-me", "SKILL.md")); err != nil {
		t.Fatalf("expected keep-me bundle to survive: %v", err)
	}
}

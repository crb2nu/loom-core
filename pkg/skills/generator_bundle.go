package skills

import (
	"fmt"
	"os"
	"path/filepath"
)

// generateBundleSkill writes an Anthropic-compatible SKILL.md bundle for a
// generic SKILL.md-format target (currently zed and opencode). It mirrors the
// gemini/codex bundle layout — <skillsBase>/skills/<name>/SKILL.md plus copied
// scripts/references/assets — but is parameterized on target so ${SKILL_PATH}
// resolves to the target's own skills home.
//
// The skills base directory comes from resolveTargetDir(target), which returns
// g.OutputDir when set. The sync layer points OutputDir at the parent of the
// profile's SkillsHomePath (e.g. $HOME/.config/zed) so the appended skills/
// segment lands at the configured skills home.
func (g *Generator) generateBundleSkill(skill *Skill, target string) error {
	baseDir := g.resolveTargetDir(target)
	if baseDir == "" {
		return fmt.Errorf("no skills output directory resolved for target %s", target)
	}

	skillDir := filepath.Join(baseDir, "skills", skill.Name)

	if g.Verbose {
		fmt.Printf("Generating %s skill: %s -> %s\n", target, skill.Name, skillDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create %s skill: %s\n", target, skillDir)
		return nil
	}
	if err := validateBundleDestination(baseDir, skillDir, skill); err != nil {
		return err
	}

	for _, subdir := range []string{"scripts", "references", "assets/templates"} {
		if err := os.MkdirAll(filepath.Join(skillDir, subdir), 0755); err != nil {
			return fmt.Errorf("create %s: %w", subdir, err)
		}
	}

	skillMD := g.generateBundleSkillMD(skill, target)
	skillMDPath := filepath.Join(skillDir, "SKILL.md")
	// Atomic write: external file watchers (e.g. the agent reading the skill)
	// must never observe a partial/empty SKILL.md. See generateCodexSkill.
	if err := writeFileAtomic(skillMDPath, []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	return g.copyBundleResources(skill, skillDir)
}

// copyBundleResources copies a skill's scripts, references, and assets from
// the registry source tree into a generated SKILL.md bundle directory.
func (g *Generator) copyBundleResources(skill *Skill, skillDir string) error {
	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	for _, resource := range bundleResources(skill) {
		srcPath, err := managedFilePath(sourceSkillDir, resource.path)
		if err != nil {
			return fmt.Errorf("source %s %s: %w", resource.kind, resource.path, err)
		}
		dstPath, err := managedFilePath(skillDir, resource.path)
		if err != nil {
			return fmt.Errorf("destination %s %s: %w", resource.kind, resource.path, err)
		}
		if err := copyDeliveredFile(srcPath, dstPath); err != nil {
			return fmt.Errorf("copy %s %s: %w", resource.kind, resource.path, err)
		}
	}
	return nil
}

type bundleResource struct{ kind, path string }

func bundleResources(skill *Skill) []bundleResource {
	if skill.Common == nil {
		return nil
	}
	var resources []bundleResource
	for _, script := range skill.Common.Scripts {
		if script != nil && script.Path != "" {
			resources = append(resources, bundleResource{"script", script.Path})
		}
	}
	for _, ref := range skill.Common.References {
		resources = append(resources, bundleResource{"reference", filepath.Join("references", ref)})
	}
	for _, asset := range skill.Common.Assets {
		resources = append(resources, bundleResource{"asset", filepath.Join("assets", asset)})
	}
	return resources
}

// Validate every destination before creating scaffolding or writing SKILL.md.
// The anchor is the configured platform root, so symlinks at the bundle itself
// and in resource directories cannot redirect writes outside that root.
func validateBundleDestination(root, skillDir string, skill *Skill) error {
	rel, err := filepath.Rel(root, skillDir)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("skill bundle %s must be within output root %s", skillDir, root)
	}
	paths := []string{"SKILL.md", "scripts", "references", "assets/templates"}
	for _, resource := range bundleResources(skill) {
		paths = append(paths, resource.path)
	}
	for _, path := range paths {
		if _, err := managedFilePath(root, filepath.Join(rel, path)); err != nil {
			return fmt.Errorf("validate bundle destination: %w", err)
		}
	}
	return nil
}

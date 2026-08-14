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
	if skill.Common == nil {
		return nil
	}
	for _, script := range skill.Common.Scripts {
		if script == nil || script.Path == "" {
			continue
		}
		srcPath := filepath.Join(sourceSkillDir, script.Path)
		dstPath := filepath.Join(skillDir, script.Path)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		if err := copyFile(srcPath, dstPath); err != nil && g.Verbose {
			fmt.Printf("Warning: could not copy script %s: %v\n", script.Path, err)
		}
	}
	for _, ref := range skill.Common.References {
		srcPath := filepath.Join(sourceSkillDir, "references", ref)
		dstPath := filepath.Join(skillDir, "references", ref)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		if err := copyFile(srcPath, dstPath); err != nil && g.Verbose {
			fmt.Printf("Warning: could not copy reference %s: %v\n", ref, err)
		}
	}
	for _, asset := range skill.Common.Assets {
		srcPath := filepath.Join(sourceSkillDir, "assets", asset)
		dstPath := filepath.Join(skillDir, "assets", asset)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		if err := copyFile(srcPath, dstPath); err != nil && g.Verbose {
			fmt.Printf("Warning: could not copy asset %s: %v\n", asset, err)
		}
	}
	return nil
}

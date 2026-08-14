// generator_codex_gemini.go contains Codex and Gemini skill generation logic.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// generateCodexSkill generates a Codex skill in SKILL.md + scripts/ + references/ + assets/ format.
func (g *Generator) generateCodexSkill(skill *Skill) error {
	// For Codex, OutputDir refers to the skills root directory (not the platform root).
	outputDir := g.OutputDir
	if outputDir == "" {
		outputDir = g.CodexSkillsDir
	}
	if outputDir == "" {
		outputDir = filepath.Join(g.CodexHome, "skills")
	}

	skillDir := filepath.Join(outputDir, skill.Name)

	if g.Verbose {
		fmt.Printf("Generating Codex skill: %s -> %s\n", skill.Name, skillDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create Codex skill: %s\n", skillDir)
		return nil
	}

	// Create skill directory structure
	for _, subdir := range []string{"scripts", "references", "assets/templates"} {
		if err := os.MkdirAll(filepath.Join(skillDir, subdir), 0755); err != nil {
			return fmt.Errorf("create %s: %w", subdir, err)
		}
	}

	// Generate SKILL.md
	skillMD := g.generateCodexSkillMD(skill)
	skillMDPath := filepath.Join(skillDir, "SKILL.md")
	// Atomic write to avoid codex file watcher observing partial/empty files,
	// which triggers spurious "missing YAML frontmatter" errors (openai/codex#11495).
	if err := writeFileAtomic(skillMDPath, []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	// Copy scripts
	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	if skill.Common.Scripts != nil {
		for _, script := range skill.Common.Scripts {
			srcPath := filepath.Join(sourceSkillDir, script.Path)
			dstPath := filepath.Join(skillDir, script.Path)

			if err := copyFile(srcPath, dstPath); err != nil {
				if g.Verbose {
					fmt.Printf("Warning: could not copy script %s: %v\n", script.Path, err)
				}
			}
		}
	}

	// Copy references
	if skill.Common.References != nil {
		for _, ref := range skill.Common.References {
			srcPath := filepath.Join(sourceSkillDir, "references", ref)
			dstPath := filepath.Join(skillDir, "references", ref)

			if err := copyFile(srcPath, dstPath); err != nil {
				if g.Verbose {
					fmt.Printf("Warning: could not copy reference %s: %v\n", ref, err)
				}
			}
		}
	}

	// Copy assets
	if skill.Common.Assets != nil {
		for _, asset := range skill.Common.Assets {
			srcPath := filepath.Join(sourceSkillDir, "assets", asset)
			dstPath := filepath.Join(skillDir, "assets", asset)

			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
				return err
			}

			if err := copyFile(srcPath, dstPath); err != nil {
				if g.Verbose {
					fmt.Printf("Warning: could not copy asset %s: %v\n", asset, err)
				}
			}
		}
	}

	return nil
}

// generateCodexSkillMD generates the SKILL.md content for a Codex skill.
func (g *Generator) generateCodexSkillMD(skill *Skill) string {
	return g.generateBundleSkillMD(skill, "codex")
}

// generateBundleSkillMD builds the Anthropic-compatible SKILL.md content for
// any SKILL.md-format target (codex, zed, opencode). The target only affects
// ${SKILL_PATH}/${CODEX_HOME} resolution in the instructions body.
func (g *Generator) generateBundleSkillMD(skill *Skill, target string) string {
	var sb strings.Builder

	// YAML frontmatter
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", skill.Name))

	// Description: normalize whitespace and escape for YAML
	desc := strings.TrimSpace(skill.Common.Description)
	desc = strings.ReplaceAll(desc, "\n", " ")
	sb.WriteString(fmt.Sprintf("description: \"%s\"\n", escapeYAMLString(desc)))
	shortDesc := shortSkillDescription(desc)
	if shortDesc != "" {
		sb.WriteString("metadata:\n")
		sb.WriteString(fmt.Sprintf("  short-description: \"%s\"\n", escapeYAMLString(shortDesc)))
	}
	sb.WriteString("---\n\n")

	// Instructions body with resolved paths
	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	instructions := skill.ResolveInstructions(target, g.CodexHome, sourceSkillDir)
	sb.WriteString(instructions)

	// Bundled Resources section (only if resources exist)
	hasResources := len(skill.Common.Scripts) > 0 || len(skill.Common.References) > 0 || len(skill.Common.Assets) > 0
	if hasResources {
		sb.WriteString("\n## Bundled Resources\n\n")

		for _, script := range skill.Common.Scripts {
			sb.WriteString(fmt.Sprintf("- `%s`\n", script.Path))
		}

		for _, ref := range skill.Common.References {
			sb.WriteString(fmt.Sprintf("- `references/%s`\n", ref))
		}

		for _, asset := range skill.Common.Assets {
			sb.WriteString(fmt.Sprintf("- `assets/%s`\n", asset))
		}
	}

	return sb.String()
}

// generateGeminiSkill generates a Gemini CLI skill bundle in .gemini/skills/<name>/.
func (g *Generator) generateGeminiSkill(skill *Skill) error {
	return g.generateGeminiLikeSkill(skill, "gemini")
}

// generateAntigravitySkill generates an Antigravity 2.0 skill bundle. The file
// format matches Gemini's SKILL.md bundle shape, but target-specific registry
// overrides are resolved against "antigravity".
func (g *Generator) generateAntigravitySkill(skill *Skill) error {
	return g.generateGeminiLikeSkill(skill, "antigravity")
}

func (g *Generator) generateGeminiLikeSkill(skill *Skill, target string) error {
	baseDir := g.resolveTargetDir(target)
	if baseDir == "" {
		baseDir = filepath.Join(g.RepoRoot, ".gemini")
		if target == "antigravity" {
			baseDir = filepath.Join(baseDir, "antigravity")
		}
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

	skillMD := g.generateGeminiLikeSkillMD(skill, target)
	skillMDPath := filepath.Join(skillDir, "SKILL.md")
	// Atomic write: see generator_codex_gemini.go:generateCodexSkill for rationale.
	if err := writeFileAtomic(skillMDPath, []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	if skill.Common.Scripts != nil {
		for _, script := range skill.Common.Scripts {
			srcPath := filepath.Join(sourceSkillDir, script.Path)
			dstPath := filepath.Join(skillDir, script.Path)
			if err := copyFile(srcPath, dstPath); err != nil && g.Verbose {
				fmt.Printf("Warning: could not copy script %s: %v\n", script.Path, err)
			}
		}
	}

	if skill.Common.References != nil {
		for _, ref := range skill.Common.References {
			srcPath := filepath.Join(sourceSkillDir, "references", ref)
			dstPath := filepath.Join(skillDir, "references", ref)
			if err := copyFile(srcPath, dstPath); err != nil && g.Verbose {
				fmt.Printf("Warning: could not copy reference %s: %v\n", ref, err)
			}
		}
	}

	if skill.Common.Assets != nil {
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
	}

	return nil
}

// generateGeminiSkillMD generates the SKILL.md content for a Gemini skill.
func (g *Generator) generateGeminiSkillMD(skill *Skill) string {
	return g.generateGeminiLikeSkillMD(skill, "gemini")
}

func (g *Generator) generateGeminiLikeSkillMD(skill *Skill, target string) string {
	var sb strings.Builder

	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", skill.Name))
	desc := strings.TrimSpace(skill.Common.Description)
	desc = strings.ReplaceAll(desc, "\n", " ")
	sb.WriteString(fmt.Sprintf("description: \"%s\"\n", escapeYAMLString(desc)))
	sb.WriteString("---\n\n")

	// Use the final Gemini/Antigravity skills home path to keep references
	// stable after sync.
	skillsHome := strings.TrimSpace(g.GeminiSkillsHome)
	if skillsHome == "" {
		skillsHome = "$HOME/.gemini/skills"
		if target == "antigravity" {
			skillsHome = "$HOME/.gemini/antigravity/skills"
		}
	}
	skillPath := fmt.Sprintf("%s/%s", strings.TrimRight(skillsHome, "/"), skill.Name)
	instructions := skill.ResolveInstructions(target, g.CodexHome, skillPath)
	sb.WriteString(instructions)

	hasResources := len(skill.Common.Scripts) > 0 || len(skill.Common.References) > 0 || len(skill.Common.Assets) > 0
	if hasResources {
		sb.WriteString("\n## Bundled Resources\n\n")
		for _, script := range skill.Common.Scripts {
			sb.WriteString(fmt.Sprintf("- `%s`\n", script.Path))
		}
		for _, ref := range skill.Common.References {
			sb.WriteString(fmt.Sprintf("- `references/%s`\n", ref))
		}
		for _, asset := range skill.Common.Assets {
			sb.WriteString(fmt.Sprintf("- `assets/%s`\n", asset))
		}
	}

	return sb.String()
}

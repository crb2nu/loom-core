// generator_claude_kilocode.go contains Claude and Kilocode skill generation logic.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// generateClaudeSkillByType routes to the appropriate Claude generator based on skill type.
func (g *Generator) generateClaudeSkillByType(skill *Skill) ([]string, error) {
	skillType := skill.GetType("claude")

	switch skillType {
	case "command":
		return g.generateClaudeCommand(skill)
	case "skill":
		return g.generateClaudeAgentSkill(skill)
	case "rule":
		return g.generateClaudeRule(skill)
	default:
		// Fall back to legacy .agents/skills/ format
		return g.generateClaudeSkill(skill)
	}
}

// generateClaudeAgentSkill writes an Agent Skills bundle to
// <baseDir>/skills/<name>/SKILL.md with copied scripts/references/assets.
// This is the modern Claude Code surface replacing legacy commands/<name>.md;
// after a successful write it prunes the skill's stale command file so the
// two layouts never present the same skill twice.
func (g *Generator) generateClaudeAgentSkill(skill *Skill) ([]string, error) {
	baseDir := g.resolveTargetDir("claude")
	if baseDir == "" {
		baseDir = filepath.Join(g.WorkspaceRoot, ".claude")
	}
	skillDir := filepath.Join(baseDir, "skills", skill.Name)

	if g.Verbose {
		fmt.Printf("Generating Claude agent skill: %s -> %s\n", skill.Name, skillDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create Claude agent skill: %s\n", skillDir)
		return g.codexManifestFiles(skill), nil
	}

	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return nil, fmt.Errorf("create skill dir: %w", err)
	}

	skillMD := g.generateClaudeAgentSkillMD(skill, skillDir)
	// Atomic write: a live Claude Code session hot-reloads edits to existing
	// SKILL.md files and must never observe a partial file.
	if err := writeFileAtomic(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		return nil, fmt.Errorf("write SKILL.md: %w", err)
	}

	if err := g.copyBundleResources(skill, skillDir); err != nil {
		return nil, err
	}

	// Prune the legacy command file this bundle supersedes. Claude Code would
	// otherwise keep resolving /name against both layouts.
	staleCommand := filepath.Join(baseDir, "commands", skill.Name+".md")
	if _, err := os.Stat(staleCommand); err == nil {
		if err := os.Remove(staleCommand); err != nil && g.Verbose {
			fmt.Printf("Warning: could not remove stale command %s: %v\n", staleCommand, err)
		}
	}

	return g.codexManifestFiles(skill), nil
}

// generateClaudeAgentSkillMD builds SKILL.md content with Claude Code
// frontmatter. skillDir is the final bundle path so ${SKILL_PATH} references
// resolve to the delivered location, not the registry source tree.
func (g *Generator) generateClaudeAgentSkillMD(skill *Skill, skillDir string) string {
	var sb strings.Builder

	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", skill.Name))

	desc := strings.TrimSpace(skill.Common.Description)
	desc = strings.ReplaceAll(desc, "\n", " ")
	sb.WriteString(fmt.Sprintf("description: \"%s\"\n", escapeYAMLString(desc)))

	if whenToUse := strings.TrimSpace(skill.GetWhenToUse("claude")); whenToUse != "" {
		whenToUse = strings.ReplaceAll(whenToUse, "\n", " ")
		sb.WriteString(fmt.Sprintf("when_to_use: \"%s\"\n", escapeYAMLString(whenToUse)))
	}
	if skill.GetDisableModelInvocation("claude") {
		sb.WriteString("disable-model-invocation: true\n")
	}
	if ctx := strings.TrimSpace(skill.GetContext("claude")); ctx != "" {
		sb.WriteString(fmt.Sprintf("context: %s\n", ctx))
	}
	sb.WriteString("---\n\n")

	instructions := skill.ResolveInstructions("claude", g.CodexHome, skillDir)
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

// generateClaudeCommand writes a Claude slash command to .claude/commands/<name>.md.
func (g *Generator) generateClaudeCommand(skill *Skill) ([]string, error) {
	baseDir := g.resolveTargetDir("claude")
	if baseDir == "" {
		baseDir = filepath.Join(g.WorkspaceRoot, ".claude")
	}
	outputDir := filepath.Join(baseDir, "commands")

	if g.Verbose {
		fmt.Printf("Generating Claude command: %s -> %s\n", skill.Name, outputDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create Claude command: %s/%s.md\n", outputDir, skill.Name)
		return []string{filepath.Join("commands", skill.Name+".md")}, nil
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create commands dir: %w", err)
	}

	var sb strings.Builder

	// YAML frontmatter with description
	desc := strings.TrimSpace(skill.Common.Description)
	desc = strings.ReplaceAll(desc, "\n", " ")
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("description: \"%s\"\n", escapeYAMLString(desc)))
	sb.WriteString("---\n\n")

	// Title and instructions
	title := toTitleCase(skill.Name)
	sb.WriteString(fmt.Sprintf("# %s\n\n", title))

	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	instructions := skill.ResolveInstructions("claude", g.CodexHome, sourceSkillDir)

	// Strip the first header since we already have the title
	instructions = stripFirstHeader(instructions)
	sb.WriteString(instructions)

	outputPath := filepath.Join(outputDir, skill.Name+".md")
	if err := os.WriteFile(outputPath, []byte(sb.String()), 0644); err != nil {
		return nil, fmt.Errorf("write command markdown: %w", err)
	}

	return []string{filepath.Join("commands", skill.Name+".md")}, nil
}

// generateClaudeRule writes a Claude rule to .claude/rules/<name>.md.
func (g *Generator) generateClaudeRule(skill *Skill) ([]string, error) {
	baseDir := g.resolveTargetDir("claude")
	if baseDir == "" {
		baseDir = filepath.Join(g.WorkspaceRoot, ".claude")
	}
	outputDir := filepath.Join(baseDir, "rules")

	if g.Verbose {
		fmt.Printf("Generating Claude rule: %s -> %s\n", skill.Name, outputDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create Claude rule: %s/%s.md\n", outputDir, skill.Name)
		return []string{filepath.Join("rules", skill.Name+".md")}, nil
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create rules dir: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("<!-- Generated by loom from skills-registry.yaml -->\n")

	title := toTitleCase(skill.Name)
	sb.WriteString(fmt.Sprintf("# %s\n\n", title))

	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	instructions := skill.ResolveInstructions("claude", g.CodexHome, sourceSkillDir)
	instructions = stripFirstHeader(instructions)
	sb.WriteString(instructions)

	outputPath := filepath.Join(outputDir, skill.Name+".md")
	if err := os.WriteFile(outputPath, []byte(sb.String()), 0644); err != nil {
		return nil, fmt.Errorf("write rule markdown: %w", err)
	}

	return []string{filepath.Join("rules", skill.Name+".md")}, nil
}

// generateClaudeSkill generates a Claude Code skill as a simple markdown file (legacy .agents/skills/ format).
func (g *Generator) generateClaudeSkill(skill *Skill) ([]string, error) {
	outputDir := g.OutputDir
	if outputDir == "" {
		outputDir = filepath.Join(g.WorkspaceRoot, ".agents", "skills")
	}

	if g.Verbose {
		fmt.Printf("Generating Claude skill: %s -> %s\n", skill.Name, outputDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create Claude skill: %s/%s.md\n", outputDir, skill.Name)
		return nil, nil
	}

	// Create output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	// Generate markdown file
	content := g.generateClaudeSkillMD(skill)
	outputPath := filepath.Join(outputDir, skill.Name+".md")
	if err := os.WriteFile(outputPath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("write skill markdown: %w", err)
	}

	return nil, nil
}

// generateClaudeSkillMD generates the markdown content for a Claude skill.
func (g *Generator) generateClaudeSkillMD(skill *Skill) string {
	var sb strings.Builder

	// Title from skill name (convert kebab-case to Title Case)
	title := toTitleCase(skill.Name)
	sb.WriteString(fmt.Sprintf("# %s\n\n", title))

	// Description
	desc := strings.TrimSpace(skill.Common.Description)
	sb.WriteString(desc)
	sb.WriteString("\n\n")

	// Instructions with resolved paths
	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	instructions := skill.ResolveInstructions("claude", g.CodexHome, sourceSkillDir)

	// For Claude, we strip the first header since we already have the title
	instructions = stripFirstHeader(instructions)
	sb.WriteString(instructions)

	// Add script references section if there are scripts
	if len(skill.Common.Scripts) > 0 {
		sb.WriteString("\n## Scripts\n\n")
		for _, script := range skill.Common.Scripts {
			scriptPath := filepath.Join(sourceSkillDir, script.Path)
			desc := script.Description
			if desc == "" {
				desc = "Script"
			}
			sb.WriteString(fmt.Sprintf("- `%s` - %s\n", scriptPath, desc))
		}
	}

	// Add reference to source location
	sb.WriteString("\n## Source\n\n")
	sb.WriteString(fmt.Sprintf("Skill source: `%s`\n", sourceSkillDir))

	return sb.String()
}

// generateKilocodeSkill generates a Kilocode skill based on its type.
func (g *Generator) generateKilocodeSkill(skill *Skill) ([]string, error) {
	skillType := skill.GetType("kilocode")

	switch skillType {
	case "workflow":
		return g.generateKilocodeWorkflow(skill)
	case "rule":
		return g.generateKilocodeRule(skill)
	default:
		// Default to rule format
		return g.generateKilocodeRule(skill)
	}
}

// generateKilocodeRule writes a Kilocode rule to .kilocode/rules/<name>.md.
func (g *Generator) generateKilocodeRule(skill *Skill) ([]string, error) {
	baseDir := g.resolveTargetDir("kilocode")
	if baseDir == "" {
		baseDir = filepath.Join(g.RepoRoot, ".kilocode")
	}
	outputDir := filepath.Join(baseDir, "rules")

	if g.Verbose {
		fmt.Printf("Generating Kilocode rule: %s -> %s\n", skill.Name, outputDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create Kilocode rule: %s/%s.md\n", outputDir, skill.Name)
		return []string{filepath.Join("rules", skill.Name+".md")}, nil
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create rules dir: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("<!-- Generated by loom from skills-registry.yaml -->\n")

	title := toTitleCase(skill.Name)
	sb.WriteString(fmt.Sprintf("# %s\n\n", title))

	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	instructions := skill.ResolveInstructions("kilocode", g.CodexHome, sourceSkillDir)
	instructions = stripFirstHeader(instructions)
	sb.WriteString(instructions)

	outputPath := filepath.Join(outputDir, skill.Name+".md")
	if err := os.WriteFile(outputPath, []byte(sb.String()), 0644); err != nil {
		return nil, fmt.Errorf("write rule markdown: %w", err)
	}

	return []string{filepath.Join("rules", skill.Name+".md")}, nil
}

// generateKilocodeWorkflow writes a Kilocode workflow to .kilocode/workflows/<name>.yaml.
func (g *Generator) generateKilocodeWorkflow(skill *Skill) ([]string, error) {
	baseDir := g.resolveTargetDir("kilocode")
	if baseDir == "" {
		baseDir = filepath.Join(g.RepoRoot, ".kilocode")
	}
	outputDir := filepath.Join(baseDir, "workflows")

	if g.Verbose {
		fmt.Printf("Generating Kilocode workflow: %s -> %s\n", skill.Name, outputDir)
	}

	if g.DryRun {
		fmt.Printf("[dry-run] Would create Kilocode workflow: %s/%s.yaml\n", outputDir, skill.Name)
		return []string{filepath.Join("workflows", skill.Name+".yaml")}, nil
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, fmt.Errorf("create workflows dir: %w", err)
	}

	sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)
	instructions := skill.ResolveInstructions("kilocode", g.CodexHome, sourceSkillDir)

	desc := strings.TrimSpace(skill.Common.Description)
	desc = strings.ReplaceAll(desc, "\n", " ")

	var sb strings.Builder
	sb.WriteString("# Generated by loom from skills-registry.yaml\n")
	sb.WriteString("version: 1\n")
	sb.WriteString(fmt.Sprintf("name: \"%s\"\n", toTitleCase(skill.Name)))
	sb.WriteString(fmt.Sprintf("description: \"%s\"\n", escapeYAMLString(desc)))
	sb.WriteString("on:\n")
	sb.WriteString(fmt.Sprintf("  slash_command: \"/%s\"\n", skill.Name))
	sb.WriteString("steps:\n")
	sb.WriteString("  - id: execute\n")
	sb.WriteString("    prompt:\n")
	sb.WriteString("      content: |\n")

	// Indent instructions for YAML block scalar
	for _, line := range strings.Split(instructions, "\n") {
		if line == "" {
			sb.WriteString("\n")
		} else {
			sb.WriteString("        " + line + "\n")
		}
	}

	outputPath := filepath.Join(outputDir, skill.Name+".yaml")
	if err := os.WriteFile(outputPath, []byte(sb.String()), 0644); err != nil {
		return nil, fmt.Errorf("write workflow yaml: %w", err)
	}

	return []string{filepath.Join("workflows", skill.Name+".yaml")}, nil
}

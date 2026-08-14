// generator_validation.go contains skill validation and registry date update logic.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ValidationError describes a missing resource in a skill.
type ValidationError struct {
	Skill        string // Skill name
	ResourceType string // "script", "reference", or "asset"
	Path         string // Expected path on disk
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: missing %s: %s", e.Skill, e.ResourceType, e.Path)
}

var writeLikeScriptPattern = regexp.MustCompile(`(?i)(\b(os\.writefile|write_text|write_bytes|mkdir|mkdtemp|copy2|copyfile|rename|unlink|rmtree)\b|\b(mkdir|cp|mv|rm|touch|install)\b|\b(kubectl\s+apply|git\s+(add|commit|push)|sed\s+-i|perl\s+-i)\b|>>|>\s*[[:alnum:]_./-])`)

// devNullRedirectPattern matches output-discarding redirects (>/dev/null,
// 2>/dev/null, &>/dev/null, >&2) which are not file writes and must not
// count against always_allow safety.
var devNullRedirectPattern = regexp.MustCompile(`(?i)([12&]?>{1,2}\s*/dev/null|>&\s*[12])`)

// nonWriteAnglePattern matches ">" usages that are not shell redirects:
// "->" (Python/Go/Rust arrows, HTML comments) and numeric comparisons
// like "> 0" or ">= 100".
var nonWriteAnglePattern = regexp.MustCompile(`(->|>=?\s*\d+\b)`)

// alwaysAllowMarker is an explicit per-script opt-in for always_allow.
// A script containing this marker (in a comment, with a rationale) is
// accepted even if it matches write-like patterns — used for verification
// and snapshot scripts whose writes are their expected, scoped output.
const alwaysAllowMarker = "loom-always-allow"

// claudeListingCharCap is Claude Code's cap on the combined
// description+when_to_use length shown in skill listings; the host silently
// truncates beyond it (code.claude.com/docs/en/skills frontmatter reference).
const claudeListingCharCap = 1536

func scriptByName(spec *SkillSpec, name string) (*Script, bool) {
	for _, script := range spec.Scripts {
		if script == nil || strings.TrimSpace(script.Name) == "" {
			continue
		}
		if script.Name == name {
			return script, true
		}
	}
	return nil, false
}

func scriptIsAlwaysAllowSafe(path string) (bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	text := strings.ToLower(string(content))
	if strings.Contains(text, "--dry-run") || strings.Contains(text, alwaysAllowMarker) {
		return true, nil
	}

	// Output-discarding redirects and non-redirect ">" usages (arrows,
	// numeric comparisons) are not file writes.
	text = devNullRedirectPattern.ReplaceAllString(text, "")
	text = nonWriteAnglePattern.ReplaceAllString(text, "")

	return !writeLikeScriptPattern.MatchString(text), nil
}

// Validate checks that all scripts, references, and assets referenced by
// enabled skills exist on disk. Returns a slice of validation errors (empty
// means everything is valid).
func (g *Generator) Validate() []ValidationError {
	var errs []ValidationError

	targets := []string{g.Target}
	if g.Target == "all" {
		targets = AllTargets
	}

	// Collect unique enabled skills across requested targets.
	seen := make(map[string]bool)
	var skills []*Skill
	claudeRequested := false
	for _, target := range targets {
		if target == "claude" {
			claudeRequested = true
		}
		for _, skill := range g.Registry.Skills {
			if skill.IsEnabled(target) && !seen[skill.Name] {
				seen[skill.Name] = true
				skills = append(skills, skill)
			}
		}
	}

	for _, skill := range skills {
		if skill.Common == nil {
			continue
		}
		sourceSkillDir := filepath.Join(g.SourceDir, skill.Name)

		// Claude Code appends when_to_use to the description in skill
		// listings and truncates the combination at 1536 characters —
		// anything past the cap silently disappears from trigger matching.
		if claudeRequested && skill.IsEnabled("claude") {
			desc := strings.TrimSpace(skill.Common.Description)
			whenToUse := strings.TrimSpace(skill.GetWhenToUse("claude"))
			if combined := len(desc) + len(whenToUse); combined > claudeListingCharCap {
				errs = append(errs, ValidationError{
					Skill:        skill.Name,
					ResourceType: "when_to_use",
					Path:         fmt.Sprintf("description+when_to_use is %d chars (Claude listing cap %d)", combined, claudeListingCharCap),
				})
			}
		}

		for _, script := range skill.Common.Scripts {
			if script == nil || script.Path == "" {
				continue
			}
			p := filepath.Join(sourceSkillDir, script.Path)
			if _, err := os.Stat(p); err != nil {
				errs = append(errs, ValidationError{
					Skill:        skill.Name,
					ResourceType: "script",
					Path:         p,
				})
			}
		}
		for _, ref := range skill.Common.References {
			p := filepath.Join(sourceSkillDir, "references", ref)
			if _, err := os.Stat(p); err != nil {
				errs = append(errs, ValidationError{
					Skill:        skill.Name,
					ResourceType: "reference",
					Path:         p,
				})
			}
		}
		for _, asset := range skill.Common.Assets {
			p := filepath.Join(sourceSkillDir, "assets", asset)
			if _, err := os.Stat(p); err != nil {
				errs = append(errs, ValidationError{
					Skill:        skill.Name,
					ResourceType: "asset",
					Path:         p,
				})
			}
		}

		for _, allow := range skill.Common.AlwaysAllow {
			allow = strings.TrimSpace(allow)
			if allow == "" {
				continue
			}
			script, ok := scriptByName(skill.Common, allow)
			if !ok || strings.TrimSpace(script.Path) == "" {
				errs = append(errs, ValidationError{
					Skill:        skill.Name,
					ResourceType: "always_allow",
					Path:         fmt.Sprintf("%s (not found in common.scripts)", allow),
				})
				continue
			}
			p := filepath.Join(sourceSkillDir, script.Path)
			safe, err := scriptIsAlwaysAllowSafe(p)
			if err != nil {
				errs = append(errs, ValidationError{
					Skill:        skill.Name,
					ResourceType: "always_allow",
					Path:         fmt.Sprintf("%s (%v)", p, err),
				})
				continue
			}
			if !safe {
				errs = append(errs, ValidationError{
					Skill:        skill.Name,
					ResourceType: "always_allow",
					Path:         fmt.Sprintf("%s (write-capable script without --dry-run default)", p),
				})
			}
		}
	}

	return errs
}

// updatedLineRe matches the top-level `updated:` field in the registry YAML.
var updatedLineRe = regexp.MustCompile(`(?m)^updated:\s*.*$`)

// UpdateRegistryDate rewrites the `updated:` field in the on-disk registry
// YAML to today's date (YYYY-MM-DD). The replacement is a targeted line swap
// so the rest of the file (comments, ordering, formatting) is preserved.
// Returns nil if RegistryPath is empty or the file has no `updated:` line.
func (g *Generator) UpdateRegistryDate() error {
	if g.RegistryPath == "" {
		return nil
	}

	today := time.Now().Format("2006-01-02")
	newLine := "updated: " + today

	data, err := os.ReadFile(g.RegistryPath)
	if err != nil {
		return fmt.Errorf("read registry for date update: %w", err)
	}

	original := string(data)
	if !updatedLineRe.MatchString(original) {
		return nil // no updated: field to patch
	}

	updated := updatedLineRe.ReplaceAllString(original, newLine)
	if updated == original {
		return nil // already current
	}

	if g.Verbose {
		fmt.Printf("Updating registry date to %s\n", today)
	}

	return os.WriteFile(g.RegistryPath, []byte(updated), 0644)
}

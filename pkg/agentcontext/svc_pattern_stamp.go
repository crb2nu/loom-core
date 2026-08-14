// svc_pattern_stamp.go -- the STAMP verb: pattern + materials -> Plan.
//
// Stamping is the production operation of the Pattern Loom. Given a vetted
// Pattern and a bundle of Materials, it (1) validates the materials against the
// pattern's materials_schema, (2) records the required tools manifest, and (3)
// expands the pattern's slice_template (with material substitution) into a
// concrete Plan in the shared store. The Plan is then executable by Mills.
//
// SCOPE (S1 core): materials validation + Plan expansion are built and tested
// here. Live tools-manifest probing (devbox/gitlab/flux presence) and the Mills
// BacklogItem projection + pipeline run are the integration step, deferred until
// the merge path stabilizes; the result surfaces tools_required so a caller (the
// B1 front door) can gate on them.
package agentcontext

import (
	"context"
	"fmt"
	"path"
	"strings"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// HandlePatternStamp orchestrates a stamp across the pattern catalog and the
// plan store (both held by Service).
func (s *Service) HandlePatternStamp(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return stampPattern(ctx, s.patterns, s.plans, args)
}

// stampPattern is the testable core. It takes the two services directly so it
// can be unit-tested without constructing a full Service.
func stampPattern(ctx context.Context, patterns *PatternSvc, plans *PlanSvc, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	patternID := v.Required("pattern_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	materials, _ := args["materials"].(map[string]any)
	if materials == nil {
		return mcp.ErrorResult(fmt.Errorf("materials object is required")), nil
	}

	pattern, err := patterns.fetch(ctx, patternID)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("load pattern: %w", err)), nil
	}
	if pattern == nil {
		return mcp.ErrorResult(fmt.Errorf("pattern %q not found", patternID)), nil
	}

	// 1) Validate + normalize materials against the schema. The returned subs map
	// drives placeholder substitution in the slice template.
	subs, err := validateAndNormalizeMaterials(pattern.MaterialsSchema, materials)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("materials validation: %w", err)), nil
	}

	// 2) Collect the required tools manifest and, when the caller supplies the
	// probed environment (available_tools), ENFORCE it: a pattern names exactly
	// the tools its follower needs, so a stamp into an environment missing one
	// is a guaranteed failure — abort loudly here instead of producing a Plan
	// that can't be built. agent-context can't itself invoke devbox_status /
	// verify_token / flux_probe (it is an MCP server, not a client), so the
	// caller (HUD/CLI/operator) does the live probe and passes the result; with
	// no available_tools the manifest is surfaced but not gated (back-compat).
	var toolsRequired []string
	for _, t := range pattern.ToolsManifest {
		if t.Required {
			toolsRequired = append(toolsRequired, t.Name)
		}
	}
	availableTools := v.StringSlice("available_tools")
	toolsProbed := len(availableTools) > 0
	if toolsProbed {
		available := make(map[string]bool, len(availableTools))
		for _, a := range availableTools {
			available[a] = true
		}
		var missing []string
		for _, t := range pattern.ToolsManifest {
			if t.Required && !toolRequirementMet(t, available) {
				missing = append(missing, t.Name)
			}
		}
		if len(missing) > 0 {
			return mcp.ErrorResult(fmt.Errorf(
				"stamp aborted: required tools missing from the environment: %s — a pattern stamps only where its tools_manifest is satisfied",
				strings.Join(missing, ", "))), nil
		}
	}

	// 2b) Resolve the optional target_dir material: where inside the HOST repo
	// the stamped files land. A pattern's slice template is written repo-root
	// relative (a fresh, dedicated repo); when Mills executes the stamp against
	// an existing repo (its single configured repo is a monorepo), an empty
	// target_dir would instruct the agent to overwrite the host's own root
	// go.mod/Makefile/README. Prefixing every slice file confines the stamp.
	targetDir, err := sanitizeTargetDir(subs["target_dir"])
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("materials validation: %w", err)), nil
	}
	subs["target_dir"] = targetDir

	// 3) Expand the slice template into concrete seed slices.
	seedSlices := make([]any, 0, len(pattern.SliceTemplate))
	for _, tpl := range pattern.SliceTemplate {
		files := make([]any, 0, len(tpl.Files))
		for _, f := range tpl.Files {
			files = append(files, path.Join(targetDir, substituteMaterials(f, subs)))
		}
		goal := substituteMaterials(tpl.Goal, subs)
		if targetDir != "" {
			goal += "\n\nAll files for this slice live under `" + targetDir + "/` inside the host repository (a self-contained project with its own module/manifest and build files); create the directory tree as needed and do NOT create or modify any file outside it — the host repo root has its own top-level build files."
		}
		if tpl.AcceptanceCriteria != "" {
			goal = strings.TrimSpace(goal + "\n\nAcceptance: " + substituteMaterials(tpl.AcceptanceCriteria, subs))
		}
		seedSlices = append(seedSlices, map[string]any{
			"name":  substituteMaterials(tpl.Name, subs),
			"goal":  goal,
			"files": files,
		})
	}

	// Build a deterministic plan id from the pattern + the primary material.
	// Patterns name their primary differently by what they make (a service, a
	// CLI tool); falling through to the slug keeps the id stable but collapses
	// all stamps of that pattern onto one plan, so patterns should always
	// declare one of the recognized primaries.
	primary := firstNonEmpty(subs["service_name"], subs["tool_name"], subs["name"], pattern.Slug)
	planID := "plan-stamp-" + patternSlug(pattern.Slug+"-"+primary)
	title := "Stamp: " + pattern.Makes + " — " + primary

	planArgs := map[string]any{
		"id":        planID,
		"title":     title,
		"project":   v.String("project", ""),
		"namespace": v.String("namespace", ""),
		"agent_id":  v.String("agent_id", ""),
		"phase":     PlanPhasePlanned,
		"spec_doc":  buildStampManifest(pattern, subs, toolsRequired),
		"slices":    seedSlices,
	}
	if _, err := plans.Create(ctx, planArgs); err != nil {
		return mcp.ErrorResult(fmt.Errorf("create stamped plan: %w", err)), nil
	}

	return mcp.JSONResult(map[string]any{
		"ok":              true,
		"plan_id":         planID,
		"pattern_id":      pattern.ID,
		"pattern_version": pattern.Version,
		"materials":       subs,
		"slices":          seedSlices,
		"slice_count":     len(seedSlices),
		"tools_required":  toolsRequired,
		"tools_probed":    toolsProbed,
		"deploy_contract": substituteMaterials(pattern.DeployContract, subs),
		"note":            "Plan expanded in the store. Pass available_tools to enforce the tools_manifest; the Mills BacklogItem projection is the HUD enqueue path (POST /api/patterns/stamp enqueue=true).",
	})
}

// validateAndNormalizeMaterials checks supplied materials against the schema and
// returns a substitution map for template expansion. Enforces required fields
// and enum membership; applies defaults; derives entity.name / entity_lower for
// object materials named "entity".
func validateAndNormalizeMaterials(schema []MaterialField, materials map[string]any) (map[string]string, error) {
	subs := map[string]string{}
	for _, f := range schema {
		raw, present := materials[f.Name]
		missing := !present || raw == nil || raw == ""
		if missing {
			if f.Required && f.Default == "" {
				return nil, fmt.Errorf("missing required material %q", f.Name)
			}
			if f.Default != "" {
				subs[f.Name] = f.Default
			}
			continue
		}
		switch f.Type {
		case "object":
			subs[f.Name] = marshalJSON(raw)
			if m, ok := raw.(map[string]any); ok {
				if name := toString(m["name"]); name != "" {
					subs[f.Name+".name"] = name
					subs[f.Name+"_lower"] = strings.ToLower(name)
				}
			}
		case "list":
			subs[f.Name] = marshalJSON(raw)
		default:
			val := toString(raw)
			if f.Type == "enum" && len(f.Enum) > 0 && !stringSliceContains(f.Enum, val) {
				return nil, fmt.Errorf("material %q = %q is not one of %v", f.Name, val, f.Enum)
			}
			subs[f.Name] = val
		}
	}
	// Resolve placeholders nested inside default values (e.g. a module_path
	// default of "github.com/crb2nu/{{service_name}}"), one level deep.
	for k, val := range subs {
		subs[k] = substituteMaterials(val, subs)
	}
	return subs, nil
}

// substituteMaterials replaces {{key}} tokens in s with their values from subs.
func substituteMaterials(s string, subs map[string]string) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	for k, v := range subs {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	return s
}

// buildStampManifest renders the canonical spec_doc for a stamped plan: which
// pattern + version produced it, the resolved materials, the pinned axes, the
// gauge, and the deploy contract. This is the human-reviewable provenance.
func buildStampManifest(p *Pattern, subs map[string]string, toolsRequired []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Stamped from `%s` v%s\n\n", p.ID, p.Version)
	fmt.Fprintf(&b, "**Makes**: %s\n\n", p.Makes)
	b.WriteString("Generated by the Mills Pattern Loom stamp. The pattern pins the architecture; only the materials below vary.\n\n")

	b.WriteString("## Materials\n\n")
	for _, f := range p.MaterialsSchema {
		if val, ok := subs[f.Name]; ok {
			fmt.Fprintf(&b, "- `%s` = %s\n", f.Name, val)
		}
	}
	if len(toolsRequired) > 0 {
		fmt.Fprintf(&b, "\n## Required tools\n\n%s\n", strings.Join(toolsRequired, ", "))
	}
	if len(p.Pins) > 0 {
		b.WriteString("\n## Pinned architecture\n\n")
		for _, pin := range p.Pins {
			fmt.Fprintf(&b, "- **%s**: %s\n", pin.Axis, pin.Value)
		}
	}
	if p.Gauge != nil {
		b.WriteString("\n## Gauge (acceptance swatch)\n\n")
		for _, c := range p.Gauge.Commands {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
		for _, a := range p.Gauge.Assertions {
			fmt.Fprintf(&b, "- %s\n", substituteMaterials(a, subs))
		}
	}
	if dc := substituteMaterials(p.DeployContract, subs); dc != "" {
		fmt.Fprintf(&b, "\n## Deploy contract\n\n%s\n", dc)
	}
	return b.String()
}

// sanitizeTargetDir normalizes the optional target_dir material into a clean,
// repo-relative path ("" = repo root, the fresh-repo case). Absolute paths and
// paths escaping the repo (".." after cleaning) are rejected: a stamp must not
// be able to write outside the repository it targets.
func sanitizeTargetDir(td string) (string, error) {
	td = strings.TrimSpace(td)
	if td == "" {
		return "", nil
	}
	if strings.HasPrefix(td, "/") {
		return "", fmt.Errorf("target_dir %q must be repo-relative, not absolute", td)
	}
	cleaned := path.Clean(td)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("target_dir %q escapes the repository root", td)
	}
	return cleaned, nil
}

// stringSliceContains reports whether s is in xs.
func stringSliceContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// firstNonEmpty returns the first non-empty string, or "" if none.
func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// toolRequirementMet reports whether a required tool is satisfied by the
// caller-probed environment. A requirement is met when available_tools names
// the capability by its manifest Name, by its Check token, or via an
// MCP-namespaced entry that embeds the Name (e.g. "devbox" is satisfied by
// "devbox__devbox_status"). Matching is intentionally lenient so a caller can
// pass either capability names ("devbox","gitlab") or raw MCP tool names.
func toolRequirementMet(req ToolRequirement, available map[string]bool) bool {
	if req.Name != "" && available[req.Name] {
		return true
	}
	if req.Check != "" && available[req.Check] {
		return true
	}
	if req.Name == "" {
		return false
	}
	for a := range available {
		if strings.Contains(a, req.Name) {
			return true
		}
	}
	return false
}

// svc_plan_render.go -- Store→markdown mirror. The Plan in Qdrant is canonical;
// this renders a human/MR-reviewable `.loom/*.md` projection of it. The file is
// written atomically (tempfile + rename) because external watchers (codex,
// gemini, fs inotify) read `.loom/` and a non-atomic O_TRUNC write exposes a
// partial-read window. Agents read the STORE by plan_id; this file is the
// review snapshot.
package agentcontext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// HandlePlanRender renders a plan's markdown mirror, optionally writing it.
func (s *Service) HandlePlanRender(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.plans.Render(ctx, args)
}

// Render returns the markdown projection of a plan and, when a path is given,
// writes it atomically and records the path as the plan's mirror_path.
func (ps *PlanSvc) Render(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	planID := v.Required("plan_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	plan, err := ps.fetch(ctx, planID)
	if err != nil || plan == nil {
		return mcp.ErrorResult(fmt.Errorf("plan %q not found", planID)), nil
	}
	plan.Slices = ps.slicesForPlan(ctx, planID)

	md := renderPlanMarkdown(plan)

	out := map[string]any{
		"ok":       true,
		"plan_id":  plan.ID,
		"markdown": md,
		"bytes":    len(md),
	}

	if path := v.String("path", ""); path != "" {
		if err := writePlanMirrorAtomic(path, md); err != nil {
			return mcp.ErrorResult(fmt.Errorf("write mirror: %w", err)), nil
		}
		out["path"] = path
		// Record the mirror path on the plan (best-effort; default on).
		if v.Bool("set_mirror_path", true) && plan.MirrorPath != path {
			plan.MirrorPath = path
			plan.UpdatedAt = time.Now().UTC()
			if perr := ps.persist(ctx, plan); perr != nil {
				ps.logger.Warn("record mirror_path failed", "plan_id", plan.ID, "error", perr)
			} else {
				ps.mu.Lock()
				ps.plans[plan.ID] = plan
				ps.mu.Unlock()
			}
		}
	}
	return mcp.JSONResult(out)
}

// renderPlanMarkdown produces the deterministic markdown projection of a plan.
// Pure function — no I/O — so it is directly unit-testable.
func renderPlanMarkdown(p *Plan) string {
	var b strings.Builder
	title := p.Title
	if title == "" {
		title = p.Slug
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	fmt.Fprintf(&b, "- **Plan ID**: `%s`\n", p.ID)
	fmt.Fprintf(&b, "- **Phase**: %s\n", p.Phase)
	if p.Project != "" {
		fmt.Fprintf(&b, "- **Project**: %s\n", p.Project)
	}
	if p.Namespace != "" {
		fmt.Fprintf(&b, "- **Namespace**: %s\n", p.Namespace)
	}
	if p.CreatedBy != "" {
		fmt.Fprintf(&b, "- **Created by**: %s\n", p.CreatedBy)
	}
	if !p.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "- **Created**: %s\n", p.CreatedAt.Format(time.RFC3339))
	}
	if !p.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "- **Updated**: %s\n", p.UpdatedAt.Format(time.RFC3339))
	}
	if p.MillsBacklogID != "" {
		fmt.Fprintf(&b, "- **Mills backlog**: `%s`\n", p.MillsBacklogID)
	}
	if p.GitLabIssueIID > 0 {
		fmt.Fprintf(&b, "- **GitLab issue**: #%d\n", p.GitLabIssueIID)
	}
	b.WriteString("\n> Rendered from the Loom plan store (canonical). ")
	b.WriteString("Edit via `agent_plan_*` tools, not this file.\n")

	if p.RiskiestAssumption != "" || p.KillTest != "" {
		b.WriteString("\n## Riskiest assumption + kill-test\n\n")
		if p.RiskiestAssumption != "" {
			fmt.Fprintf(&b, "**Assumption**: %s\n\n", p.RiskiestAssumption)
		}
		if p.KillTest != "" {
			fmt.Fprintf(&b, "**Kill test**: %s\n\n", p.KillTest)
		}
		if p.KillTestStatus != "" {
			fmt.Fprintf(&b, "**Status**: %s\n", p.KillTestStatus)
		}
	}

	if p.Success != nil {
		b.WriteString("\n## Success criteria\n\n")
		for _, t := range p.Success.Tests {
			fmt.Fprintf(&b, "- Test: %s\n", t)
		}
		for _, m := range p.Success.Metrics {
			fmt.Fprintf(&b, "- Metric: %s\n", m)
		}
		if p.Success.ManualCheck != "" {
			fmt.Fprintf(&b, "- Manual: %s\n", p.Success.ManualCheck)
		}
	}

	if len(p.Dependencies) > 0 {
		b.WriteString("\n## Dependencies\n\n")
		for _, d := range p.Dependencies {
			fmt.Fprintf(&b, "- `%s`\n", d)
		}
	}

	if len(p.MRRefs) > 0 || len(p.PipelineRefs) > 0 || len(p.DeployRefs) > 0 {
		b.WriteString("\n## Lifecycle refs\n\n")
		if len(p.MRRefs) > 0 {
			fmt.Fprintf(&b, "- MRs: %s\n", strings.Join(p.MRRefs, ", "))
		}
		if len(p.PipelineRefs) > 0 {
			fmt.Fprintf(&b, "- Pipelines: %s\n", strings.Join(p.PipelineRefs, ", "))
		}
		if len(p.DeployRefs) > 0 {
			fmt.Fprintf(&b, "- Deploys: %s\n", strings.Join(p.DeployRefs, ", "))
		}
	}

	if len(p.PhaseHistory) > 0 {
		b.WriteString("\n## Phase history\n\n")
		b.WriteString("| From | To | At | Actor | Note |\n|---|---|---|---|---|\n")
		for _, h := range p.PhaseHistory {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				h.From, h.To, h.At.Format(time.RFC3339), h.Actor, oneLine(h.Note))
		}
	}

	if strings.TrimSpace(p.SpecDoc) != "" {
		b.WriteString("\n## Spec\n\n")
		b.WriteString(strings.TrimRight(p.SpecDoc, "\n"))
		b.WriteString("\n")
	}

	if len(p.Slices) > 0 {
		b.WriteString("\n## Slices\n\n")
		for _, s := range p.Slices {
			fmt.Fprintf(&b, "### %d. %s — `%s`\n\n", s.Order, s.Name, s.Phase)
			fmt.Fprintf(&b, "- **Slice ID**: `%s`\n", s.ID)
			if s.Goal != "" {
				fmt.Fprintf(&b, "- **Goal**: %s\n", s.Goal)
			}
			if len(s.Files) > 0 {
				fmt.Fprintf(&b, "- **Files**: %s\n", strings.Join(s.Files, ", "))
			}
			if s.BranchName != "" {
				fmt.Fprintf(&b, "- **Branch**: %s\n", s.BranchName)
			}
			if s.AssignedAgentID != "" {
				fmt.Fprintf(&b, "- **Assignee**: %s\n", s.AssignedAgentID)
			}
			if len(s.DependsOn) > 0 {
				fmt.Fprintf(&b, "- **Depends on**: %s\n", strings.Join(s.DependsOn, ", "))
			}
			if s.AcceptanceCriteria != "" {
				fmt.Fprintf(&b, "- **Acceptance**: %s\n", s.AcceptanceCriteria)
			}
			if s.MRRef != "" {
				fmt.Fprintf(&b, "- **MR**: %s\n", s.MRRef)
			}
			for _, d := range s.Decisions {
				fmt.Fprintf(&b, "- **Decision**: %s\n", oneLine(d))
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// oneLine collapses newlines so a value stays inside a markdown table cell.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

// writePlanMirrorAtomic writes the mirror via a same-directory tempfile + rename
// so watchers never observe a partial file. Mirrors pkg/skills.writeFileAtomic
// (which is unexported).
func writePlanMirrorAtomic(path, content string) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".plan-*.md.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

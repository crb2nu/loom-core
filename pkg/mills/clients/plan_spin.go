package clients

// plan_spin.go -- the Spinning Room's write surface on the Plan Store (Live
// Beam slice 3 / F2). AuthorDraftPlan authors a phase=draft plan carrying the
// operator's chosen frame's decomposition (slices), a warp-beam priority, and
// an audit trail (which frame/model/backend spun it, from what brief). Draft
// plans are deliberately NOT emitted by the plan-slice emitter — the operator
// reviews the draft and advances it to planned/in_progress before the beam
// picks it up.
//
// *PlanClient satisfies spin.DraftPlanAuthor; the operator's spin handler holds
// the interface, not the concrete type, so the spinner stays testable.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/spin"
)

// spinningRoomActor is the attribution recorded as the draft plan's creator.
const spinningRoomActor = "mills:spinning-room"

// AuthorDraftPlan creates a phase=draft plan from a spun decomposition and
// returns its plan_id. Unlike AuthorSlicedPlan (which authors a planned plan
// with a deterministic, title-derived id so a re-running council upserts), a
// spin is an operator action: the store mints a fresh id so re-spinning a brief
// yields a new draft rather than clobbering the one under review.
func (c *PlanClient) AuthorDraftPlan(ctx context.Context, in spin.DraftPlanInput) (string, error) {
	if c == nil || c.Hub == nil {
		return "", errors.New("plan: client not configured")
	}
	if strings.TrimSpace(in.Title) == "" {
		return "", errors.New("plan: draft plan title required")
	}
	if len(in.Slices) == 0 {
		return "", errors.New("plan: draft plan needs >=1 slice")
	}
	slices := make([]map[string]any, 0, len(in.Slices))
	for _, s := range in.Slices {
		if strings.TrimSpace(s.Name) == "" {
			continue
		}
		sl := map[string]any{"name": s.Name}
		if g := strings.TrimSpace(s.Goal); g != "" {
			sl["goal"] = g
		}
		if len(s.Files) > 0 {
			sl["files"] = s.Files
		}
		// Connective tissue: depends_on is emitted by slice NAME (the store
		// resolves names to slice_ids); interface_contracts + acceptance_criteria
		// are free-form. Omitted when empty so a sparse frame authors a clean slice.
		if len(s.DependsOn) > 0 {
			sl["depends_on"] = s.DependsOn
		}
		if ic := strings.TrimSpace(s.InterfaceContracts); ic != "" {
			sl["interface_contracts"] = ic
		}
		if ac := strings.TrimSpace(s.AcceptanceCriteria); ac != "" {
			sl["acceptance_criteria"] = ac
		}
		slices = append(slices, sl)
	}
	if len(slices) == 0 {
		return "", errors.New("plan: draft plan has no named slices")
	}

	args := map[string]any{
		"title":    in.Title,
		"phase":    "draft",
		"slices":   slices,
		"spec_doc": draftSpecDoc(in),
		"agent_id": spinningRoomActor,
	}
	if o := strings.TrimSpace(in.Objective); o != "" {
		args["objective"] = o
	}
	if p := strings.ToUpper(strings.TrimSpace(in.Priority)); p != "" {
		args["priority"] = p
	}
	if p := strings.TrimSpace(in.Project); p != "" {
		args["project"] = p
	}
	if ns := strings.TrimSpace(in.Namespace); ns != "" {
		args["namespace"] = ns
	}
	if rf := strings.TrimSpace(in.RespunFrom); rf != "" {
		// Provenance: link this fresh draft back to the plan it redoes so the HUD
		// can surface "respun from …" and offer a one-click supersede.
		args["respun_from"] = rf
	}

	body, err := c.Hub.CallTool(ctx, c.serverName(), "agent_plan_create", args)
	if err != nil && body == "" {
		return "", fmt.Errorf("plan: author draft: %w", err)
	}
	parsed, perr := decodePlanCreateResponse(body)
	if perr != nil {
		return "", fmt.Errorf("plan: author draft decode: %w; raw=%q", perr, truncateBody(body, 240))
	}
	if !parsed.OK && parsed.PlanID == "" {
		return "", fmt.Errorf("plan: author draft reported failure: %q", truncateBody(body, 240))
	}
	return parsed.PlanID, nil
}

// draftSpecDoc renders the audit header + the roving brief into the plan's
// spec_doc so a reviewer sees which frame spun it, from what brief, and what it
// cost — the run-scoped provenance the F2 governance line requires recorded on
// the plan row.
func draftSpecDoc(in spin.DraftPlanInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", in.Title)
	b.WriteString("_Spun in the Mills Spinning Room (draft — review before advancing to `planned`)._\n\n")
	frame := strings.TrimSpace(in.Frame)
	if frame == "" {
		frame = "(unnamed)"
	}
	fmt.Fprintf(&b, "- **Frame**: %s\n", frame)
	if m := strings.TrimSpace(in.Model); m != "" {
		fmt.Fprintf(&b, "- **Model**: %s\n", m)
	}
	if bk := strings.TrimSpace(in.Backend); bk != "" {
		fmt.Fprintf(&b, "- **Backend**: %s\n", bk)
	}
	if len(in.Competitors) > 0 {
		// Competitive spin (F2): sibling drafts were spun from the same roving
		// on these frames — compare before advancing one and abandon the rest.
		fmt.Fprintf(&b, "- **Competing frames**: %s\n", strings.Join(in.Competitors, ", "))
	}
	if rf := strings.TrimSpace(in.RespunFrom); rf != "" {
		fmt.Fprintf(&b, "- **Respun from**: `%s`\n", rf)
	}
	fmt.Fprintf(&b, "- **Spun at**: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("## Brief (roving)\n\n")
	b.WriteString(strings.TrimSpace(in.Brief))
	b.WriteString("\n")
	return b.String()
}

// compile-time assertion that *PlanClient satisfies the spinning room's draft
// plan author.
var _ spin.DraftPlanAuthor = (*PlanClient)(nil)

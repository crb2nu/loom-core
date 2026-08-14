// svc_pattern_taste.go -- the taste gate: how a Pattern earns "approved".
//
// A pattern is registered as a `candidate` and earns `approved` either by human
// curation (explicit promote, the "we find this tasteful" judgement) or by
// shipping enough green instances (instances_shipped_green >= threshold). The
// Mills council rails (A1) and the front door (B1) offer `approved` patterns by
// default, so the taste gate is what controls which patterns the factory will
// actually stamp.
//
// RecordInstance is the hook the stamp/merge path (A2) calls when a stamped
// instance merges green; it increments the green count and auto-promotes a
// candidate once the threshold is reached.
package agentcontext

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// PatternApprovalThreshold is the default number of green-shipped instances at
// which a candidate pattern auto-promotes to approved. Human curation can also
// approve directly (force) — taste is not purely mechanical.
const PatternApprovalThreshold = 1

// ---- Service delegates -----------------------------------------------------

func (s *Service) HandlePatternPromote(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	return s.patterns.Promote(ctx, args)
}

// NOTE: HandlePatternRecordInstance lives in svc_patterns_a2.go — it wraps the
// taste-gate core (recordInstanceCore below) with the A2 engram-population pass.

// Promote changes a pattern's approval status. With no `to_status`, it promotes
// candidate→approved iff the green-instance threshold is met. With `to_status`
// it sets that status explicitly; promoting to `approved` below threshold
// requires `force` (the human-curation override). Any status can be set with
// force (e.g. deprecate an approved pattern).
func (ps *PatternSvc) Promote(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("pattern_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	to := v.String("to_status", "")
	force := v.Bool("force", false)

	p, err := ps.fetch(ctx, id)
	if err != nil {
		return mcp.ErrorResult(fmt.Errorf("load pattern: %w", err)), nil
	}
	if p == nil {
		return mcp.ErrorResult(fmt.Errorf("pattern %q not found", id)), nil
	}
	green := instancesShippedGreen(p)

	if to == "" {
		// Auto-promotion: candidate → approved on threshold.
		if p.Status != PatternStatusCandidate {
			return mcp.JSONResult(map[string]any{"ok": true, "pattern_id": p.ID, "status": p.Status, "note": "not a candidate; nothing to promote"})
		}
		if green < PatternApprovalThreshold && !force {
			return mcp.ErrorResult(fmt.Errorf("pattern %q has %d/%d green instances; not eligible for auto-promotion (pass to_status=approved + force to override)", id, green, PatternApprovalThreshold)), nil
		}
		p.Status = PatternStatusApproved
	} else {
		if !patternStatusValid(to) {
			return mcp.ErrorResult(fmt.Errorf("invalid to_status %q", to)), nil
		}
		if to == PatternStatusApproved && p.Status == PatternStatusCandidate && green < PatternApprovalThreshold && !force {
			return mcp.ErrorResult(fmt.Errorf("pattern %q has %d/%d green instances; pass force=true to approve below threshold (human curation)", id, green, PatternApprovalThreshold)), nil
		}
		p.Status = to
	}

	p.UpdatedAt = time.Now().UTC()
	if err := ps.persist(ctx, p); err != nil {
		return mcp.ErrorResult(fmt.Errorf("persist pattern: %w", err)), nil
	}
	ps.mu.Lock()
	ps.patterns[p.ID] = p
	ps.mu.Unlock()
	return mcp.JSONResult(map[string]any{"ok": true, "pattern_id": p.ID, "status": p.Status, "instances_shipped_green": green})
}

// recordInstanceOutcome is the typed result of recording a green instance,
// shared by the taste-gate MCP tool (RecordInstance) and the A2 green-stamp
// hook (HandlePatternRecordInstance). Pattern is the persisted, updated record.
type recordInstanceOutcome struct {
	Pattern  *Pattern
	Promoted bool
}

// recordInstanceCore is the authoritative taste-gate mutation: it increments a
// pattern's green-instance count, records the merged MR in provenance notes, and
// auto-promotes a candidate to approved once the threshold is reached. It
// returns the persisted pattern so callers can layer additional work (A2 engram
// population) on top of the same record without a second fetch.
func (ps *PatternSvc) recordInstanceCore(ctx context.Context, id, mrRef string) (recordInstanceOutcome, error) {
	p, err := ps.fetch(ctx, id)
	if err != nil {
		return recordInstanceOutcome{}, fmt.Errorf("load pattern: %w", err)
	}
	if p == nil {
		return recordInstanceOutcome{}, fmt.Errorf("pattern %q not found", id)
	}
	if p.Provenance == nil {
		p.Provenance = &PatternProvenance{}
	}
	p.Provenance.InstancesShippedGreen++
	if mrRef != "" {
		note := "green: " + mrRef
		if p.Provenance.Notes == "" {
			p.Provenance.Notes = note
		} else {
			p.Provenance.Notes = strings.TrimSpace(p.Provenance.Notes) + "; " + note
		}
	}

	promoted := false
	if p.Status == PatternStatusCandidate && p.Provenance.InstancesShippedGreen >= PatternApprovalThreshold {
		p.Status = PatternStatusApproved
		promoted = true
	}

	p.UpdatedAt = time.Now().UTC()
	if err := ps.persist(ctx, p); err != nil {
		return recordInstanceOutcome{}, fmt.Errorf("persist pattern: %w", err)
	}
	ps.mu.Lock()
	ps.patterns[p.ID] = p
	ps.mu.Unlock()
	return recordInstanceOutcome{Pattern: p, Promoted: promoted}, nil
}

// RecordInstance is the standalone taste-gate MCP tool: it records a green
// instance and returns the updated count/status. The A2 hook
// (HandlePatternRecordInstance) shares recordInstanceCore and additionally
// populates engrams; this entrypoint stays for direct taste-gate callers.
func (ps *PatternSvc) RecordInstance(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("pattern_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	mrRef := v.String("mr_ref", "")

	out, err := ps.recordInstanceCore(ctx, id, mrRef)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	return mcp.JSONResult(map[string]any{
		"ok":                      true,
		"pattern_id":              out.Pattern.ID,
		"instances_shipped_green": instancesShippedGreen(out.Pattern),
		"status":                  out.Pattern.Status,
		"promoted":                out.Promoted,
	})
}

// instancesShippedGreen returns a pattern's green-instance count (0 if unset).
func instancesShippedGreen(p *Pattern) int {
	if p.Provenance == nil {
		return 0
	}
	return p.Provenance.InstancesShippedGreen
}

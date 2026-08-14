// svc_patterns_a2.go -- Slice A2: populate engrams from green stamps.
//
// When a stamped instance merges green, the Pattern Loop does two things the
// engram engine never had a producer for:
//
//  1. VERIFY the pattern's composed engrams against the merged instance's
//     checkout, flipping each from `unverified` to `verified` and appending the
//     instance repo to `unlocked_in`. The factory becomes the thing that proves
//     its own building blocks.
//  2. SURFACE novel slices — those composing no engram — as engram *candidates*
//     (never auto-added; minting an engram is a taste decision that must earn a
//     proof).
//
// This is wired into the green-stamp hook (HandlePatternRecordInstance, which
// also drives the B2 taste gate). The engram work is strictly best-effort: a
// verify or persistence failure is surfaced in the response but must NEVER undo
// the green-instance increment / auto-promotion the taste gate performed.
package agentcontext

import (
	"context"
	"strings"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/validate"
)

// HandlePatternRecordInstance records a green-shipped instance (taste gate, B2)
// and then populates engrams from it (A2). It supersedes the plain taste-gate
// delegate: the green-count increment + promotion happen first and are
// authoritative; the engram verify/candidate pass is additive and best-effort.
//
// Args:
//
//	pattern_id (required) — the stamped pattern.
//	mr_ref               — the merged MR (recorded in provenance notes).
//	repo                 — instance repo id for `unlocked_in` (default: cwd base).
//	repo_root            — checkout dir file_ref proofs resolve against. In the
//	                       live Mills flow this is the merged instance's checkout;
//	                       when absent, file_ref proofs resolve against cwd and
//	                       will not verify (engrams stay unverified — safe).
func (s *Service) HandlePatternRecordInstance(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	id := v.Required("pattern_id")
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	mrRef := v.String("mr_ref", "")

	// Taste gate first — authoritative, must not be undone by the engram pass.
	out, err := s.patterns.recordInstanceCore(ctx, id, mrRef)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}

	// A2: best-effort engram population from this green instance.
	repo := v.String("repo", inferRepoName())
	repoRoot := v.String("repo_root", "")
	verified := s.verifyComposedEngrams(ctx, out.Pattern, repo, repoRoot)
	candidates := surfaceEngramCandidates(out.Pattern)

	return mcp.JSONResult(map[string]any{
		"ok":                      true,
		"pattern_id":              out.Pattern.ID,
		"instances_shipped_green": instancesShippedGreen(out.Pattern),
		"status":                  out.Pattern.Status,
		"promoted":                out.Promoted,
		"engrams_verified":        verified,
		"engram_candidates":       candidates,
	})
}

// verifyComposedEngrams verifies each engram a pattern composes against the
// merged instance checkout (repoRoot) and records `unlocked_in`. Reuses the
// exact engram verify machinery (verifyOne + applyVerifyResult) that
// HandleEngramVerify uses. Best-effort: an engram missing from the catalog is
// reported "skipped" and a persistence failure is folded into the result's
// reason — neither propagates.
func (s *Service) verifyComposedEngrams(ctx context.Context, p *Pattern, repo, repoRoot string) []VerifyResult {
	if p == nil || len(p.Engrams) == 0 {
		return nil
	}
	opts := VerifyOptions{
		RepoRoot:    repoRoot,
		SkipCommand: true, // command: proofs need the devbox sandbox (deferred)
	}
	out := make([]VerifyResult, 0, len(p.Engrams))
	for _, uri := range p.Engrams {
		item, err := s.lookupEngramByURI(uri)
		if err != nil || item == nil {
			out = append(out, VerifyResult{
				URI:         uri,
				ProofKind:   "unknown",
				Status:      "skipped",
				Reason:      "engram not present in catalog (seed it before stamping)",
				LastChecked: time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}
		res := verifyOne(ctx, item, repo, opts)
		if err := s.applyVerifyResult(ctx, item, res, repo); err != nil {
			res.Reason = strings.TrimSpace(res.Reason + "; persist failed: " + err.Error())
		}
		out = append(out, res)
	}
	return out
}

// EngramCandidate is a stamped slice that composes no engram yet — a gap in the
// catalog the Pattern Loop surfaces for authoring. Deliberately NOT auto-added:
// minting an engram is a taste decision (it must earn a proof), so A2 reports
// candidates and leaves the mint to a human/agent via agent_engram_add.
type EngramCandidate struct {
	SliceName string `json:"slice_name"`
	Reason    string `json:"reason"`
}

// surfaceEngramCandidates returns one candidate per pattern slice template that
// composes no engram. A fully-decomposed pattern (every slice cites an engram)
// yields none — that is the goal state, not a defect.
func surfaceEngramCandidates(p *Pattern) []EngramCandidate {
	if p == nil {
		return nil
	}
	var out []EngramCandidate
	for _, tpl := range p.SliceTemplate {
		if len(tpl.Engrams) == 0 {
			out = append(out, EngramCandidate{
				SliceName: tpl.Name,
				Reason:    "slice composes no engram; candidate for a new engram (author via agent_engram_add)",
			})
		}
	}
	return out
}

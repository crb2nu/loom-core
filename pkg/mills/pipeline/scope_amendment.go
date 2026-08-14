package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Scope-gate reliability (plan .loom/plan-mills-scope-gate-reliability-2026-07-26.md).
//
// The 24h KPI on 2026-07-26 read 83% escalation / 17% auto-merge against a
// 95.7% gate_pass_rate: runs were dying at ONE gate, and the handling — rewind
// to implement, full respawn, escalate unclassified after 3 attempts — could
// not converge, because the violating files were files the implementer NEEDED.
// This file is the runner-side half of the fix:
//
//	S1 amend-and-proceed  when the ONLY failing gate is `scope` and policy
//	                      admits every violation, CAS-append the files to the
//	                      item's slice scope and continue on the existing diff.
//	S2 escalation hygiene when it does not, escalate with a [class=config]
//	                      marker, a structured `scope_violations` artifact, and
//	                      a Draft rescue MR so the good diff is visible instead
//	                      of stranded on an un-MR'd branch.
//
// The admissibility rules themselves are a pure function in
// pkg/mills/gates/scope_amendment.go; everything here is effect.

const (
	// postImplementGateStage is the only auto_gate the scope amendment fires
	// on. Scope is evaluated exactly once per run (post_implement_gate is the
	// sole stage listing it), and pinning the stage keeps a future template
	// that reuses the gate name elsewhere from inheriting an amendment path it
	// was never reviewed for.
	postImplementGateStage = "post_implement_gate"

	// maxScopeRetryAttempts caps implement attempts for a scope-only gate
	// failure that the amendment refused. 2 == the original attempt plus ONE
	// self-correction respawn: a genuine detour deserves a chance to correct
	// itself, but the third attempt has never produced a different diff (the
	// 2026-07-26 cohort burned the full budget on byte-identical retries).
	maxScopeRetryAttempts = 2

	// scopeViolationsArtifact is the stage_results artifact key carrying the
	// structured amendment decision on a scope escalation. Read by the HUD/CLI
	// to offer widen-and-requeue without parsing the prose reason (which is
	// truncated at gateFailureDetailMaxLen and renders at most 8 paths).
	scopeViolationsArtifact = "scope_violations"

	// scopeRescueMRTitlePrefix marks the Draft MR opened over an escalated
	// run's branch. Draft so nothing can merge it, and deliberately NOT
	// auto-merge-armed: the whole point is that a human decides between
	// widening the item's scope and closing the work.
	scopeRescueMRTitlePrefix = "Draft: [scope-escalated] "
)

// maybeAmendScope evaluates a scope-only gate failure for auto-amendment and,
// when policy admits every violation, applies it. Returns (true, decision) when
// the pipeline should CONTINUE past the failed gate on the existing diff;
// (false, decision) when the caller must fall through to the normal rewind /
// escalate path. The decision is nil only when the amendment never got far
// enough to form one (no store, no violations recomputed).
//
// Never returns an error: an amendment that cannot be applied is a
// fall-through, not a run failure. The gate already failed; the worst case is
// the behaviour that shipped before this slice.
func (r *Runner) maybeAmendScope(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	policy *mills.Policy,
	verdict gateVerdict,
) (bool, *gates.AmendmentDecision) {
	if item == nil || r.Store == nil || r.Store.Backlog == nil {
		return false, nil
	}
	// Recompute the FULL violation list from the same StageInput the gate
	// judged. The persisted reason renders at most 8 paths and is then
	// truncated, so parsing it back would silently under-report the reach.
	violations := gates.ScopeViolations(verdict.Input)
	if len(violations) == 0 {
		return false, nil
	}
	pol := policy.Pipeline.ScopeAmendment
	if !policy.Pipeline.ScopeAmendmentEnabled() {
		mills.ScopeAmendmentsTotal.WithLabelValues("disabled").Inc()
		d := gates.EvaluateScopeAmendment(item, violations, pol, policy.Pipeline.ProtectedPaths)
		return false, &d
	}

	decision, applied := r.applyScopeAmendment(ctx, item, violations, pol, policy.Pipeline.ProtectedPaths)
	if !applied {
		if decision.Admitted {
			// Admissible but the backlog CAS lost twice — a competing writer
			// is mutating the item right now. Falling through to the retry
			// path is safe; the next evaluation re-tries the amendment.
			mills.ScopeAmendmentsTotal.WithLabelValues("conflict").Inc()
		} else {
			mills.ScopeAmendmentsTotal.WithLabelValues("refused").Inc()
		}
		return false, &decision
	}

	mills.ScopeAmendmentsTotal.WithLabelValues("admitted").Inc()
	summary := decision.Summary()
	// Record a PASS row for the gate alongside the fail runGate already
	// persisted. Both rows are true: the gate failed on the authored envelope
	// and passed on the amended one, and gate_pass_rate should see the
	// resolution rather than only the miss.
	row := &store.GateOutcome{
		PipelineRunID: run.ID,
		AfterStage:    stage.ID,
		GateName:      scopeGateName,
		Outcome:       store.GateOutcomePass,
		Reasons:       []string{summary},
		JudgedBy:      "go",
		EvaluatedAt:   r.now(),
	}
	mills.GateEvaluationsTotal.WithLabelValues(scopeGateName, string(store.GateOutcomePass)).Inc()
	if perr := r.Store.Pipeline.PutGate(ctx, row); perr != nil {
		r.logger().Warn("pipeline gate persist failed", "error", perr)
	}
	r.logger().Info("pipeline gate: scope amended; continuing without respawn",
		"run", run.ID, "gate", stage.ID, "files", decision.AdmittedFiles(),
		"ancestor_depth", decision.AncestorDepth)
	r.event(ctx, "pipeline.gate.scope_amended", "ok", map[string]any{
		"run": run.ID, "gate": stage.ID, "backlog": item.ID,
		"files": decision.AdmittedFiles(), "summary": summary,
		"ancestor_depth": decision.AncestorDepth, "max_files": decision.MaxFiles,
	})
	return true, &decision
}

// applyScopeAmendment evaluates and, when admitted, CAS-writes the widened
// slice scope onto the backlog item. Returns the decision it acted on and
// whether the write landed.
//
// The CAS budget is one re-read plus one retry: BacklogDAO.Put compare-and-swaps
// on Revision, and the loser must re-evaluate against the FRESH slices (the
// winning writer may itself have widened scope, changing which slice each file
// belongs to) rather than replay a decision computed against a stale row.
func (r *Runner) applyScopeAmendment(
	ctx context.Context,
	item *store.BacklogItem,
	violations []string,
	pol mills.ScopeAmendmentPolicy,
	protectedPaths []string,
) (gates.AmendmentDecision, bool) {
	var decision gates.AmendmentDecision
	for attempt := 0; attempt < 2; attempt++ {
		decision = gates.EvaluateScopeAmendment(item, violations, pol, protectedPaths)
		if !decision.Admitted {
			return decision, false
		}
		before := item.Slices
		item.Slices = gates.ApplyAmendment(before, decision)
		err := r.Store.Backlog.Put(ctx, item)
		if err == nil {
			return decision, true
		}
		item.Slices = before
		if !errors.Is(err, store.ErrStaleWrite) {
			// A real store failure. Log and fall through to the normal retry
			// path — an amendment is never worth failing a run over.
			r.logger().Warn("pipeline: scope amendment write failed",
				"backlog", item.ID, "error", err)
			return decision, false
		}
		fresh, gerr := r.Store.Backlog.Get(ctx, item.ID)
		if gerr != nil {
			r.logger().Warn("pipeline: scope amendment re-read failed",
				"backlog", item.ID, "error", gerr)
			return decision, false
		}
		*item = *fresh
	}
	r.logger().Warn("pipeline: scope amendment lost the backlog CAS twice; falling through to retry",
		"backlog", item.ID)
	return decision, false
}

// scopeEscalationReason builds the S2 escalation reason for a scope-cap
// escalation and, as side effects, persists the structured evidence and opens
// the Draft rescue MR. Both side effects are best-effort: a failure is logged
// and the escalation proceeds with whatever text it has.
func (r *Runner) scopeEscalationReason(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	failDetail string,
	attemptCap int,
	decision *gates.AmendmentDecision,
) string {
	r.persistScopeViolations(ctx, run, stage, decision)
	reason := fmt.Sprintf(
		"gate %s failed (%s) [class=%s]; %s exceeded %d attempts — the diff reaches files outside the item's declared slice scope that scope-amendment policy does not admit; widen Slices[].files (or close the item), then requeue",
		stage.ID, failDetail, ClassConfig, stage.RetryFrom, attemptCap)
	if decision != nil && decision.Refusal != "" {
		reason += "; " + decision.Refusal
	}
	if iid := r.openScopeRescueMR(ctx, run, item, stage, decision); iid != 0 {
		reason += fmt.Sprintf("; rescue draft MR !%d holds the diff", iid)
	}
	return reason
}

// persistScopeViolations writes the amendment decision onto the gate stage's
// stage_results row under the scope_violations artifact key, so the HUD/CLI can
// render per-file rule verdicts instead of re-deriving them from prose. The row
// is recorded with a gate_fail outcome so loadPriorOutputs (which only
// rehydrates successful stages) can never mistake it for stage output.
func (r *Runner) persistScopeViolations(
	ctx context.Context,
	run *store.PipelineRun,
	stage Stage,
	decision *gates.AmendmentDecision,
) {
	if decision == nil || r.Store == nil || r.Store.Pipeline == nil {
		return
	}
	// Round-trip through JSON so the artifact map holds plain scalars/maps,
	// matching every other artifacts_json payload (the column is JSON, and a
	// Go struct would encode field names the readers do not expect).
	raw, err := json.Marshal(decision)
	if err != nil {
		r.logger().Warn("pipeline: encode scope violations failed", "run", run.ID, "error", err)
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		r.logger().Warn("pipeline: decode scope violations failed", "run", run.ID, "error", err)
		return
	}
	now := r.now()
	outcome := store.StageOutcomeGateFail
	if err := r.Store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         stage.ID,
		Attempt:       1,
		StartedAt:     now,
		EndedAt:       &now,
		Outcome:       &outcome,
		Artifacts: map[string]any{
			"stage_id":              stage.ID,
			scopeViolationsArtifact: payload,
		},
	}); err != nil {
		r.logger().Warn("pipeline: persist scope violations failed", "run", run.ID, "error", err)
	}
}

// openScopeRescueMR opens a Draft MR over the run's branch so a scope
// escalation leaves the (good) diff visible instead of stranded. Returns the MR
// iid, or 0 when no MR was opened.
//
// Best-effort by contract: the implement stage pushes the branch, but a run can
// escalate before that lands, the project may be unreachable, or the operator
// may have no GitLab client wired at all. None of those may mask the
// escalation, so every failure path is a warn + 0 (mirrors the Cleanup stage's
// treatment of its own side effects). Never armed for auto-merge: the human's
// choice between "widen scope" and "close it" is the whole point.
func (r *Runner) openScopeRescueMR(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	decision *gates.AmendmentDecision,
) int64 {
	if r.RescueMR == nil || item == nil {
		return 0
	}
	branch := BranchContractFor(run, item, stage, "").SourceBranch
	if branch == "" {
		return 0
	}
	resp, err := r.RescueMR(ctx, run, item, CreateMRRequest{
		BacklogID:    item.ID,
		SourceBranch: branch,
		TargetBranch: "main",
		Title:        ClampMRTitle(scopeRescueMRTitlePrefix + item.Title),
		Description:  scopeRescueMRBody(run, item, decision),
		AutoMerge:    false,
	})
	if err != nil {
		r.logger().Warn("pipeline: scope rescue MR not opened", "run", run.ID, "branch", branch, "error", err)
		return 0
	}
	r.logger().Info("pipeline: scope rescue draft MR opened",
		"run", run.ID, "mr_iid", resp.MRIID, "branch", branch, "adopted", resp.Adopted)
	r.event(ctx, "pipeline.gate.scope_rescue_mr", "warn", map[string]any{
		"run": run.ID, "backlog": item.ID, "mr_iid": resp.MRIID,
		"branch": branch, "adopted": resp.Adopted,
	})
	return resp.MRIID
}

// scopeRescueMRBody renders the Draft MR description: what the run produced,
// which files fell outside the declared envelope and why the amendment refused
// each one, and the two actions a human can take.
func scopeRescueMRBody(run *store.PipelineRun, item *store.BacklogItem, decision *gates.AmendmentDecision) string {
	var b strings.Builder
	b.WriteString("**Mills run escalated at the `scope` gate.** The diff on this branch is complete but reaches files outside the backlog item's declared slice scope, and the reach was not auto-admissible.\n\n")
	fmt.Fprintf(&b, "- Backlog item: `%s`\n", item.ID)
	if run != nil {
		fmt.Fprintf(&b, "- Pipeline run: `%s`\n", run.ID)
	}
	if decision != nil {
		fmt.Fprintf(&b, "- Amendment policy: ancestor_depth=%d, max_files=%d\n", decision.AncestorDepth, decision.MaxFiles)
		if decision.Refusal != "" {
			fmt.Fprintf(&b, "- Refusal: %s\n", decision.Refusal)
		}
		if len(decision.DeclaredDirs) > 0 {
			fmt.Fprintf(&b, "- Declared directories: `%s`\n", strings.Join(decision.DeclaredDirs, "`, `"))
		}
		b.WriteString("\n| File | Admissible | Rule | Shared ancestor |\n|---|---|---|---|\n")
		for _, v := range decision.Verdicts {
			fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s |\n",
				v.File, boolStr(v.Admitted, "yes", "no"), v.Rule, codeOrDash(v.Ancestor))
		}
	}
	b.WriteString("\n**Next step — pick one:**\n\n")
	b.WriteString("1. Widen the item's slice scope to include the files above, then requeue the item. The work on this branch is reusable.\n")
	b.WriteString("2. If the reach is a genuine detour, close this MR and the backlog item.\n\n")
	b.WriteString("_Draft on purpose: auto-merge is not armed for a rescue MR._\n")
	return b.String()
}

func codeOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return "`" + s + "`"
}

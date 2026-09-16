package overseer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Shepherd action vocabulary. Committed events use
// "overseer.shepherd.<action>"; dry-run decisions append ".dryrun".
const (
	shepherdActor = "overseer.shepherd"

	// actionRelaunch is the guarded escalated→queued transition for items the
	// auto-requeue sweep structurally cannot reach.
	actionRelaunch = "relaunch"
	// actionOrphanFlag is the event-only attention flag for items whose MR
	// was closed (not merged) and which have sat unattended since.
	actionOrphanFlag = "orphan_flagged"
	// actionScopeWiden is the C1 committed action: every recorded scope
	// violation is admissible under CURRENT policy, so the item's slices are
	// CAS-widened and the item requeued (branch adoption + immune-S3
	// un-draft finish the rescue).
	actionScopeWiden = "scope_widen"
	// actionScopeFlag is the event-only attention flag for scope escalations
	// that remain inadmissible: the widen-vs-close decision, surfaced with
	// the per-file verdict table instead of rotting in a draft MR.
	actionScopeFlag = "scope_decision_flagged"

	shepherdTickKind    = "overseer.shepherd.tick"
	shepherdSubjectKind = "backlog_item"
)

const (
	// shepherdCandidateBatch bounds the escalated-shelf scan per tick.
	shepherdCandidateBatch = 128
	// shepherdCandidateWindow bounds how far back the shelf scan reaches.
	// Older escalations are a curation problem, not a relaunch problem.
	shepherdCandidateWindow = 30 * 24 * time.Hour
	// activeClassWindow is the recency window inside which OTHER items
	// escalating with the same fingerprint mean the class is still burning:
	// relaunching into it is the exact burn the 08-18 S3 relaunch produced.
	activeClassWindow = 6 * time.Hour
	// ghostSparkMRClosedKind is the reconciler's first-writer marker for an
	// escalated item whose MR was observed closed-without-merging. Literal
	// (not imported) because pkg/mills imports this package's sibling
	// consumers; a drift only weakens orphan detection, never corrupts.
	ghostSparkMRClosedKind = "reconciler.ghost_spark_mr_closed"
)

// Shepherd is the escalated-shelf overseer (shepherd program B1): it owns the
// items every other automatic path structurally excludes. The auto-requeue
// sweep refuses items whose latest run carries an MR, cross-repo items, and
// code-class escalations; the ghost sweeps only reconcile against merged
// GitLab reality. Everything else lands on a shelf that — before this agent —
// only a human ever drained.
//
// v1 posture: the relaunch predicate is deliberately weak (aged + retryable +
// auto-requeue-unreachable + never shepherd-relaunched), so the agent soaks
// in dry-run producing proposal evidence for the promotion report. The
// environment-delta upgrade (relaunch only when a fix plausibly landed —
// mined failure fingerprints) replaces the predicate in a later slice; the
// harness, caps, audit trail, and orphan attention are the durable skeleton.
type Shepherd struct {
	Store *store.Store
	// Policy returns the live policy snapshot (hot-reload honored per tick).
	Policy   func() *mills.Policy
	Recorder *ActionRecorder
	Logger   *slog.Logger
	// HomeProject distinguishes cross-repo items (auto-requeue excludes them;
	// the shepherd may relaunch them).
	HomeProject string
	// Now is used by tests; defaults to time.Now UTC.
	Now func() time.Time
}

// Name implements Agent.
func (s *Shepherd) Name() string { return "shepherd" }

// Tick implements Agent: one bounded pass over the escalated shelf.
func (s *Shepherd) Tick(ctx context.Context) (TickResult, error) {
	res := TickResult{}
	if s == nil || s.Store == nil || s.Store.Backlog == nil || s.Store.Pipeline == nil ||
		s.Store.Events == nil || s.Recorder == nil {
		return res, errors.New("shepherd: not configured")
	}
	pol := s.policy()
	if pol == nil || !pol.ShepherdEnabled() {
		return res, nil
	}
	sp := pol.Overseers.Shepherd
	now := s.now()
	dryRun := mills.DryRunOn(sp.DryRun)

	// Relaunch candidates are the RETRYABLE escalations; the scope pass below
	// scans the config-class remainder, so an empty candidate list must not
	// end the tick.
	candidates, err := s.Store.Backlog.ListByEndedSince(ctx, now.Add(-shepherdCandidateWindow), shepherdCandidateBatch)
	if err != nil {
		return res, fmt.Errorf("shepherd: list candidates: %w", err)
	}
	dayUsed, err := s.Recorder.DayUsed(ctx, now, actionRelaunch, actionScopeWiden)
	if err != nil {
		return res, fmt.Errorf("shepherd: day-cap read: %w", err)
	}
	budget := &tickBudget{tickCap: sp.TickCap(), dayUsed: dayUsed, dayCap: sp.DayCap()}

	for _, cand := range candidates {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if cand == nil {
			continue
		}
		res.Inspected++
		item, ierr := s.Store.Backlog.Get(ctx, cand.ID)
		if ierr != nil || item == nil || item.State != store.BacklogEscalated {
			continue
		}
		runs, rerr := s.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if rerr != nil {
			res.Errored++
			continue
		}
		latest := newestRun(runs)
		if latest == nil {
			continue
		}
		age := now.Sub(item.UpdatedAt)
		if latest.EndedAt != nil {
			age = now.Sub(*latest.EndedAt)
		}
		if age < sp.MinShelfAge() {
			// The young shelf belongs to the auto-requeue sweep and to fresh
			// triage; the shepherd only drains what has demonstrably stuck.
			res.Skipped++
			continue
		}

		s.flagClosedMROrphan(ctx, &res, sp, item, runs, now)

		if autoRequeueReachable(item, latest, s.HomeProject) {
			// The bounded auto-requeue sweep owns this shape; a second
			// relauncher would double-spend its caps.
			res.Skipped++
			continue
		}
		escalatedAt := item.UpdatedAt
		if latest.EndedAt != nil {
			escalatedAt = *latest.EndedAt
		}
		s.proposeRelaunch(ctx, &res, item, latest, escalatedAt, now, budget, dryRun, sp)
	}

	s.reviewScopeEscalations(ctx, &res, sp, pol, budget, dryRun, now)

	if res.Inspected == 0 {
		return res, nil
	}
	if err := s.Store.Events.Append(ctx, &store.Event{
		Actor: shepherdActor, Kind: shepherdTickKind,
		Payload: map[string]any{
			"inspected": res.Inspected, "acted": res.Acted, "planned": res.Planned,
			"skipped": res.Skipped, "errored": res.Errored,
			"dry_run": dryRun, "day_used": budget.dayUsed, "day_cap": budget.dayCap,
		},
	}); err != nil && s.Logger != nil {
		s.Logger.Warn("shepherd: tick event append failed", "error", err)
	}
	return res, nil
}

// autoRequeueReachable reports whether SweepAutoRequeue could act on this item
// itself: home-project, latest run without an MR, and a retryable
// infrastructure-shaped class. The shepherd stays out of that lane.
func autoRequeueReachable(item *store.BacklogItem, latest *store.PipelineRun, home string) bool {
	if latest.MRIID != nil && *latest.MRIID > 0 {
		return false
	}
	if item.TargetProject != "" && home != "" && !store.SameRepo(item.TargetProject, home) {
		return false
	}
	switch latest.FailureClass {
	case "infrastructure", "transient", "transient_quota":
		return true
	}
	switch latest.EscalationClass {
	case "infra", "transient", "transient_quota":
		return true
	}
	return false
}

// flagClosedMROrphan emits a once-only attention flag for an item whose MR the
// ghost sweep recorded closed-without-merging and which has sat unattended
// past the orphan age. Event-only in every mode: the widen-vs-close decision
// stays human until the scope-rescue slice lands.
func (s *Shepherd) flagClosedMROrphan(
	ctx context.Context, res *TickResult, sp mills.ShepherdPolicy,
	item *store.BacklogItem, runs []*store.PipelineRun, now time.Time,
) {
	run := newestRunWithMR(runs)
	if run == nil {
		return
	}
	closed, err := s.Store.Events.FirstBySubjectKind(ctx, "pipeline_run", run.ID, ghostSparkMRClosedKind)
	if err != nil || closed == nil {
		return
	}
	if now.Sub(closed.OccurredAt) < sp.OrphanAge() {
		return
	}
	payload := map[string]any{
		"backlog_id": item.ID, "run_id": run.ID, "mr_iid": derefIID(run.MRIID),
		"mr_closed_at": closed.OccurredAt.UTC().Format(time.RFC3339),
		"reason":       "mr closed without merging; item still escalated",
	}
	ok, err := s.Recorder.RecordOnce(ctx, actionOrphanFlag, shepherdSubjectKind, item.ID, payload)
	if err != nil {
		res.Errored++
		return
	}
	if ok {
		res.Planned++
	}
}

// proposeRelaunch records (dry-run / not-allowed) or commits (allowed) one
// guarded escalated→queued transition. One shepherd relaunch per item, ever:
// a relaunch that re-escalates needs a stronger reason than this predicate
// can supply — that is the environment-delta slice's job.
func (s *Shepherd) proposeRelaunch(
	ctx context.Context, res *TickResult, item *store.BacklogItem,
	latest *store.PipelineRun, escalatedAt, now time.Time,
	budget *tickBudget, dryRun bool, sp mills.ShepherdPolicy,
) {
	prior, err := s.Recorder.SubjectCount(ctx, actionRelaunch, shepherdSubjectKind, item.ID)
	if err != nil {
		res.Errored++
		return
	}
	if prior > 0 {
		res.Skipped++
		return
	}
	// Environment-delta predicate (B2): a relaunch needs a reason to believe
	// the world changed. A stamped fingerprint supplies it from the cohort —
	// a sibling item with the same failure shape MERGED after this run
	// escalated (the class demonstrably cleared), and no OTHER item is
	// currently escalating with it (the class is not still burning). An
	// unstamped run (pre-B2) keeps the v1 aged-and-unreachable reason so the
	// legacy shelf stays drainable during the transition.
	reason := "aged retryable escalation unreachable by auto-requeue (no fingerprint)"
	clearedBy := ""
	if fp := latest.FailureSignature; fp != "" {
		cohort, cerr := s.Store.Backlog.FailureSignatureCohort(ctx, fp, item.ID, escalatedAt, now.Add(-activeClassWindow))
		if cerr != nil {
			res.Errored++
			return
		}
		if cohort.Active > 0 {
			// Same class escalated again recently on another item: still
			// broken; a relaunch would join the burn. Not even a proposal.
			res.Skipped++
			return
		}
		if cohort.ClearedBy == "" {
			res.Skipped++
			return
		}
		clearedBy = cohort.ClearedBy
		reason = "failure-shape cohort cleared: sibling merged after this escalation"
	}
	payload := map[string]any{
		"backlog_id": item.ID, "run_id": latest.ID,
		"prior_class":       latest.EscalationClass,
		"mr_iid":            derefIID(latest.MRIID),
		"failure_signature": latest.FailureSignature,
		"cleared_by":        clearedBy,
		"reason":            reason,
	}
	if dryRun || !sp.Allow.Relaunch {
		if ok, rerr := s.Recorder.RecordOnce(ctx, actionRelaunch, shepherdSubjectKind, item.ID, payload); rerr != nil {
			res.Errored++
		} else if ok {
			res.Planned++
		}
		return
	}
	if !budget.can() {
		res.Skipped++
		return
	}
	payload["day_used"] = budget.dayUsed + 1
	payload["day_cap"] = budget.dayCap
	// The committed relaunch bypasses Recorder.Record: the event rides the
	// guarded transition atomically, and DayUsed sees it by actor+kind.
	if _, terr := s.Store.Backlog.TransitionStateWithEvent(
		ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogQueued,
		&store.Event{
			Actor: shepherdActor, Kind: shepherdActor + "." + actionRelaunch,
			SubjectKind: shepherdSubjectKind, SubjectID: item.ID,
			Payload: payload,
		},
	); terr != nil {
		if errors.Is(terr, store.ErrStaleWrite) {
			res.Skipped++ // a concurrent writer moved the item; clean skip
			return
		}
		res.Errored++
		if s.Logger != nil {
			s.Logger.Warn("shepherd: relaunch transition failed", "backlog", item.ID, "error", terr)
		}
		return
	}
	budget.commit()
	res.Acted++
	if s.Logger != nil {
		s.Logger.Info("shepherd: relaunched escalated item",
			"backlog", item.ID, "run", latest.ID,
			"day_used", budget.dayUsed, "day_cap", budget.dayCap)
	}
}

func newestRun(runs []*store.PipelineRun) *store.PipelineRun {
	var latest *store.PipelineRun
	for _, run := range runs {
		if run == nil {
			continue
		}
		if latest == nil || run.StartedAt.After(latest.StartedAt) ||
			(run.StartedAt.Equal(latest.StartedAt) && run.Attempts > latest.Attempts) {
			latest = run
		}
	}
	return latest
}

func newestRunWithMR(runs []*store.PipelineRun) *store.PipelineRun {
	var latest *store.PipelineRun
	for _, run := range runs {
		if run == nil || run.MRIID == nil || *run.MRIID == 0 {
			continue
		}
		if latest == nil || run.StartedAt.After(latest.StartedAt) ||
			(run.StartedAt.Equal(latest.StartedAt) && run.Attempts > latest.Attempts) {
			latest = run
		}
	}
	return latest
}

func derefIID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Shepherd) policy() *mills.Policy {
	if s.Policy == nil {
		return nil
	}
	return s.Policy()
}

func (s *Shepherd) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// reviewScopeEscalations is the C1 pass: scope escalations are config-class
// (never relaunch candidates) and their draft rescue MRs ask a human to pick
// "widen scope + requeue" or "close" — a decision nothing owned. The pass
// re-evaluates each item's persisted scope_violations against CURRENT policy
// and the item's CURRENT slices: policy loosened, protected paths narrowed, or
// a manual partial widen since the escalation all flip the verdict. Admissible
// → widen + requeue (allow.scope_widen, dry-run honored); still inadmissible →
// a once-only attention flag carrying the per-file verdict table.
func (s *Shepherd) reviewScopeEscalations(
	ctx context.Context, res *TickResult, sp mills.ShepherdPolicy,
	pol *mills.Policy, budget *tickBudget, dryRun bool, now time.Time,
) {
	items, err := s.Store.Backlog.ListByStateLimit(ctx, store.BacklogEscalated, shepherdCandidateBatch)
	if err != nil {
		res.Errored++
		return
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return
		}
		if item == nil {
			continue
		}
		runs, rerr := s.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if rerr != nil {
			res.Errored++
			continue
		}
		latest := newestRun(runs)
		// Cheap pre-filter: only config-class escalations persist a scope
		// verdict table; everything else was pass A's business.
		if latest == nil || latest.EscalationClass != "config" {
			continue
		}
		escalatedAt := item.UpdatedAt
		if latest.EndedAt != nil {
			escalatedAt = *latest.EndedAt
		}
		if now.Sub(escalatedAt) < sp.MinShelfAge() {
			continue
		}
		files := s.scopeViolationFiles(ctx, latest.ID)
		if len(files) == 0 {
			continue
		}
		res.Inspected++
		decision := gates.EvaluateScopeAmendment(item, files, pol.Pipeline.ScopeAmendment, pol.ProtectedPathsFor(item.TargetProject))
		if !decision.Admitted {
			payload := map[string]any{
				"backlog_id": item.ID, "run_id": latest.ID,
				"refusal": decision.Refusal, "verdicts": verdictTable(decision),
				"reason": "scope escalation still inadmissible under current policy; widen-vs-close needs a human",
			}
			if ok, ferr := s.Recorder.RecordOnce(ctx, actionScopeFlag, shepherdSubjectKind, item.ID, payload); ferr != nil {
				res.Errored++
			} else if ok {
				res.Planned++
			}
			continue
		}
		s.widenAndRequeue(ctx, res, item, latest, decision, files, budget, dryRun, sp)
	}
}

// scopeViolationFiles loads the persisted scope_violations artifact from the
// run's stage results and returns the recorded violating files (newest row
// wins). Empty when the run never persisted a verdict table.
func (s *Shepherd) scopeViolationFiles(ctx context.Context, runID string) []string {
	stages, err := s.Store.Pipeline.ListStages(ctx, runID)
	if err != nil {
		return nil
	}
	for i := len(stages) - 1; i >= 0; i-- {
		st := stages[i]
		if st == nil || st.Artifacts == nil {
			continue
		}
		raw, ok := st.Artifacts["scope_violations"]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var decision gates.AmendmentDecision
		if err := json.Unmarshal(encoded, &decision); err != nil {
			continue
		}
		files := make([]string, 0, len(decision.Verdicts))
		for _, v := range decision.Verdicts {
			if v.File != "" {
				files = append(files, v.File)
			}
		}
		return files
	}
	return nil
}

// widenAndRequeue commits (or proposes) the admissible half of C1: the CAS
// widen mirrors the runner's applyScopeAmendment budget (one re-read, one
// retry, fresh evaluation each attempt), then the guarded escalated→queued
// transition carries the audit event. One shepherd scope-widen per item ever.
func (s *Shepherd) widenAndRequeue(
	ctx context.Context, res *TickResult, item *store.BacklogItem,
	latest *store.PipelineRun, decision gates.AmendmentDecision, files []string,
	budget *tickBudget, dryRun bool, sp mills.ShepherdPolicy,
) {
	prior, err := s.Recorder.SubjectCount(ctx, actionScopeWiden, shepherdSubjectKind, item.ID)
	if err != nil {
		res.Errored++
		return
	}
	if prior > 0 {
		res.Skipped++
		return
	}
	payload := map[string]any{
		"backlog_id": item.ID, "run_id": latest.ID,
		"files": files, "verdicts": verdictTable(decision),
		"reason": "all recorded scope violations admissible under current policy",
	}
	if dryRun || !sp.Allow.ScopeWiden {
		if ok, rerr := s.Recorder.RecordOnce(ctx, actionScopeWiden, shepherdSubjectKind, item.ID, payload); rerr != nil {
			res.Errored++
		} else if ok {
			res.Planned++
		}
		return
	}
	if !budget.can() {
		res.Skipped++
		return
	}
	pol := s.policy()
	widened := false
	for attempt := 0; attempt < 2; attempt++ {
		d := gates.EvaluateScopeAmendment(item, files, pol.Pipeline.ScopeAmendment, pol.ProtectedPathsFor(item.TargetProject))
		if !d.Admitted {
			res.Skipped++ // a concurrent writer changed the ground; not ours anymore
			return
		}
		before := item.Slices
		item.Slices = gates.ApplyAmendment(before, d)
		if err := s.Store.Backlog.Put(ctx, item); err == nil {
			widened = true
			break
		} else {
			item.Slices = before
			if !errors.Is(err, store.ErrStaleWrite) {
				res.Errored++
				return
			}
			fresh, gerr := s.Store.Backlog.Get(ctx, item.ID)
			if gerr != nil {
				res.Errored++
				return
			}
			*item = *fresh
		}
	}
	if !widened {
		res.Skipped++
		return
	}
	payload["day_used"] = budget.dayUsed + 1
	payload["day_cap"] = budget.dayCap
	if _, terr := s.Store.Backlog.TransitionStateWithEvent(
		ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogQueued,
		&store.Event{
			Actor: shepherdActor, Kind: shepherdActor + "." + actionScopeWiden,
			SubjectKind: shepherdSubjectKind, SubjectID: item.ID,
			Payload: payload,
		},
	); terr != nil {
		if errors.Is(terr, store.ErrStaleWrite) {
			res.Skipped++ // widen is durable and harmless; the race winner owns the item
			return
		}
		res.Errored++
		return
	}
	budget.commit()
	res.Acted++
	if s.Logger != nil {
		s.Logger.Info("shepherd: widened scope and requeued",
			"backlog", item.ID, "run", latest.ID, "files", len(files),
			"day_used", budget.dayUsed, "day_cap", budget.dayCap)
	}
}

// verdictTable projects an AmendmentDecision into the compact per-file table
// attention events carry.
func verdictTable(d gates.AmendmentDecision) []map[string]any {
	out := make([]map[string]any, 0, len(d.Verdicts))
	for _, v := range d.Verdicts {
		out = append(out, map[string]any{
			"file": v.File, "admitted": v.Admitted, "rule": v.Rule, "ancestor": v.Ancestor,
		})
	}
	return out
}

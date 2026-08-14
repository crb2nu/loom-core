package mills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Event kind + subject used by the auto-requeue sweep. The per-item cap is read
// back from these durable events (CountBySubjectKind), so the cap survives an
// operator restart without a dedicated counter column; the per-day cap reads the
// same kind fleet-wide (CountByKindSince).
const (
	eventKindAutoRequeued  = "reconciler.auto_requeued"
	autoRequeueSubjectKind = "backlog_item"
)

// Eligible-class labels for the metric + event payload. "external_dependency"
// is not a base ErrorClass — it is the incident category that takes precedence
// when a run carries a dependency marker (external incidents persist as
// config-class with the marker set), so it gets its own label here.
const (
	autoRequeueClassInfra              = "infra"
	autoRequeueClassTransient          = "transient"
	autoRequeueClassTransientQuota     = "transient_quota"
	autoRequeueClassExternalDependency = "external_dependency"
	autoRequeueClassCode               = "code"
	autoRequeueClassConfig             = "config"
)

// ItemJournalEnvName mirrors pipeline.ItemJournalEnv (pkg/mills cannot import
// pkg/mills/pipeline — pipeline imports mills). A parity test in the pipeline
// package pins the two strings together. Code/config auto-requeue eligibility
// reads it because those retries are only meaningful when the item journal is
// feeding prior-attempt context into the retry's stage prompts.
const ItemJournalEnvName = "LOOM_MILLS_ITEM_JOURNAL"

// itemJournalActive reports whether the per-item memory journal is on for this
// operator process. Same read the pipeline's record/render hooks perform.
func itemJournalActive() bool {
	return strings.TrimSpace(os.Getenv(ItemJournalEnvName)) != ""
}

const (
	// autoRequeueCandidateBatchSize bounds the escalated-item scan per sweep.
	autoRequeueCandidateBatchSize = 64
	// autoRequeueMaxPerTick caps how many requeues one tick commits so a burst
	// of newly-eligible items drains over several ticks (mirrors the ghost
	// sweep's per-tick lookup cap). The per-day cap is the real fleet bound.
	autoRequeueMaxPerTick = 6
	// autoRequeueRecheckCooldown defers re-inspecting an item the sweep found
	// ineligible so a large escalated pile can't pin every tick to the same
	// scan. Process-local; a restart re-inspects each once.
	autoRequeueRecheckCooldown = 15 * time.Minute
	// autoRequeueExternalIncidentThreshold / Window mirror pkg/mills/pipeline's
	// externalIncidentDegradedModeThreshold / Window: an item escalated for an
	// external dependency is NOT eligible while that dependency has >= threshold
	// terminal escalations in the window (an ACTIVE degraded-mode incident). The
	// literals are duplicated because pkg/mills cannot import pkg/mills/pipeline
	// (pipeline imports mills) — keep them in lockstep.
	autoRequeueExternalIncidentThreshold = 3
	autoRequeueExternalIncidentWindow    = 24 * time.Hour
)

// AutoRequeueSweepResult summarises one bounded auto-requeue sweep.
type AutoRequeueSweepResult struct {
	// Inspected is the number of escalated candidates evaluated (after the
	// recheck-cooldown and cross-repo filters).
	Inspected int
	// Requeued is the number of items flipped escalated→queued this sweep.
	Requeued int
	// Skipped is the number of candidates found ineligible (wrong class, still
	// cooling, per-item cap hit, active external incident, open MR, or a stale
	// claim lost to a concurrent writer).
	Skipped int
	// Errored is the number of candidates whose store lookup or transition
	// failed; each is retried on a later tick.
	Errored int
}

// autoRequeueEval is the outcome of the per-item eligibility decision.
type autoRequeueEval struct {
	eligible   bool
	class      string // the eligible fault class (metric/event label) when eligible
	priorHits  int    // prior auto-requeues for this item (for the "n/cap" note)
	skipReason string // human-readable reason when not eligible
}

// deferAutoRequeueRecheck pushes an item's next auto-requeue inspection past the
// recheck cooldown. delete-on-requeue keeps the process-local map tracking only
// items still parked escalated.
func (r *Reconciler) deferAutoRequeueRecheck(itemID string, now time.Time) {
	if r.autoRequeueRecheck == nil {
		r.autoRequeueRecheck = make(map[string]time.Time)
	}
	r.autoRequeueRecheck[itemID] = now.Add(autoRequeueRecheckCooldown)
}

// SweepAutoRequeue is the bounded auto-requeue control loop: each tick it flips a
// small, capped number of retryable escalated backlog items back to queued so a
// human no longer has to POST the requeue endpoint. It reuses the guarded
// escalated→queued transition (StartQueuedItemOpts / closeGhostSpark path — the
// aggregate claim-version fence), so a requeued item re-enters the queue and is
// admitted, scope-serialized, and budget-checked by the normal tryStart path on
// a later tick. Nothing here starts a run directly.
//
// Eligibility (see autoRequeueEligible for the exact order):
//   - infra / transient / transient_quota → eligible after a cooldown.
//   - external-dependency incidents → eligible only once the matching incident
//     is no longer active (below the degraded-mode threshold in the window).
//   - code / config / unclassified → NEVER (a human signal).
//   - an item whose latest run has an MR is left for the ghost-spark sweep
//     (merged) or a human (open/closed) — requeuing would re-implement work
//     that already has a branch.
//
// Caps: per-item lifetime (default 2, read from durable events so it survives a
// restart), fleet-wide rolling-24h (default 6), and never when the pipeline run
// budget (MaxRunsPerDay via CountBudgetedSince) is exhausted. Disabled when the
// policy is off or pipeline.auto_requeue.enabled is false; a nil error with a
// zero result then means "sweep did nothing".
func (r *Reconciler) SweepAutoRequeue(ctx context.Context) (AutoRequeueSweepResult, error) {
	res := AutoRequeueSweepResult{}
	if r == nil || r.Store == nil || r.Store.Backlog == nil ||
		r.Store.Pipeline == nil || r.Store.Events == nil {
		return res, errors.New("reconciler: not configured")
	}
	policy := r.Policy.Current()
	if !policy.IsEnabled() || !policy.Pipeline.AutoRequeueEnabled() {
		return res, nil // sweep disabled
	}
	arp := policy.Pipeline.AutoRequeue
	now := r.now()

	// Nothing escalated ⇒ the common path — skip the cap/budget queries entirely.
	candidates, err := r.Store.Backlog.ListByStateLimit(ctx, store.BacklogEscalated, autoRequeueCandidateBatchSize)
	if err != nil {
		return res, fmt.Errorf("auto-requeue: list escalated: %w", err)
	}
	if len(candidates) == 0 {
		return res, nil
	}

	// Fleet-wide rolling-24h cap: read the base count once, then track requeues
	// committed within this tick against it (dayUsed + res.Requeued).
	dayCap := arp.DayCap()
	dayUsed, err := r.Store.Events.CountByKindSince(ctx, eventKindAutoRequeued, now.Add(-24*time.Hour))
	if err != nil {
		return res, fmt.Errorf("auto-requeue: count day window: %w", err)
	}
	if dayUsed >= dayCap {
		return res, nil // fleet already at its daily unattended-retry budget
	}

	// Run budget: never requeue into an exhausted MaxRunsPerDay. Estimate 0 —
	// the requeue itself spends nothing; the real cost is checked again when the
	// item is admitted on a later tick. A blocked decision here also covers the
	// concurrency ceiling, which is fine: no point requeuing what can't start.
	if r.Budget != nil {
		decision, berr := r.Budget.Allow(ctx, TierPipeline, 0)
		if berr != nil {
			return res, fmt.Errorf("auto-requeue: budget check: %w", berr)
		}
		if !decision.Allowed {
			r.append(ctx, "reconciler.auto_requeue_budget_blocked", "skipped", map[string]any{
				"reasons": decision.Reasons,
			})
			return res, nil
		}
	}

	cooldown := arp.CooldownDuration()
	itemCap := arp.ItemCap()
	for _, item := range candidates {
		if res.Requeued >= autoRequeueMaxPerTick || dayUsed+res.Requeued >= dayCap {
			break
		}
		if item == nil {
			continue
		}
		if until, ok := r.autoRequeueRecheck[item.ID]; ok && now.Before(until) {
			continue
		}
		// Cross-repo items are excluded here as they are in the ghost-spark
		// sweep: their MR IIDs live in another project's sequence and cross-repo
		// dispatch is separately gated (CrossRepoPolicy), so requeuing one could
		// only churn it back to a state tryStart skips. Leave them for a human
		// (cross-repo auto-requeue is a follow-up).
		if item.TargetProject != "" {
			r.deferAutoRequeueRecheck(item.ID, now)
			continue
		}
		res.Inspected++
		runs, lerr := r.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if lerr != nil {
			r.append(ctx, "reconciler.auto_requeue_failed", "error", map[string]any{
				"backlog": item.ID, "error": lerr.Error(),
			})
			res.Errored++
			continue
		}
		run := mostRecentRun(runs)
		eval := r.autoRequeueEligible(ctx, item, run, now, cooldown, itemCap, arp.IncludeCodeConfig)
		if !eval.eligible {
			res.Skipped++
			r.deferAutoRequeueRecheck(item.ID, now)
			if r.Logger != nil {
				r.Logger.Debug("auto-requeue: skip", "backlog", item.ID, "reason", eval.skipReason)
			}
			continue
		}
		committed, cerr := r.commitAutoRequeue(ctx, item, run, eval, itemCap, dayUsed+res.Requeued, dayCap)
		switch {
		case cerr != nil:
			res.Errored++
		case committed:
			res.Requeued++
		default:
			// Stale claim: a concurrent writer already moved the item off
			// escalated. Clean skip, no error storm.
			res.Skipped++
			r.deferAutoRequeueRecheck(item.ID, now)
		}
	}
	if res.Inspected > 0 || res.Errored > 0 {
		r.append(ctx, "reconciler.auto_requeue_sweep", "ok", map[string]any{
			"inspected": res.Inspected, "requeued": res.Requeued,
			"skipped": res.Skipped, "errored": res.Errored,
			"day_used": dayUsed + res.Requeued, "day_cap": dayCap,
		})
	}
	return res, nil
}

// autoRequeueEligible decides whether an escalated item may be auto-requeued now.
// Order is load-bearing:
//  1. no run ⇒ nothing to judge.
//  2. latest run has an MR ⇒ the ghost-spark sweep's (merged) or a human's
//     (open/closed) domain — never requeue (would re-implement an existing
//     branch). The MR's presence alone is decisive, so this path deliberately
//     performs no second GitLab lookup after the bounded ghost-spark sweep.
//  3. external-dependency marker ⇒ eligible only once the incident has cleared
//     (below threshold in the window). This precedes the base-class check
//     because external incidents persist as config-class with the marker set.
//  4. otherwise the base class decides: infra/transient/transient_quota are
//     eligible; everything else (code/config/unclassified) never is.
//
// The cooldown and per-item cap are the last gates for every eligible class.
func (r *Reconciler) autoRequeueEligible(
	ctx context.Context, item *store.BacklogItem, run *store.PipelineRun,
	now time.Time, cooldown time.Duration, itemCap int, includeCodeConfig bool,
) autoRequeueEval {
	if run == nil {
		return autoRequeueEval{skipReason: "no pipeline run"}
	}
	if run.MRIID != nil && *run.MRIID != 0 {
		return autoRequeueEval{skipReason: "latest run has an MR; ghost-spark/human path"}
	}

	if run.ExternalDependencyID != "" || run.ExternalDependency != "" {
		active, err := r.externalIncidentActive(ctx, run, now)
		if err != nil {
			return autoRequeueEval{skipReason: "external incident lookup failed: " + err.Error()}
		}
		if active {
			return autoRequeueEval{skipReason: "external dependency incident still active"}
		}
		if r.ExternalIncidentRetryDecision != nil {
			allowed, disposition, err := r.ExternalIncidentRetryDecision(ctx, run.ID)
			if err != nil {
				return autoRequeueEval{skipReason: "external incident retry policy failed: " + err.Error()}
			}
			if !allowed {
				if disposition == "" {
					disposition = "wait_for_dependency_recovery"
				}
				if disposition == "wait_for_dependency_recovery" {
					dwell, timedOut, dwellErr := r.reconcileExternalIncidentDwell(ctx, run, now)
					if dwellErr != nil {
						return autoRequeueEval{skipReason: "external incident dwell failed: " + dwellErr.Error()}
					}
					if timedOut {
						return autoRequeueEval{skipReason: "external dependency recovery dwell timed out"}
					}
					if dwell.CompletionReason != "" {
						return autoRequeueEval{skipReason: "external dependency recovery dwell completed: " + dwell.CompletionReason}
					}
				}
				r.append(ctx, "reconciler.auto_requeue_parked", "skipped", map[string]any{
					"backlog": item.ID, "run": run.ID, "class": autoRequeueClassExternalDependency,
					"disposition": disposition,
				})
				return autoRequeueEval{skipReason: "parked with disposition " + disposition}
			}
		}
		return r.finishEligibility(ctx, item, run, now, cooldown, itemCap, autoRequeueClassExternalDependency)
	}

	class := autoRequeueBaseClass(run)
	if class == "" {
		// Code/config opt-in (policy include_code_config): a context-carrying
		// one-shot retry. Every guard fails closed: without the item journal a
		// retry would be blind repetition; without the classifier's retryable
		// verdict it is a known-hard failure; a $0 run is no-op noise (!899)
		// and retrying it is a loop; a prior auto-requeue of ANY class means
		// the unattended path already had its shot.
		if cc := autoRequeueCodeConfigClass(run); cc != "" && includeCodeConfig {
			if !itemJournalActive() {
				return autoRequeueEval{skipReason: "code/config requeue requires the item journal (" + ItemJournalEnvName + ") active"}
			}
			if run.EscalationRetryable == nil || !*run.EscalationRetryable {
				return autoRequeueEval{skipReason: "code/config run not marked retryable by the classifier"}
			}
			if run.CostUSD <= 0 {
				return autoRequeueEval{skipReason: "code/config run spent $0 (no-op noise; not retried)"}
			}
			// One-shot: cap 1 regardless of per_item_max.
			return r.finishEligibility(ctx, item, run, now, cooldown, 1, cc)
		}
		return autoRequeueEval{skipReason: fmt.Sprintf("class %q is not auto-requeueable", run.EscalationClass)}
	}
	return r.finishEligibility(ctx, item, run, now, cooldown, itemCap, class)
}

func (r *Reconciler) reconcileExternalIncidentDwell(ctx context.Context, run *store.PipelineRun, now time.Time) (store.ExternalIncidentDwell, bool, error) {
	maxDwell := r.Policy.Current().Pipeline.AutoRequeue.ExternalIncidentMaxDwell()
	dwell, err := r.Store.Pipeline.BeginExternalIncidentDwell(
		ctx, run.ID, run.ExternalDependencyID, run.ExternalDependency, now, now.Add(maxDwell),
	)
	if err != nil {
		return dwell, false, err
	}
	if dwell.CompletionReason != "" || now.Before(dwell.DeadlineAt) {
		return dwell, false, nil
	}
	dwell, won, err := r.CompleteExternalIncidentDwell(ctx, run, store.ExternalIncidentDwellTimeout, now)
	if err != nil {
		return dwell, false, err
	}
	return dwell, won && dwell.CompletionReason == store.ExternalIncidentDwellTimeout, nil
}

// CompleteExternalIncidentDwell is shared by recovery, timeout, and fast-kill.
// Only the writer that wins the durable CAS emits telemetry and an event.
func (r *Reconciler) CompleteExternalIncidentDwell(ctx context.Context, run *store.PipelineRun, reason string, now time.Time) (store.ExternalIncidentDwell, bool, error) {
	if r == nil || r.Store == nil || r.Store.Pipeline == nil || run == nil {
		return store.ExternalIncidentDwell{}, false, errors.New("external incident dwell completion: reconciler and run required")
	}
	dwell, won, err := r.Store.Pipeline.CompleteExternalIncidentDwell(ctx, run.ID, reason, now)
	if err != nil || !won {
		return dwell, won, err
	}
	ExternalIncidentDwellDurationSeconds.WithLabelValues(reason).Observe(dwell.ElapsedDuration.Seconds())
	payload := map[string]any{
		"outcome": reason, "run": run.ID, "backlog": run.BacklogID,
		"dependency_id": dwell.DependencyID, "dependency": dwell.Dependency,
		"started_at": dwell.StartedAt, "deadline_at": dwell.DeadlineAt,
		"elapsed_seconds": dwell.ElapsedDuration.Seconds(),
		"failure_class":   run.FailureClass, "escalation_class": run.EscalationClass,
		"retryable": run.EscalationRetryable,
	}
	if appendErr := r.Store.Events.Append(ctx, &store.Event{
		Actor: "reconciler", Kind: "reconciler.external_incident_dwell_completed",
		SubjectKind: "pipeline_run", SubjectID: run.ID, Payload: payload,
	}); appendErr != nil && r.Logger != nil {
		r.Logger.Warn("external incident dwell event failed", "run", run.ID, "error", appendErr)
	}
	return dwell, true, nil
}

// autoRequeueCodeConfigClass maps a run to the opt-in code/config label, or ""
// for every other class. External-dependency-marked runs never reach here (the
// dependency branch above takes precedence).
func autoRequeueCodeConfigClass(run *store.PipelineRun) string {
	switch strings.ToLower(strings.TrimSpace(run.EscalationClass)) {
	case autoRequeueClassCode:
		return autoRequeueClassCode
	case autoRequeueClassConfig:
		return autoRequeueClassConfig
	default:
		return ""
	}
}

// finishEligibility applies the cooldown and per-item cap that gate every
// eligible class, returning the eligible verdict with the class + prior-hit
// count for the recurrence note.
func (r *Reconciler) finishEligibility(
	ctx context.Context, item *store.BacklogItem, run *store.PipelineRun,
	now time.Time, cooldown time.Duration, itemCap int, class string,
) autoRequeueEval {
	escAt := run.StartedAt
	if run.EndedAt != nil {
		escAt = *run.EndedAt
	}
	if now.Before(escAt.Add(cooldown)) {
		return autoRequeueEval{skipReason: "cooldown not elapsed"}
	}
	prior, err := r.Store.Events.CountBySubjectKind(ctx, autoRequeueSubjectKind, item.ID, eventKindAutoRequeued)
	if err != nil {
		return autoRequeueEval{skipReason: "per-item count failed: " + err.Error()}
	}
	if prior >= itemCap {
		return autoRequeueEval{skipReason: fmt.Sprintf("per-item cap reached (%d/%d)", prior, itemCap)}
	}
	return autoRequeueEval{eligible: true, class: class, priorHits: prior}
}

// autoRequeueBaseClass maps a run's persisted escalation_class to an eligible
// auto-requeue class label, or "" when the class is not auto-requeueable
// (code/config/unclassified fail closed to a human).
func autoRequeueBaseClass(run *store.PipelineRun) string {
	switch strings.ToLower(strings.TrimSpace(run.EscalationClass)) {
	case autoRequeueClassInfra:
		return autoRequeueClassInfra
	case autoRequeueClassTransient:
		return autoRequeueClassTransient
	case autoRequeueClassTransientQuota:
		return autoRequeueClassTransientQuota
	default:
		return ""
	}
}

// externalIncidentActive reports whether the dependency the run escalated
// against still has an ACTIVE degraded-mode incident: >= threshold terminal
// escalations in the rolling window. Mirrors the escalator's own recurrence
// count (ID preferred over name) so auto-requeue and degraded mode agree on
// "active".
func (r *Reconciler) externalIncidentActive(ctx context.Context, run *store.PipelineRun, now time.Time) (bool, error) {
	count, err := r.Store.Pipeline.CountRecentExternalDependencyIncidents(ctx, store.ExternalIncidentQuery{
		ExternalDependencyID: run.ExternalDependencyID,
		ExternalDependency:   run.ExternalDependency,
		Since:                now.Add(-autoRequeueExternalIncidentWindow),
	})
	if err != nil {
		return false, err
	}
	return count >= autoRequeueExternalIncidentThreshold, nil
}

// commitAutoRequeue performs the guarded escalated→queued transition and records
// the durable event, metric, log, and (best-effort) issue comment. It returns
// (true, nil) on a committed requeue, (false, nil) on a clean stale-claim skip
// (a concurrent human requeue / ghost-spark reap / peer operator won the race),
// and (false, err) only on a genuine store failure.
func (r *Reconciler) commitAutoRequeue(
	ctx context.Context, item *store.BacklogItem, run *store.PipelineRun,
	eval autoRequeueEval, itemCap, dayUsed, dayCap int,
) (bool, error) {
	attempt := eval.priorHits + 1
	event := &store.Event{
		Actor:       "reconciler",
		Kind:        eventKindAutoRequeued,
		SubjectKind: autoRequeueSubjectKind,
		SubjectID:   item.ID,
		Payload: map[string]any{
			"backlog_id": item.ID, "run_id": run.ID, "class": eval.class,
			"attempt": attempt, "per_item_cap": itemCap,
			"day_used": dayUsed + 1, "day_cap": dayCap,
		},
	}
	updated, err := r.Store.Backlog.TransitionStateWithEvent(
		ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogQueued, event,
	)
	if err != nil {
		if errors.Is(err, store.ErrStaleWrite) {
			return false, nil // lost the race; clean skip
		}
		r.append(ctx, "reconciler.auto_requeue_failed", "error", map[string]any{
			"backlog": item.ID, "run": run.ID, "class": eval.class, "error": err.Error(),
		})
		return false, err
	}
	if updated != nil {
		*item = *updated
	}
	if eval.class == autoRequeueClassExternalDependency {
		if _, lookupErr := r.Store.Pipeline.GetExternalIncidentDwell(ctx, run.ID); lookupErr == nil {
			if _, _, completeErr := r.CompleteExternalIncidentDwell(ctx, run, store.ExternalIncidentDwellRecovered, r.now()); completeErr != nil && r.Logger != nil {
				r.Logger.Warn("auto-requeue: complete external incident dwell failed", "run", run.ID, "error", completeErr)
			}
		} else if !errors.Is(lookupErr, store.ErrNotFound) && r.Logger != nil {
			r.Logger.Warn("auto-requeue: load external incident dwell failed", "run", run.ID, "error", lookupErr)
		}
	}
	delete(r.autoRequeueRecheck, item.ID)

	AutoRequeuesTotal.WithLabelValues(eval.class).Inc()
	if r.Logger != nil {
		r.Logger.Info("auto-requeued escalated item",
			"backlog", item.ID, "run", run.ID, "class", eval.class,
			"attempt", attempt, "per_item_cap", itemCap,
			"day_used", dayUsed+1, "day_cap", dayCap)
	}

	note := fmt.Sprintf("auto-requeued (%d/%d), class=%s", attempt, itemCap, eval.class)
	if r.AutoRequeueIssueCommenter != nil {
		if cerr := r.AutoRequeueIssueCommenter.CommentAutoRequeued(ctx, item, run, note); cerr != nil && r.Logger != nil {
			r.Logger.Warn("auto-requeue: issue comment failed", "backlog", item.ID, "error", cerr)
		}
	}
	return true, nil
}

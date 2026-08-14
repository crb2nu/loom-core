package overseer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/textsim"
)

// Groomer action vocabulary. Committed events use
// "overseer.groomer.<action>"; dry-run decisions append ".dryrun".
const (
	groomerActor = "overseer.groomer"

	actionDedupClose    = "dedup_close"    // retire the younger of a duplicate pair
	actionCloseObsolete = "close_obsolete" // retire an LLM-judged-obsolete zombie
	actionReprioritize  = "reprioritize"   // adjacent-bucket priority demotion
	actionZombieFlag    = "zombie_flagged" // event-only staleness flag
	groomerTickKind     = "overseer.groomer.tick"

	groomerSubjectKind = "backlog_item"
)

const (
	// groomerCandidateBatch bounds the queued-backlog scan per tick, like the
	// auto-requeue sweep's candidate batch. Pairwise similarity is O(n²) on
	// titles, fine at this size.
	groomerCandidateBatch = 200
	// groomerConfidenceMin is the LLM-verdict confidence floor: anything
	// below it is treated as "no verdict" and the action is skipped.
	groomerConfidenceMin = 0.8
)

// dupVerdict is the strict JSON envelope a gray-band dedup verdict must use.
type dupVerdict struct {
	Verdict    string  `json:"verdict"` // "duplicate" | "distinct"
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// zombieVerdict is the close-vs-keep envelope for runless aged items.
type zombieVerdict struct {
	Verdict    string  `json:"verdict"` // "close" | "keep"
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// Groomer is the backlog-hygiene overseer: deterministic duplicate/zombie/
// staleness signals over the queued backlog, LLM verdicts for the gray
// areas, and capped guarded actions (retire, demote) with a full audit
// trail. Fail-safe posture: LLM unavailable ⇒ judgment-gated actions are
// skipped, deterministic flags still land; dry-run ⇒ every would-be action
// is recorded and nothing mutates.
type Groomer struct {
	Store *store.Store
	// Policy returns the live policy snapshot (hot-reload honored per tick).
	Policy   func() *mills.Policy
	Triage   *Triage
	Recorder *ActionRecorder
	Logger   *slog.Logger
	// Now is used by tests; defaults to time.Now UTC.
	Now func() time.Time
}

// Name implements Agent.
func (g *Groomer) Name() string { return "groomer" }

// tickBudget tracks committed actions against the per-tick and rolling-24h
// caps. Dry-run decisions never consume it (they record under distinct
// event kinds, so the durable day count only sees committed actions).
type tickBudget struct {
	tickUsed, tickCap int
	dayUsed, dayCap   int
}

func (b *tickBudget) can() bool {
	return b.tickUsed < b.tickCap && b.dayUsed < b.dayCap
}

func (b *tickBudget) commit() { b.tickUsed++; b.dayUsed++ }

// llmBudget tracks verdict calls against the per-tick cap and remembers a
// failed backend so one outage doesn't burn the whole cap on errors.
type llmBudget struct {
	used, cap int
	down      bool
}

func (b *llmBudget) can() bool { return !b.down && b.used < b.cap }

// Tick implements Agent: one bounded grooming pass over the queued backlog.
func (g *Groomer) Tick(ctx context.Context) (TickResult, error) {
	res := TickResult{}
	if g == nil || g.Store == nil || g.Store.Backlog == nil || g.Store.Events == nil || g.Recorder == nil {
		return res, errors.New("groomer: not configured")
	}
	pol := g.policy()
	if pol == nil || !pol.GroomerEnabled() {
		return res, nil
	}
	gp := pol.Overseers.Groomer
	now := g.now()
	dryRun := mills.DryRunOn(gp.DryRun)

	items, err := g.Store.Backlog.ListByStateLimit(ctx, store.BacklogQueued, groomerCandidateBatch)
	if err != nil {
		return res, fmt.Errorf("groomer: list queued: %w", err)
	}
	// Deliberately no early return on an empty queued lane: the merged-duplicate
	// pass grooms ESCALATED items, and an idle queue (queue_depth 0 is the
	// steady state between council rounds) is exactly when the escalated pile
	// most needs draining. The tick still exits without an event when both
	// lanes turn out to be empty — see the res.Inspected check below.
	res.Inspected = len(items)

	dayUsed, err := g.Recorder.DayUsed(ctx, now, actionDedupClose, actionCloseObsolete, actionReprioritize)
	if err != nil {
		return res, fmt.Errorf("groomer: day-cap read: %w", err)
	}
	budget := &tickBudget{tickCap: gp.TickCap(), dayUsed: dayUsed, dayCap: gp.DayCap()}
	llm := &llmBudget{cap: gp.LLMCallCap(), down: !g.Triage.Available()}

	retired := map[string]bool{}
	if len(items) > 0 {
		g.groomDuplicates(ctx, &res, items, gp, budget, llm, dryRun, retired)
		g.groomZombies(ctx, &res, items, gp, budget, llm, dryRun, now, retired)
		g.groomStalePriorities(ctx, &res, items, gp, budget, dryRun, now, retired)
	}
	g.groomEscalatedDuplicatesOfMerged(ctx, &res, gp, budget, llm, dryRun, retired)
	if res.Inspected == 0 {
		// Nothing in either lane — stay silent rather than emitting an idle
		// tick event every interval, matching the pre-existing behaviour when
		// the queued lane was empty.
		return res, nil
	}

	note := ""
	if llm.down {
		note = "llm_unavailable"
	}
	res.Note = note
	if err := g.Store.Events.Append(ctx, &store.Event{
		Actor: groomerActor, Kind: groomerTickKind,
		Payload: map[string]any{
			"inspected": res.Inspected, "acted": res.Acted, "planned": res.Planned,
			"skipped": res.Skipped, "errored": res.Errored,
			"dry_run": dryRun, "llm_calls": llm.used, "note": note,
			"day_used": budget.dayUsed, "day_cap": budget.dayCap,
		},
	}); err != nil && g.Logger != nil {
		g.Logger.Warn("groomer: tick event append failed", "error", err)
	}
	return res, nil
}

// groomDuplicates scans queued items pairwise. Pairs at/above the threshold
// are deterministic duplicates; pairs in the gray band get an LLM verdict.
// The OLDER item is canonical, the younger is the retire candidate.
func (g *Groomer) groomDuplicates(
	ctx context.Context, res *TickResult, items []*store.BacklogItem,
	gp mills.GroomerPolicy, budget *tickBudget, llm *llmBudget,
	dryRun bool, retired map[string]bool,
) {
	threshold := gp.DedupThreshold()
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			if a == nil || b == nil || retired[a.ID] || retired[b.ID] {
				continue
			}
			score := textsim.TitleJaccard(a.Title, b.Title)
			if score < textsim.GrayBandFloor {
				continue
			}
			canonical, candidate := olderFirst(a, b)
			payload := map[string]any{
				"canonical_id": canonical.ID, "canonical_title": canonical.Title,
				"jaccard": score, "allowed": gp.Allow.DedupClose,
			}
			if score >= threshold {
				payload["basis"] = "deterministic"
				g.retire(ctx, res, budget, candidate, actionDedupClose, gp.Allow.DedupClose, dryRun, retired, payload)
				continue
			}
			// Gray band: LLM verdict required; no triage ⇒ no action.
			if !llm.can() {
				res.Skipped++
				continue
			}
			llm.used++
			var v dupVerdict
			cost, err := g.Triage.Verdict(ctx, dedupPrompt(canonical, candidate, score), &v)
			if err != nil {
				llm.down = true
				res.Errored++
				if g.Logger != nil {
					g.Logger.Warn("groomer: dedup verdict failed", "a", canonical.ID, "b", candidate.ID, "error", err)
				}
				continue
			}
			if strings.EqualFold(v.Verdict, "duplicate") && v.Confidence >= groomerConfidenceMin {
				payload["basis"] = "llm"
				payload["confidence"] = v.Confidence
				payload["reason"] = v.Reason
				payload["cost_usd"] = cost
				g.retire(ctx, res, budget, candidate, actionDedupClose, gp.Allow.DedupClose, dryRun, retired, payload)
			} else {
				res.Skipped++
			}
		}
	}
}

// groomEscalatedDuplicatesOfMerged retires an ESCALATED item whose duplicate
// has already MERGED. Every other groomer pass works the queued lane, because
// an escalated item normally implies a human is coming back to it. This pass is
// the one case where that inference is provably false: the work exists on main
// under the canonical item, so there is nothing left for anyone to come back
// for, and the duplicate only pollutes the escalated pile and re-seeds the
// council with work it already shipped.
//
// Two things differ from groomDuplicates, both deliberate:
//
//   - Canonical is always the MERGED item, never the older one. olderFirst's
//     age heuristic breaks ties between two candidates of equal standing; here
//     one of them demonstrably shipped, which outranks age.
//   - Only merged×escalated pairs are considered. Retiring an escalated item
//     against a merely queued or escalated twin would discard work that no one
//     has done yet — the exact "retiring a distinct item silently loses work"
//     failure the dedup prompt warns about.
//
// Reuses the dedup_close action so it inherits the existing allow flag, day cap
// and per-subject once-only guard rather than adding parallel policy surface;
// the payload's "basis" names the merged canonical so the two are separable in
// the audit trail.
func (g *Groomer) groomEscalatedDuplicatesOfMerged(
	ctx context.Context, res *TickResult,
	gp mills.GroomerPolicy, budget *tickBudget, llm *llmBudget,
	dryRun bool, retired map[string]bool,
) {
	escalated, err := g.Store.Backlog.ListByStateLimit(ctx, store.BacklogEscalated, groomerCandidateBatch)
	if err != nil {
		res.Errored++
		if g.Logger != nil {
			g.Logger.Warn("groomer: list escalated failed", "error", err)
		}
		return
	}
	if len(escalated) == 0 {
		return
	}
	merged, err := g.Store.Backlog.ListByStateLimit(ctx, store.BacklogMerged, groomerCandidateBatch)
	if err != nil {
		res.Errored++
		if g.Logger != nil {
			g.Logger.Warn("groomer: list merged failed", "error", err)
		}
		return
	}
	if len(merged) == 0 {
		return
	}
	res.Inspected += len(escalated)
	threshold := gp.DedupThreshold()
	for _, candidate := range escalated {
		if candidate == nil || retired[candidate.ID] {
			continue
		}
		// Best match across the merged set, so a near-miss twin cannot mask a
		// stronger one and the LLM is asked about the strongest pair only.
		var (
			canonical *store.BacklogItem
			best      float64
		)
		for _, m := range merged {
			if m == nil || m.ID == candidate.ID {
				continue
			}
			if score := textsim.TitleJaccard(candidate.Title, m.Title); score > best {
				canonical, best = m, score
			}
		}
		if canonical == nil || best < textsim.GrayBandFloor {
			continue
		}
		payload := map[string]any{
			"canonical_id": canonical.ID, "canonical_title": canonical.Title,
			"canonical_state": string(store.BacklogMerged),
			"jaccard":         best, "allowed": gp.Allow.DedupClose,
			"from_state": string(store.BacklogEscalated),
		}
		if best >= threshold {
			payload["basis"] = "deterministic_merged_canonical"
			g.retireFrom(ctx, res, budget, candidate, store.BacklogEscalated,
				actionDedupClose, gp.Allow.DedupClose, dryRun, retired, payload)
			continue
		}
		if !llm.can() {
			res.Skipped++
			continue
		}
		llm.used++
		var v dupVerdict
		cost, verr := g.Triage.Verdict(ctx, dedupPrompt(canonical, candidate, best), &v)
		if verr != nil {
			llm.down = true
			res.Errored++
			if g.Logger != nil {
				g.Logger.Warn("groomer: merged-dedup verdict failed",
					"canonical", canonical.ID, "candidate", candidate.ID, "error", verr)
			}
			continue
		}
		if strings.EqualFold(v.Verdict, "duplicate") && v.Confidence >= groomerConfidenceMin {
			payload["basis"] = "llm_merged_canonical"
			payload["confidence"] = v.Confidence
			payload["reason"] = v.Reason
			payload["cost_usd"] = cost
			g.retireFrom(ctx, res, budget, candidate, store.BacklogEscalated,
				actionDedupClose, gp.Allow.DedupClose, dryRun, retired, payload)
		} else {
			res.Skipped++
		}
	}
}

// groomZombies flags queued items past the zombie age with zero pipeline
// runs (event-only, once per item), then — when a verdict is available —
// retires the ones the LLM judges obsolete.
func (g *Groomer) groomZombies(
	ctx context.Context, res *TickResult, items []*store.BacklogItem,
	gp mills.GroomerPolicy, budget *tickBudget, llm *llmBudget,
	dryRun bool, now time.Time, retired map[string]bool,
) {
	if g.Store.Pipeline == nil {
		return
	}
	for _, item := range items {
		if item == nil || retired[item.ID] || now.Sub(item.CreatedAt) < gp.ZombieAge() {
			continue
		}
		runs, err := g.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if err != nil {
			res.Errored++
			continue
		}
		if len(runs) > 0 {
			continue
		}
		ageDays := int(now.Sub(item.CreatedAt).Hours() / 24)
		if _, err := g.Recorder.FlagOnce(ctx, actionZombieFlag, groomerSubjectKind, item.ID, map[string]any{
			"age_days": ageDays, "priority": string(item.Priority),
		}); err != nil {
			res.Errored++
			continue
		}
		if !llm.can() {
			continue
		}
		llm.used++
		var v zombieVerdict
		cost, err := g.Triage.Verdict(ctx, zombiePrompt(item, ageDays), &v)
		if err != nil {
			llm.down = true
			res.Errored++
			continue
		}
		if strings.EqualFold(v.Verdict, "close") && v.Confidence >= groomerConfidenceMin {
			g.retire(ctx, res, budget, item, actionCloseObsolete, gp.Allow.CloseObsolete, dryRun, retired, map[string]any{
				"age_days": ageDays, "basis": "llm", "confidence": v.Confidence,
				"reason": v.Reason, "cost_usd": cost, "allowed": gp.Allow.CloseObsolete,
			})
		}
	}
}

// groomStalePriorities demotes P0/P1 items untouched past the staleness age
// by one bucket. Demotion-only by design: an urgent flag that nobody acted
// on for a week is stale signal, while promotions stay human/LLM territory
// (not implemented in this slice).
func (g *Groomer) groomStalePriorities(
	ctx context.Context, res *TickResult, items []*store.BacklogItem,
	gp mills.GroomerPolicy, budget *tickBudget,
	dryRun bool, now time.Time, retired map[string]bool,
) {
	for _, item := range items {
		if item == nil || retired[item.ID] {
			continue
		}
		demoted, ok := demoteOne(item.Priority)
		if !ok || now.Sub(item.UpdatedAt) < gp.StalePriorityAge() {
			continue
		}
		payload := map[string]any{
			"from": string(item.Priority), "to": string(demoted),
			"stale_days": int(now.Sub(item.UpdatedAt).Hours() / 24),
			"allowed":    gp.Allow.Reprioritize,
		}
		prior, err := g.Recorder.SubjectCount(ctx, actionReprioritize, groomerSubjectKind, item.ID)
		if err != nil {
			res.Errored++
			continue
		}
		if prior >= 1 {
			res.Skipped++
			continue
		}
		if dryRun {
			if ok, err := g.Recorder.RecordOnce(ctx, actionReprioritize, groomerSubjectKind, item.ID, payload); err != nil {
				res.Errored++
			} else if ok {
				res.Planned++
			}
			continue
		}
		if !gp.Allow.Reprioritize || !budget.can() {
			res.Skipped++
			continue
		}
		fresh, err := g.Store.Backlog.Get(ctx, item.ID)
		if err != nil {
			res.Errored++
			continue
		}
		if fresh.State != store.BacklogQueued || fresh.Priority != item.Priority {
			res.Skipped++ // raced a human/reconciler edit; re-evaluate next tick
			continue
		}
		fresh.Priority = demoted
		if err := g.Store.Backlog.Put(ctx, fresh); err != nil {
			if errors.Is(err, store.ErrStaleWrite) {
				res.Skipped++
			} else {
				res.Errored++
			}
			continue
		}
		budget.commit()
		res.Acted++
		if err := g.Recorder.Record(ctx, actionReprioritize, groomerSubjectKind, item.ID, payload); err != nil && g.Logger != nil {
			g.Logger.Warn("groomer: reprioritize event append failed", "backlog", item.ID, "error", err)
		}
	}
}

// retire performs (or plans) the queued→retired transition for one item.
// Order of gates: per-item lifetime once → dry-run plan → allow flag →
// caps → CAS transition. A stale write (the reconciler claimed the item
// first, or a human moved it) is a clean skip, mirroring commitAutoRequeue.
func (g *Groomer) retire(
	ctx context.Context, res *TickResult, budget *tickBudget,
	item *store.BacklogItem, action string, allowed, dryRun bool,
	retired map[string]bool, payload map[string]any,
) {
	g.retireFrom(ctx, res, budget, item, store.BacklogQueued, action, allowed, dryRun, retired, payload)
}

// retireFrom is retire with an explicit from-state. The duplicate/zombie/
// priority passes all groom the queued lane; the merged-duplicate pass grooms
// escalated items, and the from-state is what fences the transition against a
// concurrent requeue or human edit.
func (g *Groomer) retireFrom(
	ctx context.Context, res *TickResult, budget *tickBudget,
	item *store.BacklogItem, from store.BacklogState,
	action string, allowed, dryRun bool,
	retired map[string]bool, payload map[string]any,
) {
	prior, err := g.Recorder.SubjectCount(ctx, action, groomerSubjectKind, item.ID)
	if err != nil {
		res.Errored++
		return
	}
	if prior >= 1 {
		res.Skipped++
		return
	}
	if dryRun {
		// Record the would-be action (AppendOnce keyed on the .dryrun kind, so
		// a week-long soak yields one auditable event per item, not one per
		// tick) even when the action class is not yet allowed — the soak's
		// whole point is auditing verdicts before flipping allow flags.
		if ok, rerr := g.Recorder.RecordOnce(ctx, action, groomerSubjectKind, item.ID, payload); rerr != nil {
			res.Errored++
		} else if ok {
			res.Planned++
			retired[item.ID] = true // treat as consumed so one tick can't plan two actions for it
		}
		return
	}
	if !allowed || !budget.can() {
		res.Skipped++
		return
	}
	event := g.Recorder.Event(action, groomerSubjectKind, item.ID, payload)
	if _, err := g.Store.Backlog.TransitionStateWithEvent(
		ctx, item.ID, item.ClaimVersion, from, store.BacklogRetired, event,
	); err != nil {
		if errors.Is(err, store.ErrStaleWrite) {
			res.Skipped++ // lost the race to the pipeline-start claim or a human; clean skip
			return
		}
		res.Errored++
		if g.Logger != nil {
			g.Logger.Warn("groomer: retire failed", "backlog", item.ID, "action", action, "error", err)
		}
		return
	}
	budget.commit()
	res.Acted++
	retired[item.ID] = true
	// The committed retire bypasses Recorder.Record (the event rides the
	// TransitionStateWithEvent transaction), so count it here.
	mills.OverseerActionsTotal.WithLabelValues(g.Name(), action, "committed").Inc()
	if g.Logger != nil {
		g.Logger.Info("groomer: retired backlog item", "backlog", item.ID, "action", action, "payload", payload)
	}
}

// olderFirst orders a pair by CreatedAt: the older (canonical) item first.
func olderFirst(a, b *store.BacklogItem) (canonical, candidate *store.BacklogItem) {
	if b.CreatedAt.Before(a.CreatedAt) {
		return b, a
	}
	return a, b
}

// demoteOne returns the next-lower priority bucket for demotable priorities.
func demoteOne(p store.Priority) (store.Priority, bool) {
	switch p {
	case store.P0:
		return store.P1, true
	case store.P1:
		return store.P2, true
	default:
		return p, false
	}
}

// dedupPrompt asks for a strict duplicate/distinct verdict on a title pair.
func dedupPrompt(canonical, candidate *store.BacklogItem, score float64) string {
	var sb strings.Builder
	sb.WriteString("You judge whether two software-backlog items describe the SAME deliverable.\n")
	sb.WriteString("Reply with ONLY a JSON object: {\"verdict\":\"duplicate\"|\"distinct\",\"confidence\":<0..1>,\"reason\":\"<one sentence>\"}.\n")
	sb.WriteString("Err toward \"distinct\": retiring a distinct item silently loses work, keeping a duplicate merely wastes a review.\n\n")
	writeItemSummary(&sb, "Item A (older, would be kept)", canonical)
	writeItemSummary(&sb, "Item B (younger, would be retired as duplicate)", candidate)
	fmt.Fprintf(&sb, "Title similarity (Jaccard): %.2f\n", score)
	return sb.String()
}

// zombiePrompt asks for a close/keep verdict on an aged runless item.
func zombiePrompt(item *store.BacklogItem, ageDays int) string {
	var sb strings.Builder
	sb.WriteString("You judge whether an aged, never-started software-backlog item is still worth keeping.\n")
	sb.WriteString("Reply with ONLY a JSON object: {\"verdict\":\"close\"|\"keep\",\"confidence\":<0..1>,\"reason\":\"<one sentence>\"}.\n")
	sb.WriteString("Err toward \"keep\": closing live intent silently loses work.\n\n")
	writeItemSummary(&sb, "Item", item)
	fmt.Fprintf(&sb, "Age: %d days queued with zero pipeline runs.\n", ageDays)
	return sb.String()
}

// writeItemSummary appends a compact, bounded description of one item.
func writeItemSummary(sb *strings.Builder, label string, item *store.BacklogItem) {
	fmt.Fprintf(sb, "%s:\n  id: %s\n  title: %s\n  priority: %s\n  created: %s\n",
		label, item.ID, item.Title, item.Priority, item.CreatedAt.Format("2006-01-02"))
	if len(item.Labels) > 0 {
		fmt.Fprintf(sb, "  labels: %s\n", strings.Join(item.Labels, ", "))
	}
	if spec := strings.TrimSpace(item.SpecDoc); spec != "" {
		if len(spec) > 240 {
			spec = spec[:240] + "…"
		}
		fmt.Fprintf(sb, "  spec: %s\n", strings.ReplaceAll(spec, "\n", " "))
	}
	sb.WriteString("\n")
}

func (g *Groomer) policy() *mills.Policy {
	if g.Policy == nil {
		return nil
	}
	return g.Policy()
}

func (g *Groomer) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now().UTC()
}

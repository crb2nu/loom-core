package mills

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Consecutive-spawn-infra circuit breaker.
//
// Per-run spawn classification (pkg/mills/pipeline/spawn_class.go) already
// names each spawn-layer defect and marks it retryable infrastructure, so an
// individual run recovers. What nothing did was AGGREGATE those failures: when
// an agent vendor has an outage (the 2026-07-25 ChatGPT Codex websocket 503s,
// issue #382), every queued item is dispatched into the dead vendor in turn,
// burns its three spawn attempts, and escalates. The reconciler kept feeding
// the outage one run at a time; plan_slice's 54.5%/30d stage error rate is
// dominated by exactly this shape.
//
// This breaker reads the durable escalation trail each tick and HOLDS new
// dispatch while the last few runs all died at the spawn layer for the SAME
// reason. It is the fast complement to the mechanisms that already exist and
// deliberately does not rebuild them:
//
//   - pipeline.BreakerEvaluator reads a 24h KPI window — far too slow to catch
//     a storm that consumes the queue in minutes.
//   - the sentinel overseer trips on dependency PROBES, not observed spawn
//     failures, and is default-OFF; this breaker must work with the overseers
//     disabled, so it is not wired through them.
//   - auto-requeue (reconciler_auto_requeue.go) retries AFTER the fact; holding
//     dispatch is what keeps its bounded budget from being spent straight back
//     into the same outage.
//
// Fail-safe bias throughout: the breaker only ever holds NEW dispatch. It never
// kills a run, never mutates a backlog item, in-flight runs are untouched, and
// any read failure leaves it CLOSED (a broken query must not stop the fleet).
const (
	// spawnBreakerEventActor / spawnBreakerEventKind identify the durable
	// escalation trail the breaker reads. pipeline.Runner appends
	// "pipeline.run.escalated" under actor "pipeline" with the full escalation
	// reason in the payload, and that reason carries the "[reason=<token>]"
	// marker the runner stamps for spawn-infrastructure defects. Reading the
	// marker back is why the breaker needs no copy of the spawn needle set:
	// a new token added in pkg/mills/pipeline participates automatically.
	spawnBreakerEventActor = "pipeline"
	spawnBreakerEventKind  = "pipeline.run.escalated"

	// spawnBreakerReasonPrefix is the marker-token namespace that counts as a
	// spawn-transport failure. Every SpawnReason* token is "spawn-"-prefixed and
	// the marker is only stamped for them today; the prefix check keeps a future
	// non-spawn "[reason=…]" marker from tripping a SPAWN breaker.
	spawnBreakerReasonPrefix = "spawn-"

	// spawnBreakerScanLimit bounds the newest-first window scan. The read rides
	// ListByActorSince, which uses the existing idx_events_occurred window scan
	// (the same reasoning as the overseers' recent-actions query): no new index
	// on the events table, which is the hot append path the fleet-reliability
	// benchmark gate protects.
	//
	// Truncation is fail-safe in the direction that matters. Actor "pipeline"
	// also carries per-stage start/done chatter, so a BUSY-and-HEALTHY window
	// can hit the limit — and losing the oldest events there can only make the
	// breaker less likely to trip. During an actual storm runs die at their
	// first stage, so the escalations are dense in the newest slice the scan
	// keeps.
	spawnBreakerScanLimit = 500
)

// spawnBreakerEventReader is the event read side the breaker needs. It matches
// *store.EventDAO; the Reconciler seam exists so tests can inject a reader that
// fails (proving a read error keeps the breaker closed).
type spawnBreakerEventReader interface {
	ListByActorSince(ctx context.Context, actor string, since time.Time, limit int) ([]*store.Event, error)
}

// SpawnBreakerStatus is one evaluation of the spawn-transport breaker. The zero
// value is the closed (dispatch allowed) verdict.
type SpawnBreakerStatus struct {
	// Open reports that dispatch is being held.
	Open bool
	// Reason is the spawn reason token that tripped it (e.g.
	// "spawn-stdin-misconfig"), empty when closed.
	Reason string
	// Failures is the number of DISTINCT runs that escalated with Reason inside
	// the window.
	Failures int
	// FirstAt / LastAt bracket those failures.
	FirstAt time.Time
	LastAt  time.Time
	// HoldUntil is LastAt + cooldown: the earliest moment the breaker
	// half-opens. Zero when closed.
	HoldUntil time.Time
	// Blocker is the operator-facing string published on the autonomy-blockers
	// surface, empty when closed.
	Blocker string
}

// SpawnTransportBreakerStatus evaluates the breaker against the current policy
// and clock without any dispatch side effects. It is exported so the operator's
// status/capability surface can publish the same verdict Tick acts on (see
// docs/MILLS.md — the reconciler's blocker already reaches the durable
// `reconciler.tick` event; folding it into the capability report's
// `autonomy_blockers` is operator wiring, not reconciler logic).
func (r *Reconciler) SpawnTransportBreakerStatus(ctx context.Context) SpawnBreakerStatus {
	if r == nil || r.Policy == nil {
		return SpawnBreakerStatus{}
	}
	return r.evaluateSpawnBreaker(ctx, r.Policy.Current(), r.now())
}

// evaluateSpawnBreaker decides whether new dispatch is held right now.
//
// The verdict is derived purely from the durable escalation trail — no
// in-memory state machine — so an operator restart neither forgets an open
// breaker nor invents one, and two operators reading the same store agree:
//
//	OPEN  ⇔ some spawn reason token has >= threshold distinct escalated runs
//	        inside the window AND now < lastFailure + cooldown.
//
// Half-open falls out of the same expression: once the cooldown elapses the
// verdict flips closed and dispatch resumes. One fresh same-reason failure then
// re-opens it immediately, because the earlier failures are still inside the
// window (which is why the window is longer than the cooldown by default). The
// window also bounds the maximum hold: failures that age out stop counting even
// if the cooldown would still be running.
func (r *Reconciler) evaluateSpawnBreaker(ctx context.Context, policy *Policy, now time.Time) SpawnBreakerStatus {
	if r == nil || policy == nil || !policy.Pipeline.SpawnBreakerEnabled() {
		return SpawnBreakerStatus{}
	}
	reader := r.spawnBreakerReader()
	if reader == nil {
		return SpawnBreakerStatus{}
	}
	cfg := policy.Pipeline.SpawnBreaker
	window := cfg.WindowDuration()
	events, err := reader.ListByActorSince(ctx, spawnBreakerEventActor, now.Add(-window), spawnBreakerScanLimit)
	if err != nil {
		// Fail-safe: a read failure must never stop the fleet. Log and dispatch.
		if r.Logger != nil && contextCancellationError(ctx, err) == nil {
			r.Logger.Warn("spawn breaker: escalation read failed; breaker stays closed", "error", err)
		}
		return SpawnBreakerStatus{}
	}

	tallies := tallySpawnBreakerFailures(events)
	threshold := cfg.FailureThreshold()
	cooldown := cfg.CooldownDuration()
	for _, t := range tallies {
		if len(t.runs) < threshold || !now.Before(t.last.Add(cooldown)) {
			continue
		}
		status := SpawnBreakerStatus{
			Open:      true,
			Reason:    t.reason,
			Failures:  len(t.runs),
			FirstAt:   t.first,
			LastAt:    t.last,
			HoldUntil: t.last.Add(cooldown),
		}
		status.Blocker = spawnBreakerBlocker(status, now)
		return status
	}
	return SpawnBreakerStatus{}
}

// spawnBreakerReader returns the event read side, preferring the test seam.
func (r *Reconciler) spawnBreakerReader() spawnBreakerEventReader {
	if r.spawnBreakerEvents != nil {
		return r.spawnBreakerEvents
	}
	if r.Store != nil && r.Store.Events != nil {
		return r.Store.Events
	}
	return nil
}

// spawnBreakerTally accumulates the distinct runs that escalated with one spawn
// reason token inside the window.
type spawnBreakerTally struct {
	reason string
	runs   map[string]struct{}
	first  time.Time
	last   time.Time
}

// tallySpawnBreakerFailures groups escalation events by spawn reason token,
// worst first (most distinct runs, then most recent). Runs are de-duplicated by
// id so one run that somehow published twice counts once — the trip condition
// must mean "N runs died", not "N rows exist".
func tallySpawnBreakerFailures(events []*store.Event) []spawnBreakerTally {
	byReason := map[string]*spawnBreakerTally{}
	for _, ev := range events {
		if ev == nil || ev.Kind != spawnBreakerEventKind {
			continue
		}
		reason := spawnBreakerReasonToken(stringField(ev.Payload, "reason"))
		if reason == "" {
			continue
		}
		runID := stringField(ev.Payload, "run")
		if runID == "" {
			// No run id (a legacy or hand-written row): key the de-dup on the
			// event id so it still counts exactly once.
			runID = fmt.Sprintf("event:%d", ev.ID)
		}
		t, ok := byReason[reason]
		if !ok {
			t = &spawnBreakerTally{reason: reason, runs: map[string]struct{}{}, first: ev.OccurredAt, last: ev.OccurredAt}
			byReason[reason] = t
		}
		t.runs[runID] = struct{}{}
		if ev.OccurredAt.Before(t.first) {
			t.first = ev.OccurredAt
		}
		if ev.OccurredAt.After(t.last) {
			t.last = ev.OccurredAt
		}
	}
	out := make([]spawnBreakerTally, 0, len(byReason))
	for _, t := range byReason {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].runs) != len(out[j].runs) {
			return len(out[i].runs) > len(out[j].runs)
		}
		if !out[i].last.Equal(out[j].last) {
			return out[i].last.After(out[j].last)
		}
		return out[i].reason < out[j].reason
	})
	return out
}

// spawnBreakerReasonToken extracts the "[reason=<token>]" marker the runner
// stamps on a spawn-infrastructure escalation, or "" when the reason carries no
// spawn marker. Same shape as the "[class=…]" marker the escalation-class
// labels already parse out of these reason strings.
func spawnBreakerReasonToken(reason string) string {
	const marker = "[reason="
	i := strings.Index(reason, marker)
	if i < 0 {
		return ""
	}
	rest := reason[i+len(marker):]
	j := strings.Index(rest, "]")
	if j < 0 {
		return ""
	}
	token := strings.ToLower(strings.TrimSpace(rest[:j]))
	if !strings.HasPrefix(token, spawnBreakerReasonPrefix) {
		return ""
	}
	return token
}

// spawnBreakerBlocker renders the operator-facing blocker line. It names the
// evidence (how many runs, which spawn reason, over what span) and when
// dispatch resumes, so the string is actionable on its own wherever the
// autonomy blockers are displayed.
func spawnBreakerBlocker(s SpawnBreakerStatus, now time.Time) string {
	span := s.LastAt.Sub(s.FirstAt)
	if span < 0 {
		span = 0
	}
	// A single-failure span reads oddly ("0m"); report the age of the evidence
	// instead so the operator sees how fresh it is.
	if s.Failures <= 1 || span == 0 {
		span = now.Sub(s.LastAt)
		if span < 0 {
			span = 0
		}
	}
	return fmt.Sprintf(
		"spawn transport breaker open: %dx %s in %s — holding dispatch until %s",
		s.Failures, s.Reason, formatSpawnBreakerSpan(span),
		s.HoldUntil.UTC().Format(time.RFC3339),
	)
}

// formatSpawnBreakerSpan renders a duration the way an operator reads it in a
// status line ("28m", "1h05m") rather than Go's default ("28m0s").
func formatSpawnBreakerSpan(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	minutes := int(d.Round(time.Minute) / time.Minute)
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
}

// stringField reads a string payload field, tolerating a missing key or a
// non-string value (event payloads round-trip through JSON).
func stringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	s, _ := payload[key].(string)
	return s
}

// Package guard is the Mill Staff guarded-auto-act substrate: the harness
// and audit recorder every autonomous staff actor rides — deterministic
// evidence first, optional LLM judgment second, and actions that are
// per-tick capped, per-day capped from durable events, dry-run by default,
// and always audited in the append-only events table under a stable actor.
//
// Consumers: the overseer agents (actor "overseer.<agent>" — groomer /
// sentinel / foreman, which re-export these types as aliases) and the
// council mutator (actor "council.mutator"). The contract was established
// by the reconciler's auto-requeue sweep and extracted here when the
// council became the second consumer (docs/FACTORY_MODEL.md §Mill staff).
package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// TickResult summarises one agent tick for the status API and the tick event.
type TickResult struct {
	// Inspected counts the entities the agent evaluated this tick.
	Inspected int `json:"inspected"`
	// Acted counts committed (non-dry-run) actions.
	Acted int `json:"acted"`
	// Planned counts dry-run "would act" decisions.
	Planned int `json:"planned"`
	// Skipped counts candidates found ineligible or blocked by a cap.
	Skipped int `json:"skipped"`
	// Errored counts candidates whose store/LLM interaction failed.
	Errored int `json:"errored"`
	// Note carries a short free-form annotation ("llm_unavailable", …).
	Note string `json:"note,omitempty"`
}

// Agent is one supervisory loop body. Tick must be safe to call repeatedly
// and bound its own work; the harness supplies pacing, gating, and status.
type Agent interface {
	Name() string
	Tick(ctx context.Context) (TickResult, error)
}

// AgentStatus is the harness-owned status snapshot for GET /api/mills/overseers.
type AgentStatus struct {
	Name       string     `json:"name"`
	Paused     bool       `json:"paused"`
	LastTickAt *time.Time `json:"last_tick_at,omitempty"`
	LastResult TickResult `json:"last_result"`
	LastError  string     `json:"last_error,omitempty"`
}

// defaultTickTimeout bounds one agent tick. Generous because a groomer tick
// may make several LLM verdict calls; the per-call budget lives in Triage.
const defaultTickTimeout = 5 * time.Minute

// Harness runs one Agent on a policy-driven interval with the operator's
// standard loop contract: synchronous Run for errgroup supervision,
// self-gating on Enabled every tick (hot-reload honored), an
// ActiveOperations counter for the destructive-safety activity snapshot, and
// a runtime soft-pause for the ops endpoints.
type Harness struct {
	Agent Agent
	// Enabled is the live admission barrier: work admission ∧ master gate ∧
	// per-agent policy enable. Read at every tick.
	Enabled func() bool
	// Interval returns the current tick cadence; read before every wait so a
	// policy hot-reload takes effect on the next tick.
	Interval func() time.Duration
	// TickTimeout bounds one tick; zero uses defaultTickTimeout.
	TickTimeout time.Duration
	// BootTick, when positive, bounds the delay before the FIRST tick after
	// Run starts (the effective first delay is min(BootTick, Interval)).
	// Without it, every Recreate rollout resets a long interval's clock —
	// on a churn-heavy day a 60m groomer may never reach its first tick
	// (observed on the 2026-08-01 soak: three operator rolls in one evening,
	// zero groomer ticks). Zero preserves the interval-first behavior.
	BootTick time.Duration
	Logger   *slog.Logger
	// Clock is used by tests; defaults to time.Now.
	Clock func() time.Time

	active atomic.Int64
	paused atomic.Bool

	mu         sync.Mutex
	running    bool
	lastTickAt time.Time
	lastResult TickResult
	lastErr    string
}

// Run drives the agent until ctx cancels. Synchronous, errgroup-shaped:
// returns nil on clean shutdown, an error only on misconfiguration.
func (h *Harness) Run(ctx context.Context) error {
	if h == nil || h.Agent == nil {
		return errors.New("overseer harness: not configured")
	}
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return fmt.Errorf("overseer harness %s: already running", h.Agent.Name())
	}
	h.running = true
	h.mu.Unlock()

	logger := h.logger()
	logger.Info("overseer loop started", "agent", h.Agent.Name())
	first := h.interval()
	if h.BootTick > 0 && h.BootTick < first {
		first = h.BootTick
	}
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("overseer loop stopped", "agent", h.Agent.Name())
			return nil
		case <-timer.C:
		}
		if h.enabled() && !h.paused.Load() {
			if _, err := h.TickOnce(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("overseer tick failed", "agent", h.Agent.Name(), "error", err)
			}
		}
		timer.Reset(h.interval())
	}
}

// TickOnce executes exactly one bounded agent tick and records its outcome.
// Exported so the manual-tick endpoint can drive the same path the loop uses.
func (h *Harness) TickOnce(ctx context.Context) (TickResult, error) {
	if h == nil || h.Agent == nil {
		return TickResult{}, errors.New("overseer harness: not configured")
	}
	h.active.Add(1)
	defer h.active.Add(-1)

	timeout := h.TickTimeout
	if timeout <= 0 {
		timeout = defaultTickTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := h.now()
	res, err := h.Agent.Tick(tctx)
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	mills.OverseerTicksTotal.WithLabelValues(h.Agent.Name(), outcome).Inc()
	mills.OverseerTickDurationSeconds.WithLabelValues(h.Agent.Name()).Observe(h.now().Sub(start).Seconds())

	h.mu.Lock()
	h.lastTickAt = h.now()
	h.lastResult = res
	h.lastErr = ""
	if err != nil {
		h.lastErr = err.Error()
	}
	h.mu.Unlock()
	return res, err
}

// ActiveOperations satisfies the operator's activity-source contract.
func (h *Harness) ActiveOperations() int64 {
	if h == nil {
		return 0
	}
	return h.active.Load()
}

// SetPaused flips the runtime soft-pause (in-memory; a restart clears it —
// durable disablement is a policy edit).
func (h *Harness) SetPaused(v bool) { h.paused.Store(v) }

// Paused reports the runtime soft-pause state.
func (h *Harness) Paused() bool { return h != nil && h.paused.Load() }

// Status returns the harness-owned status snapshot.
func (h *Harness) Status() AgentStatus {
	if h == nil || h.Agent == nil {
		return AgentStatus{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := AgentStatus{
		Name:       h.Agent.Name(),
		Paused:     h.paused.Load(),
		LastResult: h.lastResult,
		LastError:  h.lastErr,
	}
	if !h.lastTickAt.IsZero() {
		t := h.lastTickAt
		s.LastTickAt = &t
	}
	return s
}

func (h *Harness) enabled() bool { return h.Enabled == nil || h.Enabled() }

func (h *Harness) interval() time.Duration {
	if h.Interval != nil {
		if d := h.Interval(); d > 0 {
			return d
		}
	}
	return time.Hour
}

func (h *Harness) now() time.Time {
	if h.Clock != nil {
		return h.Clock()
	}
	return time.Now().UTC()
}

func (h *Harness) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// ActionRecorder writes an agent's audit trail into the append-only events
// table under a stable actor ("overseer.<agent>"). Committed actions use
// kind "overseer.<agent>.<action>"; dry-run decisions use the same kind with
// a ".dryrun" suffix so day caps (which count committed kinds only) are
// never consumed by a soak.
type ActionRecorder struct {
	Events *store.EventDAO
	// Actor is the stable event actor, e.g. "overseer.groomer".
	Actor string
	// DryRun is read per record so a policy hot-reload flips behavior
	// mid-loop. Nil means dry-run (fail-safe).
	DryRun func() bool
}

// dryRun resolves the fail-safe default: no wiring means dry-run.
func (r *ActionRecorder) dryRun() bool { return r == nil || r.DryRun == nil || r.DryRun() }

// agentLabel derives the metrics agent label from the actor: overseer
// actors keep their short name ("overseer.groomer" → "groomer", the
// pre-extraction label shape); any other staff actor ("council.mutator")
// labels under its full actor string, which stays self-describing.
func (r *ActionRecorder) agentLabel() string {
	if rest, ok := strings.CutPrefix(r.Actor, "overseer."); ok {
		return rest
	}
	return r.Actor
}

// countAction records one audit write in the actions metric. Mode is the
// recorder-contract mode: dryrun/committed (Record/RecordOnce), observed
// (Observe), flagged (FlagOnce). The groomer's transactional retire path
// commits via BacklogDAO.TransitionStateWithEvent and counts itself.
func (r *ActionRecorder) countAction(action, mode string) {
	mills.OverseerActionsTotal.WithLabelValues(r.agentLabel(), action, mode).Inc()
}

// recordMode resolves the mode label for Record/RecordOnce writes.
func (r *ActionRecorder) recordMode() string {
	if r.dryRun() {
		return "dryrun"
	}
	return "committed"
}

// Kind returns the committed event kind for an action.
func (r *ActionRecorder) Kind(action string) string { return r.Actor + "." + action }

// kindFor returns the kind an action records under given the live dry-run state.
func (r *ActionRecorder) kindFor(action string) string {
	if r.dryRun() {
		return r.Kind(action) + ".dryrun"
	}
	return r.Kind(action)
}

// Record appends one action event (kind resolved against dry-run state).
func (r *ActionRecorder) Record(ctx context.Context, action, subjectKind, subjectID string, payload map[string]any) error {
	if r == nil || r.Events == nil {
		return errors.New("overseer recorder: not configured")
	}
	err := r.Events.Append(ctx, &store.Event{
		Actor:       r.Actor,
		Kind:        r.kindFor(action),
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Payload:     payload,
	})
	if err == nil {
		r.countAction(action, r.recordMode())
	}
	return err
}

// RecordOnce appends one action event at most once per (kind, subject) —
// used for flags and dry-run decisions that would otherwise repeat every
// tick. Returns whether this call recorded the first instance.
func (r *ActionRecorder) RecordOnce(ctx context.Context, action, subjectKind, subjectID string, payload map[string]any) (bool, error) {
	if r == nil || r.Events == nil {
		return false, errors.New("overseer recorder: not configured")
	}
	ok, err := r.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       r.Actor,
		Kind:        r.kindFor(action),
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Payload:     payload,
	})
	if err == nil && ok {
		r.countAction(action, r.recordMode())
	}
	return ok, err
}

// Observe appends an observation event under the COMMITTED kind regardless
// of dry-run state. Observations (incident opened/cleared, anomaly detected)
// describe reality rather than mutate it, so a soak must record them
// identically to production.
func (r *ActionRecorder) Observe(ctx context.Context, action, subjectKind, subjectID string, payload map[string]any) error {
	if r == nil || r.Events == nil {
		return errors.New("overseer recorder: not configured")
	}
	err := r.Events.Append(ctx, &store.Event{
		Actor:       r.Actor,
		Kind:        r.Kind(action),
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Payload:     payload,
	})
	if err == nil {
		r.countAction(action, "observed")
	}
	return err
}

// FlagOnce appends an observation event at most once per (kind, subject)
// under the COMMITTED kind regardless of dry-run state — flags mutate
// nothing, so a soak's flags must not re-mint when dry-run later flips off.
func (r *ActionRecorder) FlagOnce(ctx context.Context, action, subjectKind, subjectID string, payload map[string]any) (bool, error) {
	if r == nil || r.Events == nil {
		return false, errors.New("overseer recorder: not configured")
	}
	ok, err := r.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       r.Actor,
		Kind:        r.Kind(action),
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Payload:     payload,
	})
	if err == nil && ok {
		r.countAction(action, "flagged")
	}
	return ok, err
}

// Event builds (without persisting) a committed-kind action event for use
// with transactional writers like BacklogDAO.TransitionStateWithEvent, which
// must append the event atomically with the state change.
func (r *ActionRecorder) Event(action, subjectKind, subjectID string, payload map[string]any) *store.Event {
	return &store.Event{
		Actor:       r.Actor,
		Kind:        r.Kind(action),
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Payload:     payload,
	}
}

// DayUsed sums the rolling-24h committed-action count across the given
// actions. Read from durable events so the day cap survives a restart,
// mirroring the auto-requeue sweep's mechanism.
func (r *ActionRecorder) DayUsed(ctx context.Context, now time.Time, actions ...string) (int, error) {
	if r == nil || r.Events == nil {
		return 0, errors.New("overseer recorder: not configured")
	}
	total := 0
	since := now.Add(-24 * time.Hour)
	for _, a := range actions {
		n, err := r.Events.CountByKindSince(ctx, r.Kind(a), since)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// SubjectCount returns the all-time committed-action count for one subject —
// the per-item lifetime cap read, identical to auto-requeue's.
func (r *ActionRecorder) SubjectCount(ctx context.Context, action, subjectKind, subjectID string) (int, error) {
	if r == nil || r.Events == nil {
		return 0, errors.New("overseer recorder: not configured")
	}
	return r.Events.CountBySubjectKind(ctx, subjectKind, subjectID, r.Kind(action))
}

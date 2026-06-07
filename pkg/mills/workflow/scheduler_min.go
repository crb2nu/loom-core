package workflow

// scheduler_min.go is the minimal wall-clock driver for the imperative workflow
// runtime (Mills dynamic-workflows, plan .loom/134 §S6-min). It mirrors the
// cadence + errgroup-friendly shape of pkg/mills.Scheduler: a synchronous tick
// loop the operator supervises in its errgroup alongside the council + pipeline
// schedulers.
//
// Self-gating: every tick first consults the policy gate. When workflows are
// disabled (the default-OFF flag, policy.workflows.enabled=false) the tick is a
// no-op, so the scheduler is safe to always wire — it does nothing until the
// S1c canary flips the flag.
//
// Per-run advance: for each running imperative run the scheduler calls
// interp.Run, which replays the durable journal (completed steps short-circuit;
// a pending-with-spawn-id step re-attaches via Resume; the first un-recorded
// effect runs live). This is what reconciles interrupted spawns on restart.
//
// REQUIRED between-step safety (§5 mustChange): before advancing a run the
// scheduler checks BOTH the policy gate AND the run's paused_at. The global kill
// switch (policy.enabled) is eventually-consistent (GitOps→Flux→ConfigMap poll)
// and can only block NEW ticks — it cannot abort an in-flight run. paused_at is
// the FAST stop: an operator setting paused_at on a run stops it advancing on
// the very next tick, between steps.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// PolicyGate is the minimal policy surface the scheduler needs. The operator
// adapts *mills.PolicyManager to this (a one-line closure over
// pm.Current().WorkflowsEnabled()), keeping the workflow package free of a
// dependency on pkg/mills.
type PolicyGate interface {
	// WorkflowsEnabled reports whether the imperative runtime should advance
	// runs this tick. False (the default-OFF flag) makes the tick a no-op.
	WorkflowsEnabled() bool
}

// PolicyGateFunc adapts a func to a PolicyGate.
type PolicyGateFunc func() bool

// WorkflowsEnabled satisfies PolicyGate.
func (f PolicyGateFunc) WorkflowsEnabled() bool { return f() }

const defaultWorkflowSchedulerInterval = 60 * time.Second

// WorkflowScheduler ticks the imperative runtime. The zero value is unusable;
// use NewWorkflowScheduler.
type WorkflowScheduler struct {
	dao    *store.WorkflowDAO
	interp *WorkflowInterpreter
	gate   PolicyGate
	logger *slog.Logger

	// Interval is the tick cadence. Zero falls back to 60s (mirrors
	// mills.Scheduler). Tests set a small value.
	Interval time.Duration

	// running guards against double-Run.
	mu      sync.Mutex
	running bool
}

// NewWorkflowScheduler builds a scheduler over a DAO, the runtime, and a policy
// gate. A nil logger falls back to slog.Default. A nil gate is treated as
// always-disabled (fail-closed) so a misconfigured wire never activates the
// runtime.
func NewWorkflowScheduler(dao *store.WorkflowDAO, interp *WorkflowInterpreter, gate PolicyGate, logger *slog.Logger) *WorkflowScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &WorkflowScheduler{
		dao:      dao,
		interp:   interp,
		gate:     gate,
		logger:   logger,
		Interval: defaultWorkflowSchedulerInterval,
	}
}

// Run drives the tick loop until ctx cancels. Returns nil on clean shutdown so
// it composes with errgroup.WithContext alongside the other operator
// schedulers. A nil dao or interp makes Run a benign block-until-cancel no-op
// (degraded boot), so the operator's g.Go(...) stays balanced.
func (s *WorkflowScheduler) Run(ctx context.Context) error {
	if s == nil {
		return errors.New("workflow scheduler: nil")
	}
	if s.dao == nil || s.interp == nil {
		s.logger.Warn("workflow scheduler disabled (runtime not configured); idling")
		<-ctx.Done()
		return nil
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("workflow scheduler: already running")
	}
	s.running = true
	s.mu.Unlock()

	interval := s.Interval
	if interval <= 0 {
		interval = defaultWorkflowSchedulerInterval
	}
	s.logger.Info("workflow scheduler starting", "interval", interval)

	// First tick fires immediately so a restart resumes interrupted runs
	// without waiting a full interval.
	s.tick(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick is one pass: self-gate on the flag, list running imperative runs, and
// advance each one (honoring its paused_at between steps).
func (s *WorkflowScheduler) tick(ctx context.Context) {
	// Self-gate: default-OFF flag. No work until the canary flips it on.
	if s.gate == nil || !s.gate.WorkflowsEnabled() {
		return
	}

	runs, err := s.dao.ListRunningImperativeRuns(ctx)
	if err != nil {
		s.logger.Warn("workflow scheduler: list running runs failed", "error", err)
		return
	}
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		s.advance(ctx, run)
	}
}

// advance drives one run through interp.Run, but only after the between-step
// safety checks pass. This is the §5 mustChange: a fast stop that takes effect
// between steps, since policy.enabled can't abort an in-flight spawn.
func (s *WorkflowScheduler) advance(ctx context.Context, run *store.WorkflowRun) {
	// Between-step stop #1: policy may have flipped OFF since the tick started
	// (hot-reload). Re-check before EACH run so a mid-tick disable stops the
	// next run cleanly.
	if !s.gate.WorkflowsEnabled() {
		s.logger.Info("workflow scheduler: workflows disabled mid-tick; stopping advance", "run_id", run.ID)
		return
	}

	// Between-step stop #2: the run is paused. Re-load the run so we observe a
	// paused_at written since the ListRunningImperativeRuns snapshot (the list
	// query filters on state='running', but an operator may set paused_at +
	// state='paused' concurrently; a fresh load is the authoritative check).
	fresh, err := s.dao.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		s.logger.Warn("workflow scheduler: reload run failed; skipping", "run_id", run.ID, "error", err)
		return
	}
	if fresh.PausedAt != nil || fresh.State != store.WorkflowRunRunning {
		s.logger.Info("workflow scheduler: run paused/stopped; not advancing",
			"run_id", run.ID, "state", string(fresh.State), "paused", fresh.PausedAt != nil)
		return
	}

	if err := s.interp.Run(ctx, fresh); err != nil {
		s.logger.Warn("workflow scheduler: run advance failed", "run_id", run.ID, "error", err)
	}
}

// CreateImperativeRun is the minimal admin/test entrypoint to launch the canary:
// it inserts a workflow_runs row with engine='imperative', state='running' for a
// backlog item. S7 (council selection) does not exist yet, so this is the only
// way to enqueue an imperative run. The id should be caller-supplied and stable
// (e.g. "wf-canary-<ts>") so the durable journal keys are reproducible.
//
// It is a method on the scheduler purely for discoverability (the launch site
// lives next to the consumer); it only needs the DAO.
func (s *WorkflowScheduler) CreateImperativeRun(ctx context.Context, id, backlogID string) (*store.WorkflowRun, error) {
	return CreateImperativeRun(ctx, s.dao, id, backlogID)
}

// CreateImperativeRun inserts a fresh running imperative workflow run. Exposed
// as a package function so callers that hold only a *store.WorkflowDAO (the
// operator admin path, tests) can launch a canary without a scheduler.
func CreateImperativeRun(ctx context.Context, dao *store.WorkflowDAO, id, backlogID string) (*store.WorkflowRun, error) {
	if dao == nil {
		return nil, errors.New("workflow: nil dao")
	}
	if id == "" {
		return nil, errors.New("workflow: run id required")
	}
	now := time.Now().UTC()
	run := &store.WorkflowRun{
		ID:                 id,
		BacklogID:          backlogID,
		Engine:             store.WorkflowEngineImperative,
		Template:           "workflow-canary",
		TemplateVersion:    "v0",
		InterpreterVersion: HostInterpreterVersion,
		State:              store.WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := dao.PutWorkflowRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

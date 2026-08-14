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
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crb2nu/loom/pkg/mills"
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

// RunDeadlineGate is the OPTIONAL PolicyGate extension carrying the
// imperative-run wall-clock bound. A gate that does not implement it (older
// wiring, PolicyGateFunc, tests) disables the deadline sweep entirely —
// fail-open here is deliberate, because terminalizing runs on a default a
// caller never chose would be a behavior change smuggled through an interface
// upgrade. The operator's gate implements it from live policy.
type RunDeadlineGate interface {
	// MaxRunAge returns the wall-clock bound for a running imperative run.
	// Non-positive disables the sweep.
	MaxRunAge() time.Duration
}

// PolicyGateFunc adapts a func to a PolicyGate.
type PolicyGateFunc func() bool

// WorkflowsEnabled satisfies PolicyGate.
func (f PolicyGateFunc) WorkflowsEnabled() bool { return f() }

const defaultWorkflowSchedulerInterval = 60 * time.Second
const defaultWorkflowMaxConcurrentRuns = 4

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
	// MaxConcurrentRuns bounds independent imperative run advances in one tick.
	// A single agent() can stay in flight for minutes; advancing runs serially
	// made the first slow spawn starve every sibling and reduced the dynamic
	// runtime to an accidental global singleton. Zero defaults to 4.
	MaxConcurrentRuns int
	// Enabled is an optional in-process handoff fence. Production uses it to
	// keep a just-created canary from advancing until its admission transaction
	// has completed and the policy generation has been revalidated.
	Enabled func() bool

	// running guards against double-Run.
	mu         sync.Mutex
	running    bool
	activeMu   sync.Mutex
	activeRuns map[string]int
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
		dao:               dao,
		interp:            interp,
		gate:              gate,
		logger:            logger,
		Interval:          defaultWorkflowSchedulerInterval,
		MaxConcurrentRuns: defaultWorkflowMaxConcurrentRuns,
		activeRuns:        make(map[string]int),
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
	started := time.Now()
	outcome := "disabled"
	defer func() {
		mills.WorkflowSchedulerTicksTotal.WithLabelValues(outcome).Inc()
		mills.WorkflowSchedulerTickDurationSeconds.Observe(time.Since(started).Seconds())
	}()
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	// Self-gate: default-OFF flag. No work until the canary flips it on.
	if s.gate == nil || !s.gate.WorkflowsEnabled() {
		return
	}

	runs, err := s.dao.ListRunningImperativeRuns(ctx)
	if err != nil {
		if ctx.Err() != nil {
			outcome = "cancelled"
		} else {
			outcome = "error"
		}
		s.logger.Warn("workflow scheduler: list running runs failed", "error", err)
		return
	}
	limit := s.MaxConcurrentRuns
	if limit <= 0 {
		limit = defaultWorkflowMaxConcurrentRuns
	}
	g, advanceCtx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for _, run := range runs {
		if advanceCtx.Err() != nil {
			break
		}
		run := run
		g.Go(func() error {
			s.advance(advanceCtx, run)
			return nil
		})
	}
	_ = g.Wait()
	if ctx.Err() != nil {
		outcome = "cancelled"
	} else {
		outcome = "ok"
	}
}

// advance drives one run through interp.Run, but only after the between-step
// safety checks pass. This is the §5 mustChange: a fast stop that takes effect
// between steps, since policy.enabled can't abort an in-flight spawn.
func (s *WorkflowScheduler) advance(ctx context.Context, run *store.WorkflowRun) {
	outcome := "fenced"
	defer func() { mills.WorkflowRunAdvancesTotal.WithLabelValues(outcome).Inc() }()
	// Publish before the first policy/state read so cleanup and crash fencing
	// cannot observe zero in the decision→interpreter handoff window.
	s.beginActiveRun(run.ID)
	defer s.endActiveRun(run.ID)
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	// Between-step stop #1: policy may have flipped OFF since the tick started
	// (hot-reload). Re-check before EACH run so a mid-tick disable stops the
	// next run cleanly.
	if !s.gate.WorkflowsEnabled() {
		outcome = "disabled"
		s.logger.Info("workflow scheduler: workflows disabled mid-tick; stopping advance", "run_id", run.ID)
		return
	}

	// Between-step stop #2: the run is paused. Re-load the run so we observe a
	// paused_at written since the ListRunningImperativeRuns snapshot (the list
	// query filters on state='running', but an operator may set paused_at +
	// state='paused' concurrently; a fresh load is the authoritative check).
	fresh, err := s.dao.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		outcome = "reload_error"
		s.logger.Warn("workflow scheduler: reload run failed; skipping", "run_id", run.ID, "error", err)
		return
	}
	if fresh.PausedAt != nil || fresh.State != store.WorkflowRunRunning {
		outcome = "paused"
		s.logger.Info("workflow scheduler: run paused/stopped; not advancing",
			"run_id", run.ID, "state", string(fresh.State), "paused", fresh.PausedAt != nil)
		return
	}

	// Wall-clock bound: a run still running past the policy deadline is
	// terminalized as error instead of advanced. The lifecycle CAS settles it
	// (reservation released, claimed item escalated for review), so a wedged
	// run can never hold quiescence hostage. The spawn's own supervision
	// bounds the agent; this bounds the RUN.
	if deadlineGate, ok := s.gate.(RunDeadlineGate); ok && fresh.StartedAt != nil {
		if maxAge := deadlineGate.MaxRunAge(); maxAge > 0 {
			if age := time.Since(fresh.StartedAt.UTC()); age > maxAge {
				outcome = "deadline"
				now := time.Now().UTC()
				fresh.State = store.WorkflowRunError
				fresh.EndedAt = &now
				won, err := s.dao.CompareAndSetWorkflowRunLifecycle(ctx, fresh, store.WorkflowRunRunning)
				if err != nil {
					s.logger.Warn("workflow scheduler: deadline terminalize failed",
						"run_id", run.ID, "age", age.String(), "error", err)
					return
				}
				if won {
					mills.WorkflowRunsTerminalTotal.WithLabelValues(string(store.WorkflowRunError), "deadline").Inc()
					s.logger.Warn("workflow scheduler: run exceeded wall-clock bound; terminalized",
						"run_id", run.ID, "age", age.String(), "max_age", maxAge.String())
				}
				return
			}
		}
	}

	if err := s.interp.Run(ctx, fresh); err != nil {
		outcome = "error"
		s.logger.Warn("workflow scheduler: run advance failed", "run_id", run.ID, "error", err)
		return
	}
	outcome = "ok"
}

func (s *WorkflowScheduler) beginActiveRun(runID string) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeRuns == nil {
		s.activeRuns = make(map[string]int)
	}
	s.activeRuns[runID]++
	mills.WorkflowRunsAdvancing.Inc()
}

func (s *WorkflowScheduler) endActiveRun(runID string) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeRuns[runID] <= 1 {
		delete(s.activeRuns, runID)
		mills.WorkflowRunsAdvancing.Dec()
		return
	}
	s.activeRuns[runID]--
	mills.WorkflowRunsAdvancing.Dec()
}

// ActiveOperations reports imperative interpreters currently executing.
func (s *WorkflowScheduler) ActiveOperations() int64 {
	total, _ := s.ActiveOperationSnapshot()
	return total
}

// ActiveRunIDs binds the activity count to exact durable workflow identities
// so the crash gate may allow only its target interpreter.
func (s *WorkflowScheduler) ActiveRunIDs() []string {
	_, ids := s.ActiveOperationSnapshot()
	return ids
}

// ActiveOperationSnapshot returns the count and exact run IDs under one lock.
// The destructive safety gate must never combine a pre-transition count with
// post-transition identities from separate reads.
func (s *WorkflowScheduler) ActiveOperationSnapshot() (int64, []string) {
	if s == nil {
		return 0, nil
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	var total int64
	ids := make([]string, 0, len(s.activeRuns))
	for id, count := range s.activeRuns {
		total += int64(count)
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return total, ids
}

// CreateImperativeRun is the minimal admin/test entrypoint to launch the canary:
// it inserts a workflow_runs row with engine='imperative', state='running' for a
// backlog item. Since S7 shipped, production runs enter through
// Store.ClaimWorkflowStart (frozen council selections resolved in the
// reconciler); this path remains for canaries and tests only. The id should be
// caller-supplied and stable (e.g. "wf-canary-<ts>") so the durable journal
// keys are reproducible.
//
// It is a method on the scheduler purely for discoverability (the launch site
// lives next to the consumer); it only needs the DAO.
func (s *WorkflowScheduler) CreateImperativeRun(ctx context.Context, id, backlogID string) (*store.WorkflowRun, error) {
	return CreateImperativeRun(ctx, s.dao, id, backlogID)
}

// CreateImperativeRunWithAgentType launches the same canary with an explicit
// portable worker harness. The selected harness is persisted in immutable run
// metadata and is therefore fixed for every replay of this run id.
func (s *WorkflowScheduler) CreateImperativeRunWithAgentType(ctx context.Context, id, backlogID, agentType string) (*store.WorkflowRun, error) {
	return CreateImperativeRunWithAgentType(ctx, s.dao, id, backlogID, agentType)
}

// CreateImperativeRun inserts a fresh running imperative workflow run. Exposed
// as a package function so callers that hold only a *store.WorkflowDAO (the
// operator admin path, tests) can launch a canary without a scheduler.
func CreateImperativeRun(ctx context.Context, dao *store.WorkflowDAO, id, backlogID string) (*store.WorkflowRun, error) {
	return CreateImperativeRunWithAgentType(ctx, dao, id, backlogID, "")
}

// CreateImperativeRunWithAgentType inserts a fresh running imperative canary
// whose portable agent choice is immutable workflow identity. Empty agentType
// retains the legacy claude-code default for existing callers.
func CreateImperativeRunWithAgentType(ctx context.Context, dao *store.WorkflowDAO, id, backlogID, agentType string) (*store.WorkflowRun, error) {
	return CreateImperativeRunWithOptions(ctx, dao, id, backlogID, agentType, false)
}

// CreateImperativeRunWithOptions inserts a fresh running imperative canary.
// merging selects the S6-full merging canary: template version v3 and merging
// params are stamped together so durable identity and derived script agree
// (CanaryMergingFromRun fails closed on any mismatch).
func CreateImperativeRunWithOptions(ctx context.Context, dao *store.WorkflowDAO, id, backlogID, agentType string, merging bool) (*store.WorkflowRun, error) {
	if dao == nil {
		return nil, errors.New("workflow: nil dao")
	}
	if id == "" {
		return nil, errors.New("workflow: run id required")
	}
	workflowParams, err := canaryWorkflowParamsJSONWithOptions(agentType, merging)
	if err != nil {
		return nil, err
	}
	templateVersion := CanaryTemplateVersion
	if merging {
		templateVersion = CanaryMergingTemplateVersion
	}
	now := time.Now().UTC()
	run := &store.WorkflowRun{
		ID:                 id,
		BacklogID:          backlogID,
		Engine:             store.WorkflowEngineImperative,
		Template:           CanaryTemplateName,
		TemplateVersion:    templateVersion,
		InterpreterVersion: HostInterpreterVersion,
		WorkflowParams:     workflowParams,
		State:              store.WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := dao.CreateWorkflowRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

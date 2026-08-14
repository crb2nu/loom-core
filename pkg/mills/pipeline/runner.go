// Package pipeline drives a backlog item through the mills-default-pipeline
// DAG defined in cmd/mcp-mentatlab/templates/mills-default-pipeline.yaml.
//
// The Runner is the operator-side state machine that materialises a
// pipeline_runs row through every stage, persists stage_results +
// gate_outcomes after each step, and either reaches a terminal state
// (done, escalated, paused) or returns control so the reconciler can
// resume on the next tick.
//
// Slice 4.1 ships the engine: stage iteration, gate evaluation, retry on
// gate fail, and resume-on-restart. Worker dispatch is behind the
// WorkerDispatcher interface so slice 4.2 can drop in spawn/devbox/MCP
// implementations without touching this file.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// Stage describes one node in the pipeline DAG. It captures only the
// metadata the runner needs; the actual worker logic for non-gate stages
// is resolved through WorkerDispatcher.
type Stage struct {
	// ID matches the node id in mills-default-pipeline.yaml.
	ID string
	// Type is one of "llm", "agent_spawn", "shell", "auto_gate".
	Type string
	// State is the pipeline_runs.state to record while this stage runs.
	// Gate stages inherit the state of the upstream non-gate stage.
	State store.PipelineState
	// Gates is the ordered list of gate names to evaluate (auto_gate only).
	Gates []string
	// RetryFrom names the upstream stage to re-run when a gate fails.
	// Empty for non-gate stages. The static contract is captured in
	// DefaultStages; runtime can override for custom DAGs later.
	RetryFrom string
}

// DefaultStages mirrors mills-default-pipeline.yaml. Order is significant.
//
// The set of gates per auto_gate matches §"Pipeline flow template" and
// §"Stage gates — required v1 set" in .loom/90-…; LLM-judged gates
// (spec_conformance, pr_self_review) are listed here but only fire when
// slice 4.5 registers them on the gate registry.
var DefaultStages = []Stage{
	{ID: "plan_slice", Type: "llm", State: store.PipelinePlanning},
	{ID: "research", Type: "llm", State: store.PipelinePlanning},
	{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing},
	{
		ID:        "post_implement_gate",
		Type:      "auto_gate",
		State:     store.PipelineImplementing,
		RetryFrom: "implement",
		Gates:     []string{"nonempty_diff", "branch_pushed", "diff_size", "scope", "fabricated_slice", "path_policy", "secret_scan", "commit_format", "docs_guardrail"},
	},
	{ID: "tests", Type: "shell", State: store.PipelineTesting},
	{
		ID:        "post_tests_gate",
		Type:      "auto_gate",
		State:     store.PipelineTesting,
		RetryFrom: "implement",
		Gates:     []string{},
	},
	{ID: "pr_self_review", Type: "agent_spawn", State: store.PipelineReviewing},
	{
		ID:        "post_review_gate",
		Type:      "auto_gate",
		State:     store.PipelineReviewing,
		RetryFrom: "pr_self_review",
		Gates:     []string{"spec_conformance", "pr_self_review"},
	},
	{ID: "mr", Type: "shell", State: store.PipelineMR},
	{
		ID:        "post_mr_gate",
		Type:      "auto_gate",
		State:     store.PipelineMR,
		RetryFrom: "mr",
		Gates:     []string{},
	},
	{ID: "ci_watch", Type: "shell", State: store.PipelineCI},
	{
		ID:        "post_ci_gate",
		Type:      "auto_gate",
		State:     store.PipelineCI,
		RetryFrom: "implement",
		Gates:     []string{},
	},
	{ID: "merge", Type: "shell", State: store.PipelineMerging},
	{
		ID:        "post_merge_gate",
		Type:      "auto_gate",
		State:     store.PipelineMerging,
		RetryFrom: "merge",
		Gates:     []string{},
	},
	{ID: "cleanup", Type: "shell", State: store.PipelineMerging},
}

// StageOutput is the bundle every worker returns to the runner. Fields
// are loosely typed because the dispatcher in slice 4.2 wraps a mix of
// spawn calls, MCP tool calls, and shell commands; the runner only needs
// what gates and downstream stages will consume.
type StageOutput struct {
	CostUSD        float64
	SpawnID        string
	LogTail        string
	Artifacts      map[string]any
	FilesChanged   []string
	LinesAdded     int
	LinesRemoved   int
	DiffPatch      []byte
	CommitMessages []string
	// MRIID, when non-zero, is propagated up onto pipeline_runs.mr_iid.
	MRIID int64
	// WorktreePath, when set, is propagated up onto pipeline_runs.worktree_path.
	WorktreePath string
	// MergedSHA is populated by the merge stage; the runner stores it on
	// the run row so eval Loop B can attribute outcomes.
	MergedSHA string
	// CostEstimated records whether CostUSD is a Loom-side estimate (e.g.
	// Codex) rather than an authoritative SDK figure. Additive provenance
	// surfaced from SpawnResponse.CostEstimated; it never changes the
	// CostUSD value. Defaults false (real or unavailable cost).
	CostEstimated bool

	// Model + Backend attribute this attempt's cost to a model tier for the
	// per-model telemetry roll-up (persisted on stage_results via migration
	// 013). A worker sets them when it knows which model/backend produced the
	// work: the research worker surfaces the resolved FlexInfer model +
	// "flexinfer"; the spawn worker surfaces the resolved agent/model +
	// "spawn". Empty is fine — the aggregation buckets unattributed rows under
	// "unknown".
	Model   string
	Backend string
}

type stageAcceptRecorderKey struct{}
type resumeSpawnIDKey struct{}
type stageAttemptKey struct{}
type stageRetryContextKey struct{}
type mergeRecoveryPipelineCreateRecorderKey struct{}
type mergeRecoveryPipelineCreateAttemptedKey struct{}
type ciWatchFlakeRescueRecorderKey struct{}
type ciWatchFlakeRescueAttemptedKey struct{}
type ciWatchFlakeRescueFirstJobsKey struct{}
type mergeRecovery405RecorderKey struct{}
type headTransitionSeqKey struct{}

const mergeRecoveryPipelineCreateAttemptedArtifact = "merge_recovery_pipeline_create_attempted"
const ciWatchFlakeRescueAttemptedArtifact = "ci_watch_flake_rescue_attempted"
const ciWatchFlakeRescueFirstJobsArtifact = "ci_watch_flake_rescue_first_jobs"

// StageRetryContext describes why the runner is re-dispatching a stage after
// a downstream auto_gate failed. It is threaded to the WorkerDispatcher (via
// context, mirroring the resume spawn id) so spawn prompt builders can tell
// the fresh agent it is a RETRY — which gate failed, and that the previous
// attempt's work (including any plan-store slice status it advanced) must be
// treated as stale and redone. Without it, a plan-linked retry agent resolves
// the plan via agent_plan_get, finds the slice already claimed/implemented by
// the discarded attempt, does nothing, and fails nonempty_diff — masking the
// original gate failure (observed live 2026-07-01 on
// PIPE-pattern-stamp-go-rest-service-{widget,gadget}-…).
type StageRetryContext struct {
	// Attempt is the attempt number of the dispatch this context rides on
	// (2 for the first retry). Zero until the runner stamps it at dispatch.
	Attempt int
	// GateStage is the auto_gate stage whose failure first triggered a
	// retry of this stage (e.g. "post_implement_gate").
	GateStage string
	// FirstFailure summarizes the failing gates of the FIRST recorded
	// failure ("scope: file docs/x.md outside slice scope"). This is the
	// original defect; later failures are often knock-on effects
	// (nonempty_diff on a do-nothing retry).
	FirstFailure string
	// LastFailure summarizes the most recent failure. Equal to
	// FirstFailure on the first retry.
	LastFailure string
}

var errStagePending = errors.New("pipeline: stage remains pending")

// dedupedStageAttemptError marks a terminal reconciliation result that was
// folded into an earlier poll-timeout attempt for the same spawn. It preserves
// the original dispatch error for classification while letting Drive keep its
// retry counter at the durable attempt number.
type dedupedStageAttemptError struct {
	err     error
	attempt int
}

func (e *dedupedStageAttemptError) Error() string { return e.err.Error() }
func (e *dedupedStageAttemptError) Unwrap() error { return e.err }

// errRunTerminated signals that the run's persisted state was moved to a
// terminal state (done/escalated/paused) by a writer other than this Drive
// goroutine — the manual /escalate handler or the pause kill-switch — while
// this goroutine was mid-stage. Drive treats it as a clean stop so a stale
// in-memory run.State can never clobber the out-of-band terminal row back to a
// non-terminal stage (which would resurrect the run on the next restart's
// resume). See runTerminatedExternally.
var errRunTerminated = errors.New("pipeline: run terminated out-of-band")

func withStageAcceptRecorder(ctx context.Context, fn func(spawnID string) error) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, stageAcceptRecorderKey{}, fn)
}

func stageAcceptRecorderFromContext(ctx context.Context) func(spawnID string) error {
	fn, _ := ctx.Value(stageAcceptRecorderKey{}).(func(string) error)
	return fn
}

func withMergeRecoveryPipelineCreateRecorder(ctx context.Context, fn func() error) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, mergeRecoveryPipelineCreateRecorderKey{}, fn)
}

func withCIWatchFlakeRescueRecorder(ctx context.Context, fn func([]FailedJob) error) context.Context {
	return context.WithValue(ctx, ciWatchFlakeRescueRecorderKey{}, fn)
}
func recordCIWatchFlakeRescue(ctx context.Context, jobs []FailedJob) error {
	fn, _ := ctx.Value(ciWatchFlakeRescueRecorderKey{}).(func([]FailedJob) error)
	if fn == nil {
		return errors.New("ci_watch flake rescue recorder not configured")
	}
	return fn(jobs)
}
func withCIWatchFlakeRescueAttempted(ctx context.Context, attempted bool) context.Context {
	return context.WithValue(ctx, ciWatchFlakeRescueAttemptedKey{}, attempted)
}
func ciWatchFlakeRescueAttemptedFromContext(ctx context.Context) bool {
	attempted, _ := ctx.Value(ciWatchFlakeRescueAttemptedKey{}).(bool)
	return attempted
}
func withCIWatchFlakeRescueFirstJobs(ctx context.Context, jobs []FailedJob) context.Context {
	return context.WithValue(ctx, ciWatchFlakeRescueFirstJobsKey{}, jobs)
}
func ciWatchFlakeRescueFirstJobsFromContext(ctx context.Context) []FailedJob {
	jobs, _ := ctx.Value(ciWatchFlakeRescueFirstJobsKey{}).([]FailedJob)
	return jobs
}

// RecordMergeRecoveryPipelineCreate durably fences the non-idempotent GitLab
// create-pipeline POST before the client sends it. A missing recorder is a
// no-op for direct client use; Runner-dispatched merge stages install one.
func RecordMergeRecoveryPipelineCreate(ctx context.Context) error {
	fn, _ := ctx.Value(mergeRecoveryPipelineCreateRecorderKey{}).(func() error)
	if fn == nil {
		return nil
	}
	return fn()
}

func withMergeRecovery405Recorder(ctx context.Context, fn func(map[string]any)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, mergeRecovery405RecorderKey{}, fn)
}

// RecordMergeRecovery405 reports that the merge stage is about to close+reopen
// an MR to clear a stale-head 405, so the HUD shows what the pipeline did to
// somebody's MR rather than inferring it from a log tail. It is called BEFORE
// the mutation: an operator must be able to find the event even if the process
// dies mid-cycle. A missing recorder is a no-op for direct client use.
func RecordMergeRecovery405(ctx context.Context, detail map[string]any) {
	fn, _ := ctx.Value(mergeRecovery405RecorderKey{}).(func(map[string]any))
	if fn == nil {
		return
	}
	fn(detail)
}

func withMergeRecoveryPipelineCreateAttempted(ctx context.Context, attempted bool) context.Context {
	if !attempted {
		return ctx
	}
	return context.WithValue(ctx, mergeRecoveryPipelineCreateAttemptedKey{}, true)
}

func mergeRecoveryPipelineCreateAttemptedFromContext(ctx context.Context) bool {
	attempted, _ := ctx.Value(mergeRecoveryPipelineCreateAttemptedKey{}).(bool)
	return attempted
}

func withHeadTransitionSeq(ctx context.Context, seq int64) context.Context {
	if seq == 0 {
		return ctx
	}
	return context.WithValue(ctx, headTransitionSeqKey{}, seq)
}

func headTransitionSeqFromContext(ctx context.Context) int64 {
	seq, _ := ctx.Value(headTransitionSeqKey{}).(int64)
	return seq
}

func withResumeSpawnID(ctx context.Context, spawnID string) context.Context {
	if spawnID == "" {
		return ctx
	}
	return context.WithValue(ctx, resumeSpawnIDKey{}, spawnID)
}

func resumeSpawnIDFromContext(ctx context.Context) string {
	spawnID, _ := ctx.Value(resumeSpawnIDKey{}).(string)
	return spawnID
}

// ResumeSpawnIDFromContext exposes the resume spawn id the runner stashes
// on the stage context. Custom WorkerDispatcher implementations (outside
// this package) must read it when building a JobContext, otherwise
// SpawnWorker.Run can't re-attach to an accepted spawn after an operator
// restart and the stage gets stuck in a pending loop against
// ErrStageSpawnConflict.
func ResumeSpawnIDFromContext(ctx context.Context) string {
	return resumeSpawnIDFromContext(ctx)
}

// WithResumeSpawnID is the exported counterpart of ResumeSpawnIDFromContext
// for tests that drive a custom dispatcher without going through Runner.
func WithResumeSpawnID(ctx context.Context, spawnID string) context.Context {
	return withResumeSpawnID(ctx, spawnID)
}

func withStageAttempt(ctx context.Context, attempt int) context.Context {
	if attempt <= 0 {
		return ctx
	}
	return context.WithValue(ctx, stageAttemptKey{}, attempt)
}

func stageAttemptFromContext(ctx context.Context) int {
	attempt, _ := ctx.Value(stageAttemptKey{}).(int)
	return attempt
}

// StageAttemptFromContext exposes the dispatch attempt number the runner
// stashes on the stage context. Custom WorkerDispatcher implementations must
// thread it onto JobContext.Attempt so SpawnWorker derives a per-attempt
// idempotency key — otherwise every attempt of a stage shares one key and a
// retry re-attaches to the previous attempt's terminal spawn instead of
// launching a fresh one.
func StageAttemptFromContext(ctx context.Context) int {
	return stageAttemptFromContext(ctx)
}

// WithStageAttempt is the exported counterpart of StageAttemptFromContext
// for tests that drive a custom dispatcher without going through Runner.
func WithStageAttempt(ctx context.Context, attempt int) context.Context {
	return withStageAttempt(ctx, attempt)
}

func withStageRetryContext(ctx context.Context, rc *StageRetryContext) context.Context {
	if rc == nil {
		return ctx
	}
	return context.WithValue(ctx, stageRetryContextKey{}, rc)
}

// StageRetryContextFromContext exposes the retry context the runner stashes
// on the stage context when it re-dispatches a stage after a gate failure.
// Custom WorkerDispatcher implementations should surface it on
// JobContext.RetryContext (the in-package Dispatcher does) so prompt builders
// can instruct the fresh agent to redo the discarded work.
func StageRetryContextFromContext(ctx context.Context) *StageRetryContext {
	rc, _ := ctx.Value(stageRetryContextKey{}).(*StageRetryContext)
	return rc
}

// WithStageRetryContext is the exported counterpart of
// StageRetryContextFromContext for tests that drive a custom dispatcher
// without going through Runner.
func WithStageRetryContext(ctx context.Context, rc *StageRetryContext) context.Context {
	return withStageRetryContext(ctx, rc)
}

// WorkerDispatcher executes one non-gate stage. Slice 4.2 supplies the
// real implementation (spawn / weaver / devbox / mcp); slice 4.1 ships
// the runner against this interface and tests use a fake.
type WorkerDispatcher interface {
	Dispatch(
		ctx context.Context,
		run *store.PipelineRun,
		item *store.BacklogItem,
		stage Stage,
		prior map[string]StageOutput,
	) (StageOutput, error)
}

// Runner drives one pipeline run end-to-end. It is safe for concurrent
// Drive calls against different runs; a single run is serialised by the
// caller (the reconciler issues one Start per queued item per tick).
type Runner struct {
	Store *store.Store
	// IncidentWriter persists classified external-dependency incidents. New
	// wires the production store; nil keeps direct test constructors and
	// store-less integrations backward compatible. Writes are best-effort.
	IncidentWriter interface {
		Put(context.Context, *store.IncidentRecord) (bool, error)
	}
	Gates      *gates.Registry
	Dispatcher WorkerDispatcher
	Policy     *mills.PolicyManager
	Stages     []Stage
	Clock      func() time.Time
	Logger     *slog.Logger
	// Escalator, when set, is invoked after the runner transitions a
	// run to PipelineEscalated. Failure-record + issue + handoff
	// publication is best-effort: an Escalator error is logged but does
	// not undo the state transition.
	Escalator EscalationHandler
	// OnMerged, when set, is invoked synchronously after a run reaches
	// PipelineDone (the merge stage + cleanup completed). Slice 4.7
	// wires this to eval.OutcomeAttributor.OnMerged so each merge
	// produces exactly one pipeline_outcome eval row. Errors are logged
	// but do not undo the state transition.
	OnMerged func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error
	// OnAutoRetry (Slice 3d) is invoked after a transient-cap
	// escalation is converted into an auto-retry (run escalated, item
	// kept queued). The operator wires this to scheduler.KickNow so
	// the new pipeline_run dispatches within ~1s instead of waiting
	// for the next scheduled reconciler tick.
	OnAutoRetry func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error
	// OnEscalated, when set, is invoked synchronously after a run
	// transitions to PipelineEscalated via the real escalation path
	// (auto-retried transient escalations do NOT fire it). The operator
	// wires this to squads.OutcomeRecorder.OnEscalated so squad
	// confidence reflects failures, not just merges. Errors are logged
	// but never undo the state transition.
	OnEscalated func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error
	// MemoryConsolidator, when set AND LOOM_MILLS_MEMORY_CONSOLIDATE is on,
	// distils the oldest entries of an item's memory journal at record time
	// once the snapshot passes the soft threshold (see item_memory.go). Nil —
	// the default, and every test that does not opt in — means the journal is
	// only ever bounded by the hard row cap, exactly as before. A consolidator
	// failure never fails a stage and never loses an entry.
	MemoryConsolidator journalengine.Consolidator
	// SliceHydrator, when set, lets the runner materialize file-bearing
	// slice scope from the plan store onto a slice-less, plan-linked item
	// right after the plan_slice stage completes (see hydrateSliceScope).
	// Nil disables hydration (tests, hub-less operators) and the scope
	// gate's slice-less advisory skip applies instead.
	SliceHydrator PlanSliceHydrator
	// AutonomyGate, when set, must allow each stage before Drive continues.
	// This is the in-run counterpart to reconciler.AutonomyGate: it stops a
	// run that was already accepted before MR/CI/merge continuation proceeds
	// under a newly-blocked operator.
	AutonomyGate AutonomyGateFunc
	// RescueMR, when set, opens a Draft merge request over an escalated run's
	// branch. Wired only for the scope-gate escalation today (S2 of the
	// 2026-07-26 scope-gate reliability plan): the implement stage pushes a
	// complete diff, but a run that dies at post_implement_gate never reaches
	// the `mr` stage, so the work sits invisible on an un-MR'd branch until a
	// human goes looking (the hand-authored rescue MR !1249 is the workaround
	// this automates). Nil — every test, and any operator without a GitLab
	// client — simply skips the rescue; the escalation is unchanged. The hook
	// must never arm auto-merge.
	RescueMR func(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, req CreateMRRequest) (CreateMRResponse, error)
	// HealthGates, when configured, is evaluated before any pipeline
	// stage or cross-repo integration work starts. A blocked verdict
	// escalates the run without dispatching workers.
	HealthGates HealthGatePreflight
	// DegradedPolicy, when configured, evaluates structured degraded-mode
	// dependency evidence before each stage. A degraded allow proceeds with an
	// audit event; a blocked verdict escalates before dispatching workers.
	DegradedPolicy DegradedPolicyFunc
	// active is the process-local run-ID guard shared by direct Runner
	// drives and RunnerStarter fan-out drives. A run remains registered
	// through its entire Integrator -> post-run lifecycle.
	active      sync.Map
	activeCount atomic.Int64
	// wg tracks the detached Drive goroutines launched by Start (and the
	// fan-out goroutines launched by RunnerStarter, which register here
	// too). Wait blocks until they all exit — used by tests to avoid
	// leaking goroutines past the store's teardown, and available to
	// operators for a clean shutdown.
	wg sync.WaitGroup
	// CrossRepoIntegrator, when set, switches the runner into the
	// cross-repo path for any backlog item that has an open
	// cross_repo_run row. Unset means single-repo behaviour for every
	// item; an open cross_repo_run with no integrator wired returns a
	// clear error rather than silently routing through the single-repo
	// flow. See slice 4.2/4.3 in
	// .loom/94-implementation-plan-mills-v2-…2026-05-02.md.
	CrossRepoIntegrator CrossRepoIntegrator
}

// CrossRepoIntegrator is the subset of crossrepo.Integrator the pipeline
// runner depends on. Defined here so the runner stays agnostic to the
// concrete crossrepo package and tests can supply a fake without pulling
// in the GitLab/policy wiring.
type CrossRepoIntegrator interface {
	WaitForGreen(ctx context.Context, run *store.CrossRepoRun) (store.CrossRepoState, error)
	AtomicMerge(ctx context.Context, run *store.CrossRepoRun) (store.CrossRepoState, error)
}

// New constructs a Runner with sensible defaults. A nil PolicyManager is
// treated as "no policy snapshot" — gate retries default to 3 attempts.
func New(s *store.Store, gr *gates.Registry, d WorkerDispatcher, pm *mills.PolicyManager) *Runner {
	r := &Runner{
		Store:      s,
		Gates:      gr,
		Dispatcher: d,
		Policy:     pm,
		Stages:     DefaultStages,
		Clock:      time.Now,
		Logger:     slog.Default(),
	}
	if s != nil {
		r.IncidentWriter = s.Incidents
	}
	return r
}

// Start satisfies mills.PipelineStarter. It validates inputs, kicks off
// Drive in a goroutine, and returns nil on accept. The reconciler relies
// on the contract that progress is reported via stage_results + events.
func (r *Runner) Start(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if r == nil || r.Store == nil || r.Dispatcher == nil {
		return errors.New("pipeline: runner not configured")
	}
	if run == nil || run.ID == "" {
		return errors.New("pipeline: run.ID required")
	}
	if item == nil || item.ID == "" {
		return errors.New("pipeline: item.ID required")
	}
	if !r.activate(run.ID) {
		return nil
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.deactivate(run.ID)
		// Drive uses a detached context; a reconciler tick that returns
		// must not cancel an in-flight run.
		bg := context.Background()
		if err := r.Drive(bg, run, item); err != nil {
			if state, terminal := r.runTerminalResolution(bg, run); terminal {
				r.logger().Info("pipeline drive error ignored; run already terminal",
					"run", run.ID, "state", state, "error", err)
				return
			}
			r.logger().Error("pipeline drive failed", "run", run.ID, "error", err)
			if eerr := r.escalateWithItem(bg, run, item, Classify(err), fmt.Sprintf("pipeline drive failed: %v", err)); eerr != nil {
				if state, terminal := r.runTerminalResolution(bg, run); terminal {
					r.logger().Info("pipeline drive failure escalation skipped; run already terminal",
						"run", run.ID, "state", state, "error", eerr)
					return
				}
				r.logger().Error("pipeline drive failure escalation failed", "run", run.ID, "error", eerr)
			}
		}
	}()
	return nil
}

// activate claims a run ID for this operator process. RunnerStarter shares
// this guard so duplicate reconciler deliveries cannot start a fan-out while
// a direct or fan-out drive for the same run is already active.
func (r *Runner) activate(runID string) bool {
	if _, loaded := r.active.LoadOrStore(runID, struct{}{}); loaded {
		// The reconciler intentionally redelivers active rows every tick while a
		// long-running spawn is owned by this process. That is routine dedup, not
		// an operator warning; keeping it at WARN produced one false incident line
		// per run per minute during the spawn-stall drills.
		r.logger().Debug("pipeline start skipped; run already active in this operator", "run", runID)
		return false
	}
	r.activeCount.Add(1)
	return true
}

func (r *Runner) deactivate(runID string) {
	r.active.Delete(runID)
	r.activeCount.Add(-1)
}

// ActiveOperations reports pipeline drives still executing, including
// post-terminal attribution/audit hooks after the durable run row is done.
func (r *Runner) ActiveOperations() int64 {
	if r == nil {
		return 0
	}
	return r.activeCount.Load()
}

// Wait blocks until every detached Drive goroutine launched by Start (and
// the fan-out goroutines launched by RunnerStarter) has exited. It is the
// deterministic stop barrier tests use before closing the store so a
// still-running drive loop can't write to a torn-down DB or race shared
// state. Safe to call when nothing is in flight (returns immediately) and
// safe to call repeatedly.
func (r *Runner) Wait() {
	if r == nil {
		return
	}
	r.wg.Wait()
}

// Drive runs the state machine synchronously to a terminal state. It is
// the test entry point and the unit under test for slice 4.1.
//
// Drive is resume-safe: if run.CurrentStage is set and matches a stage
// in r.Stages, execution picks up at that index. New runs (CurrentStage
// empty) start at index 0.
//
// Cross-repo branch (slice 4.2/4.3): when the backlog item has an open
// cross_repo_run row the Runner hands off to handleCrossRepoRun instead
// of stepping through r.Stages. The detection is "open run exists" —
// the planner's caller is responsible for materialising that row before
// dispatching the pipeline. See .loom/94-…2026-05-02.md slice 4.2.
func (r *Runner) Drive(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if r.Store == nil || r.Dispatcher == nil {
		return errors.New("pipeline: runner not configured")
	}
	if blocked, err := r.runPreflight(ctx, run, item); blocked || err != nil {
		return err
	}
	if cross, err := r.openCrossRepoRun(ctx, item); err != nil {
		return err
	} else if cross != nil {
		return r.handleCrossRepoRun(ctx, cross, run, item)
	}
	startIdx, err := r.resumeIndex(run)
	if err != nil {
		return err
	}
	prior, err := r.loadPriorOutputs(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("pipeline: load prior outputs: %w", err)
	}

	policy := r.policy()
	maxAttempts := policy.Pipeline.Retry.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	// transientRetryCap is the *extra* free retries allowed for
	// Transient + TransientQuota error classes on top of MaxAttempts.
	// Hard cap on total attempts is maxAttempts + transientRetryCap so
	// a permanent transient (e.g. flexinfer down for hours) escalates
	// instead of looping forever.
	transientRetryCap := policy.Pipeline.Retry.TransientRetryCap
	if transientRetryCap <= 0 {
		transientRetryCap = 5
	}

	// attempts tracks per-stage attempt count for the live Drive call.
	// On resume we seed it from the persisted stage_results so retry
	// caps survive operator restarts.
	attempts, err := r.seedAttempts(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("pipeline: seed attempts: %w", err)
	}
	// effectiveAttempts counts only "real" failures (Code + Infra) toward
	// the MaxAttempts budget. Free transient retries bump `attempts`
	// (for stage_results.attempt monotonicity) but not this counter.
	// Reset across resumes by design: simpler than persisting error
	// class per attempt, and operator restarts are rare enough that
	// re-burning a free retry budget after a restart is acceptable
	// (Slice 2c trade-off, documented in error_class.go).
	effectiveAttempts := map[string]int{}

	// retryCtxs tracks, per RetryFrom stage, why that stage is being
	// re-dispatched after a gate failure. Seeded from the persisted
	// gate_outcomes so a Drive that resumes mid-retry (operator restart)
	// still tells the fresh spawn it is a retry.
	retryCtxs, err := r.seedRetryContexts(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("pipeline: seed retry contexts: %w", err)
	}

	// judgeUnparseableRetries counts, per auto_gate stage, how many FREE
	// re-judges we have spent recovering an ungradeable LLM-judge score
	// envelope (raw=""). These re-run only the gate (a fresh judge call that
	// re-invokes the client's larger-budget recovery) WITHOUT respawning the
	// upstream RetryFrom stage, so a model-provider hiccup on finished work
	// never burns the code-class attempt budget (issue #348).
	judgeUnparseableRetries := map[string]int{}

	// judgeTransportRetries counts, per auto_gate stage, how many FREE
	// re-judges we have spent on a gate that ERRORED (no verdict at all:
	// litellm 400, provider 429, timeout, 5xx). Same budget shape as
	// judgeUnparseableRetries — gate-only re-runs, no respawn, no attempt
	// spend — because a judge whose transport failed said nothing about the
	// diff (issue #378).
	judgeTransportRetries := map[string]int{}

	for i := startIdx; i < len(r.Stages); i++ {
		stage := r.Stages[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		// Bail before touching a run an out-of-band actor already terminated
		// (manual /escalate, pause kill-switch, or a competing goroutine that
		// reached a terminal stage). Without this, this goroutine's stale
		// run.State would clobber the terminal row back to a non-terminal
		// stage and the next operator restart's resume would re-activate a run
		// it was meant to skip — the 2026-07-01 "escalate isn't durable" wedge.
		if terminal, err := r.runTerminatedExternally(ctx, run); err != nil {
			return err
		} else if terminal {
			return nil
		}
		if allowed, err := r.enforceAutonomy(ctx, run, item, stage); err != nil {
			return err
		} else if !allowed {
			return nil
		}
		if allowed, err := r.enforceDegradedPolicy(ctx, run, item, stage); err != nil {
			return err
		} else if !allowed {
			return nil
		}

		if stage.Type == "auto_gate" {
			verdict, err := r.runGate(ctx, run, item, stage, prior, policy)
			if err != nil {
				rejudge, gerr := r.handleGateError(ctx, run, item, stage, err, judgeTransportRetries)
				if !rejudge {
					return gerr
				}
				i-- // re-run this same auto_gate stage (fresh judge call, no respawn)
				continue
			}
			failDetail := verdict.FailDetail
			if verdict.Pass {
				continue
			}
			if verdict.JudgeUnparseable {
				// Every failing gate here is an LLM judge that RAN but returned
				// a response we could not parse into a score envelope (raw="").
				// This is an EXTERNAL model-provider dependency failure — a
				// reasoning-model budget squeeze (issue #348) that survived the
				// client's own boosted-retry recovery — not a code defect in the
				// diff. Respawning the (already-successful, $-costly) RetryFrom
				// stage cannot change the model's output; it only burns the
				// code-class attempt budget on finished work ($2 in #348).
				//
				// Give it a bounded FREE re-judge: re-run ONLY the gate (a fresh
				// judge call that re-invokes the client's larger-budget recovery),
				// no agent respawn, no attempt-budget spend. If the verdict is
				// still ungradeable after the cap, escalate as an external
				// dependency incident (class=config, retryable=false) rather than
				// as code, so the operator waits for the provider / raises the
				// judge token budget instead of chasing a phantom code bug.
				n := judgeUnparseableRetries[stage.ID] + 1
				if n <= maxJudgeUnparseableRetries {
					judgeUnparseableRetries[stage.ID] = n
					r.logger().Info("pipeline gate: judge envelope ungradeable; free re-judge without respawn",
						"run", run.ID, "gate", stage.ID, "attempt", n, "cap", maxJudgeUnparseableRetries, "failure", failDetail)
					r.event(ctx, "pipeline.gate.judge_rejudge", "warn", map[string]any{
						"run": run.ID, "gate": stage.ID, "attempt": n, "failure": failDetail,
					})
					i-- // re-run this same auto_gate stage (fresh judge call, no respawn)
					continue
				}
				incident, _ := mcperror.ClassifyExternalIncident(failDetail)
				return r.escalateWithItem(ctx, run, item, ClassConfig, fmt.Sprintf(
					"gate %s failed (%s) [class=%s]: %s (%s): the LLM rubric judge returned an ungradeable score envelope after %d free recovery re-judges — an external model-provider dependency failure, not a code defect. Wait for %s to recover, or raise FLEXINFER_JUDGE_MAX_TOKENS for reasoning models, then requeue",
					stage.ID, failDetail, ClassConfig, incident.ID, incident.Summary, maxJudgeUnparseableRetries, incident.Dependency))
			}
			if verdict.Terminal {
				// A terminal gate verdict is a function of the item's own
				// state (e.g. slice-less scope), not of the worker's diff —
				// re-running RetryFrom cannot flip it. Escalate on first
				// sight instead of burning the attempt budget (escalations
				// #272–#278 each spent 3 implement attempts + ~$0.60 on the
				// same deterministic fail). class=config: fix the item, not
				// the code.
				return r.escalateWithItem(ctx, run, item, ClassConfig, fmt.Sprintf("gate %s failed terminally (not retried) [class=%s]: %s — fix the backlog item (e.g. re-decompose it with slices), then re-dispatch", stage.ID, ClassConfig, failDetail))
			}
			// Scope auto-amendment (S1). Runs BEFORE the rewind: when the only
			// failing gate is `scope` and every violating file is a
			// sibling-directory reach policy admits, widen the item's declared
			// scope and CONTINUE on the diff we already have. Retrying could
			// never converge on those files — the implementer needs them, so a
			// fresh spawn reaches for them again (token-sweep re-edited the same
			// two files on every attempt, ~$1.7–5 of spawn per loop).
			scopeOnly := verdict.scopeOnlyFailure() && stage.ID == postImplementGateStage
			var scopeDecision *gates.AmendmentDecision
			if scopeOnly {
				amended, decision := r.maybeAmendScope(ctx, run, item, stage, policy, verdict)
				scopeDecision = decision
				if amended {
					continue
				}
			}
			// Gate failure: rewind to RetryFrom and retry, bumping
			// the upstream stage's attempt counter. Cap at maxAttempts.
			rewindIdx, ok := r.indexOf(stage.RetryFrom)
			if !ok || stage.RetryFrom == "" {
				return r.escalateWithItem(ctx, run, item, ClassConfig, fmt.Sprintf("gate %s failed (%s) and no RetryFrom defined", stage.ID, failDetail))
			}
			// Record why the upstream stage is being retried so the next
			// dispatch (and the escalation, if the cap is hit) can name
			// the FIRST failing gate, not just the latest knock-on.
			rc := retryCtxs[stage.RetryFrom]
			if rc == nil {
				rc = &StageRetryContext{GateStage: stage.ID, FirstFailure: failDetail}
				retryCtxs[stage.RetryFrom] = rc
			}
			rc.LastFailure = failDetail
			// A non-admissible scope failure gets ONE self-correction respawn,
			// not the full maxAttempts budget: the amendment evaluator has
			// already established the reach is NOT a too-narrow envelope, so
			// attempts 2 and 3 are re-litigating a verdict that does not move
			// (the 2026-07-26 cohort burned all three on identical diffs).
			// Scoped to this gate only — every other gate keeps maxAttempts.
			attemptCap := maxAttempts
			if scopeOnly && maxScopeRetryAttempts < attemptCap {
				attemptCap = maxScopeRetryAttempts
			}
			if attempts[stage.RetryFrom]+1 > attemptCap {
				// The diff kept failing this gate across the whole attempt
				// budget: a code fault, EXCEPT for the scope gate, where the
				// item's declared envelope is what is wrong. Naming it here
				// stops the needle fallback from inferring a class out of
				// failDetail, which is arbitrary gate/agent-authored text (a
				// test gate reporting "connection reset by peer" would
				// otherwise read infra and become auto-requeue-eligible).
				gateExhaustedClass := ClassCode
				reason := fmt.Sprintf("gate %s failed (%s); %s exceeded %d attempts", stage.ID, failDetail, stage.RetryFrom, attemptCap)
				if scopeOnly {
					gateExhaustedClass = ClassConfig
					// S2: the scope cap escalation is a CONFIG fault (fix the
					// item's slice list, not the code). Before this marker it
					// carried no "[class=…]" at all, so escalationClassLabel
					// read "unclassified" and the auto-requeue sweep skipped it.
					reason = r.scopeEscalationReason(ctx, run, item, stage, failDetail, attemptCap, scopeDecision)
				}
				if rc.FirstFailure != "" && rc.FirstFailure != failDetail {
					reason += fmt.Sprintf("; first failure (gate %s): %s", rc.GateStage, rc.FirstFailure)
				}
				return r.escalateWithItem(ctx, run, item, gateExhaustedClass, reason)
			}
			r.logger().Info("pipeline retry", "run", run.ID, "from", stage.RetryFrom, "attempt", attempts[stage.RetryFrom]+1, "gate", stage.ID, "failure", failDetail)
			i = rewindIdx - 1 // -1 so the for-loop ++ lands on rewindIdx
			continue
		}

		// Non-gate stage: dispatch the worker.
		attempt := attempts[stage.ID] + 1
		pending, err := r.pendingStage(ctx, run.ID, stage.ID)
		if err != nil {
			return fmt.Errorf("pipeline: load pending stage: %w", err)
		}
		if pending != nil {
			attempt = pending.Attempt
		}
		attempts[stage.ID] = attempt
		dispatchCtx := ctx
		if rc := retryCtxs[stage.ID]; rc != nil {
			rc.Attempt = attempt
			dispatchCtx = withStageRetryContext(ctx, rc)
		}
		out, err := r.runStage(dispatchCtx, run, item, stage, prior, attempt, pending)
		if err != nil {
			if errors.Is(err, errStagePending) {
				r.logger().Info("pipeline drive stopped; stage remains pending", "run", run.ID, "stage", stage.ID, "attempt", attempt)
				return nil
			}
			if errors.Is(err, errRunTerminated) {
				r.logger().Info("pipeline drive stopped; run terminated out-of-band", "run", run.ID, "stage", stage.ID, "attempt", attempt)
				return nil
			}
			if errors.Is(err, store.ErrStageSpawnConflict) {
				r.logger().Info("pipeline drive stopped; stage attempt already has an accepted spawn", "run", run.ID, "stage", stage.ID, "attempt", attempt)
				return nil
			}
			var deduped *dedupedStageAttemptError
			if errors.As(err, &deduped) {
				// The reconciliation result completed an existing poll-timeout
				// attempt, rather than creating a second attempt for the same
				// spawn. Count subsequent retries from that durable attempt.
				attempts[stage.ID] = deduped.attempt
			}

			// The merge stage's exact-identity MR read found the head moved
			// off the CI-authorized SHA. That is a durable state change, not a
			// flake: record it in the head-transition ledger and either rewind
			// the run so every source-sensitive gate and CI re-run for the
			// successor, or escalate when the run has exhausted its transition
			// budget (#374). Checked before Classify so a head movement never
			// burns the code-class attempt budget on a merge that cannot
			// succeed by retrying.
			var headMoved *MergeSourceSHAMismatchError
			if errors.As(err, &headMoved) && r.Store.MRHeadTransitions != nil {
				rewound, herr := r.recordExternalHeadMovement(ctx, run, item, stage, headMoved)
				if herr != nil {
					return herr
				}
				if rewound {
					return nil
				}
				// Budget exhausted: recordExternalHeadMovement already
				// escalated, so the run is terminal.
				return nil
			}

			// Slice 2c: classify the failure so transient flakes
			// (k8s pod GC, MCP transport drop, flexinfer timeout)
			// don't burn the MaxAttempts budget meant for real-code
			// failures. Kill-test (2026-05-24) showed ~62% of
			// failing stage_results were transient.
			cls := Classify(err)
			incident, externalIncident := classifyExternalStageIncident(err, out)
			if externalIncident {
				cls = ClassConfig
				r.recordExternalIncident(ctx, incident)
			}
			mills.PipelineStageErrorClassTotal.WithLabelValues(stage.ID, string(cls)).Inc()
			if !IsFreeRetry(cls) {
				effectiveAttempts[stage.ID]++
			}

			if externalIncident {
				return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s known external dependency incident (not retried) [class=%s]: %s (%s): %v; wait for %s to recover, then requeue", stage.ID, cls, incident.ID, incident.Summary, err, incident.Dependency))
			}

			// Git-clone init/build failures carry a specific, non-mergeable
			// remediation (create the repo, fix the branch, fix the spawn git
			// token) that the generic 405/422 merge advice below would bury.
			// Escalate a TERMINAL clone failure on first sight with that
			// guidance — the class is already ClassConfig via Classify, so the
			// [class=config] marker gives the escalation metadata
			// escalation_class=config / retryable=false, and a missing repo
			// won't appear on retry. A DNS/network clone blip classifies
			// transient (IsTerminal=false) and falls through to the retry path.
			if gc, ok := ClassifyGitCloneError(err.Error()); ok && IsTerminal(gc.Class) {
				return r.escalateWithItem(ctx, run, item, gc.Class, fmt.Sprintf("stage %s terminal git-clone error (not retried) [class=%s]: %s", stage.ID, gc.Class, gc.Message))
			}

			// An MR that never reported a head SHA carries its own remediation
			// (rebase the conflicted branch, repush a deleted one, reopen a
			// closed MR) that the generic 405/422 merge advice below would
			// bury. Escalate on first sight with that guidance: the head-SHA
			// window is already bounded in the client, and re-watching a
			// headless MR only replays "head sha pending" (2026-07-26).
			if errors.Is(err, ErrMRHeadSHAUnavailable) {
				return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s terminal MR-state error (not retried) [class=%s]: %v — the MR has no head sha, so no branch pipeline can exist; rebase or repush the source branch (or reopen the MR), then requeue", stage.ID, cls, err))
			}

			// The head SHA exists but the project never built a push pipeline
			// for it. The remediation is CI configuration, not a rebase, so it
			// gets its own guidance — and re-watching only replays "branch
			// pipeline pending" for another bounded window.
			if errors.Is(err, ErrBranchPipelineUnavailable) {
				return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s terminal CI-config error (not retried) [class=%s]: %v — the MR head has no push pipeline; check the project's workflow rules (a repo that only builds merge_request_event pipelines never produces one) and that CI is enabled, or repush the branch, then requeue", stage.ID, cls, err))
			}

			// Terminal config errors escalate on first sight — an
			// identical retry can only return the identical error
			// (merge 405 burned 3 attempts per run; escalations
			// #148/#150).
			if IsTerminal(cls) {
				return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s terminal config error (not retried) [class=%s]: %v — for a 405 check the project's merge method, merge-when-pipeline-succeeds availability, and approval rules; for a 422 (\"cannot be merged\") rebase the source branch onto the target and requeue", stage.ID, cls, err))
			}

			// A ci_watch whose pipeline reached a terminal non-success
			// state is deterministic per-stage: a retry re-polls the SAME
			// dead pipeline and returns the identical error within seconds
			// (escalation #292, 2026-07-08: attempts 2 and 3 failed in 2.1s
			// and 1.3s). Escalate on first sight — the class stays as
			// classified (code: the diff broke CI) and a human/requeue can
			// still recover after retrying the pipeline in GitLab.
			if errors.Is(err, ErrCIPipelineTerminal) {
				if cls == ClassTransient {
					return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s ended in a retryable CI runner-system failure [class=%s]: %v — auto-requeue may retry the item within its existing budget", stage.ID, cls, err))
				}
				return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s errored deterministically (not retried) [class=%s]: %v — the MR pipeline reached a terminal non-success state; re-watching cannot change it. Fix the branch (or retry the pipeline in GitLab) and requeue", stage.ID, cls, err))
			}

			// A ci_watch run whose pipeline was STILL RUNNING at the watch hard
			// cap is an external CI-dependency stall, not a code failure:
			// re-watching just re-hits the same cap. Escalate once as a RETRYABLE
			// external-dependency incident keyed on the stuck pipeline URL (a
			// later requeue can still succeed once CI drains) instead of burning
			// the attempt budget or blaming the diff (S3). Checked via errors.As
			// so the concrete pipeline URL survives to the escalation metadata.
			var stall *CIWatchStalledError
			if errors.As(err, &stall) {
				return r.escalateCIWatchStall(ctx, run, item, stage.ID, stall)
			}

			// A distinct spawn-infrastructure reason (agent-CLI timeout /
			// stdin-misconfig) names the defect at the spawn/cluster layer so
			// the escalation stops reading as a code bug (escalations #351,
			// #356-#359). Empty for every other failure.
			reasonSuffix := ""
			if reason, ok := SpawnInfraReason(err); ok {
				reasonSuffix = fmt.Sprintf(" [reason=%s]", reason)
			}

			// Hard cap on total attempts (free + budgeted) so a
			// permanent transient can't loop forever.
			if attempts[stage.ID] >= maxAttempts+transientRetryCap {
				return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s errored after %d total attempts (cap %d) [class=%s]%s: %v", stage.ID, attempts[stage.ID], maxAttempts+transientRetryCap, cls, reasonSuffix, err))
			}
			// Budget cap on real (Code + Infra) failures.
			if effectiveAttempts[stage.ID] >= maxAttempts {
				return r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf("stage %s errored after %d attempts [class=%s]%s: %v", stage.ID, effectiveAttempts[stage.ID], cls, reasonSuffix, err))
			}

			// Backoff for quota errors so we don't immediately
			// re-hit the rate limit (seconds scale) or the saturated
			// spawn pool (minutes scale — see saturationBackoff).
			if backoff := retryBackoff(cls, err, attempts[stage.ID]); backoff > 0 {
				r.logger().Info("pipeline retry backoff", "run", run.ID, "stage", stage.ID, "class", cls, "backoff", backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return ctx.Err()
				}
			} else {
				r.logger().Info("pipeline retry", "run", run.ID, "stage", stage.ID, "class", cls, "attempt", attempts[stage.ID], "effective_attempts", effectiveAttempts[stage.ID])
			}

			// Retry the same stage by stepping back one (loop will ++).
			i--
			continue
		}
		if prev, ok := prior[stage.ID]; ok {
			var carried bool
			if out, carried = carryForwardDiff(prev, out); carried {
				r.logger().Info("pipeline retry reported no new work; carrying prior attempt's diff forward for gating",
					"run", run.ID, "stage", stage.ID, "attempt", attempts[stage.ID])
				r.event(ctx, "pipeline.stage.diff_carried_forward", "ok", map[string]any{
					"run": run.ID, "stage": stage.ID, "attempt": attempts[stage.ID],
				})
			}
		}
		prior[stage.ID] = out
		if stage.ID == "plan_slice" {
			// The decomposition the plan_slice stage just authored lives in
			// the plan store; a slice-less item picks its file scope up here
			// so post_implement_gate enforces a real envelope instead of
			// recording an advisory skip (escalations #332/#338: both items
			// reached the gate slice-less despite a successful plan_slice).
			r.hydrateSliceScope(ctx, run, item)
		}
	}

	return r.markDone(ctx, run, item)
}

// resumeIndex returns the stage index to start (or restart) at. A run
// with no CurrentStage starts at 0; a run mid-flight resumes at the
// stage that was in flight when the operator stopped.
func (r *Runner) resumeIndex(run *store.PipelineRun) (int, error) {
	if run.CurrentStage == "" {
		return 0, nil
	}
	if i, ok := r.indexOf(run.CurrentStage); ok {
		return i, nil
	}
	return 0, fmt.Errorf("pipeline: run %s current_stage %q not in DAG", run.ID, run.CurrentStage)
}

// indexOf returns the position of stage id in r.Stages, or (0, false).
func (r *Runner) indexOf(id string) (int, bool) {
	for i, s := range r.Stages {
		if s.ID == id {
			return i, true
		}
	}
	return 0, false
}

// hasDiffEvidence reports whether a stage output carries any observable
// implement work — file paths parsed from spawn telemetry or a captured git
// diff. Mirrors the gates.NonEmptyDiff condition.
func hasDiffEvidence(out StageOutput) bool {
	return len(out.FilesChanged) > 0 || len(out.DiffPatch) > 0
}

// carryForwardDiff bridges the implement-retry empty-diff wedge (escalations
// #218/#221–#224/#226/#228/#231/#232): attempt 1 pushes real commits, a gate
// fails, and the fresh-clone retry finds the branch already up to date — so
// its per-attempt telemetry reports zero files and the run escalates on
// nonempty_diff even though the finished work is sitting on the branch. The
// spawn telemetry only ever describes what THAT agent session touched, not
// the branch-vs-base state (the cumulative git capture in
// clients.attachGitContext is skipped whenever WorkingDir/BaseBranch are
// unset — the deployed default until SpawnWorker gained the RepoRoot/
// BaseBranch fallbacks). The carry-forward remains the safety net for
// deployments where the capture is unavailable (no repo root, git fetch
// failing), and for the same-attempt case the capture can't reach.
//
// When a retry reports no work of its own and the previous successful
// attempt did, copy the previous attempt's diff evidence onto the retry
// output so gates judge the work that is actually on the branch. Retry
// bookkeeping (SpawnID, CostUSD, LogTail, Artifacts) is kept from the retry.
// A run whose attempts ALL report no work carries nothing, so the
// nonempty_diff empty-MR guard still fires.
func carryForwardDiff(prev, next StageOutput) (StageOutput, bool) {
	if hasDiffEvidence(next) || !hasDiffEvidence(prev) {
		return next, false
	}
	next.FilesChanged = prev.FilesChanged
	next.LinesAdded = prev.LinesAdded
	next.LinesRemoved = prev.LinesRemoved
	next.DiffPatch = prev.DiffPatch
	next.CommitMessages = prev.CommitMessages
	return next, true
}

// loadPriorOutputs rehydrates the most-recent successful output per stage
// from stage_results. Used on resume so downstream stages still see their
// inputs after an operator restart.
func (r *Runner) loadPriorOutputs(ctx context.Context, runID string) (map[string]StageOutput, error) {
	rows, err := r.Store.Pipeline.ListStages(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]StageOutput, len(rows))
	for _, sr := range rows {
		if sr.Outcome == nil || *sr.Outcome != store.StageOutcomeSuccess {
			continue
		}
		so := StageOutput{
			CostUSD:   sr.CostUSD,
			SpawnID:   sr.SpawnID,
			LogTail:   sr.LogTail,
			Artifacts: sr.Artifacts,
		}
		// The dispatcher may have stashed structured fields under
		// well-known keys. Surface the ones gates care about.
		if sr.Artifacts != nil {
			if v, ok := sr.Artifacts["files_changed"].([]any); ok {
				for _, f := range v {
					if s, ok := f.(string); ok {
						so.FilesChanged = append(so.FilesChanged, s)
					}
				}
			}
			if v, ok := sr.Artifacts["diff_patch"].(string); ok {
				so.DiffPatch = []byte(v)
			}
			if v, ok := sr.Artifacts["lines_added"].(float64); ok {
				so.LinesAdded = int(v)
			}
			if v, ok := sr.Artifacts["lines_removed"].(float64); ok {
				so.LinesRemoved = int(v)
			}
			if v, ok := sr.Artifacts["mr_iid"].(float64); ok {
				so.MRIID = int64(v)
			}
		}
		// Later rows overwrite earlier ones, but a persisted do-nothing
		// retry (empty artifacts) must not clobber an earlier attempt's
		// recorded diff — otherwise a Drive resumed after an operator
		// restart re-enters the empty-diff wedge the in-Drive
		// carry-forward fixes.
		if prev, ok := out[sr.Stage]; ok {
			so, _ = carryForwardDiff(prev, so)
		}
		out[sr.Stage] = so
	}
	return out, nil
}

// seedAttempts loads the persisted attempt count for every stage of a
// run so retry caps survive operator restarts.
func (r *Runner) seedAttempts(ctx context.Context, runID string) (map[string]int, error) {
	out := make(map[string]int)
	if r.Store == nil || runID == "" {
		return out, nil
	}
	rows, err := r.Store.Pipeline.ListStages(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, sr := range rows {
		if sr.Outcome == nil {
			continue
		}
		if sr.Attempt > out[sr.Stage] {
			out[sr.Stage] = sr.Attempt
		}
	}
	return out, nil
}

// ----- MR head transitions (#374) -----

const (
	// headTransitionRewindStage is the first source-sensitive stage. A settled
	// head movement rewinds here, so every gate that reads branch content
	// (nonempty_diff, diff_size, scope, path_policy, secret_scan,
	// commit_format, docs_guardrail) and the whole tests → review → mr →
	// ci_watch chain re-runs for the successor SHA. plan_slice / research /
	// implement are deliberately NOT re-run: the code is the same work
	// replayed onto a new base, and loadPriorOutputs still supplies the
	// implement stage's files_changed / diff_patch / commit_messages so the
	// diff gates have their inputs.
	headTransitionRewindStage = "post_implement_gate"
	// headTransitionDefaultBudget is how many settled head MOVEMENTS a single
	// run may absorb before escalating instead of rewinding. One rebase per
	// run, then a human looks — this is what bounds rebase↔push ping-pong.
	headTransitionDefaultBudget = 1
	// headTransitionBudgetEnv overrides that budget.
	headTransitionBudgetEnv = "LOOM_MILLS_MERGE_MAX_HEAD_TRANSITIONS"
)

// maxHeadTransitions resolves the per-run settled-movement budget. The stage
// env map is checked first (per-run threading / test injection), then the
// process env, mirroring ciWatchMaxMinutes. A non-positive or unparseable
// value falls back to the default; the budget is never unbounded.
func maxHeadTransitions(env map[string]string) int {
	raw := ""
	if env != nil {
		raw = strings.TrimSpace(env[headTransitionBudgetEnv])
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv(headTransitionBudgetEnv))
	}
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return headTransitionDefaultBudget
}

// headTransitionSeq is the durable CI re-authorization fence for a run: the
// highest settled head-MOVEMENT seq in the ledger. Threaded onto JobContext
// exactly like the merge-recovery create fence, so ci_watch stamps the value
// it authorized under and merge can compare.
func (r *Runner) headTransitionSeq(ctx context.Context, runID string) (int64, error) {
	if r.Store == nil || r.Store.MRHeadTransitions == nil || runID == "" {
		return 0, nil
	}
	return r.Store.MRHeadTransitions.MaxSettledSeq(ctx, runID)
}

// recordExternalHeadMovement mints and settles the durable ledger row for a
// head movement Mills did not request, then decides whether the run can
// re-gate the successor or must stop.
//
// The movement is settled 'ambiguous' by construction: an unrequested push
// carries no evidence tying it to anything Mills did, and #374's whole point
// is that such evidence would not license reusing the CI verdict anyway. What
// the row buys is durability — the escalation names both SHAs, the fence
// invalidates the stale authorization even across a restart, and the budget
// bounds how many times one run will chase a moving branch.
//
// Returns rewound=true when the run was rewound to the first source-sensitive
// stage and the caller should return without terminalizing.
func (r *Runner) recordExternalHeadMovement(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	mismatch *MergeSourceSHAMismatchError,
) (bool, error) {
	if r.Store == nil || r.Store.MRHeadTransitions == nil {
		return false, nil
	}
	dao := r.Store.MRHeadTransitions

	// A process that died mid-observation leaves an unsettled row. Settle it
	// with what we now know rather than minting a second row for the same
	// movement — the ledger must stay one row per movement.
	open, err := dao.OpenTransition(ctx, run.ID)
	if err != nil {
		return false, fmt.Errorf("pipeline: load open head transition: %w", err)
	}
	provenance := map[string]any{
		"classifier": string(store.MRHeadTransitionAmbiguous),
		"reason":     "merge-stage identity check found the mr head moved off the ci-authorized sha",
		"observed_by": map[string]any{
			"stage":       stage.ID,
			"mr_iid":      mismatch.MRIID,
			"reviewed":    mismatch.ReviewedSHA,
			"successor":   mismatch.ObservedSHA,
			"detected_at": r.now().UTC().Format(time.RFC3339Nano),
		},
	}
	if open != nil {
		if _, err := dao.Settle(ctx, store.SettleRequest{
			PipelineRunID: run.ID,
			Seq:           open.Seq,
			State:         store.MRHeadTransitionAmbiguous,
			SuccessorSHA:  mismatch.ObservedSHA,
			Provenance:    provenance,
			SettledAt:     r.now().UTC(),
		}); err != nil && !errors.Is(err, store.ErrHeadTransitionSettled) {
			return false, fmt.Errorf("pipeline: settle open head transition: %w", err)
		}
	} else {
		if _, err := dao.Open(ctx, &store.MRHeadTransition{
			PipelineRunID: run.ID,
			Project:       mismatch.Project,
			MRIID:         mismatch.MRIID,
			SourceBranch:  mismatch.SourceBranch,
			TargetBranch:  mismatch.TargetBranch,
			ReviewedSHA:   mismatch.ReviewedSHA,
			SuccessorSHA:  mismatch.ObservedSHA,
			Trigger:       store.MRHeadTriggerExternal,
			State:         store.MRHeadTransitionAmbiguous,
			Provenance:    provenance,
			RequestedAt:   r.now().UTC(),
		}); err != nil {
			return false, fmt.Errorf("pipeline: record external head transition: %w", err)
		}
	}

	settled, err := dao.CountSettled(ctx, run.ID)
	if err != nil {
		return false, fmt.Errorf("pipeline: count settled head transitions: %w", err)
	}
	budget := maxHeadTransitions(BuildMillsEnv(run, item, stage))
	r.event(ctx, "pipeline.head_transition.settled", "warn", map[string]any{
		"run": run.ID, "mr_iid": mismatch.MRIID, "trigger": string(store.MRHeadTriggerExternal),
		"state": string(store.MRHeadTransitionAmbiguous), "reviewed_sha": mismatch.ReviewedSHA,
		"successor_sha": mismatch.ObservedSHA, "settled": settled, "budget": budget,
	})

	if settled > budget {
		return false, r.escalateWithItem(ctx, run, item, ClassConfig, fmt.Sprintf(
			"stage %s: the mr head moved off the ci-authorized sha %s to %s, and the run has already absorbed %d head transition(s) (budget %d) [class=%s]: stop re-gating a moving branch — settle the source branch of mr %d in %s by hand, then requeue",
			stage.ID, mismatch.ReviewedSHA, mismatch.ObservedSHA, settled, budget, ClassConfig, mismatch.MRIID, mismatch.Project))
	}

	// Rewind. The CI authorization for reviewed_sha is now provably stale
	// (the fence alone would already fail the merge closed); re-running every
	// source-sensitive gate plus a fresh CI cycle is what re-binds the merge
	// to a SHA that was actually tested.
	run.CurrentStage = headTransitionRewindStage
	run.State = store.PipelineImplementing
	if err := r.Store.Pipeline.PutRun(ctx, run); err != nil {
		return false, fmt.Errorf("pipeline: persist head-transition rewind: %w", err)
	}
	r.logger().Info("pipeline rewound after mr head movement",
		"run", run.ID, "mr_iid", mismatch.MRIID, "reviewed_sha", mismatch.ReviewedSHA,
		"successor_sha", mismatch.ObservedSHA, "stage", headTransitionRewindStage,
		"settled", settled, "budget", budget)
	r.event(ctx, "pipeline.head_transition.rewind", "warn", map[string]any{
		"run": run.ID, "stage": headTransitionRewindStage, "mr_iid": mismatch.MRIID,
		"reviewed_sha": mismatch.ReviewedSHA, "successor_sha": mismatch.ObservedSHA,
	})
	return true, nil
}

func (r *Runner) mergeRecoveryPipelineCreateAttempted(ctx context.Context, runID string) (bool, error) {
	if r.Store == nil || r.Store.Pipeline == nil || runID == "" {
		return false, nil
	}
	rows, err := r.Store.Pipeline.ListStages(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, sr := range rows {
		if sr == nil || sr.Stage != "merge" || sr.Artifacts == nil {
			continue
		}
		if attempted, _ := sr.Artifacts[mergeRecoveryPipelineCreateAttemptedArtifact].(bool); attempted {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runner) ciWatchFlakeRescueAttempted(ctx context.Context, runID string) (bool, []FailedJob, error) {
	rows, err := r.Store.Pipeline.ListStages(ctx, runID)
	if err != nil {
		return false, nil, err
	}
	for _, sr := range rows {
		if sr != nil && sr.Stage == "ci_watch" && sr.Artifacts != nil {
			if v, _ := sr.Artifacts[ciWatchFlakeRescueAttemptedArtifact].(bool); v {
				var jobs []FailedJob
				switch raw := sr.Artifacts[ciWatchFlakeRescueFirstJobsArtifact].(type) {
				case []FailedJob:
					jobs = append(jobs, raw...)
				case []any:
					for _, v := range raw {
						if m, ok := v.(map[string]any); ok {
							id, _ := m["ID"].(float64)
							name, _ := m["Name"].(string)
							reason, _ := m["FailureReason"].(string)
							jobs = append(jobs, FailedJob{ID: int64(id), Name: name, FailureReason: reason})
						}
					}
				}
				return true, jobs, nil
			}
		}
	}
	return false, nil, nil
}

func (r *Runner) pendingStage(ctx context.Context, runID, stageID string) (*store.StageResult, error) {
	if r.Store == nil || runID == "" || stageID == "" {
		return nil, nil
	}
	rows, err := r.Store.Pipeline.ListStages(ctx, runID)
	if err != nil {
		return nil, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		sr := rows[i]
		if sr.Stage == stageID && sr.Outcome == nil && sr.SpawnID != "" {
			return sr, nil
		}
	}
	return nil, nil
}

// pollTimeoutAttempt returns an errored attempt for this exact spawn that was
// recorded when HUD polling timed out. A later terminal status observed during
// reconciliation is the resolution of that attempt, not another execution.
func (r *Runner) pollTimeoutAttempt(ctx context.Context, runID, stageID, spawnID string) (*store.StageResult, error) {
	if r.Store == nil || runID == "" || stageID == "" || spawnID == "" {
		return nil, nil
	}
	rows, err := r.Store.Pipeline.ListStages(ctx, runID)
	if err != nil {
		return nil, err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		sr := rows[i]
		if sr.Stage != stageID || sr.SpawnID != spawnID || sr.Outcome == nil || *sr.Outcome != store.StageOutcomeError {
			continue
		}
		if strings.Contains(sr.LogTail, ErrSpawnPollTimeout.Error()) {
			return sr, nil
		}
	}
	return nil, nil
}

// runStage executes one non-gate stage: persist current_stage, dispatch
// the worker, persist the stage_result row, propagate side-effects (cost,
// mr_iid, worktree_path) up onto the run row.
func (r *Runner) runStage(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	prior map[string]StageOutput,
	attempt int,
	pending *store.StageResult,
) (StageOutput, error) {
	now := r.now()
	resumeSpawnID := ""
	if pending != nil {
		now = pending.StartedAt
		resumeSpawnID = pending.SpawnID
	}
	run.CurrentStage = stage.ID
	run.State = stage.State
	if err := r.Store.Pipeline.PutRun(ctx, run); err != nil {
		return StageOutput{}, fmt.Errorf("persist run head: %w", err)
	}
	r.event(ctx, "pipeline.stage.start", "ok", map[string]any{
		"run": run.ID, "stage": stage.ID, "attempt": attempt,
	})

	acceptedSpawnID := resumeSpawnID
	stageCtx := withResumeSpawnID(ctx, resumeSpawnID)
	stageCtx = withStageAttempt(stageCtx, attempt)
	// The MR head-movement fence (#374) is read fresh for both halves of the
	// authorization: ci_watch STAMPS the value it authorized under, merge
	// COMPARES against it. Threaded through context exactly like the
	// merge-recovery create fence below so JobContext stays the only surface
	// a worker sees.
	if stage.ID == "ci_watch" || stage.ID == "merge" {
		seq, err := r.headTransitionSeq(ctx, run.ID)
		if err != nil {
			return StageOutput{}, fmt.Errorf("load mr head transition fence: %w", err)
		}
		stageCtx = withHeadTransitionSeq(stageCtx, seq)
	}
	ciWatchRescued := false
	var ciWatchFirstJobs []FailedJob
	if stage.ID == "ci_watch" {
		var err error
		var firstJobs []FailedJob
		ciWatchRescued, firstJobs, err = r.ciWatchFlakeRescueAttempted(ctx, run.ID)
		ciWatchFirstJobs = firstJobs
		if err != nil {
			return StageOutput{}, fmt.Errorf("load ci_watch flake rescue fence: %w", err)
		}
		stageCtx = withCIWatchFlakeRescueAttempted(stageCtx, ciWatchRescued)
		stageCtx = withCIWatchFlakeRescueFirstJobs(stageCtx, firstJobs)
		stageCtx = withCIWatchFlakeRescueRecorder(stageCtx, func(jobs []FailedJob) error {
			ciWatchRescued = true
			ciWatchFirstJobs = append([]FailedJob(nil), jobs...)
			return r.Store.Pipeline.PutStage(ctx, &store.StageResult{PipelineRunID: run.ID, Stage: stage.ID, Attempt: attempt, StartedAt: now, Artifacts: map[string]any{"stage_id": stage.ID, ciWatchFlakeRescueAttemptedArtifact: true, ciWatchFlakeRescueFirstJobsArtifact: jobs}})
		})
	}
	mergeRecoveryCreateAttempted := false
	if stage.ID == "merge" {
		var err error
		mergeRecoveryCreateAttempted, err = r.mergeRecoveryPipelineCreateAttempted(ctx, run.ID)
		if err != nil {
			return StageOutput{}, fmt.Errorf("load merge recovery mutation fence: %w", err)
		}
		stageCtx = withMergeRecoveryPipelineCreateAttempted(stageCtx, mergeRecoveryCreateAttempted)
		stageCtx = withMergeRecovery405Recorder(stageCtx, func(detail map[string]any) {
			payload := map[string]any{"run": run.ID, "stage": stage.ID, "attempt": attempt}
			for k, v := range detail {
				if _, taken := payload[k]; !taken {
					payload[k] = v
				}
			}
			r.event(ctx, "pipeline.merge.recovery405", "ok", payload)
		})
		stageCtx = withMergeRecoveryPipelineCreateRecorder(stageCtx, func() error {
			mergeRecoveryCreateAttempted = true
			return r.Store.Pipeline.PutStage(ctx, &store.StageResult{
				PipelineRunID: run.ID,
				Stage:         stage.ID,
				Attempt:       attempt,
				StartedAt:     now,
				Artifacts: map[string]any{
					"stage_id": stage.ID,
					mergeRecoveryPipelineCreateAttemptedArtifact: true,
				},
			})
		})
	}
	stageCtx = withStageAcceptRecorder(stageCtx, func(spawnID string) error {
		if spawnID == "" {
			return nil
		}
		acceptedSpawnID = spawnID
		return r.Store.Pipeline.PutStage(ctx, &store.StageResult{
			PipelineRunID: run.ID,
			Stage:         stage.ID,
			Attempt:       attempt,
			StartedAt:     now,
			SpawnID:       spawnID,
			Artifacts:     map[string]any{"stage_id": stage.ID},
		})
	})
	out, derr := r.Dispatcher.Dispatch(stageCtx, run, item, stage, prior)
	if ciWatchRescued {
		if out.Artifacts == nil {
			out.Artifacts = map[string]any{}
		}
		out.Artifacts[ciWatchFlakeRescueAttemptedArtifact] = true
		out.Artifacts[ciWatchFlakeRescueFirstJobsArtifact] = ciWatchFirstJobs
	}
	if mergeRecoveryCreateAttempted {
		if out.Artifacts == nil {
			out.Artifacts = map[string]any{}
		}
		out.Artifacts[mergeRecoveryPipelineCreateAttemptedArtifact] = true
	}
	if out.SpawnID == "" && acceptedSpawnID != "" {
		out.SpawnID = acceptedSpawnID
	}
	// A long dispatch (a spawn poll runs up to the client PollDeadline) is
	// exactly the window in which an operator force-escalates a stuck run.
	// If the persisted head went terminal while we were blocked, stop before
	// persisting this stage's rollup — otherwise we'd clobber the terminal
	// row back to a non-terminal stage (see runTerminatedExternally).
	if terminal, terr := r.runTerminatedExternally(ctx, run); terr != nil {
		return out, terr
	} else if terminal {
		return out, errRunTerminated
	}
	if derr != nil && out.SpawnID != "" && !hasTerminalSpawnStatus(out) {
		// A spawn that was accepted but whose poll did not reach a terminal
		// status is normally parked "pending" so the reconciler re-attaches
		// on the next tick (resume-safe across operator restarts).
		//
		// The dangerous case is a spawn the operator can never drive to a
		// terminal status again: a pod GC'd/reaped out from under it (poll
		// GETs 404 → a non-timeout error), or a pod that stays alive-but-hung
		// (poll always times out at PollDeadline). Re-attaching to the same
		// dead spawn every tick without ever burning the retry budget wedges
		// the run active-but-idle forever — observed 2026-06-25 (~13h pending
		// on a hung pod) and again 2026-07-01 (a resumed run logged "run
		// already active" every minute and never escalated). We therefore
		// count consecutive non-terminal poll failures — timeout OR error —
		// on this attempt; once they reach the stall tolerance we fall through
		// to the error path so the attempt is recorded errored and Drive's
		// retry/escalation logic re-dispatches a fresh spawn and ultimately
		// escalates. A single failure still parks: a genuine operator restart
		// mid-poll must stay resume-safe.
		failures := pendingPollFailures(pending) + 1
		if failures < maxConsecutiveSpawnPollFailures {
			// Still within tolerance: a legitimately slow-but-progressing
			// spawn (or a one-off interruption) gets another tick.
			return r.parkPendingSpawn(ctx, run, stage, attempt, now, out, derr, failures)
		}
		r.logger().Warn("pipeline spawn stalled; converting pending to errored attempt",
			"run", run.ID, "stage", stage.ID, "attempt", attempt,
			"spawn_id", out.SpawnID, "consecutive_poll_failures", failures,
			"poll_timeout", errors.Is(derr, ErrSpawnPollTimeout))
		// fall through to the error-path persistence below.
	}
	// HUD can first report a poll timeout, then later report that the same
	// spawn failed once its controller reconciles it. Fold that terminal
	// observation into the timeout row so telemetry and retry accounting retain
	// one stage attempt per SpawnID.
	dedupedPollTimeout := false
	if derr != nil && out.SpawnID != "" && hasTerminalSpawnStatus(out) && !errors.Is(derr, ErrSpawnPollTimeout) {
		priorAttempt, lookupErr := r.pollTimeoutAttempt(ctx, run.ID, stage.ID, out.SpawnID)
		if lookupErr != nil {
			return out, fmt.Errorf("load poll-timeout stage attempt: %w", lookupErr)
		}
		if priorAttempt != nil {
			attempt = priorAttempt.Attempt
			now = priorAttempt.StartedAt
			dedupedPollTimeout = true
		}
	}
	endedAt := r.now()

	mills.PipelineStageDurationSeconds.WithLabelValues(stage.ID).Observe(endedAt.Sub(now).Seconds())
	outcome := store.StageOutcomeSuccess
	if derr != nil {
		outcome = store.StageOutcomeError
	}
	if !dedupedPollTimeout {
		mills.PipelineStageAttemptsTotal.WithLabelValues(stage.ID, string(outcome)).Inc()
	}
	logTail := out.LogTail
	if derr != nil {
		logTail = buildFailureLogTail(out.LogTail, derr, stage.ID, attempt, out.SpawnID)
	}
	attributedCost := out.CostUSD
	if derr == nil && out.SpawnID != "" {
		rows, err := r.Store.Pipeline.ListStages(ctx, run.ID)
		if err != nil {
			return out, fmt.Errorf("load prior spawn costs: %w", err)
		}
		for _, row := range rows {
			if row.SpawnID == out.SpawnID {
				attributedCost -= row.CostUSD
			}
		}
		if attributedCost < 0 {
			attributedCost = 0
		}
	}
	sr := &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         stage.ID,
		Attempt:       attempt,
		StartedAt:     now,
		EndedAt:       &endedAt,
		Outcome:       &outcome,
		SpawnID:       out.SpawnID,
		CostUSD:       attributedCost,
		Model:         out.Model,
		Backend:       out.Backend,
		Artifacts:     mergeArtifacts(stage.ID, out),
		LogTail:       logTail,
	}
	if perr := r.Store.Pipeline.PutStage(ctx, sr); perr != nil {
		// PutStage failure is unrecoverable for audit purposes.
		return out, fmt.Errorf("persist stage: %w", perr)
	}
	// Journal the outcome only once the stage result itself is durable, so the
	// item's memory can never claim work the audit trail does not have. Errors
	// are recorded too: "implement attempt 1 failed the scope gate" is exactly
	// what the retry needs to not repeat itself.
	r.recordItemMemory(ctx, item, stage, attempt, out, logTail, derr)
	if derr != nil {
		r.event(ctx, "pipeline.stage.error", "error", map[string]any{
			"run": run.ID, "stage": stage.ID, "attempt": attempt, "error": derr.Error(),
		})
		if dedupedPollTimeout {
			return out, &dedupedStageAttemptError{err: derr, attempt: attempt}
		}
		return out, derr
	}

	// Roll up side effects onto the run row.
	run.CostUSD += attributedCost
	if out.MRIID != 0 {
		v := out.MRIID
		run.MRIID = &v
	}
	if out.WorktreePath != "" {
		run.WorktreePath = out.WorktreePath
	}
	if err := r.Store.Pipeline.PutRun(ctx, run); err != nil {
		return out, fmt.Errorf("persist run rollup: %w", err)
	}

	r.event(ctx, "pipeline.stage.done", "ok", map[string]any{
		"run": run.ID, "stage": stage.ID, "attempt": attempt, "cost_usd": attributedCost,
	})
	return out, nil
}

// buildFailureLogTail returns a non-empty log_tail string for a stage
// attempt that errored. It is the single source of truth for what gets
// persisted to stage_results.log_tail on the error path, including the
// pending path where the spawn was accepted but the worker call did not
// reach a terminal status. The returned string always contains
// identifiable context (stage id, attempt #, spawn id when known) so
// triage on the audit table never lands on an empty cell — historically
// 33 of 33 plan_slice error rows had no log_tail and were untriagable.
//
// Precedence:
//  1. Existing log_tail from the worker (most informative — telemetry
//     from the spawn poll, devbox check tail, etc.).
//  2. err.Error() — what the dispatcher returned upstream.
//  3. A synthetic "<stage> attempt <n> spawn <id>: no error text returned
//     by worker" fallback so the row is still searchable.
func buildFailureLogTail(existing string, err error, stageID string, attempt int, spawnID string) string {
	tail := strings.TrimSpace(existing)
	if tail == "" && err != nil {
		tail = strings.TrimSpace(err.Error())
	}
	if tail == "" {
		tail = "no error text returned by worker"
	}
	prefix := fmt.Sprintf("stage=%s attempt=%d", stageID, attempt)
	if spawnID != "" {
		prefix += " spawn=" + spawnID
	}
	// Avoid double-prefixing when the worker already echoed the stage
	// label (devbox/spawn clients sometimes do).
	if strings.Contains(tail, prefix) {
		return tail
	}
	return prefix + ": " + tail
}

// spawnPollTimeoutsArtifactKey records, on a pending stage_results row, how
// many consecutive non-terminal poll failures a single spawn attempt has
// accumulated. Persisted in artifacts_json so the count survives operator
// restarts (the reconciler re-drives from the persisted pending row). The
// literal key ("spawn_poll_timeouts") is kept for backward compatibility with
// pending rows written before the counter broadened from timeouts-only to all
// non-terminal poll failures; renaming it would silently reset in-flight
// counters on the deploy that rolls this change.
const spawnPollTimeoutsArtifactKey = "spawn_poll_timeouts"

// maxConsecutiveSpawnPollFailures bounds how many times the runner re-attaches
// to a non-terminal spawn that never reaches a terminal status — whether it
// times out at the client's PollDeadline (hung-but-alive pod) or returns a
// non-timeout error (a reaped/GC'd pod whose poll GETs 404) — before giving up
// on that spawn and recording the attempt as errored. With the default 30m
// PollDeadline, a value of 2 tolerates a legitimately slow-but-progressing
// spawn (or a one-off operator-restart-mid-poll interruption) for one extra
// tick while still converting a spawn the operator can never drive to terminal
// into a failed, transient-class attempt that burns the retry budget. Below
// this we keep the resume-safe "check again next tick" behavior; at/above it
// the stall converts so the run re-spawns and escalates instead of wedging
// active-but-idle forever.
const maxConsecutiveSpawnPollFailures = 2

// pendingPollFailures reads the consecutive non-terminal poll-failure counter
// off a pending stage_results row. Returns 0 when absent. Artifacts round-trip
// through JSON (sqlite artifacts_json), so an int stored on one Drive comes
// back as float64 on the next — both are handled.
func pendingPollFailures(pending *store.StageResult) int {
	if pending == nil || pending.Artifacts == nil {
		return 0
	}
	switch v := pending.Artifacts[spawnPollTimeoutsArtifactKey].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// runTerminatedExternally reports whether the run's persisted state has been
// moved to a terminal state (done/escalated/paused) by a writer other than
// this Drive goroutine. Drive only ever writes non-terminal stage states as it
// steps (markDone/escalateWithItem run at loop exit, not at the checkpoints
// that call this), so a terminal persisted state can only come from an
// out-of-band actor: the manual /escalate handler or the pause kill-switch.
// When it has, this goroutine must stop without re-persisting its stale head —
// otherwise it clobbers the terminal row back to a non-terminal stage and the
// next operator restart's resume re-activates a run it was meant to skip.
//
// A read error is treated as "not terminated" so a transient store blip never
// aborts an otherwise-healthy drive; the normal persist paths surface any real
// store failure.
func (r *Runner) runTerminatedExternally(ctx context.Context, run *store.PipelineRun) (bool, error) {
	if r.Store == nil || run == nil || run.ID == "" {
		return false, nil
	}
	persisted, err := r.Store.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		r.logger().Warn("pipeline: re-read run head failed; continuing drive",
			"run", run.ID, "error", err)
		return false, nil
	}
	if store.IsPipelineTerminalState(persisted.State) {
		r.event(ctx, "pipeline.drive.aborted_terminal", "ok", map[string]any{
			"run": run.ID, "state": string(persisted.State), "stage": run.CurrentStage,
		})
		return true, nil
	}
	return false, nil
}

// runTerminalResolution returns the durable terminal state for run, if one is
// already persisted. Start uses this after Drive errors so cleanup/hook errors
// that occur after a successful terminal write do not trigger a conflicting
// escalation attempt for the same run.
func (r *Runner) runTerminalResolution(ctx context.Context, run *store.PipelineRun) (store.PipelineState, bool) {
	if r.Store == nil || run == nil || run.ID == "" {
		return "", false
	}
	persisted, err := r.Store.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			r.logger().Warn("pipeline: terminal resolution re-read failed",
				"run", run.ID, "error", err)
		}
		return "", false
	}
	if store.IsPipelineTerminalState(persisted.State) {
		return persisted.State, true
	}
	return "", false
}

// parkPendingSpawn persists a stage attempt that accepted a spawn but did not
// reach a terminal status, leaving outcome NULL so the reconciler re-attaches
// next tick. pollFailures carries the running consecutive non-terminal
// poll-failure count forward so a recurring stall can be detected across
// re-drives. It returns errStagePending so Drive stops without burning the
// retry budget.
func (r *Runner) parkPendingSpawn(
	ctx context.Context,
	run *store.PipelineRun,
	stage Stage,
	attempt int,
	startedAt time.Time,
	out StageOutput,
	derr error,
	pollFailures int,
) (StageOutput, error) {
	pendingTail := buildFailureLogTail(out.LogTail, derr, stage.ID, attempt, out.SpawnID)
	art := map[string]any{"stage_id": stage.ID}
	if pollFailures > 0 {
		art[spawnPollTimeoutsArtifactKey] = pollFailures
	}
	if perr := r.Store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID,
		Stage:         stage.ID,
		Attempt:       attempt,
		StartedAt:     startedAt,
		SpawnID:       out.SpawnID,
		Model:         out.Model,
		Backend:       out.Backend,
		Artifacts:     art,
		LogTail:       pendingTail,
	}); perr != nil {
		return out, fmt.Errorf("persist pending stage: %w", perr)
	}
	r.event(ctx, "pipeline.stage.pending", "ok", map[string]any{
		"run": run.ID, "stage": stage.ID, "attempt": attempt,
		"spawn_id": out.SpawnID, "error": derr.Error(), "poll_failures": pollFailures,
	})
	return out, errStagePending
}

func hasTerminalSpawnStatus(out StageOutput) bool {
	if out.Artifacts == nil {
		return false
	}
	status, _ := out.Artifacts["status"].(string)
	switch status {
	case "completed", "failed", "stopped":
		return true
	default:
		return false
	}
}

// runGate evaluates an auto_gate stage against the gate registry. Returns
// (true, "", false, false, nil) on aggregate pass and
// (false, failDetail, terminal, judgeUnparseable, nil) on aggregate fail: the
// caller triggers retry unless terminal; judgeUnparseable is true only when
// EVERY failing gate is an ungradeable-envelope LLM-judge miss (raw=""), which
// the caller recovers/escalates as an external model-provider dependency
// instead of burning the code-class RetryFrom budget (issue #348). failDetail names
// the failing gates + reasons; terminal means at least one failing gate marked
// its verdict retry-proof, so the caller escalates instead of retrying), and
// (_, _, _, err) only on infrastructure errors.
func (r *Runner) runGate(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	prior map[string]StageOutput,
	policy *mills.Policy,
) (gateVerdict, error) {
	if r.Gates == nil || len(stage.Gates) == 0 {
		// No gates registered → vacuously pass. Logged for audit.
		r.event(ctx, "pipeline.gate.skip", "ok", map[string]any{
			"run": run.ID, "gate": stage.ID,
		})
		return gateVerdict{Pass: true}, nil
	}
	in := r.gateInputFor(ctx, stage, item, policy, prior)
	in.RunID = run.ID

	// Filter to gates that are actually registered. An unregistered gate
	// (e.g. spec_conformance before slice 4.5 lands) is treated as skip,
	// not fail — the static template lists future gates by name.
	known := r.Gates.Names()
	knownSet := make(map[string]bool, len(known))
	for _, n := range known {
		knownSet[n] = true
	}
	var toRun []string
	for _, g := range stage.Gates {
		if knownSet[g] {
			toRun = append(toRun, g)
		}
	}
	if len(toRun) == 0 {
		r.event(ctx, "pipeline.gate.skip", "ok", map[string]any{
			"run": run.ID, "gate": stage.ID, "reason": "no registered gates",
		})
		return gateVerdict{Pass: true}, nil
	}

	outcomes, allPass, err := r.Gates.EvaluateAll(ctx, toRun, in)
	if err != nil {
		return gateVerdict{}, err
	}
	terminal := false
	// judgeUnparseable is true only when the aggregate FAILED and EVERY failing
	// gate is an LLM judge that ran but returned an ungradeable score envelope
	// (JudgedBy == store.JudgedByUnparseable). A single real content fail
	// (score below threshold, a pure-Go gate) flips it false so the normal
	// respawn-and-retry path handles a genuine defect (issue #348 fix keeps the
	// external-dependency shortcut scoped to model-provider misses only).
	judgeUnparseable := !allPass
	anyFail := false
	var failed []gates.NamedOutcome
	for _, no := range outcomes {
		if no.Outcome.Pass {
			continue
		}
		anyFail = true
		failed = append(failed, no)
		if no.Outcome.Terminal {
			terminal = true
		}
		if no.Outcome.JudgedBy != store.JudgedByUnparseable {
			judgeUnparseable = false
		}
	}
	if !anyFail {
		judgeUnparseable = false
	}
	for _, no := range outcomes {
		row := &store.GateOutcome{
			PipelineRunID: run.ID,
			AfterStage:    stage.ID,
			GateName:      no.Name,
			Outcome:       store.GateOutcomePass,
			Reasons:       no.Outcome.Reasons,
			JudgedBy:      no.Outcome.JudgedBy,
			EvaluatedAt:   r.now(),
		}
		switch {
		case no.Outcome.Skip:
			// Advisory/not-applicable: proceeds like a pass but persisted as
			// 'skip' so gate_pass_rate can exclude it (see gates.Outcome.Skip).
			row.Outcome = store.GateOutcomeSkip
		case !no.Outcome.Pass:
			row.Outcome = store.GateOutcomeFail
		}
		mills.GateEvaluationsTotal.WithLabelValues(no.Name, string(row.Outcome)).Inc()
		if perr := r.Store.Pipeline.PutGate(ctx, row); perr != nil {
			r.logger().Warn("pipeline gate persist failed", "error", perr)
		}
		r.recordJudgeVerdicts(ctx, run, no.Name, no.Outcome)
	}
	failDetail := summarizeGateFailures(outcomes)
	r.event(ctx, "pipeline.gate.eval", boolStr(allPass, "ok", "fail"), map[string]any{
		"run": run.ID, "gate": stage.ID, "gates_run": toRun, "pass": allPass,
	})
	return gateVerdict{
		Pass:             allPass,
		FailDetail:       failDetail,
		Terminal:         terminal,
		JudgeUnparseable: judgeUnparseable,
		Failed:           failed,
		Input:            in,
	}, nil
}

// gateVerdict is one auto_gate stage's evaluation, bundled so Drive can act on
// WHICH gates failed rather than only on the aggregate boolean. Scope
// auto-amendment needs exactly that: it may only fire when `scope` is the sole
// failure (a diff that also trips secret_scan or diff_size is not a
// too-narrow-envelope story), and it needs the same StageInput the gate judged
// so it can recompute the full violation list instead of parsing it back out
// of the truncated reason prose.
type gateVerdict struct {
	Pass             bool
	FailDetail       string
	Terminal         bool
	JudgeUnparseable bool
	// Failed is every non-passing outcome of this evaluation, in gate order.
	Failed []gates.NamedOutcome
	// Input is the StageInput the gates were evaluated against.
	Input gates.StageInput
}

// scopeOnlyFailure reports whether the sole failing gate of this verdict is
// `scope`. This is the entry condition for auto-amendment.
func (v gateVerdict) scopeOnlyFailure() bool {
	return len(v.Failed) == 1 && v.Failed[0].Name == scopeGateName
}

// scopeGateName is the registry name of the scope gate (gates.Scope.Name()).
// Kept as a constant here so the amendment's entry condition can't silently
// stop matching if the gate is ever renamed without a compile error somewhere.
const scopeGateName = "scope"

// gateFailureDetailMaxLen bounds the failing-gate summary carried in retry
// contexts and escalation reasons. Gate reasons can embed whole file lists;
// the summary is for triage, not for replaying the full outcome (the
// gate_outcomes table keeps that).
const gateFailureDetailMaxLen = 500

// maxJudgeUnparseableRetries bounds the FREE gate re-judges spent recovering an
// ungradeable LLM-judge score envelope (raw="") before the run escalates as an
// external model-provider dependency incident. Each re-judge re-runs only the
// gate (a fresh judge call that re-invokes the client's larger-budget recovery)
// — no agent respawn, no attempt-budget spend — so a transient provider hiccup
// on finished work is absorbed cheaply while a persistent outage still
// escalates without burning the code-class RetryFrom budget (issue #348).
const maxJudgeUnparseableRetries = 2

// maxJudgeTransportRetries bounds the FREE gate re-judges spent on an auto_gate
// stage that ERRORED (the judge call itself failed: litellm 400, provider 429,
// timeout, 5xx) before the run escalates. Same shape and rationale as
// maxJudgeUnparseableRetries — gate-only re-runs, no agent respawn, no
// attempt-budget spend — because a failed judge CALL produced no verdict on the
// diff at all (issue #378).
const maxJudgeTransportRetries = 2

// handleGateError decides what to do with an auto_gate stage whose evaluation
// ERRORED instead of returning a verdict. Returns (true, nil) when the caller
// must re-run only this gate (a FREE re-judge: no respawn, no attempt spend);
// otherwise the run is escalated and the error is the escalation's.
//
// Every escalation minted here PASSES its class to escalateWithItem, so a
// judge-transport failure lands as infra/transient (auto-requeue eligible)
// instead of "unclassified" — the defect behind issue #378, where a litellm 400
// on the spec_conformance judge parked a run permanently with no class at all.
// The "[class=…]" text in the reason is operator-facing prose now, not the
// transport.
func (r *Runner) handleGateError(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	gateErr error,
	retries map[string]int,
) (bool, error) {
	cls, incident, external := classifyGateError(gateErr)
	mills.PipelineStageErrorClassTotal.WithLabelValues(stage.ID, string(cls)).Inc()
	detail := truncate(gateErr.Error(), gateFailureDetailMaxLen)

	if external {
		r.recordExternalIncident(ctx, incident)
		return false, r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf(
			"gate %s known external dependency incident (not retried) [class=%s]: %s (%s): %s; wait for %s to recover, then requeue",
			stage.ID, cls, incident.ID, incident.Summary, detail, incident.Dependency))
	}
	if !isFreeGateRejudgeClass(cls) {
		return false, r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf(
			"gate %s errored terminally (not retried) [class=%s]: %s — the judge call cannot succeed on an identical retry; fix the gate/model configuration, then requeue",
			stage.ID, cls, detail))
	}

	n := retries[stage.ID] + 1
	if n > maxJudgeTransportRetries {
		return false, r.escalateWithItem(ctx, run, item, cls, fmt.Sprintf(
			"gate %s judge call failed after %d free re-judges [class=%s]: %s — the rubric judge never graded the diff (transport-layer failure at the model provider), so this is not a code defect; requeue once the provider recovers",
			stage.ID, maxJudgeTransportRetries, cls, detail))
	}
	retries[stage.ID] = n
	r.logger().Info("pipeline gate: judge call errored; free re-judge without respawn",
		"run", run.ID, "gate", stage.ID, "class", cls,
		"attempt", n, "cap", maxJudgeTransportRetries, "error", gateErr)
	r.event(ctx, "pipeline.gate.judge_transport_rejudge", "warn", map[string]any{
		"run": run.ID, "gate": stage.ID, "class": string(cls), "attempt": n, "error": detail,
	})
	if backoff := retryBackoff(cls, gateErr, n); backoff > 0 {
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return true, nil
}

// recordExternalIncident is the single write-side choke point for pipeline
// external-dependency classifications. Persistence is observability only: a
// broken incident store is logged and never changes stage control flow.
func (r *Runner) recordExternalIncident(ctx context.Context, incident mcperror.ExternalIncident) {
	classificationMetrics.RecordClassification(telemetry.ClassificationClassExternalDependencyIncident)
	if r == nil || r.IncidentWriter == nil {
		return
	}

	source := incidentSource(incident.Kind)
	record, matched := ClassifyExternalDependencyIncident(source, incident.Evidence)
	if !matched {
		record = store.IncidentRecord{
			ID:         incident.ID,
			Class:      store.IncidentClassExternalDependency,
			Source:     source,
			Dependency: incident.Dependency,
			Shape:      mcperror.ExternalIncidentReasonCode(incident),
			Summary:    incident.Summary,
			Evidence:   incident.Evidence,
			Retryable:  mcperror.ExternalIncidentRetryable(incident),
		}
	}
	if _, err := r.IncidentWriter.Put(ctx, &record); err != nil {
		r.logger().Warn("pipeline: persist classified external dependency incident failed",
			"incident", incident.ID, "dependency", incident.Dependency, "error", err)
	}
}

func incidentSource(kind string) string {
	switch kind {
	case mcperror.ExternalIncidentKindModelProvider:
		return "model-provider"
	case mcperror.ExternalIncidentKindBlobStorage:
		return "storage"
	default:
		return "gitlab-ci"
	}
}

// classifyGateError classifies an auto_gate stage error with the same machinery
// as the non-gate stage path (Classify + external-incident override), plus one
// gate-specific rule: a gate ERROR is never ClassCode. The judge produced no
// verdict, so the diff was never graded and blaming it is a category error;
// the failure lives at the model-provider/transport layer, which is ClassInfra
// (bounded retries, auto-requeue eligible, honest metric label).
func classifyGateError(err error) (ErrorClass, mcperror.ExternalIncident, bool) {
	cls := Classify(err)
	if cls == ClassCode {
		cls = ClassInfra
	}
	incident, external := mcperror.ClassifyExternalCIIncident(err.Error())
	if external {
		cls = ClassConfig
	}
	return cls, incident, external
}

// isFreeGateRejudgeClass reports whether a gate-error class earns bounded FREE
// re-judges. Everything retry can plausibly fix qualifies; ClassConfig is
// terminal by definition (IsTerminal) and escalates on first sight.
func isFreeGateRejudgeClass(c ErrorClass) bool {
	return IsFreeRetry(c) || c == ClassInfra
}

// summarizeGateFailures renders the failing outcomes of one gate evaluation
// as "name: reason1, reason2; name2: …". Returns "" when everything passed.
func summarizeGateFailures(outcomes []gates.NamedOutcome) string {
	var parts []string
	for _, no := range outcomes {
		if no.Outcome.Pass {
			continue
		}
		p := no.Name
		if len(no.Outcome.Reasons) > 0 {
			p += ": " + strings.Join(no.Outcome.Reasons, ", ")
		}
		parts = append(parts, p)
	}
	s := strings.Join(parts, "; ")
	if len(s) > gateFailureDetailMaxLen {
		s = s[:gateFailureDetailMaxLen] + "…"
	}
	return s
}

// seedRetryContexts rehydrates per-RetryFrom retry contexts from the
// persisted gate_outcomes of a run, so a Drive that resumes mid-retry after
// an operator restart still hands the fresh spawn its retry context (which
// gate failed and why). Fail rows are walked oldest-first: the first fail
// seen for a RetryFrom stage becomes FirstFailure, the newest LastFailure.
func (r *Runner) seedRetryContexts(ctx context.Context, runID string) (map[string]*StageRetryContext, error) {
	out := map[string]*StageRetryContext{}
	if r.Store == nil || runID == "" {
		return out, nil
	}
	rows, err := r.Store.Pipeline.ListGates(ctx, runID)
	if err != nil {
		return nil, err
	}
	retryFromByGateStage := map[string]string{}
	for _, s := range r.Stages {
		if s.Type == "auto_gate" && s.RetryFrom != "" {
			retryFromByGateStage[s.ID] = s.RetryFrom
		}
	}
	for _, g := range rows {
		if g.Outcome != store.GateOutcomeFail {
			continue
		}
		retryFrom := retryFromByGateStage[g.AfterStage]
		if retryFrom == "" {
			continue
		}
		detail := g.GateName
		if len(g.Reasons) > 0 {
			detail += ": " + strings.Join(g.Reasons, ", ")
		}
		if len(detail) > gateFailureDetailMaxLen {
			detail = detail[:gateFailureDetailMaxLen] + "…"
		}
		rc := out[retryFrom]
		if rc == nil {
			rc = &StageRetryContext{GateStage: g.AfterStage, FirstFailure: detail}
			out[retryFrom] = rc
		}
		rc.LastFailure = detail
	}
	return out, nil
}

// gateInputFor builds the StageInput passed to gates. It walks `prior`
// for the most recent diff/file/test artifact regardless of which stage
// produced it.
func (r *Runner) gateInputFor(ctx context.Context, stage Stage, item *store.BacklogItem, policy *mills.Policy, prior map[string]StageOutput) gates.StageInput {
	in := gates.StageInput{Item: item, Policy: policy}
	if impl, ok := prior["implement"]; ok {
		in.FilesChanged = impl.FilesChanged
		in.LinesAdded = impl.LinesAdded
		in.LinesRemoved = impl.LinesRemoved
		in.DiffPatch = impl.DiffPatch
		in.CommitMessages = impl.CommitMessages
		in.GitCaptureStatus, in.GitCaptureReason = gitCaptureFromArtifacts(impl.Artifacts)
	}
	// prior[stage.ID] is only written after a stage completes without error
	// (the retry/escalate paths run first), so the presence of a "tests"
	// output means the devbox quality gate genuinely passed. The LLM judges
	// use this to ground out compile-health hallucinations (#304).
	if _, ok := prior["tests"]; ok {
		in.TestsPassed = true
	}
	in.ProjectBootstrapped = r.projectBootstrapped(ctx, item)
	return in
}

// gitCaptureFromArtifacts reads the cumulative-git-capture provenance the
// spawn client stamps onto every terminal stage under
// GitCaptureArtifactKey. Artifacts survive a JSON round-trip through
// stage_results, so the map may hold either the live map[string]any or a
// decoded one; both decode identically here. Anything else — a legacy row
// written before the key existed, a non-spawn worker — reads as unknown,
// which callers treat as "say nothing extra".
func gitCaptureFromArtifacts(art map[string]any) (status, reason string) {
	raw, ok := art[GitCaptureArtifactKey]
	if !ok {
		return "", ""
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return "", ""
	}
	status, _ = m["status"].(string)
	reason, _ = m["reason"].(string)
	return status, reason
}

// projectBootstrapped reports whether item targets a runtime-minted repo from
// the bootstrapped_projects registry. Matching is by RepoBase because the
// registry stores the full PathWithNamespace while a TargetProject may be
// bare or bucket-qualified. Fail-closed: any lookup error reads as "not
// bootstrapped", which only makes the scope gate stricter.
func (r *Runner) projectBootstrapped(ctx context.Context, item *store.BacklogItem) bool {
	if item == nil || strings.TrimSpace(item.TargetProject) == "" {
		return false
	}
	if r.Store == nil || r.Store.Bootstrap == nil {
		return false
	}
	minted, err := r.Store.Bootstrap.List(ctx)
	if err != nil {
		r.logger().Warn("pipeline: bootstrapped-project lookup failed; treating as not bootstrapped",
			"project", item.TargetProject, "error", err)
		return false
	}
	want := store.RepoBase(item.TargetProject)
	for _, p := range minted {
		if store.RepoBase(p.Project) == want {
			return true
		}
	}
	return false
}

// markDone closes out a run that completed cleanup successfully and
// fires the OnMerged hook for downstream eval Loop B attribution.
func (r *Runner) markDone(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	t := r.now()
	run.State = store.PipelineDone
	run.CurrentStage = ""
	run.EndedAt = &t
	if err := r.Store.Pipeline.PutRun(ctx, run); err != nil {
		return fmt.Errorf("persist run done: %w", err)
	}
	ownsBacklog, err := r.runOwnsBacklogState(ctx, run)
	if err != nil {
		return fmt.Errorf("resolve backlog state owner: %w", err)
	}
	if item != nil && ownsBacklog {
		current, err := r.Store.Backlog.TransitionState(
			ctx, item.ID, run.AggregateVersion, item.State, store.BacklogMerged,
		)
		if err != nil {
			return fmt.Errorf("persist backlog merged: %w", err)
		}
		*item = *current
	}
	mills.PipelineRunsTotal.WithLabelValues(string(store.PipelineDone)).Inc()
	mills.PipelineCostUSDTotal.WithLabelValues(string(store.PipelineDone)).Add(run.CostUSD)
	r.event(ctx, "pipeline.run.done", "ok", map[string]any{
		"run": run.ID, "cost_usd": run.CostUSD,
	})
	if r.OnMerged != nil && item != nil && ownsBacklog {
		if err := r.OnMerged(ctx, run, item); err != nil {
			r.logger().Warn("pipeline OnMerged hook failed", "run", run.ID, "error", err)
		}
	}
	// Auto-close the item's open escalation issue now that a run for it
	// succeeded (DEBT-073 / #167). Best-effort and optional: only fires when the
	// wired Escalator implements EscalationResolver; a failure is logged and
	// never undoes the done state.
	if item != nil && ownsBacklog {
		if resolver, ok := r.Escalator.(EscalationResolver); ok && resolver != nil {
			if err := resolver.ResolveOnSuccess(ctx, run, item); err != nil {
				r.logger().Warn("pipeline resolve-on-success failed", "run", run.ID, "error", err)
			}
		}
	}
	return nil
}

// escalateWithItem transitions a run to escalated, records the reason,
// and invokes the optional EscalationHandler for issue+handoff
// publication. Handler failures are logged but don't undo the state
// transition.
//
// Slice 3d: when the reason indicates a transient-class hard-cap hit
// AND the backlog item has been auto-retried fewer than
// policy.Pipeline.Retry.EscalationAutoRetryCap times, divert to
// maybeAutoRetry which marks this run escalated but keeps the backlog
// queued so the reconciler spins up a fresh pipeline_run on the next
// tick (kicked immediately via OnAutoRetry).
func (r *Runner) escalateWithItem(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, cls ErrorClass, reason string) error {
	if r.maybeAutoRetry(ctx, run, item, cls, reason) {
		return nil
	}
	if isTransientEscalationClass(cls) && r.transientRetryBudgetExhausted(ctx, item) {
		exhausted := true
		run.RetryExhausted = &exhausted
	}
	t := r.now()
	run.State = store.PipelineEscalated
	run.EndedAt = &t
	if err := r.Store.Pipeline.PutRun(ctx, run); err != nil {
		return fmt.Errorf("persist run escalated: %w", err)
	}
	r.stampEscalationMetadata(ctx, run, cls, reason, "")
	if run.RetryExhausted != nil {
		if err := r.Store.Pipeline.SetEscalationMetadata(ctx, run.ID, store.EscalationMetadata{RetryExhausted: run.RetryExhausted}); err != nil {
			r.logger().Warn("pipeline: stamp retry exhaustion failed", "run", run.ID, "error", err)
		}
	}
	ownsBacklog, err := r.runOwnsBacklogState(ctx, run)
	if err != nil {
		return fmt.Errorf("resolve backlog state owner: %w", err)
	}
	if item != nil && ownsBacklog {
		current, err := r.Store.Backlog.TransitionState(
			ctx, item.ID, run.AggregateVersion, item.State, store.BacklogEscalated,
		)
		if err != nil {
			return fmt.Errorf("persist backlog escalated: %w", err)
		}
		*item = *current
		// Freeze the item's TargetProject onto the run's event subject,
		// first-writer: the ghost-spark merged-branch sweep authorizes a
		// cross-repo branch lookup against this immutable binding, never the
		// mutable backlog field. Best-effort — the escalation stands without it
		// (the item then just stays a human's to close).
		if r.Store != nil {
			if _, err := mills.AppendEscalationTargetBinding(ctx, r.Store.Events, "pipeline", run, item); err != nil {
				r.logger().Warn("pipeline: escalation target binding append failed",
					"run", run.ID, "backlog", item.ID, "error", err)
			}
		}
	}
	mills.PipelineRunsTotal.WithLabelValues(string(store.PipelineEscalated)).Inc()
	mills.PipelineCostUSDTotal.WithLabelValues(string(store.PipelineEscalated)).Add(run.CostUSD)
	mills.EscalationsTotal.WithLabelValues(classifyEscalationReason(reason)).Inc()
	mills.EscalationClassTotal.WithLabelValues(escalationClassLabel(ErrorClass(run.EscalationClass), reason, "")).Inc()
	r.event(ctx, "pipeline.run.escalated", "error", map[string]any{
		"run": run.ID, "reason": reason,
	})
	if r.Escalator != nil && item != nil && ownsBacklog {
		if err := r.Escalator.Handle(ctx, run, item, reason); err != nil {
			r.logger().Warn("pipeline escalator failed", "run", run.ID, "error", err)
		}
	}
	if r.OnEscalated != nil && item != nil && ownsBacklog {
		if err := r.OnEscalated(ctx, run, item); err != nil {
			r.logger().Warn("pipeline OnEscalated hook failed", "run", run.ID, "error", err)
		}
	}
	return nil
}

// escalateCIWatchStall escalates a ci_watch run whose pipeline was still running
// at the watch hard cap. It escalates on first sight (a retry re-watches and
// re-hits the cap) but records the failure as a RETRYABLE external-dependency
// incident keyed on the stuck pipeline URL, so history/telemetry attribute the
// stall to CI rather than the diff and a later requeue can recover it once CI
// drains. (S3)
func (r *Runner) escalateCIWatchStall(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, stageID string, stall *CIWatchStalledError) error {
	reason := fmt.Sprintf("stage %s CI pipeline still running after the %dm watch cap (external CI dependency, not retried) [class=%s]: %v — the GitLab pipeline never reached a terminal state within the cap; wait for CI to drain (or retry the pipeline in GitLab), then requeue", stageID, stall.MaxMinutes, ClassInfra, stall)
	if err := r.escalateWithItem(ctx, run, item, ClassInfra, reason); err != nil {
		return err
	}
	// escalateWithItem already stamped the declared class, but it has no way to
	// know the dynamic pipeline URL. Record the external-dependency identity
	// explicitly so the stall keys on the stuck pipeline and is marked
	// retryable. Best-effort — a persist failure never undoes the escalation
	// (mirrors stampEscalationMetadata).
	if r.Store == nil || r.Store.Pipeline == nil {
		return nil
	}
	retryable := true
	md := store.EscalationMetadata{
		EscalationClass:      string(ClassInfra),
		FailureClass:         string(FailureInfrastructure),
		ExternalDependencyID: stall.PipelineURL,
		ExternalDependency:   ciWatchExternalDependency,
		Retryable:            &retryable,
	}
	if err := r.Store.Pipeline.SetEscalationMetadata(ctx, run.ID, md); err != nil {
		r.logger().Warn("pipeline: stamp ci_watch stall metadata failed", "run", run.ID, "error", err)
	}
	return nil
}

// maybeAutoRetry implements Slice 3d's bounded escalation auto-retry.
// Returns true (handled) when the escalation has been diverted into a
// re-queue. Caller must NOT proceed with the normal escalation path
// when this returns true. Returns false when the escalation is a
// real failure (code / infra / non-transient) that needs human eyes.
func (r *Runner) maybeAutoRetry(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, cls ErrorClass, reason string) bool {
	if item == nil {
		return false
	}
	if !isTransientEscalationClass(cls) {
		return false
	}
	ownsBacklog, err := r.runOwnsBacklogState(ctx, run)
	if err != nil {
		r.logger().Warn("auto-retry: resolve backlog state owner failed; falling through to escalation",
			"run", run.ID, "err", err)
		return false
	}
	if !ownsBacklog {
		return false
	}
	policy := r.policy()
	cap := policy.Pipeline.Retry.EscalationAutoRetryCap
	if cap <= 0 {
		return false
	}
	// Count prior escalated pipeline_runs for this backlog. The
	// current run hasn't been persisted as escalated yet, so prior
	// count + 1 is what we'd be at if this one escalated.
	priorEscalated, err := r.countEscalatedRuns(ctx, item.ID)
	if err != nil {
		r.logger().Warn("auto-retry: count escalated failed; falling through to escalation",
			"backlog", item.ID, "err", err)
		return false
	}
	if priorEscalated >= cap {
		r.logger().Info("auto-retry cap exhausted; escalating",
			"backlog", item.ID, "prior_escalated", priorEscalated, "cap", cap)
		return false
	}

	// Convert: mark this run escalated, but leave the backlog item
	// queued so the reconciler creates a fresh pipeline_run next tick.
	t := r.now()
	run.State = store.PipelineEscalated
	run.EndedAt = &t
	if err := r.Store.Pipeline.PutRun(ctx, run); err != nil {
		r.logger().Warn("auto-retry: persist run escalated failed; falling through to escalation",
			"run", run.ID, "err", err)
		return false
	}
	r.stampEscalationMetadata(ctx, run, cls, reason, "")
	// Requeue only the aggregate state. Metadata may have changed while the run
	// was active, so use the run's claim version and refresh the hook payload.
	current, err := r.Store.Backlog.TransitionState(
		ctx, item.ID, run.AggregateVersion, item.State, store.BacklogQueued,
	)
	if err != nil {
		r.logger().Warn("auto-retry: persist backlog queued failed; will rely on next tick anyway",
			"backlog", item.ID, "err", err)
	} else {
		*item = *current
	}
	mills.PipelineRunsTotal.WithLabelValues(string(store.PipelineEscalated)).Inc()
	mills.PipelineCostUSDTotal.WithLabelValues(string(store.PipelineEscalated)).Add(run.CostUSD)
	mills.EscalationsTotal.WithLabelValues("auto_retried").Inc()
	mills.AutoRequeuesTotal.WithLabelValues(string(cls)).Inc()
	mills.EscalationClassTotal.WithLabelValues(escalationClassLabel(ErrorClass(run.EscalationClass), reason, "")).Inc()
	r.event(ctx, "pipeline.run.auto_retried", "ok", map[string]any{
		"run":             run.ID,
		"backlog":         item.ID,
		"reason":          reason,
		"prior_escalated": priorEscalated,
		"cap":             cap,
	})
	r.logger().Info("pipeline auto-retried transient escalation",
		"run", run.ID, "backlog", item.ID,
		"prior_escalated", priorEscalated, "cap", cap, "reason", reason)
	if r.OnAutoRetry != nil {
		if err := r.OnAutoRetry(ctx, run, item); err != nil {
			r.logger().Warn("pipeline OnAutoRetry hook failed",
				"run", run.ID, "err", err)
		}
	}
	return true
}

func (r *Runner) transientRetryBudgetExhausted(ctx context.Context, item *store.BacklogItem) bool {
	if item == nil {
		return false
	}
	cap := r.policy().Pipeline.Retry.EscalationAutoRetryCap
	if cap <= 0 {
		return true
	}
	prior, err := r.countEscalatedRuns(ctx, item.ID)
	return err == nil && prior >= cap
}

// runOwnsBacklogState distinguishes recursive children, which own a distinct
// backlog aggregate, from Integrator slice children, which share their parent's
// backlog row and must never merge, escalate, or requeue it independently.
func (r *Runner) runOwnsBacklogState(ctx context.Context, run *store.PipelineRun) (bool, error) {
	if run == nil || run.ParentRunID == nil || *run.ParentRunID == "" {
		return true, nil
	}
	parent, err := r.Store.Pipeline.GetRun(ctx, *run.ParentRunID)
	if err != nil {
		return false, err
	}
	return parent.BacklogID != run.BacklogID, nil
}

// countEscalatedRuns counts pipeline_runs in PipelineEscalated state
// for the given backlog id. Used by maybeAutoRetry to enforce the
// EscalationAutoRetryCap. Returns 0 + nil when ListByBacklog is empty.
func (r *Runner) countEscalatedRuns(ctx context.Context, backlogID string) (int, error) {
	runs, err := r.Store.Pipeline.ListByBacklog(ctx, backlogID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pr := range runs {
		if pr.State == store.PipelineEscalated {
			n++
		}
	}
	return n, nil
}

// isTransientEscalationClass reports whether an escalation's DECLARED class is
// one the Slice 2c retry loop can usefully retry. ClassCode and ClassInfra are
// real failures that retry can't fix.
//
// This reads the class the escalating call site passed, not the "[class=…]"
// text in the reason. Substring-matching the prose was the old transport, and
// it diverged in both directions once callers began declaring a class: a
// genuinely transient escalation whose reason happened not to carry the marker
// (e.g. adapter.go's "integrator drive failed: …") never auto-retried, while a
// ClassCode gate exhaustion whose failDetail quoted an embedded
// "[class=transient]" from nested output auto-retried against its own verdict.
func isTransientEscalationClass(cls ErrorClass) bool {
	return cls == ClassTransient || cls == ClassTransientQuota
}

// mergeArtifacts flattens a StageOutput into the JSON map persisted into
// stage_results.artifacts_json, retaining whatever the dispatcher set
// plus the typed fields gates rely on.
func mergeArtifacts(stageID string, out StageOutput) map[string]any {
	dst := map[string]any{}
	for k, v := range out.Artifacts {
		dst[k] = v
	}
	if len(out.FilesChanged) > 0 {
		dst["files_changed"] = out.FilesChanged
	}
	if out.LinesAdded != 0 {
		dst["lines_added"] = out.LinesAdded
	}
	if out.LinesRemoved != 0 {
		dst["lines_removed"] = out.LinesRemoved
	}
	if len(out.DiffPatch) > 0 {
		dst["diff_patch"] = string(out.DiffPatch)
	}
	if len(out.CommitMessages) > 0 {
		dst["commit_messages"] = out.CommitMessages
	}
	if out.MRIID != 0 {
		dst["mr_iid"] = out.MRIID
	}
	if out.MergedSHA != "" {
		dst["merged_sha"] = out.MergedSHA
	}
	if out.WorktreePath != "" {
		dst["worktree_path"] = out.WorktreePath
	}
	dst["stage_id"] = stageID
	return dst
}

// recordJudgeVerdicts persists the numeric verdicts behind an LLM-judged gate
// as store.JudgeVerdictEventKind events, one per consulted judge, so judge
// quality can later be joined against what the run actually did (see
// guard.BuildJudgeCalibrationReport). gate_outcomes has no score column and
// the pass path never renders the score into Reasons, so this is the only
// place the number survives the evaluation.
//
// Best-effort by construction: calibration evidence is worth strictly less
// than a verdict, so an append failure is logged and the gate stands.
func (r *Runner) recordJudgeVerdicts(ctx context.Context, run *store.PipelineRun, gateName string, out gates.Outcome) {
	if r.Store == nil || r.Store.Events == nil || run == nil {
		return
	}
	for _, j := range out.Judgements {
		err := r.Store.Events.Append(ctx, &store.Event{
			Actor:       "pipeline",
			Kind:        store.JudgeVerdictEventKind,
			SubjectKind: store.JudgeVerdictSubjectKind,
			SubjectID:   run.ID,
			Payload: map[string]any{
				"run_id":      run.ID,
				"backlog_id":  run.BacklogID,
				"gate":        gateName,
				"judge_model": j.Model,
				"role":        j.Role,
				"score":       j.Score,
				"threshold":   j.Threshold,
				"pass":        j.Pass,
				"attempt":     run.Attempts,
			},
		})
		if err != nil {
			r.logger().Warn("judge verdict append failed",
				"error", err, "run", run.ID, "gate", gateName, "role", j.Role)
		}
	}
}

func (r *Runner) event(ctx context.Context, kind, outcome string, payload map[string]any) {
	if r.Store == nil || r.Store.Events == nil {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["outcome"] = outcome
	if err := r.Store.Events.Append(ctx, &store.Event{
		Actor:   "pipeline",
		Kind:    kind,
		Payload: payload,
	}); err != nil {
		r.logger().Warn("pipeline append event failed", "error", err, "kind", kind)
	}
}

func (r *Runner) policy() *mills.Policy {
	if r.Policy == nil {
		return mills.Default()
	}
	return r.Policy.Current()
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

// openCrossRepoRun returns the most-recent cross_repo_runs row for the
// backlog item if it sits in a non-terminal state the runner should
// drive. Returns (nil, nil) for the common single-repo case.
//
// "Non-terminal" today is open + gates_green + merging — anything before
// the integrator finishes its job. Merged/reverted/failed rows are
// historical artifacts and should not re-enter the pipeline.
func (r *Runner) openCrossRepoRun(ctx context.Context, item *store.BacklogItem) (*store.CrossRepoRun, error) {
	if r.Store == nil || r.Store.CrossRepo == nil || item == nil {
		return nil, nil
	}
	rows, err := r.Store.CrossRepo.ListByBacklog(ctx, item.ID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: lookup cross_repo for %s: %w", item.ID, err)
	}
	for _, row := range rows {
		if isCrossRepoActive(row.State) {
			return row, nil
		}
	}
	return nil, nil
}

func isCrossRepoActive(s store.CrossRepoState) bool {
	switch s {
	case store.CrossRepoOpen, store.CrossRepoGatesGreen, store.CrossRepoMerging:
		return true
	default:
		return false
	}
}

// handleCrossRepoRun drives a cross-repo run through WaitForGreen +
// AtomicMerge, persisting state transitions on cross_repo_runs and
// closing out the *store.PipelineRun envelope when the integrator
// reaches a terminal state. Per-repo MR creation is intentionally
// out-of-band today (see TODO below).
func (r *Runner) handleCrossRepoRun(
	ctx context.Context,
	cross *store.CrossRepoRun,
	run *store.PipelineRun,
	item *store.BacklogItem,
) error {
	if r.CrossRepoIntegrator == nil {
		return r.escalateWithItem(ctx, run, item, ClassConfig, fmt.Sprintf(
			"cross-repo run %s present but integrator not configured", cross.ID))
	}
	// TODO(slice 4.2 followup): fan out per-repo plan stages; for now
	// assume MRs are created out-of-band by the planner caller.
	greenState, err := r.CrossRepoIntegrator.WaitForGreen(ctx, cross)
	if perr := r.persistCrossState(ctx, cross, greenState); perr != nil {
		r.logger().Warn("crossrepo persist gates_green failed",
			"cross_repo_run", cross.ID, "error", perr)
	}
	if err != nil {
		return r.escalateWithItem(ctx, run, item, Classify(err), fmt.Sprintf(
			"cross-repo wait_for_green: %v", err))
	}
	mergeState, err := r.CrossRepoIntegrator.AtomicMerge(ctx, cross)
	if perr := r.persistCrossState(ctx, cross, mergeState); perr != nil {
		r.logger().Warn("crossrepo persist merge state failed",
			"cross_repo_run", cross.ID, "state", mergeState, "error", perr)
	}
	if err != nil {
		return r.escalateWithItem(ctx, run, item, Classify(err), fmt.Sprintf(
			"cross-repo atomic_merge: %v", err))
	}
	if mergeState != store.CrossRepoMerged {
		return r.escalateWithItem(ctx, run, item, ClassConfig, fmt.Sprintf(
			"cross-repo terminal state %s", mergeState))
	}
	return r.markDone(ctx, run, item)
}

// persistCrossState pushes a state transition onto cross_repo_runs.
// Wraps the DAO so the runner doesn't grow conditional nil checks at
// every call site.
func (r *Runner) persistCrossState(ctx context.Context, cross *store.CrossRepoRun, state store.CrossRepoState) error {
	if r.Store == nil || r.Store.CrossRepo == nil || cross == nil || state == "" {
		return nil
	}
	cross.State = state
	return r.Store.CrossRepo.SetState(ctx, cross.ID, state)
}

// stampEscalationMetadata records the terminal classification metadata on a
// just-escalated run. The historical escalation_class is parsed from the
// reason's "[class=…]" marker so budget accounting keeps its existing
// behaviour; the policy-facing failure class, retryability, and any recognized
// external dependency incident are persisted for downstream history/handoff
// readers. Best-effort: a persist error is logged but never undoes the
// escalation.
func (r *Runner) stampEscalationMetadata(ctx context.Context, run *store.PipelineRun, cls ErrorClass, reason, lastLogTail string) {
	if r.Store == nil || run == nil || run.ID == "" {
		return
	}
	taggedID, taggedDependency := run.ExternalDependencyID, run.ExternalDependency
	md := stampEscalationMetadataOn(ctx, r.Store.Pipeline, run.ID, cls, reason, lastLogTail, r.logger())
	if taggedID != "" {
		md = routeExternalDependencyEscalation(md, taggedID, taggedDependency)
		if err := r.Store.Pipeline.SetEscalationMetadata(ctx, run.ID, md); err != nil {
			r.logger().Warn("pipeline: route external dependency escalation failed", "run", run.ID, "error", err)
		}
	}
	applyEscalationMetadata(run, md)
}

// applyEscalationMetadata copies a just-persisted classification back onto the
// in-memory run. Both escalate paths need this, not just the Runner's: the
// Escalator runs next, inside the same call, and builds its FailureRecord from
// the run struct it is handed (escalate.go BuildRecord reads
// run.EscalationClass). A run whose fields the DB write never touched is
// reported as unclassified while the row says otherwise — and because
// SetEscalationMetadata coalesces on non-empty, the Escalator's needle-inferred
// class then overwrites the caller's declared one on the way back out.
func applyEscalationMetadata(run *store.PipelineRun, md store.EscalationMetadata) {
	if run == nil {
		return
	}
	if md.EscalationClass != "" {
		run.EscalationClass = md.EscalationClass
	}
	if md.FailureClass != "" {
		run.FailureClass = md.FailureClass
	}
	if md.ExternalDependencyID != "" {
		run.ExternalDependencyID = md.ExternalDependencyID
	}
	if md.ExternalDependency != "" {
		run.ExternalDependency = md.ExternalDependency
	}
	if md.Retryable != nil {
		run.EscalationRetryable = md.Retryable
	}
}

// stampEscalationMetadataOn is the Runner/Integrator-shared write. Both escalate
// paths must persist a class: the Integrator's did not stamp AT ALL, so every
// fan-out parent that escalated (sub-run failure, missing integration branch,
// merge failure, merge conflict) landed with escalation_class NULL no matter
// what the caller knew.
func stampEscalationMetadataOn(
	ctx context.Context,
	dao *store.PipelineDAO,
	runID string,
	cls ErrorClass,
	reason, lastLogTail string,
	logger *slog.Logger,
) store.EscalationMetadata {
	md := escalationMetadataFromEvidence(cls, reason, lastLogTail)
	if dao == nil || runID == "" {
		return md
	}
	if md.EscalationClass == "" && md.FailureClass == "" &&
		md.ExternalDependencyID == "" && md.ExternalDependency == "" &&
		md.Retryable == nil {
		return md
	}
	if err := dao.SetEscalationMetadata(ctx, runID, md); err != nil {
		if logger != nil {
			logger.Warn("pipeline: stamp escalation metadata failed",
				"run", runID, "metadata", md, "error", err)
		}
	}
	return md
}

// escalationMetadataFromEvidence builds the persisted classification. cls is
// AUTHORITATIVE: it comes from the escalating call site, which knows what it
// just decided, so nothing here re-derives a fact the caller already had.
//
// The needle fallback survives for exactly one case — a caller that genuinely
// has no verdict to declare (a stored record replayed without one). Inferring a
// class from prose is a last resort, not the transport: the reason embeds
// arbitrary gate/agent-authored text (a gate whose failDetail happens to read
// "connection reset by peer" would otherwise classify a code failure as infra
// and make it auto-requeue-eligible).
func escalationMetadataFromEvidence(cls ErrorClass, reason, lastLogTail string) store.EscalationMetadata {
	var md store.EscalationMetadata
	if !cls.Valid() {
		cls = classifyUnmarkedEscalation(reason, lastLogTail)
	}
	if cls.Valid() {
		failure := FailureClassFromErrorClass(cls)
		retryable := failure.Retryable()
		md.EscalationClass = string(cls)
		md.FailureClass = string(failure)
		md.Retryable = &retryable
	}
	if incident, ok := mcperror.ClassifyExternalCIIncident(strings.TrimSpace(reason + "\n" + lastLogTail)); ok {
		md.ExternalDependencyID = incident.ID
		md.ExternalDependency = incident.Dependency
	}
	return md
}

func classifyExternalStageIncident(err error, out StageOutput) (mcperror.ExternalIncident, bool) {
	if err == nil && out.LogTail == "" {
		return mcperror.ExternalIncident{}, false
	}
	evidence := strings.TrimSpace(out.LogTail)
	if err != nil {
		if evidence != "" {
			evidence += "\n"
		}
		evidence += err.Error()
	}
	return mcperror.ClassifyExternalCIIncident(evidence)
}

// classifyUnmarkedEscalation is the fallback for escalation reasons that carry
// no "[class=…]" marker. It runs the same needle taxonomy every stage error
// goes through (Classify) over the reason plus the last stage log tail.
//
// WHY THIS EXISTS: the class was transported as a marker embedded in a
// human-readable reason string and recovered by re-parsing it, so any escalate
// path that formatted its reason without the marker persisted NO classification
// at all — escalation_class, escalation_failure_class and escalation_retryable
// were all left NULL (stampEscalationMetadata's early return). Roughly half the
// escalateWithItem call sites are in that shape: gate-retry-cap exhaustion, a
// gate with no RetryFrom, drive/adapter/integrator failures, cross-repo, and
// the policy blocks. Those runs bucket as "unclassified" in
// CountEscalationsByClassSince, and autoRequeueBaseClass fails them closed to a
// human — so a genuine infra failure arriving on one of these paths was both
// invisible and permanently stranded. The scope variant of exactly this bug was
// fixed in isolation (see the marker note on the gate-retry-cap escalation);
// this generalizes the fix instead of chasing one format string at a time.
//
// SAFETY: Classify's fallthrough is ClassCode (error_class.go), and
// autoRequeueBaseClass only admits infra/transient/transient_quota — code and
// config fail closed. So unrecognized prose keeps today's exact
// never-auto-requeued behavior, and the only behavior change is that evidence
// matching a KNOWN infra/transient needle becomes correctly classified and
// requeue-eligible. Empty evidence stays unclassified rather than being guessed
// into a class it cannot support.
func classifyUnmarkedEscalation(reason, lastLogTail string) ErrorClass {
	evidence := strings.TrimSpace(strings.TrimSpace(reason) + "\n" + strings.TrimSpace(lastLogTail))
	if evidence == "" {
		return ""
	}
	return Classify(errors.New(evidence))
}

// escalationClassLabel returns the bounded metric label for an escalation's
// terminal fault class. It takes the caller's declared class and validates it
// against the closed ErrorClass taxonomy, falling back to evidence-based
// inference only when the caller has no verdict; anything still unrecognized
// maps to "unclassified" so mills_pipeline_escalation_class_total can never
// mint an unbounded series. Callers increment it co-located with
// mills.EscalationsTotal so the two metrics share one population
// (sum-by-class == sum-by-reason).
func escalationClassLabel(cls ErrorClass, reason, lastLogTail string) string {
	if string(cls) == telemetry.EscalationClassExternalDependency {
		return telemetry.EscalationClassExternalDependency
	}
	if cls.Valid() {
		return string(cls)
	}
	if inferred := classifyUnmarkedEscalation(reason, lastLogTail); inferred.Valid() {
		return string(inferred)
	}
	return "unclassified"
}

// classifyEscalationReason maps the free-form escalation reason string
// into one of a small set of label values bounded enough to keep
// Prometheus cardinality predictable. Anything we don't recognise gets
// "other" so dashboards always see a complete partition.
func classifyEscalationReason(reason string) string {
	switch {
	case strings.Contains(reason, "exceeded"):
		return "retry_cap_exceeded"
	case strings.Contains(reason, "merge conflict"):
		return "integrator_conflict"
	case strings.Contains(reason, "allocate worktree") || strings.Contains(reason, "alloc"):
		return "integrator_alloc_fail"
	case strings.Contains(reason, "gate ") || strings.Contains(reason, "gate:"):
		return "gate_fail"
	case strings.Contains(reason, "errored") || strings.Contains(reason, "stage error"):
		return "stage_error"
	case strings.Contains(reason, "cross-repo"):
		return "cross_repo"
	default:
		return "other"
	}
}

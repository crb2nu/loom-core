package mills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Reconciler is the desired-state loop that turns queued backlog items into
// running pipeline runs. It is the operator's "control law": every tick it
// inspects the canonical store and either starts a new pipeline run, defers
// (deps unmet, budget exhausted), or logs a no-op tick into the events
// table for audit.
//
// The reconciler is intentionally cheap and idempotent. The expensive work
// (per-stage spawn, gate evaluation) lives in PipelineStarter implementations
// (slice 4.x), which the reconciler invokes asynchronously. A long-running
// pipeline run does NOT block subsequent reconcile ticks.
type Reconciler struct {
	Store   *store.Store
	Policy  *PolicyManager
	Budget  *Budget
	Starter PipelineStarter
	Clock   func() time.Time
	Logger  *slog.Logger
	// DispatchLeaseDuration bounds exclusive ownership of one outbox delivery.
	// It is not a worker-execution lease or fencing token; PipelineStarter must
	// still use run-id idempotency after a crash and lease expiry.
	DispatchLeaseDuration time.Duration
	DispatchRetryPolicy   store.DispatchRetryPolicy
	// ClaimFaultHook observes deterministic transactional admission boundaries.
	// Production leaves it nil; reliability tests use it to prove Tick work is
	// bounded independently of queue depth.
	ClaimFaultHook store.ClaimPipelineStartFaultHook

	// HomeProject is the repo this operator executes against by default
	// (the GitLab project it is configured with). It is the reference for the
	// cross-repo gate: a backlog item whose TargetProject names a different
	// repo is "cross-repo" and only runs when CrossRepoPolicy.Enabled. Empty
	// disables the gate (every item is treated as home-repo — pre-cross-repo
	// behavior).
	HomeProject string

	// OperatorSessionID, when set, supplies the operator's CURRENT agent-context
	// session id, stamped onto every backlog-driven pipeline run as
	// ParentSessionID. That id flows to SpawnRequest.ParentSessionID and the
	// LOOM_PARENT_SESSION_ID stage env, so a stage spawn can recall what the
	// operator recorded instead of starting cold. Read through the func on every
	// claim (never frozen at construction): the operator re-establishes its
	// session after a hub outage, and a stale copy would point at a dead row.
	// Nil leaves ParentSessionID empty — pre-continuity behavior.
	OperatorSessionID func() string

	// AutonomyGate, when set, must report ready before the reconciler starts
	// queued work. The operator wires this to its capability matrix so
	// policy.enabled=true is necessary but not sufficient for autonomous
	// writes when required dependencies are missing or stubbed.
	AutonomyGate AutonomyGateFunc

	// SquadRouter, when set, is consulted before handing each pipeline
	// run off to the Starter (Phase 2 v2.0 reconciler integration). The
	// returned squad attribution is emitted as a "reconciler.squad_routed"
	// event keyed on (subject_kind=pipeline_run, subject_id=run.ID) so
	// the squads.OutcomeRecorder can read it back at merge time. Nil
	// keeps v1 behavior — no routing, no event, no attribution.
	SquadRouter SquadRouter

	// ProvenanceStageModels resolves the stage→model map a run is dispatched
	// under, for the run.provenance stamp. Injected because the effective
	// chain is not readable from Policy alone: the LOOM_MILLS_SPAWN_AGENT /
	// LOOM_MILLS_SPAWN_MODEL break-glass is applied by the operator's spawn
	// closure, above ResolveAgentRoute. Nil falls back to policy resolution,
	// which is correct for every deployment that sets no break-glass.
	ProvenanceStageModels func(item *store.BacklogItem) map[string]string

	// ProvenancePromptHashes returns one sha256 (ProvenanceDigest format) per
	// prompt template the operator has wired, for the run.provenance stamp.
	// The templates are compile-time constants in the operator binary, so the
	// map is process-stable and the operator computes it once. Nil stamps an
	// empty map — an unresolvable prompt surface is recorded as unknown, never
	// guessed.
	ProvenancePromptHashes func() map[string]string

	// WorkflowSelector, when set, is consulted for each queued item BEFORE the
	// pipeline claim (S7). A frozen imperative selection routes the item
	// through ClaimWorkflowStart (no pipeline run, no dispatch — the
	// imperative scheduler discovers the run directly); no selection keeps the
	// DAG path byte-identical. Nil disables imperative routing entirely.
	WorkflowSelector WorkflowSelector

	// GhostSparkMRState, when set, enables the ghost-spark reap sweep
	// (SweepGhostSparks): each sweep pass asks GitLab for the MR state of the
	// oldest escalated backlog items whose most-recent run carries an MRIID and
	// transitions escalated→merged the ones whose MR already merged out-of-band
	// (merge-when-pipeline-succeeds landed it after the run escalated at the
	// merge stage). Nil disables the sweep entirely — the reconciler is
	// otherwise unchanged. The GitLab client (pkg/mills/clients) satisfies it.
	GhostSparkMRState MRStateClient
	// GhostSparkMRStateForProject scopes a ghost-spark lookup to the durable
	// project recorded by the run's successful MR lifecycle stages. Production
	// wires GitLabClient.ForProject. When nil, only HomeProject may use the base
	// GhostSparkMRState client; foreign projects fail closed.
	GhostSparkMRStateForProject func(project string) MRStateClient

	// GhostSparkMergedBranch, when set together with GhostSparkBranchesFor,
	// enables the sweep's second pass: escalated items whose most-recent run
	// never recorded an MRIID (it escalated at a scope/docs gate, before the mr
	// stage) but whose deterministic branch was merged by hand afterwards. The
	// IID-driven pass structurally cannot see these — no IID to look up — so
	// without this they stay escalated forever with their work already on main.
	// Nil leaves that pass disabled and the sweep behaves exactly as before.
	GhostSparkMergedBranch MergedBranchMRClient
	// GhostSparkBranchesFor resolves the deterministic branch names an item's
	// work would land on (its source branch plus one per slice). Injected as a
	// func because the canonical contract lives in pkg/mills/pipeline, which
	// imports this package — production wires it from
	// pipeline.BranchContractFor so there is exactly one branch-naming
	// implementation. Nil disables the merged-branch pass.
	GhostSparkBranchesFor func(item *store.BacklogItem) []string
	// GhostSparkMergedBranchForProject scopes a merged-branch lookup to a
	// cross-repo item's target project, authorized ONLY by the immutable
	// escalation-time binding event (EscalationTargetBindingKind) — never by
	// the mutable target_project field. Production wires
	// GitLabClient.ForProject. Nil keeps the merged-branch pass home-only
	// (pre-binding behavior): cross-repo items are left alone.
	GhostSparkMergedBranchForProject func(project string) MergedBranchMRClient

	// GhostSparkResolver, when set, auto-closes the open escalation issue for a
	// ghost spark the sweep reaps. The pipeline Escalator satisfies it via
	// ResolveOnSuccess (it owns the dedup + issue-close GitLab machinery). Nil
	// leaves the issue open — the state transition + metric still run.
	GhostSparkResolver GhostSparkResolver

	// GhostSparkGreenMRAdopter, when set, lets the IID pass merge an escalated
	// run's open-and-green MR instead of leaving it for a human. Nil disables
	// adoption: the sweep then treats every "opened" MR as before.
	GhostSparkGreenMRAdopter GreenMRAdopter

	// AutoRequeueIssueCommenter, when set, posts an "auto-requeued (n/cap)" note
	// on an item's open escalation issue each time the bounded auto-requeue
	// sweep (SweepAutoRequeue) requeues it. Optional: when nil the sweep is
	// log-only. The reconciler does not otherwise carry a GitLab issue client —
	// the pipeline Escalator owns issue machinery — so wiring this is a
	// deferred follow-up (see docs/MILLS.md). The requeue itself, its event,
	// and its metric never depend on this hook.
	AutoRequeueIssueCommenter AutoRequeueIssueCommenter
	// ExternalIncidentRetryDecision applies the persisted paid-retry
	// guardrail before the auto-requeue sweep releases an external incident.
	// Production wires pipeline.RetryPolicy; nil preserves legacy behavior.
	ExternalIncidentRetryDecision func(context.Context, string) (bool, string, error)

	// RegressionMergedMRs and RegressionCommits together enable the post-merge
	// regression attribution sweep (SweepRegressionAttribution): merged MRs on
	// one side, default-branch commits on the other, joined only by Git's
	// canonical revert trailer. Both nil-disable the sweep — attribution
	// without the merged-MR list would have nothing to attribute TO, and
	// without the commit list nothing to attribute FROM. Production wires both
	// from the GitLab client.
	RegressionMergedMRs MergedMRLister
	RegressionCommits   BranchCommitLister
	// RegressionSweepInterval bounds how often the sweep runs; zero uses
	// DefaultRegressionSweepInterval (1h). The reconciler ticks far faster than
	// a revert can plausibly land, so the sweep is rate-limited rather than run
	// every tick.
	RegressionSweepInterval time.Duration
	// RegressionLookback bounds the merged-MR and commit windows; zero uses
	// defaultRegressionLookback (168h).
	RegressionLookback time.Duration
	// RegressionBranch is the branch reverts are read from; empty uses "main".
	RegressionBranch string

	// SignatureEvidenceClassified reports whether the factory's live failure
	// classifiers already explain a piece of escalation evidence. It enables
	// the signature-candidate mining sweep (SweepSignatureMining): nil disables
	// mining entirely, because a miner that cannot tell an explained failure
	// from an unexplained one only proposes signatures the factory already has.
	// Injected as a func because the classifiers live in pkg/mills/pipeline,
	// which imports this package — production wires it from
	// pipeline.KnownFailureSignature so there is exactly one classifier corpus.
	SignatureEvidenceClassified func(text string) bool
	// SignatureMiningInterval bounds how often the mining sweep runs; zero uses
	// DefaultSignatureMiningInterval (6h). The corpus is a two-week window of
	// human-paced escalations, so the sweep is rate-limited rather than run
	// every tick.
	SignatureMiningInterval time.Duration
	// SignatureMiningLookback bounds the evidence window; zero uses
	// defaultSignatureMiningLookback (336h).
	SignatureMiningLookback time.Duration

	// LearningSignals, when set, enables the learning-signal export sweep
	// (SweepLearningSignals): each pass recomputes the judge calibration,
	// promotion evidence and configuration outcome reports over a fixed window
	// and republishes them as Prometheus gauges, so alerting can watch signals
	// that are otherwise only readable as request-time JSON. Nil disables the
	// sweep. Injected because the report builders live in pkg/mills/guard,
	// which imports this package.
	LearningSignals LearningSignalPublisher
	// LearningSignalInterval bounds how often the export sweep runs; zero uses
	// DefaultLearningSignalInterval (30m).
	LearningSignalInterval time.Duration
	// LearningSignalWindow bounds the aggregation window the gauges describe;
	// zero uses defaultLearningSignalWindow (336h).
	LearningSignalWindow time.Duration

	// RepoEnsurer, when set, runs the plan→repo bootstrap pre-flight before a
	// cross-repo item's pipeline dispatches: if the item's TargetProject has no
	// GitLab repo yet AND policy allow-lists its group, the repo is minted so
	// the clone step succeeds instead of escalating on a git-clone 404 (the
	// new-project handoff case). Nil disables the pre-flight — a missing repo
	// falls through to the clone-time terminal escalation (pre-bootstrap
	// behavior). Non-retryable failures park immediately; retryable failures
	// consume the pipeline retry budget. Satisfied by *bootstrap.Service.
	RepoEnsurer RepoEnsurer

	// Now is unset by default; constructors fill it. Public so tests can
	// rewrite it between ticks.

	// ghostSparkRecheck is a test-visible cache of the durable store throttle.
	// EscalationSweeper is its sole production owner; the database remains the
	// authority across process restarts.
	ghostSparkRecheck map[string]time.Time
	// ghostSparkTransition is a deterministic test seam around the atomic
	// backlog+event commit. Production always leaves it nil.
	ghostSparkTransition ghostSparkTransitionFunc
	// ghostSparkIIDContext is a deterministic test seam around the IID pass's
	// reserved-deadline context. Production always leaves it nil.
	ghostSparkIIDContext func(context.Context) (context.Context, context.CancelFunc)

	// spawnBreakerEvents is a deterministic test seam around the spawn-transport
	// breaker's escalation read (see spawn_breaker.go). Production always leaves
	// it nil — the store's EventDAO is used.
	spawnBreakerEvents spawnBreakerEventReader

	// autoRequeueRecheck defers re-inspecting an escalated item the auto-requeue
	// sweep found ineligible (wrong class, per-item cap hit, still cooling,
	// active incident) so a large escalated pile can't pin every tick to the
	// same per-tick scan (see deferAutoRequeueRecheck). Process-local: a restart
	// merely re-inspects each once. Serial ticks, so no locking.
	autoRequeueRecheck map[string]time.Time

	// nextRegressionSweep is the earliest tick at which the regression
	// attribution sweep runs again (see regressionSweepDue). Process-local.
	nextRegressionSweep time.Time

	// nextSignatureMining is the earliest tick at which the signature-candidate
	// mining sweep runs again (see signatureMiningDue). Process-local.
	nextSignatureMining time.Time

	// nextLearningSignals is the earliest tick at which the learning-signal
	// export sweep runs again (see learningSignalDue). Process-local.
	nextLearningSignals time.Time
}

// AutoRequeueIssueCommenter posts a short recurrence note on an escalated item's
// open escalation issue when the bounded auto-requeue sweep requeues it. It is
// optional (nil ⇒ log-only): the reconciler carries no GitLab issue client of
// its own, so a concrete implementation is wired from the operator only when the
// Escalator's issue machinery is exposed to the reconciler (a follow-up). The
// signature takes the note the sweep already composed ("auto-requeued (n/cap)")
// so the implementation is a thin GitLab call.
type AutoRequeueIssueCommenter interface {
	CommentAutoRequeued(ctx context.Context, item *store.BacklogItem, run *store.PipelineRun, note string) error
}

// MRStateClient resolves a merge request's lifecycle state ("opened", "merged",
// "closed", "locked") by IID. Satisfied by *clients.GitLabClient; the ghost-spark
// reap sweep uses it to detect merge-when-pipeline-succeeds merges that landed
// after a run escalated at the merge stage.
type MRStateClient interface {
	MRState(ctx context.Context, mrIID int64) (string, error)
}

// MergedBranchMRClient resolves the most recent MERGED merge request for a
// source branch. Satisfied by *clients.GitLabClient. The ghost-spark sweep uses
// it for the population MRStateClient cannot reach: items whose run escalated
// before the mr stage, so no IID was ever recorded, but whose branch was merged
// by hand afterwards.
type MergedBranchMRClient interface {
	MergedMRForBranch(ctx context.Context, sourceBranch string) (iid int64, mergedAt time.Time, ok bool, err error)
}

// GreenMRAdopter merges an escalated run's still-OPEN merge request when GitLab
// reports it mergeable with a green head pipeline. Satisfied by
// *clients.GitLabClient.
//
// This exists for the CI-infrastructure population. Both other ghost-spark
// passes are archaeology — they close items whose work ALREADY landed. This one
// finishes work that never landed: the 2026-08-02 storm killed pipelines with
// runner_system_failure, runs escalated as if the code were bad, a human retried
// the pipelines and they went green on unchanged code — and then the MRs sat
// open and mergeable with nobody to press merge, because the run is terminal and
// no stage owns it any more (!1390/!1391 waited ~7h for a human).
//
// adopted reports whether this call actually merged the MR; reason carries the
// refusal ("not open", "pipeline not green", …) for the ledger when it did not.
// A nil adopter disables the behavior and the sweep is exactly as before.
type GreenMRAdopter interface {
	AdoptGreenMR(ctx context.Context, mrIID int64) (adopted bool, reason string, err error)
}

// RepoEnsurer makes a cross-repo item's TargetProject exist before its pipeline
// dispatches, minting an empty seeded repo when it is missing and policy allows
// the target's group. created reports whether this call did the minting (vs the
// repo already existing — idempotent). A non-nil error is a transient/config
// failure the reconciler defers on. Satisfied by *bootstrap.Service.EnsureRepo;
// the narrow interface keeps pkg/mills free of a bootstrap import (bootstrap
// depends on store + clients, not pkg/mills).
type RepoEnsurer interface {
	EnsureRepo(ctx context.Context, project, reason string) (created bool, webURL string, err error)
	// SeedPaths returns the repo-root files the mint's root commit creates, so
	// the reconciler can declare them in the minted item's scope. It travels on
	// this interface rather than as a pkg/mills constant for the same reason
	// EnsureRepo does — pkg/mills must not import bootstrap — and so the list
	// can only ever come from the component that actually writes those files.
	SeedPaths() []string
}

// repoEnsureClassifiedError preserves classification without an import cycle.
type repoEnsureClassifiedError interface {
	error
	FailureCode() string
	Retryable() bool
}

const (
	bootstrapFailureEventKind   = "reconciler.bootstrap_failure"
	bootstrapEscalatedEventKind = "reconciler.bootstrap_escalated"
	bootstrapFailureSubjectKind = "bootstrap_target"
)

// GhostSparkResolver closes the open escalation issue for a backlog item whose
// escalated run's MR merged out-of-band. The signature mirrors the pipeline
// Escalator's ResolveOnSuccess so the concrete *pipeline.Escalator satisfies it
// structurally without pkg/mills importing pkg/mills/pipeline (which would be an
// import cycle — pipeline imports mills).
type GhostSparkResolver interface {
	ResolveOnSuccess(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error
}

type ghostSparkTransitionFunc func(
	ctx context.Context,
	id string,
	expectedClaimVersion int64,
	from store.BacklogState,
	to store.BacklogState,
	event *store.Event,
) (*store.BacklogItem, bool, error)

// SquadRouter is the contract the reconciler depends on for v2 squad
// routing. Production wiring satisfies this with *squads.Router; tests
// inject a fake. Returning a SquadDecision with SquadName==FallbackName
// is normal — the reconciler still emits an attribution event so the
// audit trail records the routing decision (even when the choice was
// "none of the configured squads").
type SquadRouter interface {
	Pick(ctx context.Context, item *store.BacklogItem) (SquadDecision, error)
}

// WorkflowSelector is the S7 contract for imperative template resolution.
// The operator wires pkg/mills/workflow's registry-backed resolver through an
// adapter (pkg/mills cannot import pkg/mills/workflow — the workflow package
// sits downstream of worker/pipeline/council, which import this package).
//
// Outcomes:
//   - (nil, "", nil): no selection — take the DAG pipeline path, unchanged.
//   - (sel, "", nil): frozen imperative selection — claim via ClaimWorkflowStart.
//   - (nil, reason, nil): selection cannot start now (workflows disabled, or
//     an invalid selection that authoring guards missed) — the reconciler
//     skips the item with the reason, fail-closed; it never falls back to
//     the DAG over the author's explicit choice.
//   - (_, _, err): infrastructure error — defer and retry next tick.
type WorkflowSelector interface {
	Resolve(ctx context.Context, item *store.BacklogItem, workflowsEnabled bool) (*store.WorkflowSelection, string, error)
}

// SquadDecision is the subset of the squads.Decision shape the reconciler
// needs. Defined here to keep pkg/mills free of an import cycle on
// pkg/mills/squads (squads imports pkg/mills/store, not pkg/mills itself,
// but the operator wiring is cleaner with the contract here too).
type SquadDecision struct {
	SquadName  string
	PathClass  string
	Confidence float64
	SampleSize int
	Reason     string
}

// PipelineStarter spawns a pipeline run for a queued backlog item. The store
// commits the run, backlog claim, admission reservation, workflow metadata,
// transition, and dispatch intent before this interface is called. The
// starter is responsible for driving stages forward (slice 4.x). A nil error
// means "accepted, will report progress via stage_results / events"; an error
// leaves the committed dispatch intent pending for crash-safe retry.
type PipelineStarter interface {
	Start(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error
}

// StartQueuedResult reports the outcome of a manual start request.
type StartQueuedResult struct {
	Run       *store.PipelineRun
	Decision  string
	Reason    string
	Blockers  []string
	BacklogID string
}

// ResumeInFlightResult reports startup recovery work for pipeline runs that
// were active when the previous operator process exited.
type ResumeInFlightResult struct {
	Inspected int
	Resumed   int
	Errored   int
}

// TerminalBacklogSyncResult reports stale running backlog items repaired from
// terminal pipeline state during startup.
type TerminalBacklogSyncResult struct {
	Inspected int
	Updated   int
	Skipped   int
	Errored   int
}

var (
	ErrPolicyDisabled   = errors.New("reconciler: policy disabled")
	ErrBacklogNotQueued = errors.New("reconciler: backlog item is not queued")
)

// AutonomyBlockedError reports a fail-closed autonomy gate.
type AutonomyBlockedError struct {
	Blockers []string
}

func (e *AutonomyBlockedError) Error() string {
	if e == nil || len(e.Blockers) == 0 {
		return "reconciler: autonomy blocked"
	}
	return "reconciler: autonomy blocked: " + strings.Join(e.Blockers, "; ")
}

// AutonomyGateFunc returns whether autonomous pipeline starts are allowed and
// the human-readable blockers when they are not.
type AutonomyGateFunc func(ctx context.Context) (ready bool, blockers []string)

// NewReconciler constructs a Reconciler with sensible defaults. Logger and
// Clock fall back to slog.Default() and time.Now respectively.
func NewReconciler(s *store.Store, pm *PolicyManager, b *Budget, starter PipelineStarter) *Reconciler {
	return &Reconciler{
		Store:                 s,
		Policy:                pm,
		Budget:                b,
		Starter:               starter,
		Clock:                 time.Now,
		Logger:                slog.Default(),
		DispatchLeaseDuration: store.DefaultDispatchLeaseDuration(),
		DispatchRetryPolicy:   store.DefaultDispatchRetryPolicy(),
	}
}

// Tick performs one reconciliation pass. It is safe to call concurrently
// (the SQLite store serialises writers under busy_timeout) but the
// scheduler in scheduler.go drives it sequentially to keep the audit log
// readable.
//
// Tick is the contract for tests: drive it directly with fake stores +
// starters to exercise every transition path.
func (r *Reconciler) Tick(ctx context.Context) (TickResult, error) {
	if r == nil || r.Store == nil {
		return TickResult{}, errors.New("reconciler: not configured")
	}
	tickStart := r.now()
	defer func() {
		ReconcileTickDurationSeconds.Observe(r.now().Sub(tickStart).Seconds())
	}()
	r.refreshDispatchOutboxGauge(ctx)

	// Committed start intents predate the current admission decision. Recover
	// them before policy/autonomy gates so disabling new work cannot strand a run
	// that already consumed budget and transitioned its backlog to running.
	startedThisTick := make(map[string]bool)
	res := TickResult{}
	dispatchStarted, dispatchErrs := r.pickupPendingDispatches(ctx, startedThisTick)
	res.Inspected += dispatchStarted + dispatchErrs
	res.Started += dispatchStarted
	res.Errored += dispatchErrs
	terminalSync, terminalSyncErr := r.SyncTerminalBacklogs(ctx)
	if terminalSyncErr != nil {
		res.Inspected++
		res.Errored++
		r.append(ctx, "reconciler.backlog_terminal_sync_failed", "error", map[string]any{
			"error": terminalSyncErr.Error(),
		})
	} else {
		res.Inspected += terminalSync.Inspected
		res.Errored += terminalSync.Errored
	}

	policy := r.Policy.Current()
	if !policy.IsEnabled() {
		res.SkipReason = "policy disabled"
		outcome := "skipped"
		if res.Inspected > 0 {
			outcome = tickOutcomeLabel(res)
		}
		ReconcileTicksTotal.WithLabelValues(outcome).Inc()
		r.append(ctx, "reconciler.tick", "skipped", map[string]any{
			"reason": "policy disabled", "dispatch_started": dispatchStarted,
			"dispatch_errored": dispatchErrs, "terminal_synced": terminalSync.Updated,
			"terminal_sync_errored": terminalSync.Errored,
		})
		r.refreshDispatchOutboxGauge(ctx)
		return res, nil
	}
	ready, blockers := true, []string(nil)
	if r.AutonomyGate != nil {
		ready, blockers = r.AutonomyGate(ctx)
	}
	// Spawn-transport breaker: hold NEW dispatch while the last few runs all
	// died at the spawn layer for the same reason (a vendor outage). Evaluated
	// only when the gate would otherwise admit work — an already-blocked tick
	// starts nothing, so there is no point spending the read. It joins the same
	// blockers list and the same durable blocked-tick event; in-flight runs and
	// the recovery paths above are deliberately upstream of it.
	if ready {
		if breaker := r.evaluateSpawnBreaker(ctx, policy, r.now()); breaker.Open {
			ready = false
			blockers = append(blockers, breaker.Blocker)
			if r.Logger != nil {
				r.Logger.Warn("spawn transport breaker open; holding dispatch",
					"reason", breaker.Reason, "failures", breaker.Failures,
					"hold_until", breaker.HoldUntil.UTC().Format(time.RFC3339))
			}
		}
	}
	if !ready {
		res.SkipReason = "autonomy blocked"
		outcome := "skipped"
		if res.Inspected > 0 {
			outcome = tickOutcomeLabel(res)
		}
		ReconcileTicksTotal.WithLabelValues(outcome).Inc()
		r.append(ctx, "reconciler.tick", "skipped", map[string]any{
			"reason":           "autonomy blocked",
			"blockers":         blockers,
			"dispatch_started": dispatchStarted,
			"dispatch_errored": dispatchErrs, "terminal_synced": terminalSync.Updated,
			"terminal_sync_errored": terminalSync.Errored,
		})
		r.refreshDispatchOutboxGauge(ctx)
		return res, nil
	}

	queued, err := r.Store.Backlog.ListByStateLimit(
		ctx, store.BacklogQueued, queuedAdmissionBatchSize(policy),
	)
	if err != nil {
		ReconcileTicksTotal.WithLabelValues("errored").Inc()
		return res, fmt.Errorf("read queue: %w", err)
	}
	// Heuristic dispatch ranker (W3.2, default-off). When enabled, reorder the
	// queued slice by expected merge probability so the limited dispatch slots
	// go to the work most likely to merge — chronically-escalating items yield
	// to fresher work. Best-effort: an escalation-history read error keeps the
	// store's FIFO-within-priority order (the ranker is a strict refinement).
	if policy.Pipeline.RankerEnabled {
		escSince := time.Now().Add(-rankerEscalationWindow)
		if escRuns, eErr := r.Store.Pipeline.ListByStateSince(ctx, store.PipelineEscalated, escSince); eErr == nil {
			escCounts := make(map[string]int, len(escRuns))
			for _, run := range escRuns {
				if run != nil {
					escCounts[run.BacklogID]++
				}
			}
			queued = Rank(queued, escCounts, time.Now())
		} else {
			r.append(ctx, "reconciler.ranker_skipped", "warn", map[string]any{"error": eErr.Error()})
		}
	}
	r.refreshActiveGauges(ctx)
	res.Inspected += len(queued)

	// startedThisTick records run IDs already started earlier in this same
	// tick (queued-item launches + queued subruns). pickupInFlightRuns must
	// skip them: a run created by tryStart is non-terminal, so the in-flight
	// re-driver would otherwise re-invoke Starter.Start on it in the SAME
	// tick — double-counting res.Started (and churning a redundant Start the
	// runner's active-guard then no-ops). It looked like a double-start
	// (DEBT-079 #176); the active-guard always prevented an actual second
	// drive, but the count was wrong and flaky under the race scheduler.
	for _, item := range queued {
		decision, run, _, err := r.tryStart(ctx, item, policy)
		if err != nil {
			r.append(ctx, "reconciler.start_failed", "error", map[string]any{
				"item": item.ID, "error": err.Error(),
			})
			res.Errored++
			continue
		}
		switch decision {
		case decisionStarted:
			res.Started++
			if run != nil {
				startedThisTick[run.ID] = true
			}
		case decisionDeferred:
			res.Deferred++
		case decisionSkipped:
			res.Skipped++
		}
	}

	// Phase 6 slice 6.2: also pick up queued subruns. A worker
	// running under this operator may have called the recursion
	// endpoint mid-stage, which inserts a pipeline_runs row in
	// state=queued with parent_run_id != NULL. The Starter knows
	// how to drive an existing run row forward, so the same
	// PipelineStarter wired for backlog-item launches is reused.
	subStarted, subErrs := r.pickupQueuedSubruns(ctx, startedThisTick)
	res.Started += subStarted
	res.Errored += subErrs

	// M7: re-drive non-terminal pipeline_runs whose runner goroutine
	// is no longer alive in this process. A transient HUD error during
	// a spawn poll (or an operator pod rollout) can cause Runner.Drive
	// to exit with a pending stage_results row but no live goroutine
	// scanning it. Without a mid-life re-driver the run wedges until
	// manual intervention. Idempotency is enforced downstream by
	// Runner.Start's r.active.LoadOrStore guard: a live goroutine
	// returns nil + warn log; a missing goroutine gets re-spawned and
	// reattaches to the pending spawn via SpawnResumeClient.Resume.
	//
	// Stays inside Tick's existing policy + autonomy gates above, so a
	// paused operator does not re-drive anything.
	inflightStarted, inflightErrs := r.pickupInFlightRuns(ctx, startedThisTick)
	res.Started += inflightStarted
	res.Errored += inflightErrs
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// Post-merge regression attribution: which merged MRs were later reverted
	// on the default branch. Rate-limited to its own interval (a revert is
	// human-paced) and, like the sweeps above, kept OUT of TickResult — a
	// GitLab outage on a read-only attribution pass must not mark reconcile
	// health errored.
	regression := RegressionSweepResult{}
	if now := r.now(); r.regressionSweepDue(now) {
		// Stamp the next attempt BEFORE running so a persistently failing
		// sweep backs off to its interval instead of retrying every tick.
		r.nextRegressionSweep = now.Add(r.regressionSweepInterval())
		regCtx, regCancel := context.WithTimeout(ctx, regressionSweepTimeout)
		var regErr error
		regression, regErr = r.SweepRegressionAttribution(regCtx)
		regCtxErr := regCtx.Err()
		regCancel()
		if parentErr := ctx.Err(); parentErr != nil {
			return res, fmt.Errorf("regression attribution sweep: %w", parentErr)
		}
		if regErr != nil {
			outcome := "error"
			if errors.Is(regCtxErr, context.DeadlineExceeded) || errors.Is(regErr, context.DeadlineExceeded) {
				outcome = "timeout"
			}
			r.append(ctx, "reconciler.regression_sweep_failed", outcome, map[string]any{
				"error": regErr.Error(),
			})
		}
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// Signature-candidate mining: which unexplained escalations share a shape
	// no live classifier matches. Read-only and advisory — it writes proposals,
	// never classifications — so like the sweeps above it is rate-limited, given
	// its own timeout, and kept OUT of TickResult.
	mining := SignatureMiningSweepResult{}
	if now := r.now(); r.signatureMiningDue(now) {
		// Stamp the next attempt BEFORE running so a persistently failing sweep
		// backs off to its interval instead of retrying every tick.
		r.nextSignatureMining = now.Add(r.signatureMiningInterval())
		minCtx, minCancel := context.WithTimeout(ctx, signatureMiningSweepTimeout)
		var minErr error
		mining, minErr = r.SweepSignatureMining(minCtx)
		minCtxErr := minCtx.Err()
		minCancel()
		if parentErr := ctx.Err(); parentErr != nil {
			return res, fmt.Errorf("signature mining sweep: %w", parentErr)
		}
		if minErr != nil {
			outcome := "error"
			if errors.Is(minCtxErr, context.DeadlineExceeded) || errors.Is(minErr, context.DeadlineExceeded) {
				outcome = "timeout"
			}
			SignatureMiningErrorsTotal.WithLabelValues("sweep").Inc()
			r.append(ctx, "reconciler.signature_mining_sweep_failed", outcome, map[string]any{
				"error": minErr.Error(),
			})
		}
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// Learning-signal export: republish the judge calibration, promotion
	// evidence and configuration outcome reports as gauges so alerting can see
	// them. Read-only over the store and, like the sweeps above, rate-limited,
	// given its own timeout, and kept OUT of TickResult — a window too large to
	// aggregate must not mark reconcile health errored.
	learning := LearningSignalSweepResult{}
	if now := r.now(); r.learningSignalDue(now) {
		// Stamp the next attempt BEFORE running so a persistently failing sweep
		// backs off to its interval instead of retrying every tick.
		r.nextLearningSignals = now.Add(r.learningSignalInterval())
		lsCtx, lsCancel := context.WithTimeout(ctx, learningSignalSweepTimeout)
		var lsErr error
		learning, lsErr = r.SweepLearningSignals(lsCtx)
		lsCtxErr := lsCtx.Err()
		lsCancel()
		if parentErr := ctx.Err(); parentErr != nil {
			return res, fmt.Errorf("learning signal sweep: %w", parentErr)
		}
		if lsErr != nil {
			outcome := "error"
			if errors.Is(lsCtxErr, context.DeadlineExceeded) || errors.Is(lsErr, context.DeadlineExceeded) {
				outcome = "timeout"
			}
			LearningSignalExportErrorsTotal.WithLabelValues("sweep").Inc()
			r.append(ctx, "reconciler.learning_signal_sweep_failed", outcome, map[string]any{
				"error": lsErr.Error(),
			})
		}
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	tickOutcome := tickOutcomeLabel(res)
	ReconcileTicksTotal.WithLabelValues(tickOutcome).Inc()
	r.append(ctx, "reconciler.tick", "ok", map[string]any{
		"inspected": res.Inspected, "started": res.Started,
		"deferred": res.Deferred, "skipped": res.Skipped, "errored": res.Errored,
		"dispatch_started": dispatchStarted, "dispatch_errored": dispatchErrs,
		"terminal_synced": terminalSync.Updated, "terminal_sync_errored": terminalSync.Errored,
		"subrun_started": subStarted, "subrun_errored": subErrs,
		"inflight_started": inflightStarted, "inflight_errored": inflightErrs,
		"regression_commits_scanned":        regression.CommitsScanned,
		"regression_attributed":             regression.Attributed,
		"regression_errored":                regression.Errored,
		"signature_texts_scanned":           mining.TextsScanned,
		"signature_unclassified":            mining.Unclassified,
		"signature_clustered":               mining.Clustered,
		"signature_candidates":              mining.Candidates,
		"signature_mining_errored":          mining.Errored,
		"learning_signal_gates":             learning.Gates,
		"learning_signal_joined_verdicts":   learning.JoinedVerdicts,
		"learning_signal_promotion_actions": learning.PromotionActions,
		"learning_signal_config_runs":       learning.ConfigRuns,
		"learning_signal_regressions":       learning.Regressions,
	})
	r.refreshDispatchOutboxGauge(ctx)
	return res, nil
}

// StartQueuedOptions adjusts how StartQueuedItem treats a non-queued item.
type StartQueuedOptions struct {
	// RequeueEscalated flips an item found in state=escalated back to
	// queued before starting it. This is the sanctioned human "re-run
	// after escalation" path: escalation parks an item for human review,
	// and the reviewing human acting through an admin-authenticated start
	// IS that review. Only escalated qualifies — running/merged items
	// still refuse to start, and paused stays a deliberate operator hold
	// that must be lifted through the backlog upsert.
	RequeueEscalated bool
}

// StartQueuedItem starts one queued backlog item immediately through the same
// dependency, budget, policy, squad-routing, and starter path used by Tick.
func (r *Reconciler) StartQueuedItem(ctx context.Context, backlogID string) (StartQueuedResult, error) {
	return r.StartQueuedItemOpts(ctx, backlogID, StartQueuedOptions{})
}

// StartQueuedItemOpts is StartQueuedItem with explicit options; see
// StartQueuedOptions for the requeue-after-escalation behavior.
func (r *Reconciler) StartQueuedItemOpts(ctx context.Context, backlogID string, opts StartQueuedOptions) (StartQueuedResult, error) {
	if r == nil || r.Store == nil {
		return StartQueuedResult{}, errors.New("reconciler: not configured")
	}
	backlogID = strings.TrimSpace(backlogID)
	if backlogID == "" {
		return StartQueuedResult{}, errors.New("reconciler: backlog id required")
	}
	policy := r.Policy.Current()
	if !policy.IsEnabled() {
		return StartQueuedResult{BacklogID: backlogID, Decision: "skipped", Reason: "policy disabled"}, ErrPolicyDisabled
	}
	if r.AutonomyGate != nil {
		ready, blockers := r.AutonomyGate(ctx)
		if !ready {
			return StartQueuedResult{
				BacklogID: backlogID,
				Decision:  "skipped",
				Reason:    "autonomy blocked",
				Blockers:  blockers,
			}, &AutonomyBlockedError{Blockers: blockers}
		}
	}
	item, err := r.Store.Backlog.Get(ctx, backlogID)
	if err != nil {
		return StartQueuedResult{BacklogID: backlogID, Decision: "error"}, err
	}
	if item.State == store.BacklogEscalated && opts.RequeueEscalated {
		// Route the escalated→queued flip through the guarded transition (the
		// aggregate claim-version + from-state fence) rather than a blind Put,
		// for symmetry with the ghost-spark sweep's closeGhostSpark: a concurrent
		// writer that already moved the item off escalated (or bumped its claim
		// version) now fails cleanly here instead of resurrecting a re-running
		// item via last-writer-wins. Wrong-state callers are unaffected — a
		// non-escalated item never enters this branch and still 409s at the
		// BacklogQueued check below.
		updated, err := r.Store.Backlog.TransitionState(
			ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogQueued,
		)
		if err != nil {
			return StartQueuedResult{BacklogID: backlogID, Decision: "error"}, fmt.Errorf("requeue: %w", err)
		}
		if updated != nil {
			item = updated
		}
		r.append(ctx, "reconciler.requeued", "ok", map[string]any{
			"item": item.ID, "via": "start_endpoint",
		})
	}
	if item.State != store.BacklogQueued {
		return StartQueuedResult{
			BacklogID: backlogID,
			Decision:  "skipped",
			Reason:    fmt.Sprintf("state is %s", item.State),
		}, fmt.Errorf("%w: %s", ErrBacklogNotQueued, item.State)
	}
	decision, run, reason, err := r.tryStart(ctx, item, policy)
	res := StartQueuedResult{Run: run, Decision: decision.String(), BacklogID: backlogID, Reason: reason}
	if err != nil {
		res.Reason = err.Error()
		return res, err
	}
	if run == nil && res.Reason == "" {
		res.Reason = "not started"
	}
	return res, nil
}

// ResumeInFlightRuns starts runner goroutines for non-terminal runs that were
// already past the queued state when this operator process booted. It is
// intended to be called once during startup; normal Tick reconciliation should
// not invoke it or it could duplicate currently-running goroutines.
func (r *Reconciler) ResumeInFlightRuns(ctx context.Context) (ResumeInFlightResult, error) {
	if r == nil || r.Store == nil {
		return ResumeInFlightResult{}, errors.New("reconciler: not configured")
	}
	if r.Starter == nil {
		return ResumeInFlightResult{}, nil
	}
	runs, err := r.Store.Pipeline.ListInFlight(ctx)
	if err != nil {
		return ResumeInFlightResult{}, err
	}
	res := ResumeInFlightResult{Inspected: len(runs)}
	for _, run := range runs {
		item, lerr := r.Store.Backlog.Get(ctx, run.BacklogID)
		if lerr != nil {
			r.append(ctx, "reconciler.resume_failed", "error", map[string]any{
				"run": run.ID, "backlog": run.BacklogID, "error": lerr.Error(),
			})
			res.Errored++
			continue
		}
		if err := r.Starter.Start(ctx, run, item); err != nil {
			r.append(ctx, "reconciler.resume_failed", "error", map[string]any{
				"run": run.ID, "backlog": run.BacklogID, "error": err.Error(),
			})
			res.Errored++
			continue
		}
		r.append(ctx, "reconciler.resumed", "ok", map[string]any{
			"run": run.ID, "backlog": run.BacklogID, "state": string(run.State), "stage": run.CurrentStage,
		})
		res.Resumed++
	}
	if res.Inspected > 0 {
		r.append(ctx, "reconciler.resume_tick", "ok", map[string]any{
			"inspected": res.Inspected, "resumed": res.Resumed, "errored": res.Errored,
		})
	}
	return res, nil
}

// SyncTerminalBacklogs repairs running backlog rows whose pipeline runs already
// reached a terminal state. It processes a bounded batch so it is safe on every
// normal tick as well as startup, skips any backlog with an active run, and only
// mirrors done/escalated/paused terminal pipeline state onto stale backlog rows.
func (r *Reconciler) SyncTerminalBacklogs(ctx context.Context) (TerminalBacklogSyncResult, error) {
	if r == nil || r.Store == nil {
		return TerminalBacklogSyncResult{}, errors.New("reconciler: not configured")
	}
	items, err := r.Store.Backlog.ListTerminalRepairCandidates(
		ctx, terminalBacklogSyncBatchSize,
	)
	if err != nil {
		return TerminalBacklogSyncResult{}, fmt.Errorf("list running backlog: %w", err)
	}
	res := TerminalBacklogSyncResult{Inspected: len(items)}
	for _, item := range items {
		runs, err := r.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if err != nil {
			r.append(ctx, "reconciler.backlog_terminal_sync_failed", "error", map[string]any{
				"backlog": item.ID, "error": err.Error(),
			})
			res.Errored++
			continue
		}
		state, ok := terminalBacklogState(runs)
		if !ok {
			res.Skipped++
			continue
		}
		item.State = state
		if err := r.Store.Backlog.Put(ctx, item); err != nil {
			r.append(ctx, "reconciler.backlog_terminal_sync_failed", "error", map[string]any{
				"backlog": item.ID, "state": string(state), "error": err.Error(),
			})
			res.Errored++
			continue
		}
		r.append(ctx, "reconciler.backlog_terminal_synced", "ok", map[string]any{
			"backlog": item.ID, "state": string(state),
		})
		res.Updated++
	}
	if res.Inspected > 0 {
		r.append(ctx, "reconciler.backlog_terminal_sync_tick", "ok", map[string]any{
			"inspected": res.Inspected, "updated": res.Updated,
			"skipped": res.Skipped, "errored": res.Errored,
		})
	}
	return res, nil
}

func terminalBacklogState(runs []*store.PipelineRun) (store.BacklogState, bool) {
	var latest *store.PipelineRun
	for _, run := range runs {
		if run == nil {
			continue
		}
		if !isTerminalPipelineState(run.State) {
			return "", false
		}
		if latest == nil || run.StartedAt.After(latest.StartedAt) || (run.StartedAt.Equal(latest.StartedAt) && run.Attempts > latest.Attempts) {
			latest = run
		}
	}
	if latest == nil {
		return "", false
	}
	switch latest.State {
	case store.PipelineDone:
		return store.BacklogMerged, true
	case store.PipelineEscalated:
		return store.BacklogEscalated, true
	case store.PipelinePaused:
		return store.BacklogPaused, true
	default:
		return "", false
	}
}

func isTerminalPipelineState(state store.PipelineState) bool {
	switch state {
	case store.PipelineDone, store.PipelineEscalated, store.PipelinePaused:
		return true
	default:
		return false
	}
}

const (
	// ghostSparkGitLabLookupsPerPass caps how many GitLab MR-state lookups the
	// reap sweep performs per pass so a large escalated backlog (91/141 items
	// live 2026-07-17) drains over several passes instead of hammering the API in
	// one pass. Oldest-first ordering makes the drain deterministic.
	ghostSparkGitLabLookupsPerPass = 10
	// ghostSparkResolverTimeout bounds the best-effort escalation issue close
	// after the canonical backlog+event transaction has committed.
	ghostSparkResolverTimeout = 3 * time.Second
	// ghostSparkCandidateBatchSize bounds the escalated-with-MR candidate scan.
	// It intentionally exceeds the lookup cap so candidates whose most-recent run
	// carries no MRIID (skipped without a GitLab call) do not starve the lookup
	// budget by consuming a candidate slot each.
	ghostSparkCandidateBatchSize = 128

	// ghostSparkBranchLookupsPerItem caps how many branch lookups the
	// merged-branch pass spends on a single item. The IID pass costs exactly
	// one GitLab call per item; without this a fan-out item with many slices
	// could drain a whole pass's budget by itself.
	ghostSparkBranchLookupsPerItem = 3

	// ghostSparkBranchLookupsPerPass is the merged-branch pass's own reserved
	// per-pass GitLab budget, separate from ghostSparkGitLabLookupsPerPass.
	// Sharing one counter meant the IID pass — which had ~100 escalated
	// candidates to work through — spent the entire allowance before the
	// branch pass ran, so it closed nothing across 16 consecutive production
	// ticks. A reservation is the only thing that guarantees the newer pass a
	// turn; total GitLab calls per sweep stay bounded at the sum of the two.
	ghostSparkBranchLookupsPerPass = 5
	// ghostSparkRecheckCooldown is how long a non-resolving candidate (MR still
	// open, or closed-abandoned and left escalated) is skipped before the sweep
	// re-checks it. Such items never leave the escalated set, so without a
	// cooldown the oldest-first ordering would let ten of them permanently
	// consume the whole per-pass lookup budget and stall the drain.
)

// deferGhostSparkRecheck advances the durable exponential throttle. The map is
// only a per-process cache owned by the single EscalationSweeper goroutine.
func (r *Reconciler) deferGhostSparkRecheck(ctx context.Context, itemID string, now time.Time, mrIID ...int64) error {
	iid := int64(0)
	if len(mrIID) > 0 {
		iid = mrIID[0]
	} else if runs, err := r.Store.Pipeline.ListByBacklog(ctx, itemID); err == nil {
		if run := mostRecentRun(runs); run != nil && run.MRIID != nil {
			iid = *run.MRIID
		}
	} else {
		return fmt.Errorf("load pipeline runs for escalation recheck %s: %w", itemID, err)
	}
	state, err := r.Store.Backlog.DeferEscalationRecheck(ctx, itemID, iid, now)
	if err != nil {
		return fmt.Errorf("persist escalation recheck %s: %w", itemID, err)
	}
	if r.ghostSparkRecheck == nil {
		r.ghostSparkRecheck = make(map[string]time.Time)
	}
	r.ghostSparkRecheck[itemID] = state.RecheckAfter
	return nil
}

func (r *Reconciler) ghostSparkRecheckDue(ctx context.Context, itemID string, now time.Time, mrIID ...int64) (bool, error) {
	state, err := r.Store.Backlog.EscalationRecheck(ctx, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if len(mrIID) > 0 && err == nil && state.MRIID != mrIID[0] {
		return true, nil
	}
	if until, ok := r.ghostSparkRecheck[itemID]; ok && now.Before(until) {
		return false, nil
	}
	return !now.Before(state.RecheckAfter), nil
}

// GhostSparkSweepResult summarises one ghost-spark reap sweep.
type GhostSparkSweepResult struct {
	// Inspected is the number of GitLab MR-state lookups performed (bounded by
	// ghostSparkGitLabLookupsPerPass).
	Inspected int
	// Merged is the number of items transitioned escalated→merged because their
	// MR had merged out-of-band.
	Merged int
	// MRClosed is the number of items newly recorded as having an abandoned
	// (closed, not merged) MR. Re-observations on later ticks do not recount.
	MRClosed int
	// Errored is the number of GitLab lookups or transitions that failed; the
	// affected item is retried on a later tick.
	Errored int
	// GreenAdopted is the number of escalated items whose still-open MR the
	// sweep merged itself because GitLab reported it green and mergeable. A
	// subset of Merged: every adoption is also a close.
	GreenAdopted int

	// Per-pass counters for the merged-branch pass. Without these a silent
	// sweep is ambiguous — "no escalated item lacks an MR IID" and "candidates
	// exist but none had a merged branch" and "the pass never got to run" all
	// look identical from outside, which is exactly the dead end that made the
	// first production rollout undiagnosable.
	//
	// BranchCandidates is how many escalated-without-MR items the pass saw
	// before any filtering, so zero here means the SQL found nothing rather
	// than the pass being starved or every lookup missing.
	BranchCandidates int
	// BranchInspected is the number of GitLab branch lookups performed.
	BranchInspected int
	// BranchMerged is the number of items closed by the merged-branch pass;
	// it is also counted in Merged.
	BranchMerged int
	// BranchBindingSkipped is the number of candidates skipped fail-closed
	// because their most-recent run carries no escalation-time target binding
	// event, or the binding no longer matches the item's current
	// target_project (the item was retargeted after it escalated). These items
	// stay escalated for a human; the counter makes the skip observable.
	BranchBindingSkipped int
}

// SweepGhostSparks reconciles escalated "ghost spark" backlog items against
// GitLab MR reality. A run can escalate at the merge stage while GitLab's
// merge-when-pipeline-succeeds lands the MR minutes later; the item then sits
// escalated forever even though its work merged, and the "later run succeeds"
// auto-close path never fires (no later run happens). Each sweep pass asks GitLab
// for the MR state of the oldest escalated items whose most-recent run carries
// an MRIID:
//   - MR merged  ⇒ transition the item escalated→merged (respecting the
//     aggregate claim version + the one-way from-state guard), annotate a
//     first-writer event, auto-close the open escalation issue, count "merged".
//   - MR closed  ⇒ leave the item escalated (a human decision) but count
//     "mr_closed" once so the abandoned-MR pile is measurable.
//   - MR open/locked or a non-cancellation lookup error ⇒ leave the item
//     untouched and defer its next check so a poison head cannot starve later
//     candidates. Context cancellation/deadline errors stop the sweep
//     immediately.
//
// Idempotent: a reaped item leaves the escalated set so a later sweep skips it,
// and the counter/issue-close/event side effects are gated on a first-writer
// event so a re-observation is a no-op. Nil MR-state wiring disables the sweep.
func (r *Reconciler) SweepGhostSparks(ctx context.Context) (GhostSparkSweepResult, error) {
	res := GhostSparkSweepResult{}
	if r == nil || r.Store == nil || r.Store.Backlog == nil || r.Store.Pipeline == nil {
		return res, errors.New("reconciler: not configured")
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	// The two passes are independently enabled: the IID pass needs an MR-state
	// client, the merged-branch pass needs its own client plus a branch
	// resolver. Wiring either one alone must work — gating both on the
	// MR-state client would silently disable the branch pass.
	iidPass := r.GhostSparkMRState != nil || r.GhostSparkMRStateForProject != nil
	branchPass := r.GhostSparkMergedBranch != nil && r.GhostSparkBranchesFor != nil
	if !iidPass && !branchPass {
		return res, nil // sweep disabled (no GitLab client wired)
	}
	passCtx := ctx
	iidCancel := func() {}
	if r.ghostSparkIIDContext != nil {
		ctx, iidCancel = r.ghostSparkIIDContext(ctx)
	} else if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		ctx, iidCancel = context.WithDeadline(ctx, time.Now().Add(remaining*2/3))
	}
	defer iidCancel()
	lookups := 0
	now := time.Now()
	var timedOutRecheck struct {
		itemID string
		mrIID  int64
	}
	candidates := []*store.BacklogItem(nil)
	if iidPass {
		var err error
		candidates, err = r.Store.Backlog.ListEscalatedWithMR(ctx, ghostSparkCandidateBatchSize)
		if err != nil {
			if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
				return res, cancelErr
			}
			return res, fmt.Errorf("list escalated-with-mr: %w", err)
		}
	}
iidLoop:
	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if lookups >= ghostSparkGitLabLookupsPerPass {
			break
		}
		if item == nil {
			continue
		}
		// Non-resolving items (MR still open, or closed-abandoned and
		// deliberately left escalated) never leave the escalated set and,
		// oldest-first, would permanently occupy every per-pass lookup slot —
		// starving the merged tail the sweep exists to drain. Skip items
		// inside their re-check cooldown; the map is process-local, so a
		// restart merely re-checks each once.
		runs, lerr := r.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if lerr != nil {
			if cancelErr := contextCancellationError(ctx, lerr); cancelErr != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && passCtx.Err() == nil {
					break iidLoop
				}
				return res, cancelErr
			}
			r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
				"backlog": item.ID, "error": lerr.Error(),
			})
			res.Errored++
			continue
		}
		run := mostRecentRun(runs)
		if run == nil || run.MRIID == nil || *run.MRIID == 0 {
			// The most-recent attempt never opened an MR (e.g. a requeue that
			// escalated before the mr stage) — an earlier attempt's stale MR must
			// not drive a ghost-close. Nothing to reconcile; no GitLab call.
			continue
		}
		due, dueErr := r.ghostSparkRecheckDue(ctx, item.ID, now, *run.MRIID)
		if dueErr != nil {
			return res, fmt.Errorf("ghost-spark recheck state %s: %w", item.ID, dueErr)
		}
		if !due {
			continue
		}
		project, perr := r.Store.Pipeline.AuthorizedProject(ctx, run.ID)
		if perr != nil {
			r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
				"backlog": item.ID, "run": run.ID, "mr_iid": *run.MRIID, "error": perr.Error(),
			})
			if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
				return res, err
			}
			res.Errored++
			continue
		}
		mrState := r.GhostSparkMRState
		if r.GhostSparkMRStateForProject != nil {
			mrState = r.GhostSparkMRStateForProject(project)
		} else if r.HomeProject == "" || !store.SameRepo(project, r.HomeProject) {
			mrState = nil
		}
		if mrState == nil {
			r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
				"backlog": item.ID, "run": run.ID, "mr_iid": *run.MRIID,
				"project": project, "error": "no MR-state client for durable project",
			})
			if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
				return res, err
			}
			res.Errored++
			continue
		}
		lookups++
		res.Inspected++
		state, serr := mrState.MRState(ctx, *run.MRIID)
		if serr != nil {
			if cancelErr := contextCancellationError(ctx, serr); cancelErr != nil {
				// Do not let one oldest, slow MR monopolize every bounded
				// sweep. The cancellation still propagates (and a canceled
				// parent still aborts the tick); this only lets a later tick
				// inspect the next candidate.
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && passCtx.Err() == nil {
					// Persist this throttle after the reserved branch phase. SQLite
					// contention here must not consume the branch allocation.
					if r.ghostSparkRecheck == nil {
						r.ghostSparkRecheck = make(map[string]time.Time)
					}
					r.ghostSparkRecheck[item.ID] = now.Add(30 * time.Minute)
					timedOutRecheck.itemID = item.ID
					timedOutRecheck.mrIID = *run.MRIID
					break iidLoop
				}
				if err := r.deferGhostSparkRecheck(passCtx, item.ID, now, *run.MRIID); err != nil {
					return res, err
				}
				return res, cancelErr
			}
			r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
				"backlog": item.ID, "run": run.ID, "mr_iid": *run.MRIID, "error": serr.Error(),
			})
			if deferErr := r.deferGhostSparkRecheck(passCtx, item.ID, now); deferErr != nil {
				return res, deferErr
			}
			res.Errored++
			continue
		}
		if err := ctx.Err(); err != nil {
			if deferErr := r.deferGhostSparkRecheck(passCtx, item.ID, now); deferErr != nil {
				return res, deferErr
			}
			if errors.Is(err, context.DeadlineExceeded) && passCtx.Err() == nil {
				break iidLoop
			}
			return res, err
		}
		switch strings.ToLower(strings.TrimSpace(state)) {
		case "merged":
			closed, closeErr := r.closeGhostSpark(ctx, item, run, project)
			if closed {
				res.Merged++
			}
			if closeErr != nil {
				return res, closeErr
			}
			if !closed {
				res.Errored++
			}
		case "closed":
			if r.recordGhostSparkMRClosed(ctx, item, run) {
				res.MRClosed++
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
				return res, err
			}
		default:
			// "opened" / "locked". This used to be unconditionally "a genuine
			// escalation awaiting a human", which was true until CI
			// infrastructure started manufacturing escalations: the MR can be
			// open, green and mergeable, with the run terminal so no stage owns
			// pressing merge. Adopt exactly that shape; anything else still
			// waits for a human, just on a deferred re-check so it cannot
			// monopolize the lookup budget.
			adopted, aerr := r.adoptGreenMR(ctx, item, run, *run.MRIID, project, &res)
			if aerr != nil {
				return res, aerr
			}
			if !adopted {
				if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
					return res, err
				}
			}
		}
	}
	// Restore the full pass context so branch lookups always retain their
	// reserved final third even when the IID pass consumes its sub-deadline.
	if err := passCtx.Err(); err != nil {
		return res, err
	}
	if err := r.sweepMergedBranchSparks(passCtx, &res, now); err != nil {
		return res, err
	}
	if timedOutRecheck.itemID != "" {
		if err := passCtx.Err(); err == nil {
			if err := r.deferGhostSparkRecheck(passCtx, timedOutRecheck.itemID, now, timedOutRecheck.mrIID); err != nil {
				return res, err
			}
		} else if !errors.Is(err, context.DeadlineExceeded) {
			return res, err
		}
	}
	if err := passCtx.Err(); err != nil {
		return res, err
	}
	// Also emit when the branch pass merely SAW candidates: a sweep that
	// inspected nothing because every candidate was on cooldown is a different
	// state from one with no candidates at all, and telling them apart from
	// outside is the whole point of the per-pass counters.
	if res.Inspected > 0 || res.Errored > 0 || res.BranchCandidates > 0 {
		r.append(passCtx, "reconciler.ghost_spark_sweep", "ok", map[string]any{
			"inspected": res.Inspected, "merged": res.Merged,
			"mr_closed": res.MRClosed, "errored": res.Errored,
			"green_adopted":          res.GreenAdopted,
			"branch_candidates":      res.BranchCandidates,
			"branch_inspected":       res.BranchInspected,
			"branch_merged":          res.BranchMerged,
			"branch_binding_skipped": res.BranchBindingSkipped,
		})
	}
	return res, nil
}

// sweepMergedBranchSparks is the sweep's second pass: escalated items whose
// most-recent run never recorded an MRIID, whose deterministic branch GitLab
// reports as merged. The IID pass structurally cannot reach these — a run that
// escalated at a scope or docs gate never reached the mr stage, so there is no
// IID to look up — and no later run happens, so the "later run succeeds"
// auto-close never fires either. The work sits on main while the item reads
// escalated forever.
//
// Three guards keep this from closing the wrong item, because "a merged MR
// exists on a branch" is weaker evidence than "this run's MR merged":
//
//  1. Project authorization. These runs have no successful mr/ci stage
//     artifact to resolve a durable project from, so mutable target_project
//     must never route the lookup. Home items use the home client, as before.
//     A cross-repo item is looked up ONLY in the project frozen by its run's
//     escalation-time binding event (EscalationTargetBindingKind) and only
//     while the item still targets that same repo — no binding (a legacy
//     escalation), a retargeted item, or a nil per-project client all skip
//     fail-closed. This is the branch pass's analogue of the IID pass's
//     AuthorizedProject stage provenance.
//  2. Exact branch match, re-checked client-side in MergedMRForBranch.
//  3. The merge must be NEWER than the escalated attempt started. A branch can
//     carry a merged MR from an earlier attempt that was then requeued and
//     escalated again for further work; closing on that stale merge would
//     discard a live escalation. This is the time-based analogue of the IID
//     pass's "an earlier attempt's stale MR must not drive a ghost-close".
//
// Shares the caller's per-pass GitLab lookup budget and the recheck cooldown, so
// enabling it cannot increase the sweep's call volume beyond the existing cap.
// Binding resolution is a local store read, deliberately outside that budget.
func (r *Reconciler) sweepMergedBranchSparks(
	ctx context.Context,
	res *GhostSparkSweepResult,
	now time.Time,
) error {
	if r.GhostSparkMergedBranch == nil || r.GhostSparkBranchesFor == nil {
		return nil // pass disabled
	}
	// RESERVED budget, deliberately not the IID pass's remainder. Sharing one
	// counter starved this pass to zero in production: with ~100 escalated
	// items the IID pass spends all ten lookups on nearly every tick, so the
	// branch pass returned immediately, every tick, and closed nothing for an
	// hour across 16 reconciler ticks. That is the same starvation the IID
	// pass's own recheck cooldown exists to prevent, one level up. Total
	// GitLab calls stay bounded at ghostSparkGitLabLookupsPerPass +
	// ghostSparkBranchLookupsPerPass.
	lookups := 0
	candidates, err := r.Store.Backlog.ListEscalatedWithoutMR(ctx, ghostSparkCandidateBatchSize)
	if err != nil {
		if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
			return cancelErr
		}
		return fmt.Errorf("list escalated-without-mr: %w", err)
	}
	res.BranchCandidates = len(candidates)
	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lookups >= ghostSparkBranchLookupsPerPass {
			break
		}
		if item == nil {
			continue
		}
		due, dueErr := r.ghostSparkRecheckDue(ctx, item.ID, now)
		if dueErr != nil {
			return fmt.Errorf("merged-branch recheck state %s: %w", item.ID, dueErr)
		}
		if !due {
			continue
		}
		target := strings.TrimSpace(item.TargetProject)
		isHome := target == "" || (r.HomeProject != "" && store.SameRepo(target, r.HomeProject))
		// Guard 1 fast path: with no per-project client the pass is home-only,
		// exactly the pre-binding behavior — skip before any store read.
		if !isHome && r.GhostSparkMergedBranchForProject == nil {
			continue
		}
		runs, lerr := r.Store.Pipeline.ListByBacklog(ctx, item.ID)
		if lerr != nil {
			if cancelErr := contextCancellationError(ctx, lerr); cancelErr != nil {
				return cancelErr
			}
			r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
				"backlog": item.ID, "error": lerr.Error(),
			})
			res.Errored++
			continue
		}
		run := mostRecentRun(runs)
		if run == nil {
			continue
		}
		// Guard 1: authorize the lookup project against the immutable
		// escalation-time binding, never the mutable target_project field.
		mrClient, project, ok, clientErr := r.mergedBranchClientFor(ctx, res, item, run, target, isHome, now)
		if clientErr != nil {
			return clientErr
		}
		if !ok {
			continue
		}
		branches := r.GhostSparkBranchesFor(item)
		if len(branches) == 0 {
			continue
		}
		// One heavily-sliced item must not spend the whole tick's budget; the
		// IID pass costs one lookup per item, so keep this comparable.
		if len(branches) > ghostSparkBranchLookupsPerItem {
			branches = branches[:ghostSparkBranchLookupsPerItem]
		}
		var (
			foundIID  int64
			foundWhen time.Time
			found     bool
			lookupErr error
		)
		for _, branch := range branches {
			if strings.TrimSpace(branch) == "" {
				continue
			}
			if lookups >= ghostSparkBranchLookupsPerPass {
				break
			}
			lookups++
			res.Inspected++
			res.BranchInspected++
			iid, mergedAt, ok, serr := mrClient.MergedMRForBranch(ctx, branch)
			if serr != nil {
				if cancelErr := contextCancellationError(ctx, serr); cancelErr != nil {
					if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
						return err
					}
					return cancelErr
				}
				lookupErr = serr
				break
			}
			// Guard 3: the merge must postdate the escalated attempt.
			if ok && !mergedAt.Before(run.StartedAt) {
				foundIID, foundWhen, found = iid, mergedAt, true
				break
			}
		}
		if lookupErr != nil {
			r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
				"backlog": item.ID, "run": run.ID, "error": lookupErr.Error(),
			})
			if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
				return err
			}
			res.Errored++
			continue
		}
		if !found {
			// No merged branch yet (or only a stale pre-attempt merge): a
			// genuine escalation awaiting a human. Defer so it cannot
			// monopolize later budgets.
			if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
				return err
			}
			continue
		}
		closed, closeErr := r.closeGhostSparkWithMR(ctx, item, run, foundIID, "merged_branch", project)
		if closed {
			res.Merged++
			res.BranchMerged++
			r.append(ctx, "reconciler.ghost_spark_merged_branch", "ok", map[string]any{
				"backlog": item.ID, "run": run.ID, "mr_iid": foundIID,
				"merged_at": foundWhen.UTC().Format(time.RFC3339),
				"project":   project,
			})
		}
		if closeErr != nil {
			return closeErr
		}
		if !closed {
			res.Errored++
		}
	}
	return nil
}

// mergedBranchClientFor resolves which MergedBranchMRClient (and project) may
// answer a candidate's branch lookups. Home items keep the home client — with
// one hardening: a home item whose run's escalation-time binding froze a
// FOREIGN project was retargeted home after it escalated, and home-branch
// evidence cannot close it. Cross-repo items require the binding to exist AND
// still match the item's current target; the per-project client is then built
// from the FROZEN project. ok=false means skip this candidate (the item stays
// escalated untouched); binding-based skips defer the recheck cooldown because
// the binding is immutable — re-reading it next tick cannot change the answer.
func (r *Reconciler) mergedBranchClientFor(
	ctx context.Context,
	res *GhostSparkSweepResult,
	item *store.BacklogItem,
	run *store.PipelineRun,
	target string,
	isHome bool,
	now time.Time,
) (MergedBranchMRClient, string, bool, error) {
	var events *store.EventDAO
	if r.Store != nil {
		events = r.Store.Events
	}
	bound, haveBinding, berr := escalationTargetBinding(ctx, events, run.ID)
	if berr != nil {
		if contextCancellationError(ctx, berr) == nil {
			r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
				"backlog": item.ID, "run": run.ID, "error": berr.Error(),
			})
			res.Errored++
		}
		return nil, "", false, berr
	}
	boundHome := bound == "" || (r.HomeProject != "" && store.SameRepo(bound, r.HomeProject))
	if isHome {
		if haveBinding && !boundHome {
			res.BranchBindingSkipped++
			if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
				return nil, "", false, err
			}
			return nil, "", false, nil
		}
		return r.GhostSparkMergedBranch, r.HomeProject, true, nil
	}
	if !haveBinding || !store.SameRepo(bound, target) {
		res.BranchBindingSkipped++
		if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
			return nil, "", false, err
		}
		return nil, "", false, nil
	}
	client := r.GhostSparkMergedBranchForProject(bound)
	if client == nil {
		r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
			"backlog": item.ID, "run": run.ID,
			"project": bound, "error": "no merged-branch client for bound project",
		})
		if err := r.deferGhostSparkRecheck(ctx, item.ID, now); err != nil {
			return nil, "", false, err
		}
		res.Errored++
		return nil, "", false, nil
	}
	return client, bound, true, nil
}

// contextCancellationError distinguishes cancellation from ordinary per-item
// errors so callers stop work instead of turning an expired budget into a
// cascade of retry/audit writes.
func contextCancellationError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// adoptGreenMR merges an escalated run's open MR when GitLab reports it green
// and mergeable, then closes the ghost spark against that same IID. Returns
// whether the MR was adopted (so the caller knows not to defer a re-check).
//
// A refusal is not an error: "not open"/"pipeline not green" is the normal
// answer for a genuine escalation, and recording it every tick would drown the
// ledger — only an actual adoption or a lookup failure is appended.
func (r *Reconciler) adoptGreenMR(
	ctx context.Context,
	item *store.BacklogItem,
	run *store.PipelineRun,
	mrIID int64,
	project string,
	res *GhostSparkSweepResult,
) (bool, error) {
	if r.GhostSparkGreenMRAdopter == nil {
		return false, nil
	}
	adopted, reason, err := r.GhostSparkGreenMRAdopter.AdoptGreenMR(ctx, mrIID)
	if err != nil {
		if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
			return false, cancelErr
		}
		r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
			"backlog": item.ID, "run": run.ID, "mr_iid": mrIID,
			"pass": "green_mr_adoption", "error": err.Error(),
		})
		res.Errored++
		return false, nil
	}
	if !adopted {
		return false, nil
	}
	res.GreenAdopted++
	r.append(ctx, "reconciler.ghost_spark_green_adopted", "ok", map[string]any{
		"backlog": item.ID, "run": run.ID, "mr_iid": mrIID, "reason": reason,
	})
	closed, closeErr := r.closeGhostSparkWithMR(ctx, item, run, mrIID, "adopted_green_mr", project)
	if closed {
		res.Merged++
	}
	if closeErr != nil {
		return true, closeErr
	}
	if !closed {
		res.Errored++
	}
	return true, nil
}

// closeGhostSpark transitions a ghost-spark item escalated→merged, records a
// first-writer "reconciler.ghost_spark_closed" event keyed on the run in the
// same transaction, then auto-closes its open escalation issue (best-effort)
// under a separate bounded context. The aggregate claim version fences a
// concurrent human requeue (escalated→queued), while the atomic event prevents
// a canceled append from leaving an unannotated merged item.
func (r *Reconciler) closeGhostSpark(
	ctx context.Context,
	item *store.BacklogItem,
	run *store.PipelineRun,
	project string,
) (bool, error) {
	return r.closeGhostSparkWithMR(ctx, item, run, derefInt64(run.MRIID), "merged", project)
}

// closeGhostSparkWithMR is closeGhostSpark with the resolved MR identity and
// outcome supplied explicitly. The merged-branch pass discovers its IID from
// GitLab rather than from run.MRIID (which is nil for that population, by
// definition), and records a distinct outcome so the two paths stay
// distinguishable in the event ledger and the closed-total metric. project is
// the repo the MR identity resolves in (MR IIDs are per-project, so a
// cross-repo close is ambiguous without it); empty is tolerated for callers
// that predate project provenance.
func (r *Reconciler) closeGhostSparkWithMR(
	ctx context.Context,
	item *store.BacklogItem,
	run *store.PipelineRun,
	mrIID int64,
	outcome string,
	project string,
) (bool, error) {
	closePayload := map[string]any{
		"backlog_id": item.ID, "run_id": run.ID,
		"mr_iid": mrIID, "outcome": outcome,
	}
	if project != "" {
		closePayload["project"] = project
	}
	event := &store.Event{
		Actor:       "reconciler",
		Kind:        "reconciler.ghost_spark_closed",
		SubjectKind: "pipeline_run",
		SubjectID:   run.ID,
		Payload:     closePayload,
	}
	transition := r.ghostSparkTransition
	if transition == nil {
		transition = r.Store.Backlog.TransitionStateWithEventOnce
	}
	updated, appended, err := transition(ctx, item.ID, item.ClaimVersion, store.BacklogEscalated, store.BacklogMerged, event)
	if err != nil {
		if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
			return false, cancelErr
		}
		r.append(ctx, "reconciler.ghost_spark_failed", "error", map[string]any{
			"backlog": item.ID, "run": run.ID, "mr_iid": derefInt64(run.MRIID),
			"error": err.Error(),
		})
		return false, nil
	}
	if updated != nil {
		*item = *updated
	}
	// The item has left the escalated set for good, so any recheck cooldown we
	// scheduled for it while its MR was still open/closed is now dead weight —
	// prune it so the process-local map tracks only live escalated candidates.
	// delete on a nil map is a no-op, so this is safe before the map is seeded.
	delete(r.ghostSparkRecheck, item.ID)
	if !appended {
		// A prior sweep already reconciled this run — don't double-count or
		// re-close the issue.
		return true, nil
	}
	GhostSparksClosedTotal.WithLabelValues(outcome).Inc()
	// Trustworthy Verdicts S1: the closure means the escalation's work landed,
	// so supersede the run's verdict explicitly. The resolver also recognizes
	// the ghost_spark_closed event above (legacy/retroactive coverage), but the
	// run.verdict.* kind is the canonical contract later sources extend.
	// Best-effort: a failed append leaves the legacy event carrying the same
	// correction.
	if r.Store != nil && r.Store.Events != nil {
		verdictPayload := map[string]any{
			"class":       RunVerdictClassMergedAfterEscalation,
			"prior_class": run.EscalationClass,
			"outcome":     outcome,
			"backlog_id":  item.ID,
			"mr_iid":      mrIID,
		}
		if project != "" {
			verdictPayload["project"] = project
		}
		if _, verr := r.Store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
			Actor:       "reconciler",
			Kind:        RunVerdictKindGhostSparkMerged,
			SubjectKind: "pipeline_run",
			SubjectID:   run.ID,
			Payload:     verdictPayload,
		}); verr != nil && r.Logger != nil {
			r.Logger.Warn("reconciler: run verdict append failed",
				"run", run.ID, "error", verr)
		}
	}
	parentErr := ctx.Err()
	if r.GhostSparkResolver != nil {
		// The state+event transaction is already canonical. Finish its one
		// external issue-close side effect with an independent bounded cleanup
		// context even when the sweep budget expires immediately after commit.
		// This intentionally does not use context.WithoutCancel.
		resolverCtx, resolverCancel := context.WithTimeout(context.Background(), ghostSparkResolverTimeout)
		resolveErr := r.GhostSparkResolver.ResolveOnSuccess(resolverCtx, run, item)
		resolverCanceled := contextCancellationError(resolverCtx, resolveErr)
		resolverCancel()
		if resolveErr != nil && resolverCanceled == nil && r.Logger != nil {
			r.Logger.Warn("reconciler: ghost-spark issue auto-close failed",
				"run", run.ID, "backlog", item.ID, "error", resolveErr)
		}
	}
	if parentErr != nil {
		return true, parentErr
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return true, nil
}

// recordGhostSparkMRClosed counts an escalated item whose MR was closed without
// merging (abandoned). The item is LEFT escalated for a human; a first-writer
// event keyed on the run makes the counter increment exactly once even though
// the item is re-checked on later ticks. Returns true only on the first record.
func (r *Reconciler) recordGhostSparkMRClosed(ctx context.Context, item *store.BacklogItem, run *store.PipelineRun) bool {
	appended, err := r.appendGhostSparkEvent(ctx, "reconciler.ghost_spark_mr_closed", item, run, "mr_closed")
	if err != nil || !appended {
		return false
	}
	GhostSparksClosedTotal.WithLabelValues("mr_closed").Inc()
	return true
}

// appendGhostSparkEvent records a first-writer sweep event keyed on the pipeline
// run so counters/issue-close fire exactly once per run across re-checks. It
// returns (appended, err) mirroring EventDAO.AppendOnceBySubjectKind: appended
// is false when the event already existed. A nil Events store is treated as
// "not appended" without error so a store without events degrades to a no-op.
func (r *Reconciler) appendGhostSparkEvent(ctx context.Context, kind string, item *store.BacklogItem, run *store.PipelineRun, outcome string) (bool, error) {
	if r.Store == nil || r.Store.Events == nil {
		return false, nil
	}
	appended, err := r.Store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       "reconciler",
		Kind:        kind,
		SubjectKind: "pipeline_run",
		SubjectID:   run.ID,
		Payload: map[string]any{
			"backlog_id": item.ID, "run_id": run.ID,
			"mr_iid": derefInt64(run.MRIID), "outcome": outcome,
		},
	})
	if err != nil && contextCancellationError(ctx, err) == nil && r.Logger != nil {
		r.Logger.Warn("reconciler: append ghost-spark event failed",
			"kind", kind, "run", run.ID, "error", err)
	}
	return appended, err
}

// mostRecentRun returns the pipeline run with the latest StartedAt (breaking
// ties on the higher attempt number), or nil for an empty/all-nil slice.
func mostRecentRun(runs []*store.PipelineRun) *store.PipelineRun {
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

// derefInt64 safely dereferences a *int64 (0 for nil) for log/event payloads.
func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

const pendingDispatchBatchSize = 128

const terminalBacklogSyncBatchSize = 128

// maxQueuedAdmissionBatchSize is the hard per-tick admission ceiling when a
// policy leaves MaxConcurrentRuns uncapped. With a configured concurrency cap,
// Tick inspects the smaller of the two limits. Manual StartQueuedItem calls are
// deliberately unaffected.
const maxQueuedAdmissionBatchSize = 128

func queuedAdmissionBatchSize(policy *Policy) int {
	limit := maxQueuedAdmissionBatchSize
	if policy != nil {
		if configured := policy.Budgets.Pipeline.MaxConcurrentRuns; configured > 0 && configured < limit {
			limit = configured
		}
	}
	return limit
}

// pickupPendingDispatches drains the durable start outbox. A claim is visible
// here only after its encompassing SQLite transaction committed, so no caller
// can observe a run without the matching backlog transition, reservation,
// workflow metadata, and transition record. Delivery is deliberately
// at-least-once: if the process dies after Starter accepts but before the
// delivered marker commits, the next process retries and the runner's durable
// stage journal reattaches instead of duplicating the effect.
func (r *Reconciler) pickupPendingDispatches(ctx context.Context, startedThisTick map[string]bool) (int, int) {
	if r == nil || r.Store == nil || r.Starter == nil {
		return 0, 0
	}
	intents, err := r.Store.ClaimPendingDispatches(
		ctx, pendingDispatchBatchSize, r.now(), r.DispatchLeaseDuration,
	)
	if err != nil {
		r.append(ctx, "reconciler.dispatch_pickup_failed", "error", map[string]any{"error": err.Error()})
		return 0, 1
	}
	var started, errored int
	for _, intent := range intents {
		if intent == nil {
			continue
		}
		run, runErr := r.Store.Pipeline.GetRun(ctx, intent.RunID)
		if runErr != nil {
			markErr := r.recordDispatchFailure(ctx, intent, runErr.Error())
			r.append(ctx, "reconciler.dispatch_pickup_failed", "error", map[string]any{
				"intent": intent.ID, "run": intent.RunID,
				"error": errors.Join(runErr, markErr).Error(),
			})
			errored++
			continue
		}
		// A non-queued run proves a prior delivery advanced durable state before
		// its acknowledgment was recorded. Adopt that effect by closing the
		// intent without invoking Starter again; normal in-flight reconciliation
		// may re-drive it on a later tick if execution is still non-terminal.
		if run.State != store.PipelineQueued {
			if err := r.Store.MarkDispatchDelivered(ctx, intent.ID, intent.LeaseToken, r.now()); err != nil {
				r.append(ctx, "reconciler.dispatch_ack_failed", "error", map[string]any{
					"intent": intent.ID, "run": intent.RunID, "error": err.Error(),
				})
				errored++
				continue
			}
			if startedThisTick != nil {
				startedThisTick[run.ID] = true
			}
			r.append(ctx, "reconciler.dispatch_adopted", "ok", map[string]any{
				"intent": intent.ID, "run": intent.RunID, "state": string(run.State),
			})
			started++
			continue
		}
		item, itemErr := r.Store.Backlog.Get(ctx, intent.BacklogID)
		if itemErr != nil {
			markErr := r.recordDispatchFailure(ctx, intent, itemErr.Error())
			r.append(ctx, "reconciler.dispatch_pickup_failed", "error", map[string]any{
				"intent": intent.ID, "run": intent.RunID,
				"backlog": intent.BacklogID,
				"error":   errors.Join(itemErr, markErr).Error(),
			})
			errored++
			continue
		}
		if run.AggregateVersion != intent.AggregateVersion ||
			item.ClaimVersion != intent.AggregateVersion || item.State != store.BacklogRunning {
			_, markErr := r.Store.MarkDispatchFailed(
				ctx, intent.ID, intent.LeaseToken, "dispatch superseded by a newer backlog aggregate",
				r.now(), store.DispatchRetryPolicy{MaxAttempts: 1},
			)
			if markErr != nil {
				r.append(ctx, "reconciler.dispatch_obsolete_failed", "error", map[string]any{
					"intent": intent.ID, "run": intent.RunID, "error": markErr.Error(),
				})
				errored++
				continue
			}
			r.append(ctx, "reconciler.dispatch_obsolete", "skipped", map[string]any{
				"intent": intent.ID, "run": intent.RunID,
				"intent_version": intent.AggregateVersion, "backlog_version": item.ClaimVersion,
			})
			continue
		}
		if err := r.dispatchCommittedStart(ctx, intent, run, item); err != nil {
			errored++
			continue
		}
		if startedThisTick != nil {
			startedThisTick[run.ID] = true
		}
		started++
	}
	return started, errored
}

// dispatchCommittedStart hands one committed outbox intent to the starter and
// acknowledges it only after acceptance. Squad routing and the run-provenance
// stamp are intentionally on this side of the commit so crash recovery
// reconstructs the same attribution even when the original process died
// immediately after ClaimPipelineStart.
func (r *Reconciler) dispatchCommittedStart(
	ctx context.Context,
	intent *store.PendingDispatch,
	run *store.PipelineRun,
	item *store.BacklogItem,
) error {
	if r == nil || r.Starter == nil {
		return nil
	}
	r.routeToSquad(ctx, run, item)
	r.stampRunProvenance(ctx, "pipeline_run", run.ID, "pipeline", item)
	if err := r.Starter.Start(ctx, run, item); err != nil {
		markErr := r.recordDispatchFailure(ctx, intent, err.Error())
		r.append(ctx, "reconciler.start_failed", "starter", map[string]any{
			"item": item.ID, "run": run.ID, "intent": intent.ID, "error": err.Error(),
		})
		if markErr != nil {
			return errors.Join(err, fmt.Errorf("record dispatch failure: %w", markErr))
		}
		return err
	}
	if err := r.Store.MarkDispatchDelivered(ctx, intent.ID, intent.LeaseToken, r.now()); err != nil {
		r.append(ctx, "reconciler.dispatch_ack_failed", "error", map[string]any{
			"item": item.ID, "run": run.ID, "intent": intent.ID, "error": err.Error(),
		})
		return fmt.Errorf("acknowledge dispatch %d: %w", intent.ID, err)
	}
	r.append(ctx, "reconciler.started", "ok", map[string]any{
		"item": item.ID, "run": run.ID, "intent": intent.ID,
		"estimate_usd": item.Budget.MaxCostUSD,
	})
	return nil
}

func (r *Reconciler) recordDispatchFailure(ctx context.Context, intent *store.PendingDispatch, message string) error {
	if r == nil || r.Store == nil || intent == nil {
		return errors.New("record dispatch failure: reconciler not configured")
	}
	result, err := r.Store.MarkDispatchFailed(
		ctx, intent.ID, intent.LeaseToken, message, r.now(), r.DispatchRetryPolicy,
	)
	if err != nil {
		return err
	}
	if result != nil && result.DeadLettered {
		PipelineRunsTotal.WithLabelValues(string(store.PipelineEscalated)).Inc()
		EscalationsTotal.WithLabelValues("dispatch_dead_letter").Inc()
		EscalationClassTotal.WithLabelValues("config").Inc()
		r.append(ctx, "reconciler.dispatch_dead_lettered", "error", map[string]any{
			"intent": intent.ID, "run": intent.RunID, "backlog": intent.BacklogID,
			"attempts": result.Attempts, "error": message,
		})
	}
	return nil
}

// pickupQueuedSubruns looks up every pipeline run created by
// recursion.SubrunGuard but not yet started (state=queued AND
// parent_run_id IS NOT NULL AND attempts=0) and asks the
// PipelineStarter to drive each forward. Errors are logged
// per-row and counted into the tick result; one failure does not
// block the rest. Returns (started, errored) so Tick can roll the
// counters into TickResult.
// startedThisTick, when non-nil, records the run IDs started here so a
// later pickupInFlightRuns in the same tick skips them.
func (r *Reconciler) pickupQueuedSubruns(ctx context.Context, startedThisTick map[string]bool) (int, int) {
	if r.Store == nil || r.Starter == nil {
		return 0, 0
	}
	subruns, err := r.Store.Pipeline.ListQueuedSubruns(ctx)
	if err != nil {
		r.append(ctx, "reconciler.subrun_pickup_failed", "error", map[string]any{"error": err.Error()})
		return 0, 1
	}
	var started, errored int
	for _, run := range subruns {
		// Look up the backlog item the subrun targets so the
		// Starter has the same JobContext shape it gets for a
		// fresh-from-backlog launch. A subrun with a missing
		// item is a corrupted state — log + skip.
		item, lerr := r.Store.Backlog.Get(ctx, run.BacklogID)
		if lerr != nil {
			r.append(ctx, "reconciler.subrun_pickup_failed", "error", map[string]any{
				"run": run.ID, "backlog": run.BacklogID, "error": lerr.Error(),
			})
			errored++
			continue
		}
		if err := r.Starter.Start(ctx, run, item); err != nil {
			r.append(ctx, "reconciler.subrun_start_failed", "error", map[string]any{
				"run": run.ID, "backlog": run.BacklogID, "error": err.Error(),
			})
			errored++
			continue
		}
		if startedThisTick != nil {
			startedThisTick[run.ID] = true
		}
		r.append(ctx, "reconciler.subrun_started", "ok", map[string]any{
			"run": run.ID, "backlog": run.BacklogID, "depth": run.Depth,
			"parent_run": derefString(run.ParentRunID),
		})
		started++
	}
	return started, errored
}

// pickupInFlightRuns re-invokes the PipelineStarter for every non-terminal
// pipeline_runs row. Intended cadence: every Tick. The Starter is expected
// to be idempotent for an already-driving run (production wiring is
// Runner.Start, which uses r.active.LoadOrStore to no-op a duplicate).
//
// Rationale: when a runner goroutine exits with a stage in pending state
// (errStagePending after a transient HUD error, or a panic-recovered Drive)
// nothing else picks the run back up. ResumeInFlightRuns only fires once at
// operator startup. Without this re-driver a transient spawn-poll error
// strands a run until manual escalation.
//
// Errors are logged per-row and counted into the tick result; one failure
// does not block the rest. Returns (started, errored).
//
// skip names run IDs already started earlier in this same tick (queued
// launches + subruns). They are non-terminal so ListInFlight returns them,
// but re-invoking Start in the same tick only double-counts and churns a
// redundant call the runner's active-guard no-ops (DEBT-079). They are
// re-driven on the NEXT tick if their goroutine has since exited.
func (r *Reconciler) pickupInFlightRuns(ctx context.Context, skip map[string]bool) (int, int) {
	if r.Store == nil || r.Starter == nil {
		return 0, 0
	}
	runs, err := r.Store.Pipeline.ListInFlight(ctx)
	if err != nil {
		r.append(ctx, "reconciler.inflight_pickup_failed", "error", map[string]any{"error": err.Error()})
		return 0, 1
	}
	var started, errored int
	for _, run := range runs {
		if skip[run.ID] {
			continue
		}
		item, lerr := r.Store.Backlog.Get(ctx, run.BacklogID)
		if lerr != nil {
			r.append(ctx, "reconciler.inflight_pickup_failed", "error", map[string]any{
				"run": run.ID, "backlog": run.BacklogID, "error": lerr.Error(),
			})
			errored++
			continue
		}
		if err := r.Starter.Start(ctx, run, item); err != nil {
			r.append(ctx, "reconciler.inflight_start_failed", "error", map[string]any{
				"run": run.ID, "backlog": run.BacklogID, "error": err.Error(),
			})
			errored++
			continue
		}
		r.append(ctx, "reconciler.inflight_redriven", "ok", map[string]any{
			"run": run.ID, "backlog": run.BacklogID,
			"state": string(run.State), "stage": run.CurrentStage,
		})
		started++
	}
	return started, errored
}

// derefString safely dereferences a *string for log payloads.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TickResult summarises the work one Tick performed. Useful for tests +
// HUD; the scheduler also exports it as a Prometheus gauge in slice 5.1.
type TickResult struct {
	Inspected  int
	Started    int
	Deferred   int
	Skipped    int
	Errored    int
	SkipReason string
}

// IsNoOp reports whether the tick had nothing to look at — either the
// queue was empty or the policy was disabled. The scheduler uses this to
// decide when to back off to the idle-throttle cadence (slice 6.1).
// Inspected > 0 means the operator is doing meaningful bookkeeping even
// if every item was deferred, so we keep ticking on the fast cadence.
func (r TickResult) IsNoOp() bool {
	return r.Inspected == 0 && r.Started == 0
}

type startDecision int

const (
	decisionStarted  startDecision = iota
	decisionDeferred               // dependencies unmet or budget exhausted
	decisionSkipped                // explicitly out of scope (e.g. paused)
)

func (d startDecision) String() string {
	switch d {
	case decisionStarted:
		return "started"
	case decisionDeferred:
		return "deferred"
	case decisionSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// startImperativeRun commits the S7 workflow start boundary for an item whose
// selection froze to an imperative template. The kernel's CAS + shared-budget
// admission mirror ClaimPipelineStart; no pipeline run and no dispatch intent
// exist afterward — the workflow scheduler discovers the running imperative
// run directly, and the terminal settle (reservation release + item
// escalation) is folded into the run's terminal lifecycle CAS.
func (r *Reconciler) startImperativeRun(
	ctx context.Context,
	item *store.BacklogItem,
	policy *Policy,
	sel *store.WorkflowSelection,
	estimate float64,
) (startDecision, *store.PipelineRun, string, error) {
	claim, err := r.Store.ClaimWorkflowStart(ctx, store.ClaimWorkflowStartRequest{
		BacklogID:            item.ID,
		ExpectedClaimVersion: item.ClaimVersion,
		ExpectedRevision:     item.Revision,
		Selection:            *sel,
		EstimateUSD:          estimate,
		ParentSessionID:      r.operatorSessionID(),
		Limits: store.PipelineStartLimits{
			MaxUSDPerRun:      policy.Budgets.Pipeline.MaxUSDPerRun,
			MaxUSDPerDay:      policy.Budgets.Pipeline.MaxUSDPerDay,
			MaxRunsPerDay:     policy.Budgets.Pipeline.MaxRunsPerDay,
			MaxConcurrentRuns: policy.Budgets.Pipeline.MaxConcurrentRuns,
		},
		Now: r.now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrClaimConflict):
			WorkflowStartClaimsTotal.WithLabelValues("conflict").Inc()
			r.append(ctx, "reconciler.claim_conflict", "skipped", map[string]any{
				"item": item.ID, "expected_version": item.ClaimVersion,
				"expected_revision": item.Revision, "lane": "workflow",
			})
			return decisionSkipped, nil, "already claimed by another reconciler", nil
		default:
			var exceeded *store.BudgetExceededError
			if errors.As(err, &exceeded) {
				WorkflowStartClaimsTotal.WithLabelValues("budget").Inc()
				r.append(ctx, "reconciler.deferred", "budget_transaction", map[string]any{
					"item": item.ID, "reasons": exceeded.Reasons,
					"spent": exceeded.SpentUSD, "reserved": exceeded.ReservedUSD,
					"runs": exceeded.Runs, "active_runs": exceeded.ActiveRuns,
					"lane": "workflow",
				})
				return decisionDeferred, nil, "budget: " + strings.Join(exceeded.Reasons, "; "), nil
			}
			WorkflowStartClaimsTotal.WithLabelValues("error").Inc()
			return decisionDeferred, nil, "", fmt.Errorf("claim workflow start: %w", err)
		}
	}
	if claim == nil || claim.Run == nil {
		return decisionStarted, nil, "", errors.New("claim workflow start: incomplete committed result")
	}
	WorkflowStartClaimsTotal.WithLabelValues("committed").Inc()
	r.append(ctx, "reconciler.workflow_started", "ok", map[string]any{
		"item": item.ID, "run": claim.Run.ID,
		"template":         claim.Run.Template,
		"template_version": claim.Run.TemplateVersion,
	})
	// Attribution parity with the DAG lane: without this, squad routing data
	// silently thins as imperative-lane adoption grows. Same post-commit
	// boundary, subject_kind=workflow_run. Outcome recording stays deferred
	// (see squads.SquadRoutedWorkflowSubjectKind).
	r.routeToSquadSubject(ctx, "workflow_run", claim.Run.ID, "workflow", item)
	r.stampRunProvenance(ctx, "workflow_run", claim.Run.ID, "workflow", item)
	return decisionStarted, nil, "", nil
}

// tryStart evaluates dependencies + budget + policy and either kicks off a
// pipeline run (returning decisionStarted) or defers / skips with a reason
// recorded in the events log.
func (r *Reconciler) tryStart(ctx context.Context, item *store.BacklogItem, policy *Policy) (startDecision, *store.PipelineRun, string, error) {
	// Dependency check: every backlog item in item.Dependencies must be in
	// state=merged. Anything else (running, paused, escalated) blocks.
	if len(item.Dependencies) > 0 {
		ok, blocker, err := r.dependenciesMet(ctx, item)
		if err != nil {
			return decisionDeferred, nil, "", err
		}
		if !ok {
			r.append(ctx, "reconciler.deferred", "deps", map[string]any{
				"item": item.ID, "blocked_by": blocker,
			})
			return decisionDeferred, nil, fmt.Sprintf("blocked by dependency %s (not merged)", blocker), nil
		}
	}

	// Scope-overlap serialization: two concurrent runs whose slices declare
	// files in the same directory tree can only race to a merge conflict
	// (see pkg/mills/scope_overlap.go for the incident this encodes). Defer
	// until the blocking run leaves state=running; the on-merge KickNow
	// picks the deferred item up within ~1s of the blocker landing.
	if policy.Pipeline.SerializeOverlappingScopesEnabled() {
		fairness := policy.Pipeline.ScopeFairness
		if fairness.IsEnabled() {
			blocker, witness, err := r.scopeReservationBlocker(ctx, item, fairness.Hold())
			if err != nil {
				return decisionDeferred, nil, "", fmt.Errorf("scope reservation check: %w", err)
			}
			if blocker != "" {
				r.append(ctx, "reconciler.deferred", "scope_reservation", map[string]any{"item": item.ID, "blocked_by": blocker, "witness": witness})
				return decisionDeferred, nil, fmt.Sprintf("scope reserved by starved item %s (shared scope: %s)", blocker, witness), nil
			}
		}
		blocker, witness, err := r.scopeOverlapBlocker(ctx, item)
		if err != nil {
			return decisionDeferred, nil, "", fmt.Errorf("scope overlap check: %w", err)
		}
		if blocker != "" {
			r.append(ctx, "reconciler.deferred", "scope_overlap", map[string]any{
				"item": item.ID, "blocked_by": blocker, "witness": witness,
			})
			if fairness.IsEnabled() {
				state, tripped, ferr := r.Store.Backlog.RecordScopeDeferral(ctx, item.ID, r.now(), fairness.Deferrals(), fairness.Age())
				if ferr != nil {
					return decisionDeferred, nil, "", fmt.Errorf("record scope deferral: %w", ferr)
				}
				ScopeDeferralCount.WithLabelValues(item.ID).Set(float64(state.DeferralCount))
				ScopeQueueAgeSeconds.WithLabelValues(item.ID).Set(r.now().Sub(state.FirstDeferredAt).Seconds())
				if tripped {
					ScopeStarvationTotal.Inc()
					r.append(ctx, "reconciler.scope_starvation", "reserved", map[string]any{"item": item.ID, "deferral_count": state.DeferralCount, "first_deferred_at": state.FirstDeferredAt, "blocked_by": blocker, "witness": witness})
				}
			}
			return decisionDeferred, nil, fmt.Sprintf(
				"scope overlap with running item %s (shared scope: %s)", blocker, witness), nil
		}
	}

	// The council estimates per-item cost via item.Budget.MaxCostUSD. Admission
	// is evaluated below inside ClaimPipelineStart; doing a separate Budget.Allow
	// read here would both duplicate SQL and reopen the check-then-claim race the
	// reservation transaction is designed to close.
	estimate := item.Budget.MaxCostUSD

	// Policy gate: items flagged require_human_review without an explicit
	// human handoff in flight are deferred — the reconciler doesn't pick
	// them up autonomously; a human (or the escalation path) does.
	if item.Policy.RequireHumanReview {
		r.append(ctx, "reconciler.skipped", "policy", map[string]any{
			"item": item.ID, "reason": "require_human_review=true",
		})
		return decisionSkipped, nil, "require_human_review=true; a human hand-off must start this item", nil
	}

	// Cross-repo gate (fail-closed): an item targeting a repo other than this
	// operator's home repo can only run when cross_repo execution is explicitly
	// enabled. Skipping (rather than running) is the safe default — a misrouted
	// or premature cross-repo item must NEVER land changes in the home repo.
	// The item stays queued until cross_repo is enabled or the target is
	// corrected. HomeProject empty disables the gate (pre-cross-repo behavior).
	if target := strings.TrimSpace(item.TargetProject); target != "" &&
		r.HomeProject != "" && !store.SameRepo(target, r.HomeProject) &&
		!policy.CrossRepo.Enabled {
		r.append(ctx, "reconciler.skipped", "cross_repo", map[string]any{
			"item":           item.ID,
			"target_project": target,
			"home_project":   r.HomeProject,
			"reason":         "cross_repo_disabled",
		})
		return decisionSkipped, nil, fmt.Sprintf(
			"cross_repo disabled: target %s is not home repo %s", target, r.HomeProject), nil
	}

	// Plan→repo bootstrap pre-flight: a cross-repo item whose TargetProject has
	// no GitLab repo yet (a new-project handoff) would fail at the spawn's
	// git-clone and escalate. When bootstrap is enabled AND the target's group
	// is allow-listed, mint the repo now so the clone succeeds; a transient
	// GitLab failure defers the item to retry next tick. When bootstrap is
	// disabled or the group is not allow-listed this is a no-op — a missing repo
	// falls through to the clone-time terminal escalation (the repository-not-
	// found classifier), unchanged from pre-bootstrap behavior.
	if proceed, terminal, preflightReason, err := r.ensureTargetRepo(ctx, policy, item); err != nil {
		return decisionDeferred, nil, "", err
	} else if !proceed {
		if terminal {
			return decisionSkipped, nil, preflightReason, nil
		}
		return decisionDeferred, nil, preflightReason, nil
	}

	// S7 imperative selection: consulted exactly once, BEFORE the pipeline
	// claim, so a frozen selection routes the item through ClaimWorkflowStart
	// (no pipeline run, no dispatch — the imperative scheduler discovers the
	// run directly). No selection keeps the DAG path byte-identical. A
	// selection that cannot start (workflows disabled, invalid template that
	// slipped past authoring guards) SKIPS fail-closed — never a silent DAG
	// fallback over the author's explicit choice.
	if r.WorkflowSelector != nil {
		sel, holdReason, selErr := r.WorkflowSelector.Resolve(ctx, item, policy.WorkflowsEnabled())
		if selErr != nil {
			WorkflowSelectionOutcomesTotal.WithLabelValues("error").Inc()
			return decisionDeferred, nil, "", fmt.Errorf("workflow selection %s: %w", item.ID, selErr)
		}
		if holdReason != "" {
			WorkflowSelectionOutcomesTotal.WithLabelValues("hold").Inc()
			r.append(ctx, "reconciler.skipped", "workflow_selection", map[string]any{
				"item": item.ID, "reason": holdReason,
			})
			return decisionSkipped, nil, holdReason, nil
		}
		if sel != nil {
			WorkflowSelectionOutcomesTotal.WithLabelValues("selected").Inc()
			return r.startImperativeRun(ctx, item, policy, sel, estimate)
		}
		WorkflowSelectionOutcomesTotal.WithLabelValues("none").Inc()
	}

	// Commit the complete start boundary before any external work can run.
	// ClaimPipelineStart's first write is a queued+claim-version CAS; the same
	// SQLite transaction then re-checks admission, allocates the next attempt,
	// creates the pipeline/workflow rows, reserves budget, appends exactly one
	// aggregate transition, and writes a unique dispatch intent. A concurrent
	// reconciler can therefore lose only with ErrClaimConflict, never by
	// leaving a half-created run or oversubscribing a checked cap.
	now := r.now().UTC()
	claimStarted := time.Now()
	claim, err := r.Store.ClaimPipelineStart(ctx, store.ClaimPipelineStartRequest{
		BacklogID:                  item.ID,
		ExpectedClaimVersion:       item.ClaimVersion,
		ExpectedRevision:           item.Revision,
		SerializeOverlappingScopes: policy.Pipeline.SerializeOverlappingScopesEnabled(),
		EnforceScopeReservations:   policy.Pipeline.ScopeFairness.IsEnabled(),
		HomeProject:                r.HomeProject,
		Template:                   policy.Pipeline.DefaultTemplate,
		EstimateUSD:                estimate,
		ParentSessionID:            r.operatorSessionID(),
		Limits: store.PipelineStartLimits{
			MaxUSDPerRun:      policy.Budgets.Pipeline.MaxUSDPerRun,
			MaxUSDPerDay:      policy.Budgets.Pipeline.MaxUSDPerDay,
			MaxRunsPerDay:     policy.Budgets.Pipeline.MaxRunsPerDay,
			MaxConcurrentRuns: policy.Budgets.Pipeline.MaxConcurrentRuns,
		},
		Now:       now,
		FaultHook: r.ClaimFaultHook,
	})
	PipelineStartClaimDurationSeconds.Observe(time.Since(claimStarted).Seconds())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrClaimConflict):
			PipelineStartClaimsTotal.WithLabelValues("conflict").Inc()
			r.append(ctx, "reconciler.claim_conflict", "skipped", map[string]any{
				"item": item.ID, "expected_version": item.ClaimVersion,
				"expected_revision": item.Revision,
			})
			return decisionSkipped, nil, "already claimed by another reconciler", nil
		case errors.Is(err, store.ErrScopeReservationConflict):
			var conflict *store.ScopeReservationConflictError
			if !errors.As(err, &conflict) {
				return decisionSkipped, nil, "scope reservation conflict", err
			}
			PipelineStartClaimsTotal.WithLabelValues("conflict").Inc()
			r.append(ctx, "reconciler.deferred", "scope_reservation_transaction", map[string]any{
				"item": item.ID, "blocked_by": conflict.BlockerID,
				"witness": conflict.Witness,
			})
			return decisionDeferred, nil, fmt.Sprintf(
				"scope reserved by queued item %s (shared scope: %s)",
				conflict.BlockerID, conflict.Witness), nil
		default:
			var scopeConflict *store.ScopeConflictError
			if errors.As(err, &scopeConflict) {
				PipelineStartClaimsTotal.WithLabelValues("conflict").Inc()
				r.append(ctx, "reconciler.deferred", "scope_overlap_transaction", map[string]any{
					"item": item.ID, "blocked_by": scopeConflict.BlockerID,
					"witness": scopeConflict.Witness,
				})
				return decisionDeferred, nil, fmt.Sprintf(
					"scope overlap with running item %s (shared scope: %s)",
					scopeConflict.BlockerID, scopeConflict.Witness), nil
			}
			var exceeded *store.BudgetExceededError
			if errors.As(err, &exceeded) {
				PipelineStartClaimsTotal.WithLabelValues("budget").Inc()
				r.append(ctx, "reconciler.deferred", "budget_transaction", map[string]any{
					"item": item.ID, "reasons": exceeded.Reasons,
					"spent": exceeded.SpentUSD, "reserved": exceeded.ReservedUSD,
					"runs": exceeded.Runs, "active_runs": exceeded.ActiveRuns,
				})
				return decisionDeferred, nil, "budget: " + strings.Join(exceeded.Reasons, "; "), nil
			}
			PipelineStartClaimsTotal.WithLabelValues("error").Inc()
			return decisionDeferred, nil, "", fmt.Errorf("claim pipeline start: %w", err)
		}
	}
	PipelineStartClaimsTotal.WithLabelValues("committed").Inc()
	ScopeDeferralCount.DeleteLabelValues(item.ID)
	ScopeQueueAgeSeconds.DeleteLabelValues(item.ID)
	if claim == nil || claim.Run == nil || claim.Backlog == nil || claim.Dispatch == nil {
		return decisionStarted, nil, "", errors.New("claim pipeline start: incomplete committed result")
	}

	// The outbox remains pending when no starter is configured. That is safer
	// than acknowledging work no process accepted; a later configured process
	// will drain it through pickupPendingDispatches.
	if r.Starter == nil {
		r.append(ctx, "reconciler.claimed", "pending_dispatch", map[string]any{
			"item": claim.Backlog.ID, "run": claim.Run.ID, "intent": claim.Dispatch.ID,
		})
		return decisionStarted, claim.Run, "", nil
	}
	intent, err := r.Store.ClaimPendingDispatch(
		ctx, claim.Dispatch.ID, r.now(), r.DispatchLeaseDuration,
	)
	if err != nil {
		if errors.Is(err, store.ErrDispatchClaimConflict) {
			r.append(ctx, "reconciler.claimed", "dispatch_claimed_elsewhere", map[string]any{
				"item": claim.Backlog.ID, "run": claim.Run.ID, "intent": claim.Dispatch.ID,
			})
			return decisionStarted, claim.Run, "", nil
		}
		return decisionStarted, claim.Run, "", fmt.Errorf("claim committed dispatch: %w", err)
	}
	if err := r.dispatchCommittedStart(ctx, intent, claim.Run, claim.Backlog); err != nil {
		return decisionStarted, claim.Run, "", err
	}
	return decisionStarted, claim.Run, "", nil
}

// ensureTargetRepo runs the plan→repo bootstrap pre-flight for a cross-repo
// item. It returns proceed=true when the item may continue (home-repo item,
// no ensurer wired, bootstrap not engaged for this group, or the repo already
// exists / was just minted). Retryable failures return a deferred result until
// the durable attempt count reaches pipeline.retry.max_attempts; terminal
// failures and exhausted retry budgets atomically park the item as escalated so
// it cannot occupy the fast reconcile cadence forever.
//
// The group allow-list decision (Policy.CrossRepoBootstrapGroupAllowed) is the
// single gate: when it is false the pre-flight does nothing and a missing repo
// falls through to the normal clone-time escalation — no behavior change from
// pre-bootstrap. When it is true, EnsureRepo enforces the same allow-list again
// (defense in depth) and mints the repo if absent.
func (r *Reconciler) ensureTargetRepo(ctx context.Context, policy *Policy, item *store.BacklogItem) (proceed, terminal bool, reason string, err error) {
	if r.RepoEnsurer == nil || item == nil {
		return true, false, "", nil
	}
	target := strings.TrimSpace(item.TargetProject)
	if target == "" || (r.HomeProject != "" && store.SameRepo(target, r.HomeProject)) {
		return true, false, "", nil // home repo always exists
	}
	group := targetProjectGroup(target)
	if !policy.CrossRepoBootstrapGroupAllowed(group) {
		// Bootstrap not enabled or the group is not allow-listed: leave the
		// missing-repo case to the clone-time terminal escalation.
		return true, false, "", nil
	}
	created, _, err := r.RepoEnsurer.EnsureRepo(ctx, target, item.ID)
	if err != nil {
		code, retryable := classifyRepoEnsureFailure(err)
		attempt, recordErr := r.recordBootstrapFailure(ctx, item, target, group, code, retryable, err)
		if recordErr != nil {
			return false, false, "", recordErr
		}
		maxAttempts := policy.Pipeline.Retry.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 3
		}
		terminal = !retryable || attempt >= maxAttempts
		failureReason := fmt.Sprintf(
			"repo bootstrap pre-flight failed for %s (code=%s attempt=%d/%d): %v",
			target, code, attempt, maxAttempts, err,
		)
		if terminal {
			if transitionErr := r.escalateBootstrapFailure(
				ctx, item, target, group, code, retryable, attempt, maxAttempts, err,
			); transitionErr != nil {
				return false, false, "", transitionErr
			}
			return false, true, failureReason + "; item parked escalated for operator repair", nil
		}
		r.append(ctx, "reconciler.deferred", "bootstrap_error", map[string]any{
			"item": item.ID, "target_project": target, "group": group,
			"failure_code": code, "retryable": retryable,
			"attempt": attempt, "max_attempts": maxAttempts, "error": err.Error(),
		})
		return false, false, failureReason, nil
	}
	if created {
		r.append(ctx, "reconciler.bootstrap", "repo_created", map[string]any{
			"item": item.ID, "target_project": target, "group": group,
		})
		if r.Logger != nil {
			r.Logger.Info("mills bootstrap: minted repo for cross-repo item",
				"item", item.ID, "target_project", target, "group", group)
		}
		// Declare the scaffolding we just seeded. The root commit creates
		// README/.gitlab-ci.yml/.gitignore/AGENTS.md, and its own AGENTS.md
		// tells the first implementer to replace the placeholder CI with real
		// fmt/lint/test stages — but the item's slices were authored before the
		// repo existed, so they cannot name a repo-root file, and the scope
		// amendment correctly refuses a root reach (no shared ancestor). The
		// run therefore escalated for doing exactly what we asked it to do
		// (2026-07-27, services/housemd: "1 file(s) outside slice scope:
		// .gitlab-ci.yml", twice on an identical diff). Widening the gate would
		// weaken every established repo; declaring what we seeded costs nothing
		// and is visible on the item.
		r.stampSeedScopeOnMint(ctx, item, target)
	}
	return true, false, "", nil
}

// seedScopeSliceName is the slice the mint appends. Stable and singular so a
// re-mint (or a retried stamp) is idempotent rather than additive.
const seedScopeSliceName = "repo-scaffold"

// stampSeedScopeOnMint appends the bootstrap's seeded repo-root files to the
// item's declared scope, once, on the run that minted the repo.
//
// Best-effort by design: a failure here must not fail the mint. The repo now
// exists, which is the irreversible part; if this write is lost the run behaves
// exactly as it did before this fix (scope-gate escalation), and the next
// reconcile re-attempts the stamp. Mirrors applyScopeAmendment's CAS budget of
// one re-read plus one retry.
func (r *Reconciler) stampSeedScopeOnMint(ctx context.Context, item *store.BacklogItem, target string) {
	if r.Store == nil || r.Store.Backlog == nil || item == nil || r.RepoEnsurer == nil {
		return
	}
	seeded := r.RepoEnsurer.SeedPaths()
	if len(seeded) == 0 {
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		if hasSeedScopeSlice(item.Slices) {
			return // already declared (re-mint, or a prior stamp landed)
		}
		item.Slices = append(item.Slices, store.Slice{
			Name:  seedScopeSliceName,
			Files: seeded,
			Tests: []string{},
		})
		err := r.Store.Backlog.Put(ctx, item)
		if err == nil {
			r.append(ctx, "reconciler.bootstrap", "seed_scope_declared", map[string]any{
				"item": item.ID, "target_project": target,
				"slice": seedScopeSliceName, "files": seeded,
			})
			if r.Logger != nil {
				r.Logger.Info("mills bootstrap: declared seeded scaffold in item scope",
					"item", item.ID, "target_project", target, "files", seeded)
			}
			return
		}
		// CAS lost: re-read and retry once against the fresh row. Drop the
		// optimistic append so the re-check sees the stored slices.
		fresh, ferr := r.Store.Backlog.Get(ctx, item.ID)
		if ferr != nil || fresh == nil {
			if r.Logger != nil {
				r.Logger.Warn("mills bootstrap: seed scope stamp lost",
					"item", item.ID, "target_project", target, "error", err)
			}
			return
		}
		*item = *fresh
	}
	if r.Logger != nil {
		r.Logger.Warn("mills bootstrap: seed scope stamp lost to concurrent writers",
			"item", item.ID, "target_project", target)
	}
}

// hasSeedScopeSlice reports whether the item already declares the mint's
// scaffold slice, keyed on the slice name so the check is independent of how
// many files SeedPaths currently returns.
func hasSeedScopeSlice(slices []store.Slice) bool {
	for _, s := range slices {
		if s.Name == seedScopeSliceName {
			return true
		}
	}
	return false
}

func classifyRepoEnsureFailure(err error) (code string, retryable bool) {
	code, retryable = "unclassified", true
	var classified repoEnsureClassifiedError
	if errors.As(err, &classified) {
		if candidate := strings.TrimSpace(classified.FailureCode()); candidate != "" {
			code = candidate
		}
		retryable = classified.Retryable()
	}
	return code, retryable
}

func bootstrapFailureSubjectID(itemID, target string) string {
	return fmt.Sprintf("%d:%s:%s", len(itemID), itemID, strings.Trim(strings.TrimSpace(target), "/"))
}

// recordBootstrapFailure durably counts an attempt before admission decides.
func (r *Reconciler) recordBootstrapFailure(
	ctx context.Context,
	item *store.BacklogItem,
	target, group, code string,
	retryable bool,
	ensureErr error,
) (int, error) {
	if r.Store == nil || r.Store.Events == nil || item == nil {
		return 0, errors.New("reconciler: bootstrap failure accounting not configured")
	}
	subjectID := bootstrapFailureSubjectID(item.ID, target)
	event := &store.Event{
		OccurredAt:  r.now().UTC(),
		Actor:       "reconciler",
		Kind:        bootstrapFailureEventKind,
		SubjectKind: bootstrapFailureSubjectKind,
		SubjectID:   subjectID,
		Payload: map[string]any{
			"item": item.ID, "target_project": target, "group": group,
			"failure_code": code, "retryable": retryable, "error": ensureErr.Error(),
		},
	}
	if err := r.Store.Events.Append(ctx, event); err != nil {
		return 0, fmt.Errorf("reconciler: record bootstrap failure for %s: %w", item.ID, err)
	}
	attempts, err := r.Store.Events.CountBySubjectKind(
		ctx, bootstrapFailureSubjectKind, subjectID, bootstrapFailureEventKind,
	)
	if err != nil {
		return 0, fmt.Errorf("reconciler: count bootstrap failures for %s: %w", item.ID, err)
	}
	return attempts, nil
}

// escalateBootstrapFailure atomically parks the item and its terminal event.
func (r *Reconciler) escalateBootstrapFailure(
	ctx context.Context,
	item *store.BacklogItem,
	target, group, code string,
	retryable bool,
	attempt, maxAttempts int,
	ensureErr error,
) error {
	terminalEvent := &store.Event{
		OccurredAt:  r.now().UTC(),
		Actor:       "reconciler",
		Kind:        bootstrapEscalatedEventKind,
		SubjectKind: "backlog_item",
		SubjectID:   item.ID,
		Payload: map[string]any{
			"item": item.ID, "target_project": target, "group": group,
			"failure_code": code, "retryable": retryable,
			"attempt": attempt, "max_attempts": maxAttempts, "error": ensureErr.Error(),
		},
	}
	_, err := r.Store.Backlog.TransitionStateWithEvent(
		ctx, item.ID, item.ClaimVersion, store.BacklogQueued, store.BacklogEscalated, terminalEvent,
	)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrStaleWrite) {
		return fmt.Errorf("reconciler: escalate bootstrap failure for %s: %w", item.ID, err)
	}
	current, getErr := r.Store.Backlog.Get(ctx, item.ID)
	if getErr != nil {
		return fmt.Errorf("reconciler: inspect stale bootstrap escalation for %s: %w", item.ID, getErr)
	}
	if current.State != store.BacklogQueued {
		// Another reconciler or human already moved the item. The guarded write
		// did its job; do not turn a benign race into a tick error.
		return nil
	}
	return fmt.Errorf("reconciler: escalate bootstrap failure for %s: %w", item.ID, err)
}

// targetProjectGroup returns the GitLab group path of a project path — every
// segment before the last. "services/familyforge" → "services"; "labs/x/y" →
// "labs/x". A bare name (no slash) or empty input returns "" (no group), which
// the allow-list check treats as not allowed.
func targetProjectGroup(project string) string {
	p := strings.Trim(strings.TrimSpace(project), "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// routeToSquad runs the squad router for a DAG pipeline run and emits an
// attribution event keyed on (subject_kind=pipeline_run, subject_id=run.ID).
func (r *Reconciler) routeToSquad(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) {
	r.routeToSquadSubject(ctx, "pipeline_run", run.ID, "pipeline", item)
}

// routeToSquadSubject runs the squad router and emits an attribution event
// keyed on (subject_kind, subject_id) — pipeline_run for the DAG lane,
// workflow_run for imperative starts (squads.SquadRoutedWorkflowSubjectKind;
// literals here because pkg/mills cannot import pkg/mills/squads). Both
// lanes attribute at the post-commit boundary so crash recovery reconstructs
// identical attribution. Best-effort: router errors and event-append errors
// are logged but do not block the run. When SquadRouter is nil, a no-op.
func (r *Reconciler) routeToSquadSubject(ctx context.Context, subjectKind, runID, lane string, item *store.BacklogItem) {
	if r == nil || r.SquadRouter == nil {
		return
	}
	if r.Store == nil || r.Store.Events == nil {
		return
	}
	if _, err := r.Store.Events.FirstBySubjectKind(
		ctx, subjectKind, runID, "reconciler.squad_routed",
	); err == nil {
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		if r.Logger != nil {
			r.Logger.Warn("reconciler: read stable squad attribution failed",
				"run", runID, "error", err)
		}
		return
	}
	decision, err := r.SquadRouter.Pick(ctx, item)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("reconciler: squad routing failed", "item", item.ID, "error", err)
		}
		return
	}
	payload := map[string]any{
		"run_id":      runID,
		"backlog_id":  item.ID,
		"squad_name":  decision.SquadName,
		"path_class":  decision.PathClass,
		"confidence":  decision.Confidence,
		"sample_size": decision.SampleSize,
		"reason":      decision.Reason,
		"outcome":     "ok",
		"lane":        lane,
	}
	_, err = r.Store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       "reconciler",
		Kind:        "reconciler.squad_routed",
		SubjectKind: subjectKind,
		SubjectID:   runID,
		Payload:     payload,
	})
	if err != nil && r.Logger != nil {
		r.Logger.Warn("reconciler: append squad_routed event failed",
			"error", err, "run", runID)
	}
}

// dependenciesMet returns (true, "", nil) when every Dependency item is
// in state=merged. Otherwise returns the first blocker's id.
func (r *Reconciler) dependenciesMet(ctx context.Context, item *store.BacklogItem) (bool, string, error) {
	for _, dep := range item.Dependencies {
		got, err := r.Store.Backlog.Get(ctx, dep)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// A dependency that no longer exists blocks indefinitely;
				// surface as a clear blocker rather than silently passing.
				return false, dep, nil
			}
			return false, dep, fmt.Errorf("read dep %s: %w", dep, err)
		}
		if got.State != store.BacklogMerged {
			return false, dep, nil
		}
	}
	return true, "", nil
}

func (r *Reconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// operatorSessionID resolves the operator's current agent-context session id,
// or "" when the operator never established one (hub unconfigured/unreachable).
// Resolved per claim so a session re-established after a hub outage is used.
func (r *Reconciler) operatorSessionID() string {
	if r == nil || r.OperatorSessionID == nil {
		return ""
	}
	return strings.TrimSpace(r.OperatorSessionID())
}

// pipelineActiveStates is the set of non-terminal pipeline states the
// reconciler refreshes gauges for. Mirrors the active-list filter in
// handlePipelineRunsList so the dashboard count matches the REST API.
var pipelineActiveStates = []store.PipelineState{
	store.PipelineQueued, store.PipelinePlanning, store.PipelineSlicing,
	store.PipelineImplementing, store.PipelineTesting, store.PipelineReviewing,
	store.PipelineMR, store.PipelineCI, store.PipelineMerging,
}

// refreshActiveGauges samples the per-state active-pipeline counts and
// writes them to PipelineActiveGauge. Called once per tick — cheap
// because the DAO indexes pipeline_runs by state.
func (r *Reconciler) refreshActiveGauges(ctx context.Context) {
	if r.Store == nil || r.Store.Pipeline == nil {
		return
	}
	for _, s := range pipelineActiveStates {
		runs, err := r.Store.Pipeline.ListByState(ctx, s)
		if err != nil {
			continue
		}
		PipelineActiveGauge.WithLabelValues(string(s)).Set(float64(len(runs)))
	}
}

func (r *Reconciler) refreshDispatchOutboxGauge(ctx context.Context) {
	if r == nil || r.Store == nil {
		return
	}
	pending, err := r.Store.CountPendingDispatches(ctx)
	if err != nil {
		return
	}
	PipelineDispatchOutboxPending.Set(float64(pending))
}

// tickOutcomeLabel collapses TickResult into a single label value for
// ReconcileTicksTotal so cardinality stays bounded.
func tickOutcomeLabel(res TickResult) string {
	switch {
	case res.Errored > 0:
		return "errored"
	case res.Started > 0:
		return "started_one"
	case res.Deferred > 0:
		return "deferred"
	case res.Skipped > 0:
		return "skipped"
	default:
		return "no_op"
	}
}

func (r *Reconciler) append(ctx context.Context, kind, outcome string, payload map[string]any) {
	if r.Store == nil || r.Store.Events == nil {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["outcome"] = outcome
	if err := r.Store.Events.Append(ctx, &store.Event{
		Actor:   "reconciler",
		Kind:    kind,
		Payload: payload,
	}); err != nil && r.Logger != nil {
		r.Logger.Warn("reconciler: append event failed", "error", err, "kind", kind)
	}
}

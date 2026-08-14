// Package runner is the operator-side wiring that stitches every
// council component (brief → reviewers → editor → artifacts → judge →
// mutator) into one end-to-end Run() call. Lives in its own package
// so pkg/mills/council and pkg/mills/eval can stay free of each other's
// imports (eval already depends on council; the runner depends on both
// + store + the policy manager, which would create a cycle if it
// lived in either).
package runner

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crb2nu/loom/internal/loomconcurrency"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/eval"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
	sharedpolicy "github.com/crb2nu/loom/pkg/policy"
)

// TransientFailureOutcome is the runner-facing result of routing a classified
// failure. Classification is retained verbatim for requeue and escalation;
// Decision adds durable exhaustion and persistence evidence.
type TransientFailureOutcome struct {
	Classification pipeline.FailureClassification
	Decision       pipeline.FailureRouteDecision
}

// RouteTransientFailure is the runner integration boundary for bounded
// automatic requeues. It never returns a requeue unless Store has atomically
// persisted the claim. The caller should persist Decision on its terminal
// escalation/audit record when Route is FailureRouteEscalate.
func (r *Runner) RouteTransientFailure(ctx context.Context, backlogID string, classification pipeline.FailureClassification, cap int) TransientFailureOutcome {
	var claimer pipeline.TransientRequeueClaimer
	if r != nil && r.Store != nil {
		claimer = r.Store
	}
	return TransientFailureOutcome{
		Classification: classification,
		Decision:       pipeline.DecideTransientRequeue(ctx, claimer, backlogID, classification, cap),
	}
}

// councilReviewerTimeout covers queued inference on the shared local 35B
// backend while keeping every lens independently bounded. Three reviewers run
// in parallel and only a majority is required, so 90 seconds allows two
// serialized slots to complete without extending the whole Council indefinitely.
const (
	councilReviewerTimeout = 90 * time.Second
	councilCleanupTimeout  = 10 * time.Second
)

// ErrBudgetDenied is returned by Admit when the read-only budget preflight
// refuses the run. Handlers map it — and the admission transaction's typed
// *store.CouncilBudgetExceededError — to the same "come back later" response.
var ErrBudgetDenied = errors.New("council budget denied")

// ErrIntentsMissing is returned by Execute when the compiled brief was marked
// intents_missing (the canonical roadmap_intents store is empty) and policy
// requires roadmap intents. Exported so handlers and tests can classify the
// refusal instead of pattern-matching the message.
var ErrIntentsMissing = errors.New("council intents missing")

// StageBudgets bounds each council phase independently so one wedged
// participant cannot hold an admitted run — and the budget reservation it
// carries — open until the 6-hour admission lease expires.
//
// Overall is applied once at the top of Execute rather than in the HTTP
// handler, which is what finally bounds the scheduled (cron) path: it runs on
// the scheduler's root context and has no handler to cap it.
//
// Zero on any field means "use the default" (see withDefaults); a negative
// value means "no independent bound for this stage" — the operator escape
// hatch for a workload the default is wrong for.
type StageBudgets struct {
	Overall   time.Duration
	Brief     time.Duration
	Reviewers time.Duration
	Debate    time.Duration
	Editor    time.Duration
	Artifacts time.Duration
	Judge     time.Duration
	Persist   time.Duration
	Mutator   time.Duration
}

// DefaultStageBudgets sizes Overall to cover the two slow stages back to back
// (debate 10m + editor 8m) plus change; Reviewers is an envelope over the
// existing per-lens councilReviewerTimeout rather than a replacement for it.
// These are deliberately generous first values — tighten them from the
// observed distribution of legitimate production passes.
func DefaultStageBudgets() StageBudgets {
	return StageBudgets{
		Overall:   20 * time.Minute,
		Brief:     2 * time.Minute,
		Reviewers: 3 * time.Minute,
		Debate:    10 * time.Minute,
		Editor:    8 * time.Minute,
		Artifacts: 1 * time.Minute,
		Judge:     3 * time.Minute,
		Persist:   30 * time.Second,
		Mutator:   2 * time.Minute,
	}
}

// withDefaults fills unset (zero) fields from DefaultStageBudgets so a
// zero-value Runner — every existing caller and test — is bounded too.
func (b StageBudgets) withDefaults() StageBudgets {
	d := DefaultStageBudgets()
	if b.Overall == 0 {
		b.Overall = d.Overall
	}
	if b.Brief == 0 {
		b.Brief = d.Brief
	}
	if b.Reviewers == 0 {
		b.Reviewers = d.Reviewers
	}
	if b.Debate == 0 {
		b.Debate = d.Debate
	}
	if b.Editor == 0 {
		b.Editor = d.Editor
	}
	if b.Artifacts == 0 {
		b.Artifacts = d.Artifacts
	}
	if b.Judge == 0 {
		b.Judge = d.Judge
	}
	if b.Persist == 0 {
		b.Persist = d.Persist
	}
	if b.Mutator == 0 {
		b.Mutator = d.Mutator
	}
	return b
}

// Runner stitches the slice 3.1–3.6 components into a single end-to-end
// council pass: roadmap extract → brief → reviewers → editor →
// artifacts → judge → mutator. It is the surface the operator's REST
// handlers + scheduler trigger; the FakeReviewer + FakeEditor make the
// dryrun path exercise every step without a live agent.
//
// Wiring is dependency-injected so production swaps in the spawn-backed
// reviewer + editor without touching this file.
//
// This is the council runner, not the imperative workflow runtime in
// pkg/mills/workflow. It intentionally has no workflow KPI lifecycle hooks:
// workflow_active_runs, workflow_completed_steps, and workflow_failed_steps
// are snapshot queries over Store.Workflow's durable journal. Adding
// increment/decrement callbacks here would count council activity as workflow
// activity and would drift whenever this process restarts.
type Runner struct {
	Store     *store.Store
	Policy    *mills.PolicyManager
	Budget    *mills.Budget
	Reviewers *council.Dispatcher
	Editor    council.Editor
	// Moderator is the optional Phase 5 dependency wired only when
	// `policy.debate.enabled.<trigger>` is true. When nil (the v1
	// default) the runner skips the debate path even if the policy
	// flag is set, falling back to single-pass and logging the
	// degradation. Production wires a frontier-model moderator.
	Moderator council.Moderator
	Writer    *council.ArtifactWriter
	Mutator   *council.BacklogMutator
	Judge     *eval.Judge
	RepoRoot  string
	Logger    *slog.Logger

	// ConcurrencyPolicy bounds simultaneous runner admissions. Its zero value
	// preserves the pre-policy default; an invalid explicit value is retained
	// as an admission error so no run can bypass malformed policy.
	ConcurrencyPolicy sharedpolicy.PipelineConcurrencyPolicy
	concurrencyOnce   sync.Once
	concurrency       *loomconcurrency.Concurrency
	concurrencyErr    error

	// Signals, when set, feeds recent workspace pain (Loki error clusters)
	// into the council brief so proposals are grounded in real failures
	// (W3.1 of .loom/126). Optional — nil omits the workspace-signals
	// section. SignalWindow defaults to 24h when zero.
	Signals      council.WorkspaceSignalSource
	SignalWindow time.Duration

	// FactoryExhaust, when set, feeds the mill's own open self-maintenance
	// issues (quarantined flaky tests, audit-advisory digests) into the
	// council brief so an unattended shift has machine-filed demand to draw
	// on. Optional — nil omits the section. Bounds and the on/off switch come
	// from policy per-run (council.sources.factory_exhaust), so a hot reload
	// takes effect without a restart.
	FactoryExhaust council.FactoryExhaustSource

	// HealthGates, when configured, supplies the infrastructure gate verdict
	// that council.ApplyClassificationPolicy requires before a run's proposals
	// may reach the artifact writer, the judge, or the backlog. Nil — the
	// default, and what cmd/loom-mills-operator wires today — leaves the
	// classified planning policy unapplied so the run is byte-identical to the
	// pre-policy path. This mirrors pipeline.Runner.HealthGates: an
	// unconfigured gate is a no-op, but a configured one is fail-closed.
	HealthGates HealthGatePreflight

	// StageBudgets bounds each phase (and the whole pass) independently.
	// The zero value is fully defaulted, so leaving it unset still bounds
	// every stage; main.go overrides it from LOOM_MILLS_COUNCIL_*_TIMEOUT.
	StageBudgets StageBudgets

	// Now is injectable for deterministic IDs in tests + dryrun. Defaults
	// to time.Now.
	Now func() time.Time

	// OnArtifactsCommitted fires after a non-dryrun council run has
	// successfully persisted its artifacts + verdict + (optionally)
	// backlog deltas. The audit Triggers wire here to enqueue an
	// adversarial review against the freshly-committed artifact set.
	// Errors are logged but do NOT roll back the run.
	OnArtifactsCommitted func(ctx context.Context, run *store.CouncilRun, refs []store.ArtifactRef)
}

// HealthGatePreflight supplies the latest infrastructure gate verdict. It is
// declared consumer-side, structurally identical to pipeline.HealthGatePreflight,
// so pipeline.FailClosedPreflight satisfies it without the council runner
// importing pkg/mills/pipeline (which imports council).
type HealthGatePreflight interface {
	DecideHealthGates(ctx context.Context) (gates.HealthDecision, error)
}

// RunInput tunes one Run() invocation.
type RunInput struct {
	// Trigger identifies what fired the run; surfaced into council_runs.
	Trigger store.CouncilTrigger
	// Dryrun makes the run write artifacts to a scratch dir under
	// RepoRoot/.loom/dryrun/<runID>/ instead of .loom/, skip backlog
	// mutation, and return the populated RunResult so the caller can
	// inspect what *would* have happened.
	Dryrun bool
	// Reason is a free-form note (e.g. "manual via CLI") logged into
	// council_runs.notes.
	Reason string
}

// RunResult is the audit footprint of one Run call.
type RunResult struct {
	RunID         string
	Brief         *council.Brief
	Reviews       []council.ReviewerOutput
	Editor        *council.EditorOutput
	Write         *council.WriteResult
	Verdict       *eval.Verdict
	Mutation      *council.MutationResult
	Dryrun        bool
	StartedAt     time.Time
	EndedAt       time.Time
	CostUSDApprox float64
}

// Run executes a council pass end to end as the composition of Admit (commit
// the durable attempt) and Execute (do the long, expensive work). Errors fall
// into two buckets:
//   - infrastructure: brief/reviewers/editor/writer/judge couldn't run
//     at all → return non-nil error, the operator retries.
//   - quality: judge marked the run partial → run completes, mutations
//     are skipped, RunResult.Verdict.Partial is true, error is nil.
//
// Callers that must outlive their trigger's context (the async HTTP endpoint)
// call Admit and Execute separately, so the 202 can carry an already-committed
// run id and the work can run on the operator's lifetime context instead.
func (r *Runner) Run(ctx context.Context, in RunInput) (*RunResult, error) {
	adm, err := r.Admit(ctx, in)
	if err != nil {
		// Pre-split contract: once the id is minted the caller gets it back
		// alongside the failure. adm is nil before the mint, and result() is
		// nil-safe, so both shapes are preserved exactly.
		return adm.result(), err
	}
	return r.Execute(ctx, adm)
}

func (r *Runner) acquireConcurrency(ctx context.Context) error {
	if r == nil {
		return errors.New("council runner not configured")
	}
	r.concurrencyOnce.Do(func() {
		r.concurrencyErr = r.ConcurrencyPolicy.Validate()
		if r.concurrencyErr != nil {
			return
		}
		r.concurrency = loomconcurrency.NewConcurrency(r.ConcurrencyPolicy.EffectiveLimit())
		r.logf("council runner concurrency configured", "concurrency_limit", r.concurrency.Limit())
	})
	if r.concurrencyErr != nil {
		return fmt.Errorf("council runner concurrency policy: %w", r.concurrencyErr)
	}
	return r.concurrency.Acquire(ctx)
}

// Admission is the outcome of Runner.Admit. On a non-dryrun success it is
// durable: the provisional council_runs row is committed (GET
// /api/mills/council/runs/{id} already resolves it) and its budget
// reservation is held until Execute's finalizer releases it. A dryrun
// admission is deliberately nonpersistent, so Run stays nil.
type Admission struct {
	RunID  string
	Input  RunInput
	Policy *mills.Policy
	// Run is the committed provisional row (outcome=running). Nil for a
	// dryrun, and nil on the Admission returned alongside an error.
	Run            *store.CouncilRun
	ReservationUSD float64
	EstimateUSD    float64
	StartedAt      time.Time
}

// result seeds the RunResult Execute fills in. Nil-safe so Run can hand back
// the same (possibly nil) result the pre-split code returned on early failure.
func (a *Admission) result() *RunResult {
	if a == nil {
		return nil
	}
	return &RunResult{
		RunID:     a.RunID,
		Dryrun:    a.Input.Dryrun,
		StartedAt: a.StartedAt,
	}
}

// Admit performs every check that must precede spend and then commits the
// attempt: dependency wiring, policy enablement, trigger normalization, run-id
// mint, the read-only budget preflight, and the atomic ClaimCouncilStart.
//
// A non-nil Admission returned WITH a non-nil error means the run id was
// minted but nothing was committed; only a nil error means the row is durable.
func (r *Runner) Admit(ctx context.Context, in RunInput) (*Admission, error) {
	if r == nil || r.Store == nil || r.Policy == nil || r.Budget == nil || r.Editor == nil || r.Writer == nil {
		return nil, errors.New("council runner not configured")
	}
	if r.Reviewers == nil {
		return nil, errors.New("council runner: reviewer dispatcher required")
	}
	if r.Judge == nil {
		return nil, errors.New("council runner: judge required")
	}
	if r.Mutator == nil {
		return nil, errors.New("council runner: backlog mutator required")
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	policy := r.Policy.Current()
	if !policy.IsEnabled() {
		return nil, errors.New("council runner: policy disabled")
	}
	// The public run request has always treated an omitted trigger as a manual
	// invocation. Normalize at the runner boundary so every caller, including
	// the atomic admission record and metrics, observes the same value.
	if in.Trigger == "" {
		in.Trigger = store.CouncilTriggerManual
	}

	adm := &Admission{
		RunID:       newCouncilRunID(now),
		Input:       in,
		Policy:      policy,
		EstimateUSD: councilReservationEstimate(policy),
		StartedAt:   now,
	}
	r.logf("council run starting", "run_id", adm.RunID, "trigger", in.Trigger, "dryrun", in.Dryrun)

	// Dry-runs deliberately remain nonpersistent, but they still make real
	// provider calls in production. This read-only preflight applies the current
	// per-run/day policy before either dry-run or durable work starts. Only the
	// durable claim below can serialize concurrent spend.
	decision, err := r.Budget.Allow(ctx, mills.TierCouncil, adm.EstimateUSD)
	if err != nil {
		return adm, fmt.Errorf("council budget check: %w", err)
	}
	if !decision.Allowed {
		return adm, fmt.Errorf("%w: %s", ErrBudgetDenied, strings.Join(decision.Reasons, "; "))
	}
	if in.Dryrun {
		return adm, nil
	}

	limits := policy.Budgets.Council
	claim, err := r.Store.ClaimCouncilStart(ctx, store.ClaimCouncilStartRequest{
		RunID:       adm.RunID,
		Trigger:     in.Trigger,
		EstimateUSD: adm.EstimateUSD,
		Limits: store.CouncilStartLimits{
			MaxUSDPerRun:      limits.MaxUSDPerRun,
			MaxUSDPerDay:      limits.MaxUSDPerDay,
			MaxRunsPerDay:     limits.MaxRunsPerDay,
			MaxConcurrentRuns: limits.MaxConcurrentRuns,
		},
		Now:   now,
		Notes: strings.TrimSpace(in.Reason),
	})
	if err != nil {
		return adm, fmt.Errorf("council admission: %w", err)
	}
	adm.Run = claim.Run
	adm.ReservationUSD = claim.Reservation.ReservedUSD
	return adm, nil
}

// Execute runs an admitted council pass: brief → reviewers → editor →
// artifacts → judge → mutator. It owns the deferred finalizer, so every exit
// path — normal, error, panic, context cancellation — writes a terminal
// outcome and releases the reservation on a fresh cleanup context, which is
// why the context that failed can never drop the terminal write.
func (r *Runner) Execute(ctx context.Context, adm *Admission) (res *RunResult, runErr error) {
	if r == nil || r.Store == nil {
		return nil, errors.New("council runner not configured")
	}
	if adm == nil || adm.Policy == nil {
		return nil, errors.New("council runner: admission required")
	}
	if err := r.acquireConcurrency(ctx); err != nil {
		return adm.result(), err
	}
	defer r.concurrency.Release()
	in := adm.Input
	policy := adm.Policy
	res = adm.result()

	var (
		finalRun          *store.CouncilRun
		costs             councilCostAccumulator
		committedWrite    *council.WriteResult
		notifyCommit      bool
		costUnpriced      bool
		reservationUSD    float64
		reviewerQuorumErr error
	)
	estimate := adm.EstimateUSD

	// The overall cap lives here, not in the HTTP handler, so the scheduled
	// (cron) path — which calls Run on the scheduler's root context — inherits
	// it without any change in pkg/mills/council_scheduler.go. The finalizer
	// below runs on its own detached context and is unaffected.
	budgets := r.StageBudgets.withDefaults()
	if budgets.Overall > 0 {
		var cancelOverall context.CancelFunc
		ctx, cancelOverall = context.WithTimeout(ctx, budgets.Overall)
		defer cancelOverall()
	}

	if !in.Dryrun {
		if adm.Run == nil {
			return res, errors.New("council runner: admission has no committed run")
		}
		finalRun = adm.Run
		reservationUSD = adm.ReservationUSD

		// Once admission commits, every return path gets a fresh, bounded cleanup
		// context to terminalize the attempt and release its reservation even when
		// the participant context is what failed.
		defer func() {
			panicValue := recover()
			if panicValue != nil {
				// A panic can interrupt a provider adapter after billing but before
				// it returns cost metadata. Retain the admission reservation rather
				// than treating the unknown remainder as free.
				costUnpriced = true
			}
			if costUnpriced {
				if costs.total() < reservationUSD {
					// A paid provider attempt without a trustworthy price consumes
					// the remaining reservation instead of silently becoming $0.
					costs.add("frontier", reservationUSD-costs.total())
				}
				finalRun.Notes = appendCouncilNote(finalRun.Notes,
					fmt.Sprintf("unpriced provider spend charged conservatively at reservation %.2f USD", reservationUSD))
			}
			ended := r.now()
			res.EndedAt = ended
			res.CostUSDApprox = costs.total()
			finalRun.EndedAt = &ended
			finalRun.CostFrontierUSD = costs.frontier
			finalRun.CostLocalUSD = costs.local
			if panicValue != nil {
				finalRun.Outcome = store.CouncilOutcomeError
				finalRun.Notes = appendCouncilNote(finalRun.Notes, fmt.Sprintf("panic: %v", panicValue))
			} else if runErr != nil {
				finalRun.Outcome = store.CouncilOutcomeError
				finalRun.Notes = appendCouncilNote(finalRun.Notes, runErr.Error())
			} else if finalRun.Outcome == store.CouncilOutcomeRunning || finalRun.Outcome == "" {
				finalRun.Outcome = store.CouncilOutcomeError
				finalRun.Notes = appendCouncilNote(finalRun.Notes, "run exited without terminal outcome")
			}

			finalizeCtx, cancel := context.WithTimeout(context.Background(), councilCleanupTimeout)
			persistErr := r.Store.FinalizeCouncilRun(finalizeCtx, finalRun)
			cancel()
			if persistErr != nil {
				wrapped := fmt.Errorf("finalize council run: %w", persistErr)
				runErr = errors.Join(runErr, wrapped)
			}

			trigger := string(in.Trigger)
			metricOutcome := finalRun.Outcome
			if persistErr != nil {
				metricOutcome = store.CouncilOutcomeError
			}
			outcome := string(metricOutcome)
			mills.CouncilRunsTotal.WithLabelValues(trigger, outcome).Inc()
			mills.CouncilCostUSDTotal.WithLabelValues(trigger).Add(res.CostUSDApprox)
			mills.CouncilDurationSeconds.WithLabelValues(trigger).Observe(ended.Sub(res.StartedAt).Seconds())

			if panicValue == nil && persistErr == nil && runErr == nil && notifyCommit && r.OnArtifactsCommitted != nil {
				hookCtx, hookCancel := context.WithTimeout(context.Background(), councilCleanupTimeout)
				r.OnArtifactsCommitted(hookCtx, finalRun, committedWrite.ArtifactRefs)
				hookCancel()
			}
			if panicValue != nil {
				panic(panicValue)
			}
		}()
	} else {
		// Dry-runs remain write-free, but their returned accounting must not
		// present unknown paid-provider spend as zero. Use the same conservative
		// estimate that passed the read-only budget preflight.
		defer func() {
			if costUnpriced && costs.total() < estimate {
				costs.add("frontier", estimate-costs.total())
			}
			res.CostUSDApprox = costs.total()
			res.EndedAt = r.now()
		}()
	}

	// ----- Roadmap extract → canonical intent store -----
	// The brief reads roadmap_intents; nothing else fills it, so the extractor
	// runs first or the store stays empty forever and the preflight below
	// blocks every run. Best-effort by contract: a missing, unreadable, or
	// unparseable ROADMAP.md logs and proceeds to the intents-missing path. It
	// must never fail a council run.
	if r.RepoRoot != "" {
		_ = r.stage(ctx, res.RunID, "roadmap", budgets.Brief, func(sctx context.Context) error {
			path := filepath.Join(r.RepoRoot, "ROADMAP.md")
			sha, shaErr := roadmapBlobSHA(path)
			if shaErr != nil {
				r.logf("roadmap extract skipped", "run_id", res.RunID, "path", path, "error", shaErr)
				return nil
			}
			rm, extractErr := council.ExtractFromFile(path, sha)
			if extractErr != nil {
				r.logf("roadmap extract failed", "run_id", res.RunID, "path", path, "error", extractErr)
				return nil
			}
			if len(rm.Intents) == 0 {
				// Never sync an empty parse: SyncToStore would DeleteStale-wipe
				// every existing intent and self-inflict the fail-closed block
				// below on the next tick.
				r.logf("roadmap extract produced no open intents", "run_id", res.RunID, "path", path)
				return nil
			}
			sres, syncErr := rm.SyncToStore(sctx, r.Store.Roadmap)
			if syncErr != nil {
				r.logf("roadmap sync failed", "run_id", res.RunID, "error", syncErr)
				return nil
			}
			r.logf("roadmap synced", "run_id", res.RunID, "sha", sha,
				"upserted", sres.Upserted, "retired", sres.Retired)
			return nil
		})
	}

	// ----- Brief -----
	var brief *council.Brief
	err := r.stage(ctx, res.RunID, "brief", budgets.Brief, func(sctx context.Context) error {
		sources := council.BriefSources{Store: r.Store, RepoRoot: r.RepoRoot, Now: r.Now, Signals: r.Signals, SignalWindow: r.SignalWindow}
		// Factory-exhaust demand is policy-gated (default ON) and read per-run
		// so a hot reload takes effect without a restart. Leaving the source
		// nil is the policy-off shape: Compile omits the section entirely
		// rather than rendering it empty or unavailable.
		if r.FactoryExhaust != nil && policy.CouncilFactoryExhaustEnabled() {
			sources.FactoryExhaust = r.FactoryExhaust
			sources.FactoryExhaustLookback = policy.CouncilFactoryExhaustLookback()
			sources.FactoryExhaustLimit = policy.CouncilFactoryExhaustMaxItems()
		}
		var briefErr error
		brief, briefErr = council.Compile(sctx, sources)
		return briefErr
	})
	if err != nil {
		return res, fmt.Errorf("brief: %w", err)
	}
	res.Brief = brief

	// ----- Intent preflight (fail-closed) -----
	// council.Compile stamped IntentsMissingMarker into the Markdown and set
	// brief.IntentsMissing when the canonical store came back empty. Gating on
	// that same field here makes the mark and the block one decision about one
	// object, and stops the run before it spends anything on reviewers or the
	// editor. Applied to dryruns too, so the audit surface cannot diverge from
	// the scheduled path.
	if brief.IntentsMissing && policy.CouncilRequireRoadmapIntents() {
		if finalRun != nil {
			finalRun.Notes = appendCouncilNote(finalRun.Notes,
				"council run blocked: canonical roadmap intent store empty (brief marked intents_missing); override with council.require_roadmap_intents: false")
		}
		r.logf("council run blocked: intents missing", "run_id", res.RunID, "trigger", string(in.Trigger))
		return res, fmt.Errorf("%w: canonical roadmap intent store is empty; set council.require_roadmap_intents: false to override", ErrIntentsMissing)
	}

	// ----- Reviewers + Editor -----
	// Mills v2 Phase 5: when policy.Debate enables this trigger, the
	// reviewers + editor run as a multi-round debate via
	// council.Debate. Otherwise we follow the v1 single-pass path
	// (parallel reviewers → editor.Edit). Both paths converge on the
	// same EditorOutput shape so the artifact / judge / mutator
	// stages downstream are unchanged.
	lenses := council.LensesFromPolicy(policy)
	reviews := []council.ReviewerOutput(nil)
	var out *council.EditorOutput
	debateEnabledByPolicy := len(lenses) > 0 && policy.Debate.Enabled.AllowedFor(string(in.Trigger))
	debateEngaged := debateEnabledByPolicy && r.Moderator != nil
	if debateEnabledByPolicy && !debateEngaged {
		r.logf("debate skipped: no moderator wired",
			"run_id", res.RunID, "trigger", in.Trigger)
	}
	if debateEngaged {
		debate := &council.Debate{
			Editor:    r.Editor,
			Reviewers: r.Reviewers,
			Lenses:    lenses,
			Moderator: r.Moderator,
			Now:       r.Now,
		}
		var dres *council.DebateResult
		derr := r.stage(ctx, res.RunID, "debate", budgets.Debate, func(sctx context.Context) error {
			var e error
			dres, e = debate.Run(sctx, council.DebateInput{
				Brief:              brief,
				MaxUSD:             policy.Debate.MaxUSD,
				MaxRounds:          policy.Debate.MaxRounds,
				EarlyExitThreshold: policy.Debate.EarlyExitThreshold,
				PerReviewerTimeout: councilReviewerTimeout,
				MinQuorum:          (len(lenses) + 1) / 2,
			})
			return e
		})
		if dres != nil {
			costs.add("frontier", dres.TotalCostUSD)
			costUnpriced = costUnpriced || dres.CostUnpriced
			res.CostUSDApprox = costs.total()
		}
		if derr != nil {
			return res, fmt.Errorf("debate: %w", derr)
		}
		out = dres.Editor
		reviews = dres.Reviews
		// Stamp the per-round transcript onto the sidecar so the
		// artifact writer + audit pool consume the same shape.
		out.Sidecar.Debate = &council.SidecarDebate{
			Enabled:         true,
			Rounds:          dres.Rounds,
			EarlyExitReason: dres.EarlyExitReason,
			TotalCostUSD:    dres.TotalCostUSD,
		}
		// Roll the full debate spend (editor.propose + every
		// reviewer.critique + every moderator.assess + every
		// editor.revise) into the run's CostUSD so the artifact
		// writer's downstream stamp into council_runs.cost_*_usd
		// reflects the *full* debate cost — not just the editor's
		// final-revise cost (which is what out.CostUSD alone holds).
		// Without this, CouncilDAO.SumCostSince undercounts debate
		// runs and the daily council cap can overshoot.
		//
		// All debate spend is attributed to the Frontier bucket in
		// slice 5.2; per-round backend tracking lands in slice 5.3
		// alongside the HUD panel and refines the split.
		out.Sidecar.CostUSD = council.SidecarCost{
			Frontier: dres.TotalCostUSD,
			Local:    0,
		}
		r.logf("debate complete",
			"run_id", res.RunID,
			"trigger", in.Trigger,
			"rounds", len(dres.Rounds),
			"early_exit_reason", dres.EarlyExitReason,
			"cost_usd", dres.TotalCostUSD,
		)
	} else {
		if len(lenses) > 0 {
			// The Reviewers budget is an envelope over the whole dispatch; each
			// lens stays independently bounded by councilReviewerTimeout.
			err = r.stage(ctx, res.RunID, "reviewers", budgets.Reviewers, func(sctx context.Context) error {
				var e error
				reviews, e = r.Reviewers.Dispatch(sctx, brief, lenses, council.DispatchOptions{
					PerReviewerTimeout: councilReviewerTimeout,
					MinQuorum:          (len(lenses) + 1) / 2, // simple majority by default
				})
				return e
			})
			if err != nil {
				reviewerQuorumErr = err
				for _, review := range reviews {
					if review.Err != nil {
						r.logf("reviewer failed",
							"run_id", res.RunID,
							"lens", review.Lens.Name,
							"model", review.Lens.Model,
							"backend", review.Lens.Backend,
							"error", review.Err,
						)
					}
				}
				r.logf("reviewer quorum failure", "run_id", res.RunID, "error", err)
				// Continue — the editor can still produce
				// salvage artifacts. The runner forces the
				// terminal verdict partial after evaluation.
			}
			for _, review := range reviews {
				costs.add(review.Lens.Backend, review.CostUSD)
				costUnpriced = costUnpriced || review.CostUnpriced
			}
		}
		err = r.stage(ctx, res.RunID, "editor", budgets.Editor, func(sctx context.Context) error {
			var e error
			out, e = r.Editor.Edit(sctx, brief, reviews)
			return e
		})
		if out != nil {
			costs.addEditor(out)
			costUnpriced = costUnpriced || out.CostUnpriced
			res.Editor = out
			res.CostUSDApprox = costs.total()
		}
		if err != nil {
			return res, fmt.Errorf("editor: %w", err)
		}
		if out == nil {
			return res, errors.New("editor: empty result")
		}
		out.Sidecar.CostUSD = costs.sidecar()
	}
	res.Reviews = reviews
	res.Editor = out

	// ----- Classified planning policy -----
	// The evidence-driven counterpart to council.ApplyEditorGuardrails, which
	// each editor client already applies from the model's own prose. Here the
	// incident class comes from the run's classified ci_watch evidence and
	// planning is fail-closed on storage health. It runs before the artifact
	// write so the artifact on disk, the judge's sidecar, and the mutator all
	// observe the same post-policy proposal set.
	r.applyClassificationPolicy(ctx, res.RunID, brief, out)

	// ----- Artifacts -----
	writer := r.Writer
	if in.Dryrun && writer != nil {
		// Re-target writer at a per-run scratch dir so dryrun doesn't
		// pollute the canonical .loom/ directory. The mkdir is best-
		// effort; failure surfaces as a write error below.
		dryDir := filepath.Join(r.RepoRoot, ".loom", "dryrun", res.RunID, ".loom")
		if err := mkdirAll(dryDir, 0o755); err != nil {
			return res, fmt.Errorf("dryrun mkdir: %w", err)
		}
		writer = &council.ArtifactWriter{
			RepoRoot: filepath.Join(r.RepoRoot, ".loom", "dryrun", res.RunID),
			Now:      r.Now,
		}
	}
	var wr *council.WriteResult
	err = r.stage(ctx, res.RunID, "artifacts", budgets.Artifacts, func(sctx context.Context) error {
		var e error
		wr, e = writer.Write(sctx, res.RunID, out)
		return e
	})
	if err != nil {
		return res, fmt.Errorf("artifacts: %w", err)
	}
	res.Write = wr
	committedWrite = wr
	if finalRun != nil {
		mergeCouncilWrite(finalRun, wr.Run)
		wr.Run = finalRun
	}

	var verdict *eval.Verdict
	err = r.stage(ctx, res.RunID, "judge", budgets.Judge, func(sctx context.Context) error {
		var e error
		verdict, e = r.Judge.Run(sctx, eval.Input{
			Sidecar:      &out.Sidecar,
			WriteResult:  wr,
			EditorOutput: out,
			Store:        r.Store,
			Now:          r.Now,
		})
		return e
	})
	if verdict != nil {
		res.Verdict = verdict
		costUnpriced = costUnpriced || verdict.CostUnpriced
		for _, providerCost := range verdict.ProviderCosts {
			costs.add(providerCost.Backend, providerCost.CostUSD)
		}
		res.CostUSDApprox = costs.total()
	}
	// The sidecar file is the judge's input, so it intentionally contains
	// generation spend only. The terminal council_runs cost columns finalized
	// by the defer are canonical and additionally include evaluator spend.
	if err != nil {
		return res, fmt.Errorf("eval: %w", err)
	}
	if reviewerQuorumErr != nil {
		// The editor is intentionally allowed to salvage artifacts from a
		// degraded reviewer set, but those artifacts must never mutate the
		// canonical backlog without the configured quorum. Preserve the rubric
		// score for diagnosis while forcing the operational verdict partial.
		verdict.Partial = true
		verdict.Results = append(verdict.Results, eval.CriterionResult{
			Name:    "reviewer_quorum",
			Score:   0,
			Weight:  0,
			Reasons: []string{reviewerQuorumErr.Error()},
		})
		if finalRun != nil {
			finalRun.Notes = appendCouncilNote(finalRun.Notes,
				"reviewer quorum failure forced partial verdict: "+reviewerQuorumErr.Error())
		}
	}

	// ----- Classify terminal outcome -----
	outcome := store.CouncilOutcomeSuccess
	if verdict.Partial {
		outcome = store.CouncilOutcomePartial
	}
	// An editor that returned no usable content is a failed run even
	// when the artifact write succeeded with a "No model output returned."
	// placeholder. Demote loudly so operators see the failure on the
	// Council tab instead of a misleading green 'success'. This wins
	// over Partial — empty is a strictly worse signal.
	if out != nil && out.Empty {
		outcome = store.CouncilOutcomeError
		if finalRun != nil {
			finalRun.Notes = appendCouncilNote(finalRun.Notes, "editor returned empty response")
		}
	}
	if finalRun != nil {
		finalRun.Outcome = outcome
	}
	if !in.Dryrun {
		// Both writes stay best-effort: the stage error is logged, never
		// returned, because by this point the artifacts are on disk and the
		// council_runs row is committed.
		_ = r.stage(ctx, res.RunID, "persist", budgets.Persist, func(sctx context.Context) error {
			var persistErr error
			if err := verdict.PersistTo(sctx, r.Store.Eval, res.RunID); err != nil {
				r.logf("persist eval verdict failed", "run_id", res.RunID, "error", err)
				persistErr = err
			}
			// Persist debate transcript rows after the council run row
			// exists (FK on council_run_id). Slice 5.2: skip in dryrun
			// to keep .loom/dryrun runs zero-side-effect; best-effort
			// logging on per-row error so a single failure doesn't
			// unwind the whole run (the run already succeeded — its
			// artifacts are already on disk).
			if out.Sidecar.Debate != nil && len(out.Sidecar.Debate.Rounds) > 0 {
				persistDebateTranscript(sctx, r.Store.Debate, res.RunID, out.Sidecar.Debate.Rounds, r.now, r.logf)
			}
			return persistErr
		})
	}

	// ----- Mutator -----
	// Dryrun synthesises a no-op mutation result so the audit log
	// records the intent without writing to the canonical store. We
	// can't go through Apply() with SkipBecausePartial because the
	// council run row also wasn't persisted, and BacklogItem's FK on
	// council_run_id would fail.
	var mutation *council.MutationResult
	if in.Dryrun {
		mutation = &council.MutationResult{
			TotalProposed: len(out.BacklogProposals),
			Skipped:       true,
			SkipReason:    "dryrun",
		}
	} else {
		err = r.stage(ctx, res.RunID, "mutator", budgets.Mutator, func(sctx context.Context) error {
			var e error
			mutation, e = r.Mutator.Apply(sctx, res.RunID, out, council.MutationOptions{
				// 0 → mutator default (10) per .loom/89- §10.x.
				SkipBecausePartial: verdict.Partial,
				RepoRoot:           r.RepoRoot,
				// Merged-work grounding is policy-gated (default ON) and reads
				// its window from the same section. The mutator's source stays
				// nil unless the operator wired GitLab, so this is inert in
				// tests and GitLab-less deployments rather than fail-closed.
				MergedWorkGroundingDisabled: !policy.CouncilMergedWorkGroundingEnabled(),
				MergedWorkLookback:          policy.CouncilMergedWorkLookback(),
			})
			return e
		})
		if err != nil {
			return res, fmt.Errorf("backlog mutator: %w", err)
		}
		if reviewerQuorumErr != nil && mutation.Skipped {
			mutation.SkipReason = "reviewer quorum failure; mutations dropped"
		}
	}
	res.Mutation = mutation

	// Refresh BacklogDeltas on the persisted run row.
	if !in.Dryrun && len(mutation.CreatedItems) > 0 {
		finalRun.BacklogDeltas.Created = mutation.CreatedIDs()
	}

	// Record this run into the council lane's durable memory, AFTER everything
	// it did is durable (artifacts on disk, verdict persisted, mutations
	// applied or deliberately skipped) so the journal can never claim work the
	// audit trail lacks. Dryruns are deliberately side-effect-free and are
	// excluded. Best-effort: recordCouncilMemory swallows every failure.
	if !in.Dryrun {
		r.recordCouncilMemory(ctx, out, verdict, mutation)
	}

	res.EndedAt = r.now()
	if !in.Dryrun {
		notifyCommit = true
	}
	r.logf("council run complete",
		"run_id", res.RunID,
		"score", verdict.Score,
		"partial", verdict.Partial,
		"created", len(mutation.CreatedItems),
		"truncated", mutation.Truncated,
		"cost_usd_approx", res.CostUSDApprox,
	)
	return res, nil
}

// applyClassificationPolicy enforces the classified planning contract on the
// editor output before it is written, judged, or turned into backlog work.
//
// It is deliberately a no-op until HealthGates is configured. The policy is
// fail-closed by contract — a missing storage-health verdict is unknown health
// and blocks planning — so calling it without a real gate would either wedge
// every run or require synthesizing a healthy verdict, which would launder
// "unknown" into "safe". Leaving it unapplied keeps the default path
// byte-identical to the pre-policy runner and makes enforcement an explicit
// operator decision, exactly as pipeline.Runner.HealthGates already works.
func (r *Runner) applyClassificationPolicy(ctx context.Context, runID string, brief *council.Brief, out *council.EditorOutput) {
	if r.HealthGates == nil || out == nil {
		return
	}
	decision, err := r.HealthGates.DecideHealthGates(ctx)
	if err != nil {
		// A gate that cannot be evaluated is unknown health, not healthy
		// health. Log the reason and let the policy block rather than
		// assuming the last-known state — and never fail the run here: the
		// editor output is still valid, it just may not mint work.
		decision = gates.HealthDecision{Allowed: false, FailClosed: true, Status: "block"}
		r.logf("classified planning policy: health gates unavailable", "run_id", runID, "error", err)
	}
	outcome := council.ApplyClassificationPolicy(out, council.ClassificationPolicyInput{
		IncidentClass: councilIncidentClass(brief),
		StorageHealth: storageVerdictForPlanning(decision),
	})
	if outcome.PlanningAllowed && outcome.OutsideSystemSuppressed == 0 && outcome.OmitReason == "" {
		return
	}
	r.logf("classified planning policy applied",
		"run_id", runID,
		"planning_allowed", outcome.PlanningAllowed,
		"fail_closed", outcome.FailClosed,
		"outside_system_suppressed", outcome.OutsideSystemSuppressed,
		"omit_reason", outcome.OmitReason,
	)
}

// storageVerdictForPlanning adapts a gate decision to the council's narrow
// planning contract. The two sides use different status vocabularies: the gate
// emits pass/block/observe, while council.planningStorageHealthy recognizes
// only "" and "pass" as healthy and blocks on anything else. Forwarding the
// gate's word verbatim would make an *allowing* observe-mode verdict silently
// drop every proposal, so the allow/deny decision is carried by Allowed and
// the status is normalized into the council's own vocabulary.
func storageVerdictForPlanning(decision gates.HealthDecision) *council.StorageHealthVerdict {
	if decision.Allowed {
		return &council.StorageHealthVerdict{Allowed: true, Status: "pass"}
	}
	return &council.StorageHealthVerdict{Allowed: false, Status: "block"}
}

// councilIncidentClass reduces the brief's classified CI evidence to the single
// incident class the planning policy keys on.
//
// Unanimity is required on purpose. An external-dependency classification tells
// the policy to keep only repo-owned guardrail/docs/telemetry/config follow-ups
// and drop everything else, so declaring the whole run external because one
// failure in the 24h window was external would suppress legitimate repository
// work. A mixed window means there is real in-repo work to plan, and returns
// the empty class — which the policy passes through untouched.
func councilIncidentClass(brief *council.Brief) string {
	if brief == nil || len(brief.ClassifiedCIFailures) == 0 {
		return ""
	}
	contexts := council.IncidentContextsFromClassifiedCIFailures(brief.ClassifiedCIFailures)
	if len(contexts) == 0 {
		return ""
	}
	for _, incident := range contexts {
		if incident.Class != council.CIIncidentExternalDependency {
			return ""
		}
	}
	return string(council.CIIncidentExternalDependency)
}

// stage runs one council phase under its own deadline and is the single place
// per-stage lifecycle logs are emitted. A stage that blows its budget is
// reported as such, so the terminal council_runs row names the phase that
// failed instead of a bare "context deadline exceeded".
//
// Credential rule: this helper logs run_id, stage, duration_ms, status and the
// error text only. Model/backend attribution stays at the reviewer call site;
// no config struct, header, or token is ever passed here.
func (r *Runner) stage(ctx context.Context, runID, name string, budget time.Duration, fn func(context.Context) error) error {
	stageCtx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		stageCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	r.logf("council stage start", "run_id", runID, "stage", name)
	started := time.Now()
	err := fn(stageCtx)
	elapsed := time.Since(started)

	status := "ok"
	if err != nil {
		status = "err"
	}
	kv := []any{"run_id", runID, "stage", name, "duration_ms", elapsed.Milliseconds(), "status", status}
	if err != nil {
		kv = append(kv, "error", err.Error())
	}
	r.logf("council stage end", kv...)

	if err != nil && errors.Is(stageCtx.Err(), context.DeadlineExceeded) {
		if ctx.Err() == nil {
			return fmt.Errorf("stage %s exceeded %s budget: %w", name, budget, err)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("stage %s aborted: council run exceeded its overall budget: %w", name, err)
		}
	}
	return err
}

// newCouncilRunID keeps the sortable wall-clock prefix while adding enough
// entropy that simultaneous scheduled/manual admissions cannot overwrite one
// another's provisional rows.
func newCouncilRunID(t time.Time) string {
	return "COUNCIL-" + t.Format("2006-01-02-150405") + "-" + uuid.NewString()[:8]
}

func councilReservationEstimate(policy *mills.Policy) float64 {
	if policy == nil {
		return 0
	}
	if policy.Budgets.Council.MaxUSDPerRun > 0 {
		return policy.Budgets.Council.MaxUSDPerRun
	}
	// A daily-only policy has no estimator-derived run ceiling yet. Reserving
	// the full day cap is conservative and prevents overlapping overspend.
	return policy.Budgets.Council.MaxUSDPerDay
}

type councilCostAccumulator struct {
	frontier float64
	local    float64
}

func (c *councilCostAccumulator) add(backend string, cost float64) {
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return
	}
	if isLocalCouncilBackend(backend) {
		c.local += cost
		return
	}
	c.frontier += cost
}

func (c *councilCostAccumulator) addEditor(out *council.EditorOutput) {
	if out == nil {
		return
	}
	frontier := out.Sidecar.CostUSD.Frontier
	local := out.Sidecar.CostUSD.Local
	c.add("frontier", frontier)
	c.add("flexinfer", local)
	accounted := frontier + local
	if remainder := out.CostUSD - accounted; remainder > 0 {
		c.add(out.Backend, remainder)
	}
}

func (c councilCostAccumulator) total() float64 { return c.frontier + c.local }

func (c councilCostAccumulator) sidecar() council.SidecarCost {
	return council.SidecarCost{Frontier: c.frontier, Local: c.local}
}

func isLocalCouncilBackend(backend string) bool {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "flexinfer", "local", "ollama", "vllm", "llamacpp":
		return true
	default:
		return false
	}
}

func mergeCouncilWrite(dst, src *store.CouncilRun) {
	if dst == nil || src == nil {
		return
	}
	dst.Artifacts = append([]store.ArtifactRef(nil), src.Artifacts...)
	dst.BacklogDeltas = src.BacklogDeltas
	dst.Sidecar = src.Sidecar
	dst.BranchName = src.BranchName
	dst.CommitSHA = src.CommitSHA
	dst.Notes = appendCouncilNote(dst.Notes, src.Notes)
}

func appendCouncilNote(existing, note string) string {
	existing = strings.TrimSpace(existing)
	note = strings.TrimSpace(note)
	if existing == "" {
		return note
	}
	if note == "" || note == existing {
		return existing
	}
	return existing + "; " + note
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) logf(msg string, kv ...any) {
	if r.Logger != nil {
		r.Logger.Info(msg, kv...)
	}
}

// mkdirAll is a tiny wrapper so the Runner can be patched in tests if
// FS injection ever becomes useful. Today it's a straight pass-through
// to os.MkdirAll.
func mkdirAll(path string, perm uint32) error {
	return os.MkdirAll(path, os.FileMode(perm))
}

// roadmapBlobSHA returns the git blob object id of path — byte-identical to
// `git hash-object <path>` — recorded as roadmap_intents.last_seen_in_roadmap_sha
// so DeleteStale can retire intents whose source bullet was edited away.
//
// Computed in-process rather than by shelling out to git: the council hot path
// should not depend on a git binary, and RepoRoot is not guaranteed to be a
// repository. The single os.ReadFile also doubles as the missing/unreadable
// guard, so ExtractFromFile is only reached for a file we could read.
//
// SHA-1 here is a content address dictated by git's object format, not a
// security primitive.
func roadmapBlobSHA(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// persistDebateTranscript inserts every SidecarDebateRound into
// council_debate_rounds. Lives outside Runner.Run so the loop body
// stays scannable; per-row failures are logged but do not bubble — by
// the time we get here the council artifacts are already on disk and
// the council_runs row is committed, so re-raising would leave the
// caller no recovery path. Returns the number of rows successfully
// persisted (mainly for tests + future telemetry).
func persistDebateTranscript(
	ctx context.Context,
	dao *store.DebateDAO,
	runID string,
	rounds []council.SidecarDebateRound,
	now func() time.Time,
	logf func(msg string, kv ...any),
) int {
	if dao == nil || len(rounds) == 0 {
		return 0
	}
	written := 0
	for i := range rounds {
		round := &rounds[i]
		dbRound := &store.CouncilDebateRound{
			CouncilRunID:   runID,
			RoundIndex:     round.Round,
			Role:           store.DebateRole(round.Role),
			CostUSD:        round.CostUSD,
			Summary:        round.Summary,
			ArtifactDeltas: artifactDeltasToStore(round.ArtifactDeltas),
			CreatedAt:      now(),
		}
		if err := dao.AppendRound(ctx, dbRound); err != nil {
			if logf != nil {
				logf("persist debate round failed",
					"run_id", runID,
					"round_index", round.Round,
					"role", round.Role,
					"error", err,
				)
			}
			continue
		}
		written++
	}
	return written
}

// artifactDeltasToStore adapts the sidecar's typed delta slice to the
// loose []map[string]any shape the store DAO accepts. Mirrors the
// schema's `artifact_deltas_json` column (free-form JSON).
func artifactDeltasToStore(deltas []council.SidecarDebateDelta) []map[string]any {
	if len(deltas) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(deltas))
	for _, d := range deltas {
		entry := map[string]any{"path": d.Path}
		if d.LineRange != "" {
			entry["line_range"] = d.LineRange
		}
		if d.Action != "" {
			entry["action"] = d.Action
		}
		out = append(out, entry)
	}
	return out
}

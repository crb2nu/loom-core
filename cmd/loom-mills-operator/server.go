package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/audit"
	"github.com/crb2nu/loom/pkg/mills/bootstrap"
	"github.com/crb2nu/loom/pkg/mills/clients"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/runner"
	"github.com/crb2nu/loom/pkg/mills/spin"
	"github.com/crb2nu/loom/pkg/mills/squads"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// operator owns the state shared between HTTP handlers — the canonical
// store, the policy manager, the budget enforcer, and (slice 3.7+) the
// council runner that orchestrates an end-to-end planning pass.
type operator struct {
	store       *store.Store
	policy      *mills.PolicyManager
	budget      *mills.Budget
	runner      *runner.Runner    // optional; nil disables /api/mills/council/{run,dryrun}
	reconciler  *mills.Reconciler // optional; nil disables manual pipeline starts
	spawnClient interface {
		Stop(context.Context, string) error
	} // optional; stops live HUD spawns on pipeline pause
	regressionGate *gates.RegressionGate // optional; nil makes the alerts webhook return 503
	squadsLoader   *squads.Loader        // optional; nil makes squad endpoints return empty / 404

	// healthGates is the infrastructure admission chain (storage health +
	// local config). Optional; nil means LOOM_MILLS_HEALTH_GATES=off and the
	// status endpoint omits health_gates.
	healthGates *healthGateWiring

	// Audit (Phase 3). Set by main.go when FlexInfer is configured + a
	// reviewer is registered; nil leaves the read endpoints serving
	// canonical-store rows and the admin /run endpoint returning 503.
	auditDispatcher *audit.Dispatcher
	auditWorker     *audit.QueueWorker
	auditTriggers   *audit.Triggers
	auditPolicy     *audit.PoolPolicy

	// Pipeline recursion (Phase 6 slice 6.1). The guard runs the
	// depth/budget/cycle checks before the DAO insert. Always set
	// — the guard itself short-circuits when policy.recursion is
	// disabled (V2-D6 default), so wiring it from newOperator is
	// safe.
	subrunGuard *pipeline.SubrunGuard

	// Kill-switch GitOps auto-PR (plan 42 Slice 1b). gitopsClient is a
	// SEPARATE GitLab client scoped to platform/gitops — the pipeline
	// token can't (and mustn't) write there. nil leaves
	// POST /api/mills/policy/kill-switch returning 503.
	gitopsClient        gitopsCommitter
	gitopsPolicyPath    string
	gitopsDefaultBranch string

	// gitlabBaseURL is the GitLab instance web base (scheme+host, e.g.
	// "https://gitlab.flexinfer.ai"), derived from the configured API URL. It
	// is surfaced on GET /api/mills/status so the HUD can build a clickable MR
	// link — <gitlabBaseURL>/<BacklogItem.TargetProject>/-/merge_requests/<iid>
	// — from data it already holds (mill-floor B1). Runs still carry only the
	// MR iid; keeping the base here avoids a per-run schema change and works
	// across repos (the per-item TargetProject supplies the path). Empty when
	// no GitLab API URL is configured — the HUD then degrades to an iid chip.
	gitlabBaseURL string

	// verdictMRStateForProject verifies operator verdict overrides against the
	// project that owns the run's MR. The durable resolver is preferred; the
	// configured project exists for legacy runs that predate MR provenance.
	verdictMRStateForProject func(string) mills.MRStateClient
	verdictProjectResolver   pipelineProjectResolver
	verdictDefaultProject    string

	// Plan store reader for the backlog intake: hydrates slice scope onto
	// plan-linked items that arrive without one (see hydratePlanSliceScope).
	// Set by main.go once the MCP hub client exists; nil skips hydration.
	planReader backlogPlanReader

	// telemetryCache memoizes the GET /api/mills/telemetry/stages roll-up for a
	// few seconds per window so a burst of HUD polls collapses into one
	// aggregation. Always set by newOperator.
	telemetryCache *telemetryStageCache

	// wiring is the resolved model-wiring snapshot captured once at startup
	// (buildWiringSnapshot). Served verbatim by GET /api/mills/wiring. Set by
	// main.go after the LLM clients/council/dispatcher are wired; nil (handler
	// 503) until then and in unit operators that don't populate it.
	wiringMu sync.RWMutex
	wiring   *WiringSnapshot

	// Spinning Room (Live Beam slice 3): turns an operator brief into a draft
	// plan via a policy-chosen model frame. Set by main.go once the MCP hub
	// client exists (draft plans are authored over the hub); nil leaves
	// POST /api/mills/spin returning 503.
	spinner *spin.Spinner

	// Plan→repo bootstrap: mints a new GitLab project from a Spinning Room
	// plan. Set by main.go when BOTH the GitLab client and the MCP hub exist
	// (repo create + plan re-scope); nil leaves POST /api/mills/projects/
	// bootstrap returning 503. Policy-gated at request time on top
	// (cross_repo.enabled + cross_repo.allow_bootstrapped).
	bootstrapper *bootstrap.Service

	// Async spins (plan .loom/166). POST /api/mills/spin/async runs the spinner
	// in a detached goroutine and returns 202 immediately so a minutes-long
	// frontier spin never exceeds the client-facing proxy timeout. spinBaseCtx
	// roots those goroutines at the OPERATOR lifetime (not the request), so a
	// client disconnect can't cancel an in-flight spin but operator shutdown
	// can. spinSem bounds concurrency (frontier spins are expensive/slow); it
	// is buffered to the configured cap and is never nil (newOperator seeds a
	// default so handler tests work without main.go wiring).
	spinBaseCtx context.Context
	spinSem     chan struct{}
	// spinWorkers includes detached model/plan-store workers that may outlive a
	// watchdog timeout because a dependency ignored context cancellation.
	spinWorkers atomic.Int64
	// spinAsyncBudget overrides the async-spin wall-clock cap (the spinAsyncBudget
	// const). 0 ⇒ the const. Tests set a short budget to exercise the watchdog
	// without waiting the full 10 minutes.
	spinAsyncBudget time.Duration

	// Async council runs (#334). POST /api/mills/council/async admits the run
	// synchronously — so the 202 carries an already-durable run id — then
	// executes it under councilBaseCtx, the OPERATOR lifetime context, so the
	// edge cutting the client connection at ~100s can't cancel a council pass
	// that takes minutes. councilSem only prevents goroutine pileup; the real
	// spend/concurrency bound is the admission kernel's policy limits, and the
	// wall-clock bound is runner.StageBudgets.Overall. Never nil (newOperator
	// seeds a default so handler tests work without main.go wiring).
	councilBaseCtx context.Context
	councilSem     chan struct{}

	// overseers maps agent name → registration (harness + policy accessors)
	// for the /api/mills/overseers endpoints. Populated during main.go
	// wiring; nil/empty in handler tests that don't exercise the overseers
	// (the status endpoint then returns an empty agent list).
	overseers map[string]overseerEntry
	// admissionSuppressors are overseer-owned vetoes composed into the
	// background loops' workAdmissionEnabled closure (sentinel/foreman TTL
	// leases). They gate ONLY the automated work-admission loops — HTTP
	// `admit` endpoints stay open so a human can still act during a
	// dependency incident. Registered during single-threaded wiring, read
	// under admissionMu afterwards.
	admissionSuppressors []func() bool

	logger    *slog.Logger
	authority operatorAuthorityIdentity

	capabilitiesMu sync.RWMutex
	capabilities   capabilityWiring
	ready          atomic.Bool

	// admissionMu + activeAdmissions turn policy.enabled=false into a
	// drainable admission barrier: requests admitted before the hot reload are
	// counted until completion, and requests arriving after it are rejected.
	admissionMu            sync.Mutex
	activeAdmissions       atomic.Int64
	canaryAdmissions       atomic.Int64
	policyGeneration       atomic.Uint64
	workflowCanaryMu       sync.Mutex
	activitySources        map[string]activeOperationSource
	activityReady          bool
	activityWiringRequired bool
	activityWiringError    string
	activityGeneration     uint64
	crashLease             *safetyCrashLease
}

type activeOperationSource interface {
	ActiveOperations() int64
}

type namedActivitySource struct {
	name   string
	source activeOperationSource
}

const (
	activitySourceReconciler = "reconciler"
	activitySourcePipeline   = "pipeline"
	activitySourceCrossRun   = "cross_run"
	activitySourceCouncil    = "council"
	activitySourceCanary     = "canary"
	activitySourceWorkflow   = "workflow"
)

var requiredActivitySourceNames = []string{
	activitySourceReconciler,
	activitySourcePipeline,
	activitySourceCrossRun,
	activitySourceCouncil,
	activitySourceCanary,
	activitySourceWorkflow,
}

// defaultSpinConcurrency bounds how many async spins run at once by default.
// Frontier frames (claude-opus-4-8) are slow + costly, so the default is
// deliberately small; main.go can override it from LOOM_MILLS_SPIN_MAX_CONCURRENT.
const defaultSpinConcurrency = 2

func newOperator(st *store.Store, pm *mills.PolicyManager, b *mills.Budget, logger *slog.Logger) *operator {
	o := &operator{
		store:        st,
		policy:       pm,
		budget:       b,
		logger:       logger,
		capabilities: newCapabilityWiring(Config{}),
		// Async-spin concurrency gate. Seeded here so handler tests (which call
		// newOperator directly) get a working semaphore; main.go may replace it
		// with a policy/env-sized channel before the listener starts.
		spinSem: make(chan struct{}, defaultSpinConcurrency),
		// Async-council worker gate, seeded for the same reason as spinSem.
		councilSem: make(chan struct{}, defaultCouncilAsyncConcurrency),
		// Default regression gate: same store + policy + default 30min
		// window. Tests that want to skip the gate clear this field.
		regressionGate: &gates.RegressionGate{Store: st, Policy: pm},
		// Phase 6 recursion guard. PolicyFunc reads from the same
		// PolicyManager so hot-reloads of recursion.enabled /
		// max_depth / subrun_max_budget_share take effect mid-run.
		subrunGuard: &pipeline.SubrunGuard{
			Store:      st,
			PolicyFunc: pm.Current,
		},
		activitySources: make(map[string]activeOperationSource),
		authority:       operatorAuthorityIdentityFromEnv(),
		// Short-TTL memo for the telemetry stages roll-up (see handler).
		telemetryCache: newTelemetryStageCache(telemetryCacheTTLFromEnv(logger)),
	}
	// Unit/embedded callers have no late-bound background wiring. Production
	// explicitly clears this before building its source set and marks it ready
	// only after every optional worker has registered.
	o.activityReady = true
	o.policyGeneration.Store(1)
	if pm != nil {
		pm.Subscribe(func(_, _ *mills.Policy) {
			o.policyGeneration.Add(1)
		})
	}
	return o
}

// withRunner attaches a council runner. Operators that don't want
// council functionality (e.g. the stub used by handler tests) leave
// runner unset and the council POST endpoints respond 503.
func (o *operator) withRunner(r *runner.Runner) *operator {
	o.runner = r
	return o
}

// withHealthGates stores the infrastructure admission gates so GET
// /api/mills/status can publish the health_gates key the HUD tile reads. nil
// (gates disabled) leaves the key absent, which the HUD renders as its
// fail-closed default.
func (o *operator) withHealthGates(w *healthGateWiring) *operator {
	o.healthGates = w
	return o
}

// withGitLabBaseURL derives and stores the GitLab instance web base from the
// configured REST API URL (mill-floor B1). The input is the API URL
// ("https://gitlab.flexinfer.ai/api/v4"); the stored value is scheme+host
// ("https://gitlab.flexinfer.ai") so the HUD can append
// "/<project>/-/merge_requests/<iid>". A blank or unparseable API URL leaves
// the base empty and the endpoint simply omits it (the HUD degrades to a chip).
func (o *operator) withGitLabBaseURL(apiURL string) *operator {
	o.gitlabBaseURL = gitlabWebBaseURL(apiURL)
	return o
}

func (o *operator) withVerdictMRVerification(forProject func(string) mills.MRStateClient, resolver pipelineProjectResolver, defaultProject string) *operator {
	o.verdictMRStateForProject = forProject
	o.verdictProjectResolver = resolver
	o.verdictDefaultProject = strings.TrimSpace(defaultProject)
	return o
}

// gitlabWebBaseURL turns a GitLab REST API URL into its instance web base
// (scheme://host). "https://gitlab.flexinfer.ai/api/v4" → "https://gitlab.flexinfer.ai".
// Returns "" for blank input or a URL missing a scheme/host, so callers can
// treat "" as "no clickable base available".
func gitlabWebBaseURL(apiURL string) string {
	trimmed := strings.TrimSpace(apiURL)
	if trimmed == "" {
		return ""
	}
	base := strings.TrimSuffix(strings.TrimRight(trimmed, "/"), "/api/v4")
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// withReconciler attaches the desired-state reconciler used by the manual
// pipeline start endpoint. The scheduler owns the same pointer.
func (o *operator) withReconciler(r *mills.Reconciler) *operator {
	o.reconciler = r
	return o
}

func (o *operator) withSpawnClient(c interface {
	Stop(context.Context, string) error
}) *operator {
	o.spawnClient = c
	return o
}

// withWiringSnapshot stores the resolved model-wiring snapshot served by
// GET /api/mills/wiring. Copies by value so a later mutation of the caller's
// struct can't race the handler's readers.
func (o *operator) withWiringSnapshot(s WiringSnapshot) *operator {
	o.wiringMu.Lock()
	snap := s
	o.wiring = &snap
	o.wiringMu.Unlock()
	return o
}

// withSquadsLoader attaches a squads.Loader. nil leaves the loader unset
// so the squad endpoints return empty list / 404 — the operator still
// boots cleanly when no squad manifests are mounted.
func (o *operator) withSquadsLoader(l *squads.Loader) *operator {
	o.squadsLoader = l
	return o
}

// withAudit attaches the Phase 3 audit subsystem: dispatcher, queue
// worker, triggers, and the pool policy used when admin re-runs default
// to the policy ensemble. Any nil leaves the audit endpoints in their
// degraded state (read-only via canonical store; /run returns 503).
func (o *operator) withAudit(d *audit.Dispatcher, w *audit.QueueWorker, t *audit.Triggers, p *audit.PoolPolicy) *operator {
	o.auditDispatcher = d
	o.auditWorker = w
	o.auditTriggers = t
	o.auditPolicy = p
	return o
}

// gitopsCommitter is the slice of the GitLab client the kill-switch
// handler needs: read the policy file and stage a branch+commit. Kept as
// an interface so handler tests can stub GitOps without a live GitLab.
// *clients.GitLabClient satisfies it.
type gitopsCommitter interface {
	GetRawFile(ctx context.Context, filePath, ref string) (string, error)
	CreateCommit(ctx context.Context, req clients.CreateCommitRequest) (clients.CreateCommitResponse, error)
	CreateMR(ctx context.Context, req pipeline.CreateMRRequest) (pipeline.CreateMRResponse, error)
}

// withKillSwitch attaches the GitOps-scoped GitLab client + the policy
// file path the kill-switch MR edits. nil client leaves
// POST /api/mills/policy/kill-switch returning 503. policyPath / branch
// fall back to sane defaults when empty.
func (o *operator) withKillSwitch(c gitopsCommitter, policyPath, branch string) *operator {
	o.gitopsClient = c
	o.gitopsPolicyPath = policyPath
	if o.gitopsPolicyPath == "" {
		o.gitopsPolicyPath = "k3s/mills/configmap-policy.yaml"
	}
	o.gitopsDefaultBranch = branch
	if o.gitopsDefaultBranch == "" {
		o.gitopsDefaultBranch = "main"
	}
	return o
}

// withSpinner attaches the Spinning Room. nil leaves POST /api/mills/spin
// returning 503; the frames listing still serves policy so the HUD can show
// the room as unavailable rather than erroring.
func (o *operator) withSpinner(s *spin.Spinner) *operator {
	o.spinner = s
	return o
}

// markReady flips the readyz response from 503 to 200. Called once startup
// completes so Kubernetes only routes traffic after migrations + initial
// policy load are done.
func (o *operator) markReady() { o.ready.Store(true) }

func (o *operator) withActivitySources(sources ...namedActivitySource) {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.activitySources = make(map[string]activeOperationSource, len(sources))
	o.activityWiringError = ""
	for _, source := range sources {
		o.registerActivitySourceLocked(source.name, source.source)
	}
	o.activityGeneration++
}

func (o *operator) addActivitySource(name string, source activeOperationSource) {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.registerActivitySourceLocked(name, source)
	o.activityGeneration++
}

func (o *operator) registerActivitySourceLocked(name string, source activeOperationSource) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		o.activityWiringError = "activity source has an empty name"
	case isNilActivitySource(source):
		o.activityWiringError = "activity source " + name + " is nil"
	case o.activitySources[name] != nil:
		o.activityWiringError = "duplicate activity source " + name
	default:
		for existingName, existing := range o.activitySources {
			if sameActivitySourceInstance(existing, source) {
				o.activityWiringError = "activity source " + name + " duplicates instance " + existingName
				return
			}
		}
		o.activitySources[name] = source
	}
}

func isNilActivitySource(source activeOperationSource) bool {
	if source == nil {
		return true
	}
	v := reflect.ValueOf(source)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

func sameActivitySourceInstance(a, b activeOperationSource) bool {
	if isNilActivitySource(a) || isNilActivitySource(b) {
		return false
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	return av.Kind() == reflect.Pointer && bv.Kind() == reflect.Pointer &&
		av.Type() == bv.Type() && av.Pointer() == bv.Pointer()
}

func (o *operator) beginActivitySourceWiring() {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.activityWiringRequired = true
	o.activityReady = false
	o.activityGeneration++
}

func (o *operator) markActivitySourcesReady() {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.activityReady = o.activityWiringError == "" && len(missingRequiredActivitySources(o.activitySources)) == 0
	o.activityGeneration++
}

func missingRequiredActivitySources(sources map[string]activeOperationSource) []string {
	missing := make([]string, 0, len(requiredActivitySourceNames))
	for _, name := range requiredActivitySourceNames {
		if sources[name] == nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// requireWorkAdmission rejects new workload creation while the durable global
// kill switch is disabled. The in-flight counter closes the hot-reload race:
// safety preflight waits for already-admitted handlers to finish before a pod
// may be deleted.
func (o *operator) requireWorkAdmission(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o.admissionMu.Lock()
		policy := o.policy.Current()
		leaseActive := o.crashLeaseActiveLocked(time.Now().UTC())
		if policy == nil || !policy.IsEnabled() || leaseActive {
			o.admissionMu.Unlock()
			http.Error(w, "Mills work admission is closed by policy or an active crash lease", http.StatusServiceUnavailable)
			return
		}
		o.activeAdmissions.Add(1)
		o.admissionMu.Unlock()
		defer o.activeAdmissions.Add(-1)
		next(w, r)
	}
}

// trackSafetyActivity keeps operational mutations (pause/fail/abort,
// kill-switch, telemetry correlation) usable while work admission is closed,
// but makes their full handler lifetime visible to the destructive safety
// snapshot. Unlike requireWorkAdmission it never rejects on policy state.
func (o *operator) trackSafetyActivity(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o.admissionMu.Lock()
		if o.crashLeaseActiveLocked(time.Now().UTC()) {
			o.admissionMu.Unlock()
			http.Error(w, "operation locked during active crash lease", http.StatusLocked)
			return
		}
		o.activeAdmissions.Add(1)
		o.admissionMu.Unlock()
		defer o.activeAdmissions.Add(-1)
		next(w, r)
	}
}

func (o *operator) workAdmissionOpen() bool {
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	policy := o.policy.Current()
	return policy != nil && policy.IsEnabled() && !o.crashLeaseActiveLocked(time.Now().UTC())
}

// addAdmissionSuppressor registers an overseer veto over automated work
// admission. Call during wiring, before the errgroup starts.
func (o *operator) addAdmissionSuppressor(fn func() bool) {
	if fn == nil {
		return
	}
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	o.admissionSuppressors = append(o.admissionSuppressors, fn)
}

// admissionSuppressed reports whether any overseer veto is live. Each veto
// is itself fail-safe (TTL lease + policy/dry-run guards read at call time).
func (o *operator) admissionSuppressed() bool {
	o.admissionMu.Lock()
	suppressors := o.admissionSuppressors
	o.admissionMu.Unlock()
	for _, fn := range suppressors {
		if fn() {
			return true
		}
	}
	return false
}

// httpMux returns the REST + MCP listener mux. Read-only routes are
// open; mutating routes are wrapped in requireAdmin so they reject
// every caller when LOOM_MILLS_ADMIN_TOKEN is unset and require a Bearer
// match when it isn't.
func (o *operator) httpMux() http.Handler {
	mux := http.NewServeMux()
	admit := func(handler http.HandlerFunc) http.HandlerFunc {
		return requireAdmin(o.requireWorkAdmission(handler))
	}
	operate := func(handler http.HandlerFunc) http.HandlerFunc {
		return requireAdmin(o.trackSafetyActivity(handler))
	}
	grade := func(handler http.HandlerFunc) http.HandlerFunc {
		operated := operate(handler)
		return func(w http.ResponseWriter, r *http.Request) {
			authorization := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(authorization, "Bearer ") &&
				!subtleEqual(strings.TrimPrefix(authorization, "Bearer "), currentAdminToken()) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="loom-mills", error="invalid_token"`)
				http.Error(w, "invalid Bearer token", http.StatusForbidden)
				return
			}
			operated(w, r)
		}
	}

	// Status / policy / KPIs (read-only).
	mux.HandleFunc("GET /api/mills/status", o.handleStatusFull)
	mux.HandleFunc("GET /api/mills/capabilities", o.handleCapabilities)
	mux.HandleFunc("GET /api/mills/policy", o.handlePolicy)
	mux.HandleFunc("GET /api/mills/kpis", o.handleKPIs)
	mux.HandleFunc("GET /api/mills/telemetry/stages", o.handleTelemetryStages)
	// Resolved model-wiring snapshot (which model/backend the judge, weaver,
	// council lenses, and each spawn stage use). Read-only non-secret config —
	// open like /status and /telemetry.
	mux.HandleFunc("GET /api/mills/wiring", o.handleWiring)
	// Overseers (supervisory agents): open status read + gated runtime
	// controls. Enabling/disabling an agent is a policy ConfigMap change;
	// these endpoints only soft-pause a loop or drive one manual tick.
	mux.HandleFunc("GET /api/mills/overseers", o.handleOverseersStatus)
	mux.HandleFunc("POST /api/mills/overseers/{agent}/pause", operate(o.handleOverseerPause))
	mux.HandleFunc("POST /api/mills/overseers/{agent}/resume", admit(o.handleOverseerResume))
	mux.HandleFunc("POST /api/mills/overseers/{agent}/tick", admit(o.handleOverseerTick))
	// Promotion evidence: what the guarded actors under ?actor= actually did
	// over ?window=, split dry-run vs executed. Open read like the overseers
	// status it aggregates.
	mux.HandleFunc("GET /api/mills/promotion-report", o.handlePromotionReport)
	// Revert-precise post-merge regressions over ?window=. Open read like the
	// promotion report — it aggregates the sweep's durable events.
	mux.HandleFunc("GET /api/mills/regressions", o.handleRegressionsList)
	// Classifier-signature candidates mined from the escalations no live
	// classifier explains, over ?window=. Open read like the regressions above
	// — proposals only; promoting one is a reviewed code change.
	mux.HandleFunc("GET /api/mills/signature-candidates", o.handleSignatureCandidatesList)
	// Judge calibration: the LLM gates' own scores joined to what the runs
	// they graded actually did over ?window=. Open read like the report above.
	mux.HandleFunc("GET /api/mills/judge-calibration", o.handleJudgeCalibration)
	// Config outcomes: run.provenance stamps joined to what the runs they
	// describe merged, escalated, cost and regressed over ?window=. Open read
	// like the reports above.
	mux.HandleFunc("GET /api/mills/config-outcomes", o.handleConfigOutcomes)
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/verdict", requireAdmin(o.handlePipelineRunVerdict))
	// Demand log: proposals the council declined to mint (merged-work
	// suppressions) over ?window= — what the factory chose NOT to make. Open
	// read like the reports above; it renders the mutator's own audit trail.
	mux.HandleFunc("GET /api/mills/demand-log", o.handleDemandLog)
	mux.HandleFunc("GET /api/mills/safety/quiescence", o.handleSafetyQuiescence)
	mux.HandleFunc("POST /api/mills/safety/crash-lease", requireAdmin(o.handleSafetyCrashLease))
	mux.HandleFunc("POST /api/mills/safety/crash-lease/{token}/renew", requireAdmin(o.handleSafetyCrashLeaseRenew))
	mux.HandleFunc("DELETE /api/mills/safety/crash-lease/{token}", requireAdmin(o.handleSafetyCrashLeaseRelease))

	// Global autonomy kill-switch (plan 42 Slice 1b). Opens a GitOps
	// auto-PR flipping policy `enabled:` in platform/gitops rather than a
	// live write-through (Flux owns the ConfigMap). Admin-gated.
	mux.HandleFunc("POST /api/mills/policy/kill-switch", operate(o.handlePolicyKillSwitch))

	// Council.
	mux.HandleFunc("GET /api/mills/council/runs", o.handleCouncilRunsList)
	mux.HandleFunc("GET /api/mills/council/runs/{id}", o.handleCouncilRunGet)
	mux.HandleFunc("GET /api/mills/council/runs/{id}/debate", o.handleCouncilRunDebate)
	mux.HandleFunc("POST /api/mills/council/run", admit(o.handleCouncilRun))
	mux.HandleFunc("POST /api/mills/council/dryrun", admit(o.handleCouncilDryrun))
	// Async council (#334): 202 + an already-committed run id, then the pass
	// runs detached from the request so a client/edge disconnect can't kill it.
	// Poll GET /api/mills/council/runs/{id}. Both synchronous endpoints above
	// stay exactly as they are for the CLI, docs, and status scripts.
	mux.HandleFunc("POST /api/mills/council/async", admit(o.handleCouncilAsync))

	// Spinning Room (Live Beam slice 3): the operator picks a model frame,
	// gives it a brief, and it spins a draft plan into the Plan Store. The
	// frame list is an open read (policy, like GET /api/mills/policy); the spin
	// itself mutates the Plan Store, so it is admin-gated.
	mux.HandleFunc("GET /api/mills/spinning-room/frames", o.handleSpinningRoomFrames)
	mux.HandleFunc("POST /api/mills/spin", admit(o.handleSpin))
	// Async spins (plan .loom/166): 202 + spin_id, spin runs in the background,
	// draft lands in the Plan Store. The status reads mirror the council run
	// endpoints (open reads); the spin itself mutates the Plan Store so the POST
	// is admin-gated like the synchronous /spin.
	mux.HandleFunc("POST /api/mills/spin/async", admit(o.handleSpinAsync))
	mux.HandleFunc("GET /api/mills/spin/runs", o.handleSpinRunsList)
	mux.HandleFunc("GET /api/mills/spin/runs/{id}", o.handleSpinRunGet)

	// Plan→repo bootstrap: mint a new GitLab project from a Spinning Room
	// plan (repo create + seed commit + registry row + plan re-scope). The
	// mint mutates GitLab AND the Plan Store, so it is admin-gated like
	// /spin; the registry list is an open read like /spin/runs.
	mux.HandleFunc("POST /api/mills/projects/bootstrap", admit(o.handleProjectBootstrap))
	mux.HandleFunc("GET /api/mills/projects/bootstrapped", o.handleBootstrappedList)

	// Pipeline.
	mux.HandleFunc("GET /api/mills/merge-queue", o.handleMergeQueueList)
	mux.HandleFunc("POST /api/mills/merge-queue/enqueue", requireAdmin(o.handleMergeQueueEnqueue))
	mux.HandleFunc("GET /api/mills/pipeline/runs", o.handlePipelineRunsList)
	mux.HandleFunc("GET /api/mills/pipeline/runs/{id}", o.handlePipelineRunGet)
	// MR head-movement ledger (#374). Open read like the run detail above:
	// when a merge fails closed because the branch moved, this is where an
	// operator sees which SHA was authorized, which one GitLab holds now, and
	// what evidence classified the movement.
	mux.HandleFunc("GET /api/mills/pipeline/runs/{id}/transitions", o.handlePipelineRunTransitions)
	mux.HandleFunc("POST /api/mills/pipeline/runs/{backlog_id}/start", admit(o.handlePipelineStart))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/pause", operate(o.handlePipelinePause))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/resume", admit(o.handlePipelineResume))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/escalate", operate(o.handlePipelineEscalate))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/grade", grade(o.handlePipelineGrade))
	mux.HandleFunc("POST /api/mills/pipeline/runs/{id}/subrun", admit(o.handlePipelineSubrunCreate))

	// Workflow step-log (plan .loom/134 §S4a). Read-only views over the durable
	// workflow_runs/workflow_steps journal so an operator can observe an
	// imperative run's step log live (vs. `kubectl exec … sqlite3`). The detail
	// endpoint nests steps inline, mirroring the pipeline run+stages pattern.
	mux.HandleFunc("GET /api/mills/workflow/runs", o.handleWorkflowRunsList)
	mux.HandleFunc("GET /api/mills/workflow/runs/{id}", o.handleWorkflowRunGet)
	// Admin: launch the S6-min canary imperative run (the only remote entrypoint
	// to enqueue an imperative run until S7 council selection exists). Used by the
	// S1c deployed kill-test (.loom/135). Mutating -> requireAdmin.
	mux.HandleFunc("POST /api/mills/workflow/canary", requireAdmin(o.handleWorkflowCanaryStart))
	// Admin: workflow run lifecycle mutations — the live-mitigation surface for
	// a stuck imperative run (2026-07-09 wf-canary zombie loop: no mutation
	// endpoint existed, so clearing two dead runs required a code deploy). The
	// scheduler honors paused_at/state between steps. Mutating -> requireAdmin.
	mux.HandleFunc("POST /api/mills/workflow/runs/{id}/pause", operate(o.handleWorkflowRunPause))
	mux.HandleFunc("POST /api/mills/workflow/runs/{id}/resume", admit(o.handleWorkflowRunResume))
	mux.HandleFunc("POST /api/mills/workflow/runs/{id}/fail", operate(o.handleWorkflowRunFail))

	// Backlog.
	mux.HandleFunc("GET /api/mills/backlog", o.handleBacklogList)
	mux.HandleFunc("GET /api/mills/taste/aggregates", o.handleTasteAggregates)
	mux.HandleFunc("GET /api/mills/backlog/{id}", o.handleBacklogGet)
	mux.HandleFunc("POST /api/mills/backlog", admit(o.handleBacklogCreate))
	mux.HandleFunc("POST /api/mills/backlog/sync", admit(o.handleBacklogSync))
	mux.HandleFunc("GET /api/mills/cost-preview", o.handleCostPreview)

	// Escalations. Relaunch candidates is an open read like /backlog and
	// /pipeline/runs: it only projects canonical-store rows the HUD polls.
	mux.HandleFunc("GET /api/mills/escalations/relaunch-candidates", o.handleEscalationRelaunchCandidates)

	// Squads (Phase 2 slice 2.4). Read endpoints are open; route-test is an
	// authenticated diagnostic dry-run. It remains available while work
	// admission is closed because it only reads the loader + canonical store.
	mux.HandleFunc("GET /api/mills/squads", o.handleSquadsList)
	mux.HandleFunc("GET /api/mills/squads/{name}", o.handleSquadGet)
	mux.HandleFunc("GET /api/mills/squads/{name}/memory", o.handleSquadMemory)
	mux.HandleFunc("GET /api/mills/squads/{name}/outcomes", o.handleSquadOutcomes)
	mux.HandleFunc("POST /api/mills/squads/{name}/route-test", requireAdmin(o.handleSquadRouteTest))

	// Eval.
	mux.HandleFunc("GET /api/mills/eval/scores", o.handleEvalScores)
	mux.HandleFunc("POST /api/mills/eval/run-cross", admit(o.handleEvalRunCross))

	// Audit (Phase 3 slice 3.4). Read endpoints serve canonical-store
	// rows even when the dispatcher isn't wired (HUD never sees a 503
	// on a poll). The admin /run endpoint requires the dispatcher +
	// queue worker; without them it returns 503 with a clear message.
	mux.HandleFunc("GET /api/mills/audit/findings", o.handleAuditFindings)
	mux.HandleFunc("GET /api/mills/audit/findings/{id}", o.handleAuditFindingDetails)
	mux.HandleFunc("POST /api/mills/audit/run", admit(o.handleAuditRun))

	// Cross-repo (Phase 4 slice 4.4). Read endpoints serve canonical-store
	// rows; abort is admin-gated like other mutating endpoints.
	mux.HandleFunc("GET /api/mills/cross-repo/runs", o.handleCrossRepoList)
	mux.HandleFunc("GET /api/mills/cross-repo/runs/{id}", o.handleCrossRepoGet)
	mux.HandleFunc("POST /api/mills/cross-repo/runs/{id}/abort", operate(o.handleCrossRepoAbort))

	// Regression gate (slice 6.3): Alertmanager telemetry webhook. Keep it
	// authenticated but outside the workload-admission barrier: returning 503
	// while a canary window is open makes Alertmanager retry indefinitely and
	// removes the correlation signal needed during fault injection.
	mux.HandleFunc("POST /api/mills/alerts/regression", operate(o.handleRegressionAlert))

	// Adaptive policy proposals (Phase 7 slice 7.2).
	mux.HandleFunc("GET /api/mills/policy/proposals", o.handlePolicyProposalsList)
	mux.HandleFunc("POST /api/mills/policy/proposals/{id}/apply", admit(o.handlePolicyProposalApply))
	mux.HandleFunc("POST /api/mills/policy/proposals/{id}/reject", operate(o.handlePolicyProposalReject))

	// Anything else under /api/mills returns 404 with a clear message; the
	// catch-all "/" stays 501 so unprefixed paths don't get mistaken for
	// missing API routes.
	mux.HandleFunc("/api/mills/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "operator REST root; see /api/mills/status", http.StatusNotFound)
	})
	return o.withOperatorAuthority(mux)
}

// metricsMux returns the lifecycle listener mux: /healthz, /readyz, /metrics.
// Kept on a separate listener so health probes don't queue behind real
// traffic and so a misbehaving handler can't break liveness.
func (o *operator) metricsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", o.handleHealth)
	mux.HandleFunc("/readyz", o.handleReady)
	mux.Handle("/metrics", promhttp.Handler())
	return mux
}

func (o *operator) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if err := o.store.DB().Ping(); err != nil {
		http.Error(w, "db unreachable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (o *operator) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !o.ready.Load() {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// (handleStatus stub removed — slice 2.4 wires handleStatusFull which
// pulls real values from the canonical store.)

// httpServer constructs an http.Server with sensible timeouts.
func httpServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// runListener starts an HTTP listener and blocks until ctx cancels or the
// server returns an unrecoverable error.
func runListener(ctx context.Context, label string, srv *http.Server, logger *slog.Logger) error {
	if srv.Addr == "" {
		return nil
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listener starting", "label", label, "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s: %w", label, err)
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

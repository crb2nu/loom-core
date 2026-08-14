package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// effectiveProject resolves the repo a stage targets for the given item: the
// item's per-item TargetProject when set, else the worker's configured fallback
// (the operator home repo). An empty item target yields home-repo behavior, so
// this is backward compatible for every item that doesn't opt into cross-repo.
func effectiveProject(item *store.BacklogItem, fallback string) string {
	if item != nil {
		if tp := strings.TrimSpace(item.TargetProject); tp != "" {
			return tp
		}
	}
	return fallback
}

// JobContext is the bundle every Worker receives. The dispatcher fills
// it from the run + item + stage so workers don't have to reach back
// into the runner.
type JobContext struct {
	Run   *store.PipelineRun
	Item  *store.BacklogItem
	Stage Stage
	Prior map[string]StageOutput
	// ResumeSpawnID is set by the runner when a previous operator
	// process accepted a spawn for this stage attempt but stopped before
	// the spawn reached a terminal result.
	ResumeSpawnID string
	// Attempt is the runner's dispatch attempt number for this stage (1 for
	// the first dispatch; seedAttempts keeps it monotonic across operator
	// restarts). SpawnWorker folds it into the stage's deterministic
	// idempotency key so a retry gets a fresh spawn while a duplicate
	// dispatch of the SAME attempt dedupes into a re-attach. Zero when the
	// dispatch path did not stamp it (direct Worker.Run calls in tests).
	Attempt int
	// RetryContext is non-nil when the runner is re-dispatching this stage
	// because a downstream auto_gate failed. Prompt builders use it to tell
	// the fresh agent it is a retry: which gate failed, and that any plan-
	// store slice status advanced by the discarded attempt is stale and the
	// work must be redone. Nil on a first attempt.
	RetryContext *StageRetryContext
	// MergeRecoveryPipelineCreateAttempted is durable across merge-stage
	// attempts and operator restarts. Once true, recovery may reconcile an
	// existing same-identity API pipeline but must never POST another one.
	MergeRecoveryPipelineCreateAttempted bool
	// HeadTransitionSeq is the run's durable CI re-authorization fence: the
	// highest settled MR head-MOVEMENT seq in the mr_head_transitions ledger
	// at dispatch time (0 for a run whose head never moved, which is every
	// legacy run). ci_watch stamps it alongside ci_sha; merge refuses to
	// proceed when the stamp no longer matches, so a head that moved after CI
	// went green can never be merged on the strength of the old verdict (#374).
	HeadTransitionSeq int64
	Budget            store.Budget
	// Env carries the LOOM_MILLS_* variables every worker propagates to
	// child processes (spawn, devbox exec, mcp tool calls). Workers may
	// add more before invoking the underlying client.
	Env map[string]string
}

// Worker executes one stage. The dispatcher resolves stage id → Worker
// at runtime; tests inject fakes that record calls and return canned
// StageOutputs.
type Worker interface {
	Run(ctx context.Context, jc JobContext) (StageOutput, error)
}

// Dispatcher routes stages to workers. The zero value is unusable; use
// NewDispatcher.
type Dispatcher struct {
	routes  map[string]Worker
	fallthr Worker
}

// NewDispatcher constructs a Dispatcher from a route table. A nil routes
// map is allowed; the dispatcher then falls back to fallback for every
// stage. A nil fallback errors on unmapped stages — the recommended mode
// for production so a typo can't silently no-op a write stage.
func NewDispatcher(routes map[string]Worker, fallback Worker) *Dispatcher {
	if routes == nil {
		routes = make(map[string]Worker)
	}
	return &Dispatcher{routes: routes, fallthr: fallback}
}

// Dispatch implements WorkerDispatcher. It looks up the stage's worker
// and invokes it with a populated JobContext.
func (d *Dispatcher) Dispatch(
	ctx context.Context,
	run *store.PipelineRun,
	item *store.BacklogItem,
	stage Stage,
	prior map[string]StageOutput,
) (StageOutput, error) {
	if d == nil {
		return StageOutput{}, errors.New("dispatcher: nil")
	}
	w := d.routes[stage.ID]
	if w == nil {
		w = d.fallthr
	}
	if w == nil {
		return StageOutput{}, fmt.Errorf("dispatcher: no worker for stage %q", stage.ID)
	}
	env := BuildMillsEnv(run, item, stage)
	env["LOOM_MILLS_ATTEMPT"] = strconv.Itoa(stageAttemptFromContext(ctx))
	jc := JobContext{
		Run:                                  run,
		Item:                                 item,
		Stage:                                stage,
		Prior:                                prior,
		ResumeSpawnID:                        resumeSpawnIDFromContext(ctx),
		Attempt:                              stageAttemptFromContext(ctx),
		RetryContext:                         StageRetryContextFromContext(ctx),
		MergeRecoveryPipelineCreateAttempted: mergeRecoveryPipelineCreateAttemptedFromContext(ctx),
		HeadTransitionSeq:                    headTransitionSeqFromContext(ctx),
		Budget:                               item.Budget,
		Env:                                  env,
	}
	return w.Run(ctx, jc)
}

// Register adds or replaces the worker for a stage id. Useful when wiring
// the operator at startup (slice 4.7) and when tests want to swap one
// worker without reconstructing the whole route table.
func (d *Dispatcher) Register(stageID string, w Worker) {
	if d.routes == nil {
		d.routes = make(map[string]Worker)
	}
	d.routes[stageID] = w
}

// BuildMillsEnv returns the canonical LOOM_MILLS_* env-var bundle every
// worker forwards to its child. Persisted contract — child processes
// (spawn, devbox exec, mcp tools) parse these to record their parent
// run for cost attribution and audit.
func BuildMillsEnv(run *store.PipelineRun, item *store.BacklogItem, stage Stage) map[string]string {
	env := map[string]string{
		"LOOM_MILLS_RUN_ID":     run.ID,
		"LOOM_MILLS_BACKLOG_ID": item.ID,
		"LOOM_MILLS_STAGE":      stage.ID,
	}
	if run.ParentSessionID != "" {
		env["LOOM_PARENT_SESSION_ID"] = run.ParentSessionID
	}
	if run.WorktreePath != "" {
		env["LOOM_MILLS_WORKTREE"] = run.WorktreePath
	}
	if branch := BranchContractFor(run, item, stage, "").SourceBranch; branch != "" {
		env["LOOM_MILLS_BRANCH"] = branch
	}
	return env
}

// ----- Default worker implementations -----
//
// Each worker is a thin shell over a Client interface so the operator
// can wire a real backend at startup and tests can inject a fake. The
// concrete network glue lives in slice 4.7's main.go wiring; slice 4.2
// ships the contract surfaces and a fallback no-op.

// ErrSpawnPollTimeout is returned (wrapped) by a SpawnClient when polling
// an accepted spawn exceeds its poll deadline without the spawn reaching a
// terminal status. Spawn backends MUST wrap it
// (fmt.Errorf("...: %w", ErrSpawnPollTimeout)) so the runner can tell a
// stalled-but-alive spawn apart from a transient poll interruption: repeated
// poll timeouts on the SAME spawn attempt are converted into a failed
// (transient-class) attempt that burns the retry budget, instead of parking
// the stage pending forever and re-attaching to the same dead spawn on every
// reconciler tick. See clients.HUDSpawnClient.pollSpawn and Runner.runStage.
var ErrSpawnPollTimeout = errors.New("spawn: poll deadline exceeded")

// ErrSpawnTerminalFailure is returned (wrapped) by a SpawnClient when the
// spawn itself reached a terminal non-completed status (failed/stopped).
// Unlike a poll timeout, the spawn can never make further progress, and a
// resume/retry keyed on the same deterministic idempotency key re-attaches to
// the SAME dead spawn — so callers whose retry semantics are attach-by-key
// (the imperative workflow runtime) MUST treat this as terminal for the run
// rather than retrying every tick forever. Spawn backends wrap it
// (fmt.Errorf("...: %w", ErrSpawnTerminalFailure)); see
// clients.HUDSpawnClient.pollSpawn and workflow.WorkflowInterpreter.Run.
var ErrSpawnTerminalFailure = errors.New("spawn: terminal non-completed status")

// ErrPipelinePollTimeout is returned (wrapped) by a GitLabClient when the
// ci_watch stage's PollPipeline exceeds its PollDeadline without the MR's
// branch pipeline reaching a terminal state. Clients MUST wrap it
// (fmt.Errorf("...: %w", ErrPipelinePollTimeout)) so Classify can tag the
// failure ClassInfra instead of the default ClassCode: a poll timeout means
// the pipeline is stuck/slow at the CI layer, not that the diff is a real code
// bug, so conflating it with code failures both mis-attributed the escalation-
// class metric (escalations #149/#153) and buried it among genuine build/test
// breaks. Infra shares Code's retry accounting (both count against MaxAttempts,
// neither is a free transient retry), so the total wall-clock is bounded at
// MaxAttempts × PollDeadline while the class is reported at the cluster/CI layer
// where the fix lives. Wrapped errors should also embed the pipeline web_url so
// the escalation is directly actionable.
var ErrPipelinePollTimeout = errors.New("pipeline: poll deadline exceeded")

// ErrMRHeadSHAUnavailable is returned (wrapped) by a GitLabClient when the MR
// reported no head SHA for the whole bounded head-SHA window, so the branch
// pipeline ci_watch is waiting for can never materialize. It is DISTINCT from
// ErrPipelinePollTimeout on purpose: a poll timeout means a real pipeline is
// slow, and ci_watch answers it by extending the watch. A headless MR has no
// pipeline to extend toward, so the same extension logic turned a deterministic
// GitLab state into 90 minutes of "MR <n> head sha pending" followed by a stall
// escalation naming a pipeline that never existed (three such escalations on
// 2026-07-26; !1239 sat at sha=null / has_conflicts=true / merge_status=
// cannot_be_merged for 15 hours). Classify maps it ClassConfig — the fix is a
// rebase, repush, or reopen in GitLab, and an identical re-poll can only
// observe the identical state.
var ErrMRHeadSHAUnavailable = errors.New("pipeline: merge request head sha never materialized")

// ErrBranchPipelineUnavailable is the with-a-head-SHA twin of
// ErrMRHeadSHAUnavailable: GitLab published the MR head, but no `push` pipeline
// for it ever appeared within the bounded branch-pipeline window. The causes are
// all project configuration — `workflow:rules` that admit only
// `merge_request_event`, CI disabled on the project, an operator deleting the
// pipeline — so a re-poll observes the identical nothing. It is DISTINCT from
// ErrPipelinePollTimeout for the same reason its twin is: ci_watch answers a
// poll timeout by extending the watch, which for a pipeline that cannot exist
// only replays "MR <n> branch pipeline pending for <sha>" across the full 90m
// cap and then blames a phantom stall. Classify maps it ClassConfig.
var ErrBranchPipelineUnavailable = errors.New("pipeline: no branch pipeline for merge request head sha")

// ErrMergeRequestClosed marks a merge that must stop because an operator closed
// the MR. Clients wrap it so Classify can preserve that manual stop as terminal
// configuration rather than retrying the merge stage as a code failure.
var ErrMergeRequestClosed = errors.New("pipeline: merge request is closed")

// ErrMergeAuthorizationStale marks a deterministic mismatch between the
// CI-authorized source/ref/SHA tuple and GitLab's current state. Retrying the
// same merge cannot make that authorization valid; a new gate/CI cycle is
// required.
var ErrMergeAuthorizationStale = errors.New("pipeline: merge authorization is stale")

// MergeSourceSHAMismatchError is the ONE stale-authorization shape that names
// a head movement rather than a routing defect: the MR still points at the
// authorized source and target branch, but its head SHA is no longer the SHA
// CI tested. It carries both SHAs structurally so the runner can mint a
// durable external head-transition row (#374) instead of scraping them out of
// an error string.
//
// It wraps ErrMergeAuthorizationStale, so every existing errors.Is check and
// the error text both behave exactly as before.
type MergeSourceSHAMismatchError struct {
	MRIID        int64
	Project      string
	SourceBranch string
	TargetBranch string
	// ReviewedSHA is the head CI authorized (the ci_sha artifact).
	ReviewedSHA string
	// ObservedSHA is the head GitLab reports now — the successor.
	ObservedSHA string
	// Message preserves the historical error text verbatim so error-class
	// needles and operator-facing strings are unchanged.
	Message string
}

func (e *MergeSourceSHAMismatchError) Error() string {
	if e == nil {
		return "pipeline: merge source sha mismatch"
	}
	return e.Message
}

func (e *MergeSourceSHAMismatchError) Unwrap() error { return ErrMergeAuthorizationStale }

// ----- MR head observation (#374 slice 1) -----
//
// These types are the contract for reading GitLab's after-the-fact record of
// head movements. Nothing in the pipeline mutates GitLab through them: the
// observation path only ever reports what GitLab already did. The rebase
// trigger itself lands in a later slice behind a default-off policy flag.

// HeadVerdict is the classifier outcome for one observed head movement.
type HeadVerdict string

const (
	// HeadVerdictNoop means the MR head never moved — decided by SHA equality
	// on the MR itself, never by absence of ledger evidence.
	HeadVerdictNoop HeadVerdict = "noop"
	// HeadVerdictAttributed means exactly one diff-refresh moved the head from
	// the reviewed SHA to the successor, with corroborating (or absent) push
	// evidence. It licenses a re-gate, never a re-use of the CI verdict.
	HeadVerdictAttributed HeadVerdict = "attributed"
	// HeadVerdictAmbiguous means the movement cannot be attributed: more than
	// one movement, a chain that does not start at the reviewed SHA, a
	// movement with no witness at all, or a settle deadline exhausted.
	HeadVerdictAmbiguous HeadVerdict = "ambiguous"
	// HeadVerdictFailed means GitLab reported the rebase itself failed
	// (rebase_in_progress=false with a non-empty merge_error).
	HeadVerdictFailed HeadVerdict = "failed"
)

// HeadCursorRequest asks for the ledger positions to snapshot BEFORE a
// mutation, so the observation afterwards can enumerate only new rows.
type HeadCursorRequest struct {
	Project      string
	MRIID        int64
	SourceBranch string
	TargetBranch string
	Env          map[string]string
}

// HeadCursors is the pre-mutation snapshot: the MR head, the newest MR
// version id, the newest push-event id on the source branch, and the target
// branch tip.
type HeadCursors struct {
	SHA              string
	VersionsCursor   int64
	EventsCursor     int64
	TargetHeadSHA    string
	RebaseInProgress bool
}

// HeadObservationRequest asks the client to settle and classify a head
// movement relative to the SHA that held the CI authorization.
type HeadObservationRequest struct {
	Project      string
	MRIID        int64
	SourceBranch string
	TargetBranch string
	// ReviewedSHA is the head CI authorized. Equality against the live MR
	// head is the ONLY way a noop is decided.
	ReviewedSHA string
	// VersionsCursor / EventsCursor are the pre-mutation snapshot; only rows
	// with a strictly greater id count as new evidence.
	VersionsCursor int64
	EventsCursor   int64
	// SettleDeadline caps the wait for rebase_in_progress to clear. Zero
	// resolves from LOOM_MILLS_MERGE_REBASE_SETTLE_SECONDS, then 120s.
	SettleDeadline time.Duration
	Env            map[string]string
}

// MRVersionRecord is one row of GET .../merge_requests/:iid/versions — the
// PRIMARY witness for a head movement, because GitLab writes it on the MR's
// own diff-refresh path rather than the async activity feed.
type MRVersionRecord struct {
	ID             int64  `json:"id"`
	HeadCommitSHA  string `json:"head"`
	BaseCommitSHA  string `json:"base"`
	StartCommitSHA string `json:"start"`
	CreatedAt      string `json:"at"`
}

// PushEventRecord is one row of GET .../events?action=pushed, filtered to the
// source branch. CORROBORATION only: the activity feed is asynchronous, so an
// empty set never contradicts a version row.
type PushEventRecord struct {
	ID         int64  `json:"id"`
	CommitFrom string `json:"from"`
	CommitTo   string `json:"to"`
	Ref        string `json:"ref"`
	Author     string `json:"author"`
	Action     string `json:"action"`
	CreatedAt  string `json:"at"`
}

// HeadObservation is the classifier's verdict plus the verbatim evidence that
// produced it, so an escalation is diagnosable without re-querying GitLab.
type HeadObservation struct {
	Verdict          HeadVerdict
	Reason           string
	SuccessorSHA     string
	RebaseInProgress bool
	MergeError       string
	Versions         []MRVersionRecord
	Pushes           []PushEventRecord
	VersionsCursor   int64
	EventsCursor     int64
	Attempts         int
	SettledAfterMS   int64
}

// State maps a verdict onto the durable ledger state.
func (v HeadVerdict) State() store.MRHeadTransitionState {
	switch v {
	case HeadVerdictNoop:
		return store.MRHeadTransitionNoop
	case HeadVerdictAttributed:
		return store.MRHeadTransitionAttributed
	case HeadVerdictFailed:
		return store.MRHeadTransitionFailed
	default:
		return store.MRHeadTransitionAmbiguous
	}
}

// Provenance renders the evidence bundle stored in
// mr_head_transitions.provenance_json.
func (o HeadObservation) Provenance() map[string]any {
	versions := o.Versions
	if versions == nil {
		versions = []MRVersionRecord{}
	}
	pushes := o.Pushes
	if pushes == nil {
		pushes = []PushEventRecord{}
	}
	return map[string]any{
		"versions_cursor_before": o.VersionsCursor,
		"events_cursor_before":   o.EventsCursor,
		"versions_after":         versions,
		"pushes_after":           pushes,
		"rebase_poll": map[string]any{
			"attempts":         o.Attempts,
			"settled_after_ms": o.SettledAfterMS,
			"merge_error":      o.MergeError,
		},
		"classifier": string(o.Verdict),
		"reason":     o.Reason,
	}
}

// ErrMergeRecoveryConfig marks a deterministic configuration rejection while
// preparing merge readiness (for example GitLab rejecting an API pipeline
// because an included project is inaccessible). HTTP 5xx errors are not
// wrapped with this sentinel and remain transient.
var ErrMergeRecoveryConfig = errors.New("pipeline: merge recovery configuration rejected")

// ErrMergeRequestLocked marks GitLab's temporary locked MR state after the
// client's in-stage reconciliation deadline expires. It is transient rather
// than configuration: a later bounded Runner attempt may observe the lock
// released, while the client guarantees it issues no merge PUT while locked.
var ErrMergeRequestLocked = errors.New("pipeline: merge request is temporarily locked")

// ErrCIPipelineTerminal is returned (wrapped) by the ci_watch stage when the
// MR's branch pipeline reached a terminal NON-success state (failed/canceled/
// skipped). Unlike a poll timeout, this outcome is deterministic per-stage: a
// retry re-polls the same finished pipeline and returns the identical error
// within seconds (escalation #292, 2026-07-08: retries 2 and 3 completed in
// 2.1s and 1.3s with zero new signal). The runner escalates on first sight
// instead of burning the attempt budget; the error class from Classify is
// preserved (code — the diff broke CI) so metrics stay honest.
var ErrCIPipelineTerminal = errors.New("pipeline: ci reached terminal non-success state")

// CIWatchStalledError is returned by the ci_watch stage when the MR's branch
// pipeline was STILL RUNNING at the ci_watch watch hard cap
// (MILLS_CI_WATCH_MAX_MINUTES, default 90m) after exhausting its poll-session
// extensions. It is NOT a code failure: a slow-but-healthy CI run stalled the
// watch (live evidence 2026-07-16: 7/7d ci_watch errors were poll timeouts with
// the pipeline status still running/pending, 5 runs escalated here — the single
// largest escalation sink). The runner escalates once as a RETRYABLE
// external-dependency incident keyed on the stuck pipeline URL (a later requeue
// can still succeed once CI drains) instead of burning the attempt budget or
// blaming the diff. It unwraps to ErrPipelinePollTimeout so a caller that does
// not special-case it still classifies it ClassInfra rather than ClassCode. (S3)
type CIWatchStalledError struct {
	PipelineURL string
	MaxMinutes  int
	MRIID       int64
	LastStatus  string
}

func (e *CIWatchStalledError) Error() string {
	loc := e.PipelineURL
	if loc == "" {
		loc = "pipeline url unknown"
	}
	status := e.LastStatus
	if status == "" {
		status = "running"
	}
	return fmt.Sprintf("ci_watch: mr %d pipeline still %s after the %dm watch cap (%s)",
		e.MRIID, status, e.MaxMinutes, loc)
}

// Unwrap lets errors.Is(err, ErrPipelinePollTimeout) and Classify treat an
// unhandled stall as ClassInfra (bounded retries) rather than ClassCode.
func (e *CIWatchStalledError) Unwrap() error { return ErrPipelinePollTimeout }

const (
	// ciWatchDefaultMaxMinutes is the total ci_watch wall-clock cap, spread
	// across poll-session extensions, when MILLS_CI_WATCH_MAX_MINUTES is unset.
	// The default (90m) is 3× the 30m per-poll-session deadline so a slow CI run
	// gets two extensions before it is treated as an external-dependency stall.
	ciWatchDefaultMaxMinutes = 90
	// ciWatchPollSessionMinutes mirrors the GitLab client's default PollPipeline
	// deadline (clients.GitLabConfig.PollDeadline, 30m). It is the reference
	// per-session watch window used to size how many extensions the hard cap
	// grants; the client remains the authority on the actual per-session wait.
	ciWatchPollSessionMinutes = 30
	// ciWatchExternalDependency is the stable dependency name recorded on a
	// ci_watch stall escalation. The per-run external_dependency_id is the stuck
	// pipeline URL (unique per MR); this names the dependency class for labels.
	ciWatchExternalDependency = "gitlab_ci"
)

// ciWatchMaxMinutes resolves the total ci_watch wall-clock cap from
// MILLS_CI_WATCH_MAX_MINUTES (minutes). The stage env map is checked first so
// the operator can thread it per-run/tests can inject it, then the process env.
// A missing, non-positive, or unparseable value falls back to the default.
func ciWatchMaxMinutes(env map[string]string) int {
	raw := ""
	if env != nil {
		raw = strings.TrimSpace(env["MILLS_CI_WATCH_MAX_MINUTES"])
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("MILLS_CI_WATCH_MAX_MINUTES"))
	}
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return ciWatchDefaultMaxMinutes
}

// ciWatchExtensionBudget is how many EXTRA poll sessions runCI grants beyond the
// first before it treats a still-running pipeline as an external-dependency
// stall. 0 preserves the pre-S3 single-session behavior (escalate on the first
// timeout). For the 90m default over 30m sessions this is 2.
func ciWatchExtensionBudget(env map[string]string) int {
	maxMin := ciWatchMaxMinutes(env)
	sessions := maxMin / ciWatchPollSessionMinutes
	if maxMin%ciWatchPollSessionMinutes != 0 {
		sessions++ // ceil: a partial session still gets a full watch window
	}
	if sessions < 1 {
		sessions = 1
	}
	return sessions - 1
}

// SpawnClient is the operator-facing facade over the spawn HTTP service.
// Implementations live outside this package; tests use a fake.
type SpawnClient interface {
	Run(ctx context.Context, req SpawnRequest) (SpawnResponse, error)
}

// SpawnResumeClient is implemented by spawn backends that can re-attach
// to an already accepted spawn after an operator restart.
//
// Resume carries only the spawn id, so a backend implementing ONLY this
// interface cannot run the post-terminal cumulative git capture — it has
// no branch/base/checkout coordinates. Prefer SpawnContextResumeClient.
type SpawnResumeClient interface {
	Resume(ctx context.Context, spawnID string) (SpawnResponse, error)
}

// SpawnResumeContext carries the coordinates the spawn client's
// post-terminal cumulative git capture (clients.attachGitContext) needs.
// They are identical to the fields SpawnRequest supplies on the initial
// dispatch; a resume re-attaches to an existing spawn id and therefore
// never rebuilds a SpawnRequest, which is why they must be threaded
// separately.
type SpawnResumeContext struct {
	// Project is the repo the spawn targets (HUD checkout resolution).
	Project string
	// WorkingDir is the operator-readable checkout the capture fetches
	// and diffs in — the run's worktree, else the operator-local clone.
	WorkingDir string
	// BaseBranch is the ref the branch diff is taken against.
	BaseBranch string
	// Branch is the spawn's source branch.
	Branch string
}

// SpawnContextResumeClient is implemented by spawn backends that can
// re-attach to an accepted spawn AND still run the post-terminal
// cumulative git capture.
//
// The operator re-attaches on every pod rollout (reconciler
// pickupInFlightRuns → Runner.Start → SpawnResumeClient.Resume), and the
// deployment uses a Recreate strategy driven by Flux image automation, so
// the resume path is hot, not exceptional. While resume dropped the
// capture coordinates, every stage that finished across a rollout produced
// per-attempt telemetry only — the exact signature of issue #224 (a fresh
// agent finds its work already pushed, the pipeline cannot see the diff,
// and the attempt burns retry budget on nonempty_diff).
type SpawnContextResumeClient interface {
	ResumeWithContext(ctx context.Context, spawnID string, rc SpawnResumeContext) (SpawnResponse, error)
}

// SpawnRequest carries every field the spawn API needs to start a
// subordinate Claude/Codex/Gemini run for this stage.
type SpawnRequest struct {
	Prompt     string
	WorkingDir string
	// Model — despite its name — selects the spawn AGENT/harness VENDOR
	// (claude-code|codex|gemini), NOT the LLM model. It is mapped to the HUD
	// spawn API's agent_type via clients.agentTypeOrDefault. The vendor-native
	// LLM model id is carried separately in AgentModel. This naming quirk is
	// load-bearing history; renaming it touches many call sites and tests.
	Model string
	// AgentModel is the vendor-native LLM model id (e.g. "gpt-5.6-terra") the
	// spawned agent CLI should run. Distinct from Model (which selects the
	// VENDOR). Empty means "no per-spawn override" — the HUD spawn server then
	// applies its own vendor default (SPAWN_CODEX_MODEL env / resolveCodexModel
	// for codex). Only the codex spawn path consumes it today; claude-code and
	// gemini have no CLI model knob, so a set value is ignored with a wiring
	// log on the spawn server rather than erroring. Populated by SpawnWorker
	// from the RouteFor decision (env break-glass > agent/* label > agent_routing
	// rule > policy stage_models > empty).
	AgentModel      string
	Env             map[string]string
	BudgetUSD       float64
	BudgetTurns     int
	BudgetMinutes   int
	ParentSessionID string
	StageID         string
	OnAccepted      func(spawnID string) error

	// BacklogID, Project, Branch, BaseBranch, and Namespace are
	// populated by SpawnWorker from JobContext for spawn services that
	// require git + agent-context routing (the loom HUD mobile API).
	// Stage workers that don't need them ignore them.
	BacklogID  string
	Project    string
	Branch     string
	BaseBranch string
	Namespace  string

	// Substrate is the devbox backend identifier the spawn service should
	// run this stage against (e.g. "k8s", "harvester-vm"). Populated by
	// SpawnWorker from policy via SubstrateFor; empty means "use the
	// spawn service's default backend" — current behavior pre-Slice-2b.
	// The HUD spawn server consumes this in a follow-up slice (2c) by
	// translating it to DEVBOX_BACKEND on the spawn pod's env. Spec:
	// .loom/45-product-spec-mills-harvester-vm-substrate-2026-05-25.md
	Substrate string

	// CompletionHoldSeconds is an opt-in driver-owned foreground hold after a
	// successful agent command. Zero preserves the normal completion path. It
	// exists for deployed lifecycle fault tests that must keep the exact spawn
	// running without relying on model/tool-session behavior.
	CompletionHoldSeconds int

	// IdempotencyKey is a caller-supplied replay key. Empty means legacy
	// behavior (server-minted spawn id). When non-empty, the HUD spawn
	// client sends it as idempotency_key so the controller derives a
	// deterministic id and dedupes a duplicate create into an AlreadyExists
	// re-attach. Set by the Mills durable runtime (Slice 2b, .loom/130-133)
	// AND — since the unkeyed-spawn restart fix — by SpawnWorker itself
	// (stageIdempotencyKey), so a mobile-hud restart mid-turn re-drives or
	// re-attaches the stage spawn instead of fail-fasting it with "agent
	// turn driver lost across mobile-hud restart; unkeyed spawn cannot be
	// re-driven".
	IdempotencyKey string
}

// SpawnResponse summarises what the spawn returned. Workers translate
// it into a StageOutput for the runner.
type SpawnResponse struct {
	SpawnID        string
	CostUSD        float64
	LogTail        string
	FilesChanged   []string
	LinesAdded     int
	LinesRemoved   int
	DiffPatch      []byte
	CommitMessages []string
	Artifacts      map[string]any

	// CostEstimated records whether CostUSD is a Loom-side estimate
	// rather than an authoritative SDK figure. It mirrors
	// bridge.SpawnTelemetry.CostEstimated, which the HUD spawn API
	// already serialises on the wire (cost_estimated) but the operator's
	// telemetry subset historically dropped. Additive provenance only —
	// it never changes CostUSD's value. Codex spawns set it true; Claude
	// and Gemini leave it false. The Layer-1 Worker contract derives
	// CostSource (real|estimated|unavailable) from this plus CostUSD.
	CostEstimated bool

	// TokenUsage is the spawn's cumulative token accounting, mirroring
	// bridge.SpawnTokenUsage. Before this field a spawn-dispatched stage
	// reported cost-USD only, so "did the agent harness get its prompt
	// prefix cached" was unanswerable for the stages that burn the most
	// budget (implement, plan_slice, pr_self_review). Read-only: nothing
	// in mills decides on it yet. The zero value means the harness
	// reported no usage — see SpawnTokenUsage.Reported.
	TokenUsage SpawnTokenUsage
}

// SpawnTokenUsage is one spawn's cumulative token accounting across every
// turn, mirroring bridge.SpawnTokenUsage on the HUD side.
//
// The four counts are ADDITIVE, not overlapping: a turn's billed prompt is
// InputTokens+CacheCreationTokens+CacheReadTokens. This is Loom's canonical
// convention and it is not what every harness reports natively — Codex's
// `cached_input_tokens` is a SUBSET of its `input_tokens`, which
// internal/hud/spawn_codex_parser.go subtracts out before accumulating. Do
// not re-derive a share by dividing into InputTokens.
type SpawnTokenUsage struct {
	// InputTokens is the FRESH (uncached) prompt across all turns.
	InputTokens int
	// OutputTokens is the generated length across all turns.
	OutputTokens int
	// CacheCreationTokens is the prompt written INTO the cache (charged at
	// a premium by Anthropic). Only the Claude harness reports it; Codex
	// and Gemini leave it zero.
	CacheCreationTokens int
	// CacheReadTokens is the prompt served FROM the cache — the number the
	// prompt-cache question turns on.
	CacheReadTokens int
}

// Reported says whether the harness supplied any token accounting at all.
// A flat zero is ambiguous (no usage reported vs. a spawn that truly burned
// nothing), so callers gate on this rather than treating zeros as measured —
// the same discipline pkg/llmusage applies to completion usage.
func (u SpawnTokenUsage) Reported() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 ||
		u.CacheCreationTokens > 0 || u.CacheReadTokens > 0
}

// SpawnWorker dispatches a stage to the spawn service. Used for
// plan_slice, pr_self_review, and implement (with a worktree).
type SpawnWorker struct {
	Client SpawnClient
	// Model overrides the request's model field. Empty falls through
	// to the spawn service default.
	Model string
	// PromptFor returns the prompt body for this stage. The operator
	// supplies a function that pulls from the spec doc / sidecar; tests
	// inject a static string returner.
	PromptFor func(jc JobContext) string
	// NeedsWorktree marks stages that must allocate a per-run worktree
	// before invoking the spawn (implement). The allocator wires in
	// from outside; the worker just propagates run.WorktreePath.
	NeedsWorktree bool
	// Project is the repo name spawns target. Falls back to
	// "loom-core" when empty. The HUD spawn API needs this to resolve
	// the worktree base + git remote.
	Project string
	// Namespace is the agent_context namespace the spawn writes into.
	// Falls back to "loom-mills" — the same namespace the operator's
	// own session uses, so handoffs stay routable.
	Namespace string
	// BaseBranch is what spawned worktrees branch off AND the base ref
	// the spawn client's post-terminal git capture diffs against
	// (clients.attachGitContext). Empty resolves to "main" in Run — the
	// same default the HUD spawn server applies — so an unwired field
	// can no longer silently disable the cumulative-diff capture
	// (issue #224).
	BaseBranch string
	// RepoRoot is the operator-local clone used as the git-capture
	// working dir when the run has no allocated worktree
	// (jc.Run.WorktreePath == "" — the standard non-integrator path,
	// where NeedsWorktree is a marker nothing reads). The capture only
	// fetches and diffs refs, never checks anything out, so sharing the
	// operator's main clone across concurrent runs is safe. Empty with
	// no run worktree skips the capture (legacy behavior).
	RepoRoot string
	// SubstrateFor returns the devbox backend the spawn service should
	// use for the given stage id. The operator wires this from
	// PolicyManager.Current().SubstrateForStage so hot-reloaded policy
	// changes take effect on the next stage attempt. Nil-safe: when
	// unset, Run leaves SpawnRequest.Substrate empty and the spawn
	// service falls back to its compiled-in default backend (current
	// behavior pre-Slice-2b). Spec: .loom/45-product-spec-mills-…
	SubstrateFor func(stage string) string

	// RouteFor returns the spawn agent (harness/vendor id, e.g.
	// "claude-code", "codex", "gemini") AND the vendor-native LLM model id
	// (e.g. "gpt-5.6-sol") the spawn service should run this stage of THIS
	// backlog item on. Agent and model resolve together because a model id is
	// only meaningful to the vendor that owns it, so splitting them across two
	// callbacks would let a re-targeted harness inherit the other vendor's
	// model pin.
	//
	// The item is threaded in so per-ITEM routing (agent/* label,
	// pipeline.agent_routing rules) can decide alongside the per-STAGE
	// stage_agents map — this is what lets claude-code and codex implementers
	// run simultaneously across the queue. The operator wires
	// cmd/loom-mills-operator.spawnRouteFor, which owns the full precedence
	// chain (env break-glass > label > rule > stage_agents > default) and
	// re-reads policy on every call so hot-reloaded routing takes effect on
	// the next stage attempt. ctx is carried so that closure can record the
	// decision on the event stream at dispatch time.
	//
	// Nil-safe: when unset, Run keeps w.Model and leaves AgentModel empty
	// (pre-routing behavior). A non-empty Agent OVERRIDES Model for this
	// stage; a non-empty Model sets SpawnRequest.AgentModel, which — for the
	// codex path — pins `codex exec --model`.
	RouteFor func(ctx context.Context, stage string, item *store.BacklogItem) mills.AgentDecision
}

// Run satisfies Worker.
func (w *SpawnWorker) Run(ctx context.Context, jc JobContext) (StageOutput, error) {
	if w.Client == nil {
		return StageOutput{}, fmt.Errorf("spawn worker: client not configured for %s", jc.Stage.ID)
	}
	prompt := ""
	if w.PromptFor != nil {
		prompt = w.PromptFor(jc)
	}
	home := w.Project
	if home == "" {
		home = "loom-core"
	}
	// Per-item cross-repo routing: an item's TargetProject overrides the
	// worker's home repo. Empty target = home (unchanged behavior). The
	// reconciler's fail-closed gate ensures a cross-repo item only reaches
	// here when cross_repo execution is enabled.
	project := effectiveProject(jc.Item, home)
	namespace := w.Namespace
	if namespace == "" {
		namespace = "loom-mills"
	}
	branch := BranchContractFor(jc.Run, jc.Item, jc.Stage, "").SourceBranch
	if branch == "" {
		return StageOutput{}, fmt.Errorf("spawn worker: source branch unavailable for backlog %q", jc.Item.ID)
	}
	substrate := ""
	if w.SubstrateFor != nil {
		substrate = w.SubstrateFor(jc.Stage.ID)
	}
	// Per-item, per-stage harness + model selection: RouteFor resolves the
	// effective agent and vendor model together (env break-glass > agent/*
	// label > agent_routing rule > policy stage_agents > default, all folded
	// into the operator's closure). A non-empty Agent overrides the worker's
	// static Model; a nil closure preserves w.Model and leaves AgentModel
	// empty — so behavior is byte-identical when nothing is configured.
	model := w.Model
	agentModel := ""
	routing := mills.AgentDecision{}
	if w.RouteFor != nil {
		routing = w.RouteFor(ctx, jc.Stage.ID, jc.Item)
		if routing.Agent != "" {
			model = routing.Agent
		}
		agentModel = routing.Model
	}
	baseBranch := w.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	// Prefer the run's own worktree (integrator path); fall back to the
	// operator-local clone so the spawn client's git capture has a repo
	// to fetch + diff in on the standard path. The operator-local clone
	// (w.RepoRoot) is the HOME repo, so it is only a valid capture dir when
	// this item targets the home repo — for a cross-repo item leave workingDir
	// empty (skip the operator-local capture); the spawn pod does its own git
	// capture on the freshly cloned target repo.
	workingDir := jc.Run.WorktreePath
	if workingDir == "" && store.SameRepo(project, home) {
		workingDir = w.RepoRoot
	}
	req := SpawnRequest{
		Prompt:          prompt,
		WorkingDir:      workingDir,
		Model:           model,
		AgentModel:      agentModel,
		Env:             jc.Env,
		BudgetUSD:       jc.Budget.MaxCostUSD,
		BudgetTurns:     jc.Budget.MaxTurns,
		BudgetMinutes:   jc.Budget.MaxPipelineMinutes,
		ParentSessionID: jc.Run.ParentSessionID,
		StageID:         jc.Stage.ID,
		OnAccepted:      stageAcceptRecorderFromContext(ctx),
		BacklogID:       jc.Item.ID,
		Project:         project,
		Branch:          branch,
		BaseBranch:      baseBranch,
		Namespace:       namespace,
		Substrate:       substrate,
		IdempotencyKey:  stageIdempotencyKey(jc),
	}
	// Attribute this stage's cost to the resolved agent/model + the spawn
	// substrate for the per-model telemetry roll-up. Stamped on every return
	// (including error/park paths) so the persisted stage row carries identity
	// — including WHY this harness was chosen, so the routing decision is
	// answerable from the stage_results row and not only from the event stream.
	stamp := func(out StageOutput) StageOutput {
		out.Model = model
		out.Backend = spawnBackendLabel
		if mills.AgentRouted(routing) {
			if out.Artifacts == nil {
				out.Artifacts = map[string]any{}
			}
			out.Artifacts[agentRoutingArtifactKey] = map[string]any{
				"agent":      model,
				"model":      agentModel,
				"decided_by": routing.DecidedBy,
			}
		}
		return out
	}
	if jc.ResumeSpawnID != "" {
		// Prefer the context-carrying resume so a stage that finishes
		// across an operator rollout still gets the cumulative
		// branch-vs-base capture. The coordinates are the same ones the
		// initial dispatch put on req; a bare SpawnResumeClient remains
		// supported (older/test backends) but captures nothing.
		if ctxResumer, ok := w.Client.(SpawnContextResumeClient); ok {
			resp, err := ctxResumer.ResumeWithContext(ctx, jc.ResumeSpawnID, SpawnResumeContext{
				Project:    project,
				WorkingDir: workingDir,
				BaseBranch: baseBranch,
				Branch:     branch,
			})
			return stamp(spawnResponseToStageOutput(resp)), err
		}
		resumer, ok := w.Client.(SpawnResumeClient)
		if !ok {
			return stamp(StageOutput{SpawnID: jc.ResumeSpawnID}), fmt.Errorf("spawn worker: client cannot resume spawn %s", jc.ResumeSpawnID)
		}
		resp, err := resumer.Resume(ctx, jc.ResumeSpawnID)
		if err != nil {
			return stamp(spawnResponseToStageOutput(resp)), err
		}
		return stamp(spawnResponseToStageOutput(resp)), nil
	}
	resp, err := w.Client.Run(ctx, req)
	if err != nil {
		return stamp(spawnResponseToStageOutput(resp)), err
	}
	return stamp(spawnResponseToStageOutput(resp)), nil
}

// spawnBackendLabel is the telemetry backend bucket for spawn-dispatched stages
// (implement / plan_slice / pr_self_review). The specific agent/model is carried
// in StageOutput.Model; "spawn" identifies the substrate that ran it.
const spawnBackendLabel = "spawn"

// stageIdempotencyKey derives the deterministic replay key for one stage
// dispatch attempt. Keying the spawn is what lets the HUD survive its own
// restart mid-turn: classifyInterruptedSpawn (internal/hud/spawn.go) re-drives
// a keyed spawn — or re-attaches to its still-running supervised turn — while
// an UNKEYED one is fail-fasted with "agent turn driver lost across mobile-hud
// restart; unkeyed spawn cannot be re-driven", which killed every in-flight
// Mills stage on every mobile-hud rollout (PIPE-bl-mills-operator-stop-lever-
// 20260726, pr_self_review, 2026-07-26).
//
// The attempt number is part of the key on purpose: spawnWithKey re-attaches
// to ANY durable state with the same key, including a terminal one, so a
// retry that reused its predecessor's key would instantly adopt the old
// failure. Attempts are monotonic per stage across operator restarts
// (seedAttempts), so attempt N's key never collides with attempt N-1's; a
// duplicate dispatch of the SAME attempt (crash in the record→dispatch
// window) dedupes into an AlreadyExists re-attach instead of a second pod.
//
// Empty when the run identity is missing or the attempt is non-positive. The
// latter is a fail-safe for custom dispatchers that forget to thread the
// runner's attempt: an unkeyed fresh spawn is expensive but correct, whereas
// an attempt-0 key aliases every retry to the first terminal spawn.
func stageIdempotencyKey(jc JobContext) string {
	if jc.Run == nil || jc.Run.ID == "" || jc.Stage.ID == "" || jc.Attempt <= 0 {
		return ""
	}
	return fmt.Sprintf("mills-stage:%s:%s:%d", jc.Run.ID, jc.Stage.ID, jc.Attempt)
}

// agentRoutingArtifactKey is the stage_results artifact slot carrying the
// harness-selection provenance ({agent, model, decided_by}). Written only when
// per-item routing actually CLAIMED the dispatch (mills.AgentRouted — an
// agent/* label or an agent_routing rule), never for the stage_agents/default
// rungs, so an operator who never opted in keeps byte-identical artifact rows.
const agentRoutingArtifactKey = "agent_routing"

// GitCaptureArtifactKey is the stage_results artifact slot carrying the
// cumulative branch-vs-base git-capture outcome the spawn client records
// on every terminal spawn: {status, reason, working_dir, base_ref,
// head_ref, files, diff_bytes, commits, resumed}.
//
// It exists so an operator can answer "did the capture run?" from a
// persisted stage row. Issue #224 was unanswerable in production for the
// opposite reason: every capture failure returned silently and reached
// the gates as zero files plus an empty diff — byte-identical to a branch
// that genuinely carries no work, which is what nonempty_diff escalates
// on. Exported because the writer lives in pkg/mills/clients.
const GitCaptureArtifactKey = "git_capture"

// Spawn token-usage artifact slots. These land in stage_results.artifacts_json
// via mergeArtifacts (which copies StageOutput.Artifacts wholesale), so they
// are queryable per stage attempt without a schema migration. Written only
// when the harness actually reported usage — a stage row that predates this,
// or one run by a harness that reports no tokens, keeps its byte-identical
// artifact map rather than gaining a misleading row of zeros.
const (
	spawnInputTokensArtifactKey         = "input_tokens"
	spawnOutputTokensArtifactKey        = "output_tokens"
	spawnCacheReadTokensArtifactKey     = "cache_read_tokens"
	spawnCacheCreationTokensArtifactKey = "cache_creation_tokens"
)

func spawnResponseToStageOutput(resp SpawnResponse) StageOutput {
	return StageOutput{
		CostUSD:        resp.CostUSD,
		SpawnID:        resp.SpawnID,
		LogTail:        resp.LogTail,
		Artifacts:      withSpawnTokenArtifacts(resp.Artifacts, resp.TokenUsage),
		FilesChanged:   resp.FilesChanged,
		LinesAdded:     resp.LinesAdded,
		LinesRemoved:   resp.LinesRemoved,
		DiffPatch:      resp.DiffPatch,
		CommitMessages: resp.CommitMessages,
		CostEstimated:  resp.CostEstimated,
	}
}

// withSpawnTokenArtifacts adds the token-usage slots to a spawn's artifact
// map. Unreported usage returns art untouched (including nil), so the only
// stage rows that change shape are the ones that gained real numbers.
//
// Each count is written independently on >0: a Codex spawn reports no
// cache-creation tokens at all, and emitting an explicit 0 for it would be
// indistinguishable from a Claude spawn that wrote nothing into the cache.
func withSpawnTokenArtifacts(art map[string]any, u SpawnTokenUsage) map[string]any {
	if !u.Reported() {
		return art
	}
	if art == nil {
		art = map[string]any{}
	}
	if u.InputTokens > 0 {
		art[spawnInputTokensArtifactKey] = u.InputTokens
	}
	if u.OutputTokens > 0 {
		art[spawnOutputTokensArtifactKey] = u.OutputTokens
	}
	if u.CacheReadTokens > 0 {
		art[spawnCacheReadTokensArtifactKey] = u.CacheReadTokens
	}
	if u.CacheCreationTokens > 0 {
		art[spawnCacheCreationTokensArtifactKey] = u.CacheCreationTokens
	}
	return art
}

// WeaverClient is the codebase-aware FlexInfer subagent facade. Used by
// the research stage to gather grounded context before implement.
type WeaverClient interface {
	Research(ctx context.Context, req WeaverRequest) (WeaverResponse, error)
}

// WeaverRequest is the bundle a research call ships off.
type WeaverRequest struct {
	// RunID is the pipeline_runs.id the call belongs to. Carried so the
	// shadow-mode diff recorder can update the research_diff column on
	// the right row (PipelineDAO.SetResearchDiff is keyed by run id).
	// Zero/empty when the caller doesn't have a run context — the
	// recorder must tolerate that and skip the persist.
	RunID     string
	BacklogID string
	Prompt    string
	Env       map[string]string
	BudgetUSD float64
	// DeclaredPaths are the file paths the backlog item's slices declare —
	// the enforced scope envelope, including the exact paths new files will
	// be created at. The research grounding guard exempts them from
	// hallucination validation: they are legitimate references even when
	// they do not exist in the repository yet.
	DeclaredPaths []string
}

// WeaverResponse carries the research notes back. Notes is appended to
// the stage_results.artifacts_json under "research_notes" for downstream
// stages.
type WeaverResponse struct {
	SpawnID string
	CostUSD float64
	LogTail string
	Notes   string
	// Model + Backend attribute the research cost to a model tier for the
	// per-model telemetry roll-up. The FlexInfer weaver client sets them to
	// the resolved model id and "flexinfer"; empty when a delegator answered
	// (the row then buckets under "unknown").
	Model    string
	Backend  string
	Citation map[string]any

	// Usage is the completion's normalized token accounting, including the
	// share of the prompt the serving engine returned from its prefix cache.
	// The FlexInfer legacy path fills it from the response's usage block; a
	// delegator that answers over the weaver RPC has no usage to report and
	// leaves it zero (Usage.Reported() is false there — read that as
	// "unknown", not as a 0% hit rate).
	Usage llmusage.Usage
}

// WeaverWorker dispatches the research stage.
type WeaverWorker struct {
	Client    WeaverClient
	PromptFor func(jc JobContext) string
}

// Run satisfies Worker.
func (w *WeaverWorker) Run(ctx context.Context, jc JobContext) (StageOutput, error) {
	if w.Client == nil {
		return StageOutput{}, fmt.Errorf("weaver worker: client not configured")
	}
	prompt := ""
	if w.PromptFor != nil {
		prompt = w.PromptFor(jc)
	}
	runID := ""
	if jc.Run != nil {
		runID = jc.Run.ID
	}
	resp, err := w.Client.Research(ctx, WeaverRequest{
		RunID:         runID,
		BacklogID:     jc.Item.ID,
		Prompt:        prompt,
		Env:           jc.Env,
		BudgetUSD:     jc.Budget.MaxCostUSD,
		DeclaredPaths: declaredSlicePaths(jc.Item),
	})
	if err != nil {
		// Research is advisory context, not load-bearing. When every candidate
		// model is unavailable (503-parked shared GPU), soft-skip with an
		// explicit note and a SUCCESS outcome instead of erroring the stage —
		// which otherwise burned retries and escalated (live 2026-07-16: 24
		// research 503s in 7d, 3 runs escalated at research against a parked
		// model). Any other error still fails the stage.
		if IsModelUnavailable(err) {
			note := researchSkipNote(err)
			return StageOutput{
				LogTail: note,
				Artifacts: map[string]any{
					"research_notes":              note,
					researchSkippedArtifactKey:    true,
					researchSkipReasonArtifactKey: note,
				},
			}, nil
		}
		return StageOutput{}, err
	}
	art := map[string]any{"research_notes": resp.Notes}
	if resp.Citation != nil {
		art["citation"] = resp.Citation
	}
	addResearchTokenArtifacts(art, resp.Usage)
	return StageOutput{
		CostUSD:   resp.CostUSD,
		SpawnID:   resp.SpawnID,
		LogTail:   resp.LogTail,
		Model:     resp.Model,
		Backend:   resp.Backend,
		Artifacts: art,
	}, nil
}

// declaredSlicePaths flattens the item's slice file lists for the research
// grounding guard. Deduplicated, first-seen order; nil when the item carries
// no slices (guard then validates every referenced path against the repo).
func declaredSlicePaths(item *store.BacklogItem) []string {
	if item == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, s := range item.Slices {
		for _, f := range s.Files {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if _, ok := seen[f]; ok {
				continue
			}
			seen[f] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

// Research token-usage artifact slots. Parallel to the spawn slots above but
// named for the completions dialect the research stage actually speaks: a
// single chat call with a prompt/completion split, where the cached count is
// a SUBSET of prompt_tokens rather than a sibling of it. Keeping the two
// vocabularies distinct stops a reader from summing them as if they were the
// same measurement.
const (
	researchPromptTokensArtifactKey     = "prompt_tokens"
	researchCompletionTokensArtifactKey = "completion_tokens"
	researchCachedTokensArtifactKey     = "cached_prompt_tokens"
)

// addResearchTokenArtifacts records the research completion's token usage in
// the stage artifact map. A no-op when the lane reported no usage, so a
// delegated (weaver-RPC) research row keeps its previous shape.
//
// cached_prompt_tokens is written only when non-zero. Zero is genuinely
// ambiguous here — vLLM builds predating prompt_tokens_details report no
// cached count at all — and an explicit 0 would let an unmeasured lane
// masquerade as a measured 0% hit rate in any aggregation over these rows.
func addResearchTokenArtifacts(art map[string]any, u llmusage.Usage) {
	if art == nil || !u.Reported() {
		return
	}
	if u.PromptTokens > 0 {
		art[researchPromptTokensArtifactKey] = u.PromptTokens
	}
	if u.CompletionTokens > 0 {
		art[researchCompletionTokensArtifactKey] = u.CompletionTokens
	}
	if u.CachedPromptTokens > 0 {
		art[researchCachedTokensArtifactKey] = u.CachedPromptTokens
	}
}

// DevboxClient is the devbox quality-gate facade used by the tests stage.
type DevboxClient interface {
	QualityGate(ctx context.Context, req DevboxRequest) (DevboxResponse, error)
}

// DevboxRequest carries the project + agent id + env to a quality-gate run.
//
// Checks, when non-empty, scopes the gate to the named subset (one of
// "fmt"/"lint"/"test"/"diff"). The canary path uses this to skip the
// codebase-wide lint/test on safe-fixture backlog items that only touch
// non-Go assets.
type DevboxRequest struct {
	Project      string
	AgentID      string
	Env          map[string]string
	Checks       []string
	TestCommands []string
}

// DevboxResponse summarises the gate verdict + per-check results.
type DevboxResponse struct {
	Passed   bool
	CostUSD  float64
	LogTail  string
	Checks   []DevboxCheck
	Language string
}

// DevboxCheck captures one fmt/lint/test run inside the gate.
type DevboxCheck struct {
	Name     string
	Passed   bool
	ExitCode int
	Duration float64
	Output   string
}

// ErrDevboxGateNoChecks is returned (wrapped) by the tests stage when the
// devbox quality gate reports not-passed but executed ZERO checks. A healthy
// gate always runs at least one check and names any failures; an empty check
// set with a not-passed verdict is an infrastructure contract violation (a
// recycled/evicted sandbox that never produced a verdict), not a test failure.
// Classify maps it to ClassTransient so the stage retries free instead of the
// empty result burning the implement budget as a phantom code failure (live
// 2026-07-16: tests "0/0 checks marked failed; gate reported not passed" ×4
// escalated as code).
var ErrDevboxGateNoChecks = errors.New("devbox: quality gate reported not-passed with no executed checks")

// DevboxWorker dispatches the tests stage.
type DevboxWorker struct {
	Client  DevboxClient
	Project string
	AgentID string
}

// gitCloneTestsScope is the devbox quality-gate selector for every Mills
// item. The hub-spawned devbox sandbox clones ONLY services/loom-core, but
// loom-core's go.work `use`s sibling repos (../../libs/fi-accel/go/fiaccel,
// fi-mcp-kit, mcp-go) and fi-accel is cgo. So whole-module `go vet ./...` /
// `go test ./...` fail immediately in the sandbox (~79ms,
// "directory ../../libs/... does not exist") regardless of the change.
// `gofmt -l .` needs no build and runs cleanly.
//
// The authoritative lint/vet/test/build runs in GitLab CI, which the
// ci_watch stage gates before merge, so scoping this pre-flight to fmt loses
// no merge safety. This scope was originally applied only to mills-canary
// items (which touch a single Markdown fixture); the first real (non-canary)
// Go change — MILLS-DEBT-TICKLABEL-20260624 — escalated at exactly this gate
// ("PASS fmt / FAIL lint (79ms)"), proving the sandbox limitation is
// universal, so the scope is now applied to all items. A richer pre-flight
// that runs the changed packages with GOWORK=off is tracked as a follow-up.
var gitCloneTestsScope = []string{"fmt"}

const skippedDeclaredTestsArtifactKey = "skipped_declared_tests"

// devboxScopeFor returns the quality-gate Checks selector for a backlog item.
// All items use the sandbox-safe scope (see gitCloneTestsScope); GitLab CI,
// enforced by the ci_watch stage, remains the authoritative lint/test/build
// gate before merge.
func devboxScopeFor(_ *store.BacklogItem) []string {
	return gitCloneTestsScope
}

// declaredTestEnvWord matches one leading KEY=value environment assignment.
// Declared tests in this workspace are conventionally written as
// "GOWORK=off go test ./pkg/..." — the sandbox checkout carries a go.work
// whose toolchain requirement the sandbox Go may not satisfy, so bare
// `go test` dies instantly on workspace resolution (observed 2026-08-14:
// "go.work requires go >= 1.26.4 (running go 1.26.0; GOTOOLCHAIN=local)",
// FAIL test:0 in ~100ms while the same command passed under GOWORK=off).
var declaredTestEnvWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^\s]*\s+`)

func declaredDevboxTests(item *store.BacklogItem) (allowed, skipped []string) {
	if item == nil {
		return nil, nil
	}
	for _, command := range item.Success.Tests {
		rest := command
		sawGowork := false
		for {
			m := declaredTestEnvWord.FindString(rest)
			if m == "" {
				break
			}
			if strings.HasPrefix(m, "GOWORK=") {
				sawGowork = true
			}
			rest = rest[len(m):]
		}
		if !strings.HasPrefix(rest, "go test ") {
			skipped = append(skipped, command)
			continue
		}
		if !sawGowork {
			// Commands run via `sh -c` in the sandbox, so a leading env
			// assignment applies to the whole entry. Default to module mode:
			// the sandbox tests exactly one module and its go.work is not a
			// supported surface here.
			command = "GOWORK=off " + command
		}
		allowed = append(allowed, command)
	}
	return allowed, skipped
}

// Run satisfies Worker.
func (w *DevboxWorker) Run(ctx context.Context, jc JobContext) (StageOutput, error) {
	if w.Client == nil {
		return StageOutput{}, fmt.Errorf("devbox worker: client not configured")
	}
	testCommands, skippedTests := declaredDevboxTests(jc.Item)
	resp, err := w.Client.QualityGate(ctx, DevboxRequest{
		// Per-item cross-repo routing: the devbox sandbox clones the target
		// repo fresh (git-clone mode), so honoring the item's TargetProject is
		// enough to run the tests stage against another repo. Empty target =
		// the worker's home repo (unchanged).
		Project:      effectiveProject(jc.Item, w.Project),
		AgentID:      w.AgentID,
		Env:          jc.Env,
		Checks:       devboxScopeFor(jc.Item),
		TestCommands: testCommands,
	})
	if err != nil {
		return StageOutput{}, err
	}
	if !resp.Passed {
		artifacts := map[string]any{
			"checks":   resp.Checks,
			"language": resp.Language,
		}
		if len(skippedTests) > 0 {
			artifacts[skippedDeclaredTestsArtifactKey] = skippedTests
		}
		out := StageOutput{
			CostUSD:   resp.CostUSD,
			LogTail:   resp.LogTail,
			Artifacts: artifacts,
		}
		if len(resp.Checks) == 0 {
			// Not-passed with ZERO executed checks is an infrastructure contract
			// violation, not a test failure: the gate never produced a verdict
			// (recycled/evicted sandbox, backend blip). Wrap ErrDevboxGateNoChecks
			// so Classify tags it ClassTransient (free retry) instead of burning
			// the implement budget as a phantom code failure, and carry the gate
			// JSON tail so the escalation, if the transient persists, is
			// actionable (live 2026-07-16: "0/0 checks marked failed; gate
			// reported not passed" ×4).
			return out, fmt.Errorf("devbox quality gate reported not-passed with no executed checks; gate tail: %q: %w", strings.TrimSpace(resp.LogTail), ErrDevboxGateNoChecks)
		}
		// Treat a quality-gate fail as an error so the runner can retry
		// implement; the gate-fail/escalate path picks it up by attempt count.
		//
		// The error must name the FAILING checks and carry their output tails:
		// the old shape ("devbox quality gate failed: 1 checks") counted the
		// TOTAL checks and discarded the failure text, so Classify could never
		// see infra needles like `container not found ("devbox")` — a recycled
		// sandbox pod burned all attempts as class=code in ~4s and the
		// escalation gave the operator nothing to act on (escalation #289,
		// 2026-07-08).
		return out, fmt.Errorf("devbox quality gate failed (%s)", summarizeFailedChecks(resp.Checks))
	}
	artifacts := map[string]any{
		"checks":   resp.Checks,
		"language": resp.Language,
		"passed":   true,
	}
	if len(skippedTests) > 0 {
		artifacts[skippedDeclaredTestsArtifactKey] = skippedTests
	}
	return StageOutput{
		CostUSD:   resp.CostUSD,
		LogTail:   resp.LogTail,
		Artifacts: artifacts,
	}, nil
}

// summarizeFailedChecks renders the failing quality-gate checks as
// "N/M checks failed: name[exit=K]: <output tail>; ...". The output tail is
// kept (last bytes, where compilers/test runners print the actual error) and
// capped per check so the stage error stays readable in escalation issues
// while still carrying enough text for Classify's needle matching.
func summarizeFailedChecks(checks []DevboxCheck) string {
	const maxTail = 240
	var failed []string
	for _, c := range checks {
		if c.Passed {
			continue
		}
		detail := fmt.Sprintf("%s[exit=%d]", c.Name, c.ExitCode)
		if tail := strings.TrimSpace(c.Output); tail != "" {
			if len(tail) > maxTail {
				tail = "…" + tail[len(tail)-maxTail:]
			}
			detail += ": " + strings.ReplaceAll(tail, "\n", " | ")
		}
		failed = append(failed, detail)
	}
	if len(failed) == 0 {
		// Passed=false but no per-check failure recorded — keep a stable
		// fallback rather than an empty summary.
		return fmt.Sprintf("0/%d checks marked failed; gate reported not passed", len(checks))
	}
	return fmt.Sprintf("%d/%d checks failed: %s", len(failed), len(checks), strings.Join(failed, "; "))
}

// GitLabClient is the merge-request lifecycle facade for the mr / ci_watch
// / merge / cleanup stages.
type GitLabClient interface {
	CreateMR(ctx context.Context, req CreateMRRequest) (CreateMRResponse, error)
	PollPipeline(ctx context.Context, req PollPipelineRequest) (PollPipelineResponse, error)
	Merge(ctx context.Context, req MergeRequestArgs) (MergeResponse, error)
	Cleanup(ctx context.Context, req CleanupRequest) (CleanupResponse, error)
}

type GitLabJobRetrier interface {
	RetryJob(context.Context, int64) error
}

// GitLabPollDeadlineProvider exposes the client's configured per-session
// polling budget so a terminal-job rescue can reuse the original absolute
// deadline instead of silently starting a second full polling window.
type GitLabPollDeadlineProvider interface {
	PipelinePollDeadline() time.Duration
}

// FailedJob is the current (non-retried) failed GitLab job observed on a
// terminal pipeline.
type FailedJob struct {
	ID            int64
	Name          string
	FailureReason string
}

// CreateMRRequest is the bundle a `mr` stage ships.
//
// AutoMerge carries the resolved per-item policy intent through the worker
// contract. The GitLab client intentionally does not arm
// merge_when_pipeline_succeeds at MR creation because that creates a detached
// failed head pipeline in this repository; ci_watch followed by the explicit
// merge stage remains the only merge path. See GitLabWorker.AutoMergeFor.
type CreateMRRequest struct {
	BacklogID    string
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
	AutoMerge    bool
	Env          map[string]string
}

// CreateMRResponse carries the new MR iid back.
type CreateMRResponse struct {
	MRIID        int64
	URL          string
	Project      string
	SourceBranch string
	TargetBranch string
	CostUSD      float64
	// Adopted is true when CreateMR did not open a new MR but instead adopted
	// an existing open MR for the source branch (GitLab returned 409 "Another
	// open merge request already exists"). The mr stage records this so the
	// run continues on the existing MR rather than escalating.
	Adopted bool
}

// PollPipelineRequest asks the GitLab CI integration to wait on the MR's
// pipeline. Workers don't loop themselves; the client is expected to
// block until terminal state and return.
type PollPipelineRequest struct {
	MRIID        int64
	Project      string
	SourceBranch string
	TargetBranch string
	Env          map[string]string
}

// PollPipelineResponse reports the terminal CI verdict.
type PollPipelineResponse struct {
	Status string // "success" | "failed" | "canceled" | "timeout"
	// Project, SourceBranch, TargetBranch, and SHA form the durable CI
	// authorization. The merge stage persists and reuses the exact tuple so a
	// reroute, source change, retarget, or later branch push cannot bypass the
	// revision and destination that passed CI.
	Project      string
	SourceBranch string
	TargetBranch string
	SHA          string
	CostUSD      float64
	LogTail      string
	// PipelineURL is the web_url of the most recent branch pipeline observed for
	// the MR head SHA ("" when none appeared yet). Populated on a poll-session
	// timeout so the ci_watch stage can extend the watch and, at the hard cap,
	// key the external-dependency stall on the stuck pipeline. (S3)
	PipelineURL string
	// LastStatus is the last NON-terminal pipeline status observed when a poll
	// session times out ("running"|"pending"|"created"|""). Empty on a terminal
	// return. Used only for richer ci_watch extension logging. (S3)
	LastStatus string
	// FailedJobReasons contains failure_reason for every current failed job in a
	// terminal non-success pipeline. Empty means the client could not establish
	// a complete set, so callers must retain the conservative code verdict.
	FailedJobReasons []string
	FailedJobs       []FailedJob
}

// MergeRequestArgs collects the inputs for the merge call.
type MergeRequestArgs struct {
	MRIID                           int64
	Project                         string
	SourceBranch                    string
	TargetBranch                    string
	ExpectedSHA                     string
	RecoveryPipelineCreateAttempted bool
	Env                             map[string]string
}

// MergeResponse returns the merge sha.
type MergeResponse struct {
	MergedSHA string
	CostUSD   float64
}

// CleanupRequest tells the GitLab/git layer to release the worktree +
// branch tied to the run.
type CleanupRequest struct {
	WorktreePath string
	BranchName   string
	MRIID        int64
	Project      string
	TargetBranch string
	Env          map[string]string
}

// CleanupResponse reports outcome.
type CleanupResponse struct {
	CostUSD float64
	LogTail string
}

// BranchPusher publishes a local branch to origin so the subsequent
// CreateMR call has something to point at. Production wires a
// CommandRunner-backed implementation that shells `git push`; tests
// inject a fake.
//
// Why this exists: pre-2026-05-25 Mills had a structural gap — the
// spawn agent committed to the NFS-shared worktree, the operator
// then created the GitLab MR pointing at that branch, but no one
// ever pushed. GitLab accepted the MR row anyway with no head_sha,
// and ci_watch hung forever waiting on a pipeline that couldn't
// exist. This interface makes the push explicit so future stages
// see a real branch on origin.
type BranchPusher interface {
	// Push pushes HEAD of workingDir's git checkout to origin under
	// the given branch name. Implementations should be idempotent —
	// pushing an already-up-to-date ref must succeed, not error.
	Push(ctx context.Context, workingDir, branch string) error
}

// GitLabWorker dispatches mr / ci_watch / merge / cleanup. The same
// worker handles all four stages; it dispatches internally on jc.Stage.ID.
type GitLabWorker struct {
	Client GitLabClient
	// MRTitle / MRDescription return the strings the worker should send
	// to CreateMR. The operator wires these to draw from the spec doc;
	// tests can return constants.
	MRTitle       func(jc JobContext) string
	MRDescription func(jc JobContext) string
	// SourceBranch / TargetBranch return the refs to use. SourceBranch
	// falls back to BranchContractFor; TargetBranch falls back to "main".
	SourceBranch func(jc JobContext) string
	TargetBranch func(jc JobContext) string
	// AutoMergeFor resolves this item's auto-merge policy intent. Operator main
	// wires it to combine item.Policy.AutoMerge with policy.LabelOverrideFor;
	// nil falls back to jc.Item.Policy.AutoMerge. The GitLab client records the
	// intent in the request contract but deliberately leaves create-time MWPS
	// disabled; the explicit merge stage performs the merge after ci_watch.
	AutoMergeFor func(jc JobContext) bool
	// BranchPusher, when wired, pushes the source branch to origin
	// before runMR calls CreateMR. Nil → skip push (legacy behaviour:
	// assume some other path publishes the branch). Operator main
	// wires a CommandRunner-backed clients.GitBranchPusher.
	BranchPusher BranchPusher
	// PlanMRRecorder, when wired, records the created MR onto the plan
	// slice(s) a plan-linked item came from so the take-up reconciler can
	// true them to merged. Nil → skip (pre-fix behaviour: the plan's mr_ref
	// stays empty unless the spawned agent recorded it itself, which is the
	// gap that stalled plan-stamp-loom-runbook-loom-runbook on 2026-08-01).
	// The write is best-effort and never fails the stage.
	PlanMRRecorder PlanMRRecorder
	// Logger receives the worker's advisory (non-stage-failing) diagnostics.
	// Nil defaults to slog.Default().
	Logger *slog.Logger
	// ForProject returns a GitLabClient scoped to the given repo project path,
	// for per-item cross-repo routing (an item's TargetProject). Nil, an empty
	// project, or a nil return falls back to Client (the home repo). The
	// operator wires this to clients.GitLabClient.ForProject; the returned
	// client must carry a token authorized on the target repo (the services
	// group token — a deployment concern gated with cross_repo.enabled).
	ForProject func(project string) GitLabClient
	// FlakyJobs resolves the policy-listed jobs eligible for one bounded retry.
	// Nil/empty disables rescue; the operator supplies the production defaults.
	FlakyJobs func() []string
	// MergeQueue, when wired together with a true MergeQueueEnabled, reroutes
	// the merge stage through the serial merge queue: enqueue the CI-authorized
	// candidate, wait for the processor's verdict. Nil → direct merge
	// (pre-queue behaviour).
	MergeQueue MergeQueue
	// MergeQueueEnabled is the hot-reloaded policy fence (merge_queue.enabled),
	// consulted at stage entry and on every wait poll so a policy flip takes
	// effect without an operator restart.
	MergeQueueEnabled func() bool
	// MergeQueuePollInterval overrides the wait poll cadence (tests).
	MergeQueuePollInterval time.Duration
}

// clientFor returns the GitLab client this item's stages should use: a
// per-item client scoped to item.TargetProject when ForProject is wired and
// the item opts into cross-repo, else the home Client. Backward compatible —
// an item with no TargetProject always gets Client.
func (w *GitLabWorker) clientFor(item *store.BacklogItem) GitLabClient {
	if w.ForProject != nil && item != nil {
		if tp := strings.TrimSpace(item.TargetProject); tp != "" {
			if c := w.ForProject(tp); c != nil {
				return c
			}
		}
	}
	return w.Client
}

// clientForProject routes a resumed stage from its persisted authorization,
// never from the backlog item's mutable TargetProject. A nil/unknown factory
// result falls back to Client; the concrete GitLab client then independently
// rejects a project mismatch before making a request.
func (w *GitLabWorker) clientForProject(project string) GitLabClient {
	if w.ForProject != nil {
		if project = strings.TrimSpace(project); project != "" {
			if c := w.ForProject(project); c != nil {
				return c
			}
		}
	}
	return w.Client
}

// Run satisfies Worker.
func (w *GitLabWorker) Run(ctx context.Context, jc JobContext) (StageOutput, error) {
	if w.Client == nil {
		return StageOutput{}, fmt.Errorf("gitlab worker: client not configured")
	}
	switch jc.Stage.ID {
	case "mr":
		return w.runMR(ctx, jc)
	case "ci_watch":
		return w.runCI(ctx, jc)
	case "merge":
		return w.runMerge(ctx, jc)
	case "cleanup":
		return w.runCleanup(ctx, jc)
	default:
		return StageOutput{}, fmt.Errorf("gitlab worker: unsupported stage %q", jc.Stage.ID)
	}
}

// gitlabMRTitleMaxChars is GitLab's hard cap on merge_requests.title —
// anything longer 400s with `{"title":["is too long (maximum is 255
// characters)"]}` (observed live 2026-08-01 on
// PIPE-hud-mill-efficiency-strip-1, whose council-minted backlog title
// exceeded the cap and errored the mr stage).
const gitlabMRTitleMaxChars = 255

// ClampMRTitle bounds an MR title to GitLab's title cap, rune-safe, with a
// trailing ellipsis marking the cut. Backlog titles are free text minted by
// councils and dynamic workflows; nothing upstream bounds them.
func ClampMRTitle(title string) string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) <= gitlabMRTitleMaxChars {
		return string(runes)
	}
	return string(runes[:gitlabMRTitleMaxChars-1]) + "…"
}

func (w *GitLabWorker) runMR(ctx context.Context, jc JobContext) (StageOutput, error) {
	sourceBranch := w.sourceBranch(jc)
	if sourceBranch == "" {
		return StageOutput{}, fmt.Errorf("mr: source branch unavailable for backlog %q", jc.Item.ID)
	}
	// Push the spawn agent's commits to origin before opening the MR.
	// Without this, GitLab accepts the MR row but it has no head_sha
	// (canary !518 surfaced this on 2026-05-25). Push is best-effort
	// when worktree path is missing — the legacy code path assumed a
	// pre-pushed branch.
	client := w.clientFor(jc.Item)
	if w.BranchPusher != nil && jc.Run != nil && jc.Run.WorktreePath != "" {
		if err := w.BranchPusher.Push(ctx, jc.Run.WorktreePath, sourceBranch); err != nil {
			return StageOutput{}, fmt.Errorf("mr: push %q to origin: %w", sourceBranch, err)
		}
	} else if jc.Run == nil || jc.Run.WorktreePath == "" {
		if lookup, ok := client.(interface {
			GetBranch(context.Context, string) (string, bool, error)
		}); ok {
			if _, exists, err := lookup.GetBranch(ctx, sourceBranch); err == nil && !exists {
				return StageOutput{}, fmt.Errorf("mr: source branch %q does not exist on origin; the implement spawn never pushed it", sourceBranch)
			}
		}
	}
	req := CreateMRRequest{
		BacklogID:    jc.Item.ID,
		SourceBranch: sourceBranch,
		TargetBranch: callOr(w.TargetBranch, jc, "main"),
		Title:        ClampMRTitle(callOr(w.MRTitle, jc, jc.Item.Title)),
		Description:  callOr(w.MRDescription, jc, ""),
		AutoMerge:    w.computeAutoMerge(jc),
		Env:          jc.Env,
	}
	resp, err := client.CreateMR(ctx, req)
	if err != nil {
		return StageOutput{}, err
	}
	// The MR now exists. Record it on the plan before returning so the
	// take-up reconciler has an mr_ref to poll — best-effort, never fatal.
	w.recordPlanMRRef(ctx, jc, resp.MRIID)
	logTail := ""
	if resp.Adopted {
		logTail = fmt.Sprintf("mr: adopted existing !%d", resp.MRIID)
	}
	return StageOutput{
		CostUSD: resp.CostUSD,
		MRIID:   resp.MRIID,
		LogTail: logTail,
		Artifacts: map[string]any{
			"mr_url":           resp.URL,
			"mr_iid":           resp.MRIID,
			"mr_project":       resp.Project,
			"mr_source_branch": resp.SourceBranch,
			"mr_target_branch": resp.TargetBranch,
			"branch":           req.SourceBranch,
			"created":          !resp.Adopted,
			"adopted":          resp.Adopted,
		},
	}, nil
}

func (w *GitLabWorker) runCI(ctx context.Context, jc JobContext) (StageOutput, error) {
	mrIID := mrIIDFrom(jc)
	if mrIID == 0 {
		return StageOutput{}, fmt.Errorf("ci_watch: no mr_iid in run")
	}
	pollReq, err := mrPollRequestFrom(jc, mrIID)
	if err != nil {
		return StageOutput{}, err
	}
	client := w.clientForProject(pollReq.Project)
	maxExtensions := ciWatchExtensionBudget(jc.Env)
	pollCtx := ctx
	cancelPoll := func() {}
	if provider, ok := client.(GitLabPollDeadlineProvider); ok && provider.PipelinePollDeadline() > 0 {
		pollCtx, cancelPoll = context.WithTimeout(ctx, provider.PipelinePollDeadline())
	}
	defer cancelPoll()

	var logTail strings.Builder
	var cost float64
	lastPipelineURL := ""
	lastStatus := ""
	rescued := ciWatchFlakeRescueAttemptedFromContext(ctx)
	firstFailure := ciWatchFlakeRescueFirstJobsFromContext(ctx)

	// The GitLab client enforces a per-poll-session deadline (default 30m). A
	// slow-but-healthy CI run must not kill the autonomous run, so when a poll
	// session times out with the pipeline still non-terminal we keep watching in
	// bounded extensions up to the MILLS_CI_WATCH_MAX_MINUTES hard cap. Only at
	// the cap do we escalate — as a retryable external-dependency stall, not a
	// code failure (see CIWatchStalledError). (S3)
	for extension := 0; ; extension++ {
		resp, err := client.PollPipeline(pollCtx, pollReq)
		cost += resp.CostUSD
		appendCIWatchLog(&logTail, resp.LogTail)
		if resp.PipelineURL != "" {
			lastPipelineURL = resp.PipelineURL
		}
		if resp.LastStatus != "" {
			lastStatus = resp.LastStatus
		}

		if err == nil {
			if err := validateCITerminalIdentity(resp, pollReq); err != nil {
				return StageOutput{CostUSD: cost, LogTail: logTail.String()}, err
			}
			artifacts := map[string]any{
				"ci_status":        resp.Status,
				"ci_project":       resp.Project,
				"ci_source_branch": resp.SourceBranch,
				"ci_target_branch": resp.TargetBranch,
				"ci_sha":           resp.SHA,
				// The head-movement fence travels WITH the authorization it
				// belongs to. merge re-reads it and refuses to run when the
				// ledger has advanced since (#374).
				ciTransitionSeqArtifact: jc.HeadTransitionSeq,
			}
			out := StageOutput{
				CostUSD:   cost,
				LogTail:   logTail.String(),
				Artifacts: artifacts,
			}
			if resp.Status != "success" {
				if !rescued && flakyRetryEligible(resp.FailedJobs, callStrings(w.FlakyJobs)) {
					if err := recordCIWatchFlakeRescue(ctx, resp.FailedJobs); err != nil {
						return out, fmt.Errorf("persist ci_watch flake rescue fence: %w", err)
					}
					rescued = true
					firstFailure = append([]FailedJob(nil), resp.FailedJobs...)
					retrier, ok := client.(GitLabJobRetrier)
					if !ok {
						return out, fmt.Errorf("gitlab client does not support job retry")
					}
					for _, job := range resp.FailedJobs {
						if err := retrier.RetryJob(ctx, job.ID); err != nil {
							return out, fmt.Errorf("retry flaky CI job %q (%d): %w", job.Name, job.ID, err)
						}
					}
					fmt.Fprintf(&logTail, "ci_watch: %s failed, auto-retried once\n", failedJobNames(firstFailure))
					continue
				}
				if rescued && len(firstFailure) > 0 {
					fmt.Fprintf(&logTail, "ci_watch: %s failed, auto-retried once, failed again (%s)\n", failedJobNames(firstFailure), failedJobNames(resp.FailedJobs))
				}
				return out, &CIPipelineTerminalError{
					Status: resp.Status, MRIID: mrIID, FailedJobReasons: resp.FailedJobReasons,
					FailedJobs: resp.FailedJobs, FirstFailedJobs: firstFailure, AutoRetried: rescued,
				}
			}
			return out, nil
		}

		// A non-timeout error (network blip, context cancel, GitLab 5xx) is not a
		// stall — surface it unchanged so the runner classifies/retries it.
		if !errors.Is(err, ErrPipelinePollTimeout) {
			return StageOutput{CostUSD: cost, LogTail: logTail.String()}, err
		}

		// Poll session timed out. PollPipeline only returns a nil error on a
		// terminal state, so the pipeline is still non-terminal (pending/running,
		// or not yet visible). Extend the watch while the budget allows.
		if extension < maxExtensions {
			// Watch extensions intentionally receive a fresh session budget. The
			// shared deadline above exists only to keep an immediate flake rescue
			// inside the first session's original budget.
			cancelPoll()
			pollCtx = ctx
			status := lastStatus
			if status == "" {
				status = "running"
			}
			fmt.Fprintf(&logTail, "ci_watch: pipeline still %s after %dm, extending watch (%d/%d)%s\n",
				status, ciWatchPollSessionMinutes, extension+1, maxExtensions, ciWatchPipelineNote(lastPipelineURL))
			continue
		}

		// Hard cap reached with the pipeline still running: an external CI stall,
		// not a code bug. Escalate once as a retryable external-dependency
		// incident keyed on the stuck pipeline URL (handled in the runner).
		return StageOutput{CostUSD: cost, LogTail: logTail.String()}, &CIWatchStalledError{
			PipelineURL: lastPipelineURL,
			MaxMinutes:  ciWatchMaxMinutes(jc.Env),
			MRIID:       mrIID,
			LastStatus:  lastStatus,
		}
	}
}

func callStrings(fn func() []string) []string {
	if fn == nil {
		return nil
	}
	return fn()
}

func flakyRetryEligible(failed []FailedJob, eligible []string) bool {
	if len(failed) == 0 || len(eligible) == 0 {
		return false
	}
	allowed := make(map[string]struct{}, len(eligible))
	for _, name := range eligible {
		allowed[strings.TrimSpace(name)] = struct{}{}
	}
	for _, job := range failed {
		if job.ID <= 0 || strings.TrimSpace(job.FailureReason) != "script_failure" {
			return false
		}
		if _, ok := allowed[strings.TrimSpace(job.Name)]; !ok {
			return false
		}
	}
	return true
}

func failedJobNames(jobs []FailedJob) string {
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, job.Name)
	}
	return strings.Join(names, ", ")
}

// appendCIWatchLog appends a poll session's log tail, ensuring a trailing
// newline so extension notes and successive sessions stay on their own lines.
func appendCIWatchLog(b *strings.Builder, tail string) {
	if tail == "" {
		return
	}
	b.WriteString(tail)
	if !strings.HasSuffix(tail, "\n") {
		b.WriteByte('\n')
	}
}

// ciWatchPipelineNote renders the " (pipeline: <url>)" suffix for extension log
// lines, or "" when no pipeline has been observed yet.
func ciWatchPipelineNote(url string) string {
	if url == "" {
		return ""
	}
	return " (pipeline: " + url + ")"
}

func (w *GitLabWorker) runMerge(ctx context.Context, jc JobContext) (StageOutput, error) {
	mrIID := mrIIDFrom(jc)
	if mrIID == 0 {
		return StageOutput{}, fmt.Errorf("merge: no mr_iid in run")
	}
	// Serial merge queue (policy merge_queue.enabled): hand the CI-authorized
	// candidate to the queue and wait for its verdict instead of merging
	// directly. See pipeline/mergequeue.go.
	if w.mergeQueueActive() {
		return w.runMergeViaQueue(ctx, jc, mrIID)
	}
	return w.runMergeDirect(ctx, jc, mrIID)
}

// runMergeDirect is the pre-queue merge body: SHA-preconditioned merge of the
// exact ci_watch authorization, with the client's bounded recovery machinery.
func (w *GitLabWorker) runMergeDirect(ctx context.Context, jc JobContext, mrIID int64) (StageOutput, error) {
	mergeReq, err := ciMergeRequestFrom(jc, mrIID)
	if err != nil {
		return StageOutput{}, err
	}
	mergeReq.RecoveryPipelineCreateAttempted = jc.MergeRecoveryPipelineCreateAttempted
	resp, err := w.clientForProject(mergeReq.Project).Merge(ctx, mergeReq)
	if err != nil {
		return StageOutput{}, err
	}
	return StageOutput{
		CostUSD:   resp.CostUSD,
		MergedSHA: resp.MergedSHA,
		Artifacts: map[string]any{
			"merged_sha":     resp.MergedSHA,
			"merged_project": mergeReq.Project,
		},
	}, nil
}

func (w *GitLabWorker) runCleanup(ctx context.Context, jc JobContext) (StageOutput, error) {
	mrIID := mrIIDFrom(jc)
	if mrIID == 0 {
		return StageOutput{}, fmt.Errorf("cleanup: no mr_iid in run")
	}
	mrReq, err := mrPollRequestFrom(jc, mrIID)
	if err != nil {
		// Cleanup runs only after a successful merge. Legacy rows may predate MR
		// provenance; deleting no branch is safe and lets the already-merged run
		// complete without guessing a mutable project or branch.
		return StageOutput{LogTail: fmt.Sprintf("cleanup: skipped branch deletion without durable mr provenance: %v", err)}, nil
	}
	resp, err := w.clientForProject(mrReq.Project).Cleanup(ctx, CleanupRequest{
		WorktreePath: jc.Run.WorktreePath,
		BranchName:   mrReq.SourceBranch,
		MRIID:        mrIID,
		Project:      mrReq.Project,
		TargetBranch: mrReq.TargetBranch,
		Env:          jc.Env,
	})
	if err != nil {
		return StageOutput{}, err
	}
	return StageOutput{
		CostUSD: resp.CostUSD,
		LogTail: resp.LogTail,
		Artifacts: map[string]any{
			"cleanup_project": mrReq.Project,
		},
	}, nil
}

// computeAutoMerge resolves the policy intent carried on CreateMRRequest.
// GitLab creation deliberately does not map it to MWPS; the explicit merge
// stage remains authoritative. Precedence:
//  1. If AutoMergeFor callback is wired, it wins (operator main routes
//     policy.LabelOverrideFor + item.Policy.AutoMerge through here).
//  2. Else fall back to the item's own ItemPolicy.AutoMerge.
//
// Disabled by default so an operator without explicit opt-in keeps the same
// Mills-driven merge policy.
func (w *GitLabWorker) computeAutoMerge(jc JobContext) bool {
	if w.AutoMergeFor != nil {
		return w.AutoMergeFor(jc)
	}
	if jc.Item == nil {
		return false
	}
	return jc.Item.Policy.AutoMerge
}

// logger returns the worker's logger, defaulting to the process logger.
func (w *GitLabWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

func (w *GitLabWorker) sourceBranch(jc JobContext) string {
	// SourceBranch is retained on GitLabWorker for source compatibility, but is
	// intentionally not consulted. A callback can depend on attempt or
	// escalation state and would violate BranchContractFor's retry stability.
	return BranchContractFor(jc.Run, jc.Item, jc.Stage, "").SourceBranch
}

// mrIIDFrom pulls the MRIID off the run row, falling back to the
// `mr_iid` artifact recorded by the mr stage.
func mrIIDFrom(jc JobContext) int64 {
	if jc.Run.MRIID != nil && *jc.Run.MRIID != 0 {
		return *jc.Run.MRIID
	}
	if mr, ok := jc.Prior["mr"]; ok && mr.MRIID != 0 {
		return mr.MRIID
	}
	return 0
}

// mrPollRequestFrom restores the MR-stage routing and ref provenance. CI must
// not follow a backlog item that was rerouted after MR creation, even when the
// new project happens to contain the same MR IID.
func mrPollRequestFrom(jc JobContext, mrIID int64) (PollPipelineRequest, error) {
	mr, ok := jc.Prior["mr"]
	if !ok || mr.Artifacts == nil || mr.MRIID == 0 {
		return PollPipelineRequest{}, fmt.Errorf("ci_watch: missing durable mr provenance for mr %d: %w", mrIID, ErrMergeAuthorizationStale)
	}
	if mr.MRIID != mrIID {
		return PollPipelineRequest{}, fmt.Errorf("ci_watch: persisted mr iid %d does not match run mr iid %d: %w", mr.MRIID, mrIID, ErrMergeAuthorizationStale)
	}
	read := func(key string) string {
		value, _ := mr.Artifacts[key].(string)
		return strings.TrimSpace(value)
	}
	req := PollPipelineRequest{
		MRIID:        mrIID,
		Project:      read("mr_project"),
		SourceBranch: read("mr_source_branch"),
		TargetBranch: read("mr_target_branch"),
		Env:          jc.Env,
	}
	for _, field := range []struct {
		key   string
		value string
	}{
		{"mr_project", req.Project},
		{"mr_source_branch", req.SourceBranch},
		{"mr_target_branch", req.TargetBranch},
	} {
		if field.value == "" {
			return PollPipelineRequest{}, fmt.Errorf("ci_watch: no %s from mr stage for mr %d: %w", field.key, mrIID, ErrMergeAuthorizationStale)
		}
	}
	return req, nil
}

func validateCITerminalIdentity(resp PollPipelineResponse, req PollPipelineRequest) error {
	if strings.TrimSpace(resp.SHA) == "" {
		return fmt.Errorf("ci_watch: terminal pipeline returned no sha for mr %d: %w", req.MRIID, ErrMergeAuthorizationStale)
	}
	for _, identity := range []struct {
		name string
		got  string
		want string
	}{
		{"project", resp.Project, req.Project},
		{"source branch", resp.SourceBranch, req.SourceBranch},
		{"target branch", resp.TargetBranch, req.TargetBranch},
	} {
		if identity.got != identity.want {
			return fmt.Errorf("ci_watch: terminal pipeline %s %q does not match mr-stage authorization %q for mr %d: %w", identity.name, identity.got, identity.want, req.MRIID, ErrMergeAuthorizationStale)
		}
	}
	return nil
}

// ciTransitionSeqArtifact is the ci_watch stage artifact holding the run's max
// settled MR head-MOVEMENT seq at the moment CI authorized its SHA. Free-form
// artifacts_json means no schema change; absent (legacy rows) reads as 0.
const ciTransitionSeqArtifact = "ci_transition_seq"

// artifactInt64 reads a numeric artifact tolerantly. In-memory a stage output
// carries a real int64; after an operator restart the same value arrives back
// through artifacts_json as a float64. Both must compare equal, or a restart
// would fail-close every merge.
func artifactInt64(artifacts map[string]any, key string) int64 {
	if artifacts == nil {
		return 0
	}
	switch v := artifacts[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// ciMergeRequestFrom rebuilds the exact authorization tuple persisted by
// ci_watch. Legacy or incomplete rows fail closed before a GitLab client is
// selected or called.
func ciMergeRequestFrom(jc JobContext, mrIID int64) (MergeRequestArgs, error) {
	ci, ok := jc.Prior["ci_watch"]
	if !ok || ci.Artifacts == nil {
		return MergeRequestArgs{}, fmt.Errorf("merge: no ci_sha from successful ci_watch; refusing unbound merge for mr %d: %w", mrIID, ErrMergeAuthorizationStale)
	}
	// The head-movement fence (#374). A settled non-noop transition in the
	// mr_head_transitions ledger advances jc.HeadTransitionSeq past whatever
	// ci_watch stamped, which means the head this authorization was issued
	// for is no longer the head GitLab holds. Retrying the same merge cannot
	// make that authorization valid — a full re-gate + fresh CI must run for
	// the successor SHA, so this fails closed exactly like a missing ci_sha.
	//
	// Legacy rows carry no stamp: they read 0 and compare against a run with
	// no ledger rows, which is also 0, so they keep merging unchanged. No
	// backfill is required.
	if stamped := artifactInt64(ci.Artifacts, ciTransitionSeqArtifact); stamped != jc.HeadTransitionSeq {
		return MergeRequestArgs{}, fmt.Errorf(
			"merge: ci authorization was issued at head transition %d but the run has settled %d; the mr head moved after ci went green for mr %d: %w",
			stamped, jc.HeadTransitionSeq, mrIID, ErrMergeAuthorizationStale)
	}
	read := func(key string) string {
		value, _ := ci.Artifacts[key].(string)
		return strings.TrimSpace(value)
	}
	req := MergeRequestArgs{
		MRIID:        mrIID,
		Project:      read("ci_project"),
		SourceBranch: read("ci_source_branch"),
		TargetBranch: read("ci_target_branch"),
		ExpectedSHA:  read("ci_sha"),
		Env:          jc.Env,
	}
	for _, field := range []struct {
		key   string
		value string
	}{
		{"ci_sha", req.ExpectedSHA},
		{"ci_project", req.Project},
		{"ci_source_branch", req.SourceBranch},
		{"ci_target_branch", req.TargetBranch},
	} {
		if field.value == "" {
			return MergeRequestArgs{}, fmt.Errorf("merge: no %s from successful ci_watch; refusing incomplete authorization for mr %d: %w", field.key, mrIID, ErrMergeAuthorizationStale)
		}
	}
	return req, nil
}

// callOr returns fn(jc) if fn is non-nil and returns a non-empty string;
// otherwise fallback.
func callOr(fn func(JobContext) string, jc JobContext, fallback string) string {
	if fn == nil {
		return fallback
	}
	v := fn(jc)
	if v == "" {
		return fallback
	}
	return v
}

// DefaultRoutes constructs a stage→worker map with the standard
// wiring: spawn workers for plan_slice/implement/pr_self_review, weaver
// for research, devbox for tests, gitlab for mr/ci_watch/merge/cleanup.
//
// Callers supply concrete clients; nil clients produce errors at run
// time, surfaced in stage_results.outcome=error so the failure is
// auditable rather than silent.
//
// substrateFor selects the devbox backend per spawn-driven stage; a nil
// value is permitted and yields SpawnRequest.Substrate="" (the spawn
// service's default backend). Production wires
// PolicyManager.Current().SubstrateForStage here so hot-reloaded policy
// changes take effect on the next stage attempt.
//
// routeFor selects the spawn agent + vendor-native LLM model per spawn-driven
// stage AND per backlog item; a nil value preserves the spawn worker's static
// Model and leaves SpawnRequest.AgentModel empty so the spawn service keeps its
// vendor default. Production wires the operator's spawnRouteFor closure (env
// break-glass > agent/* label > agent_routing rule > stage_agents > default) so
// hot-reloaded routing takes effect on the next stage attempt.
func DefaultRoutes(spawn SpawnClient, weaver WeaverClient, devbox DevboxClient, gitlab GitLabClient, project, agentID string, promptFor func(string) func(JobContext) string, substrateFor func(stage string) string, routeFor func(ctx context.Context, stage string, item *store.BacklogItem) mills.AgentDecision) map[string]Worker {
	if promptFor == nil {
		promptFor = func(string) func(JobContext) string { return nil }
	}
	gw := &GitLabWorker{Client: gitlab}
	return map[string]Worker{
		"plan_slice":     &SpawnWorker{Client: spawn, PromptFor: promptFor("plan_slice"), SubstrateFor: substrateFor, RouteFor: routeFor},
		"research":       &WeaverWorker{Client: weaver, PromptFor: promptFor("research")},
		"implement":      &SpawnWorker{Client: spawn, PromptFor: promptFor("implement"), NeedsWorktree: true, SubstrateFor: substrateFor, RouteFor: routeFor},
		"tests":          &DevboxWorker{Client: devbox, Project: project, AgentID: agentID},
		"pr_self_review": &SpawnWorker{Client: spawn, PromptFor: promptFor("pr_self_review"), SubstrateFor: substrateFor, RouteFor: routeFor},
		"mr":             gw,
		"ci_watch":       gw,
		"merge":          gw,
		"cleanup":        gw,
	}
}

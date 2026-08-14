package workflow

// runtime.go is the Layer-3 imperative workflow runtime (Mills dynamic-
// workflows, plan .loom/134 §S6-min). It is the thin glue that turns the pure
// S0 interpreter + DAO journal into something that drives REAL side-effects: an
// agent() call dispatches a subordinate spawn via a worker.WorkerRunner, a
// gate() call evaluates a (currently trivial) pass/fail, and every effect is
// memoized in the durable step journal so a crash-and-resume re-attaches
// instead of re-dispatching.
//
// S6-min scope (default-OFF behind policy.workflows.enabled — see scheduler_min.go):
//   - portable canary script: agent('implement') → gate('trivial') → done.
//     The selected claude-code/codex harness is immutable workflow metadata, so
//     replay re-derives the same script. It STOPS at the gate; there is NO
//     merge() step (merge idempotency is S6-full).
//   - agent() executor: runner.Run with a DETERMINISTIC idempotency key derived
//     from run.ID + step_key + call_hash, so a resume re-derives the same key
//     and the spawn controller's exactly-once dedupe (S2b/S2c) re-attaches.
//   - gate() executor: trivial pass evaluator.
//   - resume: a step already recorded 'pending' with a spawn_id re-attaches via
//     resumer.Resume(idempotencyKey) instead of re-dispatching.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/worker"
)

// canaryScript is the legacy/default Claude S6-min canary. Portable runs derive
// the same program with their persisted agent type substituted. It dispatches
// one implement agent, evaluates one trivial gate, then ends. It deliberately STOPS at the
// gate — no merge() — because merge idempotency is deferred to S6-full and a
// merging canary cannot be validated until then (.loom/134 §S6-min, §safety).
//
// The script body uses the S0 universe's effect builtins (agent/gate). Keyword
// args (model, budget_usd) are hashed into the call_hash, so they are part of
// the step's determinism fingerprint.
const canaryScript = `
agent('implement', model='claude-code', budget_usd=1.0)
gate('trivial')
`

const (
	// CanaryTemplateName and CanaryTemplateVersion are durable workflow
	// identity. The deployed kill-test validates both before fault injection.
	CanaryTemplateName          = "workflow-canary"
	CanaryTemplateVersion       = "v2"
	legacyCanaryTemplateVersion = "v1"
	// CanaryMergingTemplateVersion is the S6-full merging canary: the same
	// bounded crash choreography as v2, plus a single journaled merge()
	// effect after the gate. It exists to assert PASS-3 (no double-merge
	// across crashes) — the one property the pre-merge canary defers.
	// Merging runs are only ever created with explicit merging params;
	// version and params must agree or the runtime fails closed.
	CanaryMergingTemplateVersion = "v3"
	// CanaryHoldSeconds keeps the successful spawn lifecycle intentionally in
	// flight long enough for both bounded deployment restarts. The driver-owned,
	// side-effect-free hold may replay after CRASH B; S1c does not claim
	// turn-level exactly-once execution. It is exported so the deployed kill-test
	// proves the exact process rather than maintaining a second duration.
	//
	// 300s, raised from 90s after the 2026-07-28 in-cluster gate. The hold is
	// the whole time budget for the CRASH A -> CRASH B choreography: delete the
	// operator pod, wait for its replacement to pull/boot/become Ready, wait for
	// the loom-mills Kustomization that CRASH A knocked over to reconverge, then
	// re-prove the full identity fence before CRASH B may fire. At 90s that
	// budget was marginal — one run made it, the next reached "done" mid-fence
	// and aborted the gate. The hold is a side-effect-free sleep in a pod that
	// is torn down immediately afterwards, so buying a 5x margin costs only
	// pod-seconds and removes a race that has nothing to do with what S1c
	// measures.
	CanaryHoldSeconds = 300
)

// CanaryScript exposes the legacy/default Claude canary for tests and callers
// that predate portable agent selection.
func CanaryScript() string { return canaryScript }

type canaryWorkflowParams struct {
	AgentType string `json:"agent_type"`
	// Merging selects the S6-full merging canary (template version v3): the
	// script gains a trailing merge('canary') effect. Immutable workflow
	// identity, so replay derives the same program byte-for-byte.
	Merging bool `json:"merging,omitempty"`
}

// ResolveCanaryAgentType validates the portable S1c harness choice and returns
// its canonical token. Empty preserves the legacy behavior by selecting
// claude-code; every non-empty value must be one of the two explicitly audited
// canary harnesses. In particular, worker aliases and other supported worker
// types are not accepted implicitly on this destructive test surface.
func ResolveCanaryAgentType(agentType string) (string, error) {
	agentType = strings.TrimSpace(agentType)
	if agentType == "" {
		return worker.AgentTypeClaudeCode, nil
	}
	switch agentType {
	case worker.AgentTypeClaudeCode, worker.AgentTypeCodex:
		return agentType, nil
	default:
		return "", fmt.Errorf("workflow: unsupported canary agent_type %q (want claude-code or codex)", agentType)
	}
}

// CanaryAgentTypeFromRun restores the immutable canary harness choice from the
// durable workflow params. Runs created before portable canaries had NULL
// params and retain their historical claude-code behavior. Invalid persisted
// JSON or an unsupported value fails closed before the runtime executes an
// effect.
func CanaryAgentTypeFromRun(run *store.WorkflowRun) (string, error) {
	if run == nil {
		return "", errors.New("workflow: nil run")
	}
	if strings.TrimSpace(run.WorkflowParams) == "" {
		return worker.AgentTypeClaudeCode, nil
	}
	var params canaryWorkflowParams
	if err := json.Unmarshal([]byte(run.WorkflowParams), &params); err != nil {
		return "", fmt.Errorf("workflow: decode canary params for run %s: %w", run.ID, err)
	}
	if strings.TrimSpace(params.AgentType) == "" {
		return "", fmt.Errorf("workflow: invalid canary params for run %s: agent_type required", run.ID)
	}
	agentType, err := ResolveCanaryAgentType(params.AgentType)
	if err != nil {
		return "", fmt.Errorf("workflow: invalid canary params for run %s: %w", run.ID, err)
	}
	return agentType, nil
}

func canaryWorkflowParamsJSONWithOptions(agentType string, merging bool) (string, error) {
	agentType, err := ResolveCanaryAgentType(agentType)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(canaryWorkflowParams{AgentType: agentType, Merging: merging})
	if err != nil {
		return "", fmt.Errorf("workflow: encode canary params: %w", err)
	}
	return string(encoded), nil
}

// CanaryMergingFromRun restores the immutable merging selection and enforces
// that durable template version and params agree — a v3 run without merging
// params (or a v2 run with them) is identity tampering and fails closed
// before any effect executes.
func CanaryMergingFromRun(run *store.WorkflowRun) (bool, error) {
	if run == nil {
		return false, errors.New("workflow: nil run")
	}
	var params canaryWorkflowParams
	if strings.TrimSpace(run.WorkflowParams) != "" {
		if err := json.Unmarshal([]byte(run.WorkflowParams), &params); err != nil {
			return false, fmt.Errorf("workflow: decode canary params for run %s: %w", run.ID, err)
		}
	}
	versionMerging := run.Template == CanaryTemplateName && run.TemplateVersion == CanaryMergingTemplateVersion
	if params.Merging != versionMerging {
		return false, fmt.Errorf("workflow: run %s merging identity mismatch: params.merging=%t template_version=%q",
			run.ID, params.Merging, run.TemplateVersion)
	}
	return params.Merging, nil
}

// canaryScriptFromRun derives the exact program from immutable durable params.
// The returned script is therefore byte-stable across operator restarts, so
// step keys, call hashes, idempotency keys, and spawn identity all replay under
// the same agent harness selected at launch.
func canaryScriptFromRun(run *store.WorkflowRun) (string, error) {
	agentType, err := CanaryAgentTypeFromRun(run)
	if err != nil {
		return "", err
	}
	merging, err := CanaryMergingFromRun(run)
	if err != nil {
		return "", err
	}
	script := canaryScript
	if agentType != worker.AgentTypeClaudeCode {
		script = fmt.Sprintf("\nagent('implement', model='%s', budget_usd=1.0)\ngate('trivial')\n", agentType)
	}
	if merging {
		script += "merge('canary')\n"
	}
	return script, nil
}

// WorkflowInterpreter is the Layer-3 runtime: it executes one imperative
// workflow run against the durable DAO journal, wiring the S0 interpreter's
// effect primitives to real executors. One instance is shared across all runs
// (it is stateless per-run; the run id is carried into Run).
type WorkflowInterpreter struct {
	dao     *store.WorkflowDAO
	runner  worker.WorkerRunner
	resumer worker.WorkerResumer // optional; nil when the runner can't resume
	logger  *slog.Logger

	// spawnProject / spawnBaseBranch are the git-routing defaults every
	// agent() spawn inherits. Unlike the DAG pipeline's SpawnWorker, a
	// workflow script has no JobContext to derive Project/Branch from, yet
	// the HUD spawn API hard-requires Project+Branch (pkg/mills/clients/
	// spawn.go validation). Set via SetSpawnDefaults from operator config so
	// they match the DAG's SpawnWorker.Project; execAgent falls back to
	// "loom-core" when spawnProject is unset so a zero-value interpreter
	// (tests) still builds a valid SpawnRequest shape.
	spawnProject    string
	spawnBaseBranch string
	// merger executes the merging canary's single merge effect. Nil fails
	// closed: a merge() call without a configured executor errors instead of
	// silently passing, so a misconfigured operator cannot fake PASS-3.
	merger CanaryMergeExecutor
	// beforeTransitionCAS is a deterministic interleaving hook for the
	// lifecycle-fence regression test. Production leaves it nil.
	beforeTransitionCAS func()
	// registry resolves non-canary templates (S7). Defaults to the compiled-in
	// closed set; the canary keeps its own pre-S7 identity path.
	registry *Registry
	// backlogItemLookup, when set, loads (title, specDoc) for a run's backlog
	// item so registry-template spawns get a spec-aware work prompt instead of
	// the canary protocol. Best-effort: a lookup failure degrades to a generic
	// work prompt, never fails the spawn.
	backlogItemLookup func(ctx context.Context, id string) (title, specDoc string, err error)
}

// SetBacklogItemLookup wires the backlog reader the operator uses to derive
// spec-aware prompts for registry-template runs. Nil keeps generic prompts.
func (wi *WorkflowInterpreter) SetBacklogItemLookup(fn func(ctx context.Context, id string) (string, string, error)) {
	wi.backlogItemLookup = fn
}

// SetSpawnDefaults wires the git-routing context every agent() spawn needs.
// project is the repo spawns target (the HUD spawn API resolves it to a git
// remote + worktree base); baseBranch is what spawned worktrees branch off
// (empty defers to the spawn service default, "main"). The operator wires
// these from config so they match the DAG pipeline's SpawnWorker.Project.
// Safe to call on a nil-runner interpreter; it only mutates defaults.
func (wi *WorkflowInterpreter) SetSpawnDefaults(project, baseBranch string) {
	wi.spawnProject = project
	wi.spawnBaseBranch = baseBranch
}

// NewWorkflowInterpreter builds a runtime over a WorkflowDAO and a runner. If
// the runner also implements worker.WorkerResumer it is used to re-attach to an
// already-dispatched spawn on resume; otherwise resume re-dispatches under the
// same deterministic key (the spawn controller's AlreadyExists backstop still
// makes that exactly-once). A nil logger falls back to slog.Default.
func NewWorkflowInterpreter(dao *store.WorkflowDAO, runner worker.WorkerRunner, logger *slog.Logger) *WorkflowInterpreter {
	if logger == nil {
		logger = slog.Default()
	}
	wi := &WorkflowInterpreter{dao: dao, runner: runner, logger: logger, registry: NewDefaultRegistry()}
	if r, ok := runner.(worker.WorkerResumer); ok {
		wi.resumer = r
	}
	return wi
}

// scriptForRun derives the byte-stable program for a run from its frozen
// identity. The canary keeps its proven pre-S7 path; every other template
// resolves through the closed registry (S7). Both fail closed on identity
// problems — the caller terminalizes the run.
func (wi *WorkflowInterpreter) scriptForRun(run *store.WorkflowRun) (string, error) {
	if run.Template == CanaryTemplateName {
		return canaryScriptFromRun(run)
	}
	if wi.registry == nil {
		return "", fmt.Errorf("workflow runtime: no template registry configured for run %s (template %s@%s)",
			run.ID, run.Template, run.TemplateVersion)
	}
	return wi.registry.ScriptFromRun(run)
}

// Run executes (first run OR replay — same code path) the canary script for one
// imperative workflow run. It builds a DAOJournal for run.ID, installs the real
// effect executor, and runs the script on the S0 interpreter. Completed steps
// short-circuit from the journal; the first un-recorded effect runs live; a
// pending-with-spawn-id step re-attaches via Resume.
//
// On clean completion the run is transitioned to state='done'. A QuarantineError
// (nondeterminism tripwire) transitions the run to 'quarantined' and is NOT
// returned as a fatal error (the run is frozen, siblings unaffected). A
// worker.ErrSpawnTerminalFailure transitions the run to 'error': the spawn
// reached a terminal non-completed status, and because resume/retry re-derives
// the SAME deterministic idempotency key it can only re-attach to the same dead
// spawn — retrying every tick loops forever (the wf-canary zombie loop,
// 2026-07-09). Any other error leaves the run 'running' so the next tick
// retries — the pending journal row keeps the interrupted step recoverable.
func (wi *WorkflowInterpreter) Run(ctx context.Context, run *store.WorkflowRun) error {
	if wi == nil || wi.dao == nil {
		return fmt.Errorf("workflow runtime: not configured")
	}
	if run == nil || run.ID == "" {
		return fmt.Errorf("workflow runtime: run.ID required")
	}
	if wi.runner == nil {
		return fmt.Errorf("workflow runtime: no worker runner configured for run %s", run.ID)
	}
	// Engine discriminator guard (S7): the engine is immutable at creation and
	// the imperative interpreter must never advance a DAG run — selection can
	// never re-route a started run, and neither can a caller bug.
	if run.Engine != store.WorkflowEngineImperative {
		return fmt.Errorf("workflow runtime: run %s has engine %q, refusing non-imperative advance", run.ID, run.Engine)
	}
	script, err := wi.scriptForRun(run)
	if err != nil {
		// Workflow params are immutable identity, so retrying cannot repair an
		// invalid harness choice, an unknown template, or a content-hash
		// drift. Terminalize before returning to avoid a scheduler zombie
		// that logs the same fail-closed rejection every tick.
		wi.transition(ctx, run, store.WorkflowRunError)
		return fmt.Errorf("workflow runtime: restore script: %w", err)
	}

	journal := NewDAOJournalFromDAO(ctx, wi.dao)
	host := NewEffectHost(run.ID, journal)
	host.SetEffectExec(wi.effectExec(ctx, run))

	if err := host.Run(script); err != nil {
		var q *QuarantineError
		if errAsQuarantine(err, &q) {
			wi.logger.Warn("workflow run quarantined (nondeterminism)",
				"run_id", run.ID, "step_key", q.StepKey, "want_hash", q.Want, "got_hash", q.Got)
			wi.transition(ctx, run, store.WorkflowRunQuarantined)
			return nil
		}
		if errors.Is(err, worker.ErrSpawnTerminalFailure) {
			// The spawn itself terminally failed (status=failed/stopped).
			// Retrying cannot help: the deterministic idempotency key pins
			// every resume AND every re-dispatch to the same dead spawn, so a
			// 'running' run would re-fail on every tick forever. Freeze the
			// run as 'error'; siblings are unaffected.
			wi.logger.Warn("workflow run failed (terminal spawn failure)",
				"run_id", run.ID, "error", err)
			wi.transition(ctx, run, store.WorkflowRunError)
			return nil
		}
		// Other (transient) error: leave the run 'running' so the next tick
		// retries. The pending journal row keeps the interrupted step
		// recoverable (resume re-runs read-through).
		return fmt.Errorf("workflow run %s: %w", run.ID, err)
	}

	wi.transition(ctx, run, store.WorkflowRunDone)
	wi.logger.Info("workflow run complete", "run_id", run.ID)
	return nil
}

// effectExec returns the real effect executor bound to a run. It is the seam
// the S0 host invokes on a cache-miss (a LIVE effect). The host has already
// appended the 'pending' row BEFORE calling this (record-before-effect), and
// appends the terminal 'success' row AFTER this returns — including the SpawnID
// + CostSource this returns in the EffectResult.
func (wi *WorkflowInterpreter) effectExec(ctx context.Context, run *store.WorkflowRun) func(stepKey, primKind string, args map[string]any, seq int64) (EffectResult, error) {
	return func(stepKey, primKind string, args map[string]any, seq int64) (EffectResult, error) {
		switch primKind {
		case "agent":
			return wi.execAgent(ctx, run, stepKey, args)
		case "gate":
			return wi.execGate(ctx, run, stepKey, args)
		case "merge":
			return wi.execMerge(ctx, run, stepKey, args)
		case "ctx_now", "ctx_uuid":
			// Deterministic context primitives: no external side-effect. Memoize
			// a stable value derived from the step key so replay is byte-stable
			// (the canary never calls these, but the executor stays total).
			return EffectResult{Value: fmt.Sprintf("%s#%d", primKind, seq)}, nil
		default:
			return EffectResult{}, fmt.Errorf("workflow run %s: unknown effect %q", run.ID, primKind)
		}
	}
}

// execAgent dispatches one subordinate spawn via the WorkerRunner. The
// idempotency key is DETERMINISTIC per logical step (DeriveStepIdempotencyKey),
// so a resume re-derives the same key and the spawn controller's exactly-once
// dedupe (S2b/S2c AlreadyExists backstop) re-attaches to the same pod instead
// of double-spawning.
//
// Resume path: if the durable step is already 'pending' with a recorded
// spawn_id (an interrupted dispatch) AND a resumer is available, re-attach via
// Resume(idempotencyKey) rather than issuing a fresh Run. DeriveSpawnID maps
// the key to the same spawn id the create produced, so Resume lands on the
// right pod.
func (wi *WorkflowInterpreter) execAgent(ctx context.Context, run *store.WorkflowRun, stepKey string, args map[string]any) (EffectResult, error) {
	name := stringArg(args, "_0")          // first positional = logical agent name
	model := stringArg(args, "model")      // keyword: harness/agent type
	budget := floatArg(args, "budget_usd") // keyword: per-spawn budget
	if model == "" {
		model = worker.AgentTypeClaudeCode
	}

	idemKey := DeriveStepIdempotencyKey(run.ID, stepKey, args)

	// Resume re-attach: a pending step carrying a spawn_id means a prior process
	// dispatched this spawn but crashed before recording success. Re-attach
	// instead of re-dispatching when the runner can resume.
	if prior, err := wi.dao.GetStep(ctx, run.ID, stepKey); err == nil &&
		prior.Status == store.WorkflowStepPending {
		if wi.resumer != nil && prior.SpawnID != "" {
			wi.logger.Info("workflow resume: re-attaching to in-flight spawn",
				"run_id", run.ID, "step_key", stepKey, "spawn_id", prior.SpawnID)
			res, rerr := wi.resumer.Resume(ctx, idemKey)
			if rerr != nil {
				return EffectResult{}, fmt.Errorf("resume spawn %s (run %s): %w", prior.SpawnID, run.ID, rerr)
			}
			return workerResultToEffect(name, res), nil
		}
		// The record-before-dispatch row normally has no spawn_id yet. Log the
		// independently derived target before reissuing Run so deployed crash
		// evidence proves this process took the deterministic idempotency path,
		// rather than attributing an old pod's historical dedupe line.
		wi.logger.Info("workflow resume: re-dispatching pending spawn with deterministic key",
			"run_id", run.ID, "step_key", stepKey,
			"spawn_id", worker.DeriveSpawnID(idemKey))
	}

	// Git-routing context. The HUD spawn API hard-requires Project + Branch
	// (pkg/mills/clients/spawn.go validation); a workflow script has no
	// JobContext to derive them from, so they come from the interpreter's
	// configured defaults plus a per-run branch. Project falls back to
	// "loom-core" to match the DAG SpawnWorker default. BaseBranch empty
	// defers to the spawn service default ("main").
	project := wi.spawnProject
	if project == "" {
		project = "loom-core"
	}

	req := worker.WorkerRequest{
		AgentType:             model,
		Model:                 model,
		Prompt:                wi.agentPrompt(ctx, run, name),
		BudgetUSD:             budget,
		BacklogID:             run.BacklogID,
		ParentSessionID:       run.ParentSessionID,
		Project:               project,
		Branch:                wi.spawnBranch(run),
		BaseBranch:            wi.spawnBaseBranch,
		Namespace:             "loom-mills",
		Substrate:             "k8s", // S6-min canary targets k8s only (.loom/134 §S6-min)
		CompletionHoldSeconds: canaryCompletionHoldSeconds(run),
		IdempotencyKey:        idemKey,
	}
	res, err := wi.runner.Run(ctx, req)
	if err != nil {
		// Propagate without a success append: the pending row stays recoverable
		// so the next tick re-runs read-through (resume re-attaches via the same
		// deterministic key). Record the spawn id we DID get (if any) so resume
		// can re-attach precisely.
		if res.SpawnID != "" {
			wi.recordPendingSpawnID(ctx, run.ID, stepKey, res.SpawnID)
		}
		return EffectResult{}, fmt.Errorf("agent %q spawn (run %s): %w", name, run.ID, err)
	}
	return workerResultToEffect(name, res), nil
}

// CanaryMergeOutcome is the durable result of the merging canary's single
// merge effect. AlreadyMerged distinguishes an idempotent replay (the merge
// landed in a prior interrupted attempt) from a first-time merge — both are
// success, and the distinction is recorded for the PASS-3 audit.
type CanaryMergeOutcome struct {
	MRIID          int64  `json:"mr_iid"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	SourceBranch   string `json:"source_branch"`
	AlreadyMerged  bool   `json:"already_merged"`
}

// CanaryMergeExecutor performs the merging canary's end-to-end idempotent
// merge: ensure the deterministic canary branch/commit/MR exist for runID,
// then drive the MR to merged. Every sub-step MUST be lookup-first so a
// replay after any crash converges on the same single merge (the operator
// wires this to GitLabClient.Merge, whose merged-state reconciliation makes
// the final PUT idempotent).
type CanaryMergeExecutor interface {
	MergeCanary(ctx context.Context, runID string) (CanaryMergeOutcome, error)
}

// SetMergeExecutor wires the merging canary's merge effect. Safe to leave nil
// on operators that never run merging canaries; merge() then fails closed.
func (wi *WorkflowInterpreter) SetMergeExecutor(m CanaryMergeExecutor) { wi.merger = m }

// execMerge executes the single journaled merge effect of the merging canary
// (S6-full PASS-3 surface). The host has already recorded the 'pending' row
// (record-before-effect) and will append 'success' with this result; a replay
// of a recorded success short-circuits in read-through without re-entering
// this executor, and a replay of an interrupted 'pending' re-invokes the
// idempotent executor, which converges on the already-landed merge.
func (wi *WorkflowInterpreter) execMerge(ctx context.Context, run *store.WorkflowRun, stepKey string, args map[string]any) (EffectResult, error) {
	if wi.merger == nil {
		return EffectResult{}, fmt.Errorf("workflow run %s: merge() requires a configured merge executor", run.ID)
	}
	merging, err := CanaryMergingFromRun(run)
	if err != nil {
		return EffectResult{}, err
	}
	if !merging {
		return EffectResult{}, fmt.Errorf("workflow run %s: merge() is only valid in a merging canary (template %s)", run.ID, CanaryMergingTemplateVersion)
	}
	name := stringArg(args, "_0")
	wi.logger.Info("workflow merge effect", "run_id", run.ID, "step_key", stepKey, "merge", name)
	outcome, err := wi.merger.MergeCanary(ctx, run.ID)
	if err != nil {
		return EffectResult{}, fmt.Errorf("merge %q (run %s): %w", name, run.ID, err)
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		return EffectResult{}, fmt.Errorf("merge %q (run %s): encode outcome: %w", name, run.ID, err)
	}
	return EffectResult{Value: "merge:" + string(encoded)}, nil
}

// execGate evaluates a gate. S6-min: the only canary gate is "trivial" → pass.
// A non-trivial gate name still passes (the trivial evaluator is intentionally
// permissive); S6-full replaces this with a real gate registry. The result is
// memoized so replay returns the same verdict.
func (wi *WorkflowInterpreter) execGate(_ context.Context, _ *store.WorkflowRun, _ string, args map[string]any) (EffectResult, error) {
	name := stringArg(args, "_0")
	// Trivial pass evaluator. Returns a structured result so the journal records
	// a meaningful blob and a future real gate can extend the shape.
	return EffectResult{Value: fmt.Sprintf("gate:%s=pass", name)}, nil
}

// spawnBranch derives the per-run feature branch every agent() spawn for this
// run targets. Deterministic in run.ID so a replay/resume re-derives the same
// branch and re-attaches to the same worktree. (Exactly-once spawn keys off the
// IdempotencyKey, not the branch, but a stable branch keeps the worktree
// identity stable across a resume.) The S6-min canary STOPS at the gate and
// never merges, so a fresh unique branch per run is safe.
func (wi *WorkflowInterpreter) spawnBranch(run *store.WorkflowRun) string {
	return store.WorkflowRunBranchPrefix + run.ID
}

// agentPrompt derives the spawn prompt. Canary templates keep their bounded
// crash-canary protocol verbatim (durable replay contracts — the spawn driver
// owns the side-effect-free completion hold). Registry-template runs (S7) get
// a spec-aware WORK prompt derived from the backlog item; the lookup is
// best-effort and only runs on a live dispatch (replay short-circuits on the
// journal, and a resume re-attaches by idempotency key, so prompt derivation
// never affects exactly-once identity).
func (wi *WorkflowInterpreter) agentPrompt(ctx context.Context, run *store.WorkflowRun, name string) string {
	var identity string
	if run.BacklogID != "" {
		identity = fmt.Sprintf("Mills imperative workflow %s, step %q for backlog item %s.", run.ID, name, run.BacklogID)
	} else {
		identity = fmt.Sprintf("Mills imperative workflow %s, step %q.", run.ID, name)
	}
	if run.Template != CanaryTemplateName {
		return wi.registryWorkPrompt(ctx, run, identity)
	}
	// Template versions are durable replay contracts. A v1 run may be
	// re-dispatched after an operator upgrade, so retain its model-owned hold
	// instead of pairing the v2 prompt with a zero driver hold and collapsing
	// the crash window.
	if run.Template == CanaryTemplateName && run.TemplateVersion == legacyCanaryTemplateVersion {
		return fmt.Sprintf("%s Crash-recovery canary protocol: use the shell tool exactly once to run `sleep %d`; keep that foreground shell call in flight and wait until it actually exits (a yielded or still-running shell session is not completion); do not edit files or invoke any other tool. After it completes, reply with exactly MILLS_CANARY_OK.",
			identity, CanaryHoldSeconds)
	}
	return fmt.Sprintf("%s Crash-recovery canary protocol: do not edit files or invoke tools; reply with exactly MILLS_CANARY_OK. The spawn driver owns the bounded completion hold after this response.", identity)
}

// registryWorkPrompt builds the spec-aware work prompt for an S7 registry
// run. The contract mirrors the template's shape: implement on the current
// branch, commit and push, and STOP — the run's gate evaluates next, and the
// item escalates for human review regardless of outcome (v1 templates never
// merge). A failed or missing backlog lookup degrades to a generic prompt.
func (wi *WorkflowInterpreter) registryWorkPrompt(ctx context.Context, run *store.WorkflowRun, identity string) string {
	task := "Implement the assigned backlog item."
	if wi.backlogItemLookup != nil && run.BacklogID != "" {
		if title, specDoc, err := wi.backlogItemLookup(ctx, run.BacklogID); err == nil {
			if title = strings.TrimSpace(title); title != "" {
				task = "Task: " + title + "."
			}
			if specDoc = strings.TrimSpace(specDoc); specDoc != "" {
				task += " Spec reference: " + specDoc + " (read it in the repo before starting)."
			}
		} else {
			wi.logger.Warn("workflow: backlog lookup for work prompt failed; using generic prompt",
				"run_id", run.ID, "backlog_id", run.BacklogID, "error", err)
		}
	}
	return fmt.Sprintf("%s %s Work on the CURRENT branch only: implement the change, run the relevant tests, then commit and push. Do NOT create merge requests, do NOT merge, do NOT push to any other branch — this run stops pre-merge and a human reviews the branch after the workflow's gate. When your committed work is pushed, you are done.",
		identity, task)
}

func canaryCompletionHoldSeconds(run *store.WorkflowRun) int {
	if run != nil && run.Template == CanaryTemplateName &&
		(run.TemplateVersion == CanaryTemplateVersion || run.TemplateVersion == CanaryMergingTemplateVersion) {
		return CanaryHoldSeconds
	}
	return 0
}

// recordPendingSpawnID advances the pending step to carry the spawn id we
// received on a failed/partial dispatch, so a later resume re-attaches to the
// right pod. Best-effort: a write failure is logged, not propagated (the run
// retries regardless).
func (wi *WorkflowInterpreter) recordPendingSpawnID(ctx context.Context, runID, stepKey, spawnID string) {
	prior, err := wi.dao.GetStep(ctx, runID, stepKey)
	if err != nil {
		return
	}
	prior.SpawnID = spawnID
	if _, err := wi.dao.AppendStep(ctx, prior); err != nil {
		wi.logger.Warn("workflow: failed to record pending spawn id",
			"run_id", runID, "step_key", stepKey, "spawn_id", spawnID, "error", err)
	}
}

// transition best-effort updates the run state. A write failure is logged, not
// returned: the run's terminal status is observability; the durable step
// journal is the source of truth for resume.
func (wi *WorkflowInterpreter) transition(ctx context.Context, run *store.WorkflowRun, state store.WorkflowRunState) {
	loaded, err := wi.dao.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		wi.logger.Warn("workflow transition: load run failed",
			"run_id", run.ID, "state", string(state), "error", err)
		return
	}
	// A lifecycle endpoint may have paused/failed the run while an agent spawn
	// was in flight. That operator decision is a terminal fence: a late worker
	// result must never resurrect it as done (or overwrite pause/error with a
	// different runtime transition).
	if loaded.State != store.WorkflowRunRunning {
		wi.logger.Info("workflow transition skipped: stored run is no longer running",
			"run_id", run.ID, "stored_state", string(loaded.State), "requested_state", string(state))
		run.State = loaded.State
		return
	}
	if wi.beforeTransitionCAS != nil {
		wi.beforeTransitionCAS()
	}
	loaded.State = state
	updated, err := wi.dao.CompareAndSetWorkflowRunLifecycle(ctx, loaded, store.WorkflowRunRunning)
	if err == nil && updated {
		mills.WorkflowRunsTerminalTotal.WithLabelValues(string(state), "runtime").Inc()
	}
	if err != nil {
		wi.logger.Warn("workflow transition: lifecycle CAS failed",
			"run_id", run.ID, "state", string(state), "error", err)
		return
	}
	if updated {
		run.State = state
		return
	}
	// A pause/fail won after our load. Re-read it and preserve the operator's
	// decision instead of publishing the stale requested transition in memory.
	current, err := wi.dao.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		wi.logger.Warn("workflow transition: reload CAS winner failed", "run_id", run.ID, "error", err)
		return
	}
	run.State = current.State
	wi.logger.Info("workflow transition skipped: concurrent lifecycle mutation won",
		"run_id", run.ID, "stored_state", string(current.State), "requested_state", string(state))
}

// DeriveStepIdempotencyKey returns the deterministic idempotency key for one
// logical agent step. It is derived from run.ID + step_key + the call's
// canonical args hash, so:
//   - it is STABLE across a crash/resume (the same step re-derives the same
//     key), driving the spawn controller's exactly-once dedupe (S2b/S2c); and
//   - it is UNIQUE per logical step (two distinct agent() calls in the same run
//     get distinct keys).
//
// The args are folded in via the same canonical call-hash the journal uses, so
// the key matches the step's determinism fingerprint.
func DeriveStepIdempotencyKey(runID, stepKey string, args map[string]any) string {
	return DeriveStepIdempotencyKeyFromHash(runID, stepKey, canonicalCallHash("agent", args))
}

// DeriveStepIdempotencyKeyFromHash is the callHash-taking form of
// DeriveStepIdempotencyKey. Exposed so observers that hold a journal row
// (run_id + step_key + call_hash — e.g. the S1c kill-test harness) can
// derive the spawn identity of an IN-FLIGHT dispatch: during a healthy
// runner.Run the pending row does not carry spawn_id (it is only recorded
// on completion or on a failed dispatch), so the id must be re-derived,
// not read.
func DeriveStepIdempotencyKeyFromHash(runID, stepKey, callHash string) string {
	h := sha256.New()
	h.Write([]byte(runID))
	h.Write([]byte{0x00})
	h.Write([]byte(stepKey))
	h.Write([]byte{0x00})
	h.Write([]byte(callHash))
	return "wf-" + hex.EncodeToString(h.Sum(nil))[:24]
}

// workerResultToEffect maps a worker.WorkerResult into the host EffectResult,
// carrying the memoized value (a small structured summary — NOT the whole diff,
// which is not needed for replay) plus the spawn provenance the terminal step
// records.
func workerResultToEffect(name string, res worker.WorkerResult) EffectResult {
	return EffectResult{
		Value: map[string]any{
			"agent":         name,
			"spawn_id":      res.SpawnID,
			"files_changed": res.FilesChanged,
			"lines_added":   res.LinesAdded,
			"lines_removed": res.LinesRemoved,
		},
		SpawnID:    res.SpawnID,
		CostSource: res.CostSource.String(),
		CostUSD:    res.CostUSD,
	}
}

// errAsQuarantine is a tiny errors.As wrapper kept local so runtime.go does not
// import "errors" twice across files; it reports whether err (or anything it
// wraps) is a *QuarantineError, binding it into target.
func errAsQuarantine(err error, target **QuarantineError) bool {
	for err != nil {
		if q, ok := err.(*QuarantineError); ok {
			*target = q
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// stringArg reads a string-valued arg from the canonical args map (positionals
// are keyed "_0","_1",…; keywords by name). Missing or non-string → "".
func stringArg(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// floatArg reads a numeric arg (int64 or float64 — starToGo produces either)
// from the canonical args map. Missing or non-numeric → 0.
func floatArg(args map[string]any, key string) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	default:
		return 0
	}
}

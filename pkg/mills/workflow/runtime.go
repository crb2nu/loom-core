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
//   - hardcoded canary script: agent('implement') → gate('trivial') → done. It
//     STOPS at the gate; there is NO merge() step (merge idempotency is S6-full).
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
	"fmt"
	"log/slog"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/worker"
)

// canaryScript is the hardcoded S6-min canary. It dispatches one implement
// agent, evaluates one trivial gate, then ends. It deliberately STOPS at the
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

// CanaryScript exposes the hardcoded canary for the admin entrypoint + tests.
func CanaryScript() string { return canaryScript }

// WorkflowInterpreter is the Layer-3 runtime: it executes one imperative
// workflow run against the durable DAO journal, wiring the S0 interpreter's
// effect primitives to real executors. One instance is shared across all runs
// (it is stateless per-run; the run id is carried into Run).
type WorkflowInterpreter struct {
	dao     *store.WorkflowDAO
	runner  worker.WorkerRunner
	resumer worker.WorkerResumer // optional; nil when the runner can't resume
	logger  *slog.Logger
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
	wi := &WorkflowInterpreter{dao: dao, runner: runner, logger: logger}
	if r, ok := runner.(worker.WorkerResumer); ok {
		wi.resumer = r
	}
	return wi
}

// Run executes (first run OR replay — same code path) the canary script for one
// imperative workflow run. It builds a DAOJournal for run.ID, installs the real
// effect executor, and runs the script on the S0 interpreter. Completed steps
// short-circuit from the journal; the first un-recorded effect runs live; a
// pending-with-spawn-id step re-attaches via Resume.
//
// On clean completion the run is transitioned to state='done'. A QuarantineError
// (nondeterminism tripwire) transitions the run to 'quarantined' and is NOT
// returned as a fatal error (the run is frozen, siblings unaffected). Any other
// error leaves the run 'running' so the next tick retries — the pending journal
// row keeps the interrupted step recoverable.
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

	journal := NewDAOJournalFromDAO(ctx, wi.dao)
	host := NewEffectHost(run.ID, journal)
	host.SetEffectExec(wi.effectExec(ctx, run))

	if err := host.Run(canaryScript); err != nil {
		var q *QuarantineError
		if errAsQuarantine(err, &q) {
			wi.logger.Warn("workflow run quarantined (nondeterminism)",
				"run_id", run.ID, "step_key", q.StepKey, "want_hash", q.Want, "got_hash", q.Got)
			wi.transition(ctx, run, store.WorkflowRunQuarantined)
			return nil
		}
		// Non-quarantine error: leave the run 'running' so the next tick
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
	if wi.resumer != nil {
		if prior, err := wi.dao.GetStep(ctx, run.ID, stepKey); err == nil &&
			prior.Status == store.WorkflowStepPending && prior.SpawnID != "" {
			wi.logger.Info("workflow resume: re-attaching to in-flight spawn",
				"run_id", run.ID, "step_key", stepKey, "spawn_id", prior.SpawnID)
			res, rerr := wi.resumer.Resume(ctx, idemKey)
			if rerr != nil {
				return EffectResult{}, fmt.Errorf("resume spawn %s (run %s): %w", prior.SpawnID, run.ID, rerr)
			}
			return workerResultToEffect(name, res), nil
		}
	}

	req := worker.WorkerRequest{
		AgentType:       model,
		Model:           model,
		Prompt:          wi.agentPrompt(run, name),
		BudgetUSD:       budget,
		BacklogID:       run.BacklogID,
		ParentSessionID: run.ParentSessionID,
		Namespace:       "loom-mills",
		Substrate:       "k8s", // S6-min canary targets k8s only (.loom/134 §S6-min)
		IdempotencyKey:  idemKey,
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

// agentPrompt derives a terse prompt for a logical agent step. S6-min keeps it
// minimal; S6-full will load spec-doc-aware prompts. Deterministic given the
// run + name so the call_hash and any prompt-derived behavior is stable.
func (wi *WorkflowInterpreter) agentPrompt(run *store.WorkflowRun, name string) string {
	if run.BacklogID != "" {
		return fmt.Sprintf("Mills imperative workflow %s, step %q for backlog item %s.", run.ID, name, run.BacklogID)
	}
	return fmt.Sprintf("Mills imperative workflow %s, step %q.", run.ID, name)
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
	loaded.State = state
	if err := wi.dao.PutWorkflowRun(ctx, loaded); err != nil {
		wi.logger.Warn("workflow transition: put run failed",
			"run_id", run.ID, "state", string(state), "error", err)
		return
	}
	run.State = state
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
	callHash := canonicalCallHash("agent", args)
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

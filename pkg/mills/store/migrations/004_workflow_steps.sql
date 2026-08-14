-- +goose Up
-- +goose StatementBegin

-- 004_workflow_steps.sql — Layer-2 durable workflow step/event journal.
--
-- This is the stable persistence layer for the Mills durable workflow
-- engine (the imperative runtime ships in a later slice). It is purely
-- additive: nothing in 001/002/003 changes, and the live pipeline is
-- unaffected.
--
-- DUAL SOURCE-OF-TRUTH (load-bearing invariant):
--   * Legacy `dag` runs do NOT write workflow_steps. Only future
--     `imperative` runs append rows here.
--   * workflow_steps is the SOURCE OF TRUTH for imperative resume: the
--     runtime replays the append-only step log to reconstruct state. The
--     generic `events` table (migration 001) stays ADVISORY — it is for
--     audit/debug, never for resume.
--
-- RECORD-BEFORE-RESULT ordering invariant:
--   A step is appended with status='pending' BEFORE its effect runs, then
--   updated to 'success' (with result_blob) once the effect completes. A
--   crash between the two writes leaves a recoverable 'pending' row, which
--   the runtime reconciles on restart.
--
-- Spec/plan: .loom/130-133 (Mills dynamic-workflows). S1 in-process
-- kill-test passed 2026-06-06 (feat/mills-workflow-killtest-spike).

-- workflow_runs — one row per workflow execution.
--
-- `engine` is an IMMUTABLE discriminator set at creation: 'dag' for the
-- legacy pipeline (kept for symmetry / cross-reference; legacy runs do not
-- write workflow_steps), 'imperative' for the durable runtime. It must
-- never change for a given run id.
CREATE TABLE workflow_runs (
    id                          TEXT PRIMARY KEY,
    backlog_id                  TEXT REFERENCES backlog_items(id) ON DELETE SET NULL,
    engine                      TEXT NOT NULL,              -- IMMUTABLE: dag | imperative
    template                    TEXT NOT NULL,
    template_version            TEXT NOT NULL,
    interpreter_version         TEXT NOT NULL,
    workflow_params             TEXT,                       -- opaque JSON params blob
    state                       TEXT NOT NULL,              -- running | paused | done | escalated | error | quarantined
    paused_at                   TEXT,
    resumed_at                  TEXT,
    started_at                  TEXT,
    ended_at                    TEXT,
    cost_usd                    REAL NOT NULL DEFAULT 0,
    parent_session_id           TEXT
);
CREATE INDEX idx_workflow_runs_state  ON workflow_runs(state);
CREATE INDEX idx_workflow_runs_engine ON workflow_runs(engine);

-- workflow_steps — append-only journal of every effect/event in a run.
--
-- `step_key` is an OPAQUE structured key string minted by the runtime
-- (e.g. a deterministic path through the workflow); the store treats it as
-- a bare string and never parses it. UNIQUE(run_id, step_key) makes
-- AppendStep idempotent: re-appending an identical recorded step is a
-- no-op, and a 'pending' row can transition to a terminal status.
--
-- `call_hash` fingerprints the recorded call site/args. A MISMATCH on an
-- existing step_key signals nondeterminism and must be surfaced to the
-- caller (never silently overwritten) so the runtime can quarantine.
CREATE TABLE workflow_steps (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id                      TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    step_key                    TEXT NOT NULL,              -- opaque structured key string
    event_type                  TEXT NOT NULL,              -- spawn_requested | gate_eval | … (see types.go)
    call_hash                   TEXT NOT NULL,              -- determinism fingerprint
    idempotency_key             TEXT,
    status                      TEXT NOT NULL,              -- pending | success | error | gate_fail | skipped
    spawn_id                    TEXT,
    started_at                  TEXT,
    ended_at                    TEXT,
    result_blob                 TEXT,                       -- opaque JSON result (set on completion)
    cost_usd                    REAL NOT NULL DEFAULT 0,
    cost_source                 TEXT,                       -- real | estimated | unavailable
    effect_count                INTEGER NOT NULL DEFAULT 0,
    UNIQUE(run_id, step_key)
);

-- Pending-step scan: drives crash-between-writes reconciliation. Partial
-- index keeps it tiny (only un-finished rows land here).
CREATE INDEX idx_workflow_pending
    ON workflow_steps(run_id) WHERE status = 'pending';

-- Replay scan: ordered reconstruction of a run from its step log.
CREATE INDEX idx_workflow_replay
    ON workflow_steps(run_id, step_key);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_replay;
DROP INDEX IF EXISTS idx_workflow_pending;
DROP TABLE IF EXISTS workflow_steps;
DROP INDEX IF EXISTS idx_workflow_runs_engine;
DROP INDEX IF EXISTS idx_workflow_runs_state;
DROP TABLE IF EXISTS workflow_runs;
-- +goose StatementEnd

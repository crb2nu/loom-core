-- +goose Up
-- +goose StatementBegin

-- Monotonic queue-claim version. The transactional start kernel is the only
-- writer; general backlog upserts deliberately preserve it.
ALTER TABLE backlog_items ADD COLUMN claim_version INTEGER NOT NULL DEFAULT 0;

-- Monotonic compare-and-swap version for every backlog mutation. Keeping this
-- separate from claim_version prevents metadata edits from racing admission
-- while claim_version remains the aggregate transition identity.
ALTER TABLE backlog_items ADD COLUMN row_version INTEGER NOT NULL DEFAULT 0;

-- Correlates a pipeline row with the backlog aggregate version that created it.
ALTER TABLE pipeline_runs ADD COLUMN aggregate_version INTEGER NOT NULL DEFAULT 0;

-- Independent mutation revision for pipeline stage/state rollups. Aggregate
-- version remains immutable correlation; row_version fences duplicate runners.
ALTER TABLE pipeline_runs ADD COLUMN row_version INTEGER NOT NULL DEFAULT 0;

-- Covers the reconciler's queued scan and the first claim CAS predicate.
CREATE INDEX idx_backlog_claim_queue
    ON backlog_items(state, priority, created_at, id);

-- Rolling budget admission reads the last 24 hours on every claim. Keep that
-- window logarithmic as pipeline history grows into tens of thousands of rows.
CREATE INDEX idx_pipeline_started_at
    ON pipeline_runs(started_at);

-- Estimated pipeline spend is reserved at claim time. This closes the window
-- where many concurrent starts all observe the same pre-dispatch daily spend.
CREATE TABLE pipeline_budget_reservations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL UNIQUE REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    backlog_id      TEXT NOT NULL REFERENCES backlog_items(id) ON DELETE CASCADE,
    reserved_usd    REAL NOT NULL CHECK (reserved_usd >= 0),
    state           TEXT NOT NULL CHECK (state IN ('active', 'released')),
    created_at      TEXT NOT NULL,
    released_at     TEXT
);
CREATE INDEX idx_pipeline_reservations_active
    ON pipeline_budget_reservations(state, created_at)
    WHERE state = 'active';

-- One immutable transition row per backlog aggregate version. The generic
-- events table remains advisory; this ledger proves committed state changes.
CREATE TABLE pipeline_transitions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    backlog_id          TEXT NOT NULL REFERENCES backlog_items(id) ON DELETE CASCADE,
    aggregate_version   INTEGER NOT NULL,
    run_id              TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    kind                TEXT NOT NULL,
    from_state          TEXT NOT NULL,
    to_state            TEXT NOT NULL,
    occurred_at         TEXT NOT NULL,
    UNIQUE(backlog_id, aggregate_version)
);
CREATE INDEX idx_pipeline_transitions_run
    ON pipeline_transitions(run_id);

-- Durable start outbox. The unique run_id key guarantees that retries and
-- concurrent claim attempts produce one logical dispatch intent.
CREATE TABLE pending_dispatches (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id              TEXT NOT NULL UNIQUE REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    backlog_id          TEXT NOT NULL REFERENCES backlog_items(id) ON DELETE CASCADE,
    aggregate_version   INTEGER NOT NULL,
    kind                TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('pending', 'delivered', 'dead_letter')),
    attempts            INTEGER NOT NULL DEFAULT 0,
    last_error          TEXT,
    next_attempt_at     TEXT NOT NULL,
    lease_token         TEXT,
    lease_expires_at    TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    delivered_at        TEXT,
    dead_lettered_at    TEXT
);
CREATE INDEX idx_pending_dispatches_ready
    ON pending_dispatches(status, attempts, next_attempt_at, lease_expires_at, id)
    WHERE status = 'pending';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_pending_dispatches_ready;
DROP TABLE IF EXISTS pending_dispatches;
DROP INDEX IF EXISTS idx_pipeline_transitions_run;
DROP TABLE IF EXISTS pipeline_transitions;
DROP INDEX IF EXISTS idx_pipeline_reservations_active;
DROP TABLE IF EXISTS pipeline_budget_reservations;
DROP INDEX IF EXISTS idx_pipeline_started_at;
DROP INDEX IF EXISTS idx_backlog_claim_queue;
ALTER TABLE pipeline_runs DROP COLUMN row_version;
ALTER TABLE pipeline_runs DROP COLUMN aggregate_version;
ALTER TABLE backlog_items DROP COLUMN row_version;
ALTER TABLE backlog_items DROP COLUMN claim_version;
-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin

-- S7 ClaimWorkflowStart (.loom/134 §S7): imperative workflow runs started
-- from queued backlog items share the pipeline budget-reservation pool and
-- the aggregate transition journal, so run_id in both tables becomes
-- POLYMORPHIC — a pipeline_runs id (DAG lane) OR an imperative
-- workflow_runs id. SQLite cannot drop a foreign key in place, so both
-- tables are rebuilt without the REFERENCES pipeline_runs(id) constraint.
--
-- What is deliberately kept: the backlog_items FK (every row still belongs
-- to a real item), the UNIQUE(run_id) reservation key, the
-- UNIQUE(backlog_id, aggregate_version) exactly-once transition ledger, and
-- both partial/covering indexes. What is deliberately lost: ON DELETE
-- CASCADE from pipeline_runs — acceptable because runs are append-only in
-- practice and both start kernels mint run_id inside the same transaction
-- that writes these rows, so a dangling reference cannot be created.

CREATE TABLE pipeline_budget_reservations_new (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL UNIQUE,
    backlog_id      TEXT NOT NULL REFERENCES backlog_items(id) ON DELETE CASCADE,
    reserved_usd    REAL NOT NULL CHECK (reserved_usd >= 0),
    state           TEXT NOT NULL CHECK (state IN ('active', 'released')),
    created_at      TEXT NOT NULL,
    released_at     TEXT
);
INSERT INTO pipeline_budget_reservations_new
    (id, run_id, backlog_id, reserved_usd, state, created_at, released_at)
    SELECT id, run_id, backlog_id, reserved_usd, state, created_at, released_at
    FROM pipeline_budget_reservations;
DROP TABLE pipeline_budget_reservations;
ALTER TABLE pipeline_budget_reservations_new RENAME TO pipeline_budget_reservations;
CREATE INDEX idx_pipeline_reservations_active
    ON pipeline_budget_reservations(state, created_at)
    WHERE state = 'active';

CREATE TABLE pipeline_transitions_new (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    backlog_id          TEXT NOT NULL REFERENCES backlog_items(id) ON DELETE CASCADE,
    aggregate_version   INTEGER NOT NULL,
    run_id              TEXT NOT NULL,
    kind                TEXT NOT NULL,
    from_state          TEXT NOT NULL,
    to_state            TEXT NOT NULL,
    occurred_at         TEXT NOT NULL,
    UNIQUE(backlog_id, aggregate_version)
);
INSERT INTO pipeline_transitions_new
    (id, backlog_id, aggregate_version, run_id, kind, from_state, to_state, occurred_at)
    SELECT id, backlog_id, aggregate_version, run_id, kind, from_state, to_state, occurred_at
    FROM pipeline_transitions;
DROP TABLE pipeline_transitions;
ALTER TABLE pipeline_transitions_new RENAME TO pipeline_transitions;
CREATE INDEX idx_pipeline_transitions_run
    ON pipeline_transitions(run_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Down migration intentionally re-adds the pipeline_runs FK. It will fail if
-- imperative (WF-*) rows exist — delete them first; that is the correct
-- fail-closed behavior for a downgrade that cannot represent them.
CREATE TABLE pipeline_budget_reservations_old (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL UNIQUE REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    backlog_id      TEXT NOT NULL REFERENCES backlog_items(id) ON DELETE CASCADE,
    reserved_usd    REAL NOT NULL CHECK (reserved_usd >= 0),
    state           TEXT NOT NULL CHECK (state IN ('active', 'released')),
    created_at      TEXT NOT NULL,
    released_at     TEXT
);
INSERT INTO pipeline_budget_reservations_old
    (id, run_id, backlog_id, reserved_usd, state, created_at, released_at)
    SELECT id, run_id, backlog_id, reserved_usd, state, created_at, released_at
    FROM pipeline_budget_reservations;
DROP TABLE pipeline_budget_reservations;
ALTER TABLE pipeline_budget_reservations_old RENAME TO pipeline_budget_reservations;
CREATE INDEX idx_pipeline_reservations_active
    ON pipeline_budget_reservations(state, created_at)
    WHERE state = 'active';

CREATE TABLE pipeline_transitions_old (
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
INSERT INTO pipeline_transitions_old
    (id, backlog_id, aggregate_version, run_id, kind, from_state, to_state, occurred_at)
    SELECT id, backlog_id, aggregate_version, run_id, kind, from_state, to_state, occurred_at
    FROM pipeline_transitions;
DROP TABLE pipeline_transitions;
ALTER TABLE pipeline_transitions_old RENAME TO pipeline_transitions;
CREATE INDEX idx_pipeline_transitions_run
    ON pipeline_transitions(run_id);
-- +goose StatementEnd

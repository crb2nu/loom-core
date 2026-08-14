-- +goose Up
-- +goose StatementBegin

-- Council admission reserves its conservative estimate before any model call.
-- The provisional council_runs row is the durable attempt record; this table
-- protects the gap between that admission and terminal actual-cost accounting.
CREATE TABLE council_budget_reservations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL UNIQUE REFERENCES council_runs(id) ON DELETE CASCADE,
    reserved_usd    REAL NOT NULL CHECK (reserved_usd >= 0),
    state           TEXT NOT NULL CHECK (state IN ('active', 'released')),
    created_at      TEXT NOT NULL,
    expires_at      TEXT NOT NULL,
    released_at     TEXT
);
CREATE INDEX idx_council_reservations_active
    ON council_budget_reservations(state, expires_at, created_at)
    WHERE state = 'active';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_council_reservations_active;
DROP TABLE IF EXISTS council_budget_reservations;
-- +goose StatementEnd

-- +goose Up
CREATE TABLE scope_fairness_state (
    backlog_id TEXT PRIMARY KEY REFERENCES backlog_items(id) ON DELETE CASCADE,
    first_deferred_at TEXT NOT NULL,
    deferral_count INTEGER NOT NULL DEFAULT 0,
    reserved_at TEXT
);
CREATE INDEX idx_scope_fairness_reserved ON scope_fairness_state(reserved_at) WHERE reserved_at IS NOT NULL;

-- +goose Down
DROP TABLE scope_fairness_state;

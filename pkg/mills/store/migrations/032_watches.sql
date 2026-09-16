-- +goose Up
-- +goose StatementBegin
CREATE TABLE watches (
    id                 TEXT PRIMARY KEY,
    subject_kind       TEXT NOT NULL CHECK (subject_kind IN ('backlog_item', 'merge_request', 'pipeline_run')),
    subject_id         TEXT NOT NULL,
    terminal_condition TEXT NOT NULL,
    note               TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL CHECK (state IN ('active', 'met', 'expired', 'cancelled')),
    created_at         TEXT NOT NULL,
    expires_at         TEXT NOT NULL,
    resolved_at        TEXT,
    resolution         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_watches_active_expiry
    ON watches(expires_at, id) WHERE state = 'active';
CREATE INDEX idx_watches_subject
    ON watches(subject_kind, subject_id, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS watches;
-- +goose StatementEnd

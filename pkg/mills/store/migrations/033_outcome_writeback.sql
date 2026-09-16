-- +goose Up
-- +goose StatementBegin
CREATE TABLE outcome_writebacks (
    backlog_id      TEXT PRIMARY KEY REFERENCES backlog_items(id) ON DELETE CASCADE,
    pipeline_run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    plan_id         TEXT,
    outcome         TEXT NOT NULL CHECK (outcome IN ('merged', 'escalated')),
    attempts        INTEGER NOT NULL CHECK (attempts >= 0),
    cost_usd        REAL NOT NULL CHECK (cost_usd >= 0),
    dispatch_score  REAL NOT NULL,
    grade           TEXT CHECK (grade IS NULL OR grade IN ('keep', 'meh', 'regret')),
    written_at      TEXT NOT NULL
);

CREATE INDEX idx_outcome_writebacks_written_at
    ON outcome_writebacks(written_at);
CREATE INDEX idx_outcome_writebacks_plan_written
    ON outcome_writebacks(plan_id, written_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_outcome_writebacks_plan_written;
DROP INDEX IF EXISTS idx_outcome_writebacks_written_at;
DROP TABLE IF EXISTS outcome_writebacks;
-- +goose StatementEnd

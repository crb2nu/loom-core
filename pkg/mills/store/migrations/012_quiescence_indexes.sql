-- +goose Up

-- The destructive-safety read counts only council rows without a terminal
-- timestamp. Council history is append-heavy, so keep that proof proportional
-- to active work rather than total historical runs.
CREATE INDEX idx_council_active
    ON council_runs(ended_at)
    WHERE ended_at IS NULL;

-- Keep each terminal-deny-list proof proportional to non-terminal work. The
-- predicates intentionally match ReadQuiescence exactly, including unknown
-- future states, so SQLite can use these as covering partial indexes.
CREATE INDEX idx_pipeline_quiescence_active
    ON pipeline_runs(state)
    WHERE state NOT IN ('done', 'escalated', 'paused');

CREATE INDEX idx_workflow_quiescence_active
    ON workflow_runs(state)
    WHERE state NOT IN ('done', 'escalated', 'error', 'quarantined');

CREATE INDEX idx_spin_quiescence_active
    ON spin_runs(status)
    WHERE status NOT IN ('succeeded', 'failed', 'timeout');

CREATE INDEX idx_cross_repo_quiescence_active
    ON cross_repo_runs(state)
    WHERE state NOT IN ('merged', 'reverted', 'failed');

CREATE INDEX idx_dispatch_quiescence_active
    ON pending_dispatches(status)
    WHERE status NOT IN ('delivered', 'dead_letter');

-- +goose Down

DROP INDEX IF EXISTS idx_dispatch_quiescence_active;
DROP INDEX IF EXISTS idx_cross_repo_quiescence_active;
DROP INDEX IF EXISTS idx_spin_quiescence_active;
DROP INDEX IF EXISTS idx_workflow_quiescence_active;
DROP INDEX IF EXISTS idx_pipeline_quiescence_active;
DROP INDEX IF EXISTS idx_council_active;

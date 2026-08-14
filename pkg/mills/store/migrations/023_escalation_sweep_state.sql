CREATE TABLE escalation_sweep_state (
    backlog_id TEXT PRIMARY KEY REFERENCES backlog_items(id) ON DELETE CASCADE,
    mr_iid INTEGER NOT NULL DEFAULT 0,
    recheck_after TEXT NOT NULL,
    recheck_streak INTEGER NOT NULL DEFAULT 0 CHECK (recheck_streak >= 0)
);

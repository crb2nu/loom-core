-- The ordering tuple is independent of the candidate's lifetime and state.
CREATE TABLE auto_requeue_sweep_cursor (
 singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
 backlog_id TEXT NOT NULL,
 priority TEXT NOT NULL,
 created_at TEXT NOT NULL
);

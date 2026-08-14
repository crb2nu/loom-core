CREATE TABLE external_incident_dwells (
    run_id                  TEXT PRIMARY KEY REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    dependency_id           TEXT NOT NULL DEFAULT '',
    dependency              TEXT NOT NULL DEFAULT '',
    started_at              TEXT NOT NULL,
    deadline_at             TEXT NOT NULL,
    completed_at            TEXT,
    completion_reason       TEXT,
    elapsed_duration_millis INTEGER,
    CHECK (completion_reason IS NULL OR completion_reason IN ('recovered', 'timeout', 'fast_kill'))
);

CREATE INDEX idx_external_incident_dwells_open_deadline
    ON external_incident_dwells(deadline_at)
    WHERE completed_at IS NULL;

CREATE TABLE IF NOT EXISTS default_branch_pipeline_observations (
    project TEXT NOT NULL,
    branch TEXT NOT NULL,
    pipeline_id TEXT NOT NULL,
    classification TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    PRIMARY KEY (project, branch, pipeline_id)
);

CREATE INDEX IF NOT EXISTS idx_default_branch_pipeline_observations_recent
ON default_branch_pipeline_observations(project, branch, observed_at DESC, pipeline_id DESC);

CREATE TABLE IF NOT EXISTS main_red_external_holds (
    project TEXT NOT NULL,
    branch TEXT NOT NULL,
    activated_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    escalation_sent_at TEXT,
    cleared_at TEXT,
    PRIMARY KEY (project, branch)
);

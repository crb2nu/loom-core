-- A stamp is an explicit intent to deliver generated work to one project.
-- There is deliberately no default target: callers must bind the destination
-- before the intent becomes durable.
CREATE TABLE cross_repo_stamps (
    id             TEXT PRIMARY KEY,
    target_project TEXT NOT NULL CHECK (
        length(trim(target_project, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0
    ),
    created_at     TEXT NOT NULL
);

CREATE INDEX idx_cross_repo_stamps_target_project_created
    ON cross_repo_stamps(target_project, created_at);

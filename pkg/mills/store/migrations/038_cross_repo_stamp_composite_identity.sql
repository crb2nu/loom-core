-- A Pattern Loom stamp is scoped to its destination. Preserve existing rows
-- while replacing the legacy pattern-id-only primary key with the target and
-- pattern composite identity.
CREATE TABLE cross_repo_stamps_v2 (
    id             TEXT NOT NULL,
    target_project TEXT NOT NULL CHECK (
        length(trim(target_project, char(9) || char(10) || char(11) || char(12) || char(13) || ' ')) > 0
    ),
    created_at     TEXT NOT NULL,
    PRIMARY KEY (target_project, id)
);

INSERT INTO cross_repo_stamps_v2 (id, target_project, created_at)
SELECT id, target_project, created_at FROM cross_repo_stamps;

DROP TABLE cross_repo_stamps;
ALTER TABLE cross_repo_stamps_v2 RENAME TO cross_repo_stamps;

CREATE INDEX idx_cross_repo_stamps_target_project_created
    ON cross_repo_stamps(target_project, created_at);

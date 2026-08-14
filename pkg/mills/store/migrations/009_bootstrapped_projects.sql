-- 009_bootstrapped_projects.sql — registry of projects the operator minted at
-- runtime from a Spinning Room plan (POST /api/mills/projects/bootstrap).
--
-- Motivation: a spin (or the plans compare/merge editor) can author a draft
-- plan for a product that has NO repo yet — the plan lands with project=""
-- and the plan-slice emitter can never source it. Bootstrap creates the
-- GitLab project, seeds an initial commit, re-scopes the plan onto the new
-- path, and records the project here. The emitter unions this table with the
-- static cross_repo.demand_projects allowlist each tick (gated by
-- cross_repo.enabled AND cross_repo.allow_bootstrapped — two-key, fails
-- closed), so a bootstrapped repo dispatches without a per-repo gitops edit.
--
-- project is the full GitLab path ("services/procmodel") — the same string
-- stamped as emitted items' TargetProject and set as the plan's canonical
-- project id. One row per project; a re-bootstrap of the same path is a
-- conflict, not an upsert (the repo already exists on GitLab).
-- Append-only: nothing in 001–008 changes.

CREATE TABLE bootstrapped_projects (
    project    TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL,
    web_url    TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

-- The emitter's per-tick union reads the whole table ordered by age; newest
-- rows last so log lines read chronologically.
CREATE INDEX idx_bootstrapped_created ON bootstrapped_projects(created_at);

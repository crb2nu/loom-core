-- +goose Up
-- +goose StatementBegin

-- Failure-shape fingerprint (shepherd program B2). Stamped at escalation
-- classification time from the normalized failure evidence tail
-- (pkg/mills/sigfp.Fingerprint); empty = unstamped (pre-B2 runs, or evidence
-- too short to name a failure). The shepherd's environment-delta predicate
-- joins stamps against stamps: a cohort sibling that merged after this run
-- escalated is evidence the class cleared; recent same-fingerprint
-- escalations mean the class is still active and relaunching is burn.
ALTER TABLE pipeline_runs ADD COLUMN failure_signature TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_pipeline_runs_failure_signature
    ON pipeline_runs(failure_signature)
    WHERE failure_signature <> '';

-- +goose StatementEnd

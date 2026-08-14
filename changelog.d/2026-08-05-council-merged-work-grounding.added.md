- **Mills: the council stops proposing work that already merged**
  (`pkg/mills/council/merged_work.go`, `pkg/mills/council/backlog_mutator.go`,
  `pkg/mills/textsim/textsim.go`, `pkg/mills/clients/council_merged_work.go`,
  `pkg/mills/policy.go`): the Drawing Office's brief is assembled before the
  tick's merges land, so it kept re-minting shipped work — three of five sparks
  on 2026-08-04 collided with just-merged MRs and burned their escalation
  attempts on empty diffs. `BacklogMutator.Apply` now grounds every candidate
  title against the merge requests GitLab reports merged in a 14d window, using
  the same `textsim` thresholds as dedup and the same two bands (hard, plus a
  gray band gated on a merge inside 7d). `textsim.NormalizeWorkTitle` strips the
  conventional-commit prefix and the `psl-…`/`— <slice-slug>` decoration first,
  without which a proposal scores 0.625 against the MR that shipped it.
  Suppressions record a distinct `merged_work_skip` guard action (subject
  `merge_request/!<iid>`) so the promotion report counts them apart from
  `dedup_skip`, and raise
  `mills_council_merged_work_skipped_total{band}`. A merged-MR fetch failure
  fails OPEN — proposals proceed ungrounded, warn logged,
  `mills_council_merged_work_errors_total` incremented — so a GitLab outage can
  never block the council. Policy-gated via `council.dedup.merged_work.enabled`
  (default true) and `lookback_hours`.

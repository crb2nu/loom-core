- **mills: revert-precise post-merge regression attribution** (`pkg/mills/reconciler_regression.go`,
  `pkg/mills/clients/gitlab_commits.go`, `cmd/loom-mills-operator/handlers_regression_attribution.go`):
  a periodic reconciler sweep joins the MRs merged in the last `168h` to the default
  branch's commits over the same window and records a regression only when a commit
  carries Git's canonical revert trailer (`This reverts commit <sha>`) naming a merged
  MR's landed commit. Matching accepts a full SHA or an unambiguous prefix of at least
  12 characters and refuses anything shorter or matching more than one merged MR;
  there is deliberately no file-overlap, timing, or similarity heuristic, because a
  wrong attribution teaches the factory a false lesson and every downstream consumer
  reads these events as fact. Each attribution is a first-writer event
  (`regression.attributed`, actor `reconciler.regression`, keyed on the regressed MR)
  carrying `regressed_mr_iid`, `merged_sha`, `revert_sha`, and `revert_title`, so
  repeated sweeps over an overlapping window converge instead of accumulating
  duplicates. The sweep is off unless a GitLab client is wired, rate-limited by
  `LOOM_MILLS_REGRESSION_SWEEP_INTERVAL` (Go duration, default `1h`), bounded by its
  own timeout, and kept out of `TickResult` so a GitLab outage on a read-only
  attribution pass never marks reconcile health errored.
  `GET /api/mills/regressions?window=<duration>` (default `336h`) lists what it
  attributed. Adds `mills_regression_attributions_total` and
  `mills_regression_sweep_errors_total{stage}` (`pkg/mills/metrics.go`), plus
  `GitLabClient.ListBranchCommits` and `MergeRequestListItem.LandedSHA()`, which
  resolves a merged MR's landed commit in the same preference order the merge path
  records (`merged_commit_sha` → `merge_commit_sha` → `squash_commit_sha` → `sha`).

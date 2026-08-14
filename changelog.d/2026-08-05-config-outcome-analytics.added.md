- **Mills: per-configuration win rates close the first learning loop**
  (`pkg/mills/guard/config_outcomes.go`,
  `cmd/loom-mills-operator/handlers_config_outcomes.go`,
  `pkg/mills/store/dao_pipeline.go`): the `run.provenance` stamps recorded which
  policy revision and stage-model mix each run started under, but nothing ever
  read them back against what those runs did, so "is this configuration better?"
  stayed an unanswerable question. `GET /api/mills/config-outcomes?window=336h`
  now joins the stamps to terminal outcomes, run cost, the run's own
  `judge.verdict` grades, and the `regression.attributed` reverts reachable
  through its merge request — grouped per `policy_checksum` and per (stage,
  model) pin with merge rate, mean judge score, judge pass rate, and cost per
  run. `ListTerminalOutcomesSince` carries `cost_usd` and `mr_iid` to make the
  cost and regression joins possible. Judge figures average per run so a retried
  gate cannot dominate a group; unstamped runs are counted as `uncovered_runs`
  and regressions whose MR predates the window as `unlinked` rather than
  attributed to a guess; a saturated window scan fails loud.

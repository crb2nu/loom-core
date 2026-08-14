- **Mills: learning-signal gauges for alerting** (`pkg/mills/metrics.go`,
  `pkg/mills/reconciler_learning_signals.go`, `pkg/mills/guard/learning_signals.go`,
  `cmd/loom-mills-operator/main.go`, `docs/MILLS.md`): judge calibration,
  promotion evidence, configuration outcomes and post-merge regressions were
  readable only as request-time JSON from the operator's report endpoints, so
  the alert pipeline could not see them — a gate's judge could drift for a
  fortnight and nothing would page. A new reconciler sweep (default `30m`,
  `LOOM_MILLS_LEARNING_SIGNAL_INTERVAL`; window `336h`,
  `LOOM_MILLS_LEARNING_SIGNAL_WINDOW`; off switch
  `LOOM_MILLS_LEARNING_SIGNAL_EXPORT`) republishes those same reports as
  `mills_judge_calibration_mean_score{gate,outcome}`,
  `mills_judge_calibration_discrimination{gate}`,
  `mills_judge_calibration_graded_runs{gate}`,
  `mills_promotion_evidence_actions{actor}`, `mills_config_outcome_merge_rate`,
  `mills_config_outcome_runs` and `mills_regressions_window_total`. It is a
  projection, not a second aggregation: `guard.LearningSignalExporter` calls the
  same builders the endpoints serve, wired from the operator because
  `pkg/mills/guard` imports `pkg/mills`. The headline consumer is a judge-drift
  alert — discrimination decaying toward `0` while `graded_runs` holds up means
  the judge no longer separates shipped work from escalated work. A mean with no
  observations publishes `NaN` rather than the report's `0`, so a threshold
  alert stays quiet on an empty window instead of firing on a fabricated zero,
  and a pass that cannot build all three reports publishes nothing at all —
  the gauges hold their last good values while
  `mills_learning_signal_export_errors_total{report}` marks them stale. Label
  sets stay closed: `policy_checksum` and judge model are deliberately not
  exported.

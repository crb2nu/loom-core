The learning-loop reports partition runs by their current VERDICT instead of
their frozen terminal class (Trustworthy Verdicts S2, `.loom/135`). A
superseded escalation — its MR merged via the ghost-spark sweep or green-MR
adoption — leaves the escalated bucket in both the config-outcome and
judge-calibration reports, so win rates, judge discrimination signals, and
per-configuration economics stop training on resolved incidents. Corrections
stay labeled, never silently folded: config outcomes gain a
`merged_after_escalation` partition column (counted as a win by `merge_rate`),
and judge calibration counts corrected verdicts on the merged side of the
discrimination signal with explicit `corrected_verdicts`/`corrected_runs`
fields. Correction events ride the reports' existing kind-filtered window
scan — both the explicit `run.verdict.*` events and the legacy ghost-spark
closure event, so history corrects retroactively.

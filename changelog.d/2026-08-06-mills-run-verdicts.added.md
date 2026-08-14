Give Mills runs a supersedable verdict (Trustworthy Verdicts S1,
`.loom/135`). A run's terminal row stays immutable history, but the VERDICT —
what we currently believe — resolves as the newest correction event on the
run's own event subject: ghost-spark closures (all three passes, green-MR
adoption included) now append an explicit first-writer
`run.verdict.ghost_spark_merged` event, and the resolver also recognizes the
pre-existing `reconciler.ghost_spark_closed` closure event, so every
escalation the sweep already rescued reads `merged_after_escalation`
retroactively with no backfill. The run detail endpoint's evidence block
carries the resolved verdict (`evidence.verdict`), distinct from the frozen
`EscalationClass`, labeled `merged_after_escalation` rather than silently
folded into merged. Downstream slices point reports, retry policy, and storm
detection at the verdict HEAD.

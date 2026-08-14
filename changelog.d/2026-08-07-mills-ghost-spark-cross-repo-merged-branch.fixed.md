Let the ghost-spark merged-branch pass adopt cross-repo items
(`pkg/mills/reconciler.go`, `pkg/mills/escalation_binding.go`). A cross-repo
item that escalated before the `mr` stage had no path back: the IID pass needs
`run.MRIID` (nil by definition), and the merged-branch pass was home-project
only, so even after the item's branch was hand-merged in the target repo it
stayed escalated forever unless closed by a manual backlog upsert — which also
skipped the run-verdict supersede, leaving the run reading `escalated
class=code` (the 2026-08-07 `services/procmodel` saga). Every pipeline
escalation now freezes the item's `target_project` onto the run's event
subject first-writer (`pipeline.run.escalation_target`, written by the runner,
the integrator, and the manual force-escalate endpoint); the merged-branch
pass authorizes a cross-repo lookup only against that immutable binding —
never the mutable `target_project` field — and only while the item still
targets the bound repo, then closes through the shared choke point so the
`run.verdict.ghost_spark_merged` supersede (`merged_after_escalation`) and
issue auto-close fire exactly as they do at home. Close and verdict events now
record the resolving `project` (MR IIDs are per-project), sweeps report a
`branch_binding_skipped` counter for fail-closed skips, and legacy escalations
without a binding, retargeted items, and unwired per-project clients all stay
untouched. Exact-branch-match and merge-newer-than-escalation guards and the
per-tick GitLab lookup budgets are unchanged.

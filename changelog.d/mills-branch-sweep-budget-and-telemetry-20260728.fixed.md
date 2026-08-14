Mills reconciler: give the merged-branch ghost-spark pass a reserved GitLab
budget, and per-pass telemetry.

The pass added in !1305 shared the IID pass's per-tick lookup counter. With
~100 escalated items the IID pass spends that allowance on nearly every tick,
so the branch pass returned immediately and closed nothing across 16
consecutive production reconciler ticks — the same starvation the IID pass's
own recheck cooldown exists to prevent, one level up. It now has its own
reserved budget; total GitLab calls per sweep stay bounded at the sum of the
two caps.

The sweep also reported nothing about the new pass, so a silent sweep was
ambiguous between "no escalated item lacks an MR IID", "candidates exist but
no branch merged", and "the pass never got to run" — which is what made the
first rollout undiagnosable from outside. `GhostSparkSweepResult` now carries
`BranchCandidates`, `BranchInspected` and `BranchMerged`, and the
`reconciler.ghost_spark_sweep` event emits them (and now fires when the branch
pass merely saw candidates, so a fully-cooled-down tick is still visible).

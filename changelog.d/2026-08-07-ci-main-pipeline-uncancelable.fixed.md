Exempt default-branch pipelines from auto-cancel-on-new-commit so deploys can
no longer be starved by merge traffic. On 2026-08-07 five consecutive main
pipelines (22296→22332) never reached their deploy stage: Mills factory merges
landed every ~10 minutes and `workflow.auto_cancel.on_new_commit: interruptible`
canceled each predecessor mid-validation, leaving the cluster on a 10+ hour old
operator/HUD image while the fixes sat merged on main. Making only the deploy
jobs non-interruptible cannot fix this — they are stage-gated behind lint/test,
so canceling an upstream validation job kills the not-yet-started deploy stage
anyway. Main pipelines now set `auto_cancel: on_new_commit: none` via a
per-ref `workflow:rules` override and run to completion; feature branches keep
the interruptible pruning, and the buildkit image jobs stay serialized per ref
through their `resource_group` with unchanged `changes:` gates.

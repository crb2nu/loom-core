The reconciler's ghost-spark sweep now merges an escalated run's merge request
itself when GitLab reports it open, conflict-free, mergeable and carrying a
green head pipeline — closing the backlog item with outcome `adopted_green_mr`.

This is the CI-infrastructure population. The sweep's two existing passes are
archaeology: they close items whose work already landed. This one finishes work
that never landed. On 2026-08-02 a LAN storm made GitLab kill jobs with
`runner_system_failure`; runs escalated as though the code were at fault, a
human retried the pipelines and they went green on unchanged code — and then
the MRs sat open and mergeable with nobody to press merge, because the run is
terminal and no stage owns it any more (!1390 and !1391 waited about seven
hours for a human).

Merging without a human is the mill's most consequential action, so every
ambiguous answer refuses: the MR must be exactly `opened`, `has_conflicts` must
be false, `detailed_merge_status` must be `mergeable` (so `ci_still_running` or
an unresolved review thread can never be merged out from under a reviewer), and
a head pipeline must exist and be `success` — a missing head pipeline is the
husk shape and is never treated as green. A refusal cools the item down rather
than re-probing every tick, and an MR that merges concurrently between the
check and the merge counts as success rather than an error.

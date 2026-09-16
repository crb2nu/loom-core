Accept a successful merge-request pipeline as merge proof for external
merge-queue candidates. `driveAwaitingPipeline` only polled BRANCH pipelines
on the source branch, but repos like `services/flexinfer` gate MRs on detached
merge-request pipelines and park their branch pipelines on blocking manual
jobs — a status that never turns terminal on its own but sat in the
keep-waiting case. On 2026-08-25 flexinfer MR !1004 (queue entry 207) had a
green MR pipeline the whole time, yet the queue minted a manual-parked
recovery branch pipeline at the 5-minute grace and evicted `ci_timeout` at
exactly 45 minutes; the merge had to be finished by hand. External candidates
now also resolve the newest `merge_request_event` pipeline for the head SHA:
success promotes to merging, and a live one holds the wait open without
minting a recovery branch pipeline underneath it. A red MR pipeline is
deliberately NOT eviction evidence — GitLab pins a spurious 0-job failed
merge-request placeholder on MR heads even in repos whose workflow rules
suppress MR pipelines, so a red there proves nothing and the candidate keeps
its normal timeout bound. Branch pipeline status `manual` is now treated like
`skipped` for all candidates: the create/timeout recovery path engages
immediately instead of waiting out the full 45-minute bound on a pipeline
that can never go green by itself.

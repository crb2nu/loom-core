- **Mills council**: the scheduler catches up a cron slot missed during a
  restart (an operator rollout at 17:59–18:01 on 2026-09-02 skipped the 18:00
  council outright): on start it fires the most recent slot inside a
  15-minute grace unless a run already covers it. Council run rows now carry
  the mutator's accounting in `Notes` (`mutator: created=… deduped=…
  merged_work_skipped=… plan_lane=…`), so a zero-yield run explains itself,
  and proposals routed to the plan lane count as backlog deltas
  (`plan:<id>`) instead of reading as a dry run in `council_yield`.

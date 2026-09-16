- **Mills overseers**: the S2 dry-run soak evidence is now actually recorded.
  `overseer.RecordDryRunDecision` / `store.RecordOverseerSoakDecision` had no
  callers, so `mills_overseer_soak_*` never moved, the persisted verdict
  could only fail closed, and the promotion runbook checklist was
  unsatisfiable for groomer/sentinel/foreman. Every dry-run action record now
  writes a would-have-acted decision, every dry-run tick that would act
  nothing writes a no-action decision, and `GET /api/mills/overseers` carries
  the projected verdict (`soak`: elapsed days, decisions, would-have-acted,
  divergences, promotable/fail_closed with reasons).

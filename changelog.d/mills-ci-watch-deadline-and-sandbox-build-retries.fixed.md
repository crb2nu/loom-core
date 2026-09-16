- **Mills pipeline**: `ci_watch` now recognises its own per-session deadline
  (sized from the GitLab client's 30m poll deadline) as a poll-session timeout
  and extends the watch, instead of surfacing the bare `context deadline
  exceeded` as a stage error. The stage deadline always fired a hair before the
  client's internal one, so every pipeline longer than 30 minutes ended attempt
  1 with `error: context deadline exceeded` and was retried as an anonymous
  transient; the watch-extension and hard-cap stall escalation path never ran
  in production (live 2026-09-02, fi-fhir pipeline 25391).
- **Mills pipeline**: a tests-stage gate call that gives up because the sandbox
  image is *still building* is now a free transient retry (bounded by the
  transient cap) instead of an infra failure that burned two of the three
  `MaxAttempts` on every cold-build day; a build that actually fails stays
  infra.

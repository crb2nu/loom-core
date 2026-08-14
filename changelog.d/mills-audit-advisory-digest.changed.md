- **Audit advisory findings now fold into a rolling per-day digest issue**
  (`pkg/mills/audit/followup.go`, `pkg/mills/clients/gitlab.go`): the audit
  follow-up writer previously opened one GitLab issue per sub-threshold
  (survival < 0.60) advisory finding, piling up dozens of low-signal issues.
  When the wired issue client supports it (production's GitLab client), findings
  now collapse into a single `Audit advisory digest — YYYY-MM-DD (UTC)` issue
  per day — one issue plus a comment per finding — matched via a stable
  `mills-audit-digest` body marker, mirroring the escalation dedup idiom.
  Auto-enabled via capability detection and fail-open; issuers that only support
  `CreateIssue` keep the legacy one-issue-per-finding behaviour.

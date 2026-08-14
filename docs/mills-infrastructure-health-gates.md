# Mills Infrastructure Health Gates

Mills pipeline autonomy now has an explicit infrastructure preflight decision:
the operator evaluates the MCP hub, GitLab, vector store, devbox substrate, and
other critical dependencies before a pipeline run starts or resumes.

The decision model is fail-closed. Missing evidence, stale evidence, no declared
critical dependencies, unknown critical state, or any critical dependency that is
not healthy blocks the run and records the reason. Non-critical dependency
failures remain visible to the HUD but do not block autonomy by themselves.

Blocked runs are escalated before worker dispatch, with a failure reason that
includes the health-gate verdict and any remediation hints. During
stability-first planning, remediation and infrastructure-health work is pulled
ahead of feature work so the council spends capacity restoring autonomy first.

Operator surfaces:

- `health_gates.allowed`: whether autonomous Mills pipeline work may proceed.
- `health_gates.fail_closed`: whether the block came from missing/unknown/stale
  evidence rather than a known degraded component.
- `health_gates.reasons`: human-readable block reasons.
- `health_gates.remediations`: suggested actions to recover the gate.
- HUD monitor, alerting, and fleet projections expose the same verdict.

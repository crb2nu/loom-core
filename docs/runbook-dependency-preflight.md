# Dependency preflight gate runbook

The `dependency_preflight` gate prevents live kill-tests from starting unless every declared external dependency has affirmative, fresh `healthy` evidence. It fails closed when a dependency is degraded, down, unknown, missing, stale, or cannot be probed.

## Invocation

Construct `gates.DependencyPreflight` with the complete dependency declaration, a `DependencyProber`, and the same freshness window used by the service health source. Register that configured gate in the workflow registry and evaluate `dependency_preflight` immediately before the live kill-test. Do not start the live test unless the returned outcome has `Pass: true` and the JSON verdict has `admitted: true`.

The package default registry exposes the gate name for lookup and configuration validation. Its unconfigured instance intentionally returns a degraded verdict, so omitted declarations cannot admit a test.

## Reading the verdict

`Outcome.Reasons[0]` is deterministic JSON with `schema_version`, `gate`, `status`, `admitted`, and a name-sorted `dependencies` array. Each dependency includes a bounded `state`, stable `reason_code`, evidence timestamp when available, and remediation. Only `dependency.healthy` admits execution. `dependency.degraded`, `dependency.down`, `dependency.unknown`, `dependency.missing`, `dependency.stale`, and `dependency.probe_error` all produce `status: "degraded"` and `admitted: false`.

Probe errors are deliberately recorded as dependency verdicts rather than gate execution errors. Invalid declarations, such as empty or duplicate names, are configuration errors and must be corrected before retrying.

## Recovery evidence

After remediation, rerun every probe. Recovery requires a `healthy` state whose `observed_at` timestamp is not in the future and is within the configured maximum age. Save the complete JSON verdict with the kill-test run record. Never reuse an earlier admitted verdict after its freshness window expires.

## Escalation

Escalate to the dependency owner when degraded or down evidence persists. Escalate missing or unknown states to the workflow owner to correct declarations and health mappings. Escalate probe errors to the platform owner with the dependency name and reason code; inspect credentials, routing, and the health source without copying secrets into the run record. Keep the kill-test parked until a fresh all-healthy verdict is recorded.

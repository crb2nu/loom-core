# Mills Escalation and Dependency Failures

Mills council output must separate repository work from dependency incidents.
When a run is blocked by GitLab, OpenAI, FlexInfer, Kubernetes, a registry,
storage, networking, or another outside service, the council should call that
out as an external dependency incident instead of turning it into speculative
repo remediation.

## Council Output Rules

- Label related backlog proposals with `external-dependency-incident`.
- Create backlog proposals only when the follow-up changes files in this
  repository, such as classifiers, retry policy, telemetry, docs, config, tests,
  or operator runbooks.
- Do not create proposals whose only action is to fix, restart, reconfigure,
  contact, or increase quota for an outside system.
- If there is no actionable in-repo follow-up, emit an empty proposal list with
  an `omit_reason`, for example `external dependency incident; no actionable
  in-repo follow-up`.

## Examples

Actionable in-repo follow-up:

```json
{
  "proposals": [
    {
      "title": "Classify transient GitLab 503s as dependency incidents",
      "labels": ["external-dependency-incident"],
      "slices": [
        {
          "name": "classifier",
          "goal": "Record GitLab 503 responses as dependency incidents instead of code defects.",
          "files": ["pkg/mills/clients/gitlab.go", "pkg/mills/clients/gitlab_test.go"]
        }
      ]
    }
  ]
}
```

No in-repo follow-up:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

The post-parse editor guardrail in `pkg/mills/council/editor_guardrails.go`
backs up the prompt contract by dropping external-only remediation proposals
that have no file-backed repository action. The guardrail treats no-file
proposals to remediate, restart, reconfigure, rotate credentials for, contact,
or increase quota on an outside dependency as non-actionable for this repo,
even when the proposal title omits the word "incident". File-backed follow-ups
using either council plan slices or legacy backlog slices are preserved and
labeled when the surrounding council output identifies an external dependency
incident.

## Substrate recovery releases

The reconciler treats a capability recovery as a separate release path from
ordinary auto-requeue. Health tracking records `substrate_red` when an incident
opens and `substrate_recovered` when it closes; the recovery event carries the
capability plus the inclusive `red_at`/`green_at` window.

On each tick, Mills requeues escalated items whose latest run:

- ended inside that window;
- has an eligible escalation class (`transient`, `infra`, or
  `transient_quota` by default); and
- has a failure signature mapped to the recovered capability.

Recovery releases do not consume the rolling 24-hour auto-requeue allowance or
the item's lifetime retry count. They still pass the pipeline run-budget and
concurrency admission check, enter the normal queued path (including scope
serialization), and are bounded by `release.max_per_sweep` (default 10). A
remaining pile is reconsidered on later ticks. While the latest health edge for
a mapped capability is red, ordinary auto-requeue leaves its victims parked so
it does not spend retry budget on a known-broken substrate.

Each successful release atomically records a pipeline-run event with
`released_by=substrate_recovered:<capability>` while preserving the run's
original escalation class. Configure the class and signature mapping under
`pipeline.auto_requeue.release`; set `enabled: false` there for the release-path
kill switch. Built-in signatures each name a distinct capability row:
`spawn_infra` releases on `hud_spawn` (spawn pod readiness), `sandbox_build`
and `mcp_hub_session` release on `mcp_hub_session` (the devbox quality gate is
reached through the hub session, and its outages surface there), and
`ci_poll_timeout` releases on `gitlab`. Custom mappings may name any capability
row exposed by the operator.

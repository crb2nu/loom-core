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

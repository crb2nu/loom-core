# Degraded-Mode Incident Response

Use this runbook when Loom, Mills, the HUD, or agent-context is still serving
some requests but a dependency incident makes one or more capabilities
unreliable. Degraded mode is not a normal retry loop. It is an operator state:
protect running work, stop wasteful automation, preserve evidence, and resume
only after the dependency boundary is clear.

Related runbooks:

- `docs/mills-incident-runbook.md` for Mills pipeline, plan, and gate triage.
- `docs/council-external-dependency-incidents.md` for council output rules.
- `docs/external-dependency-incidents.md` for the repository follow-up boundary.
- `docs/operational-fault-runbooks.md` for workspace, plan, and branch faults.

## Degraded-Mode Triggers

Declare degraded mode when any shared dependency failure can affect more than
one agent, pipeline, branch, or operator action:

- GitLab API, runner, registry, artifact, checkout, or MR operations are
  failing outside the branch diff.
- Kubernetes API, storage, DNS, TLS, ingress, or cluster networking is
  intermittently unavailable.
- Model provider, embedding, Qdrant, Langfuse, or another agent-context
  dependency returns repeated 5xx, auth, quota, or timeout errors.
- The HUD or daemon starts with a documented degraded subsystem, such as spawn
  recovery unavailable while read-only routes still serve.
- The first meaningful failure appears on `main`, unrelated branches, multiple
  Mills runs, or before repository scripts execute.

Do not declare degraded mode for a deterministic repository failure caused by
the active branch. Patch those as `repo-fix` or `ci-config-fix`.

## First Five Minutes

1. Stop new autonomous admission for the affected work family. For Mills, pause
   policy through GitOps before allowing more backlog items to start.
2. Leave safe read-only surfaces online. Do not restart healthy components only
   to make the status look clean.
3. Escalate or hold runs that would mutate state through the failing dependency:
   MR creation, merge, cleanup, spawn creation, image publish, deploy, or
   external API writes.
4. Capture the first meaningful failure before rerunning anything:

   ```bash
   git status --short --branch
   git diff --name-only origin/main...HEAD
   git log --oneline origin/main..HEAD
   kubectl logs -n loom-mills deploy/loom-mills-operator --tail=300
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | jq
   ```

5. For plan-linked work, resolve the live plan from the shared store:

   ```text
   agent_plan_get{plan_id:"<plan-id>"}
   ```

The plan store is canonical. Do not recover from a checked-out `.loom` mirror
or an old spawn prompt.

## Operator Expectations

During degraded mode, operators are expected to keep the system predictable:

- Name the degraded dependency, impacted capability, start time, and evidence
  source in the Mills escalation note, MR discussion, incident channel, or
  shift handoff.
- Keep successful read-only paths available when they do not risk data loss or
  duplicate writes.
- Fail closed for state-changing automation whose preconditions cannot be
  verified.
- Avoid unbounded retries. One bounded retry is allowed only after new recovery
  evidence exists or when the failure class is explicitly transient and narrow.
- Do not create repository backlog for outside-service work such as restarting
  GitLab runners, contacting a provider, increasing quota, rotating external
  credentials, or waiting for a status page.
- Create loom-core follow-up only for repository-owned classifiers, retry
  policy, telemetry, configuration, tests, or operator docs.

## Dependency Response Matrix

| Dependency | Degraded behavior | Operator action | Resume signal |
| --- | --- | --- | --- |
| GitLab API, MR, CI, runner, registry, artifact upload | Hold MR, merge, cleanup, image-publish, and `ci_watch` loops that depend on GitLab; keep local docs/code work scoped but do not claim recovery from a green unrelated job | Capture project, branch, MR IID, pipeline ID, job ID, runner ID, first failed job, and first useful error; compare with `main` or unrelated branches | GitLab status or owner recovery plus one bounded retry of the same stage succeeds |
| Kubernetes API, storage, DNS, TLS, ingress | Keep HUD/daemon read routes serving when possible; block spawn/deploy/reconcile actions that require the failing API | Capture namespace, resource, pod, node, event, endpoint, and client error; avoid deleting or recreating resources until object identity is known | API reads and the blocked mutation preflight both succeed with current resource versions |
| Model provider or judge backend | Pause gates that would spend repeated model calls; keep deterministic tests and local checks available | Capture model name, provider, request class, status/error, retry count, and whether deterministic tests passed | Provider recovery evidence plus one same-diff retry passes, or a documented tiebreaker resolves judge/test dissent |
| Agent-context, Qdrant, embeddings, session memory | Do not treat missing recall or context writes as proof that work is complete; preserve local evidence in the branch or MR | Capture tool name, namespace, agent/session IDs, storage endpoint, and whether reads or writes failed | Context reads and writes succeed for the affected namespace, or the work has an explicit no-context fallback note |
| HUD spawn recovery or Mills operator state | Keep status and read-only inspection routes serving; reject new spawns if durable state cannot be reconciled | Capture run ID, spawn ID, recovery error, policy state, lease state, and latest operator logs | Recovery loop clears degraded status and a fresh capability check reports `autonomy_ready=true` |

## Evidence Template

Use this shape in the escalation note or MR discussion:

```text
Disposition: external_dependency_incident
Degraded dependency: <GitLab runners | Kubernetes API | model provider | Qdrant | HUD spawn recovery>
Start window: <UTC timestamp or range>
Impacted capability: <ci_watch | merge | spawn | deploy | recall | judge gate>
Affected work: <backlog item, plan id, run id, branch, MR IID, pipeline id>
First meaningful error: <job/tool/stage and exact error summary>
Branch-diff check: <why the active diff did not introduce the failing dependency>
Current operator state: <paused | escalated | read-only serving | bounded retry pending>
Resume condition: <specific recovery evidence required before retry>
Repo follow-up: <none | classifier/retry/telemetry/config/tests/docs file list>
```

If there is no repository-owned follow-up, say that directly:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

## Retry And Resume Rules

Do not resume because time passed. Resume when the failed dependency has
specific recovery evidence and the affected stage can prove its preconditions.

Allowed:

- One bounded retry after a provider, GitLab, runner, registry, or Kubernetes
  owner reports recovery.
- One bounded retry when the same stage preflight now succeeds and the branch
  diff has not changed.
- Continuing read-only inspection while write paths remain blocked.

Not allowed:

- Repeated autonomous retries with the same first meaningful error.
- Retrying by changing unrelated branch files to make an external incident look
  like a repo fix.
- Marking a run recovered when only an unrelated job or branch succeeded.
- Opening a backlog item whose only resolution is outside this repository.

Before handing work back to automation, verify:

```bash
git diff --name-only origin/main...HEAD
git status --short
```

For branch-backed work, ensure the branch has an intentional pushed commit. For
docs-only incident follow-up, the diff should be limited to the runbook,
required changelog entry, and any intentionally changed docs tests.

## Closeout

Close degraded mode with one of these dispositions:

- `external_dependency_incident`: dependency evidence recorded; no repo-local
  follow-up required.
- `external_dependency_incident_with_follow_up`: dependency evidence recorded
  and a repository-owned follow-up landed for classifier, retry policy,
  telemetry, configuration, tests, or docs.
- `repo-fix`: investigation proved the active branch caused the failure.
- `branch-hygiene-fix`: missing push, empty MR, stale plan, wrong worktree, or
  unrelated staged files caused the operational symptom.

The closeout note must include the recovery evidence, retry result, and whether
autonomous admission was resumed. If the dependency remains unstable, keep the
affected work family paused and hand off the exact resume condition.

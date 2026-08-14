# CI Triage Runbook

Operator workflow for repeated GitLab pipeline failures on loom-core branches
and Mills-generated merge requests. Use this runbook when a branch fails more
than once, multiple branches fail in the same job family, or the Mills
`ci_watch` stage escalates without a clear repository fix.

The goal is to decide whether the failure is repository-owned, pipeline
configuration-owned, runner infrastructure, or an external dependency incident
before retrying or opening follow-up work.

## Triage Triggers

Start a triage record when any of these occur:

- The same job fails twice on the same branch after a clean retry.
- Three or more branches fail with the same job, image pull, network, service,
  or dependency error in a one-hour window.
- A Mills pipeline reaches `ci_watch` and escalates, stalls, or produces an
  empty MR after CI retry.
- A runner, registry, GitLab API, package mirror, Kubernetes, or external model
  provider error appears in otherwise unrelated MRs.

Do not keep retrying while the failure class is unknown. Capture evidence first,
then retry only when the class supports retry.

## Failure Classification

Use the first failing job as the primary signal, then check whether later jobs
are follow-on effects.

| Class | Signals | Owner | Next action |
|---|---|---|---|
| Repository test or build failure | Deterministic Go test failure, lint error, compile error, contract golden diff, or docs guardrail failure with file paths in the branch diff | loom-core change author | Fix the branch and rerun only the affected jobs |
| Pipeline configuration failure | YAML syntax error, missing CI variable introduced by this repo, wrong job rules, invalid image name from `.gitlab-ci.yml`, or guardrail script regression | loom-core maintainers | Patch CI config or scripts and add tests when possible |
| Runner or cluster infrastructure | Runner unavailable, pod scheduling failure, disk pressure, executor timeout before repo checkout, OOMKilled runner pod, or stuck job with no user script output | platform / runner operator | Pause autonomous retries, collect runner evidence, escalate infrastructure |
| External dependency incident | GitLab 5xx/API outage, registry outage, package proxy outage, DNS/TLS failures to outside services, rate limits, auth provider outage, model provider outage, or Kubernetes API outage outside the repo change | dependency owner / incident channel | Mark as external dependency incident; create repo work only for classifiers, retry policy, docs, telemetry, or config improvements |
| Flake or transient dependency | Non-deterministic test timeout, temporary connection reset, one-off image pull backoff, or single runner hiccup that succeeds on one retry | repository or platform, depending on recurrence | Retry once, then reclassify if it repeats |
| Scope or branch hygiene failure | Empty diff, wrong branch, stale plan, missing docs update, unrelated files staged, or MR opened without commits | Mills/operator workflow | Follow `docs/operational-fault-runbooks.md` before retrying |

When the evidence points to an outside service, do not create a backlog item
whose only remediation is "restart GitLab", "increase quota", "contact vendor",
or "fix the registry". See `docs/mills-escalation-and-dependency-failures.md`
for the council output contract.

## Preflight Checks

Run these before changing pipeline state:

```bash
git status --short --branch
git diff --name-only origin/main...HEAD
git log --oneline origin/main..HEAD
```

Confirm the branch is non-empty and contains only the intended slice plus any
required documentation updates. If the run came from a plan-linked backlog item,
resolve the canonical plan with `agent_plan_get` and compare the live slice
files with the branch diff.

Check local guardrails that commonly fail late in CI:

```bash
bash scripts/ci/check_docs_guardrails.sh
bash scripts/ci/check_docs_guardrails_test.sh
make ci-contracts
```

For Go-facing changes, run the narrow package tests first, then broaden:

```bash
go test ./pkg/<package>/...
go test ./cmd/<command>/...
go test ./...
```

For Mills-managed runs, capture operator state before retrying:

```bash
loom mills pipelines list # active/non-terminal runs
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | \
  jq '.[] | select(.State == "escalated")'
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/status" | jq
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | jq
```

If `autonomy_ready=false`, `repo_root` is red, or multiple active runs show the
same failing stage, pause new autonomous work before retrying CI.

## Evidence Collection

Record enough detail for a maintainer to reproduce the classification without
rerunning the job:

- Project, MR IID, branch name, pipeline ID, job ID, runner ID, and commit SHA.
- First failing job name, stage, exit code, and started/finished timestamps.
- The first meaningful error line and the 50 lines before it.
- Whether a manual retry passed or failed, with retry job ID.
- Diff summary from `git diff --stat origin/main...HEAD`.
- Relevant Mills run ID, backlog item ID, plan ID, slice name, and spawn ID.
- Any dependency status page, incident ID, Kubernetes event, or runner log that
  supports an infrastructure or external dependency classification.

Use bounded log reads so triage commands do not hang clients:

```bash
# GitLab job logs through the GitLab MCP or API are preferred when available.
# For cluster-side Mills evidence:
kubectl logs -n loom-mills deploy/loom-mills-operator --since=30m --tail=500
kubectl get events -A --sort-by=.lastTimestamp | tail -80
```

For a specific Mills pipeline run:

```bash
curl -sf -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>" | jq
```

Attach the evidence summary to the MR, the Mills escalation issue, or the
incident record. Include the classification and the reason a retry is or is not
safe.

## External Dependency Incident Check

Before filing repo remediation, check whether the same failure is visible
outside the branch:

1. Compare at least two unrelated failing pipelines or one failing pipeline
   against a recent successful `main` pipeline.
2. Check GitLab, registry, package mirror, model provider, and Kubernetes API
   status for the affected time window.
3. Look for common errors: `5xx`, `429`, `connection reset`, `TLS handshake
   timeout`, `temporary failure in name resolution`, `no healthy upstream`,
   `image pull backoff`, `runner system failure`, or API deadline exceeded.
4. Confirm the branch diff did not introduce the dependency endpoint, token
   name, image tag, or job rule that failed.
5. If the dependency is external, label the record
   `external-dependency-incident` and create in-repo follow-up only when it
   changes loom-core behavior, tests, telemetry, retry policy, or docs.

If the incident has no actionable in-repo follow-up, close the loop with an
evidence note instead of adding backlog work.

## Retry Policy

Retry only after the class is known:

- Repository or pipeline configuration failures: fix first, then rerun the
  failed job or push a new commit.
- Runner or cluster infrastructure: retry after the runner owner confirms
  recovery or after a fresh runner is available.
- External dependency incidents: retry after status recovery or when a bounded
  backoff window has elapsed.
- Flakes: retry once. If the same job fails again, treat it as repeated and
  reclassify.
- Branch hygiene failures: do not retry the pipeline until the branch diff,
  plan slice, and pushed commits are correct.

For Mills runs, prefer a fresh stage retry only when the branch and dependency
state are known-good. Force-escalate instead of retrying if the MR is empty, the
plan slice is stale, or the failure could affect multiple autonomous runs.

## Closeout

Every repeated-failure triage should end with one of these dispositions:

- `repo-fix`: commit or MR fixes loom-core code, tests, docs, or CI scripts.
- `ci-config-fix`: commit or MR fixes pipeline configuration.
- `runner-incident`: platform owner accepted runner or cluster remediation.
- `external-dependency-incident`: outside service incident recorded; repo work
  only if it improves classification, retries, telemetry, or documentation.
- `flake-retried`: single retry passed and no recurrence appeared in the next
  comparable pipeline window.
- `operator-escalated`: Mills run was paused or force-escalated for manual
  disposition.

Include the disposition, evidence links, and any follow-up issue or plan ID in
the MR discussion or Mills escalation note.

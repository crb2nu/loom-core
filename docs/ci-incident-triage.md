# CI Incident Triage Runbook

Use this runbook when GitLab pipelines fail repeatedly across unrelated
loom-core branches, Mills-generated merge requests, or plan slices. The goal is
incident classification: decide whether the repeated failure is caused by a
shared dependency, runner infrastructure, CI configuration, or the repository
diff before any broad retry loop resumes.

This runbook covers the three repeated-failure families that most often block
autonomous delivery:

- `ci/main` failures on default-branch or merge-request validation pipelines.
- Renovate merge requests that repeatedly fail dependency update validation.
- Image-build gates that block `loom-core`, `custom-server`, or
  `loom-mills-operator` image publication.

For a single-branch failure workflow, use `docs/ci-triage-runbook.md` first.
For Mills council escalation wording, see
`docs/mills-escalation-and-dependency-failures.md`.

## When To Open An Incident Triage

Open an incident triage record when any of these conditions are true:

- The same job fails twice on the same branch after a clean retry.
- The same failure signature appears on three or more unrelated branches in a
  one-hour window.
- A Mills run reaches `ci_watch`, escalates, or stalls without a repository
  diff that explains the failing job.
- Jobs fail before user scripts run because the runner, registry, GitLab API,
  package mirror, Kubernetes API, or model provider is unavailable.
- A previously green branch starts failing without new commits.

Do not keep retrying while the failure class is unknown. Capture evidence,
classify the failure, then choose the retry path that matches the class.

## Observed Failure Clusters

| Cluster | Common signals | Likely owner | Immediate action |
| --- | --- | --- | --- |
| Repository regression | Compile errors, deterministic Go test failures, lint failures, contract golden diffs, docs guardrail failures that point to files changed by the branch | Branch author or loom-core maintainer | Fix the diff, add or update tests if behavior changed, then rerun the failed job |
| CI configuration regression | `.gitlab-ci.yml` syntax errors, invalid job rules, missing variables introduced by the repo, broken guardrail scripts, invalid image names from repo config | loom-core maintainer | Patch CI config or script logic; verify with the narrow script/test before rerunning the pipeline |
| Runner or cluster incident | Runner unavailable, pod scheduling failures, disk pressure, executor system failure, OOMKilled runner pods, stuck jobs with no user script output | Platform or runner operator | Pause autonomous retries, collect runner evidence, escalate to the runner owner |
| External dependency incident | GitLab 5xx, registry outage, package proxy outage, DNS or TLS failures, 429 rate limits, auth provider errors, model provider outage, Kubernetes API errors outside the branch diff | Dependency owner or incident channel | Mark the triage as external; create repo work only for classifier, retry, telemetry, config, or docs improvements |
| Flake or transient dependency | One-off timeout, connection reset, image pull backoff, or runner hiccup that succeeds on one retry and does not recur elsewhere | Repository or platform depending on recurrence | Retry once; if it repeats, reclassify as an incident |
| Branch or plan hygiene | Empty MR, missing push, stale plan slice, wrong worktree, unrelated files staged, or missing required docs update | Mills/operator workflow | Fix branch state before retrying; verify the live plan and pushed commits |

Treat cross-branch correlation as a stronger signal than any single job log. If
unrelated branches fail in the same job family or before checkout, assume an
incident until the evidence proves the repository diff caused it.

## Critical Failure Families

Use this table to choose the first owner and evidence path for the common
pipeline families. The job family does not determine the class by itself; it
only determines which evidence to collect first.

| Family | First evidence to inspect | Repository-owned examples | External or platform examples |
| --- | --- | --- | --- |
| `ci/main` | First failed validation job, merge base, branch diff, docs guardrail output, contract golden diff | Go compile or test failure introduced by the branch; changed `.gitlab-ci.yml` rule broke a job; docs guardrail names changed code-facing files without a docs update | GitLab API 5xx before checkout; runner pod cannot start; package proxy or model provider unavailable across unrelated branches |
| Renovate MR | Updated dependency, lockfile diff, Renovate commit message, package registry response, affected package tests | New dependency version breaks API compatibility; generated lockfile or module metadata is inconsistent; repo policy rejects the update | Upstream registry 429/5xx; package tarball unavailable; checksum database or proxy outage; GitLab rate limit blocks Renovate branch updates |
| Image-build gate | Build job log before the first Dockerfile step, BuildKit pod events, registry push/pull response, changed image inputs | Dockerfile, build script, `.dockerignore`, frontend build, Go build, or image tag rule changed in the branch | BuildKit runner unavailable; base image pull fails; Harbor/GitLab registry outage; DNS/TLS/auth failure while pushing or pulling image layers |

For Renovate MRs, do not assume every failure is external. A dependency update
that compiles but fails a repository test is repository-owned until the failing
assertion is proven to be a test-environment dependency incident. Conversely,
registry, checksum, package-proxy, and GitLab API failures that affect multiple
Renovate branches are dependency incidents even though they appear on Renovate
MRs.

For image-build gates, separate build-input failures from publication failures.
If the Dockerfile, build script, frontend bundle, Go binary, or tag rule fails
inside the branch's changed inputs, fix the branch. If the job fails before
executing the build, while pulling a base image, while starting BuildKit, or
while pushing layers to the registry, classify it as runner, registry, or
external dependency until cross-branch evidence says otherwise.

## Observed Repository Incidents

Use these known clusters as examples when matching a new repeated failure:

| Observed cluster | Evidence pattern | Classification |
| --- | --- | --- |
| Harbor or DockerHub cache 401 on runner image pull | GitLab runner pod fails before user scripts; the same image path works with `ci-jobs/harbor-creds`; Harbor logs show upstream 401 or cache-miss revalidation | External dependency or registry cache incident, not a branch regression |
| Runner image or executor startup failure | Job never reaches checkout or project scripts; Kubernetes events show image pull, scheduling, disk, OOM, or executor system errors | Runner or cluster incident |
| Docs guardrail failure after code-facing edits | `guardrails:docs-cli` or `scripts/ci/check_docs_guardrails.sh` names changed files in the branch and no unrelated branch has the signature | Repository or CI configuration regression |
| Mills `ci_watch` escalation with no matching diff cause | The MR branch has the intended slice diff, but the first failing GitLab job points to GitLab API, registry, DNS, TLS, runner, or quota errors shared by other branches | External dependency or runner incident; pause autonomous retries |

Do not generalize from the example names alone. Match the timing, first failing
job, branch diff, and cross-branch recurrence before assigning the class.

## End-To-End Triage Path

1. Freeze the retry loop.
   Stop manual and autonomous retries for the affected job family until the
   failure class is known. For Mills runs, pause or force-escalate the run if
   the failure can affect multiple active branches.

2. Establish branch scope.
   Confirm whether the failing branch has a meaningful diff and whether the
   changed files can plausibly affect the failing job.

   ```bash
   git status --short --branch
   git diff --name-only origin/main...HEAD
   git log --oneline origin/main..HEAD
   ```

3. Resolve plan scope for Mills work.
   For plan-linked backlog items, fetch the live plan from the shared store and
   compare the slice files with the branch diff. The store is canonical.

   ```text
   agent_plan_get{plan_id:"<plan-id>"}
   ```

4. Find the first meaningful failure.
   Start with the first failing job in the pipeline, not the final downstream
   failure. Record the job name, stage, runner, exit code, timestamps, and the
   first error line plus surrounding context.

5. Correlate across pipelines.
   Compare the failure against a recent successful `main` pipeline and at least
   one unrelated branch if available. Matching infrastructure, GitLab, registry,
   DNS, TLS, or rate-limit errors across unrelated branches usually indicate an
   external or runner incident.

6. Hand off single-branch remediation when the branch diff is a plausible
   cause.
   Follow `docs/ci-triage-runbook.md` for the detailed branch workflow. Use
   narrow checks first, then broaden.

   ```bash
   bash scripts/ci/check_docs_guardrails.sh
   bash scripts/ci/check_docs_guardrails_test.sh
   make ci-contracts
   go test ./...
   ```

7. Classify and document the incident.
   Assign one failure cluster, write the evidence summary, and state whether a
   retry is allowed. If evidence is mixed, keep the incident open and escalate
   to the owner of the highest-risk shared dependency.

8. Apply the family-specific decision test.
   For `ci/main`, verify whether the first failing job maps to the branch diff
   or to a shared service. For Renovate, verify whether the dependency update
   itself breaks repository behavior or whether registry/GitLab/package
   infrastructure is failing. For image-build gates, verify whether the build
   input failed or whether runner, BuildKit, base-image, or registry operations
   failed outside the diff.

9. Retry or remediate.
   Retry only after the class is known and the owner confirms the retry
   condition. Repository and CI configuration regressions require a fix before
   rerun. External dependency and runner incidents require recovery evidence or
   a bounded backoff window.

## Evidence Checklist

Capture enough detail for another operator to reproduce the classification
without rerunning the failing job:

- Project path, MR IID, branch name, pipeline ID, job ID, runner ID, and commit
  SHA.
- First failing job name, stage, exit code, started timestamp, and finished
  timestamp.
- First meaningful error line and the 50 lines before it.
- Retry job ID and whether the retry passed or failed.
- `git diff --stat origin/main...HEAD` for branch-owned failures.
- Mills run ID, backlog item ID, plan ID, slice name, and spawn ID when the
  branch is Mills-managed.
- Renovate package name, current version, target version, package manager, and
  registry or package-proxy response when the branch is Renovate-managed.
- Image name, tag, Dockerfile path, BuildKit runner or pod, base-image source,
  and registry response when the failing job is an image-build gate.
- Dependency status page, incident ID, Kubernetes event, runner log, or GitLab
  status evidence for shared incidents.

Use bounded reads for logs so client tool calls do not time out:

```bash
kubectl logs -n loom-mills deploy/loom-mills-operator --since=30m --tail=500
kubectl get events -A --sort-by=.lastTimestamp | tail -80
```

For a Mills pipeline run:

```bash
curl -sf -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>" | jq
```

## External Dependency Decision Test

Classify the failure as an external dependency incident when all of these are
true:

- The branch diff did not introduce the endpoint, token name, image tag, job
  rule, or package that failed.
- The same signature appears on unrelated branches, on `main`, or before repo
  checkout.
- The error points to a shared service such as GitLab, container registry,
  package mirror, DNS, TLS, auth, Kubernetes API, or model provider.
- A retry without service recovery would only consume runner time or create
  duplicate failures.

When the incident has no actionable loom-core change, close with an evidence
note instead of adding repo backlog. In-repo follow-up is appropriate only when
it improves detection, retries, telemetry, guardrails, configuration, or this
documentation.

## Retry Policy

- Repository regressions: fix the branch, commit tests or docs as needed, then
  rerun the affected job.
- CI configuration regressions: patch the CI file or script, run the narrow
  script/test locally, then rerun the pipeline.
- Runner or cluster incidents: retry after the platform owner confirms runner
  recovery or a healthy runner pool is available.
- External dependency incidents: retry after the dependency status recovers or
  after a bounded backoff window with no new matching failures.
- Flakes: retry once. If the same job fails again, stop and reclassify.
- Branch or plan hygiene failures: do not retry until the branch has the
  intended commits, the live plan slice matches the diff, and the branch has
  been pushed.

## Closeout Dispositions

End every repeated-failure triage with exactly one disposition:

- `repo-fix`: a commit or MR fixes loom-core code, tests, docs, or contracts.
- `ci-config-fix`: a commit or MR fixes GitLab CI configuration or guardrail
  scripts.
- `runner-incident`: a platform owner accepted runner or Kubernetes remediation.
- `external-dependency-incident`: an outside service incident was recorded;
  repo work is limited to detection, retry, telemetry, configuration, or docs.
- `flake-retried`: one retry passed and no recurrence appeared in the next
  comparable pipeline window.
- `operator-escalated`: a Mills run was paused, force-escalated, or handed to a
  human operator because automated retry was unsafe.

Include the disposition, evidence links, retry decision, and follow-up issue or
plan ID in the MR discussion, Mills escalation note, or incident record.

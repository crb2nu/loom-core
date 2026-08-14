# Mills Incident Triage Runbook

Use this runbook when a Mills-managed branch, CI pipeline, or local operator
workflow fails and the first question is whether the fault belongs to this
repository, a shared external service, or the operator's local configuration.

The goal is to classify the incident before retrying, then follow the shortest
repo-local path to recovery. For CI-only detail, see
`docs/ci-triage-runbook.md` and `docs/ci-incident-triage.md`. For Mills
operations, see `docs/MILLS_RUNBOOK.md`.

## Start Here

Pause retries until the failure class is known. A repeated failed retry is
evidence, but an unbounded retry loop hides the first useful signal and can
block unrelated Mills work.

Capture the local branch and plan context first:

```bash
git status --short --branch
git diff --name-only origin/main...HEAD
git log --oneline origin/main..HEAD
```

For plan-linked backlog items, resolve the canonical plan from the shared store
and compare the live slice file list with the branch diff:

```text
agent_plan_get{plan_id:"<plan-id>"}
```

Do not rely on stale `.loom` mirrors when classifying a Mills incident.

## Failure Classes

| Class | Signals | Owner | First action |
| --- | --- | --- | --- |
| CI or repository regression | Compile, test, lint, contract, docs guardrail, or image-build failure points at files changed by the branch | Branch implementer | Fix the diff, run the narrow local check, commit, and rerun CI |
| CI configuration regression | `.gitlab-ci.yml`, guardrail script, job rule, image name, or required variable changed in this repo and broke pipeline setup | loom-core maintainer | Patch the CI/config change and add or update script tests when possible |
| External dependency incident | GitLab, registry, package mirror, DNS, TLS, Kubernetes API, runner, auth provider, model provider, or quota failure appears outside the branch diff or across unrelated branches | Dependency or platform owner | Stop autonomous retries, record evidence, retry only after recovery or bounded backoff |
| Local Mills configuration failure | Local CLI, daemon, proxy, tunnel, token, generated config, or repo checkout fails before remote CI owns the state | Operator on the affected workstation or daemon host | Rebuild, regenerate, sync, and verify daemon/local config before touching CI |
| Branch or plan hygiene failure | Empty MR, missing push, wrong branch, stale live plan, unrelated staged files, or missing required docs update | Mills/operator workflow | Correct branch state and pushed commits before retrying or opening an MR |

Use the first meaningful failure as the primary signal. Later errors are often
downstream effects, especially after a missing push, failed image pull, or
operator auth failure.

## CI Incident Path

Use this path when GitLab has a pipeline or job ID.

1. Find the first failed job, not the final downstream failure. Record the job
   name, stage, pipeline ID, job ID, runner ID, commit SHA, timestamps, and the
   first meaningful error line.
2. Compare the failure with the branch diff. If the failing package, script,
   Dockerfile, docs guardrail, or contract file changed in this branch, treat it
   as repository-owned until proven otherwise.
3. Run the narrow local check that matches the failing job:

   ```bash
   bash scripts/ci/check_docs_guardrails.sh
   bash scripts/ci/check_docs_guardrails_test.sh
   make ci-contracts
   go test ./...
   ```

4. If the job fails before checkout, before user scripts, during runner pod
   startup, or while pulling/pushing images, classify it as runner, registry, or
   external dependency unless the branch changed that CI input.
5. Retry only after the class supports retry. Repository and CI configuration
   failures need a branch fix first. Runner and external dependency incidents
   need recovery evidence or a bounded backoff window.

For image-build gates, separate build-input failures from publication failures.
Dockerfile, frontend, Go build, or tag-rule errors are repository-owned when
the branch changed those inputs. Base image pulls, BuildKit startup, registry
push failures, and DNS or TLS errors are shared-service incidents unless the
diff introduced the failing endpoint or image.

## External Dependency Path

Use this path when the error mentions a shared service or appears across
unrelated branches.

Classify the failure as an external dependency incident when all of these are
true:

- The branch diff did not introduce the endpoint, token name, image tag, job
  rule, package, model, or Kubernetes target that failed.
- The same signature appears on `main`, unrelated branches, multiple Mills
  runs, or before repository checkout.
- The error points to GitLab, a container registry, package mirror, DNS, TLS,
  auth, Kubernetes API, runner infrastructure, storage, or a model provider.
- Retrying immediately would only consume runner or operator capacity.

Capture enough evidence for another operator to confirm the classification:

```bash
kubectl get events -A --sort-by=.lastTimestamp | tail -80
kubectl logs -n loom-mills deploy/loom-mills-operator --since=30m --tail=500
```

For a Mills pipeline run:

```bash
curl -sf -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>" | jq
```

Create repo work only when the fix changes loom-core behavior, retry policy,
classification, telemetry, configuration, tests, or documentation. Do not open
repo backlog whose only remediation is to restart, reconfigure, contact, or
increase quota for an outside service. See
`docs/mills-escalation-and-dependency-failures.md` for the council output
contract.

## Local Configuration Path

Use this path when the failure happens before GitLab CI owns the branch, or when
the local CLI, daemon, proxy, token, tunnel, generated config, or checkout state
is suspect.

Verify the installed binaries and generated configs:

```bash
make build
./bin/loom generate configs --target all --hub-mode
for profile in vscode antigravity codex claude gemini kilocode; do
  ./bin/loom sync "$profile" --regen --hub-mode --all-projects --skip-worktrees
done
./bin/loom sync skills all
```

Check daemon and tunnel health:

```bash
./bin/loomd
curl http://localhost:9876/health
./bin/loom tunnel status
```

For an existing developer install, prefer the standard reload targets after
code changes:

```bash
make dev-upgrade
# or, when active proxy connections may be interrupted deliberately:
make dev-reload
```

Confirm these local prerequisites before escalating to CI:

- The current branch contains the intended slice diff and required docs update.
- The branch has at least one local commit and has been pushed to the remote.
- `loomd` is healthy and the proxy can reconnect after a daemon restart.
- `~/.config/loom/config.yaml` has the expected OTel endpoint if observability
  status is part of the incident.
- Required tokens for GitLab, GitHub, model providers, and Mills admin APIs are
  present in the environment or configured secret store.
- SSH tunnels are connected before testing remote Kubernetes workflows.

If local configuration was the cause, fix the generated config, token, tunnel,
or install state locally. Do not retry CI until the branch and pushed commits
are correct.

## Branch Closeout

Every Mills incident triage should end with one disposition:

- `repo-fix`: code, tests, docs, or CI scripts changed in this repository.
- `ci-config-fix`: pipeline configuration changed and the narrow check passed.
- `external-dependency-incident`: outside service incident recorded; repo work
  exists only for classifier, retry, telemetry, config, tests, or docs changes.
- `local-config-fix`: local daemon, proxy, token, tunnel, generated config, or
  checkout state fixed before retrying remote work.
- `branch-hygiene-fix`: branch, plan scope, staged files, changelog, or pushed
  commits corrected.
- `flake-retried`: one bounded retry passed and no recurrence appeared in the
  comparable pipeline window.

Include the disposition, evidence links, run IDs, plan ID, slice name, and any
follow-up issue or MR in the Mills escalation note or merge request discussion.

# Loom Mills — Operator Runbook

Day-2 procedures for `loom-mills-operator`. For architecture and policy reference, see `docs/MILLS.md`.

All `kubectl` commands assume `KUBECONFIG=~/workspace/platform/gitops/.kube/k3s.yaml` (or `kc-k3s` alias). Mac CLI examples use `LOOM_MILLS_OPERATOR_URL` (default `https://mills.flexinfer.ai`) for the REST + MCP listener; mutation examples also require an operator admin bearer token. The public ingress has a separate Cloudflare Access edge gate, authenticated with `LOOM_MILLS_CF_ACCESS_ID` / `LOOM_MILLS_CF_ACCESS_SECRET` (or the generic `CF_ACCESS_CLIENT_ID` / `CF_ACCESS_CLIENT_SECRET` fallback). The operator runs in namespace `loom-mills`. Lifecycle probes and Prometheus metrics live on a separate `:9090` listener and are not assumed to be exposed at the public REST URL.

## GitLab webhooks for `ci_watch`

`ci_watch` treats a per-poll cap while GitLab still reports `created`,
`pending`, or `running` as a free transient retry. The stage result durably
records the pipeline ID and first-observed timestamp; the next attempt polls
that exact pipeline ID instead of selecting newer branch work, so it does not
consume `pipeline.retry.max_attempts` or its generic transient cap. A separate
`pipeline.ci_watch.max_wall_clock_minutes` ceiling bounds the total watch (90
minutes by default). Resumed sessions use only the remaining allowance; a watch
that has already reached its ceiling escalates before polling again. A terminal `failed`/`canceled` pipeline escalates
immediately; ceiling and terminal escalation text includes the pipeline ID and
observed runtime. Merge-queue polling is unaffected.

Reattachment supports both branch pipelines and detached MR-head pipelines.
If a head-change rewind reruns the MR stage, that successful reauthorization
clears the old watch so CI can select the successor head's pipeline.

Set `LOOM_MILLS_GITLAB_WEBHOOK_SECRET` to a strong shared secret and
`LOOM_MILLS_OPERATOR_URL` to the externally reachable operator base URL. At
startup the operator idempotently ensures the configured GitLab project has a
Pipeline/Merge Request hook targeting
`$LOOM_MILLS_OPERATOR_URL/api/mills/hooks/gitlab`. The receiver does not accept
the operator admin bearer; it authenticates GitLab's `X-Gitlab-Token` with the
shared secret. Keep the endpoint behind TLS and do not log or place the secret
in URLs.

Webhook payloads are wake-up hints only. `ci_watch` always re-reads the MR and
pipeline through the GitLab API before deciding success or failure. While a
subscription is live its safety poll runs every five minutes; if either env var
is missing or registration fails, startup warns and the existing polling path
remains available. A bounded subscriber drops its oldest hint on overflow;
alert on growth of `mills_webhook_events_dropped_total` from the metrics
listener.

After deployment, confirm GitLab shows both Pipeline and Merge Request events
enabled for the hook, trigger several pipelines, and inspect the one-day stage
roll-up:

```bash
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/telemetry/stages?window=1d" |
  jq '.stages[] | select(.stage == "ci_watch")'
```

Webhook-enabled runs should bring `ci_watch` p50 below 120 seconds. If it does
not, check operator logs for the registration warning, GitLab hook delivery
history for 401/connection failures, and the overflow counter before changing
the five-minute fallback.

When a terminal pipeline has failed jobs, `ci_watch` treats it as transient CI
infrastructure only when every failed job has a runner-level GitLab
`failure_reason`: `runner_system_failure`, `job_execution_timeout`,
`stuck_or_timeout_failure`, `runner_unsupported`, `stale_schedule`, or
`scheduler_failure`. That path receives the bounded transient retry and is
recorded as `external_dependency.gitlab.ci_infrastructure`. A pipeline with any
`script_failure` or unknown reason remains code-classified, including pipelines
that mix script and runner-level failures. Job traces are not fetched for this
decision; the transient retry cap bounds a genuine job hang reported as
`job_execution_timeout`.

The `loom` client and bundled status snapshot resolve both auth layers automatically. Literal `curl` examples against the public URL assume equivalent `Authorization` and `CF-Access-*` headers are supplied by the operator; they are omitted below for readability.

## Quick status

```bash
loom mills status                                           # one-liner from Mac
kubectl get deploy,po,pvc -n loom-mills                     # cluster snapshot
kubectl logs -n loom-mills deploy/loom-mills-operator --tail=200
curl -sf $LOOM_MILLS_OPERATOR_URL/api/mills/capabilities | jq
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/kpis?window=1d" | jq
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/telemetry/stages?window=1d" | jq

# Optional lifecycle/raw Prometheus checks. Keep this port-forward running in
# another terminal; these routes are not on LOOM_MILLS_OPERATOR_URL.
kubectl -n loom-mills port-forward svc/loom-mills-operator 9090:9090
curl -sf http://127.0.0.1:9090/readyz                       # 200 once initialized
curl -sf http://127.0.0.1:9090/metrics | grep '^mills_'
```

`/readyz` on the metrics listener is service readiness: the HTTP process,
migrations, and policy manager are initialized. It is not an autonomy approval signal. Before enabling or
trusting unattended writes, check `/api/mills/status` or
`/api/mills/capabilities` and require `autonomy_ready=true`. When this is
false, the scheduler may continue ticking for observability, but the reconciler
will not start queued backlog items.

Common autonomy blockers are intentional fail-closed states: missing admin auth,
missing or unwritable `LOOM_MILLS_REPO_ROOT/.loom`, unwired FlexInfer/GitLab/HUD
spawn/MCP hub dependencies, NoOp dispatcher coverage on write stages, or fake
council participants. Read-only APIs may stay healthy while these blockers are
present.

Stage failure counters also retain deliberate fail-fast and spawn-stall harness
runs. Group the corresponding Loki rows by run/backlog ID before declaring a
real-work regression; those failures are evidence that a negative-path gate
fired. The `mills_autonomous_merges_real` KPI excludes heartbeat canaries, but
stage-attempt telemetry is intentionally unfiltered.

In k3s, `LOOM_MILLS_REPO_ROOT=/workspace/loom-core` is backed by the same
Longhorn RWO PVC as `/var/lib/loom-mills`. There is no initContainer, git-sync
sidecar, or CronJob: the checkout is created in-process at operator boot by
`ensureRepoRoot` (`cmd/loom-mills-operator/repo_root.go`) using the GitLab
project token. If credentials, network, `git`, or the checkout itself fail, the
pod should still become service-ready, but `/api/mills/capabilities` must show
`repo_root` red and `autonomy_ready=false`.

**Known gap — the checkout does not refresh.** `ensureRepoRoot` early-returns
when `repoRootReady` is satisfied, and that check only asserts that `.git`
exists and `.loom` is a writable directory. Once the PVC holds a good clone,
those are true forever, so the `fetch` / `checkout` / `merge --ff-only` block is
never reached on a healthy pod and the tree stays pinned at whatever commit it
was first cloned at. Accumulated untracked `.loom/*COUNCIL-*` artifacts would
also make `--ff-only` warn-and-skip even if it were reached.

For a tests-stage escalation containing `tests checkout infrastructure: pushed
head unresolvable for <branch>`, verify that the implement stage pushed the
named branch and that the operator token can read the item's target GitLab
project. This failure is deliberate: Mills will not run or pass an unpinned
quality gate.

Consequence for planning: the roadmap extractor fills `roadmap_intents` from
whatever `ROADMAP.md` is on the PVC, so a stale checkout produces stale-but-present
intents. The fail-closed preflight stays green while the *signal* is wrong, and
`eval` `roadmap_alignment` grades against it. To check freshness:

```bash
kubectl -n loom-mills exec deploy/loom-mills-operator -- \
  git -C /workspace/loom-core log -1 --format='%H %ci'
```

If that lags `origin/main`, restore freshness out-of-band (re-clone the PVC path
or roll the pod after clearing it) and file the `ensureRepoRoot` refresh fix.

## Pipeline-start recovery

A queued item is admitted with its run, workflow identity, capacity
reservation, transition ledger row, and dispatch intent in one SQLite
transaction. The starter runs only after that transaction commits. A pod crash
between those steps is self-healing: the next reconciler tick leases the
committed intent or recovers an acknowledged-but-still-queued top-level run and
drives the same run ID forward.

Use the outbox gauge and structured events to distinguish a brief restart drain
from a stuck starter:

```bash
curl -sf http://127.0.0.1:9090/metrics | \
  grep '^mills_pipeline_dispatch_outbox_pending '
kubectl logs -n loom-mills deploy/loom-mills-operator --since=30m | \
  grep -E 'reconciler\.(started|dispatch_dead_lettered|dispatch_ack_failed)'
```

The metrics command assumes the `:9090` port-forward from Quick status is
running. The gauge may rise briefly during a restart and should then fall. Delivery is
at-least-once by design, with the run ID as the starter idempotency key. Do not
delete outbox or reservation rows to force progress: lease fencing prevents a
late consumer from acknowledging another consumer's work, and manual deletion
can hide an unaccepted start.

After the bounded retry ceiling, Mills marks the intent `dead_letter`,
escalates the current run/backlog aggregate, synchronizes the workflow terminal
state, and releases the reservation atomically. Investigate the associated
`reconciler.dispatch_dead_lettered` event and starter configuration, then use
the normal human-resolution/requeue path. Obsolete intents from an older
aggregate are retired without escalating a newer run.

The operator remains a singleton. These recovery guarantees cover process
restart under SQLite WAL; they do not provide multi-replica worker fencing.

## Run provenance

Every run is stamped once at that same post-commit boundary with a
`run.provenance` event (actor `reconciler.provenance`), keyed
`subject_kind=pipeline_run` for the DAG lane and `workflow_run` for the
imperative lane. The payload carries `policy_checksum` (sha256 of the exact
policy bytes the active policy was parsed from — the same value as the
deployment's `loom.flexinfer.ai/policy-checksum` annotation when the pod is in
sync), `stage_models`, and `prompt_hashes`. It is the join key for per-version
win-rate and cost analysis: without it a merged run cannot be attributed to the
policy revision, model pins, or prompt templates that produced it.

The stamp records START-TIME INTENT. A mid-run swap — policy hot-reload, a
council fallback editor substituting for an unreachable remote backend — is
attributable from the per-dispatch `pipeline.stage.agent_routed` events
instead, not from the stamp.

```bash
curl -sf http://127.0.0.1:9090/metrics | grep '^mills_run_provenance_stamps_total'
```

`outcome="duplicate"` is crash-recovery replay re-reaching an already-stamped
run and is normal. A rising `outcome="error"` means runs are landing without
the join key; the operator logs `append run provenance stamp failed` with the
run id. Provenance is best-effort by design — a failed stamp never blocks a
dispatch — so this is an analytics-completeness alert, not an availability one.

## Post-review re-test (`tested_head`)

`pr_self_review` may push fix commits, and a review-authored commit is untested
by construction. `post_review_gate` therefore runs the deterministic
`tested_head` gate first: it compares the review's resulting branch head
(`pushed_commits.head_sha` on the `pr_self_review` stage row) with the
`tested_sha` artifact of the latest `tests` stage. An unchanged head passes and
the LLM judges run as usual. A moved head fails the gate with both SHAs and the
runner re-dispatches `tests` pinned to the review head: it does not re-run
`implement` or `pr_self_review`, and the re-test consumes no implement attempt.
The replacement verdict is the one `post_tests_gate` judges; on the way back the
review stage is skipped because it already ran for exactly that head. Either SHA
unresolved (legacy rows, no GitLab head resolution) records an advisory `skip`.

Evidence and knobs:

- `gate_outcomes` rows `tested_head` under `post_review_gate` (`fail` = re-test
  ordered) and the `pipeline.review_head.retest` /
  `pipeline.review_head.review_skipped` events on the run.
- `mills_pipeline_review_head_moved_total{outcome="retested|clean"}` counts how
  often review rewrites the tree.
- A run whose head still mismatches after two re-tests escalates with
  `[class=infra]` ("after N post-review re-tests"): the tests stage keeps
  verifying a revision other than the branch head, usually a push racing the
  pipeline. Check the branch, then requeue.

## Updating backlog items safely

`POST /api/mills/backlog` creates an item when its ID is new. Updating an
existing ID requires the `Revision` returned by the latest backlog GET/list
response. A stale write returns HTTP 409 with
`{"error":"stale-backlog-write"}`. Re-read the item, merge the intended
metadata change, and retry with the new `Revision`; do not blindly replay the old
body because it may overwrite a lifecycle transition.

## Pause and resume

The kill switch is `policy.enabled: false`. Edits to the mounted ConfigMap propagate via fsnotify within seconds; in-flight runs continue under their captured policy, but no new runs start.

### Pause

```bash
# Edit platform/gitops/k3s/mills/configmap-policy.yaml in the gitops repo:
#   enabled: false
# Then reconcile:
flux reconcile kustomization apps -n flux-system --with-source

# Wait for hot-reload signal in operator logs:
kubectl logs -n loom-mills deploy/loom-mills-operator --since=2m | grep "policy reloaded"

# Confirm the reconciler is parked:
loom mills status                # expected: enabled=false, queue_depth=0 (no new picks)
```

The reconciler exits cleanly within one tick (≤60s). The HTTP and metrics listeners stay up so HUD reads continue working.

### Resume

Reverse the edit (`enabled: true` or remove the field), reconcile, watch for the next `policy reloaded` log.

### Emergency pause without GitOps

Only when GitOps is unavailable (e.g., Flux is down). Patches the mounted ConfigMap directly; **must** be reverted by a GitOps commit afterwards or Flux will fight you.

```bash
kubectl patch configmap -n loom-mills loom-mills-policy \
  --type merge -p '{"data":{"policy.yaml":"<full yaml with enabled: false>"}}'
# Restore via git/Flux as soon as the issue is resolved.
```

## Force-escalate a stuck pipeline run

A run can wedge if a stage worker hangs (e.g., `ci_watch` waiting on a CI job that's been canceled). Force-escalation transitions the run to `escalated`, records a failure record in `events`, and (if configured) opens a GitLab issue + agent-context handoff.

```bash
# Find the run
loom mills pipelines list

# Escalate
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  $LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>/escalate

# Confirm
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | \
  jq '.[] | select(.State == "escalated")'
```

The reconciler will not auto-retry escalated items; a human must edit the linked YAML or close the GitLab issue with a deliberate `human-resolved` label to unblock.

### Escalation classes: substrate is never terminal on first sight

Two failures look like configuration but are substrate, and neither may escalate as terminal `config` on the first occurrence. (1) The autonomy circuit breaker's `mcp_hub_session` probe fails with `wait for call slot: context deadline exceeded` (hub contention — several runs plus the reconciler sharing one hub) or with `dial tcp` / `i/o timeout` / `connection refused` / `no such host` (hub down): the pipeline HOLDS the run in place (`pipeline.stage.held`, backoff 1→5 min inside `pipeline.autonomy_hold_minutes`, default 30; completed stages and the attempt count are untouched) and only when the window expires escalates as retryable `infra` (`… [class=infra] [reason_code=capability_red] (transient capability hold window expired)`) so auto-requeue picks it up. (2) A spawn's git-clone init container dies on a GitLab transport blip (`error: RPC failed; HTTP 502 …`, `fatal: expected flush after ref listing`, `fatal: early EOF`, `fatal: the remote end hung up unexpectedly`): the clone classifier reports `transient git-clone transport error cloning <repo> — retrying` and the stage retries free. Authentication, permission, schema, missing-repo and bad-ref failures still escalate terminal `config` immediately, and an exit-128 clone whose captured tail holds no git phrase keeps the generic config default ("no git message was captured").

### Outside-agent pickup and requeue

An outside agent may finish an escalated item's implementation before the item
is requeued. Commit and push that work to the exact source branch Mills derives
for the item, then requeue it. Before dispatching `implement`, Mills inspects
that origin branch against `main`. A nonempty diff is adopted as the successful
implement result and proceeds through the normal post-implement gates; no new
implement agent is spawned. The implement stage record contains
`adopted_branch`, `adopted_head_sha`, and `adopted_at` provenance.

Requeueing adopts whatever nonempty work currently exists on that branch,
including a stale prior attempt. Review the remote branch before requeueing;
the normal gates remain the safety boundary. If reimplementation is required,
delete or rename the remote item branch before requeueing so the probe observes
a missing branch and dispatches normally. There is currently no force-policy
flag.

Keep a conflict-free branch when the last escalation was the substrate rather
than the diff (class `infra`, or a tests verdict that "also fails against bare
main"): requeue with `POST /api/mills/pipeline/runs/<item-id>/start?requeue=1`
(or `mills_backlog_amend_scope … requeue=true force=true`) so the probe adopts
the existing work. Deleting such a branch buys a full re-implement for nothing
(2026-09-14: `bl-devbox-sandbox-quota-headroom-20260913` lost a 25-minute
implement this way). Delete or rename only when the diff itself is wrong.

The probe fails closed when origin, the base ref, or the diff cannot be read:
Mills records an implement error instead of risking an overwrite. A push racing
between the probe and a new spawn is still possible; avoid pushing to the item
branch after requeue begins.

### Slice branch ref collisions and husk branches

For a single-slice run, the canonical source branch is nested as
`<type>/<item-id>/<slice>`. A legacy pre-slice branch named exactly
`<type>/<item-id>` prevents git from creating that nested ref. The typical
symptom is one or more dash-flattened husk branches followed by repeated
`[branch_pushed.missing_ref]` failures.

Before a fresh `implement` spawn, Mills now checks all origin heads for strict
path-prefix collisions. It deletes an exact same-item legacy ref only after
confirming that no open merge request uses it, then records a
`branch_contract.ref_retired` run event and proceeds on the canonical branch.
Foreign collisions and legacy refs with open MRs fail before spawn with
`[branch_contract.ref_collision]`. If manual recovery is needed, delete the
legacy ref and any husks, seed the exact contract branch, and requeue the item.

## The imperative workflow lane

Most backlog items run the DAG pipeline (queued → plan_slice → … → merge), covered by the sections above. A backlog item can instead carry a `policy.workflow_template` selection (see "Workflow template registry (S7)" in `docs/mills-operational-guardrails.md`); admission then routes it through `ClaimWorkflowStart` into a **workflow run** — an imperative run driven by a `go.starlark.net` program from the compiled-in template registry, executed by the durable-journal runtime instead of the pipeline stage graph. These items get **no pipeline run at all**; everything about what happened lives in the workflow run and its step journal.

Key differences from a pipeline run worth knowing before you inspect one:

- The run's engine, template, template version, interpreter version, and clamped params are **frozen at start** and never re-resolved — editing the backlog item cannot re-route an in-flight run.
- Every terminal outcome — `done`, `error`, or `quarantined` — **escalates the backlog item and stops before merge**. v1 templates never open an MR or merge; a `done` run still needs a human to review and land the work product. Don't read `done` as "shipped."
- The run's work product lives on a deterministic branch named `mills-wf/<run-id>`, pushed by the run itself. There is no other record of the diff — the branch **is** the deliverable to review.

### Inspecting a run

**HUD — Workflows tab.** Under Mills, the Workflows panel lists every imperative run (`GET /api/mills/workflow/runs` under the hood, polled every 15s): run ID, engine, `template@version`, state, step count, backlog ID, cost, and start time. Click a row to open the step-timeline drawer, which shows each journaled step with a derived badge — `live` (ran a real effect this execution), `cache_hit` (replayed from the journal, no live effect), `pending` (interrupted mid-step), `failed`, or `quarantined` (the whole run halted on a nondeterminism/call-hash mismatch). The drawer also shows the interpreter version pin and the frozen selection params (content hash + clamped params/enums) when present.

If the run belongs to a backlog item, open that item's detail drawer instead (Backlog panel → row) to see the "Imperative workflow" section: the item's template selection, every run claimed for it, and — critically — the escalation attention banner once a run has settled terminal. The banner surfaces the run's outcome, a copy-to-clipboard for the `mills-wf/<run-id>` branch, and an explicit "pre-merge template — nothing was merged" reminder, so a reviewer triaging an escalated item doesn't have to cross-reference the Workflows tab manually.

**REST — `/api/mills/workflow/runs`.**

```bash
# List recent runs (default limit 50, max 200), newest first:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs?limit=50" | \
  jq '.runs[] | {id, backlog_id, template, template_version, state, step_count, cost_usd}'

# One run plus its full step journal (badges included):
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>" | jq
```

The list endpoint is a flat, cheap summary (no nested steps); the detail endpoint returns the run plus every journaled step, each with the same `live` / `cache_hit` / `pending` / `failed` / `quarantined` badge the HUD drawer shows. When the run carries a frozen S7 selection or canary identity, the detail response also includes a `workflow_params` field (opaque JSON — content hash + clamped params/enums) for inspecting exactly what identity the run is pinned to.

### Responding to a `workflow.terminal_settle` escalation

When a claim-started imperative run reaches any terminal state, the same transaction that commits its lifecycle CAS also releases its budget reservation, escalates the backlog item, and records a `workflow.terminal_settle` event on the item (`GET /api/mills/backlog/<id>` events, or the HUD attention banner described above). The event payload carries `run_id`, `final_state`, the `branch` (`mills-wf/<run-id>`), the `template`, and a human-readable `reason` string.

As the operator handling the escalation:

1. **Identify the run and its outcome** from the event payload or the HUD banner — `done`, `error`, or `quarantined`.
2. **Review the work product on `mills-wf/<run-id>`** regardless of outcome. A `done` state means the run's script completed without error, not that the diff is safe to ship — nothing has been merged, and there is no CI/MR gate to trust yet.
3. **For `error` or `quarantined`**, also check the step journal (`GET /api/mills/workflow/runs/<run_id>`) for the failing/quarantined step before deciding whether to retry. `quarantined` specifically means a step's re-derived `call_hash` didn't match the journal — treat it as a nondeterminism bug in the template or its inputs, not a transient failure to retry blindly.
4. **Disposition the item** the same way you would any escalated backlog item: merge the branch manually if the work product is good, edit the item and re-trigger if it needs rework, or close it out. There is no "resume to merge" action — the workflow lane stops pre-merge by design.
5. If the run is still live and clearly wedged rather than settled (no forward step progress, past `workflows.max_run_minutes`), use the pause/fail controls below instead of waiting — a wedged run is force-terminalized as `error` by the scheduler's wall-clock bound anyway, but you don't have to wait for it.

## Pause / resume / fail a workflow run

Imperative workflow runs (the durable-journal runtime behind `policy.workflows.enabled`) have their own lifecycle mutations, separate from pipeline escalation. Use these to stop a wedged run live — e.g. a zombie whose spawn died terminally (the 2026-07-09 wf-canary loop) — instead of waiting for a code deploy.

```bash
# Find the run
curl -sf $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs | jq '.runs[] | {id, state}'

# Pause (running → paused; takes effect between steps, on the next scheduler tick)
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>/pause

# Resume (paused → running)
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>/resume

# Fail (running|paused → error; TERMINAL — no un-fail. Optional audit reason.)
curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  -d '{"reason":"zombie spawn, pod gone"}' \
  $LOOM_MILLS_OPERATOR_URL/api/mills/workflow/runs/<run_id>/fail
```

Expected output: the run's lifecycle view, e.g. `{"id":"wf-canary-…","state":"paused","paused_at":"…"}`. An invalid transition returns `409` with the current state. Pause/fail are eventually-consistent like the policy kill switch: an in-flight step finishes its current attempt first; the scheduler skips the run from the next tick.

## Replay a council run

Useful when you want to verify a fix to the brief assembler, swap an ensemble member, or compare A/B outputs.

### Dry-run (safe)

Runs the full council pipeline against a scratch DB; nothing is committed and no GitLab mutations happen. The sidecar + plan paths are returned for inspection.

```bash
loom mills council dryrun
# Output: sidecar JSON path, .loom/<NN>-… draft paths, total cost
```

### Real replay with a different ensemble

```bash
# 1. Edit policy.council.ensemble in platform/gitops/k3s/mills/configmap-policy.yaml
#    e.g., swap editor.model from claude-opus to codex-gpt5.
# 2. flux reconcile.
# 3. Wait for "policy reloaded" log line.
# 4. Trigger:
loom mills council run

# 5. Inspect the new run:
loom mills eval list
# HUD: Mills → Eval panel; sort by created_at desc.
```

When the run lands, compare cost + Loop A score + downstream Loop B attribution against the previous run with the same brief content (same `roadmap_intents` snapshot).

V2.1 will add first-class A/B replay UI; until then, the manual flow above is the supported path.

## Council blocked on missing roadmap intents

Symptom: every council run terminalizes immediately with `outcome=error` and a
note containing `require_roadmap_intents`; no reviewer or editor spend occurs and
`cost_usd_approx` is `0`. The fail-closed intent preflight fired because the
canonical `roadmap_intents` store is empty (see "Fail-closed intent preflight" in
`docs/MILLS.md`).

```bash
# Confirm the store is empty and the brief was marked:
loom mills council runs --limit 5          # look for the blocked note
kubectl -n loom-mills exec deploy/loom-mills-operator -- \
  sqlite3 /var/lib/loom-mills/mills.db 'SELECT COUNT(*) FROM roadmap_intents;'
```

Fix the cause, not the symptom — the extractor fills the store from
`$LOOM_MILLS_REPO_ROOT/ROADMAP.md` on every run, so an empty store means that
file is missing, unreadable, or has no open `- [ ]` bullets under a plannable
section:

```bash
kubectl -n loom-mills exec deploy/loom-mills-operator -- \
  sh -c 'ls -l /workspace/loom-core/ROADMAP.md && grep -c "^- \[ \]" /workspace/loom-core/ROADMAP.md'
```

Break-glass only, when planning must proceed against an empty store: set
`council.require_roadmap_intents: false` in
`platform/gitops/k3s/mills/configmap-policy.yaml` **and** bump
`loom.flexinfer.ai/policy-checksum` in `platform/gitops/k3s/mills/deployment.yaml`
(fsnotify misses the `..data` symlink swap). Revert once the store refills.

Note: the block happens after admission, so each blocked tick consumes a
`council_runs` row. The council tier sets no `max_runs_per_day` today, so this is
harmless; if one is ever added, a wedged store could exhaust the daily allowance
and block the recovery run too.

## Sandbox drill overseer

The `sandbox_drill` sentinel periodically starts `concurrency` isolated devbox
quality gates at the same time (default 2, every 6 hours) and requires each to
return a complete verdict within `budget_seconds` (default 900). Configure it
under `overseers.sandbox_drill` with `enabled`, `interval_minutes`,
`budget_seconds`, `concurrency`, `project` (default `loom-core`), and
`cpu_throttling_ceiling_percent` (default 25). The master `overseers.enabled`
gate must also be on. Every gate uses its own `mills-drill-<n>` agent ID and is
stopped after the attempt, regardless of outcome.

A failed drill creates one deduplicated `mills-overseer` incident containing
per-gate wall time, exit codes, timeout text, and CPU-throttling evidence; the
next fully green drill closes it. `mills_sandbox_drill_seconds` classifies gate
attempts as `verdict`, `timeout`, or `error`, while
`mills_sandbox_drill_last_success_timestamp` advances only for a fully green
drill. A `verdict` outcome can still be unhealthy when the returned throttling
percentage exceeds the ceiling. If concurrent gates appear to stop or disrupt
one another, check for residual agent-ID truncation causing both IDs to select
the same sandbox pod.

## Audit a merged change

Find which council brief produced the slice that produced the merge:

```bash
# Get the pipeline run by MR iid:
curl -sf \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?mr_iid=<iid>"

# Eval Loop B attribution and Loop A score:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/eval/scores?limit=200" | \
  jq '.[] | select(.SubjectID == "<run_id>" or .SubjectID == "<council_run_id>")'
```

For deeper inspection, the `events` table has the full per-stage trace (slog rows are also written via JSON to stderr; aggregated in Loki).

## Serial merge queue

GitLab CE has no merge trains. Without the queue, parallel Mills branches go
stale while 17–28 minute pipelines run; main moves underneath them and their
merge PUTs die with `has_conflicts` (killed !1509 and !1511 on 2026-08-08),
and merges land in bursts that stack uncancelable main pipelines. The serial
queue (`pkg/mills/mergequeue`) guarantees every MR is CI-tested on the exact
main it lands on, one candidate at a time per `(project, target_branch)` lane.

### Semantics

When `merge_queue.enabled` is true, the pipeline's `merge` stage validates the
full `ci_watch` authorization once (including the #374 head-movement fence),
**enqueues** the candidate into the durable `merge_queue` table, and waits.
The processor drives only the head of each lane:

1. **Up to date** (MR `diff_refs.base_sha` == target tip): merge immediately —
   the run's own green CI is the proof.
2. **Behind**: request a rebase (a durable `mr_head_transitions` ledger row;
   cursors snapshotted before the PUT, verdict settled via the #374 observer),
   await a fresh branch pipeline on the rebased head, then merge with the
   rebased SHA as the precondition.
3. **Terminal settle**: `merged` wakes the waiting stage with the merged SHA;
   `evicted` fails the stage with a **typed** verdict and the run falls through
   to the runner. **The queue never retries internally** — but the runner now
   acts per reason: `head_moved` carries the observed successor SHA and rides
   the #374 head-movement rewind (re-gate + re-prove, no escalation), and an
   evicted run's next stage attempt re-enters the queue through the full
   enqueue-time authorization (one *active* candidate per run since migration
   031; the settled rows remain as audit history).

Eviction reasons and their error classes: `rebase_conflict`,
`rebase_ambiguous`, `ci_red` → code; `ci_timeout` → infra; `mr_closed` →
config; `head_moved` → rewind (see above); `merge_failed` → classified from
the GitLab error text (405/422/cannot-be-merged → config). An enqueue into a
full lane (depth ≥ `merge_queue.max_depth`, default 10) escalates immediately
with reason `queue_full` — backpressure surfaces instead of silently deepening
the queue.

The rebased-head pipeline wait is `merge_queue.await_pipeline_minutes`
(default 45, hot-reloaded). Set it above the observed runner-queue latency
before enabling `requeue_evictions`: on 2026-09-10 CI queues sat 60–90
minutes deep, every candidate evicted `ci_timeout`, and each re-adoption
rebased and launched another full pipeline — deepening the queue that caused
the timeout. Symptom in `/api/mills/merge-queue`: the same MR settling
`evicted ci_timeout` repeatedly while GitLab shows dozens of pending jobs.

Before any recovery API mint, the queue, `ci_watch`, and mrwatch shepherd look
for the newest exact `(source branch, head SHA)` pipeline from
`push|api|web|trigger`. Created, waiting-for-resource, preparing, pending,
running, and successful pipelines are adopted; failed or canceled pipelines do
not block a legitimate replacement. The shepherd must remain disabled until the mobile HUD image
containing this lookup and its per-head mint fence is deployed.

Duplicate signature: two active `source=api` pipelines with the same ref and
SHA, often only a few IDs apart. Confirm with the project pipelines API,
grouping results by `(ref, sha)` and checking `source`, `status`, and `id`.
During an incident, cancel only the newer API duplicate with
`POST /projects/:id/pipelines/:pipeline_id/cancel`; retain the older pipeline
that jobs and queue entries already reference. Do not bulk-cancel pipelines or
cancel a push pipeline. Attribution is available in INFO logs and in
`mills_pipeline_mints_total{minted_by}` /
`mills_pipeline_adoptions_total{minted_by}`.

Recovery and speculative pipelines minted by the queue carry pipeline-level
`MILLS_MERGE_QUEUE=1` and `MILLS_MERGE_QUEUE_MR=<iid>` variables. The queue
first adopts an eligible existing pipeline, so adopted pipelines remain
untagged and keep the ordinary CI resource profile. For queue-minted pipelines,
`lint`, `vet`, `test:unit`, `test:reliability`, and `build:binaries` request 3
CPUs with a 4-CPU limit; non-queue pipelines retain each job's existing sizing.
This changes neither runner tags nor the total runner slot count.

Durability: queue state and the rebase ledger live in the canonical SQLite
store, so an operator restart resumes the head candidate exactly where the
previous process died — re-observing, never re-mutating. The resumed merge
stage re-finds its entry FIRST and skips authorization re-validation (the
queue's own rebase legitimately advances the #374 fence).

### Rollout / rollback

The queue is a policy flip — no restart, no schema action:

```yaml
merge_queue:
  enabled: true      # default false
  max_depth: 10      # optional
```

Rolling back (`enabled: false` or removing the section) halts the processor on
its next tick without losing state; runs waiting in the merge stage observe
the disable and fall back to the direct merge (a queue-rebased candidate then
fails closed on the stale fence and escalates for a clean re-gate — expected).

### Observability

- `GET /api/mills/pipeline/runs/{id}` gains a `merge_queue` block (state,
  position, eviction reason) whenever the run has an entry.
- Prometheus: `mills_mergequeue_depth`, `mills_mergequeue_wait_seconds`,
  `mills_mergequeue_evictions_total{reason}`, `mills_mergequeue_merged_total`,
  `mills_merge_queue_pipeline_wall_seconds{lane}` (GitLab `duration`), and
  `mills_merge_queue_pipeline_queued_seconds{lane}` (GitLab
  `queued_duration`). The bounded lane is `queue` for queue-minted pipelines
  and `adopted` for pre-existing pipelines.
- Events: `mergequeue.merged` / `mergequeue.evicted` rows against the run.
- Stage wait bound: `LOOM_MILLS_MERGE_QUEUE_WAIT_MINUTES` (default 180) —
  the merge stage escalates if the queue produces no verdict in that window
  (dead processor / saturated queue).

## Shift-report finishing state

`GET /api/mills/shift-report` includes a `finishing` summary and a `deploy`
block on every bolt card. The narrative and Markdown contain a `Finishing:`
line whenever at least one filtered bolt is applicable. Sparks retain
`deploy: null`, and foreign-project bolts are `not_applicable` because the
operator build cannot confirm their rollout.

`pending` means the bolt's merge commit is not yet an ancestor of the running
operator `build_sha`. It normally lasts only for the image build and Flux
rollout (typically several minutes); persistent pending state means the image
or GitOps reconciliation should be inspected. `unknown` means the local git
ancestry check failed and does not prevent the report from being served.
The pending and unknown gauges update when a shift report is composed.

## Recover from a corrupted DB

The canonical SQLite DB lives on a Longhorn RWO PVC. WAL replay handles most operator restarts; nothing should be needed for a clean kill. The procedures below are for the rare cases where the DB is unrecoverable.

### Symptom: `database is locked` or schema check fails on boot

1. Capture diagnostics:

   ```bash
   kubectl logs -n loom-mills deploy/loom-mills-operator --previous --tail=500 > /tmp/mills-prev-logs.txt
   kubectl exec -n loom-mills deploy/loom-mills-operator -- sqlite3 /var/lib/loom-mills/state.db "PRAGMA integrity_check;"
   ```

2. If `integrity_check` returns anything other than `ok`, restore from the most recent nightly backup in MinIO:

   ```bash
   # List backups (nightly CronJob writes one per day):
   mc ls minio/loom-mills-backups/

   # Pick a backup to restore:
   BACKUP=2026-04-30T06-00-00.db

   # Scale operator to 0 to release the PVC:
   kubectl scale deploy -n loom-mills loom-mills-operator --replicas=0

   # Restore via a one-shot Job (the manifest lives at
   # platform/gitops/k3s/mills/jobs/restore-from-backup.yaml; render with the
   # chosen filename):
   kubectl create -n loom-mills -f - <<EOF
   apiVersion: batch/v1
   kind: Job
   metadata:
     generateName: mills-restore-
   spec:
     template:
       spec:
         restartPolicy: Never
         containers:
           - name: restore
             image: registry.harbor.lan/library/loom-mills-operator:stable
             command: ["/bin/sh", "-euxc"]
             args:
               - |
                 mc cp minio/loom-mills-backups/$BACKUP /var/lib/loom-mills/state.db
                 sqlite3 /var/lib/loom-mills/state.db 'PRAGMA integrity_check;'
             volumeMounts:
               - { name: state, mountPath: /var/lib/loom-mills }
         volumes:
           - name: state
             persistentVolumeClaim:
               claimName: mills-state
   EOF

   # Watch the restore Job to completion:
   kubectl logs -n loom-mills job/<job-name> -f

   # Bring the operator back up:
   kubectl scale deploy -n loom-mills loom-mills-operator --replicas=1
   kubectl rollout status deploy -n loom-mills loom-mills-operator
   ```

3. After restore, run a council dryrun to confirm the brief assembler still works against the restored canonical state. Then resume normal operation.

### Loss-window note

The nightly CronJob captures one snapshot per day. Items committed since the last snapshot are lost in a restore. Re-run the council to reproduce — `roadmap_intents` is idempotent and `.loom/backlog/*.yaml` is regenerated from the canonical store, so most state self-heals.

## Inspect what the operator is doing right now

```bash
# Active runs (council + pipeline):
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/status" | jq

# What gates fired in the last hour:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | jq

# What the budget enforcer thinks:
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/kpis?window=1d" | \
  jq '{SnapshotAt, Metrics: (.Metrics | {pipeline_cost_usd, gate_pass_rate, pipeline_merged_real, regression_rate})}'
```

## Production rollout staging

The default-on flip (slice 6.6) was landed for the in-binary policy default. Each cluster overlay still owns whether the mills is enabled in *its* ConfigMap. Stage rollouts in this order; never flip more than one environment per week.

### Stage 1 — Local

A developer can run the operator against a local SQLite file and a local policy YAML for the smallest possible smoke test:

```bash
make build/loom-mills-operator
mkdir -p /tmp/loom-mills
./bin/loom-mills-operator \
  --db-path     /tmp/loom-mills/state.db \
  --policy-path testdata/policy.yaml \
  --listen      :8090 \
  --metrics-addr :9090

curl -sf localhost:9090/healthz                            # 200
curl -sf -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  localhost:8090/api/mills/status | jq
```

Local runs use the FakeReviewer/FakeEditor (`cmd/loom-mills-operator/main.go: buildCouncilRunner`) when no FlexInfer or HUD spawn is configured; this is sufficient for handler smoke tests but does not exercise real model calls.

### Stage 2 — Dev cluster (kc-k3s)

```bash
# In the platform/gitops repo on a dev branch:
# Edit platform/gitops/k3s/mills/configmap-policy.yaml: enabled: true
# Commit, push, open MR, merge.

flux reconcile kustomization apps -n flux-system --with-source
kubectl rollout status deploy -n loom-mills loom-mills-operator
```

Soak for one week. Verify each acceptance criterion against the deployed cluster:

| Criterion | Check |
|---|---|
| Council dryrun produces sidecar + 3 markdown docs in <8 min for <$5 with eval ≥0.7 | `loom mills council dryrun` and inspect cost/score in HUD |
| End-to-end backlog → merged MR via fixture run | `loom mills backlog list --json | jq '.[] | select(.State == "merged")'` after seeding a fixture |
| Eval Loops A/B/C populated | `loom mills eval list` returns recent rows for all three subject kinds |
| Idle-throttle drops reconciler cadence | `loom mills status` shows `next_tick_in_s` ≥ 240 when queue is empty |
| HUD `Mills` view renders four panels | Visit `<HUD>/mills` |
| Backups produced nightly | `mc ls minio/loom-mills-backups/` shows last 7 entries |

### Stage 3 — Production cluster

Only after dev has been green for 7 consecutive days. Same edit + reconcile flow, but watch the regression KPI:

```bash
# Keep the :9090 port-forward from Quick status running, record the starting
# counter value, then watch for any increase during rollout.
curl -sf http://127.0.0.1:9090/metrics | grep '^mills_regression_count_total'
watch -n 30 'curl -sf http://127.0.0.1:9090/metrics | grep "^mills_regression_count_total"'
```

If `mills_regression_count_total` increases from the recorded baseline in the first 24h, pause and investigate before any further flips. This counter is Alertmanager-correlated; the REST `Metrics.regression_rate` field is a label-driven backlog KPI and is not an equivalent rollout alarm. The kill switch (`enabled: false`) is the safest and lowest-blast-radius rollback.

## GitLab issue importer (Slice 1a of .loom/43)

The operator can pull open issues labelled `mills-eligible` from its
configured GitLab project (`GITLAB_PROJECT` env) and create one
BacklogItem per fresh issue. Disabled by default; opt in via policy.

```yaml
intake:
  gitlab:
    enabled: true
    eligible_label: "mills-eligible"   # required label on the issue
    poll_interval_seconds: 300         # 5 min
    default_priority: "P2"             # fallback if no priority:Px label
```

**Label semantics on the issue side:**
- `mills-eligible` (required) — the importer's selector. Add this on
  any issue you want Mills to pick up.
- `priority:P0` / `priority:P1` / `priority:P2` / `priority:P3`
  (optional) — sets the BacklogItem priority. Highest priority wins
  when multiple are present. Default is `default_priority`.

**Dedup**: BacklogItem ID is `gl-<project_id>-<issue_iid>`, so the
importer is safe to re-run. Once an item exists, the importer does NOT
update it — reconciler/council state transitions (queued → running →
merged/escalated) are preserved across ticks.

**Verify**:
```bash
# After enabling, watch for the import line in operator logs:
kubectl -n loom-mills logs deploy/loom-mills-operator -f | \
  grep "gitlab importer"
# Check the backlog API for the new item:
loom mills backlog list | grep gl-
```

**Don't enable until pipelines are reaching `merge` stage.** Per the
2026-05-24 kill-test (`.loom/local/handoffs/mills-autonomy-killtest-...`)
100% of pipeline_runs were escalating at `tests` or `plan_slice`.
Adding more intake before Slices 2c + 2e land would just deepen the
escalation pile.

## Source-branch push before MR

The `mr` stage publishes the spawn agent's commits to origin before
calling GitLab `CreateMR`. Without this, GitLab accepts the MR row
but it points at a branch with no `head_sha`, and `ci_watch` hangs
forever waiting for a pipeline that can't exist.

There are TWO push paths (belt-and-suspenders):

1. **Spawn-side push (primary)**: the `implement` stage prompt
   explicitly instructs the agent to `git push -u origin HEAD` as
   its final step. The spawn pod has git credentials configured.
   This is the path that actually fires today because the single-
   repo SpawnWorker does NOT use Mills' WorktreeAllocator — so
   the operator doesn't know the worktree path and can't push
   from outside the pod.

2. **Operator-side push (fallback, currently inert)**: when
   `jc.Run.WorktreePath` is set, `GitLabWorker.runMR` runs the
   `clients.GitBranchPusher` before `CreateMR`. No live caller sets
   `WorktreePath` today, so this path is inert. A future slice should
   propagate the spawn-side WorkingDir back through SpawnResponse so
   the fallback kicks in for real.

- Implementation: `pkg/mills/clients/branch_pusher.go`
  (`clients.GitBranchPusher`) shells `git push --force-with-lease -u
  origin HEAD:<branch>` from the run's `WorktreePath`.
- Interface: `pipeline.BranchPusher` (4-line contract). The operator
  wires the production pusher onto `GitLabWorker.BranchPusher` at
  startup; tests inject fakes.
- Idempotency: `--force-with-lease` lets retries safely overwrite a
  stale prior push without clobbering unrelated upstream work. A
  no-op push (HEAD already at origin) exits 0.

Failure mode (push errors before MR is created): the `mr` stage
returns an error and the runner retries per the Slice 2c
classification — a transport / quota error gets free retries; a
real `git push` rejection counts against MaxAttempts.

## v2 rollout staging (preview)

When `.loom/94-implementation-plan-mills-v2-hierarchical-swarm-2026-05-02.md` Phase 8 lands, each v2 feature flips behind its own policy flag with a 1-week soak between flips. Order:

1. `policy.squads.enabled: true`
2. `policy.audit.enabled: true` (advisory-only by default)
3. `policy.council.debate.enabled.incident: true`
4. `policy.cross_repo.enabled: true` — gates per-item `TargetProject` routing to non-home repos (already flipped; see "Cross-repo demand sourcing" below)
5. `policy.adaptive_policy.enabled: true` (manual-apply only)

Detail per slice in `.loom/94-…2026-05-02.md` Phase 8. The rollback playbook for v2 lives in [MILLS_V2_ROLLBACK.md](MILLS_V2_ROLLBACK.md) — covers feature flag disable, policy proposal revert, and DB restore from MinIO.

## Useful loops

```bash
# Watch council runs as they complete:
watch -n 30 'loom mills eval list | head -10'

# Watch escalation rate:
watch -n 60 'kubectl logs -n loom-mills deploy/loom-mills-operator --since=10m | jq -r "select(.msg==\"escalation\") | [.time, .item_id] | @tsv"'

# Diff the live policy vs. git:
kubectl get configmap -n loom-mills loom-mills-policy -o jsonpath='{.data.policy\.yaml}' | \
  diff - platform/gitops/k3s/mills/configmap-policy.yaml
```

## Spawn pool capacity & multi-repo

Mills pipeline stages (plan_slice / research / implement) run as **spawn pods**.
The spawn orchestrator is embedded in **mobile-hud** (`internal/hud/spawn.go`),
*not* the operator — the operator only POSTs a spawn request to the HUD. So the
spawn knobs are env vars on the **mobile-hud** Deployment
(`k8s/base/servers/mobile-hud/deployment.yaml`), which deploys via the Flux
`loom-hub-servers` Kustomization. The gitops override patch
(`platform/gitops/clusters/k3s/flux-system/kustomization-loom-hub-servers.yaml`)
does **not** touch these, so the base-manifest values are authoritative.

| Env var | Meaning | Current |
|---|---|---|
| `SPAWN_MAX_CONCURRENT` | Global cap on active spawn pods (mobile-hud is a single replica, so this is a fleet-wide ceiling). | `10` |
| `SPAWN_MAX_CONCURRENT_BUILDS` | Concurrent spawn *image* builds (buildah); throttled independently so a higher run cap can't cause a build thundering herd. | `1` |
| `SPAWN_DEFAULT_CPU` / `SPAWN_DEFAULT_MEMORY_MB` | Per-spawn resource limits. | `0.5` / `4096` |
| `SPAWN_SYNC_MODE` | Workspace sync mode. `git-clone` = the pod clones the repo fresh (source of truth); the workspace mount is used only to fingerprint the repo for the Dockerfile. | `git-clone` |
| `SPAWN_GIT_BASE_URL` | Git base the pod clones from; rooted at the `services/` group. | `http://192.168.50.218/services` |
| `SPAWN_PROJECTS` | Comma-separated repos shown in the HUD/mobile spawn **picker**. Cosmetic — *not* an enforcement gate. | see manifest |

**Project labels.** Spawn requests keep the canonical project path (for example,
`services/loom-core`) in durable state, while the pod's `loom.dev/project` label
is normalized to a Kubernetes-safe, 63-character value. The label is for pod
discovery and degraded recovery only; clone and routing logic must continue to
use the canonical request value.

**Raising concurrency.** Cluster allocatable memory is ~1.25 TiB (16 nodes), so
even `SPAWN_MAX_CONCURRENT` at 10 (= 5 CPU + 40 GiB) is <4% of capacity. Capacity
is not the constraint — demand is. Bump the value in the base manifest and let
Flux roll mobile-hud; no gitops-patch change is needed.

**Expanding to more repos.** A stage targets a repo via the pipeline worker's
`Project` (defaults to `loom-core`). For a repo to spawn cleanly:

1. **Merge token reach** — the Mills GitLab worker uses the `services`-group
   token (`loom-mills-gitlab-group/api-token`, Maintainer on group `services`),
   so only `services/*` repos can be opened+merged. Cross-group repos (libs/,
   platform/) need a separate token grant.
2. **Git-clone group** — the pod clones `SPAWN_GIT_BASE_URL/<name>.git`, i.e. the
   `services/` group. Non-services repos would need a group-aware base URL.
3. **Workspace fingerprint (best-effort)** — `resolveProjectPath` looks for the
   repo on the `loom-hub-workspace` PVC (Longhorn **RWO**, mounted read-only) to
   fingerprint it for an accurate runtime image. In **git-clone mode** a repo
   that is *not* staged on the PVC no longer hard-fails: the orchestrator falls
   back to a lexical `services/<name>` path and a generic runtime image, and the
   init-container clone provides the real source. Staging the repo on the PVC
   still yields a better-fingerprinted image (correct language toolchain), so for
   build-heavy repos prefer to stage it. Populating/resizing that RWO PVC is a
   coordinated gitops op (single-writer; co-locate the writer with mobile-hud).

### Multi-repo demand activation (S6)

The steps above make a stage *able* to run against a `services/*` repo. **S6**
makes demand itself target foreign repos: the plan-slice emitter sources an
allowlist of non-home projects and stamps each emitted item's `TargetProject`,
so the S2/S3/S4 routing carries the whole run cross-repo. Activation is
**two-key** — both must be true or foreign demand stays inert:

1. **Execution key** — `policy.cross_repo.enabled: true`. Already flipped
   2026-07-05 for the keystone (flexdeck!244). This alone changes nothing about
   demand; it only lets a foreign-targeted item *execute* instead of being
   skipped fail-closed by the reconciler.
2. **Demand key** — `policy.cross_repo.demand_projects: [services/<repo>, …]`.
   The emitter consults this list ONLY when `enabled` is also true
   (`Policy.CrossRepoDemandProjects`), so a stray allowlist can never source
   foreign demand while execution is off.

**Procedure to onboard a repo (e.g. `services/flexdeck`):**

1. **Branch pipeline** — the target repo must run a pipeline on the MR's source
   branch (flexdeck did this via flexdeck!242) so `ci_watch` has a green gate to
   merge on. Without it the autonomous merge can't confirm success.
2. **Set the allowlist** — add the repo to `cross_repo.demand_projects` in the
   gitops policy ConfigMap (`platform/gitops/k3s/mills/…`).
3. **Roll the operator** — the emitter snapshots policy at startup and fsnotify
   misses ConfigMap `..data` swaps, so bump the deployment pod-checksum (or
   `rollout-restart`) to pick up the new allowlist.
4. **Author demand** — the emitter emits from **Plans scoped to the target
   project** that have a ready (`plan_slice_emitter.ready_phase`, default
   `pending`) slice **with declared files**, in the emitter's namespace
   (`plan_slice_emitter.namespace`). A foreign repo with no such Plan produces
   nothing; author a `services/<repo>` Plan with a ready slice to drive a run.
5. **Verify** — operator log `plan-slice emitter created backlog item … demand`,
   the item's pipeline runs against the target repo, an MR opens+merges **in the
   target repo**, and `mills_autonomous_merges_real` increments.

**Plan lifecycle scan contract.** The emitter queries only `planned` and
`in_progress` Plans before looking for pending slices. Take-up queries only
`planned`, `in_progress`, `in_review`, and `merging` Plans. Terminal plan bodies
remain durable in the Plan Store but are not transferred or decoded on every
poll. If `/api/mills/safety/quiescence` shows `plan_slice_emitter` or `takeup`
active across an entire poll interval, inspect agent-context latency and the
operator logs; do not widen either query to an unfiltered namespace scan.

**Rollback** — remove the repo from `demand_projects` (stops new foreign
demand) or set `cross_repo.enabled: false` (also fail-closes any already-queued
foreign item on the next reconciler tick), then roll the operator.

### Diagnosing a stuck merged-MR escalation

The ghost-spark sweep settles a merged MR by its durable `(project, MR IID)`
stamp. An external rebase may change the head SHA or actor after `ci_watch`;
those values do not invalidate a merged MR's identity. A restart runs an
immediate sweep, so a rollout of a settlement fix may close several previously
stuck items at once; the transition is CAS-safe and repeat sweeps are no-ops.

Start with the run's durable events instead of reconstructing a GitLab timeline.
For every rejected candidate the reconciler appends
`reconciler.ghost_spark_skipped` with a stable `precondition` and non-empty
`detail`:

- `cooldown`: the durable `recheck_after` has not elapsed.
- `project_provenance`: the MR project stamp is missing, malformed, or
  contradictory; do not guess a project from the current backlog target. A
  legacy empty escalation binding is interpreted as the configured home
  project only when the backlog item is itself a home-project item; the same
  binding on a foreign item remains fail-closed.
- `client_selection`: no GitLab client is configured for the stamped project.
- `candidate_exclusion`: the item has no MR-bearing run, no deterministic rescue
  branch, or the bounded pass lookup budget was exhausted.

Correlate the event by its `pipeline_run` subject (or `backlog_item` when no run
exists), then inspect `run`, `mr_iid`, `precondition`, and `detail`. A cooldown
or lookup-budget event should clear on a later pass. Provenance and client
selection are configuration/data faults and also produce
`reconciler.ghost_spark_failed`; repair the stamp or project-scoped client and
let the next sweep retry. A successful recovery emits
`reconciler.ghost_spark_closed` with `project`, `mr_iid`, and `outcome`.

`escalation_sweep_state` is not created or advanced for a
`project_provenance` skip because no GitLab lookup occurred; its
matching `reconciler.ghost_spark_failed` event is written once for the run and
MR IID. If the item remains stuck, repair the durable binding or home-project
configuration and expect the next sweep to retry immediately rather than wait
for `recheck_after`.

## See also

- [HUD fleet-roll unavailability](runbooks/hud-fleet-roll-unavailability.md)

## Sources

- `cmd/loom-mills-operator/main.go` (lifecycle, env vars, fakes vs. wired clients)
- `cmd/loom-mills-operator/auth.go` (admin token loading)
- `pkg/mills/policy.go` (`enabled`, `IsEnabled`, kill-switch semantics)
- `pkg/mills/policy_manager.go` (fsnotify hot-reload)
- `pkg/mills/reconciler.go` (one-tick exit on disable)
- `pkg/mills/scheduler.go` (cron + idle throttle)
- `pkg/mills/eval/{judge,outcome_attributor,council_roi,cross_run}.go` (Loops A/B/C)
- `pkg/mills/store/migrate.go` (migrations on boot; safe replay)
- `platform/gitops/k3s/mills/` (manifests; not in this repo — lives in the gitops repo)
- `docs/MILLS.md` (architecture + policy reference)
- `.loom/91-implementation-plan-agent-swarm-council-pipeline-2026-04-25.md` §"Default-off rollout"

## Factory Health Plane — detector replay (S0 kill-test)

`scripts/mills/health-replay.sh` reconstructs main-branch pipeline health over a
window using the Health Plane detector semantics (only terminal success/failed
pipelines flip health; skipped/canceled/manual never do) and prints every red
window with would-page markers. Read-only; requires `GITLAB_TOKEN`.

    GITLAB_TOKEN=... scripts/mills/health-replay.sh [days] [page_threshold_min]

Kill-test run 2026-08-29 (30 days, threshold 90m), gating
`plan-mills-factory-health-plane-2026-08`:

- 411 main pipelines: 246 success, 91 failed, 74 canceled.
- 40 red windows; **16 would-page** — every one genuinely red main (0 false
  fires; canceled/skipped never flipped state). Largest epochs: ~46h
  (08-23→25), ~18h (08-19→20), ~12h ×2 (08-12, 08-13), plus the 991m
  advisory window (GO-2026-6303) closed by !1762 at 08-29 13:39Z.
- Verdict: the detector assumption HOLDS. Expected page volume ≈16/month at
  the 90m threshold — tune threshold or quiet hours to taste.
- Productionizing notes for the S1 poller: use per-pipeline `finished_at`
  (list-API `updated_at` mutates on retry and can produce small negative
  window artifacts), dedupe to one page per window, and initialize every
  metric label combination at start (counter-born-nonzero guard).

## Rolling out a new model

Model ids are free-form in policy (`validModelToken` checks shape only); the
harness or API is the authority that rejects an unknown id at run time. What
the operator needs from *you* when a new tier ships (Claude Fable 5.1,
GPT-6 "astra", …) is, in order:

1. **Price it, or it is not merely $0 — it is a whole-reservation charge.**
   Council/spin cost lookups are exact-match. An unpriced model loses the
   per-attempt ceiling (`unpricedAttemptCeilingUSD`) and a single timed-out
   lens charges the entire run reservation. Add a row to
   `budgets.model_prices` in the gitops policy the day the id is known:

   ```yaml
   budgets:
     model_prices:
       gpt-6-astra:      { provider: openai,    input_per_million: 8,  cached_input_per_million: 0.8, output_per_million: 40 }
       claude-fable-5-1: { provider: anthropic, input_per_million: 10, cached_input_per_million: 1, cache_write_per_million: 12.5, output_per_million: 50 }
   ```

   Rows hot-reload with the policy and win over the compiled tables in
   `pkg/mills/clients/{council_anthropic,council_openai,council}.go`. Promote
   a confirmed price into the compiled table on the next image. Codex
   spawn-stage estimates use a separate table
   (`internal/hud/bridge/spawn_pricing.go`); add the row there too or the
   spawn cost is estimated at the gpt-5-codex fallback rate.
2. **Wire it where the policy already chooses models.** All of these are
   gitops edits in `k3s/mills/configmap-policy.yaml`:
   - Spinning Room: add a frame `{ name: <frame>, model: <id>, backend: anthropic|openai|flexinfer }`
     (hot-reloads, no checksum bump). A frame whose model the API rejects
     fails that spin loudly at the editor phase; prefer adding the frame
     alongside the current one rather than renaming it.
   - Council editor: `council.ensemble.editor: { model: <id>, backend: anthropic|openai }`;
     keep `editor_fallback_model` a *local* flexinfer id. The chain is
     editor → `editor_fallback` (the OTHER vendor's frontier model; an
     anthropic editor defaults to `{ model: gpt-5.5, backend: openai }`,
     `backend: none` opts out) → local flexinfer, so retargeting the editor
     to a new vendor usually means re-pinning `editor_fallback` to the old
     one. Needs the `loom.flexinfer.ai/policy-checksum` bump on the
     deployment.
   - Pipeline stages: `pipeline.stage_models.<plan_slice|implement|pr_self_review>: <id>`
     and/or an `agent_routing` rule `route: { agent: codex|claude-code, model: <id> }`.
     Re-targeting the *agent* in a rule drops the stage_models pin on purpose
     (`codex exec --model claude-…` fails every stage); set the model on the
     rule itself.
   - Reviewer lenses take `litellm` or `flexinfer` only. A frontier reviewer
     goes through the LiteLLM gateway as `oa/<id>` — which also needs the
     alias on the gateway side (platform/gitops litellm config).
3. **Check the harness version gate.** OpenAI gates new families behind a
   minimum Codex CLI (`gpt-5.6` needed codex ≥ 0.143, 400 "requires a newer
   version of Codex"). Bump `SPAWN_CODEX_VERSION` on the mobile-hud
   deployment before routing a new OpenAI tier through codex.
4. **Verify on `/api/mills/wiring`** (HUD → Mills → Overview): every
   surface's `{backend, model}` as the operator actually resolved it, plus
   `policy.policy_checksum`. Then watch the first run's stage records —
   `Model`/`Backend` land at pick end, `Artifacts.agent_routing.model`
   at routing — and the council run's `evidence.provenance.stage_models`.
5. **Both spawn paths honour the pin.** The legacy CLI path passes
   `--model` / `codex exec --model`; the SDK driver (`loom-spawn-driver`)
   accepts `--model` from the same request field as of this runbook
   revision. Older driver bundles ignore the flag (forward-compatible
   parser) and run the harness default — rebuild with
   `make sync-spawn-driver` after `npm run build` in `tools/spawn-driver`.

## Shift report: docs mirror freshness

`GET /api/mills/finishing/docs-mirror` returns the hourly cached J5 freshness
check, and `GET /api/mills/shift-report` includes the same result. Before the
first tick it is `unknown` with reason `not yet computed`. A drifted result's
missing plus stale count is the publish backlog; extra files are reported
separately. `mills_finishing_docs_mirror_drift_files` tracks missing plus stale
files, while `mills_finishing_docs_mirror_age_seconds` tracks the last mirror
commit age. Unknown checks leave the last meaningful gauges untouched.

To remediate, run `pnpm sync:loom-core-docs` in `services/flexinfer-site`,
review and commit the generated mirror, then use its normal pipeline. The
operator deliberately does not modify the site repository.

## Finished-goods digest

The operator composes a deterministic finished-goods digest once each UTC day
at `LOOM_MILLS_DIGEST_AT` (default `06:00`, `HH:MM` UTC; an invalid value
keeps the default). Each digest covers the previous UTC day and reuses the
shift-report composer (`GET /api/mills/shift-report?window=24h` ending at the
day boundary), so it is exactly the ledger a reader would have fetched at
00:00 UTC. The job follows the housekeeping boot-phase ordering: it starts
after the reconciler boot tick and its first compose is the next slot, never
the boot path, so a restart that straddles the slot leaves that day to the
admin rerun below. Each day is appended once as a `finishing.digest` event
(subject id `YYYY-MM-DD`, written on the dedicated ledger writer); after it
commits, the event is handed to the reconciler's digest and mobile push hooks
when they are configured. No LLM is involved.

Read a stored day at `GET /api/mills/finishing/digest?day=YYYY-MM-DD`, or omit
`day` for the newest; both are single-row index reads and answer 404 for a day
that was never composed. An admin
`POST /api/mills/finishing/digest/run?day=YYYY-MM-DD` (default: yesterday)
runs a day through the identical job path and is how a missed day is
backfilled: a day that has not ended yet is rejected with 400 (a partial
ledger must never become the day's digest), an existing day returns
`inserted: false` and delivers no hook twice, and a stored digest whose hook
delivery failed still returns 200 with `delivery_error` set. The HUD proxies
both routes. The `mills_finishing_digest_total{outcome}` counter records
`composed`, `skipped` (day already stored) and `error` (compose or append
failed, or the digest was stored but a hook rejected it); the scheduler logs
`finishing digest` warnings for failed days.

## Onboarding a repo or creating a project from the HUD

Work → Projects is the intake surface. A repo becomes a Mills project in two
halves, and the HUD shows which half is missing for every repo it lists:

| Half | Endpoint | Effect | Takes effect |
|------|----------|--------|--------------|
| Runtime registration | `POST /api/mills/projects/onboard` (`X-Admin-Token`) | row in the bootstrapped registry; the repo may source demand and resolves the *global* protected-paths list under the two-key gate (`cross_repo.enabled` + `allow_bootstrapped`) | immediately |
| Git policy | `POST /api/mills/projects/policy-mr` (operator admin token) | gitops MR adding the repo to `cross_repo.demand_projects` and, when asked, `intake.gitlab.projects`, `protected_paths_per_repo`, `per_repo_overrides`, plus the deployment `policy-checksum` bump | after merge + operator restart |

`GET /api/mills/projects` returns every repo the operator knows (home,
demand list, issue intake, runtime registry) with `ready`, `blockers`
(`cross_repo_disabled`, `not_in_demand`, `allow_bootstrapped_off`,
`protected_paths_unknown`) and `pending` (`git_policy_missing`) computed by
the same policy accessors the reconciler and gates use — so the chip in the
HUD is the operator's verdict, not a guess. A 404 from this route means the
deployed operator image predates intake: the HUD then labels every repo
`unknown`, keeps the forge half working (create/import), and disables the
register-only action rather than promising a registration that would fail.

Forge half (HUD daemon, `internal/hud/domain/forge`): identity comes from
`GITLAB_TOKEN`/`GITHUB_TOKEN` env or the daemon host's `glab`/`gh` logins
(`GET /api/forge/auth` shows which, never the token). **New project** creates
the GitLab project under a chosen group (libs/* default public, else
private), seeds README, a green `.gitlab-ci.yml`, AGENTS.md, ROADMAP.md,
CHANGELOG.md, `.loom/README.md` (+ go.mod/cmd/Makefile for the go template),
creates the GitHub repo and installs the push mirror — `dry_run: true`
returns the exact step plan first. **Onboard repo** from GitHub looks for a
GitLab twin by repo name (registry, then GitLab search) and defaults to
onboarding that twin; importing again would mint a duplicate. Only with no
twin (or on explicit choice) is the repo imported into GitLab with GitHub
kept as the mirror.

The policy MR is anchored text insertion (comments preserved), idempotent,
and fails closed on a missing anchor; the branch is
`mills/onboard-<slug>-<stamp>`. Merge it like any policy change — the
checksum bump restarts the operator, which re-snapshots `demand_projects`.
# Merge queue pipeline proofs and speculation

The merge queue treats a successful pipeline as proof of a Git tree, not only
of a commit SHA. It checks exact-SHA pipelines first and then project pipelines
whose commit `tree_id` equals the freshly rebased queue head. Missing commit or
tree data fails closed, and the normal rebase-conflict check still runs before
pipeline proof is considered.

Every accepted entry persists `detail.proof` (`pipeline_id`, `sha`, `tree`) and
`detail.proof_source` (`sha`, `tree`, or `speculative`). The same values appear
in the `mergequeue.merged` audit event. Use
`mills_merge_queue_proof_source_total` to compare proof sources.

`merge_queue.speculation_depth` is opt-in and defaults to `0` (off); `2` is the
recommended rollout value. It is bounded by `merge_queue.max_depth`. Operators
should watch `mills_merge_queue_speculation_total{outcome="hit|miss|cancelled"}`
and remove orphaned `mills-mq/spec-*` refs before increasing the depth.

## Poisoned Go module index on the shared sandbox cache

Symptom: lint/typecheck reports `open /gocache/gomod/.../go_below_19.go: no such file or directory`
even though `stat`, `cat`, and Go's `os.ReadFile` can read the file. The gate may
report that the same failure occurs against bare main. A module index created
during concurrent module extraction or a stale NFS view can retain the open
error in the shared build cache after the file becomes readable.

From the affected sandbox and repository, compare the same package with the
shared cache, a fresh build cache, and module indexing disabled:

```sh
pkg=github.com/modern-go/concurrent
fresh_cache=$(mktemp -d)
GOWORK=off GOMODCACHE=/gocache/gomod GOCACHE=/gocache/go-build GODEBUG=goindex=1 go list -e -json "$pkg"
GOWORK=off GOMODCACHE=/gocache/gomod GOCACHE="$fresh_cache" GODEBUG=goindex=1 go list -e -json "$pkg"
GOWORK=off GOMODCACHE=/gocache/gomod GOCACHE=/gocache/go-build GODEBUG=goindex=0 go list -e -json "$pkg"
rm -rf "$fresh_cache"
```

Inspect the JSON `Error`/`DepsErrors` fields (`-e` can exit successfully despite
package errors). Shared-cache failure with both other comparisons clean and the
file readable identifies the poisoned index observed on 2026-09-14.

For manual recovery, pause gates sharing the claim, locate the index data file
containing the exact error, and inspect the matches before deleting anything:

```sh
cache=/gocache/go-build
error='open /gocache/gomod/github.com/modern-go/concurrent@v0.0.0-20180306012644-bacd9c7ef1dd/go_below_19.go: no such file or directory'
find "$cache" -type f -name '*-d' -exec grep -aFl -- "$error" {} +
# Set this to a confirmed matching index data file from the output above.
index_data=/gocache/go-build/XX/CONFIRMED_OUTPUT_ID-d
output_id=$(basename "$index_data" -d)
find "$cache" -type f -name '*-a' -exec grep -Fl -- "$output_id" {} + |
while IFS= read -r action; do
    rm -f -- "$action"
done
rm -f -- "$index_data"
```

Alternatively, `GOCACHE=/gocache/go-build go clean -cache` clears the entire build
cache (all sharing pods will rebuild). Neither recipe needs to remove the module
download cache. Repeat the shared-cache diagnosis, then resume gates.

New sandbox pods with a shared cache claim set `GODEBUG=goindex=0`, preserving
unrelated settings, to prevent recurrence. Automatic detection and eviction are
out of scope; file a follow-up if this recurs after the environment change.

### Empty lint-parity output

`lint:parity` captures stdout and stderr together in a redacted, UTF-8-safe
8 KiB diagnostic tail, including baseline-oracle reruns. Populated findings
such as `noctx` remain lint verdicts and should guide code fixes.

A nonzero exit with no captured output is degraded infrastructure, carrying
`LintParityInfraWarning` and failure signature `lint_parity_no_output`.
Classification happens before command headers or synthetic diagnostics are
added. The tests stage retries this as infrastructure; inspect sandbox execution
and output transport instead of changing code without findings. An empty
baseline result likewise cannot establish that the change caused the failure.
Exit 127 (unavailable linter) and exit 7 with zero findings retain their
existing degraded behavior.

# Mills Ops — Reference

Backing references for `mills-ops`. Skill body lives in `mcp/context/skills-registry.yaml` (entry: `mills-ops`).

## Primary docs

- Architecture and policy reference: `docs/MILLS.md`
- Day-2 procedures (pause/resume, escalate, replay, recover, rollout): `docs/MILLS_RUNBOOK.md`
- Forward direction (Mills v2): `.loom/92-research-mills-v2-hierarchical-swarm-2026-05-02.md`, `.loom/93-product-spec-mills-v2-hierarchical-swarm-2026-05-02.md`, `.loom/94-implementation-plan-mills-v2-hierarchical-swarm-2026-05-02.md`

## Source files

- Operator binary: `cmd/loom-mills-operator/`
- Core packages: `pkg/mills/{store,council,pipeline,eval,gates,clients,policy*,reconciler,scheduler,budget,metrics}.go`
- HUD panels: `internal/hud/frontend/src/lib/components/Mills/{CouncilPanel,PipelinesPanel,BacklogPanel,EvalPanel}.svelte`
- Mac CLI: `cmd/loom/cmd_mills*.go`
- MentatLab DAG: `cmd/mcp-mentatlab/templates/mills-default-pipeline.yaml`
- GitOps manifests: `platform/gitops/k3s/mills/` (separate repo)

## Common commands

```bash
loom mills status
loom mills council dryrun
loom mills council run
loom mills backlog list --json | jq '.[] | select(.State == "queued")'
loom mills eval list --since=2026-07-12T00:00:00Z
loom mills pipelines list

# Terminal history (the CLI list intentionally shows active runs only)
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | jq

# Read-only cluster snapshot
bash mcp/skills/mills-ops/scripts/mills_status_snapshot.sh > /tmp/mills-status.md
```

## Kill switch

`policy.enabled: false` in `platform/gitops/k3s/mills/configmap-policy.yaml`. Reconcile with `flux reconcile kustomization apps -n flux-system --with-source`. Operator hot-reloads within seconds; in-flight runs continue under their captured policy. See `docs/MILLS_RUNBOOK.md` §"Pause and resume".

## Recovery from corrupted DB

Restore from MinIO `loom-mills-backups/` (nightly). See `docs/MILLS_RUNBOOK.md` §"Recover from a corrupted DB" for the full procedure.

## Eval framework

- Loop A — synchronous artifact judge inline at council run end (`pkg/mills/eval/judge.go`).
- Loop B — per-merge outcome attribution, async (`pkg/mills/eval/outcome_attributor.go`, `pkg/mills/eval/council_roi.go`).
- Loop C — weekly cross-run consistency, scheduled Sunday 0600 UTC (`pkg/mills/eval/cross_run.go`, `pkg/mills/eval/cross_run_scheduler.go`).

## Telemetry

- `LOOM_MILLS_OPERATOR_URL` is the REST + MCP base URL. Use its read-only
  `/api/mills/kpis?window=1d` and `/api/mills/telemetry/stages?window=1d`
  endpoints for remote diagnostics. `/healthz`, `/readyz`, and raw `/metrics`
  live on the separate `:9090` metrics listener; reach them through the
  cluster, a port-forward, or the configured Prometheus backend rather than
  appending those paths to the public REST URL.
- Public ingress authentication has two independent layers. Cloudflare Access
  uses `LOOM_MILLS_CF_ACCESS_ID` / `LOOM_MILLS_CF_ACCESS_SECRET` (generic env
  and shared-config fallbacks are supported); `LOOM_ADMIN_TOKEN` or
  `LOOM_MILLS_TOKEN` is the operator bearer token. Loopback calls omit the
  Cloudflare headers.
- Metrics namespace: `mills_*` (see `pkg/mills/metrics.go`). Key incident
  series are `mills_pipeline_stage_attempts_total`,
  `mills_pipeline_stage_error_class_total`, `mills_takeup_ticks_total`,
  `mills_plan_slice_emitter_ticks_total`, `mills_mcphub_calls_total`, and
  `mills_workflow_scheduler_ticks_total`.
- Grafana dashboard: `platform/gitops/monitoring/dashboards/mills.json`.

When the direct operator or Kubernetes API is unreachable, query the configured
Prometheus/Loki backends instead of treating missing `kubectl` access as missing
telemetry:

```promql
up{namespace="loom-mills"}
sum by (outcome) (mills_takeup_ticks_total)
sum by (stage,outcome) (mills_pipeline_stage_attempts_total)
sum by (stage,error_class) (mills_pipeline_stage_error_class_total)
sum by (outcome) (mills_plan_slice_emitter_ticks_total)
sum by (outcome) (mills_workflow_scheduler_ticks_total)
mills_autonomous_merges_real{window=~"1d|7d|30d"}
```

```logql
sum(count_over_time({namespace="loom-mills"} |= "take-up tick timed out" [6h]))
sum(count_over_time({namespace="loom-mills"} |= "plan-slice emitter" [6h]))
```

A timeout configured for 120s but observed near 600s is a harness/transport
failure, not merely a slow tool. Correlate `mills_mcphub_queue_wait_seconds`,
`mills_mcphub_transport_retries_total`, and gateway DNS/closed-connection logs.

Stage counters include deliberate fail-fast/spawn-stall drills. Before calling
an `error_class="code"` spike a product regression, group the matching logs by
run/backlog ID and separate explicitly launched harness runs from real demand.
`mills_autonomous_merges_real` removes heartbeat canaries from the merge KPI,
but the stage-attempt counters intentionally retain drill failures as evidence
that their gates fired.

## Rollout staging

Local → dev cluster (kc-k3s) → production. One-week soak between flips. Watch `mills_regression_count_total` after each flip; pause via `enabled: false` if non-zero in the first 24h.

V2 staged flips (when v2 ships): squads → audit (advisory) → debate (incident only) → cross-repo (after dogfood) → adaptive policy (manual-apply). Detail in `.loom/94-…2026-05-02.md` Phase 8.

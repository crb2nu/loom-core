# Loom Core Roadmap

> Last Updated: 2026-08-06
> Tier: 1 (see workspace AGENTS.md "Portfolio Tiers")
> Tracking Issue: https://gitlab.flexinfer.ai/services/loom-core/-/issues/239

<!--
Convention (portfolio-refresh 2026-H2, see libs/STANDARDS.md "Roadmap & Backlog"):
- This file states CURRENT TRUTH, derived from git activity and deployed state —
  never re-date stale content. Each refresh MR must cite its evidence (git-log
  window inspected, deploy-state query used).
- Backlog lives in GitLab issues (P1/P2/P3 labels + milestones), NOT in this file.
  This file links the backlog; it does not duplicate it.
- If a live plan exists in the agent-context plan store, reference its plan_id
  here; the store is canonical and this file is a rendered summary.
- The Mills council extracts unchecked bullets under Now/Next into
  roadmap_intents — bullets here are demand-sourcing inputs, phrase them as
  outcomes with their gates.
- Staleness SLO: Tier 1/2 repos must have this file dated within 90 days.
  `bin/portfolio-inventory --roadmaps` reports conformance.
-->

## Current Status

Loom Core is the production backend for the Loom MCP runtime: the `loom` CLI,
`loomd` daemon, aggregating proxy, the `cmd/mcp-*` Go server binaries, the HUD
web dashboard, the agent-context store, and the Mills autonomous-delivery
operator. Merges land daily. As of 2026-08-04 the Mills demand-sourcing loop is
closed end-to-end: the council's entire queued plan backlog was verified
shipped and the plan store reconciled (2026-08-04 session); the failure
classifier fails closed on external-dependency signatures with bounded
transient requeue and cost-by-class telemetry; dynamic workflows completed
their first production run and merged the work product (!1395). The Mill Staff
charter (`.loom/brainstorm-mill-staff-consolidation-2026-08-01.md`,
`docs/FACTORY_MODEL.md` §3) landed S0/S1/S3 and the `pkg/mills/guard`
substrate; the overseers ("the Alley") deployed 2026-08-01 in dry-run and the
soak is live (Prometheus `mills_overseer_ticks_total` / `_actions_total`
verified 2026-08-04 — note: zero groomer dedup verdicts yet, so the promotion
review must inspect dry-run events, not just the absence of false positives).
MR-awareness shipped M1/M2/M3b/M4 (registry, `agent_mr_status` + CLI, proxy
trailer, bounded shepherd) plus the mobile merge attention lane. The
agent-context embedder fallback path gained fail-closed degradation thresholds
and a fallback-ratio gauge (!1417).

Update 2026-08-05: the roadmap→intent loop is proven — overnight the council
consumed the 08-04 refresh and delivered 13 bolts (concurrency policy knob,
embedder Grafana panel+alert, config-gated OTel export, multi-repo intake
fail-closed ×2, ci-incident-classifier fixture+regression suites, langfuse
runbook; shift report 2026-08-05 13:51 UTC + `git log origin/main`). The
self-improving-factory waves 1+2 merged (9 MRs, !1417–!1426): plan-store truth
sweep + mrwatch merged signal, promotion evidence report, operator-override
labels, judge-verdict calibration, revert-precise regression attribution, and
run provenance stamps. Wave 3 (council merged-work grounding at authoring,
config-outcome analytics over provenance events, signature-candidate mining)
is in flight locally — deliberately not listed as intents below until merged,
to avoid duplicate proposals. Three overnight sparks were closed as
superseded-duplicate work (#468–#470) — the class wave 3's grounding slice
eliminates.

- **Plan store**: council demand-sourcing plans all reconciled to `merged`
  2026-08-04 except `plan-council-add-fail-closed-thresholds-…` (in review,
  !1417); `plan-loom-core-fleet-reliability-arch-20260710` (architecture);
  `plan-pattern-loom-mills` (slices A2/B1 merged; B2 + live e2e open)
- **Deployed**: k3s via Flux — `loom-mills-operator` + `loom-hub` (overseers
  dry-run since 2026-08-01); CLI/daemon/proxy local-first on developer machines
- **CI**: platform/gitops Go template family; **pipeline success required to
  merge**; flake quarantine + honest reruns live (!1409)

## Now

- [ ] Mill Staff S2 promotion — hold the ≥1-week overseer dry-run soak (live
  since 2026-08-01), then read `GET /api/mills/promotion-report` (shipped
  2026-08-05, explicit ZeroEvidence flag) for verdict volume and
  false-positive dedup verdicts; if evidence is zero, extend the soak or make
  an explicit low-evidence call before flipping `allow.dedup_close` in the
  gitops policy ConfigMap (policy-checksum bump required)
- [ ] Mill Staff S4 finish — unified "Mill Staff" HUD group (Drawing Office /
  Drawing-in / The Alley) with a recent-actions strip over
  `Events.ListByActorSince`; decide the squad-manifest advisory fields
  (enforce `budget_share`/`gates` at dispatch or remove them from the schema)
- [x] Mill Staff shed list (classifier half) — `council/ci_incident_classifier.go`
  verified and test-hardened in place (fixture + regression suites, landed
  autonomously 2026-08-05); kept, not deleted
- [x] Mill Staff shed list — unread council notification trigger implemented
  behind a flag (landed autonomously 2026-08-06)
- [ ] Pattern Loom close-out — run the live queued-proof kill-test (enqueue a
  synthetic widget stamp, verify reconciler pickup, pause before merge), wire
  live tools-manifest probing into stamp, land A2's merged-instance checkout
  into `repo_root`, finish B2's candidate→approved taste gate
- [ ] Pattern Loom cross-repo stamping — give stamps a target project
  (BacklogItem has no Project field; `target_dir` is a monorepo stopgap) so
  patterns can land outside loom-core
- [ ] MR-awareness remainder — the scripted kill-test (a) harness landed
  autonomously 2026-08-06; run it, then land M3a delta-gated mr-status
  injection via hook additionalContext (Claude/Gemini); close-out audit of
  the shipped M4 shepherd and M5 attention-lane surfacing against
  `product-spec-mr-awareness-2026-07-18.md`

## Next

- [ ] Autonomy throughput — the concurrency limit is now a policy field
  (landed autonomously 2026-08-05); raise it under the S4 supervisor +
  admission caps once S2 promotion evidence is in, watching the KPI snapshot
  and shift reports for regression
- [ ] Ops sweep — one-time bulk-close of pre-existing stale audit-advisory
  issues (#339–#355 class; new findings already fold into the daily digest)
- [ ] Semantic merged-work grounding — the lexical Jaccard band cannot see
  same-work-different-words duplicates (live 2026-08-06: `move-autonomy-
  concurrency-limit-into-policy` restated the just-merged `promote-pipeline-
  concurrency-limit-to-a-policy-field`, title similarity ≈0.27 vs the 0.55
  band; escalation #475, judge-corroborated). Extend the grounding with an
  embedding-assisted comparison behind the same policy flag and fail-open
  discipline, shadow-first per the promotion-evidence playbook
- [ ] Signature miner stop-phrases — the first mined candidate was a generic
  test-command fragment (`go test <path> <path>`); teach the n-gram picker a
  stop-phrase list so candidates name failure signatures, not tooling
- [ ] Promote the OpenRouter credit-exhaustion signature — provider 402
  "requires more credits" currently classifies as `code` (live: canary
  autopilot #477); once the miner surfaces it with shadow evidence, add it
  to the external-dependency classifier with the litellm/credits source tag
- [x] Embedder degradation observability — gauge shipped in !1417; Grafana
  panel + alert landed autonomously 2026-08-05
- [x] OTel trace export from daemon (#12) — config-gated export landed
  autonomously 2026-08-05
- [ ] Monolith splits — `internal/hud/spawn.go` (#168),
  `cmd/loom-mills-operator/main.go` (#169), `cmd_sync.go` (#170)
- [x] Mills multi-repo intake safety-rails — fail-closed on unlisted/unknown
  target repos and on classification errors, landed autonomously 2026-08-05

## Later

- Debate Mode stays parked (decision 2026-08-04): wire a real Moderator behind
  the default-off flags or delete ~600 LOC, when the Drawing Office needs
  multi-round quality
- Fleet S6 (hub child broker, clustered Postgres), S7 (MCP Tasks adapter) —
  gated on fleet-plan evidence
- Weaver S6-full/S7 registry, squad planner (gap map)
- gpt-5.6 terra/sol re-flip — gated on #347–#351 class fixes + codexVersion bump
- Off-LAN MCP-via-hub (Cloudflare Access); Zed `context_servers` surface + ICC
  tool-cap rebalance
- OpenAI Responses orchestration experimental track (#63)
- Enterprise RBAC/gateway hardening suite (#25–#29); Simplification EPICs 1–3
  (#65–#67)
- Mills harvester-vm substrate Phase B (curated base image, warm pool)

## Backlog

Full backlog: [P1 issues](https://gitlab.flexinfer.ai/services/loom-core/-/issues/?label_name%5B%5D=P1) ·
[P2](https://gitlab.flexinfer.ai/services/loom-core/-/issues/?label_name%5B%5D=P2) ·
[P3](https://gitlab.flexinfer.ai/services/loom-core/-/issues/?label_name%5B%5D=P3) ·
[Milestones](https://gitlab.flexinfer.ai/services/loom-core/-/milestones)

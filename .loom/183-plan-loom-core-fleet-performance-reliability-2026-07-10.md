# Loom Core Fleet Performance and Reliability Architecture

- **Plan ID**: `plan-loom-core-fleet-reliability-arch-20260710`
- **Phase**: planned
- **Project**: services/loom-core
- **Namespace**: loom-core/architecture/fleet-performance-reliability-2026-07
- **Created by**: codex-root
- **Created**: 2026-07-11T01:17:51Z
- **Updated**: 2026-07-11T01:22:14Z

> Rendered from the Loom plan store (canonical). Edit via `agent_plan_*` tools, not this file.

## Riskiest assumption + kill-test

**Assumption**: A shared set of ownership, admission, identity, and durability contracts can harden local and fleet paths without forcing the low-latency local fast path through the durable Mills control plane.

**Kill test**: Implement one vertical slice in local stdio and Mills/hub modes; run 1, 16, and 64 clients with transport resets, process death, store contention, cancellation, and retry storms. Reject if local warm p95 overhead regresses by >5 ms or >10%, resources/queues grow unbounded, route readiness remains green while calls fail, recovery exceeds 5 s local or 15 s hub, or any non-idempotent effect is duplicated.

**Status**: not_run

## Success criteria

- Test: Targeted daemon/mux/pool and Mills store/workflow suites pass under -race on every branch
- Test: Hermetic fake-hub and crash-point gauntlet runs without skipped scenarios
- Metric: 30-minute 25-client soak >=99.9% success with zero permanent wedges or misroutes
- Metric: 100 reconnect cycles settle within +5 goroutines and +5 MiB post-GC
- Metric: Mills crash matrix yields one logical result and zero duplicate effects
- Manual: Local and deployed canaries expose matching build/capability digests, honest degraded states, and a retained benchmark/fault report.

## Phase history

| From | To | At | Actor | Note |
|---|---|---|---|---|
| draft | planned | 2026-07-11T01:20:26Z | codex-root | Evidence-backed architecture synthesis complete; eight sequenced slices and kill thresholds recorded. |

## Spec

## Decision

Adopt a two-lane reliability kernel:

- A synchronous fast lane keeps ordinary local MCP calls storage-free.
- A durable execution lane handles long-running, resumable, or effect-heavy work for Mills, spawns, and future MCP task adapters.
- Both lanes share generation ownership, invocation identity, bounded admission, endpoint truth, capability identity, and telemetry contracts.

The immediate P0 is not a broad refactor: remove the contradictory hub mux cache from the fresh-per-Dial pool path, prove clean reconnect/resource ownership, and make the branch gauntlet mandatory.

## Goals

1. Contain one transport or server failure to its owning generation and affected calls.
2. Keep local warm-call latency within the stated budget while scaling concurrent agents.
3. Make overload explicit and bounded rather than allowing pool/heartbeat storms.
4. Make Mills admission, state transitions, worker ownership, cancellation, and external effects crash-safe.
5. Make health, build identity, capability schemas, telemetry, and CI report observable truth.
6. Provide a protocol-neutral execution substrate that can later project to negotiated MCP task extensions.

## Non-goals

- Rewriting all MCP servers or Mills workflows at once.
- Sending ordinary sub-second local calls through a durable database.
- Enabling multi-replica Mills before transaction, lease, fencing, and effect-adoption kill tests pass.
- Claiming literal exactly-once delivery from external systems.
- Implementing the legacy experimental MCP Task wire directly in storage.

## Load-bearing assumption and kill test

Assumption: one set of ownership, admission, identity, and durability contracts can harden local and fleet paths without forcing the local fast path through the durable Mills control plane.

Kill test: implement one vertical slice in local stdio and Mills/hub modes. Run 1, 16, and 64 clients while injecting transport resets, process death, store contention, cancellation, and retry storms. Reject the design if local warm p95 overhead regresses by more than 5 ms or 10 percent, whichever is larger; queues, sockets, goroutines, or memory are unbounded; route readiness remains green while calls fail; recovery exceeds 5 seconds locally or 15 seconds through hub; or any non-idempotent effect is duplicated.

## Architectural principles

1. One physical resource has one owner. Pools, muxes, clients, and supervisors may not each believe they own the same socket.
2. Every replaceable resource has a monotonic generation. Old failures cannot stop or mutate a newer generation.
3. Admission happens before expensive allocation. Queues and control traffic have explicit limits and fairness.
4. Replay requires proof. Read-only and idempotent operations may retry; uncertain mutations dedupe or return an ambiguous outcome.
5. Commit intent before dispatch. Durable work records claim, reservation, transition, and outbox intent atomically.
6. Worker ownership is fenced. A stale lease epoch cannot complete, merge, publish, or cancel.
7. Health means call readiness. Liveness, readiness, dependency degradation, circuit state, and idle state are separate.
8. Capability identity is data. Build SHA, dirty bit, config digest, protocol set, and tool-schema digest are visible and cache keys.
9. Local speed is a product constraint. The direct path adds no execution-store write unless durability is requested or required.

## Target contracts

### Endpoint generation

Key: server, target, generation.

Owns the physical transport topology, active-call leases, notification consumer, state transitions, and close path. States are Cold, Connecting, Ready, HalfOpen, Draining, and Closed. Replacement is singleflight and compare-and-swap by generation.

### Invocation

Carries invocation_id, request_hash, effect_class, agent/session/auth scope, deadline, and parent correlation through proxy, daemon, hub, and worker boundaries. Duplicate IDs with different hashes are rejected. A lost mutation response without a dedupe record becomes AMBIGUOUS_OUTCOME rather than an automatic replay.

### Admission and reservations

Bounded global, per-server, per-agent, and durable-worker queues use weighted fairness with aging plus a reserved control lane. Admission can reserve money, turns, wall time, spawn slots, CPU, and memory atomically. Overload is returned quickly with retry_after.

### Durable execution

A protocol-neutral record contains execution_id, kind, target binding, command hash, auth scope, desired/current state, aggregate version, lease owner/epoch/expiry, runtime deadline, result retention, immutable terminal result/error, and resource reservation linkage.

SQLite WAL remains the embedded backend. Clustered Postgres is added only after the same contract passes the two-worker fencing and effect-adoption kill test.

### Effect ledger and outbox

Each external effect is keyed by logical execution/stage/effect and stores request hash, intent, status, and adopted external resource identity. Spawn, MR create, merge, notification, audit, evaluation, and issue-close adapters reconcile uncertain outcomes. Outbox delivery is retried independently from the committed state transition.

### Capability and telemetry truth

Initialization and health expose build SHA, dirty state, startup/epoch ID, config/registry digest, protocol versions, extension capabilities, and tool-schema digest. Tool caches are keyed and invalidated by that identity. One endpoint state store feeds routing and health. One telemetry lifecycle owns traces and metrics, with bounded label cardinality and real export status.

## Phased sequence

### Phase A: stabilize and make regressions executable

- S0: mandatory hermetic branch reliability gate and baseline artifacts.
- S1: hub transport ownership correction plus generation-aware supervision.
- S2: Mills workflow invariant repair and transactional ClaimNext transition.

No later architecture slice ships until the relevant S0 gate is green.

### Phase B: bound and make work crash-safe

- S3: end-to-end invocation identity, replay policy, weighted admission, and reserved control capacity.
- S4: durable Execution, fenced leases, effect ledger/outbox, cancellation, and atomic reservations on SQLite.
- S5: unified endpoint health, build/capability ledger, honest telemetry, and diagnostic bundles.

### Phase C: scale and standardize

- S6: bounded hub child broker and clustered persistence/worker adapter. Multi-replica operation is gated by fencing, child-count, and resource soak tests.
- S7: versioned MCP task-extension adapter over Execution. Preserve old synchronous clients and the direct local path. Cross-repo sagas and other long-running features consume this substrate rather than creating another scheduler.

## Initial SLOs

- Local warm proxy overhead: p50 <=10 ms, p95 <=25 ms, p99 <=75 ms at 100 callers.
- Cold local initialization: p95 <=1 s and p99 <=3 s, excluding external authentication.
- Forced transport close: recovery p99 <=5 s, no daemon restart, at most one failed owning call.
- Hub synthetic no-op: >=99.9 percent success, p95 <=250 ms, p99 <=1 s.
- Thirty-minute 25-client soak: >=99.9 percent success, zero misroutes/deadlocks, post-GC RSS growth <10 percent.
- One hundred reconnects: settle within +5 goroutines and +5 MiB post-GC.
- Overload: return in <100 ms with retry_after and never exceed configured queues.
- Mills claim: 100 racers yield one run, one lease epoch, and one dispatch intent.
- Mills crash recovery: no duplicate spawn, MR, or merge; resume within one scheduler interval +30 s.
- Ten-thousand-item queue: claim p95 <100 ms and fewer than 20 SQL statements per tick.
- Branch performance gate: block >10 percent latency/time, >15 percent allocations, or >5 percent sustained resource growth.

## Validation and rollout

Per merge request, under ten minutes:

1. format, vet, lint;
2. hermetic core package tests with precise import-based exclusions only;
3. contracts and live OpenAPI handler conformance;
4. targeted race suites for daemon, muxstdio, Mills workflow/store/pipeline, and custom-server;
5. fake-hub close/reconnect/leak test that fails if scenarios skip;
6. sixty-second mixed load and resource-settle test.

Nightly adds fuzzing, repeated race, median-of-ten benchmark comparison, thirty-minute soak, and transport/process/SQLite/dependency/telemetry fault matrices.

Canary promotion records the exact build/config/schema digest, runs synthetic local and hub calls, periodically kills sockets/children/pods, simulates OTLP loss, and automatically rolls back on SLO burn. Promote only with zero permanent wedges, response misroutes, duplicate external effects, or leaked children.

## Workflow-pack impact

- Research: index readiness has separate population, embedding, and lexical-fallback states; Morph or other embedding overload automatically degrades to local lexical search.
- Technical writing: canonical plan-store IDs precede mirrors; evidence includes runtime build/capability identity.
- Test and ship: the branch gauntlet is required and produces versioned benchmark/fault artifacts.
- Troubleshooting: one bundle captures generation IDs, routes/circuits, pool/queue depth, active leases, build/config/schema digests, goroutine/heap data, and correlated invocation/execution traces.
- Coordination: agent TODOs, plan slices, Mills backlog state, and durable executions remain distinct but share stable correlation IDs. Presence and heartbeat traffic use the reserved control lane.

## Main risks and decision gates

1. Independent hub sockets may be too expensive at fleet scale. Fix ownership first; choose the shared broker only from measured socket/child/resource data.
2. A generic Execution abstraction may become too broad. Kill it early if SQLite and clustered adapters cannot share claim/heartbeat/complete semantics without slowing the direct path.
3. Central effect classification can drift. Prefer registry-declared defaults plus code-enforced overrides for high-risk tools.
4. Postgres and multi-replica operation add operational cost. Keep leader-elected singleton Mills until the demand and fencing proof justify them.
5. MCP task semantics are evolving. Keep versioned wire adapters outside storage and do not advertise capabilities before durable creation and auth isolation work.

## Sources

Detailed evidence and external primary sources are in `.loom/182-research-loom-core-fleet-architecture-2026-07-10.md`. Key implementation anchors include `internal/daemon/daemon_new.go:243-265`, `internal/daemon/muxcache.go:49-63,187`, `internal/daemon/callpipeline_routing.go:319-359`, `pkg/mills/reconciler.go:746-781`, `pkg/mills/store/store.go:73-125`, `.gitlab-ci.yml:555-717`, and `../../libs/mcp-go/websocket.go:477-500`.

## Slices

### 1. P0 — Hermetic branch reliability gauntlet — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#1`
- **Goal**: Make transport, daemon/proxy, Mills crash, race, and performance regressions unskippable before merge.
- **Files**: .gitlab-ci.yml, Makefile, scripts/ci/, internal/integration/, internal/daemon/, pkg/transport/muxstdio/, pkg/mills/, cmd/custom-server/
- **Branch**: codex/arch-fleet-s0-reliability-gate
- **Acceptance**: Branch CI uses precise dependency-based exclusions, runs the daemon/proxy fake-hub suite with LOOM_RUN_INTEGRATION, targeted race and crash tests cannot silently skip, and versioned benchmark comparison blocks the agreed regression thresholds within a ten-minute MR budget.

### 2. P0 — Transport ownership and generation supervisor — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#2`
- **Goal**: Give every local/hub transport and server process one generation-scoped owner so close, retry, reaping, and replacement cannot leak or ABA-kill healthy resources.
- **Files**: internal/daemon/daemon_new.go, internal/daemon/daemon.go, internal/daemon/muxcache.go, internal/daemon/callpipeline_routing.go, internal/daemon/callpipeline_errors.go, internal/daemon/daemon_loops.go, internal/daemon/daemon_lifecycle.go, internal/daemon/*_test.go
- **Branch**: codex/arch-fleet-s1-transport-generations
- **Depends on**: plan-loom-core-fleet-reliability-arch-20260710#1
- **Acceptance**: Fresh hub Dial transports are pool-owned with no hub mux cache alias; 25 cold calls open only the intended sockets; one forced close causes at most one owning-call failure and one coordinated replacement; 1,000 subsequent calls succeed; no pool exhaustion or closed-mux reuse; sockets, goroutines, and heap settle within bounds.

### 3. P0 — Mills transactional transition kernel — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#3`
- **Goal**: Make backlog claim, attempt allocation, run creation, budget reservation, workflow journal initialization, and dispatch intent one versioned SQLite transaction.
- **Files**: pkg/mills/reconciler.go, pkg/mills/store/dao_backlog.go, pkg/mills/store/dao_pipeline.go, pkg/mills/store/dao_workflow.go, pkg/mills/store/migrations/, pkg/mills/workflow/journal_dao.go, pkg/mills/workflow/killtest/, pkg/mills/**/*_test.go
- **Branch**: codex/arch-fleet-s2-mills-transactions
- **Depends on**: plan-loom-core-fleet-reliability-arch-20260710#1
- **Acceptance**: Immutable workflow metadata survives first append; 100 concurrent starts create one run and one dispatch intent; fault injection after every SQL statement leaves no dangling run, budget, or backlog state; stale versions update zero rows; each committed aggregate version has one transition record; the S1c dual-crash test reaches terminal state three consecutive times without duplicate spawn or zombie.

### 4. P1 — Invocation safety and bounded admission — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#4`
- **Goal**: Carry stable invocation identity and effect class end to end while enforcing fair bounded capacity with a reserved control lane.
- **Files**: cmd/loom/proxy.go, cmd/loom/proxy_transport.go, cmd/loom/proxy_handlers.go, internal/daemon/daemon_call.go, internal/daemon/config.go, internal/daemon/session.go, internal/daemon/daemon_transport.go, internal/daemon/callpipeline_*, pkg/mills/budget.go, pkg/mills/ranker.go, pkg/mills/pipeline/integrator.go
- **Branch**: codex/arch-fleet-s3-invocation-admission
- **Depends on**: plan-loom-core-fleet-reliability-arch-20260710#2, plan-loom-core-fleet-reliability-arch-20260710#3
- **Acceptance**: Uncertain mutating calls execute once or return AMBIGUOUS_OUTCOME; duplicate invocation IDs with matching hashes coalesce and mismatched hashes fail; 10k idle/hung clients plateau at configured connection/goroutine caps; a noisy agent cannot starve another or control traffic; overload returns within 100 ms with retry_after; fleet reservations never exceed cost or slot caps and lower priority work ages into service.

### 5. P1 — Durable Execution, fencing, effects, and cancellation — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#5`
- **Goal**: Create the protocol-neutral durable lane for Mills, spawns, and long-running operations with fenced ownership and recoverable external effects.
- **Files**: pkg/execution/, pkg/mills/store/migrations/, pkg/mills/workflow/, pkg/mills/pipeline/, pkg/mills/clients/, internal/spawn/, cmd/loom-mills-operator/
- **Branch**: codex/arch-fleet-s4-durable-execution
- **Depends on**: plan-loom-core-fleet-reliability-arch-20260710#3, plan-loom-core-fleet-reliability-arch-20260710#4
- **Acceptance**: Two workers racing 100 executions produce one active lease epoch each; takeover occurs within 15 seconds and stale-owner writes are rejected; crash before/after each spawn/MR/merge and DB commit adopts one external effect; outbox drains after restart; cancellation is durable, stops work within the substrate SLO, cannot be overwritten, and cleans resources; direct non-durable local calls perform no execution-store write.

### 6. P1 — Endpoint truth, capability ledger, and telemetry — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#6`
- **Goal**: Unify route and monitor health and make every operational claim attributable to an exact build/config/schema identity and exported signal.
- **Files**: internal/daemon/health.go, internal/daemon/daemon_dispatch_status.go, internal/daemon/daemon_dispatch_otel.go, internal/daemon/daemon_loops.go, internal/daemon/otel_metrics.go, internal/daemon/metrics.go, cmd/loomd/main.go, cmd/mcp-agent-context/main.go, pkg/mcpotel/, docs/operations/
- **Branch**: codex/arch-fleet-s5-truth-ledger
- **Depends on**: plan-loom-core-fleet-reliability-arch-20260710#2
- **Acceptance**: Router and health agree after one observation; circuits require configured consecutive recovery successes and one half-open probe; health snapshots remain under 50 ms during mass failure; build SHA, dirty bit, boot ID, config and tool-schema digests are non-empty and match canary artifacts; fake collector receives real traces and metrics within 10 seconds; event-drop and hub-latency metrics are accurate; exporter failure never fails a tool call and high-cardinality agent labels are absent.

### 7. P2 — Bounded hub worker fabric and clustered execution backend — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#7`
- **Goal**: Decouple client connection count from MCP child/background-service count and add clustered workers only after the single-owner contracts are proven.
- **Files**: cmd/custom-server/, pkg/execution/postgres/, k8s/base/servers/, k8s/base/kustomization.yaml, docs/operations/
- **Branch**: codex/arch-fleet-s6-hub-cluster
- **Depends on**: plan-loom-core-fleet-reliability-arch-20260710#4, plan-loom-core-fleet-reliability-arch-20260710#5, plan-loom-core-fleet-reliability-arch-20260710#6
- **Acceptance**: 100 WebSocket clients stay within the configured child/worker cap with no zombies and memory below 70 percent of pod limit; warm handshake p99 <1 s; child SIGKILL recovers within 30 s; two replicas pass lease/fencing tests; 1,000 parallel state updates lose none; cold recovery claims outstanding executions within 5 s; singleton background maintenance is leader-elected or disabled in non-owner workers.

### 8. P2 — Versioned MCP Tasks extension adapter — `pending`

- **Slice ID**: `plan-loom-core-fleet-reliability-arch-20260710#8`
- **Goal**: Project durable Execution records into negotiated MCP task wire shapes without coupling storage to a protocol revision or slowing legacy/local clients.
- **Files**: ../../libs/mcp-go/types.go, ../../libs/mcp-go/server.go, cmd/loom/proxy_handlers.go, cmd/loom/proxy.go, internal/daemon/daemon_dispatch.go, pkg/execution/mcp/, pkg/mills/clients/mcphub.go, internal/contracts/
- **Branch**: codex/arch-fleet-s7-mcp-tasks
- **Depends on**: plan-loom-core-fleet-reliability-arch-20260710#5, plan-loom-core-fleet-reliability-arch-20260710#6
- **Acceptance**: Current 2024-11-05/2025-06-18 clients remain unchanged; negotiated task clients receive a handle only after durable creation; disconnect/restart resumes by opaque ID; get/update/cancel are auth-isolated; terminal result/error matches the original RPC; stale workers cannot overwrite cancellation; direct non-task throughput regresses <2 percent and performs no new DB write; both legacy experimental and finalized extension compatibility decisions are contract-tested rather than embedded in storage.


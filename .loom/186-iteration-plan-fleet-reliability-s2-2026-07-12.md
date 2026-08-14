# RALPH Iteration: Fleet Reliability S2

## Review

- Roadmap milestone: Loom Core fleet performance and reliability architecture
- Canonical plan: `plan-loom-core-fleet-reliability-arch-20260710`
- Canonical slice: `plan-loom-core-fleet-reliability-arch-20260710#3`
- Spec section: P0 — Mills transactional transition kernel
- Branch: `codex/arch-fleet-s2-mills-transactions`
- Prior decisions to preserve:
  - The ordinary local MCP fast path remains storage-free; this slice changes only the durable Mills control plane.
  - S0's mandatory hermetic branch gate remains the merge authority, must fail on absent or skipped required scenarios, and must report race coverage honestly.
  - S1's single-owner and monotonic-generation contracts remain intact; Mills durability must not reintroduce ambiguous ownership outside the store.
  - SQLite WAL remains the embedded singleton backend. Multi-replica Mills stays disabled until later lease, fencing, and effect-adoption kill tests pass.
  - Commit intent before dispatch: no external starter or effect runs inside the claim transaction, and a lost response never becomes a claim of literal exactly-once external delivery.

## Align

- Slice name: P0 — Mills transactional transition kernel
- Scope in:
  - Add monotonic aggregate versions and conditional queued/version compare-and-swap semantics for backlog start admission.
  - Introduce one SQLite claim transaction covering backlog claim, budget and worker-slot reservation, next-attempt allocation, pipeline-run creation, DAG workflow metadata initialization, a transition-ledger row, and one unique pending dispatch-outbox intent.
  - Commit the pending dispatch intent before invoking `PipelineStarter`; consume pending intents after commit and recover them on operator restart.
  - Keep terminal reservation release and backlog/run state synchronization sufficient to prevent leaked capacity and stale running state.
  - Make workflow-run identity fields immutable and make concurrent step append idempotent without read-decide-write races.
  - Add deterministic racer, SQL fault-point, and crash-after-commit/before-dispatch proofs to the mandatory fleet reliability suite.
- Scope out:
  - Durable worker leases, lease epochs, fencing tokens, effect adoption/ledger semantics, and generalized transactional side-effect reconciliation.
  - PostgreSQL, clustered persistence, leader election, or multi-replica Mills.
  - End-to-end invocation identity, weighted fleet admission, reserved control capacity, or `AMBIGUOUS_OUTCOME` handling outside Mills start admission.
  - MCP Tasks adapters, protocol negotiation, or changes that route ordinary local calls through durable storage.
  - A broad terminal state-machine redesign beyond reservation release and existing backlog/run state synchronization.
- Acceptance criteria:
  - Immutable workflow metadata survives the first and every later journal append.
  - One hundred concurrent starts for the same queued backlog item commit exactly one aggregate version, one attempt/run, one reservation, one transition-ledger record, and one pending dispatch intent.
  - Fault injection after every SQL statement leaves no dangling run, reservation, workflow row, transition, outbox intent, or partially advanced backlog state.
  - A stale expected version updates zero rows and cannot allocate another attempt or dispatch intent.
  - Every committed aggregate version has exactly one transition record, and every admitted start has exactly one uniquely keyed dispatch intent.
  - Restart after commit but before dispatch consumes the pending intent and reaches the starter once; replay remains idempotent and cannot create another run or reservation.
  - Concurrent identical workflow-step appends converge on one durable row and one legal pending-to-terminal advance; mismatched hashes remain quarantined without overwriting the recorded call.
  - A 10,000-item queue keeps claim latency below 100 ms p95; candidate selection is bounded independently of queue depth, and each admitted start uses fewer than 20 deterministic transaction boundaries under the retained singleton SQLite configuration.
  - The deployed S1c dual-crash workflow reaches terminal state three consecutive times without a duplicate spawn or zombie.
  - Fleet reliability suite v3 records all four new exact tests, including the 10,000-item p95 bound, retains the existing scenarios and benchmark thresholds, and completes inside the ten-minute branch budget.
- Dependencies/blockers:
  - S0 and S1 are merged; the branch gate and generation-owned transport lifecycle are available prerequisites.
  - Hermetic store and workflow proofs require only a temporary SQLite database; they must not depend on an ambient daemon, live hub, Kubernetes, Qdrant, model provider, or fi-accel archive.
  - The final S1c acceptance leg requires a deployed operator and stable k3s vantage; local transaction proofs must land before that live canary is attempted.
- Risk notes:
  - The transaction's first queued/version CAS write serializes SQLite writers by design. A transaction that scans too much or performs external work can make contention violate the 10,000-item claim latency target.
  - Budget and slot checks are currently read-then-decide. Reservation accounting must live in the same transaction as the claim or concurrent racers can over-admit work.
  - Dispatch is at-least-once after commit. The unique outbox key and starter-side idempotency must absorb replay; this slice does not claim exactly-once behavior for downstream GitLab or spawn effects.
  - Existing full-row upserts can clobber newer state or immutable workflow identity. New transition paths must use expected-version predicates and insert-only or immutable-field validation without silently changing legacy read behavior.
  - Reservation release must be idempotent across terminal sync and restart, or capacity can leak or be returned twice.
- Riskiest assumption: singleton SQLite WAL can keep a 10,000-item queued claim below 100 ms p95 while 100 racers produce exactly one committed start and bounded losers, without weakening atomic budget/slot reservation or transition history.
- Kill test: race 100 claimers against one queued item; inject a failure after each deterministic transaction boundary and require a wholly old or wholly new aggregate; then crash after commit but before dispatch, restart the reconciler/outbox consumer, and require one run, one reservation, one transition, one consumed dispatch intent, one starter invocation, and no zombie. Repeat against a 10,000-item queue and reject the design if claim p95 reaches 100 ms, candidate work grows with queue depth, or an admitted claim reaches 20 transaction boundaries.

## Land

- Planned file areas: `pkg/mills/reconciler.go`, `pkg/mills/budget.go`, `pkg/mills/store/{store.go,types.go,dao_backlog.go,dao_pipeline.go,dao_workflow.go,migrations/,*_test.go}`, `pkg/mills/workflow/{journal_dao.go,killtest/}`, and `scripts/ci/fleet_reliability_suite_v1.json`.
- Implementation steps:
  1. Add backward-compatible schema for aggregate versions, reservations, transition records, and uniquely keyed pending dispatch intents, with indexes supporting an ordered queued claim.
  2. Implement the store-level claim transaction: queued/version CAS, budget/slot reservation, next attempt and run, DAG workflow identity, transition row, and dispatch intent commit as one unit.
  3. Replace the reconciler's independent run/backlog writes with the claim result, then dispatch only committed intents and recover pending intents on restart; release reservations through idempotent terminal synchronization.
  4. Enforce workflow-run immutable fields and CAS-safe concurrent append semantics while preserving deterministic replay and mismatch quarantine.
  5. Add exact concurrency/fault tests, the 10,000-item latency/query proof, crash-recovery coverage, and the deployed three-run S1c canary evidence.

## Prove

- Targeted tests:
  - `GOWORK=off go test -race ./pkg/mills/store -run 'TestClaimPipelineStart_ConcurrentExactlyOne|TestClaimPipelineStart_FaultMatrixAtomicity|TestClaimPipelineStart_TenThousandQueueP95AndStatementBound|TestWorkflowAppendStep_ConcurrentIdempotency'`
  - `GOWORK=off go test -race ./pkg/mills/workflow/...`
  - Focused reconciler tests for stale-version losers, pending-intent restart recovery, and idempotent reservation release.
- Performance/fault proof:
  - Run the 100-racer matrix repeatedly under `-race`.
  - Execute every deterministic SQL fault point and compare complete pre/post aggregate invariants.
  - Benchmark an ordered claim against 10,000 queued items; retain p95 and SQL statement-count evidence.
  - Run the S1c dual-crash harness to terminal state three consecutive times after deployment.
- Broader tests: `GOWORK=off go test ./pkg/mills/...`, `GOWORK=off CGO_ENABLED=0 go test ./...`, `make ci-contracts ci-openapi`, and `GOWORK=off make ci-reliability`.
- Lint/static checks: `gofmt`, `go vet` on changed buildable packages, `golangci-lint run`, manifest/schema verification, migration round-trip tests, pre-commit, and `git diff --check`.
- CI checks: the required branch reliability job must find and execute every manifested exact test without skips, preserve the paired benchmark thresholds, publish run-scoped evidence on failure, stay under ten minutes, and reach terminal green before merge.

## Outcome (2026-07-12)

- The start kernel now commits the queued/version CAS, serialized scope check,
  budget and slot snapshot, attempt allocation, pipeline run, active
  reservation, immutable DAG workflow identity, transition row, and unique
  dispatch intent in one transaction. Ten injectable post-statement boundaries
  prove every failure rolls back to a wholly old aggregate.
- One hundred concurrent claimers converge on one run, one reservation, one
  workflow, one transition, and one intent. Stale claim/revision writers and
  overlapping-scope contenders allocate nothing.
- Dispatch consumption uses expiring ownership tokens, due-time ordering,
  bounded exponential backoff, stale-token fencing, restart recovery, and an
  atomic dead-letter path that terminalizes the current aggregate, releases its
  reservation, synchronizes workflow state, and emits terminal metrics.
- Pipeline and backlog mutations now carry independent row revisions. Runner,
  integrator, recursion, manual-start, and repair paths use aggregate-guarded
  state-only backlog transitions so metadata edits cannot be overwritten by a
  long-lived run.
- Workflow identity is insert-only after initialization. Concurrent identical
  step appends converge; pending provenance remains first-writer authoritative;
  a differing terminal handle returns a typed conflict and hash mismatches are
  quarantined with both stored and incoming diagnostics.
- The retained 10,000-item claim benchmark recorded 12.812 ms p95 across 100
  samples. A real Tick over 10,000 queued rows inspected/admitted only the four
  policy slots, executed ten claim boundaries per admission, and completed in
  38.5 ms under `-race`.
- Acceptance refinement: the draft's “fewer than 20 SQL statements per tick”
  assumed the historical one-start-per-tick loop. The implemented scheduler
  intentionally batches up to `min(max_concurrent_runs, 128)`. The scalable
  invariant is therefore a bounded indexed candidate read plus ten transaction
  boundaries per admitted item, independent of total queue depth; total Tick
  work scales with admitted capacity, not with 10,000 queued rows.
- Fleet reliability suite v3 is manifest-driven from an immutable per-run
  snapshot and currently contains 49 exact tests, including 30 race-enabled
  scenarios, plus four paired benchmarks. A missing, skipped, count-mismatched,
  or mid-run-mutated scenario fails closed.
- Pre-ship size review recorded approximately 7.3k changed lines; more than
  3.6k are tests or reliability-manifest evidence. The slice remains one atomic
  change because the schema,
  claim DAO, reconciler adoption path, workflow invariants, and mandatory gate
  form one compatibility boundary: landing any subset would either expose an
  unwired migration or permit starts without their required recovery proof.
- Native local builds remain bounded by the pinned fi-accel module's missing
  `fi_accel.h`; the dependency-light race suites and full `CGO_ENABLED=0`
  repository/contract/OpenAPI paths are the hermetic local proofs.
- The deployed S1c dual-crash workflow has not been claimed by this code-only
  iteration. Its three consecutive terminal canary runs remain a rollout gate
  after merge and deployment.

## Handoff/Harvest

- Docs to update after proof: this iteration record, `ROADMAP.md`, the canonical Plan Store slice, and Mills operational documentation if recovery behavior changes an operator action.
- Agent-context entries to add: claim-transaction boundary, SQLite contention result, version/CAS contract, reservation lifecycle, dispatch-outbox recovery semantics, workflow immutability decision, fault-matrix evidence, and S1c live result.
- Next-slice candidate: `plan-loom-core-fleet-reliability-arch-20260710#4` — invocation safety and bounded admission, unblocked only after this transaction kernel and S1 remain green.

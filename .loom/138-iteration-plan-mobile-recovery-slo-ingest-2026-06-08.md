# RALPH Iteration Plan — Mobile Recovery-SLO Telemetry Ingestion (2026-06-08)

## Review

- Roadmap milestone: Mobile App + HUD Polish; gap-to-backlog `.loom/32` **MBL-5: SSE
  resilience + fallback SLOs** — last unchecked item: "Publish recovery SLO telemetry to
  a cross-surface (HUD) dashboard". Status *In progress* (in-app measurement done in
  `.loom/137`, merged `!668`).
- Prior slice (`.loom/137`): `ConnectionHealthMonitor` now times each transient outage
  and exposes `recoveryStats` (count/mean/nearest-rank p95), `meetsRecoverySLO`, and a
  rolling `recoverySampleSeconds` window (cap 50). SLO target **p95 ≤ 30s**. That data
  lives only on-device.
- The cross-surface item is multi-surface (mobile uploader → backend ingest/aggregate →
  HUD Svelte tile) and decomposes into ≥3 slices. This is **slice 1 of 3**: the backend
  ingestion + aggregation + read API foundation.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The wire contract defined here (a device POSTs its rolling
`samples: [seconds]` window + optional `slo_target_seconds`; server recomputes per-device
count/mean/nearest-rank-p95 and pools all devices for a fleet p95) matches what the iOS
`ConnectionHealthMonitor` can emit, so the future uploader slice needs no contract change.

**Kill test**: `ConnectionHealthMonitor` already exposes `recoverySampleSeconds` (rolling,
cap 50) and `recoveryP95TargetSeconds` (30). Server p95 uses the identical nearest-rank
formula (`rank=ceil(0.95n)`, `index=clamp(rank-1)`), verified by a Go test that reproduces
the Swift `recoveryStats` result on the same input vector. → If the Go p95 matches the
Swift p95 on a shared fixture, the contract is faithful.

**Failure mode if wrong**: the uploader slice would have to transform/recompute on-device
or the dashboard p95 would disagree with the in-app figure operators already see.

**Status**: passed 2026-06-08 — `TestRecoveryStore_P95_MatchesSwiftNearestRank` reproduces
the Swift formula (see `recovery_telemetry_test.go`).

## Align

- Slice name: **Backend recovery-SLO telemetry ingestion + fleet aggregation + read API.**

- Scope in:
  - `POST /api/mobile/v1/telemetry/recovery` — ingest one device's rolling recovery
    sample window. Scope-gated (`mobile:telemetry`), rate-limited (mutation class),
    validated, audit-logged. Keyed by `X-Device-ID`.
  - `GET /api/mobile/v1/telemetry/recovery` — fleet aggregate (device_count, total_samples,
    fleet mean, fleet nearest-rank p95, slo_target, devices_meeting_slo, meets_slo, per-device
    breakdown, updated_at). Scope `mobile:read`.
  - In-memory `recoveryStore` owned by `MobileDomain` (mirrors the rate-limiter / revocation
    in-memory pattern); injectable clock for deterministic tests; per-device cap 50; pooled
    fleet p95.
  - Go unit + handler tests; extend the mobile scope-contract matrix.

- Scope out:
  - iOS uploader (next slice) and HUD Svelte tile (slice after) — no mobile/Svelte changes here.
  - Persistence/restart durability — in-memory only for v1 (matches existing mobile stores).
  - Per-device history/time-series — only the latest snapshot per device is retained.

- Acceptance criteria:
  1. Ingest validates: missing `X-Device-ID` → 400; empty/invalid `samples` → 400; bad JSON
     → 400; non-finite/≤0 samples rejected; >50 samples truncated to most-recent 50.
  2. `mobile:telemetry` is **off by default** (not in the default scope set); ingest returns
     403 without it. Read uses `mobile:read` (in the default set).
  3. Server per-device p95 equals the Swift `recoveryStats` nearest-rank p95 on a shared
     fixture (kill-test).
  4. Fleet aggregate pools all devices' samples for mean/p95; `meets_slo` = fleet p95 ≤ target;
     `devices_meeting_slo` counts per-device verdicts; empty store → zeros + `meets_slo=true`.
  5. Re-ingest from the same device replaces that device's snapshot (no unbounded growth).
  6. `go test ./internal/hud/...` green incl. the extended scope-contract matrix (count 27→29,
     `mobile:telemetry` added to isolation set). `make lint` clean.

- Dependencies/blockers: none. Branch `feat/mobile-recovery-slo-ingest` off `origin/main`
  (`2e3997e4`).

## Land

- Planned file areas:
  - `internal/hud/domain/mobile/types.go` — add `ScopeTelemetry`.
  - `internal/hud/domain/mobile/recovery_telemetry.go` (new) — store + DTOs + p95/mean.
  - `internal/hud/domain/mobile/handler_telemetry.go` (new) — ingest + read handlers.
  - `internal/hud/domain/mobile/mobile.go` — register 2 routes; init store in `New`.
  - `internal/hud/domain/mobile/recovery_telemetry_test.go` (new) — store unit tests.
  - `internal/hud/domain/mobile/handler_telemetry_test.go` (new) — handler tests.
  - `internal/hud/app_test.go` — extend `TestMobileContract_AllScopesRequired`.
  - `.loom/32-mobile-gap-to-backlog-map.md` — MBL-5 progress note.

## Prove

- Tests: `go test ./internal/hud/domain/mobile/... ./internal/hud/ -run 'Recovery|MobileContract'`,
  then `go test ./internal/hud/...`. Via devbox.
- Lint/static: pre-commit (gofmt/goimports/vet/golangci-lint/build); `make lint`.
- CI: GitLab pipeline to terminal; fix-and-retry on red.

## Handoff/Harvest

- Docs: `.loom/32` MBL-5 — note backend ingestion/aggregation/read landed; uploader + HUD
  tile remain.
- Agent-context: decision (off-by-default telemetry scope; pooled fleet p95; in-memory store),
  finding (Swift/Go p95 parity).
- Next-slice candidates: (2) iOS recovery-telemetry uploader → this endpoint; (3) HUD Svelte
  recovery-SLO tile reading the aggregate.

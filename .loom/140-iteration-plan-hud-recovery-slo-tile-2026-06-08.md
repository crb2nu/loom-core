# RALPH Iteration Plan — HUD Recovery-SLO Tile (2026-06-08)

## Review

- Roadmap milestone: Mobile App + HUD Polish; gap-to-backlog `.loom/32` **MBL-5: SSE
  resilience + fallback SLOs**. Cross-surface publishing decomposed into 3 slices.
- Slice 1 (`.loom/138`, `!670`): backend ingest/aggregate/read API.
- Slice 2 (`.loom/139`, `!671`): iOS uploader posts the rolling window.
- This is **slice 3 of 3** (the last MBL-5 item): the HUD operator web UI tile that reads
  the fleet aggregate and renders recovery-SLO health. Completing it closes MBL-5.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The HUD Svelte web UI can read the recovery aggregate. The
slice-1 read endpoint is under the **bearer-gated** mobile API (`/api/mobile/v1/...`,
`mobile:read`), but the operator web UI is same-origin with **no bearer token** — so it
needs a different path to the same data.

**Kill test**: Verified against the codebase (this session): HUD-internal reads use
same-origin `/api/*` with no bearer (e.g. `internal/hud/routes.go` `/api/fleet`, `/api/kpis`;
frontend `fetch('/api/...')` in `OverviewPanel.svelte`). The mobile read calls
`requireMobileScope` (`handler_telemetry.go`), which a browser cannot satisfy. → The tile
must read a **new HUD-internal route**, not the mobile endpoint. **Status**: passed
2026-06-08 — resolved by registering `GET /api/telemetry/recovery` (same-origin, raw JSON)
that calls the existing `recoveryStore.Aggregate()`.

**Failure mode if wrong**: the tile would 401/403 against `/api/mobile/v1/telemetry/recovery`
and render perpetually empty/broken in the operator UI.

## Align

- Slice name: **HUD recovery-SLO tile + same-origin read route.**

- Scope in:
  - **Backend**: `GET /api/telemetry/recovery` registered by the mobile domain
    (`handleHUDRecoveryAggregate`) — same-origin, no scope gate, returns the **raw**
    `RecoveryAggregate` (no `{ok,data,meta}` envelope) from the same `recoveryStore` the
    mobile read uses. Go handler tests.
  - **Frontend**: `RecoverySLOCard.svelte` — self-contained `fetch('/api/telemetry/recovery')`
    + 30s poll (mirrors `ShuttlePanel`). Renders fleet p95 vs target with a meets/over
    `Badge`, headline counts (devices, devices-meeting-SLO, fleet mean, total samples), a
    per-device breakdown, and an empty state before any device reports. Mounted on
    `OverviewPanel`.
  - Rebuild the `go:embed`'d frontend dist (`make hud-frontend`) and commit it.

- Scope out:
  - No new mobile API surface (the mobile read endpoint is unchanged).
  - No auth on the HUD-internal route — consistent with the other unauthenticated
    same-origin `/api/*` operator reads; the data (device counts, p95 seconds, opaque
    keychain device IDs) is non-sensitive operator telemetry.
  - Granting the `mobile:telemetry` scope to device tokens — a deployment concern; the tile
    renders a valid empty/vacuous rollup until devices report.

- Acceptance criteria:
  1. `GET /api/telemetry/recovery` returns 200 **without** an Authorization header and the
     body is the raw aggregate (no `ok`/`data` envelope keys).
  2. The route reads the same store as `/api/mobile/v1/telemetry/recovery` (ingest → both
     reflect it).
  3. Empty store → `device_count:0`, `meets_slo:true`, `devices:[]` (not null).
  4. The Svelte tile renders fleet p95 / target + meets/over badge + counts + per-device
     rows when data is present, and a clear empty state when `device_count == 0`.
  5. The scope-contract matrix is unaffected (the new route is outside `/api/mobile/v1`).
  6. `go build ./cmd/loom` (embeds dist) + `go test ./internal/hud/...` green; frontend
     `pnpm build` succeeds; dist committed.

- Dependencies/blockers: none. Branch `feat/hud-recovery-slo-tile` off `main` (`8386fa76`).

## Land

- `internal/hud/domain/mobile/handler_telemetry.go` — `handleHUDRecoveryAggregate`.
- `internal/hud/domain/mobile/mobile.go` — register `GET /api/telemetry/recovery`; package doc.
- `internal/hud/domain/mobile/handler_telemetry_test.go` — 2 handler tests.
- `internal/hud/frontend/src/lib/components/RecoverySLOCard.svelte` (new) — the tile.
- `internal/hud/frontend/src/lib/components/OverviewPanel.svelte` — import + mount.
- `internal/hud/frontend/dist/**` — rebuilt embedded bundle.
- `docs/MOBILE_COMPANION_API.md`, `.loom/32` — docs + gap-map close-out.

## Prove

- Go: `go test ./internal/hud/...` (incl. contract matrix) + `go build ./cmd/loom`.
- Frontend: `make hud-frontend` (Svelte compile/type-check) — dist regenerated.
- No frontend unit-test harness exists; the tile is proven by a successful build + the
  Go-side data contract tests. CI: GitLab pipeline to terminal; fix-and-retry on red.

## Handoff/Harvest

- Docs: `.loom/32` MBL-5 → **Done**; `docs/MOBILE_COMPANION_API.md` documents the
  HUD-internal route + tile.
- Agent-context: decision (HUD-internal raw read vs bearer mobile endpoint), finding
  (MBL-5 closed end to end).
- Next: MBL-5 is complete. Next loop picks a fresh gap-map item (e.g. MBL-6 notification
  severity/action policy, or another open MBL issue).

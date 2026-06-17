# Iteration Plan: restart hysteresis under systemic failure pressure (Slice 4.3 of .loom/149)

**Date**: 2026-06-17
**Branch**: `feat/health-restart-hysteresis`
**Parent plan**: `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md` (Slice 4, third bullet)

## Context

Slice 0 (config mitigation) and Slice 1 (muxstdio close-cause observability) are
merged; Slice 4.1 (configurable deep-probe timeout, `.loom/152`) is merged. This
iteration lands the remaining Slice 4 hardening.

### Finding: Slice 4.2 is architecturally already satisfied

Slice 4's second bullet — "Don't count hub-stage transport failures toward
local-server restart decisions" — turns out to be a no-op given the current code:
the health monitor **only probes local processes**, never the hub.

- `fetchServerToolsViaPool` reads from `d.pool` = `connPool`, whose `DialFunc`
  calls `procMgr.Dial` → a **local stdio** subprocess
  (`internal/daemon/daemon_new.go:172-226`, `352`).
- The deep probe (`fetchServerToolsWithTimeout`) spawns a **dedicated local
  subprocess** (`daemon_tools_fetch.go:28-109`).
- The hub transport lives in a separate `hubPool`/`hubClient`
  (`daemon_new.go:243-266`, `353`), which the health monitor never touches.

So a hub `muxstdio: transport closed` never reaches the health monitor's
failure counter. The documented `gitlab` collateral restarts came from slow
local subprocess **start** under storm load (`.loom/149` Evidence #7), which
Slice 4.1's 10s→30s timeout bump addressed. No code change is required for 4.2;
this plan records the finding instead.

### What 4.3 adds (the real backstop)

Even with the wider deep-probe timeout, a sufficiently heavy hub-storm load can
slow *many* local probes at once (host CPU/IO pressure), flapping several healthy
local servers to unhealthy in the same sweep and triggering a wave of restarts —
exactly when restarting is least helpful. Slice 4.3 adds hysteresis: when a
system-wide failure signal is high, suppress auto-restart (still mark unhealthy
for visibility) and let the next sweep re-evaluate once pressure subsides.

Signal chosen: **count of distinct servers that failed a probe within a rolling
window**. Measured entirely inside the health monitor (it already observes every
probe outcome), so no cross-module coupling. A single genuinely-broken server
cannot trip its own suppression; it takes ≥N distinct servers failing together,
which is the systemic signature.

## Scope

**In**:
- `ServerHealthStatus.LastFailure time.Time` (json `last_failure,omitempty`),
  stamped on every probe failure.
- `HealthMonitor.restartPressureThreshold int` + `restartPressureWindow time.Duration`
  fields; `HealthMonitorConfig.RestartPressureThreshold` +
  `RestartPressureWindow`; defaults in `DefaultHealthMonitorConfig`
  (threshold **3** distinct servers, window **60s**).
- `countRecentlyFailedServersLocked(now)` helper (caller holds `h.mu`).
- In `checkServer`, before calling `handleRestart`: if
  `restartPressureThreshold > 0` and the recent-failed-server count
  `>= restartPressureThreshold`, **skip** the restart, log at Warn, and emit an
  `EventServerHealth` with `restart_suppressed: true`.
- Config plumbing: `HealthConfig.RestartPressureThreshold` (yaml
  `restart_pressure_threshold`) + `RestartPressureWindowSeconds` (yaml
  `restart_pressure_window_seconds`) → `ToHealthMonitorConfig`.

**Out** (follow-up / other slices):
- Slice 1/2 libs/mcp-go transport durability (separate repo + go.mod bump).
- Slice 3 hub per-conn spawn.
- Slice 5 HUD degraded-state surfacing.

## Acceptance criteria
1. `DefaultHealthMonitorConfig()` → `RestartPressureThreshold == 3`,
   `RestartPressureWindow == 60s`.
2. `NewHealthMonitor` wires both fields; zero/negative window → 60s fallback;
   threshold passed through verbatim (0 = disabled).
3. `countRecentlyFailedServersLocked` counts only servers whose `LastFailure` is
   within the window; ignores zero-value and stale failures.
4. With pressure ≥ threshold, `handleRestart` is **not** invoked and the server
   stays `Healthy == false` (marked unhealthy, restart suppressed).
5. With pressure < threshold (or threshold 0), restart proceeds as before.
6. `HealthConfig` overrides flow through `ToHealthMonitorConfig`; nil/zero →
   defaults.
7. `go test ./internal/daemon/ -run 'Health|RestartPressure'` green; build/vet/fmt clean.

## Risks
- A genuinely-down server during an unrelated multi-server blip could have its
  restart delayed by one or more sweeps. Mitigated: suppression is transient
  (re-evaluated every `CheckInterval`); the server stays marked unhealthy so the
  state is visible; threshold 3 distinct servers rarely coincides outside a real
  systemic event. Cost of a delayed restart << cost of a restart storm.
- Backward-compatible: under normal operation < 3 servers fail together, so the
  default path is unchanged. Operators can set threshold 0 to disable.

## Test plan
- `TestDefaultHealthMonitorConfig` extended (new fields).
- `TestNewHealthMonitor_RestartPressureWired` + window fallback.
- `TestCountRecentlyFailedServers_{WindowFiltering,IgnoresZeroValue}`.
- `TestHealthConfig_ToHealthMonitorConfig` extended for the new knobs.

## Status
- [x] implemented
- [x] tests green (`go test ./internal/daemon/` full suite ok 9.7s; build + vet + gofmt clean 2026-06-17). New tests: `TestNewHealthMonitor_RestartPressureWired`, `TestCountRecentlyFailedServers_{WindowFiltering,IgnoresZeroValue}`, `TestShouldSuppressRestart`, extended `TestDefaultHealthMonitorConfig` + `TestHealthConfig_ToHealthMonitorConfig_{Custom,NegativePressureDisables}`.
- [ ] MR merged

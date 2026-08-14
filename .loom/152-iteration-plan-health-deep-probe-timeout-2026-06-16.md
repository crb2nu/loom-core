# Iteration Plan: configurable deep-probe timeout (Slice 4.1 of .loom/149)

**Date**: 2026-06-16
**Branch**: `feat/health-deep-probe-timeout`
**Parent plan**: `.loom/149-plan-backend-hub-transport-stability-2026-06-12.md` (Slice 4, first bullet)

## Context

Slice 0 (config mitigation) and Slice 1 (muxstdio close-cause observability) are
merged. Slice 4 hardens the health monitor against false-positive restarts.
This iteration lands the first, lowest-risk bullet of Slice 4.

Documented false positive (`.loom/149` Evidence #7): the health monitor's deep
(process-spawning) probe runs under a hardcoded **10s** timeout
(`internal/daemon/daemon_tools_fetch.go:27`, plus a 10s parent ctx at
`health.go:159`). Under retry-storm load, subprocess **start** alone can exceed
10s → `start: context deadline exceeded` → server marked unhealthy → auto-restart
of a *healthy* local server (observed for `gitlab`). The plan's fix: make the
deep-probe timeout configurable, default **30s** (matches
`defaultDaemonControlRPCTimeout`, `callpipeline.go:16`).

## Scope

**In**:
- `HealthMonitorConfig.DeepProbeTimeout` (default 30s in `DefaultHealthMonitorConfig`).
- `HealthMonitor.deepProbeTimeout` field, wired in `NewHealthMonitor` (fallback to
  30s when ≤0 so existing callers that build `HealthMonitorConfig{...}` literals
  without the field don't regress to a zero timeout).
- `fetchServerToolsWithTimeout(ctx, name, timeout)`; `fetchServerTools` stays a
  10s wrapper so the **cache-warming** caller (`daemon_tools_cache.go:101`) keeps
  its fast-fail behavior unchanged.
- `checkServer` deep-probe sites use `h.deepProbeTimeout`.
- `checkAllServers` parent ctx budget widened to `max(deepProbeTimeout, 10s)` so
  the child deep-probe deadline isn't clipped back to 10s.
- Config plumbing: `HealthConfig.DeepProbeTimeoutSeconds`
  (yaml `deep_probe_timeout_seconds`) → `ToHealthMonitorConfig`.

**Out** (follow-up slices, per `.loom/149` Slice 4):
- Don't count hub-stage transport failures toward local-server restart decisions.
- Restart hysteresis under system-wide retry pressure.
- Slices 2 (libs/mcp-go transport durability), 3 (hub per-conn spawn), 5 (HUD degraded surfacing).

## Acceptance criteria
1. `DefaultHealthMonitorConfig().DeepProbeTimeout == 30s`.
2. `NewHealthMonitor` wires `deepProbeTimeout`; zero/negative cfg → 30s fallback.
3. Deep probe path uses the configured timeout; `fetchServerTools` (cache caller)
   still bounded at 10s.
4. `HealthConfig.DeepProbeTimeoutSeconds > 0` overrides; nil/zero → 30s default.
5. `go test ./internal/daemon/ -run 'Health|FetchServerTools'` green; lint/build green.

## Risks
- Wider parent ctx in `checkAllServers` (10s → 30s) lengthens the worst-case
  health-sweep wall time. Mitigated: pool probes return fast on success/failure;
  only the (interval-gated, running-server-only) deep probe consumes the budget.
- Backward-compatible: default only *relaxes* the timeout (10s → 30s); no behavior
  change for healthy servers that respond < 10s.

## Test plan
- `TestDefaultHealthMonitorConfig` extended (DeepProbeTimeout == 30s).
- `TestNewHealthMonitor_DeepProbeTimeoutWired` + zero-value fallback.
- `TestHealthConfig_ToHealthMonitorConfig_{Defaults,Custom}` extended.

## Status
- [x] implemented
- [x] tests green (`go test ./internal/daemon/` full suite ok 9.4s; build + vet + gofmt clean 2026-06-16)
- [ ] MR merged

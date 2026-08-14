# Iteration Plan: Mills Budget Fuel Gauge

**Date:** 2026-07-08
**Source research:** `.loom/174` thread F ("a 'fuel' gauge for the rolling
24 h budget window"), deferred from the garnish MR (!1015) because the HUD
payload carried no budget data.

## Riskiest assumption + kill-test

**Load-bearing assumption**: everything needed exists server-side —
`pkg/mills.Budget` already computes rolling-24h spend/run counts
(`spentSince`/`runsSince`, `budget.go`), the store reader discounts
non-recurring escalations (`CountBudgetedSince`), and the operator holds
`o.budget` (`server.go:33`).

**Kill test**: in-code (2026-07-08): all three legs verified in source;
new `WindowUsage` is a thin public projection over existing private
plumbing, pinned by `TestBudget_WindowUsage` against the fixture policy.

**Failure mode if wrong**: a second spend-aggregation path that could
disagree with enforcement. Avoided by construction — `WindowUsage` calls
the SAME `spentSince`/`runsSince` the enforcer uses.

**Status**: passed 2026-07-08.

## Slices (one MR)

1. **Go**: `Budget.WindowUsage(ctx, tier)` — informational
   `{spent_usd, cap_usd, runs, runs_cap}` snapshot; zero caps mean
   "not configured", never "empty tank". `/api/mills/status` gains a
   `budget: {pipeline, council}` object; a failed tier read omits the
   tier rather than failing status.
2. **HUD**: `BudgetWindowUsage` on `MillsStatus`; rune-free
   `fuelReading` (em dash when absent — never a guessed level; uncapped
   shows spend only; capped maps remaining fraction to ok/warn/error);
   a tank-bar gauge (`fuel · 24h pipeline budget`) in the Factory strip.

## Verification

- Go: `TestBudget_WindowUsage{,_Unconfigured}`; operator builds.
  (macOS-local full-pkg run hits the documented fsnotify flake
  `TestPolicyManager_HotReload_K8sConfigMapSwap` — unrelated; CI is the
  arbiter.)
- vitest: absent/uncapped/threshold/overspend-clamp cases (121/121).
- Preview MCP: all four gauge states driven live via mocked status.

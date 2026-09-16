package main

import "time"

// Boot ordering (2026-09-07/08 boot-burst incident, pods 6b9f8bbd7f-mgbmp,
// bb774967c-m82pl, 7dff69845b-2qnkl). Every long-running loop below the
// errgroup used to fire its first pass in the same ~5ms after "loom-mills-
// operator booting". On a cold page cache the single SQLite store then served
// the report-rollup warm-up (judge_calibration 4.3s, promotion 7.0s,
// config_outcomes 3.2s, overseers 16.1s — 15–60ms each at steady state), the
// reconciler's boot tick with all four housekeeping sweeps due, the escalation
// sweep and the take-up/emitter initial ticks at once, and the loops with the
// shortest budgets lost: the auto-requeue sub-budget (20s) expired with 16
// candidates unjudged, the boot tick died at its 30s deadline inside whichever
// housekeeping sweep it reached first, and the 17 ledger rows meant to explain
// both were appended on the same dead contexts and lost.
//
// The order is now fixed rather than a race:
//
//  1. listeners, pollers and the report-rollup warm-up start immediately —
//     the warm-up touches the widest event windows the operator reads, so
//     everything after it reads a warm cache;
//  2. the reconciler's boot tick (control law only — housekeeping sweeps are
//     staggered onto the following ticks) and the intake loops start once
//     the warm-up has returned;
//  3. the escalation sweeper's first pass starts once that boot tick has
//     returned.
//
// Each wait is budgeted so a wedged phase can never hold a control loop
// hostage: the waiter logs and proceeds.
const (
	// bootRollupWarmupWait caps how long the reconciler and intake loops wait
	// for the report-rollup warm-up. The cold-cache refresh measured ~31s in
	// the incident; each report has its own 45s build timeout, so the cap
	// covers one pathological report without stalling dispatch further.
	bootRollupWarmupWait = 90 * time.Second
	// bootReconcilerTickWait caps the escalation sweeper's wait for the
	// reconciler's boot tick: that tick itself waits up to
	// bootRollupWarmupWait and then runs under the 30s scheduler tick
	// deadline, so the cap must exceed their sum.
	bootReconcilerTickWait = bootRollupWarmupWait + 60*time.Second
)

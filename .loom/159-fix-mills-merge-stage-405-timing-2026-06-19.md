# Fix — Mills merge stage 405 is a timing race, not a config block (2026-06-19)

## Context

Slice A2 (north-star: first autonomous merge). This session drove the k8s
pipeline end-to-end with a real commit; the remaining gates were the `scope`
gate (fixed, MR !729) and the final `merge` 405.

## Finding

GitLab returns **HTTP 405** on `PUT /merge_requests/{iid}/merge` while the MR's
`merge_status` is still settling after its pipeline turned green — it flips to
`can_be_merged` within seconds. Observed directly while arming auto-merge on
!728 and !729: the immediate merge call 405'd, then succeeded seconds later.

The Mills operator's merge stage hit this as the **terminal** failure on the
runs that reached `ci_watch` green (escalations
[#147](https://gitlab.flexinfer.ai/services/loom-core/-/issues/147),
[#148](https://gitlab.flexinfer.ai/services/loom-core/-/issues/148),
[#150](https://gitlab.flexinfer.ai/services/loom-core/-/issues/150)). DEBT-073
classified merge-405 as terminal `ClassConfig` — correct that 3 verbatim retries
in ~2s gave no signal, but the *root cause* is **timing**: 2s is far too fast for
GitLab to recompute mergeability. So those runs escalated on a transient.

## Fix

`pkg/mills/clients/gitlab.go` `Merge`: poll past the transient 405 *within the
stage*. Re-attempt the merge on a "not mergeable yet" 405 with `cfg.PollInterval`
backoff up to `mergeReadyTimeout` (2m). Any non-405 error (auth, 409 conflict)
returns immediately — no swallowing real failures. A genuinely permanent 405
still surfaces after the window, where the runner's terminal classification
escalates it correctly (no fast no-op attempts).

`isMergeNotReady` matches the GitLab client error shape (`status 405` /
`method not allowed`), mirroring `error_class.go`'s merge-405 detection —
deliberately not bare `405` (appears in timestamps/IDs).

Tests: `TestMerge_RetriesNotMergeableYet405` (2×405 then 200 → success, 3
attempts), `TestMerge_NonRetryableErrorReturnsImmediately` (409 → no retry).

## Why now

This pre-clears the expected last gate so the next green canary run can reach
`merged` (north-star 0→1) without escalating on the merge timing. Refs
`.loom/126` (Phase A / Slice A2), `.loom/158`.

# Iteration Plan — Mills `ci_watch` poll timeout → classify infra + surface pipeline URL

> RALPH slice, 2026-07-06. Branch `fix/mills-ci-watch-timeout-classify`.
> Closes part of **DEBT-073 / #167 class a** (recurring Mills escalation classes).

## Context

`#167` (DEBT-073, P1) enumerates four recurring bot-filed escalation classes.
Audit of the current tree found three already fixed:

- **class b** (merge 405 → terminal `ClassConfig`): DONE (`error_class.go:51-59,122-129`).
- **class c** (scope-gate canary allowlist `heartbeat.md`): DONE (`gates/scope.go:98-100`).
- **class d** (kubelet/proxy 5xx): DONE — handled as `transient` free-retry
  (`error_class.go:177-204`), arguably better than the issue's proposed `infra`.

Remaining open: **class a** — `ci_watch` poll timeout. The GitLab client's
`PollPipeline` caps a single poll at `PollDeadline` (30m) but its timeout error
(`"gitlab: pipeline poll timed out after 30m"`) falls through `Classify` to the
default `ClassCode`, so a stuck/slow CI pipeline is reported as a real code bug
and buried among build/test breaks (escalations #149/#153). The pipeline URL is
never surfaced, so the escalation isn't actionable.

## Scope

**In:**
- `ErrPipelinePollTimeout` sentinel (`pkg/mills/pipeline/dispatcher.go`), mirroring `ErrSpawnPollTimeout`.
- `PollPipeline` wraps its timeout with the sentinel + embeds the branch pipeline `web_url` (`pkg/mills/clients/gitlab.go`; `shaPipeline` gains `web_url`).
- `Classify` maps `errors.Is(err, ErrPipelinePollTimeout)` → `ClassInfra` (`pkg/mills/pipeline/error_class.go`).
- Unit tests + CHANGELOG entry.

**Out (separate slices / ops):**
- Escalation issue **dedup + auto-close** (#167 criterion e).
- One-time **bulk triage** of the stale ~60 escalation issues (ops, not code).
- Any change to the runner's retry-budget machinery (kept identical).
- Reducing the retry count for a poll timeout below `MaxAttempts` (a genuinely
  slow pipeline can still go green on a re-poll — do not risk premature escalate).

## Acceptance criteria

- [x] `Classify(fmt.Errorf("...: %w", ErrPipelinePollTimeout)) == ClassInfra`.
- [x] `ci_watch` timeout error satisfies `errors.Is(err, ErrPipelinePollTimeout)`.
- [x] Timeout error + poll log tail embed the branch pipeline `web_url`.
- [x] `ClassInfra` is not free-retry and not terminal (retry semantics unchanged).
- [ ] `go build ./...`, `go vet`, `gofmt`, `golangci-lint`, `go test ./pkg/mills/...` clean.
- [ ] MR merged to `main` (pipeline-success gate).

## Risk notes

- **Low blast radius**: `Code`↔`Infra` are retry-identical (both count against
  `MaxAttempts`, neither is a free transient retry — `error_class.go:254-263`).
  This is a labeling correction + an actionable-URL add, not a behavior change.
- Existing `PollPipeline` timeout tests still hold: `err != nil` and
  `resp.Status == "timeout"` are both preserved by the wrap.
- `errors.Is` check placed BEFORE string matching in `Classify` so the embedded
  URL can't accidentally match a different needle.

## Test plan

- `pkg/mills/pipeline`: `TestClassify_PipelinePollTimeoutIsInfra`.
- `pkg/mills/clients`: `TestPollPipeline_TimeoutWrapsSentinelAndSurfacesURL`.
- Regression: full `go test ./pkg/mills/...` (existing PollPipeline + classifier suites).

## Riskiest assumption + kill-test

Internal-only refactor of an existing classifier seam — no external-system
behavior claim. The load-bearing internal assumption (`Code` and `Infra` retry
identically, so reclassifying is safe) is directly asserted by
`TestClassify_PipelinePollTimeoutIsInfra` + `TestIsFreeRetry`/`TestIsTerminal`.
**Status**: passed 2026-07-06 (unit tests).

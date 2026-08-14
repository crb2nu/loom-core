# RALPH Iteration: Class-Aware Mills Escalations

## Review

- Milestone: DEBT-073 / issue #167 criterion e: dedup by backlog item + failure class and close after later success.
- Preserve best-effort publication/resolution, fail-open lookup, legacy markers, and base `IssueClient` compatibility.

## Riskiest assumption + kill-test

**Load-bearing assumption**: `FailureRecord.FailureClass` is normalized before publication and is stable enough for dedup identity.

**Kill test**: drive one backlog through repeated `code` and distinct `configuration` failures; prove same-class reuse, cross-class separation, and success resolution of all threads. Exercise production GitLab lookup against mixed legacy/class markers.

**Failure mode if wrong**: equivalent failures fragment or unrelated classes share one thread, preserving operator noise.

**Status**: passed 2026-07-18 via `go test ./pkg/mills/pipeline ./pkg/mills/clients`.

## Align

- In: class markers, exact-class recurrence lookup, backlog-wide success resolution, legacy/unclassified compatibility, tests, changelog.
- Out: historical bulk cleanup, live K3s canary, classification-policy changes, audit follow-up issues.
- Accept: same class reuses; different class separates; success closes all; lookup fails open; partial close continues; quality gates pass.

## Land

- Files: `pkg/mills/{pipeline,clients}/`, tests, and `changelog.d/`; add optional capabilities without breaking existing issue-client interfaces.
- Render exact markers, close all matching refs, and cover compatibility/error branches.

## Prove

- `go test ./pkg/mills/...`; `go test -race ./pkg/mills/pipeline ./pkg/mills/clients`.
- `golangci-lint`, `make ci-contracts`, `make changelog-check`, `git diff --check`, required GitLab CI.

## Handoff/Harvest

- Update issue #167 with landed semantics; retain historical cleanup and live-canary follow-ups.

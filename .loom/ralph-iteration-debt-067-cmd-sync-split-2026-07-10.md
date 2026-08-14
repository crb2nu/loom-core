# RALPH Iteration Plan — DEBT-067 command sync split

## Riskiest assumption + kill-test

**Load-bearing assumption**: `cmd/loom/cmd_sync.go` contains command factories and local helpers that can be moved into focused files without changing Cobra parent/child registration order, flags, defaults, help text, or runtime behavior.

**Kill test**: Capture the recursive `loom generate --help`, `loom sync --help`, and relevant child help output before and after the split; require byte-for-byte equality, then run `go test ./cmd/loom/... ./pkg/sync/... ./pkg/generator/... ./pkg/skills/...` and a built `loom sync status` smoke test. Any command-tree/help difference fails the slice.

**Failure mode if the assumption is wrong**: A mechanical-looking refactor could silently remove a subcommand, move a flag to the wrong command, or change defaults used by config and skill synchronization.

**Status**: passed 2026-07-10 (16 pre/post CLI help contracts matched byte-for-byte; focused suites recorded in the MR evidence)

## Review

- Roadmap milestone: Next — split `cmd_sync.go` (#170 / DEBT-067).
- Spec section(s): GitLab issue #170 acceptance criteria.
- Prior decisions to preserve: generated/home-managed profile behavior, stable Loom binary resolution, hosted-skill destinations, and mirror source-wins semantics.

## Align

- Slice name: Decompose the Loom generate/sync command surface without behavior change.
- Scope in: move generate, pull/backup, skill sync, status, and mirror factories/helpers into focused sibling files; preserve the root `newSyncCmd` composition.
- Scope out: feature changes, registry content changes, generated config refreshes, and MCP server fixes already active in another worktree.
- Acceptance criteria: `cmd_sync.go` becomes the sync registration/execution root; command tree/flags/help remain identical; focused and broad tests pass.
- Dependencies/blockers: none; branch starts from current `main` and avoids the user-modified canonical worktree.

## Land

- Planned file areas: `cmd/loom/cmd_sync*.go`, focused command-contract tests, this iteration plan.
- Implementation steps:
  1. Capture command help contracts and split factories/helpers by responsibility.
  2. Add/adjust tests only where needed to make command-tree preservation explicit.
  3. Format, test, self-review, and ship through MR/CI.

## Prove

- Tests to run: focused command tests; `go test ./cmd/loom/... ./pkg/sync/... ./pkg/generator/... ./pkg/skills/...`; broader repository gate as practical.
- Lint/static checks: `gofmt`, `go vet`/repository lint gate.
- CI checks: GitLab pipeline to terminal success before merge.

### Results

- Passed: 16/16 pre/post CLI help contracts, scoped pre-commit hooks, focused Go suites, targeted golangci-lint, build, and 29 contract goldens.
- Full `go test ./...`: all packages passed except `pkg/mills/TestPolicyManager_HotReload_K8sConfigMapSwap`; the same isolated failure reproduces on unchanged `main`, so it is baseline fsnotify debt outside this slice.

## Handoff/Harvest

- Docs to update: issue #170/MR evidence and roadmap only after landing.
- Agent-context entries to add: split boundary decision, contract proof, final MR/CI result.
- Next-slice candidates: build the next config/skills enhancement atop the lower-conflict command layout.

# Skills sprint, first increment

Branch: `codex/skills-reliability-sprint`.
Starting revision: `96e63c4602f2984b08e528fc26ef46224b45b1ab`.
Plan: `plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28`.

This increment implements S1 authoring/API conformance and the delivery-integrity
portion of S2. The S0 feasibility experiment completed with a failed gate:
cross-case read contamination invalidated Codex's absent control, and Claude was
not authenticated. That evidence narrows the release to deterministic repairs.
It does not support activation accuracy or behavior improvement claims.

## Local evidence

- Full repository `go test -p 2 ./...`: passed, including contract tests.
- Full repository `golangci-lint run --timeout 5m ./...`: passed, zero issues.
- Final affected-package lint after review fixes: passed, zero issues. Full
  repository tests were repeated after the ownership/manifest fixes and passed.
- Current-source `loom generate skills --target all --validate`: passed for all
  80 registry skills and their resources.
- Python scaffold unit tests: three passed; Go scaffold integration passed.
- Changelog fragments, documentation guardrail and diff whitespace checks passed.

Go checks used Go 1.26.5, `GOWORK=off`, `CGO_ENABLED=0`, `GOMAXPROCS=2` and a task
cache under `/private/tmp`. The app worktree cannot resolve the relative sibling
modules in `go.work`, and the native accelerator header `fi_accel.h` is not
available. These results do not certify the native accelerator build. Checks
ran under bounded process wrappers; no daemon reload or real profile sync was
part of validation.

## Review scope and limits

The diff exceeds the self-review guideline of 500 changed lines because it
includes the sprint research/plan, three retained diagnostic traces, and
regression matrices across bundle generation, home delivery, cleanup and schema
conformance. Production changes share the existing generator and manifest
paths; no new agent runtime or evaluation platform is introduced.

Required resources now fail delivery explicitly. SHA-256 manifest entries permit
safe retirement of unchanged files. Modified stale files and legacy files with
unknown hashes are preserved and reported as conflicts. Individual file copies
are atomic; the whole directory is not transactional. Earlier successful copies
can remain updated after a later error, with the previous manifest retained.
Regression coverage includes bundle symlinks checked before writes, repo/home
directory aliases, rejection of incomplete manifest objects, and restoration
from a verified v2 bundle to v1 while retaining unmanaged neighbors.

S2 catalog collision diagnostics and a staged whole-bundle rollback workflow
remain open. S3 needs enforced read isolation, actual model identity, a valid
absent control and restored Claude authentication before live comparisons.
S4 pilot behavior changes and S5 canary rollout remain gated. See the
[probe evidence](../mcp/skills/_eval/baseline/2026-09-13.json) and
[evaluation guide](../docs/SKILL_EVALUATION.md).

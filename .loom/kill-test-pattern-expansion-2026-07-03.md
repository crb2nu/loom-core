# Kill-test: Pattern Loom catalog expansion (go-mcp-server, go-cli, python-fastapi-service)

**Date**: 2026-07-03 · **Scope**: 3 new builtin patterns · **Verdict**: **PASS ×3 (all approved)**

## What was tested

**Assumption (riskiest)**: a pattern authored *without* a kill-test is presumed stampable but usually isn't — the S1 kill-test showed a fresh pattern under-specifies seams the author can't see. So each new builtin pattern had to earn `approved` the same way `pattern-go-rest-service` did: a **context-free follower** executes the stamped instruction book alone, and an **independent gauge** (written before any follower output existed, never shown to the follower) scores the result.

**Method** (per pattern, run in parallel):

1. Generated the stamped manifest via the real `stampPattern` path (`pkg/agentcontext/manifest_dump_test.go`, `MANIFEST_DUMP_DIR=… go test -run TestDumpStampManifests`) with synthetic materials — real stamp bytes, not a reconstruction.
2. Handed the manifest to a fresh agent with zero conversation context, under the S1 rules: follow the pins exactly, add nothing, record every unspecified choice in `GAPS.md` classified ARCHITECTURE vs COSMETIC.
3. Scored with an independent gauge (`scratchpad/killtest/gauge-{mcp,cli,fastapi}.py`).
4. Negative search: file inventory vs spec + feature scan for unrequested architecture.

## Evidence

| Pattern | Materials | Gauge | ARCH gaps | Negative search |
|---|---|---|---|---|
| `pattern-go-mcp-server` | `mcp-echo`, tools `echo`+`add` | **18/18 PASS** | 1 | clean |
| `pattern-go-cli` | `sprockctl`, cmds `greet`+`count` | **17/17 PASS** | 0 | clean |
| `pattern-python-fastapi-service` | `widget-api`, entity `Widget{name,quantity}` | **15/15 PASS** | 3 | clean |

All three instances: exact file inventory (spec + allowed `GAPS.md` only), `go.mod` with no require block / pyproject deps exactly as pinned, and zero hits on the unrequested-feature scan (prometheus/jwt/middleware/sql/router libs/cobra/sqlalchemy/structlog/…).

**Architecture-class gaps found → pinned (the S1 feedback loop):**

- *go-mcp-server*: material field type `int` → JSON Schema `"integer"` mapping was unpinned → now in the `tool_schema` pin.
- *python-fastapi-service*: (1) exact endpoint set incl. **no update endpoint** and plural derivation were unpinned → new `endpoints` pin; (2) "unexpected create error 500" was implemented as a global app-wide handler → `status_codes` pin now says exactly that; (3) `ValidationError` seam had no producer → `error_model` pin now names it as the scaffolded seam for follow-up slices.
- *go-cli*: **zero** architecture gaps (8 cosmetic only) — the pin discipline transfers.

## Conclusion

The assumption held in the useful direction: the pins authored from the S1 playbook were close to complete (25/25 build+behavior gauge groups passed with no retry), and the follower-gaps loop surfaced exactly the residual seams, which are now pinned. All three patterns seed as `approved` with `ApprovedBy: kill-test 2026-07-03`; `instances_shipped_green` stays 0 until a real Mills stamp merges (`agent_pattern_record_instance` takes over from there).

**Out of scope (deliberate)**: the full Mills e2e (stamp → enqueue → spawn → MR → merge) is proven separately (Pattern Loom kill-test 2026-07-02, sprocket stamp → !874); nothing in this expansion touches that plumbing — the pipeline is pattern-agnostic (`PlanID` is the whole contract).

Instances + gauges preserved at `scratchpad/killtest/` (session-scoped); manifests regenerable via `manifest_dump_test.go`.

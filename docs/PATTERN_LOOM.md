# Mills Pattern Loom

The Pattern Loom turns Mills into a **factory keyed by intent**: a user arrives
with intent to make a *type of thing* (a service, tool, product), Mills holds a
library of vetted **Patterns** that work for that type, and it **stamps** the
chosen pattern with the user's **Materials** to produce a deployed working
instance.

The metaphor is a textile pattern: an instruction book anyone can follow given
**materials** + **basic tools**. The pattern encodes a master's taste; the
follower needs none of their own.

## Object model

| Noun / verb | What it is | Where |
|---|---|---|
| **Engram** | An atomic, proof-gated technique (a "stitch"). Seeded by the Pattern Loop (A2): a pattern's composed building blocks, verified + unlocked when a stamp merges green. | `pkg/agentcontext/svc_engrams.go`, `pkg/agentcontext/schema_engrams_builtin.go` |
| **Pattern** | A product archetype (a "garment blueprint"). Composes engrams and **pins** the load-bearing architecture so only Materials vary. | `pkg/agentcontext/schema_pattern.go` |
| **Materials** | The user's typed inputs that fill a Pattern's `materials_schema` (the "fabric"). | supplied per stamp |
| **Stamp** | The verb: `stamp(pattern_id, materials) → Plan`. Validates materials, then expands the slice template into a Plan the Mills pipeline runs. | `pkg/agentcontext/svc_pattern_stamp.go` |

A `Pattern` declares:

- `materials_schema` — typed inputs (name, type, required, enum, default, example).
- `tools_manifest` — the closed set of required capabilities (the "basic tools");
  a stamp aborts if a required tool is absent.
- `pins` — the **closed architecture decisions** that make the pattern *stampable*
  rather than a suggestion. The S1 kill-test proved a pattern must pin not just
  transport/storage/deps but the **error envelope, per-endpoint bodies, wiring
  convention, error model, and full status-code table**.
- `gauge` — the swatch: the smallest end-to-end check (commands + black-box
  assertions) proving a stamp is correct in *this* environment.
- `slice_template` — slice blueprints, expanded with Materials (placeholder
  substitution) into concrete `PlanSlice`s on stamp.
- `deploy_contract` — what "deployed working version" means for this type.
- `provenance` — author / approver / `instances_shipped_green` (the taste gate).

### Builtin catalog

Every builtin is code-defined (`schema_pattern.go`, `schema_patterns_builtin.go`)
and upserted on service start; each earned `approved` through its own kill-test
(context-free follower agent + independent gauge, per the S1 method):

| Pattern | Makes | Kill-test |
|---|---|---|
| `pattern-go-rest-service` v0.2 | stdlib-only Go HTTP/JSON single-entity CRUD service | 2026-06-28, gauge 10/10 (`.loom/kill-test-pattern-go-rest-service-2026-06-28.md`) |
| `pattern-go-mcp-server` v0.1 | stdlib-only Go MCP server (newline-delimited JSON-RPC 2.0 over stdio; tool registry with generated inputSchema) | 2026-07-03, gauge 18/18 |
| `pattern-go-cli` v0.1 | stdlib-only Go CLI (`flag.FlagSet` subcommands, pinned exit codes, ldflags version) | 2026-07-03, gauge 17/17, zero architecture gaps |
| `pattern-python-fastapi-service` v0.1 | FastAPI single-entity CRUD mirroring the Go REST external contract (same error envelope / status table / 16-hex ids; `app/` package pinned entity-independent) | 2026-07-03, gauge 15/15 |

The 2026-07-03 expansion evidence lives in
`.loom/kill-test-pattern-expansion-2026-07-03.md`; stamped manifests for future
kill-tests are regenerable via `pkg/agentcontext/manifest_dump_test.go`
(`MANIFEST_DUMP_DIR=<dir> go test ./pkg/agentcontext -run TestDumpStampManifests`).

**Authoring a new builtin**: add the constructor to
`schema_patterns_builtin.go` + its composed engrams to
`schema_engrams_builtin.go` (`TestPattern_SeedBuiltins_CatalogWellFormed`
enforces pattern↔engram non-drift and placeholder derivability), seed it
`candidate`, run the kill-test, pin whatever ARCHITECTURE-class gaps the
follower records, and only then flip it to `approved` with the kill-test in
provenance.

## MCP tools (`mcp-agent-context`)

| Tool | Purpose |
|---|---|
| `agent_pattern_add` | Register/upsert a Pattern (`pattern-<slug>`). |
| `agent_pattern_get` | Fetch a Pattern by id. |
| `agent_pattern_list` | List by `makes` / `status` (cross-agent). |
| `agent_pattern_search` | Semantic search over name+makes+description. |
| `agent_pattern_stamp` | Stamp `{pattern_id, materials}` → a Plan (`plan_id`, `slice_count`, `tools_required`). |

Patterns are cross-agent (never `agent_id`-scoped), Qdrant-backed
(`agent_patterns_v1`), and seeded idempotently on service start.

## Surfaces

### Track A — council rails (inward)

The Mills council editor injects the approved-pattern catalog into its prompt and
records a `pattern_id` per proposal, so autonomous proposals conform to a vetted
pattern instead of free-styling architecture. Works behind both the FlexInfer and
gpt-5.4 editors. See `pkg/mills/clients/pattern.go`, `pkg/mills/clients/council.go`.

**Engram population (A2).** The empty engram catalog never had a producer; the
Pattern Loop becomes one. A pattern's composed building blocks are seeded as
tier-1 engrams on startup (`schema_engrams_builtin.go`), and the green-stamp hook
(`agent_pattern_record_instance`) verifies each one against the merged instance's
checkout — flipping `proof_status` to `verified` and appending the instance repo
to `unlocked_in`. A stamped slice that composes **no** engram is surfaced as an
`engram_candidate` (never auto-added — minting an engram must earn its proof via
`agent_engram_add`). Every builtin pattern composes engrams proven by stable,
material-independent layout files: the Go REST trio
(`engram://go-http/server-graceful-shutdown`, `engram://go-config/env-config-struct`,
`engram://go-build/stdlib-only-gomod`), the MCP pair
(`engram://go-jsonrpc/stdio-line-protocol`, `engram://go-mcp/tool-registry-inputschema`),
the CLI pair (`engram://go-cli/flagset-subcommand-dispatch`,
`engram://go-cli/ldflags-version-injection`), and the FastAPI trio
(`engram://py-fastapi/app-factory-di`, `engram://py-fastapi/error-envelope-handlers`,
`engram://py-config/env-dataclass-config`). `go-build/stdlib-only-gomod` is
shared — all three stdlib-only Go patterns compose it, so a green stamp of any
of them verifies it. See `pkg/agentcontext/svc_patterns_a2.go`. (Live wiring of
the merged-instance checkout path lands with the full Mills e2e stamp; the
mechanism + verify are unit-tested now.)

### Track B — front door (outward)

A human entrypoint to stamp a pattern:

```bash
loom mills patterns --status approved          # list the catalog
loom mills stamp --pattern pattern-go-rest-service \
  --materials @materials.json --project services/loom-core
```

HUD REST: `GET /api/patterns?status=approved` and `POST /api/patterns/stamp`
(`internal/hud/api_patterns.go`, bridge in `internal/hud/bridge/agent_patterns.go`).

**Stamp → Mills (S1 e2e seam).** `POST /api/patterns/stamp` accepts an opt-in
`"enqueue": true`. By default a stamp only writes a Plan; with `enqueue` the HUD
also projects a queued `BacklogItem{PlanID, Labels:["mills-pattern-stamp"]}` into
the operator (`POST /api/mills/backlog`, `internal/hud/domain/mills/enqueue.go`),
which the reconciler picks up and runs through the existing pipeline — the Mills
pipeline is pattern-agnostic, so `PlanID` is the whole contract (the agent
resolves slices via `agent_plan_get`). Enqueuing kicks off the autonomous loop,
so it is **admin-token gated** (the bare stamp is not).

HUD page: **Mills → Patterns** lists the approved catalog and renders a materials
form generated from each pattern's `materials_schema`, then stamps via the
endpoints above and shows the resulting `plan_id` + required tools. Selecting a
pattern also exposes its **instruction book** — a collapsible pins / gauge /
composed-engrams / deploy-contract / provenance section — so an operator can
read exactly what a stamp closes before supplying materials
(`internal/hud/frontend/src/lib/components/mills/PatternsPanel.svelte`).

## Authoring patterns (the taste gate)

A pattern's `status` controls whether the factory will stamp it. The lifecycle is
`candidate → approved → deprecated`, and the Mills rails (A1) and front door (B1)
offer **approved** patterns by default.

- **Register** a pattern with `agent_pattern_add` — it starts as `candidate`.
- **Earn approval** one of two ways:
  - *Human curation* — `agent_pattern_promote {pattern_id, to_status: "approved", force: true}`.
    This is the explicit "we find this tasteful" judgement (how the seed
    `pattern-go-rest-service` was approved from its kill-test, with zero shipped
    instances).
  - *Proven by use* — each time a stamped instance merges green, the stamp/merge
    path calls `agent_pattern_record_instance {pattern_id, mr_ref, repo, repo_root}`,
    which increments `instances_shipped_green` and **auto-promotes** the candidate
    to `approved` once it reaches `PatternApprovalThreshold` (default 1). The same
    call also runs the A2 engram population pass (verify composed engrams + surface
    candidates) — best-effort, so an engram hiccup never undoes the promotion.
- **Retire** a drifted pattern with `agent_pattern_promote {pattern_id, to_status: "deprecated"}`.

So taste is both editorial (a human can approve directly) and empirical (a
pattern that keeps shipping green earns its place automatically).

## Status & roadmap

Plan: `plan-pattern-loom-mills` (resolve live via `agent_plan_get`).

| Slice | State |
|---|---|
| S0 — Pattern catalog | shipped |
| S1 — stamp() verb | shipped (core); **Mills BacklogItem projection seam shipped** (opt-in `enqueue`, admin-gated); **tools-manifest enforcement shipped** (`available_tools` → stamp aborts if a required tool is absent); the full autonomous pipeline run (stamp→merged MR→deploy) remains a deliberate kill-test |
| A1 — pattern-constrained council editor | shipped |
| B1 — front door | shipped — CLI + HUD REST + HUD page (Mills → Patterns) |
| A2 — populate engrams from green stamps | shipped (core) — seed + verify-on-green-stamp + candidate surfacing; live merged-instance checkout wiring deferred with the Mills e2e stamp |
| B2 — pattern authoring + taste gate | shipped (`agent_pattern_promote` / `_record_instance`) |

The new `agent_pattern_*` tools require an `mcp-agent-context` redeploy to go live.
Rationale and evidence: `.loom/164-brainstorm-pattern-loom-mills-2026-06-28.md`,
`.loom/kill-test-pattern-go-rest-service-2026-06-28.md`.

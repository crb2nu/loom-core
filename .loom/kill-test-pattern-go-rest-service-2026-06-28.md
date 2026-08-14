# Kill-test: pattern-go-rest-service stamp (Mills Pattern Loom S1)

**Date**: 2026-06-28 · **Plan**: `plan-pattern-loom-mills` slice S1 · **Verdict**: **PASS (lean form)**

## What was tested

**Assumption (S1, riskiest)**: a vetted Pattern + typed materials can be stamped into a buildable, contract-satisfying result **with zero per-instance human architecture decisions** — i.e. a Pattern is a real "instruction book anyone can follow," not a suggestion.

**Method (isolation by design)**: the assumption is about *stamping determinism + faithful execution*, which is separable from Mills' merge/deploy plumbing (separately proven by A2 on 2026-06-24, and under active repair this week). So the lean kill-test exercised only the new risk:

1. Authored a *tightened* `pattern-go-rest-service` that **closes** every load-bearing architecture axis left open by the prose `go-service-scaffold` skill (transport, storage, deps, ID scheme, config, shutdown, layout, domain type, full API contract).
2. Filled synthetic **materials**: a `widget` service (entity `Widget{name,quantity}`, in-memory storage, no auth).
3. Handed the stamped instruction book to a **fresh agent with zero conversation context**, under three rules: stdlib-only, make no architecture decision beyond the spec, and record every unspecified choice in `GAPS.md` (classified ARCHITECTURE vs COSMETIC).
4. Measured against an **independent black-box gauge** (`scratchpad/gauge.sh`) the implementer never saw.

## Evidence

**Faithful execution — gauge 10/10 (PASS):**
```
✅ go build ./cmd/widget      ✅ go test ./...
✅ GET /healthz -> 200        ✅ POST /widgets -> 201  (echoes name, quantity, server id a6473ba0…)
✅ GET /widgets/{id} -> 200   ✅ get round-trips entity   ✅ GET unknown id -> 404
GAUGE: 10 passed, 0 failed → VERDICT: GAUGE_PASS
```

**Negative search — zero unrequested architecture (PASS):**
- `go.mod`: no `require` block; every import stdlib or internal.
- File inventory matches the spec exactly — no extra files.
- Feature scan (prometheus/metrics/jwt/oauth/middleware/sql/postgres/chi/gin/echo/zerolog): only hit was `signal` (SIGINT/SIGTERM), which the pattern requested. No DB, router lib, auth, or logging lib invented.

**Determinism — NOT literally zero, but bounded (the key nuance):** the follower reported **7 architecture-class gaps**, all at *boundaries the v0.1 pattern under-specified*, none touching load-bearing architecture:
1. error-response body shape (chose `{"error":...}`)
2. `readyz` body (chose empty JSON)
3. where DI/composition lives (chose `internal/server.Run()`)
4. constructor + route-registration API names (`NewMemoryRepo`, `Handler.Routes()`)
5. validation-error mechanism (sentinel `ErrValidation` + `errors.Is`)
6. JSON-decode-failure → 400 mapping
7. unspecified 500 path for non-validation create errors

## Conclusion

The assumption **holds**: a Pattern that pins the load-bearing axes makes the residual freedom collapse to a **small, enumerable set of boundary decisions** — none of which is architecture-shaping. A fresh, context-free agent produced a building, 10/10-gauge-passing service with no invented deps and no scope creep.

**Design payoff (feeds S0)** — to reach *literally* zero improvisation, the Pattern object must additionally pin:
- a **standard error envelope** (one shape for all non-2xx bodies),
- a **body for every endpoint** (incl. secondary ones like `readyz`),
- a **wiring/composition convention** (where DI lives; constructor naming),
- a **pinned error model** (sentinel vs typed) for the service↔handler seam,
- a **complete status-code table** including failure paths (the 500s).

These become required fields of the S0 `Pattern` schema.

**Out of scope (deliberate)**: full Mills e2e (plan_slice→spawn→MR→merge→deploy) was not exercised — that plumbing is proven separately (A2) and under repair. The full-pipeline stamp is the integration step, to run once the merge path is stable.

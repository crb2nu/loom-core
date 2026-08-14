# Mills async spins — operator runbook

Async spins let the Mills Spinning Room drive a **slow frontier frame** (e.g.
`jacquard` → `claude-opus-4-8`) without holding an HTTP connection open for the
whole synthesis. The request returns `202` immediately with a `spin_id`; the
operator spins in a background goroutine; the resulting draft plan lands in the
agent-context Plan Store.

## Why it exists

A synchronous spin (`POST /api/mills/spin`) holds the connection for the full
model call + plan-store write. `!924` stopped that from *hanging*, but a
frontier frame legitimately runs minutes — longer than the **client-facing proxy
timeout** (Cloudflare returns HTTP 524 after ~100s of origin silence), which is
unreachable from the operator. Async is the durable fix: nothing holds the
connection, so frame latency no longer matters.

The synchronous `/api/mills/spin` still exists (fine for fast local frames and
CLI/tests). The HUD "Spin a plan" dialog uses the async path for **every** frame.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/mills/spin/async` | admin | Accept a spin; returns `202 {spin_id, status, status_url}` |
| `GET` | `/api/mills/spin/runs` | open | List recent spin runs (newest first; `?limit=N`) |
| `GET` | `/api/mills/spin/runs/{id}` | open | One spin run's status + result |

The request body is identical to `/api/mills/spin`: `{brief, frame}` for a
single frame, or `{brief, frames:[…]}` for a competitive spin, plus optional
`priority`, `project`, `namespace`.

### Status lifecycle

`pending` → `running` → one of `succeeded` | `failed` | `timeout`.

- `succeeded`: `plan_ids` holds the authored draft plan id(s) — 1 for a plain
  spin, N for a competitive one. A partial competitive failure still reports
  `succeeded` with a summary in `error`.
- `failed`: `error` carries the reason (disabled room, unknown frame,
  editor/author error, or operator shutdown).
- `timeout`: the spin exceeded its per-request budget (10m; retryable).

## Trigger a spin (operator)

From the HUD: **Plans panel → ⟳ Spin a plan**. The dialog returns instantly and
a pulsing "N spinning…" indicator tracks in-flight spins; the draft appears on
the board when it lands.

By hand (needs the admin token — secret `loom-mills-admin`, key `admin-token`,
namespace `loom-mills`):

```bash
TOKEN=$(kubectl -n loom-mills get secret loom-mills-admin \
  -o jsonpath='{.data.admin-token}' | base64 -d)

curl -sS -X POST https://mills.flexinfer.ai/api/mills/spin/async \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"brief":"…","frame":"jacquard","priority":"P2"}'
# → {"spin_id":"spin-YYYYMMDD-HHMMSS-xxxxxxxx","status":"pending","status_url":"…"}
```

Poll it (open read, no token):

```bash
curl -sS https://mills.flexinfer.ai/api/mills/spin/runs/<spin_id> | jq .status
```

## Recovering a stuck / orphaned run

The operator is a **singleton** pod, so its in-memory spin goroutines die with
the pod on every deploy/restart. On startup the operator **orphan-sweeps** any
`pending`/`running` row to `failed` (`error: "orphaned: operator restarted…"`),
so a run never appears permanently stuck.

Key point: the **durable artifact is the Plan Store draft**, not the `spin_runs`
row. If a spin authored its draft plan before the pod died, the plan is still in
agent-context (`agent_plan_list`/`agent_plan_get`) even though its `spin_runs`
row reads `failed(orphaned)`. To find recently-spun drafts:

```
agent_plan_list{project: "services/loom-core", phase: "draft"}
```

Nothing to "un-stick" — just re-run the spin if no draft landed.

## Concurrency

Bounded by a semaphore (default **2** concurrent spins — frontier frames are
slow + costly). Override with `LOOM_MILLS_SPIN_MAX_CONCURRENT` on the operator
Deployment. A POST is rejected with `429` once `pending+running ≥ 32`.

## Observability

- **`mills_async_spins_total{outcome}`** — async lifecycle: `accepted` on the
  202, then `succeeded`/`failed`/`timeout` at completion. `accepted` minus the
  terminal sum is the current in-flight count.

  ```promql
  # async spin success rate over 1h
  sum(rate(mills_async_spins_total{outcome="succeeded"}[1h]))
    / sum(rate(mills_async_spins_total{outcome=~"succeeded|failed|timeout"}[1h]))

  # rising timeout share → a frame outgrowing the 10m budget
  sum(rate(mills_async_spins_total{outcome="timeout"}[1h]))
  ```

- **`mills_spin_total{frame,outcome}`** — per-**frame** reliability (ok/error),
  fed by both sync and async spins. Use this to compare frames (opus vs fable5
  vs gpt-5.5): which frame actually yields usable drafts, and how often — the
  signal that decides whether a frame stays in `spinning_room.frames`.

  ```promql
  sum by (frame) (rate(mills_spin_total{outcome="ok"}[24h]))
    / sum by (frame) (rate(mills_spin_total[24h]))
  ```

## References

- Handlers: `cmd/loom-mills-operator/handlers_spin_async.go`
- Store: `pkg/mills/store/{dao_spin.go,migrations/007_spin_runs.sql}`
- Metrics: `pkg/mills/metrics.go` (`AsyncSpinsTotal`, `SpinsTotal`)
- Plan: `.loom/166-plan-mills-async-spins-2026-07-04.md`

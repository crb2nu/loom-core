# Harvest: push-based CI-failure notification (`libs/fi-gitlab-hookify`)

> **Status:** contract documentation for a future port. No code has been ported.
> **Source repo:** `libs/fi-gitlab-hookify` (Python), archived 2026-08-15 as part of the
> 2026-08 portfolio uplift. Citations are `path:line` relative to that repo at its final
> commit on `main`.
> **Why this document exists:** the repo goes read-only. The design — not the code — is the
> asset, and it is strictly better on latency than loom-core's current poll-based CI flow.
> This file exists so a port needs no archaeology.

## 1. Why this was harvested

loom-core's CI-failure flow is **poll-based** today: `poll_pipeline` and `pipeline_summary`
in `cmd/mcp-gitlab`, plus the `ci-failure-recovery` skill. The agent burns a blocking poll
waiting for GitLab to change state.

hookify is **push-based**: GitLab posts on state change, the receiver writes a status file,
and each agent vendor reads it through a thin shim at a natural turn boundary. On latency
that is strictly better — the only question is what to keep.

The repo carried **102 tests** (verified, see §6). They are the real specification and the
most valuable thing here after the vendor-shim deltas.

## 2. Repo shape

| Aspect | Value | Cite |
|---|---|---|
| Distribution | `flexinfer-gitlab-hookify` v0.1.0 | `pyproject.toml:6-7` |
| Import package | `gitlab_hookify` (src layout) | `pyproject.toml:50-51` |
| Python | `>=3.11,<3.14` | `pyproject.toml:10` |
| Console script | `gitlab-hookify = gitlab_hookify.cli:app` | `pyproject.toml:44-45` |
| Runtime deps | fastapi, uvicorn, pydantic v2, httpx, typer, prometheus-client, structlog | `pyproject.toml:24-32` |
| mypy | `strict = true` | `pyproject.toml:72-77` |

```
src/gitlab_hookify/
  config.py            # Settings from env
  cli.py               # typer: status | clear | migrate | serve
  models.py            # pydantic: status models + GitLab payload models
  webhook/{app,auth,handlers}.py
  status/{base,manager,manager_v2}.py
  observability/{logging,metrics,middleware}.py
hooks/
  core.py              # shared, agent-agnostic
  ci_notify.py         # Claude Code shim
  codex_notify.py      # Codex CLI shim
  gemini/{ci_notify.py, gemini-extension.json, GEMINI.md}
tests/                 # 102 tests
k8s/                   # kustomize + SOPS
```

Deployed to namespace `ci` behind ingress `gitlab-ci-hookify.flexinfer.ai`, exposing only
`/hook`, `/healthz`, `/status` (`k8s/ingress.yaml:19-38`). The Deployment runs a bare
`python:3.12-slim` that `pip install`s a wheel from a ConfigMap at start
(`k8s/deployment.yaml:24-39`) — there is no purpose-built image.

> **Deployment smell worth preserving as a lesson:** the status volume is an `emptyDir`
> (`k8s/deployment.yaml:71-72`) with `CI_STATUS_PATH: /data/ci-status.json`
> (`k8s/configmap.yaml:10`), so cluster-side state is lost on pod restart. The local agent
> path (`~/.claude/…`) and the cluster path are **different stores**; the hook bridges them
> over HTTP (`hooks/core.py:105-122`).

## 3. The `~/.claude/ci-status.json` status-file contract

The load-bearing artifact. Two storage generations coexist behind one Protocol.

### 3.1 v1 schema (single file)

Root model `CIStatus` (`models.py:86-91`).

| Field | Type | Default | Meaning | Cite |
|---|---|---|---|---|
| `version` | str | `"1.0.0"` | Schema version. Never bumped; **v2 does not set it.** | `models.py:89` |
| `updated_at` | datetime (UTC) | `now()` | Whole-file write timestamp | `models.py:90`, `manager.py:60` |
| `projects` | `dict[str, ProjectStatus]` | `{}` | Keyed by GitLab `path_with_namespace` | `models.py:91`, `handlers.py:38` |

- `ProjectStatus` (`models.py:78-83`): `project_id: int`, `web_url: str`,
  `branches: dict[str, BranchStatus]` — keyed by branch ref.
- `BranchStatus` (`models.py:69-75`): `pipeline: PipelineInfo`, `jobs: list[JobInfo]`,
  `failed_jobs: list[str]` (job **names**), `updated_at: datetime`.
- `PipelineInfo` (`models.py:54-66`): `id`, `status`, `web_url`, `ref`, `started_at`,
  `finished_at`, `commit_sha` (**truncated to 12 chars**, `handlers.py:76`),
  `commit_message` (**first line, ≤80 chars**, `handlers.py:59-61`).
- `JobInfo` (`models.py:38-51`): `id`, `name`, `stage`, `status`, `duration`,
  `failure_reason`, `web_url`, `started_at`, `finished_at`.

Enums (`models.py:9-36`) are `StrEnum` with `model_config = {"use_enum_values": True}`, so
they serialize as bare strings. `PipelineStatus` has 11 members including
`waiting_for_resource` and `preparing`; `JobStatus` has 8 and lacks those.

### 3.2 Example payload

```json
{
  "version": "1.0.0",
  "updated_at": "2026-08-15T12:00:00.123456+00:00",
  "projects": {
    "group/my-project": {
      "project_id": 100,
      "web_url": "https://gitlab.example.com/group/my-project",
      "branches": {
        "main": {
          "pipeline": {
            "id": 12345,
            "status": "failed",
            "web_url": "https://gitlab.example.com/group/my-project/-/pipelines/12345",
            "ref": "main",
            "started_at": "2025-01-05T10:00:00+00:00",
            "finished_at": "2025-01-05T10:05:00+00:00",
            "commit_sha": "abc123def456",
            "commit_message": "Add new feature"
          },
          "jobs": [
            {
              "id": 222,
              "name": "test",
              "stage": "test",
              "status": "failed",
              "duration": 120.5,
              "failure_reason": "script_failure",
              "web_url": "https://gitlab.example.com/group/my-project/-/jobs/222",
              "started_at": null,
              "finished_at": null
            }
          ],
          "failed_jobs": ["test"],
          "updated_at": "2026-08-15T12:00:00.123456+00:00"
        }
      }
    }
  }
}
```

`commit_sha` is exactly 12 chars and `commit_message` is first-line-only — both are
deliberate truncations, not accidents of the fixture.

### 3.3 v2 schema (per-project files)

Enabled by `CI_STATUS_USE_SCALABLE_STORAGE=true`. Directory contract
(`manager_v2.py:37-47`):

```
~/.claude/ci-status/
├── index.json                    # {project_path: ISO8601 last_updated}
├── projects/
│   └── group__project.json       # bare ProjectStatus (NO version/updated_at wrapper)
└── *.lock                        # flock sidecars
```

Key encoding is `project_path.replace("/", "__") + ".json"` (`manager_v2.py:65-67`), reversed
by `f.stem.replace("__", "/")` (`manager_v2.py:289`, `:299`). **This is lossy** if a real
project path contains a literal `__`. A port should use URL-encoding or a hash.

The per-project file is a **bare `ProjectStatus`** (`manager_v2.py:176`), not the `CIStatus`
envelope; `read_all()` reassembles the envelope (`manager_v2.py:295-303`).

### 3.4 Write atomicity and locking

Both generations use **temp-file + `rename()`** in the same directory, so writes are
POSIX-atomic: v1 at `manager.py:63-66`, v2 at `manager_v2.py:107-110`.

Locking is where they diverge, and it is the entire reason v2 exists:

- **v1 uses only `asyncio.Lock`** (`manager.py:31`, held across read-modify-write at `:85`,
  `:129`, `:209`, `:228`). This is **in-process only** — multiple uvicorn workers lose
  updates. Documented at `status/__init__.py:44-46`.
- **v2 uses `fcntl.flock`** with shared/exclusive modes (`manager_v2.py:73-91`),
  cross-process safe, on a `.lock` sidecar rather than the data file — so the atomic rename
  cannot invalidate the lock.

`read_all()` / `prune_stale()` glob `*.json` (`manager_v2.py:260`, `:298`), which safely
excludes in-flight `.tmp` and `.lock` files.

### 3.5 Staleness — two independent TTLs

Do not conflate these.

| TTL | Value | Where | Effect |
|---|---|---|---|
| Read-side freshness | **30 minutes** | `hooks/core.py:151`, applied `:271` | Status older than 30 min is treated as *no failure*. Silent suppression. |
| Write-side pruning | **24 hours** | `manager.py:217`, `manager_v2.py:247` | Deletes stale branch entries and empty projects. |

`is_stale` **fails closed toward "stale"**: `None`, `""`, and unparseable timestamps all
return `True` (`core.py:161-162`, `:173-174`; tests `test_hookify.py:150-157`). It normalizes
a trailing `Z` to `+00:00` (`:165`) and assumes UTC for naive timestamps (`:168-169`).

The 24h prune has **no scheduler** — it only runs via `gitlab-hookify clear --older-than`
(`cli.py:109-123`). A port needs a background sweeper or it never prunes.

### 3.6 Keying and success cleanup

- Keyed by `(project_path, branch)`. **Only one pipeline per branch is retained** — a new
  pipeline ID replaces the entry and **resets `jobs` to `[]`** (`manager.py:101-105`;
  `manager_v2.py:169-171`). Concurrent pipelines on the same branch clobber each other.
- Job upsert is by `job.id` (`manager.py:151-158`), so a retried job with a new ID appends
  rather than replaces.
- **There is no delete-on-success.** `failed_jobs` is fully recomputed from all jobs on every
  update (`manager.py:161-163`; `manager_v2.py:223-225`), so when jobs go green the list
  empties and `check_ci_failures` short-circuits (`core.py:274-276`). Data is only *removed*
  by the 24h prune or an explicit `clear`.

## 4. Webhook receiver shape

FastAPI + uvicorn; app object `gitlab_hookify.webhook.app:app` (`webhook/app.py:33-37`),
default port 8080 (`cli.py:207`).

| Method | Path | Purpose | Codes | Cite |
|---|---|---|---|---|
| GET | `/` | service info incl. `storage_type`, `webhook_configured` | 200 | `app.py:44-60` |
| GET | `/healthz` | k8s probe | 200 | `app.py:63-66` |
| GET | `/metrics` | Prometheus text | 200 / **404 if `METRICS_ENABLED=false`** | `app.py:69-82` |
| **POST** | **`/hook`** | GitLab receiver | 200 / **401** | `app.py:85-123` |
| GET | `/status` | full `CIStatus` dump | 200 | `app.py:126-134` |
| GET | `/status/{project_path:path}` | one project | 200 / 404 | `app.py:137-154` |

> `/status` is **unauthenticated** and dumps every project. That is how the agent hooks read
> remote state (`core.py:105-122`), and it is publicly exposed through the ingress
> (`k8s/ingress.yaml:33-38`). **Do not port this as-is.**

### 4.1 Secret verification — fail-closed

`X-Gitlab-Token` header (`app.py:88`), checked by `validate_token` (`auth.py:9-30`):

```python
if not expected_token:          # auth.py:21-23
    logger.warning("No webhook secret configured, rejecting all requests")
    return False
if not provided_token:          # auth.py:25-27
    return False
return hmac.compare_digest(provided_token, expected_token)   # auth.py:30
```

An empty `WEBHOOK_SECRET` **rejects everything**, and the comparison is constant-time. Both
are pinned by tests (`test_webhook.py:26-28`, `:34-45`).

`X-Gitlab-Event` is bound (`app.py:89`) but **only logged** (`app.py:112`) — routing is driven
by the body's `object_kind`.

### 4.2 Events consumed

Dispatch at `app.py:114-123`:

| `object_kind` | GitLab UI name | Handler |
|---|---|---|
| `"pipeline"` | Pipeline events | `handle_pipeline_event` (`handlers.py:25`) |
| `"build"` | Job events | `handle_job_event` (`handlers.py:97`) |
| anything else | — | `200 {"skipped": true, "reason": …}` |

Merge Request Hook is **not** consumed. Setup enables exactly Pipeline + Job events
(`README.md:57-59`).

Fields actually read — pipeline (`handlers.py:34-42`, `:55-76`):
`object_attributes.{id, ref, sha, status, created_at, finished_at}`,
`project.{id, path_with_namespace, web_url}`, `commit.message`. Job (`handlers.py:106-116`):
`project.{path_with_namespace, web_url}`, `ref`, `build_id`, `build_name`, `build_stage`,
`build_status`, `build_duration`, `build_failure_reason`, `pipeline_id`.

Both `web_url` values are **synthesized**, not read from the payload:
`f"{project.web_url}/-/pipelines/{id}"` (`handlers.py:72`) and `…/-/jobs/{id}`
(`handlers.py:144`). Unknown enum values degrade to `PENDING` with a warning rather than
500ing (`handlers.py:63-67`, `:131-135`).

### 4.3 Filtering — the single most important gotcha

**The receiver does not filter at all.** Every pipeline status (`running`, `pending`,
`success`, `failed`, …) is written to the status file.

Filtering happens entirely on the **read** side, and it keys off *failed jobs*, not pipeline
status:

```python
failed_jobs = branch_status.get("failed_jobs", [])   # hooks/core.py:274
if not failed_jobs:
    return None, None
```

`failed_jobs` is only ever populated by `update_job` (`manager.py:161-163`).

> **Consequence: if only Pipeline events are enabled in GitLab, a `failed` pipeline produces
> zero notifications.** Job events are mandatory. A port should either filter server-side on
> pipeline status, or make the read-side check `pipeline.status == "failed" OR failed_jobs`.

### 4.4 Idempotency, ordering, and error-handling gaps

No event-ID ledger, no replay window, no nonce. Idempotency is *structural*: repeated
webhooks overwrite the same `(project, branch)` key, and job upsert is by `job.id`.

Ordering protection is a **single guard**: a job event whose `pipeline_id` does not match the
stored pipeline is **silently dropped** (`manager.py:144-148`; `manager_v2.py:209-211`). That
is the only thing preventing a late job event from a superseded pipeline corrupting the
current one. Port it.

Gaps a port must fix:

- Body parsed with a bare `await request.json()` (`app.py:109`), no try/except — a non-JSON
  body raises rather than returning a clean 400.
- A structurally-wrong-but-valid-JSON pipeline payload reaches `PipelineInfo(id=None)`
  (`handlers.py:69-70`) → pydantic `ValidationError` → 500.
- **No request body size limit.** (loom-core already caps at 1 MB,
  `internal/hud/domain/webhook/handlers.go:35`.)
- Prometheus `project` is an unbounded-cardinality label (`observability/metrics.py:33-43`).

## 5. Per-vendor notify shims

`hooks/core.py` holds all agent-agnostic logic; each vendor file is a thin adapter. There is
no separate "claude" directory — **`hooks/ci_notify.py` is the Claude shim.**

### 5.1 Shared resolution order (`hooks/core.py:230-279`)

1. Derive `project_path` from `git remote get-url origin` (`:44-80`; handles
   `git@host:g/p.git`, `https://`, `http://`, nested groups, missing `.git`) and `branch`
   from `git rev-parse --abbrev-ref HEAD` (`:23-41`). Both subprocess calls have a **5 s
   timeout**.
2. Bail if either is `None` (`:244-245`).
3. Try remote `GET $CI_STATUS_URL`, **3 s timeout** (`:105-122`, `:250-252`).
4. Else v2 per-project file (`:255-256`).
5. Else v1 single file (`:258-262`).
6. Bail if the branch entry is missing, **stale (>30 min)**, or `failed_jobs` is empty
   (`:267-276`).
7. Render Markdown via `format_failure_message` (`:177-227`).

The message body is identical across all three vendors: `## CI Pipeline Failed`,
project/branch/pipeline-link/commit, then `### Failed Jobs:` bullets formatted
`**name** (stage): reason (Ns) [View logs](url)` (`core.py:199-227`).

### 5.2 The vendor deltas — this is the load-bearing knowledge

| | Claude Code | Codex CLI | Gemini CLI |
|---|---|---|---|
| File | `hooks/ci_notify.py` | `hooks/codex_notify.py` | `hooks/gemini/ci_notify.py` |
| Transport in | stdin JSON | stdin **or** `argv[1]` | stdin JSON |
| Transport out | stdout JSON | desktop toast + stderr | stdout JSON |
| Context-injection key | `systemMessage` (`:70`) | **none** | `message` (`:83`) |
| Trigger events | `Stop`; `PostToolUse`+Bash+`git push` (`:33-47`) | turn completion | `AfterTool`, `SessionEnd` |
| Event-name stability | stable | absent | 3 aliases per event |
| Message length limit | none | **497 chars** (`:33-34`) | none |
| Error surfacing | in-band warning | stderr only | in-band warning |
| Ships a context doc | no | no | **yes (`GEMINI.md`)** |

**Claude** — must **always print valid JSON and always `exit(0)`**, even on a no-op
(`:59-60`, `:64-65`) and on exception (`:72-77`, in a `finally`). Claude's hook runner treats
a non-zero exit or non-JSON stdout as a hook failure that blocks the turn, so exceptions are
converted into an in-band `systemMessage` warning rather than surfaced as a crash.

**Codex** — the biggest delta: **Codex has no context-injection return contract.** Its
`notify` hook is fire-and-forget, so the only way to reach the human is an OS toast:
`osascript -e 'display notification …'` on macOS, `notify-send -u critical` on Linux, Windows
unimplemented (`:36-47`). Consequences: payload arrives on stdin *or* `argv[1]` depending on
tty (`:82-91`); the toast body is hard-truncated to 497 chars because both notifiers mangle
long bodies; both subprocess calls use `timeout=5` + `capture_output=True` so a missing
`notify-send` cannot hang or pollute the agent (`:39`, `:42-46`); it **defaults to firing on
every turn** (`:74`) because Codex does not reliably surface `type`/`command`, only
*rejecting* known non-completion `event_type` values (`:66-67`); errors go to stderr only,
deliberately — "silent failure — don't break Codex workflow" (`:112-114`).

**Gemini** — different return key (`message`, not `systemMessage`) plus pervasive instability
the shim absorbs: event field is `event` *or* `hook_type` (`:39`); event values are
`SessionEnd`/`session_end` and `AfterTool`/`after_tool`/`tool_response` (`:42`, `:46`); three
shell-tool aliases `shell` / `execute_command` / `run_command` (`:51`); `tool_input` may be a
dict *or* a bare string with key `command` *or* `cmd` (`:52-56`). Gemini extensions also ship
a **manifest plus a context doc** — `gemini-extension.json` declares a `context` array
pointing at `GEMINI.md` (`:12-17`), which preloads a failure-reason glossary
(`script_failure`, `stuck_or_timeout_failure`, `runner_system_failure`) into the agent
(`GEMINI.md:18-23`). No other vendor has that affordance. The shim also documents, but never
uses, a third-state protocol: `{"action": "STOP_EXECUTION"}` aborts the agent (`:14-17`).

## 6. Configuration

All env-driven through `Settings.__init__` (`config.py:26-59`), lazily memoized
(`config.py:62-75`) with a `reload_settings()` test escape hatch (`:78-86`).

| Variable | Default | Effect | Cite |
|---|---|---|---|
| `WEBHOOK_SECRET` | `""` | Expected `X-Gitlab-Token`. **Empty ⇒ all requests 401.** | `config.py:28`, `auth.py:21-23` |
| `CI_STATUS_USE_SCALABLE_STORAGE` | `"false"` | `"true"` selects v2 per-project storage | `config.py:31-33` |
| `CI_STATUS_AUTO_MIGRATE` | `"true"` | On v2 startup, migrate a v1 file and back it up to `.json.bak` | `config.py:34`, `manager_v2.py:331-335` |
| `CI_STATUS_V1_STATUS_PATH` | → `CI_STATUS_PATH` → `~/.claude/ci-status.json` | v1 path (two-level fallback) | `config.py:37-43` |
| `CI_STATUS_PATH` | — | legacy alias; `/data/ci-status.json` in k8s | `config.py:41` |
| `CI_STATUS_V2_BASE_PATH` | `~/.claude/ci-status` | v2 base dir | `config.py:45-46` |
| `LOG_LEVEL` | `"info"` | structlog level | `config.py:49` |
| `LOG_FORMAT` | `"json"` | `"console"` ⇒ colorized dev renderer | `config.py:50` |
| `METRICS_ENABLED` | `"true"` | `false` ⇒ `/metrics` 404s | `config.py:53` |
| `DEBUG_MODE` | `"false"` | debug flag for hooks | `config.py:56` |
| `HOOK_DEBUG_FILE` | `~/.claude/hookify-debug.log` | **dead config** — referenced nowhere else | `config.py:57-59` |
| `CI_STATUS_URL` | `https://gitlab-ci-hookify.flexinfer.ai/status` | **hook-side only**, read straight from `os.environ`, not part of `Settings` | `hooks/core.py:20` |

Boolean parsing is uniformly `os.getenv(...).lower() == "true"`, so `1`, `yes`, and
`"TRUE "` (trailing space) are all **false**.

## 7. Tests — 102 verified

`grep -rc "def test_" tests/`; no `parametrize` anywhere, so defined == collected == 102.

| File | Count | Lines | Covers |
|---|---:|---:|---|
| `tests/test_hookify.py` | 24 | 270 | git context parsing, staleness, Claude trigger matrix, message formatting |
| `tests/test_status_v2.py` | 23 | 506 | per-project storage, locking, naming, migration, index |
| `tests/test_webhook.py` | 20 | 292 | auth, endpoints, handler integration |
| `tests/test_models.py` | 18 | 194 | pydantic models + GitLab payload parsing |
| `tests/test_status.py` | 17 | 342 | v1 storage |
| **Total** | **102** | 1729 | |

### Tests that encode non-obvious contracts — the port spec

**Security**

- `test_webhook.py:26-28` `test_empty_expected_token` — an **unconfigured secret must
  reject**, not allow.
- `test_webhook.py:34-45` `test_timing_safe` — constant-time compare is a contract, not an
  implementation detail.

**Race conditions / ordering**

- `test_status.py:249`, `test_status_v2.py:257-281` `test_job_update_wrong_pipeline` — a job
  event with a mismatched `pipeline_id` **must be silently ignored**, neither applied nor
  errored. The only defense against out-of-order delivery from a superseded pipeline.
- `test_status.py:113`, `test_status_v2.py:153-198` `test_new_pipeline_resets_jobs` — a new
  pipeline ID on the same branch **wipes the job list**. Without it, a rerun shows stale
  failures forever.
- `test_status.py:182` — same `job.id` **replaces** rather than appends.
- `test_status_v2.py:232-255`, `test_status.py:219` — `failed_jobs` is **recomputed** every
  update, never incrementally appended. This is the de-facto success-cleanup mechanism.

**Malformed input**

- `test_hookify.py:120-126` — a corrupt status file must degrade to `{}`, never raise into
  the agent's turn. Same at the manager layer (`test_status.py:52`, `manager.py:49-51`).
- `test_hookify.py:150-157` — `"not-a-timestamp"`, `""`, and `None` **all count as stale**,
  i.e. fail toward "don't notify".
- `test_webhook.py:168-177` — an unsupported `object_kind` is a **200 with
  `skipped: true`**, not a 4xx. GitLab retries and eventually disables hooks that return
  errors, so this must not be a failure code.

**Storage / key encoding**

- `test_status_v2.py:83-92` — pins the lossy `/` ↔ `__` mapping, including nested groups.
- `test_status_v2.py:427-472` — migration **asserts the `.json.bak` backup exists**;
  migration must never destroy the source.

**Git context parsing** — `test_hookify.py:41-92` covers six remote formats (SSH, HTTPS,
HTTP, nested group, no `.git` suffix, git failure → `None`). **Port this table verbatim.**

**Trigger matrix** — `test_hookify.py:163-198`: `Stop` yes; `PostToolUse`+Bash+`git push`
yes; `PostToolUse`+Bash+`ls -la` no; `PostToolUse`+Edit no; `PreToolUse` no.

> **Harness quirk, for context:** `conftest.py:14-24` is an autouse fixture that unregisters
> every Prometheus collector between tests, because module-level `Counter`/`Gauge`
> definitions (`metrics.py:13-67`) otherwise raise duplicate-registration errors when
> `test_webhook.py:62-71` force-reimports modules to pick up new env vars. A Go port
> sidesteps this entirely; it is recorded only to explain the otherwise-baffling
> module-deletion dance.

## 8. Port notes for loom-core

### 8.1 Prior art already in loom-core

**Merged — `internal/hud/domain/webhook/`** (~1155 LOC incl. 531 LOC of tests):

| Route | Cite |
|---|---|
| `POST /api/webhook/gitlab` | `internal/hud/domain/webhook/webhook.go:30` |
| `POST /api/webhook/github` | `:31` |
| `GET /api/webhook/config` | `:32` |
| `GET /api/webhook/events` | `:33` |

It already does GitLab token verify + GitHub HMAC-SHA256 (`verify.go:14-41`), a 1 MB body cap
with clean 400s (`handlers.go:35-45`), a 100-entry in-memory event ring buffer
(`webhook.go:36-58`), `failed`-only filtering (`mapper.go:34-36`), branch resolution
preferring the MR source branch (`mapper.go:12-17`), routing to an **already-active agent on
that branch** before spawning a fresh one (`handlers.go:58-97`), and event broadcast on
`ci.pipeline.failure.routed` / `webhook.received` (`handlers.go:81`, `:103`). Config:
`WEBHOOK_INBOUND_ENABLED`, `WEBHOOK_GITLAB_SECRET`, `WEBHOOK_GITHUB_SECRET`
(`internal/daemon/hud_embed.go:284-285`), default **off** (`internal/hud/app.go:168`).

**Unmerged — branch `feat/bl-mills-ciwatch-webhook-events-20260809/impl` (MR !1560).** Adds
`cmd/loom-mills-operator/handlers_webhooks.go` and `pkg/mills/webhookbus/webhookbus.go`: a
bounded, non-blocking, drop-oldest in-process hint bus (`webhookbus.go:61-83`) with a
`mills_webhook_events_dropped_total` counter (`:19-22`), which
`pkg/mills/pipeline/dispatcher.go` uses to **wake** a CI watch instead of sleeping out a poll
interval. Its handler routes on the `X-Gitlab-Event` header (`handlers_webhooks.go:34-38`)
rather than `object_kind`.

> Its design comment is the key architectural lesson, and it **disagrees with hookify**:
> *"Payload state is deliberately omitted: consumers must re-read GitLab as the source of
> truth"* (`webhookbus.go:10-12`). The webhook is a **latency hint**, not a datastore.
> hookify instead makes the payload authoritative and persists it — which is what forces the
> entire staleness/TTL/prune apparatus in §3.5.

### 8.2 What is new vs. what exists

| hookify piece | loom-core status |
|---|---|
| `POST /hook` receiver, secret verify, 401 | exists |
| `failed`-only filtering | exists, and stricter (server-side) |
| Route-to-active-agent-on-branch | exists, and better; hookify has no equivalent |
| Poll-loop wake on webhook | exists but **unmerged** (MR !1560) |
| Body cap / clean 400s | exists; hookify lacks both |
| **Job Hook (`object_kind: "build"`) ingestion** | **NEW** — and per §4.3 it is the only thing that populates `failed_jobs` |
| **Persistent per-project/branch status store** | **NEW** — loom-core has a volatile 100-entry ring buffer |
| **`~/.claude/ci-status.json` file contract** | **NEW** — needed only if out-of-process agent hooks must read it |
| **Per-vendor notify shims** | **NEW** — §5 is the load-bearing knowledge |
| **Client-side git-context detection** | **NEW** — loom-core learns branch from presence records |
| Unauthenticated `/status` read API | **do not port** |

### 8.3 Recommendations, in priority order

1. **Fix the fail-open secret.** `verifyGitLabToken` returns `true` when the configured
   secret is empty (`internal/hud/domain/webhook/verify.go:15-17`). hookify fails **closed**
   (`auth.py:21-23`, pinned by `test_webhook.py:26-28`). Port the hookify semantics, or at
   minimum refuse to start with `WEBHOOK_INBOUND_ENABLED=true` and an empty secret. *(Filed
   separately as a security follow-up — this is a live defect, not a port task.)*
2. **Add Job Hook ingestion.** Extend `GitLabPipelineEvent` (`types.go:6-26`) with a
   build-event sibling carrying `build_name` / `build_stage` / `build_failure_reason` /
   `build_duration`. Without it a ported notification says "pipeline failed" where hookify
   says ``test`` (test): script_failure (120.5s) [View logs]``.
3. **Keep webhook-as-hint, not webhook-as-truth.** Adopt the `webhookbus.go:10-12` stance and
   re-read GitLab via the existing `mcp-gitlab` client. This deletes the entire
   staleness/TTL/prune surface (§3.5) and the v1/v2 storage split.
4. **Keep the pipeline-ID mismatch guard regardless** (`manager.py:144-148`) and its tests.
   Out-of-order delivery from a superseded pipeline is real even for a hint bus.
5. **Merge or explicitly supersede MR !1560** — it is the highest-leverage latency win
   already written, turning `poll_pipeline`'s 5–10 s polling
   (`cmd/mcp-gitlab/pipelines.go:406-427`) into an event-driven wake.
6. **Do not port the `__` key encoding** (`manager_v2.py:65-67`); use URL-encoding or a hash.
7. **Bound the `project` metric label** (`observability/metrics.py:33-43`).
8. **If external agent shims are still needed**, port §5's three adapters as thin Go binaries,
   or keep the Python scripts pointed at a loom-core HTTP endpoint via `CI_STATUS_URL`
   (`hooks/core.py:20`) — that indirection already exists and is the cheapest bridge.

## 9. Provenance

- Harvest decision: `.loom/local/portfolio-uplift-2026-08/30-synthesis.md` §5 item 9
  ("Push-based CI failure notification"), operator-approved 2026-08-15.
- Source repo archived after this document merged.

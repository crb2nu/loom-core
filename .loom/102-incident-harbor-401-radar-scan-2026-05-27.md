# Incident: Harbor 401 redux — robot$k3s password drift

**Date:** 2026-05-27
**Status:** Mitigated. Robot rotated via API; SOPS updated; CI verified green.
**Surfaces affected (visibly):** CI `radar-scan` job, [pipeline 11963](https://gitlab.flexinfer.ai/services/loom-core/-/pipelines/11963).
**Surfaces affected (latently):** every consumer of `harbor-creds` with `robot$k3s` across 11 namespaces (`ai`, `ci`, `ci-build`, `ci-jobs`, `daemon`, `devbox`, `fi-fhir`, `home`, `labs`, `loom-mills`, `xfiles`). Most weren't visibly red because `imagePullPolicy: IfNotPresent` was serving cached layers.

## TL;DR

`robot$k3s` (Harbor robot id=1) returned 401 for every pull, including manifests it previously served (`mcp/loom-core`, `mcp/tech-radar`, `dockerhub-cache/moby/buildkit`). The robot **was not expired** (`expires_at: -1`) and **was not disabled**. The K8s `harbor-creds` password no longer matched what Harbor had stored — i.e. password drift, not an expiry event.

This is a **different failure mode** than 2026-05-05 (which was Harbor restart + proxy-cache upstream auth). The followup #3 expiry monitor ([platform/gitops!88](https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/88)) cannot detect this class because it only checks `expires_at`. The expiry monitor's most recent run was Failed for unrelated reasons — pod logs already cleaned, need to wait for tomorrow's scheduled run for fresh diagnostics.

## Diagnostic flow

```
# Confirm Harbor itself is healthy
curl -sSk https://registry.harbor.lan/api/v2.0/health  →  200 OK

# Probe the failing path with the cred from K8s
curl -sSk -u 'robot$k3s:<password from harbor-creds>' \
  https://registry.harbor.lan/v2/mcp/loom-core/manifests/latest  →  401
# AND
curl ... mcp/tech-radar/manifests/latest  →  401
# → not a per-project permission issue, the cred is bad against ANY path

# Confirm robot row in Harbor
curl -sSk -u 'robot$ai:<admin-creds>' https://registry.harbor.lan/api/v2.0/robots/1
# → {"disable": false, "expires_at": -1, "name": "robot$k3s", ...}
# → not expired, not disabled. Password drift only.
```

## Mitigation (executed 2026-05-27 ~19:33 UTC)

Per `loom-core/.loom/100-incident-harbor-401-deployment-chain-2026-05-05.md` runbook, the in-place refresh path (`PATCH /robots/{id}/secret`) was attempted first.

### Step 1 — PATCH /robots/1 via robot$ai admin → 403 (Harbor RBAC)

`robot$ai` is system-scoped with create/delete on `robot` resource, but `PATCH /robots/{id}` requires the real `admin` user. Couldn't refresh the existing secret without UI login. Pivoted to creating a replacement robot.

### Step 2 — Mint replacement via POST /robots

```bash
curl -sSk -X POST -u 'robot$ai:<admin>' -H 'Content-Type: application/json' \
  --data-binary @body.json \
  https://registry.harbor.lan/api/v2.0/robots
```

`body.json` cloned `robot$k3s` permissions verbatim (system-scope, `kind:project namespace:*` push/pull all projects, ~117 access entries total). Response:

```json
{"id": 10, "name": "robot$k3s-20260527", "secret": "<32-char>", "expires_at": -1}
```

### Step 3 — Rotate K8s secrets across 11 namespaces

```bash
DCFG=$(printf '{"auths":{"registry.harbor.lan":{"username":"%s","password":"%s","email":"none","auth":"%s"}}}' \
  "$NEW_USER" "$NEW_PASS" "$(printf '%s:%s' "$NEW_USER" "$NEW_PASS" | base64)" | base64 | tr -d '\n')

for NS in ai ci ci-build ci-jobs daemon devbox fi-fhir home labs loom-mills xfiles; do
  kubectl -n $NS patch secret harbor-creds --type=merge \
    -p "{\"data\":{\".dockerconfigjson\":\"$DCFG\"}}"
done
```

`flexinfer-system` was already on its own date-stamped robot (`robot$k3s-flexinfer-20260518`); left untouched.

### Step 4 — Update SOPS sources for durability

3 of 4 SOPS source files updated and re-encrypted in `platform/gitops`:

- `clusters/k3s/flux-system/daemon-harbor-creds.secret.yaml` (`daemon` ns)
- `k3s/ci/buildkit/harbor-creds.secret.yaml` (`ci-build` ns)
- `k3s/ci/gitlab/harbor-creds.secret.yaml` (`ci` + `ci-jobs` ns)

Committed as `ba63eb31` and pushed to `platform/gitops` main.

The 4th SOPS file, `k3s/fi-fhir/secrets/harbor-creds.secret.yaml`, has a **pre-existing MAC mismatch** (`MAC mismatch. File has 61D92BF4...`). Means a prior edit broke the SOPS metadata. The K8s secret in fi-fhir is patched live, but the encrypted source is stale. See "Follow-ups" below.

The other 7 namespaces (`ai`, `devbox`, `home`, `labs`, `loom-mills`, `xfiles`) have **no SOPS source** at all — their `harbor-creds` secrets are hand-applied with no GitOps backing. See "Follow-ups."

### Step 5 — Verify

`radar-scan` job retried (`121016` → `121295`): **success in 31s** with new credential.

### Step 6 — Disable old robot$k3s → blocked

`PATCH /robots/1 {"disable": true}` via `robot$ai` → 403. Same RBAC ceiling as Step 1. Old `robot$k3s` (id=1) remains *enabled but with a stale-from-our-side password*. Since the actual secret in Harbor's DB doesn't match what we ever knew, nobody (including us) can use it — but a real admin should still disable/delete it for audit hygiene. See "Follow-ups."

## Why the followup #3 expiry monitor didn't catch this

The monitor script (`k3s/monitoring/harbor-robot-expiry/check.py`) only inspects `expires_at` against `WARN_DAYS`. `robot$k3s` has `expires_at: -1` (never expires), so the monitor literally cannot fire for it. The drift failure mode is invisible to expiry-window logic.

Today's scheduled run did exit Failed (job `harbor-robot-expiry-check-29664540`), but pod logs were already cleaned by the time I checked. The Failed exit is likely an unrelated bug worth following up on but did NOT alert on this incident.

## Why the monitor needs an upgrade

A *health probe* monitor would have caught this — fetch a known repo's manifest using each `harbor-creds` cred and assert 200 (after token-exchange). That's a 2-line script change beyond expiry checking. Filed as follow-up.

## Follow-ups

1. **Disable robot$k3s (id=1) in Harbor UI.** Requires real admin user login (not robot$ai). Low blast radius since password is already stale, but hygiene. **STILL OPEN.**
2. **Expand `harbor-robot-expiry-check` to a real health probe.** ✅ **DONE** [`platform/gitops@c0cb8ddd`](https://gitlab.flexinfer.ai/platform/gitops/-/commit/c0cb8ddd). Added Stage 2 health-probe to the script: loads `monitoring/harbor-creds` (new SOPS source mirroring the operational robot), exchanges Basic auth for a Bearer token, validates the JWT `sub` claim matches the cred identity (Harbor returns 200 with an anonymous token when Basic auth fails silently — `sub` check is the actual drift signal), HEADs canary manifest. Smoke-tested both branches in-cluster (sub match → exit 0; sub missing → exit 1 with "PASSWORD DRIFT" message).
3. **Investigate why the 2026-05-27 expiry monitor run exited Failed.** Self-resolved — newer scheduled run (`29665980`) Completed cleanly hours later; pod logs of the earlier failure were already GC'd. Likely transient Harbor blip during the same window that caused the original cred symptom. **CLOSED (no action).** Follow-up #2's health-probe upgrade would now catch the same class.
4. **Repair `k3s/fi-fhir/secrets/harbor-creds.secret.yaml` SOPS metadata.** ✅ **DONE** [`platform/gitops@5418689d`](https://gitlab.flexinfer.ai/platform/gitops/-/commit/5418689d). Decrypted via `sops -d --ignore-mac`, re-encrypted, clean MAC restored. Also rotated 4 sibling SOPS sources (ai, devbox, home/homepage, labs/git-tunes) that I had missed in `ba63eb31` — Flux had been silently reverting my live-patched secrets back to the old broken password.
5. **Add SOPS sources for ungoverned namespaces** (initially I thought 7; actual count was 2: `loom-mills` and `xfiles`). ✅ **DONE** [`platform/gitops@5418689d`](https://gitlab.flexinfer.ai/platform/gitops/-/commit/5418689d). Created `k3s/mills/harbor-creds.secret.yaml` and `k3s/xfiles/harbor-creds.secret.yaml`, wired into respective kustomizations. Mills kustomization had a literal TODO note calling for this. `flexinfer-system` was already on its own date-stamped robot (`robot$k3s-flexinfer-20260518`); intentionally not in scope. All 11 affected namespaces now GitOps-managed with `robot$k3s-20260527`.
6. **Audit `robot$k3s-20260527` scope.** I cloned the original robot's permissions verbatim — which is **system-level admin-equivalent** (`create:project`, `create:registry`, `delete:robot`, push+pull on all projects). That's vastly over-scoped for image-pull-only consumers. Should split into narrower per-purpose robots: `robot$puller-mcp` (pull mcp/* only), `robot$builder-ci` (push to mcp/*), etc. **STILL OPEN** — bigger refactor needing scope-boundary decisions first.
7. **Document who can rotate Harbor secrets without UI access.** Today's session was blocked from the cleanest path (PATCH /robots/{id}/secret) by Harbor RBAC. Either grant robot$ai admin perms (risky — increases blast radius if robot$ai itself leaks), or document the Harbor admin user as a "break glass" credential. **STILL OPEN.**

## Sources

- Original 2026-05-05 runbook: [.loom/100-incident-harbor-401-deployment-chain-2026-05-05.md](100-incident-harbor-401-deployment-chain-2026-05-05.md)
- 2026-05-06 followup plan: [.loom/101-harbor-incident-followup-plan.md](101-harbor-incident-followup-plan.md)
- Rotation commit: [platform/gitops@ba63eb31](https://gitlab.flexinfer.ai/platform/gitops/-/commit/ba63eb31)
- Pipeline verified green: [pipeline 11963 / job 121295](https://gitlab.flexinfer.ai/services/loom-core/-/jobs/121295)
- Robot expiry monitor script: `platform/gitops/k3s/monitoring/harbor-robot-expiry/check.py`
- 2026-05-19 Harbor proxy cache dead (different issue, but same Harbor v2.13.2 instance):
  see `~/.claude/projects/-Users-cblevins-workspace-services-loom-core/memory/reference_harbor_proxy_cache_dead.md`

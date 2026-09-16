# LinkedIn Session Recovery Runbook

## Purpose

Recover LinkedIn MCP experimental messaging sessions when LinkedIn invalidates cookies or issues a challenge.

## Symptoms

- `linkedin_*messaging*` tools return auth challenge/forbidden errors
- `linkedin_session_health` returns `challenge` or `logged_out`
- Daemon logs show challenge-triggered recovery attempts

## Prerequisites

1. BrowserKit runtime on host:
- `bash scripts/browserkit/install_deps.sh` (pinned flexinfer-browser-kit + playwright, then the chromium download)

2. Secrets configured:
- `LINKEDIN_SESSION_COOKIE`
- `LINKEDIN_JSESSIONID`
- `LINKEDIN_LOGIN_USERNAME`
- `LINKEDIN_LOGIN_PASSWORD`

## Recovery Procedure

1. Check status:
- `loom tools call linkedin__linkedin_auth_status --args '{}' --json`

2. Force live health probe:
- `loom tools call linkedin__linkedin_session_health --args '{"refresh":true}' --json`

3. Attempt assisted recovery:
- `loom tools call linkedin__linkedin_session_recover --args '{"mode":"interactive"}' --json`

4. Re-check health:
- `loom tools call linkedin__linkedin_session_health --args '{"refresh":true}' --json`

5. Smoke test messaging read:
- `loom tools call linkedin__linkedin_list_conversations --args '{"start":0,"count":5}' --json`

## Expected Outcomes

- Recovery returns `state=healthy`
- Refreshed `li_at` and `JSESSIONID` persist into Loom secrets
- Messaging calls succeed without manual cookie paste

## Failure Modes

- BrowserKit dependencies missing: install prerequisites above
- Cooldown active: wait for `LINKEDIN_SESSION_RECOVERY_COOLDOWN_SECONDS`
- Checkpoint/CAPTCHA unresolved: complete manually in interactive browser and re-run recovery
- Persistent challenge: rotate session credentials and retry

## Cluster (loom-hub) Deployment

The `linkedin` pod in `loom-hub` runs with `LINKEDIN_BROWSERKIT_MODE=off`:
the `mcp/custom-server` image ships no python/chromium, so health probes and
recovery can never run in-cluster. The pod does cookie-only Voyager HTTP; a
stale cookie surfaces as a clean auth-challenge error instead of a
`LINKEDIN_BROWSERKIT_PYTHON` NotConfigured error.

Refreshing the cluster session:

1. Run recovery on a workstation (see procedure above). Recovered `li_at` +
   `JSESSIONID` persist to the local Loom secret store (macOS Keychain,
   service `loom`).
2. Build `LINKEDIN_COOKIE_BUNDLE` from the refreshed BrowserKit storage state
   (`~/.config/loom/linkedin-browserkit/<session>.json`): every cookie on a
   `linkedin.com` domain EXCEPT `li_at`/`JSESSIONID`, serialized as
   `name=value; name=value`. The device cookies (`bcookie`, `bscookie`,
   `lidc`, ...) must accompany the session cookie — LinkedIn revokes a
   `li_at` presented without them (observed twice, 2026-09-01: session died
   on the pod's first messaging call while curl probes with the same li_at
   kept working).
3. Sync `LINKEDIN_SESSION_COOKIE`, `LINKEDIN_JSESSIONID`, and
   `LINKEDIN_COOKIE_BUNDLE` into
   `platform/gitops/k3s/loom-hub/secrets.yaml` via `sops set` and merge.
4. `flux reconcile` the owning kustomization, then
   `kubectl rollout restart deployment/linkedin -n loom-hub` (env-from-secret
   is not hot-reloaded).
5. Do NOT exercise the pod's tools until the deployment is running a build
   with browser-fidelity headers (User-Agent + Accept-Language +
   `LINKEDIN_COOKIE_BUNDLE` support in `cmd/mcp-linkedin/transport.go`) — a
   pod without them burns the fresh session on its first messaging call.

Mode caveats:

- `silent` recovery CLEARS the persisted browser storage state before it
  attempts login, and headless login is currently defeated by LinkedIn's login
  page (form fields not found). Prefer `interactive` — it reuses nothing
  destructive, pre-fills the form, and waits up to 4 minutes for a human to
  complete login/2FA.

## Safety Constraints

- Stealth/browser automation is best-effort and may still be challenged
- Avoid aggressive retry loops; this MCP performs one retry after successful recovery
- Keep `linkedin_session_recover` out of registry `always_allow`

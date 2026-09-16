- **Inbound webhook verification fails closed on an empty secret**
  (`internal/hud/domain/webhook/verify.go`): `verifyGitLabToken` and
  `verifyGitHubSignature` returned `true` when the configured secret was
  empty, so `WEBHOOK_INBOUND_ENABLED=true` with an unset
  `WEBHOOK_GITLAB_SECRET` (or `WEBHOOK_GITHUB_SECRET`) accepted **any
  unauthenticated request** to `POST /api/webhook/gitlab` / `/github` —
  endpoints that route CI-failure events to active agents and can spawn
  fresh ones. Exposure was latent (inbound is default-off and no gitops
  manifest enables it), but the contract was fail-open. Both verifiers now
  reject every request when their secret is empty (constant-time comparison
  unchanged), and `hud.NewApp` refuses to start when inbound webhooks are
  enabled with no secret configured for either vendor, so the
  misconfiguration surfaces at boot instead of silently 401-ing real
  webhooks. A single configured secret is sufficient; the unconfigured
  vendor's endpoint rejects all requests. Ports the fail-closed semantics
  from `libs/fi-gitlab-hookify` (`auth.py`), per
  `docs/harvest-gitlab-hookify-notify-contract.md` §8.3 item 1.

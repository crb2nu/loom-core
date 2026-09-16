- Spawn: pool several Claude subscriptions. `SPAWN_CLAUDE_OAUTH_TOKEN_KEYS`
  on mobile-hud lists cluster-agent-auth keys that each hold a
  `claude setup-token` for a different account, as `key[=weekday[@HH]]` with
  the UTC weekday/hour the account's weekly usage window resets. Annotated
  accounts are paced: each claude-code spawn goes to the account furthest
  behind its own weekly pace (spend since its last reset, projected over the
  window, from the HUD's spawn ledger), so two accounts that reset on
  different days are drawn down evenly instead of one exhausting while the
  other idles toward its reset; level accounts round-robin. Unannotated pools
  round-robin. `spawn.State.auth_account` records the key a pod ran under,
  Mills stage artifacts carry it as `auth_account`, and
  `loom agent auth status` reports every `claude-oauth-token*` key. Unset
  keeps the single-key behaviour. Mint the second token with
  `platform/gitops/bin/sync-agent-tokens --mint-claude-token --key=claude-oauth-token-2`
  before listing it.

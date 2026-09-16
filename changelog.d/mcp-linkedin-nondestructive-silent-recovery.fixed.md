Stop mcp-linkedin's silent BrowserKit recovery from destroying persisted
session state. The recover action deleted
`~/.config/loom/linkedin-browserkit/<session>.json` before attempting a
headless credentialed login; LinkedIn's login page defeats headless form fill
("username field not found"), so on 2026-09-01 a storage state whose `li_at`
cookie was still valid for ~170 days was destroyed and the session ended
`logged_out` with nothing recoverable. Silent recovery now probes the persisted
state against `/voyager/api/me` first and reuses it when healthy (re-syncing
the secret store with the still-valid cookie), copies the state to a
timestamped `<session>.json.<stamp>.bak` (keeping the 5 newest) before any step
that could rewrite or clear it, refuses to unlink state that could not be
backed up, and surfaces the backup path plus the reuse/clear decision in the
tool response warnings.

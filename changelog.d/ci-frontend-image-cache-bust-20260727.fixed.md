Image builds: the Dockerfile frontend stage is now cache-keyed on the git
tree hash of `internal/hud/frontend` (new `FRONTEND_TREE` build-arg, passed
by `build:image:loom-core`). BuildKit's context checksum produced a false
cache hit on 2026-07-27 (pipeline 21165 → `loom-core:20260727-161926`),
shipping a HUD bundle that silently dropped 9 merged frontend commits —
including the Operator Deck mirror-collapse fix and the PanelShell token
sweep — from the deployed cluster HUD. With the tree hash anchoring the
layer chain, a stale bundle can no longer be reused when frontend source
has changed.

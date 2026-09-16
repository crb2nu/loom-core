- **Loom hub / devbox**: the mcp-devbox state store (`DEVBOX_CACHE_DIR/state.json`)
  now lives on a 1Gi Longhorn volume (`devbox-state`) and the deployment rolls
  with `Recreate`. The store is what tells the manager a sandbox image is
  already built; it was on the container filesystem, so every hub rollout (one
  per loom-core merge, 11 in 41h) booted empty and rebuilt the same
  `mcp/devbox/<project>:e3b0c44` git-clone image — 13 cold builds in 38h, each
  stalling the Mills tests stage for 6–20 minutes. A rollout now restarts the
  sandboxes from the registry image instead.

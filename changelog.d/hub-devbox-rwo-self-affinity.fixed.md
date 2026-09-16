- **Loom hub / devbox**: the devbox Deployment gains a required same-node
  self-affinity for its RWO `devbox-state` volume. The gitops loom-hub-servers
  overlay patches every mcp-server Deployment to RollingUpdate
  maxSurge:1/maxUnavailable:0, which silently overrode the `Recreate` shipped
  with the volume; without the affinity the next rollout's surge pod could
  land on another node and wedge on the Longhorn attach.

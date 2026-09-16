- **Loom hub**: the `custom-server` image now ships `openssh-client`, so the
  hub's `asus_router` MCP server no longer answers every call with
  `exec: "ssh": executable file not found in $PATH`. `mcp-asus-router` gains
  `ASUS_ROUTER_SSH_KEY` (passed as `ssh -i … -o IdentitiesOnly=yes`) and warns
  at boot when `ssh` is missing; the hub deployment addresses the router by IP
  and mounts the identity from an optional `asus-router-ssh` Secret, which
  still has to be created in platform/gitops for the tools to authenticate.

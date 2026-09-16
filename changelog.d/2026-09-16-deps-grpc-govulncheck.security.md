- **Bump `google.golang.org/grpc` to v1.83.2** (`go.mod`): v1.82.1 carried
  GO-2026-6443 (server panic on missing authority/Host headers), GO-2026-6441
  (xDS RBAC header-match bypass) and GO-2026-6348 (HTTP/2 DATA-frame heap
  exhaustion); the advisory for GO-2026-6443 was then amended to mark v1.83.1
  affected too, so the fix landed in two steps (!1996, then v1.83.2).
  `security:govulncheck` had failed every main pipeline since 2026-09-15
  22:57Z, which skipped every `build:image:*` job, so no operator, loom-core
  or custom-server image had been published since 18:33Z.

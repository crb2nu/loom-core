- **CI Go module cache actually restores now** (`.gitlab-ci.yml`): every Go job
  had been building cold. The per-branch archive (module cache + `.go-build`)
  was ~970MB against the runner's 512MB upload cap, so `prepare:go-cache`
  never uploaded it, and `fallback_keys: [main]` resolved to
  `main-non_protected` while protected `main` wrote `main-protected`. Jobs now
  share one constant `go-mod-shared` key (`unprotect: true`) holding the
  module download cache and Go tooling, written only by default-branch
  pipelines and read by every ref. `.go-build` stays uncached until the runner
  cap and cache storage are raised in platform/gitops. `prepare:go-cache` logs
  the archive component sizes so a future overflow is visible in the trace.

- **CI build cache gets its own cache entry** (`.gitlab-ci.yml`): `.go-build`
  is cached under a second shared key, `go-build-shared`, next to
  `go-mod-shared`. The runner sizes each archive against its upload cap
  separately, so a build cache that outgrows the cap can no longer drag the
  module cache into "stored only locally" with it. Uploading it depends on
  the 2GiB cap and persistent MinIO from platform/gitops !667; until that
  rolls, the runner logs the build archive as stored locally and jobs carry
  on with the module cache.

Key devbox sandbox images by their Dockerfile as well as their dependency
manifests. The image tag was the hash of `go.mod`/`go.sum` (and friends)
alone, so any manager that agreed on the manifests trusted whatever image
already sat under that tag — regardless of how it was built. After
2026-09-04's remote-manifest fingerprinting the hub started reusing
`mcp/devbox/loom-core:092ff5c`, a tag a workstation manager had pushed with
golangci-lint v1 on board, and every Mills `lint:parity` check failed with
"configuration file for golangci-lint v2 with golangci-lint v1", escalating
seven runs as infrastructure in the first night (the check also failed against
bare main, so the classifier was right: the image was the fault).

`fingerprintProject` now folds the rendered Dockerfile into the hash
(`detect.RecipeHash`), so a recipe change — a different base image, a
different tool install, a template fix — yields a new tag and one honest
rebuild from the registered base, and a bad image cannot be inherited across
manager versions. The generic git-clone fallback is stamped the same way, so
it no longer collides with the empty-input hash. Existing sandboxes rebuild
once on the next cold call.

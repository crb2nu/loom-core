Running `pnpm build` in `internal/hud/frontend` no longer deletes the tracked
`dist/.gitkeep`. Vite's `emptyOutDir` wiped the whole directory, so every
frontend build left a spurious `D dist/.gitkeep` in `git status`; when that
deletion slipped into a commit (e.g. via `git add -A`), every Go CI job that
typechecks `internal/hud` went red with the opaque
`pattern all:frontend/dist: no matching files found`, because the placeholder
is what keeps `//go:embed all:frontend/dist` valid before vite has run.
`emptyOutDir` is now off and a small `preserveDistGitkeep` plugin cleans
`dist/` itself, sparing (and restoring) the placeholder — so stale assets
still never leak into the embed, and the guard applies to direct `pnpm build`
runs, not only `make hud-frontend` (which already restored the file
afterwards).

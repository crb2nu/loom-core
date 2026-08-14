- Pre-commit/pre-push stash wrapper (`scripts/hooks/with-stashed-worktree.sh`)
  no longer corrupts the working tree when the commit being verified contains a
  file rename. The restore step now copies the exact captured bytes back
  per-path from the dangling stash commit instead of `git stash apply`, whose
  3-way merge degenerated into rename/delete + add/add conflicts on staged
  renames — leaving conflict markers, an unmerged index, and a commit that
  aborted with `error: Error building trees` after all checks passed. Adds a
  rename-safety regression test (`with-stashed-worktree_rename_test.sh`) wired
  into `make test-hooks` and the `test:hooks` CI job.

#!/usr/bin/env bash
# Integration test for with-stashed-worktree.sh concurrency safety.
#
# Spins up a throwaway repo with two linked worktrees (which share refs/stash),
# dirties each with distinct staged / unstaged / untracked content, then runs
# the stash wrapper concurrently in both with an overlapping wrapped command.
# Asserts each worktree's own changes are restored with no cross-contamination
# and no leaked stashes — the exact failure mode of the old stash@{0} pop.
set -euo pipefail

# Hermeticity: git exports GIT_DIR / GIT_INDEX_FILE / GIT_WORK_TREE (and
# friends) into every hook process. This test builds its OWN throwaway repos
# and worktrees, so any inherited git env would point its `git init`/`git add`/
# `git worktree` commands at the REAL repository and corrupt its index. Unset
# everything that locates a repo so the test behaves identically whether run
# standalone, via `make test-hooks`, in CI, or nested inside a pre-commit hook.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_PREFIX \
      GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE \
      GIT_IMPLICIT_WORK_TREE

HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPTS_DIR="$(cd "${HOOK_DIR}/.." && pwd)"

TMP="$(mktemp -d)"
cleanup() {
  git -C "${TMP}/repo" worktree remove --force "${TMP}/wtA" 2>/dev/null || true
  git -C "${TMP}/repo" worktree remove --force "${TMP}/wtB" 2>/dev/null || true
  rm -rf "${TMP}"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# --- build a repo with the scripts under test committed in ---
mkdir -p "${TMP}/repo"
cd "${TMP}/repo"
git init -q
git config user.email test@example.com
git config user.name test
git config commit.gpgsign false
mkdir -p scripts/hooks scripts/dev
cp "${HOOK_DIR}/with-stashed-worktree.sh" scripts/hooks/
cp "${SCRIPTS_DIR}/dev/with-clean-git-env.sh" scripts/dev/
chmod +x scripts/hooks/*.sh scripts/dev/*.sh
# Realistic dirty state: staged change lives in one tracked file, the unstaged
# change in a DIFFERENT tracked file, plus an untracked file. (Staged+unstaged
# edits to the same lines would make `git stash pop` 3-way-conflict regardless
# of this script — not the concurrency property under test.)
printf 'base\n' > staged.txt
printf 'base\n' > unstaged.txt
git add -A
git commit -qm base

git worktree add -q "${TMP}/wtA" -b a HEAD
git worktree add -q "${TMP}/wtB" -b b HEAD

dirty() {
  local wt="$1" tag="$2"
  cd "${wt}"
  printf 'staged-%s\n' "${tag}" > staged.txt
  git add staged.txt                                   # staged content
  printf 'unstaged-%s\n' "${tag}" > unstaged.txt       # unstaged (different file)
  printf 'untracked-%s\n' "${tag}" > "new-${tag}.txt"  # untracked
}

dirty "${TMP}/wtA" A
dirty "${TMP}/wtB" B

# Wrapped command: assert the worktree is staged-only mid-check, then sleep to
# force overlap with the sibling worktree's invocation.
cat > "${TMP}/check.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
tag="$1"
grep -q "staged-${tag}" staged.txt || { echo "check: staged content missing for ${tag}" >&2; exit 1; }
# unstaged change must be reverted to committed state during the check...
if grep -q "unstaged-${tag}" unstaged.txt; then echo "check: unstaged not stashed for ${tag}" >&2; exit 1; fi
# ...and the untracked file stashed away.
if [[ -e "new-${tag}.txt" ]]; then echo "check: untracked not stashed for ${tag}" >&2; exit 1; fi
sleep 2
EOF
chmod +x "${TMP}/check.sh"

# Run both concurrently (overlapping the 2s sleeps).
( cd "${TMP}/wtA" && ./scripts/hooks/with-stashed-worktree.sh "${TMP}/check.sh" A ) &
pidA=$!
( cd "${TMP}/wtB" && ./scripts/hooks/with-stashed-worktree.sh "${TMP}/check.sh" B ) &
pidB=$!

rcA=0; rcB=0
wait "${pidA}" || rcA=$?
wait "${pidB}" || rcB=$?
[[ "${rcA}" -eq 0 ]] || fail "wtA invocation exited ${rcA}"
[[ "${rcB}" -eq 0 ]] || fail "wtB invocation exited ${rcB}"

# --- assert each worktree restored its OWN content, no cross-contamination ---
assert_restored() {
  local wt="$1" tag="$2" other="$3"
  cd "${wt}"
  grep -q "staged-${tag}" staged.txt     || fail "${tag}: staged content lost"
  grep -q "unstaged-${tag}" unstaged.txt  || fail "${tag}: unstaged content not restored"
  [[ -f "new-${tag}.txt" ]]               || fail "${tag}: untracked file not restored"
  grep -q "${other}" unstaged.txt        && fail "${tag}: CROSS-CONTAMINATION — has ${other}'s unstaged change"
  [[ -e "new-${other}.txt" ]]             && fail "${tag}: CROSS-CONTAMINATION — has ${other}'s untracked file"
  return 0
}
assert_restored "${TMP}/wtA" A B
assert_restored "${TMP}/wtB" B A

# --- no leaked loom-hook-temporary stashes ---
leaked="$(git -C "${TMP}/repo" stash list 2>/dev/null | grep -c "loom-hook-temporary" || true)"
[[ "${leaked}" -eq 0 ]] || fail "leaked ${leaked} loom-hook-temporary stash(es)"

echo "PASS: concurrency-safe stash/restore with no cross-contamination or leaks"

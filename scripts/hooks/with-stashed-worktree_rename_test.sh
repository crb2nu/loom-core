#!/usr/bin/env bash
# Rename-safety regression test for with-stashed-worktree.sh.
#
# Reproduces the 2026-07-25 incident: committing a staged file RENAME (with the
# content also rewritten, so similarity-based rename detection cannot pair it)
# while the renamed file additionally carries unstaged edits. The old restore
# path (`git stash apply <sha>`) 3-way-merged against the stash's base commit;
# the stash side presented the rename as delete(old)+add(new), so the merge
# degenerated into rename/delete + add/add conflicts — conflict markers in the
# renamed file, an unmerged index, and the commit itself aborting with
# `error: Error building trees` AFTER every check passed.
#
# The fixed restore copies the exact captured bytes back per-path from the
# dangling stash commit's tree, so this test asserts: clean wrapper exit with
# no warnings, exact worktree restoration (including an unstaged deletion),
# an untouched staged index, and a subsequent commit that succeeds.
set -euo pipefail

# Hermeticity: see with-stashed-worktree_test.sh — inherited git env would
# point this test's throwaway repo commands at the real repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_PREFIX \
      GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE \
      GIT_IMPLICIT_WORK_TREE

HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPTS_DIR="$(cd "${HOOK_DIR}/.." && pwd)"

TMP="$(mktemp -d)"
cleanup() { rm -rf "${TMP}"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# A user-global core.hooksPath would run foreign hooks on this repo's commits
# (including stash push/pop logic that itself corrupts renames). Pin hooks to
# an empty dir so the test exercises ONLY the wrapper under test.
mkdir -p "${TMP}/nohooks" "${TMP}/repo"
cd "${TMP}/repo"
git init -q
git config user.email test@example.com
git config user.name test
git config commit.gpgsign false
git config core.hooksPath "${TMP}/nohooks"
mkdir -p scripts/hooks scripts/dev
cp "${HOOK_DIR}/with-stashed-worktree.sh" scripts/hooks/
cp "${SCRIPTS_DIR}/dev/with-clean-git-env.sh" scripts/dev/
chmod +x scripts/hooks/*.sh scripts/dev/*.sh

printf 'recipe line 1\nrecipe line 2\nrecipe line 3\n' > svc_recipes.go
printf 'other 1\nother 2\n' > other.txt
printf 'doomed\n' > deleted.txt
git add -A
git commit -qm base

# --- dirty state: the incident shape ---
# Staged: rename svc_recipes.go -> svc_engram_helpers.go with a full rewrite
# (content similarity too low for merge rename detection to pair the paths).
git mv svc_recipes.go svc_engram_helpers.go
printf 'engram helper A\nengram helper B\nengram helper C\n' > svc_engram_helpers.go
git add -A
staged_blob="$(git rev-parse :svc_engram_helpers.go)"
# Unstaged, on top of the staged rename: an edit to the renamed file, an edit
# to an unrelated file, a file deletion, and an untracked file.
printf 'engram helper A\nengram helper B\nengram helper C\nWIP unstaged\n' > svc_engram_helpers.go
printf 'other 1\nother 2 EDITED\n' > other.txt
rm -f deleted.txt
printf 'scratch\n' > untracked.txt

# Wrapped command: mid-check the worktree must hold exactly the staged content.
cat > "${TMP}/check.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ! -e svc_recipes.go ]] || { echo "check: old rename path present" >&2; exit 1; }
grep -q 'engram helper A' svc_engram_helpers.go || { echo "check: renamed file missing staged content" >&2; exit 1; }
if grep -q 'WIP unstaged' svc_engram_helpers.go; then echo "check: unstaged edit visible" >&2; exit 1; fi
if grep -q 'EDITED' other.txt; then echo "check: unstaged edit to other.txt visible" >&2; exit 1; fi
[[ -e deleted.txt ]] || { echo "check: unstaged deletion not reverted" >&2; exit 1; }
[[ ! -e untracked.txt ]] || { echo "check: untracked file visible" >&2; exit 1; }
EOF
chmod +x "${TMP}/check.sh"

wrapper_err="${TMP}/wrapper.err"
./scripts/hooks/with-stashed-worktree.sh "${TMP}/check.sh" 2> "${wrapper_err}" \
  || fail "wrapper exited non-zero: $(cat "${wrapper_err}")"
[[ -s "${wrapper_err}" ]] && fail "wrapper emitted warnings: $(cat "${wrapper_err}")"

# --- worktree restored exactly ---
grep -q 'WIP unstaged' svc_engram_helpers.go       || fail "unstaged edit to renamed file lost"
grep -q '<<<<<<<' svc_engram_helpers.go            && fail "conflict markers in renamed file"
grep -q 'EDITED' other.txt                          || fail "unstaged edit to other.txt lost"
[[ ! -e deleted.txt ]]                              || fail "unstaged deletion resurrected"
[[ -f untracked.txt ]]                              || fail "untracked file not restored"
[[ ! -e svc_recipes.go ]]                           || fail "old rename path resurrected"

# --- index untouched: staged rename intact, nothing unmerged ---
[[ -z "$(git ls-files --unmerged)" ]]               || fail "index left unmerged"
[[ "$(git rev-parse :svc_engram_helpers.go)" == "${staged_blob}" ]] \
  || fail "staged content of renamed file changed"

# --- the commit the hook was guarding must now succeed (was: 'error: Error
# --- building trees' from the unmerged index the stash-apply merge left) ---
git commit -qm 'rename svc_recipes.go -> svc_engram_helpers.go' \
  || fail "commit failed after restore"
[[ "$(git rev-parse HEAD:svc_engram_helpers.go)" == "${staged_blob}" ]] \
  || fail "committed content is not the staged content"
git show HEAD --stat --format= | grep -q 'svc_engram_helpers.go' \
  || fail "renamed file missing from commit"

echo "PASS: rename-safe stash/restore — staged rename committed, worktree restored byte-exact"

#!/usr/bin/env bash
set -euo pipefail

# with-stashed-worktree.sh — run a command against staged-only content.
#
# Wraps the pre-commit / pre-push checks: it removes unstaged and untracked
# changes from the working tree so the checks see exactly what is about to be
# committed, then restores the working tree afterwards.
#
# CONCURRENCY SAFETY — the bug this implementation exists to avoid:
#   refs/stash is SHARED across every linked worktree of a repository, and a
#   bare `git stash pop` always targets stash@{0}. With several agents
#   committing concurrently in sibling .claude/worktrees/*, one invocation's
#   pop would apply/drop ANOTHER worktree's stash — scrambling WIP, leaking
#   dozens of "loom-hook-temporary" stashes, and injecting merge-conflict
#   markers into rendered files that then got committed.
#
#   This implementation NEVER touches refs/stash. It captures the dirty state
#   as a *dangling* stash commit via `git stash create` (which writes NO ref),
#   discards only the changed tracked files from the working tree, moves
#   untracked files aside, runs the command, then restores those same paths'
#   exact captured bytes from that commit's tree and moves the untracked files
#   back. Because nothing is ever written to the shared refs/stash, concurrent
#   invocations in sibling worktrees cannot collide — each only ever touches
#   its own dangling commit.
#
# RENAME SAFETY — why the restore is a content copy, NOT `git stash apply`:
#   stash apply performs a 3-way merge against the stash's base commit and must
#   re-discover any staged rename by content similarity. When the commit being
#   verified renames a file, the stash side of that merge presents as
#   delete(old)+add(new), so the merge degenerates into rename/delete + add/add
#   conflicts: conflict markers land in the renamed file and the index is left
#   unmerged, which then aborts the commit itself with `error: Error building
#   trees`. This wrapper knows exactly which paths it reverted, so it restores
#   precisely those paths from the stash commit's worktree tree with
#   `git restore --source=<sha>` — a deterministic byte copy that cannot
#   conflict and never touches the index (`git restore` also re-deletes tracked
#   paths that were deleted in the worktree at capture time, since they are
#   absent from the source tree).
#
#   Crash recovery: if the process is hard-killed before the restore trap runs,
#   the dirty state survives as an unreferenced commit (the printed SHA),
#   recoverable via `git stash apply <sha>` within gc's grace window. Untracked
#   files survive under the scratch dir noted on any restore failure.
#
# Note on the silent killers fixed alongside the concurrency bug (see
# .loom history / memory note project_hook_stash_concurrency):
#   1. `set -e` inside an EXIT trap can corrupt the wrapper's exit status,
#      aborting the commit while the hook printed success. The restore trap
#      below runs `set +e` and ends with `return 0`.
#   2. `set -o pipefail` + an early-exiting pipe consumer (awk/head) SIGPIPEs
#      the upstream git process and trips errexit once the output is large
#      enough to fill the pipe buffer. This script writes git output to files
#      and never pipes it into an early-exiting consumer.

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
WITH_CLEAN_GIT_ENV="${REPO_ROOT}/scripts/dev/with-clean-git-env.sh"

cd "${REPO_ROOT}"

# Worktree-correct git: with-clean-git-env.sh unsets inherited GIT_DIR/
# GIT_WORK_TREE so repo discovery is cwd-based.
git_clean() { "${WITH_CLEAN_GIT_ENV}" git "$@"; }

# Scratch space for stashed-aside untracked files. Anchor it under the git
# common dir (same filesystem as the working tree, so `mv` is an atomic rename,
# and never visible to `git ls-files`). Fall back to TMPDIR if that lookup
# fails for any reason.
scratch_root="$(git_clean rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)"
if [[ -z "${scratch_root}" || ! -d "${scratch_root}" ]]; then
  scratch_root="${TMPDIR:-/tmp}"
fi
UNTRACKED_DIR="$(mktemp -d "${scratch_root%/}/loom-hook-untracked.XXXXXX")"
UNTRACKED_LIST="$(mktemp "${scratch_root%/}/loom-hook-untracked-list.XXXXXX")"
CHANGED_LIST="$(mktemp "${scratch_root%/}/loom-hook-changed.XXXXXX")"

stash_sha=""
untracked_saved=false

# --- capture the full dirty state as a dangling stash commit (no ref) ---
# `git stash create` records staged + unstaged tracked changes and prints the
# commit SHA without modifying the working tree, index, OR refs/stash. It does
# NOT capture untracked files — those are handled separately below.
stash_sha="$(git_clean stash create "loom-hook-temporary" 2>/dev/null || true)"
stash_sha="$(printf '%s' "${stash_sha}" | tr -d '[:space:]')"

# --- discard only the unstaged tracked changes (keep staged = --keep-index) ---
# Restore the index version of every file that differs in the working tree
# (`git diff --name-only` = unstaged changes vs the index). Touching only the
# changed files avoids rewriting — and re-mtime-ing — the entire tree, which
# would thrash Go's build cache on every commit. CHANGED_LIST is kept for the
# restore trap: it is the exact set of paths the restore must copy back.
if [[ -n "${stash_sha}" ]]; then
  git_clean diff --name-only -z > "${CHANGED_LIST}" 2>/dev/null || true
  if [[ -s "${CHANGED_LIST}" ]]; then
    git_clean checkout-index -f -z --stdin < "${CHANGED_LIST}" 2>/dev/null || true
  fi
fi

# --- move untracked files aside so the checks don't see them ---
git_clean ls-files --others --exclude-standard -z > "${UNTRACKED_LIST}" 2>/dev/null || true
if [[ -s "${UNTRACKED_LIST}" ]]; then
  while IFS= read -r -d '' rel; do
    [[ -n "${rel}" ]] || continue
    dest="${UNTRACKED_DIR}/${rel}"
    mkdir -p "$(dirname "${dest}")" 2>/dev/null || true
    mv "${rel}" "${dest}" 2>/dev/null || true
    untracked_saved=true
  done < "${UNTRACKED_LIST}"
fi

restore() {
  # NEVER let this trap corrupt the wrapper's exit status: a non-zero result
  # from any restore step under `set -e` would abort the commit while the hook
  # printed success. Disable errexit and end with `return 0`.
  set +e

  # Restore unstaged tracked changes by copying the exact captured bytes back
  # from the dangling stash commit's tree, for precisely the paths reverted
  # above. Deliberately NOT `git stash apply`: its 3-way merge conflicts on
  # staged renames (see RENAME SAFETY in the header) and would leave conflict
  # markers plus an unmerged index behind a hook that reported success.
  # --literal-pathspecs: CHANGED_LIST entries are literal paths, not globs.
  if [[ -n "${stash_sha}" && -s "${CHANGED_LIST}" ]]; then
    if ! git_clean --literal-pathspecs restore --source="${stash_sha}" --worktree \
        --pathspec-from-file="${CHANGED_LIST}" --pathspec-file-nul >/dev/null 2>&1; then
      echo "with-stashed-worktree: WARNING — could not cleanly restore stashed changes." >&2
      echo "  Your uncommitted changes are preserved in dangling commit ${stash_sha}." >&2
      echo "  Recover with: git stash apply ${stash_sha}" >&2
    fi
  fi

  # Move untracked files back.
  if [[ "${untracked_saved}" == "true" && -s "${UNTRACKED_LIST}" ]]; then
    local restore_failed=false
    while IFS= read -r -d '' rel; do
      [[ -n "${rel}" ]] || continue
      src="${UNTRACKED_DIR}/${rel}"
      [[ -e "${src}" ]] || continue
      mkdir -p "$(dirname "${rel}")" 2>/dev/null || true
      mv "${src}" "${rel}" 2>/dev/null || restore_failed=true
    done < "${UNTRACKED_LIST}"
    if [[ "${restore_failed}" == "true" ]]; then
      echo "with-stashed-worktree: WARNING — some untracked files could not be moved back." >&2
      echo "  They are preserved under ${UNTRACKED_DIR}." >&2
      return 0
    fi
  fi

  rm -rf "${UNTRACKED_DIR}" "${UNTRACKED_LIST}" "${CHANGED_LIST}" 2>/dev/null || true
  return 0
}
trap restore EXIT

# Run the wrapped command. Capture its status WITHOUT tripping `set -e`, so the
# EXIT trap restores cleanly and we propagate the command's real exit code.
rc=0
"$@" || rc=$?
exit "${rc}"

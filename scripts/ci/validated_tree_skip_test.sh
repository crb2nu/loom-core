#!/usr/bin/env bash
# Regression guard for validated_tree_skip.sh. Pure git + bash, no network:
# builds throwaway repositories for each merge shape and asserts whether the
# sourced script ends the job (exit 0 with the skip line) or returns so the
# job continues.
set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/validated_tree_skip.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }

# run_case NAME EXPECT(skip|continue) [ENV=VAL ...]; the repo must be the cwd.
run_case() {
	local name="$1" expect="$2"
	shift 2
	local out
	out="$(env -i PATH="$PATH" HOME="$HOME" CI_COMMIT_SHA="$(git rev-parse HEAD)" "$@" \
		bash -c 'set -eo pipefail; . "$0"; echo CONTINUED' "$SCRIPT" 2>&1)" || fail "$name: script exited non-zero: $out"
	case "$expect" in
	skip)
		echo "$out" | grep -q '^validated-tree skip:' || fail "$name: expected skip line, got: $out"
		echo "$out" | grep -q 'CONTINUED' && fail "$name: job continued after a skip: $out"
		;;
	continue)
		echo "$out" | grep -q 'CONTINUED' || fail "$name: job did not continue: $out"
		echo "$out" | grep -q '^validated-tree skip:' && fail "$name: skipped unexpectedly: $out"
		;;
	esac
	pass "$name"
}

mkrepo() {
	local dir
	dir="$(mktemp -d)"
	cd "$dir"
	git init -q -b main
	git config user.email ci@test
	git config user.name ci
	git config commit.gpgsign false
	echo a >a
	git add a
	git commit -qm A
}

# 1. Rebased/queue-style merge: main tip is an ancestor of the head, so the
#    merge tree equals the head tree → skip.
mkrepo
git checkout -qb feat
echo b >b && git add b && git commit -qm B
git checkout -q main
git merge -q --no-ff -m "Merge branch 'feat' into 'main'" feat
[ "$(git rev-parse 'HEAD^{tree}')" = "$(git rev-parse 'HEAD^2^{tree}')" ] || fail "fixture: rebased merge trees differ"
run_case "rebased merge on main skips" skip CI_COMMIT_BRANCH=main CI_DEFAULT_BRANCH=main CI_PIPELINE_SOURCE=push CI_JOB_NAME=test:unit

# 2. Same commit, but a branch pipeline → never skip.
run_case "branch pipeline continues" continue CI_COMMIT_BRANCH=feat CI_DEFAULT_BRANCH=main CI_PIPELINE_SOURCE=push

# 3. Escape hatch.
run_case "escape hatch continues" continue CI_COMMIT_BRANCH=main CI_DEFAULT_BRANCH=main CI_PIPELINE_SOURCE=push CI_VALIDATED_TREE_SKIP=false

# 4. Schedule/web pipelines on main are not merges of a validated head.
run_case "non-push source continues" continue CI_COMMIT_BRANCH=main CI_DEFAULT_BRANCH=main CI_PIPELINE_SOURCE=schedule

# 5. Three-way merge: main advanced after the branch forked, so the merge
#    tree is one neither pipeline saw → full validation.
mkrepo
git checkout -qb feat
echo b >b && git add b && git commit -qm B
git checkout -q main
echo c >c && git add c && git commit -qm C
git merge -q --no-ff -m "Merge branch 'feat' into 'main'" feat
[ "$(git rev-parse 'HEAD^{tree}')" != "$(git rev-parse 'HEAD^2^{tree}')" ] || fail "fixture: three-way merge trees equal"
run_case "three-way merge continues" continue CI_COMMIT_BRANCH=main CI_DEFAULT_BRANCH=main CI_PIPELINE_SOURCE=push

# 6. A plain (non-merge) commit on main.
mkrepo
echo d >d && git add d && git commit -qm D
run_case "direct commit continues" continue CI_COMMIT_BRANCH=main CI_DEFAULT_BRANCH=main CI_PIPELINE_SOURCE=push

# 7. Outside a work tree (GIT_STRATEGY none job that never cloned).
cd "$(mktemp -d)"
out="$(env -i PATH="$PATH" HOME="$HOME" CI_COMMIT_BRANCH=main CI_DEFAULT_BRANCH=main CI_PIPELINE_SOURCE=push bash -c 'set -eo pipefail; . "$0"; echo CONTINUED' "$SCRIPT" 2>&1)"
echo "$out" | grep -q CONTINUED || fail "no work tree: job did not continue: $out"
pass "no work tree continues"

echo "validated_tree_skip: all cases passed"

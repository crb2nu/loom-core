#!/usr/bin/env bash
# validated_tree_skip.sh — SOURCE this as the first `script` step of a
# validation job that also runs on branch pipelines. On the default branch it
# ends the job successfully (exit 0) when the commit under test is a merge
# commit whose tree is byte-identical to its second parent — the MR head —
# because that exact tree already passed the MR's own pipeline.
#
# Why the skip is sound:
#   * The project requires a green head pipeline before merge
#     (`only_allow_merge_if_pipeline_succeeds`), and the second parent of a
#     GitLab merge commit IS that head.
#   * A tree is the complete content of the checkout. Equal trees mean the
#     merge introduced nothing the MR pipeline did not compile, lint and test.
#     That is exactly the shape the Mills merge queue and any rebased MWPS
#     merge produce (main tip is an ancestor of the head, so the merge tree
#     equals the head tree). A three-way merge of an un-rebased branch has a
#     tree neither pipeline saw, so it is never skipped.
#   * When a token is available (VALIDATED_TREE_TOKEN, a read_api token —
#     CI_JOB_TOKEN cannot list pipelines), the script additionally requires a
#     successful pipeline for the head SHA and runs the job in full otherwise.
#
# Why it exists: every merge to main re-ran the full ~150 job-minute
# validation set on a tree that had just passed it, on a runner pool of ~12
# slots. On 2026-09-02 six main pipelines stacked behind each other and the
# hub/operator images for the day's fixes queued for hours.
#
# Escape hatch: set CI_VALIDATED_TREE_SKIP=false (pipeline variable) to force
# full validation on main. Main-only jobs (test:race, test:benchmark,
# security:*) must NOT source this — they never ran on the MR pipeline.
#
# Contract: a skipped job prints one `validated-tree skip:` line. Sourced, not
# executed, so `exit 0` ends the JOB, not a subshell; every early return below
# means "not applicable, run the job".

validated_tree_skip() {
	if [ "${CI_VALIDATED_TREE_SKIP:-true}" != "true" ]; then
		echo "validated-tree: disabled by CI_VALIDATED_TREE_SKIP; running ${CI_JOB_NAME:-job} in full"
		return 0
	fi
	[ -n "${CI_COMMIT_BRANCH:-}" ] || return 0
	[ "${CI_COMMIT_BRANCH}" = "${CI_DEFAULT_BRANCH:-main}" ] || return 0
	[ "${CI_PIPELINE_SOURCE:-push}" = "push" ] || return 0
	git rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 0

	local head parents head_tree mr_sha mr_tree
	head="${CI_COMMIT_SHA:-HEAD}"
	parents="$(git rev-list --parents -n 1 "$head" 2>/dev/null | wc -w | tr -d ' ')"
	# rev-list prints the commit followed by its parents: a merge has 3 words.
	[ "$parents" = "3" ] || return 0
	head_tree="$(git rev-parse "${head}^{tree}" 2>/dev/null)" || return 0
	mr_sha="$(git rev-parse "${head}^2" 2>/dev/null)" || return 0
	mr_tree="$(git rev-parse "${mr_sha}^{tree}" 2>/dev/null)" || return 0
	if [ "$head_tree" != "$mr_tree" ]; then
		echo "validated-tree: merge tree ${head_tree:0:12} differs from MR head tree ${mr_tree:0:12}; running ${CI_JOB_NAME:-job} in full"
		return 0
	fi

	local evidence="project policy (green head pipeline required to merge)"
	if [ -n "${VALIDATED_TREE_TOKEN:-}" ] && [ -n "${CI_API_V4_URL:-}" ] && [ -n "${CI_PROJECT_ID:-}" ]; then
		local ids
		ids="$(curl -sS --max-time 20 -H "PRIVATE-TOKEN: ${VALIDATED_TREE_TOKEN}" \
			"${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/pipelines?sha=${mr_sha}&status=success&per_page=1" 2>/dev/null |
			tr -d '\n' | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')" || ids=""
		if [ -z "$ids" ]; then
			echo "validated-tree: no successful pipeline recorded for MR head ${mr_sha:0:12}; running ${CI_JOB_NAME:-job} in full"
			return 0
		fi
		evidence="pipeline #${ids} on MR head"
	fi

	echo "validated-tree skip: ${CI_JOB_NAME:-job} — merge ${head:0:12} has tree ${head_tree:0:12}, identical to MR head ${mr_sha:0:12} (${evidence}); nothing new to validate on ${CI_COMMIT_BRANCH}"
	exit 0
}

validated_tree_skip

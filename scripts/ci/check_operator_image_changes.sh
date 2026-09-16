#!/usr/bin/env bash
# check_operator_image_changes.sh — fail when build:image:loom-mills-operator's
# `changes:` list in .gitlab-ci.yml no longer covers every repository package
# the operator binary links.
#
# Why: the operator image job only rebuilds (and Flux only rolls the operator)
# when a path in that list changes. The list was narrowed on 2026-09-11 from
# cmd/**, internal/**, pkg/** to the binary's actual import footprint so that
# an mcp-* server or HUD change no longer restarts the operator. A narrow list
# is only safe while it is complete; this guard makes a new dependency
# directory a CI failure instead of a silently stale operator.
#
# Usage: bash scripts/ci/check_operator_image_changes.sh [ci-file]
#   Needs a Go toolchain and the module cache (runs `go list -deps`).
set -euo pipefail

ci_file="${1:-.gitlab-ci.yml}"
module="github.com/crb2nu/loom"
job="build:image:loom-mills-operator"

deps="$(GOWORK=off go list -deps ./cmd/loom-mills-operator | grep "^${module}/" | sed "s#^${module}/##" | sort -u)"
if [ -z "$deps" ]; then
	echo "check_operator_image_changes: go list returned no in-module deps" >&2
	exit 2
fi

deps_file="$(mktemp)"
trap 'rm -f "$deps_file"' EXIT
printf '%s\n' "$deps" >"$deps_file"

python3 - "$ci_file" "$job" "$deps_file" <<'PY'
import sys, re

ci_file, job, deps_file = sys.argv[1], sys.argv[2], sys.argv[3]
deps = [l.strip() for l in open(deps_file) if l.strip()]
text = open(ci_file).read()

# Isolate the job block: from "<job>:" at column 0 to the next column-0 key.
m = re.search(r'^' + re.escape(job) + r':\n(.*?)(?=^\S)', text, re.S | re.M)
if not m:
    print(f"check_operator_image_changes: job {job} not found in {ci_file}")
    sys.exit(2)
block = m.group(1)

# Every `changes:` list in the block (default-branch and branch rules).
lists = re.findall(r'^\s+changes:\n((?:\s+- .*\n)+)', block, re.M)
if not lists:
    print(f"check_operator_image_changes: no changes: lists under {job}")
    sys.exit(2)
globs_per_rule = [[l.strip()[2:].strip() for l in lst.splitlines() if l.strip().startswith('- ')] for lst in lists]

def covers(glob, directory):
    """Does a GitLab `changes:` glob cover files directly under `directory`?"""
    if glob.endswith('/**/*'):
        prefix = glob[:-5]
        return directory == prefix or directory.startswith(prefix + '/')
    if glob.endswith('/*'):
        return directory == glob[:-2]
    return False

required_files = ['go.mod', 'go.sum', 'Dockerfile.loom-mills-operator', 'scripts/ci/buildkit-build.sh']
problems = []
for i, globs in enumerate(globs_per_rule, 1):
    for f in required_files:
        if f not in globs:
            problems.append(f"rule {i}: missing literal entry {f}")
    for d in deps:
        if not any(covers(g, d) for g in globs):
            problems.append(f"rule {i}: dependency directory {d} is not covered")

if problems:
    print(f"check_operator_image_changes: {job} changes: list is stale ({len(problems)} problem(s)):")
    for p in problems:
        print("  - " + p)
    print("Add the directory (or a covering glob) to every changes: list of the job in .gitlab-ci.yml.")
    sys.exit(1)
print(f"check_operator_image_changes: {len(deps)} operator dependency directories covered by {len(globs_per_rule)} rule(s)")
PY

#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
default_repo_root=$(dirname -- "$script_dir")
repo_root=${CLASSIFIER_PATTERN_REPO_ROOT:-$default_repo_root}
registry_file=${CLASSIFIER_PATTERN_REGISTRY_FILE:-$repo_root/pkg/mills/pipeline/classifier.go}
test_root=${CLASSIFIER_PATTERN_TEST_ROOT:-$repo_root/pkg/mills/pipeline}

fail() {
	printf 'classifier pattern coverage: %s\n' "$*" >&2
	exit 1
}

registry_entries() {
	sed -n '/classifier-pattern-registry:begin/,/classifier-pattern-registry:end/p' "$registry_file" |
		sed -n 's/^[[:space:]]*"\([^"]*\)"[[:space:]]*:[[:space:]]*"\([^"]*\)"[[:space:]]*,[[:space:]]*$/\1|\2/p'
}

validate_registry_format() {
	begin_count=$(grep -c 'classifier-pattern-registry:begin' "$registry_file" || true)
	end_count=$(grep -c 'classifier-pattern-registry:end' "$registry_file" || true)
	[ "$begin_count" -eq 1 ] || fail "registry must contain exactly one begin marker"
	[ "$end_count" -eq 1 ] || fail "registry must contain exactly one end marker"

	invalid_lines=$(sed -n '/classifier-pattern-registry:begin/,/classifier-pattern-registry:end/p' "$registry_file" |
		sed '1d;$d' |
		sed '/^[[:space:]]*$/d;/^[[:space:]]*\/\//d;/^[[:space:]]*"[^"]*"[[:space:]]*:[[:space:]]*"[^"]*"[[:space:]]*,[[:space:]]*$/d')
	[ -z "$invalid_lines" ] || fail "registry contains an invalid entry: $(printf '%s\n' "$invalid_lines" | head -n 1)"
}

check_coverage() {
	[ -f "$registry_file" ] || fail "registry does not exist: $registry_file"
	[ -d "$test_root" ] || fail "test root does not exist: $test_root"
	validate_registry_format

	entries=$(registry_entries)
	[ -n "$entries" ] || fail "no entries found in registry: $registry_file"
	sorted_entries=$(printf '%s\n' "$entries" | LC_ALL=C sort -t '|' -k1,1)
	[ "$entries" = "$sorted_entries" ] || fail "registry entries are not sorted by pattern ID"
	pattern_count=$(printf '%s\n' "$entries" | cut -d '|' -f 1 | wc -l | tr -d ' ')
	unique_pattern_count=$(printf '%s\n' "$entries" | cut -d '|' -f 1 | LC_ALL=C sort -u | wc -l | tr -d ' ')
	[ "$pattern_count" = "$unique_pattern_count" ] || fail "registry contains duplicate pattern IDs"

	printf '%s\n' "$entries" | while IFS='|' read -r pattern_id runbook_path; do
		[ -n "$pattern_id" ] || fail "registry contains an empty pattern ID"
		[ -n "$runbook_path" ] || fail "pattern $pattern_id has no linked runbook"

		if ! find "$test_root" -type f -name '*_test.go' -exec grep -F -q -- "$pattern_id" {} +; then
			fail "pattern $pattern_id has no Go test fixture under $test_root"
		fi

		runbook="$repo_root/$runbook_path"
		[ -f "$runbook" ] || fail "pattern $pattern_id links missing runbook: $runbook_path"
		grep -F -q -- "$pattern_id" "$runbook" ||
			fail "runbook $runbook_path does not reference pattern $pattern_id"
	done
}

expect_failure() {
	want=$1
	shift
	output_file=$self_test_root/output
	if "$@" >"$output_file" 2>&1; then
		fail "self-test expected failure containing: $want"
	fi
	grep -F -q -- "$want" "$output_file" ||
		fail "self-test failure did not contain '$want': $(cat "$output_file")"
}

self_test() {
	self_test_root=$(mktemp -d)
	trap 'rm -rf "$self_test_root"' EXIT HUP INT TERM
	mkdir -p "$self_test_root/pkg" "$self_test_root/docs"
	registry="$self_test_root/classifier.go"
	fixture="$self_test_root/pkg/classifier_test.go"
	runbook="$self_test_root/docs/runbook.md"
	pattern=external_dependency.example.failure
	{
		printf '%s\n' '// classifier-pattern-registry:begin'
		printf '\t"%s": "docs/runbook.md",\n' "$pattern"
		printf '%s\n' '// classifier-pattern-registry:end'
	} >"$registry"
	printf 'package fixture\n// fixture: %s\n' "$pattern" >"$fixture"
	printf '# Runbook\n\nPattern: `%s`\n' "$pattern" >"$runbook"

	CLASSIFIER_PATTERN_REPO_ROOT="$self_test_root" \
		CLASSIFIER_PATTERN_REGISTRY_FILE="$registry" \
		CLASSIFIER_PATTERN_TEST_ROOT="$self_test_root/pkg" "$0"

	printf 'package fixture\n' >"$fixture"
	expect_failure "has no Go test fixture" env \
		CLASSIFIER_PATTERN_REPO_ROOT="$self_test_root" \
		CLASSIFIER_PATTERN_REGISTRY_FILE="$registry" \
		CLASSIFIER_PATTERN_TEST_ROOT="$self_test_root/pkg" "$0"
	printf 'package fixture\n// fixture: %s\n' "$pattern" >"$fixture"

	rm "$runbook"
	expect_failure "links missing runbook" env \
		CLASSIFIER_PATTERN_REPO_ROOT="$self_test_root" \
		CLASSIFIER_PATTERN_REGISTRY_FILE="$registry" \
		CLASSIFIER_PATTERN_TEST_ROOT="$self_test_root/pkg" "$0"
	printf '# Runbook without the identifier\n' >"$runbook"
	expect_failure "does not reference pattern" env \
		CLASSIFIER_PATTERN_REPO_ROOT="$self_test_root" \
		CLASSIFIER_PATTERN_REGISTRY_FILE="$registry" \
		CLASSIFIER_PATTERN_TEST_ROOT="$self_test_root/pkg" "$0"

	registry_bad="$self_test_root/classifier-bad.go"
	sed '/classifier-pattern-registry:end/i\
\	broken registry entry
' "$registry" >"$registry_bad"
	expect_failure "registry contains an invalid entry" env \
		CLASSIFIER_PATTERN_REPO_ROOT="$self_test_root" \
		CLASSIFIER_PATTERN_REGISTRY_FILE="$registry_bad" \
		CLASSIFIER_PATTERN_TEST_ROOT="$self_test_root/pkg" "$0"

	printf '%s\n' 'classifier pattern coverage self-test: ok'
}

case ${1:-} in
	--self-test) self_test ;;
	"") check_coverage ;;
	*) fail "usage: $0 [--self-test]" ;;
esac

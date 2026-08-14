#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
config_path=${1:-"$repo_root/cmd/mcp-mentatlab/templates/mills-default-pipeline.yaml"}

case "$config_path" in
  /*) ;;
  *) config_path="$repo_root/$config_path" ;;
esac

cd "$repo_root"
LOOM_GUARDRAIL_CONFIG_PATH="$config_path" \
  go test ./pkg/mills/gates -run '^TestValidateGuardrailConfigEntryPoint$' -count=1

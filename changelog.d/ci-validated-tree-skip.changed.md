- **CI throughput**: main pipelines no longer re-run validation that the MR
  pipeline already performed on the identical tree. Jobs that also run on
  branch pipelines (lint, vet, test:unit, test:reliability,
  test:enterprise-smoke, test:integration, guardrails:docs-cli,
  lint:rbac-policy) source `scripts/ci/validated_tree_skip.sh`, which ends the
  job green on the default branch when the commit is a merge whose tree is
  byte-identical to its MR head — the shape every merge-queue and rebased
  auto-merge produces; a three-way merge of an un-rebased branch still runs in
  full. Main-only jobs (test:race, test:benchmark, security:*) and the image
  builds are unchanged. `CI_VALIDATED_TREE_SKIP=false` forces full
  validation; `VALIDATED_TREE_TOKEN` (read_api) adds a pipeline-status check.
  Branch caches fall back to main's warm Go cache (`fallback_keys`) so a
  branch's first pipeline stops rebuilding the module and build cache cold.

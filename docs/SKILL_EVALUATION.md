# Skill evaluation

Loom separates deterministic skill delivery checks from measurements of agent
behavior. A valid registry and complete bundle are prerequisites for evaluating
selection or task outcomes; they are not evidence that a host selected a skill.

The canonical sprint is
`plan-loom-skills-reliable-delivery-and-behavioral-evaluation-spri-04bf28`.
Its local [plan mirror](../.loom/plan-skills-enhancement-2026-09-13.md) and
[research review](../.loom/research-skills-enhancement-2026-09-13.md) describe the
proposed datasets and release criteria.

## Host feasibility probe

The [baseline fixtures](../mcp/skills/_eval/baseline/cases.json) establish whether
a host can expose loading evidence and produce independently inspectable output.
They are synthetic diagnostics, excluded from the future held-out dataset.

1. Record the source revision, fixture SHA-256, CLI version, authentication
   availability and exact invocation settings. Never record credentials.
2. Create a fresh temporary working directory for each case, containing the
   declared `keep.txt`. Install only the synthetic skill for positive and negative
   cases; omit it for the absent control. Enforce a read boundary that excludes
   other cases, prior outputs and grader data. Separate working directories and
   results outside the working roots are insufficient: workspace-write permits
   reads beyond the working directory.
3. Run each case with the host's normal sandbox, a bounded wall-clock timeout and
   fresh session state. Disable unrelated catalog entries for this invocation
   where supported. Do not change real installed skills or user configuration.
4. Inspect files independently. The positive receipt must contain all three
   expected values. A negative case must create the summary without a receipt.
   The absent control must fail the positive receipt grader. All cases must
   preserve `keep.txt`.
5. Inspect the tool trace for the exact fixture path and a successful read.
   Classify loading as **observed**, **inferred** from distinctive output alone,
   or **unavailable**. An agent's assertion that it used a skill is insufficient.
6. Record host failures, timeouts and missing usage/model fields as unavailable
   or null. These are neither behavioral failures nor successful evaluations.

The initial Codex invocation uses `exec --ignore-user-config --ephemeral --json
--skip-git-repo-check --sandbox workspace-write`, temporary `log_dir` and
`sqlite_home`, and invocation-local `skills.config` disable entries for discovered
home skills. Plugin and hook integrations are disabled for this synthetic probe;
execution rules and the sandbox remain active. The default host model is used;
its identity must be recorded from host evidence when exposed.

Codex documents skill disable entries in its [skills guide](https://developers.openai.com/codex/skills/)
and state-directory overrides in the [configuration reference](https://developers.openai.com/codex/config-reference/).
The installed CLI's help takes precedence over assumptions about flags available
in a newer release. Project skill discovery and loading trace visibility must
still be verified empirically.

## Current scope and release gate

The [2026-09-13 evidence](../mcp/skills/_eval/baseline/2026-09-13.json) fails the
feasibility gate. Codex read the positive fixture and produced its distinctive
receipt; the negative prompt produced a summary without a receipt. But the absent
control read `../positive/receipt.json` and copied its answer. The trace proves
cross-case contamination, so that control is invalid. Claude Code had no active
login, and no Claude model run was attempted. The actual Codex model identity was
not exposed in the retained JSONL, so this record leaves it null.

The first implementation increment covers authoring validation and delivery
integrity, with deterministic regression tests. Before S3 runs its benchmark,
use read-isolated case environments, verify the absent control rejects the
distinctive marker, and record actual model identity. Behavioral pilot changes
and activation-based release claims remain gated on completed host probes.

S1/S2 checks use temporary directories and cover malformed authoring, scaffold
defaults, registered planning arguments, failed resource delivery, manifest
publication, stale-file retirement and preservation of unmanaged or modified
files. Do not regenerate real home profiles as part of those tests.

The broader S3 runner, frozen routing/outcome datasets, controlled repeated
current/candidate comparisons and two-host canary/rollback evidence remain sprint
work. Require a specified batch budget before launching that paid evaluation
matrix. Keep unknown usage distinct from measured zero usage.

## Delivery manifest migration

New manifests include SHA-256 hashes of delivered files. Retiring a file requires
a matching prior hash. A missing legacy hash or modified stale file produces a
conflict and preserves both the file and previous manifest. Review that file
before manually moving it out of the managed path or deleting it. Do not invent
ownership hashes to suppress the diagnostic. An ordinary successful regeneration
establishes hashes for current generated files.

Delivery replaces individual files atomically and publishes the manifest after
copying and pruning succeed. It is not a transaction across the whole directory:
earlier successful copies can remain updated after a later failure. A failure
must therefore prevent an installation-success claim, and a retry must use a
verified source bundle. Whole-bundle rollback and catalog collision diagnostics
remain S2 work beyond this first increment.

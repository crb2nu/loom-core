# Harvest: `services/diff-surgeon` — confidence-scored fuzzy diff applier

> **Status:** contract documentation for a future port. **No code has been ported.**
> **Source repo:** `services/diff-surgeon` (Go), archived 2026-08-15 in the 2026-08 portfolio
> uplift. Citations are `path:line` relative to that repo's final `main`.
> **Why here:** loom-core is the named destination. The uplift synthesis flagged this as
> *"the most immediately applicable idea in the whole survey set"* — and it is greenfield:
> loom-core contains no fuzzy diff applier today.

## 1. The one-paragraph case

Fuzzy hunk location under line-number shift, with bounded tolerance and a calibrated
confidence score, is the genuinely hard part of diff-surgeon, and nothing in the current fleet
reproduces it. loom-core's `morph_fast_apply` is a **remote paid LLM API with no offline
story** (§5). diff-surgeon is **local, offline, deterministic, confidence-scored, and
reversible**. They are not competitors: Morph turns *intent* into an edit when you have no
diff; diff-surgeon applies an *existing* diff whose line numbers have rotted. The second
capability does not exist in loom-core.

## 2. Repo shape

| Property | Value | Cite |
|---|---|---|
| Module | `gitlab.flexinfer.ai/services/diff-surgeon` | `go.mod:1` |
| Go | `1.22.0` (CI image `golang:1.22`) | `go.mod:3`, `.gitlab-ci.yml:18` |
| Binary | `bin/surgeon` from `./cmd/surgeon` | `Makefile:8` |
| Vendored | yes, `GOFLAGS: -mod=vendor` | `.gitlab-ci.yml:15` |
| Size | 4,043 non-vendor Go LOC across 13 files | — |

```
cmd/surgeon/       main.go(417) git_apply.go(234) mcp_serve.go(368) serve.go(237) env.go(37)
internal/parser/   parser.go(278)   + parser_test.go(53)
internal/matcher/  matcher.go(421) similarity_fallback.go(21) similarity_fiaccel.go(29)
internal/patcher/  patcher.go(1097) + patcher_test.go(675)
pkg/types/         types.go(176)     — the entire public data model
mcp/context/       registry.yaml     — MCP server descriptor (server name `diff_surgeon`)
testdata/          example.diff, repo/src/auth.py
```

Dependencies (`go.mod:5-16`): `spf13/cobra`, `google/uuid`, `pmezard/go-difflib`,
`libs/mcp-go v0.1.0`, and `libs/fi-accel/go/fiaccel` behind a build tag. **No third-party
diff/patch library** — parser, matcher and applier are all hand-rolled.

Six subcommands (`cmd/surgeon/main.go:28-33`): `apply`, `analyze`, `rollback`, `serve`
(HTTP), `mcp-serve` (stdio MCP, three tools `patch/apply` · `patch/analyze` ·
`patch/rollback`), `git-apply`.

`apply` flags and defaults (`main.go:240-253`), each with an env override via
`cmd/surgeon/env.go:8-37`: `--patch/-p`, `--base` (`.`, `SURGEON_BASE_PATH`), `--fuzzy`
(`0.8`, `SURGEON_FUZZY_THRESHOLD`), `--window` (`100`, `SURGEON_SEARCH_WINDOW`),
`--backup-dir` (`.surgeon-backups`, `SURGEON_BACKUP_DIR`), `--dry-run`, `--backup` (`true`),
`--interactive`, `--diff`, `--candidates` (`0`), `--strict`, `--strict-min` (`0.99`),
`--strict-shift` (`0`), `--json`. Empty or unparseable env values silently fall back.

## 3. The fuzzy matcher — `internal/matcher`

This is the asset.

### 3.1 The anchor set

`FindBestMatch` (`matcher.go:64-98`) matches on **context lines plus deletion lines** — exactly
the lines that must exist in the pre-image:

```go
// internal/matcher/matcher.go:295-303
func extractContextAndDeletions(hunk *types.Hunk) []string {
	var lines []string
	for _, line := range hunk.Lines {
		if line.Type == types.LineContext || line.Type == types.LineDeletion {
			lines = append(lines, line.Content)
		}
	}
	return lines
}
```

There is **no leading/trailing anchor distinction** — the whole pre-image block is matched as
one contiguous window, and the match position is the block start. Anchoring is positional, not
landmark-based.

### 3.2 Three-tier location cascade

**Tier 1 — exact position** (`tryExactMatch`, `matcher.go:177-195`). Score the anchor block at
`hunk.OriginalStart - 1`; accept only at `similarity >= 0.99`. Strategy `"exact"`.

**Tier 2 — expanding nearby window** (`searchNearby`, `matcher.go:198-230`):

```go
// internal/matcher/matcher.go:81-89
windows := []int{5, 10, 20, 50, 100}
for _, window := range windows {
	if window > m.searchWindow {
		break
	}
	if candidate := m.searchNearby(contextLines, fileLines, hunk.OriginalStart, window); candidate != nil {
		return candidate, nil
	}
}
```

Each pass scans `[start-1-window, start-1+window)`, keeps the highest-scoring position
`>= fuzzyThreshold`, and short-circuits at `>= 0.99` (`:222-225`). Strategy `"nearby"`.
**Because the loop returns as soon as any window yields a candidate, the tightest window
wins** — this is the primary bias toward small shifts. `m.searchWindow` only *truncates* the
ladder; it never adds windows beyond 100.

**Tier 3 — whole-file fuzzy scan** (`fuzzySearch`, `matcher.go:233-266`), with a cheap
first-line prefilter before paying for the full block score:

```go
// internal/matcher/matcher.go:243-251
for i := 0; i <= len(fileLines)-len(context); i++ {
	// Quick check: first line should be similar
	if levenshteinSimilarity(firstLine, normalizeWhitespace(fileLines[i])) < m.fuzzyThreshold {
		continue
	}
	similarity := m.calculateSimilarity(context, fileLines[i:i+len(context)])
	if similarity >= m.fuzzyThreshold {
		...
	}
}
```

Strategy `"fuzzy"`; returns the global argmax with no early exit.

> **Failure returns `(nil, nil)`** (`matcher.go:97`) — a nil candidate with a nil error.
> Callers must nil-check, not err-check.

### 3.3 The degenerate case — the sharpest edge in the design

A hunk with no context and no deletions (a pure insertion) cannot be located at all:

```go
// internal/matcher/matcher.go:66-73
contextLines := extractContextAndDeletions(hunk)
if len(contextLines) == 0 {
	// No context, can only try exact position
	return &types.MatchCandidate{
		Line:       hunk.OriginalStart,
		Confidence: 0.5,
		Strategy:   "no_context",
	}, nil
}
```

`0.5` is a magic literal appearing twice (`matcher.go:71`, `:112`). It is neither above nor
below a threshold in any meaningful sense — and under the default 0.8 it is still **returned
as a match** by `FindBestMatch`, because the threshold is not applied on this path. Only
strict mode rejects it. **A port must make this explicit** rather than inheriting a bare
constant.

### 3.4 The confidence score — exact formula

**Block level** (`matcher.go:269-292`):

```go
func (m *Matcher) calculateSimilarity(expected, actual []string) float64 {
	if len(expected) != len(actual) {
		// Penalize length mismatch but don't fail completely
		ratio := float64(min(len(expected), len(actual))) / float64(max(len(expected), len(actual)))
		return ratio * m.calculateSimilarity(
			expected[:min(len(expected), len(actual))],
			actual[:min(len(expected), len(actual))],
		)
	}
	if len(expected) == 0 {
		return 1.0
	}
	var totalSimilarity float64
	for i := range expected {
		totalSimilarity += levenshteinSimilarity(
			normalizeWhitespace(expected[i]),
			normalizeWhitespace(actual[i]),
		)
	}
	return totalSimilarity / float64(len(expected))
}
```

**Unweighted arithmetic mean of per-line similarity, scaled by a length ratio when the blocks
differ in length.** No positional weighting — a mismatch in the first anchor line costs
exactly as much as one in the last.

**Line level** (pure-Go default, `similarity_fallback.go:5-21`):

```go
func levenshteinSimilarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}
	lengthDiff := abs(len(a) - len(b))
	maxLen := max(len(a), len(b))
	if float64(lengthDiff)/float64(maxLen) > 0.5 {
		return 0.0
	}
	distance := levenshteinDistance(a, b)
	return 1.0 - float64(distance)/float64(maxLen)
}
```

Three cliffs to port deliberately: exact string ⇒ `1.0`; **either side empty ⇒ `0.0`** (a
blank context line vs a non-blank line scores zero, not "close"); and the **length-ratio
guillotine** — lengths differing by more than 50% of the longer return `0.0` without computing
distance. That last is a performance guard that doubles as a semantic rule.

`levenshteinDistance` (`matcher.go:332-365`) is a standard two-row DP and is **byte-indexed**
(`a[i-1] != b[j-1]`), so multi-byte UTF-8 compares per byte. Fine for ASCII; **a port
targeting non-Latin identifiers should switch to runes.**

> **The biggest correctness caveat.** `normalizeWhitespace` (`matcher.go:306-324`) collapses
> every run of `unicode.IsSpace` to a single ASCII space and trims the ends, so
> **indentation is invisible to the matcher**. For Python that is a real hazard: a hunk can
> match a block at the wrong nesting depth with confidence `1.0`. The applier does not
> re-check indentation either (`patcher.go:571`, `:581` use the same normalized comparison).
> **A port should add an indentation-aware tie-break or a post-match indent assertion.**

### 3.5 Thresholds — every constant that gates a decision

| Constant | Value | Meaning | Cite |
|---|---|---|---|
| default `FuzzyThreshold` | `0.8` | min block similarity for nearby/fuzzy | `matcher.go:51`, `patcher.go:80` |
| default `SearchWindow` | `100` | caps the window ladder | `matcher.go:54`, `patcher.go:84` |
| exact-match gate | `0.99` | Tier 1 acceptance | `matcher.go:185` |
| nearby early-exit | `0.99` | stop scanning the window | `matcher.go:223` |
| `no_context` confidence | `0.5` | pure-insertion hunks | `matcher.go:71`, `:112` |
| probe budget | `max(limit*20, 50)` | heap size for `--candidates` | `matcher.go:122` |
| default `strictMin` | `0.99` | strict-mode floor | `patcher.go:126-127`, `main.go:251` |
| default `strictMaxShift` | `0` | strict-mode max line drift | `main.go:252`, `patcher.go:605-607` |
| fallback-suggestion threshold | `0.0001` | surface *any* candidate for the error message | `patcher.go:1078` |
| analyze `likely` | `>= 0.8` | applicability band | `patcher.go:910` |
| analyze `unlikely` | `>= 0.5` | applicability band | `patcher.go:912` |
| analyze `impossible` | `< 0.5` | applicability band | `patcher.go:914` |

Behavior per band, default config, non-strict:

| Confidence | Outcome |
|---|---|
| `1.0` | exact or perfect fuzzy; applied — `shift` may still be non-zero |
| `>= 0.99` | Tier 1 accepts; strict accepts iff shift `<= strictMaxShift` |
| `[0.8, 0.99)` | Tier 2/3 accept and apply; **strict rejects** with a `strict_rejected` conflict |
| `= 0.5` | only the `no_context` pseudo-match; applied non-strict, rejected strict |
| `< 0.8` | `FindBestMatch` returns nil ⇒ already-applied probe ⇒ else conflict |

Strict acceptance is a **conjunction of confidence and displacement**
(`patcher.go:598-609`): `match.Confidence >= strictMin && abs(match.Line-expectedLine) <=
strictMaxShift`. With both defaults (`0.99` / `0`) strict mode is effectively *"apply only if
`git apply` would have applied it."*

> **There is a second, independent confidence gate at write time.** Even after a block match
> is accepted, every individual line is re-verified (`patcher.go:565-585`): a context or
> deletion line scoring below `fuzzyThreshold` aborts the hunk with `"context mismatch at line
> N"` / `"deletion mismatch at line N"`. So a block mean of 0.85 can pass while one deletion
> at 0.6 aborts. **A port must reproduce both gates** — this is what stops the mean from
> washing out a single badly-wrong deletion. Note deletions are matched but not echoed into
> output (`:584`), and **additions are inserted verbatim with no verification** (`:586-588`).

### 3.6 Candidate enumeration and tie-breaking

`FindTopMatches` (`matcher.go:102-174`) powers `--candidates N`: probe every offset by
first-line similarity, keep the top `maxProbe = max(limit*20, 50)` via a **min-heap**
(`:122-139`) — `O(n log k)` rather than `O(n·k)` — then fully score only the survivors,
deduplicating by line number (`:162-171`).

Tie-break (`matcher.go:416-421`): confidence descending, then **closest to the expected
line**. Inside Tier 2/3 the tie-break is weaker — strict `>` means the **first (lowest-index)
position wins ties** (`:218-220`, `:259-261`), so **ambiguous matches are silently resolved to
the earlier one.** It does not refuse ambiguity.

> **Strategy strings mean different things depending on origin.** `FindTopMatches` assigns
> strategy *post hoc* by distance (`classifyStrategy`, `matcher.go:395-403`), not by which
> tier found the match. A port should unify this.

### 3.7 Failure modes and refusals

- **Binary patches**: refused before any matching — apply emits a conflict
  `"binary patches are not supported"` (`patcher.go:142-163`); analyze reports
  `impossible` / `binary_unsupported` (`:706-722`). The parser detects both
  `GIT binary patch` (`parser.go:78`) and `Binary files … differ` (`parser.go:19`).
- **Path escape** (`patcher.go:1085-1097`): absolute paths, `..`, and `..`-prefixed relative
  paths are rejected and abort the whole file.
- **Hunk start past EOF**: `"hunk start line %d is past end of file"` (`patcher.go:558`).
- **No match**: before conceding it runs the already-applied probe (below); failing that it
  emits a conflict carrying a best-effort suggestion from a throwaway matcher at threshold
  `0.0001` (`patcher.go:1075-1083`), so the operator always gets a "closest thing we saw"
  pointer plus `"try lowering fuzzy threshold (current %.2f)"`.

### 3.8 The idempotency probe — the nicest trick in the repo

```go
// internal/patcher/patcher.go:611-625
func findAlreadyAppliedMatch(m *matcher.Matcher, hunk *types.Hunk, fileLines []string) *types.MatchCandidate {
	after := extractContextAndAdditions(hunk)
	...
	afterHunk := &types.Hunk{OriginalStart: hunk.OriginalStart, ...}
	for _, l := range after {
		afterHunk.Lines = append(afterHunk.Lines, types.Line{Type: types.LineContext, Content: l})
	}
	match, _ := m.FindBestMatch(afterHunk, fileLines)
	return match
}
```

It synthesizes a pseudo-hunk from the **post-image** and reuses the same matcher. Scoring
`>= fuzzyThreshold` means the hunk is already applied: recorded as `already_applied`, counted
in `HunksSkipped`, and — critically — **`lineOffset` is still advanced by `adds - dels`** so
subsequent hunks stay aligned (`patcher.go:365-383`). Invoked at three sites: no-match
(`:366`), strict rejection (`:420`), and post-write failure (`:491`).

Paired with **cumulative drift tracking**: each applied hunk advances `lineOffset` and the next
hunk's expected start is pre-shifted (`patcher.go:340`, `:350-351`, `:527`), so the matcher
searches around a *corrected* prediction. That is why the ±5 window suffices for most
multi-hunk files.

### 3.9 Determinism caveat

An accelerated similarity path exists behind a build tag (`similarity_fiaccel.go:1-19`,
`//go:build fiaccel`) delegating to `fiaccel.StringSimilarity` and **falling back silently on
error**. fi-accel's similarity is **not guaranteed to equal the pure-Go value**, so confidence
numbers are reproducible *within* a build configuration, not necessarily across. Nothing in
the Makefile or CI builds with `fiaccel`, so the default is pure Go. **A port should either
drop the tag or pin one implementation.**

### 3.10 Types worth quoting

`pkg/types/types.go` is the whole contract in 176 lines with zero behavior.

```go
// pkg/types/types.go:170-176
type MatchCandidate struct {
	Line       int     `json:"line"`
	Confidence float64 `json:"confidence"`
	Strategy   string  `json:"strategy"`
	Context    string  `json:"context"` // Matched context snippet
}

// pkg/types/types.go:88-97
type HunkAdjustment struct {
	HunkIndex     int              `json:"hunk_index"`
	OriginalLine  int              `json:"original_line"`
	AppliedLine   int              `json:"applied_line"`
	Shift         int              `json:"shift"`
	Confidence    float64          `json:"confidence"`
	MatchStrategy string           `json:"match_strategy"` // exact, nearby, fuzzy
	Candidates    []MatchCandidate `json:"candidates,omitempty"`
}
```

Plus `ConflictInfo` (`:99-108`, carrying `ExpectedContext` / `ActualContext` / `Suggestions` /
`Candidates`), `Hunk`/`Line`/`LineType` (`:19-44`), `ApplyRequest`/`ApplyResult`/`FileResult`
(`:46-79`), `Applicability` (`:139-144`) and `HunkAnalysis` (`:146-159`), `BackupInfo`
(`:161-168`).

> `FileResult.Confidence` is the mean over **applied** hunks only (`patcher.go:533-535`) —
> skipped and conflicted hunks do not dilute it, which **flatters the number**. Fix or document
> in a port.

The interactive seam is small and clean, worth lifting verbatim
(`patcher.go:26-49`): `type HunkDecider func(ctx HunkContext) (HunkDecision, error)`, where
`HunkContext` carries `Action` / `File` / `HunkIndex` / `HunksTotal` / `HunkHeader` /
`HunkLines` / `ExpectedLine` / `MatchedLine` / `MatchStrategy` / `MatchConfidence` /
`ExpectedContext` / `MatchedContext`.

## 4. `surgeon git-apply` — the worktree strategy

`cmd/surgeon/git_apply.go:20-234`. Flags: `--patch/-p`, `--repo` (`.`), `--ref`, `--branch`,
`--commit`, `--message`, `--keep-worktree`, plus the shared fuzzy/window/backup-dir/dry-run set
(`:152-164`).

### 4.1 Exact sequence

1. **Reject `--commit --dry-run`** up front (`:40-42`).
2. Read the patch from `--patch` or stdin (`:45-60`).
3. `exec.LookPath("git")` — hard fail if absent (`:66-68`).
4. **5-minute ceiling on the whole operation**: `context.WithTimeout(cmd.Context(), 5*time.Minute)`
   (`:70-71`). Every git call is a `CommandContext`, so a hang is bounded.
5. Resolve `repoRoot` via `git -C <repo> rev-parse --show-toplevel` (`:73`).
6. `needsWorktree := ref != "" || branch != ""` (`:80`). **With no `--ref`/`--branch`/`--commit`
   this command applies directly to the working tree — no isolation at all.**
7. **Auto-branch for `--commit`** (`:82-85`): `branch = fmt.Sprintf("surgeon/%s",
   uuid.New().String()[:8])` and `needsWorktree = true`. So `--commit` alone always gets
   isolation.
8. **Create the worktree** (`createWorktree`, `:191-226`) at
   **`<repoRoot>/.surgeon-worktrees/<8-hex-uuid>`**, mode `0700`. The directory is named by
   uuid, *not* by branch. Target ref defaults to `HEAD` (`:199-202`). Three cases: existing
   `--branch` ⇒ `git worktree add <wt> <branch>`, and combining it with `--ref` is a hard error
   (`:205-214`); new `--branch` ⇒ `git worktree add -b <branch> <wt> <targetRef>` (`:216-218`);
   no branch ⇒ `git worktree add --detach <wt> <targetRef>` (`:222-224`), so nothing can
   accidentally advance a branch.
9. **Apply inside it** with `basePath = worktreePath` — using **`patcher.Apply`, not `git
   apply`**. The fuzzy applier does the work; git only provides the sandbox. `ReturnDiff:
   false` is hardcoded (`:106`) and `--interactive` is unavailable here.
10. **On failure, stop and preserve** (`:119-124`): `"Worktree preserved at %s for
    inspection"`, then `"patch did not apply cleanly"`.
11. **Optional commit** (`:126-138`): `git add -A` then `git commit -m <msg>`, default message
    **`"Apply patch via Diff Surgeon"`**. **No author/committer override, no trailer, no
    sign-off** — the commit inherits ambient `user.name`/`user.email`.
12. **Teardown** (`:140-146`, `:228-234`): unless `--keep-worktree`, `git worktree remove
    --force <path>` then `os.RemoveAll`. Removal failure is a **warning, not an error**.

### 4.2 The rollback guarantee, precisely

**What is atomic: nothing, at the multi-file level.** Be blunt about this in a port.

- **The worktree is a sandbox, not a transaction.** It guarantees the *canonical checkout* is
  untouched when `--ref`/`--branch`/`--commit` are used. That is the real guarantee, and it is
  a good one.
- **Files are written incrementally** (`patcher.go:538-543`, inside the per-file loop at
  `:141-188`). A hard error on file 3 leaves files 1 and 2 already modified
  (`:179-183` breaks the loop and sets `Success=false`).
- **Backups are whole-file snapshots captured lazily** — only when the first hunk of that file
  is about to be written (`patcher.go:520-523`). Files with zero applied hunks are never
  snapshotted and never written, so **a strict-mode refusal genuinely leaves the file
  byte-identical** (asserted at `patcher_test.go:150-156`).
- **The backup manifest is persisted last** (`saveBackup`, `:191-196`, writing
  `<base>/<backupDir>/<id>/backup.json`, `:973-990`). **If the process is killed mid-apply,
  files are already modified and no manifest exists — that state is unrecoverable via `surgeon
  rollback`.** Worse, a manifest write failure is downgraded to a message on an otherwise
  successful result: `"applied but backup failed: %v"` (`:193`).
- **Rollback is a restore, not an undo** (`:940-971`): delete everything in `CreatedFiles`,
  then overwrite each `Files` entry with stored content. It does **not** verify current content
  matches what was applied, so rolling back after further edits silently discards them.
- **Conflicts ≠ no changes.** `result.Success` is false if *any* conflict exists (`:199-201`),
  but hunks that did match were already written. A "failed" `git-apply` leaves a
  **partially-patched worktree** — which is exactly why it preserves the worktree.
- Backup IDs are 8 hex chars from a UUID (`:133`) — same width as the worktree name. Widen in
  a port.

**Summary for a port: the worktree *is* the rollback mechanism.** `.surgeon-backups` is a
convenience for the no-worktree path; discarding the worktree is the only true all-or-nothing
undo.

### 4.3 Mapping onto this workspace — and why to drop the git plumbing

The workspace mandates worktree-first development with worktrees under `{repo}/.worktrees/`
allocated through `agent_worktree_allocate` (workspace `AGENTS.md`). diff-surgeon's
`.surgeon-worktrees/<uuid>` **predates and conflicts with** that convention: different root,
uuid instead of branch name, and invisible to `bin/workspace-clean --report --worktrees` and
to the assignment ledger.

loom-core already implements the sanctioned path — `pkg/agentcontext/svc_worktree.go:71-86`
defaults `baseDir` to `filepath.Join(repoPath, ".worktrees")` and runs
`git worktree add -b <branch> <absPath> <baseBranch>`, with tracked `WorktreeAssignment`
records (`pkg/agentcontext/worktree.go:32-60`), presence linkage, TTL, disk accounting,
orphan-on-agent-exit (`worktree.go:26-28`), and persistence. Handlers
`HandleWorktreeAllocate` / `HandleWorktreeRelease` / `HandleWorktreeList` at
`pkg/agentcontext/worktree.go:13-23`. There is also `cmd/mcp-git-worktree` exposing
`git_worktree_list` / `_add` / `_remove` / `_prune` (`main.go:67-121`).

**Port conclusion: drop `createWorktree`/`removeWorktree` entirely** (~45 lines). loom-core
already does it better and with bookkeeping. The porting value is `patcher` + `matcher`.

## 5. `--interactive` per-hunk review loop

`cmd/surgeon/main.go:110-164`. Worth keeping as a **UX reference**.

Guarded against stdin conflict (`:63-65`): `--interactive` is refused when the patch comes
from stdin, since prompt and patch would fight over the same fd.

Three decision points via `HunkContext.Action` (`:119-140`): `create_file`, `delete_file`,
`apply_hunk`. For a hunk the display is:

```
File: src/auth.py (hunk 1/2)
Match: expected line 42 -> 45 (nearby, confidence 0.98)
@@ -42,7 +42,9 @@ def validate_token(token: str) -> bool:
 <hunk body, rendered with +/-/space prefixes>
Matched context (first 5 lines):
  <5 lines from the file at the matched position>
```

Body rendering is `renderHunkLines` (`:258-271`); matched context is capped at 5 lines
(`patcher.go:1034-1044`).

> **Showing the confidence and the shift *before* asking is the key UX move** — the operator
> decides on evidence, not vibes. That is the part to carry into any surface, MCP included.

Bindings (`:142-162`), prompt `"Apply? [Y]es/[n]o/[a]ll/[q]uit: "`:

| Key | Action |
|---|---|
| `y` / `yes` / **Enter** | apply this hunk |
| `n` / `no` | skip, recorded as `SkippedHunk{Reason: "skipped by user"}` |
| `a` / `all` | apply this and all remaining without prompting |
| `q` / `quit` | abort with error `"aborted"` |
| anything else | reprint `Please enter y, n, a, or q.` and re-prompt |

Capital `Y` signals the default and bare Enter accepts. `EOF` is treated as a readable line
(`:145-147`), so a closed stdin does not crash mid-review. Aborting with `q` propagates up
through `ApplyWithOptions` → `applyFile` (`patcher.go:467-469`) and returns immediately —
**leaving already-applied files written** (same non-atomicity as §4.2). There is no "undo last
hunk", no "edit hunk", no "split hunk": a deliberately narrow subset of `git add -p`.

## 6. Contrast with `morph_fast_apply`

Everything below is verified against loom-core source and registry config in this repo.

### 6.1 What `morph_fast_apply` actually is

`cmd/mcp-morph-fast-apply/main.go`, 332 lines, registering two identical tools `edit_file` and
`morph_edit_file` (an explicit alias, `main.go:83-130`), both taking `{path, instruction,
update}`, all required (`:102`).

- **Requires an API key or refuses**: `apiKey = strings.TrimSpace(os.Getenv("MORPH_API_KEY"))`
  (`:136`); empty ⇒ `mcperror.NotConfigured("MORPH_API_KEY", …)` (`:149-151`). Asserted by
  `TestGetConfigDefaultsAndValidation` (`main_test.go:24-31`). The registry sources it from the
  macOS keychain: `MORPH_API_KEY: "${keychain:MORPH_API_KEY}"` (`mcp/context/registry.yaml:1185`).
- **Endpoint and model**: `https://api.morphllm.com/v1` (`:140-141`), model `morph-v3-large`
  (`:146`), overridable via `MORPH_BASE_URL` / `MORPH_MODEL`; registry pins
  `MORPH_MODEL: "${env:MORPH_APPLY_MODEL:-morph-v3-large}"` (`registry.yaml:1186-1187`). The
  call is an OpenAI-shaped chat completion to `baseURL + "/chat/completions"` with
  `Authorization: Bearer` (`:250-256`).
- **Network is mandatory.** `httpClient.Do` failure yields `"failed to call Morph API: %w"`
  (`:258-260`), 90s client timeout (`:36-38`) matching `registry.yaml:1180`. No cache, no local
  model, no degraded path. **Offline it simply cannot edit a file.**
- **Non-deterministic.** The request body is exactly `{model, messages, stream:false}`
  (`:234-243`) — **no temperature, no top_p, no seed**. The output is an LLM sample written
  over the file wholesale with no diff, no review, and **no backup** (`:312-322`).
- It packs **the entire current file** into every request (`:230-232`), which the source itself
  notes (`:290-292`).
- Both tools are on the auto-approve list — `always_allow: [edit_file, morph_edit_file]`
  (`registry.yaml:1182-1184`) — so this destructive, non-deterministic write runs **without a
  confirmation prompt**.
- What it does well: path confinement via `pathsec.ValidatePath` against
  `MORPH_WORKSPACE_ROOT`/`WORKSPACE_ROOT`/cwd (`:156-194`, traversal rejection asserted at
  `main_test.go:65-67`), 10 MB input / 20 MB response caps (`:30-33`, `:265-272`), token
  accounting through `llmusage.Observer` including cached-prefix tracking (`:302-306`), and
  OTel tracing (`:104`, `:130`). It is a well-built client. The question is what it is a client
  *of*.

### 6.2 Capability comparison

| Capability | diff-surgeon | `morph_fast_apply` |
|---|---|---|
| Execution locus | in-process, pure Go | remote HTTPS (`main.go:250`) |
| Works offline | **yes** — no network code anywhere | **no** (`:258-260`) |
| Marginal cost | zero | per-token API billing (`:285-295`) |
| Credential required | none | `MORPH_API_KEY` or refuses (`:149-151`) |
| Deterministic | **yes**¹ | **no** — unseeded LLM sample (`:234-243`) |
| Input format | unified diff (standard, reviewable) | prose instruction + elided code sketch (`:93-100`) |
| Confidence score | **0–1 per hunk** (`types.go:172`) | none |
| Rejects low confidence | `--strict` + thresholds (`patcher.go:598-609`) | no gate of any kind |
| Dry run | `--dry-run` | none |
| Preview diff | `--diff` / `return_diff` | none |
| Interactive review | `--interactive` | none; auto-approved (`registry.yaml:1182`) |
| Backup / rollback | `.surgeon-backups` + `surgeon rollback` (`patcher.go:940`) | **none** — direct overwrite (`:320`) |
| Multi-file per call | yes, one patch → N files (`patcher.go:141`) | no, one `path` per call (`:89`) |
| Idempotent re-apply | detected and skipped (`patcher.go:611-625`) | undefined |
| Whole-file re-upload | no | **yes, every call** (`:230-232`) |
| Path confinement | `cleanRelativePath` (`patcher.go:1085`) | `pathsec.ValidatePath` (`:189`) |
| Latency | local CPU, ms | network + inference, up to 90s |
| Handles semantic intent | **no** — needs a real diff | **yes**, that is the point |
| Observability | JSON result | OTel spans + token metering |

¹ With one asterisk — see §3.9.

**The honest read: these are not competitors.** Morph turns *intent* into an edit when you
have no diff yet. diff-surgeon turns an *existing diff* into a correct application when the
diff exists but its line numbers have rotted. **The gap in loom-core is real:** today there is
no offline, deterministic, reviewable path for applying a drifted diff. That gap is exactly
diff-surgeon's shape.

## 7. Tests

**14 test functions, 2 files, 728 lines** — `internal/patcher/patcher_test.go` (675 lines, 12
funcs) and `internal/parser/parser_test.go` (53 lines, 2 funcs). No benchmarks, no fuzz
targets. **`internal/matcher` and `cmd/surgeon` have no test files at all** — the matcher is
tested only transitively.

| Test | Cite | Contract encoded |
|---|---|---|
| `TestApplyAndRollback_ModificationWithBackup` | `patcher_test.go:28` | apply → backup ID → rollback restores byte-identical |
| `TestApplyAndRollback_NewFile` | `:101` | creation, `CreatedFiles`, rollback deletes |
| `TestApplyAndRollback_DeleteFile` | `:143` | deletion + restore |
| `TestApplyWithOptions_SkipAllHunks` | `:194` | `HunkDecider` returning false leaves the file untouched |
| `TestApply_ReportsConflictOnDeletionMismatch` | `:256` | mismatched deletion ⇒ `ConflictInfo` |
| `TestApply_ReturnDiff_DryRun` | `:314` | diff produced, nothing written |
| **`TestApply_IdempotentAlreadyApplied`** | `:379` | **apply twice ⇒ second run succeeds, zero conflicts, hunks skipped** |
| **`TestApply_StrictMode_RejectsShiftedMatch`** | `:445` | **+3 shift, `StrictMin 0.99` / `StrictMaxShift 0` ⇒ refuse, emit candidates, file byte-identical** |
| `TestAnalyzeWithOptions_DetailsAndAlreadyApplied` | `:507` | `Details: true` populates `HunksDetails` |
| `TestAnalyzeWithOptions_StrictRejectsShiftedMatch` | `:567` | analyze agrees with apply |
| `TestApply_BinaryPatchIsConflict` | `:621` | binary ⇒ conflict, not silent skip |
| `TestAnalyze_BinaryPatchIsImpossible` | `:648` | binary ⇒ `impossible` |
| `TestParse_DiffGitHeaderPopulatesPaths` | `parser_test.go:5` | `diff --git` ⇒ both paths |
| `TestParse_BinaryFilesMarkerSetsIsBinary` | `:35` | `Binary files … differ` ⇒ `IsBinary` |

**Style: fixture-constant + real filesystem, not table-driven.** One shared unified diff drives
most cases (`patcher_test.go:12-26`), and — this is the clever bit — **drift is controlled by
varying the number of filler lines** written into a `t.TempDir()` file.
`TestApply_IdempotentAlreadyApplied` writes 40 fillers so the target lands exactly at the
hunk's stated line 42 (`:36-38`); `TestApply_StrictMode_RejectsShiftedMatch` writes 43, with
the comment `// shift the target function down by +3 lines` (`:102`). **That single-integer
knob is how the suite exercises the matcher's shift tolerance without any matcher-level
test.**

> **No test asserts a specific confidence value.** The suite pins *decisions* (applied /
> skipped / conflicted / unchanged), not scores. Consequence for a port: **you are free to
> change the formula as long as the same decisions come out — but you get no regression net on
> the numbers.** Adding a table-driven matcher test with explicit `(expected, actual) →
> confidence` rows is the highest-value test improvement, and it should be written **before**
> porting so current behavior is captured first.

The strongest single assertion, and the one a port must not lose:

```go
// internal/patcher/patcher_test.go:150-156
afterBytes, err := os.ReadFile(authPath)
if err != nil { t.Fatal(err) }
if string(afterBytes) != original {
	t.Fatalf("expected file to remain unchanged when strict mode refuses match")
}
```

All tests are `t.Parallel()`; CI runs `-race -cover` (`.gitlab-ci.yml:57`). For comparison,
loom-core's `cmd/mcp-morph-fast-apply/main_test.go` has 9 test functions in 233 lines, all
`httptest`-stubbed against a fake Morph endpoint (`main_test.go:57-62`) — good coverage of
config, path safety and error paths; **none of edit quality, which is untestable by
construction.**

## 8. Port notes

### 8.1 What to port

| Component | Verdict |
|---|---|
| `internal/matcher` (471 lines) | **Port.** This is the asset. |
| `internal/patcher` apply/analyze/idempotency (~700 lines) | **Port.** The already-applied probe and dual-gate write are hard-won. |
| `pkg/types` (176 lines) | **Port**, trimmed to what is used. |
| `internal/parser` (278 lines) | Port or replace — unified-diff parsing is commodity. |
| `createWorktree`/`removeWorktree` (`git_apply.go:191-234`) | **Drop.** Superseded by `agentcontext.WorktreeSvc` (§4.3). |
| `.surgeon-backups` (`patcher.go:973-1008`) | Keep for the no-worktree path; not the primary safety story. |
| `cmd/surgeon/serve.go` HTTP API | **Drop.** loom-core speaks MCP. |
| `--interactive` loop (`main.go:110-164`) | Keep the **design**, not the TUI. Surface confidence, shift and matched context in the tool result and let the agent decide. |

### 8.2 Where it lands, and what it composes with

`pkg/patch/` (new), with `pkg/patch/matcher` and `pkg/patch/apply` subpackages, matching the
existing `pkg/<domain>/` convention (`pkg/agentcontext`, `pkg/codebase`, `pkg/mills`). The MCP
surface goes in `cmd/mcp-diff-surgeon/`, scaffolded per the `loom-go-mcp-scaffold` skill and
wired into the Makefile and `mcp/context/registry.yaml`.

**Verified greenfield:** a grep for `fuzzy|UnifiedDiff|[Ll]evenshtein` across `pkg/`,
`internal/`, `cmd/` returns only unrelated hits (`pkg/mills/gates/diff_size.go`,
`secret_scan.go`, GitLab MR clients), and `surgeon|diff-surgeon|diff_surgeon` appears nowhere
in loom-core.

| Existing loom-core surface | Use |
|---|---|
| `pkg/agentcontext/svc_worktree.go:49-128` | replaces `createWorktree` — tracked, TTL'd, orphan-aware, `.worktrees/<branch>` |
| `pkg/agentcontext/worktree.go:13-23` | `agent_worktree_allocate` / `_release` / `_list` |
| `cmd/mcp-git-worktree/main.go:67-121` | lower-level `git_worktree_add` / `_remove` / `_prune` |
| `pkg/pathsec` | replaces hand-rolled `cleanRelativePath` (`patcher.go:1085`) |
| `pkg/validate` | replaces manual `args["x"].(string)` casts in `mcp_serve.go` |
| `pkg/mcperror`, `pkg/mcplog`, `pkg/mcpotel`, `pkg/lifecycle` | the standard MCP server envelope |
| `internal/loomconcurrency` | concurrency policy |

The `Config`-with-zero-means-default idiom (`matcher.go:50-55`, `patcher.go:79-85`) should be
replaced by loom-core's `validate.NewArgs` defaulting, which distinguishes **absent** from
**zero** — as written, a caller cannot request a threshold of `0`.

### 8.3 Smallest useful first slice

**Ship `patch_analyze` — read-only, no writes, no worktree.**

1. Port `pkg/types` (trimmed), `internal/parser`, `internal/matcher` into `pkg/patch/`. Pure
   functions, no I/O beyond reading files.
2. Port only `Patcher.AnalyzeWithOptions` (`patcher.go:681-742`) and
   `checkApplicabilityWithOptions` (`:744-920`) — the analyze path never writes a byte.
3. Expose one MCP tool `patch_analyze` taking `{patch, base_path, fuzzy_threshold,
   search_window, max_candidates, strict, details}` and returning per-hunk confidence,
   strategy, shift and candidates.
4. **Write the matcher unit tests that do not exist**: a table locking in the `1.0` /
   empty-`0.0` / `>0.5`-length-diff-`0.0` cliffs, plus a shift-tolerance table replacing the
   filler-line trick.

This slice is useful on its own — *"will this LLM-generated diff apply, and where, and how
confident are we?"* is a question loom-core cannot currently answer — and carries **zero
destructive risk**, since nothing in the analyze path opens a file for writing.

Only once its confidence numbers are trusted in practice should `patch_apply` follow, and it
should land **worktree-first via `agent_worktree_allocate`**, with **`strict` defaulting to
`true`** — inverting diff-surgeon's default, because an autonomous agent has no operator
standing at the `[Y]es/[n]o/[a]ll/[q]uit` prompt.

## 9. Provenance

- Harvest decision: `.loom/local/portfolio-uplift-2026-08/30-synthesis.md` §4.1 and §5 item 4,
  operator-approved 2026-08-15.
- Source repo archived after this document merged.

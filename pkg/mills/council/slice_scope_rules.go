package council

// slice_scope_rules.go is the AUTHOR-side half of Mills scope-gate reliability,
// symmetric with slice_grounding.go: grounding stops the editor from inventing
// directories that do not exist, this stops it from declaring a scope that is
// too NARROW for the work it just described.
//
// The failure it targets (2026-07-26, three scope escalations in one afternoon,
// 83% escalation rate against a 95.7% gate pass rate — .loom/plan-mills-scope-
// gate-reliability-2026-07-26.md): in every case the implement agent NEEDED the
// files it touched, and the item author (council, or a human) drew
// Slices[].files around the primary directory only, omitting the sibling shared
// components the change necessarily reached.
//
//   - token-sweep declared …/components/mills/*, and had to restyle the shared
//     shell those panels render inside (…/components/shared/PanelShell.svelte).
//   - stop-lever declared cmd/loom-mills-operator + pkg/mills/pipeline, and had
//     to touch pkg/mills/store/dao_pipeline.go and pkg/mills/clients/spawn.go
//     to persist and drive the pause.
//
// Each miss costs a full respawn (~$1.7–5) per retry and, because a needed file
// is still needed on the next attempt, the retry cannot converge — it escalates.
// Runtime amendment of an under-declared scope is handled elsewhere (S1, the
// gates package); this section reduces the BASE RATE at authoring time, which
// is the only fix that costs nothing at run time.
//
// Placement: the section is returned through EditorGuardrailsPromptSection, so
// it lands in the STABLE half of the editor prompt (the instruction preamble +
// guardrails prefix that buildCouncilEditorPromptParts caches ahead of the
// per-run brief). It never varies per item, so it must not sit in the volatile
// tail — that would cold-start the Anthropic backend's prefix cache every run.
//
// Precedent: MR !848 added the repo-layout grounding block the same way, and
// MR !850 added SanitizeProposalSlices as its output-side check. Note that
// nothing here auto-widens a proposal's files; that is deliberately NOT the
// sanitizer's job (see slice_grounding.go) — the model is asked to author the
// right scope, and the runtime amendment path handles what it still misses.

// SliceScopePromptSection returns the council editor's scope-authoring
// contract: declare every directory the slice's work touches, including the
// shared code it only reaches through. Constant text — no per-item variation —
// so it is safe in the cached stable prefix.
func SliceScopePromptSection() string {
	return `
## Slice scope — list every directory the work touches

A slice's "files" is an ENFORCED allowlist: the implement stage's diff is gated
against it, and one needed-but-undeclared file escalates the whole run. The
gate's unit is the DIRECTORY of each declared path, so an inexact basename is
survivable but a missing directory is not.

- List one path inside EVERY directory the work touches, including the shared
  code the slice only reaches through: the shell or layout it renders inside,
  the shared styles, the store it reads, the DAO, schema, or client it calls.
  Work is rarely confined to the directory it is named after.
- Prefer a directory you are sure about over a basename you are guessing — the
  basename of a file that does not exist yet is systematically wrong.
- A slice whose "files" are ALL new paths is INVALID: new code that nothing
  existing imports merges dead (the post-implement fabricated_slice gate
  escalates exactly this shape). A slice that creates a file must list, in the
  SAME slice, the existing file that will import or call it — the wiring edit
  is part of the slice, not a separate one.
- Example: a design-token sweep of
  ` + "`internal/hud/frontend/src/lib/components/mills/`" + ` also restyles the shared
  shell those panels render inside, so "files" must also carry
  ` + "`internal/hud/frontend/src/lib/components/shared/PanelShell.svelte`" + `.
- Do NOT pad the list with unrelated packages: a reach into another service or
  the repo root is a genuine detour and is rejected on purpose.
`
}

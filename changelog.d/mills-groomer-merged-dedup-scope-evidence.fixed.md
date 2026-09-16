Mills groomer: merged-dedup retires now require delivered-file evidence.

The escalated-duplicates-of-merged pass judged pairs on title similarity plus
a 240-char spec prefix — evidence that cannot tell "this work already merged
under the canonical" from "a sibling slice merged, this one never did".
Sibling slices of one plan family read near-identical by construction
(`…-s-1` vs `…-s-2`), so the pass would deterministically retire an escalated
slice whose work is demonstrably NOT on main. Live witness:
`…spawn-state-pruning-with-hud-pressure-s-2` (HUD pressure metrics in
`internal/hud/monitor/` + fleetview, still on an unmerged branch) scores as a
duplicate of `bl-hud-spawn-state-pressure-prune-20260726`, whose merge
(!1241) delivered only the prune mechanics under `internal/spawn/`. The
original fragment for that pass even cited this pair as its motivating
example; the claim was false and is corrected in place.

When the escalated candidate declares slice files, retiring now additionally
requires the merged canonical's delivered files to touch them. Delivered
evidence is the captured `files_changed` union across the canonical's done
runs — capture is authoritative for what a branch actually merged — with the
canonical's declared slices as fallback when nothing was captured. Disjoint
evidence (including an unverifiable canonical with neither capture nor
slices, and cross-repo pairs) vetoes the retire, deterministic and gray-band
alike, and records a once-per-item `dedup_scope_veto` audit event. The veto
is recomputed every tick, so a later merge that genuinely covers the
candidate still drains it. Sliceless candidates keep the previous
title-based behavior — file evidence can only block a retire, never mint one.

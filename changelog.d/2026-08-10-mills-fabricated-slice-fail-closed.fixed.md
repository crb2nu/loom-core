Mills now fails closed on fabricated plan slices — plans that declare file
paths absent from the repo, which implement runs previously satisfied by
inventing dead files (a 2026-08 sweep found 17 psl-plan-council merges that
landed 27 dead Go files on main this way). Three layers: the plan-slice
emitter grounds each slice's declared files against a revision-pinned
origin/main tree at emit time and stamps `Fabricated`/`MissingFiles`/
`GroundingRevision` on the slice (plus a `mills-fabricated-suspect` label); a
new `fabricated_slice` post-implement gate escalates any diff consisting only
of newly created files with at least one non-test Go source (terminally when
the emit-time stamp corroborates the fabrication, since a retry replays the
same plan); and the council/plan_slice prompts now require every
file-creating slice to include its wiring edit to an existing consumer.

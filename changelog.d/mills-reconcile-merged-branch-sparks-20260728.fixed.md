Mills reconciler: close escalated items whose branch merged without an MR IID.

The ghost-spark sweep only ever saw items whose most-recent run recorded an
`mr_iid` — `ListEscalatedWithMR` requires `pr.mr_iid > 0` in SQL. An item that
escalated *before* the mr stage (a scope or docs gate, a failed preflight)
therefore had no IID to look up, and since no later run happens for it the
"later run succeeds" auto-close never fired either. Its branch was routinely
pushed and merged by hand afterwards, leaving the item reading `escalated`
forever with its work already on main — and the council re-proposing it.

`SweepGhostSparks` gains a second pass over `ListEscalatedWithoutMR`,
resolving each item's deterministic branches and asking GitLab for a merged MR
on them. Three guards keep it from closing the wrong item: home project only
(these runs have no stage provenance to authorize a foreign-project lookup),
exact source-branch match re-checked client-side, and the merge must postdate
the escalated attempt so a stale merge from an earlier attempt cannot discard
a live escalation. It shares the existing per-tick GitLab lookup budget and
recheck cooldown, and caps branch lookups per item, so enabling it does not
increase the sweep's call volume.

The two passes are now independently enabled — previously the whole sweep
early-returned unless an MR-*state* client was wired, which would have left
the new pass dead. Closures are recorded with a distinct `merged_branch`
outcome in the event ledger and the `mills_ghost_sparks_closed_total` metric.

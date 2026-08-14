Mills groomer: retire escalated items whose duplicate already merged.

`Groomer.Tick` listed only `BacklogQueued`, so every hygiene pass — dedup,
zombie, priority — was structurally blind to the escalated lane. With 102 of
219 backlog items escalated (66 council-authored, in at least 6 near-identical
families) nothing could ever drain them, and the council kept re-proposing work
it had already shipped.

The new pass retires an escalated item only when a **merged** item is its
duplicate. That is the one case where "escalated implies a human is coming
back" is provably false: the change is on main under the canonical item, so
there is nothing left to come back for. An escalated item whose twin is merely
queued or escalated is left alone — retiring that would silently lose work
nobody has done yet.

Two deliberate differences from the queued dedup pass: the canonical is always
the merged item rather than the older one (shipping outranks age), and only
merged×escalated pairs are considered. It reuses the `dedup_close` action so it
inherits the existing allow flag, day cap, dry-run soak and once-per-subject
guard instead of adding parallel policy surface; the audit payload's `basis`
names the merged canonical.

Also removes the early return on an empty queued lane — `queue_depth` 0 is the
steady state between council rounds, which is exactly when the escalated pile
most needs draining. The tick still stays silent when both lanes are empty.

This covers the case the merged-branch reconciler pass deliberately could not:
work delivered under a *different* item's branch, e.g.
`…spawn-state-pruning-with-hud-pressure-s-2` shipping inside
`bl-hud-spawn-state-pressure-prune-20260726`.

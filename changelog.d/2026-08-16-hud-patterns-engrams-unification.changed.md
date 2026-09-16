Unify the Pattern Loom and engram tech-tree halves of `#mills/patterns` into one
surface. The two registers now share a badge vocabulary — `ProofBadge` covers
both an engram's `proof_status` and a pattern's `status`, replacing three
divergent encodings of the same semantic (a 16% tint chip in the engram strip,
a 3px left border on tree nodes, an 18% tint chip on pattern cards), and
`TierBadge` names each engram tier and states its proof contract instead of
rendering a bare integer.

The `pattern.engrams[]` edge is now navigable in both directions. A pattern's
composed engrams were rendered as inert monospace spans nine lines above a
working engram drawer; they are buttons that open that drawer. The drawer gains
a "composed into" section listing every pattern that composes the engram —
derived by inverting the loaded pattern list, since no API serves that
direction — and selecting one returns to the stamp form. The join tolerates the
`id` vs `engram://family/slug` mismatch between node ids and pattern references,
which a direct comparison silently drops.

Also: the tech tree's edge geometry now derives from shared layout constants and
sizes its canvas to the tallest tier, replacing hardcoded path math against a
frozen 420px viewBox that detached edges from nodes once a tier grew past a
handful; the tree's hand-rolled empty and error states use the shared
`EmptyState`; and four generic selectors (`.detail`, `.detail-label`, `.links`,
`.muted`) no longer leak out of `EngramTree` into the global stylesheet.

iOS companion: "Vendor transcripts" affordance on the Operator screen — a
collapsible recent-transcripts list plus substring search over the claude +
codex desktop CLI transcripts, riding the !1251 HUD routes
(`/api/vendor-sessions[/search]`) through the mobile pairing token. A HUD
whose agent bridge is offline renders as a "bridge offline" hint (the
`degraded:true` contract), and an older daemon whose allowlist predates the
routes degrades to an "update the daemon" note instead of an error. Also
repairs the Operator screen's three stale component calls from the deck
merge (SkeletonView rows, LoomEmptyState icon/message, relativeCompact from
Date) that broke the iOS app target build.

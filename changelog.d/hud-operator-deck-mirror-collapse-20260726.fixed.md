Operator Deck agent lane: federation-mirror placeholder rows (presences
carrying the `loom-core federation mirror` fallback description with no
current task — idle proxy bases and ended-chat hook presences alike) now
collapse into a single "N mirrored presences" summary row instead of
drowning the lane as ~15 flat rows. Real conversations — including any
placeholder twin sharing a conversation group with a working member — keep
their individual rows; identities are counted, never merged, so fleetview
reconcile semantics are untouched.

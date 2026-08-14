Refresh the HUD chrome: active nav and sub-view tabs collapse three stacked
signifiers (filled card + underline bar + drop shadow) into a single tinted
pill — accent orange for views, info cyan for sub-views; per-tab shortcut
chips now reveal on hover/focus/active instead of rendering ten at once;
scrollable tab strips fade at their clipped edges and auto-scroll the active
tab into view (hash/palette navigation could previously land the selection
off-screen with no visible active state). The body canvas swaps the two small
radial blobs for one wide horizon glow, surface radii soften (sm 5→6px,
md 8→10px), and table row hover uses a translucent overlay instead of a solid
fill. The Operator Deck's queue Dispatch buttons go quiet at rest (muted
ghost) and take the accent tint on row hover, button hover, or keyboard
focus — six identical orange blocks no longer out-shout actual alerts.
Also fixes the ≤800px bottom tab bar, which had never actually pinned
to the viewport: `backdrop-filter` on the header made it the containing block
for the fixed-position strip, so the thumb-zone nav rendered clipped inside
the header while the shell reserved 64px of dead space at the bottom.

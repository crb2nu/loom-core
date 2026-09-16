- HUD: fixed unreachable content on the Factory and Patterns pages. The app
  shell and ViewShell clip by design, making the routed panel the one scroll
  owner — FactoryPanel overrode the scrolling base class with
  `overflow: hidden` (clipping the instrument rail's tail, the departures
  board, and the pattern shelf, +307px at 720p), and PatternsPanel skipped
  the base class entirely (+930px unreachable once the engram tree rendered).
  Both now scroll; the scroll-ownership contract is documented on `.panel`
  in layout.css. Audited every mills route and top-level view down to a
  520px viewport — no other panel clips without a scroll path.

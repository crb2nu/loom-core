- **HUD navigation focus pass** (`internal/hud/frontend`): sub-view keyboard
  shortcuts now resolve on the **declared** `key` instead of the sub-view's
  array position, so every rendered `<kbd>` hint is the shortcut that actually
  fires. Mills was the visible casualty of the old index lookup — its
  out-of-order keys landed presses on the wrong panels, and **Warps**/**Bolts**
  claimed `r`/`o`, which the app-wide refresh and Overview handlers shadow,
  leaving both tabs keyboard-unreachable. Mills keys are re-cut to
  `w`/`s`/`p`/`b` for the mill-floor spine and `n` for Runs; a router test now
  asserts key uniqueness, reachability, and that no sub-view ever claims a
  reserved global. Alongside that:
  - **Projects folded into Work.** It read only `/api/plans` + `/api/sessions`
    — a lens on Plans, not an eighth top-level domain — and is now
    `Work ▸ Projects`. `#projects` still resolves.
  - **Mills sub-tabs grouped.** Sixteen flat tabs split into a *Mill floor*
    spine (Overview, Factory, Warps, Shuttles, Sparks, Bolts) and *Governance*,
    rendered as captioned sections in the existing tab bar.
  - **Command palette generated from the router.** It hardcoded 15 of 37
    nav-reachable panels under invented labels and still advertised the retired
    `pipelines`/`backlog` ids; entries are now derived from the view/sub-view
    table (embed-subset filtered), so it can no longer drift from the nav.
  - **Name collisions resolved.** The Mills overview sub-view is `mills-overview`
    (`#mills/overview` redirects) and its component is `MillsOverviewPanel`, so
    it no longer shares an id *and* a component name with the standalone "Now"
    view; the Mills tab labelled "Workflows" is now **Runs**, ending the clash
    with `Work ▸ Workflows`.
  - **Cards promoted to panels.** `CrossRepoCard`/`PolicyProposalsCard` are
    `CrossRepoPanel`/`PolicyPanel`; Policy's apply/reject moved off raw
    `confirm()` onto the shared `ConfirmDialog` + `runAdminAction` path every
    other Mills mutation uses.
  - **Retired sub-view ids no longer blank the pane.** `router.navigate()`
    resolves a legacy sub-view id (`backlog`→`warps`, `pipelines`→`shuttles`)
    the way `parseHash` does; `replaceState` fires no `hashchange`, so an
    unresolved id previously sat in `router.subView` with no registered panel.
- **HUD stops showing sample data as if it were live** (`internal/hud/frontend`):
  the Mills **Overview** wiring card and **Telemetry** panel fell back to
  committed fixtures behind a "sample data" badge when their endpoints 404'd.
  Both endpoints (`GET /api/mills/wiring`, `GET /api/mills/telemetry/stages`)
  are live, so a 404 now means the connected deployment predates the route —
  the panels say exactly that instead of painting a full dashboard of
  2026-07-16 aggregates. The fixtures are gone from the bundle (the telemetry
  one survives as a test-only captured response), and the store flags are
  renamed `wiringUnavailable` / `telemetryUnavailable` to match what they mean.

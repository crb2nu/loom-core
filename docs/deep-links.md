# Loom Companion deep links (`loom://`)

Canonical catalogue of the `loom://` URL scheme handled by the iOS companion
app. This is the doc referenced from
`apps/loom-companion-ios/Sources/LoomCompanionKit/Navigation/DeepLink.swift`.

Sources of truth:

- Grammar / parsing / building: `LoomCompanionKit/Navigation/DeepLink.swift`
  (`DeepLink.from(_:)` and `DeepLink.urlString`).
- Routing (which tab / surface each link lands on):
  `Sources/LoomCompanion/ContentView.swift` → `handleDeepLink(_:)`.
- Round-trip coverage: `Tests/LoomCompanionKitTests/Navigation/DeepLinkTests.swift`.

The scheme is registered in `project.yml` under `CFBundleURLTypes`
(`CFBundleURLSchemes: [loom]`).

## Invariants

- Scheme must be exactly `loom`. Anything else parses to `nil`.
- The **host** selects the route; path components carry ids, query items carry
  filters. Unknown hosts parse to `nil` (the link is dropped, not guessed at).
- Parse and build are inverses:
  `DeepLink.from(link.url!) == link` holds for every case.
- Ids are percent-encoded on build and trimmed on parse; blank ids are `nil`.
- Filter links omit absent query items entirely rather than sending empty values.

## Primary surfaces

| URL | Lands on |
|---|---|
| `loom://dashboard` | Dashboard tab |
| `loom://people` | Agents tab |
| `loom://work` | Work tab |
| `loom://mills` | Mills tab |
| `loom://alerts` | Dashboard tab, alert-inbox sheet (there is no Alerts tab — Spawn took that slot, so the Dashboard presents the inbox) |
| `loom://connection` | Settings sheet (Connection is no longer a tab — it lives behind the Dashboard's gear) |
| `loom://handoff`, `loom://handoffs` | Work tab, handoff inbox sheet |

## Single-object routes

| URL | Lands on |
|---|---|
| `loom://session/<id>` | Agents tab → Sessions → that session's detail |
| `loom://agent/<id>` | Agents tab → Roster → that agent (session detail when the agent has a live session, otherwise `AgentDetailView`) |
| `loom://spawn/<id>` | Spawn tab, that spawn |
| `loom://workflow/<id>` | Work tab, that workflow's detail sheet |
| `loom://workflow/<id>/approve` | Same, and approves the workflow's pending step on arrival |
| `loom://pipeline/<id>/escalate` | Mills tab, confirm sheet for the admin-gated pipeline escalate. Issued by the Mills widget's per-pipeline button — `escalate` is the only supported action (the operator's pause/resume are still 501 stubs) |
| `loom://alert/<id>` | Dashboard tab, alert-inbox sheet scrolled to and highlighting that alert. `<id>` is the HUD alert store's own id (`GET /api/mobile/v1/alerts`), the same id `POST /alerts/{id}/ack` addresses |

## Filtered list routes

| URL | Query items | Lands on |
|---|---|---|
| `loom://sessions` | `status`, `agent` | Agents tab → Sessions, filter preset |
| `loom://agents` | `status`, `type` | Agents tab → Roster, filter preset |
| `loom://tasks` | `status`, `agent`, `session` | Work tab → Queue, filter preset |

Example: `loom://tasks?status=blocked&agent=claude-code`

## Bootstrap route (trusted transport only)

`loom://configure?mode=…&url=…&bearer=…[&cf_id=…][&cf_secret=…][&admin=…]`

| Query item | Meaning |
|---|---|
| `mode` | `gateway` (default) or `lan` |
| `url` | HUD base URL, e.g. `https://hud.flexinfer.ai` |
| `bearer` | Mobile operator token |
| `cf_id` / `cf_secret` | Cloudflare Access service-token pair (gateway mode) |
| `admin` | HUD admin token (`HUD_ADMIN_TOKEN`), for Mills mutations. Omit for a read-only pairing |

`url` and `bearer` are required; without either, the link parses to `nil`.

**Security:** this link carries live credentials in the URL. It is accepted
**only** from process launch arguments — the USB-trusted channel used by
`make mobile-app-run-device` / `make mobile-app-run-sim`, which pass it via
`devicectl process launch` / `simctl launch`. `LoomCompanionApp.onOpenURL`
explicitly drops `.configure` so a link arriving via Mail/Messages/Safari
cannot hijack the session. Never paste a `loom://configure` URL into a
messaging app.

## Where links are produced

- **Widgets** — each widget sets a `widgetURL`, and `AttentionLaneWidget`
  builds `loom://<lane.route>` per lane. `MillsFactoryWidget` emits the
  per-pipeline `loom://pipeline/<id>/escalate`. `SessionSummaryWidget` taps
  emit `loom://agent/<id>` (falling back to `loom://people`).
- **Push notifications** — `AppDelegate` opens the payload's `deep_link` field.
- **Share sheets** — `LoomCopyLinkButton` / `LoomShareLink` build links from
  `DeepLink.urlString`; `DeepLink.shareTitle` supplies the label.
- **Dashboard attention lanes** — routed in-process via `NavigationCoordinator`
  rather than through the URL, but using the same route vocabulary.

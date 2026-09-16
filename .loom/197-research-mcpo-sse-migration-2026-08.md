# Research: migrate mcpo's `/sse` upstreams to Streamable HTTP

- **Date:** 2026-08-26
- **Parent:** [.loom/196-plan-mcp-2026-07-28-campaign-2026-08-25.md](196-plan-mcp-2026-07-28-campaign-2026-08-25.md)
- **Scope:** decision and rollout plan only. This document intentionally makes no
  Kubernetes or `custom-server` change.

## Decision summary

mcpo can consume Streamable HTTP upstreams, but the 22 loom-hub upstreams cannot
yet be switched: `cmd/custom-server` currently implements legacy MCP SSE at
`GET /sse` plus session-scoped `POST /messages`, and WebSocket at `/ws`; it has
no Streamable HTTP handler. Implement and prove a compatible endpoint first,
then migrate one mcpo entry at a time starting with `youtube`. Do not remove
`/sse` until every consumer has moved. Retiring mcpo is not currently a cheaper
safe path: it is the sole in-repo component exposing the fleet through the
`mcpo.flexinfer.ai` OpenAPI/docs ingress, while repository search identifies no
replacement OpenAPI gateway or named downstream consumer to migrate.

## 1. mcpo version and upstream capability

`k8s/base/servers/mcpo/deployment.yaml` deploys
`ghcr.io/open-webui/mcpo:main@sha256:f2c86ef…66bf95ae`. This is a digest-pinned
`main` build, not a semver release, so the manifest alone cannot name a deployed
mcpo version. Any implementation MR must resolve that digest to its image
revision before relying on behavior beyond the documented configuration.

mcpo is the Open WebUI MCP-to-OpenAPI proxy: its README describes generated
OpenAPI endpoints and `/docs` for the proxied MCP tools. Its upstream
[0.0.14 release](https://github.com/open-webui/mcpo/releases/tag/v0.0.14)
introduced Streamable HTTP *upstream* support, including config
`type: "streamable_http"` and a `url`; the
[current README](https://github.com/open-webui/mcpo#streamable-http-mcp-servers)
documents the normalized CLI spelling `streamable-http`. The later
[0.0.18 release](https://github.com/open-webui/mcpo/releases/tag/v0.0.18)
states that the configuration spelling was normalized while retaining backward
compatibility. Therefore the target config for the deployed image must be
validated in the canary, using the spelling accepted by that digest, before a
batch edit.

This is support for mcpo **connecting to** Streamable HTTP MCP servers; it does
not make mcpo's generated OpenAPI service an MCP Streamable HTTP server.

## 2. loom transport inventory

`cmd/custom-server/main.go` registers the following public transport routes:

| Transport | Route | Current role |
| --- | --- | --- |
| MCP legacy HTTP+SSE | `GET /sse` | Starts a child stdio MCP process and emits the session endpoint and responses. |
| MCP legacy HTTP+SSE | `POST /messages?session_id=…` | Sends a JSON-RPC message to that SSE session. |
| WebSocket | `/ws` (or `MCP_WS_PATH`) | Starts the same child process for a WS client. |
| Health | `/health`, `/ready` | Kubernetes liveness/readiness. |

There is no Streamable HTTP route (for example `/mcp`) in this binary today.
The follow-up implementation must add one and establish its exact path and
protocol/version behavior with tests. The intended mcpo upstream URL is then
`http://<service>.loom-hub.svc.cluster.local:8080/<new-path>`, not a blind
`/sse` URL substitution.

## 3. Complete current mcpo consumer inventory

The source of truth is `k8s/base/servers/mcpo/configmap.yaml`; all entries have
`type: "sse"` and a URL ending in `/sse`:

1. agent-context
2. alertmanager
3. cloudflare
4. codebase-memory
5. context7
6. flux
7. github
8. gitlab
9. grafana
10. helm
11. k8s-apps-k3s
12. loki
13. longhorn-k3s
14. morph-fast-apply
15. postgres
16. prometheus
17. qdrant
18. sequentialthinking
19. tavily
20. time
21. youtube
22. zep

## 4. Sequencing, probes, rollback, and downtime

1. **Transport prerequisite:** land a tested `custom-server` Streamable HTTP
   endpoint while retaining `/sse`; exercise `initialize`, `tools/list`, and a
   representative `tools/call` against a child stdio server. Record the exact
   route and accepted MCP protocol versions.
2. **mcpo compatibility canary:** in a non-production manifest or an isolated
   mcpo instance, configure only `youtube` as Streamable HTTP. Verify mcpo logs
   establish the upstream connection, `GET /docs` and `/openapi.json` remain
   healthy, the generated youtube operation is present, and an invocation of a
   read-only youtube tool returns successfully. Also verify the existing
   `/sse` connection remains usable until the config flip.
3. **Production canary:** change only the `youtube` config entry, reconcile,
   wait for rollout readiness, and rerun those probes through
   `https://mcpo.flexinfer.ai`. Observe errors/restarts for a normal traffic
   window.
4. **Rollback:** restore the exact prior `youtube` entry (`type: "sse"` and its
   `/sse` URL), reconcile, and re-run the OpenAPI and tool-call probes. Keep the
   legacy endpoint through the final batch so this rollback remains available.
5. **Batch:** migrate small, independently observable batches (for example
   3–5 entries), passing the same docs/OpenAPI/tool-call probes for every named
   server before the next batch. Treat stateful or high-use services as their
   own canaries rather than bundling them.

mcpo mounts the ConfigMap as a file. A ConfigMap update is not a safe assumption
of live process reload for this pinned image; plan a Deployment rollout/restart
per change and expect the single replica to create a short OpenAPI proxy outage.
Schedule each change, use readiness (`/docs` in the current Deployment) before
routing traffic, and do not combine it with unrelated mcpo upgrades. The
custom-server drain behavior helps existing SSE/WS sessions close cleanly, but
does not eliminate the mcpo restart interruption.

## 5. Retire-versus-migrate checkpoint

In repository evidence, mcpo's externally reachable surface is the Ingress
`mcpo.flexinfer.ai` forwarding `/` to port 8000, and the Deployment's readiness
probe explicitly uses `/docs`. No other tracked configuration names that host,
`/openapi.json`, or an alternate OpenAPI proxy; therefore there are no
repository-identifiable downstream OpenAPI consumers to migrate. That is not
evidence that external clients do not exist.

**Recommendation:** retain mcpo and perform the transport migration unless an
operator first captures external ingress/API usage and confirms the OpenAPI
surface can be retired. The migration is bounded and preserves an unknown
external contract; retirement without that evidence risks an unobservable
breaking change. Re-evaluate retirement after the `youtube` canary, using
ingress/API telemetry and an explicit owner decision. Only after all 22 entries
use Streamable HTTP (or mcpo is deliberately retired) may the blocked SSE-surface
removal item proceed.

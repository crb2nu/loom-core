Vendor transcript federation: full-snapshot pushes 413'd against the
mobile-hud ingress's default 1MB nginx body cap, so nothing federated on
first rollout. The sender now budgets each push (~640KB of entry payload;
over-budget tails defer with their cursors intact and ship on an immediate
follow-up cycle), and the mobile-hud ingress sets
`nginx.ingress.kubernetes.io/proxy-body-size: "8m"` to match the ingest
handler's own body cap.

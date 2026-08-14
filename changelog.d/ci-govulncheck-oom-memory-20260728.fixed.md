CI: documented the hard memory ceiling on `security:govulncheck` in place.
Its `KUBERNETES_MEMORY_REQUEST`/`_LIMIT` are unchanged (4Gi/8Gi) — an
attempt to raise them to 6Gi/12Gi was reverted because both runners cap
`memory_request_overwrite_max_allowed` at 4Gi and
`memory_limit_overwrite_max_allowed` at 8Gi, so the runner rejects an
over-cap job before the pod starts and it dies in ~15s. The existing values
were already at the ceiling; raising them for real requires a
`platform/gitops` change to the runner chart. The OOMKills that prompted
this were caused by a reachable advisory (GO-2026-6061) truncating the scan,
fixed separately by the grpc bump.

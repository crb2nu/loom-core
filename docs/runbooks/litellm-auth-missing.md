# LiteLLM Authentication Missing

Use this runbook for classifier pattern
`external_dependency.litellm.missing_api_key`. Record secret references and
provider identities only; never expose API-key values in logs or incident notes.

## Detection

The classifier requires a LiteLLM-scoped error containing `missing API key`,
`API key is missing`, or `no API key`. Preserve the UTC timestamp, Mills run and
stage, LiteLLM route/model alias, selected upstream provider, workload identity,
and sanitized error.

Safely confirm that the expected Secret name/key exists, the workload references
it, and the running process has the expected configuration source. Check
LiteLLM health and sanitized logs for the route decision. Do not print the
environment, Secret manifest, authorization header, or key length/value.

## Classification

Classify as an external LiteLLM incident when a previously working gateway
route loses its provider credential without a repository change, multiple
unrelated callers fail on the same route/provider, or the gateway/secret owner
confirms missing credential material.

Likely false positives are a branch-owned secret reference or environment-name
change, selecting a model/provider that was never configured, calling LiteLLM
without the required client credential, using the wrong gateway URL, or an
authorization/permission error where a key is present. Those are caller or
repository configuration defects rather than missing upstream auth.

## Operator Action

1. Stop requests on the affected route to avoid paid retries and noisy secret
   failures. Do not substitute an unapproved provider or personal key.
2. Escalate to the LiteLLM/secret owner with the route, provider, workload, and
   sanitized evidence. The owner restores or rotates the key through the
   approved secret-management path and reconciles the gateway workload.
3. Confirm only that the expected secret reference and version are mounted or
   injected. Review new logs for successful provider initialization without
   revealing credential material.
4. Send one minimal authenticated request through the same gateway route and
   model alias. Verify a valid response and confirm there is no missing-key
   event in LiteLLM logs or metrics.
5. Run one affected Mills stage and check provider usage/cost accounting before
   resuming normal automation.

If the route works only after bypassing LiteLLM or changing providers, the
original incident is not recovered; keep it escalated until the intended route
passes verification.

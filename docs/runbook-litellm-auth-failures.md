# LiteLLM Authentication Failures

Use this runbook when a LiteLLM-scoped error says `missing API key`, `API key
is missing`, or `no API key`. Preserve the UTC time, Mills run/stage, gateway
route and model alias, upstream provider, workload identity, and sanitized
error. Record secret references only: never print environment values, Secret
manifests, authorization headers, key lengths, or key material.

## Classification

This is an external dependency incident when a previously working route loses
provider credentials without a repository change, unrelated callers fail on
the same route/provider, or the LiteLLM/secret owner confirms credential drift.
A branch-owned secret reference or environment-name change, an unsupported
model/provider, wrong gateway URL, missing client credential, or a present-key
authorization failure is caller or repository configuration work.

## Fail closed and escalate

Stop requests to the affected route to avoid paid retries and secret leakage.
Do not retry autonomously, bypass LiteLLM, use a personal key, switch providers,
rotate credentials, or change gateway identity policy. Safely check that the
expected Secret name/key reference exists, the workload references it, and the
route decision appears in sanitized gateway logs.

Escalate route, provider, workload, and sanitized evidence to the LiteLLM and
secret-management owner. That owner restores or rotates credentials through the
approved path and reconciles the gateway. Record
`external_dependency_incident`, `disposition=wait_for_dependency_recovery`,
`retry_allowed=false`, and `external-dependency-incident`. If there is no
repository-owned action, record `external dependency incident; no actionable in-repo follow-up`.

## Verify recovery

Confirm only that the expected secret reference/version is injected and that
provider initialization succeeds without revealing credentials. Send one
minimal request through the same route and model alias, confirm a valid
response and no missing-key event, then run one affected Mills stage and check
usage/cost accounting. Recovery through a bypass or provider substitution does
not recover the intended route.

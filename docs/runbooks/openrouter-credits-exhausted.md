# OpenRouter Credits Exhausted

Use this runbook for classifier pattern
`external_dependency.openrouter.credits_exhausted`. Credit balances and
account URLs are operator-facing; never paste account tokens or billing
details into incident notes.

## Detection

The classifier requires OpenRouter's distinctive HTTP 402 wording — `requires
more credits` — reaching Mills through the litellm error chain (typically
`flexinfer chat: status 402 … OpenrouterException … "This request requires
more credits, or fewer max_tokens"`). Preserve the UTC timestamp, Mills run
and stage, the litellm model group (e.g. `or/kimi-k3`), and the sanitized
error. First observed live 2026-08-06: a balance dip 402'd the research stage
of three unrelated runs (#471, #476, #477) before classification existed;
each burned to attempt exhaustion as `code`.

Safely confirm the balance state on the OpenRouter dashboard and whether the
litellm route for the failing model group has a fallback group configured
(as of 2026-08-06, `or/kimi-k3` has none — a provider 402 becomes a terminal
stage error for it).

## Classification

Classify as an external dependency incident when the 402 phrase is present
and the same route worked recently without a repository change: credit
exhaustion is an account-state outage, not a code defect. Runs retried
against an empty balance fail identically at near-zero cost.

Likely false positives: a branch that newly introduces an OpenRouter-routed
model or inflates `max_tokens` beyond any plausible balance (a configuration
defect), or a 402 from a different provider whose wording merely mentions
credits in prose. The phrase match is deliberately narrow to OpenRouter's own
error copy.

## Operator Action

1. Stop manual requeues of affected items; the bounded auto-requeue releases
   them once the incident clears, and retrying against an empty balance only
   burns attempts.
2. Top up credits on the OpenRouter dashboard (operator decision — spend), or
   reroute the affected model group in the litellm config through the
   approved gitops path. If rerouting, note that a fallback group for the
   affected route is a model-policy change, not a hotfix.
3. Verify recovery with a single canary run (live 2026-08-06: the canary
   autopilot completed 46 minutes after credits landed, with no other
   change).
4. If the same route 402s again with a non-empty balance, the failure is
   max_tokens sizing or a provider-side limit — reclassify and investigate
   the request, not the balance.

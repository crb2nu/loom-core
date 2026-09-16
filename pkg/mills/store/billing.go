package store

import "strings"

// BillingClass names who pays for a stage attempt's cost. CostUSD is always
// the vendor list-price equivalent of the tokens a stage used; the class says
// whether that figure bills anything:
//
//   - api: a metered account is charged per token (Anthropic/OpenAI API keys,
//     LiteLLM → OpenRouter). This is the only class the daily USD caps count.
//   - subscription: a flat-rate vendor plan already paid for the month —
//     Claude Code under the cluster OAuth token, Codex under the ChatGPT
//     account. Budgeted separately (max_subscription_usd_per_day) so a
//     runaway harness still stops before the vendor's rate limits do.
//   - local: inference on our own hardware (flexinfer). Electricity, not a
//     bill.
//
// Before this attribution every dollar landed in one pool and the pipeline's
// $75/day metered cap was exhausted by subscription turns that bill nothing
// (2026-09-13: $69.89 "spent" by 14:00Z, ~$60 of it Codex/Claude Code
// harness time), stalling the factory on phantom cost.
type BillingClass string

const (
	BillingAPI          BillingClass = "api"
	BillingSubscription BillingClass = "subscription"
	BillingLocal        BillingClass = "local"
)

// BillingForBackend is the fallback attribution when a worker did not say who
// pays: spawn harnesses run under the cluster OAuth subscriptions, flexinfer
// is local hardware, every other attributed backend is a metered API. An
// unattributed row stays unclassified. Migration 039 applies the same rule to
// historical rows, so keep the two in step.
func BillingForBackend(backend string) BillingClass {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "spawn":
		return BillingSubscription
	case "flexinfer", "local":
		return BillingLocal
	case "":
		return ""
	}
	return BillingAPI
}

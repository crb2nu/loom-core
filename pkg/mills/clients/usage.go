package clients

import (
	"log/slog"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// Component labels for mills LLM traffic. All of it goes through one
// *FlexInferClient, so without these every caller's cache behaviour would be
// averaged into a single indistinguishable number. They are separated because
// they re-send different things: the judge re-sends a rubric, the weaver
// re-sends a research brief, a council editor re-sends a diff. Whether any of
// them has a reusable prefix is exactly the open question.
const (
	// ComponentJudge is the rubric judge (gates.RegisterLLMGates → RubricJudge).
	ComponentJudge = "mills-judge"
	// ComponentWeaver is the research stage (WeaverClient.Research).
	ComponentWeaver = "mills-weaver"
	// ComponentCouncilEditor is a council editing turn.
	ComponentCouncilEditor = "mills-council-editor"
	// ComponentCouncilReviewer is a council review turn.
	ComponentCouncilReviewer = "mills-council-reviewer"
	// ComponentEvalJudge is the contradiction/eval judge.
	ComponentEvalJudge = "mills-eval-judge"
	// ComponentMemory is item-memory consolidation
	// (pipeline.MemoryConsolidator). Defined in pkg/mills/pipeline because
	// that is where the call site lives and the import only points one way;
	// aliased here so this stays the complete roster of mills LLM labels.
	ComponentMemory = pipeline.ComponentMemory
	// ComponentFlexInfer is the fallback when no call site narrowed the label.
	// A rising share of traffic landing here means a new caller was added
	// without a WithComponent, not that anything is broken.
	ComponentFlexInfer = "mills-flexinfer"
)

// millsUsageObserver is the single observation point for mills chat traffic.
//
// Package-level rather than a field on FlexInferClient because the client is
// constructed in several places (three times in the operator alone, plus every
// test) and threading an observer through all of them would be churn for no
// gain: the destination is a package-level Prometheus registry either way, and
// the per-caller label that actually matters travels on the context.
var millsUsageObserver = llmusage.Observer{
	Logger:    slog.Default().With("component", "mills-llm-usage"),
	Component: ComponentFlexInfer,
	Sink:      millsUsageSink{},
}

// millsUsageSink forwards per-completion token accounting to the mills
// Prometheus counters. Counters only — no gauges — because these are
// cumulative quantities and a ratio of two counters is what a dashboard wants.
type millsUsageSink struct{}

// RecordUsage implements llmusage.Sink.
//
// Cardinality: component is a closed set of the constants above, and model is
// bounded by the configured fallback chains. The zero-guard on prompt tokens
// keeps a usage-less response from creating a label pair that only ever holds
// zero.
func (millsUsageSink) RecordUsage(component, model string, u llmusage.Usage) {
	if u.PromptTokens > 0 {
		mills.LLMPromptTokensTotal.WithLabelValues(component, model).Add(float64(u.PromptTokens))
	}
	if u.CachedPromptTokens > 0 {
		mills.LLMCachedPromptTokensTotal.WithLabelValues(component, model).Add(float64(u.CachedPromptTokens))
	}
	if u.CompletionTokens > 0 {
		mills.LLMCompletionTokensTotal.WithLabelValues(component, model).Add(float64(u.CompletionTokens))
	}
}

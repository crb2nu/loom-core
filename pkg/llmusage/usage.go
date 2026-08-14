// Package llmusage is the one place loom-core normalizes and reports the
// token accounting an OpenAI-compatible completion returns.
//
// It exists for a single question: does the serving engine's prefix cache
// actually pay off on loom-core's traffic shapes? `pkg/journalengine` is
// built on the premise that it does — measured at a 79.3% engine hit rate
// with warm repeats 99% cached on a psyche-simulation workload — but that
// number was measured on a different prompt shape, on a different lane, by a
// different caller. Before anything in this repo is restructured around the
// prefix-cache contract, the traffic we already send should be made to report
// what share of its prompt it is getting for free.
//
// So this package deliberately does nothing but observe. It performs no I/O,
// changes no request, and makes no decision. Every exported symbol is either a
// JSON shape for parsing a response's `usage` block or a way to emit what was
// parsed.
//
// # The two usage shapes
//
// The chat-completions API reports the cached share of a prompt as
// `usage.prompt_tokens_details.cached_tokens`; the Responses API reports the
// same quantity as `usage.input_tokens_details.cached_tokens`. An
// OpenAI-compatible proxy in front of a local engine may return either, and
// vLLM builds that predate the field return neither. [Details] parses both and
// [Usage.CachedTokens] coalesces them, so a caller never has to care which
// dialect its lane happens to speak.
//
// A zero cached count is genuinely ambiguous: it means either "nothing hit"
// or "this engine does not report the field". The two are distinguishable only
// out of band — `pkg/agentloop` resolves it by asking the proxy for the
// engine's own hit rate via the X-Flexinfer-Prefix-Cache-Hit-Rate header. Read
// a flat zero here as "unknown", not as "the cache is broken", and go look at
// the engine before concluding anything. See docs/JOURNAL_ENGINE.md, section
// "Reading the cache data".
//
// # Emitting
//
// An [Observer] is a value a client embeds. It logs one line per completion at
// debug level under the stable message [MessageUsage] and, when the component
// already exports Prometheus metrics, forwards the same numbers to a [Sink].
// Components with no metrics stack pass a nil Sink and get the log line only —
// this package must never be the reason client_golang enters a binary that
// does not already have it.
//
// The component label is the review axis: it is what distinguishes the mills
// judge from the mills weaver from a council editor, all of which share one
// HTTP client. A client sets its default in Observer.Component; a call site
// that knows better narrows it for the duration of a request with
// [WithComponent].
package llmusage

import (
	"context"
	"log/slog"
)

// MessageUsage is the log message every completion-usage line is emitted
// under. It is stable on purpose: it is the anchor for the log query in
// docs/JOURNAL_ENGINE.md, so a Loki filter for this literal finds usage lines
// from every instrumented client in the repo at once. Changing it breaks
// saved queries and dashboards.
const MessageUsage = "llm usage"

// Structured log field names. Exported so the doc, the tests, and any future
// log-based dashboard reference one definition rather than restating string
// literals that can drift apart.
const (
	// FieldComponent is the instrumented call site (e.g. "mills-judge").
	// Deliberately not "component": several clients already set that key on
	// their logger to name the client itself, and shadowing it would make
	// "which client" and "which caller" indistinguishable in the output.
	FieldComponent = "llm_component"
	// FieldModel is the model the engine reports having served, which is not
	// always the model that was requested — a fallback chain or a gateway
	// alias can substitute one. The served name is the useful one, because
	// the prefix cache lives with the engine that served it.
	FieldModel = "llm_model"
	// FieldPromptTokens is the whole prompt this turn, cached part included.
	FieldPromptTokens = "prompt_tokens"
	// FieldCachedTokens is the part of FieldPromptTokens the engine served
	// from its prefix cache. Absent when the engine does not report it.
	FieldCachedTokens = "cached_tokens"
	// FieldCachedShare is FieldCachedTokens/FieldPromptTokens rounded to four
	// places. Absent when either input is unknown. This is the number to
	// aggregate; the raw counts are there so the ratio can be recomputed
	// weighted by size rather than averaged per request.
	FieldCachedShare = "cached_share"
	// FieldCompletionTokens is the generated length, carried along because a
	// cached-share trend is uninterpretable without knowing whether the
	// completions also changed shape.
	FieldCompletionTokens = "completion_tokens"
)

// Details is the nested cached-token block of a usage report. It is the same
// shape under both `prompt_tokens_details` and `input_tokens_details`, so one
// type serves both.
type Details struct {
	CachedTokens int `json:"cached_tokens"`
}

// Usage is one completion's normalized token accounting.
//
// The zero value is the honest representation of "the response carried no
// usage block": every accessor reports unknown rather than zero, so a client
// that fails to parse usage cannot masquerade as a client that measured a 0%
// hit rate.
type Usage struct {
	// PromptTokens is the total prompt size the engine billed, including
	// whatever part of it was cached.
	PromptTokens int
	// CachedPromptTokens is the portion of PromptTokens served from the
	// engine's prefix cache. Zero means "none reported" — see the package
	// doc on why that is not the same as "none cached".
	CachedPromptTokens int
	// CompletionTokens is the generated length.
	CompletionTokens int
}

// CachedTokens coalesces the two dialects' cached-token fields, preferring
// whichever is populated. Both being zero yields zero.
func CachedTokens(promptDetails, inputDetails Details) int {
	if inputDetails.CachedTokens > 0 {
		return inputDetails.CachedTokens
	}
	return promptDetails.CachedTokens
}

// CachedShare returns the fraction of the prompt served from cache, or -1 when
// it cannot be computed because the prompt size is unknown.
//
// -1 rather than 0 because the difference matters: a run whose share is
// genuinely 0 is a cache that is not working and wants investigating, while a
// run whose share is unknown is an engine that does not report the field and
// wants a different measurement (the engine's own hit-rate metric). Collapsing
// them into 0 turns the second into a false alarm and, worse, would let an
// unreported field quietly drag a healthy aggregate down.
func (u Usage) CachedShare() float64 {
	if u.PromptTokens <= 0 {
		return -1
	}
	return float64(u.CachedPromptTokens) / float64(u.PromptTokens)
}

// Reported says whether the response carried a usable usage block at all.
func (u Usage) Reported() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0
}

// LogAttrs returns the canonical structured fields for one completion, as the
// alternating key/value slice slog's variadic API takes.
//
// Fields whose value is unknown are omitted rather than emitted as zero, for
// the reason described on [Usage.CachedShare]: an absent cached_tokens key is
// queryable as absent, whereas a zero is indistinguishable from a real miss.
func (u Usage) LogAttrs(component, model string) []any {
	attrs := make([]any, 0, 12)
	if component != "" {
		attrs = append(attrs, FieldComponent, component)
	}
	if model != "" {
		attrs = append(attrs, FieldModel, model)
	}
	attrs = append(attrs, FieldPromptTokens, u.PromptTokens, FieldCompletionTokens, u.CompletionTokens)
	if u.CachedPromptTokens > 0 {
		attrs = append(attrs, FieldCachedTokens, u.CachedPromptTokens)
	}
	if share := u.CachedShare(); share >= 0 && u.CachedPromptTokens > 0 {
		attrs = append(attrs, FieldCachedShare, roundShare(share))
	}
	return attrs
}

// roundShare trims the ratio to four decimal places so log lines stay
// readable and byte-comparable in tests without losing useful resolution.
func roundShare(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

// Sink receives the same numbers the log line carries, for components that
// already export Prometheus metrics. Implementations must be safe for
// concurrent use and must not block: they are called on the completion path.
type Sink interface {
	RecordUsage(component, model string, u Usage)
}

// Observer emits one completion's usage. The zero value is usable and silent,
// which is what makes it safe to embed in a client whose constructor callers
// have not been updated.
type Observer struct {
	// Logger receives the usage line at debug level. Nil disables logging
	// entirely rather than falling back to slog.Default(): a library that
	// starts writing to the default logger because a field was left unset is
	// a surprise, and several of the clients here run inside MCP servers that
	// route their own logs deliberately.
	Logger *slog.Logger
	// Component is the default label for this client's traffic. A call site
	// with better information overrides it per-request via WithComponent.
	Component string
	// Sink is the optional metrics destination. Nil for components with no
	// metrics stack.
	Sink Sink
}

// Observe records one completion. It is a no-op when the response carried no
// usage block, so a proxy that omits usage produces silence rather than a
// stream of zeroes that would poison any average computed over the output.
func (o Observer) Observe(ctx context.Context, model string, u Usage) {
	if !u.Reported() {
		return
	}
	component := ComponentFrom(ctx)
	if component == "" {
		component = o.Component
	}
	if o.Logger != nil {
		o.Logger.DebugContext(ctx, MessageUsage, u.LogAttrs(component, model)...)
	}
	if o.Sink != nil {
		o.Sink.RecordUsage(component, model, u)
	}
}

// componentKey types the context value so it cannot collide with another
// package's key.
type componentKey struct{}

// WithComponent narrows the component label for completions made under the
// returned context.
//
// This exists because the interesting granularity is not the HTTP client. In
// mills, one *clients.FlexInferClient serves the rubric judge, the weaver
// research stage, and the council editor; a single label for all three would
// average their cache behaviour together, and they have no reason to behave
// alike — the judge re-sends a rubric, the weaver re-sends a research brief.
// The call site knows which it is; the client does not.
func WithComponent(ctx context.Context, component string) context.Context {
	if component == "" {
		return ctx
	}
	return context.WithValue(ctx, componentKey{}, component)
}

// ComponentFrom returns the component label set by [WithComponent], or "".
func ComponentFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	component, _ := ctx.Value(componentKey{}).(string)
	return component
}

package weaver

import (
	"context"
	"encoding/json"

	"github.com/crb2nu/loom/pkg/openairesponses"
)

// SubagentTelemetry implements openairesponses.TelemetrySink for per-subagent
// token tracking that feeds into weaver Metrics.
type SubagentTelemetry struct {
	domain  string
	metrics *Metrics
}

// NewSubagentTelemetry creates a telemetry sink scoped to a domain.
func NewSubagentTelemetry(domain string, metrics *Metrics) *SubagentTelemetry {
	return &SubagentTelemetry{domain: domain, metrics: metrics}
}

func (t *SubagentTelemetry) RecordTurnStart(_ context.Context, _ openairesponses.TurnRequest, _ openairesponses.ExecutionIdentity) {
}

func (t *SubagentTelemetry) RecordTurnEnd(_ context.Context, resp openairesponses.TurnResponse, err error, _ openairesponses.ExecutionIdentity) {
	if t.metrics == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	t.metrics.SubagentCallTotal.WithLabelValues(t.domain, status).Inc()

	if resp.PromptTokens > 0 {
		t.metrics.TokensTotal.WithLabelValues(t.domain, "prompt").Add(float64(resp.PromptTokens))
	}
	if resp.CompletionTokens > 0 {
		t.metrics.TokensTotal.WithLabelValues(t.domain, "completion").Add(float64(resp.CompletionTokens))
	}
}

func (t *SubagentTelemetry) RecordToolCall(_ context.Context, _ openairesponses.ToolCall, result openairesponses.ToolResult, err error, _ openairesponses.ExecutionIdentity) {
	if t.metrics == nil {
		return
	}
	if err != nil {
		t.metrics.ErrorsTotal.WithLabelValues(t.domain).Inc()
		return
	}
	// Count the raw tool-response size weaver consumed (pre-compression).
	// This feeds the F8 economics card's compression / token-savings /
	// context-waste ratios against the compressed answer recorded per query.
	if text := toolResultText(result); text != "" {
		t.metrics.RecordRawToolTokens(estimateTokens(text))
	}
}

// toolResultText extracts the textual payload of a tool result. The weaver
// executor sets Output to a string, but the contract type is `any`, so we
// defensively handle the common alternatives.
func toolResultText(r openairesponses.ToolResult) string {
	switch v := r.Output.(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case json.RawMessage:
		return string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

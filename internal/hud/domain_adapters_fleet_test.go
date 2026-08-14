package hud

import (
	"encoding/json"
	"testing"
)

// TestWeaverMetricsViewFromJSON verifies the F8 token-economics adapter
// decodes a real loom/weaver/metrics summary into a reachable view. This
// guards the wiring that replaced the long-standing hard stub
// (WeaverMetrics returning (zero,false)) which left the "Local util."
// ratio permanently showing "weaver_metrics_unreachable".
func TestWeaverMetricsViewFromJSON(t *testing.T) {
	// Matches pkg/weaver.Metrics.Summary() exactly: total_queries +
	// total_tokens are the fields the view consumes; the rest are ignored.
	raw := json.RawMessage(`{
		"total_queries": 42,
		"avg_latency_ms": 1234.5,
		"error_rate": 0.1,
		"total_tokens": 98765,
		"error_count": 4,
		"raw_tool_response_tokens": 50000,
		"weaver_response_tokens": 2500
	}`)

	view, reachable := weaverMetricsViewFromJSON(raw)
	if !reachable {
		t.Fatal("reachable = false, want true for a well-formed summary")
	}
	if view.TotalQueries != 42 {
		t.Errorf("TotalQueries = %d, want 42", view.TotalQueries)
	}
	if view.TotalTokens != 98765 {
		t.Errorf("TotalTokens = %d, want 98765", view.TotalTokens)
	}
	if view.RawToolTokens != 50000 {
		t.Errorf("RawToolTokens = %d, want 50000", view.RawToolTokens)
	}
	if view.ResponseTokens != 2500 {
		t.Errorf("ResponseTokens = %d, want 2500", view.ResponseTokens)
	}
}

// A well-formed but empty summary (weaver enabled but quiet) must stay
// reachable so the ratio renders "insufficient_data", not "unreachable".
func TestWeaverMetricsViewFromJSON_QuietIsReachable(t *testing.T) {
	view, reachable := weaverMetricsViewFromJSON(json.RawMessage(
		`{"total_queries":0,"avg_latency_ms":0,"error_rate":0,"total_tokens":0,"error_count":0}`))
	if !reachable {
		t.Fatal("reachable = false for zero counters, want true (on-but-quiet)")
	}
	if view.TotalQueries != 0 || view.TotalTokens != 0 {
		t.Errorf("view = %+v, want zero counters", view)
	}
}

// Malformed JSON is the only unreachable case at the parse layer.
func TestWeaverMetricsViewFromJSON_Malformed(t *testing.T) {
	if _, reachable := weaverMetricsViewFromJSON(json.RawMessage(`{not json`)); reachable {
		t.Error("reachable = true for malformed JSON, want false")
	}
}

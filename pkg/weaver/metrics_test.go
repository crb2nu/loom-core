package weaver

import "testing"

func TestMetrics_Summary(t *testing.T) {
	m := NewMetrics(nil) // nil registerer for testing
	m.RecordQuery("ok", 100, 50)
	m.RecordQuery("ok", 200, 75)
	m.RecordQuery("error", 300, 0)

	s := m.Summary()
	if s["total_queries"].(int64) != 3 {
		t.Errorf("expected 3 total queries, got %v", s["total_queries"])
	}
	if s["error_count"].(int64) != 1 {
		t.Errorf("expected 1 error, got %v", s["error_count"])
	}
	if s["total_tokens"].(int64) != 125 {
		t.Errorf("expected 125 total tokens, got %v", s["total_tokens"])
	}
	avgLatency := s["avg_latency_ms"].(float64)
	if avgLatency != 200.0 {
		t.Errorf("expected avg latency 200.0, got %v", avgLatency)
	}
	errorRate := s["error_rate"].(float64)
	expectedRate := 1.0 / 3.0
	if errorRate < expectedRate-0.001 || errorRate > expectedRate+0.001 {
		t.Errorf("expected error rate ~%v, got %v", expectedRate, errorRate)
	}
}

func TestMetrics_Summary_ToolAndResponseTokens(t *testing.T) {
	m := NewMetrics(nil)
	m.RecordRawToolTokens(1000)
	m.RecordRawToolTokens(500)
	m.RecordResponseTokens(120)

	s := m.Summary()
	if got := s["raw_tool_response_tokens"].(int64); got != 1500 {
		t.Errorf("raw_tool_response_tokens = %d, want 1500", got)
	}
	if got := s["weaver_response_tokens"].(int64); got != 120 {
		t.Errorf("weaver_response_tokens = %d, want 120", got)
	}
}

// Record methods must be nil-safe (Router.metrics may be unset) and ignore
// non-positive counts.
func TestMetrics_RecordTokens_NilAndZeroSafe(t *testing.T) {
	var m *Metrics
	m.RecordRawToolTokens(10)  // nil receiver: must not panic
	m.RecordResponseTokens(10) // nil receiver: must not panic

	live := NewMetrics(nil)
	live.RecordRawToolTokens(0)
	live.RecordRawToolTokens(-5)
	live.RecordResponseTokens(0)
	s := live.Summary()
	if got := s["raw_tool_response_tokens"].(int64); got != 0 {
		t.Errorf("raw_tool_response_tokens = %d, want 0 (non-positive ignored)", got)
	}
	if got := s["weaver_response_tokens"].(int64); got != 0 {
		t.Errorf("weaver_response_tokens = %d, want 0", got)
	}
}

func TestMetrics_Summary_Empty(t *testing.T) {
	m := NewMetrics(nil)
	s := m.Summary()

	if s["total_queries"].(int64) != 0 {
		t.Errorf("expected 0 total queries, got %v", s["total_queries"])
	}
	if s["avg_latency_ms"].(float64) != 0 {
		t.Errorf("expected 0 avg latency, got %v", s["avg_latency_ms"])
	}
	if s["error_rate"].(float64) != 0 {
		t.Errorf("expected 0 error rate, got %v", s["error_rate"])
	}
}

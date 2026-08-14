package worker

import (
	"testing"
)

func TestValidateAgentType(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		// Canonical tokens.
		{"claude-code", AgentTypeClaudeCode, false},
		{"codex", AgentTypeCodex, false},
		{"gemini", AgentTypeGemini, false},
		// Known shorthands normalise to canonical.
		{"claude", AgentTypeClaudeCode, false},
		{"claude-sonnet", AgentTypeClaudeCode, false},
		{"claude-opus", AgentTypeClaudeCode, false},
		{"openai-codex", AgentTypeCodex, false},
		// Case + whitespace tolerance.
		{"CLAUDE-CODE", AgentTypeClaudeCode, false},
		{"  codex  ", AgentTypeCodex, false},
		// Rejections.
		{"", "", true},
		{"   ", "", true},
		{"gpt-5.5", "", true},
		{"llama-3", "", true},
		{"anthropic", "", true},
	}
	for _, c := range cases {
		got, err := ValidateAgentType(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ValidateAgentType(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateAgentType(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidateAgentType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCapabilitiesFor(t *testing.T) {
	cases := []struct {
		agentType string
		want      Capabilities
		ok        bool
	}{
		{AgentTypeClaudeCode, Capabilities{SupportsRealCost: true, SupportsMultiTurn: true, SupportsStreaming: true}, true},
		{AgentTypeCodex, Capabilities{SupportsRealCost: false, SupportsMultiTurn: false, SupportsStreaming: true}, true},
		{AgentTypeGemini, Capabilities{SupportsRealCost: false, SupportsMultiTurn: false, SupportsStreaming: true}, true},
		// Shorthand resolves to the same capabilities.
		{"claude", Capabilities{SupportsRealCost: true, SupportsMultiTurn: true, SupportsStreaming: true}, true},
		// Unknown.
		{"gpt-5.5", Capabilities{}, false},
		{"", Capabilities{}, false},
	}
	for _, c := range cases {
		got, ok := CapabilitiesFor(c.agentType)
		if ok != c.ok {
			t.Errorf("CapabilitiesFor(%q) ok = %v, want %v", c.agentType, ok, c.ok)
			continue
		}
		if got != c.want {
			t.Errorf("CapabilitiesFor(%q) = %+v, want %+v", c.agentType, got, c.want)
		}
	}
}

func TestCostSourceString(t *testing.T) {
	cases := map[CostSource]string{
		CostSourceReal:        "real",
		CostSourceEstimated:   "estimated",
		CostSourceUnavailable: "unavailable",
		CostSourceUnknown:     "unknown",
		CostSource(999):       "unknown",
	}
	for cs, want := range cases {
		if got := cs.String(); got != want {
			t.Errorf("CostSource(%d).String() = %q, want %q", cs, got, want)
		}
	}
}

func TestCostSourceFromTelemetry(t *testing.T) {
	cases := []struct {
		name string
		tel  *TelemetrySnapshot
		want CostSource
	}{
		{"nil telemetry", nil, CostSourceUnavailable},
		{"claude real", &TelemetrySnapshot{TotalCostUSD: 1.23, CostEstimated: false}, CostSourceReal},
		{"codex estimated", &TelemetrySnapshot{TotalCostUSD: 0.42, CostEstimated: true}, CostSourceEstimated},
		{"gemini unavailable", &TelemetrySnapshot{TotalCostUSD: 0, CostEstimated: false}, CostSourceUnavailable},
		// Estimated marker wins even at zero cost (defensive).
		{"estimated zero", &TelemetrySnapshot{TotalCostUSD: 0, CostEstimated: true}, CostSourceEstimated},
	}
	for _, c := range cases {
		if got := costSourceFromTelemetry(c.tel); got != c.want {
			t.Errorf("%s: costSourceFromTelemetry = %v, want %v", c.name, got, c.want)
		}
	}
}

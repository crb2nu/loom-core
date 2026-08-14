package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// TestHUDSpawnTelemetry_TokenUsageWireContract pins the operator's decode to
// the HUD wire field names without introducing an internal-package dependency
// from pkg/. Producer-side contract tests separately pin the emitted JSON.
func TestHUDSpawnTelemetry_TokenUsageWireContract(t *testing.T) {
	produced := map[string]any{
		"turn_count":     7,
		"total_cost_usd": 1.25,
		"token_usage": map[string]int{
			"input_tokens":          4096,
			"output_tokens":         512,
			"cache_creation_tokens": 2048,
			"cache_read_tokens":     16384,
		},
	}
	raw, err := json.Marshal(produced)
	if err != nil {
		t.Fatalf("marshal producer telemetry: %v", err)
	}

	var consumed hudSpawnTelemetry
	if err := json.Unmarshal(raw, &consumed); err != nil {
		t.Fatalf("decode into operator subset: %v", err)
	}
	if consumed.TokenUsage == nil {
		t.Fatalf("token_usage decoded as absent from %s", raw)
	}
	got := *consumed.TokenUsage
	want := hudSpawnTokenUsage{
		InputTokens:         4096,
		OutputTokens:        512,
		CacheCreationTokens: 2048,
		CacheReadTokens:     16384,
	}
	if got != want {
		t.Errorf("token usage = %+v, want %+v (producer JSON: %s)", got, want, raw)
	}
}

// TestHUDSpawnTelemetry_TokenUsageAbsent covers the older-HUD / no-usage case.
// The pointer must stay nil so downstream can tell "nothing reported" from
// "reported all zeros"; a value type would collapse the two.
func TestHUDSpawnTelemetry_TokenUsageAbsent(t *testing.T) {
	const raw = `{"turn_count":3,"total_cost_usd":0.4,"stop_reason":"end_turn"}`
	var tel hudSpawnTelemetry
	if err := json.Unmarshal([]byte(raw), &tel); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tel.TokenUsage != nil {
		t.Errorf("token_usage = %+v, want nil when the field is absent", tel.TokenUsage)
	}
	// The pre-existing fields must be unaffected by the additive change.
	if tel.TurnCount != 3 || tel.TotalCostUSD != 0.4 || tel.StopReason != "end_turn" {
		t.Errorf("existing fields regressed: %+v", tel)
	}
}

func TestMapTelemetryToResponse_CarriesTokenUsage(t *testing.T) {
	resp := mapTelemetryToResponse(&hudSpawnState{
		SpawnID: "spawn-tokens",
		AgentID: "agent-1",
		Status:  "completed",
		Telemetry: &hudSpawnTelemetry{
			TurnCount:    4,
			TotalCostUSD: 0.9,
			TokenUsage: &hudSpawnTokenUsage{
				InputTokens:         1000,
				OutputTokens:        200,
				CacheCreationTokens: 300,
				CacheReadTokens:     4000,
			},
		},
	})
	want := pipeline.SpawnTokenUsage{
		InputTokens:         1000,
		OutputTokens:        200,
		CacheCreationTokens: 300,
		CacheReadTokens:     4000,
	}
	if resp.TokenUsage != want {
		t.Errorf("TokenUsage = %+v, want %+v", resp.TokenUsage, want)
	}
	if !resp.TokenUsage.Reported() {
		t.Error("Reported() = false for a fully populated usage block")
	}
	// Cost accounting must be untouched by the additive field.
	if resp.CostUSD != 0.9 {
		t.Errorf("CostUSD = %v, want 0.9", resp.CostUSD)
	}
}

func TestMapTelemetryToResponse_NoTokenUsageStaysUnreported(t *testing.T) {
	resp := mapTelemetryToResponse(&hudSpawnState{
		SpawnID:   "spawn-quiet",
		Status:    "completed",
		Telemetry: &hudSpawnTelemetry{TurnCount: 1, TotalCostUSD: 0.1},
	})
	if resp.TokenUsage.Reported() {
		t.Errorf("TokenUsage = %+v, want unreported when the HUD sent none", resp.TokenUsage)
	}
}

// TestRun_SurfacesTokenUsageFromHUD exercises the whole operator path — HTTP
// envelope, state decode, telemetry mapping — against bytes shaped like the
// mobile spawn-detail response, so the test fails if the `data` envelope or
// the nesting under `telemetry` changes rather than only the leaf names.
func TestRun_SurfacesTokenUsageFromHUD(t *testing.T) {
	const stateJSON = `{
	  "ok": true,
	  "data": {
	    "spawn_id": "spawn-e2e",
	    "agent_id": "claude-1",
	    "status": "completed",
	    "telemetry": {
	      "turn_count": 12,
	      "total_cost_usd": 2.5,
	      "token_usage": {
	        "input_tokens": 8192,
	        "output_tokens": 1024,
	        "cache_creation_tokens": 4096,
	        "cache_read_tokens": 65536
	      },
	      "stop_reason": "end_turn"
	    }
	  }
	}`
	ft := &hudFakeTransport{
		post: func(*http.Request) (int, any) {
			return 202, hudSpawnAcceptResponse{SpawnID: "spawn-e2e"}
		},
		get: func(*http.Request) (int, any) {
			return 200, json.RawMessage(compactJSON(t, stateJSON))
		},
	}
	c := newHUDStub(t, ft)
	resp, err := c.Run(context.Background(), sampleSpawnReq())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := pipeline.SpawnTokenUsage{
		InputTokens:         8192,
		OutputTokens:        1024,
		CacheCreationTokens: 4096,
		CacheReadTokens:     65536,
	}
	if resp.TokenUsage != want {
		t.Errorf("TokenUsage = %+v, want %+v", resp.TokenUsage, want)
	}
}

func compactJSON(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("compact fixture: %v", err)
	}
	return buf.Bytes()
}

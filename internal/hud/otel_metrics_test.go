package hud

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/crb2nu/loom/internal/spawn"
)

func TestNewHUDMetrics_SpawnTelemetryCounters(t *testing.T) {
	m := NewHUDMetrics()
	if m == nil {
		t.Fatal("NewHUDMetrics returned nil")
	}

	if m.SpawnAuthFallbackTotal == nil {
		t.Error("SpawnAuthFallbackTotal is nil")
	}
	// Verify all spawn telemetry counters are initialized (non-nil).
	if m.SpawnTokensTotal == nil {
		t.Error("SpawnTokensTotal is nil")
	}
	if m.SpawnCostTotal == nil {
		t.Error("SpawnCostTotal is nil")
	}
	if m.SpawnTurnsTotal == nil {
		t.Error("SpawnTurnsTotal is nil")
	}
	if m.SpawnToolCallsTotal == nil {
		t.Error("SpawnToolCallsTotal is nil")
	}
	if m.SpawnFileChangesTotal == nil {
		t.Error("SpawnFileChangesTotal is nil")
	}
	if m.SpawnErrorsTotal == nil {
		t.Error("SpawnErrorsTotal is nil")
	}

	// Verify existing counters are still initialized.
	if m.AgentSpawnTotal == nil {
		t.Error("AgentSpawnTotal is nil")
	}
	if m.PushNotificationTotal == nil {
		t.Error("PushNotificationTotal is nil")
	}
	if m.SpawnedAgentActive == nil {
		t.Error("SpawnedAgentActive is nil")
	}
	if m.PushDeliveryLatency == nil {
		t.Error("PushDeliveryLatency is nil")
	}
}

func TestClaudeAuthFallbackMetricLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	old := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	defer otel.SetMeterProvider(old)
	defer func() { _ = provider.Shutdown(context.Background()) }()
	t.Setenv("SPAWN_CLAUDE_AUTH_FALLBACK", "oauth-then-api")
	t.Setenv(ClaudeOAuthTokenKeysEnv, "a,b")
	o := newStopRaceOrchestrator(t, &recordingBackend{})
	o.metrics = NewHUDMetrics()
	id, _ := seedSupervisedRunningSpawn(t, o.ctrl, true)
	st, _ := o.ctrl.Get(id)
	st.AuthMode, st.AuthAccount = spawn.AuthModeClusterOAuth, "a"
	o.ctrl.UpdateState(t.Context(), st)
	calls := make(chan bool, 2)
	o.redriveSpawn = func(string, SpawnRequest) { calls <- true }
	for attempt := 0; attempt < 2; attempt++ {
		if !o.handleClaudeAuthCompletion(t.Context(), st, &claudeParseResult{sawResult: true, isError: true, result: "Please run /login"}, 0, nil) {
			t.Fatal("missing retry")
		}
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("missing launch")
		}
		var data metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &data); err != nil {
			t.Fatal(err)
		}
		var count int64
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != "hud_spawn_auth_fallback_total" {
					continue
				}
				for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
					count += point.Value
					for key, want := range map[string]string{"from": "cluster_oauth", "to": "cluster_api_key", "reason": "oauth_rejected"} {
						value, ok := point.Attributes.Value(attribute.Key(key))
						if !ok || value.AsString() != want {
							t.Fatalf("attribute %s=%v", key, value)
						}
					}
				}
			}
		}
		if count != int64(attempt) {
			t.Fatalf("attempt=%d count=%d", attempt, count)
		}
		st, _ = o.ctrl.Get(id)
		st.AuthRetryPending = false
		o.ctrl.UpdateState(t.Context(), st)
	}
}

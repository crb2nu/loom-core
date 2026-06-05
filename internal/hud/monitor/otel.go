package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// OTelMonitor polls the daemon's otel-status RPC and maintains a cached
// snapshot. It mirrors CostMonitor: a thin RefreshFunc over a bridge.Caller
// whose OnRefresh callback (wired in embed.go) broadcasts the snapshot to
// browsers as the `hud.otel` SSE event, so the frontend otelStore no longer
// has to blind-poll /api/otel.
type OTelMonitor struct {
	BaseMonitor[bridge.OTelStatusResult]
	client bridge.Caller
}

// NewOTelMonitor creates an OTelMonitor backed by the given daemon caller.
func NewOTelMonitor(client bridge.Caller, logger *slog.Logger) *OTelMonitor {
	m := &OTelMonitor{client: client}
	m.InitBase(logger, nil, "otel-monitor")
	return m
}

// Start begins the background polling goroutine at the given interval.
func (m *OTelMonitor) Start(interval time.Duration) {
	m.BaseMonitor.Start(interval, m.refresh)
}

// refresh fetches the latest OTel status from the daemon via bridge.
func (m *OTelMonitor) refresh(_ context.Context) (bridge.OTelStatusResult, error) {
	raw, err := m.client.Call("loom/otel-status", nil)
	if err != nil {
		return bridge.OTelStatusResult{}, err
	}
	var result bridge.OTelStatusResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return bridge.OTelStatusResult{}, fmt.Errorf("unmarshal otel-status: %w", err)
	}
	return result, nil
}

// Refresh forces an immediate refresh. Exposed for external callers (e.g. the
// embedded HUD's post-startup refresh).
func (m *OTelMonitor) Refresh() error {
	snap, err := m.refresh(context.Background())
	if err != nil {
		return err
	}
	m.Update(snap)
	return nil
}

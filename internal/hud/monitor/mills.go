package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// millsStatusBodyLimit caps the operator status payload we read so a
// misbehaving upstream can't balloon memory. The status object is small
// (KPIs + health + autonomy flags), well under this bound.
const millsStatusBodyLimit = 1 << 20 // 1 MiB

// MillsSnapshot is the loom-mills-operator status object (the same payload the
// HUD proxies at GET /api/mills/status), broadcast to browsers as `hud.mills`.
// It is kept as a free-form map: the operator owns the schema and the frontend
// uses the event purely as a refresh signal, so the monitor does not need a
// typed mirror that would drift from the operator.
type MillsSnapshot map[string]any

// MillsMonitor polls the loom-mills-operator status endpoint directly (the
// status route is an open read — the proxy only injects the admin bearer for
// mutations) and caches the result. Its OnRefresh callback (wired in embed.go)
// broadcasts a `hud.mills` SSE tick so the frontend millsStore can refresh on
// push instead of blind-polling every 15s.
//
// The monitor is only constructed when the operator URL is configured; on
// developer laptops (URL unset) it is never started and no `hud.mills` events
// are emitted.
type MillsMonitor struct {
	BaseMonitor[MillsSnapshot]
	statusURL string
	client    *http.Client
}

// NewMillsMonitor creates a MillsMonitor for the given operator base URL
// (e.g. http://loom-mills-operator.loom-mills.svc.cluster.local:8090, with no
// trailing /api/mills). Callers must ensure operatorBaseURL is non-empty.
func NewMillsMonitor(operatorBaseURL string, logger *slog.Logger) *MillsMonitor {
	m := &MillsMonitor{
		statusURL: strings.TrimRight(operatorBaseURL, "/") + "/api/mills/status",
		client:    &http.Client{Timeout: 10 * time.Second},
	}
	m.InitBase(logger, nil, "mills-monitor")
	return m
}

// Start begins the background polling goroutine at the given interval.
func (m *MillsMonitor) Start(interval time.Duration) {
	m.BaseMonitor.Start(interval, m.refresh)
}

// refresh GETs the operator status endpoint and decodes it into a snapshot.
func (m *MillsMonitor) refresh(ctx context.Context) (MillsSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.statusURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "loom-hud/mills-monitor")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, millsStatusBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read mills status: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mills status: HTTP %d", resp.StatusCode)
	}
	var snap MillsSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal mills status: %w", err)
	}
	return snap, nil
}

// Refresh forces an immediate refresh. Exposed for external callers (e.g. the
// embedded HUD's post-startup refresh).
func (m *MillsMonitor) Refresh() error {
	snap, err := m.refresh(context.Background())
	if err != nil {
		return err
	}
	m.Update(snap)
	return nil
}

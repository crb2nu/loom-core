package main

import (
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// telemetryStagesResponse is the wire shape of GET /api/mills/telemetry/stages.
// It wraps the store's aggregation with the window echo + generation time; the
// embedded *store.StageTelemetry promotes runs/stages/gates/escalation_funnel/
// failure_classes to the top level so the JSON matches the panel contract.
type telemetryStagesResponse struct {
	WindowSeconds int       `json:"window_seconds"`
	GeneratedAt   time.Time `json:"generated_at"`
	*store.StageTelemetry
}

// handleTelemetryStages serves the stage/gate/run roll-up for a rolling window.
// The window parsing mirrors handleKPIs (1d/7d/30d, empty == 1d); anything else
// is a 400 so a caller can't cardinality-bomb the aggregation with arbitrary
// spans. The aggregation is computed live from the canonical store (not a
// pre-rolled snapshot) because the panel wants sub-tick freshness; a short-TTL
// per-window memo (telemetryCache) collapses a burst of HUD polls into one
// aggregation while keeping that freshness. The cached generated_at is served
// verbatim so the panel's freshness read reflects the data, not the cache hit.
func (o *operator) handleTelemetryStages(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	seconds := windowSeconds(window)
	if seconds == 0 {
		http.Error(w, "window must be one of 1d, 7d, 30d", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	if tel, generatedAt, ok := o.telemetryCache.get(seconds, now); ok {
		writeJSON(w, http.StatusOK, telemetryStagesResponse{
			WindowSeconds:  seconds,
			GeneratedAt:    generatedAt,
			StageTelemetry: tel,
		})
		return
	}
	since := now.Add(-time.Duration(seconds) * time.Second)
	tel, err := o.store.Telemetry().StageTelemetry(r.Context(), since)
	if err != nil {
		http.Error(w, "telemetry aggregation failed", http.StatusInternalServerError)
		return
	}
	o.telemetryCache.put(seconds, tel, now)
	writeJSON(w, http.StatusOK, telemetryStagesResponse{
		WindowSeconds:  seconds,
		GeneratedAt:    now,
		StageTelemetry: tel,
	})
}

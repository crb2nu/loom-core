package main

import (
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
)

// configOutcomesDefaultWindow is two weeks, matching the judge calibration
// read: a configuration's win rate only exists once the runs stamped under it
// have reached a terminal state.
const configOutcomesDefaultWindow = 336 * time.Hour

// handleConfigOutcomes serves the per-configuration win-rate report over
// ?window=. Open read like GET /api/mills/judge-calibration: it aggregates
// provenance stamps and terminal run states the operator already exposes.
func (o *operator) handleConfigOutcomes(w http.ResponseWriter, r *http.Request) {
	window := configOutcomesDefaultWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid window (Go duration required): " + raw})
			return
		}
		window = d
	}
	if o.store == nil || o.store.Events == nil || o.store.Pipeline == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "events store unavailable"})
		return
	}

	now := time.Now().UTC()
	report, err := guard.BuildConfigOutcomeReport(r.Context(), o.store.Events, o.store.Pipeline, now.Add(-window), now)
	if err != nil {
		o.logger.Warn("config outcome report failed", "window", window.String(), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

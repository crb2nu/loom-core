package main

import (
	"net/http"
	"time"
)

const tasteCalibrationDefaultWindow = 14 * 24 * time.Hour

func (o *operator) handleTasteCalibration(w http.ResponseWriter, r *http.Request) {
	window := tasteCalibrationDefaultWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid window (Go duration required): " + raw})
			return
		}
		window = d
	}
	if o.store == nil || o.store.Outcomes == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "outcome writeback store unavailable"})
		return
	}
	now := time.Now().UTC()
	report, err := o.store.Outcomes.Calibration(r.Context(), now.Add(-window), now)
	if err != nil {
		o.logger.Warn("taste calibration report failed", "window", window.String(), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

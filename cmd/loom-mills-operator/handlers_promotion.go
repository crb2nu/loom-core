package main

import (
	"net/http"
	"time"
)

// Promotion-report defaults: the overseer family over the last week, the
// window a Mill Staff promotion review reads before flipping an agent's
// dry_run off.
const (
	promotionReportDefaultActor  = "overseer."
	promotionReportDefaultWindow = 168 * time.Hour
)

// handlePromotionReport serves the dry-run→promote evidence artifact for
// every guarded actor under ?actor= over ?window=. Open read like
// GET /api/mills/overseers, whose recent-actions block already exposes the
// same audit rows — this endpoint only aggregates them.
func (o *operator) handlePromotionReport(w http.ResponseWriter, r *http.Request) {
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		actor = promotionReportDefaultActor
	}
	window := promotionReportDefaultWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid window (Go duration required): " + raw})
			return
		}
		window = d
	}
	o.writeReportRollup(w, r, "promotion", window, actor)
}

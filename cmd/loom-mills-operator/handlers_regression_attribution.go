package main

import (
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Regression-listing defaults: two weeks, the span over which a revert of a
// merged change is still a live signal for judge calibration and promotion
// review. The scan limit bounds the event window read behind it.
const (
	regressionsDefaultWindow = 336 * time.Hour
	regressionsScanLimit     = 500
)

// regressionAttributionView is one attributed regression: the merged MR, the
// commit its work landed as, and the revert that undid it.
type regressionAttributionView struct {
	RegressedMRIID int64     `json:"regressed_mr_iid"`
	MergedSHA      string    `json:"merged_sha"`
	RevertSHA      string    `json:"revert_sha"`
	RevertTitle    string    `json:"revert_title"`
	AttributedAt   time.Time `json:"attributed_at"`
}

type regressionsResponse struct {
	Window      string                      `json:"window"`
	Since       time.Time                   `json:"since"`
	Count       int                         `json:"count"`
	Regressions []regressionAttributionView `json:"regressions"`
}

// handleRegressionsList serves the revert-precise post-merge regressions
// attributed over ?window=. Open read like GET /api/mills/promotion-report:
// it aggregates durable events and exposes no secrets.
func (o *operator) handleRegressionsList(w http.ResponseWriter, r *http.Request) {
	window := regressionsDefaultWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid window (Go duration required): " + raw})
			return
		}
		window = d
	}
	o.writeReportRollup(w, r, "regressions", window, "")
}

func eventPayloadString(ev *store.Event, key string) string {
	if ev == nil {
		return ""
	}
	s, _ := ev.Payload[key].(string)
	return s
}

// eventPayloadInt64 reads a numeric payload field. Payloads round-trip through
// JSON, so an int64 written by the sweep is read back as float64 — both forms
// are accepted so the value survives regardless of which side produced it.
func eventPayloadInt64(ev *store.Event, key string) int64 {
	if ev == nil {
		return 0
	}
	switch v := ev.Payload[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

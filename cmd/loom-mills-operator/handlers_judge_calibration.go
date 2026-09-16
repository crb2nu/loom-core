package main

import (
	"net/http"
	"time"
)

// judgeCalibrationDefaultWindow is two weeks: long enough for the runs graded
// early in the window to have reached a terminal state, which is the only way
// a verdict acquires ground truth.
const judgeCalibrationDefaultWindow = 336 * time.Hour

// handleJudgeCalibration serves the judge-verdict-vs-reality report over
// ?window=. Open read like GET /api/mills/promotion-report: it only aggregates
// events and terminal run states the operator already exposes.
func (o *operator) handleJudgeCalibration(w http.ResponseWriter, r *http.Request) {
	window := judgeCalibrationDefaultWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid window (Go duration required): " + raw})
			return
		}
		window = d
	}
	o.writeReportRollup(w, r, "judge_calibration", window, "")
}

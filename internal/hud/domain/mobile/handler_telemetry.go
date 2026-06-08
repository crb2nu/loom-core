package mobile

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// handleMobileRecoveryTelemetryIngest records one device's rolling
// disconnect-to-recovered sample window. Scope-gated by ScopeTelemetry
// (off by default), rate-limited (mutation class), keyed by X-Device-ID.
//
// POST /api/mobile/v1/telemetry/recovery
//
//	{ "samples": [5.2, 8.1, 22.3], "slo_target_seconds": 30 }
func (d *MobileDomain) handleMobileRecoveryTelemetryIngest(w http.ResponseWriter, r *http.Request) {
	if !d.requireMobileScope(w, r, ScopeTelemetry) {
		return
	}

	deviceID := ExtractDeviceID(r)
	if deviceID == "" {
		d.writeMobileError(w, http.StatusBadRequest, "missing_device_id", "X-Device-ID header is required")
		return
	}

	var body recoveryIngestRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		d.writeMobileError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if len(body.Samples) == 0 {
		d.writeMobileError(w, http.StatusBadRequest, "bad_request", "samples must be a non-empty array")
		return
	}

	stat := d.recovery.Ingest(deviceID, body.Samples, body.SLOTargetSeconds)
	if stat.SampleCount == 0 {
		// Every sample was non-finite / non-positive.
		d.writeMobileError(w, http.StatusBadRequest, "bad_request", "samples must contain at least one positive finite value")
		return
	}

	d.logMobileAudit(r, "telemetry_recovery_ingest", map[string]string{
		"sample_count": strconv.Itoa(stat.SampleCount),
	}, "success", nil)

	d.writeMobileJSON(w, http.StatusOK, map[string]any{
		"accepted": true,
		"device":   stat,
	})
}

// handleMobileRecoveryTelemetryRead returns the fleet-wide recovery-SLO rollup
// pooled across all reporting devices. Scope ScopeRead.
//
// GET /api/mobile/v1/telemetry/recovery
func (d *MobileDomain) handleMobileRecoveryTelemetryRead(w http.ResponseWriter, r *http.Request) {
	if !d.requireMobileScope(w, r, ScopeRead) {
		return
	}
	d.writeMobileJSON(w, http.StatusOK, d.recovery.Aggregate())
}

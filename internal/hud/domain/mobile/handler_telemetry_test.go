package mobile

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// telemetryTestDeps returns mockDeps whose mobile token grants the given scopes.
func telemetryTestDeps(scopes string) *mockDeps {
	return &mockDeps{
		config: MobileConfig{OperatorToken: "test-token", OperatorScopes: scopes},
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func telemetryMux(d *MobileDomain) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/mobile/v1/telemetry/recovery", d.handleMobileRecoveryTelemetryIngest)
	mux.HandleFunc("GET /api/mobile/v1/telemetry/recovery", d.handleMobileRecoveryTelemetryRead)
	return mux
}

func ingestReq(body string) *http.Request {
	req := httptest.NewRequest("POST", "/api/mobile/v1/telemetry/recovery", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-ID", "device-123")
	return req
}

func TestRecoveryIngest_HappyPath(t *testing.T) {
	d := New(telemetryTestDeps("mobile:read,mobile:telemetry"))
	mux := telemetryMux(d)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ingestReq(`{"samples":[5,6,7],"slo_target_seconds":30}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok=true: %s", rec.Body.String())
	}
	data, _ := env.Data.(map[string]any)
	if accepted, _ := data["accepted"].(bool); !accepted {
		t.Errorf("expected accepted=true, got %v", data["accepted"])
	}
	dev, _ := data["device"].(map[string]any)
	if dev["device_id"] != "device-123" {
		t.Errorf("device_id = %v, want device-123", dev["device_id"])
	}
	if cnt, _ := dev["sample_count"].(float64); cnt != 3 {
		t.Errorf("sample_count = %v, want 3", dev["sample_count"])
	}
}

func TestRecoveryIngest_MissingDeviceID(t *testing.T) {
	d := New(telemetryTestDeps("mobile:telemetry"))
	mux := telemetryMux(d)

	req := httptest.NewRequest("POST", "/api/mobile/v1/telemetry/recovery", strings.NewReader(`{"samples":[5]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	// No X-Device-ID.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRecoveryIngest_EmptySamples(t *testing.T) {
	d := New(telemetryTestDeps("mobile:telemetry"))
	mux := telemetryMux(d)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ingestReq(`{"samples":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty samples, got %d", rec.Code)
	}
}

func TestRecoveryIngest_InvalidJSON(t *testing.T) {
	d := New(telemetryTestDeps("mobile:telemetry"))
	mux := telemetryMux(d)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ingestReq(`{not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad JSON, got %d", rec.Code)
	}
}

func TestRecoveryIngest_AllInvalidSamples(t *testing.T) {
	d := New(telemetryTestDeps("mobile:telemetry"))
	mux := telemetryMux(d)
	rec := httptest.NewRecorder()
	// Valid JSON, but no positive finite values survive sanitization.
	mux.ServeHTTP(rec, ingestReq(`{"samples":[0,-1,-2]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when all samples invalid, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRecoveryIngest_RequiresTelemetryScope(t *testing.T) {
	// Token has read but NOT telemetry → ingest must be forbidden.
	d := New(telemetryTestDeps("mobile:read"))
	mux := telemetryMux(d)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ingestReq(`{"samples":[5]}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without mobile:telemetry, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRecoveryRead_ReflectsIngest(t *testing.T) {
	d := New(telemetryTestDeps("mobile:read,mobile:telemetry"))
	mux := telemetryMux(d)

	// Ingest from two devices.
	for _, dev := range []struct{ id, body string }{
		{"dev-a", `{"samples":[5,6,7]}`},
		{"dev-b", `{"samples":[8,9,10]}`},
	} {
		req := httptest.NewRequest("POST", "/api/mobile/v1/telemetry/recovery", strings.NewReader(dev.body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Device-ID", dev.id)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest %s: expected 200, got %d: %s", dev.id, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest("GET", "/api/mobile/v1/telemetry/recovery", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	data, _ := env.Data.(map[string]any)
	if dc, _ := data["device_count"].(float64); dc != 2 {
		t.Errorf("device_count = %v, want 2", data["device_count"])
	}
	if ts, _ := data["total_samples"].(float64); ts != 6 {
		t.Errorf("total_samples = %v, want 6", data["total_samples"])
	}
	if meets, _ := data["meets_slo"].(bool); !meets {
		t.Errorf("meets_slo = %v, want true", data["meets_slo"])
	}
}

func TestRecoveryRead_EmptyStore(t *testing.T) {
	d := New(telemetryTestDeps("mobile:read"))
	mux := telemetryMux(d)

	req := httptest.NewRequest("GET", "/api/mobile/v1/telemetry/recovery", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	data, _ := env.Data.(map[string]any)
	if dc, _ := data["device_count"].(float64); dc != 0 {
		t.Errorf("device_count = %v, want 0", data["device_count"])
	}
	if meets, _ := data["meets_slo"].(bool); !meets {
		t.Errorf("meets_slo = %v, want true (vacuous)", data["meets_slo"])
	}
}

func TestRecoveryRead_RequiresReadScope(t *testing.T) {
	// Token has telemetry but NOT read → read must be forbidden.
	d := New(telemetryTestDeps("mobile:telemetry"))
	mux := telemetryMux(d)
	req := httptest.NewRequest("GET", "/api/mobile/v1/telemetry/recovery", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without mobile:read, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHUDRecoveryAggregate verifies the same-origin operator-UI read: no auth
// required, and the body is the raw aggregate (no {ok,data,meta} envelope) so
// the HUD Svelte store can consume it like the other /api/* reads.
func TestHUDRecoveryAggregate(t *testing.T) {
	d := New(telemetryTestDeps("mobile:read,mobile:telemetry"))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/mobile/v1/telemetry/recovery", d.handleMobileRecoveryTelemetryIngest)
	mux.HandleFunc("GET /api/telemetry/recovery", d.handleHUDRecoveryAggregate)

	// Seed one device through the normal ingest path.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, ingestReq(`{"samples":[5,6,7],"slo_target_seconds":30}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// HUD-internal read with NO Authorization header — must still succeed.
	req := httptest.NewRequest("GET", "/api/telemetry/recovery", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hud read: expected 200 without auth, got %d: %s", rec.Code, rec.Body.String())
	}

	// Body must be the raw aggregate, not an envelope.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, hasEnvelope := raw["ok"]; hasEnvelope {
		t.Errorf("expected raw aggregate, got enveloped response: %s", rec.Body.String())
	}
	if _, hasData := raw["data"]; hasData {
		t.Errorf("expected raw aggregate, got enveloped response: %s", rec.Body.String())
	}

	var agg RecoveryAggregate
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("aggregate did not decode: %v", err)
	}
	if agg.DeviceCount != 1 {
		t.Errorf("device_count = %d, want 1", agg.DeviceCount)
	}
	if agg.TotalSamples != 3 {
		t.Errorf("total_samples = %d, want 3", agg.TotalSamples)
	}
	if !agg.MeetsSLO {
		t.Errorf("meets_slo = false, want true")
	}
}

// TestHUDRecoveryAggregate_EmptyStore verifies the operator tile gets a valid
// vacuous rollup before any device has reported.
func TestHUDRecoveryAggregate_EmptyStore(t *testing.T) {
	d := New(telemetryTestDeps("mobile:read"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/telemetry/recovery", d.handleHUDRecoveryAggregate)

	req := httptest.NewRequest("GET", "/api/telemetry/recovery", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var agg RecoveryAggregate
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if agg.DeviceCount != 0 {
		t.Errorf("device_count = %d, want 0", agg.DeviceCount)
	}
	if !agg.MeetsSLO {
		t.Errorf("meets_slo = false, want true (vacuous)")
	}
	if agg.Devices == nil {
		t.Errorf("devices should be an empty array, not null")
	}
}

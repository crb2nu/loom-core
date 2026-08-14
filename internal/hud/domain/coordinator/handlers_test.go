package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	hudcoord "github.com/crb2nu/loom/internal/hud/coordinator"
)

// --- test doubles ---

type stubCoordOps struct {
	compressResult *hudcoord.CompactionResult
	compressErr    error
}

func (s *stubCoordOps) RunCompression(_ context.Context) (*hudcoord.CompactionResult, error) {
	return s.compressResult, s.compressErr
}

type stubMetricsOps struct {
	handler http.Handler
}

func (s *stubMetricsOps) Handler() http.Handler { return s.handler }

type fakeDeps struct {
	coord          CoordinatorOps
	metrics        MetricsOps
	lastJSON       any
	lastJSONStatus int
	lastErrMsg     string
	lastErrStatus  int
}

func (f *fakeDeps) WriteJSON(w http.ResponseWriter, status int, v any) {
	f.lastJSON = v
	f.lastJSONStatus = status
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeDeps) WriteError(w http.ResponseWriter, status int, msg string, _ error) {
	f.lastErrMsg = msg
	f.lastErrStatus = status
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (f *fakeDeps) Coordinator() CoordinatorOps    { return f.coord }
func (f *fakeDeps) CoordinatorMetrics() MetricsOps { return f.metrics }

// --- tests ---

func TestHandleCoordinatorCompress_Disabled(t *testing.T) {
	deps := &fakeDeps{coord: nil}
	d := New(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/coordinator/compress", nil)
	d.handleCoordinatorCompress(w, r)

	if deps.lastErrStatus != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", deps.lastErrStatus)
	}
}

func TestHandleCoordinatorCompress_NilResult(t *testing.T) {
	stub := &stubCoordOps{compressResult: nil}
	deps := &fakeDeps{coord: stub}
	d := New(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/coordinator/compress", nil)
	d.handleCoordinatorCompress(w, r)

	if deps.lastJSONStatus != http.StatusOK {
		t.Fatalf("want 200, got %d", deps.lastJSONStatus)
	}
	m, ok := deps.lastJSON.(map[string]string)
	if !ok {
		t.Fatal("expected map[string]string response")
	}
	if m["status"] != "nothing_to_compress" {
		t.Fatalf("want nothing_to_compress, got %s", m["status"])
	}
}

func TestHandleCoordinatorCompress_Success(t *testing.T) {
	stub := &stubCoordOps{
		compressResult: &hudcoord.CompactionResult{Tier: "hot", CompressedCount: 5},
	}
	deps := &fakeDeps{coord: stub}
	d := New(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/coordinator/compress", nil)
	d.handleCoordinatorCompress(w, r)

	if deps.lastJSONStatus != http.StatusOK {
		t.Fatalf("want 200, got %d", deps.lastJSONStatus)
	}
}

func TestHandleCoordinatorCompress_ErrUnavailable(t *testing.T) {
	stub := &stubCoordOps{compressErr: hudcoord.ErrUnavailable}
	deps := &fakeDeps{coord: stub}
	d := New(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/coordinator/compress", nil)
	d.handleCoordinatorCompress(w, r)

	if deps.lastErrStatus != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", deps.lastErrStatus)
	}
}

func TestHandleCoordinatorCompress_OtherError(t *testing.T) {
	stub := &stubCoordOps{compressErr: fmt.Errorf("some backend error")}
	deps := &fakeDeps{coord: stub}
	d := New(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/coordinator/compress", nil)
	d.handleCoordinatorCompress(w, r)

	if deps.lastErrStatus != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", deps.lastErrStatus)
	}
}

func TestCoordinatorErrStatus(t *testing.T) {
	if s := coordinatorErrStatus(hudcoord.ErrUnavailable); s != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", s)
	}
	if s := coordinatorErrStatus(fmt.Errorf("other")); s != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", s)
	}
}

func TestRegisterRoutes(t *testing.T) {
	metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	deps := &fakeDeps{
		coord:   &stubCoordOps{},
		metrics: &stubMetricsOps{handler: metricsHandler},
	}
	d := New(deps)
	mux := http.NewServeMux()
	mw := func(h http.HandlerFunc) http.HandlerFunc { return h }
	d.RegisterRoutes(mux, mw)

	// Verify routes are registered by sending requests.
	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/api/coordinator/compress"},
		{"GET", "/api/coordinator/metrics"},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tt.method, tt.path, &bytes.Buffer{})
		mux.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound {
			t.Errorf("route %s %s not registered", tt.method, tt.path)
		}
	}
}

// TestRegisterRoutes_Pruned asserts the dark coordinator routes stay deleted.
func TestRegisterRoutes_Pruned(t *testing.T) {
	deps := &fakeDeps{coord: &stubCoordOps{}}
	d := New(deps)
	mux := http.NewServeMux()
	mw := func(h http.HandlerFunc) http.HandlerFunc { return h }
	d.RegisterRoutes(mux, mw)

	pruned := []struct {
		method string
		path   string
	}{
		{"GET", "/api/coordinator/status"},
		{"POST", "/api/coordinator/summarize/sess-1"},
		{"POST", "/api/coordinator/plan"},
	}
	for _, tt := range pruned {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(tt.method, tt.path, &bytes.Buffer{})
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("route %s %s should be pruned, got %d", tt.method, tt.path, w.Code)
		}
	}
}

func TestRegisterRoutes_NilMetrics(t *testing.T) {
	deps := &fakeDeps{coord: &stubCoordOps{}, metrics: nil}
	d := New(deps)
	mux := http.NewServeMux()
	mw := func(h http.HandlerFunc) http.HandlerFunc { return h }
	d.RegisterRoutes(mux, mw)

	// Metrics route should not be registered.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/coordinator/metrics", nil)
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for unregistered metrics, got %d", w.Code)
	}
}

func TestName(t *testing.T) {
	d := New(&fakeDeps{})
	if d.Name() != "coordinator" {
		t.Fatalf("want coordinator, got %s", d.Name())
	}
}

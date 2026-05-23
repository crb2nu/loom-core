package aimodels

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeUpstream serves a canned /v1/models response shaped like the real
// FlexInfer proxy so the handler exercises the same JSON decode path
// production hits.
func fakeUpstream(body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestHandleFlexInferModels_HappyPath(t *testing.T) {
	t.Parallel()
	upstream := fakeUpstream(`{"data":[
		{"id":"gemma4-26b-a4b-gptq","object":"model","metadata":{"phase":"Ready","ready":true}},
		{"id":"gemma4-e4b-gguf","object":"model","metadata":{"phase":"Idle","ready":false}},
		{"id":"plain-no-metadata","object":"model"}
	]}`, http.StatusOK)
	defer upstream.Close()

	deps := &fakeDeps{proxyURL: upstream.URL}
	d := New(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/aimodels/flexinfer/models", nil)
	rec := httptest.NewRecorder()
	d.handleFlexInferModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Models []flexInferModelEntry `json:"models"`
		Source string                `json:"source"`
		Error  string                `json:"error,omitempty"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "" {
		t.Fatalf("unexpected error: %q", body.Error)
	}
	if len(body.Models) != 3 {
		t.Fatalf("models len = %d, want 3", len(body.Models))
	}
	if body.Models[0].ID != "gemma4-26b-a4b-gptq" || !body.Models[0].Ready || body.Models[0].Phase != "Ready" {
		t.Errorf("Ready model not surfaced: %+v", body.Models[0])
	}
	if body.Models[1].ID != "gemma4-e4b-gguf" || body.Models[1].Ready || body.Models[1].Phase != "Idle" {
		t.Errorf("Idle model not surfaced: %+v", body.Models[1])
	}
	// Models lacking metadata default to unready / no phase — frontend
	// renders as 'unknown' rather than misleading 'idle'.
	if body.Models[2].Ready || body.Models[2].Phase != "" {
		t.Errorf("metadata-less model should default to unready/blank: %+v", body.Models[2])
	}
}

func TestHandleFlexInferModels_EmptyProxyURL(t *testing.T) {
	t.Parallel()
	deps := &fakeDeps{proxyURL: ""}
	d := New(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/aimodels/flexinfer/models", nil)
	rec := httptest.NewRecorder()
	d.handleFlexInferModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no proxy configured", rec.Code)
	}
	var body struct {
		Models []flexInferModelEntry `json:"models"`
		Source string                `json:"source"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Models) != 0 {
		t.Errorf("models should be empty with no proxy, got %d", len(body.Models))
	}
	if body.Source != "" {
		t.Errorf("source should be empty, got %q", body.Source)
	}
}

func TestHandleFlexInferModels_UpstreamErrorDegradesGracefully(t *testing.T) {
	t.Parallel()
	upstream := fakeUpstream(`internal server error`, http.StatusInternalServerError)
	defer upstream.Close()

	deps := &fakeDeps{proxyURL: upstream.URL}
	d := New(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/aimodels/flexinfer/models", nil)
	rec := httptest.NewRecorder()
	d.handleFlexInferModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("upstream errors should still produce HTTP 200 + error field, got %d", rec.Code)
	}
	var body struct {
		Models []flexInferModelEntry `json:"models"`
		Error  string                `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error == "" {
		t.Errorf("upstream 500 should populate error field")
	}
	if len(body.Models) != 0 {
		t.Errorf("upstream error should yield empty models, got %d", len(body.Models))
	}
}

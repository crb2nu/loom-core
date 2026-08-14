package handler_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/crb2nu/sprocket/internal/handler"
	"github.com/crb2nu/sprocket/internal/repository"
	"github.com/crb2nu/sprocket/internal/service"
)

func TestSprocketGauge(t *testing.T) {
	mux := newTestMux()

	health := request(t, mux, http.MethodGet, "/healthz", nil)
	assertStatus(t, health, http.StatusOK)
	assertJSONContentType(t, health)
	var healthBody map[string]string
	decodeBody(t, health, &healthBody)
	if healthBody["status"] != "ok" {
		t.Fatalf("health status = %q, want ok", healthBody["status"])
	}

	create := request(t, mux, http.MethodPost, "/sprockets", bytes.NewBufferString(`{"name":"drive sprocket","quantity":7}`))
	assertStatus(t, create, http.StatusCreated)
	assertJSONContentType(t, create)
	var created service.Sprocket
	decodeBody(t, create, &created)
	if created.Name != "drive sprocket" || created.Quantity != 7 {
		t.Fatalf("created sprocket = %+v, want supplied fields", created)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(created.ID) {
		t.Fatalf("created id = %q, want 16 lowercase hex chars", created.ID)
	}

	get := request(t, mux, http.MethodGet, "/sprockets/"+created.ID, nil)
	assertStatus(t, get, http.StatusOK)
	var got service.Sprocket
	decodeBody(t, get, &got)
	if got != created {
		t.Fatalf("round trip sprocket = %+v, want %+v", got, created)
	}

	missing := request(t, mux, http.MethodGet, "/sprockets/unknown", nil)
	assertStatus(t, missing, http.StatusNotFound)
	assertJSONContentType(t, missing)
	var errBody map[string]string
	decodeBody(t, missing, &errBody)
	if errBody["error"] == "" {
		t.Fatalf("missing error envelope = %#v, want non-empty error", errBody)
	}
}

func TestListEmptyReturnsArray(t *testing.T) {
	resp := request(t, newTestMux(), http.MethodGet, "/sprockets", nil)
	assertStatus(t, resp, http.StatusOK)

	body := bytes.TrimSpace(resp.Body.Bytes())
	if string(body) != "[]" {
		t.Fatalf("body = %s, want []", body)
	}
}

func TestCreateValidationError(t *testing.T) {
	resp := request(t, newTestMux(), http.MethodPost, "/sprockets", bytes.NewBufferString(`{"name":"","quantity":1}`))
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONContentType(t, resp)

	var body map[string]string
	decodeBody(t, resp, &body)
	if body["error"] == "" {
		t.Fatalf("error envelope = %#v, want non-empty error", body)
	}
}

func TestDefaultErrorsUseEnvelope(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "not found", method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
		{name: "method not allowed", method: http.MethodPut, path: "/sprockets", status: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := request(t, newTestMux(), tt.method, tt.path, nil)
			assertStatus(t, resp, tt.status)
			assertJSONContentType(t, resp)

			var body map[string]string
			decodeBody(t, resp, &body)
			if body["error"] == "" {
				t.Fatalf("error envelope = %#v, want non-empty error", body)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	mux := newTestMux()
	create := request(t, mux, http.MethodPost, "/sprockets", bytes.NewBufferString(`{"name":"idler","quantity":2}`))
	assertStatus(t, create, http.StatusCreated)
	var created service.Sprocket
	decodeBody(t, create, &created)

	del := request(t, mux, http.MethodDelete, "/sprockets/"+created.ID, nil)
	assertStatus(t, del, http.StatusNoContent)
	if del.Body.Len() != 0 {
		t.Fatalf("delete body = %q, want empty", del.Body.String())
	}

	get := request(t, mux, http.MethodGet, "/sprockets/"+created.ID, nil)
	assertStatus(t, get, http.StatusNotFound)
}

func newTestMux() http.Handler {
	repo := repository.NewMemorySprocketRepository()
	svc := service.NewSprocketService(repo)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := handler.NewSprocketHandler(svc, logger)
	return h.Routes()
}

func request(t *testing.T, handler http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func assertStatus(t *testing.T, resp *httptest.ResponseRecorder, want int) {
	t.Helper()
	if resp.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, want, resp.Body.String())
	}
}

func assertJSONContentType(t *testing.T, resp *httptest.ResponseRecorder) {
	t.Helper()
	if got := resp.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
}

func decodeBody(t *testing.T, resp *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, resp.Body.String())
	}
}

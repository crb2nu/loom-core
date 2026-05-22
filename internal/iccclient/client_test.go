package iccclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/iccclient"
)

func newClient(t *testing.T, h http.HandlerFunc) *iccclient.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return iccclient.NewForTest(srv.URL, srv.Client())
}

func TestEnsureConfigured_ReturnsErrorWhenEmpty(t *testing.T) {
	c := iccclient.NewForTest("", &http.Client{})
	if err := c.EnsureConfigured(); err == nil {
		t.Fatal("expected error for empty baseURL")
	}
}

func TestPostJSON_HappyPathUnwrapsEnvelope(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"id": "x_1", "value": 42},
		})
	})

	type body struct {
		ID    string `json:"id"`
		Value int    `json:"value"`
	}
	status, out, err := iccclient.PostJSON[body](context.Background(), c, "/api/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("expected 201, got %d", status)
	}
	if out.ID != "x_1" || out.Value != 42 {
		t.Fatalf("expected envelope unwrapped, got %+v", out)
	}
}

func TestPostJSON_4xxBubblesServerMessage(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad payload"})
	})

	_, _, err := iccclient.PostJSON[map[string]any](context.Background(), c, "/api/test", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad payload") {
		t.Fatalf("expected 'bad payload' bubbled, got: %v", err)
	}
}

func TestGetRaw_HappyPathBarePayload(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "project_id=prj_1" {
			t.Fatalf("expected query, got %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"projects": []any{map[string]any{"id": "prj_1"}},
		})
	})

	type resp struct {
		Projects []map[string]any `json:"projects"`
	}
	_, out, err := iccclient.GetRaw[resp](context.Background(), c, "/api/projects/overview",
		map[string]string{"project_id": "prj_1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Projects) != 1 || out.Projects[0]["id"] != "prj_1" {
		t.Fatalf("unexpected payload: %+v", out)
	}
}

func TestGetRaw_DropsEmptyQueryValues(t *testing.T) {
	var seenQuery string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})

	_, _, err := iccclient.GetRaw[map[string]any](context.Background(), c, "/api/x",
		map[string]string{"a": "1", "b": "", "c": "3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(seenQuery, "b=") {
		t.Fatalf("expected empty 'b' to be dropped, got %q", seenQuery)
	}
	if !strings.Contains(seenQuery, "a=1") || !strings.Contains(seenQuery, "c=3") {
		t.Fatalf("expected a=1 and c=3, got %q", seenQuery)
	}
}

func TestPost_TrustedContextHeadersAttached(t *testing.T) {
	var seenHeaders http.Header
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":null}`))
	})

	_, _, err := iccclient.PostJSON[*any](context.Background(), c, "/api/x", map[string]any{"y": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := seenHeaders.Get("X-Requested-With"); got != "integration-command-center" {
		t.Fatalf("expected XRW header, got %q", got)
	}
	if seenHeaders.Get("Origin") == "" {
		t.Fatalf("expected non-empty Origin")
	}
	if got := seenHeaders.Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected application/json content-type, got %q", got)
	}
}

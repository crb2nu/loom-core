package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGitLabWebBaseURL pins the derivation of the instance web base from the
// configured REST API URL (mill-floor B1).
func TestGitLabWebBaseURL(t *testing.T) {
	cases := []struct {
		name   string
		apiURL string
		want   string
	}{
		{"standard api url", "https://gitlab.flexinfer.ai/api/v4", "https://gitlab.flexinfer.ai"},
		{"trailing slash", "https://gitlab.flexinfer.ai/api/v4/", "https://gitlab.flexinfer.ai"},
		{"no api suffix", "https://gitlab.flexinfer.ai", "https://gitlab.flexinfer.ai"},
		{"host with port", "http://gitlab.local:8080/api/v4", "http://gitlab.local:8080"},
		{"whitespace padded", "  https://gitlab.flexinfer.ai/api/v4  ", "https://gitlab.flexinfer.ai"},
		{"empty", "", ""},
		{"scheme-less", "gitlab.flexinfer.ai/api/v4", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitlabWebBaseURL(tc.apiURL); got != tc.want {
				t.Errorf("gitlabWebBaseURL(%q): got %q want %q", tc.apiURL, got, tc.want)
			}
		})
	}
}

// TestWithGitLabBaseURL_StoresDerivedBase confirms the builder derives and
// stores the web base on the operator.
func TestWithGitLabBaseURL_StoresDerivedBase(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	op.withGitLabBaseURL("https://gitlab.flexinfer.ai/api/v4")
	if op.gitlabBaseURL != "https://gitlab.flexinfer.ai" {
		t.Fatalf("gitlabBaseURL: got %q want https://gitlab.flexinfer.ai", op.gitlabBaseURL)
	}
}

// TestStatus_ExposesGitLabBaseURL proves GET /api/mills/status carries the
// gitlab_base_url the HUD needs to build a clickable MR link.
func TestStatus_ExposesGitLabBaseURL(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.withGitLabBaseURL("https://gitlab.flexinfer.ai/api/v4")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/status", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		GitLabBaseURL string `json:"gitlab_base_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.GitLabBaseURL != "https://gitlab.flexinfer.ai" {
		t.Errorf("gitlab_base_url: got %q want https://gitlab.flexinfer.ai", resp.GitLabBaseURL)
	}
}

// TestStatus_GitLabBaseURL_EmptyWhenUnset confirms the field is present but
// empty when no API URL is configured, so the HUD degrades to an iid chip
// rather than a broken link.
func TestStatus_GitLabBaseURL_EmptyWhenUnset(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/status", nil)
	op.httpMux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := raw["gitlab_base_url"]
	if !ok {
		t.Fatal("gitlab_base_url key absent from status body")
	}
	if string(v) != `""` {
		t.Errorf("gitlab_base_url: got %s want empty string", v)
	}
}

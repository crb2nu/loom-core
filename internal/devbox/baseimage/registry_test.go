package baseimage

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLookup_KnownVersions(t *testing.T) {
	tests := []struct {
		lang, ver string
		wantImage string
	}{
		{"go", "1.24", "registry.harbor.lan/mcp/devbox-base/go:1.24"},
		{"go", "1.25", "registry.harbor.lan/mcp/devbox-base/go:1.25"},
		{"go", "1.25.10", "registry.harbor.lan/mcp/devbox-base/go:1.25"},
		{"go", "1.26.0", "registry.harbor.lan/mcp/devbox-base/go:1.26"},
		{"go", "1.26.6", "registry.harbor.lan/mcp/devbox-base/go:1.26"},
		{"python", "3.12", "registry.harbor.lan/mcp/devbox-base/python:3.12"},
		{"python", "3.12.13", "registry.harbor.lan/mcp/devbox-base/python:3.12"},
		{"python", "3.13", "registry.harbor.lan/mcp/devbox-base/python:3.13"},
		{"node", "20", "registry.harbor.lan/mcp/devbox-base/node:20"},
		{"node", "20.11.1", "registry.harbor.lan/mcp/devbox-base/node:20"},
		{"node", "22", "registry.harbor.lan/mcp/devbox-base/node:22"},
	}
	for _, tt := range tests {
		got := Lookup(tt.lang, tt.ver)
		if got != tt.wantImage {
			t.Errorf("Lookup(%q, %q) = %q, want %q", tt.lang, tt.ver, got, tt.wantImage)
		}
	}
}

func TestProbeRegistry(t *testing.T) {
	t.Run("self-signed TLS available", func(t *testing.T) {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer s.Close()
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // #nosec G402 -- verifies the configured Buildah-compatible posture.
		got := ProbeRegistry(context.Background(), client, s.URL, RegistryCredentials{})
		assertAllOutcomes(t, got, ProbeAvailable)
	})
	t.Run("missing", func(t *testing.T) {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }))
		defer s.Close()
		got := ProbeRegistry(context.Background(), s.Client(), s.URL, RegistryCredentials{})
		assertAllOutcomes(t, got, ProbeMissing)
	})
	t.Run("unauthorized", func(t *testing.T) {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
		defer s.Close()
		got := ProbeRegistry(context.Background(), s.Client(), s.URL, RegistryCredentials{})
		assertAllOutcomes(t, got, ProbeUnauthorized)
	})
	t.Run("dial failure", func(t *testing.T) {
		s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client, url := s.Client(), s.URL
		s.Close()
		client.Timeout = 100 * time.Millisecond
		got := ProbeRegistry(context.Background(), client, url, RegistryCredentials{})
		assertAllOutcomes(t, got, ProbeTransport)
	})
}

func assertAllOutcomes(t *testing.T, got []ProbeResult, want ProbeOutcome) {
	t.Helper()
	if len(got) != len(Languages()) {
		t.Fatalf("got %d results, want %d", len(got), len(Languages()))
	}
	for _, result := range got {
		if result.Outcome != want {
			t.Fatalf("%s:%s outcome = %q, want %q", result.Language, result.Version, result.Outcome, want)
		}
	}
}

func TestLookup_Unknown(t *testing.T) {
	if got := Lookup("go", "1.19"); got != "" {
		t.Errorf("Lookup(go, 1.19) = %q, want empty", got)
	}
	if got := Lookup("ruby", "3.0"); got != "" {
		t.Errorf("Lookup(ruby, 3.0) = %q, want empty", got)
	}
}

func TestLookup_CaseInsensitive(t *testing.T) {
	if got := Lookup("Go", "1.25"); got == "" {
		t.Error("Lookup should be case-insensitive for language")
	}
}

func TestLanguages(t *testing.T) {
	langs := Languages()
	if len(langs) != 7 {
		t.Errorf("expected 7 registered base images, got %d", len(langs))
	}
}

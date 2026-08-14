package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMillsCFAccessHeaders_Precedence verifies the service-token resolution
// order: LOOM_MILLS_CF_ACCESS_* > CF_ACCESS_CLIENT_* env. (The config-file
// fallback is exercised by loadHUDConfig's own tests; it is gated behind a
// process-wide sync.Once that would make ordering here non-deterministic.)
func TestMillsCFAccessHeaders_Precedence(t *testing.T) {
	t.Run("mills-specific env wins", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_CF_ACCESS_ID", "mills-id")
		t.Setenv("LOOM_MILLS_CF_ACCESS_SECRET", "mills-secret")
		t.Setenv("CF_ACCESS_CLIENT_ID", "generic-id")
		t.Setenv("CF_ACCESS_CLIENT_SECRET", "generic-secret")
		id, secret := millsCFAccessHeaders()
		if id != "mills-id" || secret != "mills-secret" {
			t.Fatalf("got (%q,%q), want mills-specific creds", id, secret)
		}
	})

	t.Run("falls back to generic CF env", func(t *testing.T) {
		t.Setenv("LOOM_MILLS_CF_ACCESS_ID", "")
		t.Setenv("LOOM_MILLS_CF_ACCESS_SECRET", "")
		t.Setenv("CF_ACCESS_CLIENT_ID", "generic-id")
		t.Setenv("CF_ACCESS_CLIENT_SECRET", "generic-secret")
		id, secret := millsCFAccessHeaders()
		if id != "generic-id" || secret != "generic-secret" {
			t.Fatalf("got (%q,%q), want generic CF creds", id, secret)
		}
	})
}

// TestMillsStatus_CFAccessSkippedForLoopback proves the loopback guard: even
// when a service token is configured, the CLI must NOT leak CF-Access headers
// to a loopback operator (the kubectl port-forward path). The httptest server
// binds 127.0.0.1, so it stands in for that path.
func TestMillsStatus_CFAccessSkippedForLoopback(t *testing.T) {
	var sawID, sawSecret string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mills/status", func(w http.ResponseWriter, r *http.Request) {
		sawID = r.Header.Get("CF-Access-Client-Id")
		sawSecret = r.Header.Get("CF-Access-Client-Secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"db_ok":true,"policy_enabled":true,"policy_version":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("CF_ACCESS_CLIENT_ID", "should-not-leak-id")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "should-not-leak-secret")

	cmd := newMillsCmd()
	cmd.SetArgs([]string{"status", "--operator-url", srv.URL})
	cmd.SetContext(context.Background())
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sawID != "" || sawSecret != "" {
		t.Errorf("CF-Access headers leaked to loopback operator: id=%q secret=%q", sawID, sawSecret)
	}
}

// TestMillsCFAccessGuard_RemoteVsLoopback documents the injection predicate used
// in millsClient.do: loopback targets are skipped, public ingress hosts receive
// the service token.
func TestMillsCFAccessGuard_RemoteVsLoopback(t *testing.T) {
	cases := []struct {
		url        string
		wantInject bool
	}{
		{"https://mills.flexinfer.ai", true},
		{"https://mills.flexinfer.ai/", true},
		{"http://127.0.0.1:8090", false},
		{"http://localhost:8090", false},
	}
	for _, tc := range cases {
		if got := !isLocalHUDURL(tc.url); got != tc.wantInject {
			t.Errorf("inject(%q)=%v, want %v", tc.url, got, tc.wantInject)
		}
	}
}

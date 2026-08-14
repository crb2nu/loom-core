package mills

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeDeps is a minimal Deps implementation for proxy unit tests.
type fakeDeps struct {
	cfg          Config
	adminAllowed bool
}

func (f *fakeDeps) WriteJSON(w http.ResponseWriter, status int, _ any) { w.WriteHeader(status) }
func (f *fakeDeps) WriteError(w http.ResponseWriter, status int, msg string, _ error) {
	http.Error(w, msg, status)
}
func (f *fakeDeps) RequireAdminToken(w http.ResponseWriter, _ *http.Request) bool {
	if !f.adminAllowed {
		http.Error(w, "admin required", http.StatusUnauthorized)
		return false
	}
	return true
}
func (f *fakeDeps) Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (f *fakeDeps) MillsConfig() Config  { return f.cfg }

// TestProxy_ForwardsGetReadsWithoutAdmin verifies a read route reaches the
// upstream operator and the upstream sees its own Host header (no leaked
// HUD admin token).
func TestProxy_ForwardsGetReadsWithoutAdmin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The proxy must rewrite Host to the upstream's host.
		if !strings.Contains(r.Host, "127.0.0.1") {
			t.Errorf("upstream got Host=%q, want 127.0.0.1", r.Host)
		}
		// HUD admin token must never leak to the upstream.
		if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
			t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
		}
		// Path is preserved.
		if r.URL.Path != "/api/mills/status" {
			t.Errorf("upstream path = %q, want /api/mills/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL, AdminToken: "should-not-leak"}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/status", nil)
	req.Header.Set("X-Loom-Admin-Token", "browser-supplied-should-be-stripped")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("body = %q, want upstream JSON", rec.Body.String())
	}
}

// TestProxy_InjectsBearerOnMutations verifies POSTs reach the upstream with
// Authorization: Bearer <admin-token> set from Config.AdminToken.
func TestProxy_InjectsBearerOnMutations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got != "Bearer cluster-admin-token" {
			t.Errorf("upstream Authorization = %q, want Bearer cluster-admin-token", got)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "cluster-admin-token"},
		adminAllowed: true,
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/council/dryrun", strings.NewReader(`{"reason":"smoke"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestProxy_HUDAdminGateBlocksUnauthorizedMutations verifies the HUD admin
// gate runs *before* the proxy reaches upstream, so an unauthenticated
// caller never even hits the operator.
func TestProxy_HUDAdminGateBlocksUnauthorizedMutations(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "x"},
		adminAllowed: false, // simulate missing/invalid HUD admin token
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/backlog", strings.NewReader(`{"ID":"X","Title":"x"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if upstreamHits != 0 {
		t.Errorf("upstream was called %d times, want 0 (HUD gate must block)", upstreamHits)
	}
}

// TestProxy_DisabledWhenBaseURLEmpty returns 503 on every route when the
// operator URL is unset (developer laptops, no cluster reachable).
func TestProxy_DisabledWhenBaseURLEmpty(t *testing.T) {
	d := New(&fakeDeps{cfg: Config{}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/status", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "operator not configured") {
		t.Errorf("body = %q, want 'operator not configured'", rec.Body.String())
	}
}

// TestProxy_ForwardsSquadsReadsWithoutAdmin verifies that squad read
// endpoints (list + per-squad detail/memory/outcomes) reach the upstream
// operator without the HUD admin gate firing — the HUD's Squads panel
// must be able to poll these from a browser without elevated auth.
func TestProxy_ForwardsSquadsReadsWithoutAdmin(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/mills/squads"},
		{http.MethodGet, "/api/mills/squads/hud-frontend"},
		{http.MethodGet, "/api/mills/squads/hud-frontend/memory"},
		{http.MethodGet, "/api/mills/squads/hud-frontend/outcomes"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			seen := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
					t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer upstream.Close()

			d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL}})
			mux := http.NewServeMux()
			d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if seen != tc.path {
				t.Errorf("upstream saw path = %q, want %q", seen, tc.path)
			}
		})
	}
}

// TestProxy_SquadsRouteTestRequiresAdmin verifies the admin POST gate
// blocks unauthenticated callers before the request reaches the
// operator. Mirrors TestProxy_HUDAdminGateBlocksUnauthorizedMutations
// but exercises the squad-specific path.
func TestProxy_SquadsRouteTestRequiresAdmin(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "x"},
		adminAllowed: false,
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mills/squads/hud-frontend/route-test",
		strings.NewReader(`{"backlog_id":"X"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if hits != 0 {
		t.Errorf("upstream was called %d times, want 0 (HUD gate must block)", hits)
	}
}

// TestProxy_ForwardsAuditReadsWithoutAdmin verifies audit findings list +
// detail proxy through without the HUD admin gate firing — the HUD's
// Audit panel must be able to poll these from a browser without
// elevated auth.
func TestProxy_ForwardsAuditReadsWithoutAdmin(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/mills/audit/findings"},
		{http.MethodGet, "/api/mills/audit/findings/42"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			seen := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
					t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer upstream.Close()

			d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL}})
			mux := http.NewServeMux()
			d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if seen != tc.path {
				t.Errorf("upstream saw path = %q, want %q", seen, tc.path)
			}
		})
	}
}

// TestProxy_AuditRunRequiresAdmin verifies the admin POST gate blocks
// unauthenticated callers before the request reaches the operator's
// own admin gate. Mirrors TestProxy_SquadsRouteTestRequiresAdmin.
func TestProxy_AuditRunRequiresAdmin(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "x"},
		adminAllowed: false,
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mills/audit/run",
		strings.NewReader(`{"subject_kind":"council_artifact","subject_id":"COUNCIL-X"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if hits != 0 {
		t.Errorf("upstream was called %d times, want 0 (HUD gate must block)", hits)
	}
}

// TestProxy_ForwardsSpinningRoomFramesWithoutAdmin verifies the frame list
// proxies through without the HUD admin gate firing — the "Spin a plan"
// dialog must populate its selector from a browser without elevated auth.
func TestProxy_ForwardsSpinningRoomFramesWithoutAdmin(t *testing.T) {
	seen := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
			t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":false,"frames":[]}`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/spinning-room/frames", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seen != "/api/mills/spinning-room/frames" {
		t.Errorf("upstream saw path = %q, want /api/mills/spinning-room/frames", seen)
	}
}

// TestProxy_SpinRequiresAdmin verifies POST /api/mills/spin is blocked by the
// HUD admin gate before the request reaches the operator's own admin gate.
func TestProxy_SpinRequiresAdmin(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "x"},
		adminAllowed: false,
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/spin",
		strings.NewReader(`{"brief":"b","frame":"opus"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if hits != 0 {
		t.Errorf("upstream was called %d times, want 0 (HUD gate must block)", hits)
	}
}

// TestProxy_SpinAsyncRequiresAdmin verifies POST /api/mills/spin/async is
// blocked by the HUD admin gate before reaching the operator (plan .loom/166).
func TestProxy_SpinAsyncRequiresAdmin(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "x"},
		adminAllowed: false,
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mills/spin/async",
		strings.NewReader(`{"brief":"b","frame":"jacquard"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if hits != 0 {
		t.Errorf("upstream was called %d times, want 0 (HUD gate must block)", hits)
	}
}

// TestProxy_ForwardsSpinRunsReadsWithoutAdmin verifies the async-spin status
// reads proxy through without the HUD admin gate — the dialog polls them from a
// browser without elevated auth (plan .loom/166).
func TestProxy_ForwardsSpinRunsReadsWithoutAdmin(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/mills/spin/runs"},
		{http.MethodGet, "/api/mills/spin/runs/spin-123"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			seen := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
					t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer upstream.Close()

			d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL}})
			mux := http.NewServeMux()
			d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if seen != tc.path {
				t.Errorf("upstream saw path = %q, want %q", seen, tc.path)
			}
		})
	}
}

// TestProxy_ForwardsCrossRepoReadsWithoutAdmin verifies cross-repo
// list + per-run detail proxy through without the HUD admin gate
// firing — the HUD's CrossRepo card must poll these from a browser
// without elevated auth.
func TestProxy_ForwardsCrossRepoReadsWithoutAdmin(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/mills/cross-repo/runs"},
		{http.MethodGet, "/api/mills/cross-repo/runs/XR-1"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			seen := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
					t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer upstream.Close()

			d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL}})
			mux := http.NewServeMux()
			d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if seen != tc.path {
				t.Errorf("upstream saw path = %q, want %q", seen, tc.path)
			}
		})
	}
}

// TestProxy_CrossRepoAbortRequiresAdmin verifies the admin POST gate
// blocks unauthenticated callers before the request reaches the
// operator's own admin gate.
func TestProxy_CrossRepoAbortRequiresAdmin(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "x"},
		adminAllowed: false,
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mills/cross-repo/runs/XR-1/abort", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if hits != 0 {
		t.Errorf("upstream was called %d times, want 0 (HUD gate must block)", hits)
	}
}

// TestProxy_BadGatewayWhenUpstreamDown returns 502 when the upstream is
// unreachable, with the underlying error in the body.
func TestProxy_BadGatewayWhenUpstreamDown(t *testing.T) {
	d := New(&fakeDeps{cfg: Config{BaseURL: "http://127.0.0.1:1"}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/status", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// TestProxy_TransportHasDialDeadline pins the dial deadline on the proxy
// transport. The operator deploys with strategy=Recreate, so every rollout
// briefly blackholes the old pod IP (SYN drops, no RST); without an explicit
// DialContext the default transport waits out the kernel's ~2min SYN-retry
// window and every /api/mills/* request through the HUD hangs for the whole
// rollout — the "infinite spinner" the run-detail drawer showed mid-deploy.
func TestProxy_TransportHasDialDeadline(t *testing.T) {
	u, err := url.Parse("http://loom-mills-operator.loom-mills.svc.cluster.local:8090")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newOperatorProxy(u, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	tr, ok := p.rp.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("proxy transport is %T, want *http.Transport", p.rp.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("transport.DialContext is nil — dial has no deadline, a blackholed operator IP hangs requests for ~2min")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Fatal("transport.TLSHandshakeTimeout unset")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("transport.ResponseHeaderTimeout unset")
	}
}

// TestProxy_ForwardsTelemetryStagesWithoutAdmin verifies the S5 telemetry
// roll-up proxies through as an open read (no HUD admin gate) with the path
// preserved and the HUD admin token stripped — the "Mill Telemetry" panel polls
// it from a browser without elevated auth.
func TestProxy_ForwardsTelemetryStagesWithoutAdmin(t *testing.T) {
	seen := ""
	seenQuery := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		seenQuery = r.URL.RawQuery
		if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
			t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"window_seconds":604800,"stages":[]}`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL, AdminToken: "should-not-leak"}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/telemetry/stages?window=7d", nil)
	req.Header.Set("X-Loom-Admin-Token", "browser-supplied-should-be-stripped")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seen != "/api/mills/telemetry/stages" {
		t.Errorf("upstream saw path = %q, want /api/mills/telemetry/stages", seen)
	}
	if seenQuery != "window=7d" {
		t.Errorf("upstream saw query = %q, want window=7d", seenQuery)
	}
}

// TestProxy_ForwardsOverseersWithoutAdmin verifies the overseers status
// snapshot proxies through as an open read (no HUD admin gate) with the path
// preserved and the HUD admin token stripped — the Overseers panel polls it
// from a browser without elevated auth. Without the explicit route
// registration the SPA fallback would serve HTML the frontend then fails to
// JSON.parse.
func TestProxy_ForwardsOverseersWithoutAdmin(t *testing.T) {
	seen := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
			t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true,"agents":[],"recent_actions":{}}`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL, AdminToken: "should-not-leak"}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/overseers", nil)
	req.Header.Set("X-Loom-Admin-Token", "browser-supplied-should-be-stripped")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seen != "/api/mills/overseers" {
		t.Errorf("upstream saw path = %q, want /api/mills/overseers", seen)
	}
}

// TestProxy_ForwardsStaffReportsWithoutAdmin verifies the five Mill Staff
// evidence reports proxy through as open reads with path AND query preserved
// and the HUD admin token stripped. Every one is window-bounded (?window=, and
// ?actor= on the promotion report), so a dropped query silently re-reports the
// operator's default window instead of the one the panel asked for. Without the
// explicit route registrations the SPA fallback serves HTML the frontend then
// fails to JSON.parse.
func TestProxy_ForwardsStaffReportsWithoutAdmin(t *testing.T) {
	cases := []struct{ path, query, body string }{
		{"/api/mills/promotion-report", "actor=overseer.&window=168h", `{"total_actions":0,"zero_evidence":true}`},
		{"/api/mills/judge-calibration", "window=336h", `{"total_verdicts":0,"zero_evidence":true}`},
		{"/api/mills/regressions", "window=336h", `{"count":0,"regressions":[]}`},
		{"/api/mills/config-outcomes", "window=336h", `{"stamped_runs":0,"zero_evidence":true}`},
		{"/api/mills/signature-candidates", "window=336h", `{"count":0,"candidates":[]}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			seen, seenQuery := "", ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.Path
				seenQuery = r.URL.RawQuery
				if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
					t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL, AdminToken: "should-not-leak"}})
			mux := http.NewServeMux()
			d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path+"?"+tc.query, nil)
			req.Header.Set("X-Loom-Admin-Token", "browser-supplied-should-be-stripped")
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if seen != tc.path {
				t.Errorf("upstream saw path = %q, want %q", seen, tc.path)
			}
			if seenQuery != tc.query {
				t.Errorf("upstream saw query = %q, want %q", seenQuery, tc.query)
			}
			if !strings.Contains(rec.Body.String(), strings.SplitN(tc.body, ":", 2)[0]) {
				t.Errorf("body = %q, want upstream JSON", rec.Body.String())
			}
		})
	}
}

// TestProxy_ForwardsCostPreviewWithoutAdmin verifies the per-backlog-item cost
// estimate proxies through as an open read with path AND query preserved. The
// frontend calls /api/mills/cost-preview?backlog_id=... before starting a run;
// without the explicit route registration the SPA fallback served index.html,
// which the store then failed to JSON.parse.
func TestProxy_ForwardsCostPreviewWithoutAdmin(t *testing.T) {
	seen := ""
	seenQuery := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		seenQuery = r.URL.RawQuery
		if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
			t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"backlog_id":"BL-1","estimated_usd":0.42}`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL, AdminToken: "should-not-leak"}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/cost-preview?backlog_id=BL-1", nil)
	req.Header.Set("X-Loom-Admin-Token", "browser-supplied-should-be-stripped")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seen != "/api/mills/cost-preview" {
		t.Errorf("upstream saw path = %q, want /api/mills/cost-preview", seen)
	}
	if seenQuery != "backlog_id=BL-1" {
		t.Errorf("upstream saw query = %q, want backlog_id=BL-1", seenQuery)
	}
	if !strings.Contains(rec.Body.String(), `"estimated_usd"`) {
		t.Errorf("body = %q, want upstream JSON", rec.Body.String())
	}
}

// TestProxy_ForwardsRelaunchCandidatesWithoutAdmin verifies the escalation
// relaunch-candidates projection proxies through as an open read with path AND
// query preserved and the HUD admin token stripped — the HUD polls it from a
// browser without elevated auth. Without the explicit route registration the
// SPA fallback would serve HTML the frontend then fails to JSON.parse.
func TestProxy_ForwardsRelaunchCandidatesWithoutAdmin(t *testing.T) {
	seen := ""
	seenQuery := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		seenQuery = r.URL.RawQuery
		if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
			t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL, AdminToken: "should-not-leak"}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/mills/escalations/relaunch-candidates?since=2026-07-01T00:00:00Z&limit=25", nil)
	req.Header.Set("X-Loom-Admin-Token", "browser-supplied-should-be-stripped")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seen != "/api/mills/escalations/relaunch-candidates" {
		t.Errorf("upstream saw path = %q, want /api/mills/escalations/relaunch-candidates", seen)
	}
	if seenQuery != "since=2026-07-01T00:00:00Z&limit=25" {
		t.Errorf("upstream saw query = %q, want since=2026-07-01T00:00:00Z&limit=25", seenQuery)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want upstream []", got)
	}
}

// TestProxy_ForwardsWiringWithoutAdmin verifies the resolved model-wiring
// snapshot proxies through as an open read (no HUD admin gate) with the path
// preserved and the HUD admin token stripped — the Mills Overview polls it from
// a browser without elevated auth. Without the explicit route registration the
// SPA fallback would serve HTML the frontend then fails to JSON.parse.
func TestProxy_ForwardsWiringWithoutAdmin(t *testing.T) {
	seen := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		if got := r.Header.Get("X-Loom-Admin-Token"); got != "" {
			t.Errorf("upstream got X-Loom-Admin-Token=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"judge":{"backend":"litellm"},"stages":[]}`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL, AdminToken: "should-not-leak"}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/wiring", nil)
	req.Header.Set("X-Loom-Admin-Token", "browser-supplied-should-be-stripped")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seen != "/api/mills/wiring" {
		t.Errorf("upstream saw path = %q, want /api/mills/wiring", seen)
	}
}

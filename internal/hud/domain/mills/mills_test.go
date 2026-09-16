package mills

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGradeRoute_AdminGateAndProxy(t *testing.T) {
	const body = `{"grade":"keep","note":"ship more"}`
	for _, tc := range []struct {
		name         string
		adminAllowed bool
		wantStatus   int
		wantHits     int
	}{
		{name: "authorized", adminAllowed: true, wantStatus: http.StatusOK, wantHits: 1},
		{name: "unauthorized", adminAllowed: false, wantStatus: http.StatusUnauthorized, wantHits: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if r.URL.Path != "/api/mills/pipeline/runs/run-1/grade" {
					t.Errorf("path = %q", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer operator-secret" {
					t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
				}
				got, _ := io.ReadAll(r.Body)
				if string(got) != body {
					t.Errorf("body = %q, want %q", got, body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"grade":"keep","note":"ship more"}`))
			}))
			defer upstream.Close()

			d := New(&fakeDeps{
				cfg:          Config{BaseURL: upstream.URL, AdminToken: "operator-secret"},
				adminAllowed: tc.adminAllowed,
			})
			mux := http.NewServeMux()
			d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/mills/pipeline/runs/run-1/grade", strings.NewReader(body))
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if hits != tc.wantHits {
				t.Fatalf("upstream hits = %d, want %d", hits, tc.wantHits)
			}
		})
	}
}

// The taste rollup is an open read like backlog: no admin gate, proxied
// verbatim. Without the explicit registration the SPA fallback answers HTML,
// which the iOS taste tile (and any JSON client) fails to parse — so this
// pins the route itself.
func TestTasteAggregatesRoute_ProxiesWithoutAdminGate(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/api/mills/taste/aggregates" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plans":[],"overall_graded_14d":0,"overall_merged_14d":3,"overall_coverage_14d":0}`))
	}))
	defer upstream.Close()

	d := New(&fakeDeps{
		cfg:          Config{BaseURL: upstream.URL, AdminToken: "operator-secret"},
		adminAllowed: false, // reads must not require the admin gate
	})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/taste/aggregates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
	if !strings.Contains(rec.Body.String(), `"overall_coverage_14d"`) {
		t.Fatalf("body not forwarded verbatim: %s", rec.Body.String())
	}
}

func TestDocsMirrorRouteProxiesReadUnchanged(t *testing.T) {
	const body = `{"state":"unknown","reason":"not yet computed"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/mills/finishing/docs-mirror" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	d := New(&fakeDeps{cfg: Config{BaseURL: upstream.URL}})
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/finishing/docs-mirror", nil))
	if rec.Code != 200 || rec.Body.String() != body {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

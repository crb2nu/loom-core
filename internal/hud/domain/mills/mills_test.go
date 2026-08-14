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

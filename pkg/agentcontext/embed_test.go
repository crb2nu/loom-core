package agentcontext

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeEmbedHealth(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct{ name, body, want string }{
		{"healthy zero", fmt.Sprintf(`{"status":"healthy","observed_at":%q,"fail_closed_counts":{"pattern":0,"writes":0}}`, now.Format(time.RFC3339Nano)), ""},
		{"threshold boundary", fmt.Sprintf(`{"status":"healthy","observed_at":%q,"fail_closed_counts":{}}`, now.Add(-5*time.Minute).Format(time.RFC3339Nano)), ""},
		{"non-zero fail-closed", fmt.Sprintf(`{"status":"healthy","observed_at":%q,"fail_closed_counts":{"pattern":1}}`, now.Format(time.RFC3339Nano)), "non-zero"},
		{"unhealthy status", fmt.Sprintf(`{"status":"unhealthy","observed_at":%q,"fail_closed_counts":{}}`, now.Format(time.RFC3339Nano)), "status"},
		{"malformed", `{"status":`, "decode"},
		{"missing counts", fmt.Sprintf(`{"status":"healthy","observed_at":%q}`, now.Format(time.RFC3339Nano)), "missing"},
		{"stale", fmt.Sprintf(`{"status":"healthy","observed_at":%q,"fail_closed_counts":{}}`, now.Add(-5*time.Minute-time.Nanosecond).Format(time.RFC3339Nano)), "stale"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, tt.body) }))
			defer srv.Close()
			_, err := ProbeEmbedHealth(context.Background(), srv.Client(), srv.URL, now, 5*time.Minute)
			if tt.want == "" && err != nil {
				t.Fatalf("ProbeEmbedHealth() error = %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("ProbeEmbedHealth() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestProbeEmbedHealthUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint, client := srv.URL, srv.Client()
	srv.Close()
	if _, err := ProbeEmbedHealth(context.Background(), client, endpoint, time.Now(), time.Minute); err == nil {
		t.Fatal("ProbeEmbedHealth() succeeded for unavailable endpoint")
	}
}

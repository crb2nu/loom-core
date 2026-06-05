package monitor

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewMillsMonitorStatusURL(t *testing.T) {
	cases := map[string]string{
		"http://op:8090":        "http://op:8090/api/mills/status",
		"http://op:8090/":       "http://op:8090/api/mills/status",
		"http://op.svc:8090///": "http://op.svc:8090/api/mills/status",
	}
	for base, want := range cases {
		m := NewMillsMonitor(base, nil)
		if m.statusURL != want {
			t.Errorf("base %q: statusURL=%q, want %q", base, m.statusURL, want)
		}
	}
}

func TestMillsMonitorRefreshDecodesAndBroadcasts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mills/status" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"autonomy_ready":true,"last_merge_at":"2026-06-05T00:00:00Z"}`))
	}))
	defer srv.Close()

	m := NewMillsMonitor(srv.URL, nil)
	var broadcast MillsSnapshot
	m.OnRefresh(func(snap MillsSnapshot) { broadcast = snap })

	if err := m.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if ready, _ := m.Snapshot()["autonomy_ready"].(bool); !ready {
		t.Errorf("expected autonomy_ready=true in snapshot, got %+v", m.Snapshot())
	}
	if broadcast == nil {
		t.Fatal("expected OnRefresh to receive the snapshot")
	}
	if _, ok := broadcast["last_merge_at"]; !ok {
		t.Errorf("expected last_merge_at in broadcast snapshot, got %+v", broadcast)
	}
}

func TestMillsMonitorRefreshNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	m := NewMillsMonitor(srv.URL, nil)
	if err := m.Refresh(); err == nil {
		t.Fatal("expected error on non-200 status, got nil")
	}
}

func TestMillsMonitorStopIdempotent(t *testing.T) {
	m := NewMillsMonitor("http://op:8090", nil)
	m.Stop()
	m.Stop()
}

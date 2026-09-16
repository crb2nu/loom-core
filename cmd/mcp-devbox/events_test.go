package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/devbox/backend"
)

func TestRecordBaseImageFallbackEmitsEventAndMetricOnce(t *testing.T) {
	var requests int
	var content string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var payload struct {
			Entries []struct {
				Content string `json:"content"`
			} `json:"entries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		content = payload.Entries[0].Content
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	m := &manager{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics: newMetrics(),
		events:  newEventEmitter(strings.TrimPrefix(srv.URL, "http://"), slog.Default()),
	}
	m.recordBaseImageFallback("example", &backend.BuildResult{BaseImageFallback: &backend.BaseImageFallback{
		Language: "go", Version: "1.27", Reason: "unmapped_version",
	}})
	if requests != 1 {
		t.Fatalf("event requests = %d, want 1", requests)
	}
	for _, want := range []string{`"project":"example"`, `"language":"go"`, `"version":"1.27"`, `"reason":"unmapped_version"`} {
		if !strings.Contains(content, want) {
			t.Errorf("event content %q missing %q", content, want)
		}
	}
	metrics, err := m.metrics.gather()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics, `loom_devbox_base_image_fallbacks_total{language="go",project="example",reason="unmapped_version",version="1.27"} 1`) {
		t.Fatalf("fallback metric missing:\n%s", metrics)
	}
}

package mills

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyCouncilRoutesBypassDefaultResponseHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	proxy := newOperatorProxy(upstreamURL, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	transport, ok := proxy.rp.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("proxy transport is %T, want *http.Transport", proxy.rp.Transport)
	}
	if got, want := transport.ResponseHeaderTimeout, 30*time.Second; got != want {
		t.Fatalf("default response header timeout = %s, want %s", got, want)
	}
	transport.ResponseHeaderTimeout = 5 * time.Millisecond
	councilTransport, ok := proxy.councilRP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("council proxy transport is %T, want *http.Transport", proxy.councilRP.Transport)
	}
	if got, want := councilTransport.ResponseHeaderTimeout, 11*time.Minute; got != want {
		t.Fatalf("council response header timeout = %s, want %s", got, want)
	}

	for _, path := range []string{
		"/api/mills/council/run",
		"/api/mills/council/dryrun",
	} {
		t.Run(strings.TrimPrefix(path, "/api/mills/council/"), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
			if recorder.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
			}
		})
	}

	t.Run("ordinary route keeps default timeout", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/mills/status", nil))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
		}
	})
}

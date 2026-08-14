package mills

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// operatorProxy is a thin reverse proxy that forwards /api/mills/* from
// the HUD to the in-cluster loom-mills-operator. It:
//
//   - rewrites Host so the upstream sees its own service name
//   - injects Authorization: Bearer <admin-token> when the request is a
//     mutation (POST/PUT/PATCH/DELETE) AND the caller didn't already
//     supply one (HUD admin token is the source of truth here; the
//     operator token never reaches the browser)
//   - drops hop-by-hop headers + the HUD's own bearer (so we never leak
//     it to the operator)
//
// The upstream URL is fixed for the lifetime of the HUD process. Config
// hot-reload would require recreating the proxy; not needed for v1.
type operatorProxy struct {
	upstream  *url.URL
	token     string
	logger    *slog.Logger
	rp        *httputil.ReverseProxy
	councilRP *httputil.ReverseProxy
}

const (
	defaultResponseHeaderTimeout = 30 * time.Second
	// Council handlers have a ten-minute request budget and a short response
	// margin. Keep the HUD's internal proxy alive beyond both without weakening
	// the fail-fast timeout used by ordinary Mills polling routes.
	councilResponseHeaderTimeout = 11 * time.Minute
)

func newOperatorProxy(upstream *url.URL, token string, logger *slog.Logger) *operatorProxy {
	p := &operatorProxy{upstream: upstream, token: token, logger: logger}
	p.rp = p.newReverseProxy(defaultResponseHeaderTimeout)
	p.councilRP = p.newReverseProxy(councilResponseHeaderTimeout)
	return p
}

func (p *operatorProxy) newReverseProxy(responseHeaderTimeout time.Duration) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director:       p.director,
		ErrorHandler:   p.errorHandler,
		FlushInterval:  -1,
		ModifyResponse: nil,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// The operator deploys with strategy=Recreate (single SQLite
			// writer), so every rollout has a window where the old pod IP
			// blackholes: SYN packets drop with no RST. Without a dial
			// deadline the transport waits out the kernel's ~2min SYN-retry
			// window and every /api/mills/* request through the HUD "hangs"
			// for the whole rollout. Fail the dial fast instead — the
			// frontend renders a retryable error and its background poll
			// self-heals once the operator is back.
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func (p *operatorProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isCouncilRunRequest(r) {
		p.councilRP.ServeHTTP(w, r)
		return
	}
	p.rp.ServeHTTP(w, r)
}

func isCouncilRunRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path == "/api/mills/council/run" ||
		r.URL.Path == "/api/mills/council/dryrun"
}

func (p *operatorProxy) director(req *http.Request) {
	req.URL.Scheme = p.upstream.Scheme
	req.URL.Host = p.upstream.Host
	req.Host = p.upstream.Host
	// Path: keep /api/mills/* exactly as-is; the operator listens on the
	// same path prefix so the rewrite is a no-op.
	if p.upstream.Path != "" && p.upstream.Path != "/" {
		req.URL.Path = strings.TrimRight(p.upstream.Path, "/") + req.URL.Path
	}
	// Strip the HUD's own admin-token header — the operator has its own
	// admin gate and shouldn't trust anything the browser sent. Then
	// inject the operator's admin token for mutations.
	req.Header.Del("X-Loom-Admin-Token")
	if p.token != "" && isMutation(req.Method) {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	// Identify ourselves so operator audit logs show the call origin.
	if ua := req.Header.Get("User-Agent"); ua == "" {
		req.Header.Set("User-Agent", "loom-hud/proxy")
	} else if !strings.Contains(ua, "loom-hud") {
		req.Header.Set("User-Agent", ua+" (via loom-hud/proxy)")
	}
}

func (p *operatorProxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if p.logger != nil {
		p.logger.Warn("mills proxy upstream error",
			"path", r.URL.Path, "method", r.Method, "error", err)
	}
	http.Error(w, "loom-mills operator unreachable: "+err.Error(), http.StatusBadGateway)
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

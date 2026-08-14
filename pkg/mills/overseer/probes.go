package overseer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Probe is one deployment-health check. Implementations must be cheap,
// side-effect free, and respect ctx (the sentinel applies the per-probe
// timeout). The interface is the seam a k8s/Flux rollout prober slots into
// later without touching the sentinel.
type Probe interface {
	Name() string
	Check(ctx context.Context) error
}

// HTTPProbe reports healthy on a 2xx response from a fixed URL. Anything
// else — transport error, timeout, 4xx (auth broken counts as unhealthy: the
// mill cannot use a dependency it cannot authenticate to), 5xx — is a
// failure with a compact reason.
type HTTPProbe struct {
	ProbeName string
	URL       string
	// Header entries (e.g. PRIVATE-TOKEN, Authorization) sent verbatim.
	Header http.Header
	// Client defaults to a fresh no-redirect client; the sentinel's ctx
	// carries the timeout.
	Client *http.Client
}

// Name implements Probe.
func (p *HTTPProbe) Name() string { return p.ProbeName }

// Check implements Probe.
func (p *HTTPProbe) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	for k, vs := range p.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// NewHTTPProbe builds an HTTPProbe with optional single-value headers.
// Empty header values are dropped so callers can pass unconditional maps.
func NewHTTPProbe(name, url string, headers map[string]string) *HTTPProbe {
	h := http.Header{}
	for k, v := range headers {
		if strings.TrimSpace(v) != "" {
			h.Set(k, v)
		}
	}
	return &HTTPProbe{ProbeName: name, URL: url, Header: h, Client: &http.Client{
		// The sentinel ctx bounds the wall clock; this transport-level cap is
		// a backstop for a probe used outside it.
		Timeout: 2 * time.Minute,
	}}
}

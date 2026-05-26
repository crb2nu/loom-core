// Package iccclient is a shared HTTP wrapper around the Integration
// Command Center (ICC) backend, used by the loom-core MCP server
// binaries that need to talk to ICC (mcp-icc-capture, mcp-icc, ...).
//
// The MCP server is a trusted-context caller: it sends
// Content-Type + X-Requested-With + Origin and the backend accepts
// that. HMAC signing is intentionally NOT implemented — that's a
// future hardening slice — but the package is structured so it can
// be added later without changing handler call sites.
package iccclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/env"
)

// Client is a thin HTTP wrapper around the ICC backend.
type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

// defaultTimeout is used when ICC_TIMEOUT_SECONDS is unset / invalid.
const defaultTimeout = 30 * time.Second

// New reads ICC_* env vars and returns a configured client. It never
// fails — missing ICC_BASE_URL is fine at startup so pure-local tools
// still work without the backend reachable. Network-backed tools call
// EnsureConfigured() at call time and fail loud if the base URL is
// empty.
func New(logger *slog.Logger) *Client {
	// ICC_BASE_URL is the canonical name. ICC_API_URL is the
	// historical fallback so existing deployments don't have to flip
	// env vars in lockstep with the MCP-server roll-out.
	base := strings.TrimRight(strings.TrimSpace(
		env.StringWithFallbacks("ICC_BASE_URL", "ICC_API_URL"),
	), "/")
	timeout := time.Duration(env.Int("ICC_TIMEOUT_SECONDS", int(defaultTimeout/time.Second))) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL:    base,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
	}
}

// NewForTest builds a Client wired directly to an httptest server.
// Use only from *_test.go files.
func NewForTest(baseURL string, httpClient *http.Client) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
		logger:     slog.Default(),
	}
}

// BaseURL returns the configured base URL (or "" if not configured).
// Exposed mainly for diagnostics and test assertions.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// EnsureConfigured returns an error if ICC_BASE_URL is empty so
// callers fail loud rather than silently no-op against a missing
// backend.
func (c *Client) EnsureConfigured() error {
	if c == nil || c.baseURL == "" {
		return errors.New("ICC not configured: set ICC_BASE_URL")
	}
	return nil
}

// Post sends a JSON POST to <baseURL>/<path> with the three required
// headers (Content-Type, X-Requested-With, Origin) and returns the
// raw status code + body bytes. Callers branch on shape (e.g. paths
// that template an id into the URL need raw bytes, PostJSON's typed
// wrapper would lose that flexibility).
func (c *Client) Post(ctx context.Context, path string, body any) (int, []byte, error) {
	if err := c.EnsureConfigured(); err != nil {
		return 0, nil, err
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("encode request body: %w", err)
	}

	reqURL := c.baseURL + ensureLeadingSlash(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	c.setTrustedContextHeaders(ctx, req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST %s: %w", reqURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// Get sends a GET to <baseURL>/<path>?<query> with the trusted-context
// headers and returns the raw status + body bytes. The query map is
// encoded with url.Values so callers don't have to think about
// percent-encoding.
func (c *Client) Get(ctx context.Context, path string, query map[string]string) (int, []byte, error) {
	if err := c.EnsureConfigured(); err != nil {
		return 0, nil, err
	}

	reqURL := c.baseURL + ensureLeadingSlash(path)
	if len(query) > 0 {
		vals := url.Values{}
		for k, v := range query {
			if v == "" {
				continue
			}
			vals.Set(k, v)
		}
		if encoded := vals.Encode(); encoded != "" {
			reqURL += "?" + encoded
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	c.setTrustedContextHeaders(ctx, req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", reqURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// toolKey is the unexported context key used to thread the MCP tool
// name through to setTrustedContextHeaders. Per-request attribution
// is opt-in: handlers that want the X-ICC-MCP-Tool header set must
// call WithTool on their context before invoking Post/Get/etc.
type toolKey struct{}

// WithTool returns a derived context carrying the MCP tool name so
// the next outbound ICC request includes X-ICC-MCP-Tool: <name>.
// Empty names are dropped; the backend reads this header to populate
// the icc_mcp_writes_total{tool,outcome} metric. See
// services/project-management/docs/connectors.md §"MCP Write Path".
func WithTool(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, toolKey{}, name)
}

// toolFromContext extracts the tool name set by WithTool, or "" when
// the caller did not opt in.
func toolFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(toolKey{}).(string); ok {
		return v
	}
	return ""
}

// setTrustedContextHeaders applies the three headers the ICC backend's
// origin gate accepts, plus the per-tool attribution header when the
// context carries a tool name. Kept in one place so future hardening
// (e.g. HMAC) has a single seam to extend.
func (c *Client) setTrustedContextHeaders(ctx context.Context, req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "integration-command-center")
	req.Header.Set("Origin", c.baseURL)
	if tool := toolFromContext(ctx); tool != "" {
		req.Header.Set("X-ICC-MCP-Tool", tool)
	}
}

// Envelope is the standard ICC response envelope:
// {"ok": bool, "result": <payload>} on success and
// {"error": "<message>"} on failure. Some ICC endpoints return the
// result payload directly without the envelope (e.g. /api/state,
// /api/projects/overview) — use Decode for those.
type Envelope[T any] struct {
	OK     bool   `json:"ok"`
	Result T      `json:"result"`
	Error  string `json:"error"`
}

// PostJSON is a convenience wrapper around Post that decodes the
// response envelope into a typed result. It returns the raw status
// code so callers can still distinguish 201 (fresh) from 200
// (idempotent) when the response body carries the distinction
// implicitly.
func PostJSON[T any](ctx context.Context, c *Client, path string, body any) (int, T, error) {
	var zero T
	status, raw, err := c.Post(ctx, path, body)
	if err != nil {
		return status, zero, err
	}

	if status >= 200 && status < 300 {
		if len(raw) == 0 {
			return status, zero, fmt.Errorf("ICC returned empty body (status=%d)", status)
		}
		var env Envelope[T]
		if err := json.Unmarshal(raw, &env); err != nil {
			return status, zero, fmt.Errorf("decode response: %w; body=%s", err, string(raw))
		}
		return status, env.Result, nil
	}

	return status, zero, classifyError(status, raw)
}

// GetJSON is the GET counterpart of PostJSON. Some ICC list endpoints
// return a bare payload (no envelope), e.g. the projects/overview
// route. Use Decode for those rather than calling GetJSON.
func GetJSON[T any](ctx context.Context, c *Client, path string, query map[string]string) (int, T, error) {
	var zero T
	status, raw, err := c.Get(ctx, path, query)
	if err != nil {
		return status, zero, err
	}

	if status >= 200 && status < 300 {
		if len(raw) == 0 {
			return status, zero, fmt.Errorf("ICC returned empty body (status=%d)", status)
		}
		var env Envelope[T]
		if err := json.Unmarshal(raw, &env); err != nil {
			return status, zero, fmt.Errorf("decode response: %w; body=%s", err, string(raw))
		}
		return status, env.Result, nil
	}

	return status, zero, classifyError(status, raw)
}

// GetRaw is the bare-payload counterpart of GetJSON. It assumes the
// response body IS the result (no {"ok":..., "result":...} wrapper).
// Use for /api/state, /api/projects/overview, /api/needs-attention,
// the /api/projects/<id>/* read endpoints, etc.
func GetRaw[T any](ctx context.Context, c *Client, path string, query map[string]string) (int, T, error) {
	var zero T
	status, raw, err := c.Get(ctx, path, query)
	if err != nil {
		return status, zero, err
	}
	if status >= 200 && status < 300 {
		if len(raw) == 0 {
			return status, zero, fmt.Errorf("ICC returned empty body (status=%d)", status)
		}
		var out T
		if err := json.Unmarshal(raw, &out); err != nil {
			return status, zero, fmt.Errorf("decode response: %w; body=%s", err, string(raw))
		}
		return status, out, nil
	}
	return status, zero, classifyError(status, raw)
}

func classifyError(status int, raw []byte) error {
	// Try to pull a structured error message out of the body.
	var env Envelope[json.RawMessage]
	if json.Unmarshal(raw, &env) == nil && env.Error != "" {
		return fmt.Errorf("ICC %d: %s", status, env.Error)
	}
	return fmt.Errorf("ICC %d: %s", status, strings.TrimSpace(string(raw)))
}

func ensureLeadingSlash(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

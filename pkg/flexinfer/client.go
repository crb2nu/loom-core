// Package flexinfer provides an HTTP client for the FlexInfer OpenAI-compatible proxy.
package flexinfer

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

	"github.com/crb2nu/loom/pkg/llmusage"
)

// ChatMessage represents a single message in a chat conversation.
type ChatMessage struct {
	Role    string `json:"role"`    // "system", "user", "assistant"
	Content string `json:"content"` // Message text
}

// ChatCompletionRequest is the request body for /v1/chat/completions.
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// ChatCompletionResponse is the response from /v1/chat/completions.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   ChatCompletionUsage    `json:"usage"`
}

// ChatCompletionChoice represents one completion choice.
type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatCompletionUsage reports token consumption.
//
// PromptTokensDetails / InputTokensDetails carry the cached share of the
// prompt. The proxy in front of a local engine may speak either dialect and
// older vLLM builds emit neither, so both are parsed and coalesced by
// CachedTokens. A zero is "not reported", not "nothing hit" — see
// pkg/llmusage.
type ChatCompletionUsage struct {
	PromptTokens        int              `json:"prompt_tokens"`
	CompletionTokens    int              `json:"completion_tokens"`
	TotalTokens         int              `json:"total_tokens"`
	PromptTokensDetails llmusage.Details `json:"prompt_tokens_details"`
	InputTokensDetails  llmusage.Details `json:"input_tokens_details"`
}

// CachedTokens is the portion of PromptTokens the engine served from its
// prefix cache, across both usage dialects.
func (u ChatCompletionUsage) CachedTokens() int {
	return llmusage.CachedTokens(u.PromptTokensDetails, u.InputTokensDetails)
}

// Normalized converts the wire usage into the shape pkg/llmusage reports.
func (u ChatCompletionUsage) Normalized() llmusage.Usage {
	return llmusage.Usage{
		PromptTokens:       u.PromptTokens,
		CachedPromptTokens: u.CachedTokens(),
		CompletionTokens:   u.CompletionTokens,
	}
}

// ModelInfo describes an available model.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse is the response from /v1/models.
type ModelsResponse struct {
	Data []ModelInfo `json:"data"`
}

// Client is an HTTP client for the FlexInfer OpenAI-compatible proxy.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	breaker *CircuitBreaker
	logger  *slog.Logger
	// usage observes each completion's token accounting. Read-only
	// instrumentation; the zero value is silent, so a caller that never calls
	// SetUsageSink still gets the log line and nothing else.
	usage llmusage.Observer
}

// SetUsageSink attaches a Prometheus (or other) destination for per-completion
// token accounting. Optional: this package exports no metrics of its own, so
// the sink comes from whichever component owns a registry — internal/hud/
// coordinator does, and wires itself up in its own constructor.
//
// Not part of NewClient's signature because every existing caller would have
// to pass nil, and the instrumentation is meant to be invisible to callers who
// do not care about it.
func (c *Client) SetUsageSink(sink llmusage.Sink) {
	if c == nil {
		return
	}
	c.usage.Sink = sink
}

// ErrLiteLLMMissingAPIKey is returned locally when a LiteLLM gateway is
// configured without credentials. It is intentionally deterministic so
// operators can distinguish a configuration fault from a remote outage.
var ErrLiteLLMMissingAPIKey = errors.New("flexinfer: LiteLLM API key is not configured; configure the gateway API key")

// HTTPStatusError is returned when FlexInfer responds with an HTTP error
// status. Callers can inspect it with errors.As.
type HTTPStatusError struct {
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.Body == "" {
		return fmt.Sprintf("flexinfer: status %d", e.StatusCode)
	}
	return fmt.Sprintf("flexinfer: status %d: %s", e.StatusCode, e.Body)
}

// NewClient creates a Client targeting the given base URL.
// The timeout parameter sets the HTTP client timeout; pass 0 for the default (30s).
func NewClient(baseURL, apiKey string, timeout time.Duration, breaker *CircuitBreaker, logger *slog.Logger) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: timeout,
		},
		breaker: breaker,
		logger:  logger.With("component", "flexinfer-client"),
		usage: llmusage.Observer{
			Logger:    logger.With("component", "flexinfer-client"),
			Component: "flexinfer",
		},
	}
}

// Complete sends a chat completion request through the circuit breaker.
func (c *Client) Complete(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if err := c.preflightAuth(); err != nil {
		return nil, err
	}

	var resp *ChatCompletionResponse

	err := c.breaker.Execute(func() error {
		var err error
		resp, err = c.doComplete(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// CompleteSimple is a convenience wrapper for the common case: system prompt +
// user message -> string response.
func (c *Client) CompleteSimple(ctx context.Context, model, systemPrompt, userMessage string, maxTokens int) (string, error) {
	req := ChatCompletionRequest{
		Model: model,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMessage},
		},
		MaxTokens:   maxTokens,
		Temperature: 0.3,
	}

	resp, err := c.Complete(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("flexinfer: empty response (no choices)")
	}
	return resp.Choices[0].Message.Content, nil
}

// Models lists available models from FlexInfer.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	if err := c.preflightAuth(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("flexinfer models request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("flexinfer models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("flexinfer models: status %d: %s", resp.StatusCode, body)
	}

	var result ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("flexinfer models decode: %w", err)
	}
	return result.Data, nil
}

// HealthCheck verifies FlexInfer is reachable by listing models.
func (c *Client) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := c.Models(ctx)
	if err != nil {
		return fmt.Errorf("flexinfer health check: %w", err)
	}
	return nil
}

// doComplete performs the actual HTTP request to /v1/chat/completions.
func (c *Client) doComplete(ctx context.Context, reqBody ChatCompletionRequest) (*ChatCompletionResponse, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("flexinfer marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("flexinfer create request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.http.Do(req)
	latency := time.Since(start)

	if err != nil {
		c.logger.Warn("flexinfer request failed", "model", reqBody.Model, "latency", latency, "error", err)
		return nil, fmt.Errorf("flexinfer request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		c.logger.Warn("flexinfer non-200", "model", reqBody.Model, "status", resp.StatusCode, "body", string(respBody))
		return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var result ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("flexinfer decode response: %w", err)
	}

	c.logger.Debug("flexinfer complete",
		"model", result.Model,
		"latency", latency,
		"prompt_tokens", result.Usage.PromptTokens,
		"completion_tokens", result.Usage.CompletionTokens,
	)
	// Separate line from the one above on purpose: that one is this client's
	// own debug trace and its fields are free to change, while this one is the
	// stable cross-client usage record docs/JOURNAL_ENGINE.md queries by name.
	c.usage.Observe(ctx, result.Model, result.Usage.Normalized())

	return &result, nil
}

// setHeaders adds authorization and common headers.
func (c *Client) setHeaders(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

// preflightAuth rejects a direct LiteLLM gateway with no usable API key before
// it can make an HTTP request or contribute a failure to the circuit breaker.
// Keyless FlexInfer-proxy URLs deliberately do not match this guardrail.
func (c *Client) preflightAuth() error {
	if isLiteLLMGatewayURL(c.baseURL) && strings.TrimSpace(c.apiKey) == "" {
		return ErrLiteLLMMissingAPIKey
	}
	return nil
}

func isLiteLLMGatewayURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(parsed.Hostname()), "litellm")
}

// Breaker returns the circuit breaker used by this client.
func (c *Client) Breaker() *CircuitBreaker {
	return c.breaker
}

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// APIKey returns the configured API key.
func (c *Client) APIKey() string {
	return c.apiKey
}

// IsCircuitOpen reports whether err is the local circuit-breaker-open sentinel.
func IsCircuitOpen(err error) bool {
	return errors.Is(err, ErrCircuitOpen)
}

// IsProviderOverload reports whether err looks like provider overload or rate
// limiting rather than an application/configuration failure.
func IsProviderOverload(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return isOverloadStatus(statusErr.StatusCode) || containsOverloadSignal(statusErr.Body)
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "overload") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "capacity") ||
		strings.Contains(lower, "temporarily unavailable")
}

func isOverloadStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 522:
		return true
	default:
		return false
	}
}

func containsOverloadSignal(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "overload") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "capacity") ||
		strings.Contains(lower, "temporarily unavailable")
}

// ResolveBaseURL chooses the base URL an OpenAI-compatible client should
// target, given a primary (possibly keyed) gateway and an optional
// keyless proxy.
//
// In some deployments gatewayURL points at a LiteLLM gateway that
// requires an API key. When no key is configured, every request — model
// listing and completions alike — fails with 401 "No api key passed in".
// The FlexInfer proxy serves the same model catalog and completions
// without auth, so when there is no gateway key but a keyless proxy URL
// is available, prefer the proxy. Returns gatewayURL unchanged when an
// API key is set or no proxy URL is provided.
func ResolveBaseURL(gatewayURL, apiKey, proxyURL string) string {
	if strings.TrimSpace(apiKey) == "" && strings.TrimSpace(proxyURL) != "" {
		return proxyURL
	}
	return gatewayURL
}

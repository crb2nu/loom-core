package flexinfer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMockFlexInfer creates a test server that mimics FlexInfer endpoints.
func newMockFlexInfer(t *testing.T, completionResponse string, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ModelsResponse{
				Data: []ModelInfo{
					{ID: "qwen3-8b", Object: "model", OwnedBy: "local"},
					{ID: "llama3-70b", Object: "model", OwnedBy: "local"},
				},
			})

		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			var req ChatCompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Logf("mock: failed to decode request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusCode)

			if statusCode == http.StatusOK {
				json.NewEncoder(w).Encode(ChatCompletionResponse{
					ID:      "chatcmpl-test",
					Object:  "chat.completion",
					Created: time.Now().Unix(),
					Model:   req.Model,
					Choices: []ChatCompletionChoice{
						{
							Index:        0,
							Message:      ChatMessage{Role: "assistant", Content: completionResponse},
							FinishReason: "stop",
						},
					},
					Usage: ChatCompletionUsage{
						PromptTokens:     100,
						CompletionTokens: 50,
						TotalTokens:      150,
					},
				})
			}

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func TestClient_CompleteSimple(t *testing.T) {
	server := newMockFlexInfer(t, `{"summary": "test summary"}`, http.StatusOK)
	defer server.Close()

	breaker := NewCircuitBreaker(5, time.Second)
	client := NewClient(server.URL, "", 0, breaker, slog.Default())

	result, err := client.CompleteSimple(context.Background(), "qwen3-8b", "system", "user msg", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "test summary") {
		t.Fatalf("expected result to contain 'test summary', got: %s", result)
	}
}

func TestClient_CompleteSimple_ServerError(t *testing.T) {
	server := newMockFlexInfer(t, "", http.StatusInternalServerError)
	defer server.Close()

	breaker := NewCircuitBreaker(5, time.Second)
	client := NewClient(server.URL, "", 0, breaker, slog.Default())

	_, err := client.CompleteSimple(context.Background(), "qwen3-8b", "system", "user msg", 100)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected HTTPStatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", statusErr.StatusCode)
	}
}

func TestClient_Models(t *testing.T) {
	server := newMockFlexInfer(t, "", http.StatusOK)
	defer server.Close()

	breaker := NewCircuitBreaker(5, time.Second)
	client := NewClient(server.URL, "", 0, breaker, slog.Default())

	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "qwen3-8b" {
		t.Fatalf("expected first model to be qwen3-8b, got %s", models[0].ID)
	}
}

func TestClient_HealthCheck(t *testing.T) {
	server := newMockFlexInfer(t, "", http.StatusOK)
	defer server.Close()

	breaker := NewCircuitBreaker(5, time.Second)
	client := NewClient(server.URL, "", 0, breaker, slog.Default())

	if err := client.HealthCheck(context.Background()); err != nil {
		t.Fatalf("expected healthy, got: %v", err)
	}
}

func TestClient_HealthCheck_Unreachable(t *testing.T) {
	breaker := NewCircuitBreaker(5, time.Second)
	client := NewClient("http://127.0.0.1:1", "", 0, breaker, slog.Default())

	err := client.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestClient_CircuitBreakerIntegration(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ModelsResponse{Data: []ModelInfo{{ID: "test"}}})
			return
		}
		callCount++
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer server.Close()

	breaker := NewCircuitBreaker(3, time.Hour)
	client := NewClient(server.URL, "", 0, breaker, slog.Default())

	// Trigger 3 failures to open the circuit.
	for i := 0; i < 3; i++ {
		_, _ = client.CompleteSimple(context.Background(), "test", "s", "u", 10)
	}

	if breaker.State() != StateOpen {
		t.Fatalf("expected circuit open, got %s", breaker.State())
	}

	// Next call should be rejected without hitting the server.
	prevCount := callCount
	_, err := client.CompleteSimple(context.Background(), "test", "s", "u", 10)
	if err == nil {
		t.Fatal("expected error when circuit is open")
	}
	if callCount != prevCount {
		t.Fatal("expected no server call when circuit is open")
	}
	if !IsCircuitOpen(err) {
		t.Fatalf("expected circuit-open classifier to match %v", err)
	}
}

func TestClient_ProviderOverloadClassifier(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "too many requests", err: &HTTPStatusError{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "bad gateway", err: &HTTPStatusError{StatusCode: http.StatusBadGateway}, want: true},
		{name: "service unavailable", err: &HTTPStatusError{StatusCode: http.StatusServiceUnavailable}, want: true},
		{name: "cloudflare timeout", err: &HTTPStatusError{StatusCode: 522}, want: true},
		{name: "body signal", err: &HTTPStatusError{StatusCode: http.StatusInternalServerError, Body: "provider at capacity"}, want: true},
		{name: "ordinary bad request", err: &HTTPStatusError{StatusCode: http.StatusBadRequest, Body: "bad prompt"}, want: false},
		{name: "string signal", err: errors.New("upstream rate limit exceeded"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsProviderOverload(tt.err); got != tt.want {
				t.Fatalf("IsProviderOverload(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestClient_AuthHeader(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			json.NewEncoder(w).Encode(ModelsResponse{Data: []ModelInfo{{ID: "test"}}})
			return
		}
		json.NewEncoder(w).Encode(ChatCompletionResponse{
			Choices: []ChatCompletionChoice{{Message: ChatMessage{Content: "ok"}}},
		})
	}))
	defer server.Close()

	breaker := NewCircuitBreaker(5, time.Second)
	client := NewClient(server.URL, "test-key-123", 0, breaker, slog.Default())

	_, _ = client.CompleteSimple(context.Background(), "test", "s", "u", 10)
	if authHeader != "Bearer test-key-123" {
		t.Fatalf("expected Bearer auth header, got: %q", authHeader)
	}
}

func TestClient_LiteLLMMissingAPIKeyPreflight(t *testing.T) {
	var requestCount int
	client := NewClient("https://litellm.example.test", "   ", 0, NewCircuitBreaker(1, time.Hour), slog.Default())
	client.http.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		requestCount++
		return nil, errors.New("transport should not be called")
	})

	_, err := client.CompleteSimple(context.Background(), "remote-model", "system", "user", 10)
	if !errors.Is(err, ErrLiteLLMMissingAPIKey) {
		t.Fatalf("expected missing LiteLLM key error, got %v", err)
	}
	if got, want := err.Error(), "flexinfer: LiteLLM API key is not configured; configure the gateway API key"; got != want {
		t.Fatalf("unexpected error: got %q, want %q", got, want)
	}
	if requestCount != 0 {
		t.Fatalf("expected preflight to make no HTTP requests, got %d", requestCount)
	}
	if state := client.Breaker().State(); state != StateClosed {
		t.Fatalf("expected preflight not to affect circuit breaker, got %s", state)
	}

	if err := client.HealthCheck(context.Background()); !errors.Is(err, ErrLiteLLMMissingAPIKey) {
		t.Fatalf("expected health check to return missing LiteLLM key error, got %v", err)
	}
	if requestCount != 0 {
		t.Fatalf("expected health-check preflight to make no HTTP requests, got %d", requestCount)
	}
}

func TestClient_LiteLLMAuthPreflightAllowsConfiguredRoutes(t *testing.T) {
	for _, tt := range []struct {
		name    string
		baseURL string
		apiKey  string
	}{
		{name: "authenticated LiteLLM gateway", baseURL: "https://litellm.example.test", apiKey: "sk-configured"},
		{name: "keyless FlexInfer proxy", baseURL: "https://flexinfer-proxy.example.test", apiKey: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.baseURL, tt.apiKey, 0, NewCircuitBreaker(1, time.Hour), slog.Default())
			requestCount := 0
			client.http.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				requestCount++
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
					Header:     make(http.Header),
				}, nil
			})
			_, err := client.CompleteSimple(context.Background(), "model", "system", "user", 10)
			if err != nil {
				t.Fatalf("expected configured route to complete, got %v", err)
			}
			if requestCount != 1 {
				t.Fatalf("expected one HTTP request, got %d", requestCount)
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestResolveBaseURL(t *testing.T) {
	t.Parallel()
	const (
		gateway = "http://litellm.ai.svc.cluster.local"
		proxy   = "http://flexinfer-proxy.flexinfer-system.svc.cluster.local"
	)
	tests := []struct {
		name    string
		gateway string
		apiKey  string
		proxy   string
		want    string
	}{
		{
			// The reported bug: keyed gateway, no key provisioned, keyless
			// proxy available -> must use the proxy (avoids 401).
			name:    "no key with proxy falls back to proxy",
			gateway: gateway,
			apiKey:  "",
			proxy:   proxy,
			want:    proxy,
		},
		{
			name:    "blank key with proxy falls back to proxy",
			gateway: gateway,
			apiKey:  "   ",
			proxy:   proxy,
			want:    proxy,
		},
		{
			// Operator provisioned a key -> honor the gateway, don't
			// silently re-route to the proxy.
			name:    "key set keeps gateway",
			gateway: gateway,
			apiKey:  "sk-secret",
			proxy:   proxy,
			want:    gateway,
		},
		{
			name:    "no key and no proxy keeps gateway",
			gateway: gateway,
			apiKey:  "",
			proxy:   "",
			want:    gateway,
		},
		{
			// Only the keyless proxy configured (gateway unset) -> proxy.
			name:    "only proxy configured",
			gateway: "",
			apiKey:  "",
			proxy:   proxy,
			want:    proxy,
		},
		{
			name:    "nothing configured returns empty",
			gateway: "",
			apiKey:  "",
			proxy:   "",
			want:    "",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveBaseURL(tt.gateway, tt.apiKey, tt.proxy); got != tt.want {
				t.Errorf("ResolveBaseURL(%q, %q, %q) = %q, want %q",
					tt.gateway, tt.apiKey, tt.proxy, got, tt.want)
			}
		})
	}
}

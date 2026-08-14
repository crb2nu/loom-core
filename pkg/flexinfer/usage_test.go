package flexinfer

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
)

// usageServer serves one chat completion with a caller-supplied raw usage
// block, so the tests below exercise the real JSON tags rather than a
// round-trip through our own structs (which would pass even if a tag were
// misspelled).
func usageServer(t *testing.T, rawUsage string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gemma4",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":` + rawUsage + `}`))
	}))
}

func testClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	return NewClient(endpoint, "", 5*time.Second, NewCircuitBreaker(5, time.Minute), slog.New(slog.DiscardHandler))
}

// TestCachedTokensParsedFromBothDialects is the load-bearing assertion of the
// whole instrumentation: if these JSON tags are wrong, every downstream metric
// reads a permanent 0% cache hit rate and nothing errors to say so.
func TestCachedTokensParsedFromBothDialects(t *testing.T) {
	tests := []struct {
		name       string
		rawUsage   string
		wantPrompt int
		wantCached int
	}{
		{
			name:       "chat completions dialect",
			rawUsage:   `{"prompt_tokens":4096,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":3968}}`,
			wantPrompt: 4096,
			wantCached: 3968,
		},
		{
			name:       "responses dialect",
			rawUsage:   `{"prompt_tokens":4096,"completion_tokens":12,"input_tokens_details":{"cached_tokens":2048}}`,
			wantPrompt: 4096,
			wantCached: 2048,
		},
		{
			name:       "engine omits the details block entirely",
			rawUsage:   `{"prompt_tokens":4096,"completion_tokens":12}`,
			wantPrompt: 4096,
			wantCached: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := usageServer(t, tc.rawUsage)
			defer srv.Close()

			resp, err := testClient(t, srv.URL).Complete(context.Background(), ChatCompletionRequest{
				Model:    "gemma4",
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Usage.PromptTokens != tc.wantPrompt {
				t.Errorf("prompt tokens = %d, want %d", resp.Usage.PromptTokens, tc.wantPrompt)
			}
			if got := resp.Usage.CachedTokens(); got != tc.wantCached {
				t.Errorf("cached tokens = %d, want %d", got, tc.wantCached)
			}
		})
	}
}

// TestUsageSinkReceivesCompletion covers the metrics path the HUD coordinator
// relies on, including the served-model attribution.
func TestUsageSinkReceivesCompletion(t *testing.T) {
	srv := usageServer(t, `{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":793}}`)
	defer srv.Close()

	sink := &captureSink{}
	client := testClient(t, srv.URL)
	client.SetUsageSink(sink)

	// A call site that narrows the component label; the client's own default
	// would otherwise apply.
	ctx := llmusage.WithComponent(context.Background(), "coordinator-summarizer")
	if _, err := client.Complete(ctx, ChatCompletionRequest{
		Model:    "requested-alias",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(sink.calls) != 1 {
		t.Fatalf("expected 1 sink call, got %d", len(sink.calls))
	}
	got := sink.calls[0]
	if got.component != "coordinator-summarizer" {
		t.Errorf("component = %q, want coordinator-summarizer", got.component)
	}
	if got.model != "gemma4" {
		t.Errorf("model = %q, want the served model gemma4 rather than the requested alias", got.model)
	}
	if got.usage.CachedPromptTokens != 793 {
		t.Errorf("cached prompt tokens = %d, want 793", got.usage.CachedPromptTokens)
	}
	if share := got.usage.CachedShare(); share != 0.793 {
		t.Errorf("cached share = %v, want 0.793", share)
	}
}

// TestUsageSinkSilentWithoutUsageBlock: a proxy that omits usage must produce
// no sample, so an unmeasured lane cannot drag an aggregate toward zero.
func TestUsageSinkSilentWithoutUsageBlock(t *testing.T) {
	srv := usageServer(t, `{}`)
	defer srv.Close()

	sink := &captureSink{}
	client := testClient(t, srv.URL)
	client.SetUsageSink(sink)

	if _, err := client.Complete(context.Background(), ChatCompletionRequest{
		Model:    "gemma4",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(sink.calls) != 0 {
		t.Fatalf("expected no sink calls when usage is absent, got %d", len(sink.calls))
	}
}

type captureSink struct {
	mu    sync.Mutex
	calls []capturedCall
}

type capturedCall struct {
	component string
	model     string
	usage     llmusage.Usage
}

func (s *captureSink) RecordUsage(component, model string, u llmusage.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, capturedCall{component: component, model: model, usage: u})
}

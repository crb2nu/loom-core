package openairesponses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIClientCreate_SendsRequestAndParsesToolCalls(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "resp_123",
			"conversation": "conv_123",
			"output_text":  "ignored because message text exists",
			"output": []map[string]any{
				{
					"type":      "function_call",
					"call_id":   "call_1",
					"name":      "math__add",
					"arguments": `{"a":2,"b":40}`,
				},
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "ready"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	client, err := NewAPIClient(APIClientConfig{
		APIKey:     "test-key",
		BaseURL:    srv.URL + "/v1",
		Timeout:    5 * time.Second,
		MaxRetries: 0,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	resp, err := client.Create(context.Background(), TurnRequest{
		Model: "gpt-5",
		Input: "hello",
		Tools: []ToolDefinition{
			{
				Name:        "math__add",
				Description: "Add numbers",
				InputSchema: map[string]any{"type": "object"},
				Strict:      true,
			},
		},
		Context: ContextStrategy{
			Mode:               ContextModeChain,
			PreviousResponseID: "resp_prev",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if gotBody["model"] != "gpt-5" {
		t.Fatalf("model = %#v", gotBody["model"])
	}
	if gotBody["previous_response_id"] != "resp_prev" {
		t.Fatalf("previous_response_id = %#v", gotBody["previous_response_id"])
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", gotBody["tools"])
	}
	if resp.ResponseID != "resp_123" {
		t.Fatalf("response id = %q", resp.ResponseID)
	}
	if resp.ConversationID != "conv_123" {
		t.Fatalf("conversation id = %q", resp.ConversationID)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d", len(resp.ToolCalls))
	}
	if string(resp.ToolCalls[0].Arguments) != `{"a":2,"b":40}` {
		t.Fatalf("arguments = %s", string(resp.ToolCalls[0].Arguments))
	}
	if resp.Terminal {
		t.Fatal("expected non-terminal response when tool calls are present")
	}
}

func TestAPIClientCreate_EncodesToolOutputsAndRetries(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, `{"error":{"message":"transient","type":"server_error"}}`, http.StatusBadGateway)
			return
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		input, ok := body["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("input = %#v", body["input"])
		}
		item := input[0].(map[string]any)
		if item["type"] != "function_call_output" {
			t.Fatalf("input item type = %#v", item["type"])
		}
		if item["output"] != `{"sum":42}` {
			t.Fatalf("output = %#v", item["output"])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_done",
			"output":      []map[string]any{{"type": "message", "content": []map[string]any{{"type": "output_text", "text": "42"}}}},
			"output_text": "42",
		})
	}))
	defer srv.Close()

	client, err := NewAPIClient(APIClientConfig{
		APIKey:     "test-key",
		BaseURL:    srv.URL + "/v1",
		Timeout:    5 * time.Second,
		MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	resp, err := client.Create(context.Background(), TurnRequest{
		Model: "gpt-5",
		Input: []ToolResult{{CallID: "call_1", Output: map[string]any{"sum": 42}}},
		Context: ContextStrategy{
			Mode: ContextModeStateless,
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !resp.Terminal {
		t.Fatal("expected terminal response")
	}
}

func TestNewAPIClient_RequiresAPIKey(t *testing.T) {
	if _, err := NewAPIClient(APIClientConfig{}); err == nil {
		t.Fatal("expected api key error")
	}
}

func TestAPIClientCreate_SendsCacheKeyAndParsesUsage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_u",
			"output_text": "done",
			"usage": map[string]any{
				"input_tokens":         1200,
				"output_tokens":        340,
				"input_tokens_details": map[string]any{"cached_tokens": 1024},
			},
		})
	}))
	defer srv.Close()

	client, err := NewAPIClient(APIClientConfig{APIKey: "test-key", BaseURL: srv.URL + "/v1", Timeout: 5 * time.Second, MaxRetries: 0})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	resp, err := client.Create(context.Background(), TurnRequest{
		Model:          "gpt-5.4",
		Input:          "hello",
		Context:        ContextStrategy{Mode: ContextModeStateless},
		PromptCacheKey: "loom-mills-council-editor",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if gotBody["prompt_cache_key"] != "loom-mills-council-editor" {
		t.Errorf("prompt_cache_key = %#v, want the routing key", gotBody["prompt_cache_key"])
	}
	if resp.PromptTokens != 1200 || resp.CompletionTokens != 340 {
		t.Errorf("tokens in=%d out=%d, want 1200/340 (usage was previously unparsed)", resp.PromptTokens, resp.CompletionTokens)
	}
	if resp.CachedTokens != 1024 {
		t.Errorf("cached tokens = %d, want 1024", resp.CachedTokens)
	}
}

func TestAPIClientCreate_OmitsCacheKeyWhenUnset(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "r", "output_text": "ok"})
	}))
	defer srv.Close()
	client, _ := NewAPIClient(APIClientConfig{APIKey: "k", BaseURL: srv.URL + "/v1", Timeout: 5 * time.Second, MaxRetries: 0})
	if _, err := client.Create(context.Background(), TurnRequest{Model: "gpt-5.4", Input: "x", Context: ContextStrategy{Mode: ContextModeStateless}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, present := gotBody["prompt_cache_key"]; present {
		t.Errorf("prompt_cache_key present with no key set; must be omitted")
	}
}

// The chat-completions-style usage shape (prompt_tokens / completion_tokens /
// prompt_tokens_details.cached_tokens) is coalesced too, so an OpenAI-compatible
// proxy returning the older shape still reports usage + cache hits.
func TestResponsesUsage_ChatCompletionsFallbackShape(t *testing.T) {
	u := &responsesAPIUsage{PromptTokens: 900, CompletionTokens: 12}
	u.PromptTokensDetails.CachedTokens = 512
	if u.prompt() != 900 || u.completion() != 12 || u.cached() != 512 {
		t.Errorf("coalesce = in:%d out:%d cached:%d, want 900/12/512", u.prompt(), u.completion(), u.cached())
	}
	var nilU *responsesAPIUsage
	if nilU.prompt() != 0 || nilU.completion() != 0 || nilU.cached() != 0 {
		t.Error("nil usage must coalesce to zeros")
	}
}

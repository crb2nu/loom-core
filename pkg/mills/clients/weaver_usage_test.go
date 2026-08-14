package clients

import (
	"context"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// weaverCachedBody is shaped like a real LiteLLM/vLLM chat-completions
// response for the research lane: a chat-dialect `prompt_tokens_details`
// block where cached_tokens is a SUBSET of prompt_tokens.
const weaverCachedBody = `{
  "model": "qwen3-8b-instruct",
  "choices": [
    {"message": {"role": "assistant", "content": "research notes body"}, "finish_reason": "stop"}
  ],
  "usage": {
    "prompt_tokens": 4096,
    "completion_tokens": 256,
    "total_tokens": 4352,
    "prompt_tokens_details": {"cached_tokens": 3968}
  }
}`

// weaverInputDetailsBody is the Responses-API dialect of the same thing. An
// OpenAI-compatible proxy in front of the research lane may return either, so
// both must land on the same normalized field.
const weaverInputDetailsBody = `{
  "model": "qwen3-8b-instruct",
  "choices": [{"message": {"role": "assistant", "content": "notes"}}],
  "usage": {
    "prompt_tokens": 2048,
    "completion_tokens": 64,
    "input_tokens_details": {"cached_tokens": 1024}
  }
}`

func TestWeaverClient_ResearchSurfacesCachedTokens(t *testing.T) {
	for _, tc := range []struct {
		name           string
		body           string
		wantPrompt     int
		wantCached     int
		wantCompletion int
	}{
		{"prompt_tokens_details", weaverCachedBody, 4096, 3968, 256},
		{"input_tokens_details", weaverInputDetailsBody, 2048, 1024, 64},
		// The pre-cache-era shape: no details block at all. Must parse
		// cleanly with a zero cached count rather than erroring — an engine
		// that predates the field is the common case on the local lane.
		{"no details block", successBody, 50, 0, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewWeaverClient(newStubClient(t, tc.body, 200))
			resp, err := w.Research(context.Background(), pipeline.WeaverRequest{
				BacklogID: "BL-USAGE",
				Prompt:    "research how X works",
			})
			if err != nil {
				t.Fatalf("research: %v", err)
			}
			if resp.Usage.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", resp.Usage.PromptTokens, tc.wantPrompt)
			}
			if resp.Usage.CachedPromptTokens != tc.wantCached {
				t.Errorf("CachedPromptTokens = %d, want %d", resp.Usage.CachedPromptTokens, tc.wantCached)
			}
			if resp.Usage.CompletionTokens != tc.wantCompletion {
				t.Errorf("CompletionTokens = %d, want %d", resp.Usage.CompletionTokens, tc.wantCompletion)
			}
			// The citation map is the pre-existing surface; it must keep
			// reporting the same two counts it always did.
			if resp.Citation["prompt_tokens"] != tc.wantPrompt {
				t.Errorf("citation prompt_tokens = %v, want %d", resp.Citation["prompt_tokens"], tc.wantPrompt)
			}
		})
	}
}

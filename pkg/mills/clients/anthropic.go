package clients

// anthropic.go -- a thin wrapper over the official Anthropic Go SDK
// (github.com/anthropics/anthropic-sdk-go) exposing the single-turn text
// completion the council editor needs. It mirrors pkg/openairesponses as the
// "frontier editor over a hosted API" backend, but for Claude models
// (claude-opus-4-8, claude-fable-5, claude-sonnet-5, ...) via the Messages API.
//
// The editor (council_anthropic.go) depends on the local anthropicMessenger
// interface, NOT the concrete client, so it stays unit-testable with a fake —
// the same decoupling council_openai.go uses for the Responses client.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/llmusage"
)

// Env vars for the Anthropic Messages API backend. The loom-scoped var wins so
// an operator can point Mills at a distinct key without disturbing anything
// else that reads the SDK-standard ANTHROPIC_API_KEY (which the SDK also reads
// on its own, and which the operator Deployment sets from the loom-mills-
// anthropic secret).
const (
	AnthropicAPIKeyEnvVar  = "LOOM_ANTHROPIC_API_KEY"
	AnthropicBaseURLEnvVar = "LOOM_ANTHROPIC_BASE_URL"
)

// anthropicDefaultMaxTokens bounds one editor completion. A plan decomposition
// (three markdown docs + the structured proposals block) fits comfortably; the
// call streams, so this large a ceiling never risks an HTTP timeout.
const anthropicDefaultMaxTokens int64 = 32000

// AnthropicAPIKeyFromEnv returns the configured Anthropic API key, preferring
// the loom-scoped var and falling back to the SDK-standard name. Empty when
// neither is set — callers treat that as "backend unavailable" and degrade to
// the local flexinfer editor rather than failing.
func AnthropicAPIKeyFromEnv() string {
	return env.StringWithFallbacks(AnthropicAPIKeyEnvVar, "ANTHROPIC_API_KEY")
}

// AnthropicBaseURLFromEnv returns an optional API base-URL override (e.g. a
// gateway/proxy). Empty ⇒ the SDK default (api.anthropic.com).
func AnthropicBaseURLFromEnv() string {
	return strings.TrimSpace(os.Getenv(AnthropicBaseURLEnvVar))
}

// anthropicMessageRequest is one stateless completion. System is the STABLE
// prefix (instructions + repo layout + pattern catalog) placed in a
// cache_control'd system block so repeated calls with the same prefix (e.g.
// several Spinning-Room spins on the same frame within the 5-minute cache TTL)
// read it at ~0.1x instead of paying full input price each time. Prompt is the
// VOLATILE user turn (this run's brief + reviewer notes). System is optional —
// empty ⇒ no system block, no caching.
type anthropicMessageRequest struct {
	Model     string
	System    string
	Prompt    string
	MaxTokens int64
	// DisableThinking sends thinking {type: disabled} instead of adaptive.
	// For small fixed completions (the rubric judge's score envelope) adaptive
	// thinking shares MaxTokens and can consume ALL of it on a complex diff,
	// returning an empty text body — observed live 2026-08-09 as "tiebreaker
	// unavailable ... unparseable response; raw=\"\"" on claude-sonnet-5.
	// Only set this for models that accept disabled thinking (Sonnet 5 does);
	// claude-fable-5 rejects it with a 400, so the council editor must never
	// set it.
	DisableThinking bool
}

// anthropicMessageResult is the minimal completion shape the editor consumes.
// It deliberately hides the SDK types so the editor's fake in tests doesn't
// need to construct SDK structs.
type anthropicMessageResult struct {
	Text         string
	InputTokens  int64
	OutputTokens int64
	// CacheCreationInputTokens / CacheReadInputTokens surface prompt-cache
	// activity (tokens written to vs read from the cache), so the editor can
	// record cache effectiveness in its sidecar notes — the "verify caching
	// with usage" check the Anthropic prompt-caching guidance recommends. A
	// persistent cache_read of 0 across repeated spins on the same frame means
	// a silent invalidator (or the prefix is below the model's min-cacheable
	// length) — visible instead of hidden.
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	// Refusal is true when a safety classifier declined the request
	// (stop_reason == "refusal"). The editor treats a refusal as an empty
	// run so the FallbackCouncilEditor falls back to flexinfer.
	Refusal bool
}

// anthropicMessenger is the surface the council editor depends on.
// *AnthropicClient satisfies it; tests inject a fake.
type anthropicMessenger interface {
	CreateMessage(ctx context.Context, req anthropicMessageRequest) (anthropicMessageResult, error)
}

// AnthropicClient wraps the SDK client for a single stateless text turn.
type AnthropicClient struct {
	api anthropic.Client
}

// AnthropicClientConfig configures NewAnthropicClient. Only APIKey is required.
type AnthropicClientConfig struct {
	APIKey  string
	BaseURL string        // optional override; empty ⇒ SDK default
	Timeout time.Duration // per-request wall-clock cap; 0 ⇒ SDK default (10m)
}

// NewAnthropicClient builds a client, or returns an error when no API key is
// configured (so the operator can log + degrade rather than panic).
func NewAnthropicClient(cfg AnthropicClientConfig) (*AnthropicClient, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, fmt.Errorf("anthropic client: api key required; set %s or ANTHROPIC_API_KEY", AnthropicAPIKeyEnvVar)
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if b := strings.TrimSpace(cfg.BaseURL); b != "" {
		opts = append(opts, option.WithBaseURL(b))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(cfg.Timeout))
	}
	return &AnthropicClient{api: anthropic.NewClient(opts...)}, nil
}

// CreateMessage runs one stateless user turn and returns the concatenated text
// output plus token usage. It STREAMS the response (accumulating into a single
// Message) so a large, minutes-long plan decomposition never hits the SDK's
// non-streaming HTTP timeout — the pattern the Claude API guidance recommends
// for high max_tokens.
//
// Adaptive thinking is requested: it is the supported on-mode for every current
// Claude model (Opus 4.8/4.7, Sonnet 5, Fable 5) and lets the model calibrate
// reasoning depth to the brief. budget_tokens / temperature are deliberately
// NOT set — they are rejected (400) on those models.
//
// Prompt caching: req.System (the stable prefix) goes in a system block marked
// cache_control:ephemeral (5-minute TTL), so repeated calls sharing that prefix
// read it at ~0.1x. Render order is tools → system → messages, and the thinking
// config is held constant, so nothing silently invalidates the system-cache
// prefix between calls. A prefix below the model's minimum cacheable length
// (e.g. 1024 tokens on Opus 4.8) simply isn't cached — no error.
func (c *AnthropicClient) CreateMessage(ctx context.Context, req anthropicMessageRequest) (anthropicMessageResult, error) {
	if c == nil {
		return anthropicMessageResult{}, errors.New("anthropic client: nil client")
	}
	if strings.TrimSpace(req.Model) == "" {
		return anthropicMessageResult{}, errors.New("anthropic client: model required")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	// Adaptive is the default; DisableThinking opts a request out so its whole
	// MaxTokens budget is text (rubric judge). Toggling thinking between
	// requests does not invalidate the tools/system cache tier — only the
	// messages tier, which these stateless single-turn calls never reuse.
	thinking := anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}
	if req.DisableThinking {
		disabled := anthropic.NewThinkingConfigDisabledParam()
		thinking = anthropic.ThinkingConfigParamUnion{OfDisabled: &disabled}
	}
	params := anthropic.MessageNewParams{
		Model:     req.Model,
		MaxTokens: maxTokens,
		Thinking:  thinking,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
	}
	if strings.TrimSpace(req.System) != "" {
		// One breakpoint on the last (only) system block caches the whole
		// stable prefix. Keep the text un-mutated so its bytes are identical
		// across calls — any change invalidates the prefix.
		params.System = []anthropic.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}

	stream := c.api.Messages.NewStreaming(ctx, params)
	msg := anthropic.Message{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return anthropicMessageResult{}, fmt.Errorf("anthropic client: accumulate: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return anthropicMessageResult{}, fmt.Errorf("anthropic client: stream: %w", err)
	}
	// Same observation point as the OpenAI-compatible clients (MR !1223): one
	// structured "llm usage" debug line plus the mills_llm_* counters. The
	// component label rides on ctx — the council editor and rubric judge each
	// tag their own via llmusage.WithComponent before calling here.
	millsUsageObserver.Observe(ctx, servedModel(string(msg.Model), req.Model), anthropicLLMUsage(msg.Usage))

	var sb strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return anthropicMessageResult{
		Text:                     sb.String(),
		InputTokens:              msg.Usage.InputTokens,
		OutputTokens:             msg.Usage.OutputTokens,
		CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		Refusal:                  string(msg.StopReason) == string(anthropic.StopReasonRefusal),
	}, nil
}

// anthropicLLMUsage maps the Messages API usage block onto the llmusage shape
// every OpenAI-compatible client in the repo reports through, so the Anthropic
// council backend shows up in the same `llm usage` log lines and mills_llm_*
// counters instead of being invisible there (previously these numbers reached
// only the editor's sidecar notes).
//
// The mapping keeps Anthropic's native semantics: input_tokens EXCLUDES the
// cache-read and cache-write tokens, whereas OpenAI's prompt_tokens INCLUDES
// its cached part. So for this backend cached_tokens is cache READS alongside
// the uncached remainder, not a subset of prompt_tokens — cached_share exceeds
// 1 on a warm prefix, which is the expected warm signature here, not a bug.
// docs/JOURNAL_ENGINE.md "Reading the cache data" carries the same caveat.
//
// CacheCreationInputTokens is deliberately NOT forwarded: it is a third
// quantity — tokens written to the cache this turn, billed at a premium — with
// no OpenAI-side equivalent, so a mills_llm_* counter for it would exist for
// exactly one backend and read as permanently zero everywhere else. It stays
// where it already lands: the editor's sidecar notes and the cost accounting
// in council_anthropic.go.
func anthropicLLMUsage(u anthropic.Usage) llmusage.Usage {
	return llmusage.Usage{
		PromptTokens:       int(u.InputTokens),
		CachedPromptTokens: int(u.CacheReadInputTokens),
		CompletionTokens:   int(u.OutputTokens),
	}
}

var _ anthropicMessenger = (*AnthropicClient)(nil)

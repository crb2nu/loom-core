package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/llmusage"
)

// APIClientConfig configures the OpenAI Responses HTTP client.
type APIClientConfig struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	MaxRetries int
	HTTPClient *httpclient.Client
	UserAgent  string
	// Logger, when non-nil, receives one llmusage line per completion. Nil
	// keeps this package silent, which is why it is opt-in: pkg/openairesponses
	// had no logger at all and a library that starts writing to slog.Default()
	// because a struct field was left unset is a surprise.
	Logger *slog.Logger
	// Component labels this client's traffic in logs. Defaults to
	// "openai-responses". A caller serving several roles from one client
	// overrides it per-request with llmusage.WithComponent.
	Component string
}

// APIClient implements ResponsesClient against the OpenAI Responses API.
type APIClient struct {
	baseURL   string
	apiKey    string
	http      *httpclient.Client
	userAgent string
	// usage observes each completion's token accounting. This client already
	// parsed cached_tokens (responsesAPIUsage.cached) and surfaced it on
	// TurnResponse; the observer is what makes it visible without a caller
	// having to write its own logging.
	usage llmusage.Observer
}

// NewAPIClient validates config and constructs an API-backed Responses client.
func NewAPIClient(cfg APIClientConfig) (*APIClient, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("responses api key is required; set %s or OPENAI_API_KEY", APIKeyEnvVar)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	httpc := cfg.HTTPClient
	if httpc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultRequestTimeout
		}
		maxRetries := cfg.MaxRetries
		if maxRetries < 0 {
			maxRetries = DefaultMaxRetries
		}
		httpc = httpclient.New(httpclient.Config{
			Timeout:        timeout,
			MaxRetries:     maxRetries,
			RetryBaseDelay: 200 * time.Millisecond,
			RetryMaxDelay:  2 * time.Second,
		})
	}

	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = "loom-responses/experimental"
	}

	component := strings.TrimSpace(cfg.Component)
	if component == "" {
		component = "openai-responses"
	}

	return &APIClient{
		baseURL:   baseURL,
		apiKey:    apiKey,
		http:      httpc,
		userAgent: userAgent,
		usage: llmusage.Observer{
			Logger:    cfg.Logger,
			Component: component,
		},
	}, nil
}

type responsesAPITool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict,omitempty"`
}

type responsesAPIToolOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type responsesAPIRequest struct {
	Model              string             `json:"model"`
	Input              any                `json:"input,omitempty"`
	Tools              []responsesAPITool `json:"tools,omitempty"`
	PreviousResponseID string             `json:"previous_response_id,omitempty"`
	Conversation       any                `json:"conversation,omitempty"`
	PromptCacheKey     string             `json:"prompt_cache_key,omitempty"`
}

type responsesAPIResponse struct {
	ID             string                   `json:"id"`
	ConversationID string                   `json:"conversation_id,omitempty"`
	Conversation   any                      `json:"conversation,omitempty"`
	OutputText     string                   `json:"output_text,omitempty"`
	Output         []responsesAPIOutputItem `json:"output,omitempty"`
	Usage          *responsesAPIUsage       `json:"usage,omitempty"`
}

// responsesAPIUsage parses the response usage block. The Responses API uses
// input_tokens / output_tokens / input_tokens_details.cached_tokens; the
// chat-completions-style prompt_tokens / completion_tokens /
// prompt_tokens_details.cached_tokens fields are parsed too and coalesced, so
// an OpenAI-compatible proxy that returns the older shape still reports usage.
type responsesAPIUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// prompt / completion / cached coalesce the two usage-field families.
func (u *responsesAPIUsage) prompt() int {
	if u == nil {
		return 0
	}
	if u.InputTokens > 0 {
		return u.InputTokens
	}
	return u.PromptTokens
}

func (u *responsesAPIUsage) completion() int {
	if u == nil {
		return 0
	}
	if u.OutputTokens > 0 {
		return u.OutputTokens
	}
	return u.CompletionTokens
}

func (u *responsesAPIUsage) cached() int {
	if u == nil {
		return 0
	}
	if u.InputTokensDetails.CachedTokens > 0 {
		return u.InputTokensDetails.CachedTokens
	}
	return u.PromptTokensDetails.CachedTokens
}

type responsesAPIOutputItem struct {
	Type      string                    `json:"type"`
	CallID    string                    `json:"call_id,omitempty"`
	Name      string                    `json:"name,omitempty"`
	Arguments string                    `json:"arguments,omitempty"`
	Content   []responsesAPIContentItem `json:"content,omitempty"`
	Text      string                    `json:"text,omitempty"`
	Refusal   string                    `json:"refusal,omitempty"`
}

type responsesAPIContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesAPIErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Create executes one non-stream Responses API request.
func (c *APIClient) Create(ctx context.Context, req TurnRequest) (TurnResponse, error) {
	if err := req.Validate(); err != nil {
		return TurnResponse{}, err
	}

	payload, err := buildAPIRequest(req)
	if err != nil {
		return TurnResponse{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return TurnResponse{}, fmt.Errorf("marshal responses request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return TurnResponse{}, fmt.Errorf("build responses request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return TurnResponse{}, fmt.Errorf("responses request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, truncated, err := httpclient.ReadBodyWithLimit(resp.Body, 2*1024*1024)
	if err != nil {
		return TurnResponse{}, fmt.Errorf("read responses body: %w", err)
	}
	if truncated {
		return TurnResponse{}, fmt.Errorf("responses body exceeded 2097152 bytes")
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return TurnResponse{}, decodeResponsesAPIError(resp.StatusCode, respBody)
	}

	var apiResp responsesAPIResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return TurnResponse{}, fmt.Errorf("decode responses body: %w", err)
	}
	turn, err := normalizeAPIResponse(apiResp, respBody)
	if err != nil {
		return turn, err
	}
	c.usage.Observe(ctx, req.Model, llmusage.Usage{
		PromptTokens:       turn.PromptTokens,
		CachedPromptTokens: turn.CachedTokens,
		CompletionTokens:   turn.CompletionTokens,
	})
	return turn, nil
}

func buildAPIRequest(req TurnRequest) (responsesAPIRequest, error) {
	payload := responsesAPIRequest{
		Model:          req.Model,
		Input:          req.Input,
		PromptCacheKey: strings.TrimSpace(req.PromptCacheKey),
	}
	if len(req.Tools) > 0 {
		payload.Tools = make([]responsesAPITool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			payload.Tools = append(payload.Tools, responsesAPITool{
				Type:        "function",
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
				Strict:      tool.Strict,
			})
		}
	}
	switch v := req.Input.(type) {
	case []ToolResult:
		input, err := encodeToolOutputs(v)
		if err != nil {
			return responsesAPIRequest{}, err
		}
		payload.Input = input
	}
	if id := strings.TrimSpace(req.Context.PreviousResponseID); id != "" {
		payload.PreviousResponseID = id
	}
	if id := strings.TrimSpace(req.Context.ConversationID); id != "" {
		payload.Conversation = id
	}
	return payload, nil
}

func encodeToolOutputs(results []ToolResult) ([]responsesAPIToolOutput, error) {
	outputs := make([]responsesAPIToolOutput, 0, len(results))
	for _, result := range results {
		callID := strings.TrimSpace(result.CallID)
		if callID == "" {
			return nil, fmt.Errorf("tool result missing call_id")
		}
		text, err := stringifyToolOutput(result)
		if err != nil {
			return nil, fmt.Errorf("encode tool output %q: %w", callID, err)
		}
		outputs = append(outputs, responsesAPIToolOutput{
			Type:   "function_call_output",
			CallID: callID,
			Output: text,
		})
	}
	return outputs, nil
}

func stringifyToolOutput(result ToolResult) (string, error) {
	if result.IsError && strings.TrimSpace(result.ErrorText) != "" {
		return result.ErrorText, nil
	}
	switch v := result.Output.(type) {
	case nil:
		return "{}", nil
	case string:
		return v, nil
	case json.RawMessage:
		return string(v), nil
	case []byte:
		return string(v), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func normalizeAPIResponse(apiResp responsesAPIResponse, raw json.RawMessage) (TurnResponse, error) {
	toolCalls := make([]ToolCall, 0, len(apiResp.Output))
	textParts := make([]string, 0, len(apiResp.Output)+1)
	if strings.TrimSpace(apiResp.OutputText) != "" {
		textParts = append(textParts, strings.TrimSpace(apiResp.OutputText))
	}

	for _, item := range apiResp.Output {
		switch item.Type {
		case "function_call":
			toolCalls = append(toolCalls, ToolCall{
				CallID:     strings.TrimSpace(item.CallID),
				ToolName:   strings.TrimSpace(item.Name),
				Arguments:  json.RawMessage(item.Arguments),
				RawPayload: mustJSON(item),
			})
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" || content.Type == "text" {
					if text := strings.TrimSpace(content.Text); text != "" {
						textParts = append(textParts, text)
					}
				}
			}
		case "output_text", "text":
			if text := strings.TrimSpace(item.Text); text != "" {
				textParts = append(textParts, text)
			}
		case "refusal":
			if text := strings.TrimSpace(item.Refusal); text != "" {
				textParts = append(textParts, text)
			}
		}
	}

	return TurnResponse{
		ResponseID:       strings.TrimSpace(apiResp.ID),
		ConversationID:   extractConversationID(apiResp),
		OutputText:       strings.Join(textParts, "\n"),
		ToolCalls:        toolCalls,
		Terminal:         len(toolCalls) == 0,
		RawPayload:       raw,
		PromptTokens:     apiResp.Usage.prompt(),
		CompletionTokens: apiResp.Usage.completion(),
		CachedTokens:     apiResp.Usage.cached(),
	}, nil
}

func extractConversationID(resp responsesAPIResponse) string {
	if id := strings.TrimSpace(resp.ConversationID); id != "" {
		return id
	}
	switch v := resp.Conversation.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if id, ok := v["id"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func decodeResponsesAPIError(status int, body []byte) error {
	var envelope responsesAPIErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && strings.TrimSpace(envelope.Error.Message) != "" {
		if kind := strings.TrimSpace(envelope.Error.Type); kind != "" {
			return fmt.Errorf("responses api HTTP %d (%s): %s", status, kind, envelope.Error.Message)
		}
		return fmt.Errorf("responses api HTTP %d: %s", status, envelope.Error.Message)
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("responses api HTTP %d: %s", status, msg)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

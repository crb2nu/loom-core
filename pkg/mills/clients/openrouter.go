package clients

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
	"os"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

// OpenRouterClientConfig configures the independent OpenRouter billing path.
type OpenRouterClientConfig struct {
	APIKey, BaseURL, HTTPReferer, Title string
	Timeout                             time.Duration
	Logger                              *slog.Logger
	Component                           string
}

type OpenRouterClient struct {
	config OpenRouterClientConfig
	http   *http.Client
}

// OpenRouterConfigFromEnv reads only OpenRouter credentials and routing settings.
func OpenRouterConfigFromEnv() OpenRouterClientConfig {
	return OpenRouterClientConfig{APIKey: strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")), BaseURL: strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL")), HTTPReferer: os.Getenv("OPENROUTER_HTTP_REFERER"), Title: os.Getenv("OPENROUTER_X_TITLE")}
}

func NewOpenRouterClient(c OpenRouterClientConfig) (*OpenRouterClient, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, errors.New("openrouter: API key required")
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://openrouter.ai/api/v1"
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("openrouter: invalid base URL")
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Minute
	}
	return &OpenRouterClient{config: c, http: &http.Client{Timeout: c.Timeout}}, nil
}

// Create adapts Chat Completions to the shared editor's normalized turn surface.
// No automatic retries: the caller's vendor breaker and fallback chain own failures.
func (c *OpenRouterClient) Create(ctx context.Context, in openairesponses.TurnRequest) (openairesponses.TurnResponse, error) {
	var out openairesponses.TurnResponse
	payload := struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
		Stream   bool             `json:"stream"`
	}{Model: in.Model, Messages: []map[string]any{{"role": "user", "content": in.Input}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.config.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if c.config.HTTPReferer != "" {
		req.Header.Set("HTTP-Referer", c.config.HTTPReferer)
	}
	if c.config.Title != "" {
		req.Header.Set("X-Title", c.config.Title)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return out, err
	}
	var wire struct {
		chatResponse
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &wire)
	if resp.StatusCode >= 300 || wire.Error != nil {
		apiErr := &openairesponses.APIError{Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-ID"), Message: http.StatusText(resp.StatusCode)}
		if wire.Error != nil {
			apiErr.Code = strings.Trim(string(wire.Error.Code), "\"")
			apiErr.Message = wire.Error.Message
			if resp.StatusCode < 300 {
				var status int
				if json.Unmarshal(wire.Error.Code, &status) == nil {
					apiErr.Status = status
				}
			}
		}
		return out, apiErr
	}
	if decodeErr != nil {
		return out, fmt.Errorf("openrouter: decode response: %w", decodeErr)
	}
	cached := llmusage.CachedTokens(wire.Usage.PromptTokensDetails, wire.Usage.InputTokensDetails)
	served := wire.Model
	if served == "" {
		served = in.Model
	}
	(llmusage.Observer{Logger: c.config.Logger, Component: c.config.Component}).Observe(ctx, served, wire.normalizedUsage())
	out.PromptTokens, out.CompletionTokens, out.CachedTokens = wire.Usage.PromptTokens, wire.Usage.CompletionTokens, max(cached, 0)
	if len(wire.Choices) > 0 {
		choice := wire.Choices[0]
		out.OutputText = choice.Message.Content
		if choice.Message.Refusal != "" || choice.FinishReason == "content_filter" {
			return out, &VendorError{Vendor: "openrouter", Kind: VendorRefusal, Err: errors.New("request refused")}
		}
	}
	return out, nil
}

// OpenRouterCouncilEditor shares all editor prompt, parser, and guardrail behavior.
type OpenRouterCouncilEditor OpenAIResponsesCouncilEditor

func (e *OpenRouterCouncilEditor) Edit(ctx context.Context, brief *council.Brief, reviews []council.ReviewerOutput) (*council.EditorOutput, error) {
	if e == nil {
		return nil, errors.New("openrouter council editor: client not configured")
	}
	shared := OpenAIResponsesCouncilEditor(*e)
	shared.Backend = "openrouter"
	return shared.editWithVendor(ctx, brief, reviews, "openrouter", openRouterResponseCostUSD)
}

func openRouterResponseCostUSD(model string, r openairesponses.TurnResponse) (float64, bool) {
	p, ok := lookupOpenRouterPrice(model)
	if !ok {
		return 0, false
	}
	prompt := max(r.PromptTokens, 0)
	cached := min(max(r.CachedTokens, 0), prompt)
	return (float64(prompt-cached)*p.InputPerMillion + float64(cached)*p.CachedInputPerMillion + float64(max(r.CompletionTokens, 0))*p.OutputPerMillion) / 1e6, true
}

type OpenRouterRubricJudge struct {
	Client     *OpenRouterClient
	Model      string
	Breaker    *VendorBreaker
	RubricBody func(string) string
}

func (j *OpenRouterRubricJudge) Judge(ctx context.Context, rubric string, in gates.StageInput) (gates.RubricVerdict, error) {
	if j == nil || j.Client == nil {
		return gates.RubricVerdict{}, errors.New("openrouter rubric judge: client not configured")
	}
	b := vendorBreaker(j.Breaker)
	if err := b.check("openrouter"); err != nil {
		return gates.RubricVerdict{}, err
	}
	body := j.RubricBody
	if body == nil {
		body = defaultRubricBody
	}
	resp, err := j.Client.Create(llmusage.WithComponent(ctx, ComponentJudge), openairesponses.TurnRequest{Model: j.Model, Input: composePrompt(body(rubric), in)})
	if err != nil {
		return gates.RubricVerdict{}, b.failure("openrouter", err)
	}
	b.Reset("openrouter")
	score, reasons, err := parseRubricEnvelope(resp.OutputText)
	return gates.RubricVerdict{Model: j.Model, Score: score, Reasons: reasons}, err
}

var _ council.Editor = (*OpenRouterCouncilEditor)(nil)
var _ gates.RubricJudge = (*OpenRouterRubricJudge)(nil)

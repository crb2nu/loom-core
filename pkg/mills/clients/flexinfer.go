// Package clients provides production implementations of the pipeline
// + gates Client interfaces declared in pkg/mills/pipeline and
// pkg/mills/gates. Each backing service is wrapped in a thin Go client
// that the operator constructs at startup; tests use the in-package
// fakes from the consuming packages.
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
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/agentloop"
	"github.com/crb2nu/loom/pkg/aimodels"
	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// FlexInferConfig captures the connection settings for a FlexInfer
// OpenAI-compatible HTTP proxy. The operator reads these from env at
// startup and constructs one shared client.
//
// FlexInfer's proxy exposes /v1/chat/completions, /v1/models, etc. and
// fans out to the active model registry. The operator's autonomy spec
// requires every LLM-judged gate use FlexInfer (never frontier); this
// client is the only LLM exit path the gates package consumes.
type FlexInferConfig struct {
	// ProxyURL is the base URL of the FlexInfer proxy; in-cluster this is
	// pkg/flexinfer.DefaultProxyURL. Trailing slash is tolerated.
	//
	// Deliberately has no default: the operator reads FLEXINFER_PROXY_URL
	// and leaves this empty when unset, which is what disables the
	// LLM-judged gates. Defaulting it to the in-cluster URL would arm
	// those gates against an unreachable host off-cluster.
	ProxyURL string
	// JudgeModel is the model id the proxy resolves for rubric calls.
	// Empty resolves through pkg/aimodels (RoleMillsJudge).
	JudgeModel string
	// JudgeModelFallbacks are ordered alternates tried when a chat call
	// against the preceding candidate 404s as model-not-found (model drift:
	// undeployed, renamed, or mid-rollout) OR 503-parks
	// ("service_unavailable" / "parked behind a higher-priority primary" on
	// the shared-GPU proxy). Resolution precedence when left empty:
	// FLEXINFER_JUDGE_MODEL_FALLBACKS (comma-separated env), then the
	// aimodels role chain. A PINNED JudgeModel now also gains this degrade
	// path — a parked 503 storm walks to an alternate instead of re-dialing
	// the same parked GPU 8× (research: 24×503 in 7d). Set this explicitly to
	// override; set the env to hand a pinned model a specific chain.
	JudgeModelFallbacks []string
	// WeaverModel may be larger (more grounded research). Empty
	// resolves through pkg/aimodels (RoleMillsResearch); if that is
	// also empty, falls back to JudgeModel.
	WeaverModel string
	// WeaverModelFallbacks mirrors JudgeModelFallbacks for the research
	// model chain (env override: FLEXINFER_WEAVER_MODEL_FALLBACKS).
	WeaverModelFallbacks []string
	// DisableRegistryFallbacks suppresses the aimodels-registry auto-
	// resolution the constructor otherwise applies to blank models and
	// empty fallback chains. The registry only knows FlexInfer-proxy GPU
	// model ids, so a client dialing the LiteLLM gateway (backend
	// "litellm", e.g. or/kimi-k3) must NOT inherit them: on a frontier
	// outage the fallback walk would 404 flexinfer ids against the
	// gateway and mis-classify a transient park as config drift. When
	// true, the primary model must be supplied explicitly and fallbacks
	// come ONLY from the FLEXINFER_*_MODEL_FALLBACKS env (interpreted as
	// gateway-routable ids) — a backend-local degrade chain. Left false
	// (the default) the FlexInfer-proxy behavior is byte-identical.
	DisableRegistryFallbacks bool
	// Token, when set, is sent as a Bearer auth header (the proxy
	// supports OAuth bearer in front of vLLM).
	Token string
	// Timeout caps any individual HTTP call. Default 5min. The Mills
	// research stage asks for up to 1024 tokens; a 26B model on a
	// warm GPU takes ~2min for that, and cold-start adds more. 30s
	// (the prior default) escalated every research call against a
	// healthy backend.
	Timeout time.Duration
}

// FlexInferClient is the shared HTTP client; both RubricJudge and
// WeaverClient use it. Callers usually construct one and reuse.
type FlexInferClient struct {
	cfg  FlexInferConfig
	http *httpclient.Client

	// mu guards the per-model unavailability memory below.
	mu sync.Mutex
	// modelUnavailableUntil records, per model id, the deadline until which
	// chatFallback skips re-dialing it because it recently returned a
	// 503-parked ("service_unavailable" / "parked behind a higher-priority
	// primary") rejection. Bounded by the candidate set; no background
	// goroutine — entries expire lazily on read. Prevents the observed
	// 503-parked storm where a single run hit one parked model 8×.
	modelUnavailableUntil map[string]time.Time
	// unavailableCooldown overrides the default parked-model cooldown. Zero
	// uses defaultModelUnavailableCooldown (60s). Set by tests.
	unavailableCooldown time.Duration
	// clock is the time source for cooldown math. Nil uses time.Now. Set by
	// tests so cooldown windows are deterministic without sleeps.
	clock func() time.Time
}

// defaultModelUnavailableCooldown is how long a model that returned a
// 503-parked rejection is skipped by chatFallback before being re-dialed.
// Short on purpose: the park clears when the higher-priority primary yields
// the GPU, usually within a minute.
const defaultModelUnavailableCooldown = 60 * time.Second

// NewFlexInferClient validates the config and returns a ready client.
// An empty ProxyURL is allowed only for tests via WithRoundTripper.
func NewFlexInferClient(cfg FlexInferConfig) (*FlexInferClient, error) {
	if cfg.ProxyURL == "" {
		return nil, errors.New("flexinfer: ProxyURL required")
	}
	// allowRegistry gates every aimodels-registry lookup below. The
	// registry only carries FlexInfer-proxy GPU model ids, so a LiteLLM-
	// gateway client (DisableRegistryFallbacks) opts out entirely and
	// relies on explicit models + env-listed, gateway-routable fallbacks.
	allowRegistry := !cfg.DisableRegistryFallbacks
	if cfg.JudgeModel == "" && allowRegistry {
		// Resolve the full role chain (primary + fallbacks) so a
		// model-not-found on the primary walks to the registry's
		// declared alternates instead of failing the stage (the
		// gemma4-26b-a4b-gptq 404 escalated #230's research stage while
		// its -5930k twin sat Ready). Last-resort default matches the
		// registry primary so the fallback never references an
		// undeployed model.
		chain := aimodels.DefaultResolver().ResolveWithFallbacks(aimodels.RoleMillsJudge)
		if len(chain) == 0 {
			chain = []string{"gemma4-26b-a4b-gptq"}
		}
		cfg.JudgeModel = chain[0]
		if len(cfg.JudgeModelFallbacks) == 0 {
			cfg.JudgeModelFallbacks = chain[1:]
		}
	}
	// A PINNED judge model (or a registry chain of length 1) still needs a
	// degrade path so a 503-parked / drifted primary walks to an alternate
	// instead of re-dialing the same unservable model. Env override first,
	// then (registry clients only) the role chain.
	if len(cfg.JudgeModelFallbacks) == 0 {
		cfg.JudgeModelFallbacks = degradeChain(cfg.JudgeModel, "FLEXINFER_JUDGE_MODEL_FALLBACKS", aimodels.RoleMillsJudge, allowRegistry)
	}
	if cfg.WeaverModel == "" && allowRegistry {
		chain := aimodels.DefaultResolver().ResolveWithFallbacks(aimodels.RoleMillsResearch)
		if len(chain) == 0 {
			chain = append([]string{cfg.JudgeModel}, cfg.JudgeModelFallbacks...)
		}
		cfg.WeaverModel = chain[0]
		if len(cfg.WeaverModelFallbacks) == 0 {
			cfg.WeaverModelFallbacks = chain[1:]
		}
	}
	if len(cfg.WeaverModelFallbacks) == 0 {
		cfg.WeaverModelFallbacks = degradeChain(cfg.WeaverModel, "FLEXINFER_WEAVER_MODEL_FALLBACKS", aimodels.RoleMillsResearch, allowRegistry)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	hcfg := httpclient.DefaultConfig()
	hcfg.Timeout = cfg.Timeout
	c := httpclient.New(hcfg)
	if cfg.Token != "" {
		c.SetHeader("Authorization", "Bearer "+cfg.Token)
	}
	return &FlexInferClient{cfg: cfg, http: c}, nil
}

// JudgeModel returns the resolved rubric-judge model id (the primary the
// RubricJudge dials). Exposed so the operator can log/assert the effective
// judge model regardless of which backend (FlexInfer proxy vs LiteLLM
// gateway) this client fronts.
func (c *FlexInferClient) JudgeModel() string {
	if c == nil {
		return ""
	}
	return c.cfg.JudgeModel
}

// WeaverModel returns the resolved research/weaver model id.
func (c *FlexInferClient) WeaverModel() string {
	if c == nil {
		return ""
	}
	return c.cfg.WeaverModel
}

// JudgeModelFallbacks returns a copy of the resolved ordered judge-model
// degrade chain (the alternates chatFallback walks on a 404/503-park). Exposed
// so the operator can surface the EFFECTIVE fallback list — post env override
// (FLEXINFER_JUDGE_MODEL_FALLBACKS) and post aimodels-registry resolution — in
// the /wiring snapshot. Returns nil for a nil receiver; the copy prevents a
// caller mutating the client's internal chain.
func (c *FlexInferClient) JudgeModelFallbacks() []string {
	if c == nil || len(c.cfg.JudgeModelFallbacks) == 0 {
		return nil
	}
	return append([]string(nil), c.cfg.JudgeModelFallbacks...)
}

// WeaverModelFallbacks mirrors JudgeModelFallbacks for the research/weaver
// model degrade chain (env override FLEXINFER_WEAVER_MODEL_FALLBACKS).
func (c *FlexInferClient) WeaverModelFallbacks() []string {
	if c == nil || len(c.cfg.WeaverModelFallbacks) == 0 {
		return nil
	}
	return append([]string(nil), c.cfg.WeaverModelFallbacks...)
}

// RegistryFallbacksDisabled reports whether this client suppresses the
// aimodels-registry auto-resolution (true for a LiteLLM-gateway client, whose
// fallbacks come ONLY from the FLEXINFER_*_MODEL_FALLBACKS env as gateway-
// routable ids). Exposed for the /wiring snapshot so an operator can see the
// judge is on a backend-local degrade chain, not the FlexInfer role chain.
func (c *FlexInferClient) RegistryFallbacksDisabled() bool {
	if c == nil {
		return false
	}
	return c.cfg.DisableRegistryFallbacks
}

// degradeChain resolves the ordered fallback models a PINNED primary (one the
// registry didn't resolve, so it has no automatic role chain) walks to when it
// 404s or 503-parks. Precedence: the FLEXINFER_*_MODEL_FALLBACKS env override
// (comma-separated), then — only when allowRegistry is true — the aimodels role
// chain. The primary itself is filtered out so it is never re-dialed as its own
// fallback. A LiteLLM-gateway client passes allowRegistry=false so it never
// inherits FlexInfer-proxy model ids the gateway can't route (backend-local
// degrade: env-listed litellm ids or nothing).
func degradeChain(primary, envKey string, role aimodels.Role, allowRegistry bool) []string {
	src := splitModelList(os.Getenv(envKey))
	if len(src) == 0 && allowRegistry {
		src = aimodels.DefaultResolver().ResolveWithFallbacks(role)
	}
	return dedupeModels(src, primary)
}

// splitModelList parses a comma-separated model list, trimming blanks.
func splitModelList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// dedupeModels returns models with blanks, the exclude id, and duplicates
// removed, preserving order.
func dedupeModels(models []string, exclude string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" || m == exclude {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// chatRequest mirrors the OpenAI-compatible request body. We intentionally
// keep the surface narrow: messages, sampling bounds, and the one template
// control needed by structured judges. No streaming or tools.
type chatRequest struct {
	Model              string          `json:"model"`
	Messages           []chatMessage   `json:"messages"`
	Temperature        float64         `json:"temperature"`
	MaxTokens          int             `json:"max_tokens,omitempty"`
	ChatTemplateKwargs map[string]bool `json:"chat_template_kwargs,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ReasoningContent / Reasoning carry a thinking model's chain-of-thought
	// when the gateway surfaces it in a SEPARATE message field rather than in
	// Content. LiteLLM→OpenRouter returns kimi-k3's reasoning here
	// (`reasoning_content`); other gateways use `reasoning`. The rubric judge
	// never grades these (the score envelope lives in Content), but their
	// presence proves a reasoning model ran, which the empty-content guard uses
	// to size its recovery retry. omitempty keeps request bodies byte-identical
	// (chatMessage is marshaled for the outbound `messages` array too).
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Reasoning        string `json:"reasoning,omitempty"`
}

// chatResponse covers only the fields we read.
type chatResponse struct {
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// Cost is the provider-reported USD cost for this call. The
		// FlexInfer proxy omits it (local GPU inference); LiteLLM/OpenRouter
		// report the real upstream charge here, which beats the flat
		// local-tier estimate below by orders of magnitude for frontier
		// models like kimi-k3.
		Cost float64 `json:"cost"`
		// CompletionTokensDetails.ReasoningTokens is the portion of the
		// completion budget a thinking model spent on chain-of-thought. For
		// kimi-k3 via LiteLLM this counts AGAINST max_tokens, so when it
		// approaches the ceiling the score envelope is squeezed out of Content
		// entirely (live capture 2026-07-17: reasoning_tokens=1021/1024 →
		// content=NULL, finish_reason=length). Used to size the judge's
		// empty-content recovery retry.
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		// PromptTokensDetails.CachedTokens / InputTokensDetails.CachedTokens
		// are the share of PromptTokens the engine served from its prefix
		// cache. Read-only: mills makes no decision on it yet. It is here so
		// the question "would journalengine's cache contract pay off on mills
		// traffic" can be answered from data instead of from the psyche
		// numbers, which were measured on a different prompt shape.
		PromptTokensDetails llmusage.Details `json:"prompt_tokens_details"`
		InputTokensDetails  llmusage.Details `json:"input_tokens_details"`
	} `json:"usage"`
}

// normalizedUsage converts the wire usage into the shape pkg/llmusage reports.
func (r *chatResponse) normalizedUsage() llmusage.Usage {
	if r == nil {
		return llmusage.Usage{}
	}
	return llmusage.Usage{
		PromptTokens:       r.Usage.PromptTokens,
		CachedPromptTokens: llmusage.CachedTokens(r.Usage.PromptTokensDetails, r.Usage.InputTokensDetails),
		CompletionTokens:   r.Usage.CompletionTokens,
	}
}

// chatChoice is one OpenAI-compatible completion choice. FinishReason
// carries the backend's stop reason ("stop", "length", …) when present;
// "length" means the completion hit the token ceiling and its JSON
// envelope is probably truncated (the RubricJudge uses this to trigger a
// bounded larger-budget retry).
type chatChoice struct {
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatOptions struct {
	disableThinking bool
	// cacheKey pins the proxy's prefix-cache routing for THIS call
	// (X-Flexinfer-Cache-Key). It must be per-request, never a client-global
	// SetHeader: one FlexInferClient is shared by every lane (judge, weaver,
	// council), so a global key would route unrelated prompts to the same
	// replica and defeat the point. Empty sends no header, which is the
	// pre-existing behavior for every caller that has no owner id.
	cacheKey string
}

// cacheKeyItemPrefix namespaces a routing key by backlog item. The proxy keys
// replica affinity on the raw header value, so the prefix keeps Mills item
// journals from colliding with other producers (agentloop sessions use
// "session:<id>").
const cacheKeyItemPrefix = "mills-item:"

// itemCacheKey builds the per-item routing key the research lane pins. Returns
// "" for a blank id so the header is simply absent rather than a meaningless
// "mills-item:" that would collide across every id-less call.
func itemCacheKey(backlogID string) string {
	backlogID = strings.TrimSpace(backlogID)
	if backlogID == "" {
		return ""
	}
	return cacheKeyItemPrefix + backlogID
}

func (c *FlexInferClient) chatWithOptions(ctx context.Context, model, prompt string, maxTokens int, opts chatOptions) (string, *chatResponse, error) {
	if c == nil {
		return "", nil, errors.New("flexinfer: client nil")
	}
	req := chatRequest{
		Model:       model,
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   maxTokens,
	}
	if opts.disableThinking && isLocalFlexInferModel(model) {
		req.ChatTemplateKwargs = map[string]bool{"enable_thinking": false}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}
	url := strings.TrimRight(c.cfg.ProxyURL, "/") + "/v1/chat/completions"
	// Built here rather than via c.http.Post so opts.cacheKey can ride as a
	// per-request header. http.NewRequestWithContext over a *bytes.Reader
	// populates GetBody, so the shared client's retry path still replays the
	// body exactly as it did through Post.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, fmt.Errorf("flexinfer chat: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(opts.cacheKey); key != "" {
		httpReq.Header.Set(agentloop.HeaderCacheKey, key)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", nil, fmt.Errorf("flexinfer chat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(buf))
		lower := strings.ToLower(msg)
		cerr := fmt.Errorf("flexinfer chat: status %d: %s", resp.StatusCode, msg)
		// A 404 whose body names the model is the proxy's
		// model-not-found rejection — the one failure chatFallback can
		// fix by walking to the next candidate.
		if resp.StatusCode == http.StatusNotFound && strings.Contains(lower, "model") {
			cerr = fmt.Errorf("%w (%w)", cerr, errModelNotFound)
		}
		// LiteLLM/OpenRouter can successfully resolve a model group, then
		// select a provider whose route does not implement chat completions.
		// This is candidate-specific, so a configured alternate can serve the
		// request. Keep the match deliberately narrower than a generic 400:
		// both the provider reason and the endpoint-incompatibility message
		// from the nested OpenRouter error must be present.
		if resp.StatusCode == http.StatusBadRequest &&
			strings.Contains(lower, "invalid_request_body") &&
			strings.Contains(lower, "does not support endpoint: completions") {
			cerr = fmt.Errorf("%w (%w)", cerr, errModelRouteIncompatible)
		} else if resp.StatusCode == http.StatusBadRequest {
			// Any OTHER 400 is still candidate-specific: LiteLLM surfaces a
			// provider's rejection (litellm.BadRequestError from OpenRouter,
			// "Available Model Group Fallbacks=None") as a 400 whose body names
			// only the model it happened to route to. It says nothing about the
			// next configured candidate, so chatFallback walks on instead of
			// failing the whole judge call — issue #378 parked a run on exactly
			// this shape. If every candidate rejects the request the chain
			// returns the last body, so a genuinely malformed request still
			// surfaces its real error.
			cerr = fmt.Errorf("%w (%w)", cerr, errModelBadRequest)
		}
		// A 503 service_unavailable — including the shared-GPU proxy's
		// "model '…' is parked behind a higher-priority primary" rejection —
		// means THIS model is temporarily unservable, not gone. chatFallback
		// walks to the next candidate and parks this one in a short cooldown
		// so it isn't re-dialed every attempt (live: research 24×503 in 7d,
		// one run 8× against a single parked model). Wraps the pipeline
		// sentinel so the research stage can soft-skip and Classify maps it to
		// transient.
		// A 429 joins them: a provider rate limit is this candidate being
		// temporarily unservable, so the chain walks to the next model and the
		// rate-limited one is parked in the same cooldown rather than re-dialed
		// on every re-judge. Before this a 429 matched none of the fallback
		// signatures and returned straight out of chatFallback with the chain
		// untouched (issue #378).
		//
		// The remaining upstream 5xx (500/502/504) join them too. A raw
		// `status 500: Internal Server Error` is what the LiteLLM/flexinfer
		// proxies emit when the provider they routed THIS model to blows up
		// mid-request — same "this candidate is temporarily unservable" shape as
		// a 503, just without the structured body. Treated as a hard failure it
		// returned straight out of chatFallback with the chain untouched and
		// failed the stage; the 2026-07-26 audit found 3 research escalations
		// whose whole log_tail was
		// `stage=research attempt=1: flexinfer chat: status 500: Internal Server
		// Error`, none of them code defects.
		if isTransientUpstreamStatus(resp.StatusCode) ||
			resp.StatusCode == http.StatusTooManyRequests ||
			strings.Contains(lower, "service_unavailable") ||
			strings.Contains(lower, "parked behind") {
			cerr = fmt.Errorf("%w (%w)", cerr, pipeline.ErrModelUnavailable)
		}
		return "", nil, cerr
	}
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", nil, fmt.Errorf("flexinfer chat decode: %w", err)
	}
	// Observed before the no-choices guard below: an empty-choices response
	// still consumed a prompt and still reports its cached share, and the
	// reasoning-recovery retry path deliberately produces these. Skipping them
	// would bias the measurement toward well-behaved calls.
	millsUsageObserver.Observe(ctx, servedModel(parsed.Model, model), parsed.normalizedUsage())
	if len(parsed.Choices) == 0 {
		return "", &parsed, errors.New("flexinfer chat: no choices in response")
	}
	return parsed.Choices[0].Message.Content, &parsed, nil
}

// servedModel prefers the model the engine reports having served over the one
// requested. A fallback chain or a LiteLLM group alias can substitute one, and
// the prefix cache lives with the engine that actually served the prompt, so
// attributing a hit rate to the requested name would be misleading.
func servedModel(reported, requested string) string {
	if strings.TrimSpace(reported) != "" {
		return reported
	}
	return requested
}

// isTransientUpstreamStatus reports whether an HTTP status from the proxy is a
// transient upstream failure attributable to the ONE model the proxy routed to,
// rather than a terminal rejection of the request itself. 503 is the
// shared-GPU park; 500/502/504 are the shapes the LiteLLM gateway and the
// flexinfer proxy emit when the selected provider/engine errors, is unreachable,
// or times out. All four mean "try the next candidate, park this one" — the
// alternate frequently routes to a different provider or GPU and answers.
//
// Deliberately NOT >= 500: a 501 Not Implemented is a capability statement about
// the endpoint, not a per-model blip, and the next candidate hits it identically.
// 4xx stays terminal here (429 has its own quota semantics; the 400/404 shapes
// are handled by their own sentinels above).
func isTransientUpstreamStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, // 500
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	default:
		return false
	}
}

// errModelNotFound marks a chat failure as the proxy's model-not-found
// rejection so chatFallback can distinguish "this model is gone" (walk
// the fallback chain) from every other failure (return immediately).
var errModelNotFound = errors.New("model not found")

// errModelRouteIncompatible marks a provider-specific 400 where the resolved
// model cannot serve the chat-completions endpoint. Unlike other bad requests,
// chatFallback may fix this by trying the next configured model.
var errModelRouteIncompatible = errors.New("model route incompatible")

// errModelBadRequest marks a generic (non-route-incompatible) 400 from the
// proxy. The rejection is attributed to the model LiteLLM routed to, so
// chatFallback tries the next candidate before giving up; the last body is
// preserved when every candidate rejects the request.
var errModelBadRequest = errors.New("model rejected the request")

// fallbacksFor returns the configured fallback chain for a primary
// model. Ad-hoc model ids (Chat callers passing their own) get none.
func (c *FlexInferClient) fallbacksFor(model string) []string {
	switch model {
	case c.cfg.JudgeModel:
		return c.cfg.JudgeModelFallbacks
	case c.cfg.WeaverModel:
		return c.cfg.WeaverModelFallbacks
	default:
		return nil
	}
}

// chatFallback is chat plus model-drift, provider-route, rate-limit, and
// model-unavailability resilience: when the requested model 404s as
// model-not-found, returns any 400 (the narrow route-incompatibility signature
// or a generic provider rejection), 429s, OR returns a transient upstream 5xx
// (500/502/503/504 — including the "service_unavailable" / "parked behind a
// higher-priority primary" park), it walks the model's configured fallback
// chain in order (the aimodels registry declares e.g. the -5930k twin GPU
// variant, then the "fast-text" alias) and returns the first candidate that
// answers. Any OTHER failure (transport, decode, 501, auth) returns immediately
// — those are outages the next candidate hits identically. Every status the
// chain walks is attributed by the proxy to the ONE model it routed to, which is
// what makes the next candidate worth trying.
//
// A parked model is also parked in a short in-client cooldown so the whole
// run stops re-dialing it (live: one research run hit a single parked model
// 8×). When EVERY candidate is unavailable the error wraps
// pipeline.ErrModelUnavailable (transient/retryable, research soft-skips it);
// when every candidate is gone the error is "no candidate model deployed"
// wrapping model-not-found, which classifies as code (real config drift →
// escalate + human).
//
// Live triggers: escalation #230 (2026-07-01) — research burned 3 attempts on
// `status 404: Model 'gemma4-26b-a4b-gptq' not found` while the -5930k twin
// sat Ready; the 2026-07-16 telemetry sweep — 24 research 503s in 7d, all
// `status 503 … model parked behind higher-priority primary`, with a pinned
// FLEXINFER_WEAVER_MODEL that carried no fallbacks; and the 2026-07-26 audit —
// 3 research escalations whose entire log_tail was `stage=research attempt=1:
// flexinfer chat: status 500: Internal Server Error`, which the chain skipped
// entirely because a raw 5xx was classed as an un-degradable outage.
func (c *FlexInferClient) chatFallback(ctx context.Context, model, prompt string, maxTokens int) (string, *chatResponse, error) {
	return c.chatFallbackWithOptions(ctx, model, prompt, maxTokens, chatOptions{})
}

func (c *FlexInferClient) structuredChatFallback(ctx context.Context, model, prompt string, maxTokens int) (string, *chatResponse, error) {
	return c.chatFallbackWithOptions(ctx, model, prompt, maxTokens, chatOptions{disableThinking: true})
}

func (c *FlexInferClient) chatFallbackWithOptions(ctx context.Context, model, prompt string, maxTokens int, opts chatOptions) (string, *chatResponse, error) {
	if c == nil {
		return "", nil, errors.New("flexinfer: client nil")
	}
	candidates := make([]string, 0, 1+len(c.fallbacksFor(model)))
	candidates = append(candidates, model)
	for _, m := range c.fallbacksFor(model) {
		if m != "" && !slices.Contains(candidates, m) {
			candidates = append(candidates, m)
		}
	}
	var (
		lastErr              error
		sawUnavailable       bool
		sawRouteIncompatible bool
		sawBadRequest        bool
	)
	for i, m := range candidates {
		// A model still inside its 503-parked cooldown is skipped, not
		// re-dialed — the whole point of the cooldown is to stop the storm.
		if c.modelInCooldown(m) {
			lastErr = fmt.Errorf("flexinfer chat: model %q parked (unavailable cooldown): %w", m, pipeline.ErrModelUnavailable)
			sawUnavailable = true
			slog.Default().Warn("flexinfer: skipping model in unavailable cooldown; walking fallback chain",
				"model", m, "remaining", len(candidates)-i-1)
			continue
		}
		content, resp, err := c.chatWithOptions(ctx, m, prompt, maxTokens, opts)
		if err == nil {
			if i > 0 {
				slog.Default().Warn("flexinfer: primary model degraded; served by fallback",
					"primary", model, "used", m)
			}
			return content, resp, nil
		}
		switch {
		case errors.Is(err, pipeline.ErrModelUnavailable):
			// Parked (503) / rate-limited (429) / transient upstream 5xx:
			// park this model so it isn't re-dialed, then walk on.
			c.markModelUnavailable(m)
			sawUnavailable = true
			lastErr = err
			slog.Default().Warn("flexinfer: model unavailable (parked/5xx); walking fallback chain",
				"model", m, "remaining", len(candidates)-i-1)
		case errors.Is(err, errModelNotFound):
			lastErr = err
			slog.Default().Warn("flexinfer: model not found; walking fallback chain",
				"model", m, "remaining", len(candidates)-i-1)
		case errors.Is(err, errModelRouteIncompatible):
			lastErr = err
			sawRouteIncompatible = true
			slog.Default().Warn("flexinfer: model route incompatible; walking fallback chain",
				"model", m, "remaining", len(candidates)-i-1)
		case errors.Is(err, errModelBadRequest):
			lastErr = err
			sawBadRequest = true
			slog.Default().Warn("flexinfer: model rejected the request (400); walking fallback chain",
				"model", m, "remaining", len(candidates)-i-1)
		default:
			// A real outage/transport error is not fixable by walking the
			// chain — surface it immediately.
			return content, resp, err
		}
	}
	if sawUnavailable {
		// Every candidate was parked/unavailable. Wrap the model-unavailable
		// sentinel so the research stage soft-skips and Classify maps this to
		// a transient (retryable) escalation instead of code-class. Carry the
		// last proxy error too so skip notes and escalation reasons show the
		// real 503 body, not just the sentinel.
		return "", nil, fmt.Errorf("flexinfer chat: all candidate models unavailable (tried %v): %w (last: %v)", candidates, pipeline.ErrModelUnavailable, lastErr)
	}
	if sawRouteIncompatible {
		return "", nil, fmt.Errorf("flexinfer chat: no compatible candidate model route (tried %v): %w", candidates, lastErr)
	}
	if sawBadRequest {
		// Every candidate rejected the request. The last provider body is
		// wrapped verbatim so the escalation names the real 400 (which model,
		// which provider, which reason) instead of a generic chain summary.
		return "", nil, fmt.Errorf("flexinfer chat: every candidate model rejected the request (tried %v): %w", candidates, lastErr)
	}
	return "", nil, fmt.Errorf("flexinfer chat: no candidate model deployed (tried %v): %w", candidates, lastErr)
}

// markModelUnavailable records that a model returned a temporarily-unservable
// rejection (503 park, 429 rate limit, or a transient upstream 5xx) so
// chatFallback skips re-dialing it until the cooldown elapses.
func (c *FlexInferClient) markModelUnavailable(model string) {
	if c == nil || model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.modelUnavailableUntil == nil {
		c.modelUnavailableUntil = make(map[string]time.Time)
	}
	c.modelUnavailableUntil[model] = c.now().Add(c.cooldown())
}

// modelInCooldown reports whether model is inside its 503-parked cooldown.
// Expired entries are cleared lazily so a recovered model is re-dialed on the
// next attempt without any background sweeper.
func (c *FlexInferClient) modelInCooldown(model string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.modelUnavailableUntil[model]
	if !ok {
		return false
	}
	if !c.now().Before(until) {
		delete(c.modelUnavailableUntil, model)
		return false
	}
	return true
}

func (c *FlexInferClient) cooldown() time.Duration {
	if c.unavailableCooldown > 0 {
		return c.unavailableCooldown
	}
	return defaultModelUnavailableCooldown
}

func (c *FlexInferClient) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

// ----- RubricJudge -----

// RubricJudge satisfies gates.RubricJudge against the FlexInfer proxy.
type RubricJudge struct {
	Client      *FlexInferClient
	MaxTokens   int // default 1024 (env FLEXINFER_JUDGE_MAX_TOKENS)
	RubricBody  func(rubric string) string
	Temperature float64 // default 0
}

// defaultJudgeMaxTokens is the rubric-judge completion budget. The prior
// 256 default truncated the {"score":…,"reasons":[…]} envelope whenever
// the pinned qwen35 judge emitted a "Thinking Process:" preamble or a few
// verbose reasons, so parseRubricEnvelope saw no complete JSON and the
// gate false-failed (all 12 inspected judge-gate failures in the 7d
// window ending 2026-07-16). 1024 clears the preamble + a full envelope.
const defaultJudgeMaxTokens = 1024

// judgeMaxTokensFromEnv resolves the rubric-judge completion budget,
// honoring FLEXINFER_JUDGE_MAX_TOKENS when it parses to a positive int
// (mirrors the constructor-time env reads elsewhere in this file, e.g.
// NewWeaverClient reading MILLS_RESEARCH_VIA_WEAVER).
func judgeMaxTokensFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_JUDGE_MAX_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultJudgeMaxTokens
}

// JudgeMaxTokensFromEnv exposes the resolved rubric-judge completion budget
// (defaultJudgeMaxTokens, or a positive FLEXINFER_JUDGE_MAX_TOKENS override) so
// the operator can report the effective judge max_tokens in the /wiring
// snapshot without re-implementing the env read.
func JudgeMaxTokensFromEnv() int {
	return judgeMaxTokensFromEnv()
}

// defaultWeaverMaxTokens is the research/weaver completion budget. It matches
// the judge default (1024), which is right for the local qwen research model
// (non-reasoning: the whole budget is notes). A reasoning model — kimi-k3 via
// the LiteLLM gateway — instead returns its chain-of-thought in a SEPARATE
// message.reasoning_content field whose tokens count AGAINST this budget, so at
// 1024 the reasoning frequently consumes the whole completion and the research
// notes (message.content) come back EMPTY. Configure FLEXINFER_WEAVER_MAX_TOKENS
// >= 4096 for any reasoning weaver model (see weaverMaxTokensFromEnv).
const defaultWeaverMaxTokens = 1024

// weaverMaxTokensFromEnv resolves the research/weaver completion budget,
// honoring FLEXINFER_WEAVER_MAX_TOKENS when it parses to a positive int. Mirrors
// judgeMaxTokensFromEnv / FLEXINFER_JUDGE_MAX_TOKENS exactly. The default (1024)
// keeps the local qwen research model byte-identical; a reasoning weaver model
// needs >= 4096 (gitops sets FLEXINFER_WEAVER_MAX_TOKENS=4096 for or/kimi-k3) so
// the chain-of-thought completes AND leaves room for the notes.
func weaverMaxTokensFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("FLEXINFER_WEAVER_MAX_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultWeaverMaxTokens
}

// WeaverMaxTokensFromEnv exposes the resolved research/weaver completion budget
// (defaultWeaverMaxTokens, or a positive FLEXINFER_WEAVER_MAX_TOKENS override) so
// the operator can report the effective weaver max_tokens in the /wiring
// snapshot without re-implementing the env read.
func WeaverMaxTokensFromEnv() int {
	return weaverMaxTokensFromEnv()
}

// NewRubricJudge wires a FlexInfer-backed judge using the canonical
// rubric prompts shipped in pkg/mills/gates.
func NewRubricJudge(c *FlexInferClient) *RubricJudge {
	return &RubricJudge{
		Client:     c,
		MaxTokens:  judgeMaxTokensFromEnv(),
		RubricBody: defaultRubricBody,
	}
}

// Judge implements gates.RubricJudge. It composes the rubric prompt
// with the StageInput-derived context, calls the proxy, and parses the
// JSON envelope from the response.
func (j *RubricJudge) Judge(ctx context.Context, rubric string, in gates.StageInput) (gates.RubricVerdict, error) {
	if j == nil || j.Client == nil {
		return gates.RubricVerdict{}, errors.New("rubric judge: client not configured")
	}
	// Attribute this call's token accounting to the judge rather than to the
	// shared FlexInfer client. Instrumentation only — nothing reads it back.
	ctx = llmusage.WithComponent(ctx, ComponentJudge)
	body := j.RubricBody
	if body == nil {
		body = defaultRubricBody
	}
	prompt := composePrompt(body(rubric), in)
	maxTokens := j.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultJudgeMaxTokens
	}
	content, resp, err := j.Client.structuredChatFallback(ctx, j.Client.cfg.JudgeModel, prompt, maxTokens)
	if err != nil {
		return gates.RubricVerdict{}, err
	}
	score, reasons, perr := parseRubricEnvelope(content)
	if perr == nil {
		return gates.RubricVerdict{
			Score:   score,
			Reasons: reasons,
			Model:   modelFrom(resp, j.Client.cfg.JudgeModel),
		}, nil
	}
	// The first response could not be graded even after truncation-tolerant
	// recovery. Retry exactly ONCE with a larger budget and a hard JSON-only
	// instruction when the miss looks budget-driven — the backend reported the
	// completion was cut at the token ceiling (finish_reason=length), OR the
	// content came back empty. An empty completion is the reasoning-model
	// squeeze: kimi-k3's chain-of-thought (returned in message.reasoning_content
	// and counted against max_tokens) consumed the whole budget before the score
	// envelope was emitted (live capture 2026-07-17: reasoning_tokens=1021/1024,
	// content=NULL). A gate must NEVER false-fail on raw="" without at least one
	// boosted retry. Bounded to a single retry.
	if judgeShouldBoostRetry(resp, content) {
		retryPrompt := prompt + "\n\n" + jsonOnlyRetryInstruction
		retryTokens := boostedRetryTokens(maxTokens, resp)
		if rc, rresp, rerr := j.Client.structuredChatFallback(ctx, j.Client.cfg.JudgeModel, retryPrompt, retryTokens); rerr == nil {
			if rscore, rreasons, rperr := parseRubricEnvelope(rc); rperr == nil {
				return gates.RubricVerdict{
					Score:   rscore,
					Reasons: rreasons,
					Model:   modelFrom(rresp, j.Client.cfg.JudgeModel),
				}, nil
			}
			// The boosted retry still couldn't be graded; carry its response so
			// the diagnostic reflects the final attempt.
			resp, content = rresp, rc
		}
	}
	return gates.RubricVerdict{Model: modelFrom(resp, j.Client.cfg.JudgeModel)},
		fmt.Errorf("rubric judge: parse: %w%s; raw=%q", perr, judgeEmptyDiagnostic(resp, content), content)
}

// judgeShouldBoostRetry reports whether an unparseable first response warrants
// the single larger-budget retry. Two budget-driven misses qualify: a
// finish_reason=length truncation (the original trigger), and an empty
// completion (raw="") — the reasoning-model squeeze where a thinking model
// (kimi-k3) spent the whole completion budget on chain-of-thought before
// emitting the envelope. A non-empty, non-truncated miss is a genuine judge
// error (free-text refusal, out-of-range score) and is surfaced without a
// wasted retry.
func judgeShouldBoostRetry(resp *chatResponse, content string) bool {
	if responseTruncatedByLength(resp) {
		return true
	}
	return strings.TrimSpace(content) == ""
}

// judgeReasoningRetryFloorTokens floors the empty-content recovery retry for
// reasoning models. Doubling a small budget (1024→2048) may not clear a
// thinking model whose reasoning alone runs ~650–1200+ tokens, so when the
// squeezed response shows reasoning activity the retry is floored here — ample
// headroom for the full chain-of-thought PLUS the score envelope (live capture:
// worst-observed completion 1227 tokens at max effort). The durable fix is the
// FLEXINFER_JUDGE_MAX_TOKENS config (recommend 4096 for kimi-k3); this is the
// client-side safety net for a misconfigured budget.
const judgeReasoningRetryFloorTokens = 4096

// boostedRetryTokens sizes the single recovery retry. It doubles the budget
// (preserving the original length-stop behavior for non-reasoning backends),
// and floors it when the first response showed reasoning activity so a thinking
// model's chain-of-thought can complete AND leave room for the envelope.
func boostedRetryTokens(maxTokens int, resp *chatResponse) int {
	n := maxTokens * 2
	if responseHadReasoning(resp) && n < judgeReasoningRetryFloorTokens {
		n = judgeReasoningRetryFloorTokens
	}
	return n
}

// responseHadReasoning reports whether a response carried a thinking model's
// chain-of-thought — either the provider counted reasoning tokens against the
// completion budget, or a reasoning message field was populated. Used to
// distinguish a reasoning-model budget squeeze from a plain empty/truncated
// completion.
func responseHadReasoning(resp *chatResponse) bool {
	if resp == nil {
		return false
	}
	if resp.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
		return true
	}
	if len(resp.Choices) > 0 {
		m := resp.Choices[0].Message
		if strings.TrimSpace(m.ReasoningContent) != "" || strings.TrimSpace(m.Reasoning) != "" {
			return true
		}
	}
	return false
}

// emptyCompletionNeedsBoostedRetry reports whether an EMPTY completion (the
// caller has already confirmed content trims to "") warrants a single
// larger-budget retry because the miss looks budget-driven rather than a genuine
// empty answer. Two signals qualify: the model emitted chain-of-thought that
// squeezed the answer out of the completion budget (responseHadReasoning), or the
// backend reported the completion was cut at the token ceiling
// (finish_reason=length). This is the reasoning-model recovery the rubric judge
// added for the kimi-k3 empty-envelope squeeze (!1133); factoring it here lets
// the weaver research path reuse the SAME detection (with boostedRetryTokens for
// the SAME 4096 reasoning floor) so a squeeze never surfaces as empty notes
// without one boosted retry. A non-reasoning backend that legitimately returns
// empty content (finish_reason=stop, no reasoning) is NOT retried — a larger
// budget would change nothing — keeping the local qwen path byte-identical.
func emptyCompletionNeedsBoostedRetry(resp *chatResponse) bool {
	return responseHadReasoning(resp) || responseTruncatedByLength(resp)
}

// judgeEmptyDiagnostic annotates the unparseable-error message when the miss is
// a reasoning-model budget squeeze, so an operator reading a raw="" escalation
// sees the real cause (reasoning ate the budget) instead of a bare empty
// string. Returns "" for ordinary misses, keeping the raw=%q suffix last so
// existing reason parsers are unaffected.
func judgeEmptyDiagnostic(resp *chatResponse, content string) string {
	if strings.TrimSpace(content) != "" || !responseHadReasoning(resp) {
		return ""
	}
	rt := resp.Usage.CompletionTokensDetails.ReasoningTokens
	return fmt.Sprintf(" (empty content after %d reasoning tokens; thinking model squeezed the completion budget — raise FLEXINFER_JUDGE_MAX_TOKENS)", rt)
}

// jsonOnlyRetryInstruction is appended to the prompt on the single
// length-stop retry to force the model past any chain-of-thought preamble
// straight to the envelope.
const jsonOnlyRetryInstruction = `Respond with ONLY the JSON object {"score":...,"reasons":[...]} and nothing else.`

// responseTruncatedByLength reports whether the backend stopped the
// completion because it hit the token ceiling (finish_reason == "length").
// Only fires when the backend actually reports a finish reason; an absent
// reason is treated as "not truncated" so we never retry blindly.
func responseTruncatedByLength(resp *chatResponse) bool {
	return resp != nil && len(resp.Choices) > 0 && resp.Choices[0].FinishReason == "length"
}

// composePrompt sandwiches the rubric body around the StageInput
// context. We keep it terse: gates run cheap, and oversize prompts blow
// the proxy's context budget.
func composePrompt(rubricBody string, in gates.StageInput) string {
	var b strings.Builder
	b.WriteString(rubricBody)
	b.WriteString("\n\n=== Inputs ===\n")
	if in.Item != nil {
		fmt.Fprintf(&b, "Backlog item: %s — %s\n", in.Item.ID, in.Item.Title)
		if in.Item.SpecDoc != "" {
			fmt.Fprintf(&b, "Spec doc: %s", in.Item.SpecDoc)
			if in.Item.SpecAnchor != "" {
				fmt.Fprintf(&b, " #%s", in.Item.SpecAnchor)
			}
			b.WriteString("\n")
		}
	}
	if len(in.FilesChanged) > 0 {
		fmt.Fprintf(&b, "Files changed (%d): %s\n", len(in.FilesChanged), strings.Join(in.FilesChanged, ", "))
	}
	if in.LinesAdded > 0 || in.LinesRemoved > 0 {
		fmt.Fprintf(&b, "Diff size: +%d / -%d lines\n", in.LinesAdded, in.LinesRemoved)
	}
	if len(in.CommitMessages) > 0 {
		b.WriteString("Commit messages:\n")
		for _, m := range in.CommitMessages {
			fmt.Fprintf(&b, "  - %s\n", strings.SplitN(m, "\n", 2)[0])
		}
	}
	// The deterministic tests stage verdict, when it ran. The rubric
	// promises the judge "the test_results from the prior tests stage";
	// without this line the judge graded compile health blind and fabricated
	// "undefined symbol" failures on code whose full-repo build had already
	// passed (escalation #304 + ~10 siblings, 2026-07-07..09). Rendered only
	// when true — false means the stage didn't run, not that it failed.
	if in.TestsPassed {
		b.WriteString("Tests stage: PASSED — the full repository (including this " +
			"diff) compiled and the deterministic fmt/lint/test suite ran green " +
			"before this review. Treat build/compile health as verified.\n")
	}
	// Always render the `=== Diff ===` section so the model sees the
	// canonical anchor referenced by rubricGroundingInstructions
	// ("Ground every concern in EXACTLY ONE specific line of the diff
	// provided below"). When the upstream stage produced no patch we
	// emit an explicit `(empty diff)` placeholder rather than omitting
	// the section — without the placeholder the model is more likely
	// to fall back to its prior context and fabricate file:line
	// references that don't exist in the change set.
	b.WriteString("\n=== Diff ===\n```diff\n")
	truncated := false
	if len(in.DiffPatch) == 0 {
		b.WriteString("(empty diff)\n")
	} else {
		// Cap the diff so the prompt stays inside the judge model's
		// context budget. The original 8 KiB cap proved far too small: a
		// routine feature slice (migration + store + runner wiring)
		// overflows it, the cut lands mid-hunk, and the strict rubric then
		// scores the change as an incomplete implementation — a false fail
		// caused by the harness, not the author (escalation #301,
		// 2026-07-09: spec_conformance 0.50, "the diff is truncated at the
		// most critical implementation point"). 32 KiB matches the spawn
		// capture cap (spawn.go defaultMaxDiffBytes) so the judge sees
		// everything the capture kept.
		const maxDiff = 32 * 1024
		patch := in.DiffPatch
		if len(patch) > maxDiff {
			cut := maxDiff
			// Cut on a line boundary so the judge never sees a torn
			// diff line, which reads as file corruption to the rubric.
			if idx := bytes.LastIndexByte(patch[:maxDiff], '\n'); idx > 0 {
				cut = idx
			}
			patch = patch[:cut]
			truncated = true
		}
		b.Write(patch)
		b.WriteString("\n")
		if truncated {
			b.WriteString("... (truncated) ...\n")
		}
	}
	b.WriteString("```\n")
	// A truncated diff must never grade as missing work. The cut can come
	// from this prompt cap OR from an upstream capture cap — MRDiff appends
	// "… [diff truncated]" (gitlab.go) and the spawn capture appends
	// "... [truncated N bytes]" (spawn.go truncationMarker) — so detect
	// those markers in the raw patch too and tell the judge explicitly.
	if truncated ||
		bytes.Contains(in.DiffPatch, []byte("[diff truncated]")) ||
		bytes.Contains(in.DiffPatch, []byte("[truncated ")) {
		b.WriteString("\nNOTE: The diff above was TRUNCATED for prompt length by the " +
			"pipeline harness, not by the author — the change continues beyond the " +
			"truncation marker. Do NOT lower the score or report deviations for " +
			"content missing after the marker (e.g. \"incomplete implementation\", " +
			"\"truncated at a critical point\"); grade only the visible content.\n")
	}
	return b.String()
}

// ErrRubricUnparseable is the sentinel returned by parseRubricEnvelope when
// the judge produced a response we can't grade — no JSON envelope, fields
// out of range, etc. Callers (notably gates.LLMGate) use errors.Is against
// this sentinel to translate a parse miss into a soft gate failure rather
// than an infrastructure escalation: the LLM ran fine, the operator just
// couldn't read the answer, so retrying the upstream stage is cheaper than
// escalating the whole pipeline.
//
// Live trigger (post-M1d canary PIPE-MILLS-CANARY-M1D-VERIFY-2, 2026-05-16):
// gemma4-26b returned the free-text string "please provide the diff..."
// for a spec_conformance judge call. parseRubricEnvelope returned an
// unwrapped string error; runner.go:276 took the no-retry escalation
// branch. Wrapping this sentinel + soft-failing in LLMGate.Evaluate
// routes that case through the existing gate-retry path instead.
//
// The gates package detects this without a back-import via a duck-typed
// predicate: any error in the chain that exposes
// IsRubricUnparseable() bool returning true is treated as a parse miss.
// rubricParseError below implements that predicate.
var ErrRubricUnparseable = errors.New("rubric judge: unparseable response")

// rubricParseError wraps ErrRubricUnparseable with an additional message
// and implements the IsRubricUnparseable() bool predicate that
// gates.LLMGate looks for. The double-handed approach (sentinel + method)
// lets callers use either errors.Is(err, ErrRubricUnparseable) (same
// package or back-import) or the package-free duck-type check (from
// pkg/mills/gates, which can't import clients).
type rubricParseError struct {
	msg string
}

func (e *rubricParseError) Error() string             { return e.msg + ": " + ErrRubricUnparseable.Error() }
func (e *rubricParseError) Unwrap() error             { return ErrRubricUnparseable }
func (e *rubricParseError) IsRubricUnparseable() bool { return true }

func newRubricParseError(format string, args ...any) error {
	return &rubricParseError{msg: fmt.Sprintf(format, args...)}
}

// rubricEnvelope is the shape parseRubricEnvelope decodes: the score is a
// pointer so a missing key is distinguishable from an explicit 0.
type rubricEnvelope struct {
	Score   *float64 `json:"score"`
	Reasons []string `json:"reasons"`
}

// parseRubricEnvelope extracts {"score": float, "reasons": [strings]}
// from the LLM response. Models often wrap the JSON in prose or fenced
// code blocks; we try several locations before giving up. When no
// complete JSON object parses — the pinned judge frequently truncates the
// envelope at its token ceiling behind a "Thinking Process:" preamble —
// we fall back to truncation-tolerant recovery (recoverTruncatedEnvelope).
//
// Failure modes wrap ErrRubricUnparseable so callers can distinguish a
// judge-output problem from a transport/infrastructure failure with
// errors.Is.
func parseRubricEnvelope(content string) (float64, []string, error) {
	candidates := extractJSONCandidates(content)
	for _, c := range candidates {
		var e rubricEnvelope
		if err := json.Unmarshal([]byte(c), &e); err == nil {
			if e.Score == nil {
				continue
			}
			if *e.Score < 0 || *e.Score > 1 {
				return 0, nil, newRubricParseError("score %v out of [0,1]", *e.Score)
			}
			return *e.Score, e.Reasons, nil
		}
	}
	// Complete-JSON parsing failed. Try to recover a score from output
	// truncated at the model's token ceiling before declaring it
	// unparseable.
	if score, reasons, ok := recoverTruncatedEnvelope(content); ok {
		return score, reasons, nil
	}
	return 0, nil, newRubricParseError("no parseable score envelope in response")
}

// truncationRecoveryNote is appended to the reasons when a score was
// recovered from a truncated response via the regex fallback, so the gate
// audit trail records that the reasons are best-effort, not the model's
// complete list.
const truncationRecoveryNote = "[recovered score from truncated judge output; reasons may be incomplete]"

// scoreRegex matches a JSON `"score": <number>` member and captures the
// numeric literal. The number is range-checked by the caller so the
// pattern itself accepts any float (avoids silently matching a prefix of
// an out-of-range value like 1.4 → 1).
var scoreRegex = regexp.MustCompile(`"score"\s*:\s*(-?\d+(?:\.\d+)?)`)

// recoverTruncatedEnvelope attempts to salvage a score from a response
// whose JSON envelope did not parse as a complete object — the dominant
// judge-gate false-fail once the 256-token cap truncated the envelope.
//
// Strategy A: brace/bracket-balance repair of each brace-anchored region,
// last anchor first (the real verdict is emitted after any schema echo),
// closing unterminated strings/arrays/objects and parsing the result.
// Strategy B (only if A finds nothing): regex-extract "score": <float>,
// accept it only when in [0,1], and best-effort collect any complete
// strings from a trailing "reasons" array.
//
// Returns ok=false for genuinely unrecoverable output (no score signal at
// all), which keeps ErrRubricUnparseable semantics intact.
// canonicalExampleEnvelopePrefix is the start of the concrete example envelope
// the rubric prompt embeds (pkg/mills/gates/rubric_boilerplate.go — keep in
// sync). Thinking-style judges echo the prompt's instructions verbatim; if the
// response truncates INSIDE that echo, balance-repair would mint a fake passing
// 1.0 verdict from the example. Recovery therefore strips complete echoes and
// truncates at a dangling partial echo before attempting repair. The
// complete-JSON path is intentionally untouched (a full echo followed by a real
// verdict already resolved correctly pre-hardening).
const canonicalExampleEnvelopePrefix = `{"score": 1.0, "reasons": ["fixture-only`

// stripCanonicalExampleEcho removes echoed copies of the prompt's example
// envelope from recovery input. Complete echoes are excised; a partial
// (truncated) echo ends the usable content, since nothing after it survived.
func stripCanonicalExampleEcho(content string) string {
	for {
		i := strings.Index(content, canonicalExampleEnvelopePrefix)
		if i < 0 {
			return content
		}
		end := strings.Index(content[i:], "]}")
		if end < 0 {
			// Truncated mid-echo: the real verdict never made it out.
			return content[:i]
		}
		content = content[:i] + content[i+end+len("]}"):]
	}
}

func recoverTruncatedEnvelope(content string) (float64, []string, bool) {
	content = stripCanonicalExampleEcho(content)
	// Strategy A: balance-repair, preferring the last brace anchor.
	for i := strings.LastIndexByte(content, '{'); i >= 0; i = strings.LastIndexByte(content[:i], '{') {
		repaired, ok := balanceRepairJSON(content[i:])
		if !ok {
			continue
		}
		var e rubricEnvelope
		if err := json.Unmarshal([]byte(repaired), &e); err == nil && e.Score != nil {
			if *e.Score >= 0 && *e.Score <= 1 {
				return *e.Score, e.Reasons, true
			}
		}
	}
	// Strategy B: regex extraction. Prefer the last in-range match (the
	// verdict follows any echoed schema/example).
	matches := scoreRegex.FindAllStringSubmatch(content, -1)
	for k := len(matches) - 1; k >= 0; k-- {
		score, err := strconv.ParseFloat(matches[k][1], 64)
		if err != nil || score < 0 || score > 1 {
			continue
		}
		reasons := append(recoverReasonStrings(content), truncationRecoveryNote)
		return score, reasons, true
	}
	return 0, nil, false
}

// balanceRepairJSON takes a string beginning at a '{' and returns a
// parseable JSON candidate. If the object already closes at top level it
// returns that balanced prefix (trailing junk dropped). Otherwise it
// treats the input as truncated: it closes any unterminated string, drops
// a dangling trailing comma or incomplete "key": member, and appends the
// closers for every still-open array/object. ok is false only for input
// that does not begin with '{'.
func balanceRepairJSON(s string) (string, bool) {
	if len(s) == 0 || s[0] != '{' {
		return "", false
	}
	var stack []byte
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, ch)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if len(stack) == 0 {
				// Top-level object closed; ignore any trailing prose.
				return s[:i+1], true
			}
		}
	}
	// Truncated: build the minimal completion.
	buf := []byte(s)
	if inString {
		if escaped {
			// Drop a dangling backslash that began an escape with no
			// following character (would be an invalid escape sequence).
			buf = buf[:len(buf)-1]
		}
		buf = append(buf, '"')
	}
	buf = trimIncompleteTail(buf)
	for j := len(stack) - 1; j >= 0; j-- {
		if stack[j] == '{' {
			buf = append(buf, '}')
		} else {
			buf = append(buf, ']')
		}
	}
	return string(buf), true
}

// trimIncompleteTail strips a trailing fragment that would make the buffer
// invalid once open containers are closed: trailing whitespace, a dangling
// comma, or a dangling `"key":` / `"key"` with no value. It assumes any
// unterminated string has already been closed by the caller.
func trimIncompleteTail(buf []byte) []byte {
	for {
		buf = trimTrailingSpace(buf)
		if len(buf) == 0 {
			return buf
		}
		switch buf[len(buf)-1] {
		case ',':
			buf = buf[:len(buf)-1]
			continue
		case ':':
			// Dangling key with no value: drop the colon, then the key
			// string, then a preceding comma if present.
			buf = trimTrailingSpace(buf[:len(buf)-1])
			if trimmed, ok := dropTrailingString(buf); ok {
				buf = trimTrailingSpace(trimmed)
				if len(buf) > 0 && buf[len(buf)-1] == ',' {
					buf = buf[:len(buf)-1]
				}
			}
			continue
		}
		return buf
	}
}

func trimTrailingSpace(buf []byte) []byte {
	for len(buf) > 0 {
		switch buf[len(buf)-1] {
		case ' ', '\t', '\n', '\r':
			buf = buf[:len(buf)-1]
		default:
			return buf
		}
	}
	return buf
}

// dropTrailingString removes a complete JSON string token that ends at the
// tail of buf (buf must end with an unescaped closing quote). It returns
// the buffer with the string removed and ok=true, or the input unchanged
// and ok=false when the tail is not a closed string.
func dropTrailingString(buf []byte) ([]byte, bool) {
	n := len(buf)
	if n == 0 || buf[n-1] != '"' {
		return buf, false
	}
	for i := n - 2; i >= 0; i-- {
		if buf[i] != '"' {
			continue
		}
		bs := 0
		for k := i - 1; k >= 0 && buf[k] == '\\'; k-- {
			bs++
		}
		if bs%2 == 0 {
			return buf[:i], true
		}
	}
	return buf, false
}

// recoverReasonStrings best-effort collects the complete quoted strings
// from the first "reasons" array in content (used by the regex fallback,
// where the object could not be balance-repaired). Incomplete trailing
// strings are skipped.
func recoverReasonStrings(content string) []string {
	idx := strings.Index(content, `"reasons"`)
	if idx < 0 {
		return nil
	}
	open := strings.IndexByte(content[idx:], '[')
	if open < 0 {
		return nil
	}
	rest := content[idx+open+1:]
	var reasons []string
	inString := false
	escaped := false
	var cur []byte
	for i := 0; i < len(rest); i++ {
		ch := rest[i]
		if inString {
			switch {
			case escaped:
				cur = append(cur, ch)
				escaped = false
			case ch == '\\':
				cur = append(cur, ch)
				escaped = true
			case ch == '"':
				inString = false
				var s string
				if err := json.Unmarshal([]byte(`"`+string(cur)+`"`), &s); err == nil {
					reasons = append(reasons, s)
				}
				cur = nil
			default:
				cur = append(cur, ch)
			}
			continue
		}
		if ch == '"' {
			inString = true
		} else if ch == ']' {
			break
		}
	}
	return reasons
}

// extractJSONCandidates returns every decodable JSON object in model output.
// Models sometimes repeat the requested schema in a prose preamble before the
// real verdict, so stopping after the first brace pair is not sufficient.
func extractJSONCandidates(s string) []string {
	var out []string
	seen := make(map[string]struct{})
	appendCandidate := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	if i := strings.Index(s, "```json"); i >= 0 {
		rest := s[i+len("```json"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			appendCandidate(rest[:j])
		}
	}
	if i := strings.Index(s, "```"); i >= 0 && len(out) == 0 {
		rest := s[i+3:]
		if j := strings.Index(rest, "```"); j >= 0 {
			appendCandidate(rest[:j])
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		var candidate json.RawMessage
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		if err := dec.Decode(&candidate); err == nil && len(candidate) > 0 && candidate[0] == '{' {
			appendCandidate(string(candidate))
			i += len(candidate) - 1
		}
	}
	return out
}

// ExtractJSONCandidates exposes the model-output JSON extraction to callers
// outside this package (the overseer triage adapter). Identical behavior to
// the rubric judge's own parsing: fenced ```json blocks first, then any
// decodable top-level object.
func ExtractJSONCandidates(s string) []string { return extractJSONCandidates(s) }

// defaultRubricBody returns the canonical prompt body for a rubric.
// The gates package owns these strings; we mirror them by name to keep
// the audit trail clean.
func defaultRubricBody(rubric string) string {
	switch rubric {
	case gates.SpecConformanceRubricName:
		return gates.SpecConformanceRubric
	case gates.PRSelfReviewRubricName:
		return gates.PRSelfReviewRubric
	default:
		// Unknown rubric: the gate authors must register the body
		// somewhere; produce a generic envelope ask so the call still
		// returns a parseable response.
		return "You are a strict reviewer. Score the following input on [0,1] and return only {\"score\": <float>, \"reasons\": [...]}\n\nRubric: " + rubric
	}
}

func modelFrom(resp *chatResponse, fallback string) string {
	if resp != nil && resp.Model != "" {
		return resp.Model
	}
	return fallback
}

// Chat is the public chat-completion entry point for callers that need
// the bare model output + cost estimate without the gate-judge envelope.
// Empty model falls back to the configured WeaverModel then JudgeModel;
// maxTokens=0 falls back to 1024.
func (c *FlexInferClient) Chat(ctx context.Context, model, prompt string, maxTokens int) (string, float64, error) {
	return c.chatCompletion(ctx, model, prompt, maxTokens, false)
}

// ChatConcise is Chat with model-visible chain-of-thought disabled through the
// OpenAI-compatible chat_template_kwargs contract. Use it for short prose
// responses that must complete inside a bounded dispatch window.
func (c *FlexInferClient) ChatConcise(ctx context.Context, model, prompt string, maxTokens int) (string, float64, error) {
	return c.chatCompletion(ctx, model, prompt, maxTokens, true)
}

// ChatStructured is Chat with model-visible chain-of-thought disabled through
// the OpenAI-compatible chat_template_kwargs contract. Use it for callers that
// require a compact JSON envelope rather than free-form prose.
func (c *FlexInferClient) ChatStructured(ctx context.Context, model, prompt string, maxTokens int) (string, float64, error) {
	return c.chatCompletion(ctx, model, prompt, maxTokens, true)
}

func (c *FlexInferClient) chatCompletion(ctx context.Context, model, prompt string, maxTokens int, structured bool) (string, float64, error) {
	content, cost, _, err := c.chatCompletionCostStatus(ctx, model, prompt, maxTokens, structured)
	return content, cost, err
}

// chatCompletionCostStatus retains whether a remote proxy supplied an
// authoritative usage.cost. The public Chat methods preserve their historical
// shape; Council adapters use this richer internal form so a LiteLLM response
// cannot pass a local flat-rate estimate off as known provider spend.
func (c *FlexInferClient) chatCompletionCostStatus(ctx context.Context, model, prompt string, maxTokens int, structured bool) (string, float64, bool, error) {
	content, _, cost, providerReported, err := c.chatCompletionResponseCostStatus(ctx, model, prompt, maxTokens, structured)
	return content, cost, providerReported, err
}

// chatCompletionResponseCostStatus is the response-bearing form of
// chatCompletionCostStatus. Most Council adapters only need content and cost;
// structured judges also need finish_reason and reasoning-token metadata to
// distinguish an empty answer from a completion-budget squeeze.
func (c *FlexInferClient) chatCompletionResponseCostStatus(ctx context.Context, model, prompt string, maxTokens int, structured bool) (string, *chatResponse, float64, bool, error) {
	if c == nil {
		return "", nil, 0, false, errors.New("flexinfer: client nil")
	}
	if model == "" {
		model = c.cfg.WeaverModel
	}
	if model == "" {
		model = c.cfg.JudgeModel
	}
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	var content string
	var resp *chatResponse
	var err error
	if structured {
		content, resp, err = c.structuredChatFallback(ctx, model, prompt, maxTokens)
	} else {
		content, resp, err = c.chatFallback(ctx, model, prompt, maxTokens)
	}
	cost := estimateCostUSD(resp)
	providerReported := resp != nil && resp.Usage.Cost > 0
	if err != nil {
		return content, resp, cost, providerReported, err
	}
	return content, resp, cost, providerReported, nil
}

// isLocalFlexInferModel reports whether model is routed to a local FlexInfer
// backend. Gateway-prefixed external models do not support vLLM's
// chat_template_kwargs parameter and reject it as an unknown request field.
func isLocalFlexInferModel(model string) bool {
	model = strings.ToLower(model)
	return !strings.HasPrefix(model, "oa/") && !strings.HasPrefix(model, "or/")
}

// ----- WeaverClient (research stage) -----

// ResearchMode controls how WeaverClient.Research dispatches a call.
// See services/loom-core/.loom/111-product-spec-weaver-qwen3-
// integration-2026-05-08.md (MW-001/002/003).
type ResearchMode string

const (
	// ResearchModeOff calls the legacy single-prompt chat against the
	// configured WeaverModel. Backward-compatible default.
	ResearchModeOff ResearchMode = "off"

	// ResearchModeShadow calls both the legacy path AND the
	// WeaverDelegator (when one is configured), returns the legacy
	// result, and records the shadow result + diff for offline
	// analysis. Used during the soak before flipping to "on".
	ResearchModeShadow ResearchMode = "shadow"

	// ResearchModeOn delegates to the configured WeaverDelegator.
	// Falls back to the legacy chat if the delegator is unconfigured
	// or returns a non-context error — degraded but never silently
	// broken.
	ResearchModeOn ResearchMode = "on"

	// EnvResearchMode is the env var read at construction time when no
	// explicit mode is set. Values: "off" (default), "shadow", "on".
	EnvResearchMode = "MILLS_RESEARCH_VIA_WEAVER"
)

// ParseResearchMode validates a string against the known modes.
// Empty or unknown values fall back to ResearchModeOff.
func ParseResearchMode(s string) ResearchMode {
	switch ResearchMode(strings.ToLower(strings.TrimSpace(s))) {
	case ResearchModeShadow:
		return ResearchModeShadow
	case ResearchModeOn:
		return ResearchModeOn
	default:
		return ResearchModeOff
	}
}

// WeaverDelegator forwards a research request to the routed
// pkg/weaver Router. Production implementation issues an in-cluster
// loom/weaver/query JSON-RPC against the daemon socket; that wiring
// lands in a follow-up MR (the daemon-RPC client is its own focused
// surface).
//
// Returning an error from Delegate causes the WeaverClient to fall
// back to the legacy chat in "on" mode, or to record a "delegate_
// failed" diff entry in "shadow" mode.
type WeaverDelegator interface {
	Delegate(ctx context.Context, req pipeline.WeaverRequest) (pipeline.WeaverResponse, error)
}

// ResearchDiffRecorder receives a structured snapshot of the legacy
// + shadow paths during ResearchModeShadow runs. Implementations
// persist to the pipeline_runs.research_diff column or a metrics
// sink. Nil is safe — shadow comparisons just skip recording.
//
// runID is the pipeline_runs.id the diff belongs to (empty when the
// caller has no run context — production recorders should skip the
// persist in that case rather than write to a fabricated key).
// backlogID is supplied for human-readable context; the diff map also
// includes "backlog_id" and "run_id" entries so a recorder writing to
// a metrics sink doesn't have to plumb both keys separately.
//
// Diff keys: backlog_id, run_id, legacy_chars, shadow_chars,
// legacy_cost_usd, shadow_cost_usd, length_delta_pct,
// shadow_error (when present), legacy_error (when present).
type ResearchDiffRecorder interface {
	Record(ctx context.Context, runID, backlogID string, diff map[string]any)
}

// WeaverClient satisfies pipeline.WeaverClient. The research stage in
// the autonomy spec is described as a "weaver subagent (codebase
// domain)"; the legacy v1 implementation is a single FlexInfer call.
// MW-001 introduces an optional delegator that issues a routed weaver
// query (multi-domain dispatch) gated behind ResearchMode.
type WeaverClient struct {
	Client       *FlexInferClient
	MaxTokens    int // default 1024 (env FLEXINFER_WEAVER_MAX_TOKENS; >=4096 for reasoning models)
	Mode         ResearchMode
	Delegator    WeaverDelegator
	DiffRecorder ResearchDiffRecorder
	Logger       *slog.Logger

	// RepoRoot grounds research-note path validation. Any file path
	// referenced in the returned notes that does not exist under
	// RepoRoot (or the run's worktree, when present) is treated as a
	// hallucination: the notes are sanitized (partial) or withheld
	// (wholesale) before they reach the implement worker. Empty
	// disables the guard. See research_grounding.go for the failure
	// mode this defends against.
	RepoRoot string
}

// NewWeaverClient wires a FlexInfer-backed research client. Reads
// MILLS_RESEARCH_VIA_WEAVER from the environment at construction
// time; callers that want explicit control can override Mode +
// Delegator on the returned struct.
func NewWeaverClient(c *FlexInferClient) *WeaverClient {
	mode := ParseResearchMode(os.Getenv(EnvResearchMode))
	return &WeaverClient{
		Client:    c,
		MaxTokens: weaverMaxTokensFromEnv(),
		Mode:      mode,
		Logger:    slog.Default().With("component", "mills-weaver-client"),
	}
}

// Research implements pipeline.WeaverClient.
//
// Whatever mode produced the notes, the output is passed through
// groundNotes before returning so a hallucinated codebase (the
// gemma4-26b failure mode in PIPE-MILLS-2026-06-29-001) can't reach
// the implement worker. The grounding guard is a no-op when no
// validation root is configured.
func (w *WeaverClient) Research(ctx context.Context, req pipeline.WeaverRequest) (pipeline.WeaverResponse, error) {
	if w == nil || w.Client == nil {
		return pipeline.WeaverResponse{}, errors.New("weaver: client not configured")
	}
	ctx = llmusage.WithComponent(ctx, ComponentWeaver)
	var (
		resp pipeline.WeaverResponse
		err  error
	)
	switch w.Mode {
	case ResearchModeShadow:
		resp, err = w.shadowResearch(ctx, req)
	case ResearchModeOn:
		resp, err = w.delegatedResearch(ctx, req)
	default:
		// off + unknown
		resp, err = w.legacyResearch(ctx, req)
	}
	if err != nil {
		return resp, err
	}
	return w.groundNotes(req, resp), nil
}

// groundNotes validates the file paths referenced in the research
// notes against the real repository and rewrites the notes when
// fabricated paths are found. The run's worktree (req.Env
// LOOM_MILLS_WORKTREE) takes precedence over the configured RepoRoot
// when it exists on disk; when neither is available the notes pass
// through untouched.
func (w *WeaverClient) groundNotes(req pipeline.WeaverRequest, resp pipeline.WeaverResponse) pipeline.WeaverResponse {
	root := researchValidationRoot(req.Env, w.RepoRoot)
	if root == "" || strings.TrimSpace(resp.Notes) == "" {
		return resp
	}
	out := SanitizeResearchNotes(resp.Notes, repoPathChecker(root), req.DeclaredPaths)
	if len(out.Dropped) == 0 {
		return resp
	}
	action := researchGuardFlagged
	if out.Withheld {
		action = researchGuardWithheld
	}
	mills.ResearchNotesGuardTotal.WithLabelValues(action).Inc()
	mills.ResearchPathsDroppedTotal.Add(float64(len(out.Dropped)))

	resp.Notes = out.Notes
	w.logger().Warn("research notes referenced non-existent repo paths; sanitized",
		"backlog_id", req.BacklogID, "run_id", req.RunID, "action", action,
		"dropped_count", len(out.Dropped), "dropped", strings.Join(out.Dropped, ","),
		"validation_root", root)
	if resp.Citation == nil {
		resp.Citation = map[string]any{}
	}
	resp.Citation["paths_dropped"] = out.Dropped
	return resp
}

// research-note guard metric action labels.
const (
	researchGuardWithheld = "withheld"
	researchGuardFlagged  = "flagged"
)

// researchValidationRoot picks the directory used to validate
// referenced paths: the run's worktree when set and present on disk,
// otherwise the configured fallback (the operator's repo checkout).
// Returns "" when neither resolves to a directory.
func researchValidationRoot(env map[string]string, fallback string) string {
	if env != nil {
		if wt := strings.TrimSpace(env["LOOM_MILLS_WORKTREE"]); isDir(wt) {
			return wt
		}
	}
	if fallback = strings.TrimSpace(fallback); isDir(fallback) {
		return fallback
	}
	return ""
}

func isDir(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// legacyResearch is the original single-prompt FlexInfer path.
func (w *WeaverClient) legacyResearch(ctx context.Context, req pipeline.WeaverRequest) (pipeline.WeaverResponse, error) {
	maxTokens := w.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultWeaverMaxTokens
	}
	// Pin prefix-cache routing to the backlog item. The research prompt leads
	// with this item's journal render, so on a multi-replica lane two items'
	// deep prefixes would otherwise compete for one replica's cache
	// arbitrarily. Blank BacklogID (test wiring, ad-hoc calls) sends no header.
	opts := chatOptions{cacheKey: itemCacheKey(req.BacklogID)}
	content, resp, err := w.chatWithReasoningRecovery(ctx, w.Client.cfg.WeaverModel, req.Prompt, maxTokens, opts)
	if err != nil {
		return pipeline.WeaverResponse{}, err
	}
	cost := estimateCostUSD(resp)
	model := modelFrom(resp, w.Client.cfg.WeaverModel)
	return pipeline.WeaverResponse{
		SpawnID: "weaver-" + model,
		CostUSD: cost,
		Notes:   content,
		// Attribute research cost to the resolved model tier for the per-model
		// telemetry roll-up. Backend is the local/gateway proxy family
		// ("flexinfer"); litellm/OpenRouter frontier models resolve through the
		// same proxy and are distinguished by their model id.
		Model:   model,
		Backend: weaverBackendLabel,
		Citation: map[string]any{
			"model":             model,
			"prompt_tokens":     usagePromptTokens(resp),
			"completion_tokens": usageCompletionTokens(resp),
		},
		// Usage carries the same two counts plus the cached share, which the
		// citation map has no slot for. The research stage promotes these to
		// top-level artifacts so the prefix-cache question can be answered by
		// querying stage_results rather than by grepping debug logs.
		Usage: resp.normalizedUsage(),
	}, nil
}

// chatWithReasoningRecovery runs the research chat and guarantees a
// reasoning-model budget squeeze never returns EMPTY notes without exactly ONE
// larger-budget retry. kimi-k3 (via the LiteLLM gateway) is a thinking model:
// its chain-of-thought comes back in a SEPARATE message.reasoning_content field
// whose tokens count AGAINST max_tokens, so at a small budget the reasoning
// consumes the whole completion and message.content (the notes) comes back empty
// (live judge capture 2026-07-17: reasoning_tokens=1021/1024, content=NULL,
// finish_reason=length). This mirrors the rubric judge's empty-envelope recovery
// (issue #348 / !1133) and REUSES the same detection (emptyCompletionNeedsBoosted
// Retry → responseHadReasoning) and retry sizing (boostedRetryTokens, floored to
// judgeReasoningRetryFloorTokens=4096 when reasoning ran) so both the judge and
// the weaver recover the squeeze identically.
//
// The local (non-reasoning) qwen path is byte-identical: a non-empty first
// answer returns immediately and never triggers a retry, and an empty answer
// with no reasoning and no length-stop is a genuine empty result that a larger
// budget can't fix, so it is returned as-is without a wasted retry. Bounded to a
// single retry.
// opts carries the per-request routing key (see legacyResearch); both the first
// call and the bounded retry send the same key so the retry lands on the replica
// that just warmed this item's prefix.
func (w *WeaverClient) chatWithReasoningRecovery(ctx context.Context, model, prompt string, maxTokens int, opts chatOptions) (string, *chatResponse, error) {
	content, resp, err := w.Client.chatFallbackWithOptions(ctx, model, prompt, maxTokens, opts)
	if err != nil {
		return "", resp, err
	}
	if strings.TrimSpace(content) != "" || !emptyCompletionNeedsBoostedRetry(resp) {
		return content, resp, nil
	}
	retryTokens := boostedRetryTokens(maxTokens, resp)
	w.logger().Warn("weaver research returned empty notes under a reasoning-model budget squeeze; retrying once with a boosted budget",
		"model", model, "first_max_tokens", maxTokens, "retry_max_tokens", retryTokens,
		"reasoning_tokens", reasoningTokensUsed(resp))
	if rc, rresp, rerr := w.Client.chatFallbackWithOptions(ctx, model, prompt, retryTokens, opts); rerr == nil && strings.TrimSpace(rc) != "" {
		return rc, rresp, nil
	}
	// Bounded to one retry: if it still came back empty (or errored) fall back to
	// the original response for cost/model attribution. The grounding guard and
	// the downstream implement worker handle empty notes; raising
	// FLEXINFER_WEAVER_MAX_TOKENS is the durable fix for a squeezed budget.
	return content, resp, nil
}

// reasoningTokensUsed returns the reasoning-token count a thinking model spent
// on chain-of-thought (0 when absent). Used only for the empty-notes retry log.
func reasoningTokensUsed(resp *chatResponse) int {
	if resp == nil {
		return 0
	}
	return resp.Usage.CompletionTokensDetails.ReasoningTokens
}

// weaverBackendLabel is the telemetry backend bucket for the research stage's
// FlexInfer-proxied model calls.
const weaverBackendLabel = "flexinfer"

// delegatedResearch routes to the configured WeaverDelegator. Falls
// back to legacy when the delegator is unconfigured or returns a
// non-context error so a transient delegation failure never breaks
// the pipeline.
func (w *WeaverClient) delegatedResearch(ctx context.Context, req pipeline.WeaverRequest) (pipeline.WeaverResponse, error) {
	if w.Delegator == nil {
		w.logger().Warn("research mode=on but no delegator configured; falling back to legacy")
		return w.legacyResearch(ctx, req)
	}
	resp, err := w.Delegator.Delegate(ctx, req)
	if err != nil {
		// Context errors propagate; everything else falls back so
		// pipeline progress isn't held hostage to delegator health.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return pipeline.WeaverResponse{}, err
		}
		w.logger().Warn("weaver delegate failed; falling back to legacy",
			"backlog_id", req.BacklogID, "error", err)
		return w.legacyResearch(ctx, req)
	}
	return resp, nil
}

// shadowResearch runs the legacy and (optionally) the delegator paths
// in parallel, returns the legacy result for backward-compat, and
// records the diff. Used during the soak window before flipping to
// "on".
func (w *WeaverClient) shadowResearch(ctx context.Context, req pipeline.WeaverRequest) (pipeline.WeaverResponse, error) {
	type shadowResult struct {
		resp pipeline.WeaverResponse
		err  error
	}
	legacyCh := make(chan shadowResult, 1)
	go func() {
		r, e := w.legacyResearch(ctx, req)
		legacyCh <- shadowResult{r, e}
	}()
	shadowCh := make(chan shadowResult, 1)
	if w.Delegator != nil {
		go func() {
			r, e := w.Delegator.Delegate(ctx, req)
			shadowCh <- shadowResult{r, e}
		}()
	} else {
		shadowCh <- shadowResult{err: errors.New("delegator not configured")}
	}

	legacy := <-legacyCh
	shadow := <-shadowCh
	w.recordDiff(ctx, req, legacy, shadow)
	return legacy.resp, legacy.err
}

func (w *WeaverClient) recordDiff(
	ctx context.Context,
	req pipeline.WeaverRequest,
	legacy, shadow struct {
		resp pipeline.WeaverResponse
		err  error
	},
) {
	if w.DiffRecorder == nil {
		return
	}
	legacyChars := len(legacy.resp.Notes)
	shadowChars := len(shadow.resp.Notes)
	var deltaPct float64
	if legacyChars > 0 {
		deltaPct = float64(shadowChars-legacyChars) / float64(legacyChars) * 100
	}
	diff := map[string]any{
		"backlog_id":       req.BacklogID,
		"run_id":           req.RunID,
		"legacy_chars":     legacyChars,
		"shadow_chars":     shadowChars,
		"legacy_cost_usd":  legacy.resp.CostUSD,
		"shadow_cost_usd":  shadow.resp.CostUSD,
		"length_delta_pct": deltaPct,
	}
	if shadow.err != nil {
		diff["shadow_error"] = shadow.err.Error()
	}
	if legacy.err != nil {
		diff["legacy_error"] = legacy.err.Error()
	}
	w.DiffRecorder.Record(ctx, req.RunID, req.BacklogID, diff)
}

func (w *WeaverClient) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

func usagePromptTokens(resp *chatResponse) int {
	if resp == nil {
		return 0
	}
	return resp.Usage.PromptTokens
}

func usageCompletionTokens(resp *chatResponse) int {
	if resp == nil {
		return 0
	}
	return resp.Usage.CompletionTokens
}

// estimateCostUSD applies a flat per-1k-token rate. Real cost comes
// from the proxy's accounting service; this is a placeholder so the
// pipeline_runs.cost_usd column has non-zero values during bring-up.
//
// Default rate: $0.0002 / 1k input + $0.0002 / 1k output (close to
// vLLM-served Qwen3-8B at internal pricing).
func estimateCostUSD(resp *chatResponse) float64 {
	if resp == nil {
		return 0
	}
	// Provider-reported cost (LiteLLM/OpenRouter) is authoritative when
	// present; the flat local-tier estimate below only fits the FlexInfer
	// proxy's GPU models, which never report cost.
	if resp.Usage.Cost > 0 {
		return resp.Usage.Cost
	}
	in := float64(resp.Usage.PromptTokens) / 1000 * 0.0002
	out := float64(resp.Usage.CompletionTokens) / 1000 * 0.0002
	return in + out
}

// SetTransport is for tests: replaces the underlying http.RoundTripper
// so test cases can serve canned responses without standing up a
// listener.
func (c *FlexInferClient) SetTransport(rt http.RoundTripper) {
	c.http.HTTP().Transport = rt
}

package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

func TestOpenRouterEditorAndJudge(t *testing.T) {
	RegisterModelPrices(map[string]ModelPrice{"anthropic/test": {Provider: "openrouter", InputPerMillion: 2, CachedInputPerMillion: 1, OutputPerMillion: 4}})
	t.Cleanup(func() { RegisterModelPrices(nil) })
	content := "## Research\nEvidence\n## Product Spec\nRequirements\n## Implementation Plan\nSteps"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test" || r.Header.Get("HTTP-Referer") != "https://loom.example" || r.Header.Get("X-Title") != "Mills" {
			t.Errorf("bad request: %s %v", r.URL, r.Header)
		}
		var req struct {
			Model    string
			Messages []struct{ Role, Content string }
			Stream   bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != "anthropic/test" || len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content == "" || req.Stream {
			t.Errorf("bad body: %+v", req)
		}
		data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10, "prompt_tokens_details": map[string]int{"cached_tokens": 40}}})
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	c, err := NewOpenRouterClient(OpenRouterClientConfig{APIKey: "test", BaseURL: srv.URL + "/api/v1", HTTPReferer: "https://loom.example", Title: "Mills"})
	if err != nil {
		t.Fatal(err)
	}
	e := &OpenRouterCouncilEditor{Client: c, Model: "anthropic/test", Breaker: NewVendorBreaker(nil)}
	out, err := e.Edit(context.Background(), &council.Brief{Markdown: "brief"}, nil)
	if err != nil || out.Empty || out.Backend != "openrouter" || len(out.Documents) != 3 || out.CostUnpriced || math.Abs(out.CostUSD-.0002) > 1e-10 {
		t.Fatalf("%+v %v", out, err)
	}
	content = `{"score":0.9,"reasons":["supported"]}`
	j := &OpenRouterRubricJudge{Client: c, Model: e.Model, Breaker: e.Breaker}
	v, err := j.Judge(context.Background(), "spec_conformance", gates.StageInput{})
	if err != nil || v.Score != .9 || v.Model != e.Model {
		t.Fatal(v, err)
	}
	content = ""
	out, err = e.Edit(context.Background(), &council.Brief{}, nil)
	if err != nil || !out.Empty {
		t.Fatal(out, err)
	}
	_, err = j.Judge(context.Background(), "spec_conformance", gates.StageInput{})
	if !errors.Is(err, ErrRubricUnparseable) {
		t.Fatal(err)
	}
}

func TestOpenRouterErrorsAndBreaker(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   VendorErrorKind
		zero   bool
	}{{402, VendorBilling, true}, {401, VendorAuth, true}, {429, VendorRateLimit, false}, {503, VendorOverloaded, false}, {400, VendorInvalidRequest, false}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("X-Request-ID", "req-1")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"failure"}}`, tc.status)
			}))
			defer srv.Close()
			c, err := NewOpenRouterClient(OpenRouterClientConfig{APIKey: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			b := NewVendorBreaker(nil)
			b.Trip("openai", VendorBilling, time.Minute)
			e := &OpenRouterCouncilEditor{Client: c, Model: "unknown/model", Breaker: b}
			out, err := e.Edit(context.Background(), &council.Brief{}, nil)
			ve, ok := AsVendorError(err)
			if !ok || ve.Vendor != "openrouter" || ve.Kind != tc.kind || ve.Status != tc.status || ve.RequestID != "req-1" || out.CostUnpriced == tc.zero {
				t.Fatal(out, err)
			}
			if tc.status != 400 {
				_, err = e.Edit(context.Background(), &council.Brief{}, nil)
				if !errors.Is(err, ErrVendorBreakerOpen) || calls != 1 {
					t.Fatal(calls, err)
				}
			}
		})
	}
}

func TestOpenRouterMalformedRefusalCancellation(t *testing.T) {
	for _, body := range []string{`not-json`, `{"choices":[{"message":{"refusal":"no"}}]}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
		c, err := NewOpenRouterClient(OpenRouterClientConfig{APIKey: "test", BaseURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		j := &OpenRouterRubricJudge{Client: c, Model: "test/model", Breaker: NewVendorBreaker(nil)}
		_, err = j.Judge(context.Background(), "spec_conformance", gates.StageInput{})
		if err == nil {
			t.Fatal("expected failure")
		}
		if strings.Contains(body, "refusal") {
			ve, ok := AsVendorError(err)
			if !ok || ve.Kind != VendorRefusal {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = j.Judge(ctx, "spec_conformance", gates.StageInput{})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		srv.Close()
	}
}

func TestOpenRouterPrices(t *testing.T) {
	RegisterModelPrices(map[string]ModelPrice{"moonshotai/test": {Provider: "openrouter", InputPerMillion: 2, OutputPerMillion: 3}, "or/test": {Provider: "openrouter", InputPerMillion: 4, OutputPerMillion: 5}, "openai/isolated": {Provider: "openai", InputPerMillion: 9}})
	t.Cleanup(func() { RegisterModelPrices(nil) })
	for model, want := range map[string]float64{"moonshotai/test": 5, "or/test": 9} {
		got, ok := openRouterResponseCostUSD(model, openairesponses.TurnResponse{PromptTokens: 1000000, CompletionTokens: 1000000})
		if !ok || got != want {
			t.Fatal(model, got, ok)
		}
	}
	for _, model := range []string{"test", "openai/isolated", "or/moonshotai/test"} {
		if _, ok := lookupOpenRouterPrice(model); ok {
			t.Fatal("unexpected alias", model)
		}
	}
}

func TestOpenRouterConfigAndUsage(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", " test ")
	t.Setenv("OPENROUTER_BASE_URL", "")
	t.Setenv("OPENROUTER_HTTP_REFERER", "https://loom.example")
	t.Setenv("OPENROUTER_X_TITLE", "Mills")
	config := OpenRouterConfigFromEnv()
	c, err := NewOpenRouterClient(config)
	if err != nil || c.config.BaseURL != "https://openrouter.ai/api/v1" || config.APIKey != "test" || config.Title != "Mills" || config.HTTPReferer != "https://loom.example" {
		t.Fatal(config, err)
	}
	if _, err := NewOpenRouterClient(OpenRouterClientConfig{}); err == nil {
		t.Fatal("missing key accepted")
	}
	config.BaseURL = ":invalid"
	if _, err := NewOpenRouterClient(config); err == nil {
		t.Fatal("invalid URL accepted")
	}
	for _, usage := range []string{`{}`, `{"prompt_tokens":100,"completion_tokens":10,"input_tokens_details":{"cached_tokens":30}}`} {
		var logs bytes.Buffer
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"model":"served/model","choices":[{"message":{"content":"ok"}}],"usage":%s}`, usage)
		}))
		config.BaseURL = srv.URL
		config.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		config.Component = ComponentCouncilEditor
		c, err = NewOpenRouterClient(config)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Create(context.Background(), openairesponses.TurnRequest{Model: "requested/model", Input: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		if usage == `{}` {
			if strings.Contains(logs.String(), `"cached_tokens"`) {
				t.Fatal(logs.String())
			}
		} else if !strings.Contains(logs.String(), `"cached_tokens":30`) || !strings.Contains(logs.String(), `"llm_model":"served/model"`) {
			t.Fatal(logs.String())
		}
		srv.Close()
	}
}

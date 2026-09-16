package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/openairesponses"
)

func TestVendorAnthropicHTTPClassification(t *testing.T) {
	for _, tc := range []struct {
		status        int
		code, message string
		kind          VendorErrorKind
		retry         bool
	}{
		{400, "invalid_request_error", "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.", VendorBilling, false},
		{400, "invalid_request_error", "bad model", VendorInvalidRequest, false},
		{401, "authentication_error", "bad key", VendorAuth, false},
		{403, "permission_error", "forbidden", VendorAuth, false},
		{429, "rate_limit_error", "slow down", VendorRateLimit, true},
		{529, "overloaded_error", "busy", VendorOverloaded, true},
		{503, "api_error", "unavailable", VendorOverloaded, true},
	} {
		t.Run(fmt.Sprint(tc.status, tc.kind), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("request-id", "req-anthropic")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, tc.code, tc.message)
			}))
			defer srv.Close()
			b := NewVendorBreaker(nil)
			c := &AnthropicClient{api: anthropic.NewClient(option.WithAPIKey("test"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0)), breaker: b}
			_, err := c.CreateMessage(context.Background(), anthropicMessageRequest{Model: "test", Prompt: "hello"})
			e, ok := AsVendorError(err)
			if !ok || e.Kind != tc.kind || e.Status != tc.status || e.Code != tc.code || e.RequestID != "req-anthropic" || e.Retryable != tc.retry {
				t.Fatalf("error: %#v", e)
			}
			var cause *anthropic.Error
			if !errors.As(err, &cause) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("lost cause/message: %v", err)
			}
			if tc.kind != VendorInvalidRequest {
				_, err = c.CreateMessage(context.Background(), anthropicMessageRequest{Model: "test"})
				if !errors.Is(err, ErrVendorBreakerOpen) || calls != 1 {
					t.Fatalf("open called HTTP: calls=%d err=%v", calls, err)
				}
			}
		})
	}
}

func TestVendorOpenAIHTTPClassificationAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		kind   VendorErrorKind
	}{
		{429, "insufficient_quota", VendorBilling}, {429, "rate_limit_exceeded", VendorRateLimit},
		{401, "invalid_api_key", VendorAuth}, {500, "server_error", VendorOverloaded}, {400, "bad_request", VendorInvalidRequest},
	} {
		t.Run(tc.code, func(t *testing.T) {
			now := time.Unix(100, 0)
			b := NewVendorBreaker(func() time.Time { return now })
			calls := 0
			success := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-request-id", "req-openai")
				if success {
					_, _ = w.Write([]byte(`{"id":"resp","status":"completed","output_text":"hello","usage":{"input_tokens":10,"output_tokens":5}}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"type":"api_error","code":%q,"message":"vendor failure"}}`, tc.code)
			}))
			defer srv.Close()
			c, err := openairesponses.NewAPIClient(openairesponses.APIClientConfig{APIKey: "test", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			ed := &OpenAIResponsesCouncilEditor{Client: c, Model: "gpt-5.4", Breaker: b}
			out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "test"}, nil)
			e, ok := AsVendorError(err)
			if !ok || e.Kind != tc.kind || e.Code != tc.code || e.RequestID != "req-openai" || e.Status != tc.status {
				t.Fatalf("error: %#v", e)
			}
			var cause *openairesponses.APIError
			if !errors.As(err, &cause) {
				t.Fatalf("lost API cause: %v", err)
			}
			if tc.kind == VendorBilling || tc.kind == VendorAuth {
				assertVendorZero(t, out)
			}
			if tc.kind != VendorInvalidRequest {
				out, err = ed.Edit(context.Background(), &council.Brief{Markdown: "test"}, nil)
				if !errors.Is(err, ErrVendorBreakerOpen) || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				assertVendorZero(t, out)
			}
			now = now.Add(31 * time.Minute)
			success = true
			if _, err = ed.Edit(context.Background(), &council.Brief{Markdown: "test"}, nil); err != nil || b.Open("openai") {
				t.Fatalf("recovery: %v", err)
			}
		})
	}
}

func TestVendorTransportAndWrapping(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, vendor := range []string{"anthropic", "openai"} {
			e := classifyVendorError(vendor, fmt.Errorf("wrapped: %w", cause))
			if e.Kind != VendorTransport || !errors.Is(e, cause) {
				t.Fatalf("%+v", e)
			}
		}
	}
	e := &VendorError{Vendor: "anthropic", Kind: VendorBilling, Err: errors.New("credits")}
	wrapped := fmt.Errorf("outer: %w", e)
	if got, ok := AsVendorError(wrapped); !ok || got != e || !IsVendorBilling(wrapped) {
		t.Fatal("lost typed error")
	}
	if classifyVendorError("anthropic", wrapped) != e {
		t.Fatal("rewrapped typed error")
	}
	if got, ok := AsVendorError(nil); ok || got != nil {
		t.Fatal("nil classified")
	}
}

func TestVendorBreakerLifecycleAndConcurrency(t *testing.T) {
	now := time.Unix(100, 0)
	b := NewVendorBreaker(func() time.Time { return now })
	b.Trip("anthropic", VendorAuth, time.Minute)
	since := *b.State("anthropic").Since
	b.Trip("anthropic", VendorBilling, time.Hour)
	if b.Open("openai") || b.State("anthropic").Kind != VendorAuth || !b.State("anthropic").Since.Equal(since) {
		t.Fatal("vendor isolation or trip changed")
	}
	state := b.State("anthropic")
	*state.Since = time.Time{}
	if !b.State("anthropic").Since.Equal(since) {
		t.Fatal("state escaped")
	}
	now = now.Add(time.Minute)
	if b.Open("anthropic") || b.State("anthropic").Since != nil {
		t.Fatal("not expired at boundary")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Trip("openai", VendorRateLimit, time.Minute)
				b.Open("openai")
				b.Reset("openai")
			}
		}()
	}
	wg.Wait()
	b.Reset("openai")
	if b.Open("openai") {
		t.Fatal("not reset")
	}
}

func TestVendorBreakerMetricsAndTTL(t *testing.T) {
	now := time.Unix(100, 0)
	original := DefaultVendorBreaker
	DefaultVendorBreaker = NewVendorBreaker(func() time.Time { return now })
	defer func() { DefaultVendorBreaker = original }()
	for _, tc := range []struct {
		kind VendorErrorKind
		ttl  time.Duration
	}{{VendorBilling, 30 * time.Minute}, {VendorAuth, 30 * time.Minute}, {VendorRateLimit, 2 * time.Minute}, {VendorOverloaded, 2 * time.Minute}} {
		before := testutil.ToFloat64(mills.VendorErrorsTotal.WithLabelValues("anthropic", string(tc.kind)))
		_ = DefaultVendorBreaker.failure("anthropic", &VendorError{Vendor: "anthropic", Kind: tc.kind})
		if got := testutil.ToFloat64(mills.VendorErrorsTotal.WithLabelValues("anthropic", string(tc.kind))); got != before+1 {
			t.Fatalf("counter=%v", got)
		}
		assertVendorGauge(t, "anthropic", 1)
		now = now.Add(tc.ttl)
		assertVendorGauge(t, "anthropic", 0)
	}
}
func assertVendorGauge(t *testing.T, vendor string, want float64) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "mills_vendor_breaker_open" {
			for _, m := range f.Metric {
				for _, l := range m.Label {
					if l.GetName() == "vendor" && l.GetValue() == vendor {
						if m.GetGauge().GetValue() != want {
							t.Fatalf("gauge=%v want %v", m.GetGauge().GetValue(), want)
						}
						return
					}
				}
			}
		}
	}
	t.Fatal("gauge missing")
}
func assertVendorZero(t *testing.T, out *council.EditorOutput) {
	t.Helper()
	if out == nil || out.CostUSD != 0 || out.Sidecar.CostUSD.Frontier != 0 || out.CostUnpriced || !strings.Contains(out.Sidecar.Notes, "no charge") {
		t.Fatalf("not known zero: %+v", out)
	}
}

func TestVendorAnthropicAccountingAndJudgePropagation(t *testing.T) {
	for _, kind := range []VendorErrorKind{VendorBilling, VendorAuth} {
		for _, cause := range []error{errors.New("rejected"), ErrVendorBreakerOpen} {
			e := &VendorError{Vendor: "anthropic", Kind: kind, Err: cause}
			fake := &fakeAnthropicMessenger{err: e}
			ed := &AnthropicCouncilEditor{Client: fake, Model: "unknown-model"}
			out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "test"}, nil)
			assertVendorZero(t, out)
			if got, ok := AsVendorError(err); !ok || got != e {
				t.Fatalf("editor lost error: %v", err)
			}
			judge := &AnthropicRubricJudge{Client: fake, Model: "claude-sonnet-5"}
			_, err = judge.Judge(context.Background(), gates.PRSelfReviewRubricName, gates.StageInput{})
			if got, ok := AsVendorError(err); !ok || got != e || !errors.Is(err, cause) {
				t.Fatalf("judge lost error: %v", err)
			}
		}
	}
}

func TestVendorAnthropicRetriesExhaustBeforeTrip(t *testing.T) {
	calls := 0
	b := NewVendorBreaker(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if b.Open("anthropic") {
			t.Error("breaker tripped before SDK exhausted retries")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("retry-after-ms", "1")
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	}))
	defer srv.Close()
	c := &AnthropicClient{api: anthropic.NewClient(option.WithAPIKey("test"), option.WithBaseURL(srv.URL), option.WithMaxRetries(2)), breaker: b}
	_, err := c.CreateMessage(context.Background(), anthropicMessageRequest{Model: "test"})
	if e, ok := AsVendorError(err); !ok || e.Kind != VendorOverloaded || calls != 3 || !b.Open("anthropic") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestVendorAnthropicSuccessRecovery(t *testing.T) {
	now := time.Unix(100, 0)
	b := NewVendorBreaker(func() time.Time { return now })
	b.Trip("anthropic", VendorBilling, time.Minute)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// A successful in-flight completion resets the circuit even if another
		// request tripped it while this request was being served.
		b.Trip("anthropic", VendorOverloaded, time.Minute)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"test\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	c, err := NewAnthropicClient(AnthropicClientConfig{APIKey: "test", BaseURL: srv.URL, Breaker: b})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateMessage(context.Background(), anthropicMessageRequest{Model: "test"})
	if !errors.Is(err, ErrVendorBreakerOpen) || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	now = now.Add(time.Minute)
	res, err := c.CreateMessage(context.Background(), anthropicMessageRequest{Model: "test"})
	if err != nil || calls != 1 || b.Open("anthropic") || res.InputTokens != 10 {
		t.Fatalf("recovery calls=%d res=%+v err=%v", calls, res, err)
	}
}

func TestVendorOpenAIMalformedAndTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`not json`)) }))
	defer srv.Close()
	c, err := openairesponses.NewAPIClient(openairesponses.APIClientConfig{APIKey: "test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	b := NewVendorBreaker(nil)
	ed := &OpenAIResponsesCouncilEditor{Client: c, Model: "gpt-5.4", Breaker: b}
	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "test"}, nil)
	if e, ok := AsVendorError(err); !ok || e.Kind != VendorUnknown || b.Open("openai") || !out.CostUnpriced {
		t.Fatalf("malformed: out=%+v err=%v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ed.Edit(ctx, &council.Brief{Markdown: "test"}, nil)
	if e, ok := AsVendorError(err); !ok || e.Kind != VendorTransport || !errors.Is(err, context.Canceled) || b.Open("openai") {
		t.Fatalf("transport: %v", err)
	}
}

func TestVendorAnthropicUsageBearingFailureStillPriced(t *testing.T) {
	res := anthropicMessageResult{InputTokens: 100, OutputTokens: 10}
	ed := &AnthropicCouncilEditor{Client: &fakeAnthropicMessenger{res: res, err: &VendorError{Vendor: "anthropic", Kind: VendorBilling, Err: errors.New("late failure")}}, Model: "claude-opus-4-8"}
	out, err := ed.Edit(context.Background(), &council.Brief{Markdown: "test"}, nil)
	want, ok := anthropicCouncilResponseCostUSD(ed.Model, res)
	if err == nil || !ok || want <= 0 || out.CostUSD != want || out.Sidecar.CostUSD.Frontier != want || out.CostUnpriced {
		t.Fatalf("lost usage charge: %+v err=%v", out, err)
	}
}

// A breaker-open rejection must carry the kind that tripped the circuit:
// callers route on IsVendorBilling, and a transient rate-limit trip must not
// masquerade as a billing hold (review finding W1, 2026-09-14).
func TestVendorBreakerRejectionCarriesTripKind(t *testing.T) {
	now := time.Now()
	b := NewVendorBreaker(func() time.Time { return now })
	b.Trip("anthropic", VendorRateLimit, 2*time.Minute)
	err := b.check("anthropic")
	var ve *VendorError
	if !errors.As(err, &ve) || !errors.Is(err, ErrVendorBreakerOpen) {
		t.Fatalf("check while open = %v, want *VendorError wrapping ErrVendorBreakerOpen", err)
	}
	if ve.Kind != VendorRateLimit {
		t.Fatalf("rejection kind = %q, want %q (the trip's kind)", ve.Kind, VendorRateLimit)
	}
	if IsVendorBilling(err) {
		t.Fatalf("a rate-limit trip must not read as billing")
	}
	b.Trip("openai", VendorBilling, 30*time.Minute)
	if err := b.check("openai"); !IsVendorBilling(err) {
		t.Fatalf("billing trip rejection = %v, want IsVendorBilling", err)
	}
}

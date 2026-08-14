package llmusage

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// TestCachedTokensCoalescesBothDialects is the reason this package exists: a
// caller must not have to know whether its lane speaks the chat-completions
// dialect or the Responses dialect.
func TestCachedTokensCoalescesBothDialects(t *testing.T) {
	tests := []struct {
		name          string
		promptDetails Details
		inputDetails  Details
		want          int
	}{
		{"chat completions dialect", Details{CachedTokens: 1024}, Details{}, 1024},
		{"responses dialect", Details{}, Details{CachedTokens: 2048}, 2048},
		{"input details win when both present", Details{CachedTokens: 1}, Details{CachedTokens: 2048}, 2048},
		{"neither reported", Details{}, Details{}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CachedTokens(tc.promptDetails, tc.inputDetails); got != tc.want {
				t.Fatalf("CachedTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDetailsParsesWireShape pins the JSON tag against the actual field name
// the engines emit. A typo here is exactly the silent failure this whole MR is
// meant to rule out: the struct would parse, report zero, and look like a cold
// cache forever.
func TestDetailsParsesWireShape(t *testing.T) {
	var usage struct {
		PromptTokens        int     `json:"prompt_tokens"`
		PromptTokensDetails Details `json:"prompt_tokens_details"`
		InputTokensDetails  Details `json:"input_tokens_details"`
	}
	raw := `{"prompt_tokens":4096,"prompt_tokens_details":{"cached_tokens":3968},"input_tokens_details":{"cached_tokens":0}}`
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if usage.PromptTokens != 4096 {
		t.Errorf("prompt_tokens = %d, want 4096", usage.PromptTokens)
	}
	if got := CachedTokens(usage.PromptTokensDetails, usage.InputTokensDetails); got != 3968 {
		t.Errorf("cached tokens = %d, want 3968", got)
	}
}

// TestCachedShareDistinguishesUnknownFromZero guards the decision documented on
// CachedShare. An engine that omits the field must not be reported as a 0% hit
// rate, because that reads as a broken cache and sends someone debugging the
// wrong layer.
func TestCachedShareDistinguishesUnknownFromZero(t *testing.T) {
	unknown := Usage{}
	if got := unknown.CachedShare(); got != -1 {
		t.Errorf("empty usage share = %v, want -1 (unknown)", got)
	}
	if unknown.Reported() {
		t.Error("empty usage must not report as measured")
	}

	genuineMiss := Usage{PromptTokens: 1000, CachedPromptTokens: 0}
	if got := genuineMiss.CachedShare(); got != 0 {
		t.Errorf("cold prompt share = %v, want 0", got)
	}
	if !genuineMiss.Reported() {
		t.Error("a prompt with tokens must report as measured")
	}

	warm := Usage{PromptTokens: 1000, CachedPromptTokens: 990}
	if got := warm.CachedShare(); got != 0.99 {
		t.Errorf("warm prompt share = %v, want 0.99", got)
	}
}

// TestLogAttrsOmitsUnknownFields keeps absent data absent, so a log query can
// filter on the presence of cached_tokens.
func TestLogAttrsOmitsUnknownFields(t *testing.T) {
	attrs := Usage{PromptTokens: 100, CompletionTokens: 20}.LogAttrs("mills-judge", "gemma4")
	joined := keys(attrs)
	for _, want := range []string{FieldComponent, FieldModel, FieldPromptTokens, FieldCompletionTokens} {
		if !contains(joined, want) {
			t.Errorf("expected field %q in %v", want, joined)
		}
	}
	for _, unwanted := range []string{FieldCachedTokens, FieldCachedShare} {
		if contains(joined, unwanted) {
			t.Errorf("field %q must be omitted when the engine did not report it", unwanted)
		}
	}

	warm := Usage{PromptTokens: 1000, CachedPromptTokens: 793, CompletionTokens: 20}.LogAttrs("mills-weaver", "kimi-k3")
	if !contains(keys(warm), FieldCachedTokens) || !contains(keys(warm), FieldCachedShare) {
		t.Errorf("expected cached fields when reported, got %v", keys(warm))
	}
	if got := value(warm, FieldCachedShare); got != 0.793 {
		t.Errorf("cached_share = %v, want 0.793", got)
	}
}

// TestObserveSilentOnUnreportedUsage: a proxy that omits usage must produce no
// output at all, rather than a stream of zeroes that would drag down any
// average computed from the logs.
func TestObserveSilentOnUnreportedUsage(t *testing.T) {
	var buf bytes.Buffer
	sink := &recordingSink{}
	obs := Observer{
		Logger:    slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Component: "test",
		Sink:      sink,
	}

	obs.Observe(context.Background(), "gemma4", Usage{})
	if buf.Len() != 0 {
		t.Errorf("expected no log output for unreported usage, got %q", buf.String())
	}
	if len(sink.calls) != 0 {
		t.Errorf("expected no sink calls for unreported usage, got %d", len(sink.calls))
	}

	obs.Observe(context.Background(), "gemma4", Usage{PromptTokens: 10, CompletionTokens: 2})
	if !strings.Contains(buf.String(), MessageUsage) {
		t.Errorf("expected %q in log output, got %q", MessageUsage, buf.String())
	}
	if len(sink.calls) != 1 {
		t.Fatalf("expected 1 sink call, got %d", len(sink.calls))
	}
	if sink.calls[0].component != "test" {
		t.Errorf("sink component = %q, want %q", sink.calls[0].component, "test")
	}
}

// TestObserveContextComponentOverridesDefault covers the mills case: one HTTP
// client, several callers whose cache behaviour there is no reason to average
// together.
func TestObserveContextComponentOverridesDefault(t *testing.T) {
	sink := &recordingSink{}
	obs := Observer{Component: "mills-flexinfer", Sink: sink}

	obs.Observe(context.Background(), "gemma4", Usage{PromptTokens: 10})
	obs.Observe(WithComponent(context.Background(), "mills-judge"), "gemma4", Usage{PromptTokens: 10})

	if len(sink.calls) != 2 {
		t.Fatalf("expected 2 sink calls, got %d", len(sink.calls))
	}
	if sink.calls[0].component != "mills-flexinfer" {
		t.Errorf("default component = %q, want mills-flexinfer", sink.calls[0].component)
	}
	if sink.calls[1].component != "mills-judge" {
		t.Errorf("overridden component = %q, want mills-judge", sink.calls[1].component)
	}
}

// TestZeroObserverIsSilent: embedding an Observer in a client must not require
// every existing constructor caller to be updated, so the zero value has to be
// safe rather than nil-panicking or defaulting to slog.Default().
func TestZeroObserverIsSilent(t *testing.T) {
	var obs Observer
	obs.Observe(context.Background(), "gemma4", Usage{PromptTokens: 100, CachedPromptTokens: 90})
	// Reaching here without a panic is the assertion.
}

func TestWithComponentIgnoresEmpty(t *testing.T) {
	ctx := WithComponent(context.Background(), "mills-judge")
	if got := ComponentFrom(WithComponent(ctx, "")); got != "mills-judge" {
		t.Errorf("empty component must not clear an existing one, got %q", got)
	}
	//nolint:staticcheck // deliberately exercising the nil-context guard
	if got := ComponentFrom(nil); got != "" {
		t.Errorf("nil context component = %q, want empty", got)
	}
}

type recordingSink struct {
	mu    sync.Mutex
	calls []sinkCall
}

type sinkCall struct {
	component string
	model     string
	usage     Usage
}

func (s *recordingSink) RecordUsage(component, model string, u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sinkCall{component: component, model: model, usage: u})
}

func keys(attrs []any) []string {
	out := make([]string, 0, len(attrs)/2)
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok {
			out = append(out, k)
		}
	}
	return out
}

func value(attrs []any, key string) any {
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok && k == key {
			return attrs[i+1]
		}
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

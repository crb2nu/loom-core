package clients

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/crb2nu/loom/pkg/llmusage"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// cachedUsageBody is a verdict response whose usage block reports a warm
// prefix in the chat-completions dialect.
const cachedUsageBody = `{
  "model": "qwen3-8b-instruct",
  "choices": [
    {"message": {"role": "assistant", "content": "{\"score\": 0.85, \"reasons\": [\"a\", \"b\"]}"}}
  ],
  "usage": {
    "prompt_tokens": 4096,
    "completion_tokens": 30,
    "total_tokens": 4126,
    "prompt_tokens_details": {"cached_tokens": 3968}
  }
}`

// TestChatResponseParsesCachedTokens pins the JSON tags on the mills usage
// struct. Mills is the highest-traffic instrumented surface in the repo, and a
// wrong tag here would silently report a permanent cold cache — the exact
// failure mode this instrumentation exists to rule out.
func TestChatResponseParsesCachedTokens(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantCached int
	}{
		{"chat completions dialect", cachedUsageBody, 3968},
		{
			name: "responses dialect",
			body: `{"model":"m","choices":[{"message":{"role":"assistant","content":"{\"score\":0.5,\"reasons\":[\"x\"]}"}}],` +
				`"usage":{"prompt_tokens":4096,"completion_tokens":10,"input_tokens_details":{"cached_tokens":1024}}}`,
			wantCached: 1024,
		},
		{
			name: "engine omits the details block",
			body: `{"model":"m","choices":[{"message":{"role":"assistant","content":"{\"score\":0.5,\"reasons\":[\"x\"]}"}}],` +
				`"usage":{"prompt_tokens":4096,"completion_tokens":10}}`,
			wantCached: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cli := newStubClient(t, tc.body, 200)
			_, resp, err := cli.chatWithOptions(context.Background(), "m", "prompt", 256, chatOptions{})
			if err != nil {
				t.Fatalf("chatWithOptions: %v", err)
			}
			usage := resp.normalizedUsage()
			if usage.PromptTokens != 4096 {
				t.Errorf("prompt tokens = %d, want 4096", usage.PromptTokens)
			}
			if usage.CachedPromptTokens != tc.wantCached {
				t.Errorf("cached prompt tokens = %d, want %d", usage.CachedPromptTokens, tc.wantCached)
			}
		})
	}
}

// TestNormalizedUsageNilSafe: chatWithOptions returns a nil *chatResponse on
// several error paths, and the observer runs on the same line as the decode.
func TestNormalizedUsageNilSafe(t *testing.T) {
	var nilResp *chatResponse
	if got := nilResp.normalizedUsage(); got.Reported() {
		t.Errorf("nil response must yield unreported usage, got %+v", got)
	}
}

// TestJudgeTagsItsOwnComponent proves the per-caller attribution works through
// a real entry point. Without it every mills caller's cache behaviour would be
// averaged into one indistinguishable number, which is the thing that makes the
// data useless for deciding the journalengine adoption question.
func TestJudgeTagsItsOwnComponent(t *testing.T) {
	var seen []string
	cli := newStubClient(t, cachedUsageBody, 200)
	cli.SetTransport(componentCapturingTransport(t, cachedUsageBody, &seen))

	judge := NewRubricJudge(cli)
	if _, err := judge.Judge(context.Background(), gates.SpecConformanceRubricName, gates.StageInput{
		Item:         &store.BacklogItem{ID: "BL-X", Title: "x"},
		FilesChanged: []string{"foo.go"},
	}); err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if len(seen) == 0 {
		t.Fatal("expected at least one chat call")
	}
	if seen[0] != ComponentJudge {
		t.Errorf("component = %q, want %q", seen[0], ComponentJudge)
	}
}

// TestServedModelPrefersEngineReport documents why the model label is taken
// from the response rather than the request: a fallback chain or gateway alias
// substitutes models, and the prefix cache belongs to the engine that served.
func TestServedModelPrefersEngineReport(t *testing.T) {
	if got := servedModel("qwen3-8b-instruct", "requested-alias"); got != "qwen3-8b-instruct" {
		t.Errorf("servedModel = %q, want the engine-reported name", got)
	}
	if got := servedModel("  ", "requested-alias"); got != "requested-alias" {
		t.Errorf("servedModel = %q, want the requested name when the engine reports none", got)
	}
}

// componentCapturingTransport records the llmusage component label carried on
// each outbound request's context. Reading it off the request works because
// WithComponent puts it on the context the client threads into the HTTP call,
// which is the same mechanism the observer reads.
func componentCapturingTransport(t *testing.T, body string, seen *[]string) roundTripFn {
	t.Helper()
	return roundTripFn(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/chat/completions" {
			return nil, errors.New("unexpected path: " + req.URL.Path)
		}
		*seen = append(*seen, llmusage.ComponentFrom(req.Context()))
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     make(http.Header),
		}, nil
	})
}

// TestMillsUsageSinkTolerantOfEmptyUsage guards the Prometheus path against
// creating all-zero series, which on a dashboard are indistinguishable from a
// measured 0% hit rate.
func TestMillsUsageSinkTolerantOfEmptyUsage(t *testing.T) {
	millsUsageSink{}.RecordUsage("mills-judge", "m", llmusage.Usage{})
	millsUsageSink{}.RecordUsage("mills-judge", "m", llmusage.Usage{PromptTokens: 10, CachedPromptTokens: 5, CompletionTokens: 2})
	// No panic and no assertion on the global counters: promauto registers
	// them process-wide, so asserting absolute values here would couple this
	// test to every other test in the package that happens to make a chat call.
}
